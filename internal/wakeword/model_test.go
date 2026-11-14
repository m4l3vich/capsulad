package wakeword

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"capsulad/internal/wakeword/oww"
)

func newTestClassifier(t *testing.T, seed uint64) *Classifier {
	t.Helper()
	m := NewClassifier("test.word", "Test Word", "en", seed)
	if err := m.Validate(); err != nil {
		t.Fatalf("NewClassifier produced an invalid model: %v", err)
	}
	return m
}

func zeroWeights(m *Classifier) {
	clear(m.W1)
	clear(m.B1)
	clear(m.W2)
	clear(m.B2)
	clear(m.W3)
	m.B3 = 0
}

func TestClassifierValidate(t *testing.T) {
	m := newTestClassifier(t, 1)
	if m.Version != ClassifierVersion || m.ContextFrames != ContextFrames ||
		m.EmbeddingDim != EmbeddingDim || m.Projection != ProjectionWidth || m.Hidden != HiddenWidth {
		t.Fatalf("unexpected dimensions: %+v", m)
	}
	if m.Display != "Test Word" {
		t.Fatalf("display = %q", m.Display)
	}

	unnamed := newTestClassifier(t, 2)
	unnamed.Display = ""
	if err := unnamed.Validate(); err != nil {
		t.Fatal(err)
	}
	if unnamed.Display != unnamed.ID {
		t.Fatalf("display default = %q, want id", unnamed.Display)
	}

	var nilModel *Classifier
	if nilModel.Validate() == nil {
		t.Fatal("nil classifier accepted")
	}

	for _, version := range []int{1, 2} {
		bad := cloneClassifier(m)
		bad.Version = version
		err := bad.Validate()
		if err == nil {
			t.Fatalf("legacy version %d accepted", version)
		}
		if !strings.Contains(err.Error(), "retrain") {
			t.Fatalf("legacy version %d error %q does not tell the user to retrain", version, err)
		}
	}

	bad := cloneClassifier(m)
	bad.Version = 9
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown version accepted")
	}
	bad = cloneClassifier(m)
	bad.ID = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("empty id accepted")
	}

	for name, mutate := range map[string]func(*Classifier){
		"context_frames": func(c *Classifier) { c.ContextFrames = 15 },
		"embedding_dim":  func(c *Classifier) { c.EmbeddingDim = 95 },
		"projection":     func(c *Classifier) { c.Projection = 23 },
		"hidden":         func(c *Classifier) { c.Hidden = 23 },
		"short w1":       func(c *Classifier) { c.W1 = c.W1[:10] },
		"short b1":       func(c *Classifier) { c.B1 = c.B1[:10] },
		"short w2":       func(c *Classifier) { c.W2 = c.W2[:10] },
		"short b2":       func(c *Classifier) { c.B2 = c.B2[:10] },
		"short w3":       func(c *Classifier) { c.W3 = c.W3[:10] },
	} {
		c := cloneClassifier(m)
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}

	c := cloneClassifier(m)
	c.W1[3] = float32(math.NaN())
	if err := c.Validate(); err == nil {
		t.Fatal("NaN weight accepted")
	}
	c = cloneClassifier(m)
	c.B3 = float32(math.Inf(1))
	if err := c.Validate(); err == nil {
		t.Fatal("infinite bias accepted")
	}

	for _, threshold := range []float64{0, 1, -0.5, 1.5, math.NaN(), math.Inf(1)} {
		c = cloneClassifier(m)
		c.Threshold = threshold
		if err := c.Validate(); err == nil {
			t.Fatalf("threshold %v accepted", threshold)
		}
	}
}

func TestLoadClassifierRoundTripAndLegacy(t *testing.T) {
	dir := t.TempDir()
	m := newTestClassifier(t, 3)
	m.Threshold = 0.62
	m.Trained = map[string]any{"samples": 16.0, "recall": 0.95}

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "model.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadClassifier(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, m) {
		t.Fatalf("round trip changed the model:\n got %+v\nwant %+v", loaded, m)
	}

	legacy := `{"version":2,"id":"old","display":"Old","threshold":0.5,"frames":16,` +
		`"mel_bins":32,"hidden":0,"mean":[],"std":[],"w2":[],"b2":0}`
	legacyPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadClassifier(legacyPath)
	if err == nil {
		t.Fatal("legacy raw-mel model loaded")
	}
	var bad ErrBadModel
	if !errors.As(err, &bad) || !strings.Contains(bad.Reason, "retrain") {
		t.Fatalf("legacy load error = %v, want retrainable ErrBadModel", err)
	}
}

func TestClassifierScore(t *testing.T) {
	m := newTestClassifier(t, 4)
	window := make([]float32, ContextFrames*EmbeddingDim)
	if got := m.Score(window); math.IsNaN(got) || got <= 0 || got >= 1 {
		t.Fatalf("score = %v, want a probability in (0,1)", got)
	}
	if got := m.Score(window[:len(window)-1]); !math.IsNaN(got) {
		t.Fatalf("short window score = %v, want NaN", got)
	}
	if got := m.Score(nil); !math.IsNaN(got) {
		t.Fatalf("nil window score = %v, want NaN", got)
	}

	// A model with no weights is a coin flip for every window.
	zeroed := newTestClassifier(t, 5)
	zeroWeights(zeroed)
	for _, fill := range []float32{0, 1, -3} {
		for i := range window {
			window[i] = fill
		}
		if got := zeroed.Score(window); math.Abs(got-0.5) > 1e-12 {
			t.Fatalf("zero model score with fill %v = %v, want 0.5", fill, got)
		}
	}
}

