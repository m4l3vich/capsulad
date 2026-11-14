package wakeword

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"capsulad/internal/audio"
	"capsulad/internal/wakeword/oww"
)

func TestSplitByContent(t *testing.T) {
	takes := make([][]int16, 8)
	for i := range takes {
		takes[i] = toneTake(1600, 4800, 6400, float64(2000+i*500))
	}
	split, err := splitByContent(takes, 2, 2)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(split.Train) != 4 || len(split.Tune) != 2 || len(split.Test) != 2 {
		t.Fatalf("counts = %d/%d/%d, want 4/2/2", len(split.Train), len(split.Tune), len(split.Test))
	}
	seen := make(map[int]int, 8)
	for _, group := range [][]int{split.Train, split.Tune, split.Test} {
		for _, idx := range group {
			if idx < 0 || idx >= 8 {
				t.Fatalf("index %d out of range", idx)
			}
			seen[idx]++
		}
	}
	if len(seen) != 8 {
		t.Fatalf("covered %d recordings, want 8", len(seen))
	}
	for idx, count := range seen {
		if count != 1 {
			t.Fatalf("index %d assigned %d times", idx, count)
		}
	}

	again, err := splitByContent(takes, 2, 2)
	if err != nil {
		t.Fatalf("split again: %v", err)
	}
	if !equalRoles(roleContents(split, takes), roleContents(again, takes)) {
		t.Fatal("split is not deterministic for identical content")
	}

	// Holdout membership must be a function of content, not capture order or
	// the training seed: permuting the inputs keeps the same roles.
	perm := []int{3, 7, 1, 0, 6, 2, 5, 4}
	shuffled := make([][]int16, len(takes))
	for i, v := range perm {
		shuffled[i] = takes[v]
	}
	permuted, err := splitByContent(shuffled, 2, 2)
	if err != nil {
		t.Fatalf("permuted split: %v", err)
	}
	if !equalRoles(roleContents(split, takes), roleContents(permuted, shuffled)) {
		t.Fatal("holdout membership changed when recording order changed")
	}

	small, err := splitByContent(takes[:3], 1, 1)
	if err != nil {
		t.Fatalf("small split: %v", err)
	}
	if len(small.Train) != 1 || len(small.Tune) != 1 || len(small.Test) != 1 {
		t.Fatalf("small split = %d/%d/%d", len(small.Train), len(small.Tune), len(small.Test))
	}

	minimum, err := splitByContent(takes[:6], 2, 2)
	if err != nil {
		t.Fatalf("six-take split: %v", err)
	}
	if len(minimum.Train) != 2 || len(minimum.Tune) != 2 || len(minimum.Test) != 2 {
		t.Fatalf("six-take split = %d/%d/%d, want 2/2/2",
			len(minimum.Train), len(minimum.Tune), len(minimum.Test))
	}

	many := make([][]int16, 20)
	for i := range many {
		many[i] = toneTake(1600, 4800, 6400, float64(2000+i*300))
	}
	wide, err := splitByContent(many, 2, 2)
	if err != nil {
		t.Fatalf("wide split: %v", err)
	}
	if len(wide.Train) != 10 || len(wide.Tune) != 5 || len(wide.Test) != 5 {
		t.Fatalf("wide split = %d/%d/%d, want 10/5/5", len(wide.Train), len(wide.Tune), len(wide.Test))
	}

	empty, err := splitByContent(nil, 0, 0)
	if err != nil || len(empty.Train)+len(empty.Tune)+len(empty.Test) != 0 {
		t.Fatalf("empty split = %+v, err %v", empty, err)
	}

	if _, err := splitByContent(takes[:3], 2, 2); err == nil {
		t.Fatal("split without any training recordings accepted")
	}
}

