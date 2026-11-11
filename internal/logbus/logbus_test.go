package logbus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/ids"
)

func TestLogFileRotatesAndKeepsOneBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capsulad.log")
	const limit = 400
	b, err := newBus(ids.NewClock(), 64, path, limit)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		b.Infof("test", "", "record %d with enough padding to rotate", i)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if int64(len(current)) > limit || int64(len(previous)) > limit {
		t.Fatalf("log files exceed cap: current=%d previous=%d", len(current), len(previous))
	}
	if !strings.Contains(string(current), "record 7") {
		t.Fatalf("newest record not in current file:\n%s", current)
	}
	if strings.Contains(string(current), "record 0") {
		t.Fatalf("oldest record still in current file:\n%s", current)
	}
	if !strings.Contains(string(previous), "record") {
		t.Fatalf("backup file has no records:\n%s", previous)
	}
}

func TestLogFileAcceptsOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capsulad.log")
	b, err := newBus(ids.NewClock(), 8, path, 16)
	if err != nil {
		t.Fatal(err)
	}
	b.Infof("test", "", "one record that is longer than the whole rotation cap")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "longer than the whole rotation cap") {
		t.Fatalf("oversized record dropped: %q", data)
	}
}
