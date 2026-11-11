package wakeword

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"capsulad/internal/wakeword/oww"
)

func halfBits(v float32) uint16 {
	f := float64(v)
	sign := uint16(0)
	if f < 0 || (f == 0 && math.Signbit(f)) {
		sign = 0x8000
		f = math.Abs(f)
	}
	if f == 0 {
		return sign
	}
	if math.IsInf(f, 0) {
		return sign | 0x7c00
	}
	if math.IsNaN(f) {
		return 0x7e00
	}
	exp := 0
	for f >= 2 {
		f /= 2
		exp++
	}
	for f < 1 {
		f *= 2
		exp--
	}
	if exp < -14 {
		m := uint16(math.Round(math.Ldexp(f, exp+24)))
		return sign | m
	}
	e := uint16(exp + 15)
	m := uint16(math.Round((f - 1) * 1024))
	if m == 1024 {
		e++
		m = 0
	}
	return sign | e<<10 | m
}

func negativeFrames(original int64) int {
	return int(negativesExpectedFrames(negativesPrerollSamples, original))
}

func baseNegativesHeader() Negatives {
	return Negatives{
		Version:               negativesVersion,
		EmbeddingDim:          oww.EmbeddingDim,
		HopSamples:            oww.StepSamples,
		FeatureExtractor:      negativesFeatureExtractor,
		FirstEmbeddingSamples: oww.FirstEmbeddingSamples,
		PrerollSamples:        negativesPrerollSamples,
		Sources: []NegativeSource{
			{ID: "src-cc0", URL: "https://example.test/cc0", License: "CC0-1.0"},
			{
				ID: "src-libri", URL: "https://www.openslr.org/12/", License: "CC-BY-4.0",
				Attribution: "Vassil Panayotov, Guoguo Chen, Daniel Povey, Sanjeev Khudanpur (2015)",
			},
		},
		Streams: []NegativeStream{
			{ID: "train-speech-1", SourceID: "src-libri", Kind: "speech", Split: "train",
				OriginalSamples: 1280, Frames: negativeFrames(1280)},
			{ID: "tune-noise-1", SourceID: "src-cc0", Kind: "noise", Split: "tune",
				OriginalSamples: 640, Frames: negativeFrames(640)},
			{ID: "test-music-1", SourceID: "src-cc0", Kind: "music", Split: "test",
				OriginalSamples: 2560, Frames: negativeFrames(2560)},
		},
	}
}

func negativesPayloadFor(header Negatives) []uint16 {
	total := 0
	for i := range header.Streams {
		total += header.Streams[i].Frames
	}
	out := make([]uint16, total*oww.EmbeddingDim)
	for i := range out {
		out[i] = halfBits(0.5)
	}
	return out
}

func negativesRaw(header Negatives, payload []uint16) []byte {
	rawHeader, err := json.Marshal(header)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	buf.WriteString(negativesMagic)
	if err := binary.Write(&buf, binary.LittleEndian, uint32(len(rawHeader))); err != nil {
		panic(err)
	}
	buf.Write(rawHeader)
	for _, v := range payload {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	return buf.Bytes()
}

func gzipBytes(raw []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func writeRawNegatives(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "negatives.bin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write corpus: %v", err)
	}
	return path
}

func writeNegatives(t *testing.T, header Negatives, payload []uint16) string {
	t.Helper()
	return writeRawNegatives(t, negativesRaw(header, payload))
}

// corpusExposureSeconds applies the loader's ExposureSeconds to every split.
// ExposureSeconds takes stream pointers, while Negatives.Streams holds values,
// so the corpus total is the sum over the split accessors.
func corpusExposureSeconds(neg *Negatives) float64 {
	var total float64
	for _, split := range []string{"train", "tune", "test"} {
		total += neg.ExposureSeconds(neg.StreamsForSplit(split))
	}
	return total
}

