package daemon

import (
	"encoding/binary"
	"testing"

	"capsulad/internal/backend"
)

func TestVolumeCueIsPercussivePCM(t *testing.T) {
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
	pcm := volumeCuePCM(format)
	if len(pcm) != int(48000*0.09)*2*2 {
		t.Fatalf("cue length = %d bytes", len(pcm))
	}
	if again := volumeCuePCM(format); &again[0] != &pcm[0] {
		t.Fatal("cue is not cached")
	}
	var peak int16
	positive, negative := false, false
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if v > peak {
			peak = v
		}
		if v < -peak {
			peak = -v
		}
		if v > 0 {
			positive = true
		}
		if v < 0 {
			negative = true
		}
	}
	if peak < 8000 || peak > 32000 {
		t.Fatalf("cue peak = %d, want a clearly audible but sub-full-scale transient", peak)
	}
	if !positive || !negative {
		t.Fatal("cue is DC, not a sound")
	}
	// Percussive: the tail must be far quieter than the attack.
	head := amplitudeAt(pcm, 100)
	tail := amplitudeAt(pcm, len(pcm)-200)
	if tail >= head {
		t.Fatalf("cue does not decay: head=%d tail=%d", head, tail)
	}
}

func amplitudeAt(pcm []byte, offset int) int {
	if offset < 0 || offset+1 >= len(pcm) {
		return 0
	}
	v := int16(binary.LittleEndian.Uint16(pcm[offset:]))
	if v < 0 {
		return -int(v)
	}
	return int(v)
}

func TestVolumeCellsRightAligned(t *testing.T) {
	if got := volumeCells(5); got[3] != volumeDigits[5] || got[2] != 0 || got[1] != 0 || got[0] != 0 {
		t.Fatalf("5%% = %v", got)
	}
	if got := volumeCells(35); got[2] != volumeDigits[3] || got[3] != volumeDigits[5] {
		t.Fatalf("35%% = %v", got)
	}
	if got := volumeCells(100); got[1] != volumeDigits[1] || got[2] != volumeDigits[0] || got[3] != volumeDigits[0] {
		t.Fatalf("100%% = %v", got)
	}
	if got := volumeCells(150); got[1] != volumeDigits[1] {
		t.Fatalf("150%% not clamped: %v", got)
	}
}
