package wakeword

import (
	"encoding/binary"
	"math"
	"strings"
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

// ScoreEngine is the shared-frontend detector interface: one Process call scores
// every model against the newest complete context. The returned slice is owned
// by the engine and must be consumed before the next call.
type ScoreEngine interface {
	Process(samples []int16) []float64
	Reset()
}

// singleEngine adapts one model's Engine to ScoreEngine, so a single-model
// detector and an ensemble share the policy below instead of keeping two copies
// of the streak, cooldown and gate rules.
type singleEngine struct {
	Engine
	score [1]float64
}

func (e *singleEngine) Process(samples []int16) []float64 {
	e.score[0] = e.Engine.Process(samples)
	return e.score[:]
}

// RunnerOptions tunes the activation policy. The defaults follow the decision
// in docs/WAKEWORD.md: two consecutive windows above the model's threshold, then
// a 2.9 s global cooldown so one utterance cannot fire twice — not even as two
// different words.
// defaultResumeDelay is how long detection stays off after the assistant
// releases the audio path: its last output is still being captured.
const defaultResumeDelay = 1200 * time.Millisecond

type RunnerOptions struct {
	// Threshold applies to the single-model NewRunner constructor. An ensemble
	// takes each model's calibrated threshold from its DetectionSpec.
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
// per-model enablement, the bound app and bounded pre-roll.
type Runner struct {
	manager *Manager
	engine  ScoreEngine
	specs   []DetectionSpec
	opts    RunnerOptions

	// streaks counts consecutive above-threshold windows per model. peaks holds
	// each model's maximum score since the last trace line.
	streaks []int
	peaks   []float64

	pending      []int16
	lastFireNS   uint64
	lastSequence uint64
	lastLogNS    uint64
	lastBeatNS   uint64
	frames       uint64
}

// NewRunner pairs a single-model engine with the manager. modelID must be
// registered as an available model, otherwise Trigger would reject the
// detection.
func NewRunner(m *Manager, engine Engine, modelID string, opts RunnerOptions) *Runner {
	opts = opts.withDefaults()
	return newRunner(m, &singleEngine{Engine: engine}, []DetectionSpec{
		{ModelID: modelID, Threshold: opts.Threshold},
	}, opts)
}

// NewEnsembleRunner pairs a shared-frontend score engine with one policy per
// head. specs must list every head in the engine's order.
func NewEnsembleRunner(m *Manager, engine ScoreEngine, specs []DetectionSpec, opts RunnerOptions) *Runner {
	opts = opts.withDefaults()
	normalized := make([]DetectionSpec, len(specs))
	for i, spec := range specs {
		normalized[i] = DetectionSpec{ModelID: spec.ModelID, Threshold: normalizeThreshold(spec.Threshold)}
	}
	return newRunner(m, engine, normalized, opts)
}

func newRunner(m *Manager, engine ScoreEngine, specs []DetectionSpec, opts RunnerOptions) *Runner {
	return &Runner{
		manager: m,
		engine:  engine,
		specs:   specs,
		opts:    opts,
		streaks: make([]int, len(specs)),
		peaks:   make([]float64, len(specs)),
	}
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
		scores := r.engine.Process(r.pending)
		r.pending = r.pending[:0]
		r.step(scores)
	}
}

// resetStream prevents audio on opposite sides of a capture gap or gate from
// completing one feature window or one consecutive-score run.
func (r *Runner) resetStream() {
	r.pending = r.pending[:0]
	clear(r.streaks)
	clear(r.peaks)
	r.engine.Reset()
}

// step applies the per-model consecutive-window rule, the global cooldown and
// the arbitration rule, then fires at most one activation.
func (r *Runner) step(scores []float64) {
	now := r.manager.clock.NowNS()
	// Periodic score telemetry: without it a mis-tuned threshold is invisible.
	// It is per model because every word has its own calibrated threshold.
	if r.opts.Trace {
		for i := range r.specs {
			if i < len(scores) && scores[i] > r.peaks[i] {
				r.peaks[i] = scores[i]
			}
		}
		if now-r.lastLogNS >= 500_000_000 {
			for i, spec := range r.specs {
				r.manager.logs.Debugf("wakeword", "", "score model=%s max=%.3f threshold=%.2f",
					spec.ModelID, r.peaks[i], spec.Threshold)
				r.peaks[i] = 0
			}
			r.lastLogNS = now
		}
	}

	inCooldown := r.lastFireNS != 0 && now-r.lastFireNS < uint64(r.opts.Cooldown)
	best := -1
	completed := false
	var bestMargin, bestScore float64
	var bestID string
	for i, spec := range r.specs {
		score := 0.0
		if i < len(scores) {
			score = scores[i]
		}
		if math.IsNaN(score) || score < spec.Threshold {
			r.streaks[i] = 0
			continue
		}
		r.streaks[i]++
		if r.streaks[i] < r.opts.MinConsecutive {
			continue
		}
		completed = true
		if inCooldown {
			continue
		}
		// A word that is not effectively enabled (or whose artifact became
		// unavailable) is dropped before arbitration, so a disabled word cannot
		// shadow an enabled one on the same hop.
		if _, ok := r.manager.detectionTarget(spec.ModelID); !ok {
			r.streaks[i] = 0
			continue
		}
		margin := (score - spec.Threshold) / (1 - spec.Threshold)
		if best < 0 || betterCandidate(margin, score, spec.ModelID, bestMargin, bestScore, bestID) {
			best, bestMargin, bestScore, bestID = i, margin, score, spec.ModelID
		}
	}
	if !completed {
		return
	}
	// Inference may have taken long enough for the assistant to acquire audio or
	// the user to mute. Do not publish a score computed before that change, and
	// do not keep embeddings from one side of the break: without this reset the
	// context after unmute would contain pre-mute audio.
	if !r.manager.detecting() {
		r.resetStream()
		return
	}
	if best < 0 {
		return
	}
	// Fire exactly once. Losers' streaks reset without consuming the cooldown:
	// they did not activate anything, and the winner's cooldown covers the whole
	// utterance anyway.
	clear(r.streaks)
	r.engine.Reset()
	winner := r.specs[best].ModelID
	if _, ok := r.manager.Trigger("wakeword", winner); !ok {
		return
	}
	r.lastFireNS = now
	r.manager.logs.Infof("wakeword", "", "detected word=%s score=%.3f", winner, bestScore)
	if r.opts.Trace && len(r.specs) > 1 {
		var others []string
		for _, spec := range r.specs {
			if spec.ModelID != winner {
				others = append(others, spec.ModelID)
			}
		}
		r.manager.logs.Debugf("wakeword", "", "arbitration winner=%s margin=%.3f; context reset for %s",
			winner, bestMargin, strings.Join(others, ","))
	}
}

// betterCandidate is the deterministic tie-break: normalized margin first
// (calibrated thresholds differ, so raw scores are not comparable across
// words), then the higher raw score, then the lexicographically smaller model
// id so two identical artifacts still resolve the same way every time.
func betterCandidate(margin, score float64, id string, bestMargin, bestScore float64, bestID string) bool {
	if margin != bestMargin {
		return margin > bestMargin
	}
	if score != bestScore {
		return score > bestScore
	}
	return id < bestID
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
