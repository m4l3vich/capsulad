// Command wakeword-negatives turns a corpus manifest of licence-clean 16 kHz
// mono PCM recordings into the CNEG0002 warmed negative-embedding format
// consumed by the on-device wake-word trainer.
//
// Every stream is warmed with a 2.32 s (37120 sample = 29 * 1280) preroll of
// real noise from the same split before the original source body is fed: the embeddings that
// cover the first seconds of speech are then computed from a continuous
// background instead of the cold all-zero frontend state. The noise clip,
// offset and gain are chosen deterministically from the stream ID, so an
// identical manifest always produces an identical corpus. The source body is
// copied exactly once, in order and unchanged; only the leading noise samples
// are added. A stream whose split has no usable noise recording is an error.
//
// The command runs the same Apache-2.0 openWakeWord frontend/embedding network
// that the live detector uses (internal/wakeword/oww), so the exported
// embeddings have exactly the same timing and alignment as runtime features.
// A fresh frontend is used for every stream, therefore no embedding window can
// span two source files or sessions. The output is a deterministic gzip
// container written atomically:
//
//	gzip                              (mtime 0, no filename; standard library)
//	"CNEG0002"                        8 ASCII bytes
//	uint32 LE                         JSON header byte length
//	UTF-8 JSON                        header (see cnegHeader)
//	N * 96 * 2 bytes                  little-endian IEEE float16 embeddings
//
// The builder script (tools/build_wakeword_negatives.py) owns download and
// ffmpeg normalisation; this command only validates the manifest and the
// converted WAV inputs. It refuses non-commercial, no-derivatives, unknown or
// malformed licences, path traversal, duplicate files, missing or silent
// same-split noise, short recordings, truncated WAV data, non-finite
// embeddings and float16 overflow.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"capsulad/internal/wakeword/oww"
)

const (
	formatMagic           = "CNEG0002"
	embeddingDim          = 96
	hopSamples            = 1280
	headerVer             = 2
	featureExtractor      = "oww-google-v1"
	firstEmbeddingSamples = 12512
	// prerollSamples is the mandatory warmed-up prefix duration: 29 embedding
	// steps (2.32 s at 16 kHz). The 1280-sample alignment keeps the source
	// mel/backbone phase identical to the cold export, so the rows after the
	// prefix reproduce the original voice features. It precedes every stream
	// and is excluded from original_samples.
	prerollSamples = 37120
	// maxNegativesFrames bounds one stream's float16 payload.
	maxNegativesFrames = 349525
	// maxNegativesContainer bounds both the compressed file and the logical
	// (decompressed) CNEG0002 container, matching the runtime loader.
	maxNegativesContainer = 32 << 20
	// prerollRMSMin/prerollRMSMax bound the quiet but variable prefix level in
	// raw int16 units (about -58 dBFS to -38 dBFS).
	prerollRMSMin = 40.0
	prerollRMSMax = 400.0
	// prerollSeedDomain keeps the ID-derived choice independent of any other
	// hash this command computes.
	prerollSeedDomain = "capsulad-cneg0002-preroll\x00"
)

// rawManifest is the builder-facing source manifest. Only these fields are
// required; extra provenance fields (for example a per-stream actual URL or
// checksum) are deliberately ignored here and preserved by the Python builder.
type rawManifest struct {
	Version int        `json:"version"`
	Sources []srcEntry `json:"sources"`
	Streams []strEntry `json:"streams"`
}

type srcEntry struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	License     string `json:"license"`
	Attribution string `json:"attribution,omitempty"`
	Language    string `json:"language,omitempty"`
}

type strEntry struct {
	ID        string `json:"id"`
	SourceID  string `json:"source_id"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Split     string `json:"split"`
	URL       string `json:"url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	Language  string `json:"language,omitempty"`
}

// cnegHeader is marshalled into the binary file. Field order is intentional:
// version, embedding_dim, hop_samples, feature_extractor,
// first_embedding_samples, preroll_samples, sources, streams; each source is
// id, url, license, attribution (omitted when empty); each stream is
// id, source_id, kind, split, original_samples, frames.
type cnegHeader struct {
	Version               int          `json:"version"`
	EmbeddingDim          int          `json:"embedding_dim"`
	HopSamples            int          `json:"hop_samples"`
	FeatureExtractor      string       `json:"feature_extractor"`
	FirstEmbeddingSamples int          `json:"first_embedding_samples"`
	PrerollSamples        int          `json:"preroll_samples"`
	Sources               []cnegSource `json:"sources"`
	Streams               []cnegStream `json:"streams"`
}

type cnegSource struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	License     string `json:"license"`
	Attribution string `json:"attribution,omitempty"`
}

// cnegStream describes one warmed stream. OriginalSamples is the exact number
// of 16 kHz samples in the source body (prefix excluded); Frames must equal
// floor((preroll_samples+original_samples-first_embedding_samples)/hop)+1.
type cnegStream struct {
	ID              string `json:"id"`
	SourceID        string `json:"source_id"`
	Kind            string `json:"kind"`
	Split           string `json:"split"`
	OriginalSamples int64  `json:"original_samples"`
	Frames          int    `json:"frames"`
}

