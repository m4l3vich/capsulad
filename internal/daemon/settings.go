package daemon

import (
	"math"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

// seedDefaults selects bundled providers on first run without overriding an
// explicit user choice. Provides hints only populate dropdowns; they never
// grant capabilities.
func (d *Daemon) seedDefaults() {
	snap := d.user.Get()
	patch := snap.Values
	changed := false
	var fields []string
	for _, info := range d.resources {
		var want string
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			want = "segment_display_provider"
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			want = "led_ring_controller"
		default:
			continue
		}
		if patch.DefaultAppByResource[info.ResourceId] != "" {
			continue
		}
		for _, app := range d.reg.List() {
			if hasProvide(app.Manifest.Provides, want) {
				if patch.DefaultAppByResource == nil {
					patch.DefaultAppByResource = map[string]string{}
				}
				patch.DefaultAppByResource[info.ResourceId] = app.Manifest.ID
				changed = true
				break
			}
		}
	}
	if patch.DefaultAssistantAppID == "" {
		for _, app := range d.reg.List() {
			if hasProvide(app.Manifest.Provides, "voice_assistant") {
				patch.DefaultAssistantAppID = app.Manifest.ID
				changed = true
				break
			}
		}
	}
	if !changed {
		return
	}
	fields = []string{"default_app_by_resource", "default_assistant_app_id"}
	if _, err := d.user.Update(snap.Revision, patch, fields); err != nil {
		d.logs.Warnf("daemon", "", "could not seed default providers: %v", err)
	}
}

func hasProvide(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

// watchSettings applies every durable settings snapshot to live subsystems.
func (d *Daemon) watchSettings() {
	_, ch, cancel := d.user.Watch()
	defer cancel()
	for {
		select {
		case <-d.ctx.Done():
			return
		case snap, ok := <-ch:
			if !ok {
				return
			}
			d.applySettings(snap)
		}
	}
}

func (d *Daemon) applySettings(snap settings.UserSnapshot) {
	v := snap.Values
	for _, info := range d.resources {
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY, frameworkv1.ResourceKind_RESOURCE_LED_RING:
			d.router.SetDefault(info.ResourceId, v.DefaultAppByResource[info.ResourceId])
		}
	}
	d.router.SetDefault("role.voice_assistant", v.DefaultAssistantAppID)
	if d.launcher != nil {
		d.launcher.SetTimezone(v.Timezone)
	}
	timezoneChanged := d.lastApplied != nil && d.lastApplied.Timezone != v.Timezone
	d.audio.SetMasterVolume(v.MasterVolume)
	d.audio.SetMuted(v.MicrophoneMuted)
	d.applyBluetoothSettings(v)
	d.applyBluetoothVolume(v.MasterVolume)
	d.wake.SetEnabled(v.WakewordEnabled)
	d.wake.SetMuted(v.MicrophoneMuted)

	if v.MicrophoneMuted != d.lastMuted {
		d.lastMuted = v.MicrophoneMuted
		d.updateMicIndicator(v.MicrophoneMuted)
	}
	d.updateSystemRing()

	prev := d.lastApplied
	d.lastApplied = &v
	if timezoneChanged {
		// A running app keeps the TZ it was launched with, so the new device
		// timezone only takes effect on the next launch.
		d.restartBootAppsForTimezone()
	}
	if prev == nil || !sameStringMap(prev.DefaultAppByResource, v.DefaultAppByResource) || prev.DefaultAssistantAppID != v.DefaultAssistantAppID {
		for _, appID := range v.DefaultAppByResource {
			d.ensureRunning(appID)
		}
		d.ensureRunning(v.DefaultAssistantAppID)
	}
}

// restartBootAppsForTimezone stops every running app and starts the boot set
// again so it picks up the new TZ. Launch refuses while a run is still
// registered, so the restart waits for each stop to complete before relaunching.
// Ad-hoc runs are left alone; their next invocation launches them with the new
// environment.
func (d *Daemon) restartBootAppsForTimezone() {
	if d.sup == nil {
		return
	}
	var stopped []string
	seen := map[string]bool{}
	for _, run := range d.sup.List("") {
		if seen[run.AppID] {
			continue
		}
		seen[run.AppID] = true
		stopped = append(stopped, run.AppID)
		d.sup.StopApp(run.AppID)
	}
	tz := d.user.Get().Values.Timezone
	d.logs.Infof("daemon", "", "timezone changed to %q; restarting %d app(s) to apply it", tz, len(stopped))
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			pending := false
			for _, appID := range stopped {
				if len(d.sup.List(appID)) > 0 {
					pending = true
					break
				}
			}
			if !pending {
				break
			}
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		d.startBootApps()
		d.logs.Infof("daemon", "", "restarted boot applications for timezone %q", tz)
	}()
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (d *Daemon) ringResourceID() string {
	for _, info := range d.resources {
		if info.Kind == frameworkv1.ResourceKind_RESOURCE_LED_RING {
			return info.ResourceId
		}
	}
	return ""
}

