package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

func TestParseClockTime(t *testing.T) {
	valid := map[string]int{
		"00:00": 0, "07:00": 420, "7:05": 425, "22:00": 1320, "23:59": 1439,
		" 09:30 ": 570,
	}
	for text, want := range valid {
		got, ok := parseClockTime(text)
		if !ok || got != want {
			t.Errorf("parseClockTime(%q) = %d, %v; want %d, true", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "7", "24:00", "12:60", "-1:00", "12:5", "12:005", "aa:bb", "12h30"} {
		if _, ok := parseClockTime(text); ok {
			t.Errorf("parseClockTime(%q) accepted an invalid time", text)
		}
	}
}

func TestWithinSchedule(t *testing.T) {
	utc := time.UTC
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 13, hour, minute, 0, 0, utc)
	}
	cases := []struct {
		name       string
		now        time.Time
		start, end int
		want       bool
	}{
		{"day window inside", at(12, 0), 9 * 60, 17 * 60, true},
		{"day window before start", at(8, 59), 9 * 60, 17 * 60, false},
		{"day window at start", at(9, 0), 9 * 60, 17 * 60, true},
		{"day window at end", at(17, 0), 9 * 60, 17 * 60, false},
		{"overnight before midnight", at(23, 30), 22 * 60, 7 * 60, true},
		{"overnight after midnight", at(6, 0), 22 * 60, 7 * 60, true},
		{"overnight at end", at(7, 0), 22 * 60, 7 * 60, false},
		{"overnight at start", at(22, 0), 22 * 60, 7 * 60, true},
		{"overnight midday", at(12, 0), 22 * 60, 7 * 60, false},
		{"equal bounds mean all day", at(3, 0), 60, 60, true},
	}
	for _, tc := range cases {
		if got := withinSchedule(tc.now, tc.start, tc.end, utc); got != tc.want {
			t.Errorf("%s: withinSchedule = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNextScheduleChange(t *testing.T) {
	utc := time.UTC
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 13, hour, minute, 0, 0, utc)
	}
	cases := []struct {
		name       string
		now        time.Time
		start, end int
		want       time.Time
		has        bool
	}{
		{"day window before end", at(12, 0), 9 * 60, 17 * 60, at(17, 0), true},
		{"day window after end rolls to tomorrow", at(23, 30), 9 * 60, 17 * 60,
			time.Date(2026, 11, 14, 9, 0, 0, 0, utc), true},
		{"overnight before midnight", at(23, 30), 22 * 60, 7 * 60,
			time.Date(2026, 11, 14, 7, 0, 0, 0, utc), true},
		{"overnight before morning end", at(6, 0), 22 * 60, 7 * 60, at(7, 0), true},
		{"overnight midday waits for start", at(12, 0), 22 * 60, 7 * 60, at(22, 0), true},
		{"whole day has no boundary", at(12, 0), 60, 60, time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := nextScheduleChange(tc.now, tc.start, tc.end, utc)
		if ok != tc.has || (tc.has && !got.Equal(tc.want)) {
			t.Errorf("%s: nextScheduleChange = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.has)
		}
	}
}

func TestRingScheduleWaitIsBounded(t *testing.T) {
	r := &builtinRing{
		now: time.Now, activeFrom: 22 * 60, activeUntil: 7 * 60,
		scheduleEnabled: true, loc: time.UTC,
	}
	r.now = func() time.Time { return time.Date(2026, 11, 13, 21, 30, 0, 0, time.UTC) }
	if got := r.scheduleWaitLocked(); got != 30*time.Minute {
		t.Fatalf("wait before an overnight window opened = %v, want 30m", got)
	}
	r.now = func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }
	if got := r.scheduleWaitLocked(); got != scheduleRecheck {
		t.Fatalf("wait inside the active window = %v, want the %v recheck ceiling", got, scheduleRecheck)
	}
	r.now = func() time.Time { return time.Date(2026, 11, 13, 6, 59, 59, 900_000_000, time.UTC) }
	if got := r.scheduleWaitLocked(); got != scheduleMinWait {
		t.Fatalf("wait for a boundary 100ms away = %v, want %v", got, scheduleMinWait)
	}
	r.scheduleEnabled = false
	if got := r.scheduleWaitLocked(); got != 0 {
		t.Fatalf("disabled schedule armed a %v timer, want none", got)
	}
}

func TestRingScheduleValidatesTimes(t *testing.T) {
	if err := validateRingSettings(map[string]json.RawMessage{"schedule_start": []byte(`"25:00"`)}); err == nil {
		t.Fatal("25:00 was accepted as an active start")
	}
	if err := validateRingSettings(map[string]json.RawMessage{"schedule_end": []byte(`"07:00"`)}); err != nil {
		t.Fatalf("07:00 was rejected: %v", err)
	}
	if err := validateRingSettings(map[string]json.RawMessage{"schedule_start": []byte(`7`)}); err == nil {
		t.Fatal("a numeric start was accepted")
	}
}

func TestRingActiveWindowBlanksBaseline(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	app := d.builtins.ring
	if app == nil {
		t.Fatal("ring system app is not registered")
	}

	setRingNow(t, app, time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local))
	if _, _, _, err := d.appCfg.Update(ringAppID, 0, []settings.Change{
		{Key: "effect", Value: "pulse"},
		{Key: "schedule_enabled", Value: true},
		{Key: "schedule_start", Value: "09:00"},
		{Key: "schedule_end", Value: "17:00"},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "ring baseline lit inside the window", func() bool {
		return ringBaselineKind(d) == presentations.ContentRingEffect
	})
	if !ringActive(t, app) {
		t.Fatal("RingService reports inactive at 12:00 inside a 09:00-17:00 window")
	}

	// Outside the window the baseline becomes explicit blank content: the
	// presentation stays selected so the backend writes zeroes, and the effect
	// returns when the window reopens.
	setRingNow(t, app, time.Date(2026, 11, 13, 20, 0, 0, 0, time.Local))
	app.refreshSchedule()
	waitFor(t, 5*time.Second, "ring baseline blank after the window closed", func() bool {
		return ringBaselineKind(d) == presentations.ContentBlank
	})
	if ringActive(t, app) {
		t.Fatal("RingService reports active at 20:00 outside a 09:00-17:00 window")
	}
	// The empty state must be observable: proto3 would otherwise omit a false
	// field and the Web UI could not distinguish it from an unreported value.
	if raw := ringStateJSON(t, app); !strings.Contains(raw, `"active":false`) {
		t.Fatalf("closed window did not report active=false: %s", raw)
	}
	if px := ringBaselinePixels(t, d); len(px) == 0 {
		t.Fatal("blank baseline rendered no pixels")
	} else {
		for i, c := range px {
			if c != (backend.RGB{}) {
				t.Fatalf("blank baseline pixel %d = %+v, want black", i, c)
			}
		}
	}
	// The blank is submitted to the device, not merely recorded as content.
	waitFor(t, 5*time.Second, "backend wrote an all-zero ring frame", func() bool {
		frame := d.dev.(*backend.Simulator).RingSnapshot()
		if len(frame.Pixels) == 0 {
			return false
		}
		for _, c := range frame.Pixels {
			if c != (backend.RGB{}) {
				return false
			}
		}
		return true
	})

	// A blank baseline still yields to another owner and returns blank after
	// that capture is released, rather than freezing or restoring the effect.
	ringID := "light.ring.main"
	external := d.pres.Open(ringID, "test.external", "run-external", presentations.Ring)
	if err := d.pres.Update(external, presentations.RingFrame([]backend.RGB{{R: 10}, {G: 10}, {B: 10}})); err != nil {
		t.Fatal(err)
	}
	cap, err := d.router.Acquire(ringID, "test.external", "run-external", external.ID, routing.TouchNone)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "external capture selected over a blank baseline", func() bool {
		return d.router.Current(ringID).SelectedAppID == "test.external"
	})
	if !d.router.ForceRelease(cap.ID) {
		t.Fatal("external capture was not released")
	}
	d.pres.Close(external)
	waitFor(t, 5*time.Second, "blank baseline restored after release", func() bool {
		sel := d.router.Current(ringID)
		return sel.SelectedAppID == ringAppID && sel.SelectedCaptureID == "" &&
			ringBaselineKind(d) == presentations.ContentBlank
	})

	setRingNow(t, app, time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local))
	app.refreshSchedule()
	waitFor(t, 5*time.Second, "ring baseline resumed after the window reopened", func() bool {
		return ringBaselineKind(d) == presentations.ContentRingEffect
	})
}

