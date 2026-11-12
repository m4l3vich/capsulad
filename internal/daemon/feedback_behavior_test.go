package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

// feedbackTestDaemon builds the routing/presentation surface the framework
// feedback cues use, without starting hardware loops. It is enough to exercise
// overlay ownership and expiry directly.
func feedbackTestDaemon(t *testing.T, infos ...*frameworkv1.ResourceInfo) *Daemon {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	pres := presentations.NewManager(clock, logs, nil)
	router := routing.New(clock, logs, pres)
	d := &Daemon{
		ctx: context.Background(), clock: clock, logs: logs, pres: pres, router: router,
		resources: map[string]*frameworkv1.ResourceInfo{},
	}
	for _, info := range infos {
		d.resources[info.ResourceId] = info
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			router.SetResourceClass(info.ResourceId, "display")
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			router.SetResourceClass(info.ResourceId, "ring")
		}
	}
	return d
}

// TestVolumeOSDSupersededTimerKeepsOverlay covers a volume step that lands
// exactly as the previous overlay expires: the old callback is already
// running when the new timer is installed, so it must not tear down the fresh
// percentage.
func TestVolumeOSDSupersededTimerKeepsOverlay(t *testing.T) {
	const segmentID = "display.segment.front"
	d := feedbackTestDaemon(t, &frameworkv1.ResourceInfo{
		ResourceId: segmentID, Kind: frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY,
	})

	d.showVolumeOSD(35)
	d.osdMu.Lock()
	firstTimer := d.osdTimer
	captureID := d.osdCapture
	overlay := d.osdPres
	d.osdMu.Unlock()
	if firstTimer == nil || captureID == "" || overlay == nil {
		t.Fatalf("volume overlay did not open (timer=%v capture=%q pres=%v)", firstTimer, captureID, overlay)
	}
	if sel := d.router.Current(segmentID); sel.SelectedCaptureID != captureID {
		t.Fatalf("volume overlay is not selected: %+v", sel)
	}

	d.showVolumeOSD(60)
	d.osdMu.Lock()
	secondTimer := d.osdTimer
	d.osdMu.Unlock()
	if secondTimer == nil || secondTimer == firstTimer {
		t.Fatal("a new volume step did not replace the overlay timer")
	}
	// The test drives expiry explicitly so a slow run cannot fire the real
	// timers first; the callbacks are the same ones production installs.
	firstTimer.Stop()
	secondTimer.Stop()

	// The stale callback lost the race with the newer volume step.
	d.expireVolumeOSD(firstTimer)
	d.osdMu.Lock()
	liveCapture, liveOverlay := d.osdCapture, d.osdPres
	d.osdMu.Unlock()
	if liveCapture != captureID || liveOverlay != overlay {
		t.Fatal("a superseded expiry cleared the live volume overlay")
	}
	if sel := d.router.Current(segmentID); sel.SelectedCaptureID != captureID {
		t.Fatalf("superseded expiry released the live capture: %+v", sel)
	}
	cells, colon, ok := overlay.RenderSegment(backend.SegmentCaps{DigitCount: 4, SegmentsPerDigit: 7, SupportsColon: true})
	if !ok || colon || !slices.Equal(cells, volumeCells(60)) {
		t.Fatalf("overlay shows %v/%v, want the newest percentage %v", cells, colon, volumeCells(60))
	}

	// Its own timer still releases the overlay and the capture.
	d.expireVolumeOSD(secondTimer)
	d.osdMu.Lock()
	releasedCapture, releasedOverlay, releasedTimer := d.osdCapture, d.osdPres, d.osdTimer
	d.osdMu.Unlock()
	if releasedCapture != "" || releasedOverlay != nil || releasedTimer != nil {
		t.Fatalf("overlay state survived expiry: capture=%q pres=%v timer=%v", releasedCapture, releasedOverlay, releasedTimer)
	}
	if _, ok := d.pres.Get(overlay.ID); ok {
		t.Fatal("volume overlay presentation survived expiry")
	}
	if sel := d.router.Current(segmentID); sel.SelectedCaptureID != "" {
		t.Fatalf("volume overlay capture survived expiry: %+v", sel)
	}
}

