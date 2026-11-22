// Package vad is a small energy endpointer for the microphone stream. It
// classifies 16-bit mono frames against a learned room floor, starts an
// utterance after a short run of speech and ends it after sustained silence so
// a breath between words never splits a request.
package vad

import (
	"encoding/binary"
	"math"
	"time"
)

// Config tunes an Endpointer. Use DefaultConfig and override the fields a turn
// needs; zero duration fields fall back to their defaults.
type Config struct {
	SampleRateHz    int
	ThresholdDB     float64
	MinSpeechDBFS   float64
	SpeechStart     time.Duration
	SilenceEnd      time.Duration
	NoSpeechTimeout time.Duration
	MaxUtterance    time.Duration
}

// DefaultConfig is a room-friendly endpointer for 16 kHz mono capture.
func DefaultConfig() Config {
	return Config{
		SampleRateHz:    16000,
		ThresholdDB:     12,
		MinSpeechDBFS:   -50,
		SpeechStart:     100 * time.Millisecond,
		SilenceEnd:      800 * time.Millisecond,
		NoSpeechTimeout: 6 * time.Second,
		MaxUtterance:    20 * time.Second,
	}
}

// Decision is the outcome of processing one frame.
type Decision int

const (
	// Waiting means no speech has started yet.
	Waiting Decision = iota
	// SpeechStarted is returned once, on the frame where the utterance begins.
	SpeechStarted
	// Speaking means the utterance is in progress.
	Speaking
	// EndOfUtterance means the speaker stopped and the turn may be sent.
	EndOfUtterance
	// NoSpeech means the user never started speaking in the allowed window.
	NoSpeech
)

func (d Decision) String() string {
	switch d {
	case Waiting:
		return "waiting"
	case SpeechStarted:
		return "speech_started"
	case Speaking:
		return "speaking"
	case EndOfUtterance:
		return "end_of_utterance"
	case NoSpeech:
		return "no_speech"
	default:
		return "unknown"
	}
}

// smoothFrames is the power-average window used to classify a frame. A short
// click or the gap between syllables must not flip the decision.
const smoothFrames = 5

// Endpointer detects one utterance. It is not safe for concurrent use.
type Endpointer struct {
	cfg      Config
	floor    float64
	samples  int
	window   [smoothFrames]float64
	windowAt int
	windowN  int

	elapsed  time.Duration
	speech   time.Duration
	silence  time.Duration
	started  bool
	done     bool
	noSpeech time.Duration
}

// New creates an endpointer. Missing duration fields use DefaultConfig values.
func New(cfg Config) *Endpointer {
	defaults := DefaultConfig()
	if cfg.SampleRateHz <= 0 {
		cfg.SampleRateHz = defaults.SampleRateHz
	}
	if cfg.ThresholdDB == 0 {
		cfg.ThresholdDB = defaults.ThresholdDB
	}
	if cfg.MinSpeechDBFS == 0 {
		cfg.MinSpeechDBFS = defaults.MinSpeechDBFS
	}
	if cfg.SpeechStart <= 0 {
		cfg.SpeechStart = defaults.SpeechStart
	}
	if cfg.SilenceEnd <= 0 {
		cfg.SilenceEnd = defaults.SilenceEnd
	}
	if cfg.NoSpeechTimeout <= 0 {
		cfg.NoSpeechTimeout = defaults.NoSpeechTimeout
	}
	if cfg.MaxUtterance <= 0 {
		cfg.MaxUtterance = defaults.MaxUtterance
	}
	return &Endpointer{cfg: cfg, floor: -60, noSpeech: cfg.NoSpeechTimeout}
}

// Reset starts a new utterance, keeping the learned room floor. A positive
// noSpeech overrides Config.NoSpeechTimeout for this utterance; zero keeps it.
func (e *Endpointer) Reset(noSpeech time.Duration) {
	e.elapsed, e.speech, e.silence = 0, 0, 0
	e.started, e.done = false, false
	e.windowAt, e.windowN = 0, 0
	e.noSpeech = e.cfg.NoSpeechTimeout
	if noSpeech > 0 {
		e.noSpeech = noSpeech
	}
}

// NoiseFloor returns the learned floor in dBFS.
func (e *Endpointer) NoiseFloor() float64 { return e.floor }

// Observe updates the floor from audio that is known not to be this
// utterance, such as the frames before a wake word. It is optional: Process
// also learns while it is waiting.
func (e *Endpointer) Observe(pcm []byte) {
	db := dbFS(rms(pcm))
	if db < -100 || math.IsInf(db, -1) {
		db = -100
	}
	if e.samples < 25 {
		e.samples++
		e.floor += (db - e.floor) / float64(e.samples)
		return
	}
	if db > e.floor+e.cfg.ThresholdDB {
		return
	}
	e.floor += (db - e.floor) * 0.02
}

// Process consumes one PCM frame and returns the utterance state.
func (e *Endpointer) Process(pcm []byte) Decision {
	d := e.duration(len(pcm))
	if e.done {
		if e.started {
			return EndOfUtterance
		}
		return NoSpeech
	}
	e.elapsed += d
	speech := e.isSpeech(e.smoothed(pcm))

	if !e.started {
		if !speech {
			e.Observe(pcm)
			e.speech = 0
		} else {
			e.speech += d
		}
		if e.speech >= e.cfg.SpeechStart {
			e.started = true
			e.elapsed = e.speech
			return SpeechStarted
		}
		if e.elapsed >= e.noSpeech {
			e.done = true
			return NoSpeech
		}
		return Waiting
	}

	if speech {
		e.silence = 0
	} else {
		e.silence += d
		e.Observe(pcm)
	}
	if e.silence >= e.cfg.SilenceEnd || e.elapsed >= e.cfg.MaxUtterance {
		e.done = true
		return EndOfUtterance
	}
	return Speaking
}

// IsSpeech classifies one frame without advancing the utterance state.
func (e *Endpointer) IsSpeech(pcm []byte) bool { return e.isSpeech(e.smoothed(pcm)) }

func (e *Endpointer) isSpeech(db float64) bool {
	return db > e.cfg.MinSpeechDBFS && db > e.floor+e.cfg.ThresholdDB
}

func (e *Endpointer) smoothed(pcm []byte) float64 {
	power := rms(pcm)
	power *= power
	e.window[e.windowAt] = power
	e.windowAt = (e.windowAt + 1) % smoothFrames
	if e.windowN < smoothFrames {
		e.windowN++
	}
	var sum float64
	for _, value := range e.window[:e.windowN] {
		sum += value
	}
	return dbFS(math.Sqrt(sum / float64(e.windowN)))
}

func (e *Endpointer) duration(bytes int) time.Duration {
	frames := bytes / 2
	if frames <= 0 {
		return 0
	}
	return time.Duration(int64(frames) * int64(time.Second) / int64(e.cfg.SampleRateHz))
}

// rms returns the full-scale RMS of 16-bit little-endian PCM.
func rms(pcm []byte) float64 {
	if len(pcm) < 2 {
		return 0
	}
	var sum float64
	samples := 0
	for offset := 0; offset+1 < len(pcm); offset += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[offset:offset+2]))) / 32768
		sum += value * value
		samples++
	}
	if samples == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(samples))
}

func dbFS(amplitude float64) float64 {
	if amplitude <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(amplitude)
}
