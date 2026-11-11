// Command waketrain trains a wake-word classifier offline from captured audio.
//
// It reads word takes (*_word.wav) and background takes (*_background.wav) from
// -samples, optional extra room/speech/hard-negative directories, and the
// licensed CNEG0002 negative-feature corpus. The result is a version-3
// classifier artifact written only when the training quality gate passes.
//
// The recordings are 16 kHz mono signed 16-bit PCM WAV, the same files the
// device capture writes. Audio never leaves the machine.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"capsulad/internal/wakeword"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "waketrain:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		samplesDir = flag.String("samples", "", "directory with *_word.wav and *_background.wav recordings (required)")
		ambientDir = flag.String("ambient", "", "extra directory of room/background *.wav recordings")
		speechDir  = flag.String("speech", "", "directory of unrelated-speech *.wav recordings")
		falseDir   = flag.String("false", "", "directory of hard-negative *.wav recordings (no wake word)")
		negPath    = flag.String("negatives", "", "CNEG0002 licensed warmed negative-feature corpus (required)")
		outPath    = flag.String("out", "", "output classifier JSON path (required)")
		seed       = flag.Uint64("seed", 1, "deterministic training seed")
		name       = flag.String("name", "", "model id, e.g. hey_capsula (required)")
		display    = flag.String("display", "", "human-readable model name (defaults to -name)")
		language   = flag.String("language", "en", "model language tag")
	)
	flag.Parse()

	if *samplesDir == "" || *negPath == "" || *outPath == "" || *name == "" {
		flag.Usage()
		return errors.New("-samples, -negatives, -out and -name are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	en, err := loadEnrollment(*samplesDir, *ambientDir, *speechDir, *falseDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "waketrain: %d word, %d room, %d speech, %d false-activation recordings\n",
		len(en.Takes), len(en.Ambient), len(en.Speech), len(en.False))

	last := make(map[string]int)
	clf, err := wakeword.Train(ctx, en, wakeword.TrainOptions{
		ID:            *name,
		Display:       *display,
		Language:      *language,
		Seed:          *seed,
		NegativesPath: *negPath,
		Progress: func(stage string, fraction float64) {
			bucket := int(fraction*100 + 0.5)
			if bucket > 100 {
				bucket = 100
			}
			if bucket < 0 {
				bucket = 0
			}
			if prev, ok := last[stage]; ok && prev == bucket && bucket != 100 {
				return
			}
			last[stage] = bucket
			fmt.Fprintf(os.Stderr, "  %-10s %3d%%\n", stage, bucket)
		},
	})
	if err != nil {
		return err
	}

	raw, err := json.MarshalIndent(clf, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding classifier: %w", err)
	}
	raw = append(raw, '\n')
	tmp := *outPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, *outPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("installing %s: %w", *outPath, err)
	}
	fmt.Printf("wrote %s: model=%s threshold=%.3f\n", *outPath, clf.ID, clf.Threshold)
	if metrics, ok := clf.Trained["metrics"].(map[string]any); ok {
		for _, part := range []string{"tune", "final_test"} {
			if m, ok := metrics[part].(map[string]any); ok {
				fmt.Printf("%s: detected %v/%v (%s), false activations %v\n", part,
					m["detected"], m["word_recordings"],
					percentText(m["recall"]), m["false_activations"])
			}
		}
	}
	return nil
}

func percentText(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%.0f%%", f*100)
	}
	return "?"
}

// loadEnrollment reads the takes the capture flow writes. Background takes are
// room recordings; -ambient, -speech and -false add recordings of those kinds
// explicitly instead of guessing from a filename inside an arbitrary directory.
func loadEnrollment(samplesDir, ambientDir, speechDir, falseDir string) (wakeword.Enrollment, error) {
	var en wakeword.Enrollment
	entries, err := listWAVs(samplesDir)
	if err != nil {
		return en, err
	}
	for _, path := range entries {
		switch {
		case strings.HasSuffix(path, "_word.wav"):
			pcm, err := readWAV(path)
			if err != nil {
				return en, err
			}
			en.Takes = append(en.Takes, pcm)
		case strings.HasSuffix(path, "_background.wav"):
			pcm, err := readWAV(path)
			if err != nil {
				return en, err
			}
			en.Ambient = append(en.Ambient, pcm)
		}
	}
	if len(en.Takes) == 0 {
		return en, fmt.Errorf("%s: no *_word.wav recordings found", samplesDir)
	}

	if ambientDir != "" {
		paths, err := listWAVs(ambientDir)
		if err != nil {
			return en, err
		}
		if len(paths) == 0 {
			return en, fmt.Errorf("%s: no *.wav recordings found", ambientDir)
		}
		for _, path := range paths {
			pcm, err := readWAV(path)
			if err != nil {
				return en, err
			}
			en.Ambient = append(en.Ambient, pcm)
		}
	}
	if speechDir != "" {
		paths, err := listWAVs(speechDir)
		if err != nil {
			return en, err
		}
		if len(paths) == 0 {
			return en, fmt.Errorf("%s: no *.wav recordings found", speechDir)
		}
		for _, path := range paths {
			pcm, err := readWAV(path)
			if err != nil {
				return en, err
			}
			en.Speech = append(en.Speech, pcm)
		}
	}
	if falseDir != "" {
		paths, err := listWAVs(falseDir)
		if err != nil {
			return en, err
		}
		if len(paths) == 0 {
			return en, fmt.Errorf("%s: no *.wav recordings found", falseDir)
		}
		for _, path := range paths {
			pcm, err := readWAV(path)
			if err != nil {
				return en, err
			}
			en.False = append(en.False, pcm)
		}
	}
	return en, nil
}

