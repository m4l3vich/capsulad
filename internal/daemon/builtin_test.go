package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	"capsulad/internal/backend"
	"capsulad/internal/dcron"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/ringpalette"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
	"capsulad/internal/supervisor"
)

// startEmptyDaemon runs the daemon against an application directory with no
// installed packages. The compiled-in clock and ring apps must still be
// registered, running and selected.
func startEmptyDaemon(t *testing.T, base string) *Daemon {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = base
	cfg.RunDir = filepath.Join(base, "run")
	cfg.SocketPath = filepath.Join(cfg.RunDir, "hal.sock")
	cfg.AppsDir = filepath.Join(base, "apps")
	cfg.AssetsDir = filepath.Join(base, "assets")
	cfg.WebUIAddr = ""
	cfg.SandboxMode = "none"
	cfg.NativeLibDir = nativeLibDir(t)
	if err := os.MkdirAll(cfg.AppsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("daemon shutdown timed out")
		}
	})
	return d
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "capsulad-builtin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

func waitForAppSelection(t *testing.T, d *Daemon, resourceID, appID string) {
	t.Helper()
	waitFor(t, 5*time.Second, appID+" selected on "+resourceID, func() bool {
		return d.router.Current(resourceID).SelectedAppID == appID
	})
}

func TestBuiltinSystemAppsWithoutInstalledPackages(t *testing.T) {
	base := shortTempDir(t)
	d := startEmptyDaemon(t, base)

	for _, id := range []string{clockAppID, ringAppID} {
		app, ok := d.reg.Get(id)
		if !ok {
			t.Fatalf("system app %s is not registered", id)
		}
		if !d.reg.IsBuiltin(id) {
			t.Fatalf("system app %s is not marked built-in", id)
		}
		if len(app.Interfaces) == 0 {
			t.Fatalf("system app %s does not publish its public contract", id)
		}
	}
	if app, _ := d.reg.Get(clockAppID); !hasProvide(app.Manifest.Provides, "segment_display_provider") {
		t.Fatal("clock app does not provide the segment display")
	}
	if app, _ := d.reg.Get(ringAppID); !hasProvide(app.Manifest.Provides, "led_ring_controller") {
		t.Fatal("ring app does not provide the ring")
	}

	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	waitFor(t, 5*time.Second, "system app runs ready", func() bool {
		for _, id := range []string{clockAppID, ringAppID} {
			runs := d.sup.List(id)
			if len(runs) != 1 || !runs[0].Virtual || runs[0].State != supervisor.Ready {
				return false
			}
			if runs[0].RunID != d.SystemAppRunID(id) {
				return false
			}
		}
		return true
	})

	for _, resourceID := range []string{"display.segment.front", "light.ring.main"} {
		if sel := d.router.Current(resourceID); sel.SelectedAppID == "capsula.builtin" {
			t.Fatalf("hidden builtin baseline still owns %s", resourceID)
		}
	}
	segSel := d.router.Current("display.segment.front")
	seg, ok := d.pres.Get(segSel.SelectedPresentation)
	if !ok {
		t.Fatal("clock presentation disappeared")
	}
	if kind := seg.ContentKind(); kind != presentations.ContentSegmentFrame {
		t.Fatalf("clock baseline content = %v", kind)
	}
	ringSel := d.router.Current("light.ring.main")
	ring, ok := d.pres.Get(ringSel.SelectedPresentation)
	if !ok {
		t.Fatal("ring presentation disappeared")
	}
	if kind := ring.ContentKind(); kind != presentations.ContentRingEffect {
		t.Fatalf("ring baseline content = %v", kind)
	}
}

func TestBuiltinBaselineRestoredAfterCaptureRelease(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)

	external := d.pres.Open("light.ring.main", "test.external", "run-external", presentations.Ring)
	if err := d.pres.Update(external, presentations.RingFrame([]backend.RGB{{}, {}, {}})); err != nil {
		t.Fatal(err)
	}
	cap, err := d.router.Acquire("light.ring.main", "test.external", "run-external", external.ID, routing.TouchNone)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "external capture selected", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == "test.external"
	})

	if !d.router.ForceRelease(cap.ID) {
		t.Fatal("capture was not released")
	}
	waitFor(t, 5*time.Second, "ring app restored", func() bool {
		sel := d.router.Current("light.ring.main")
		return sel.SelectedAppID == ringAppID && sel.SelectedCaptureID == ""
	})
	d.pres.Close(external)
}

