package routing

import (
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presentations"
)

func newHarness(t *testing.T) (*Router, *presentations.Manager, *logbus.Bus) {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	pres := presentations.NewManager(clock, logs, nil)
	r := New(clock, logs, pres)
	pres.OnChange(func(resourceID string) { r.Recompute(resourceID, "presentation changed") })
	r.SetResourceClass("display.segment.front", "display")
	r.SetResourceClass("light.ring.main", "ring")
	r.SetResourceClass("touch.panel.main", "touch")
	return r, pres, logs
}

func fallback(t *testing.T, r *Router, pres *presentations.Manager, resource string, kind presentations.Kind) *presentations.Presentation {
	t.Helper()
	p := pres.Open(resource, SystemAppID, "system", kind)
	if err := pres.Update(p, presentations.Blank()); err != nil {
		t.Fatal(err)
	}
	r.Recompute(resource, "setup")
	return p
}

func TestDisplayRoutingAndCaptures(t *testing.T) {
	r, pres, _ := newHarness(t)
	fallback(t, r, pres, "display.segment.front", presentations.Segment)
	clock := pres.Open("display.segment.front", "app.clock", "run-clock", presentations.Segment)
	if err := pres.Update(clock, presentations.SegmentFrame([]uint16{1, 2, 3, 4}, true)); err != nil {
		t.Fatal(err)
	}
	r.SetDefault("display.segment.front", "app.clock")
	if got := r.Current("display.segment.front"); got.SelectedPresentation != clock.ID || got.Reason != "default_baseline" {
		t.Fatalf("baseline not selected: %+v", got)
	}
	overlay := pres.Open("display.segment.front", "app.overlay", "run-overlay", presentations.Segment)
	// Capture acquired before first content must not blank the clock.
	cap, err := r.Acquire("display.segment.front", "app.overlay", "run-overlay", overlay.ID, TouchNone)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Current("display.segment.front"); got.SelectedPresentation != clock.ID {
		t.Fatalf("unready capture replaced baseline: %+v", got)
	}
	if err := pres.Update(overlay, presentations.SegmentFrame([]uint16{9, 9, 9, 9}, false)); err != nil {
		t.Fatal(err)
	}
	if got := r.Current("display.segment.front"); got.SelectedPresentation != overlay.ID || got.SelectedCaptureID != cap.ID {
		t.Fatalf("capture did not win: %+v", got)
	}
	if !r.Release(cap.ID, "app.overlay", "run-overlay") {
		t.Fatal("release failed")
	}
	if got := r.Current("display.segment.front"); got.SelectedPresentation != clock.ID {
		t.Fatalf("baseline did not return: %+v", got)
	}
	// Closing the presentation must drop its capture.
	cap2, err := r.Acquire("display.segment.front", "app.overlay", "run-overlay", overlay.ID, TouchNone)
	if err != nil {
		t.Fatal(err)
	}
	pres.Close(overlay)
	if got := r.Current("display.segment.front"); got.SelectedPresentation != clock.ID || got.SelectedCaptureID == cap2.ID {
		t.Fatalf("closed presentation capture survived: %+v", got)
	}
}

func TestTouchModesAndLastCaptureWins(t *testing.T) {
	r, _, _ := newHarness(t)
	r.Recompute("touch.panel.main", "setup")
	if !r.Current("touch.panel.main").SystemTouchActive {
		t.Fatal("system touch handlers should be active without captures")
	}
	partial, err := r.Acquire("touch.panel.main", "app.game", "run-game", "", TouchPartial)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Current("touch.panel.main"); got.SelectedCaptureID != partial.ID || !got.SystemTouchActive {
		t.Fatalf("partial capture state wrong: %+v", got)
	}
	full, err := r.Acquire("touch.panel.main", "app.draw", "run-draw", "", TouchFull)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Current("touch.panel.main"); got.SelectedCaptureID != full.ID || got.SystemTouchActive || got.TouchMode != TouchFull {
		t.Fatalf("full capture did not suppress system handlers: %+v", got)
	}
	if !r.Release(full.ID, "app.draw", "run-draw") {
		t.Fatal("full release failed")
	}
	if got := r.Current("touch.panel.main"); got.SelectedCaptureID != partial.ID || !got.SystemTouchActive {
		t.Fatalf("partial capture did not resume: %+v", got)
	}
	r.RemoveRun("run-game")
	if got := r.Current("touch.panel.main"); got.SelectedCaptureID != "" || !got.SystemTouchActive {
		t.Fatalf("run cleanup failed: %+v", got)
	}
}

func TestOutOfOrderReleaseAndOwnership(t *testing.T) {
	r, pres, _ := newHarness(t)
	fallback(t, r, pres, "light.ring.main", presentations.Ring)
	a := pres.Open("light.ring.main", "app.a", "run-a", presentations.Ring)
	b := pres.Open("light.ring.main", "app.b", "run-b", presentations.Ring)
	for _, p := range []*presentations.Presentation{a, b} {
		if err := pres.Update(p, presentations.RingFrame(nil)); err != nil {
			t.Fatal(err)
		}
	}
	ca, _ := r.Acquire("light.ring.main", "app.a", "run-a", a.ID, TouchNone)
	cb, _ := r.Acquire("light.ring.main", "app.b", "run-b", b.ID, TouchNone)
	if got := r.Current("light.ring.main"); got.SelectedCaptureID != cb.ID {
		t.Fatalf("newest capture not selected: %+v", got)
	}
	if r.Release(cb.ID, "app.a", "run-a") {
		t.Fatal("capture released by the wrong owner")
	}
	if !r.Release(cb.ID, "app.b", "run-b") {
		t.Fatal("correct owner could not release")
	}
	if got := r.Current("light.ring.main"); got.SelectedCaptureID != ca.ID {
		t.Fatalf("older capture did not resume: %+v", got)
	}
}
