package wakeword

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"sort"
	"sync"
)

// TrainingExample is one classifier window. Features is ContextFrames*EmbeddingDim
// float32 values (time-major embeddings). Callers build it as a view into an
// immutable embedding stream: the optimizer never copies feature windows.
type TrainingExample struct {
	Features []float32
	Positive bool
}

// FitOptions tunes FitClassifier. Zero values select the defaults: 30 epochs,
// batch size 32, learning rate 0.003, positive weight 1, the whole negative
// pool for the random draw, and no hard-negative add-on.
type FitOptions struct {
	Seed         uint64
	Epochs       int
	BatchSize    int
	LearningRate float64
	// RandomNegativesPerEpoch is how many negatives each epoch draws uniformly
	// without replacement from the whole pool. The draw is fresh every epoch, so
	// coverage never depends on an earlier selection. Zero (or a value above the
	// pool size) selects the whole pool.
	RandomNegativesPerEpoch int
	// HardNegativesPerEpoch adds a separate set of the currently highest-scoring
	// negatives on top of the random draw; it never replaces random coverage.
	// The set is refreshed every miningInterval epochs. Zero disables the add-on.
	// The effective size is capped by the pool and by miningPoolCap.
	HardNegativesPerEpoch int
	PositiveWeight        float64
	// Progress is called once per epoch with a 1-based epoch number and the
	// weighted binary cross-entropy over that epoch.
	Progress func(epoch int, loss float64)
}

const (
	defaultFitEpochs       = 30
	defaultFitBatchSize    = 32
	defaultFitLearningRate = 0.003

	// l2Decay keeps the small network's weights bounded; it applies to the
	// weight matrices, not to biases.
	l2Decay = 1e-4

	adamBeta1   = 0.9
	adamBeta2   = 0.999
	adamEpsilon = 1e-8

	// Hard-negative mining re-evaluates the pool every miningInterval epochs
	// and keeps a bounded amount of work: at most miningPoolCap windows are
	// scored, on at most miningWorkerCap goroutines. Windows are views, so the
	// extra memory is one score per candidate.
	miningInterval  = 5
	miningPoolCap   = 32768
	miningWorkerCap = 4
)

