package daemon

import (
	"testing"
	"time"

	clockv1 "capsulad/gen/go/capsulad/contracts/clock/v1"
	"capsulad/internal/backend"
	"capsulad/internal/dcron"
	"capsulad/internal/presets"
	"capsulad/internal/ringpalette"
)

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
