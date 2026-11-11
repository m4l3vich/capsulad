package wakeword

import (
	"math"
	"math/rand/v2"
	"testing"
)

// toneTake builds a deterministic speech-like recording: silence, a 440 Hz
// carrier with an amplitude envelope, then silence.
func toneTake(lead, body, tail int, amplitude float64) []int16 {
	out := make([]int16, lead+body+tail)
	for i := 0; i < body; i++ {
		env := math.Sin(math.Pi * float64(i) / float64(body))
		out[lead+i] = clamp16(amplitude * env * math.Sin(2*math.Pi*440*float64(i)/enrollmentSampleRate))
	}
	return out
}

// toneRun builds a constant-level tone without the speech-like envelope, so
// frame-energy boundaries are unambiguous in label tests.
func toneRun(n int, amplitude float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = clamp16(amplitude * math.Sin(2*math.Pi*440*float64(i)/enrollmentSampleRate))
	}
	return out
}

func TestTrimWordFindsWord(t *testing.T) {
	pcm := toneTake(9600, 8000, 14400, 8000) // 0.6 s, 0.5 s, 0.9 s
	start, end, err := trimWord(pcm)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if start < 9000 || start > 10500 {
		t.Fatalf("word start = %d samples, want around 9600", start)
	}
	if end < 16500 || end > 18000 {
		t.Fatalf("word end = %d samples, want around 17600", end)
	}
	if end-start < minWordSamples || end-start > maxWordSamples {
		t.Fatalf("word length %d out of range", end-start)
	}
}

func TestTrimWordRejects(t *testing.T) {
	cases := []struct {
		name string
		pcm  []int16
	}{
		{name: "too short", pcm: make([]int16, 1000)},
		{name: "silent", pcm: make([]int16, 2*enrollmentSampleRate)},
		{name: "clipped", pcm: func() []int16 {
			p := make([]int16, enrollmentSampleRate)
			for i := range p {
				p[i] = 32767
			}
			return p
		}()},
		{name: "too long", pcm: func() []int16 {
			p := make([]int16, 3*enrollmentSampleRate)
			for i := range p {
				p[i] = clamp16(8000 * math.Sin(2*math.Pi*440*float64(i)/enrollmentSampleRate))
			}
			return p
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := trimWord(c.pcm); err == nil {
				t.Fatal("take was accepted")
			}
		})
	}
}

func TestTrimWordIgnoresIsolatedBlips(t *testing.T) {
	// A clean word followed 300 ms later by a short loud click must not stretch
	// the labelled word end into the click: positive windows would then train
	// the classifier on non-word audio after the word.
	pcm := toneTake(9600, 8000, 0, 8000)
	pcm = append(pcm, make([]int16, 4800)...)
	for i := range 640 { // 40 ms, below trimMinRunFrames
		pcm = append(pcm, clamp16(9000*math.Sin(2*math.Pi*440*float64(i)/enrollmentSampleRate)))
	}
	start, end, err := trimWord(pcm)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if end > 18500 {
		t.Fatalf("word end = %d, want the click after the word ignored", end)
	}
	if start < 9000 || start > 10500 {
		t.Fatalf("word start = %d, want around 9600", start)
	}
}

func TestTrimWordKeepsSoftTail(t *testing.T) {
	// A short soft offset one frame after the word is part of the word, not an
	// isolated blip; dropping it would cut the final consonant or vowel.
	pcm := toneTake(3200, 8000, 0, 9000)
	pcm = append(pcm, make([]int16, 160)...)
	for i := range 640 {
		pcm = append(pcm, clamp16(4000*math.Sin(2*math.Pi*300*float64(i)/enrollmentSampleRate)))
	}
	pcm = append(pcm, make([]int16, 3200)...)
	_, end, err := trimWord(pcm)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if end < 11200+320 {
		t.Fatalf("word end = %d, soft tail was dropped", end)
	}
}

func TestTrimWordBridgesWordPause(t *testing.T) {
	pcm := toneTake(3200, 8000, 0, 9000)
	pcm = append(pcm, make([]int16, 1600)...) // 100 ms pause
	for i := range 1600 {
		pcm = append(pcm, clamp16(6000*math.Sin(2*math.Pi*330*float64(i)/enrollmentSampleRate)))
	}
	pcm = append(pcm, make([]int16, 3200)...)
	start, end, err := trimWord(pcm)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if start > 4500 {
		t.Fatalf("word start = %d, want the first part of a two-part word", start)
	}
	if end < 3200+8000+1600 {
		t.Fatalf("word end = %d, want the second part included across a 100 ms pause", end)
	}
}

func TestTrimWordRejectsSpeechAtClipEnd(t *testing.T) {
	pcm := make([]int16, 9600+8000)
	for i := 9600; i < len(pcm); i++ {
		pcm[i] = clamp16(9000 * math.Sin(2*math.Pi*440*float64(i)/enrollmentSampleRate))
	}
	if _, _, err := trimWord(pcm); err == nil {
		t.Fatal("take cut off at the clip end was accepted")
	}
}

func TestTrimWordKeepsAdjacentOnset(t *testing.T) {
	// A short onset one frame before the word is part of the word, just like
	// the soft tail. Dropping it cuts the initial consonant or vowel.
	pcm := make([]int16, 3200)
	pcm = append(pcm, toneRun(640, 5000)...)
	pcm = append(pcm, make([]int16, 320)...) // 20 ms pause, inside adjacency
	pcm = append(pcm, toneRun(8000, 9000)...)
	pcm = append(pcm, make([]int16, 4800)...)
	start, end, err := trimWord(pcm)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if start > 3200+trimHopSamples {
		t.Fatalf("word start = %d, adjacent onset was dropped", start)
	}
	if end < 3200+640+320+6000 {
		t.Fatalf("word end = %d, the sustained word was not found", end)
	}
}

