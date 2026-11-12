package sounds

import (
	"encoding/binary"
	"testing"
)

// TestEmbeddedCuesDecode guards the shipped files: every cue must remain
// 16-bit PCM with actual signal in it, so a replaced or truncated asset fails
// here instead of playing as silence or noise on a device.
func TestEmbeddedCuesDecode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clip     Clip
		minMilli int
	}{
		{"startup", Startup(), 8000},
		{"volume", Volume(), 400},
		{"microphone muted", MicrophoneMuted(), 800},
		{"microphone live", MicrophoneLive(), 150},
	} {
		clip := tc.clip
		if clip.RateHz != 44100 || clip.Channels < 1 || clip.Channels > 2 {
			t.Fatalf("%s: %d Hz / %d channels", tc.name, clip.RateHz, clip.Channels)
		}
		frames := len(clip.PCM) / (clip.Channels * 2)
		if milli := frames * 1000 / clip.RateHz; milli < tc.minMilli {
			t.Fatalf("%s: %d ms, want at least %d ms", tc.name, milli, tc.minMilli)
		}
		var peak int
		for i := 0; i+1 < len(clip.PCM); i += 2 {
			v := int(int16(binary.LittleEndian.Uint16(clip.PCM[i:])))
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if peak < 8000 {
			t.Fatalf("%s: peak sample %d is effectively silent", tc.name, peak)
		}
	}
}

func TestDecodeWAVRejectsUnsupportedEncodings(t *testing.T) {
	// A valid 16-bit mono frame, but fmt announces IEEE float samples.
	raw := []byte{
		'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E',
		'f', 'm', 't', ' ', 16, 0, 0, 0, 3, 0, 1, 0, 0x44, 0xac, 0, 0, 0, 0, 0, 0, 2, 0, 16, 0,
		'd', 'a', 't', 'a', 2, 0, 0, 0, 1, 0,
	}
	if _, err := decodeWAV(raw); err == nil {
		t.Fatal("float WAV was accepted as 16-bit PCM")
	}
	// Odd chunk padding must not shift the next chunk header.
	raw = []byte{
		'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E',
		'f', 'm', 't', ' ', 16, 0, 0, 0, 1, 0, 1, 0, 0x44, 0xac, 0, 0, 0, 0, 0, 0, 2, 0, 16, 0,
		'j', 'u', 'n', 'k', 1, 0, 0, 0, 0, 0,
		'd', 'a', 't', 'a', 2, 0, 0, 0, 5, 0,
	}
	clip, err := decodeWAV(raw)
	if err != nil {
		t.Fatalf("padded WAV: %v", err)
	}
	if got := int16(binary.LittleEndian.Uint16(clip.PCM)); got != 5 {
		t.Fatalf("padded WAV data = %d, want 5", got)
	}
}