// updateMicIndicator implements the user-selected muted-indication lifetime as
// a temporary system capture over the ring baseline. It never changes mute
// state and is distinct from the app microphone watchdog.
func (d *Daemon) updateMicIndicator(muted bool) {
	if muted {
		d.startMicIndicator()
		return
	}
	go d.fadeOutMicIndicator("unmuted")
}

// indicatorParams builds the muted indication: red on the bottom three pixels,
// because pixel 0 is the top of the ring and indices advance clockwise. A short
// fade keeps the transition from snapping, matching the stock agent.
func indicatorParams(fadeIn, intensity float64) presets.Params {
	return presets.Params{
		"color": rgb(255, 0, 0), "start": int64(5), "count": int64(3),
		"intensity": intensity, "fade_in": fadeIn,
	}
}

func cloneParams(in presets.Params) presets.Params {
	out := make(presets.Params, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (d *Daemon) startMicIndicator() {
	ringID := d.ringResourceID()
	if ringID == "" {
		return
	}
	snap := d.user.Get().Values
	d.indicatorMu.Lock()
	defer d.indicatorMu.Unlock()
	if d.micCaptureID != "" {
		return
	}
	if d.micIndicator == nil {
		d.micIndicator = d.pres.Open(ringID, routing.SystemAppID, "system", presentations.Ring)
	}
	params, err := presets.ValidateParams(mustPreset("system.ring.indicator"), indicatorParams(0.25, 1.0))
	if err != nil {
		d.logs.Errorf("daemon", "", "muted indication: %v", err)
		return
	}
	if err := d.pres.Update(d.micIndicator, presentations.RingEffect("system.ring.indicator", params)); err != nil {
		d.logs.Errorf("daemon", "", "muted indication: %v", err)
		return
	}
	cap, err := d.router.Acquire(ringID, routing.SystemAppID, "system", d.micIndicator.ID, routing.TouchNone)
	if err != nil {
		d.logs.Errorf("daemon", "", "muted indication capture: %v", err)
		return
	}
	d.micCaptureID = cap.ID
	d.micParams = params
	d.micGen++
	d.logs.Infof("daemon", "", "microphone muted indication active (capture=%s mode=%d)", cap.ID, snap.MutedIndicator)
	switch snap.MutedIndicator {
	case settings.MutedImmediate:
		d.releaseMicIndicatorLocked("immediate")
	case settings.MutedUntilUnmute:
		// Keep it until an explicit unmute.
	default:
		delay := time.Duration(snap.MutedIndicatorDelayMS) * time.Millisecond
		if delay <= 0 {
			delay = 30 * time.Second
		}
		d.micTimer = time.AfterFunc(delay, func() {
			d.fadeOutMicIndicator("expired")
		})
	}
}

// fadeOutMicIndicator tapers the indication to black before releasing the
// capture. The generation counter aborts a fade that races a fresh mute.
func (d *Daemon) fadeOutMicIndicator(reason string) {
	d.indicatorMu.Lock()
	if d.micCaptureID == "" || d.micIndicator == nil {
		d.indicatorMu.Unlock()
		return
	}
	gen := d.micGen
	p := d.micIndicator
	base := cloneParams(d.micParams)
	d.indicatorMu.Unlock()

	const steps = 6
	for i := 1; i <= steps; i++ {
		d.indicatorMu.Lock()
		stale := d.micGen != gen
		d.indicatorMu.Unlock()
		if stale {
			return
		}
		intensity, _ := base["intensity"].(float64)
		params := cloneParams(base)
		params["intensity"] = intensity * (1 - float64(i)/steps)
		params["fade_in"] = 0.0
		_ = d.pres.Update(p, presentations.RingEffect("system.ring.indicator", params))
		time.Sleep(40 * time.Millisecond)
	}

	d.indicatorMu.Lock()
	defer d.indicatorMu.Unlock()
	if d.micGen != gen {
		return
	}
	d.releaseMicIndicatorLocked(reason)
}

func (d *Daemon) releaseMicIndicatorLocked(reason string) {
	if d.micTimer != nil {
		d.micTimer.Stop()
		d.micTimer = nil
	}
	if d.micCaptureID == "" {
		return
	}
	ringID := d.ringResourceID()
	sel := d.router.Current(ringID)
	selected := sel.SelectedCaptureID == d.micCaptureID
	d.router.ForceRelease(d.micCaptureID)
	d.micCaptureID = ""
	if d.micIndicator != nil {
		_ = d.pres.Update(d.micIndicator, presentations.Blank())
	}
	if !selected {
		d.logs.Infof("daemon", "", "muted indication %s but was obscured by another capture (%s)", reason, sel.SelectedAppID)
	} else {
		d.logs.Infof("daemon", "", "muted indication %s; selected ring baseline restored", reason)
	}
}

func mustPreset(id string) presets.Info {
	info, ok := presets.Lookup(id)
	if !ok {
		panic("missing built-in preset " + id)
	}
	return *info
}

func rgb(r, g, b uint8) backend.RGB { return backend.RGB{R: r, G: g, B: b} }

// adjustVolume moves the master volume by whole percentage points, so the
// displayed value never drifts and the keypad steps stay exact. It reports
// whether the value changed; a ramp uses that to stop at a bound.
func (d *Daemon) adjustVolume(delta int) bool {
	for i := 0; i < 3; i++ {
		snap := d.user.Get()
		pct := int(math.Round(snap.Values.MasterVolume*100)) + delta
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		next := float64(pct) / 100
		changed := next != snap.Values.MasterVolume
		patch := snap.Values
		patch.MasterVolume = next
		if _, err := d.user.Update(snap.Revision, patch, []string{"master_volume"}); err == nil {
			d.logs.Infof("daemon", "", "master volume %d%%", pct)
			// Apply before the cue so the tick is heard at the new level, then
			// show the percentage for a moment.
			d.audio.SetMasterVolume(next)
			// During a fast ramp the cue is throttled so steps do not turn into
			// a buzz; the last step of a tap always plays.
			if d.cueAllowed() {
				d.playVolumeCue()
			}
			d.showVolumeOSD(pct)
			return changed
		}
	}
	d.logs.Warnf("daemon", "", "could not update master volume")
	return false
}

// cueAllowed throttles the volume tick to at most one per 80 ms.
func (d *Daemon) cueAllowed() bool {
	d.cueMu.Lock()
	defer d.cueMu.Unlock()
	if time.Since(d.lastCue) < 80*time.Millisecond {
		return false
	}
	d.lastCue = time.Now()
	return true
}

// writeUserSettings applies one mutation with bounded CAS retries.
func (d *Daemon) writeUserSettings(fields []string, mutate func(*settings.UserSettings)) error {
	var last error
	for i := 0; i < 3; i++ {
		snap := d.user.Get()
		patch := snap.Values
		mutate(&patch)
		if _, err := d.user.Update(snap.Revision, patch, fields); err == nil {
			return nil
		} else {
			last = err
		}
	}
	return last
}

func (d *Daemon) audioState() *frameworkv1.AudioState {
	return &frameworkv1.AudioState{
		MasterVolume: float32(d.audio.MasterVolume()), MicrophoneMuted: d.audio.Muted(),
		MicrophoneMuteIsHardware: d.audio.HardwareMute(),
	}
}

func (d *Daemon) setMuted(muted bool) {
	for i := 0; i < 3; i++ {
		snap := d.user.Get()
		patch := snap.Values
		patch.MicrophoneMuted = muted
		if _, err := d.user.Update(snap.Revision, patch, []string{"microphone_muted"}); err == nil {
			d.logs.Infof("daemon", "", "microphone muted=%v", muted)
			return
		}
	}
	d.logs.Warnf("daemon", "", "could not update microphone mute")
}

// updateSystemRing maintains the safe framework fallback ring presentation:
// a blue pulse while an app is capturing the microphone and no app has the
// ring selected.
func (d *Daemon) updateSystemRing() {
	ringID := d.ringResourceID()
	if ringID == "" {
		return
	}
	fallback := d.fallback[ringID]
	if fallback == nil {
		return
	}
	snap := d.user.Get().Values
	state := "blank"
	if snap.MicrophoneMuted {
		state = "blank"
	} else {
		for _, s := range d.audio.ActiveStreams() {
			if s.Input && s.AppID != "" && s.AppID != routing.SystemAppID {
				state = "mic_active"
				break
			}
		}
	}
	d.sysRingMu.Lock()
	if state == d.sysRingState {
		d.sysRingMu.Unlock()
		return
	}
	d.sysRingState = state
	d.sysRingMu.Unlock()
	switch state {
	case "mic_active":
		params, err := presets.ValidateParams(mustPreset("system.ring.pulse"), presets.Params{
			"speed": 0.9, "color": rgb(0, 136, 255), "min_intensity": 0.08,
		})
		if err == nil {
			_ = d.pres.Update(fallback, presentations.RingEffect("system.ring.pulse", params))
			return
		}
	}
	_ = d.pres.Update(fallback, presentations.Blank())
}
