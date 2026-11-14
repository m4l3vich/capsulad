package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	"capsulad/internal/backend"
	"capsulad/internal/dcron"
	"capsulad/internal/presets"
	"capsulad/internal/ringpalette"
)

func TestLegacyClockAlarmSettingsMigrateToCron(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "app-settings", clockAppID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"revision": 7,
		"values": map[string]any{
			"blink":          true,
			"alarm_label":    "Wake",
			"alarm_hour":     7,
			"alarm_minute":   30,
			"alarm_days":     10, // Tuesday and Thursday (bit 1 and bit 3).
			"alarm_timezone": "",
			"alarm_enabled":  true,
		},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	snap, err := d.appCfg.Snapshot(clockAppID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, v := range snap.Values {
		values[v.Key] = v.Value
	}
	if _, ok := values["alarm_hour"]; ok {
		t.Fatal("legacy alarm_hour survived migration")
	}
	var got string
	if err := json.Unmarshal([]byte(values["alarms"]), &got); err != nil {
		t.Fatalf("alarms value: %v (%q)", err, values["alarms"])
	}
	exprs, err := dcron.ParseList(got)
	if err != nil || len(exprs) != 1 {
		t.Fatalf("alarms = %q, err %v", got, err)
	}
	// Bit 1 = Tuesday (2), bit 3 = Thursday (4) in five-field dcron.
	if exprs[0] != "30 7 * * 2,4" {
		t.Fatalf("alarms = %q, want 30 7 * * 2,4", exprs[0])
	}
}

func TestLegacyOneShotAndTimezonePreserved(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "app-settings", clockAppID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"revision": 5,
		"values": map[string]any{
			"alarm_hour": 6, "alarm_minute": 30, "alarm_days": 0,
			"alarm_timezone": "Asia/Tokyo", "alarm_enabled": true,
		},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	snap, err := d.appCfg.Snapshot(clockAppID)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, v := range snap.Values {
		if v.Key == "alarms" {
			_ = json.Unmarshal([]byte(v.Value), &text)
		}
	}
	entries, err := dcron.ParseEntries(text)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %q, %v", text, err)
	}
	entry := entries[0]
	if entry.Expr != "30 6 * * *" || entry.Zone != "Asia/Tokyo" || !entry.Once || entry.Disabled {
		t.Fatalf("one-shot entry = %+v", entry)
	}
	waitFor(t, 2*time.Second, "clock app running", func() bool {
		app := d.builtins.clock
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running
	})
	alarms := d.builtins.clock.state().GetAlarms()
	if len(alarms) != 1 || !alarms[0].GetOneShot() || !alarms[0].GetEnabled() ||
		alarms[0].GetTimezone() != "Asia/Tokyo" || alarms[0].GetNextTriggerUnixMs() == 0 {
		t.Fatalf("migrated alarm state = %+v", alarms)
	}
}

func TestLegacyDisabledAlarmPreservedNotDropped(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "app-settings", clockAppID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"revision": 2,
		"values": map[string]any{
			"alarm_hour": 7, "alarm_minute": 0, "alarm_days": 127, "alarm_enabled": false,
		},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	snap, err := d.appCfg.Snapshot(clockAppID)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, v := range snap.Values {
		if v.Key == "alarms" {
			_ = json.Unmarshal([]byte(v.Value), &text)
		}
	}
	entries, err := dcron.ParseEntries(text)
	if err != nil || len(entries) != 1 || !entries[0].Disabled || entries[0].Once {
		t.Fatalf("disabled entry = %#v (%q, err %v)", entries, text, err)
	}
}

func TestOneShotFiresOnceAndStaysDisabled(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)
	setTestAlarmEntries(t, app, dcron.Entry{Expr: "1 9 * * *", Once: true})
	alarms := app.listAlarms().GetAlarms()
	if len(alarms) != 1 || !alarms[0].GetOneShot() || !alarms[0].GetEnabled() {
		t.Fatalf("alarms = %+v", alarms)
	}
	advance(time.Minute)
	app.tick()
	state := app.state()
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].Kind != clockv1.AlertKind_ALERT_KIND_ALARM {
		t.Fatalf("one-shot alert = %+v", state.ActiveAlerts)
	}
	if a := state.GetAlarms(); len(a) != 1 || a[0].GetEnabled() || a[0].GetNextTriggerUnixMs() != 0 || a[0].GetRinging() == false {
		t.Fatalf("one-shot after firing = %+v", state.GetAlarms())
	}
	// The alert still waits for an explicit dismissal.
	if !app.dismissAlert("") {
		t.Fatal("one-shot alert was not dismissed")
	}
	if got := app.state(); len(got.ActiveAlerts) != 0 || len(got.GetAlarms()) != 1 || got.GetAlarms()[0].GetEnabled() {
		t.Fatalf("one-shot after dismiss = %+v", got)
	}
}

func TestWriteFileAtomicReplacesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("atomic write = %q, %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "settings.json" {
			t.Fatalf("atomic write left a temporary file %q", entry.Name())
		}
	}
}