func TestRingEffectsMapSettings(t *testing.T) {
	mustGradient := func(text string) ringpalette.Gradient {
		t.Helper()
		g, err := ringpalette.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	ocean := mustGradient("linear-gradient(90deg, #00B4FF 0%, #0046D2 100%)")
	r := &builtinRing{effect: "breathe", palette: ocean.Format(), gradient: ocean, brightness: 0.5, speed: 1}
	id, params, sig := r.contentLocked()
	if id != "system.ring.breathe" {
		t.Fatalf("breathe mapped to %q", id)
	}
	if params["color"] == (backend.RGB{}) {
		t.Fatal("palette color was not applied")
	}
	firstSig := sig

	sunset := mustGradient("linear-gradient(90deg, #FF7828 0%, #FF3C96 100%)")
	r.effect = "comet"
	r.palette = sunset.Format()
	r.gradient = sunset
	id, params, sig = r.contentLocked()
	if id != "system.ring.comet" {
		t.Fatalf("comet mapped to %q", id)
	}
	if sig == firstSig {
		t.Fatal("effect change produced the same content signature")
	}
	if params["tail_color"] == params["color"] {
		t.Fatal("comet head and tail use the same palette color")
	}

	mono := mustGradient("linear-gradient(90deg, #FFFFFF 0%, #969696 100%)")
	r.effect = "rainbow"
	r.palette = mono.Format()
	r.gradient = mono
	id, params, _ = r.contentLocked()
	if id != "system.ring.rainbow" {
		t.Fatalf("rainbow mapped to %q", id)
	}
	for _, key := range []string{"color", "color_b", "color_c"} {
		c, ok := params[key].(backend.RGB)
		if !ok || c.R != c.G || c.G != c.B {
			t.Fatalf("mono rainbow %s = %#v, want a grayscale palette colour", key, params[key])
		}
	}

	if _, ok := presets.Lookup("system.ring.sparkle"); !ok {
		t.Fatal("sparkle preset is not in the catalog")
	}
	aurora := mustGradient("linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)")
	r.effect = "sparkle"
	r.palette = aurora.Format()
	r.gradient = aurora
	r.brightness = 1
	id, _, _ = r.contentLocked()
	if id != "system.ring.sparkle" {
		t.Fatalf("sparkle mapped to %q", id)
	}
}

func TestClockDisplayBlinkAndTimezone(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	at := time.Date(2026, 11, 12, 12, 34, 56, 0, loc)
	display := clockDisplayFor(at, true)
	if !display.colon {
		t.Fatal("even second with blink enabled must light the colon")
	}
	odd := at.Add(time.Second)
	if clockDisplayFor(odd, true).colon {
		t.Fatal("odd second with blink enabled must darken the colon")
	}
	if !clockDisplayFor(odd, false).colon {
		t.Fatal("blink disabled must keep the colon lit")
	}
	want := [4]uint16{clockDigitMasks[1], clockDigitMasks[2], clockDigitMasks[3], clockDigitMasks[4]}
	if display.cells != want {
		t.Fatalf("cells = %v, want %v", display.cells, want)
	}
	if got := durationDisplay(65_000_000_000, true); got.cells != [4]uint16{clockDigitMasks[0], clockDigitMasks[1], clockDigitMasks[0], clockDigitMasks[5]} {
		t.Fatalf("mm:ss display = %v", got.cells)
	}
}

// testClockTimes installs an atomic wall/monotonic source so service calls and
// the app ticker agree on a controllable instant.
func testClockTimes(t *testing.T, d *Daemon) (app *builtinClock, advance func(time.Duration)) {
	t.Helper()
	waitFor(t, 5*time.Second, "clock app running", func() bool {
		if d.builtins == nil || d.builtins.clock == nil {
			return false
		}
		app = d.builtins.clock
		app.mu.Lock()
		defer app.mu.Unlock()
		// Wait for the initial settings apply so a later watcher snapshot
		// cannot overwrite the alarm schedule installed by the test.
		return app.running && app.settingsLoaded
	})
	var wall atomic.Int64
	var mono atomic.Int64
	wall.Store(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC).UnixMilli())
	mono.Store(1_000_000_000)
	app.mu.Lock()
	app.now = func() time.Time { return time.UnixMilli(wall.Load()).UTC() }
	app.mono = func() uint64 { return uint64(mono.Load()) }
	// Alarm schedules are evaluated in the device zone. The simulated wall
	// clock is UTC, so pin the device zone to make firing deterministic on
	// hosts with any time.Local.
	app.deviceTZ = "UTC"
	app.loc = time.UTC
	app.mu.Unlock()
	return app, func(step time.Duration) {
		wall.Add(step.Milliseconds())
		mono.Add(step.Nanoseconds())
	}
}

