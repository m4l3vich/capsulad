package wakeword

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"capsulad/internal/wakeword/oww"
)

// The negative-feature corpus is a single binary file so training can stream an
// hours-long background corpus without a second feature implementation on the
// device. The file is stored gzip-compressed by default; the loader detects the
// gzip magic, so the format is independent of the file extension. The logical
// (decompressed) stream is:
//
//	"CNEG0002"                     8 bytes
//	uint32 little-endian           JSON header byte length
//	UTF-8 JSON header              schema version 2
//	payload                        float16 embeddings, stream after stream
//
// Every stream is an independent recording/session, so a 16-embedding window
// taken from one stream never joins two sources. Each stream carries a warm
// preroll of real same-split noise, so the first embeddings of an otherwise
// cold recording are trainable instead of dropping out of the context window.
// The corpus builder is responsible for picking whole recordings per split; the
// loader validates the encoding, the warm-up accounting and the license
// provenance.
const (
	negativesMagic   = "CNEG0002"
	negativesVersion = 2
	// legacyNegativesMagic is only recognised so a stale cold corpus fails with
	// an explicit regeneration instruction instead of a generic magic mismatch.
	legacyNegativesMagic = "CNEG0001"
	// negativesPrerollSamples is 2.32 s of same-split real noise, aligned to
	// 29 embedding steps so the original body's frontend phase is preserved.
	negativesPrerollSamples = 37120
	// negativesFeatureExtractor identifies the exact oww timing contract the
	// payload was produced with.
	negativesFeatureExtractor = "oww-google-v1"
	maxNegativesHeader        = 4 << 20 // 4 MiB of JSON provenance
	// maxNegativesFile bounds the on-disk file and maxNegativesLogical bounds
	// the decompressed CNEG0002 stream. Both stay at 32 MiB on the 234 MiB
	// device; the payload is also bounded by maxNegativesFrames so a corrupt
	// header cannot overflow the frame accounting or slice past the payload.
	maxNegativesFile    = 32 << 20
	maxNegativesLogical = int64(32 << 20)
	// maxNegativesFrames bounds one stream inside the file bound.
	maxNegativesFrames = maxNegativesLogical / (2 * oww.EmbeddingDim)
	// maxNegativesOriginalSamples bounds one stream's original body so the
	// preroll addition and the derived frame count cannot overflow.
	maxNegativesOriginalSamples = maxNegativesFrames*int64(oww.StepSamples) + int64(oww.FirstEmbeddingSamples)
)

// NegativeSource is one licensed recording source in the corpus header.
type NegativeSource struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	License string `json:"license"`
	// Attribution preserves the creator credit that CC-BY licenses require.
	// CC-BY is not public domain: the credit travels with the data into the
	// trained artifact.
	Attribution string `json:"attribution,omitempty"`
}

// NegativeStream is one contiguous embedding stream from one source recording.
// OriginalSamples is the number of 16 kHz samples in the original recording
// body and excludes the reused preroll. Embeddings holds Frames*EmbeddingDim
// values in time-major order. Streams are never windowed across their
// boundaries.
type NegativeStream struct {
	ID              string    `json:"id"`
	SourceID        string    `json:"source_id"`
	Kind            string    `json:"kind"`
	Split           string    `json:"split"`
	OriginalSamples int64     `json:"original_samples"`
	Frames          int       `json:"frames"`
	Embeddings      []float32 `json:"-"`
}

// Negatives is a loaded CNEG0002 corpus.
type Negatives struct {
	Version      int `json:"version"`
	EmbeddingDim int `json:"embedding_dim"`
	HopSamples   int `json:"hop_samples"`
	// FeatureExtractor and FirstEmbeddingSamples pin the exact timing of the
	// payload. Upstream openWakeWord pads early windows differently; training on
	// features with a different first-embedding offset would silently misalign
	// the classifier against oww.EmbeddingEnd.
	FeatureExtractor      string `json:"feature_extractor"`
	FirstEmbeddingSamples int    `json:"first_embedding_samples"`
	// PrerollSamples is the same-split noise prefix every stream carries. It is
	// required and must equal negativesPrerollSamples.
	PrerollSamples int              `json:"preroll_samples"`
	Sources        []NegativeSource `json:"sources"`
	Streams        []NegativeStream `json:"streams"`
}

// negativesInput is a bounded reader over the logical CNEG0002 stream, possibly
// behind a gzip container.
type negativesInput struct {
	r      io.Reader
	verify func() error
	closer io.Closer
	gzip   *gzip.Reader
}

// boundedNegativesReader refuses to hand out more than the logical corpus bound
// so a compressed file cannot expand into an unbounded allocation.
type boundedNegativesReader struct {
	r         io.Reader
	remaining int64
}

