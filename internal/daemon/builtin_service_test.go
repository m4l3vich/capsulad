package daemon

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	"capsulad/internal/dcron"
	"capsulad/internal/settings"
)

func TestClockAddAndDeleteAlarmThroughSettings(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.clock
	waitFor(t, 5*time.Second, "clock app running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running
	})

	alarm, err := app.addAlarm("0 7 * * 1,3,5", "", "", false, false, 0)
	if err != nil {
		t.Fatalf("add alarm: %v", err)
	}
	if alarm.GetSchedule() != "0 7 * * 1,3,5" || alarm.GetNextTriggerUnixMs() == 0 {
		t.Fatalf("alarm = %+v", alarm)
	}
	waitFor(t, 2*time.Second, "alarm applied from settings", func() bool {
		for _, a := range app.state().GetAlarms() {
			if a.GetSchedule() == "0 7 * * 1,3,5" {
				return true
			}
		}
		return false
	})

	// Re-adding the same expression is idempotent.
	if _, err := app.addAlarm("0 7 * * 1,3,5", "", "", false, false, 0); err != nil {
		t.Fatalf("re-add alarm: %v", err)
	}
	if got := len(app.state().GetAlarms()); got != 1 {
		t.Fatalf("alarms after idempotent add = %d, want 1", got)
	}

	// Disabling preserves the configured alarm and its identity.
	disabledAlarm, err := app.setAlarmEnabled("0 7 * * 1,3,5", false, 0)
	if err != nil {
		t.Fatalf("disable alarm: %v", err)
	}
	if disabledAlarm.GetEnabled() || disabledAlarm.GetAlarmId() != "0 7 * * 1,3,5" {
		t.Fatalf("disabled alarm = %+v", disabledAlarm)
	}
	waitFor(t, 2*time.Second, "disabled alarm applied", func() bool {
		for _, a := range app.state().GetAlarms() {
			if a.GetAlarmId() == "0 7 * * 1,3,5" {
				return !a.GetEnabled()
			}
		}
		return false
	})
	reenabled, err := app.setAlarmEnabled("0 7 * * 1,3,5", true, 0)
	if err != nil || !reenabled.GetEnabled() {
		t.Fatalf("re-enable alarm: %+v, %v", reenabled, err)
	}

	if _, err := app.addAlarm("61 7 * * *", "", "", false, false, 0); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid schedule code = %v", status.Code(err))
	}
	rev, err := d.appCfg.Revision(clockAppID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.addAlarm("0 8 * * *", "", "", false, false, rev+5); status.Code(err) != codes.Aborted {
		t.Fatalf("stale add code = %v", status.Code(err))
	}

	if _, err := app.deleteAlarm("0 7 * * 1,3,5", 0); err != nil {
		t.Fatalf("delete alarm: %v", err)
	}
	waitFor(t, 2*time.Second, "alarm removed from settings", func() bool {
		return len(app.state().GetAlarms()) == 0
	})
	if _, err := app.deleteAlarm("0 7 * * 1,3,5", 0); status.Code(err) != codes.NotFound {
		t.Fatalf("second delete code = %v", status.Code(err))
	}
}

func TestRingUpdateSettingsValidatesAndPreservesUnrelatedFields(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.ring
	waitFor(t, 5*time.Second, "ring app running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running
	})

	waitFor(t, 2*time.Second, "ring settings loaded", func() bool {
		return app.settingsSnapshot().GetRevision() > 0
	})
	before := app.settingsSnapshot()
	storeRev, err := d.appCfg.Revision(ringAppID)
	if err != nil {
		t.Fatal(err)
	}
	req := &ringv1.UpdateSettingsRequest{
		ExpectedRevision: storeRev,
		Values: &ringv1.RingSettings{
			Effect:  "static",
			Palette: "#FF0000",
		},
		UpdateFields: []string{"effect", "palette"},
	}
	snap, err := app.updateSettings(req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if snap.GetValues().GetEffect() != "static" || snap.GetValues().GetPalette() != "#FF0000" {
		t.Fatalf("updated = %+v", snap.GetValues())
	}
	if snap.GetRevision() != storeRev+1 {
		t.Fatalf("revision = %d, want %d", snap.GetRevision(), storeRev+1)
	}
	// Unrelated fields keep their previous values.
	if snap.GetValues().GetBrightness() != before.GetValues().GetBrightness() ||
		snap.GetValues().GetScheduleStart() != before.GetValues().GetScheduleStart() {
		t.Fatalf("unrelated settings changed: before %+v after %+v", before.GetValues(), snap.GetValues())
	}
	waitFor(t, 2*time.Second, "static effect applied", func() bool {
		return app.stateSnapshot().GetEffect() == "static"
	})

	staleReq := &ringv1.UpdateSettingsRequest{
		ExpectedRevision: storeRev,
		Values:           &ringv1.RingSettings{Effect: "pulse"},
		UpdateFields:     []string{"effect"},
	}
	if _, err := app.updateSettings(staleReq); status.Code(err) != codes.Aborted {
		t.Fatalf("stale update code = %v", status.Code(err))
	}

	bad := &ringv1.UpdateSettingsRequest{
		ExpectedRevision: 0,
		Values:           &ringv1.RingSettings{Palette: "aurora"},
		UpdateFields:     []string{"palette"},
	}
	if _, err := app.updateSettings(bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid palette code = %v", status.Code(err))
	}
	unknown := &ringv1.UpdateSettingsRequest{
		Values:       &ringv1.RingSettings{},
		UpdateFields: []string{"nope"},
	}
	if _, err := app.updateSettings(unknown); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown field code = %v", status.Code(err))
	}
	// The rejected updates left the accepted values untouched.
	if got := app.settingsSnapshot().GetValues().GetPalette(); got != "#FF0000" {
		t.Fatalf("palette after rejected updates = %q", got)
	}
}

