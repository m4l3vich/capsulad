package wakeword

import (
	"encoding/binary"
	"math"
	"time"

	"capsulad/internal/audio"
)

// Engine is a streaming wakeword detector. It consumes one window of 16 kHz
// mono PCM and returns a confidence in [0,1]. Implementations must be
// incremental: the runner calls Process on every window with no history
// besides what the engine keeps internally.
type Engine interface {
	// Process scores one window of samples.
	Process(samples []int16) float64
	// Reset clears streaming state (after mute, a gap, or a detection).
	Reset()
}

// RunnerOptions tunes the activation policy. The defaults follow the decision
// in docs/WAKEWORD.md: two consecutive windows above threshold, then a 2.9 s
// cooldown so one utterance cannot fire twice.
// defaultResumeDelay is how long detection stays off after the assistant
// releases the audio path: its last output is still being captured.
const defaultResumeDelay = 1200 * time.Millisecond

type RunnerOptions struct {
	Threshold      float64
	WindowSamples  int
	MinConsecutive int
	Cooldown       time.Duration
	// ProcessingTap is the capture provenance inference requires. A frame with
	// any other tap is rejected and resets streaming state, so undeclared raw
	// audio cannot silently bypass the production cleanup. Empty accepts no
	// frame at all; callers must declare the profile they intend to run.
	ProcessingTap string
	// Trace logs the peak score twice a second. It is the only way to see a
	// threshold set too high, so it is opt-in for field diagnosis.
	Trace bool
}

func (o RunnerOptions) withDefaults() RunnerOptions {
	if o.Threshold <= 0 {
		o.Threshold = 0.5
	}
	if o.WindowSamples <= 0 {
		o.WindowSamples = 1280 // 80 ms at 16 kHz, the openWakeWord step
	}
	if o.MinConsecutive <= 0 {
		o.MinConsecutive = 2
	}
	if o.Cooldown <= 0 {
		o.Cooldown = 2900 * time.Millisecond
	}
	return o
}

// Runner feeds engine windows from the framework capture tap and turns
// detections into activations through the manager, which still enforces mute,
// the selected assistant and bounded pre-roll.
type Runner struct {
	manager *Manager
	engine  Engine
	modelID string
	opts    RunnerOptions

	pending      []int16
	consecutive  int
	lastFireNS   uint64
	lastSequence uint64
	maxScore     float64
	lastLogNS    uint64
	lastBeatNS   uint64
	frames       uint64
}

// NewRunner pairs an engine with the manager. modelID must be registered as an
// available model, otherwise Trigger would reject the detection.
func NewRunner(m *Manager, engine Engine, modelID string, opts RunnerOptions) *Runner {
	return &Runner{manager: m, engine: engine, modelID: modelID, opts: opts.withDefaults()}
}

// HandleFrame consumes one capture chunk. It is called from the subscriber
// goroutine, so it must stay cheap and never block on the audio path.
func (r *Runner) HandleFrame(f audio.Frame) {
	if r.opts.Trace {
		if now := r.manager.clock.NowNS(); now-r.lastBeatNS >= 5_000_000_000 {
			enabled, muted, gated, _ := r.manager.State()
			r.manager.logs.Infof("wakeword", "", "heartbeat frames=%d rate=%d detecting=%t enabled=%t muted=%t gated=%t",
				r.frames, f.RateHz, r.manager.detecting(), enabled, muted, gated)
			r.lastBeatNS = now
		}
		r.frames++
	}
	if f.Discontinuity || (f.Sequence != 0 && r.lastSequence != 0 && f.Sequence != r.lastSequence+1) {
		r.resetStream()
	}
	r.lastSequence = f.Sequence
	// Detection must run on the declared preprocessing, not the raw vendor
	// tap. The daemon passes the effective tap; a mismatch here is a wiring
	// bug or an unsupported capture profile, so fail closed rather than score
	// audio the model was not declared against.
	if r.opts.ProcessingTap == "" || f.ProcessingTap != r.opts.ProcessingTap ||
		f.RateHz != 16000 || len(f.PCM) < 2 || len(f.PCM)%2 != 0 {
		r.resetStream()
		return
	}
	if !r.manager.detecting() {
		r.resetStream()
		return
	}
	for i := 0; i < len(f.PCM); i += 2 {
		r.pending = append(r.pending, int16(binary.LittleEndian.Uint16(f.PCM[i:])))
		if len(r.pending) < r.opts.WindowSamples {
			continue
		}
		// A capture chunk can contain several windows. Recheck the gate for
		// each one, not just before buffering the chunk.
		if !r.manager.detecting() {
			r.resetStream()
			return
		}
		score := r.engine.Process(r.pending)
		r.pending = r.pending[:0]
		if score > r.maxScore {
			r.maxScore = score
		}
		r.step(score)
	}
}