// setTestAlarms applies the canonical alarms setting synchronously, without
// waiting for the app-settings watcher goroutine.
func setTestAlarms(t *testing.T, app *builtinClock, exprs ...string) {
	t.Helper()
	raw, err := json.Marshal(dcron.FormatList(exprs))
	if err != nil {
		t.Fatal(err)
	}
	app.applySettings(0, []settings.AppSettingView{{Key: "alarms", Value: string(raw)}})
}

// setTestAlarmEntries applies canonical entries (including !once / @zone /
// !disabled) synchronously, without waiting for the settings watcher.
func setTestAlarmEntries(t *testing.T, app *builtinClock, entries ...dcron.Entry) {
	t.Helper()
	raw, err := json.Marshal(dcron.FormatEntries(entries))
	if err != nil {
		t.Fatal(err)
	}
	app.applySettings(0, []settings.AppSettingView{{Key: "alarms", Value: string(raw)}})
}

func TestBuiltinClockStopwatchSemantics(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	info := app.stopwatchCommand("StopwatchStart")
	if info.Status != clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING {
		t.Fatalf("state = %v", info.Status)
	}
	advance(2500 * time.Millisecond)
	info = app.stopwatchCommand("StopwatchPause")
	if info.Status != clockv1.StopwatchStatus_STOPWATCH_STATUS_PAUSED || info.ElapsedMs != 2500 {
		t.Fatalf("paused = %+v", info)
	}
	advance(5 * time.Second)
	info = app.stopwatchCommand("StopwatchResume")
	if info.Status != clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING || info.ElapsedMs != 2500 {
		t.Fatalf("resumed = %+v", info)
	}
	advance(500 * time.Millisecond)
	state := app.state()
	if state.Stopwatch.ElapsedMs != 3000 {
		t.Fatalf("elapsed = %d, want 3000", state.Stopwatch.ElapsedMs)
	}
	info = app.stopwatchCommand("StopwatchReset")
	if info.Status != clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE || info.ElapsedMs != 0 {
		t.Fatalf("reset = %+v", info)
	}
}

func TestBuiltinClockTimerCompletionAndCancel(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	started := app.startTimer(5000, "Tea")
	if started.Status != clockv1.TimerStatus_TIMER_STATUS_RUNNING {
		t.Fatalf("timer = %+v", started)
	}
	advance(5 * time.Second)
	app.tick()

	state := app.state()
	if len(state.Timers) != 1 || state.Timers[0].Status != clockv1.TimerStatus_TIMER_STATUS_COMPLETED {
		t.Fatalf("timers after completion = %+v", state.Timers)
	}
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].Kind != clockv1.AlertKind_ALERT_KIND_TIMER {
		t.Fatalf("alerts = %+v", state.ActiveAlerts)
	}
	if sel := d.router.Current("display.segment.front"); sel.SelectedAppID != clockAppID || sel.SelectedCaptureID == "" {
		t.Fatalf("timer alert did not take a normal display capture: %+v", sel)
	}
	if !app.dismissAlert(state.ActiveAlerts[0].AlertId) {
		t.Fatal("timer alert was not dismissed")
	}
	if got := app.state(); len(got.Timers) != 0 || len(got.ActiveAlerts) != 0 {
		t.Fatalf("state after dismiss = %+v", got)
	}
	if sel := d.router.Current("display.segment.front"); sel.SelectedCaptureID != "" {
		t.Fatalf("alert capture was not released: %+v", sel)
	}

	running := app.startTimer(60_000, "Cancel me")
	cancelled, err := app.cancelTimer(running.TimerId)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != clockv1.TimerStatus_TIMER_STATUS_CANCELLED || cancelled.RemainingMs != 0 {
		t.Fatalf("cancelled timer = %+v", cancelled)
	}
	if _, err := app.cancelTimer(running.TimerId); status.Code(err) != codes.NotFound {
		t.Fatalf("second cancel code = %v", status.Code(err))
	}
}