func TestFlatAppSettingsFilesMigrate(t *testing.T) {
	base := shortTempDir(t)
	root := filepath.Join(base, "app-settings")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	clockDoc, _ := json.Marshal(map[string]any{
		"revision": 4,
		"values":   map[string]any{"blink": false, "display_mode": "stopwatch"},
	})
	ringDoc, _ := json.Marshal(map[string]any{
		"revision": 6,
		"values": map[string]any{
			"effect": "rainbow", "palette": "spectrum", "speed": 4,
		},
	})
	if err := os.WriteFile(filepath.Join(root, clockAppID+".json"), clockDoc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ringAppID+".json"), ringDoc, 0o600); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	clockSnap, err := d.appCfg.Snapshot(clockAppID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, v := range clockSnap.Values {
		values[v.Key] = v.Value
	}
	if values["blink"] != "false" || values["display_mode"] != `"stopwatch"` {
		t.Fatalf("flat clock settings not migrated: %v", values)
	}
	ringSnap, err := d.appCfg.Snapshot(ringAppID)
	if err != nil {
		t.Fatal(err)
	}
	ringValues := map[string]string{}
	for _, v := range ringSnap.Values {
		ringValues[v.Key] = v.Value
	}
	if ringValues["palette"] != `"linear-gradient(90deg, #FF46D2 0%, #46C8FF 100%)"` {
		t.Fatalf("flat ring palette not migrated: %q", ringValues["palette"])
	}
}

func TestExistingCanonicalSettingsBeatFlatFile(t *testing.T) {
	base := shortTempDir(t)
	root := filepath.Join(base, "app-settings")
	if err := os.MkdirAll(filepath.Join(root, clockAppID), 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(map[string]any{
		"revision": 9,
		"values":   map[string]any{"blink": true},
	})
	if err := os.WriteFile(filepath.Join(root, clockAppID, "settings.json"), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	flat, _ := json.Marshal(map[string]any{
		"revision": 1,
		"values":   map[string]any{"blink": false},
	})
	if err := os.WriteFile(filepath.Join(root, clockAppID+".json"), flat, 0o600); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	snap, err := d.appCfg.Snapshot(clockAppID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range snap.Values {
		if v.Key == "blink" && v.Value != "true" {
			t.Fatalf("flat file overwrote canonical blink: %q", v.Value)
		}
	}
}

func TestLegacyAlarmLabelPreserved(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "app-settings", clockAppID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"revision": 2,
		"values": map[string]any{
			"alarm_hour": 7, "alarm_minute": 0, "alarm_days": 127,
			"alarm_enabled": true, "alarm_label": "Wake up",
		},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	waitFor(t, 2*time.Second, "clock running", func() bool {
		app := d.builtins.clock
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running
	})
	alarms := d.builtins.clock.state().GetAlarms()
	if len(alarms) != 1 || alarms[0].GetLabel() != "Wake up" {
		t.Fatalf("alarm label lost: %+v", alarms)
	}
}

func TestLegacyRingPaletteMigratesToGradient(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "app-settings", ringAppID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"revision": 3,
		"values":   map[string]any{"effect": "spinner", "palette": "ocean", "brightness": 0.5},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	snap, err := d.appCfg.Snapshot(ringAppID)
	if err != nil {
		t.Fatal(err)
	}
	var palette string
	for _, v := range snap.Values {
		if v.Key == "palette" {
			if err := json.Unmarshal([]byte(v.Value), &palette); err != nil {
				t.Fatal(err)
			}
		}
	}
	if palette != "linear-gradient(90deg, #00B4FF 0%, #0046D2 100%)" {
		t.Fatalf("palette = %q", palette)
	}
}

func TestRingStaticAndGradientFrames(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app := d.builtins.ring
	waitFor(t, 5*time.Second, "ring app running", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.running
	})
	gradient, err := ringpalette.Parse("linear-gradient(90deg, #FF0000 0%, #0000FF 100%)")
	if err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.effect = "static"
	app.palette = gradient.Format()
	app.gradient = gradient
	app.brightness = 0.5
	app.speed = 1
	app.active = true
	id, params, _ := app.contentLocked()
	app.mu.Unlock()
	if id != "system.ring.static" {
		t.Fatalf("static effect preset = %q", id)
	}
	frame, err := presets.RingFrame(id, params, 12, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := frame[0]
	if first.R < 120 || first.R > 136 || first.G != 0 || first.B != 0 {
		t.Fatalf("static pixel = %+v", first)
	}
	for i, px := range frame {
		if px != first {
			t.Fatalf("static pixel %d = %+v, want %+v", i, px, first)
		}
	}

	// A gradient palette reaches the animated effects as sampled colours.
	app.mu.Lock()
	app.effect = "comet"
	app.brightness = 1
	id, params, _ = app.contentLocked()
	app.mu.Unlock()
	if id != "system.ring.comet" {
		t.Fatalf("comet preset = %q", id)
	}
	primary, ok := params["color"].(backend.RGB)
	if !ok || primary.R != 255 || primary.B != 0 {
		t.Fatalf("comet primary = %#v", params["color"])
	}
	tail, ok := params["tail_color"].(backend.RGB)
	if !ok || tail.B != 255 || tail.R != 0 {
		t.Fatalf("comet tail = %#v", params["tail_color"])
	}
}