// resetStream prevents audio on opposite sides of a capture gap or gate from
// completing one feature window or one consecutive-score run.
func (r *Runner) resetStream() {
	r.pending = r.pending[:0]
	r.consecutive = 0
	r.maxScore = 0
	r.engine.Reset()
}

// step applies the two-window rule and the cooldown.
func (r *Runner) step(score float64) {
	// Periodic score telemetry: without it a mis-tuned threshold is invisible.
	if r.opts.Trace {
		if now := r.manager.clock.NowNS(); now-r.lastLogNS >= 500_000_000 {
			r.manager.logs.Debugf("wakeword", "", "score max=%.3f threshold=%.2f", r.maxScore, r.opts.Threshold)
			r.maxScore = 0
			r.lastLogNS = now
		}
	}
	if math.IsNaN(score) || score < r.opts.Threshold {
		r.consecutive = 0
		return
	}
	r.consecutive++
	if r.consecutive < r.opts.MinConsecutive {
		return
	}
	now := r.manager.clock.NowNS()
	if r.lastFireNS != 0 && now-r.lastFireNS < uint64(r.opts.Cooldown) {
		return
	}
	// Inference may have taken long enough for the assistant to acquire audio
	// or the user to mute. Do not publish a score computed before that change.
	if !r.manager.detecting() {
		r.resetStream()
		return
	}
	r.consecutive = 0
	r.engine.Reset()
	if _, ok := r.manager.Trigger("wakeword", r.modelID); ok {
		r.lastFireNS = now
		r.manager.logs.Infof("wakeword", "", "detected word=%s score=%.3f", r.modelID, score)
	}
}

// detecting reports whether inference should run at all.
func (m *Manager) detecting() bool {
	m.mu.Lock()
	enabled, muted, gate := m.enabled, m.muted, m.gate
	now := m.clock.NowNS()
	m.mu.Unlock()
	if !enabled || muted {
		return false
	}
	if gate != nil && !gate() {
		m.mu.Lock()
		m.resumeAtNS = 0
		m.gateWasBusy = true
		m.mu.Unlock()
		m.noteSuspended(true)
		return false
	}
	m.mu.Lock()
	if m.gateWasBusy {
		// First allowed window after a busy period starts the tail.
		m.gateWasBusy = false
		m.resumeAtNS = now + uint64(m.resumeDelay)
	}
	ready := now >= m.resumeAtNS
	m.mu.Unlock()
	if !ready {
		// Still inside the tail: detection stays off and the state stays
		// "suspended", so the resumed transition is logged exactly once.
		return false
	}
	m.noteSuspended(false)
	return true
}

// noteSuspended logs gate transitions once instead of on every window.
func (m *Manager) noteSuspended(busy bool) {
	m.mu.Lock()
	changed := m.suspended != busy
	m.suspended = busy
	m.mu.Unlock()
	if !changed {
		return
	}
	if busy {
		m.logs.Infof("wakeword", "", "detection suspended: audio path is busy")
		return
	}
	m.logs.Infof("wakeword", "", "detection resumed")
}
