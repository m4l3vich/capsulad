package oww

import (
	"math"
	"math/rand/v2"
	"testing"
)

func loadFixtureFrontend(t *testing.T, pcm []int16, chunk int) ([][]float32, *Frontend) {
	t.Helper()
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	fe, err := NewFrontend(a)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]float32
	for i := 0; i < len(pcm); i += chunk {
		end := min(i+chunk, len(pcm))
		out = append(out, fe.Feed(pcm[i:end])...)
	}
	return out, fe
}

func fixtureMel(t *testing.T, pcm []int16) [][]float32 {
	t.Helper()
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	mel, err := NewMel(a)
	if err != nil {
		t.Fatal(err)
	}
	fl := make([]float64, len(pcm))
	for i, v := range pcm {
		fl[i] = float64(v)
	}
	return mel.Feed(fl)
}

// TestStreamingMatchesFullWindow is the parity gate: every cached embedding
// must be bit-identical to evaluating the same window from scratch.
func TestStreamingMatchesFullWindow(t *testing.T) {
	ref := loadReference(t)
	pcm := loadPCM16(t, ref.PCM16Samples)
	melFrames := fixtureMel(t, pcm)
	got, _ := loadFixtureFrontend(t, pcm, 320)

	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	emb, err := NewEmbedder(a)
	if err != nil {
		t.Fatal(err)
	}
	want := ref.PCM16Embeddings
	if len(got) != want {
		t.Fatalf("%d embeddings, want %d", len(got), want)
	}
	for w := range want {
		wantVec := emb.Embed(melFrames[w*WindowStep : w*WindowStep+WindowFrames])
		if len(wantVec) != EmbeddingDim {
			t.Fatalf("window %d: full-window dimension %d", w, len(wantVec))
		}
		for i := range wantVec {
			if got[w][i] != wantVec[i] {
				t.Fatalf("window %d dim %d: streaming %v != full-window %v",
					w, i, got[w][i], wantVec[i])
			}
		}
	}
}

// TestStreamingChunkingInvariance checks that arbitrary capture chunk sizes
// produce the same stream, including the first (from-scratch) window.
func TestStreamingChunkingInvariance(t *testing.T) {
	ref := loadReference(t)
	pcm := loadPCM16(t, ref.PCM16Samples)
	base, _ := loadFixtureFrontend(t, pcm, 320)
	for _, chunk := range []int{1, 3, 157, 512, 4096} {
		got, _ := loadFixtureFrontend(t, pcm, chunk)
		if len(got) != len(base) {
			t.Fatalf("chunk %d: %d embeddings, want %d", chunk, len(got), len(base))
		}
		for w := range base {
			for i := range base[w] {
				if got[w][i] != base[w][i] {
					t.Fatalf("chunk %d window %d dim %d: %v != %v",
						chunk, w, i, got[w][i], base[w][i])
				}
			}
		}
	}
}

// TestStreamingResetPhase verifies that Reset returns the mel and every stage's
// stride phase to the start of the stream.
func TestStreamingResetPhase(t *testing.T) {
	ref := loadReference(t)
	pcm := loadPCM16(t, ref.PCM16Samples)
	fresh, _ := loadFixtureFrontend(t, pcm, 320)

	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	fe, err := NewFrontend(a)
	if err != nil {
		t.Fatal(err)
	}
	// Prime with junk, then reset mid-stream.
	for i := 0; i < 5000; i += 320 {
		fe.Feed(make([]int16, min(320, 5000-i)))
	}
	fe.Reset()
	var got [][]float32
	for i := 0; i < len(pcm); i += 320 {
		got = append(got, fe.Feed(pcm[i:min(i+320, len(pcm))])...)
	}
	if len(got) != len(fresh) {
		t.Fatalf("%d embeddings after reset, want %d", len(got), len(fresh))
	}
	for w := range fresh {
		for i := range fresh[w] {
			if got[w][i] != fresh[w][i] {
				t.Fatalf("window %d dim %d after reset: %v != %v",
					w, i, got[w][i], fresh[w][i])
			}
		}
	}
}

// TestEmbeddingTiming ties emission counts to the exported sample timing.
func TestEmbeddingTiming(t *testing.T) {
	if EmbeddingDim != 96 || StepSamples != 1280 || FirstEmbeddingSamples != 12512 {
		t.Fatalf("timing constants changed: dim=%d step=%d first=%d",
			EmbeddingDim, StepSamples, FirstEmbeddingSamples)
	}
	for i := range 4 {
		if got, want := EmbeddingEnd(i), FirstEmbeddingSamples+i*StepSamples; got != want {
			t.Fatalf("EmbeddingEnd(%d) = %d, want %d", i, got, want)
		}
	}

	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	fe, err := NewFrontend(a)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, 40000)
	for i := range pcm {
		pcm[i] = int16(4000 * math.Sin(2*math.Pi*300*float64(i)/SampleRate))
	}
	total, emitted := 0, 0
	for total < len(pcm) {
		n := min(37, len(pcm)-total)
		before := total
		out := fe.Feed(pcm[total : total+n])
		total += n
		for range out {
			end := EmbeddingEnd(emitted)
			if before >= end || total < end {
				t.Fatalf("embedding %d emitted at samples (%d,%d], expected end %d",
					emitted, before, total, end)
			}
			emitted++
		}
	}
	if want := (len(pcm)-FirstEmbeddingSamples)/StepSamples + 1; emitted != want {
		t.Fatalf("%d embeddings, want %d", emitted, want)
	}
}

