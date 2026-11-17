//go:build ffmpeg

package aac

import (
	"fmt"
	"os"
	"testing"
)

// adtsFrame is one ADTS access unit split out of a fixture.
type adtsFrame struct {
	payload []byte
}

// parseADTS splits an ADTS stream into its raw AAC access units (the ADTS
// header is transport framing that AirPlay does not send).
func parseADTS(data []byte) ([]adtsFrame, int, int, error) {
	var frames []adtsFrame
	sampleRate := 0
	channels := 0
	rates := []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	for offset := 0; offset+7 <= len(data); {
		if data[offset] != 0xFF || data[offset+1]&0xF0 != 0xF0 {
			return nil, 0, 0, fmt.Errorf("no ADTS sync word at offset %d", offset)
		}
		protectionAbsent := data[offset+1] & 0x01
		headerLen := 7
		if protectionAbsent == 0 {
			headerLen = 9
		}
		length := int(data[offset+3]&0x03)<<11 | int(data[offset+4])<<3 | int(data[offset+5]>>5)
		if length < headerLen || offset+length > len(data) {
			return nil, 0, 0, fmt.Errorf("ADTS frame at offset %d claims length %d", offset, length)
		}
		frequencyIndex := int(data[offset+2] >> 2 & 0x0F)
		if frequencyIndex < len(rates) && sampleRate == 0 {
			sampleRate = rates[frequencyIndex]
			channels = int(data[offset+2]&0x01)<<2 | int(data[offset+3]>>6)
		}
		frames = append(frames, adtsFrame{payload: data[offset+headerLen : offset+length]})
		offset += length
	}
	if len(frames) == 0 {
		return nil, 0, 0, fmt.Errorf("no ADTS frames")
	}
	return frames, sampleRate, channels, nil
}

func TestDecodeRealEncoderFixtures(t *testing.T) {
	tests := []struct {
		name       string
		sampleRate int
		channels   int
	}{
		{"aac-lc-44100-stereo", 44100, 2},
		{"aac-lc-48000-mono", 48000, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adts, err := os.ReadFile("testdata/" + tc.name + ".adts")
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			reference, err := os.ReadFile("testdata/" + tc.name + ".raw")
			if err != nil {
				t.Fatalf("reference: %v", err)
			}
			frames, sampleRate, channels, err := parseADTS(adts)
			if err != nil {
				t.Fatalf("parse ADTS: %v", err)
			}
			if sampleRate != tc.sampleRate || channels != tc.channels {
				t.Fatalf("fixture configuration = %d Hz %d ch, want %d Hz %d ch", sampleRate, channels, tc.sampleRate, tc.channels)
			}
			decoder, err := New(Config{SampleRate: sampleRate, Channels: channels, Profile: ProfileLC})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer decoder.Close()
			var pcm []byte
			for i, frame := range frames {
				pcm, err = decoder.Decode(frame.payload, pcm)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
			}
			if len(pcm) == 0 {
				t.Fatal("the decoder produced no PCM")
			}
			if len(pcm) != len(reference) {
				t.Fatalf("decoded %d bytes, reference is %d", len(pcm), len(reference))
			}
			// The reference comes from ffmpeg's own decode of the same file.
			// A second ffmpeg pass quantises to S16 in the resolver, so a
			// one-LSB difference is expected where the sample is exactly
			// half-way; anything larger would mean a decode error.
			worst := 0
			for i := 0; i+1 < len(pcm); i += 2 {
				mine := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
				want := int(int16(uint16(reference[i]) | uint16(reference[i+1])<<8))
				diff := mine - want
				if diff < 0 {
					diff = -diff
				}
				if diff > worst {
					worst = diff
				}
				if diff > 1 {
					t.Fatalf("sample %d: decoded %d, reference %d", i/2, mine, want)
				}
			}
			if os.Getenv("AAC_DUMP") != "" {
				t.Logf("%d samples, worst quantisation difference %d LSB", len(pcm)/2, worst)
			}
		})
	}
}

// TestFrameSamples pins the nominal AAC frame size the playout code assumes.
func TestFrameSamples(t *testing.T) {
	decoder, err := New(Config{SampleRate: 44100, Channels: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer decoder.Close()
	if got := decoder.FrameSamples(); got != 1024 {
		t.Fatalf("FrameSamples = %d, want 1024", got)
	}
}

// TestRejectsBadConfiguration keeps the configuration contract honest.
func TestRejectsBadConfiguration(t *testing.T) {
	for _, cfg := range []Config{
		{SampleRate: 0, Channels: 2},
		{SampleRate: 44100, Channels: 0},
		{SampleRate: 44100, Channels: 6},
		{SampleRate: 44100, Channels: 2, Profile: "heaac"},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%+v) succeeded", cfg)
		}
	}
}
