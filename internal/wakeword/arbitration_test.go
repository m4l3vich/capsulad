package wakeword

import (
	"context"
	"testing"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// vectorEngine returns a scripted score vector per Process call, so the policy
// can be exercised without audio or a frontend.
type vectorEngine struct {
	vectors [][]float64
	i       int
	resets  int
	windows int
}

func (e *vectorEngine) Process([]int16) []float64 {
	e.windows++
	if e.i >= len(e.vectors) {
		// Repeat the last vector: the runner must keep its own policy state.
		return e.vectors[len(e.vectors)-1]
	}
	v := e.vectors[e.i]
	e.i++
	return v
}

func (e *vectorEngine) Reset() { e.resets++ }

func newEnsembleManager(t *testing.T, models ...Model) *Manager {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), clock, logs, models)
	m.SetEnabled(true)
	t.Cleanup(m.Close)
	return m
}

func voiceModel(id, appID string, enabled bool, threshold float64) Model {
	return Model{
		ID: id, DisplayName: "Word " + id, EngineID: "test", Available: true,
		Enabled: enabled, AppID: appID, Threshold: threshold,
	}
}

// Two words that both complete on the same hop must fire exactly once. The
// winner is the larger normalized margin, so a lower raw score can win when its
// calibrated threshold is higher; the loser's streak resets without consuming
// the cooldown.
func TestArbitrationFiresOnceByNormalizedMargin(t *testing.T) {
	m := newEnsembleManager(t, voiceModel("word.a", "", true, 0.5), voiceModel("word.b", "app.b", true, 0.9))
	var fired []Activation
	m.SetOnFire(func(act Activation) { fired = append(fired, act) })
	// word.a: (0.75-0.5)/0.5 = 0.5. word.b: (0.875-0.75)/0.25 = 0.5 — equal
	// margins (all four values are exact in binary), so the higher raw score
	// wins: word.b.
	engine := &vectorEngine{vectors: [][]float64{{0.75, 0.875}, {0.75, 0.875}}}
	r := NewEnsembleRunner(m, engine, []DetectionSpec{
		{ModelID: "word.a", Threshold: 0.5},
		{ModelID: "word.b", Threshold: 0.75},
	}, RunnerOptions{ProcessingTap: audio.CaptureProcessingTap, Cooldown: time.Minute})
	r.HandleFrame(frameWithSamples(1280))
	if len(fired) != 0 {
		t.Fatalf("fired on one window: %v", fired)
	}
	r.HandleFrame(frameWithSamples(1280))
	if len(fired) != 1 {
		t.Fatalf("fired %d times on one hop, want exactly 1", len(fired))
	}
	if fired[0].ModelID != "word.b" || fired[0].AppID != "app.b" {
		t.Fatalf("winner = %+v, want word.b bound to app.b", fired[0])
	}
	if engine.resets != 1 {
		t.Fatalf("context resets = %d, want 1 after a single activation", engine.resets)
	}
	// The winner's cooldown is global: neither word may fire again.
	r.HandleFrame(frameWithSamples(1280))
	r.HandleFrame(frameWithSamples(1280))
	if len(fired) != 1 {
		t.Fatalf("cooldown ignored: %d activations", len(fired))
	}
}

// A smaller raw score wins when its calibrated threshold makes it the clearer
// decision relative to its own scale.
func TestArbitrationPrefersMarginOverRawScore(t *testing.T) {
	m := newEnsembleManager(t, voiceModel("word.low", "", true, 0.99), voiceModel("word.high", "", true, 0.1))
	var winner string
	m.SetOnFire(func(act Activation) { winner = act.ModelID })
	// word.low: (0.999-0.99)/0.01 = 0.90. word.high: (0.20-0.1)/0.9 = 0.111.
	engine := &vectorEngine{vectors: [][]float64{{0.999, 0.2}, {0.999, 0.2}}}
	r := NewEnsembleRunner(m, engine, []DetectionSpec{
		{ModelID: "word.low", Threshold: 0.99},
		{ModelID: "word.high", Threshold: 0.1},
	}, RunnerOptions{ProcessingTap: audio.CaptureProcessingTap})
	r.HandleFrame(frameWithSamples(1280))
	r.HandleFrame(frameWithSamples(1280))
	if winner != "word.low" {
		t.Fatalf("winner = %q, want word.low by normalized margin", winner)
	}
}

// Identical margins and scores resolve by model id, so the outcome is stable.
func TestArbitrationTieBreaksByModelID(t *testing.T) {
	m := newEnsembleManager(t, voiceModel("word.zz", "", true, 0.5), voiceModel("word.aa", "", true, 0.5))
	var winner string
	m.SetOnFire(func(act Activation) { winner = act.ModelID })
	engine := &vectorEngine{vectors: [][]float64{{0.8, 0.8}, {0.8, 0.8}}}
	r := NewEnsembleRunner(m, engine, []DetectionSpec{
		{ModelID: "word.zz", Threshold: 0.5},
		{ModelID: "word.aa", Threshold: 0.5},
	}, RunnerOptions{ProcessingTap: audio.CaptureProcessingTap})
	r.HandleFrame(frameWithSamples(1280))
	r.HandleFrame(frameWithSamples(1280))
	if winner != "word.aa" {
		t.Fatalf("winner = %q, want the lexicographically smaller id", winner)
	}
}