// roleContents maps each role to the sorted content fingerprints it holds.
func roleContents(split indexSplit, takes [][]int16) map[string]string {
	out := make(map[string]string, 3)
	for name, idx := range map[string][]int{"train": split.Train, "tune": split.Tune, "test": split.Test} {
		hashes := make([]string, len(idx))
		for i, id := range idx {
			hashes[i] = fmt.Sprintf("%016x", takeFingerprint(takes[id]))
		}
		slices.Sort(hashes)
		out[name] = strings.Join(hashes, ",")
	}
	return out
}

func equalRoles(a, b map[string]string) bool {
	for name, value := range a {
		if b[name] != value {
			return false
		}
	}
	return true
}

func TestPositiveWindowRange(t *testing.T) {
	count := 100
	wordEnd := oww.EmbeddingEnd(50) + 100
	wordStart := wordEnd - 8000
	first, last, err := positiveWindowRange(count, wordStart, wordEnd)
	if err != nil {
		t.Fatalf("window range: %v", err)
	}
	if first != 51 {
		t.Fatalf("first window = %d, want 51", first)
	}
	if last < first || last > first+5 {
		t.Fatalf("last window = %d, want first..first+5", last)
	}
	for e := first; e <= last; e++ {
		if tail := oww.EmbeddingEnd(e) - wordEnd; tail < 0 || tail > maxPostWordStepSamples {
			t.Fatalf("window %d tail = %d samples", e, tail)
		}
		windowStart := oww.EmbeddingEnd(e-ContextFrames+1) - oww.FirstEmbeddingSamples
		if windowStart > wordStart {
			t.Fatalf("window %d starts at %d, after word start %d", e, windowStart, wordStart)
		}
	}

	cases := []struct {
		name      string
		count     int
		wordStart int
		wordEnd   int
	}{
		{name: "short stream", count: ContextFrames - 1, wordStart: 40000, wordEnd: 48000},
		{name: "word before first embedding", count: 40, wordStart: 1000, wordEnd: 5000},
		{name: "stream ends at word", count: 21, wordStart: int(oww.EmbeddingEnd(15)), wordEnd: oww.EmbeddingEnd(20) + 100},
		{name: "word leaks before receptive field", count: 40, wordStart: 1000, wordEnd: oww.EmbeddingEnd(20) + 100},
		{name: "invalid bounds", count: 40, wordStart: 48000, wordEnd: 48000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := positiveWindowRange(c.count, c.wordStart, c.wordEnd); err == nil {
				t.Fatal("invalid layout accepted")
			}
		})
	}
}