func TestStageBuffersReusable(t *testing.T) {
	ref := loadReference(t)
	pcm := loadPCM16(t, ref.PCM16Samples)
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	fe, err := NewFrontend(a)
	if err != nil {
		t.Fatal(err)
	}
	bufs := make([]int, len(fe.net.stages))
	for i := range fe.net.stages {
		st := &fe.net.stages[i]
		if len(st.buf) != st.pos*st.c*st.w {
			t.Fatalf("stage %d buffer %d, want %d", i, len(st.buf), st.pos*st.c*st.w)
		}
		bufs[i] = len(st.buf)
	}
	for i := 0; i < len(pcm); i += 320 {
		fe.Feed(pcm[i:min(i+320, len(pcm))])
	}
	for i := range fe.net.stages {
		if got := len(fe.net.stages[i].buf); got != bufs[i] {
			t.Fatalf("stage %d buffer resized to %d, want %d", i, got, bufs[i])
		}
	}
}

// TestConvPositionMatchesNaive validates the convolution kernel against a
// direct scalar loop (allowing for fused multiply-add rounding).
func TestConvPositionMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	l := layerSpec{
		op: "conv", in: 3, out: 4, kh: 3, kw: 3,
		pt: 1, pl: 1, pb: 1, pr: 1,
		weights: make([]float32, 4*3*3*3),
		bias:    make([]float32, 4),
	}
	for i := range l.weights {
		l.weights[i] = rng.Float32()*2 - 1
	}
	for i := range l.bias {
		l.bias[i] = rng.Float32()*2 - 1
	}
	const inN, inW, outW = 6, 5, 5
	src := make([]float32, inN*l.in*inW)
	for i := range src {
		src[i] = rng.Float32()*2 - 1
	}
	outN := inN + l.pt + l.pb - l.kh + 1
	for p := range outN {
		got := make([]float32, l.out*outW)
		l.convPosition(got, src, inW, outW, p-l.pt)
		for oc := range l.out {
			for x := range outW {
				acc := l.bias[oc]
				for ic := range l.in {
					for ky := range l.kh {
						for kx := range l.kw {
							sx := x + kx - l.pl
							sy := p + ky - l.pt
							if sx < 0 || sx >= inW || sy < 0 || sy >= inN {
								continue
							}
							acc += l.weights[((oc*l.in+ic)*l.kh+ky)*l.kw+kx] *
								src[(sy*l.in+ic)*inW+sx]
						}
					}
				}
				if d := math.Abs(float64(got[oc*outW+x] - acc)); d > 1e-5 {
					t.Fatalf("pos %d oc %d x %d: got %v want %v (err %g)",
						p, oc, x, got[oc*outW+x], acc, d)
				}
			}
		}
	}
}

func TestPoolPositionMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 11))
	l := layerSpec{op: "maxpool", kh: 2, kw: 2, sh: 2, sw: 2}
	const inN, ch, inW = 6, 3, 6
	const outN, outW = 3, 3
	src := make([]float32, inN*ch*inW)
	for i := range src {
		src[i] = rng.Float32()*2 - 1
	}
	for p := range outN {
		got := make([]float32, ch*outW)
		l.poolPosition(got, src, inW, outW, p*l.sh, ch)
		for c := range ch {
			for x := range outW {
				want := float32(-1e30)
				for ky := range l.kh {
					for kx := range l.kw {
						want = max(want, src[((p*l.sh+ky)*ch+c)*inW+x*l.sw+kx])
					}
				}
				if got[c*outW+x] != want {
					t.Fatalf("pos %d c %d x %d: got %v want %v",
						p, c, x, got[c*outW+x], want)
				}
			}
		}
	}
}

func TestAxpy(t *testing.T) {
	src := []float32{1, -2, 3, -4, 5, -6, 7}
	dst := []float32{0.5, 1, 1.5, 2, 2.5, 3, 3.5}
	want := make([]float32, len(dst))
	for i := range dst {
		want[i] = dst[i] + 0.25*src[i]
	}
	axpy(dst, src, 0.25)
	for i := range dst {
		if d := math.Abs(float64(dst[i] - want[i])); d > 1e-6 {
			t.Fatalf("axpy[%d] = %v, want %v", i, dst[i], want[i])
		}
	}
}

func benchmarkPCM(seconds int) []int16 {
	pcm := make([]int16, SampleRate*seconds)
	for i := range pcm {
		t := float64(i) / SampleRate
		pcm[i] = int16(4000*math.Sin(2*math.Pi*300*t) + 1200*math.Sin(2*math.Pi*1900*t))
	}
	return pcm
}

// BenchmarkFrontendFeed measures the live streaming path on 10 s of audio,
// including mel, incremental convolutions and pooling.
func BenchmarkFrontendFeed(b *testing.B) {
	a, err := Load()
	if err != nil {
		b.Fatal(err)
	}
	fe, err := NewFrontend(a)
	if err != nil {
		b.Fatal(err)
	}
	pcm := benchmarkPCM(10)
	b.SetBytes(int64(len(pcm) * 2))
	b.ResetTimer()
	for range b.N {
		fe.Reset()
		for i := 0; i < len(pcm); i += 320 {
			fe.Feed(pcm[i:min(i+320, len(pcm))])
		}
	}
}

// BenchmarkEmbedWindow measures the from-scratch reference for one window.
func BenchmarkEmbedWindow(b *testing.B) {
	a, err := Load()
	if err != nil {
		b.Fatal(err)
	}
	emb, err := NewEmbedder(a)
	if err != nil {
		b.Fatal(err)
	}
	frames := make([][]float32, WindowFrames)
	for i := range frames {
		frames[i] = make([]float32, MelBins)
		for j := range frames[i] {
			frames[i][j] = float32(i+j) * 0.01
		}
	}
	b.ResetTimer()
	for range b.N {
		emb.Embed(frames)
	}
}
