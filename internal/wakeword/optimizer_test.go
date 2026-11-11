package wakeword

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

func featureExamples(n int, first float32) []TrainingExample {
	examples := make([]TrainingExample, n)
	for i := range examples {
		features := make([]float32, ContextFrames*EmbeddingDim)
		features[0] = first
		examples[i] = TrainingExample{Features: features, Positive: first > 0}
	}
	return examples
}

func TestFitClassifierSeparates(t *testing.T) {
	model := newTestClassifier(t, 7)
	model.Threshold = 0.4
	positives := featureExamples(16, 1)
	negatives := featureExamples(16, -1)

	var epochs []int
	var losses []float64
	opts := FitOptions{
		Seed:                    3,
		Epochs:                  60,
		BatchSize:               8,
		LearningRate:            0.1,
		RandomNegativesPerEpoch: 16,
		Progress: func(epoch int, loss float64) {
			epochs = append(epochs, epoch)
			losses = append(losses, loss)
		},
	}
	if err := FitClassifier(context.Background(), model, positives, negatives, opts); err != nil {
		t.Fatal(err)
	}
	if len(epochs) != opts.Epochs {
		t.Fatalf("progress called %d times, want %d", len(epochs), opts.Epochs)
	}
	for i, epoch := range epochs {
		if epoch != i+1 {
			t.Fatalf("progress epoch %d at index %d", epoch, i)
		}
		if math.IsNaN(losses[i]) || math.IsInf(losses[i], 0) {
			t.Fatalf("epoch %d loss = %v", epoch, losses[i])
		}
	}
	if model.Threshold != 0.4 {
		t.Fatalf("FitClassifier changed threshold to %v", model.Threshold)
	}

	posScore := model.Score(positives[0].Features)
	negScore := model.Score(negatives[0].Features)
	if posScore <= 0.7 || negScore >= 0.3 {
		t.Fatalf("not separated: positive=%v negative=%v", posScore, negScore)
	}
	for _, ex := range positives[1:] {
		if got := model.Score(ex.Features); got <= 0.7 {
			t.Fatalf("positive score = %v", got)
		}
	}
	for _, ex := range negatives[1:] {
		if got := model.Score(ex.Features); got >= 0.3 {
			t.Fatalf("negative score = %v", got)
		}
	}
}

func TestFitClassifierDeterministic(t *testing.T) {
	positives := featureExamples(12, 1)
	negatives := featureExamples(12, -1)
	run := func() (*Classifier, []float64) {
		model := newTestClassifier(t, 21)
		var losses []float64
		opts := FitOptions{
			Seed: 99, Epochs: 6, BatchSize: 8, LearningRate: 0.02,
			RandomNegativesPerEpoch: 6, PositiveWeight: 2,
			Progress: func(_ int, loss float64) { losses = append(losses, loss) },
		}
		if err := FitClassifier(context.Background(), model, positives, negatives, opts); err != nil {
			t.Fatal(err)
		}
		return model, losses
	}
	a, aLosses := run()
	b, bLosses := run()
	if !reflect.DeepEqual(a.W1, b.W1) || !reflect.DeepEqual(a.B1, b.B1) ||
		!reflect.DeepEqual(a.W2, b.W2) || !reflect.DeepEqual(a.B2, b.B2) ||
		!reflect.DeepEqual(a.W3, b.W3) || a.B3 != b.B3 {
		t.Fatal("same seed produced different weights")
	}
	if !reflect.DeepEqual(aLosses, bLosses) {
		t.Fatalf("loss trace differs:\n%v\n%v", aLosses, bLosses)
	}
}

func TestFitClassifierCancellationLeavesModelUntouched(t *testing.T) {
	positives := featureExamples(8, 1)
	negatives := featureExamples(8, -1)
	model := newTestClassifier(t, 31)
	before := cloneClassifier(model)
	opts := FitOptions{Seed: 1, Epochs: 10, BatchSize: 8, LearningRate: 0.02, RandomNegativesPerEpoch: 8}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := FitClassifier(ctx, model, positives, negatives, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fit error = %v", err)
	}
	if !reflect.DeepEqual(model, before) {
		t.Fatal("cancel before start changed the model")
	}

	for _, cancelEpoch := range []int{2, opts.Epochs} {
		ctx, cancel := context.WithCancel(context.Background())
		opts.Progress = func(epoch int, _ float64) {
			if epoch == cancelEpoch {
				cancel()
			}
		}
		err := FitClassifier(ctx, model, positives, negatives, opts)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation at epoch %d: %v", cancelEpoch, err)
		}
		if !reflect.DeepEqual(model, before) {
			t.Fatalf("cancellation at epoch %d committed partial weights", cancelEpoch)
		}
	}
}

