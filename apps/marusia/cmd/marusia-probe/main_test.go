package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"capsulad/apps/marusia/internal/vkclient"
)

// syntheticWAV writes a 16 kHz mono 16-bit PCM WAV whose data chunk holds
// `samples` samples; no user recording is committed or required.
func syntheticWAV(t *testing.T, dir string, rate, channels, bits uint16, samples int) string {
	t.Helper()
	payload := make([]byte, samples*int(channels)*int(bits)/8)
	if bits == 16 {
		for i := 0; i+1 < len(payload); i += 2 {
			binary.LittleEndian.PutUint16(payload[i:], uint16(3000))
		}
	}
	blockAlign := channels * bits / 8
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(payload)))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], channels)
	binary.LittleEndian.PutUint32(header[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(rate)*uint32(blockAlign))
	binary.LittleEndian.PutUint16(header[32:34], blockAlign)
	binary.LittleEndian.PutUint16(header[34:36], bits)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(payload)))
	path := filepath.Join(dir, "prompt.wav")
	if err := os.WriteFile(path, append(header, payload...), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	return path
}

func TestReadWAVReturnsThePCMPayload(t *testing.T) {
	path := syntheticWAV(t, t.TempDir(), 16000, 1, 16, 1280)
	pcm, err := readWAV(path)
	if err != nil {
		t.Fatalf("readWAV: %v", err)
	}
	if len(pcm) != 2560 {
		t.Fatalf("pcm = %d bytes, want 2560", len(pcm))
	}
	if len(pcm)%vkclient.FrameBytes != 0 {
		t.Fatalf("pcm does not split into whole %d-byte frames", vkclient.FrameBytes)
	}
}

// The activation type the operator passes must reach the init the service sees:
// the vendor's own client always names one (listen_command, keyword, button,
// flowmode), and the live probe needs to reproduce that to be evidence about
// the service's answer.
func TestActivationTypeKnobReachesTheInit(t *testing.T) {
	capabilities := vkclient.DefaultCapabilitiesJSON

	t.Setenv("MARUSIA_PROBE_ACTIVATION_TYPE", "")
	if got := speechOptions(capabilities, nil).ActivationType; got != "" {
		t.Fatalf("default activation type = %q, want the app's empty default", got)
	}

	t.Setenv("MARUSIA_PROBE_ACTIVATION_TYPE", "listen_command")
	opts := speechOptions(capabilities, nil)
	if opts.ActivationType != "listen_command" {
		t.Fatalf("activation type = %q, want listen_command", opts.ActivationType)
	}
	init := vkclient.BuildInit(opts, "sn", "44.91")
	if got := init.GetClientOptions().GetActivationType(); got != "listen_command" {
		t.Fatalf("init client_options.activation_type = %q, want listen_command", got)
	}
}

// The probe must refuse audio the service would reject instead of uploading it.
func TestReadWAVRejectsOtherFormats(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rate     uint16
		channels uint16
		bits     uint16
	}{
		{"stereo", 16000, 2, 16},
		{"8 kHz", 8000, 1, 16},
		{"8-bit", 16000, 1, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := syntheticWAV(t, t.TempDir(), tc.rate, tc.channels, tc.bits, 160)
			if _, err := readWAV(path); err == nil {
				t.Fatal("readWAV accepted a format the service rejects")
			}
		})
	}
}

func TestWriteCapturedCreates0600AndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	if err := writeCaptured(path, []byte("first")); err != nil {
		t.Fatalf("writeCaptured new file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("capture mode = %04o, want 0600", got)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first" {
		t.Fatalf("capture content = %q, %v", data, err)
	}

	existing := filepath.Join(dir, "existing.json")
	if err := os.WriteFile(existing, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeCaptured(existing, []byte("new")); err == nil {
		t.Fatal("writeCaptured overwrote an existing file")
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "old" {
		t.Fatalf("existing file changed: %q, %v", data, err)
	}
}
