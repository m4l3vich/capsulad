package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"capsulad/internal/backend"
)

func TestParseHardwareSerial(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"real line", "processor\t: 0\nHardware\t: Rockchip\nSerial\t\t: 11bfcbc5d63d55fc\n", "11bfcbc5d63d55fc"},
		{"spaces", "Serial : 11bfcbc5d63d55fc\n", "11bfcbc5d63d55fc"},
		{"serial number", "Serial Number\t: ABC-123\n", "ABC-123"},
		{"missing field", "processor\t: 0\nHardware\t: Rockchip\n", ""},
		{"empty value", "Serial\t\t:\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := parseHardwareSerial(path, hardwareSerialFields)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Fatalf("serial = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadHardwareSerialUnreadable(t *testing.T) {
	previous := hardwareSerialPath
	hardwareSerialPath = filepath.Join(t.TempDir(), "absent-cpuinfo")
	t.Cleanup(func() { hardwareSerialPath = previous })

	// An unreadable host file means "the host cannot report it", never an
	// error for callers.
	if got := readHardwareSerial(); got != "" {
		t.Fatalf("serial = %q, want empty", got)
	}
}

func TestGetInfoReportsCachedHardwareSerial(t *testing.T) {
	previous := hardwareSerialPath
	hardwareSerialPath = filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(hardwareSerialPath, []byte("Serial\t\t: 11bfcbc5d63d55fc\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { hardwareSerialPath = previous })

	d := &Daemon{
		dev:            backend.NewSimulator(backend.SimulatorOptions{}),
		bootID:         "boot-test",
		hardwareSerial: readHardwareSerial(),
	}
	info, err := (&deviceService{d: d}).GetInfo(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetHardwareSerial() != "11bfcbc5d63d55fc" {
		t.Fatalf("hardware_serial = %q", info.GetHardwareSerial())
	}

	// The value is cached: the daemon never re-reads the file per call.
	if err := os.Remove(hardwareSerialPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	again, err := (&deviceService{d: d}).GetInfo(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if again.GetHardwareSerial() != "11bfcbc5d63d55fc" {
		t.Fatalf("hardware_serial after removal = %q", again.GetHardwareSerial())
	}
}