func TestFitClassifierValidation(t *testing.T) {
	positives := featureExamples(2, 1)
	negatives := featureExamples(2, -1)
	model := newTestClassifier(t, 41)

	if err := FitClassifier(context.Background(), nil, positives, negatives, FitOptions{}); err == nil {
		t.Fatal("nil model accepted")
	}
	if err := FitClassifier(context.Background(), model, nil, negatives, FitOptions{}); err == nil {
		t.Fatal("empty positives accepted")
	}
	short := []TrainingExample{{Features: make([]float32, 10), Positive: true}}
	if err := FitClassifier(context.Background(), model, short, negatives, FitOptions{}); err == nil {
		t.Fatal("short positive features accepted")
	}
	if err := FitClassifier(context.Background(), model, positives, short, FitOptions{}); err == nil {
		t.Fatal("short negative features accepted")
	}
	if err := FitClassifier(context.Background(), model, positives, negatives,
		FitOptions{LearningRate: math.NaN()}); err == nil {
		t.Fatal("NaN learning rate accepted")
	}
	if err := FitClassifier(context.Background(), model, positives, negatives,
		FitOptions{PositiveWeight: math.Inf(1)}); err == nil {
		t.Fatal("infinite positive weight accepted")
	}
}

func TestFitClassifierAcceptsNilContext(t *testing.T) {
	model := newTestClassifier(t, 51)
	if err := FitClassifier(nil, model, featureExamples(4, 1), featureExamples(4, -1),
		FitOptions{Seed: 2, Epochs: 1, BatchSize: 4, LearningRate: 0.01, RandomNegativesPerEpoch: 4}); err != nil {
		t.Fatal(err)
	}
}

func TestFitClassifierDefaultOptions(t *testing.T) {
	model := newTestClassifier(t, 61)
	// Slice membership is the label; the redundant Positive flag must not be
	// able to invert training when a caller leaves it at the zero value.
	positives := featureExamples(4, 1)
	negatives := featureExamples(4, -1)
	for i := range positives {
		positives[i].Positive = false
	}
	for i := range negatives {
		negatives[i].Positive = true
	}
	if err := FitClassifier(context.Background(), model, positives, negatives,
		FitOptions{Seed: 3, Epochs: 1}); err != nil {
		t.Fatal(err)
	}
}

// TestMineHardNegatives checks that the hard set is exactly the requested
// number of highest-scoring unique windows from the scored sample.
func TestMineHardNegatives(t *testing.T) {
	model := newTestClassifier(t, 71)
	zeroWeights(model)
	model.W1[0] = 1  // leaves the first embedding component after ReLU
	model.W2[0] = 1  // hidden unit 0 sees step 0's projection unit 0
	model.W3[0] = 10 // sharply increasing score
	model.B3 = -5

	negatives := make([]TrainingExample, 50)
	for i := range negatives {
		features := make([]float32, ContextFrames*EmbeddingDim)
		features[0] = float32(i) / 50
		negatives[i] = TrainingExample{Features: features}
	}
	selected := mineHardNegatives(context.Background(), model, negatives, 8)
	if len(selected) != 8 {
		t.Fatalf("selected %d negatives, want 8", len(selected))
	}
	seen := make(map[int]bool, len(selected))
	for _, index := range selected {
		if seen[index] {
			t.Fatalf("duplicate negative %d", index)
		}
		seen[index] = true
	}
	// Scores increase with the feature value, so indices 49..42 are hardest.
	for index := 42; index < 50; index++ {
		if !seen[index] {
			t.Fatalf("hard negative %d not selected: %v", index, selected)
		}
	}
}