func TestLoadNegativesValid(t *testing.T) {
	header := baseNegativesHeader()
	payload := negativesPayloadFor(header)
	payload[0] = halfBits(1.5)
	payload[1] = halfBits(-0.25)
	path := writeNegatives(t, header, payload)

	neg, err := LoadNegatives(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(neg.Sources) != 2 || len(neg.Streams) != 3 {
		t.Fatalf("sources/streams = %d/%d", len(neg.Sources), len(neg.Streams))
	}
	if neg.Sources[1].Attribution == "" {
		t.Fatal("CC-BY attribution was dropped")
	}
	if len(neg.Streams[0].Embeddings) != header.Streams[0].Frames*oww.EmbeddingDim {
		t.Fatalf("stream 0 values = %d", len(neg.Streams[0].Embeddings))
	}
	if got := neg.Streams[0].OriginalSamples; got != 1280 {
		t.Fatalf("original samples = %d, want 1280", got)
	}
	if got := neg.Streams[0].Embeddings[0]; math.Abs(float64(got)-1.5) > 1e-6 {
		t.Fatalf("first embedding = %v, want 1.5", got)
	}
	if got := neg.Streams[0].Embeddings[1]; math.Abs(float64(got)+0.25) > 1e-6 {
		t.Fatalf("second embedding = %v, want -0.25", got)
	}
	// Exposure is the original body duration, never the frame count times the
	// hop and never including the reused preroll.
	if got := neg.ExposureSeconds(neg.StreamsForSplit("test")); math.Abs(got-2560.0/16000) > 1e-12 {
		t.Fatalf("test exposure = %v, want %v", got, 2560.0/16000)
	}
	if got := corpusExposureSeconds(neg); math.Abs(got-4480.0/16000) > 1e-12 {
		t.Fatalf("total exposure = %v, want %v", got, 4480.0/16000)
	}
	if got := len(neg.StreamsForSplit("tune")); got != 1 {
		t.Fatalf("tune streams = %d", got)
	}
}

func TestLoadNegativesGzip(t *testing.T) {
	header := baseNegativesHeader()
	payload := negativesPayloadFor(header)
	payload[0] = halfBits(1.5)
	payload[1] = halfBits(-0.25)
	path := writeRawNegatives(t, gzipBytes(negativesRaw(header, payload)))

	neg, err := LoadNegatives(path)
	if err != nil {
		t.Fatalf("load gzip corpus: %v", err)
	}
	if got := neg.Streams[0].Embeddings[0]; math.Abs(float64(got)-1.5) > 1e-6 {
		t.Fatalf("first embedding = %v, want 1.5", got)
	}
	if got := neg.Streams[0].Embeddings[1]; math.Abs(float64(got)+0.25) > 1e-6 {
		t.Fatalf("second embedding = %v, want -0.25", got)
	}
	if got := corpusExposureSeconds(neg); math.Abs(got-4480.0/16000) > 1e-12 {
		t.Fatalf("exposure = %v", got)
	}
}

func TestHalfToFloat32(t *testing.T) {
	cases := []struct {
		bits uint16
		want float32
	}{
		{0x0000, 0},
		{0x8000, float32(math.Copysign(0, -1))},
		{0x3800, 0.5},
		{0xbc00, -1},
		{0x3e00, 1.5},
		{0x4000, 2},
	}
	for _, c := range cases {
		got := halfToFloat32(c.bits)
		if math.Float32bits(got) != math.Float32bits(c.want) {
			t.Fatalf("half %#04x = %v, want %v", c.bits, got, c.want)
		}
	}
	if got := halfToFloat32(0x0001); math.Abs(float64(got)-math.Ldexp(1, -24)) > 1e-12 {
		t.Fatalf("subnormal = %v", got)
	}
	if got := halfToFloat32(0x7c00); !math.IsInf(float64(got), 1) {
		t.Fatalf("inf = %v", got)
	}
	if got := halfToFloat32(0x7e00); !math.IsNaN(float64(got)) {
		t.Fatalf("nan = %v", got)
	}
}

func TestLoadNegativesRejectsHeader(t *testing.T) {
	basePayload := negativesPayloadFor(baseNegativesHeader())

	cases := []struct {
		name    string
		mutate  func(*Negatives)
		payload []uint16
	}{
		{name: "cold version", mutate: func(h *Negatives) { h.Version = 1 }},
		{name: "unknown version", mutate: func(h *Negatives) { h.Version = 3 }},
		{name: "embedding dim", mutate: func(h *Negatives) { h.EmbeddingDim = 32 }},
		{name: "hop", mutate: func(h *Negatives) { h.HopSamples = 160 }},
		{name: "missing extractor", mutate: func(h *Negatives) { h.FeatureExtractor = "" }},
		{name: "wrong extractor", mutate: func(h *Negatives) { h.FeatureExtractor = "upstream-padded" }},
		{name: "wrong first embedding", mutate: func(h *Negatives) { h.FirstEmbeddingSamples = 12512 + 1280 }},
		{name: "missing preroll", mutate: func(h *Negatives) { h.PrerollSamples = 0 }},
		{name: "short preroll", mutate: func(h *Negatives) { h.PrerollSamples = 16000 }},
		{name: "nc license", mutate: func(h *Negatives) { h.Sources[0].License = "CC-BY-NC-4.0" }},
		{name: "nd license", mutate: func(h *Negatives) { h.Sources[0].License = "CC-BY-ND-4.0" }},
		{name: "unknown license", mutate: func(h *Negatives) { h.Sources[0].License = "MIT" }},
		{name: "missing license", mutate: func(h *Negatives) { h.Sources[0].License = "" }},
		{name: "missing source url", mutate: func(h *Negatives) { h.Sources[0].URL = "" }},
		{name: "empty source id", mutate: func(h *Negatives) { h.Sources[0].ID = "" }},
		{name: "duplicate source", mutate: func(h *Negatives) { h.Sources[1].ID = h.Sources[0].ID }},
		{name: "unknown source ref", mutate: func(h *Negatives) { h.Streams[0].SourceID = "nope" }},
		{name: "empty streams", mutate: func(h *Negatives) { h.Streams = nil }},
		{name: "empty sources", mutate: func(h *Negatives) { h.Sources = nil }},
		{name: "duplicate stream", mutate: func(h *Negatives) { h.Streams[1].ID = h.Streams[0].ID }},
		{name: "bad kind", mutate: func(h *Negatives) { h.Streams[0].Kind = "video" }},
		{name: "bad split", mutate: func(h *Negatives) { h.Streams[0].Split = "validation" }},
		{name: "zero original samples", mutate: func(h *Negatives) { h.Streams[0].OriginalSamples = 0 }},
		{name: "negative original samples", mutate: func(h *Negatives) { h.Streams[0].OriginalSamples = -1280 }},
		{name: "zero frames", mutate: func(h *Negatives) { h.Streams[0].Frames = 0 }},
		{name: "frames below formula", mutate: func(h *Negatives) { h.Streams[0].Frames-- }},
		{name: "frames above formula", mutate: func(h *Negatives) { h.Streams[0].Frames++ }},
		{name: "oversized original samples", mutate: func(h *Negatives) { h.Streams[0].OriginalSamples = maxNegativesOriginalSamples + 1 }},
		{
			name:   "non-finite payload",
			mutate: func(*Negatives) {},
			payload: func() []uint16 {
				p := append([]uint16(nil), basePayload...)
				p[3] = 0x7c00
				return p
			}(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			header := baseNegativesHeader()
			c.mutate(&header)
			payload := c.payload
			if payload == nil {
				payload = basePayload
			}
			path := writeNegatives(t, header, payload)
			if _, err := LoadNegatives(path); err == nil {
				t.Fatal("corpus was accepted")
			}
		})
	}
}

func TestLoadNegativesRejectsPayloadMismatch(t *testing.T) {
	header := baseNegativesHeader()
	payload := negativesPayloadFor(header)

	t.Run("truncated payload", func(t *testing.T) {
		path := writeNegatives(t, header, payload[:len(payload)-1])
		if _, err := LoadNegatives(path); err == nil {
			t.Fatal("truncated payload accepted")
		}
	})
	t.Run("excess payload", func(t *testing.T) {
		path := writeNegatives(t, header, append(append([]uint16(nil), payload...), halfBits(0.25)))
		if _, err := LoadNegatives(path); err == nil {
			t.Fatal("excess payload accepted")
		}
	})
	t.Run("trailing raw byte", func(t *testing.T) {
		raw := append(negativesRaw(header, payload), 0x00)
		if _, err := LoadNegatives(writeRawNegatives(t, raw)); err == nil {
			t.Fatal("trailing byte accepted")
		}
	})
}

// TestLoadNegativesRejectsDeclaredExpansion proves the declared payload is
// bounded before any float32 backing is allocated. The stream claims the
// largest frame count the frame bound allows, so header plus payload no longer
// fits the logical corpus bound; the file itself contains only the header.
func TestLoadNegativesRejectsDeclaredExpansion(t *testing.T) {
	header := baseNegativesHeader()
	frames := maxNegativesFrames
	header.Streams = []NegativeStream{{
		ID:              "train-huge",
		SourceID:        "src-cc0",
		Kind:            "noise",
		Split:           "train",
		OriginalSamples: (frames-1)*int64(oww.StepSamples) + int64(oww.FirstEmbeddingSamples) - negativesPrerollSamples,
		Frames:          int(frames),
	}}
	raw := negativesRaw(header, nil)
	if _, err := LoadNegatives(writeRawNegatives(t, raw)); err == nil {
		t.Fatal("oversized declared payload accepted")
	}
}

func TestLoadNegativesRejectsContainer(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "unknown magic", raw: append([]byte("CNEG0003"), 0, 0, 0, 0)},
		{name: "cold magic", raw: append([]byte("CNEG0001"), 0, 0, 0, 0)},
		{name: "huge header length", raw: append([]byte(negativesMagic), 0xff, 0xff, 0xff, 0xff)},
		{name: "truncated header", raw: []byte("CNEG0002\x10\x00\x00\x00short")},
		{name: "empty", raw: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadNegatives(writeRawNegatives(t, c.raw)); err == nil {
				t.Fatal("invalid corpus accepted")
			}
		})
	}
	if _, err := LoadNegatives(filepath.Join(t.TempDir(), "missing.bin")); err == nil {
		t.Fatal("missing corpus was accepted")
	}
}

