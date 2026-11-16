package wakeword

import "math"

// networkScratch holds the activations of one forward pass. The arrays are
// fixed-size because Validate pins every dimension to its exported constant, so
// scoring a window allocates nothing. The derivative slots are only used by the
// trainer's backward pass.
type networkScratch struct {
	proj      [ContextFrames * ProjectionWidth]float64
	hiddenPre [HiddenWidth]float64
	hidden    [HiddenWidth]float64
	// widened is the float32 window a Score/forward caller hands over, converted
	// once for the float64 kernel. It exists so the two entry points share one
	// implementation instead of two copies of the same arithmetic.
	widened [ContextFrames * EmbeddingDim]float64

	dProj   [ContextFrames * ProjectionWidth]float64
	dHidden [HiddenWidth]float64
}

// Score runs the classifier over one context window and returns P(keyword) in
// [0,1]. The window is ContextFrames*EmbeddingDim float32 values, time-major:
// all 96 embedding components of the oldest step first. A window of the wrong
// length yields NaN rather than a fabricated probability, so a caller that
// skipped validation cannot accidentally read it as a detection.
func (m *Classifier) Score(window []float32) float64 {
	if len(window) != ContextFrames*EmbeddingDim {
		return math.NaN()
	}
	var s networkScratch
	return m.forward(window, &s)
}

// forward is the offline/runtime entry point for a float32 window. Widening to
// float64 is exact, so this is the same arithmetic as forward64 and the two
// produce identical scores for the same embeddings.
func (m *Classifier) forward(x []float32, s *networkScratch) float64 {
	if len(x) != len(s.widened) {
		return math.NaN()
	}
	for i, v := range x {
		s.widened[i] = float64(v)
	}
	return m.forward64(s.widened[:], s)
}

// forward64 is the shared ensemble kernel: it scores a float64 context view so
// N detectors can read one embedding ring without converting or copying a
// window per model. It accumulates in float64 like the trainer, then rounds only
// the sigmoid result, exactly as the float32 entry point does.
func (m *Classifier) forward64(x []float64, s *networkScratch) float64 {
	for t := range ContextFrames {
		base := t * EmbeddingDim
		for c := range ProjectionWidth {
			row := m.W1[c*EmbeddingDim : (c+1)*EmbeddingDim]
			z := float64(m.B1[c])
			for i := range EmbeddingDim {
				z += float64(row[i]) * x[base+i]
			}
			if z < 0 {
				z = 0
			}
			s.proj[t*ProjectionWidth+c] = z
		}
	}
	for j := range HiddenWidth {
		row := m.W2[j*projectionInputs : (j+1)*projectionInputs]
		z := float64(m.B2[j])
		for k := range projectionInputs {
			z += float64(row[k]) * s.proj[k]
		}
		s.hiddenPre[j] = z
		if z < 0 {
			z = 0
		}
		s.hidden[j] = z
	}
	z := float64(m.B3)
	for j := range HiddenWidth {
		z += float64(m.W3[j]) * s.hidden[j]
	}
	return sigmoid(z)
}

// sigmoid is the logistic function, clamped far from the tails so a wildly
// extrapolated score cannot overflow Exp.
func sigmoid(v float64) float64 {
	if v < -60 {
		return 0
	}
	if v > 60 {
		return 1
	}
	return 1 / (1 + math.Exp(-v))
}