func TestMineHardNegativesCancellation(t *testing.T) {
	model := newTestClassifier(t, 81)
	negatives := featureExamples(100, -1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if selected := mineHardNegatives(ctx, model, negatives, 4); len(selected) != 0 {
		t.Fatalf("canceled mining selected %d negatives, want none", len(selected))
	}
}

// TestMineHardNegativesBudgetAndCoverage pins the pool-size fix: even when the
// scored pool is capped, mining must return the full requested hard budget,
// unique indices, and only windows from the bounded pool-wide sample.
func TestMineHardNegativesBudgetAndCoverage(t *testing.T) {
	model := newTestClassifier(t, 91)
	// One shared feature view keeps the test's memory flat; selection logic only
	// reads features, and equal scores exercise the deterministic tie-break.
	features := make([]float32, ContextFrames*EmbeddingDim)
	features[0] = 1

	for _, tc := range []struct {
		name string
		n, k int
	}{
		{name: "whole pool", n: 20000, k: 6000},
		{name: "capped pool", n: miningPoolCap + 4096, k: 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			negatives := make([]TrainingExample, tc.n)
			for i := range negatives {
				negatives[i] = TrainingExample{Features: features}
			}
			selected := mineHardNegatives(context.Background(), model, negatives, tc.k)
			if len(selected) != tc.k {
				t.Fatalf("selected %d negatives, want %d", len(selected), tc.k)
			}
			seen := make([]bool, tc.n)
			for _, index := range selected {
				if index < 0 || index >= tc.n {
					t.Fatalf("selected out-of-range negative %d", index)
				}
				if seen[index] {
					t.Fatalf("duplicate negative %d", index)
				}
				seen[index] = true
			}
		})
	}
}

// TestFreshRandomNegativeCoverage pins the coverage fix: every epoch draws a
// new unique set, consecutive sets differ, and repeated epochs explore far more
// of the pool than one active set would.
func TestFreshRandomNegativeCoverage(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 23))
	const n, k, epochs = 500, 25, 20
	indices := make([]int, 0, n)
	seen := make([]bool, n)
	covered, overlapTotal := 0, 0
	var previous []int
	for epoch := range epochs {
		indices = fillNegativeIndices(indices, n)
		random := sampleNegativeIndices(indices, k, rng)
		if len(random) != k {
			t.Fatalf("epoch %d drew %d negatives, want %d", epoch+1, len(random), k)
		}
		current := make(map[int]bool, k)
		for _, index := range random {
			if index < 0 || index >= n {
				t.Fatalf("epoch %d drew out-of-range negative %d", epoch+1, index)
			}
			if current[index] {
				t.Fatalf("epoch %d drew duplicate negative %d", epoch+1, index)
			}
			current[index] = true
			if !seen[index] {
				seen[index] = true
				covered++
			}
		}
		for _, index := range previous {
			if current[index] {
				overlapTotal++
			}
		}
		previous = append(previous[:0], random...)
	}
	if overlapTotal == (epochs-1)*k {
		t.Fatal("every epoch reused the previous random set")
	}
	if covered < n/3 {
		t.Fatalf("ephemeral draws covered only %d/%d negatives", covered, n)
	}
}

// gradientWindow builds a deterministic, non-saturated context window. Every
// time step and embedding component differs so a missing shared-projection
// term cannot cancel out.
func gradientWindow(offset int) []float32 {
	window := make([]float32, ContextFrames*EmbeddingDim)
	for i := range window {
		window[i] = float32(math.Sin(float64(i+offset)*0.37)*0.8 +
			0.15*math.Cos(float64(i)*0.11))
	}
	return window
}

// finiteDifferenceStep is deliberately below the fixture's ReLU margins
// (measured at 9.4e-4 for projections and 6e-3 for hidden units). An earlier
// 1e-2 step straddled those kinks: central differences then average two
// different one-sided slopes, which is a test artifact, not a wrong gradient.
// requireActivationMargins asserts the headroom before any comparison.
const finiteDifferenceStep = 1e-4

// checkParameter compares one analytic gradient with central finite differences
// taken at two steps. Both must agree tightly; a smooth function converges,
// while a kink or a wrong analytic gradient does not. The denominator uses the
// actual float32 values stored, not the requested epsilon.
func checkParameter(t *testing.T, name string, got float64, loss func() float64, get func() float32, set func(float32)) {
	t.Helper()
	for _, step := range []float64{finiteDifferenceStep, finiteDifferenceStep / 2} {
		numeric := numericDerivative(loss, get, set, step)
		if diff := math.Abs(got - numeric); diff > 1e-7+1e-6*math.Abs(numeric) {
			t.Errorf("%s (step %.1e): analytic %.9g, finite difference %.9g (diff %.3g)",
				name, step, got, numeric, diff)
		}
	}
}