func TestRingScheduleFollowsDeviceTimezone(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	app := d.builtins.ring
	if app == nil {
		t.Fatal("ring system app is not registered")
	}
	// 12:00 UTC is 21:00 in Tokyo, outside 09:00-17:00.
	setRingNow(t, app, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	defer app.setDeviceTimezone("")
	app.setDeviceTimezone("UTC")
	if _, _, _, err := d.appCfg.Update(ringAppID, 0, []settings.Change{
		{Key: "schedule_enabled", Value: true},
		{Key: "schedule_start", Value: "09:00"},
		{Key: "schedule_end", Value: "17:00"},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "ring lit in the device zone", func() bool {
		return ringBaselineKind(d) == presentations.ContentRingEffect
	})
	app.setDeviceTimezone("Asia/Tokyo")
	waitFor(t, 5*time.Second, "ring blank in a zone outside the window", func() bool {
		return ringBaselineKind(d) == presentations.ContentBlank
	})
}

// setRingNow pins the ring app's wall clock. Tests mutate it through the lock
// because the boundary loop reads it from its own goroutine.
func setRingNow(t *testing.T, r *builtinRing, at time.Time) {
	t.Helper()
	r.mu.Lock()
	r.now = func() time.Time { return at }
	r.mu.Unlock()
}

func ringBaselineKind(d *Daemon) presentations.ContentType {
	sel := d.router.Current("light.ring.main")
	if sel.SelectedAppID != ringAppID || sel.SelectedPresentation == "" {
		return presentations.ContentNone
	}
	p, ok := d.pres.Get(sel.SelectedPresentation)
	if !ok {
		return presentations.ContentNone
	}
	return p.ContentKind()
}

func ringBaselinePixels(t *testing.T, d *Daemon) []backend.RGB {
	t.Helper()
	sel := d.router.Current("light.ring.main")
	p, ok := d.pres.Get(sel.SelectedPresentation)
	if !ok {
		t.Fatal("ring baseline presentation disappeared")
	}
	px, rendered := p.RenderRing(d.dev.Ring().Caps(), d.clock.NowNS())
	if !rendered {
		t.Fatal("ring baseline did not render")
	}
	return px
}

func ringStateJSON(t *testing.T, app *builtinRing) string {
	t.Helper()
	out, err := app.handleCall(context.Background(), ringServiceName, "GetState", nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(out[0])
}

func ringActive(t *testing.T, app *builtinRing) bool {
	t.Helper()
	out, err := app.handleCall(context.Background(), ringServiceName, "GetState", nil)
	if err != nil {
		t.Fatal(err)
	}
	var state ringv1.RingState
	if err := protojson.Unmarshal(out[0], &state); err != nil {
		t.Fatal(err)
	}
	return state.Active
}
