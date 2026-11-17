package linuxdev

import (
	"math"
	"testing"

	"capsulad/internal/backend"
)

func TestEncodeWS2812Frame(t *testing.T) {
	pixels := []backend.RGB{
		{R: 255, G: 128, B: 64},
		{R: 200, G: 100, B: 50},
	}
	gamma := BuildGammaTable(2.2)
	out := EncodeWS2812Frame(pixels, 1, len(WS2812ChannelMap), WS2812ChannelMap[:], gamma)
	if len(out) != len(WS2812ChannelMap) {
		t.Fatalf("len = %d", len(out))
	}
	// Logical channel i lands at physical WS2812ChannelMap[i], after the
	// perceptual curve.
	want := map[int]byte{
		WS2812ChannelMap[0]: gamma[255],
		WS2812ChannelMap[1]: gamma[128],
		WS2812ChannelMap[2]: gamma[64],
		WS2812ChannelMap[3]: gamma[200],
		WS2812ChannelMap[4]: gamma[100],
		WS2812ChannelMap[5]: gamma[50],
	}
	for idx, value := range want {
		if out[idx] != value {
			t.Fatalf("channel %d = %d, want %d", idx, out[idx], value)
		}
	}
	for idx, value := range out {
		if _, expected := want[idx]; !expected && value != 0 {
			t.Fatalf("channel %d = %d, want black (unused pixels)", idx, value)
		}
	}
}

func TestEncodeWS2812FrameBrightness(t *testing.T) {
	pixel := []backend.RGB{{R: 255, G: 255, B: 255}}
	gamma := BuildGammaTable(2.2)
	for _, tc := range []struct {
		name       string
		brightness float64
		want       byte
	}{
		{"negative", -1, 0},
		{"nan", math.NaN(), 0},
		{"above one", 2, gamma[255]},
		{"full", 1, gamma[255]},
		{"half", 0.5, gamma[128]},
		{"auto floor", 0.05, gamma[13]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := EncodeWS2812Frame(pixel, tc.brightness, len(WS2812ChannelMap), WS2812ChannelMap[:], gamma)
			if got := out[WS2812ChannelMap[0]]; got != tc.want {
				t.Fatalf("R = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestWS2812GammaCurve pins the perceptual behaviour rather than the table: the
// endpoints are fixed, the curve is monotonic, and mid tones are darker than a
// linear ramp would make them.
func TestWS2812GammaCurve(t *testing.T) {
	table := BuildGammaTable(2.2)
	if table[0] != 0 || table[255] != 255 {
		t.Fatalf("gamma endpoints = %d..%d", table[0], table[255])
	}
	prev := byte(0)
	for i := range 256 {
		if table[i] < prev {
			t.Fatalf("gamma table is not monotonic at %d", i)
		}
		prev = table[i]
	}
	if mid := table[128]; mid >= 128 || mid < 40 {
		t.Fatalf("gamma(128) = %d, want a mid tone well below the linear 128", mid)
	}
	if low := table[32]; low >= 32 {
		t.Fatalf("gamma(32) = %d, want below the linear 32", low)
	}
}

func TestWS2812ChannelMapIsPermutation(t *testing.T) {
	seen := make([]bool, len(WS2812ChannelMap))
	for _, v := range WS2812ChannelMap {
		if v < 0 || v >= len(WS2812ChannelMap) || seen[v] {
			t.Fatalf("WS2812ChannelMap is not a permutation at index %d", v)
		}
		seen[v] = true
	}
}

// A controller with a channel count the map does not describe is driven in
// logical order instead of being permuted with the wrong table.
func TestEncodeWS2812FrameUnexpectedChannelCount(t *testing.T) {
	gamma := BuildGammaTable(2.2)
	out := EncodeWS2812Frame([]backend.RGB{{R: 255, G: 0, B: 0}}, 1, 3, WS2812ChannelMap[:], gamma)
	if len(out) != 3 || out[0] != gamma[255] || out[1] != 0 || out[2] != 0 {
		t.Fatalf("linear fallback = %v", out)
	}
	zero := EncodeWS2812Frame(nil, 1, 0, WS2812ChannelMap[:], gamma)
	if len(zero) != 0 {
		t.Fatalf("zero channels = %v", zero)
	}
}