func TestLoadNegativesRejectsGzipCorruption(t *testing.T) {
	header := baseNegativesHeader()
	payload := negativesPayloadFor(header)
	raw := negativesRaw(header, payload)
	good := gzipBytes(raw)

	t.Run("crc mismatch", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[len(bad)-1] ^= 0xff // corrupts the ISIZE/CRC trailer
		if _, err := LoadNegatives(writeRawNegatives(t, bad)); err == nil {
			t.Fatal("corrupt gzip trailer accepted")
		}
	})
	t.Run("truncated gzip", func(t *testing.T) {
		bad := append([]byte(nil), good[:len(good)-4]...)
		if _, err := LoadNegatives(writeRawNegatives(t, bad)); err == nil {
			t.Fatal("truncated gzip accepted")
		}
	})
	t.Run("trailing gzip bytes", func(t *testing.T) {
		bad := append(append([]byte(nil), good...), 0xde, 0xad, 0xbe, 0xef)
		if _, err := LoadNegatives(writeRawNegatives(t, bad)); err == nil {
			t.Fatal("trailing gzip bytes accepted")
		}
	})
	t.Run("trailing gzip member", func(t *testing.T) {
		bad := append(append([]byte(nil), good...), good...)
		if _, err := LoadNegatives(writeRawNegatives(t, bad)); err == nil {
			t.Fatal("trailing gzip member accepted")
		}
	})
	t.Run("decompression exceeds logical bound", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bomb.bin")
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		zw := gzip.NewWriter(f)
		if _, err := zw.Write(raw); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		if _, err := io.Copy(zw, &zeroReader{n: maxNegativesLogical + 1}); err != nil {
			t.Fatalf("write padding: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close file: %v", err)
		}
		if _, err := LoadNegatives(path); err == nil {
			t.Fatal("oversized decompressed stream accepted")
		}
	})
}

