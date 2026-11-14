package wakeword

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// scriptedEngine returns the supplied scores in order.
type scriptedEngine struct {
	scores    []float64
	i         int
	resets    int
	windows   int
	onProcess func()
}

func (e *scriptedEngine) Process(samples []int16) float64 {
	e.windows++
	if e.onProcess != nil {
		e.onProcess()
	}
	if e.i >= len(e.scores) {
		return 0
	}
	s := e.scores[e.i]
	e.i++
	return s
}

func (e *scriptedEngine) Reset() { e.resets++ }

func newTestManager(t *testing.T, available bool) *Manager {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), clock, logs, []Model{{
		ID: "test.word", DisplayName: "Test word", EngineID: "test", Available: available,
	}})
	m.SetEnabled(true)
	return m
}

func frameWithSamples(n int) audio.Frame {
	pcm := make([]byte, n*2)
	return audio.Frame{PCM: pcm, RateHz: 16000, ProcessingTap: audio.CaptureProcessingTap}
}

func TestRunnerFiresAfterTwoWindowsAndRespectsCooldown(t *testing.T) {
	m := newTestManager(t, true)
	var fired int
	m.SetOnFire(func(Activation) { fired++ })
	engine := &scriptedEngine{scores: []float64{0.9, 0.9, 0.9, 0.9}}
	r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap, Cooldown: 50 * time.Millisecond})

	// Two 80 ms windows above threshold fire once.
	r.HandleFrame(frameWithSamples(1280))
	if fired != 0 {
		t.Fatalf("fired after one window")
	}
	r.HandleFrame(frameWithSamples(1280))
	if fired != 1 {
		t.Fatalf("fired %d times after two windows, want 1", fired)
	}
	// The next two windows are inside the cooldown: no second activation.
	r.HandleFrame(frameWithSamples(1280))
	r.HandleFrame(frameWithSamples(1280))
	if fired != 1 {
		t.Fatalf("cooldown ignored: %d activations", fired)
	}
	time.Sleep(60 * time.Millisecond)
	engine.scores = []float64{0.9, 0.9}
	engine.i = 0
	r.HandleFrame(frameWithSamples(1280))
	r.HandleFrame(frameWithSamples(1280))
	if fired != 2 {
		t.Fatalf("after cooldown fired %d times, want 2", fired)
	}
}

func TestRunnerNeedsConsecutiveWindows(t *testing.T) {
	m := newTestManager(t, true)
	var fired int
	m.SetOnFire(func(Activation) { fired++ })
	engine := &scriptedEngine{scores: []float64{0.9, 0.1, 0.9, 0.1}}
	r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap})
	for i := 0; i < 4; i++ {
		r.HandleFrame(frameWithSamples(1280))
	}
	if fired != 0 {
		t.Fatalf("alternating windows fired %d times, want 0", fired)
	}
}

func TestRunnerIgnoresMutedAudioAndResets(t *testing.T) {
	m := newTestManager(t, true)
	var fired int
	m.SetOnFire(func(Activation) { fired++ })
	engine := &scriptedEngine{scores: []float64{0.9, 0.9, 0.9, 0.9}}
	r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap})
	m.SetMuted(true)
	for i := 0; i < 4; i++ {
		r.HandleFrame(frameWithSamples(1280))
	}
	if fired != 0 || engine.windows != 0 {
		t.Fatalf("muted runner processed audio: fired=%d windows=%d", fired, engine.windows)
	}
	if engine.resets == 0 {
		t.Fatal("muted runner did not reset the engine")
	}
}

func TestRunnerRejectsWrongRate(t *testing.T) {
	m := newTestManager(t, true)
	engine := &scriptedEngine{scores: []float64{0.9, 0.9}}
	r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap})
	f := frameWithSamples(1280)
	f.RateHz = 48000
	r.HandleFrame(f)
	r.HandleFrame(f)
	if engine.windows != 0 {
		t.Fatalf("processed a non-16 kHz stream (%d windows)", engine.windows)
	}
}

