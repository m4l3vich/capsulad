package presets

import (
	"testing"

	"capsulad/internal/backend"
)

func TestSpinnerRendersAndValidates(t *testing.T) {
	px, err := RingFrame("system.ring.spinner", Params{"speed": 0.5, "tail": int64(3)}, 12, 0.25)
	if err != nil {
		t.Fatalf("RingFrame: %v", err)
	}
	if len(px) != 12 {
		t.Fatalf("got %d pixels", len(px))
	}
	nonZero := 0
	for _, p := range px {
		if p != (backend.RGB{}) {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Fatal("spinner produced an entirely black frame")
	}
	if _, err := RingFrame("system.ring.spinner", Params{"unknown": 1.0}, 12, 0); err == nil {
		t.Fatal("unknown argument was accepted")
	}
	if _, err := RingFrame("system.ring.spinner", Params{"speed": 99.0}, 12, 0); err == nil {
		t.Fatal("out-of-range argument was accepted")
	}
	if _, err := RingFrame("no.such.effect", nil, 12, 0); err == nil {
		t.Fatal("unknown effect was accepted")
	}
}

func TestSegmentFaces(t *testing.T) {
	caps := backend.SegmentCaps{DigitCount: 4, SupportsColon: true, SupportsDecimal: false, SegmentsPerDigit: 7}
	cells, colon, err := SegmentMask("system.display.face.neutral", caps)
	if err != nil {
		t.Fatalf("neutral: %v", err)
	}
	if len(cells) != 4 || !colon {
		t.Fatalf("unexpected neutral face: %v colon=%v", cells, colon)
	}
	if _, _, err := SegmentMask("system.ring.spinner", caps); err == nil {
		t.Fatal("ring effect accepted as a segment face")
	}
	if _, _, err := SegmentMask("system.display.face.neutral", backend.SegmentCaps{DigitCount: 4, SegmentsPerDigit: 3}); err == nil {
		t.Fatal("unsupported geometry was accepted")
	}
}

func TestIndicatorLightsOneClampedRun(t *testing.T) {
	px, err := RingFrame("system.ring.indicator", Params{
		"color": backend.RGB{R: 255}, "start": int64(5), "count": int64(3),
		"intensity": 1.0, "fade_in": 0.0,
	}, 12, 0)
	if err != nil {
		t.Fatalf("RingFrame: %v", err)
	}
	want := map[int]bool{5: true, 6: true, 7: true}
	for i, p := range px {
		lit := p != (backend.RGB{})
		if lit != want[i] {
			t.Fatalf("pixel %d lit=%v, want %v", i, lit, want[i])
		}
		if lit && p != (backend.RGB{R: 255}) {
			t.Fatalf("pixel %d = %+v, want pure red", i, p)
		}
	}
	// A run that would cross the end of the ring is clamped, not wrapped.
	px, err = RingFrame("system.ring.indicator", Params{
		"color": backend.RGB{G: 255}, "start": int64(10), "count": int64(5),
		"intensity": 1.0, "fade_in": 0.0,
	}, 12, 0)
	if err != nil {
		t.Fatalf("RingFrame: %v", err)
	}
	for i, p := range px {
		if lit := p != (backend.RGB{}); lit != (i >= 10) {
			t.Fatalf("clamped run: pixel %d lit=%v", i, lit)
		}
	}
}

func TestIndicatorFadesInFromDark(t *testing.T) {
	params := Params{
		"color": backend.RGB{R: 255}, "start": int64(5), "count": int64(3),
		"intensity": 1.0, "fade_in": 0.2,
	}
	atZero, err := RingFrame("system.ring.indicator", params, 12, 0)
	if err != nil {
		t.Fatalf("RingFrame: %v", err)
	}
	if atZero[5] != (backend.RGB{}) {
		t.Fatalf("pixel 5 at t=0 = %+v, want black (fade-in start)", atZero[5])
	}
	half, err := RingFrame("system.ring.indicator", params, 12, 0.1)
	if err != nil {
		t.Fatalf("RingFrame: %v", err)
	}
	if half[5].R == 0 || half[5].R >= 255 {
		t.Fatalf("pixel 5 mid-fade = %+v, want partial red", half[5])
	}
	full, err := RingFrame("system.ring.indicator", params, 12, 0.5)
	if err != nil {
		t.Fatalf("RingFrame: %v", err)
	}
	if full[5] != (backend.RGB{R: 255}) {
		t.Fatalf("pixel 5 after fade = %+v, want full red", full[5])
	}
}