func (b *boundedNegativesReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var probe [1]byte
		n, err := b.r.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("CNEG0002 logical stream exceeds the %d byte bound", maxNegativesLogical)
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// openNegativesInput opens the corpus and detects the container from its magic.
// A gzip member is decoded strictly: trailing members or bytes, a truncated
// stream, and a CRC mismatch all fail instead of yielding a partial corpus.
func openNegativesInput(path string) (*negativesInput, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("negatives %s: %w", path, err)
	}
	fail := func(err error) (*negativesInput, error) {
		f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return fail(fmt.Errorf("negatives %s: %w", path, err))
	}
	if info.Size() > maxNegativesFile {
		return fail(fmt.Errorf("negatives %s: %d bytes exceeds the %d byte file bound", path, info.Size(), int64(maxNegativesFile)))
	}

	br := bufio.NewReader(f)
	head, err := br.Peek(2)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fail(fmt.Errorf("negatives %s: file is %d bytes, too short for a CNEG0002 header", path, info.Size()))
		}
		return fail(fmt.Errorf("negatives %s: reading header: %w", path, err))
	}
	in := &negativesInput{closer: f}
	if head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fail(fmt.Errorf("negatives %s: gzip header: %w", path, err))
		}
		gz.Multistream(false)
		in.gzip = gz
		in.r = &boundedNegativesReader{r: gz, remaining: maxNegativesLogical}
		in.verify = func() error {
			if _, err := br.Peek(1); err == nil {
				return fmt.Errorf("negatives %s: trailing bytes after the gzip-compressed CNEG0002 stream", path)
			} else if !errors.Is(err, io.EOF) {
				return fmt.Errorf("negatives %s: trailing check: %w", path, err)
			}
			return nil
		}
	} else {
		in.r = &boundedNegativesReader{r: br, remaining: maxNegativesLogical}
	}
	return in, nil
}

// requireNegativesEOF proves the declared streams consume the whole logical
// corpus. For gzip it also forces the reader through the footer, so CRC and
// truncation errors surface here rather than being silently ignored.
func requireNegativesEOF(r io.Reader, path string) error {
	var extra [1]byte
	n, err := io.ReadFull(r, extra[:])
	if n > 0 {
		return fmt.Errorf("negatives %s: trailing payload beyond the declared streams", path)
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return fmt.Errorf("negatives %s: reading payload end: %w", path, err)
}

// LoadNegatives reads and validates a CNEG0002 corpus. Sources must carry an
// explicit license from the allowlist; NC, ND and unknown licenses are rejected
// because they cannot be redistributed or used for this training set.
func LoadNegatives(path string) (*Negatives, error) {
	in, err := openNegativesInput(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if in.gzip != nil {
			in.gzip.Close()
		}
		in.closer.Close()
	}()

	var prefix [12]byte
	if _, err := io.ReadFull(in.r, prefix[:]); err != nil {
		return nil, fmt.Errorf("negatives %s: reading magic: %w", path, err)
	}
	magic := string(prefix[:8])
	if magic == legacyNegativesMagic {
		return nil, fmt.Errorf("negatives %s: %s is the obsolete cold corpus format; regenerate a warm CNEG0002 corpus with tools/build_wakeword_negatives.py", path, magic)
	}
	if magic != negativesMagic {
		return nil, fmt.Errorf("negatives %s: magic %q, want %q", path, magic, negativesMagic)
	}
	headerLen := int(binary.LittleEndian.Uint32(prefix[8:]))
	if headerLen <= 0 || headerLen > maxNegativesHeader {
		return nil, fmt.Errorf("negatives %s: header length %d out of range", path, headerLen)
	}
	if int64(12+headerLen) > maxNegativesLogical {
		return nil, fmt.Errorf("negatives %s: header length %d exceeds the %d byte logical corpus bound", path, headerLen, maxNegativesLogical)
	}

	rawHeader := make([]byte, headerLen)
	if _, err := io.ReadFull(in.r, rawHeader); err != nil {
		return nil, fmt.Errorf("negatives %s: reading header: %w", path, err)
	}
	var neg Negatives
	if err := json.Unmarshal(rawHeader, &neg); err != nil {
		return nil, fmt.Errorf("negatives %s: header: %w", path, err)
	}
	if err := validateNegativesHeader(&neg); err != nil {
		return nil, fmt.Errorf("negatives %s: %w", path, err)
	}

	// Every stream's declared size is checked against the logical bound before
	// a single float32 is allocated, so a forged header cannot force a huge
	// allocation or a slice past the payload.
	var totalFrames int64
	for i := range neg.Streams {
		frames := int64(neg.Streams[i].Frames)
		if frames > maxNegativesFrames-totalFrames {
			return nil, fmt.Errorf("negatives %s: declared streams exceed the %d frame bound", path, maxNegativesFrames)
		}
		totalFrames += frames
	}
	const sampleBytes = int64(2 * oww.EmbeddingDim)
	payloadBytes := totalFrames * sampleBytes
	if int64(12+headerLen)+payloadBytes > maxNegativesLogical {
		return nil, fmt.Errorf("negatives %s: declared %d byte payload exceeds the %d byte logical corpus bound", path, payloadBytes, maxNegativesLogical)
	}

	flat := make([]float32, int(totalFrames)*oww.EmbeddingDim)
	buf := make([]byte, 1<<20)
	pos := 0
	remaining := payloadBytes
	for remaining > 0 {
		n := int64(len(buf))
		if n > remaining {
			n = remaining
		}
		if _, err := io.ReadFull(in.r, buf[:n]); err != nil {
			return nil, fmt.Errorf("negatives %s: reading payload: %w", path, err)
		}
		for i := int64(0); i+1 < n; i += 2 {
			v := halfToFloat32(binary.LittleEndian.Uint16(buf[i:]))
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, fmt.Errorf("negatives %s: non-finite value at frame %d value %d", path, (pos / int(oww.EmbeddingDim)), pos%int(oww.EmbeddingDim))
			}
			flat[pos] = v
			pos++
		}
		remaining -= n
	}
	if pos != len(flat) {
		return nil, fmt.Errorf("negatives %s: decoded %d values, want %d", path, pos, len(flat))
	}
	if err := requireNegativesEOF(in.r, path); err != nil {
		return nil, err
	}
	if in.verify != nil {
		if err := in.verify(); err != nil {
			return nil, err
		}
	}

	offset := 0
	for i := range neg.Streams {
		frames := neg.Streams[i].Frames
		neg.Streams[i].Embeddings = flat[offset*oww.EmbeddingDim : (offset+frames)*oww.EmbeddingDim]
		offset += frames
	}
	return &neg, nil
}

