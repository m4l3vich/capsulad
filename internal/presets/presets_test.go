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

func TestCometAndSparkleRenderDeterministically(t *testing.T) {
	comet := Params{
		"speed": 0.5, "color": backend.RGB{R: 0, G: 220, B: 255},
		"tail_color": backend.RGB{R: 0, G: 64, B: 180}, "tail": int64(8), "intensity": 1.0,
	}
	a, err := RingFrame("system.ring.comet", comet, 12, 0.2)
	if err != nil {
		t.Fatalf("comet: %v", err)
	}
	b, err := RingFrame("system.ring.comet", comet, 12, 0.2)
	if err != nil {
		t.Fatalf("comet repeat: %v", err)
	}
	if len(a) != 12 || len(b) != 12 {
		t.Fatalf("comet lengths = %d/%d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("comet pixel %d changed between identical frames", i)
		}
	}
	lit := 0
	for _, px := range a {
		if px != (backend.RGB{}) {
			lit++
		}
	}
	if lit < 2 {
		t.Fatalf("comet lit %d pixels, want head plus tail", lit)
	}

	sparkle := Params{
		"speed": 1.0, "color": backend.RGB{R: 255, G: 255, B: 255},
		"color_b": backend.RGB{R: 80, G: 120, B: 255}, "density": 1.0, "intensity": 1.0,
	}
	first, err := RingFrame("system.ring.sparkle", sparkle, 12, 0.15)
	if err != nil {
		t.Fatalf("sparkle: %v", err)
	}
	second, err := RingFrame("system.ring.sparkle", sparkle, 12, 0.15)
	if err != nil {
		t.Fatalf("sparkle repeat: %v", err)
	}
	twinkles := 0
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("sparkle pixel %d flickered between identical frames", i)
		}
		if first[i] != (backend.RGB{}) {
			twinkles++
		}
	}
	if twinkles == 0 {
		t.Fatal("sparkle rendered no lit pixels")
	}
	if _, err := RingFrame("system.ring.sparkle", Params{"density": 2.0}, 12, 0); err == nil {
		t.Fatal("out-of-range density was accepted")
	}
}

func TestRainbowBrightnessIsAdditive(t *testing.T) {
	full, err := RingFrame("system.ring.rainbow", Params{"brightness": 1.0}, 12, 0.3)
	if err != nil {
		t.Fatalf("rainbow: %v", err)
	}
	half, err := RingFrame("system.ring.rainbow", Params{"brightness": 0.5}, 12, 0.3)
	if err != nil {
		t.Fatalf("rainbow half: %v", err)
	}
	darker := 0
	for i := range full {
		if half[i].R <= full[i].R && half[i].G <= full[i].G && half[i].B <= full[i].B &&
			half[i] != full[i] {
			darker++
		}
	}
	if darker == 0 {
		t.Fatal("brightness did not dim the rainbow")
	}
	// Omitting brightness keeps the historical full-level output.
	legacy, err := RingFrame("system.ring.rainbow", Params{}, 12, 0.3)
	if err != nil {
		t.Fatalf("rainbow default: %v", err)
	}
	for i := range legacy {
		if legacy[i] != full[i] {
			t.Fatalf("default brightness changed legacy output at pixel %d", i)
		}
	}
}
