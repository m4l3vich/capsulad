package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"capsulad/internal/audio"
)

func buildWAV(channels uint16, rate uint32, bits uint16, pcm []int16) []byte {
	var buf bytes.Buffer
	dataSize := len(pcm) * 2
	write := func(v any) {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}
	buf.WriteString("RIFF")
	write(uint32(36 + dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	write(uint32(16))
	write(uint16(1))
	write(channels)
	write(rate)
	write(rate * uint32(bits) * uint32(channels) / 8)
	write(bits / 8 * channels)
	write(bits)
	buf.WriteString("data")
	write(uint32(dataSize))
	for _, v := range pcm {
		write(v)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestReadWAVRoundTrip(t *testing.T) {
	pcm := []int16{0, 1, -1, 32767, -32768, 1234}
	dir := t.TempDir()
	path := filepath.Join(dir, "take_word.wav")
	writeFile(t, path, buildWAV(1, 16000, 16, pcm))

	got, err := readWAV(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(pcm) {
		t.Fatalf("samples = %d, want %d", len(got), len(pcm))
	}
	for i := range pcm {
		if got[i] != pcm[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], pcm[i])
		}
	}
}

func TestReadWAVRejectsUnsupportedFormats(t *testing.T) {
	pcm := []int16{0, 1, 2, 3}
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "stereo", raw: buildWAV(2, 16000, 16, pcm)},
		{name: "wrong rate", raw: buildWAV(1, 48000, 16, pcm)},
		{name: "eight bit", raw: buildWAV(1, 16000, 8, pcm)},
		{name: "not riff", raw: []byte("NOPE....WAVEdata")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.wav")
			writeFile(t, path, c.raw)
			if _, err := readWAV(path); err == nil {
				t.Fatal("unsupported WAV was accepted")
			}
		})
	}
	if _, err := readWAV(filepath.Join(t.TempDir(), "missing.wav")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestReadWAVTruncatedData(t *testing.T) {
	raw := buildWAV(1, 16000, 16, []int16{1, 2, 3, 4})
	raw = raw[:len(raw)-4] // shrink file, keep declared data size
	path := filepath.Join(t.TempDir(), "truncated.wav")
	writeFile(t, path, raw)
	if _, err := readWAV(path); err == nil {
		t.Fatal("truncated WAV was accepted")
	}
}

func TestLoadEnrollment(t *testing.T) {
	root := t.TempDir()
	samples := filepath.Join(root, "samples")
	ambient := filepath.Join(root, "ambient")
	speech := filepath.Join(root, "speech")
	falseDir := filepath.Join(root, "false")
	for _, dir := range []string{samples, ambient, speech, falseDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	wav := buildWAV(1, 16000, 16, []int16{0, 100, -100})
	writeFile(t, filepath.Join(samples, "wk_1_word.wav"), wav)
	writeFile(t, filepath.Join(samples, "wk_2_background.wav"), wav)
	writeFile(t, filepath.Join(samples, "notes.txt"), []byte("ignored"))
	writeFile(t, filepath.Join(ambient, "room1.wav"), wav)
	writeFile(t, filepath.Join(speech, "phrase1.wav"), wav)
	writeFile(t, filepath.Join(speech, "phrase2.wav"), wav)
	writeFile(t, filepath.Join(falseDir, "other.wav"), wav)

	en, inputs, err := loadEnrollment(samples, ambient, speech, falseDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(en.Takes) != 1 || len(en.Ambient) != 2 || len(en.Speech) != 2 || len(en.False) != 1 {
		t.Fatalf("counts = %d/%d/%d/%d, want 1/2/2/1", len(en.Takes), len(en.Ambient), len(en.Speech), len(en.False))
	}
	if inputs.Tap != audio.CaptureProcessingTap || inputs.RawConverted != 6 {
		t.Fatalf("inputs = %+v, want tap %q with 6 converted legacy raw takes", inputs, audio.CaptureProcessingTap)
	}
}

func TestLoadEnrollmentRejectsEmpty(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, err := loadEnrollment(empty, "", "", ""); err == nil {
		t.Fatal("directory without word takes accepted")
	}
	if _, _, err := loadEnrollment(filepath.Join(root, "missing"), "", "", ""); err == nil {
		t.Fatal("missing directory accepted")
	}

	samples := filepath.Join(root, "samples")
	if err := os.MkdirAll(samples, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(samples, "wk_1_word.wav"), buildWAV(1, 16000, 16, []int16{1, 2}))
	if _, _, err := loadEnrollment(samples, empty, "", ""); err == nil {
		t.Fatal("empty explicit ambient directory accepted")
	}
}