func listWAVs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".wav") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// readWAV parses the canonical 16 kHz mono 16-bit PCM WAV the device writes.
// Other formats are rejected instead of resampled: training features must be
// computed from exactly the audio the device captures.
const maxWAVDataBytes = 64 << 20

func readWAV(path string) ([]int16, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()
	r := bufio.NewReader(f)

	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("%s: reading RIFF header: %w", path, err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%s: not a RIFF/WAVE file", path)
	}

	var (
		haveFMT bool
		format  uint16
		chans   uint16
		rate    uint32
		bits    uint16
		pcm     []int16
	)
	for {
		var chunk [8]byte
		if _, err := io.ReadFull(r, chunk[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("%s: reading chunk header: %w", path, err)
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		switch string(chunk[0:4]) {
		case "fmt ":
			if size < 16 || size > 4096 {
				return nil, fmt.Errorf("%s: fmt chunk is %d bytes", path, size)
			}
			raw := make([]byte, size)
			if _, err := io.ReadFull(r, raw); err != nil {
				return nil, fmt.Errorf("%s: reading fmt chunk: %w", path, err)
			}
			format = binary.LittleEndian.Uint16(raw[0:])
			chans = binary.LittleEndian.Uint16(raw[2:])
			rate = binary.LittleEndian.Uint32(raw[4:])
			bits = binary.LittleEndian.Uint16(raw[14:])
			haveFMT = true
		case "data":
			if !haveFMT {
				return nil, fmt.Errorf("%s: data chunk before fmt chunk", path)
			}
			if format != 1 {
				return nil, fmt.Errorf("%s: WAV format %d is not PCM 16-bit mono", path, format)
			}
			if chans != 1 || rate != 16000 || bits != 16 {
				return nil, fmt.Errorf("%s: %d Hz, %d channel(s), %d-bit; want 16000 Hz mono 16-bit PCM", path, rate, chans, bits)
			}
			if size%2 != 0 {
				return nil, fmt.Errorf("%s: data chunk has odd size %d", path, size)
			}
			if size > maxWAVDataBytes {
				return nil, fmt.Errorf("%s: data chunk is %d bytes, beyond the %d byte bound", path, size, int64(maxWAVDataBytes))
			}
			pcm = make([]int16, size/2)
			buf := make([]byte, 4096)
			var off int64
			for off < size {
				n := int64(len(buf))
				if n > size-off {
					n = size - off
				}
				if _, err := io.ReadFull(r, buf[:n]); err != nil {
					return nil, fmt.Errorf("%s: truncated audio data at %d of %d bytes: %w", path, off, size, err)
				}
				for i := int64(0); i+1 < n; i += 2 {
					pcm[(off+i)/2] = int16(binary.LittleEndian.Uint16(buf[i:]))
				}
				off += n
			}
		default:
			if _, err := io.CopyN(io.Discard, r, size); err != nil {
				return nil, fmt.Errorf("%s: skipping %q chunk: %w", path, chunk[0:4], err)
			}
		}
		if size%2 == 1 {
			if _, err := r.Discard(1); err != nil {
				return nil, fmt.Errorf("%s: reading chunk padding: %w", path, err)
			}
		}
	}
	if !haveFMT {
		return nil, fmt.Errorf("%s: missing fmt chunk", path)
	}
	if len(pcm) == 0 {
		return nil, fmt.Errorf("%s: no audio data", path)
	}
	return pcm, nil
}
