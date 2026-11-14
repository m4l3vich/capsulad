package audio

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// A supervised application may not receive microphone audio without both a
// visible presentation and an audible framework cue. Every denial happens
// before a capture exists, so no audio is ever published.
func TestApplicationCaptureRequiresVisibleAndAudibleIndication(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	dev := &fakeDevice{}
	newManager := func(opts Options) *Manager {
		return NewManager(context.Background(), clock, logs, dev, opts)
	}

	m := newManager(Options{})
	defer m.Close()
	if _, err := m.StartCapture("app", "run", CaptureEndpoint, backend.AudioFormat{}, 0, "", ""); err == nil || !strings.Contains(err.Error(), "visible and audible") {
		t.Fatalf("no indication policy error = %v", err)
	}

	invisible := newManager(Options{
		IndicationActive: func(string) bool { return false },
		IndicationCue:    func(string) error { return nil },
	})
	defer invisible.Close()
	if _, err := invisible.StartCapture("app", "run", CaptureEndpoint, backend.AudioFormat{}, 0, "", ""); err == nil || !strings.Contains(err.Error(), "visible") {
		t.Fatalf("invisible indication error = %v", err)
	}

	silent := newManager(Options{
		IndicationActive: func(string) bool { return true },
		IndicationCue:    func(string) error { return errCueUnavailable },
	})
	defer silent.Close()
	if _, err := silent.StartCapture("app", "run", RawCaptureEndpoint, backend.AudioFormat{}, 0, "", ""); err == nil || !strings.Contains(err.Error(), "not audible") {
		t.Fatalf("inaudible indication error = %v", err)
	}

	cued := 0
	ok := newManager(Options{
		IndicationActive: func(string) bool { return true },
		IndicationCue:    func(string) error { cued++; return nil },
	})
	defer ok.Close()
	capture, err := ok.StartCapture("app", "run", RawCaptureEndpoint, backend.AudioFormat{}, 0, "", "")
	if err != nil {
		t.Fatalf("indicated raw capture rejected: %v", err)
	}
	if cued != 1 {
		t.Fatalf("audible cue calls = %d, want 1", cued)
	}
	if !ok.StopCapture(capture.ID()) {
		t.Fatal("raw capture did not stop")
	}
}

type cueError string

func (e cueError) Error() string { return string(e) }

const errCueUnavailable = cueError("no cue")

// A started capture stops delivering as soon as the visible indication or the
// master volume is lost, not only when the termination watchdog later fires.
func TestStartedCaptureWithholdsWhenIndicationOrVolumeDrops(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	source := &gatedSource{sizes: make(chan int), entered: make(chan struct{}, 1)}
	var visible atomic.Bool
	visible.Store(true)
	m := NewManager(context.Background(), clock, logs, &gatedDevice{source: source}, Options{
		IndicationActive: func(string) bool { return visible.Load() },
		IndicationCue:    func(string) error { return nil },
		WatchdogPollMS:   50,
		WatchdogGraceMS:  500,
	})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	cap, err := m.StartCapture("app", "run", CaptureEndpoint, backend.AudioFormat{}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ev := <-cap.Events(); ev.Started == nil {
		t.Fatalf("first event = %+v, want Started", ev)
	}

	visible.Store(false)
	advanceCapture(t, source, 640)
	select {
	case ev := <-cap.Events():
		t.Fatalf("withheld chunk delivered while the indication was lost: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	visible.Store(true)
	advanceCapture(t, source, 640)
	select {
	case ev := <-cap.Events():
		if ev.Chunk == nil || (!ev.Chunk.Discontinuity && ev.Chunk.DroppedFrames == 0) {
			t.Fatalf("resumed chunk did not report the withheld audio: %+v", ev.Chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not resume after the indication returned")
	}

	m.SetMasterVolume(0)
	advanceCapture(t, source, 640)
	select {
	case ev := <-cap.Events():
		t.Fatalf("chunk delivered at zero master volume: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	m.SetMasterVolume(0.6)
	advanceCapture(t, source, 640)
	select {
	case ev := <-cap.Events():
		if ev.Chunk == nil {
			t.Fatalf("resumed event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not resume after volume returned")
	}
	if !m.StopCapture(cap.ID()) {
		t.Fatal("capture did not stop")
	}
}
