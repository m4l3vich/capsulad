package oww

// WindowFrames is the embedding window (76 mel frames ≈ 0.96 s) and WindowStep
// is how many frames advance between embeddings (8 frames = 80 ms), matching
// the reference pipeline.
const (
	WindowFrames = 76
	WindowStep   = 8
)

// Embedding stream timing, in samples at SampleRate.
//
// The first window ends after the 512-sample analysis frame plus 75 further
// hops: 512 + 75*160 = 12512 samples. Every later window advances by one
// WindowStep of 8 hops = 1280 samples. EmbeddingEnd reports the sample count at
// which embedding index (zero-based) has just become available.
const (
	EmbeddingDim          = 96
	StepSamples           = WindowStep * Hop
	FirstEmbeddingSamples = FFTSize + (WindowFrames-1)*Hop
)

// EmbeddingEnd returns the number of captured samples required before the
// embedding with the given zero-based index can be emitted.
func EmbeddingEnd(index int) int {
	return FirstEmbeddingSamples + index*StepSamples
}

// Frontend turns the device's 16 kHz int16 capture into the 96-dim embedding
// stream a keyword classifier consumes.
type Frontend struct {
	mel *Mel
	net *streamNet

	pending []float64
	frames  []float32 // flat mel history covering [base, total), MelBins per frame
	base    int       // absolute frame index of frames[0]
	total   int       // absolute number of frames produced
	next    int       // absolute frame index at which the next window ends
}

// NewFrontend builds both stages from the embedded assets.
func NewFrontend(a *Assets) (*Frontend, error) {
	mel, err := NewMel(a)
	if err != nil {
		return nil, err
	}
	net, err := newStreamNet(a)
	if err != nil {
		return nil, err
	}
	return &Frontend{mel: mel, net: net, next: WindowFrames}, nil
}

// Reset clears all streaming state (on mute, gap, or after an activation).
// Both the mel FFT phase and every stage's stride phase return to their initial
// position, so audio from opposite sides of the break can never share a window.
func (f *Frontend) Reset() {
	f.mel.Reset()
	f.net.reset()
	f.pending = f.pending[:0]
	f.frames = f.frames[:0]
	f.base = 0
	f.total = 0
	f.next = WindowFrames
}

// Feed consumes 16-bit PCM and returns every embedding completed by this call.
// The values are passed to the mel frontend as raw int16 magnitudes, which is
// what the reference pipeline does.
func (f *Frontend) Feed(samples []int16) [][]float32 {
	if len(samples) == 0 {
		return nil
	}
	for _, s := range samples {
		f.pending = append(f.pending, float64(s))
	}
	newFrames := f.mel.Feed(f.pending)
	f.pending = f.pending[:0]
	var out [][]float32
	for _, frame := range newFrames {
		f.frames = append(f.frames, frame...)
		f.total++
		if f.total != f.next {
			continue
		}
		off := (f.next - WindowFrames - f.base) * MelBins
		window := f.frames[off : off+WindowFrames*MelBins]
		out = append(out, f.net.window(window))
		f.next += WindowStep
		// Keep only the frames future windows can still need. Trimming per
		// emission, not per feed, keeps the mel history bounded even when a
		// caller hands over a long recording in one call.
		if keep := f.next - WindowFrames; keep > f.base {
			f.frames = append(f.frames[:0], f.frames[(keep-f.base)*MelBins:]...)
			f.base = keep
		}
	}
	return out
}
