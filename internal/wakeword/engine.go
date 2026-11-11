package wakeword

import (
	"fmt"

	"capsulad/internal/wakeword/oww"
)

// EmbeddingEngine is the device inference path: oww.Frontend turns 16 kHz PCM
// into the 96-dim embedding stream, and the classifier scores the newest window
// of ContextFrames embeddings. It consumes the runner's 80 ms windows and keeps
// only the classifier context, never raw audio.
type EmbeddingEngine struct {
	frontend *oww.Frontend
	clf      *Classifier

	// context holds the last ContextFrames embeddings, oldest first, flat and
	// time-major so it can be scored without copying a window.
	context [ContextFrames * EmbeddingDim]float32
	ready   int
	// lastScore is the score of the newest full window. Repeating it when a
	// chunk completes no embedding keeps a caller that feeds smaller chunks
	// from seeing an artificial 0 between real windows.
	lastScore float64

	scratch networkScratch
}

// NewEngine builds the engine from a validated classifier. Assets are the
// embedded Apache-2.0 openWakeWord weights; the frontend owns all feature state.
func NewEngine(clf *Classifier) (*EmbeddingEngine, error) {
	if clf == nil {
		return nil, fmt.Errorf("wakeword: nil classifier")
	}
	if err := clf.Validate(); err != nil {
		return nil, err
	}
	assets, err := oww.Load()
	if err != nil {
		return nil, err
	}
	frontend, err := oww.NewFrontend(assets)
	if err != nil {
		return nil, err
	}
	return &EmbeddingEngine{frontend: frontend, clf: clf}, nil
}

// Reset clears all streaming state: the mel/embedding phase and the classifier
// context. The runner calls it on mute, a capture gap and after an activation,
// so audio from opposite sides of the break can never share a window.
func (e *EmbeddingEngine) Reset() {
	e.frontend.Reset()
	e.ready = 0
	e.lastScore = 0
}

// Process consumes one audio chunk and returns the classifier score for the
// newest full context window. It returns 0 until ContextFrames embeddings have
// been accumulated, which is the warmup the runner waits out.
func (e *EmbeddingEngine) Process(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	for _, emb := range e.frontend.Feed(samples) {
		e.push(emb)
		if e.ready < ContextFrames {
			continue
		}
		e.lastScore = e.clf.forward(e.context[:], &e.scratch)
	}
	if e.ready < ContextFrames {
		return 0
	}
	return e.lastScore
}

// push appends one embedding and evicts the oldest once the context is full.
func (e *EmbeddingEngine) push(emb []float32) {
	if e.ready < ContextFrames {
		copy(e.context[e.ready*EmbeddingDim:], emb)
		e.ready++
		return
	}
	copy(e.context[:], e.context[EmbeddingDim:])
	copy(e.context[(ContextFrames-1)*EmbeddingDim:], emb)
}

var _ Engine = (*EmbeddingEngine)(nil)