func TestClockStopStartReloadsUnchangedSettings(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.clock
	waitFor(t, 5*time.Second, "clock running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running && app.settingsLoaded
	})
	if _, err := app.addAlarm("0 7 * * *", "", "", false, false, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "alarm visible", func() bool {
		return len(app.listAlarms().GetAlarms()) == 1
	})
	app.stop()
	app.start()
	waitFor(t, 5*time.Second, "clock restarted with settings", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running && app.settingsLoaded
	})
	alarms := app.listAlarms().GetAlarms()
	if len(alarms) != 1 || alarms[0].GetSchedule() != "0 7 * * *" || alarms[0].GetNextTriggerUnixMs() == 0 {
		t.Fatalf("alarms after restart = %+v", alarms)
	}
}

func TestRingStopStartReloadsUnchangedSettings(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.ring
	waitFor(t, 5*time.Second, "ring running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running && app.settingsLoaded
	})
	rev, err := d.appCfg.Revision(ringAppID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.updateSettings(&ringv1.UpdateSettingsRequest{
		ExpectedRevision: rev,
		Values:           &ringv1.RingSettings{Effect: "static"},
		UpdateFields:     []string{"effect"},
	}); err != nil {
		t.Fatal(err)
	}
	app.stop()
	app.start()
	waitFor(t, 5*time.Second, "ring restarted with settings", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running && app.settingsLoaded
	})
	if got := app.stateSnapshot().GetEffect(); got != "static" {
		t.Fatalf("effect after restart = %q", got)
	}
}

func TestExplicitAlarmDisableDismissesAlert(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)
	setTestAlarmEntries(t, app, dcron.Entry{Expr: "1 9 * * *"})
	advance(time.Minute)
	app.tick()
	if got := app.state().GetActiveAlerts(); len(got) != 1 {
		t.Fatalf("alarm did not ring: %+v", got)
	}
	// An explicit settings disable (the friendly editor / generic
	// ApplicationConfiguration path) must dismiss the active alert.
	app.applySettings(0, []settings.AppSettingView{{
		Key: "alarms", Value: `"1 9 * * * !disabled"`,
	}})
	state := app.state()
	if len(state.GetActiveAlerts()) != 0 {
		t.Fatalf("explicit disable left an alert: %+v", state.GetActiveAlerts())
	}
	if alarms := state.GetAlarms(); len(alarms) != 1 || alarms[0].GetEnabled() {
		t.Fatalf("disabled alarm was not retained: %+v", alarms)
	}
}

func TestSetAlarmEnabledStaleFailureKeepsOneShotAlert(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)
	setTestAlarmEntries(t, app, dcron.Entry{Expr: "1 9 * * *", Once: true})
	advance(time.Minute)
	app.tick()
	if got := app.state().GetActiveAlerts(); len(got) != 1 {
		t.Fatalf("one-shot did not ring: %+v", got)
	}
	rev, err := d.appCfg.Revision(clockAppID)
	if err != nil {
		t.Fatal(err)
	}
	// A failed CAS must not clear the self-disabled marker or dismiss the alert.
	if _, err := app.setAlarmEnabled("1 9 * * *", false, rev+9); status.Code(err) != codes.Aborted {
		t.Fatalf("stale enable/disable code = %v", status.Code(err))
	}
	if got := app.state().GetActiveAlerts(); len(got) != 1 {
		t.Fatalf("failed disable dismissed the one-shot alert: %+v", got)
	}
}