// negativesExpectedFrames is the frame count implied by a warm stream: the
// preroll and the original body are one continuous 16 kHz signal fed through
// the oww frontend. Both inputs are bounded positive values, so the addition
// before the division cannot overflow.
func negativesExpectedFrames(preroll int, original int64) int64 {
	total := int64(preroll) + original
	return (total-int64(oww.FirstEmbeddingSamples))/int64(oww.StepSamples) + 1
}

func validateNegativesHeader(neg *Negatives) error {
	if neg.Version != negativesVersion {
		return fmt.Errorf("header version %d, want %d; a cold CNEG0001 corpus cannot be reused — regenerate a warm CNEG0002 corpus with tools/build_wakeword_negatives.py", neg.Version, negativesVersion)
	}
	if neg.EmbeddingDim != oww.EmbeddingDim {
		return fmt.Errorf("header embedding_dim %d, want %d", neg.EmbeddingDim, oww.EmbeddingDim)
	}
	if neg.HopSamples != oww.StepSamples {
		return fmt.Errorf("header hop_samples %d, want %d", neg.HopSamples, oww.StepSamples)
	}
	if neg.FeatureExtractor != negativesFeatureExtractor {
		return fmt.Errorf("header feature_extractor %q, want %q", neg.FeatureExtractor, negativesFeatureExtractor)
	}
	if neg.FirstEmbeddingSamples != oww.FirstEmbeddingSamples {
		return fmt.Errorf("header first_embedding_samples %d, want %d", neg.FirstEmbeddingSamples, oww.FirstEmbeddingSamples)
	}
	if neg.PrerollSamples != negativesPrerollSamples {
		return fmt.Errorf("header preroll_samples %d, want %d; the corpus was not built with the required same-split warm-up — regenerate it with tools/build_wakeword_negatives.py", neg.PrerollSamples, negativesPrerollSamples)
	}
	if len(neg.Sources) == 0 {
		return fmt.Errorf("header lists no sources")
	}
	seenSources := make(map[string]bool, len(neg.Sources))
	for i := range neg.Sources {
		src := &neg.Sources[i]
		src.ID = strings.TrimSpace(src.ID)
		if src.ID == "" {
			return fmt.Errorf("source %d has no id", i)
		}
		if seenSources[src.ID] {
			return fmt.Errorf("duplicate source id %q", src.ID)
		}
		seenSources[src.ID] = true
		if strings.TrimSpace(src.URL) == "" {
			return fmt.Errorf("source %q has no url", src.ID)
		}
		if err := checkNegativesLicense(src.License); err != nil {
			return fmt.Errorf("source %q: %w", src.ID, err)
		}
		if licenseRequiresAttribution(src.License) && strings.TrimSpace(src.Attribution) == "" {
			return fmt.Errorf("source %q: license %q requires an attribution string", src.ID, src.License)
		}
	}
	if len(neg.Streams) == 0 {
		return fmt.Errorf("header lists no streams")
	}
	seenStreams := make(map[string]bool, len(neg.Streams))
	for i := range neg.Streams {
		st := &neg.Streams[i]
		st.ID = strings.TrimSpace(st.ID)
		if st.ID == "" {
			return fmt.Errorf("stream %d has no id", i)
		}
		if seenStreams[st.ID] {
			return fmt.Errorf("duplicate stream id %q", st.ID)
		}
		seenStreams[st.ID] = true
		if !seenSources[st.SourceID] {
			return fmt.Errorf("stream %q references unknown source %q", st.ID, st.SourceID)
		}
		switch st.Kind {
		case "speech", "music", "noise":
		default:
			return fmt.Errorf("stream %q kind %q, want speech, music or noise", st.ID, st.Kind)
		}
		switch st.Split {
		case "train", "tune", "test":
		default:
			return fmt.Errorf("stream %q split %q, want train, tune or test", st.ID, st.Split)
		}
		if st.OriginalSamples <= 0 {
			return fmt.Errorf("stream %q has %d original_samples, want a positive body length", st.ID, st.OriginalSamples)
		}
		if st.OriginalSamples > maxNegativesOriginalSamples {
			return fmt.Errorf("stream %q has %d original_samples, beyond the %d sample bound", st.ID, st.OriginalSamples, maxNegativesOriginalSamples)
		}
		if st.Frames <= 0 {
			return fmt.Errorf("stream %q has %d frames", st.ID, st.Frames)
		}
		if int64(st.Frames) > maxNegativesFrames {
			return fmt.Errorf("stream %q has %d frames, beyond the %d frame bound", st.ID, st.Frames, maxNegativesFrames)
		}
		want := negativesExpectedFrames(neg.PrerollSamples, st.OriginalSamples)
		if int64(st.Frames) != want {
			return fmt.Errorf("stream %q declares %d frames but %d preroll + %d original samples produce %d",
				st.ID, st.Frames, neg.PrerollSamples, st.OriginalSamples, want)
		}
	}
	return nil
}