func TestRunnerSuspendsWhileGateReportsBusy(t *testing.T) {
	m := newTestManager(t, true)
	var fired int
	m.SetOnFire(func(Activation) { fired++ })
	busy := true
	m.SetResumeDelay(10 * time.Millisecond)
	m.SetGate(func() bool { return !busy })
	engine := &scriptedEngine{scores: []float64{0.9, 0.9, 0.9, 0.9}}
	r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap, Cooldown: 10 * time.Millisecond})

	// The assistant owns audio: the same windows that would fire stay silent.
	for i := 0; i < 4; i++ {
		r.HandleFrame(frameWithSamples(1280))
	}
	if fired != 0 {
		t.Fatalf("fired %d times while gated", fired)
	}
	// Once the assistant releases audio, detection resumes.
	busy = false
	// The first allowed frame starts the post-assistant tail, then detection runs.
	r.HandleFrame(frameWithSamples(1280))
	if fired != 0 {
		t.Fatalf("fired during the post-assistant tail")
	}
	time.Sleep(15 * time.Millisecond)
	r.HandleFrame(frameWithSamples(1280))
	r.HandleFrame(frameWithSamples(1280))
	if fired != 1 {
		t.Fatalf("fired %d times after the gate opened, want 1", fired)
	}
}

func TestRunnerResetsAcrossCaptureDiscontinuities(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sequence      uint64
		discontinuity bool
	}{
		{name: "missing chunk", sequence: 4},
		{name: "duplicate chunk", sequence: 2},
		{name: "backward sequence", sequence: 1},
		{name: "capture read error", sequence: 3, discontinuity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, true)
			fired := 0
			m.SetOnFire(func(Activation) { fired++ })
			engine := &scriptedEngine{scores: []float64{0.9, 0.9, 0.9}}
			r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap})
			feed := func(samples int, sequence uint64, discontinuity bool) {
				f := frameWithSamples(samples)
				f.Sequence, f.Discontinuity = sequence, discontinuity
				r.HandleFrame(f)
			}
			feed(1280, 1, false)
			feed(640, 2, false)
			feed(640, tc.sequence, tc.discontinuity)
			if engine.windows != 1 || fired != 0 || engine.resets != 1 {
				t.Fatalf("gap joined stale audio: windows=%d fired=%d resets=%d", engine.windows, fired, engine.resets)
			}
			feed(640, tc.sequence+1, false)
			if engine.windows != 2 || fired != 0 {
				t.Fatalf("gap retained consecutive score: windows=%d fired=%d", engine.windows, fired)
			}
			feed(1280, tc.sequence+2, false)
			if fired != 1 {
				t.Fatalf("fresh consecutive windows fired %d times, want 1", fired)
			}
		})
	}
}

func TestRunnerInvalidCaptureResetsPartialWindow(t *testing.T) {
	wrongRate := frameWithSamples(1280)
	wrongRate.RateHz = 48000
	for _, tc := range []struct {
		name  string
		frame audio.Frame
	}{
		{name: "wrong rate", frame: wrongRate},
		{name: "empty PCM", frame: frameWithSamples(0)},
		{name: "partial sample", frame: audio.Frame{RateHz: 16000, PCM: []byte{1, 2, 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, true)
			fired := 0
			m.SetOnFire(func(Activation) { fired++ })
			engine := &scriptedEngine{scores: []float64{0.9, 0.9, 0.9}}
			r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap})
			r.HandleFrame(frameWithSamples(1280))
			r.HandleFrame(frameWithSamples(640))
			r.HandleFrame(tc.frame)
			r.HandleFrame(frameWithSamples(640))
			if engine.windows != 1 || fired != 0 || engine.resets != 1 {
				t.Fatalf("invalid capture retained audio: windows=%d fired=%d resets=%d", engine.windows, fired, engine.resets)
			}
			r.HandleFrame(frameWithSamples(640))
			if fired != 0 {
				t.Fatal("invalid capture retained consecutive score")
			}
			r.HandleFrame(frameWithSamples(1280))
			if fired != 1 {
				t.Fatalf("fresh windows fired %d times, want 1", fired)
			}
		})
	}
}

