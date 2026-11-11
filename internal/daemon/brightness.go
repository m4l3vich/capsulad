package daemon

import (
	"math"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/settings"
)

// Ambient-light driven brightness, replicating the stock voice-agent.
//
// The vendor's light handler (0x27ca68, firmware 03104B1D40E15C55) maps lux to a
// brightness percentage:
//
//	lux < 5          -> 0
//	lux > 32768      -> 100
//	otherwise        -> trunc(log(0.75 + 0.05*lux) * 19.9375), clamped to [5, 95]
//
// and only re-applies when the result differs from the last applied value by
// more than 5 points. The measured output at 7.4 lux was 14/255 (5.5 %), which
// this reproduces (5 % floor). The separate pow(x,4) curve in
// ClockBrightnessCalc is not the clock brightness.
const (
	stockBrightnessScale    = 19.9375 // 0x419f806b
	stockBrightnessLambda   = 0.05    // 0x3d4ccccd
	stockBrightnessBias     = 0.75
	stockBrightnessFloor    = 5.0
	stockBrightnessCeil     = 95.0
	stockBrightnessDeadband = 0.05
	stockBrightnessMinLux   = 5.0
	stockBrightnessMaxLux   = 32768.0
	autoBrightnessPoll      = time.Second
	// brightnessTick paces the fade so a change looks like the vendor's
	// animated transition rather than a jump.
	brightnessTick     = 100 * time.Millisecond
	brightnessAnimStep = 0.06
)

// ambientBrightness returns the stock brightness fraction for a lux reading.
func ambientBrightness(lux float64) float64 {
	if math.IsNaN(lux) || lux < 0 {
		lux = 0
	}
	switch {
	case lux < stockBrightnessMinLux:
		return 0
	case lux > stockBrightnessMaxLux:
		return 1
	}
	pct := math.Trunc(math.Log(stockBrightnessBias+stockBrightnessLambda*lux) * stockBrightnessScale)
	pct = math.Max(stockBrightnessFloor, math.Min(stockBrightnessCeil, pct))
	return pct / 100
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

// effectiveBrightness resolves the brightness for one resource: the constant
// setting, or the latest sensor-derived level when the user chose "auto". The
// bool reports whether a value exists at all, so callers can keep their own
// default for unknown resources.
func (d *Daemon) effectiveBrightness(resourceID string) (float64, bool) {
	values := d.user.Get().Values
	if values.BrightnessMode[resourceID] == settings.BrightnessAuto {
		d.autoMu.Lock()
		lvl, ok := d.autoLvl[resourceID]
		d.autoMu.Unlock()
		if ok {
			return lvl, true
		}
	}
	v, ok := values.DisplayBrightness[resourceID]
	return v, ok
}

// autoBrightnessLoop samples the ambient sensor once a second and animates the
// auto-managed resources toward the stock curve.
func (d *Daemon) autoBrightnessLoop() {
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
	tick := time.NewTicker(brightnessTick)
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
		if time.Since(lastSample) >= autoBrightnessPoll {
			lastSample = time.Now()
			if haveSensor {
				if lux, ok := sensors.Read("sensor.ambient_light"); ok {
					target := ambientBrightness(lux)
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
						if known && math.Abs(prev-target) <= stockBrightnessDeadband {
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
			d.autoLvl[id] = stepToward(current, target, brightnessAnimStep)
		}
		d.autoMu.Unlock()
	}
}