type provenance struct {
	Version            int                `json:"version"`
	Generator          string             `json:"generator"`
	Format             string             `json:"format"`
	Compression        string             `json:"compression"`
	PrerollSamples     int                `json:"preroll_samples"`
	Output             string             `json:"output"`
	OutputBytes        int64              `json:"output_bytes"`
	OutputLogicalBytes int64              `json:"output_logical_bytes"`
	OutputSHA256       string             `json:"output_sha256"`
	Header             cnegHeader         `json:"header"`
	Streams            []provenanceStream `json:"streams"`
	Totals             provenanceTotals   `json:"totals"`
}

type provenanceStream struct {
	ID                 string  `json:"id"`
	SourceID           string  `json:"source_id"`
	Input              string  `json:"input"`
	InputSHA256        string  `json:"input_sha256"`
	InputURL           string  `json:"input_url,omitempty"`
	SourceRef          string  `json:"source_ref,omitempty"`
	Language           string  `json:"language,omitempty"`
	Kind               string  `json:"kind"`
	Split              string  `json:"split"`
	SampleRate         int     `json:"sample_rate"`
	Channels           int     `json:"channels"`
	BitsPerSample      int     `json:"bits_per_sample"`
	OriginalSamples    int64   `json:"original_samples"`
	PrerollSamples     int     `json:"preroll_samples"`
	PrerollNoiseID     string  `json:"preroll_noise_id"`
	PrerollNoiseInput  string  `json:"preroll_noise_input"`
	PrerollNoiseSHA256 string  `json:"preroll_noise_sha256"`
	PrerollNoiseOffset int64   `json:"preroll_noise_offset"`
	PrerollGain        float64 `json:"preroll_gain"`
	PrerollTargetRMS   float64 `json:"preroll_target_rms"`
	PrerollActualRMS   float64 `json:"preroll_actual_rms"`
	Frames             int     `json:"frames"`
	PayloadOffset      int64   `json:"payload_offset"`
	PayloadBytes       int64   `json:"payload_bytes"`
}

