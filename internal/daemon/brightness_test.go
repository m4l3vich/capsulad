package daemon

import (
	"math"
	"testing"
)

// TestAmbientBrightnessMatchesStockCurve pins the vendor mapping recovered from
// the light handler at 0x27ca68: log(0.75+0.05*lux)*19.9375, clamped to [5,95].
func TestAmbientBrightnessMatchesStockCurve(t *testing.T) {
	cases := []struct {
		lux  float64
		want float64
	}{
		{0, 0},
		{4.9, 0},
		{5, 0.05},
		{7.4, 0.05}, // measured on the unit: the vendor wrote 14/255 here
		{20, 0.11},
		{50, 0.23},
		{100, 0.34},
		{300, 0.54},
		{1000, 0.78},
		{5000, 0.95},
		{40000, 1},
	}
	for _, tc := range cases {
		if got := ambientBrightness(tc.lux); math.Abs(got-tc.want) > 0.001 {
			t.Fatalf("ambientBrightness(%v) = %.3f, want %.3f", tc.lux, got, tc.want)
		}
	}
	prev := -1.0
	for lux := 0.0; lux <= 40000; lux += 97 {
		got := ambientBrightness(lux)
		if got < prev {
			t.Fatalf("curve not monotonic at %v lux", lux)
		}
		prev = got
	}
}

func TestStepTowardAnimatesWithoutOvershoot(t *testing.T) {
	if got := stepToward(0, 0.5, 0.06); got != 0.06 {
		t.Fatalf("up step = %v", got)
	}
	if got := stepToward(0.5, 0, 0.06); math.Abs(got-0.44) > 1e-9 {
		t.Fatalf("down step = %v", got)
	}
	if got := stepToward(0.49, 0.5, 0.06); got != 0.5 {
		t.Fatalf("overshoot = %v", got)
	}
}
