// Package sounds embeds the framework's own UI cue audio: the installation
// startup jingle, the volume-change tick, the microphone mute/unmute cues and
// the Bluetooth source-connected cue.
//
// The files are the Ubuntu ubuntu-sounds 0.3 set, licensed CC BY-SA (see
// COPYING beside them). They are compiled into the daemon rather than installed
// as operator data because they are framework feedback, not application
// content: the cues must exist on every device and must not be editable,
// deletable or replaceable through the shared app asset store.
package sounds

import (
	"embed"
	"encoding/binary"
	"fmt"
)

//go:embed *.wav
var files embed.FS

// Clip is one decoded cue: interleaved 16-bit little-endian PCM at its
// original rate and channel count. The mixer resamples it to the device
// playback format, so the embedded files keep their authored rate.
type Clip struct {
	PCM      []byte
	RateHz   int
	Channels int
}

var (
	startup   = mustClip("startup.wav")
	volume    = mustClip("generic.wav")
	microMute = mustClip("warning.wav")
	microLive = mustClip("info.wav")
	btConnect = mustClip("question.wav")
)

// Startup is the installation/startup jingle.
func Startup() Clip { return startup }

// Volume is the cue for one volume step.
func Volume() Clip { return volume }

// MicrophoneMuted is the cue for a microphone that just became muted.
func MicrophoneMuted() Clip { return microMute }

// MicrophoneLive is the cue for a microphone that just became live.
func MicrophoneLive() Clip { return microLive }

// BluetoothConnected is the cue for a source that just connected while the
// speaker accepts inbound links (speaker mode). It is the authored
// question.wav clip, played once per connection by the daemon's Bluetooth
// watcher; a resync of an already-connected source does not replay it.
func BluetoothConnected() Clip { return btConnect }

// mustClip decodes one embedded file. The inputs are build constants, so a
// decode failure is a programming or packaging error and fails loudly at
// startup (and in the package test) instead of silently muting the cues.
func mustClip(name string) Clip {
	data, err := files.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("sounds: %v", err))
	}
	clip, err := decodeWAV(data)
	if err != nil {
		panic(fmt.Sprintf("sounds: %s: %v", name, err))
	}
	return clip
}

// decodeWAV parses a RIFF/WAVE file containing uncompressed 16-bit PCM. It
// rejects other encodings rather than guessing at them, because a wrong decode
// is played as noise.
func decodeWAV(data []byte) (Clip, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return Clip{}, fmt.Errorf("not a RIFF/WAVE file")
	}
	var (
		format     uint16
		channels   int
		rate       int
		bits       int
		haveFormat bool
		pcm        []byte
	)
	for off := 12; off+8 <= len(data); {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4:]))
		body := off + 8
		if size < 0 || body > len(data) || size > len(data)-body {
			return Clip{}, fmt.Errorf("chunk %q at %d is truncated", id, off)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return Clip{}, fmt.Errorf("fmt chunk is %d bytes", size)
			}
			format = binary.LittleEndian.Uint16(data[body:])
			channels = int(binary.LittleEndian.Uint16(data[body+2:]))
			rate = int(binary.LittleEndian.Uint32(data[body+4:]))
			bits = int(binary.LittleEndian.Uint16(data[body+14:]))
			haveFormat = true
		case "data":
			pcm = data[body : body+size]
		}
		// RIFF chunks are word-aligned: an odd size carries one pad byte.
		off = body + size + size%2
	}
	switch {
	case !haveFormat:
		return Clip{}, fmt.Errorf("missing fmt chunk")
	case format != 1:
		return Clip{}, fmt.Errorf("audio format %d is not 16-bit PCM", format)
	case bits != 16:
		return Clip{}, fmt.Errorf("%d-bit samples, want 16", bits)
	case channels < 1 || channels > 2:
		return Clip{}, fmt.Errorf("%d channels, want 1 or 2", channels)
	case rate <= 0:
		return Clip{}, fmt.Errorf("sample rate %d", rate)
	case len(pcm) == 0:
		return Clip{}, fmt.Errorf("missing or empty data chunk")
	case len(pcm)%(channels*2) != 0:
		return Clip{}, fmt.Errorf("data length %d is not a whole number of frames", len(pcm))
	}
	return Clip{PCM: pcm, RateHz: rate, Channels: channels}, nil
}
