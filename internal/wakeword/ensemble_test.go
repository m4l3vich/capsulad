package wakeword

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
)

// storedEmbeddings is a checked-in fixture of 16 embeddings produced by the
// incremental frontend for oww/testdata/pcm16.bin. Using stored embeddings lets
// the scoring parity test run without depending on frontend state, and keeps the
// assertion about the kernel, not about the feature extractor.
type storedEmbeddings struct {
	Dim        int         `json:"dim"`
	Embeddings [][]float32 `json:"embeddings"`
}

func loadStoredEmbeddings(t *testing.T) storedEmbeddings {
	t.Helper()
	raw, err := os.ReadFile("testdata/embeddings.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc storedEmbeddings
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Dim != EmbeddingDim {
		t.Fatalf("fixture dim = %d, want %d", doc.Dim, EmbeddingDim)
	}
	if len(doc.Embeddings) != ContextFrames {
		t.Fatalf("fixture has %d embeddings, want %d", len(doc.Embeddings), ContextFrames)
	}
	for i, emb := range doc.Embeddings {
		if len(emb) != EmbeddingDim {
			t.Fatalf("embedding %d has %d values, want %d", i, len(emb), EmbeddingDim)
		}
	}
	return doc
}

// The ensemble scores a float64 ring view; the single-model engine scores a
// float32 context. Both must return the identical score for the same embeddings,
// otherwise an artifact would behave differently after the multi-word upgrade.
func TestForward64MatchesForwardOnStoredEmbeddings(t *testing.T) {
	doc := loadStoredEmbeddings(t)
	clf := NewClassifier("word.parity", "Parity", "en", 99)
	if err := clf.Validate(); err != nil {
		t.Fatal(err)
	}
	window32 := make([]float32, 0, ContextFrames*EmbeddingDim)
	window64 := make([]float64, 0, ContextFrames*EmbeddingDim)
	for _, emb := range doc.Embeddings {
		window32 = append(window32, emb...)
		for _, v := range emb {
			window64 = append(window64, float64(v))
		}
	}
	want := clf.Score(window32)
	if math.IsNaN(want) {
		t.Fatal("fixture did not score")
	}
	var scratch networkScratch
	if got := clf.forward64(window64, &scratch); got != want {
		t.Fatalf("forward64 = %v, forward = %v; the two kernels must agree bit for bit", got, want)
	}
	// A window of the wrong length is refused rather than scored.
	if got := clf.forward(window32[:len(window32)-1], &scratch); !math.IsNaN(got) {
		t.Fatalf("short window scored as %v", got)
	}
}

func readFixturePCM(t *testing.T) []int16 {
	t.Helper()
	raw, err := os.ReadFile("../wakeword/oww/testdata/pcm16.bin")
	if err != nil {
		raw, err = os.ReadFile("oww/testdata/pcm16.bin")
	}
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return pcm
}

// A shared-frontend ensemble must produce exactly the scores the single-model
// engine produced for the same model and audio, including the warm-up zeros.
func TestEnsembleMatchesSingleModelEngine(t *testing.T) {
	pcm := readFixturePCM(t)
	clone := func(seed uint64) *Classifier {
		clf := NewClassifier("word.one", "One", "en", seed)
		if err := clf.Validate(); err != nil {
			t.Fatal(err)
		}
		return clf
	}
	first, second := clone(11), clone(22)
	ensemble, err := NewEnsemble([]*Classifier{first, second})
	if err != nil {
		t.Fatal(err)
	}
	singleOne, err := NewEngine(first)
	if err != nil {
		t.Fatal(err)
	}
	singleTwo, err := NewEngine(second)
	if err != nil {
		t.Fatal(err)
	}
	var scored int
	for i := 0; i+1280 <= len(pcm); i += 1280 {
		chunk := pcm[i : i+1280]
		wantOne := singleOne.Process(chunk)
		wantTwo := singleTwo.Process(chunk)
		got := ensemble.Process(chunk)
		if len(got) != 2 {
			t.Fatalf("scores = %d, want 2", len(got))
		}
		if got[0] != wantOne || got[1] != wantTwo {
			t.Fatalf("window %d: ensemble = %v, single = %v", i/1280, got, []float64{wantOne, wantTwo})
		}
		if wantOne != 0 || wantTwo != 0 {
			scored++
		}
	}
	if scored == 0 {
		t.Fatal("no full context was scored; the fixture is too short to prove parity")
	}
}

func TestEnsembleResetClearsRingAndScores(t *testing.T) {
	pcm := readFixturePCM(t)
	clf := NewClassifier("word.reset", "Reset", "en", 5)
	if err := clf.Validate(); err != nil {
		t.Fatal(err)
	}
	ensemble, err := NewEnsemble([]*Classifier{clf})
	if err != nil {
		t.Fatal(err)
	}
	// Warm the context to a real score, then reset and feed one chunk: the
	// emitted score must be the warm-up zero again, not the stale value.
	zero := make([]int16, 1280)
	var last float64
	// Real audio, not zeros: the assertion is that a warmed score exists before
	// the reset and is gone after it.
	for i := 0; i < 60; i++ {
		start := (i * 1280) % (len(pcm) - 1280)
		last = ensemble.Process(pcm[start : start+1280])[0]
	}
	if last == 0 {
		t.Fatal("context never warmed")
	}
	ensemble.Reset()
	// Warm-up repeats from the first embedding: the first one needs 12,512
	// samples, and no score exists until sixteen embeddings do.
	for range 12 {
		if got := ensemble.Process(zero)[0]; got != 0 {
			t.Fatalf("score after reset = %v, want warm-up zero", got)
		}
	}
	if ensemble.filled < 1 || ensemble.filled >= ContextFrames {
		t.Fatalf("ring filled = %d after 12 chunks, want a partial warm-up", ensemble.filled)
	}
}

// The per-hop ensemble work for N heads must not allocate. The frontend keeps
// its pre-existing per-embedding allocation, which is why the second half of the
// test compares total allocations across N instead of asserting zero there.
func TestEnsembleScoringDoesNotAllocate(t *testing.T) {
	classifiers := make([]*Classifier, 0, 5)
	for i := range 5 {
		clf := NewClassifier(fmt.Sprintf("word.alloc%d", i), "Alloc", "en", uint64(i+1))
		if err := clf.Validate(); err != nil {
			t.Fatal(err)
		}
		classifiers = append(classifiers, clf)
	}
	ensemble, err := NewEnsemble(classifiers)
	if err != nil {
		t.Fatal(err)
	}
	zero := make([]int16, 1280)
	for range 40 {
		ensemble.Process(zero)
	}
	if allocs := testing.AllocsPerRun(50, ensemble.scoreAll); allocs != 0 {
		t.Fatalf("scoring %d heads allocates %v per hop, want 0", ensemble.Len(), allocs)
	}

	// Total per-hop allocations must not scale with the number of heads: only
	// the shared frontend contributes, and it is built once.
	var baseline float64
	for n := 1; n <= 5; n++ {
		ens, err := NewEnsemble(classifiers[:n])
		if err != nil {
			t.Fatal(err)
		}
		for range 40 {
			ens.Process(zero)
		}
		allocs := testing.AllocsPerRun(50, func() { ens.Process(zero) })
		if n == 1 {
			baseline = allocs
			continue
		}
		if allocs != baseline {
			t.Fatalf("%d heads allocate %v per hop, 1 head allocates %v; the per-head path must add none", n, allocs, baseline)
		}
	}
}

// BenchmarkEnsembleProcess reports the per-hop cost for the device budget: one
// hop is 80 ms, so N heads must stay far below it.
func BenchmarkEnsembleProcess(b *testing.B) {
	for n := 1; n <= 5; n++ {
		b.Run(fmt.Sprintf("heads=%d", n), func(b *testing.B) {
			classifiers := make([]*Classifier, 0, n)
			for i := range n {
				clf := NewClassifier(fmt.Sprintf("word.bench%d", i), "Bench", "en", uint64(i+1))
				if err := clf.Validate(); err != nil {
					b.Fatal(err)
				}
				classifiers = append(classifiers, clf)
			}
			ensemble, err := NewEnsemble(classifiers)
			if err != nil {
				b.Fatal(err)
			}
			chunk := make([]int16, 1280)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				ensemble.Process(chunk)
			}
		})
	}
}