// FitClassifier trains model in place with minibatch Adam over weighted binary
// cross-entropy and L2 decay. It is deterministic for a given model, data,
// seed, and options. The model is replaced only after a complete successful
// run: cancellation or an error leaves the caller's model untouched.
//
// Each epoch trains on every positive plus RandomNegativesPerEpoch negatives
// drawn fresh and uniformly without replacement from the whole pool, so broad
// negative coverage is renewed every epoch instead of persisting from a single
// pre-selection. A separate hard set of HardNegativesPerEpoch negatives is
// added on top (duplicates with the random draw are allowed, because the set is
// extra exposure) and refreshed every miningInterval epochs from the currently
// highest-scoring windows of a bounded pool-wide sample. Mining never replaces
// the random draw.
func FitClassifier(ctx context.Context, model *Classifier, positives, negatives []TrainingExample, opts FitOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if model == nil {
		return errors.New("wakeword: FitClassifier: nil model")
	}
	if err := model.Validate(); err != nil {
		return err
	}
	if len(positives) == 0 {
		return errors.New("wakeword: FitClassifier: no positive examples")
	}
	if err := checkExamples("positives", positives); err != nil {
		return err
	}
	if err := checkExamples("negatives", negatives); err != nil {
		return err
	}
	epochs := opts.Epochs
	if epochs <= 0 {
		epochs = defaultFitEpochs
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultFitBatchSize
	}
	learningRate := opts.LearningRate
	if learningRate <= 0 {
		learningRate = defaultFitLearningRate
	}
	if math.IsNaN(learningRate) || math.IsInf(learningRate, 0) {
		return fmt.Errorf("wakeword: FitClassifier: learning rate %v is not finite", learningRate)
	}
	positiveWeight := opts.PositiveWeight
	if positiveWeight <= 0 {
		positiveWeight = 1
	}
	if math.IsNaN(positiveWeight) || math.IsInf(positiveWeight, 0) {
		return fmt.Errorf("wakeword: FitClassifier: positive weight %v is not finite", positiveWeight)
	}
	randomPerEpoch := opts.RandomNegativesPerEpoch
	if randomPerEpoch <= 0 || randomPerEpoch > len(negatives) {
		randomPerEpoch = len(negatives)
	}
	hardPerEpoch := opts.HardNegativesPerEpoch
	if hardPerEpoch < 0 {
		hardPerEpoch = 0
	}
	if hardPerEpoch > len(negatives) {
		hardPerEpoch = len(negatives)
	}
	if hardPerEpoch > miningPoolCap {
		hardPerEpoch = miningPoolCap
	}

	trained := cloneClassifier(model)
	rng := rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15))
	grads := newGradients()
	moments := newAdamMoments()
	// One reusable index range and order slice: every epoch reshuffles the same
	// backing arrays instead of allocating new sampling buffers.
	indexRange := make([]int, 0, len(negatives))
	order := make([]int, 0, len(positives)+randomPerEpoch+hardPerEpoch)
	var hard []int
	var scratch networkScratch
	step := 0

	for epoch := 1; epoch <= epochs; epoch++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hardPerEpoch > 0 && (epoch == 1 || (epoch-1)%miningInterval == 0) {
			hard = mineHardNegatives(ctx, trained, negatives, hardPerEpoch)
			if err := ctx.Err(); err != nil {
				return err
			}
		}

		indexRange = fillNegativeIndices(indexRange, len(negatives))
		random := sampleNegativeIndices(indexRange, randomPerEpoch, rng)
		order = order[:0]
		for i := range positives {
			order = append(order, i)
		}
		for _, index := range random {
			order = append(order, -(index + 1))
		}
		for _, index := range hard {
			order = append(order, -(index + 1))
		}
		shuffleIndices(order, rng)

		var lossSum, weightSum float64
		for start := 0; start < len(order); start += batchSize {
			end := min(start+batchSize, len(order))
			batch := order[start:end]

			batchWeight := 0.0
			for _, code := range batch {
				batchWeight += exampleWeight(code, positiveWeight)
			}
			if batchWeight <= 0 {
				continue
			}
			var batchLoss float64
			for n, code := range batch {
				if n%64 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				ex := trainingExample(code, positives, negatives)
				p := accumulateGradient(trained, ex, positiveWeight, &scratch, grads)
				weight := exampleWeight(code, positiveWeight)
				batchLoss += weight * binaryCrossEntropy(p, ex.Positive)
			}
			lossSum += batchLoss
			weightSum += batchWeight

			scaleGradients(grads, 1/batchWeight)
			addL2Gradients(trained, grads)
			step++
			bc1 := 1 - math.Pow(adamBeta1, float64(step))
			bc2 := 1 - math.Pow(adamBeta2, float64(step))
			applyAdam(trained, grads, moments, bc1, bc2, learningRate)
			resetGradients(grads)
		}

		if opts.Progress != nil {
			loss := 0.0
			if weightSum > 0 {
				loss = lossSum / weightSum
			}
			opts.Progress(epoch, loss)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := trained.Validate(); err != nil {
		return err
	}
	model.W1, model.B1 = trained.W1, trained.B1
	model.W2, model.B2 = trained.W2, trained.B2
	model.W3, model.B3 = trained.W3, trained.B3
	return nil
}

func checkExamples(name string, examples []TrainingExample) error {
	for i := range examples {
		if n := len(examples[i].Features); n != ContextFrames*EmbeddingDim {
			return fmt.Errorf("wakeword: FitClassifier: %s[%d] has %d features, want %d",
				name, i, n, ContextFrames*EmbeddingDim)
		}
	}
	return nil
}

func exampleWeight(code int, positiveWeight float64) float64 {
	if code >= 0 {
		return positiveWeight
	}
	return 1
}

// trainingExample selects an example by its encoded position. The slice it came
// from is the authoritative label: the Positive flag is derived here rather than
// trusted, so a caller that leaves the redundant field at its zero value cannot
// silently train positives as negatives.
func trainingExample(code int, positives, negatives []TrainingExample) TrainingExample {
	if code >= 0 {
		ex := positives[code]
		ex.Positive = true
		return ex
	}
	ex := negatives[-code-1]
	ex.Positive = false
	return ex
}

// fillNegativeIndices resets dst to the identity range 0..n-1 in the existing
// backing array. Reusing the caller's buffer keeps the per-epoch draw free of
// allocations.
func fillNegativeIndices(dst []int, n int) []int {
	if cap(dst) < n {
		dst = make([]int, n)
	} else {
		dst = dst[:n]
	}
	for i := range dst {
		dst[i] = i
	}
	return dst
}