func TestAlarmScheduleHandlesWeekdaysAndDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	// Thursday 2026-11-12 06:00 EST.
	now := time.Date(2026, 11, 12, 6, 0, 0, 0, ny)
	daily, err := dcron.Parse("0 7 * * *")
	if err != nil {
		t.Fatal(err)
	}
	next := daily.Next(now, ny)
	if !next.Equal(time.Date(2026, 11, 12, 7, 0, 0, 0, ny)) {
		t.Fatalf("daily next = %v", next)
	}

	// Friday is 5 in five-field dcron (0 and 7 are Sunday).
	weekly, err := dcron.Parse("0 7 * * 5")
	if err != nil {
		t.Fatal(err)
	}
	next = weekly.Next(now, ny)
	if next.Weekday() != time.Friday {
		t.Fatalf("weekly next weekday = %v", next.Weekday())
	}
	if !next.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, ny)) {
		t.Fatalf("weekly next = %v", next)
	}

	// 02:30 does not exist on 2026-03-08 in New York. The next matching wall
	// time is the following day rather than a nonexistent instant.
	gap, err := dcron.Parse("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	gapNow := time.Date(2026, 3, 8, 1, 0, 0, 0, ny)
	next = gap.Next(gapNow, ny)
	if next.IsZero() || !next.After(gapNow) {
		t.Fatalf("gap next = %v", next)
	}
	if next.Hour() != 2 || next.Minute() != 30 {
		t.Fatalf("gap next = %v, want 02:30 on the next real day", next)
	}

	// A weekday list matches only the listed days.
	listed, err := dcron.Parse("0 7 * * 1,3,5")
	if err != nil {
		t.Fatal(err)
	}
	if !listed.Matches(time.Date(2026, 11, 13, 7, 0, 0, 0, ny), ny) {
		t.Fatal("Friday did not match a Mon/Wed/Fri list")
	}
	if listed.Matches(time.Date(2026, 11, 12, 7, 0, 0, 0, ny), ny) {
		t.Fatal("Thursday matched a Mon/Wed/Fri list")
	}
	if listed.Matches(time.Date(2026, 11, 14, 7, 0, 0, 0, ny), ny) {
		t.Fatal("Saturday matched a Mon/Wed/Fri list")
	}
}

func TestBuiltinClockAlarmTriggerAndDismiss(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	setTestAlarms(t, app, "1 9 * * *")
	alarms := app.listAlarms().GetAlarms()
	if len(alarms) != 1 || alarms[0].GetNextTriggerUnixMs() == 0 {
		t.Fatalf("alarms = %+v", alarms)
	}
	advance(time.Minute)
	app.tick()

	state := app.state()
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].Kind != clockv1.AlertKind_ALERT_KIND_ALARM {
		t.Fatalf("alarm alert = %+v", state.ActiveAlerts)
	}
	if state.GetAlarms()[0].GetRinging() == false || state.GetAlarms()[0].GetAlertId() == "" {
		t.Fatalf("alarm did not report its ringing alert: %+v", state.GetAlarms())
	}
	// A dcron alarm is recurring: firing does not disable it.
	if state.GetAlarms()[0].GetNextTriggerUnixMs() == 0 {
		t.Fatal("recurring alarm lost its next trigger")
	}
	// The same minute must not queue a second alert across ticks.
	app.tick()
	if got := app.state(); len(got.ActiveAlerts) != 1 {
		t.Fatalf("ticks duplicated the alarm alert: %+v", got.ActiveAlerts)
	}
	if !app.dismissAlert("") {
		t.Fatal("alarm alert was not dismissed")
	}
	if got := app.state(); len(got.ActiveAlerts) != 0 || got.GetAlarms()[0].GetRinging() {
		t.Fatalf("state after dismiss = %+v", got)
	}
}