type provenanceTotals struct {
	Streams         int     `json:"streams"`
	Frames          int     `json:"frames"`
	OriginalSamples int64   `json:"original_samples"`
	ExposureSeconds float64 `json:"exposure_seconds"`
	PrerollSamples  int64   `json:"preroll_samples_total"`
	PayloadBytes    int64   `json:"payload_bytes"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "wakeword-negatives: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		manifestPath = flag.String("manifest", "", "source manifest JSON (required)")
		outPath      = flag.String("out", "", "CNEG0002 gzip output path (required)")
		provPath     = flag.String("provenance", "", "provenance path (default: <out>.provenance.json.gz; a .gz path is gzip-compressed)")
		chunkSamples = flag.Int("chunk-samples", 16000, "samples pushed through the frontend per chunk")
	)
	flag.Parse()
	if *manifestPath == "" || *outPath == "" {
		return errors.New("-manifest and -out are required")
	}
	if *chunkSamples <= 0 || *chunkSamples > 1<<20 {
		return fmt.Errorf("invalid -chunk-samples %d", *chunkSamples)
	}
	if *provPath == "" {
		*provPath = *outPath + ".provenance.json.gz"
	}

	m, err := loadRawManifest(*manifestPath)
	if err != nil {
		return err
	}
	srcByID, err := validateManifest(m)
	if err != nil {
		return err
	}
	baseDir := filepath.Dir(*manifestPath)
	noise, err := newNoiseIndex(baseDir, m.Streams)
	if err != nil {
		return err
	}

	if oww.EmbeddingDim != embeddingDim || oww.StepSamples != hopSamples ||
		oww.FirstEmbeddingSamples != firstEmbeddingSamples {
		return fmt.Errorf("native frontend timing changed: dim=%d step=%d first=%d, expected %d/%d/%d",
			oww.EmbeddingDim, oww.StepSamples, oww.FirstEmbeddingSamples,
			embeddingDim, hopSamples, firstEmbeddingSamples)
	}

	assets, err := oww.Load()
	if err != nil {
		return fmt.Errorf("load oww assets: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "wakeword-negatives-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	hdr := cnegHeader{
		Version:               headerVer,
		EmbeddingDim:          embeddingDim,
		HopSamples:            hopSamples,
		FeatureExtractor:      featureExtractor,
		FirstEmbeddingSamples: firstEmbeddingSamples,
		PrerollSamples:        prerollSamples,
		Sources:               make([]cnegSource, 0, len(m.Sources)),
		Streams:               make([]cnegStream, 0, len(m.Streams)),
	}
	for _, s := range m.Sources {
		hdr.Sources = append(hdr.Sources, cnegSource{
			ID:          s.ID,
			URL:         strings.TrimSpace(s.URL),
			License:     canonicalLicense(s.License),
			Attribution: strings.TrimSpace(s.Attribution),
		})
	}

	prov := provenance{
		Version:        2,
		Generator:      "capsulad/cmd/wakeword-negatives",
		Format:         formatMagic,
		Compression:    "gzip",
		PrerollSamples: prerollSamples,
		Output:         *outPath,
	}
	var payloadBytes int64
	var originalSamplesTotal int64
	for i, st := range m.Streams {
		path, err := resolveInput(baseDir, st.Path)
		if err != nil {
			return fmt.Errorf("stream %q: %w", st.ID, err)
		}
		prefix, pm, err := noise.buildPreroll(st)
		if err != nil {
			return fmt.Errorf("stream %q: %w", st.ID, err)
		}
		streamPath := filepath.Join(tmpDir, fmt.Sprintf("stream-%04d.f16", i))
		frames, samples, info, err := exportStream(assets, path, prefix, *chunkSamples, streamPath)
		if err != nil {
			return fmt.Errorf("stream %q (%s): %w", st.ID, path, err)
		}
		if frames == 0 {
			return fmt.Errorf("stream %q (%s): too short: %d samples, need at least %d for one embedding",
				st.ID, path, samples, oww.FirstEmbeddingSamples)
		}
		wantFrames, err := expectedFrames(int64(prerollSamples), samples)
		if err != nil {
			return fmt.Errorf("stream %q (%s): %w", st.ID, path, err)
		}
		if frames != wantFrames {
			return fmt.Errorf("stream %q (%s): frontend produced %d frames, expected %d for %d preroll + %d original samples",
				st.ID, path, frames, wantFrames, prerollSamples, samples)
		}
		if frames > maxNegativesFrames {
			return fmt.Errorf("stream %q (%s): %d frames exceeds limit %d", st.ID, path, frames, maxNegativesFrames)
		}
		streamPayload := int64(frames) * embeddingDim * 2
		if payloadBytes > maxNegativesContainer-streamPayload {
			return fmt.Errorf("stream %q (%s): aggregate payload exceeds the %d-byte logical CNEG0002 bound",
				st.ID, path, maxNegativesContainer)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("stream %q: checksum: %w", st.ID, err)
		}
		src := srcByID[st.SourceID]
		ps := provenanceStream{
			ID:                 st.ID,
			SourceID:           st.SourceID,
			Input:              path,
			InputSHA256:        sum,
			InputURL:           strings.TrimSpace(st.URL),
			SourceRef:          strings.TrimSpace(st.SourceRef),
			Language:           strings.TrimSpace(firstNonEmpty(st.Language, src.Language)),
			Kind:               st.Kind,
			Split:              st.Split,
			SampleRate:         info.sampleRate,
			Channels:           info.channels,
			BitsPerSample:      info.bitsPerSample,
			OriginalSamples:    samples,
			PrerollSamples:     len(prefix),
			PrerollNoiseID:     pm.noiseID,
			PrerollNoiseInput:  pm.noisePath,
			PrerollNoiseSHA256: pm.noiseSHA256,
			PrerollNoiseOffset: pm.offset,
			PrerollGain:        pm.gain,
			PrerollTargetRMS:   pm.targetRMS,
			PrerollActualRMS:   pm.actualRMS,
			Frames:             frames,
			PayloadOffset:      payloadBytes,
			PayloadBytes:       streamPayload,
		}
		if ps.InputURL == "" {
			ps.InputURL = strings.TrimSpace(src.URL)
		}
		prov.Streams = append(prov.Streams, ps)
		hdr.Streams = append(hdr.Streams, cnegStream{
			ID: st.ID, SourceID: st.SourceID, Kind: st.Kind, Split: st.Split,
			OriginalSamples: samples, Frames: frames,
		})
		payloadBytes += streamPayload
		originalSamplesTotal += samples
		fmt.Fprintf(os.Stderr, "wakeword-negatives: %s: %d original samples + %d preroll (%s @ %d, gain %.6f) -> %d frames (%.1fs)\n",
			st.ID, samples, len(prefix), pm.noiseID, pm.offset, pm.gain, frames,
			float64(samples)/float64(oww.SampleRate))
	}

	// Copy the completed header (including stream frame counts) into provenance.
	prov.Header = hdr
	prov.Totals = provenanceTotals{
		Streams:         len(prov.Streams),
		Frames:          totalFrames(hdr),
		OriginalSamples: originalSamplesTotal,
		ExposureSeconds: float64(originalSamplesTotal) / float64(oww.SampleRate),
		PrerollSamples:  int64(prerollSamples) * int64(len(prov.Streams)),
		PayloadBytes:    payloadBytes,
	}

	// Marshal the final header only after every stream has a frame count.
	headerJSON, err := marshalHeader(hdr)
	if err != nil {
		return fmt.Errorf("marshal header: %w", err)
	}
	if len(headerJSON) > math.MaxUint32 {
		return fmt.Errorf("header too large: %d bytes", len(headerJSON))
	}

	logicalBytes, err := writeArtifact(*outPath, headerJSON, tmpDir, hdr.Streams, payloadBytes)
	if err != nil {
		return err
	}
	sum, size, err := hashFile(*outPath)
	if err != nil {
		return fmt.Errorf("output checksum: %w", err)
	}
	prov.OutputLogicalBytes = logicalBytes
	prov.OutputBytes = size
	prov.OutputSHA256 = sum
	if err := writeJSONArtifact(*provPath, prov); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	fmt.Fprintf(os.Stderr, "wakeword-negatives: wrote %s (%d compressed bytes, %d logical bytes, %d streams, %d frames total, exposure %.1fs)\n",
		*outPath, size, logicalBytes, len(hdr.Streams), totalFrames(hdr),
		float64(originalSamplesTotal)/float64(oww.SampleRate))
	return nil
}

func marshalHeader(h cnegHeader) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func totalFrames(h cnegHeader) int {
	n := 0
	for _, s := range h.Streams {
		n += s.Frames
	}
	return n
}

func loadRawManifest(path string) (*rawManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var m rawManifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	// Reject trailing JSON documents or junk.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("manifest contains more than one JSON document")
		}
		return nil, fmt.Errorf("manifest trailing data: %w", err)
	}
	if m.Version != 0 && m.Version != 1 {
		return nil, fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	return &m, nil
}

// canonicalLicense returns the canonical spelling used in the binary header.
func canonicalLicense(s string) string {
	x := strings.ToLower(strings.TrimSpace(s))
	x = strings.ReplaceAll(x, "_", "-")
	x = strings.ReplaceAll(x, " ", "-")
	switch x {
	case "cc0-1.0":
		return "CC0-1.0"
	case "cc-by-4.0":
		return "CC-BY-4.0"
	case "cc-by-3.0":
		return "CC-BY-3.0"
	case "public-domain":
		return "Public-Domain"
	}
	return strings.TrimSpace(s)
}

func validateLicense(s string) error {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return errors.New("missing license")
	}
	low := strings.ToLower(raw)
	if strings.Contains(low, "noncommercial") || strings.Contains(low, "non-commercial") ||
		strings.Contains(low, "noderiv") || strings.Contains(low, "no-deriv") ||
		strings.Contains(low, "sharealike") || strings.Contains(low, "share-alike") ||
		strings.Contains(low, "-nc") || strings.HasSuffix(low, "nc") ||
		strings.Contains(low, "-nd") || strings.HasSuffix(low, "nd") ||
		strings.Contains(low, "-sa") || strings.HasSuffix(low, "sa") {
		return fmt.Errorf("licence %q is NC/ND/SA and is not allowed", raw)
	}
	switch canonicalLicense(raw) {
	case "CC0-1.0", "CC-BY-4.0", "CC-BY-3.0", "Public-Domain":
		return nil
	}
	return fmt.Errorf("unknown licence %q (allowed: CC0-1.0, CC-BY-4.0, CC-BY-3.0, Public-Domain)", raw)
}

func validID(s string) error {
	if s == "" {
		return errors.New("empty id")
	}
	if len(s) > 200 {
		return fmt.Errorf("id longer than 200 bytes")
	}
	if !utf8.ValidString(s) {
		return errors.New("id is not valid UTF-8")
	}
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return fmt.Errorf("id contains a control character or invalid rune")
		}
	}
	return nil
}

func validateManifest(m *rawManifest) (map[string]srcEntry, error) {
	if len(m.Sources) == 0 {
		return nil, errors.New("manifest has no sources")
	}
	if len(m.Streams) == 0 {
		return nil, errors.New("manifest has no streams")
	}
	srcByID := make(map[string]srcEntry, len(m.Sources))
	used := make(map[string]bool, len(m.Sources))
	for _, s := range m.Sources {
		if err := validID(s.ID); err != nil {
			return nil, fmt.Errorf("source id: %w", err)
		}
		if _, dup := srcByID[s.ID]; dup {
			return nil, fmt.Errorf("duplicate source id %q", s.ID)
		}
		if err := validateLicense(s.License); err != nil {
			return nil, fmt.Errorf("source %q: %w", s.ID, err)
		}
		license := canonicalLicense(s.License)
		if (license == "CC-BY-4.0" || license == "CC-BY-3.0") &&
			strings.TrimSpace(s.Attribution) == "" {
			return nil, fmt.Errorf("source %q: CC-BY licence needs a non-empty attribution", s.ID)
		}
		if strings.TrimSpace(s.URL) == "" {
			return nil, fmt.Errorf("source %q: missing url", s.ID)
		}
		srcByID[s.ID] = s
	}
	streamIDs := make(map[string]bool, len(m.Streams))
	paths := make(map[string]string, len(m.Streams))
	splitCounts := map[string]int{"train": 0, "tune": 0, "test": 0}
	for _, st := range m.Streams {
		if err := validID(st.ID); err != nil {
			return nil, fmt.Errorf("stream id: %w", err)
		}
		if streamIDs[st.ID] {
			return nil, fmt.Errorf("duplicate stream id %q", st.ID)
		}
		streamIDs[st.ID] = true
		if _, ok := srcByID[st.SourceID]; !ok {
			return nil, fmt.Errorf("stream %q: unknown source_id %q", st.ID, st.SourceID)
		}
		switch st.Kind {
		case "speech", "music", "noise":
		default:
			return nil, fmt.Errorf("stream %q: invalid kind %q (speech|music|noise)", st.ID, st.Kind)
		}
		switch st.Split {
		case "train", "tune", "test":
			splitCounts[st.Split]++
		default:
			return nil, fmt.Errorf("stream %q: invalid split %q (train|tune|test)", st.ID, st.Split)
		}
		clean, err := cleanRelativePath(st.Path)
		if err != nil {
			return nil, fmt.Errorf("stream %q: %w", st.ID, err)
		}
		if prev, dup := paths[clean]; dup {
			return nil, fmt.Errorf("stream %q reuses file %q already used by %q (whole-file holdouts)", st.ID, clean, prev)
		}
		paths[clean] = st.ID
		used[st.SourceID] = true
	}
	for id := range srcByID {
		if !used[id] {
			return nil, fmt.Errorf("source %q has no streams", id)
		}
	}
	for _, split := range []string{"train", "tune", "test"} {
		if splitCounts[split] == 0 {
			return nil, fmt.Errorf("manifest has no %s streams", split)
		}
	}
	return srcByID, nil
}

func cleanRelativePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("missing path")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative to the manifest", p)
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains NUL")
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the manifest directory", p)
	}
	return clean, nil
}

func resolveInput(baseDir, rel string) (string, error) {
	clean, err := cleanRelativePath(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(baseDir, clean)
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("input: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("input %q is not a regular file", abs)
	}
	return abs, nil
}

type wavInfo struct {
	dataOffset    int64
	dataSize      int64
	sampleRate    int
	channels      int
	bitsPerSample int
}

// noiseClip is one decoded 16 kHz mono noise recording available for prerolls.
type noiseClip struct {
	id      string
	path    string
	sha256  string
	samples []int16
}

// noiseIndex owns the same-split noise candidates and their decoded PCM. It
// bounds the choice to the stream's own split so a train stream never borrows
// tune/test background (and vice versa).
type noiseIndex struct {
	baseDir string
	bySplit map[string][]strEntry
	cache   map[string]*noiseClip
}

func newNoiseIndex(baseDir string, streams []strEntry) (*noiseIndex, error) {
	bySplit := make(map[string][]strEntry)
	for _, st := range streams {
		if st.Kind == "noise" {
			bySplit[st.Split] = append(bySplit[st.Split], st)
		}
	}
	for split, list := range bySplit {
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		bySplit[split] = list
	}
	for _, split := range []string{"train", "tune", "test"} {
		if len(bySplit[split]) == 0 {
			return nil, fmt.Errorf("split %q has no kind=noise stream; a real same-split preroll is required", split)
		}
	}
	return &noiseIndex{baseDir: baseDir, bySplit: bySplit, cache: make(map[string]*noiseClip)}, nil
}

func (ni *noiseIndex) load(entry strEntry) (*noiseClip, error) {
	path, err := resolveInput(ni.baseDir, entry.Path)
	if err != nil {
		return nil, fmt.Errorf("preroll noise %q: %w", entry.ID, err)
	}
	if clip, ok := ni.cache[path]; ok {
		return clip, nil
	}
	f, info, err := openPCM16WAV(path)
	if err != nil {
		return nil, fmt.Errorf("preroll noise %q: %w", entry.ID, err)
	}
	defer f.Close()
	if info.dataSize <= 0 || info.dataSize%2 != 0 {
		return nil, fmt.Errorf("preroll noise %q (%s): %d bytes of PCM", entry.ID, path, info.dataSize)
	}
	pcm := make([]int16, info.dataSize/2)
	raw := make([]byte, info.dataSize)
	if _, err := f.ReadAt(raw, info.dataOffset); err != nil {
		return nil, fmt.Errorf("preroll noise %q (%s): read PCM: %w", entry.ID, path, err)
	}
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2 : i*2+2]))
	}
	nonZero := false
	for _, v := range pcm {
		if v != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		return nil, fmt.Errorf("preroll noise %q (%s) is silent; refusing a zero-filled fallback", entry.ID, path)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return nil, fmt.Errorf("preroll noise %q checksum: %w", entry.ID, err)
	}
	clip := &noiseClip{id: entry.ID, path: path, sha256: sum, samples: pcm}
	ni.cache[path] = clip
	return clip, nil
}

// prerollMeta records the deterministic prefix decision for provenance.
type prerollMeta struct {
	noiseID     string
	noisePath   string
	noiseSHA256 string
	offset      int64
	gain        float64
	targetRMS   float64
	actualRMS   float64
}

// buildPreroll returns exactly prerollSamples int16 samples of real noise from
// the stream's own split. The clip, offset and quiet level are derived only
// from the stream ID, so reruns are reproducible. Short clips are tiled; the
// body audio is never used as padding and a missing/silent clip is an error.
func (ni *noiseIndex) buildPreroll(st strEntry) ([]int16, prerollMeta, error) {
	candidates := make([]strEntry, 0, len(ni.bySplit[st.Split]))
	for _, c := range ni.bySplit[st.Split] {
		if c.ID == st.ID {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil, prerollMeta{}, fmt.Errorf("no same-split noise stream available for preroll (split %q)", st.Split)
	}
	seed := sha256.Sum256([]byte(prerollSeedDomain + st.ID))
	clip, err := ni.load(candidates[binary.LittleEndian.Uint64(seed[0:8])%uint64(len(candidates))])
	if err != nil {
		return nil, prerollMeta{}, err
	}
	offset := int64(binary.LittleEndian.Uint64(seed[8:16]) % uint64(len(clip.samples)))
	prefix := make([]int16, prerollSamples)
	for i := range prefix {
		prefix[i] = clip.samples[(int(offset)+i)%len(clip.samples)]
	}

	var sumSq float64
	peak := 0
	for _, v := range prefix {
		f := float64(v)
		sumSq += f * f
		if a := int(math.Abs(f)); a > peak {
			peak = a
		}
	}
	rms := math.Sqrt(sumSq / float64(len(prefix)))
	if peak == 0 || rms <= 0 {
		return nil, prerollMeta{}, fmt.Errorf("preroll noise %q at offset %d is silent; refusing a zero-filled fallback", clip.id, offset)
	}
	// Log-uniform quiet target in [40, 400) raw int16 units.
	frac := float64(binary.LittleEndian.Uint64(seed[16:24])>>11) / float64(uint64(1)<<53)
	target := prerollRMSMin * math.Pow(prerollRMSMax/prerollRMSMin, frac)
	gain := target / rms
	if maxGain := 32767.0 / float64(peak); gain > maxGain {
		gain = maxGain // keep the scaled prefix inside int16 without clipping
	}
	if gain <= 0 {
		return nil, prerollMeta{}, fmt.Errorf("preroll noise %q: non-positive gain", clip.id)
	}
	var scaledSq float64
	for i, v := range prefix {
		x := math.Round(float64(v) * gain)
		if x > 32767 {
			x = 32767
		} else if x < -32768 {
			x = -32768
		}
		prefix[i] = int16(x)
		scaledSq += x * x
	}
	actualRMS := math.Sqrt(scaledSq / float64(len(prefix)))
	if actualRMS <= 0 {
		return nil, prerollMeta{}, fmt.Errorf("preroll noise %q: scaled prefix is silent", clip.id)
	}
	return prefix, prerollMeta{
		noiseID:     clip.id,
		noisePath:   clip.path,
		noiseSHA256: clip.sha256,
		offset:      offset,
		gain:        gain,
		targetRMS:   target,
		actualRMS:   actualRMS,
	}, nil
}

// expectedFrames is the format invariant: frames must match
// floor((preroll+original-first)/hop)+1 with overflow-safe addition.
func expectedFrames(preroll, original int64) (int, error) {
	if preroll != prerollSamples {
		return 0, fmt.Errorf("preroll is %d samples, expected %d", preroll, prerollSamples)
	}
	if original <= 0 {
		return 0, fmt.Errorf("original sample count %d must be positive", original)
	}
	if original > math.MaxInt64-preroll {
		return 0, errors.New("sample count overflows int64")
	}
	total := preroll + original
	if total < firstEmbeddingSamples {
		return 0, nil
	}
	frames := (total-firstEmbeddingSamples)/hopSamples + 1
	if int64(int(frames)) != frames {
		return 0, errors.New("frame count overflows int")
	}
	return int(frames), nil
}

// openPCM16WAV validates a RIFF/WAVE PCM file and returns the data chunk
// location. We accept exactly mono 16-bit 16 kHz PCM because that is what the
// runtime frontend consumes and what the Python builder writes via ffmpeg.
func openPCM16WAV(path string) (*os.File, wavInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, wavInfo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, wavInfo{}, err
	}
	size := st.Size()
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		f.Close()
		return nil, wavInfo{}, fmt.Errorf("truncated RIFF header")
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		f.Close()
		return nil, wavInfo{}, errors.New("not a RIFF/WAVE file")
	}
	pos := int64(12)
	var info wavInfo
	fmtOK := false
	for {
		if pos+8 > size {
			f.Close()
			return nil, wavInfo{}, errors.New("missing WAV data chunk")
		}
		var chunk [8]byte
		if _, err := f.ReadAt(chunk[:], pos); err != nil {
			f.Close()
			return nil, wavInfo{}, fmt.Errorf("read WAV chunk header: %w", err)
		}
		pos += 8
		id := string(chunk[0:4])
		n := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		if n < 0 || pos+n < pos || pos+n > size {
			f.Close()
			return nil, wavInfo{}, fmt.Errorf("WAV chunk %q is truncated or out of bounds", id)
		}
		switch id {
		case "fmt ":
			if n < 16 {
				f.Close()
				return nil, wavInfo{}, errors.New("short WAV fmt chunk")
			}
			buf := make([]byte, n)
			if _, err := f.ReadAt(buf, pos); err != nil {
				f.Close()
				return nil, wavInfo{}, fmt.Errorf("read WAV fmt: %w", err)
			}
			format := binary.LittleEndian.Uint16(buf[0:2])
			info.channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			info.sampleRate = int(binary.LittleEndian.Uint32(buf[4:8]))
			bits := int(binary.LittleEndian.Uint16(buf[14:16]))
			align := int(binary.LittleEndian.Uint16(buf[12:14]))
			if format != 1 {
				f.Close()
				return nil, wavInfo{}, fmt.Errorf("unsupported WAV format %d (need PCM)", format)
			}
			if info.channels != 1 || info.sampleRate != oww.SampleRate || bits != 16 || align != 2 {
				f.Close()
				return nil, wavInfo{}, fmt.Errorf("unsupported WAV layout: %d Hz, %d ch, %d-bit, align %d (need 16000/1/16/2)",
					info.sampleRate, info.channels, bits, align)
			}
			info.bitsPerSample = bits
			fmtOK = true
		case "data":
			if !fmtOK {
				f.Close()
				return nil, wavInfo{}, errors.New("WAV data chunk precedes fmt chunk")
			}
			if n%2 != 0 {
				f.Close()
				return nil, wavInfo{}, errors.New("WAV data size is not a whole number of int16 samples")
			}
			info.dataOffset = pos
			info.dataSize = n
			return f, info, nil
		}
		pos += n
		if n%2 != 0 {
			pos++ // RIFF word alignment padding
		}
	}
}

// exportStream runs one fresh frontend over the warmed prefix followed by the
// complete source file, writing its 96-dim embeddings as float16 to dst. The
// prefix and body are fed through the same frontend so the first embedding
// windows contain real background instead of the cold zero state. It returns
// the number of embeddings, the number of original body samples (prefix
// excluded) and the WAV layout.
func exportStream(assets *oww.Assets, path string, prefix []int16, chunkSamples int, dst string) (frames int, samples int64, info wavInfo, err error) {
	if len(prefix) != prerollSamples {
		return 0, 0, wavInfo{}, fmt.Errorf("preroll has %d samples, expected %d", len(prefix), prerollSamples)
	}
	f, info, err := openPCM16WAV(path)
	if err != nil {
		return 0, 0, wavInfo{}, err
	}
	defer f.Close()
	front, err := oww.NewFrontend(assets)
	if err != nil {
		return 0, 0, wavInfo{}, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, 0, wavInfo{}, err
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	var half [2]byte
	feed := func(pcm []int16) error {
		for _, emb := range front.Feed(pcm) {
			if len(emb) != embeddingDim {
				return fmt.Errorf("frontend returned %d dims, want %d", len(emb), embeddingDim)
			}
			for _, v := range emb {
				h, herr := float32ToFloat16(v)
				if herr != nil {
					return herr
				}
				binary.LittleEndian.PutUint16(half[:], h)
				if _, werr := out.Write(half[:]); werr != nil {
					return werr
				}
			}
			frames++
		}
		return nil
	}
	for off := 0; off < len(prefix); off += chunkSamples {
		end := off + chunkSamples
		if end > len(prefix) {
			end = len(prefix)
		}
		if err := feed(prefix[off:end]); err != nil {
			return 0, 0, wavInfo{}, err
		}
	}

	buf := make([]byte, chunkSamples*2)
	for off := int64(0); off < info.dataSize; {
		n := int64(len(buf))
		if remain := info.dataSize - off; remain < n {
			n = remain
		}
		if n%2 != 0 {
			return 0, 0, wavInfo{}, errors.New("truncated int16 sample")
		}
		if _, rerr := f.ReadAt(buf[:n], info.dataOffset+off); rerr != nil {
			return 0, 0, wavInfo{}, fmt.Errorf("read PCM at %d: %w", off, rerr)
		}
		pcm := make([]int16, n/2)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
		}
		if err := feed(pcm); err != nil {
			return 0, 0, wavInfo{}, err
		}
		samples += n / 2
		off += n
	}
	if err := out.Sync(); err != nil {
		return 0, 0, wavInfo{}, err
	}
	return frames, samples, info, nil
}

// float32ToFloat16 converts a finite float32 to IEEE 754 binary16 with
// round-to-nearest-even. It returns an error rather than silently writing
// infinity/NaN or overflowing the half-precision range.
func float32ToFloat16(f float32) (uint16, error) {
	b := math.Float32bits(f)
	sign := uint16((b >> 16) & 0x8000)
	exp := int((b >> 23) & 0xff)
	mant := b & 0x7fffff

	if exp == 0xff {
		return 0, errors.New("non-finite embedding value")
	}
	if exp == 0 && mant == 0 {
		return sign, nil // ±0
	}

	// Unbiased exponent for normal floats; subnormals have exp == 0.
	unbiased := exp - 127
	if exp == 0 {
		// Float32 subnormal: value = mant * 2^-149, certainly rounds to signed
		// zero in binary16 (largest is ~1.4e-45).
		return sign, nil
	}

	if unbiased > 15 {
		return 0, fmt.Errorf("embedding value %g overflows float16", f)
	}

	if unbiased >= -14 {
		hExp := uint32(unbiased + 15)
		frac := mant >> 13
		rem := mant & 0x1fff
		if rem > 0x1000 || (rem == 0x1000 && frac&1 == 1) {
			frac++
			if frac == 0x400 {
				frac = 0
				hExp++
				if hExp >= 0x1f {
					return 0, fmt.Errorf("embedding value %g overflows float16", f)
				}
			}
		}
		return sign | uint16(hExp)<<10 | uint16(frac), nil
	}

	// Subnormal binary16 result. The integer fraction is
	// (2^23+mant) * 2^(unbiased+1), rounded to nearest even.
	v := uint64(mant) | 1<<23
	shift := uint(-unbiased - 1)
	if shift >= 25 {
		return sign, nil
	}
	q := v >> shift
	rem := v & ((uint64(1) << shift) - 1)
	half := uint64(1) << (shift - 1)
	if rem > half || (rem == half && q&1 == 1) {
		q++
	}
	if q >= 0x400 {
		return sign | 0x0400, nil // rounded up to the smallest normal
	}
	return sign | uint16(q), nil
}

// writeArtifact writes the container atomically as a deterministic gzip
// stream: no filename, zero mtime, standard library deflate. The compressed
// file and the logical (decompressed) container are both bounded so the corpus
// stays within the runtime loader's limits. It returns the logical byte count.
func writeArtifact(outPath string, headerJSON []byte, tmpDir string, streams []cnegStream, payloadBytes int64) (int64, error) {
	logicalSize := int64(8+4+len(headerJSON)) + payloadBytes
	if payloadBytes < 0 || logicalSize > maxNegativesContainer {
		return 0, fmt.Errorf("logical CNEG0002 size %d exceeds the %d-byte bound", logicalSize, maxNegativesContainer)
	}
	dir := filepath.Dir(outPath)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return 0, fmt.Errorf("output directory %q: not a directory", dir)
	}
	tmp, err := os.CreateTemp(dir, ".cneg-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("create output: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(tmpName)
		}
	}()

	zw, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		return 0, err
	}
	// Deterministic header: zero mtime and no original filename. OS=255 keeps
	// the byte stable across platforms.
	zw.Header = gzip.Header{OS: 255}
	fail := func(err error) (int64, error) {
		zw.Close()
		return 0, err
	}

	if _, err := zw.Write([]byte(formatMagic)); err != nil {
		return fail(err)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(headerJSON)))
	if _, err := zw.Write(lenBuf[:]); err != nil {
		return fail(err)
	}
	if _, err := zw.Write(headerJSON); err != nil {
		return fail(err)
	}
	var rawPayload int64
	for i, st := range streams {
		p := filepath.Join(tmpDir, fmt.Sprintf("stream-%04d.f16", i))
		f, err := os.Open(p)
		if err != nil {
			return fail(fmt.Errorf("open stream payload: %w", err))
		}
		n, copyErr := io.Copy(zw, f)
		f.Close()
		if copyErr != nil {
			return fail(fmt.Errorf("copy stream payload: %w", copyErr))
		}
		want := int64(st.Frames) * embeddingDim * 2
		if n != want {
			return fail(fmt.Errorf("stream %q payload is %d bytes, header implies %d", st.ID, n, want))
		}
		rawPayload += n
	}
	if rawPayload != payloadBytes {
		return fail(fmt.Errorf("internal size mismatch: payload is %d bytes, header implies %d", rawPayload, payloadBytes))
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	written, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if written > maxNegativesContainer {
		return 0, fmt.Errorf("compressed CNEG0002 is %d bytes, exceeding the %d-byte bound", written, maxNegativesContainer)
	}
	if err := tmp.Close(); err != nil {
		ok = true
		os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("rename output: %w", err)
	}
	ok = true
	return logicalSize, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func fileSHA256(path string) (string, error) {
	sum, _, err := hashFile(path)
	return sum, err
}

// writeJSONArtifact writes the provenance document atomically. Paths ending in
// .gz are written as a deterministic gzip member (mtime 0, no original
// filename, standard-library deflate) so the full licensing snapshot fits on
// the device; any other path stays plain JSON.
func writeJSONArtifact(path string, v any) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cneg-prov-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	var w io.Writer = tmp
	var zw *gzip.Writer
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		zw, err = gzip.NewWriterLevel(tmp, gzip.BestCompression)
		if err != nil {
			return err
		}
		zw.Header = gzip.Header{OS: 255}
		w = zw
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		if zw != nil {
			zw.Close()
		}
		return err
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	ok = true
	return nil
}