func TestEvaluatePolicy(t *testing.T) {
	const high = 0.9

	t.Run("two consecutive windows fire once", func(t *testing.T) {
		scores := make([]float64, 20)
		for i := range scores {
			scores[i] = high
		}
		out := evaluatePolicy(evalSignal{scores: scores, wordStart: -1}, 0.5)
		if out.falseActivations != 1 {
			t.Fatalf("activations = %d, want 1", out.falseActivations)
		}
	})

	t.Run("single window does not fire", func(t *testing.T) {
		out := evaluatePolicy(evalSignal{scores: []float64{high}, wordStart: -1}, 0.5)
		if out.falseActivations != 0 {
			t.Fatalf("activations = %d, want 0", out.falseActivations)
		}
	})

	t.Run("cooldown suppresses then allows", func(t *testing.T) {
		scores := make([]float64, 100)
		for i := range scores {
			scores[i] = high
		}
		out := evaluatePolicy(evalSignal{scores: scores, wordStart: -1}, 0.5)
		if out.falseActivations != 3 {
			t.Fatalf("activations = %d, want 3 within 100 windows", out.falseActivations)
		}
	})

	t.Run("consecutive complete-word windows detect", func(t *testing.T) {
		end := oww.EmbeddingEnd(ContextFrames - 1)
		out := evaluatePolicy(evalSignal{scores: []float64{high, high},
			wordStart: end - 8000, wordEnd: end}, 0.5)
		if !out.detected || out.falseActivations != 0 {
			t.Fatalf("detected=%v false=%d", out.detected, out.falseActivations)
		}
	})

	t.Run("decision contains word even if preceding score was partial", func(t *testing.T) {
		end := oww.EmbeddingEnd(ContextFrames - 1)
		out := evaluatePolicy(evalSignal{scores: []float64{high, high},
			wordStart: end - 8000, wordEnd: end + 1}, 0.5)
		if !out.detected || out.falseActivations != 0 {
			t.Fatalf("post-word decision: detected=%v false=%d", out.detected, out.falseActivations)
		}
	})

	t.Run("partial-word fire consumes cooldown", func(t *testing.T) {
		fireEnd := oww.EmbeddingEnd(ContextFrames)
		out := evaluatePolicy(evalSignal{scores: []float64{high, high, high},
			wordStart: fireEnd - 8000, wordEnd: fireEnd + 1}, 0.5)
		if out.detected || out.falseActivations != 1 {
			t.Fatalf("partial-word activation: detected=%v false=%d", out.detected, out.falseActivations)
		}
	})

	t.Run("activation before the word is false", func(t *testing.T) {
		scores := []float64{high, high, high}
		wordStart := oww.EmbeddingEnd(ContextFrames + 1)
		out := evaluatePolicy(evalSignal{scores: scores, wordStart: wordStart, wordEnd: wordStart + 10}, 0.5)
		if out.detected || out.falseActivations != 1 {
			t.Fatalf("detected=%v false=%d", out.detected, out.falseActivations)
		}
	})

	t.Run("threshold filters low scores", func(t *testing.T) {
		scores := []float64{0.4, 0.4, 0.4, 0.4}
		out := evaluatePolicy(evalSignal{scores: scores, wordStart: -1}, 0.5)
		if out.falseActivations != 0 {
			t.Fatalf("activations = %d", out.falseActivations)
		}
	})
}

func TestCalibrateThreshold(t *testing.T) {
	word := evalSignal{
		scores:    []float64{0.9, 0.9, 0.9},
		wordStart: oww.EmbeddingEnd(ContextFrames-1) - 8000,
		wordEnd:   oww.EmbeddingEnd(ContextFrames - 1),
	}
	quiet := evalSignal{scores: []float64{0.1, 0.1, 0.1, 0.1}, wordStart: -1}

	second := evalSignal{scores: []float64{0.85, 0.85, 0.85},
		wordStart: word.wordStart - 100, wordEnd: word.wordEnd - 100}
	threshold, metrics, err := calibrateThreshold([]evalSignal{word, second}, []evalSignal{quiet})
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if threshold <= 0.1 || threshold > 0.85+1e-9 {
		t.Fatalf("threshold = %v, want inside (0.1, 0.85]", threshold)
	}
	if metrics.Detected != 2 || metrics.FalseActivations != 0 {
		t.Fatalf("metrics = %+v", metrics)
	}

	loud := evalSignal{scores: []float64{0.9, 0.9, 0.9, 0.9}, wordStart: -1}
	weak := evalSignal{
		scores:    []float64{0.1, 0.1, 0.1},
		wordStart: oww.EmbeddingEnd(ContextFrames),
		wordEnd:   oww.EmbeddingEnd(ContextFrames) + 10,
	}
	if _, _, err := calibrateThreshold([]evalSignal{weak, second}, []evalSignal{loud}); err == nil || !strings.Contains(err.Error(), "no threshold") {
		t.Fatalf("unreachable quality gate error = %v", err)
	}
}

func TestMeasurePolicyRequiresEightyPercent(t *testing.T) {
	m := gateMetrics{WordRecordings: 5, Detected: 4, FalseActivations: 0}
	if !m.passes() {
		t.Fatal("4/5 recall should pass")
	}
	m.Detected = 3
	if m.passes() {
		t.Fatal("3/5 recall should fail")
	}
	m.Detected = 5
	m.FalseActivations = 1
	if m.passes() {
		t.Fatal("one false activation should fail")
	}
}

