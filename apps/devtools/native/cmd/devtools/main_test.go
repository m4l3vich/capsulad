package main

import (
	"encoding/binary"
	"testing"
	"time"

	"capsula/sdk/capsula"
)

func TestSegmentMasks(t *testing.T) {
	cells := segmentMasks("1234", 4)
	want := []uint16{0x06, 0x5b, 0x4f, 0x66}
	if len(cells) != len(want) {
		t.Fatalf("cells = %v, want %v", cells, want)
	}
	for i := range want {
		if cells[i] != want[i] {
			t.Fatalf("cell %d = 0x%04x, want 0x%04x", i, cells[i], want[i])
		}
	}
	if got := segmentMasks("12", 4); got[2] != want[0] || got[3] != want[1] {
		t.Fatalf("repeated pattern = %v", got)
	}
	if got := segmentMasks("12", 0); got != nil {
		t.Fatalf("zero digits = %v, want nil", got)
	}
}

func TestRingPattern(t *testing.T) {
	frame := ringPattern(12)
	if len(frame) != 12 {
		t.Fatalf("pixels = %d, want 12", len(frame))
	}
	for i, pixel := range frame {
		if pixel == (capsula.RGB{}) {
			t.Fatalf("pixel %d is black; a known pattern must be visible", i)
		}
	}
	if frame[0].B >= frame[11].B || frame[0].G >= frame[11].G {
		t.Fatalf("gradient does not rise: %v -> %v", frame[0], frame[11])
	}
}

func TestTonePCM(t *testing.T) {
	const rate, channels = 48000, 2
	pcm := tonePCM(rate, channels, 300*time.Millisecond, 440, 0.2)
	if want := rate * channels * 2 * 3 / 10; len(pcm) != want {
		t.Fatalf("bytes = %d, want %d", len(pcm), want)
	}
	if peak := pcmPeak(pcm); peak < 4000 || peak > 8000 {
		t.Fatalf("peak = %d, want about 0.2*32767", peak)
	}
	// The fade edges start and end at zero.
	for _, off := range []int{0, len(pcm) - 2} {
		if sample := int16(binary.LittleEndian.Uint16(pcm[off:])); sample != 0 {
			t.Fatalf("edge sample at %d = %d, want 0", off, sample)
		}
	}
}

func TestProtoStringRoundTrip(t *testing.T) {
	for _, text := range []string{"", "ping", "quickjs:hello", "тест"} {
		got, err := decodeProtoString(encodeProtoString(text))
		if err != nil {
			t.Fatalf("decode %q: %v", text, err)
		}
		if got != text {
			t.Fatalf("round trip = %q, want %q", got, text)
		}
	}
	for _, payload := range [][]byte{nil, {0x08}, {0x0a, 0x05, 'a'}, {0x0a, 0x01, 0xff}} {
		if _, err := decodeProtoString(payload); err == nil {
			t.Fatalf("decode %v succeeded, want an error", payload)
		}
	}
}

func TestInvokeStep(t *testing.T) {
	if got := invokeStep(nil); got != "selftest" {
		t.Fatalf("nil invocation = %q", got)
	}
	inv := &capsula.LaunchInvocation{Arguments: []capsula.LaunchArgument{
		{Name: "other", Value: "ignored"},
		{Name: "step", Value: " Audio "},
	}}
	if got := invokeStep(inv); got != "audio" {
		t.Fatalf("step = %q, want audio", got)
	}
	if !validStep("selftest") || !validStep("echo") || validStep("bogus") {
		t.Fatal("validStep accepted or rejected the wrong names")
	}
}