// TestResetAutoLevelsDropsOnlyChangedModes pins the brightness cache
// invalidation: a level sampled in an earlier automatic period must not carry
// over a mode change, while an unchanged automatic resource keeps its fade.
func TestResetAutoLevelsDropsOnlyChangedModes(t *testing.T) {
	const (
		displayID = "display.segment.front"
		ringID    = "light.ring.main"
	)
	d := feedbackTestDaemon(t,
		&frameworkv1.ResourceInfo{ResourceId: displayID, Kind: frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY},
		&frameworkv1.ResourceInfo{ResourceId: ringID, Kind: frameworkv1.ResourceKind_RESOURCE_LED_RING},
	)
	d.autoLvl = map[string]float64{displayID: 0.9, ringID: 0.7}

	prev := &settings.UserSettings{BrightnessMode: map[string]string{ringID: settings.BrightnessAuto}}
	next := &settings.UserSettings{BrightnessMode: map[string]string{
		displayID: settings.BrightnessAuto, ringID: settings.BrightnessAuto,
	}}
	d.resetAutoLevels(prev, next)
	if _, ok := d.autoLvl[displayID]; ok {
		t.Fatal("enabling auto brightness kept the level from the constant period")
	}
	if _, ok := d.autoLvl[ringID]; !ok {
		t.Fatal("unchanged automatic brightness lost its animated level")
	}

	// The first applied snapshot has no predecessor and must not clear state.
	d.autoLvl[displayID] = 0.4
	d.resetAutoLevels(nil, next)
	if _, ok := d.autoLvl[displayID]; !ok {
		t.Fatal("the first settings snapshot cleared the brightness cache")
	}

	// Removing the auto entry is a switch back to a constant brightness.
	d.resetAutoLevels(next, &settings.UserSettings{BrightnessMode: map[string]string{ringID: settings.BrightnessAuto}})
	if _, ok := d.autoLvl[displayID]; ok {
		t.Fatal("switching to constant brightness kept a stale automatic level")
	}
	if _, ok := d.autoLvl[ringID]; !ok {
		t.Fatal("constant-mode display disturbed the automatic ring level")
	}
}

// TestMutedIndicatorLifetimeFollowsSettingsChange covers changing the muted
// indication policy while the microphone stays muted: the new lifetime must
// replace the old one immediately instead of waiting for the next mute toggle.
func TestMutedIndicatorLifetimeFollowsSettingsChange(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)

	setIndicator := func(mode settings.MutedIndicatorMode, delayMS uint32) {
		t.Helper()
		if err := d.writeUserSettings([]string{"muted_microphone_indicator"}, func(v *settings.UserSettings) {
			v.MutedIndicator = mode
			v.MutedIndicatorDelayMS = delayMS
		}); err != nil {
			t.Fatal(err)
		}
	}
	indicatorActive := func() bool {
		d.indicatorMu.Lock()
		defer d.indicatorMu.Unlock()
		return d.micCaptureID != ""
	}
	waitIndicator := func(what string, want bool) {
		t.Helper()
		waitFor(t, 3*time.Second, what, func() bool { return indicatorActive() == want })
	}

	setIndicator(settings.MutedUntilUnmute, 30000)
	d.setMuted(true)
	waitIndicator("muted indication", true)
	if !d.audio.Muted() {
		t.Fatal("muted indication changed the mute state")
	}

	// "Until unmute" -> "after 200 ms" while still muted must hide it.
	setIndicator(settings.MutedDelay, 200)
	waitIndicator("muted indication expires after the newly selected delay", false)
	if !d.audio.Muted() {
		t.Fatal("muted indication expiry changed the mute state")
	}

	// Back to "until unmute": it reappears and must outlive the old timer.
	setIndicator(settings.MutedUntilUnmute, 30000)
	waitIndicator("muted indication returns for the until-unmute policy", true)
	time.Sleep(500 * time.Millisecond)
	if !indicatorActive() {
		t.Fatal("a re-armed until-unmute indication was hidden by a stale expiry timer")
	}

	// A delay applied to an active indication must expire it from now.
	setIndicator(settings.MutedDelay, 300)
	waitIndicator("active indication expires when the delay policy is selected", false)

	d.setMuted(false)
	waitFor(t, 3*time.Second, "unmute releases the indication", func() bool {
		d.indicatorMu.Lock()
		defer d.indicatorMu.Unlock()
		return d.micCaptureID == "" && d.micIndicator == nil
	})
}