// sampleNegativeIndices chooses k distinct positions by partially shuffling
// indices in place. The returned slice aliases indices and stays valid until
// the caller refills it. k is clamped to the range length.
func sampleNegativeIndices(indices []int, k int, rng *rand.Rand) []int {
	if k >= len(indices) {
		return indices
	}
	for i := range k {
		j := i + rng.IntN(len(indices)-i)
		indices[i], indices[j] = indices[j], indices[i]
	}
	return indices[:k]
}

func shuffleIndices(order []int, rng *rand.Rand) {
	for i := len(order) - 1; i > 0; i-- {
		j := rng.IntN(i + 1)
		order[i], order[j] = order[j], order[i]
	}
}

// mineHardNegatives scores a bounded sample of the negative pool with the
// current weights and returns up to k highest-scoring indices from that sample.
// The scored sample is at most miningPoolCap windows (strided across the whole
// pool, so one long stream cannot crowd out others) and k is additionally
// capped by the sample size. The hard set is additive: it is never mixed with
// the fresh random draw and never replaces random coverage. Cancellation stops
// scoring early and returns no selection.
func mineHardNegatives(ctx context.Context, m *Classifier, negatives []TrainingExample, k int) []int {
	if k <= 0 || len(negatives) == 0 || ctx.Err() != nil {
		return nil
	}
	if k > len(negatives) {
		k = len(negatives)
	}
	stride := 1
	if len(negatives) > miningPoolCap {
		stride = (len(negatives) + miningPoolCap - 1) / miningPoolCap
	}
	type scoredNegative struct {
		index int
		score float64
	}
	pool := make([]scoredNegative, 0, len(negatives)/stride+1)
	for i := 0; i < len(negatives); i += stride {
		pool = append(pool, scoredNegative{index: i})
	}
	if k > len(pool) {
		k = len(pool)
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > miningWorkerCap {
		workers = miningWorkerCap
	}
	if workers < 1 {
		workers = 1
	}
	chunk := (len(pool) + workers - 1) / workers
	var wg sync.WaitGroup
	for w := range workers {
		lo := w * chunk
		hi := min(lo+chunk, len(pool))
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			var scratch networkScratch
			for i := lo; i < hi; i++ {
				if ctx.Err() != nil {
					return
				}
				pool[i].score = m.forward(negatives[pool[i].index].Features, &scratch)
			}
		}(lo, hi)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}

	sort.Slice(pool, func(a, b int) bool {
		if pool[a].score != pool[b].score {
			return pool[a].score > pool[b].score
		}
		return pool[a].index < pool[b].index
	})
	selected := make([]int, k)
	for i := range selected {
		selected[i] = pool[i].index
	}
	return selected
}

func binaryCrossEntropy(p float64, positive bool) float64 {
	const eps = 1e-12
	if positive {
		return -math.Log(max(p, eps))
	}
	return -math.Log(max(1-p, eps))
}

// gradients holds one float64 accumulator per parameter. Accumulating in
// float64 keeps a batch of small updates from rounding to nothing in float32.
type gradients struct {
	w1, b1, w2, b2, w3 []float64
	b3                 float64
}

func newGradients() *gradients {
	return &gradients{
		w1: make([]float64, ProjectionWidth*EmbeddingDim),
		b1: make([]float64, ProjectionWidth),
		w2: make([]float64, HiddenWidth*projectionInputs),
		b2: make([]float64, HiddenWidth),
		w3: make([]float64, HiddenWidth),
	}
}

func resetGradients(g *gradients) {
	clear(g.w1)
	clear(g.b1)
	clear(g.w2)
	clear(g.b2)
	clear(g.w3)
	g.b3 = 0
}

func scaleGradients(g *gradients, scale float64) {
	for i := range g.w1 {
		g.w1[i] *= scale
	}
	for i := range g.b1 {
		g.b1[i] *= scale
	}
	for i := range g.w2 {
		g.w2[i] *= scale
	}
	for i := range g.b2 {
		g.b2[i] *= scale
	}
	for i := range g.w3 {
		g.w3[i] *= scale
	}
	g.b3 *= scale
}

func addL2Gradients(m *Classifier, g *gradients) {
	for i, w := range m.W1 {
		g.w1[i] += l2Decay * float64(w)
	}
	for i, w := range m.W2 {
		g.w2[i] += l2Decay * float64(w)
	}
	for i, w := range m.W3 {
		g.w3[i] += l2Decay * float64(w)
	}
}

