package wakeword

import (
	"fmt"
	"math"

	"capsulad/internal/wakeword/oww"
)

// An Ensemble runs N trained keyword heads over one pretrained embedding
// frontend. That sharing is the whole point on this device: a complete frontend
// emits an embedding in 37.9 ms against an 80 ms hop, so N frontends (N times
// 38 ms) cannot keep up while N heads over the one embedding stream cost
// 0.2–0.4 ms each. Memory is likewise bounded: an in-memory head (weights plus
// its widened window) is about 53 KB per word, while the persisted JSON artifact
// is about 130 KB per word, and both sit under this one shared 1.15 MB frontend.
// The two numbers measure different things and are never interchangeable.
//
// The embedding ring is float64 and shared: heads keep only a per-head window
// and activations. Reset clears the frontend phase, the ring and every head, so
// audio from opposite sides of a mute, capture gap or activation can never
// complete one context.
type Ensemble struct {
	frontend *oww.Frontend

	// ring holds the newest ContextFrames embeddings, flat and time-major. head
	// is the next write slot, so once the ring is full the oldest embedding
	// starts at head and the window is ring[head:] followed by ring[:head].
	ring   [ContextFrames * EmbeddingDim]float64
	head   int
	filled int

	heads  []*ensembleHead
	scores []float64
}

type ensembleHead struct {
	clf *Classifier
	// window is this head's chronological view of the shared ring, widened into
	// the order forward64 expects. It is fixed-size, so a hop allocates nothing.
	window  [ContextFrames * EmbeddingDim]float64
	scratch networkScratch
}

// NewEnsemble validates every classifier and builds the single shared frontend.
// Matching the offline path, an invalid artifact fails the whole build rather
// than silently leaving one word out of the running detector.
func NewEnsemble(classifiers []*Classifier) (*Ensemble, error) {
	if len(classifiers) == 0 {
		return nil, fmt.Errorf("wakeword: ensemble needs at least one classifier")
	}
	heads := make([]*ensembleHead, 0, len(classifiers))
	for i, clf := range classifiers {
		if clf == nil {
			return nil, fmt.Errorf("wakeword: nil classifier at position %d", i)
		}
		if err := clf.Validate(); err != nil {
			return nil, err
		}
		heads = append(heads, &ensembleHead{clf: clf})
	}
	assets, err := oww.Load()
	if err != nil {
		return nil, err
	}
	frontend, err := oww.NewFrontend(assets)
	if err != nil {
		return nil, err
	}
	return &Ensemble{
		frontend: frontend,
		heads:    heads,
		scores:   make([]float64, len(heads)),
	}, nil
}

// Len is the number of heads.
func (e *Ensemble) Len() int { return len(e.heads) }

// Classifier is the head at position i.
func (e *Ensemble) Classifier(i int) *Classifier { return e.heads[i].clf }

// Process consumes one audio chunk and returns the score of every head for the
// newest complete context. The returned slice is reused; callers must read it
// before the next call. Until ContextFrames embeddings exist every score is 0,
// which is the warm-up the runner waits out (about two seconds of audio).
func (e *Ensemble) Process(samples []int16) []float64 {
	if len(samples) == 0 {
		return e.scores
	}
	for _, emb := range e.frontend.Feed(samples) {
		dst := e.ring[e.head*EmbeddingDim : (e.head+1)*EmbeddingDim]
		for i, v := range emb {
			dst[i] = float64(v)
		}
		e.head = (e.head + 1) % ContextFrames
		if e.filled < ContextFrames {
			e.filled++
			if e.filled < ContextFrames {
				continue
			}
		}
		e.scoreAll()
	}
	if e.filled < ContextFrames {
		clear(e.scores)
	}
	return e.scores
}

// scoreAll scores every head against the newest complete ring window. It is the
// steady-state per-hop work: fixed-size windows and scratch, no allocation.
func (e *Ensemble) scoreAll() {
	for i, h := range e.heads {
		e.scores[i] = h.score(e.ring[:], e.head)
	}
}

// Reset clears all streaming state: the mel/embedding phase, the shared ring and
// every score. The runner calls it on mute, a capture gap and after an
// activation.
func (e *Ensemble) Reset() {
	e.frontend.Reset()
	e.head = 0
	e.filled = 0
	clear(e.scores)
}

// score materializes this head's chronological window from the shared ring and
// runs the classifier over it.
func (h *ensembleHead) score(ring []float64, start int) float64 {
	lead := (ContextFrames - start) * EmbeddingDim
	copy(h.window[:lead], ring[start*EmbeddingDim:])
	copy(h.window[lead:], ring[:start*EmbeddingDim])
	return h.clf.forward64(h.window[:], &h.scratch)
}

// DetectionSpec is one head's activation policy: which model it belongs to and
// the threshold calibrated for that artifact. The threshold is per model because
// calibration is per model; the cooldown is global because one utterance must
// not fire two words.
type DetectionSpec struct {
	ModelID   string
	Threshold float64
}

// normalizeThreshold keeps a malformed spec from turning into an always-on
// detector. Validation happens when the ensemble is built from artifacts; this
// is the last line of defence for a hand-built spec.
func normalizeThreshold(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v >= 1 {
		return 0.5
	}
	return v
}

var _ ScoreEngine = (*Ensemble)(nil)