func TestFallBackDSTDoesNotRingTwice(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.clock
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	first := time.Date(2026, 11, 1, 1, 30, 0, 0, ny)
	app.stop()
	app.mu.Lock()
	app.deviceTZ = "America/New_York"
	app.loc = ny
	app.alarms = map[string]*clockAlarm{}
	app.alarmOrder = nil
	entry := dcron.Entry{Expr: "30 1 * * *"}
	sched, _ := dcron.Parse(entry.Expr)
	app.alarms[entry.ID()] = &clockAlarm{id: entry.ID(), text: entry.Canonical(), expr: entry.Expr, label: "Alarm", sched: sched}
	app.alarmOrder = []string{entry.ID()}
	app.alerts = map[string]*clockAlert{}
	app.alertOrder = nil
	app.mu.Unlock()
	app.advanceLocked(first, 0)
	app.advanceLocked(first.Add(time.Hour), 0)
	app.mu.Lock()
	count := len(app.alertOrder)
	app.mu.Unlock()
	if count != 1 {
		t.Fatalf("fall-back DST produced %d alerts, want 1", count)
	}
}

func TestClockWatchIsQuietWhenNothingChanged(t *testing.T) {
	c := &builtinClock{
		running: true, loc: time.UTC, watchers: map[int]chan *clockv1.ClockState{},
		alarms: map[string]*clockAlarm{}, timers: map[string]*clockTimer{},
		alerts: map[string]*clockAlert{},
		now:    func() time.Time { return time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC) },
		mono:   func() uint64 { return 1_000_000_000 },
	}
	ch, cancel := c.subscribeState()
	defer cancel()
	select {
	case <-ch: // initial snapshot
	case <-time.After(2 * time.Second):
		t.Fatal("no initial snapshot")
	}
	// First broadcast establishes the signature for this state.
	c.broadcastState()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("changed state did not emit")
	}
	// Repeating a publish with no observable change must not allocate or emit.
	c.broadcastState()
	select {
	case <-ch:
		t.Fatal("unchanged state emitted a redundant snapshot")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRingIgnoresOlderQueuedSettingsSnapshot(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.ring
	waitFor(t, 5*time.Second, "ring running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running && app.settingsLoaded
	})
	rev, err := d.appCfg.Revision(ringAppID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.updateSettings(&ringv1.UpdateSettingsRequest{
		ExpectedRevision: rev,
		Values:           &ringv1.RingSettings{Effect: "static"},
		UpdateFields:     []string{"effect"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := app.stateSnapshot().GetEffect(); got != "static" {
		t.Fatalf("effect after synchronous update = %q", got)
	}
	// A watcher snapshot from before the update must not undo it.
	app.applySettings(rev, []settings.AppSettingView{{Key: "effect", Value: `"breathe"`}})
	if got := app.stateSnapshot().GetEffect(); got != "static" {
		t.Fatalf("older queued snapshot overwrote the newer revision: %q", got)
	}
}

func TestClockIgnoresOlderQueuedSettingsSnapshot(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.clock
	waitFor(t, 5*time.Second, "clock running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running && app.settingsLoaded
	})
	newer := []settings.AppSettingView{{Key: "alarms", Value: `"0 7 * * *"`}}
	app.mu.Lock()
	rev := app.settingsRevision
	app.mu.Unlock()
	app.applySettings(rev+1, newer)
	app.applySettings(rev, []settings.AppSettingView{{Key: "alarms", Value: `"0 9 * * *"`}})
	got := app.listAlarms().GetAlarms()
	if len(got) != 1 || got[0].GetSchedule() != "0 7 * * *" {
		t.Fatalf("older clock snapshot was applied: %+v", got)
	}
}

func TestAlarmAlertDisplayUsesFiredWallTime(t *testing.T) {
	fired := time.Date(2026, 11, 12, 6, 5, 0, 0, time.UTC)
	c := &builtinClock{
		deviceTZ: "UTC", loc: time.UTC,
		alarms: map[string]*clockAlarm{
			"alarm": {id: "alarm", expr: "5 6 * * *", lastTriggeredMS: uint64(fired.UnixMilli())},
		},
	}
	display := c.alertDisplayLocked(&clockAlert{kind: alertAlarm, sourceID: "alarm"},
		time.Date(2026, 11, 12, 23, 59, 0, 0, time.UTC))
	want := [4]uint16{clockDigitMasks[0], clockDigitMasks[6], clockDigitMasks[0], clockDigitMasks[5]}
	if display.cells != want {
		t.Fatalf("alert display = %v, want %v (06:05)", display.cells, want)
	}
}

func TestRingWatchSettingsStartsWithSnapshot(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.ring
	waitFor(t, 5*time.Second, "ring app running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running && app.settingsLoaded
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan *ringv1.RingSettingsSnapshot, 2)
	go func() {
		_ = app.watchSettings(ctx, func(payload []byte) error {
			var snap ringv1.RingSettingsSnapshot
			if err := proto.Unmarshal(payload, &snap); err != nil {
				return err
			}
			got <- &snap
			return nil
		})
	}()
	select {
	case snap := <-got:
		if snap.GetRevision() == 0 || snap.GetValues().GetPalette() == "" {
			t.Fatalf("initial settings snapshot = %+v", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchSettings did not emit an initial snapshot")
	}
	cancel()
}
