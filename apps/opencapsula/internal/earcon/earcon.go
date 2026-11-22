// Package earcon renders the small in-memory cues the assistant plays when it
// starts listening and when a turn ends in error. The app ships no audio
// assets: a few milliseconds of PCM are generated once at startup.
package earcon

import (
	"encoding/binary"
	"math"
)

const sampleRate = 16000

// Cue is one rendered mono PCM16 cue at 16 kHz.
type Cue []byte

// Listen is a short rising two-tone that marks the start of a request.
func Listen() Cue { return twoTone(660, 880, 70) }

// Error is a lower, longer buzz for a failed turn.
func Error() Cue { return twoTone(320, 220, 140) }

func twoTone(first, second float64, ms int) Cue {
	half := ms / 2
	one := tone(first, half)
	two := tone(second, ms-half)
	return append(one, two...)
}

func tone(frequency float64, ms int) Cue {
	samples := sampleRate * ms / 1000
	pcm := make([]byte, samples*2)
	fade := sampleRate * 5 / 1000 // 5 ms avoids a click at either edge
	if fade > samples/2 {
		fade = samples / 2
	}
	for i := range samples {
		envelope := 1.0
		if i < fade {
			envelope = float64(i) / float64(fade)
		} else if remaining := samples - i; remaining < fade {
			envelope = float64(remaining) / float64(fade)
		}
		value := 0.22 * envelope * math.Sin(2*math.Pi*frequency*float64(i)/sampleRate)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(value*32767)))
	}
	return Cue(pcm)
}
