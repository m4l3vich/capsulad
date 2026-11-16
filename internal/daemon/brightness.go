package daemon

import (
	"math"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend/profile"
	"capsulad/internal/settings"
)

// Ambient-light driven brightness, replicating the stock voice-agent.
//
// The vendor's light handler (0x27ca68, firmware 03104B1D40E15C55) maps lux to a
// brightness percentage:
//
//	lux < MinLux         -> 0
//	lux > MaxLux         -> 100
//	otherwise            -> trunc(log(Bias + Lambda*lux) * Scale), clamped to [Floor, Ceiling]
//
// and only re-applies when the result differs from the last applied value by
// more than Deadband. The measured output at 7.4 lux was 14/255 (5.5 %), which
// the profile reproduces (5 % floor). The separate pow(x,4) curve in
// ClockBrightnessCalc is not the clock brightness. Every value comes from the
// device profile.

// ambientBrightness returns the stock brightness fraction for a lux reading.
func ambientBrightness(curve profile.Brightness, lux float64) float64 {
	if math.IsNaN(lux) || lux < 0 {
		lux = 0
	}
	switch {
	case lux < curve.MinLux:
		return 0
	case lux > curve.MaxLux:
		return 1
	}
	pct := math.Trunc(math.Log(curve.Bias+curve.Lambda*lux) * curve.Scale)
	pct = math.Max(curve.Floor, math.Min(curve.Ceiling, pct))
	return pct / 100
}

// autoBrightness maps one ambient sample to an auto-managed level. A malformed
// sample (NaN, an infinity, or a negative illuminance from a transient read) is
// reported as invalid so the caller keeps the last good target instead of
// treating it as darkness. Valid levels are floored at AutoFloor: an enabled
// automatic display must never blank, even at 0 lux.
func autoBrightness(curve profile.Brightness, lux float64) (float64, bool) {
	if math.IsNaN(lux) || math.IsInf(lux, 0) || lux < 0 {
		return 0, false
	}
	return math.Max(curve.AutoFloor, ambientBrightness(curve, lux)), true
}

// clampAutoBrightness keeps every auto path above the visible floor, including
// the window before the first valid sensor sample and persisted 0 values.
func clampAutoBrightness(curve profile.Brightness, v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, -1) || v < curve.AutoFloor {
		return curve.AutoFloor
	}
	if v > 1 {
		return 1
	}
	return v
}

// stepToward bounds one animation tick so brightness changes are a short fade
// rather than a jump.
func stepToward(current, target, step float64) float64 {
	switch {
	case current < target:
		return math.Min(target, current+step)
	case current > target:
		return math.Max(target, current-step)
	default:
		return target
	}
}

// resetAutoLevels drops cached sensor levels for resources whose brightness
// mode changed. A level belongs to the automatic period that sampled it:
// keeping it would let a re-enabled automatic display adopt a value from an
// earlier period (possibly in a different room) before the next sample.
func (d *Daemon) resetAutoLevels(prev, next *settings.UserSettings) {
	if prev == nil || next == nil {
		return
	}
	var changed []string
	for _, info := range d.resources {
		if prev.BrightnessMode[info.ResourceId] != next.BrightnessMode[info.ResourceId] {
			changed = append(changed, info.ResourceId)
		}
	}
	if len(changed) == 0 {
		return
	}
	d.autoMu.Lock()
	for _, id := range changed {
		delete(d.autoLvl, id)
	}
	d.autoMu.Unlock()
}

// effectiveBrightness resolves the brightness for one resource: the constant
// setting, or the latest sensor-derived level when the user chose "auto". The
// bool reports whether a value exists at all, so callers can keep their own
// default for unknown resources.
func (d *Daemon) effectiveBrightness(resourceID string) (float64, bool) {
	curve := d.profile().Brightness
	values := d.user.Get().Values
	if values.BrightnessMode[resourceID] == settings.BrightnessAuto {
		d.autoMu.Lock()
		lvl, ok := d.autoLvl[resourceID]
		d.autoMu.Unlock()
		if ok {
			return clampAutoBrightness(curve, lvl), true
		}
		// No valid sample yet: an enabled automatic display still must not
		// blank, so a persisted manual 0 is treated as the visible floor.
		v, ok := values.DisplayBrightness[resourceID]
		if !ok {
			return curve.AutoFloor, true
		}
		return clampAutoBrightness(curve, v), true
	}
	v, ok := values.DisplayBrightness[resourceID]
	return v, ok
}

// autoBrightnessLoop samples the ambient sensor once a second and animates the
// auto-managed resources toward the stock curve.
func (d *Daemon) autoBrightnessLoop() {
	curve := d.profile().Brightness
	sensors := d.dev.Sensors()
	haveSensor := false
	if sensors != nil {
		for _, s := range sensors.List() {
			if s.ID == "sensor.ambient_light" {
				haveSensor = true
				break
			}
		}
	}
	tick := time.NewTicker(curve.Tick)
	defer tick.Stop()
	lastSample := time.Time{}
	targets := map[string]float64{}
	warned := false
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-tick.C:
		}
		values := d.user.Get().Values
		if time.Since(lastSample) >= curve.PollInterval {
			lastSample = time.Now()
			if haveSensor {
				if lux, ok := sensors.Read("sensor.ambient_light"); ok {
					target, valid := autoBrightness(curve, lux)
					if !valid {
						// ParseFloat accepts "nan"/"inf" and a sensor can return
						// a transient negative; none of those mean darkness.
						// Keep the last good target instead of blanking.
						continue
					}
					for _, info := range d.resources {
						if info.Kind != frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY && info.Kind != frameworkv1.ResourceKind_RESOURCE_LED_RING {
							continue
						}
						if values.BrightnessMode[info.ResourceId] != settings.BrightnessAuto {
							continue
						}
						prev, known := targets[info.ResourceId]
						// Stock only re-applies when the calculated value moves by
						// more than five percentage points.
						if known && math.Abs(prev-target) <= curve.Deadband {
							continue
						}
						targets[info.ResourceId] = target
						d.logs.Infof("brightness", "", "auto brightness %.0f%% for ambient %.1f lux", target*100, lux)
					}
				}
			} else if !warned {
				d.logs.Warnf("brightness", "", "auto brightness requested but no ambient-light sensor exists; using constant values")
				warned = true
			}
		}
		d.autoMu.Lock()
		for id, target := range targets {
			if values.BrightnessMode[id] != settings.BrightnessAuto {
				continue
			}
			current, ok := d.autoLvl[id]
			if !ok {
				current = target // the first sample applies immediately
			}
			d.autoLvl[id] = stepToward(current, target, curve.AnimationStep)
		}
		d.autoMu.Unlock()
	}
}