func TestTrimWordIgnoresDetachedOnsetClick(t *testing.T) {
	// A short click 200 ms before the word is room noise, not its onset.
	pcm := make([]int16, 3200)
	pcm = append(pcm, toneRun(640, 5000)...)
	pcm = append(pcm, make([]int16, 3200)...)
	wordStart := len(pcm)
	pcm = append(pcm, toneRun(8000, 9000)...)
	pcm = append(pcm, make([]int16, 4800)...)
	start, _, err := trimWord(pcm)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if start < wordStart-trimFrameSamples {
		t.Fatalf("word start = %d, detached click before %d was included", start, wordStart)
	}
}

func TestResampleLinear(t *testing.T) {
	src := make([]int16, 1000)
	for i := range src {
		src[i] = int16(i % 300)
	}
	if got := len(resampleLinear(src, 2)); got != 500 {
		t.Fatalf("speed 2 length = %d, want 500", got)
	}
	if got := len(resampleLinear(src, 0.5)); got != 2000 {
		t.Fatalf("speed 0.5 length = %d, want 2000", got)
	}
	if got := resampleLinear(src, 1); len(got) != len(src) {
		t.Fatalf("speed 1 length = %d", len(got))
	}
	for _, v := range resampleLinear(src, 1.7) {
		if v < 0 || v > 300 {
			t.Fatalf("interpolated sample %d out of source range", v)
		}
	}
}

func TestClamp16(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want int16
	}{
		{40000, 32767}, {-40000, -32768}, {0.4, 0}, {0.6, 1}, {-0.6, -1},
	} {
		if got := clamp16(c.in); got != c.want {
			t.Fatalf("clamp16(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestAugmentWordTakeDeterministicAndBounded(t *testing.T) {
	take := toneTake(3200, 9600, 9600, 9000)
	wordStart, wordEnd, err := trimWord(take)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	var rooms, speech [][]int16
	for i := 0; i < 3; i++ {
		rooms = append(rooms, toneTake(16000, 16000, 0, 400))
		speech = append(speech, toneTake(0, 8000, 8000, 6000))
	}
	build := func(seed uint64) ([]int16, int, int) {
		rng := rand.New(rand.NewPCG(seed, seed))
		out, ws, we, err := augmentWordTake(rng, take, wordStart, wordEnd, rooms, speech)
		if err != nil {
			t.Fatalf("augment: %v", err)
		}
		return out, ws, we
	}
	a, aws, awe := build(7)
	b, bws, bwe := build(7)
	if len(a) != len(b) || aws != bws || awe != bwe {
		t.Fatal("augmentation is not deterministic for a fixed seed")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sample %d differs between identical seeds", i)
		}
	}
	if aws < minPrecontext {
		t.Fatalf("pre-word context = %d samples, want >= %d", aws, minPrecontext)
	}
	if post := len(a) - awe; post < minPostSlack {
		t.Fatalf("post-word slack = %d samples, want >= %d", post, minPostSlack)
	}
	if awe <= aws {
		t.Fatalf("degenerate word bounds %d..%d", aws, awe)
	}
	if len(a) != len(b) {
		t.Fatalf("length %d != %d", len(a), len(b))
	}
}

func TestPadOriginalBounds(t *testing.T) {
	take := toneTake(1600, 8000, 6400, 9000)
	start, end, err := trimWord(take)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	out, ws, we := padOriginal(take, start, end)
	if ws < minPrecontext {
		t.Fatalf("padded word start = %d, want >= %d", ws, minPrecontext)
	}
	if post := len(out) - we; post < minPostSlack {
		t.Fatalf("padded post slack = %d, want >= %d", post, minPostSlack)
	}
	if we-ws != end-start {
		t.Fatalf("word length changed from %d to %d", end-start, we-ws)
	}
}

func TestPadOriginalUsesCapturedBackground(t *testing.T) {
	const lead = 9600
	pcm := make([]int16, lead+8000+4800)
	for i := range lead {
		pcm[i] = clamp16(300 * math.Sin(2*math.Pi*200*float64(i)/enrollmentSampleRate))
	}
	for i := range 8000 {
		pcm[lead+i] = clamp16(8000 * math.Sin(2*math.Pi*440*float64(i)/enrollmentSampleRate))
	}
	for i := range 4800 {
		pcm[lead+8000+i] = clamp16(300 * math.Sin(2*math.Pi*200*float64(i)/enrollmentSampleRate))
	}
	start, end, err := trimWord(pcm)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	out, ws, we := padOriginal(pcm, start, end)
	if rmsInt16(out[:minPrecontext]) < 50 {
		t.Fatal("lead-in is digital silence, want the take's captured background")
	}
	if rmsInt16(out[len(out)-minPostSlack:]) < 50 {
		t.Fatal("post-roll is digital silence, want the take's captured background")
	}
	if post := len(out) - we; post < minPostSlack {
		t.Fatalf("padded post slack = %d, want >= %d", post, minPostSlack)
	}
	if ws < minPrecontext || we-ws != end-start {
		t.Fatalf("word bounds %d..%d changed from %d..%d", ws, we, start, end)
	}
}

func TestRoomNoiseTilesAndIsBounded(t *testing.T) {
	room := []int16{1, 2, 3, 4}
	rng := rand.New(rand.NewPCG(1, 2))
	out := roomNoise(rng, [][]int16{room}, 10)
	if len(out) != 10 {
		t.Fatalf("length = %d", len(out))
	}
	seen := map[int16]bool{}
	for _, v := range out {
		seen[v] = true
	}
	for _, v := range room {
		if !seen[v] {
			t.Fatalf("value %d missing from tiled noise", v)
		}
	}
}