func TestTrainValidation(t *testing.T) {
	opts := TrainOptions{ID: "test.word", NegativesPath: filepath.Join(t.TempDir(), "negatives.bin"), InputProcessing: audio.CaptureProcessingTap}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Train(ctx, Enrollment{}, opts); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled train error = %v", err)
	}

	silent := make([][]int16, 8)
	for i := range silent {
		silent[i] = make([]int16, enrollmentSampleRate)
	}
	if _, err := Train(context.Background(), Enrollment{Takes: silent}, opts); err == nil || !strings.Contains(err.Error(), "silent") {
		t.Fatalf("silent takes error = %v", err)
	}

	takes := make([][]int16, 8)
	for i := range takes {
		takes[i] = toneTake(1600, 6400, 8000, float64(6000+i*200))
	}
	rooms := make([][]int16, 3)
	speech := make([][]int16, 3)
	for i := 0; i < 3; i++ {
		rooms[i] = toneTake(0, 17600, 0, 500)
		speech[i] = toneTake(0, 16000, 0, 6000)
	}
	if _, err := Train(context.Background(), Enrollment{Takes: takes, Ambient: rooms, Speech: speech}, opts); err == nil || !strings.Contains(err.Error(), "negatives") {
		t.Fatalf("missing negatives error = %v", err)
	}

	if _, err := Train(context.Background(), Enrollment{Takes: takes, Ambient: rooms[:2], Speech: speech}, opts); err == nil || !strings.Contains(err.Error(), "room/ambient") {
		t.Fatalf("room count error = %v", err)
	}
}

func TestTrainRejectsDuplicateWordTakes(t *testing.T) {
	take := toneTake(1600, 6400, 8000, 8000)
	takes := make([][]int16, 8)
	for i := range takes {
		takes[i] = append([]int16(nil), take...)
	}
	rooms := make([][]int16, 3)
	speech := make([][]int16, 3)
	for i := 0; i < 3; i++ {
		rooms[i] = toneTake(0, 17600, 0, float64(500+i*50))
		speech[i] = toneTake(0, 16000, 0, float64(6000+i*100))
	}
	_, err := Train(context.Background(), Enrollment{Takes: takes, Ambient: rooms, Speech: speech}, TrainOptions{
		ID: "dup.word", NegativesPath: filepath.Join(t.TempDir(), "negatives.bin"), InputProcessing: audio.CaptureProcessingTap,
	})
	if err == nil || !strings.Contains(err.Error(), "duplicates word recording") {
		t.Fatalf("duplicate takes error = %v", err)
	}
}

// taggedFlat puts the frame index in every value of a frame, so a window's
// first and last features expose the context indices it covers.
func taggedFlat(base, frames int) []float32 {
	flat := make([]float32, frames*oww.EmbeddingDim)
	for e := range frames {
		for j := range oww.EmbeddingDim {
			flat[e*oww.EmbeddingDim+j] = float32(base + e)
		}
	}
	return flat
}

func TestPrefixNegativeWindowsCoverEveryCleanPreWordPhase(t *testing.T) {
	const frames = 60
	sig := &embeddedSignal{
		flat: taggedFlat(0, frames), count: frames, samples: 960000,
		wordStart: minPrecontext + 1600,
		wordEnd:   minPrecontext + 9600,
	}
	windows := appendPrefixNegativeWindows(nil, sig)
	if len(windows) == 0 {
		t.Fatal("no clean pre-word negative windows were produced")
	}
	lastEnd := -1
	for i, w := range windows {
		start := int(w.Features[0])
		end := int(w.Features[len(w.Features)-1])
		if end-start != ContextFrames-1 {
			t.Fatalf("window %d spans frames %d..%d, want %d frames", i, start, end, ContextFrames)
		}
		if audioEnd := oww.EmbeddingEnd(end); audioEnd > minPrecontext {
			t.Fatalf("window %d ends at sample %d, past the clean pre-word boundary %d", i, audioEnd, minPrecontext)
		}
		if audioEnd := oww.EmbeddingEnd(end); audioEnd > sig.wordStart {
			t.Fatalf("window %d ends at sample %d, overlapping word start %d", i, audioEnd, sig.wordStart)
		}
		if i > 0 && end != lastEnd+1 {
			t.Fatalf("window %d ends at frame %d after frame %d; a clean phase was skipped", i, end, lastEnd)
		}
		lastEnd = end
	}
	if first := int(windows[0].Features[len(windows[0].Features)-1]); first != ContextFrames-1 {
		t.Fatalf("first clean window ends at frame %d, want %d", first, ContextFrames-1)
	}
	if next := lastEnd + 1; next < frames && oww.EmbeddingEnd(next) <= minPrecontext {
		t.Fatalf("clean window ending at frame %d was omitted", next)
	}
}

