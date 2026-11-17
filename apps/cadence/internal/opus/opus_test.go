package opus_test

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"capsulad/apps/cadence/internal/opus"
)

// sine returns n samples of a mono 16-bit sine at rate/freq, little-endian.
func sine(rate, freq float64, n int) []byte {
	out := make([]byte, n*2)
	for i := range n {
		v := math.Sin(2 * math.Pi * freq * float64(i) / rate)
		binary.LittleEndian.PutUint16(out[2*i:], uint16(int16(v*0.5*32767)))
	}
	return out
}

func TestEncoderFrameSizeIsEnforced(t *testing.T) {
	enc, err := opus.NewEncoder()
	if err != nil {
		if errors.Is(err, opus.ErrUnavailable) {
			t.Skipf("libopus unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer enc.Close()
	if _, err := enc.Encode(make([]byte, opus.FrameSamples*2-2)); err == nil {
		t.Fatal("a short frame was accepted")
	}
}

// TestUplinkRoundTrip is the real codec proof: one 20 ms 16 kHz mono frame
// encodes to a bounded Opus packet, and decoding it back at the reply rate
// yields exactly 20 ms of 48 kHz PCM. The DSP is lossy, so the test asserts the
// shape and that the signal survives, not sample equality.
func TestUplinkRoundTrip(t *testing.T) {
	enc, err := opus.NewEncoder()
	if err != nil {
		if errors.Is(err, opus.ErrUnavailable) {
			t.Skipf("libopus unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer enc.Close()

	silence := make([]byte, opus.FrameSamples*2)
	packet, err := enc.Encode(silence)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) == 0 || len(packet) > 200 {
		t.Fatalf("20 ms of silence encoded to %d bytes; want a small DTX-free packet", len(packet))
	}

	tone := sine(opus.SampleRateHz, 440, opus.FrameSamples)
	packet, err = enc.Encode(tone)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) == 0 || len(packet) > 4000 {
		t.Fatalf("20 ms of tone encoded to %d bytes", len(packet))
	}

	dec, err := opus.NewDecoder(1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pcm, err := dec.Decode(packet)
	if err != nil {
		t.Fatal(err)
	}
	// 20 ms at 48 kHz mono, 16-bit.
	if want := opus.DecodeSampleRateHz / 50 * 2; len(pcm) != want {
		t.Fatalf("decoded %d bytes; want %d", len(pcm), want)
	}
	var peak float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[i:]))))
		if v > peak {
			peak = v
		}
	}
	if peak < 2000 {
		t.Fatalf("decoded tone peak %.0f is not audible signal", peak)
	}
}

func TestDecoderRejectsBadChannelCount(t *testing.T) {
	if _, err := opus.NewDecoder(3); err == nil {
		t.Fatal("3 channels accepted")
	}
}

func TestDecoderRejectsEmptyPacket(t *testing.T) {
	dec, err := opus.NewDecoder(1)
	if err != nil {
		if errors.Is(err, opus.ErrUnavailable) {
			t.Skipf("libopus unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer dec.Close()
	if _, err := dec.Decode(nil); err == nil {
		t.Fatal("empty packet accepted")
	}
}
