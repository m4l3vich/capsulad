package oww

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"testing"
)

type reference struct {
	SignalSamples   int       `json:"signal_samples"`
	MelFrames       int       `json:"mel_frames"`
	Mel             []float64 `json:"mel"`
	MelShape        []int     `json:"mel_shape"`
	WindowStart     int       `json:"window_start"`
	Embedding       []float64 `json:"embedding"`
	PCM16Samples    int       `json:"pcm16_samples"`
	PCM16Embeddings int       `json:"pcm16_embeddings"`
	PCM16Embedding0 []float64 `json:"pcm16_embedding0"`
}

func loadReference(t *testing.T) reference {
	t.Helper()
	raw, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatalf("reference: %v", err)
	}
	var ref reference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("reference: %v", err)
	}
	return ref
}

func loadSignal(t *testing.T) []float64 {
	t.Helper()
	raw, err := os.ReadFile("testdata/signal.bin")
	if err != nil {
		t.Fatalf("signal: %v", err)
	}
	out := make([]float64, len(raw)/4)
	for i := range out {
		bits := binary.LittleEndian.Uint32(raw[i*4:])
		out[i] = float64(math.Float32frombits(bits))
	}
	return out
}

func TestMelMatchesReference(t *testing.T) {
	ref := loadReference(t)
	signal := loadSignal(t)
	if len(signal) != ref.SignalSamples {
		t.Fatalf("signal length %d, want %d", len(signal), ref.SignalSamples)
	}
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	mel, err := NewMel(a)
	if err != nil {
		t.Fatal(err)
	}
	frames := mel.Feed(signal)
	if len(frames) != ref.MelFrames {
		t.Fatalf("%d mel frames, want %d", len(frames), ref.MelFrames)
	}
	cols := ref.MelShape[1]
	maxErr := 0.0
	for i := 0; i < len(ref.Mel)/cols; i++ {
		for j := 0; j < cols; j++ {
			want := ref.Mel[i*cols+j]
			got := float64(frames[i][j])
			if d := math.Abs(got - want); d > maxErr {
				maxErr = d
			}
		}
	}
	if maxErr > 1e-3 {
		t.Fatalf("mel max error %.6f exceeds 1e-3", maxErr)
	}
	t.Logf("mel max error %.6g", maxErr)
}

func TestEmbeddingMatchesReference(t *testing.T) {
	ref := loadReference(t)
	signal := loadSignal(t)
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	mel, err := NewMel(a)
	if err != nil {
		t.Fatal(err)
	}
	frames := mel.Feed(signal)
	start := ref.WindowStart
	if len(frames) < start+76 {
		t.Fatalf("not enough mel frames: %d", len(frames))
	}
	emb, err := NewEmbedder(a)
	if err != nil {
		t.Fatal(err)
	}
	got := emb.Embed(frames[start : start+76])
	if len(got) != len(ref.Embedding) {
		t.Fatalf("embedding length %d, want %d", len(got), len(ref.Embedding))
	}
	maxErr := 0.0
	for i, want := range ref.Embedding {
		if d := math.Abs(float64(got[i]) - want); d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 2e-3 {
		t.Fatalf("embedding max error %.6f exceeds 2e-3", maxErr)
	}
	t.Logf("embedding max error %.6g", maxErr)
}

func loadPCM16(t *testing.T, n int) []int16 {
	t.Helper()
	raw, err := os.ReadFile("testdata/pcm16.bin")
	if err != nil {
		t.Fatalf("pcm16: %v", err)
	}
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out
}

// TestFrontendMatchesDevicePath checks the full chain on raw 16-bit PCM, which is
// what the framework tap delivers: mel → embedding must match the reference.
func TestFrontendMatchesDevicePath(t *testing.T) {
	ref := loadReference(t)
	if len(ref.PCM16Embedding0) == 0 {
		t.Skip("fixture has no PCM16 reference")
	}
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	fe, err := NewFrontend(a)
	if err != nil {
		t.Fatal(err)
	}
	pcm := loadPCM16(t, ref.PCM16Samples)
	var embeddings [][]float32
	// Feed in 20 ms chunks like the audio tap does.
	for i := 0; i < len(pcm); i += 320 {
		end := i + 320
		if end > len(pcm) {
			end = len(pcm)
		}
		embeddings = append(embeddings, fe.Feed(pcm[i:end])...)
	}
	if len(embeddings) != ref.PCM16Embeddings {
		t.Fatalf("%d embeddings, want %d", len(embeddings), ref.PCM16Embeddings)
	}
	maxErr := 0.0
	for i, want := range ref.PCM16Embedding0 {
		if d := math.Abs(float64(embeddings[0][i]) - want); d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 2e-3 {
		t.Fatalf("first embedding max error %.6f exceeds 2e-3", maxErr)
	}
	t.Logf("device-path embedding max error %.6g", maxErr)
}