func TestCorpusNegativeWindowsCoverEveryPhase(t *testing.T) {
	neg := &Negatives{
		Version: negativesVersion, EmbeddingDim: oww.EmbeddingDim, HopSamples: oww.StepSamples,
		PrerollSamples: negativesPrerollSamples,
		Streams: []NegativeStream{
			{ID: "train-a", Kind: "speech", Split: "train", OriginalSamples: 1280, Frames: negativeFrames(1280)},
			{ID: "train-b", Kind: "music", Split: "train", OriginalSamples: 16000, Frames: negativeFrames(16000)},
			{ID: "tune", Kind: "speech", Split: "tune", OriginalSamples: 1280, Frames: negativeFrames(1280)},
			{ID: "test", Kind: "noise", Split: "test", OriginalSamples: 1280, Frames: negativeFrames(1280)},
		},
	}
	for i := range neg.Streams {
		neg.Streams[i].Embeddings = taggedFlat((i+1)*1000, neg.Streams[i].Frames)
	}
	type phase struct{ stream, area int }
	seen := map[phase]bool{}
	for _, w := range corpusNegativeWindows(neg) {
		start := int(w.Features[0])
		end := int(w.Features[len(w.Features)-1])
		if end-start != ContextFrames-1 {
			t.Fatalf("window spans frames %d..%d, want %d frames", start, end, ContextFrames)
		}
		seen[phase{stream: start/1000 - 1, area: start % 1000}] = true
	}
	want := 0
	for i, st := range neg.Streams {
		if st.Split != "train" || st.Frames < ContextFrames {
			continue
		}
		for area := 0; area+ContextFrames <= st.Frames; area++ {
			want++
			if !seen[phase{stream: i, area: area}] {
				t.Fatalf("stream %q phase %d was omitted", st.ID, area)
			}
		}
	}
	if len(seen) != want {
		t.Fatalf("windows cover %d phases, want %d", len(seen), want)
	}
	for p := range seen {
		if neg.Streams[p.stream].Split != "train" {
			t.Fatalf("window came from %q split", neg.Streams[p.stream].Split)
		}
	}
}

func TestWarmedLocalNegativeCoversClipOnset(t *testing.T) {
	assets, err := oww.Load()
	if err != nil {
		t.Fatal(err)
	}
	room := make([]int16, enrollmentSampleRate)
	for i := range room {
		room[i] = int16((i*37)%101 - 50)
	}
	clip := toneRun(enrollmentSampleRate/4, 3500)
	warmed := warmNegative(clip, [][]int16{room})
	control := warmNegative(make([]int16, len(clip)), [][]int16{room})
	embed := func(pcm []int16) []float32 {
		frontend, err := oww.NewFrontend(assets)
		if err != nil {
			t.Fatal(err)
		}
		var flat []float32
		for _, frame := range frontend.Feed(pcm) {
			flat = append(flat, frame...)
		}
		return flat
	}
	actual, quiet := embed(warmed), embed(control)
	windows := appendStreamNegativeWindows(nil, actual, len(actual)/oww.EmbeddingDim)
	controls := appendStreamNegativeWindows(nil, quiet, len(quiet)/oww.EmbeddingDim)
	onsetChangesContext := false
	for k, w := range windows {
		end := oww.EmbeddingEnd(k + ContextFrames - 1)
		diff := float32(0)
		for i, value := range w.Features {
			d := value - controls[k].Features[i]
			if d < 0 {
				d = -d
			}
			diff = max(diff, d)
		}
		if end <= minPrecontext && diff != 0 {
			t.Fatalf("clip leaked into pre-word room context ending at sample %d", end)
		}
		if end > minPrecontext && end <= minPrecontext+len(clip) && diff > 0.001 {
			onsetChangesContext = true
		}
	}
	if !onsetChangesContext {
		t.Fatal("negative training contexts omitted the short clip's onset")
	}
}

