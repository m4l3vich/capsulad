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

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
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
			t.Fatalf("system app %s publishes no interfaces", id)
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

func TestBuiltinClockMigratesLegacySettings(t *testing.T) {
	base := shortTempDir(t)
	settingsPath := filepath.Join(base, "settings.json")
	legacy := `{
  "revision": 9,
  "values": {
    "default_app_by_resource": {
      "display.segment.front": "org.capsula.clock",
      "light.ring.main": "org.capsula.clock"
    },
    "default_assistant_app_id": "org.capsula.assistant",
    "wakeword_enabled": false,
    "master_volume": 0.6
  }
}`
	if err := os.WriteFile(settingsPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyClock := filepath.Join(base, "app-settings", "org.capsula.clock", "settings.json")
	if err := os.MkdirAll(filepath.Dir(legacyClock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyClock, []byte(`{
  "revision": 4,
  "values": {"blink": false, "timezone": "Asia/Shanghai"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyRing := filepath.Join(base, "app-settings", "capsula.ring", "settings.json")
	if err := os.MkdirAll(filepath.Dir(legacyRing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyRing, []byte(`{
  "revision": 2,
  "values": {"mode": "rainbow"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	v := d.user.Get().Values
	if got := v.DefaultAppByResource["display.segment.front"]; got != clockAppID {
		t.Fatalf("segment default = %q, want %q", got, clockAppID)
	}
	if got := v.DefaultAppByResource["light.ring.main"]; got != ringAppID {
		t.Fatalf("ring default = %q, want %q", got, ringAppID)
	}
	if v.DefaultAssistantAppID != "" {
		t.Fatalf("legacy assistant default survived: %q", v.DefaultAssistantAppID)
	}

	snap, err := d.appCfg.Snapshot(builtinClockSettingsID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range snap.Values {
		got[v.Key] = v.Value
	}
	if got["blink"] != "false" || got["timezone"] != `"Asia/Shanghai"` {
		t.Fatalf("clock settings were not migrated: %v", got)
	}
	ringSnap, err := d.appCfg.Snapshot(builtinRingSettingsID)
	if err != nil {
		t.Fatal(err)
	}
	ringValues := map[string]string{}
	for _, v := range ringSnap.Values {
		ringValues[v.Key] = v.Value
	}
	if ringValues["effect"] != `"rainbow"` {
		t.Fatalf("ring mode was not migrated: %v", ringValues)
	}
	rawRing, err := os.ReadFile(legacyRing)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(rawRing, &stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Values["mode"]; ok {
		t.Fatal("obsolete ring mode alias was not removed")
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
}

func TestRingEffectsMapSettings(t *testing.T) {
	r := &builtinRing{effect: "breathe", palette: "ocean", brightness: 0.5, speed: 1}
	id, params, sig := r.contentLocked()
	if id != "system.ring.breathe" {
		t.Fatalf("breathe mapped to %q", id)
	}
	if params["color"] == (backend.RGB{}) {
		t.Fatal("palette color was not applied")
	}
	firstSig := sig

	r.effect = "comet"
	r.palette = "sunset"
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

	r.effect = "rainbow"
	r.palette = "mono"
	id, params, _ = r.contentLocked()
	if id != "system.ring.rainbow" {
		t.Fatalf("rainbow mapped to %q", id)
	}
	if sat, _ := params["saturation"].(float64); sat != 0 {
		t.Fatalf("mono rainbow saturation = %v, want 0", sat)
	}

	if _, ok := presets.Lookup("system.ring.sparkle"); !ok {
		t.Fatal("sparkle preset is not in the catalog")
	}
	r.effect = "sparkle"
	r.palette = "aurora"
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
		return app.running
	})
	var wall atomic.Int64
	var mono atomic.Int64
	wall.Store(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC).UnixMilli())
	mono.Store(1_000_000_000)
	app.mu.Lock()
	app.now = func() time.Time { return time.UnixMilli(wall.Load()).UTC() }
	app.mono = func() uint64 { return uint64(mono.Load()) }
	app.mu.Unlock()
	return app, func(step time.Duration) {
		wall.Add(step.Milliseconds())
		mono.Add(step.Nanoseconds())
	}
}

func TestClockServiceStopwatchSemantics(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	info := app.stopwatchCommand("StopwatchStart")
	if info.State != clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING {
		t.Fatalf("state = %v", info.State)
	}
	advance(2500 * time.Millisecond)
	info = app.stopwatchCommand("StopwatchPause")
	if info.State != clockv1.StopwatchStatus_STOPWATCH_STATUS_PAUSED || info.ElapsedMs != 2500 {
		t.Fatalf("paused = %+v", info)
	}
	advance(5 * time.Second)
	info = app.stopwatchCommand("StopwatchResume")
	if info.State != clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING || info.ElapsedMs != 2500 {
		t.Fatalf("resumed = %+v", info)
	}
	advance(500 * time.Millisecond)
	state := app.state()
	if state.Stopwatch.ElapsedMs != 3000 {
		t.Fatalf("elapsed = %d, want 3000", state.Stopwatch.ElapsedMs)
	}
	info = app.stopwatchCommand("StopwatchReset")
	if info.State != clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE || info.ElapsedMs != 0 {
		t.Fatalf("reset = %+v", info)
	}
}

func TestClockServiceTimerCompletionAndCancel(t *testing.T) {
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

func TestAlarmNextTriggerHandlesWeekdaysAndDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	// Thursday 2026-11-12 06:00 EST.
	now := time.Date(2026, 11, 12, 6, 0, 0, 0, ny)
	daily := &clockAlarm{hour: 7, minute: 0, enabled: true}
	next := nextAlarmTrigger(daily, now, ny)
	if !next.Equal(time.Date(2026, 11, 12, 7, 0, 0, 0, ny)) {
		t.Fatalf("daily next = %v", next)
	}

	fridayBit := uint8(1 << 4) // bit 0 Monday, so Friday is bit 4
	weekly := &clockAlarm{hour: 7, minute: 0, enabled: true, days: fridayBit}
	next = nextAlarmTrigger(weekly, now, ny)
	if next.Weekday() != time.Friday {
		t.Fatalf("weekly next weekday = %v", next.Weekday())
	}
	if !next.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, ny)) {
		t.Fatalf("weekly next = %v", next)
	}

	// 02:30 does not exist on 2026-03-08 in New York. The alarm must still fire
	// at the first valid instant after the gap instead of being skipped.
	gapNow := time.Date(2026, 3, 8, 1, 0, 0, 0, ny)
	gap := &clockAlarm{hour: 2, minute: 30, enabled: true}
	next = nextAlarmTrigger(gap, gapNow, ny)
	if next.IsZero() || !next.After(gapNow) {
		t.Fatalf("gap next = %v", next)
	}
	if next.Hour() != 3 || next.Minute() != 0 {
		t.Fatalf("gap normalized to %v, want 03:00 (first valid instant)", next)
	}

	// A disabled alarm and a zone-less alarm that cannot load still yield an
	// empty trigger rather than a bogus instant.
	disabled := &clockAlarm{hour: 7, enabled: false}
	if !nextAlarmTrigger(disabled, now, ny).IsZero() {
		t.Fatal("disabled alarm returned a trigger")
	}
	if !nextAlarmTrigger(daily, now, nil).IsZero() {
		t.Fatal("nil location returned a trigger")
	}

	// A zone-specific alarm is computed in that zone, not the caller's.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	alarm := &clockAlarm{hour: 9, minute: 0, enabled: true, tz: "Asia/Tokyo"}
	next = nextAlarmTrigger(alarm, now, tokyo)
	if got := next.In(tokyo).Format("15:04"); got != "09:00" {
		t.Fatalf("Tokyo trigger = %s", got)
	}
}

func TestClockServiceAlarmTriggerAndDisable(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	base := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	info, err := app.setAlarm(&clockv1.SetAlarmRequest{
		AlarmId: "svc", Label: "Tea", Hour: uint32(base.Hour()), Minute: uint32(base.Minute() + 1),
		DaysOfWeek: 0, Timezone: "UTC", Enabled: true,
	})
	if err != nil {
		t.Fatalf("set alarm: %v", err)
	}
	if info.NextTriggerUnixMs == 0 {
		t.Fatal("alarm has no next trigger")
	}
	advance(2 * time.Minute)
	app.tick()

	state := app.state()
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].Kind != clockv1.AlertKind_ALERT_KIND_ALARM {
		t.Fatalf("alarm alert = %+v", state.ActiveAlerts)
	}
	app.mu.Lock()
	alarm := app.alarms["svc"]
	fired := alarm.lastTriggeredMS != 0
	enabled := alarm.enabled
	app.mu.Unlock()
	if !fired || enabled {
		t.Fatalf("one-shot alarm fired=%v enabled=%v", fired, enabled)
	}
	if _, err := app.setAlarmEnabled("svc", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	app.mu.Lock()
	reenabled := app.alarms["svc"].enabled
	app.mu.Unlock()
	if !reenabled {
		t.Fatal("alarm did not re-enable")
	}
}

func TestClockServiceRPCAndRingState(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitFor(t, 5*time.Second, "clock app running", func() bool {
		if d.builtins == nil || d.builtins.clock == nil {
			return false
		}
		d.builtins.clock.mu.Lock()
		defer d.builtins.clock.mu.Unlock()
		return d.builtins.clock.running
	})
	payloads, err := d.builtins.clock.handleCall(context.Background(), clockServiceName, "GetState", nil)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if len(payloads) != 1 || !strings.Contains(string(payloads[0]), "\"local_time_rfc3339\"") {
		t.Fatalf("GetState payload = %s", payloads)
	}
	if _, err := d.builtins.clock.handleCall(context.Background(), clockServiceName, "StartTimer", []byte(`{}`)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty StartTimer code = %v", status.Code(err))
	}

	waitFor(t, 5*time.Second, "ring app running", func() bool {
		if d.builtins == nil || d.builtins.ring == nil {
			return false
		}
		d.builtins.ring.mu.Lock()
		defer d.builtins.ring.mu.Unlock()
		return d.builtins.ring.running
	})
	payloads, err = d.builtins.ring.handleCall(context.Background(), ringServiceName, "GetState", nil)
	if err != nil {
		t.Fatalf("ring GetState: %v", err)
	}
	var ring struct {
		Effect  string `json:"effect"`
		Palette string `json:"palette"`
	}
	if err := json.Unmarshal(payloads[0], &ring); err != nil {
		t.Fatalf("ring state json: %v", err)
	}
	if ring.Effect == "" || ring.Palette == "" {
		t.Fatalf("ring state = %+v", ring)
	}
}

func TestRetiredPackageIsNotLaunched(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "apps", "org.capsula.clock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "launched")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "clock"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `id = "org.capsula.clock"
name = "Old clock"
version = "0.1.0"
background = true
start_on_boot = true
restart_on_failure = true
single_instance = true
provides = ["segment_display_provider"]

[entrypoint]
runtime = "native"
path = "clock"
architecture = "aarch64"
abi = "linux-musl"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	if _, ok := d.reg.Get("org.capsula.clock"); !ok {
		t.Fatal("package was not discovered; the test is not exercising the retired path")
	}
	d.startBootApps()
	d.ensureRunning("org.capsula.clock")
	if _, _, _, err := d.Launch("org.capsula.clock", nil); err == nil {
		t.Fatal("retired package was launchable")
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("retired package was executed")
	}
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
}