func TestBuiltinPublicContractState(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitFor(t, 5*time.Second, "clock app running", func() bool {
		if d.builtins == nil || d.builtins.clock == nil {
			return false
		}
		d.builtins.clock.mu.Lock()
		defer d.builtins.clock.mu.Unlock()
		return d.builtins.clock.running
	})
	var payloads [][]byte
	emit := func(payload []byte) error {
		payloads = append(payloads, payload)
		return nil
	}
	ctx := context.Background()
	if err := d.builtins.clock.handleCall(ctx, clockServiceName, "GetState", nil, emit); err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if err := d.builtins.clock.handleCall(ctx, "capsula.internal.clock.v1.ClockService", "GetState", nil, emit); status.Code(err) != codes.Unimplemented {
		t.Fatalf("retired private ClockService name code = %v, want Unimplemented", status.Code(err))
	}
	if len(payloads) != 1 {
		t.Fatalf("GetState payload count = %d, want 1", len(payloads))
	}
	var got clockv1.ClockState
	if err := proto.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("GetState is not binary protobuf: %v", err)
	}
	if got.GetLocalTimeRfc3339() == "" || got.GetTimezone() == "" {
		t.Fatalf("GetState payload = %+v", &got)
	}
	// An empty request message is a zero duration; a protojson body is not the
	// public wire and must be rejected.
	if err := d.builtins.clock.handleCall(ctx, clockServiceName, "StartTimer", nil, emit); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty StartTimer code = %v", status.Code(err))
	}
	if err := d.builtins.clock.handleCall(ctx, clockServiceName, "StartTimer", []byte(`{}`), emit); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("JSON StartTimer code = %v", status.Code(err))
	}

	// A non-empty binary request must decode and reach the real handler. The
	// device defect was exactly this path: a JSON-only decoder rejected it.
	payloads = nil
	startReq, err := proto.Marshal(&clockv1.StartTimerRequest{DurationMs: 2000, Label: "tea"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.builtins.clock.handleCall(ctx, clockServiceName, "StartTimer", startReq, emit); err != nil {
		t.Fatalf("binary StartTimer: %v", err)
	}
	var timer clockv1.Timer
	if len(payloads) != 1 || proto.Unmarshal(payloads[0], &timer) != nil {
		t.Fatalf("StartTimer payload = %v", payloads)
	}
	if timer.GetDurationMs() != 2000 || timer.GetLabel() != "tea" || timer.GetStatus() != clockv1.TimerStatus_TIMER_STATUS_RUNNING {
		t.Fatalf("StartTimer response = %+v", &timer)
	}
	cancelReq, err := proto.Marshal(&clockv1.CancelTimerRequest{TimerId: timer.GetTimerId()})
	if err != nil {
		t.Fatal(err)
	}
	payloads = nil
	if err := d.builtins.clock.handleCall(ctx, clockServiceName, "CancelTimer", cancelReq, emit); err != nil {
		t.Fatalf("binary CancelTimer: %v", err)
	}
	var cancelled clockv1.Timer
	if len(payloads) != 1 || proto.Unmarshal(payloads[0], &cancelled) != nil {
		t.Fatalf("CancelTimer payload = %v", payloads)
	}
	if cancelled.GetStatus() != clockv1.TimerStatus_TIMER_STATUS_CANCELLED {
		t.Fatalf("CancelTimer response = %+v", &cancelled)
	}

	// WatchState must emit its snapshot before any transition arrives.
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watched := make(chan []byte, 4)
	go func() {
		_ = d.builtins.clock.handleCall(watchCtx, clockServiceName, "WatchState", nil,
			func(payload []byte) error {
				select {
				case watched <- payload:
				default:
				}
				return nil
			})
	}()
	select {
	case payload := <-watched:
		var state clockv1.ClockState
		if err := proto.Unmarshal(payload, &state); err != nil {
			t.Fatalf("watch snapshot is not binary protobuf: %v", err)
		}
		if state.GetTimezone() == "" || state.GetRevision() == 0 {
			t.Fatalf("watch snapshot = %+v", &state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchState did not emit an initial snapshot")
	}

	waitFor(t, 5*time.Second, "ring app running", func() bool {
		if d.builtins == nil || d.builtins.ring == nil {
			return false
		}
		d.builtins.ring.mu.Lock()
		defer d.builtins.ring.mu.Unlock()
		return d.builtins.ring.running
	})
	payloads = nil
	if err := d.builtins.ring.handleCall(ctx, ringServiceName, "GetState", nil, emit); err != nil {
		t.Fatalf("ring GetState: %v", err)
	}
	if err := d.builtins.ring.handleCall(ctx, "capsula.internal.ring.v1.RingService", "GetState", nil, emit); status.Code(err) != codes.Unimplemented {
		t.Fatalf("retired private RingService name code = %v, want Unimplemented", status.Code(err))
	}
	var ring ringv1.RingState
	if err := proto.Unmarshal(payloads[0], &ring); err != nil {
		t.Fatalf("ring state is not binary protobuf: %v", err)
	}
	if ring.GetEffect() == "" || !strings.HasPrefix(ring.GetPalette(), "linear-gradient(") {
		t.Fatalf("ring state = %+v", &ring)
	}
}