// The external corpus is an auxiliary training source, not a policy-matching
// held-out set: calibration and the final certificate must see only the local
// processed negatives, while CNEG is measured separately.
func TestEvalNegativeSignalsSeparatesOffPolicyCorpus(t *testing.T) {
	clf := NewClassifier("word.personal", "word", "en", 1)
	clf.Threshold = 0.5
	frames := ContextFrames + 2
	neg := &Negatives{Streams: []NegativeStream{{
		Split: "tune", Frames: frames,
		Embeddings: make([]float32, frames*EmbeddingDim),
	}}}
	local := []embeddedSignal{{
		flat: make([]float32, frames*EmbeddingDim), count: frames,
		wordStart: -1, wordEnd: -1,
	}}
	policy, aux, err := evalNegativeSignals(context.Background(), clf, neg, "tune", local)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy) != 1 || len(aux) != 1 {
		t.Fatalf("policy=%d auxiliary=%d, want 1 and 1", len(policy), len(aux))
	}
	if len(policy[0].scores) == 0 || len(aux[0].scores) == 0 {
		t.Fatal("negative evaluation produced no score windows")
	}
}

func TestFinalGateKeepsAuxiliaryVeto(t *testing.T) {
	localPass := gateMetrics{WordRecordings: 5, Detected: 5, FalseActivations: 0}
	if err := finalGateError(localPass, gateMetrics{}, gateMetrics{}, 0.5); err != nil {
		t.Fatalf("clean local and auxiliary metrics rejected: %v", err)
	}
	// Auxiliary tune false fires are a separate conservative veto.
	err := finalGateError(localPass, gateMetrics{FalseActivations: 1}, gateMetrics{}, 0.5)
	if err == nil || !strings.Contains(err.Error(), "CNEG tune") {
		t.Fatalf("auxiliary tune false fire did not block installation: %v", err)
	}
	// Auxiliary final-test false fires are a hard veto even when local passes.
	err = finalGateError(localPass, gateMetrics{}, gateMetrics{FalseActivations: 1}, 0.5)
	if err == nil || !strings.Contains(err.Error(), "CNEG final-test") {
		t.Fatalf("auxiliary final-test false fire did not block installation: %v", err)
	}
	// The local policy gate remains independent and still rejects a local fail.
	localFail := gateMetrics{WordRecordings: 5, Detected: 3, FalseActivations: 0}
	if err := finalGateError(localFail, gateMetrics{}, gateMetrics{}, 0.5); err == nil || !strings.Contains(err.Error(), "final test failed") {
		t.Fatalf("local final-test failure not reported: %v", err)
	}
}

func TestTrainOptionsRequireDeclaredProcessing(t *testing.T) {
	opts := TrainOptions{ID: "test.word", NegativesPath: filepath.Join(t.TempDir(), "negatives.bin")}
	if err := opts.check(); err == nil || !strings.Contains(err.Error(), "input processing") {
		t.Fatalf("missing input processing accepted: %v", err)
	}
	opts.InputProcessing = "framework.dsp.v2"
	if err := opts.check(); err == nil || !strings.Contains(err.Error(), "unsupported input processing") {
		t.Fatalf("unknown input processing accepted: %v", err)
	}
	opts.InputProcessing = audio.CaptureProcessingTap
	if err := opts.check(); err != nil {
		t.Fatalf("declared processed policy rejected: %v", err)
	}
}