// TestClassifierProjectionSharedAcrossTime pins the contract that one 96x24
// projection is applied to all 16 time steps and its outputs are flattened
// time-major into the hidden layer.
func TestClassifierProjectionSharedAcrossTime(t *testing.T) {
	m := newTestClassifier(t, 6)
	zeroWeights(m)
	m.W1[0] = 1 // projection unit 0 reads embedding component 0
	m.B1[0] = 0
	// Hidden unit 0 sums projection unit 0 from every time step.
	for step := range ContextFrames {
		m.W2[step*ProjectionWidth] = 1
	}
	m.W3[0] = 1
	m.B3 = -1

	window := make([]float32, ContextFrames*EmbeddingDim)
	window[0] = 1
	if got, want := m.Score(window), sigmoid(0); math.Abs(got-want) > 1e-12 {
		t.Fatalf("one active step: score = %v, want %v", got, want)
	}
	window[5*EmbeddingDim] = 1
	if got, want := m.Score(window), sigmoid(1); math.Abs(got-want) > 1e-12 {
		t.Fatalf("two active steps: score = %v, want %v", got, want)
	}
	window[0] = 0
	window[9*EmbeddingDim] = 1
	if got, want := m.Score(window), sigmoid(1); math.Abs(got-want) > 1e-12 {
		t.Fatalf("steps 5 and 9: score = %v, want %v", got, want)
	}
	// Negative projection inputs are clamped, so a negative embedding does not
	// subtract from the sum.
	window[5*EmbeddingDim], window[9*EmbeddingDim] = 0, 0
	window[0] = -1
	if got, want := m.Score(window), sigmoid(-1); math.Abs(got-want) > 1e-12 {
		t.Fatalf("negative input: score = %v, want %v", got, want)
	}
}

func flattenEmbeddings(history [][]float32) []float32 {
	flat := make([]float32, 0, len(history)*EmbeddingDim)
	for _, emb := range history {
		flat = append(flat, emb...)
	}
	return flat
}

// TestEngineMatchesOfflineScore feeds the same PCM to the live engine and to a
// reference oww.Frontend and checks every score, including the warmup rule: the
// engine emits 0 until 16 embeddings exist.
func TestEngineMatchesOfflineScore(t *testing.T) {
	clf := newTestClassifier(t, 42)
	assets, err := oww.Load()
	if err != nil {
		t.Fatal(err)
	}
	reference, err := oww.NewFrontend(assets)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(clf)
	if err != nil {
		t.Fatal(err)
	}

	pcm := make([]int16, 1280*40)
	for i := range pcm {
		pcm[i] = int16((i*37)%4000 - 2000)
	}
	var history [][]float32
	firstScored := -1
	for chunk := 0; chunk < 40; chunk++ {
		samples := pcm[chunk*1280 : (chunk+1)*1280]
		got := engine.Process(samples)
		for _, emb := range reference.Feed(samples) {
			history = append(history, append([]float32(nil), emb...))
		}
		want := 0.0
		if len(history) >= ContextFrames {
			want = clf.Score(flattenEmbeddings(history[len(history)-ContextFrames:]))
			if firstScored < 0 {
				firstScored = chunk
			}
		}
		if got != want {
			t.Fatalf("chunk %d: engine score %v, offline score %v", chunk, got, want)
		}
	}
	// The first embedding lands at sample 12512, the sixteenth at 31712, so
	// chunk 25 (index 24) is the first that can score.
	if firstScored != 24 {
		t.Fatalf("first offline score at chunk %d, want 24", firstScored)
	}
}

func TestEngineResetAndValidation(t *testing.T) {
	clf := newTestClassifier(t, 8)
	if _, err := NewEngine(nil); err == nil {
		t.Fatal("nil classifier accepted")
	}
	legacy := cloneClassifier(clf)
	legacy.Version = 2
	if _, err := NewEngine(legacy); err == nil {
		t.Fatal("legacy classifier accepted")
	}

	engine, err := NewEngine(clf)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, 1280)
	for i := range pcm {
		pcm[i] = int16(i % 500)
	}
	var scored bool
	for i := 0; i < 30; i++ {
		if engine.Process(pcm) != 0 {
			scored = true
			break
		}
	}
	if !scored {
		t.Fatal("engine never scored a full context")
	}
	engine.Reset()
	if got := engine.Process(pcm); got != 0 {
		t.Fatalf("score after reset = %v, want 0", got)
	}
}

func TestClassifierInputProcessingTap(t *testing.T) {
	clf := NewClassifier("word.personal", "word", "en", 1)
	if _, ok := clf.InputProcessingTap(); ok {
		t.Fatal("new classifier declared input provenance")
	}
	clf.Trained = map[string]any{"input_audio": map[string]any{"processing_tap": "framework.dsp.v1"}}
	tap, ok := clf.InputProcessingTap()
	if !ok || tap != "framework.dsp.v1" {
		t.Fatalf("declared tap = %q, declared=%v", tap, ok)
	}
	// A JSON round trip must preserve the declaration; model files carry it.
	raw, err := json.Marshal(clf)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Classifier
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if tap, ok := decoded.InputProcessingTap(); !ok || tap != "framework.dsp.v1" {
		t.Fatalf("round-trip tap = %q, declared=%v", tap, ok)
	}
}
