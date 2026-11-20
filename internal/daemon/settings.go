package daemon

import (
	"math"
	"os"
	"path/filepath"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/manifest"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
	"capsulad/internal/sounds"
)

// seedDefaults selects an installed provider on first run without overriding
// an explicit user choice. The compiled-in clock and ring apps are in the same
// registry as packages. Provides hints only populate dropdowns; they never
// grant capabilities.
func (d *Daemon) seedDefaults() {
	// An existing settings document is authoritative. Both an absent map
	// entry and an empty value mean the user selected no provider; neither
	// is permission to re-enable a provider at the next restart.
	if _, err := os.Stat(filepath.Join(d.cfg.DataDir, "settings.json")); err == nil {
		return
	} else if !os.IsNotExist(err) {
		d.logs.Warnf("daemon", "", "could not inspect settings before seeding providers: %v", err)
		return
	}
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
		if appID := d.preferredProvider(want); appID != "" {
			if patch.DefaultAppByResource == nil {
				patch.DefaultAppByResource = map[string]string{}
			}
			patch.DefaultAppByResource[info.ResourceId] = appID
			changed = true
		}
	}
	if patch.DefaultAssistantAppID == "" {
		if appID := d.preferredProvider("voice_assistant"); appID != "" {
			patch.DefaultAssistantAppID = appID
			changed = true
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

// preferredProvider returns an installed provider for a capability. External
// packages win over compiled-in system apps, so a user-installed clock or ring
// controller is selected on first run; built-ins remain the fallback on a
// device with no such package.
func (d *Daemon) preferredProvider(want string) string {
	var builtin string
	for _, app := range d.reg.List() {
		if !hasProvide(app.Manifest.Provides, want) {
			continue
		}
		if app.Manifest.Entrypoint.Runtime != manifest.RuntimeBuiltin {
			return app.Manifest.ID
		}
		if builtin == "" {
			builtin = app.Manifest.ID
		}
	}
	return builtin
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
	prev := d.lastApplied
	for _, info := range d.resources {
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY, frameworkv1.ResourceKind_RESOURCE_LED_RING:
			d.router.SetDefault(info.ResourceId, v.DefaultAppByResource[info.ResourceId])
		}
	}
	d.router.SetDefault("role.voice_assistant", v.DefaultAssistantAppID)
	d.reconcileProviderRuns(prev, v)
	if d.discovery != nil && (prev == nil || prev.Hostname != v.Hostname) {
		if err := d.discovery.Apply(d.ctx, v.Hostname); err != nil {
			d.logs.Warnf("mdns", "", "advertise hostname %q: %v", v.Hostname, err)
		}
	}
	if d.launcher != nil {
		d.launcher.SetTimezone(v.Timezone)
	}
	if d.builtins != nil && d.builtins.clock != nil {
		d.builtins.clock.setDeviceTimezone(v.Timezone)
	}
	if d.builtins != nil && d.builtins.ring != nil {
		d.builtins.ring.setDeviceTimezone(v.Timezone)
	}
	timezoneChanged := prev != nil && prev.Timezone != v.Timezone
	d.audio.SetMuted(v.MicrophoneMuted)
	d.applyBluetoothSettings(v)
	d.forgetBluetoothGain()
	// This snapshot may be older than a direct key/Bluetooth/AirPlay change
	// already committed; apply the durable master, not this queued field.
	d.applyLatestMasterVolume()
	d.wake.SetEnabled(v.WakewordEnabled)
	d.wake.SetMuted(v.MicrophoneMuted)
	// Per-word enablement and binding are settings, not engine state: applying
	// them refreshes the advertised models only, so a toggle takes effect on the
	// next activation without a re-subscribe or a warm-up.
	d.refreshWakewordModels()

	// The muted indication mirrors the current mute state and its configured
	// lifetime. Changing the mode or delay while the microphone stays muted
	// must apply immediately; waiting for the next toggle would leave the old
	// lifetime (for example an indication that never hides, or a stale timer)
	// in effect for the whole mute period.
	muteChanged := v.MicrophoneMuted != d.lastMuted
	indicatorChanged := prev != nil &&
		(prev.MutedIndicator != v.MutedIndicator || prev.MutedIndicatorDelayMS != v.MutedIndicatorDelayMS)
	if muteChanged || (indicatorChanged && v.MicrophoneMuted) {
		d.lastMuted = v.MicrophoneMuted
		d.updateMicIndicator(v.MicrophoneMuted)
	}
	// The cue follows the durable transition, not the boot snapshot: restoring a
	// muted device at startup is not a user action and stays silent. Both the
	// physical key and the RPC/Web UI paths arrive here through the watcher, so
	// one toggle makes exactly one sound.
	if prev != nil && prev.MicrophoneMuted != v.MicrophoneMuted {
		if v.MicrophoneMuted {
			d.playUISound(sounds.MicrophoneMuted())
		} else {
			d.playUISound(sounds.MicrophoneLive())
		}
	}
	d.updateSystemRing()
	// A cached sensor level belongs to the mode it was sampled in. Dropping it
	// when the mode changes keeps a re-enabled automatic display from adopting
	// the level from an earlier automatic period before the next sample.
	d.resetAutoLevels(prev, &v)

	d.lastApplied = &v
	if timezoneChanged {
		// A running app keeps the TZ it was launched with, so the new device
		// timezone only takes effect on the next launch.
		d.restartBootAppsForTimezone()
	}
	if prev == nil || !sameStringMap(prev.DefaultAppByResource, v.DefaultAppByResource) ||
		prev.DefaultAssistantAppID != v.DefaultAssistantAppID || !sameBindings(prev.Wakewords, v.Wakewords) {
		for _, appID := range v.DefaultAppByResource {
			d.ensureRunning(appID)
		}
		d.ensureRunning(v.DefaultAssistantAppID)
		// A newly bound app has to be running to receive its word.
		for _, entry := range v.Wakewords {
			if entry.AppID != "" {
				d.ensureRunning(entry.AppID)
			}
		}
	}
}

// sameBindings compares the per-word list, distinguishing the legacy nil state
// from an explicit empty list.
func sameBindings(a, b []settings.WakewordBinding) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// restartBootAppsForTimezone stops every running external app and starts the
// boot set again so it picks up the new TZ. Launch refuses while a run is still
// registered, so the restart waits for each stop to complete before relaunching.
// Compiled-in apps are skipped: they run in the daemon's environment and the
// clock already followed the zone through setDeviceTimezone. Ad-hoc runs are
// left alone; their next invocation launches them with the new environment.
func (d *Daemon) restartBootAppsForTimezone() {
	if d.sup == nil {
		return
	}
	var stopped []string
	seen := map[string]bool{}
	for _, run := range d.sup.List("") {
		// Compiled-in apps run inside the daemon, so they inherit no TZ
		// environment: the clock already received the new zone through
		// setDeviceTimezone, which keeps running timers and a monotonic
		// stopwatch. Stopping them here would clear that state, and
		// startBootApps deliberately never relaunches built-ins.
		if run.Virtual {
			continue
		}
		if seen[run.AppID] {
			continue
		}
		seen[run.AppID] = true
		stopped = append(stopped, run.AppID)
		d.sup.StopApp(run.AppID)
	}
	tz := d.user.Get().Values.Timezone
	if len(stopped) == 0 {
		d.logs.Infof("daemon", "", "timezone changed to %q; compiled-in apps follow it in place", tz)
		return
	}
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
	// Capture the mute period being ended before the fade goroutine runs. A
	// mute that lands first bumps the generation, so the fade aborts instead
	// of hiding the fresh indication.
	d.indicatorMu.Lock()
	gen := d.micGen
	d.indicatorMu.Unlock()
	go d.fadeOutMicIndicatorGen(gen, "unmuted")
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
	if d.micIndicator == nil {
		pres, err := d.openAppPresentation(routing.SystemAppID, "", ringID)
		if err != nil {
			d.logs.Errorf("daemon", "", "muted indication presentation: %v", err)
			return
		}
		d.micIndicator = pres
	}
	params, err := presets.ValidateParams(mustPreset("system.ring.indicator"), indicatorParams(0.25, 1.0))
	if err != nil {
		d.logs.Errorf("daemon", "", "muted indication: %v", err)
		return
	}
	// Re-assert the full indication on every start: a lifetime change can
	// arrive while a previous expiry fade is in progress, and the fade must
	// not be allowed to leave the indication dimmed.
	if err := d.updateAppPresentation(routing.SystemAppID, "", d.micIndicator, presentations.RingEffect("system.ring.indicator", params)); err != nil {
		d.logs.Errorf("daemon", "", "muted indication: %v", err)
		return
	}
	d.micParams = params
	d.micGen++
	if d.micCaptureID == "" {
		cap, err := d.acquireAppCapture(routing.SystemAppID, "", ringID, d.micIndicator.ID, routing.TouchNone)
		if err != nil {
			d.logs.Errorf("daemon", "", "muted indication capture: %v", err)
			return
		}
		d.micCaptureID = cap.ID
		d.logs.Infof("daemon", "", "microphone muted indication active (capture=%s mode=%d)", cap.ID, snap.MutedIndicator)
	}
	d.armMicIndicatorLocked(snap)
}

// armMicIndicatorLocked applies the configured indication lifetime to the
// current mute period. The mode and delay are live settings, so a re-arm
// replaces any pending expiry instead of letting the old timer hide the new
// policy early.
func (d *Daemon) armMicIndicatorLocked(values settings.UserSettings) {
	if d.micTimer != nil {
		d.micTimer.Stop()
		d.micTimer = nil
	}
	switch values.MutedIndicator {
	case settings.MutedImmediate:
		d.releaseMicIndicatorLocked("immediate")
	case settings.MutedUntilUnmute:
		// Keep it until an explicit unmute.
	default:
		delay := time.Duration(values.MutedIndicatorDelayMS) * time.Millisecond
		if delay <= 0 {
			delay = 30 * time.Second
		}
		gen := d.micGen
		var timer *time.Timer
		timer = time.AfterFunc(delay, func() { d.expireMicIndicator(timer, gen) })
		d.micTimer = timer
	}
}

// expireMicIndicator fades an indication only when its timer still owns the
// current mute period. A callback that was already running when a settings
// update re-armed the lifetime must not hide the fresh indication.
func (d *Daemon) expireMicIndicator(timer *time.Timer, gen uint64) {
	d.indicatorMu.Lock()
	current := d.micTimer == timer && d.micCaptureID != "" && d.micGen == gen
	d.indicatorMu.Unlock()
	if !current {
		return
	}
	d.fadeOutMicIndicatorGen(gen, "expired")
}

// fadeOutMicIndicatorGen tapers the indication to black before releasing the
// capture. The generation counter aborts a fade that races a fresh mute: each
// step re-checks the generation and writes while holding the indicator lock, so
// a fresh mute either aborts the fade before the frame or restores full
// intensity after it; an in-flight step can never leave the indication dimmed.
func (d *Daemon) fadeOutMicIndicatorGen(gen uint64, reason string) {
	d.indicatorMu.Lock()
	if d.micCaptureID == "" || d.micIndicator == nil || d.micGen != gen {
		d.indicatorMu.Unlock()
		return
	}
	p := d.micIndicator
	base := cloneParams(d.micParams)
	d.indicatorMu.Unlock()

	const steps = 6
	for i := 1; i <= steps; i++ {
		d.indicatorMu.Lock()
		if d.micCaptureID == "" || d.micIndicator == nil || d.micGen != gen {
			d.indicatorMu.Unlock()
			return
		}
		intensity, _ := base["intensity"].(float64)
		params := cloneParams(base)
		params["intensity"] = intensity * (1 - float64(i)/steps)
		params["fade_in"] = 0.0
		_ = d.updateAppPresentation(routing.SystemAppID, "", p, presentations.RingEffect("system.ring.indicator", params))
		d.indicatorMu.Unlock()
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
	d.releaseAppCapture(routing.SystemAppID, "", d.micCaptureID)
	d.micCaptureID = ""
	if d.micIndicator != nil {
		_ = d.updateAppPresentation(routing.SystemAppID, "", d.micIndicator, presentations.Blank())
		d.closeAppPresentation(routing.SystemAppID, "", d.micIndicator)
		d.micIndicator = nil
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
	if err := d.applyMasterVolume(next); err != nil {
		d.logs.Warnf("daemon", "", "could not update master volume: %v", err)
		return false
	}
	d.logs.Infof("daemon", "", "master volume %d%%", pct)
	// Apply before the cue so the tick is heard at the new level, then show
	// the percentage for a moment. During a fast ramp the cue is throttled so
	// steps do not turn into a buzz; the last step of a tap always plays.
	if d.cueAllowed() {
		d.playVolumeCue()
	}
	d.showVolumeOSD(pct)
	return changed
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
	if err := d.applyMicrophoneMute(muted); err != nil {
		d.logs.Warnf("daemon", "", "could not update microphone mute: %v", err)
		return
	}
	d.logs.Infof("daemon", "", "microphone muted=%v", muted)
}

// updateSystemRing reasserts the ring app's baseline after lifecycle changes.
// The app owns its content and reasserts it only when the rendered signature
// changed; explicit captures still take priority while held.
func (d *Daemon) updateSystemRing() {
	if d.builtins != nil && d.builtins.ring != nil {
		d.builtins.ring.reassert()
	}
}