// numericDerivative central-differences the loss at one float32 coordinate and
// restores the original value exactly.
func numericDerivative(loss func() float64, get func() float32, set func(float32), eps float64) float64 {
	original := get()
	plus := float32(float64(original) + eps)
	minus := float32(float64(original) - eps)
	set(plus)
	high := loss()
	set(minus)
	low := loss()
	set(original)
	return (high - low) / (float64(plus) - float64(minus))
}

// preActivationMargins returns the smallest distance from zero of any projection
// pre-activation and any hidden pre-activation. Finite differences are only
// meaningful when the step stays well inside these margins.
func preActivationMargins(m *Classifier, x []float32) (projection, hidden float64) {
	projection, hidden = math.Inf(1), math.Inf(1)
	var flat [ContextFrames * ProjectionWidth]float64
	for t := range ContextFrames {
		for c := range ProjectionWidth {
			z := float64(m.B1[c])
			for i := range EmbeddingDim {
				z += float64(m.W1[c*EmbeddingDim+i]) * float64(x[t*EmbeddingDim+i])
			}
			flat[t*ProjectionWidth+c] = math.Max(z, 0)
			if distance := math.Abs(z); distance < projection {
				projection = distance
			}
		}
	}
	for j := range HiddenWidth {
		z := float64(m.B2[j])
		for k := range projectionInputs {
			z += float64(m.W2[j*projectionInputs+k]) * flat[k]
		}
		if distance := math.Abs(z); distance < hidden {
			hidden = distance
		}
	}
	return projection, hidden
}

func requireActivationMargins(t *testing.T, m *Classifier, windows [][]float32) {
	t.Helper()
	for _, x := range windows {
		projection, hidden := preActivationMargins(m, x)
		if projection <= 4*finiteDifferenceStep || hidden <= 4*finiteDifferenceStep {
			t.Fatalf("fixture activation margin projection=%.3g hidden=%.3g is too close to zero for step %.1e",
				projection, hidden, finiteDifferenceStep)
		}
	}
}

// checkGradientSamples covers every tensor, including W1 entries used at all 16
// time steps and both ends of the flattened projection.
func checkGradientSamples(t *testing.T, m *Classifier, g *gradients, loss func() float64) {
	t.Helper()
	for _, p := range []struct{ c, i int }{{0, 0}, {0, 95}, {7, 33}, {23, 17}} {
		c, i := p.c, p.i
		index := c*EmbeddingDim + i
		checkParameter(t, fmt.Sprintf("w1[%d][%d]", c, i), g.w1[index], loss,
			func() float32 { return m.W1[index] },
			func(v float32) { m.W1[index] = v })
	}
	for _, c := range []int{0, 23} {
		index := c
		checkParameter(t, fmt.Sprintf("b1[%d]", c), g.b1[index], loss,
			func() float32 { return m.B1[index] },
			func(v float32) { m.B1[index] = v })
	}
	for _, p := range []struct{ j, k int }{{0, 0}, {0, 383}, {13, 100}, {23, 383}} {
		j, k := p.j, p.k
		index := j*projectionInputs + k
		checkParameter(t, fmt.Sprintf("w2[%d][%d]", j, k), g.w2[index], loss,
			func() float32 { return m.W2[index] },
			func(v float32) { m.W2[index] = v })
	}
	for _, j := range []int{0, 23} {
		index := j
		checkParameter(t, fmt.Sprintf("b2[%d]", j), g.b2[index], loss,
			func() float32 { return m.B2[index] },
			func(v float32) { m.B2[index] = v })
	}
	for _, j := range []int{0, 11, 23} {
		index := j
		checkParameter(t, fmt.Sprintf("w3[%d]", j), g.w3[index], loss,
			func() float32 { return m.W3[index] },
			func(v float32) { m.W3[index] = v })
	}
	checkParameter(t, "b3", g.b3, loss,
		func() float32 { return m.B3 },
		func(v float32) { m.B3 = v })
}