// A word the operator disabled must not fire and must not shadow an enabled
// word that completed on the same hop.
func TestArbitrationSkipsDisabledModel(t *testing.T) {
	m := newEnsembleManager(t, voiceModel("word.off", "", false, 0.3), voiceModel("word.on", "app.on", true, 0.9))
	var fired []Activation
	m.SetOnFire(func(act Activation) { fired = append(fired, act) })
	// The disabled word has the larger margin; it is dropped before arbitration.
	engine := &vectorEngine{vectors: [][]float64{{1.0, 0.95}, {1.0, 0.95}}}
	r := NewEnsembleRunner(m, engine, []DetectionSpec{
		{ModelID: "word.off", Threshold: 0.3},
		{ModelID: "word.on", Threshold: 0.9},
	}, RunnerOptions{ProcessingTap: audio.CaptureProcessingTap})
	r.HandleFrame(frameWithSamples(1280))
	r.HandleFrame(frameWithSamples(1280))
	if len(fired) != 1 || fired[0].ModelID != "word.on" {
		t.Fatalf("activations = %+v, want the enabled word only", fired)
	}
}

// A word that drops below its own threshold restarts its streak; another word's
// streak is independent.
func TestPerModelStreaksAreIndependent(t *testing.T) {
	m := newEnsembleManager(t, voiceModel("word.a", "", true, 0.5), voiceModel("word.b", "", true, 0.5))
	var fired []Activation
	m.SetOnFire(func(act Activation) { fired = append(fired, act) })
	engine := &vectorEngine{vectors: [][]float64{
		{0.9, 0.1}, // a: 1, b: 0
		{0.9, 0.1}, // a completes; b never accumulated a second window
		{0.9, 0.1},
	}}
	r := NewEnsembleRunner(m, engine, []DetectionSpec{
		{ModelID: "word.a", Threshold: 0.5},
		{ModelID: "word.b", Threshold: 0.5},
	}, RunnerOptions{ProcessingTap: audio.CaptureProcessingTap})
	for range 3 {
		r.HandleFrame(frameWithSamples(1280))
	}
	if len(fired) != 1 || fired[0].ModelID != "word.a" {
		t.Fatalf("activations = %+v, want only word.a", fired)
	}

	// The mirror case: word.b completes while word.a keeps resetting. A fresh
	// manager and runner keep the cases independent.
	m = newEnsembleManager(t, voiceModel("word.a", "", true, 0.5), voiceModel("word.b", "", true, 0.5))
	fired = nil
	m.SetOnFire(func(act Activation) { fired = append(fired, act) })
	engine = &vectorEngine{vectors: [][]float64{
		{0.1, 0.9},
		{0.1, 0.9},
		{0.1, 0.9},
	}}
	r = NewEnsembleRunner(m, engine, []DetectionSpec{
		{ModelID: "word.a", Threshold: 0.5},
		{ModelID: "word.b", Threshold: 0.5},
	}, RunnerOptions{ProcessingTap: audio.CaptureProcessingTap})
	for range 3 {
		r.HandleFrame(frameWithSamples(1280))
	}
	if len(fired) != 1 || fired[0].ModelID != "word.b" {
		t.Fatalf("activations = %+v, want only word.b", fired)
	}
}

// A stale score must not fire after a mute, and the context must reset so
// post-unmute audio cannot complete a pre-mute window.
func TestEnsembleRunnerRejectsStaleScoreAfterMute(t *testing.T) {
	m := newEnsembleManager(t, voiceModel("word.a", "", true, 0.5))
	fired := 0
	m.SetOnFire(func(Activation) { fired++ })
	engine := &vectorEngine{vectors: [][]float64{{0.9}, {0.9}}}
	r := NewEnsembleRunner(m, engine, []DetectionSpec{{ModelID: "word.a", Threshold: 0.5}},
		RunnerOptions{ProcessingTap: audio.CaptureProcessingTap, MinConsecutive: 1})
	m.SetMuted(true)
	r.HandleFrame(frameWithSamples(1280))
	if fired != 0 || engine.windows != 0 {
		t.Fatalf("muted audio was scored: fired=%d windows=%d", fired, engine.windows)
	}
}

// The bound app travels with the activation and is empty for a word that falls
// back to the selected assistant.
func TestActivationCarriesBoundApp(t *testing.T) {
	m := newEnsembleManager(t, voiceModel("word.a", "app.bound", true, 0.5), voiceModel("word.b", "", true, 0.5))
	act, ok := m.Trigger("wakeword", "word.a")
	if !ok || act.AppID != "app.bound" {
		t.Fatalf("bound activation = %+v ok=%v", act, ok)
	}
	act, ok = m.Trigger("wakeword", "word.b")
	if !ok || act.AppID != "" {
		t.Fatalf("unbound activation = %+v ok=%v", act, ok)
	}
	// Manual origins never carry a binding: the talk key goes to the selected
	// assistant. An API activation names a specific word, so it keeps that
	// word's binding.
	for _, origin := range []string{"touch", "play_pause_key", "manual", "simulator"} {
		act, ok = m.Trigger(origin, "")
		if !ok || act.AppID != "" {
			t.Fatalf("%s activation = %+v ok=%v, want no binding", origin, act, ok)
		}
	}
	act, ok = m.Trigger("api", "word.a")
	if !ok || act.AppID != "app.bound" {
		t.Fatalf("api activation = %+v ok=%v, want the model binding", act, ok)
	}
}