// zeroReader streams zeros without materialising them, so the expansion test
// does not need a 32 MiB buffer of its own.
type zeroReader struct {
	n int64
}

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	for i := range p {
		p[i] = 0
	}
	z.n -= int64(len(p))
	return len(p), nil
}

func TestValidateNegativesBoundsAndAttribution(t *testing.T) {
	header := baseNegativesHeader()
	if err := validateNegativesHeader(&header); err != nil {
		t.Fatalf("valid header rejected: %v", err)
	}

	header = baseNegativesHeader()
	header.Streams[0].Frames = int(maxNegativesFrames) + 1
	if err := validateNegativesHeader(&header); err == nil {
		t.Fatal("oversized frame count accepted")
	}

	header = baseNegativesHeader()
	header.Streams[0].OriginalSamples = maxNegativesOriginalSamples + 1
	if err := validateNegativesHeader(&header); err == nil {
		t.Fatal("oversized original sample count accepted")
	}

	header = baseNegativesHeader()
	header.Streams[0].Frames++
	if err := validateNegativesHeader(&header); err == nil {
		t.Fatal("frame count inconsistent with preroll + original accepted")
	}

	header = baseNegativesHeader()
	header.PrerollSamples = negativesPrerollSamples / 2
	if err := validateNegativesHeader(&header); err == nil {
		t.Fatal("short preroll accepted")
	}

	header = baseNegativesHeader()
	header.Sources[1].Attribution = ""
	if err := validateNegativesHeader(&header); err == nil {
		t.Fatal("missing CC-BY attribution accepted")
	}

	// CC0 needs no attribution.
	header = baseNegativesHeader()
	header.Sources[0].Attribution = ""
	if err := validateNegativesHeader(&header); err != nil {
		t.Fatalf("CC0 header rejected: %v", err)
	}
}