// licenseRequiresAttribution reports whether the license needs the creator
// credit carried in NegativeSource.Attribution.
func licenseRequiresAttribution(license string) bool {
	switch strings.ToLower(strings.TrimSpace(license)) {
	case "cc-by-3.0", "cc-by-4.0":
		return true
	}
	return false
}

// checkNegativesLicense accepts only redistribution-safe licenses. CC-BY
// requires attribution (kept in NegativeSource.Attribution), never NC or ND.
func checkNegativesLicense(license string) error {
	l := strings.ToLower(strings.TrimSpace(license))
	if l == "" {
		return fmt.Errorf("missing license (allowed: CC0-1.0, CC-BY-4.0, CC-BY-3.0, Public-Domain)")
	}
	switch l {
	case "cc0-1.0", "cc-by-4.0", "cc-by-3.0", "public-domain":
		return nil
	}
	if strings.Contains(l, "nc") || strings.Contains(l, "noncommercial") ||
		strings.Contains(l, "nd") || strings.Contains(l, "noderiv") {
		return fmt.Errorf("license %q is noncommercial or no-derivatives and cannot be used", license)
	}
	return fmt.Errorf("license %q is not in the allowed set (CC0-1.0, CC-BY-4.0, CC-BY-3.0, Public-Domain)", license)
}

// halfToFloat32 converts an IEEE 754 binary16 value to float32, including
// subnormals and infinities (the loader rejects non-finite results).
func halfToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// Subnormal half: normalize the mantissa into float32 form. After the
		// loop the leading one is at bit 10 and `shifts` counts how far the
		// 10-bit mantissa moved.
		shifts := uint32(0)
		for mant&0x400 == 0 {
			mant <<= 1
			shifts++
		}
		mant &= 0x3ff
		exp32 := uint32(127-24) + (10 - shifts)
		return math.Float32frombits(sign | exp32<<23 | mant<<13)
	case 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	default:
		return math.Float32frombits(sign | (exp+112)<<23 | mant<<13)
	}
}

// StreamsForSplit returns the streams whose header split matches split.
func (n *Negatives) StreamsForSplit(split string) []*NegativeStream {
	var out []*NegativeStream
	for i := range n.Streams {
		if n.Streams[i].Split == split {
			out = append(out, &n.Streams[i])
		}
	}
	return out
}

// ExposureSeconds reports the total original audio represented by the given
// streams. It sums each stream's original body samples: the reused same-split
// preroll is already counted where it was recorded, so counting it again here
// would overstate exposure, and frame hops never equal the underlying audio.
func (n *Negatives) ExposureSeconds(streams []*NegativeStream) float64 {
	var samples int64
	for _, st := range streams {
		samples += st.OriginalSamples
	}
	return float64(samples) / float64(enrollmentSampleRate)
}