// accumulateGradient performs the forward pass and one backward pass for an
// example, adds the gradients, and returns the forward probability for the
// epoch loss.
func accumulateGradient(m *Classifier, ex TrainingExample, positiveWeight float64, s *networkScratch, g *gradients) float64 {
	p := m.forward(ex.Features, s)
	weight, target := 1.0, 0.0
	if ex.Positive {
		weight, target = positiveWeight, 1
	}
	dz := (p - target) * weight

	// Output layer and hidden-layer derivative.
	g.b3 += dz
	for j := range HiddenWidth {
		g.w3[j] += dz * s.hidden[j]
		dh := dz * float64(m.W3[j])
		if s.hiddenPre[j] <= 0 {
			dh = 0
		}
		s.dHidden[j] = dh
		g.b2[j] += dh
		if dh != 0 {
			dst := g.w2[j*projectionInputs : (j+1)*projectionInputs]
			for k := range projectionInputs {
				dst[k] += dh * s.proj[k]
			}
		}
	}
	// Back through the flattening into each projected time step.
	for k := range projectionInputs {
		var dp float64
		for j := range HiddenWidth {
			if s.dHidden[j] != 0 {
				dp += float64(m.W2[j*projectionInputs+k]) * s.dHidden[j]
			}
		}
		if s.proj[k] <= 0 {
			dp = 0
		}
		s.dProj[k] = dp
	}
	for t := range ContextFrames {
		base := t * EmbeddingDim
		for c := range ProjectionWidth {
			dp := s.dProj[t*ProjectionWidth+c]
			if dp == 0 {
				continue
			}
			g.b1[c] += dp
			dst := g.w1[c*EmbeddingDim : (c+1)*EmbeddingDim]
			for i := range EmbeddingDim {
				dst[i] += dp * float64(ex.Features[base+i])
			}
		}
	}
	return p
}

type adamMoments struct {
	m1, v1   []float64
	m2, v2   []float64
	m3, v3   []float64
	mb1, vb1 []float64
	mb2, vb2 []float64
	mb3, vb3 float64
}

func newAdamMoments() *adamMoments {
	return &adamMoments{
		m1:  make([]float64, ProjectionWidth*EmbeddingDim),
		v1:  make([]float64, ProjectionWidth*EmbeddingDim),
		m2:  make([]float64, HiddenWidth*projectionInputs),
		v2:  make([]float64, HiddenWidth*projectionInputs),
		m3:  make([]float64, HiddenWidth),
		v3:  make([]float64, HiddenWidth),
		mb1: make([]float64, ProjectionWidth),
		vb1: make([]float64, ProjectionWidth),
		mb2: make([]float64, HiddenWidth),
		vb2: make([]float64, HiddenWidth),
	}
}

func adamUpdate(weights []float32, grad, m, v []float64, bc1, bc2, lr float64) {
	for i := range weights {
		g := grad[i]
		m[i] = adamBeta1*m[i] + (1-adamBeta1)*g
		v[i] = adamBeta2*v[i] + (1-adamBeta2)*g*g
		step := lr * (m[i] / bc1) / (math.Sqrt(v[i]/bc2) + adamEpsilon)
		weights[i] = float32(float64(weights[i]) - step)
	}
}

func applyAdam(m *Classifier, g *gradients, a *adamMoments, bc1, bc2, lr float64) {
	adamUpdate(m.W1, g.w1, a.m1, a.v1, bc1, bc2, lr)
	adamUpdate(m.B1, g.b1, a.mb1, a.vb1, bc1, bc2, lr)
	adamUpdate(m.W2, g.w2, a.m2, a.v2, bc1, bc2, lr)
	adamUpdate(m.B2, g.b2, a.mb2, a.vb2, bc1, bc2, lr)
	adamUpdate(m.W3, g.w3, a.m3, a.v3, bc1, bc2, lr)

	a.mb3 = adamBeta1*a.mb3 + (1-adamBeta1)*g.b3
	a.vb3 = adamBeta2*a.vb3 + (1-adamBeta2)*g.b3*g.b3
	m.B3 = float32(float64(m.B3) - lr*(a.mb3/bc1)/(math.Sqrt(a.vb3/bc2)+adamEpsilon))
}

func cloneClassifier(m *Classifier) *Classifier {
	c := *m
	c.W1 = slices.Clone(m.W1)
	c.B1 = slices.Clone(m.B1)
	c.W2 = slices.Clone(m.W2)
	c.B2 = slices.Clone(m.B2)
	c.W3 = slices.Clone(m.W3)
	c.Trained = nil
	return &c
}
