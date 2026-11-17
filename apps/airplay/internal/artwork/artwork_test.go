package artwork

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestPaletteTwoColours(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 96, 48))
	for y := range 48 {
		for x := range 96 {
			if x < 64 {
				img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	data := encodePNG(t, img)

	palette, err := Palette(data)
	if err != nil {
		t.Fatalf("Palette: %v", err)
	}
	if len(palette) != 2 {
		t.Fatalf("palette = %v, want 2 colours", palette)
	}
	if first := palette[0]; first.R < 240 || first.G > 16 || first.B > 16 {
		t.Fatalf("first colour = %v, want red", first)
	}
	if second := palette[1]; second.B < 240 || second.R > 16 || second.G > 16 {
		t.Fatalf("second colour = %v, want blue", second)
	}

	again, err := Palette(data)
	if err != nil {
		t.Fatalf("Palette (second call): %v", err)
	}
	if len(again) != len(palette) {
		t.Fatalf("second palette length = %d, want %d", len(again), len(palette))
	}
	for i := range palette {
		if again[i] != palette[i] {
			t.Fatalf("palette[%d] = %v on repeat, want %v", i, again[i], palette[i])
		}
	}
}

func TestPaletteSolidColour(t *testing.T) {
	want := RGB{R: 10, G: 200, B: 30}
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.SetRGBA(x, y, color.RGBA{R: want.R, G: want.G, B: want.B, A: 255})
		}
	}

	encoders := map[string]func(*testing.T, image.Image) []byte{
		"png":  encodePNG,
		"jpeg": encodeJPEG,
	}
	for name, encode := range encoders {
		t.Run(name, func(t *testing.T) {
			palette, err := Palette(encode(t, img))
			if err != nil {
				t.Fatalf("Palette: %v", err)
			}
			if len(palette) != 1 {
				t.Fatalf("palette = %v, want one colour", palette)
			}
			got := palette[0]
			if absDiff(got.R, want.R) > 6 || absDiff(got.G, want.G) > 6 || absDiff(got.B, want.B) > 6 {
				t.Fatalf("colour = %v, want %v (+-6)", got, want)
			}
		})
	}
}

func TestPaletteGreyscaleHasNoHue(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 256, 8))
	for y := range 8 {
		for x := range 256 {
			img.SetGray(x, y, color.Gray{Y: uint8(x)})
		}
	}

	palette, err := Palette(encodePNG(t, img))
	if err != nil {
		t.Fatalf("Palette: %v", err)
	}
	if len(palette) == 0 {
		t.Fatal("palette is empty")
	}
	if len(palette) > paletteSize {
		t.Fatalf("palette has %d colours, want at most %d", len(palette), paletteSize)
	}
	for _, c := range palette {
		if c.R != c.G || c.G != c.B {
			t.Fatalf("greyscale image yielded saturated colour %v", c)
		}
	}
}

func TestPaletteRejectsInvalidInput(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if _, err := Palette(nil); err == nil {
			t.Fatal("empty input accepted")
		}
	})

	t.Run("too large", func(t *testing.T) {
		if _, err := Palette(make([]byte, maxImageBytes+1)); err == nil {
			t.Fatal("oversized input accepted")
		}
	})

	t.Run("oversized dimensions", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, maxDimension+1, 1))
		if _, err := Palette(encodePNG(t, img)); err == nil {
			t.Fatal("oversized image accepted")
		}
	})

	t.Run("unsupported format", func(t *testing.T) {
		if _, err := Palette([]byte("not an image")); err == nil {
			t.Fatal("unsupported format accepted")
		}
	})

	t.Run("corrupt png", func(t *testing.T) {
		data := append(append([]byte{}, pngSignature...), []byte("garbage")...)
		if _, err := Palette(data); err == nil {
			t.Fatal("corrupt PNG accepted")
		}
	})
}

func TestScale(t *testing.T) {
	cases := []struct {
		name string
		in   RGB
		f    float64
		want RGB
	}{
		{"double clamps", RGB{100, 200, 255}, 2, RGB{200, 255, 255}},
		{"halve", RGB{100, 200, 255}, 0.5, RGB{50, 100, 128}},
		{"zero", RGB{10, 20, 30}, 0, RGB{}},
		{"negative", RGB{10, 20, 30}, -1, RGB{}},
		{"nan", RGB{10, 20, 30}, math.NaN(), RGB{}},
		{"rounds", RGB{1, 3, 255}, 0.4, RGB{0, 1, 102}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Scale(tc.in, tc.f); got != tc.want {
				t.Fatalf("Scale(%v, %v) = %v, want %v", tc.in, tc.f, got, tc.want)
			}
		})
	}
}

// stereoPCM builds interleaved 16-bit little-endian stereo frames at a
// constant amplitude.
func stereoPCM(frames int, amplitude float64) []byte {
	value := uint16(int16(math.Round(amplitude * 32767)))
	buf := make([]byte, frames*4)
	for i := range frames {
		binary.LittleEndian.PutUint16(buf[i*4:], value)
		binary.LittleEndian.PutUint16(buf[i*4+2:], value)
	}
	return buf
}

func silentPCM(frames int) []byte {
	return make([]byte, frames*4)
}

func TestEnvelopeAttackAndRelease(t *testing.T) {
	e := NewEnvelope()
	if got := e.Update(nil); got != 0 {
		t.Fatalf("initial level = %v, want 0", got)
	}
	if got := e.Update([]byte{1, 2}); got != 0 {
		t.Fatalf("short input changed level to %v", got)
	}

	// 100 ms of 0.8-amplitude audio should approach, but not exceed, 0.8.
	attack := e.Update(stereoPCM(4410, 0.8))
	if attack < 0.5 || attack > 0.85 {
		t.Fatalf("level after 100 ms attack = %v, want within (0.5, 0.85)", attack)
	}

	// Empty input is ignored.
	if got := e.Update(nil); got != attack {
		t.Fatalf("empty input changed level from %v to %v", attack, got)
	}

	// 600 ms of silence decays by roughly one release time constant.
	release := e.Update(silentPCM(26460))
	if release >= attack*0.5 {
		t.Fatalf("level after 600 ms release = %v, want well below %v", release, attack)
	}
	if release <= attack*0.2 {
		t.Fatalf("release too fast: level = %v (attack %v)", release, attack)
	}

	// Further silence decays towards zero.
	if got := e.Update(silentPCM(44100 * 3)); got > 0.02 {
		t.Fatalf("level after 3 s silence = %v, want near 0", got)
	}
}

func TestEnvelopeFastAttack(t *testing.T) {
	e := NewEnvelope()
	// One 40 ms time constant at full scale reaches about 1 - 1/e = 0.63.
	if got := e.Update(stereoPCM(1764, 1.0)); got < 0.5 {
		t.Fatalf("level after 40 ms = %v, want > 0.5", got)
	}
}

func TestEnvelopeFullScaleNegativeSamples(t *testing.T) {
	e := NewEnvelope()
	// Two little-endian -32768 samples: one stereo frame.
	got := e.Update([]byte{0x00, 0x80, 0x00, 0x80})
	if got <= 0 || got > 1 {
		t.Fatalf("level = %v, want within (0, 1]", got)
	}
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}
