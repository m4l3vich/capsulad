package wakeword

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
)

// Classifier dimensions. A classifier runs on the 96-dimensional embedding
// stream produced by oww.Frontend: the same projection is applied to each of
// the last 16 time steps (80 ms hop), then the 16x24 projected features are
// flattened and passed through one ReLU hidden layer and a sigmoid.
const (
	// ClassifierVersion is the only schema the device loads. Version 1 and 2
	// trained on raw mel frames and are not convertible: their weights have a
	// different meaning.
	ClassifierVersion = 3

	// ContextFrames is how many embeddings (one per 80 ms) the classifier sees.
	ContextFrames = 16
	// EmbeddingDim is the oww speech-embedding width.
	EmbeddingDim = 96
	// ProjectionWidth is the per-time-step bottleneck width.
	ProjectionWidth = 24
	// HiddenWidth is the width of the layer over the flattened projection.
	HiddenWidth = 24

	projectionInputs = ContextFrames * ProjectionWidth // 384
)

// Classifier is one trained keyword model. It consumes embeddings, never raw
// audio, so the device only pays for a small network.
//
// Version 3 forward pass, with x[t] the embedding at time step t:
//
//	p[t][c] = relu(b1[c] + sum_i w1[c][i] * x[t][i])   (shared over 16 steps)
//	h       = relu(b2 + w2 * flatten(p))               (384 -> 24)
//	score   = sigmoid(b3 + w3 . h)
//
// The projection matrix is shared across time steps, which is what makes a
// 16-frame context affordable on the device. All parameters are stored flat in
// the JSON so the format stays stable.
type Classifier struct {
	Version  int    `json:"version"`
	ID       string `json:"id"`
	Display  string `json:"display"`
	Language string `json:"language"`
	// Threshold is the decision threshold chosen by the trainer. It is part of
	// the artifact because calibration happens during training.
	Threshold float64 `json:"threshold"`

	ContextFrames int `json:"context_frames"`
	EmbeddingDim  int `json:"embedding_dim"`
	Projection    int `json:"projection"`
	Hidden        int `json:"hidden"`

	W1 []float32 `json:"w1"` // [Projection][EmbeddingDim]
	B1 []float32 `json:"b1"` // [Projection]
	W2 []float32 `json:"w2"` // [Hidden][ContextFrames*Projection]
	B2 []float32 `json:"b2"` // [Hidden]
	W3 []float32 `json:"w3"` // [Hidden]
	B3 float32   `json:"b3"`

	// Trained records the provenance and metrics the trainer reported.
	Trained map[string]any `json:"trained,omitempty"`
}

// ErrBadModel reports an unusable classifier file.
type ErrBadModel struct{ Reason string }

func (e ErrBadModel) Error() string { return "wakeword model: " + e.Reason }

func finite32(v float32) bool {
	return !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0)
}

func checkFinite(name string, values []float32) error {
	for _, v := range values {
		if !finite32(v) {
			return ErrBadModel{name + " contains a non-finite value"}
		}
	}
	return nil
}

func checkWeightLen(name string, got, want int) error {
	if got != want {
		return ErrBadModel{fmt.Sprintf("%s has %d values, want %d", name, got, want)}
	}
	return nil
}

