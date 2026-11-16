package audio

import (
	"context"
	"testing"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// TestOutputFormatComesFromOptions proves the mixer's production format is a
// configuration value, not a package constant, and that a zero value keeps the
// backend format for callers that do not select a profile.
func TestOutputFormatComesFromOptions(t *testing.T) {
	logs, err := logbus.New(ids.NewClock(), 32, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	dev := &fakeDevice{}
	fallback := NewManager(context.Background(), ids.NewClock(), logs, dev, Options{})
	if got := fallback.PlaybackFormat(); got != dev.PlaybackFormat() {
		t.Fatalf("zero-option format = %+v, want the backend format %+v", got, dev.PlaybackFormat())
	}
	want := backend.AudioFormat{Format: backend.S16LE, RateHz: 44100, Channels: 1}
	override := NewManager(context.Background(), ids.NewClock(), logs, dev, Options{OutputFormat: want})
	if got := override.PlaybackFormat(); got != want {
		t.Fatalf("configured format = %+v, want %+v", got, want)
	}
}
