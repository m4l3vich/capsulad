package oww

import (
	"math"
	"math/rand/v2"
	"testing"
)

func randomTriple(rng *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

func maxDiff(a, b []float32) float64 {
	d := 0.0
	for i := range a {
		d = max(d, math.Abs(float64(a[i]-b[i])))
	}
	return d
}

// TestConvTimeKernel checks the batched time kernel against the portable
// per-channel implementation and against the generic per-tap path used by the
// full-window reference. Scalar tails use fused multiply-add in assembly and
// separate multiply/add in the portable axpy, so compare with a tolerance.
func TestConvTimeKernel(t *testing.T) {
	rng := rand.New(rand.NewPCG(101, 202))
	for _, inC := range []int{1, 2, 3, 5} {
		for _, outC := range []int{1, 2, 4} {
			for inW := 1; inW <= 9; inW++ {
				src := randomTriple(rng, 3*inC*inW)
				weights := randomTriple(rng, outC*inC*3)
				bias := randomTriple(rng, outC)
				got := make([]float32, outC*inW)
				convTime(got, src, weights, bias, inC, outC, inW, inW)

				wantGo := make([]float32, outC*inW)
				convTimeGo(wantGo, src, weights, bias, inC, outC, inW, inW)
				if d := maxDiff(got, wantGo); d > 1e-5 {
					t.Fatalf("time inC=%d outC=%d W=%d: asm vs Go diff %g", inC, outC, inW, d)
				}

				l := layerSpec{op: "conv", in: inC, out: outC, kh: 3, kw: 1,
					weights: weights, bias: bias}
				wantGen := make([]float32, outC*inW)
				l.convGeneric(wantGen, src, inW, inW, 0)
				if d := maxDiff(got, wantGen); d > 1e-5 {
					t.Fatalf("time inC=%d outC=%d W=%d: asm vs generic diff %g", inC, outC, inW, d)
				}
			}
		}
	}
}

// TestConvFreqKernel checks the batched frequency kernel, including the padded
// first/last output lanes and every vector/tail width split.
func TestConvFreqKernel(t *testing.T) {
	rng := rand.New(rand.NewPCG(303, 404))
	for _, inC := range []int{1, 2, 3, 5} {
		for _, outC := range []int{1, 2, 4} {
			for inW := 1; inW <= 12; inW++ {
				src := randomTriple(rng, inC*inW)
				weights := randomTriple(rng, outC*inC*3)
				bias := randomTriple(rng, outC)
				got := make([]float32, outC*inW)
				convFreq(got, src, weights, bias, inC, outC, inW, inW)

				wantGo := make([]float32, outC*inW)
				convFreqGo(wantGo, src, weights, bias, inC, outC, inW, inW)
				if d := maxDiff(got, wantGo); d > 1e-5 {
					t.Fatalf("freq inC=%d outC=%d W=%d: asm vs Go diff %g", inC, outC, inW, d)
				}

				l := layerSpec{op: "conv", in: inC, out: outC, kh: 1, kw: 3,
					pl: 1, pr: 1, weights: weights, bias: bias}
				wantGen := make([]float32, outC*inW)
				l.convGeneric(wantGen, src, inW, inW, 0)
				if d := maxDiff(got, wantGen); d > 1e-5 {
					t.Fatalf("freq inC=%d outC=%d W=%d: asm vs generic diff %g", inC, outC, inW, d)
				}
			}
		}
	}
}

// Benchmarks for the hot kernels at representative layer shapes.
func BenchmarkConvTimeKernel(b *testing.B) {
	src := make([]float32, 3*24*32)
	weights := make([]float32, 24*24*3)
	bias := make([]float32, 24)
	dst := make([]float32, 24*32)
	b.ResetTimer()
	for range b.N {
		convTime(dst, src, weights, bias, 24, 24, 32, 32)
	}
}

func BenchmarkConvFreqKernel(b *testing.B) {
	src := make([]float32, 48*16)
	weights := make([]float32, 48*48*3)
	bias := make([]float32, 48)
	dst := make([]float32, 48*16)
	b.ResetTimer()
	for range b.N {
		convFreq(dst, src, weights, bias, 48, 48, 16, 16)
	}
}