// TestRapidMuteToggleKeepsIndication covers unmuting and muting again before
// the unmute fade finishes: the fade belongs to the mute period that ended and
// must not release the indication for the new one.
func TestRapidMuteToggleKeepsIndication(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)

	if err := d.writeUserSettings([]string{"muted_microphone_indicator"}, func(v *settings.UserSettings) {
		v.MutedIndicator = settings.MutedUntilUnmute
		v.MutedIndicatorDelayMS = 30000
	}); err != nil {
		t.Fatal(err)
	}
	indicatorActive := func() bool {
		d.indicatorMu.Lock()
		defer d.indicatorMu.Unlock()
		return d.micCaptureID != ""
	}
	d.setMuted(true)
	waitFor(t, 3*time.Second, "muted indication", indicatorActive)

	d.setMuted(false)
	d.setMuted(true)
	// The unmute fade runs for six 40 ms steps, so a stale fade would have
	// released the capture well before this check.
	time.Sleep(700 * time.Millisecond)
	if !d.audio.Muted() {
		t.Fatal("rapid mute toggle did not leave the microphone muted")
	}
	if !indicatorActive() {
		t.Fatal("the unmute fade released the indication of the next mute period")
	}
}

// TestTimezoneChangeKeepsBuiltinClockState covers a device timezone change:
// the compiled-in clock follows the zone in place, so its running stopwatch
// and timers must survive. The timezone restart path only relaunches external
// apps, whose environment is fixed at launch.
func TestTimezoneChangeKeepsBuiltinClockState(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitFor(t, 5*time.Second, "clock app running", func() bool {
		if d.builtins == nil || d.builtins.clock == nil {
			return false
		}
		d.builtins.clock.mu.Lock()
		defer d.builtins.clock.mu.Unlock()
		return d.builtins.clock.running
	})

	app := d.builtins.clock
	app.stopwatchCommand("StopwatchStart")
	started := app.startTimer(600_000, "tea")
	if started == nil || started.TimerId == "" {
		t.Fatal("clock did not start a timer")
	}
	runID := d.SystemAppRunID(clockAppID)

	if err := d.writeUserSettings([]string{"timezone"}, func(v *settings.UserSettings) {
		v.Timezone = "Asia/Shanghai"
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "clock follows the device timezone", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.deviceTZ == "Asia/Shanghai"
	})

	runs := d.sup.List(clockAppID)
	if len(runs) != 1 || runs[0].RunID != runID || !runs[0].Virtual {
		t.Fatalf("timezone change restarted the built-in clock: %+v", runs)
	}
	app.mu.Lock()
	tmr, ok := app.timers[started.TimerId]
	stopwatchRunning := app.stopwatch.running
	app.mu.Unlock()
	if !ok || tmr.status != timerRunning {
		t.Fatal("timezone change cleared a running timer")
	}
	if !stopwatchRunning {
		t.Fatal("timezone change stopped the stopwatch")
	}
}

// TestRingAppStopStartRestoresBaseline covers the compiled-in ring app being
// stopped and launched again: the new run must reopen and reassert a baseline
// presentation that routing selects, not stay silent after the old one closed.
func TestRingAppStopStartRestoresBaseline(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)

	runID := d.SystemAppRunID(ringAppID)
	if !d.Stop(runID) {
		t.Fatal("ring run was not stopped")
	}
	waitFor(t, 5*time.Second, "ring baseline released", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID != ringAppID
	})

	if _, _, _, err := d.Launch(ringAppID, nil); err != nil {
		t.Fatalf("relaunch ring app: %v", err)
	}
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	waitFor(t, 5*time.Second, "ring baseline presentation ready", func() bool {
		sel := d.router.Current("light.ring.main")
		p, ok := d.pres.Get(sel.SelectedPresentation)
		return ok && p.Ready()
	})
}
