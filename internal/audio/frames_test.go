package audio

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// fakeDevice feeds a fixed stream and counts reads, so the frame tap can be
// tested without hardware.
type fakeDevice struct {
	chunks atomic.Int64
	pace   time.Duration
}

func (d *fakeDevice) PlaybackFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
}
func (d *fakeDevice) CaptureFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 16000, Channels: 1}
}
func (d *fakeDevice) OpenOutput() (backend.Sink, error)  { return &fakeSink{}, nil }
func (d *fakeDevice) OpenInput() (backend.Source, error) { return &fakeSource{dev: d}, nil }
func (d *fakeDevice) SetMasterVolume(float64) error      { return nil }
func (d *fakeDevice) SetHardwareMute(bool) (bool, error) { return false, nil }
func (d *fakeDevice) Close() error                       { return nil }

type fakeSink struct{}

func (s *fakeSink) WritePCM([]byte) error { return nil }
func (s *fakeSink) Close() error          { return nil }

type fakeSource struct{ dev *fakeDevice }

func (s *fakeSource) ReadPCM(p []byte) (int, error) {
	// 20 ms of 16 kHz mono s16le.
	n := 320 * 2
	if n > len(p) {
		n = len(p)
	}
	for i := 0; i < n; i++ {
		p[i] = 0
	}
	s.dev.chunks.Add(1)
	if s.dev.pace > 0 {
		time.Sleep(s.dev.pace)
	}
	return n, nil
}
func (s *fakeSource) Close() error { return nil }

func newAudioManager(t *testing.T, dev backend.AudioDevice) *Manager {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), clock, logs, dev, Options{})
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return m
}

func TestSubscribeFramesDeliversCaptureStream(t *testing.T) {
	dev := &fakeDevice{pace: 2 * time.Millisecond}
	m := newAudioManager(t, dev)
	defer m.Close()

	frames, cancel := m.SubscribeFrames(4)
	defer cancel()

	select {
	case f := <-frames:
		if f.RateHz != 16000 {
			t.Fatalf("frame rate = %d, want the 16 kHz capture stream", f.RateHz)
		}
		if len(f.PCM) == 0 {
			t.Fatal("empty frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame delivered to the framework consumer")
	}
}

func TestFramesAreDroppedRatherThanBlockingAudio(t *testing.T) {
	dev := &fakeDevice{}
	m := newAudioManager(t, dev)
	defer m.Close()

	// Subscribe with a one-slot queue and never read it: the capture loop must
	// keep consuming from the device regardless.
	_, cancel := m.SubscribeFrames(1)
	defer cancel()
	time.Sleep(120 * time.Millisecond)
	before := dev.chunks.Load()
	if before < 10 {
		t.Fatalf("capture loop stalled with an idle consumer (%d chunks)", before)
	}
}

func TestSubscribeFramesCancelClosesChannel(t *testing.T) {
	dev := &fakeDevice{pace: time.Millisecond}
	m := newAudioManager(t, dev)
	defer m.Close()
	frames, cancel := m.SubscribeFrames(2)
	cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("frame channel was not closed on cancel")
		}
	}
}

// gatedSource lets the test advance capture by exact chunks, without timing
// assumptions about the consumer queue.
type gatedSource struct {
	sizes   chan int
	entered chan struct{}
}

func (s *gatedSource) ReadPCM(p []byte) (int, error) {
	s.entered <- struct{}{}
	n, ok := <-s.sizes
	if !ok || n < 0 {
		return 0, io.EOF
	}
	clear(p[:n])
	return n, nil
}

func (s *gatedSource) Close() error {
	close(s.sizes)
	return nil
}

type gatedDevice struct {
	fakeDevice
	source *gatedSource
}

func (d *gatedDevice) OpenInput() (backend.Source, error) { return d.source, nil }

func TestFrameContinuityAfterDroppedChunksAndSourceErrors(t *testing.T) {
	source := &gatedSource{sizes: make(chan int), entered: make(chan struct{}, 1)}
	m := newAudioManager(t, &gatedDevice{source: source})
	defer m.Close()
	frames, cancel := m.SubscribeFrames(1)
	defer cancel()
	advance := func(n int) {
		t.Helper()
		select {
		case <-source.entered:
		case <-time.After(time.Second):
			t.Fatal("capture did not request another chunk")
		}
		source.sizes <- n
		// The next read starts only after this chunk has been dispatched.
		select {
		case <-source.entered:
			source.entered <- struct{}{}
		case <-time.After(time.Second):
			t.Fatal("capture did not dispatch the chunk")
		}
	}
	advance(320)
	advance(640) // dropped: the one-slot subscriber still contains chunk 1
	first := <-frames
	advance(160)
	next := <-frames
	if first.Sequence != 1 || next.Sequence != 3 {
		t.Fatalf("dropped audio is not visible: sequences %d, %d", first.Sequence, next.Sequence)
	}
	m.mu.Lock()
	last := m.preroll[len(m.preroll)-1]
	m.mu.Unlock()
	if last.FirstFrame != 480 {
		t.Fatalf("partial reads corrupted frame position: got %d, want 480", last.FirstFrame)
	}
	advance(-1)
	advance(640)
	resumed := <-frames
	if !resumed.Discontinuity || resumed.Sequence != 4 {
		t.Fatalf("source error was hidden: sequence=%d discontinuity=%v", resumed.Sequence, resumed.Discontinuity)
	}
}