// TestGradientsMatchFiniteDifference is the backprop regression: it checks the
// W1 shared-time sum, the flattening into W2, both ReLU gates, the output layer,
// and the positive-weight scaling against central differences.
func TestGradientsMatchFiniteDifference(t *testing.T) {
	const positiveWeight = 2.5
	for _, tc := range []struct {
		name     string
		positive bool
	}{
		{name: "positive", positive: true},
		{name: "negative", positive: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newTestClassifier(t, 123)
			window := gradientWindow(0)
			requireActivationMargins(t, model, [][]float32{window})
			example := TrainingExample{Features: window, Positive: tc.positive}
			var scratch networkScratch
			grads := newGradients()
			accumulateGradient(model, example, positiveWeight, &scratch, grads)
			loss := func() float64 {
				weight := 1.0
				if tc.positive {
					weight = positiveWeight
				}
				return weight * binaryCrossEntropy(model.Score(window), tc.positive)
			}
			checkGradientSamples(t, model, grads, loss)
		})
	}
}

// TestBatchGradientScalingMatchesFiniteDifference checks that the 1/sum(weights)
// normalization produces the gradient of the weighted mean batch loss.
func TestBatchGradientScalingMatchesFiniteDifference(t *testing.T) {
	model := newTestClassifier(t, 124)
	positive := gradientWindow(0)
	negative := gradientWindow(7)
	requireActivationMargins(t, model, [][]float32{positive, negative})
	const positiveWeight = 2.0
	batchWeight := positiveWeight + 1
	var scratch networkScratch
	grads := newGradients()
	accumulateGradient(model, TrainingExample{Features: positive, Positive: true},
		positiveWeight, &scratch, grads)
	accumulateGradient(model, TrainingExample{Features: negative, Positive: false},
		positiveWeight, &scratch, grads)
	scaleGradients(grads, 1/batchWeight)
	loss := func() float64 {
		return (positiveWeight*binaryCrossEntropy(model.Score(positive), true) +
			binaryCrossEntropy(model.Score(negative), false)) / batchWeight
	}
	checkGradientSamples(t, model, grads, loss)
}

// TestAdamFirstStep verifies the bias-corrected update: at step one it must be
// lr*g/(|g|+eps), independent of the batch scale.
func TestAdamFirstStep(t *testing.T) {
	model := newTestClassifier(t, 125)
	before := cloneClassifier(model)
	grads := newGradients()
	for i := range grads.w1 {
		grads.w1[i] = 3e-3 * math.Sin(float64(i)*0.017)
	}
	for i := range grads.b1 {
		grads.b1[i] = 2e-3 * math.Cos(float64(i)*0.31)
	}
	for i := range grads.w2 {
		grads.w2[i] = 1e-3 * math.Sin(float64(i)*0.013+0.7)
	}
	for i := range grads.b2 {
		grads.b2[i] = 2.5e-3 * math.Cos(float64(i)*0.43)
	}
	for i := range grads.w3 {
		grads.w3[i] = 4e-3 * math.Sin(float64(i)*0.7)
	}
	grads.b3 = -1.5e-3
	const learningRate = 0.003
	applyAdam(model, grads, newAdamMoments(), 1-adamBeta1, 1-adamBeta2, learningRate)

	check := func(name string, grad float64, got, was float32) {
		t.Helper()
		want := float64(was) - learningRate*grad/(math.Abs(grad)+adamEpsilon)
		if diff := math.Abs(float64(got) - want); diff > 1e-6 {
			t.Errorf("%s: updated %.9g, want %.9g (diff %.3g)",
				name, got, want, diff)
		}
	}
	check("w1[0]", grads.w1[0], model.W1[0], before.W1[0])
	check("w1[5]", grads.w1[5], model.W1[5], before.W1[5])
	check("w1[last]", grads.w1[len(grads.w1)-1], model.W1[len(model.W1)-1], before.W1[len(before.W1)-1])
	check("b1[0]", grads.b1[0], model.B1[0], before.B1[0])
	check("w2[0]", grads.w2[0], model.W2[0], before.W2[0])
	check("w2[last]", grads.w2[len(grads.w2)-1], model.W2[len(model.W2)-1], before.W2[len(before.W2)-1])
	check("b2[3]", grads.b2[3], model.B2[3], before.B2[3])
	check("w3[2]", grads.w3[2], model.W3[2], before.W3[2])
	check("b3", grads.b3, model.B3, before.B3)
}