func TestRunnerRechecksGateForEveryWindow(t *testing.T) {
	m := newTestManager(t, true)
	busy := false
	m.SetGate(func() bool { return !busy })
	engine := &scriptedEngine{scores: []float64{0.1, 0.9}, onProcess: func() { busy = true }}
	r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap})
	r.HandleFrame(frameWithSamples(2560))
	if engine.windows != 1 || len(r.pending) != 0 || engine.resets != 1 {
		t.Fatalf("processed gated window: windows=%d pending=%d resets=%d", engine.windows, len(r.pending), engine.resets)
	}
}

func TestRunnerRejectsGateChangeDuringInference(t *testing.T) {
	for _, change := range []string{"assistant audio", "mute"} {
		t.Run(change, func(t *testing.T) {
			m := newTestManager(t, true)
			fired := 0
			m.SetOnFire(func(Activation) { fired++ })
			busy := false
			m.SetGate(func() bool { return !busy })
			engine := &scriptedEngine{scores: []float64{0.9}, onProcess: func() {
				if change == "mute" {
					m.SetMuted(true)
				} else {
					busy = true
				}
			}}
			r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap, MinConsecutive: 1})
			r.HandleFrame(frameWithSamples(1280))
			if fired != 0 || r.lastFireNS != 0 || engine.resets != 1 {
				t.Fatalf("accepted stale inference: fired=%d cooldown=%d resets=%d", fired, r.lastFireNS, engine.resets)
			}
		})
	}
}

func TestRunnerRejectedActivationDoesNotStartCooldown(t *testing.T) {
	m := newTestManager(t, false)
	fired := 0
	m.SetOnFire(func(Activation) { fired++ })
	engine := &scriptedEngine{scores: []float64{0.9, 0.9}}
	r := NewRunner(m, engine, "test.word", RunnerOptions{Threshold: 0.5, ProcessingTap: audio.CaptureProcessingTap, MinConsecutive: 1})
	r.HandleFrame(frameWithSamples(1280))
	m.SetModels([]Model{{ID: "test.word", Available: true}})
	r.HandleFrame(frameWithSamples(1280))
	if fired != 1 {
		t.Fatalf("rejected activation consumed cooldown: fired=%d", fired)
	}
}

// processedProbeEngine records the exact window it scored. It is used at the
// capture->activation boundary to prove the samples that reach inference are
// the declared processed samples, and that the resulting score drives the
// activation rather than a source-text or wiring assumption.
type processedProbeEngine struct {
	got     []int16
	windows int
}

func (e *processedProbeEngine) Process(samples []int16) float64 {
	e.windows++
	e.got = append(e.got[:0], samples...)
	return 0.9
}

func (e *processedProbeEngine) Reset() {}

func TestRunnerFeedsDeclaredProcessedFrameAndRejectsRaw(t *testing.T) {
	m := newTestManager(t, true)
	fired := 0
	m.SetOnFire(func(Activation) { fired++ })
	engine := &processedProbeEngine{}
	r := NewRunner(m, engine, "test.word", RunnerOptions{
		Threshold: 0.5, MinConsecutive: 1, ProcessingTap: audio.CaptureProcessingTap,
	})

	// Different continuous sample values make a raw/processed swap visible.
	const n = 1280
	pcm := make([]byte, n*2)
	for i := range n {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(i-640)))
	}
	raw := audio.Frame{PCM: pcm, RateHz: 16000}
	r.HandleFrame(raw)
	if engine.windows != 0 || fired != 0 {
		t.Fatalf("raw frame reached inference: windows=%d fired=%d", engine.windows, fired)
	}

	processed := raw
	processed.ProcessingTap = audio.CaptureProcessingTap
	r.HandleFrame(processed)
	if engine.windows != 1 {
		t.Fatalf("declared processed frame scored %d windows, want 1", engine.windows)
	}
	for i := 0; i < n; i++ {
		want := int16(i - 640)
		if engine.got[i] != want {
			t.Fatalf("inference sample %d = %d, want %d", i, engine.got[i], want)
		}
	}
	if fired != 1 {
		t.Fatalf("processed samples did not produce an activation: fired=%d", fired)
	}
}
