package testseq

import (
	"math"
	"slices"
	"testing"
	"time"

	"capsulad/sdk/capsulad"
)

func TestDigitGlyphs(t *testing.T) {
	for digit := 1; digit <= 5; digit++ {
		if got := Digit(byte('0' + digit)); got == 0 {
			t.Fatalf("digit %d has no mask", digit)
		}
		frame := DigitFrame(digit)
		if len(frame) != 4 || frame[3] != Digit(byte('0'+digit)) || frame[0] != 0 {
			t.Fatalf("DigitFrame(%d) = %#v", digit, frame)
		}
	}
	if Digit('x') != 0 || DigitFrame(0)[3] != 0 {
		t.Fatal("non-digit produced a glyph")
	}
}

func TestKeyDigit(t *testing.T) {
	want := map[string]int{
		"mic_toggle": 1, "volume_up": 2, "play_pause": 3,
		"volume_down": 4, "center": 5,
	}
	for key, digit := range want {
		if got := KeyDigit(key); got != digit {
			t.Fatalf("KeyDigit(%q) = %d, want %d", key, got, digit)
		}
	}
	if KeyDigit("other") != 0 {
		t.Fatal("unknown key mapped to a digit")
	}
}

func TestColonSchedule(t *testing.T) {
	// Upper phase, first ten seconds: upper on in even whole seconds, lower off.
	for _, tc := range []struct {
		ns           uint64
		top, bottom  bool
	}{
		{0, true, false},
		{499_999_999, true, false},
		{500_000_000, false, false},
		{1_000_000_000, true, false},
		{9_999_999_999, false, false},
	} {
		top, bottom := ColonDots(tc.ns)
		if top != tc.top || bottom != tc.bottom {
			t.Fatalf("ColonDots(%d) = %v/%v, want %v/%v", tc.ns, top, bottom, tc.top, tc.bottom)
		}
	}
	// Lower phase, next ten seconds: lower on in the first quarter of each
	// 500 ms period, upper always off.
	for _, tc := range []struct {
		ns           uint64
		top, bottom  bool
	}{
		{10_000_000_000, false, true},
		{10_249_999_999, false, true},
		{10_250_000_000, false, false},
		{10_500_000_000, false, true},
		{19_999_999_999, false, false},
	} {
		top, bottom := ColonDots(tc.ns)
		if top != tc.top || bottom != tc.bottom {
			t.Fatalf("ColonDots(%d) = %v/%v, want %v/%v", tc.ns, top, bottom, tc.top, tc.bottom)
		}
	}
	// The next upper phase repeats the first.
	if top, bottom := ColonDots(20_000_000_000); !top || bottom {
		t.Fatalf("second upper phase = %v/%v", top, bottom)
	}
}

func TestRainbowIsClockwiseAndWhiteFlash(t *testing.T) {
	frame := RainbowChase(12, 0)
	if len(frame) != 12 {
		t.Fatalf("frame has %d pixels", len(frame))
	}
	if frame[0] != (capsulad.RGB{R: 255}) {
		t.Fatalf("pixel 0 = %+v, want red", frame[0])
	}
	// A quarter of the ring is a quarter hue step.
	if got, want := frame[3], HSV(0.25, 1, 1); got != want {
		t.Fatalf("pixel 3 = %+v, want %+v", got, want)
	}
	// One second later the same hue has moved to pixel 6 for a two-second
	// revolution: the chase advances clockwise.
	if got, want := RainbowChase(12, uint64(time.Second))[6], HSV(0, 1, 1); got != want {
		t.Fatalf("chase pixel 6 after 1s = %+v, want %+v", got, want)
	}
	on := RedFlash(12, 0)
	off := RedFlash(12, 250_000_000)
	if !slices.Equal(on, filled(12, capsulad.RGB{R: 255})) {
		t.Fatalf("red flash on = %+v", on)
	}
	if !slices.Equal(off, make([]capsulad.RGB, 12)) {
		t.Fatalf("red flash off = %+v", off)
	}
}

func filled(n int, px capsulad.RGB) []capsulad.RGB {
	out := make([]capsulad.RGB, n)
	for i := range out {
		out[i] = px
	}
	return out
}

func TestToneDetector(t *testing.T) {
	format := capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: 16000,
		Channels:     1,
	}
	silence := make([]byte, 16000)
	tone := TonePCM(16000, 1, uint64(800*time.Millisecond), 440, 0.2)
	noise := TonePCM(16000, 1, uint64(800*time.Millisecond), 1000, 0.2)

	toneAmp, toneRMS := ToneAmplitude(tone, format, 440)
	if toneAmp < 0.05 || toneRMS < 0.05 {
		t.Fatalf("tone amplitude=%.4f rms=%.4f", toneAmp, toneRMS)
	}
	if !ToneHeard(toneAmp, 0) {
		t.Fatalf("tone %.4f was not heard against silence", toneAmp)
	}
	noiseAmp, _ := ToneAmplitude(noise, format, 440)
	if ToneHeard(noiseAmp, 0) {
		t.Fatalf("1000 Hz signal was accepted as 440 Hz (%.4f)", noiseAmp)
	}
	silenceAmp, _ := ToneAmplitude(silence, format, 440)
	if ToneHeard(silenceAmp, 0) {
		t.Fatal("silence was accepted as a tone")
	}
	if ToneHeard(toneAmp, toneAmp) {
		t.Fatal("a tone at its own baseline level was accepted")
	}
}

func TestToneAmplitudeStereo(t *testing.T) {
	format := capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: 16000,
		Channels:     2,
	}
	pcm := TonePCM(16000, 2, uint64(200*time.Millisecond), 440, 0.2)
	if len(pcm)%(2*2) != 0 {
		t.Fatalf("stereo PCM length %d is not frame aligned", len(pcm))
	}
	if amp, _ := ToneAmplitude(pcm, format, 440); math.Abs(amp-0.2) > 0.02 {
		t.Fatalf("stereo amplitude = %.4f, want about 0.2", amp)
	}
}
