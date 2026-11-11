package sbc

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"testing"
)

func TestDecodeFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/frame.sbc")
	if err != nil {
		t.Fatal(err)
	}
	pcm, rate, channels, err := NewDecoder().Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 44100 || channels != 2 {
		t.Fatalf("format = %d Hz/%d ch, want 44100/2", rate, channels)
	}
	if len(pcm) != 88064 {
		t.Fatalf("pcm bytes = %d, want 88064", len(pcm))
	}
	var sum float64
	for i := 0; i < len(pcm); i += 2 {
		v := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		sum += float64(v) * float64(v)
	}
	rms := math.Sqrt(sum / float64(len(pcm)/2))
	if rms < 6000 || rms > 8000 {
		t.Fatalf("rms = %.0f, want ~6936", rms)
	}
	const want = "7a23a01ee24f31d4da34d46f6bf95119ed148578ff4d26b2faf61747f31cd12c"
	got := sha256.Sum256(pcm)
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("pcm sha256 = %x", got)
	}
}