// Validate checks the schema, dimensions, lengths and value ranges. It never
// substitutes a default probability: an out-of-range threshold is an error so a
// broken artifact cannot silently turn into an always-on or never-on detector.
func (m *Classifier) Validate() error {
	if m == nil {
		return ErrBadModel{"nil classifier"}
	}
	if m.Version != ClassifierVersion {
		if m.Version == 1 || m.Version == 2 {
			return ErrBadModel{fmt.Sprintf(
				"version %d is a legacy raw-mel model; retrain it with the embedding trainer (waketrain)",
				m.Version)}
		}
		return ErrBadModel{fmt.Sprintf("unsupported version %d, want %d; retrain the model",
			m.Version, ClassifierVersion)}
	}
	if m.ID == "" {
		return ErrBadModel{"missing id"}
	}
	if m.ContextFrames != ContextFrames {
		return ErrBadModel{fmt.Sprintf("context_frames must be %d, got %d", ContextFrames, m.ContextFrames)}
	}
	if m.EmbeddingDim != EmbeddingDim {
		return ErrBadModel{fmt.Sprintf("embedding_dim must be %d, got %d", EmbeddingDim, m.EmbeddingDim)}
	}
	if m.Projection != ProjectionWidth {
		return ErrBadModel{fmt.Sprintf("projection must be %d, got %d", ProjectionWidth, m.Projection)}
	}
	if m.Hidden != HiddenWidth {
		return ErrBadModel{fmt.Sprintf("hidden must be %d, got %d", HiddenWidth, m.Hidden)}
	}
	if err := checkWeightLen("w1", len(m.W1), ProjectionWidth*EmbeddingDim); err != nil {
		return err
	}
	if err := checkWeightLen("b1", len(m.B1), ProjectionWidth); err != nil {
		return err
	}
	if err := checkWeightLen("w2", len(m.W2), HiddenWidth*projectionInputs); err != nil {
		return err
	}
	if err := checkWeightLen("b2", len(m.B2), HiddenWidth); err != nil {
		return err
	}
	if err := checkWeightLen("w3", len(m.W3), HiddenWidth); err != nil {
		return err
	}
	for name, values := range map[string][]float32{
		"w1": m.W1, "b1": m.B1, "w2": m.W2, "b2": m.B2, "w3": m.W3,
	} {
		if err := checkFinite(name, values); err != nil {
			return err
		}
	}
	if !finite32(m.B3) {
		return ErrBadModel{"b3 is not finite"}
	}
	if math.IsNaN(m.Threshold) || math.IsInf(m.Threshold, 0) ||
		m.Threshold <= 0 || m.Threshold >= 1 {
		return ErrBadModel{fmt.Sprintf("threshold %v must be a finite probability in (0,1)", m.Threshold)}
	}
	if m.Display == "" {
		m.Display = m.ID
	}
	return nil
}

// NewClassifier builds an untrained version 3 model with deterministic weight
// initialisation. The trainer overwrites the weights and threshold.
func NewClassifier(id, display, language string, seed uint64) *Classifier {
	// PCG wants two words; mixing a constant keeps nearby seeds independent.
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return &Classifier{
		Version:       ClassifierVersion,
		ID:            id,
		Display:       display,
		Language:      language,
		Threshold:     0.5,
		ContextFrames: ContextFrames,
		EmbeddingDim:  EmbeddingDim,
		Projection:    ProjectionWidth,
		Hidden:        HiddenWidth,
		W1:            randomUniform(rng, ProjectionWidth*EmbeddingDim, 1/math.Sqrt(EmbeddingDim)),
		B1:            make([]float32, ProjectionWidth),
		W2:            randomUniform(rng, HiddenWidth*projectionInputs, 1/math.Sqrt(projectionInputs)),
		B2:            make([]float32, HiddenWidth),
		W3:            randomUniform(rng, HiddenWidth, 1/math.Sqrt(HiddenWidth)),
	}
}

// randomUniform returns n values in [-limit, limit), drawn from rng. Using a
// caller-provided seed keeps training runs reproducible.
func randomUniform(rng *rand.Rand, n int, limit float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32((rng.Float64()*2 - 1) * limit)
	}
	return out
}

// InputProcessingTap reports the capture preprocessing the trainer declared
// for this model. declared=false means the artifact predates input provenance
// (or was hand-built): it was trained on the raw physical tap if it came from
// an earlier build. Callers must not reinterpret that as framework-processed.
func (m *Classifier) InputProcessingTap() (tap string, declared bool) {
	if m == nil || m.Trained == nil {
		return "", false
	}
	raw, ok := m.Trained["input_audio"]
	if !ok {
		return "", false
	}
	entry, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	tap, _ = entry["processing_tap"].(string)
	return tap, true
}

// LoadClassifier reads and validates a model file. Legacy files fail with a
// retrain message rather than being reinterpreted.
func LoadClassifier(path string) (*Classifier, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Inspect the version before decoding version-specific tensor shapes:
	// legacy b2 is a scalar, whereas v3 b2 is a vector.
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, ErrBadModel{err.Error()}
	}
	if header.Version != ClassifierVersion {
		return nil, (&Classifier{Version: header.Version}).Validate()
	}
	var m Classifier
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, ErrBadModel{err.Error()}
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
