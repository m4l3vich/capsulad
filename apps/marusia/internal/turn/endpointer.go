package turn

import (
	"encoding/binary"
	"math"
	"time"
)

// endpointer is a local end-of-speech gate over 20 ms 16 kHz mono s16le
// frames. The vendor uses a TFLite spotter and server status numbers; neither
// is reproducible offline, so this is an explicit energy gate: a learned noise
// floor, a voice threshold above it, and a hangover window.

// SilenceFloorDBFS is the integrated level below which a turn is treated as
// having captured no audio to recognize. Digital silence and a disconnected or
// muted microphone sit far below it; quiet room noise at normal gain does not.
const SilenceFloorDBFS = -60.0

type endpointer struct {
	endSilence time.Duration
	noiseDB    float64
	frames     int
	lastVoice  time.Time
	speechSeen bool
	// Integrated energy over every frame folded in. It is accumulated from the
	// normalized samples the gate already reads, so measuring a turn costs no
	// extra pass and no copy of the PCM.
	sumSquares float64
	samples    int64
}

type endState struct {
	speechSeen  bool
	endOfSpeech bool
	elapsed     time.Duration
}

func newEndpointer(endSilence time.Duration) *endpointer {
	return &endpointer{
		endSilence: endSilence,
		// Start beside digital silence; the floor adapts down/up on quiet frames.
		noiseDB: -70,
	}
}

// frame folds one captured block into the gate. Non-16-bit-aligned input is
// ignored rather than misread.
func (e *endpointer) frame(pcm []byte, now time.Time) {
	e.frames++
	if len(pcm) < 2 {
		return
	}
	var sum float64
	samples := 0
	for offset := 0; offset+1 < len(pcm); offset += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[offset:offset+2]))) / 32768
		sum += value * value
		samples++
	}
	if samples == 0 {
		return
	}
	e.sumSquares += sum
	e.samples += int64(samples)
	rms := math.Sqrt(sum / float64(samples))
	db := -120.0
	if rms > 0 {
		db = 20 * math.Log10(rms)
	}
	threshold := math.Max(e.noiseDB+9, -52)
	threshold = math.Min(threshold, -30)
	if db > threshold {
		e.lastVoice = now
		e.speechSeen = true
		return
	}
	if db > -100 {
		// Track the floor only on non-voice frames so speech does not raise it.
		e.noiseDB = 0.95*e.noiseDB + 0.05*db
	}
}

// rmsDBFS is the integrated level of every frame folded in so far. A turn with
// no samples, or with digital silence only, reports the -120 dBFS floor rather
// than a negative infinity that no log line can read.
func (e *endpointer) rmsDBFS() float64 {
	if e.samples == 0 || e.sumSquares <= 0 {
		return -120
	}
	return 20 * math.Log10(math.Sqrt(e.sumSquares/float64(e.samples)))
}

// state reports the gate's view at now.
func (e *endpointer) state(now time.Time) endState {
	return endState{
		speechSeen:  e.speechSeen,
		endOfSpeech: e.speechSeen && now.Sub(e.lastVoice) >= e.endSilence,
		elapsed:     time.Duration(e.frames) * 20 * time.Millisecond,
	}
}
