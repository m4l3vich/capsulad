package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/sdk/capsulad"
)

// These tests prove the app's half of the capture contract the daemon enforces:
// a presentation, then a rendered non-blank first frame, then the routing
// capture that selects it — and the capture released before the presentation.
// The daemon's own qualification (selection + freshness) is covered by
// internal/daemon/indication_test.go.

type fakePres struct {
	owner    *fakeRingClient
	frames   int
	nonBlank bool
	closed   bool
}

func (p *fakePres) ID() string { return "pres-1" }

func (p *fakePres) Ring(pixels []capsulad.RGB) error {
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()
	p.frames++
	for _, px := range pixels {
		if px.R != 0 || px.G != 0 || px.B != 0 {
			p.nonBlank = true
			break
		}
	}
	if p.frames == 1 {
		p.owner.order = append(p.owner.order, "frame")
	}
	return nil
}

func (p *fakePres) Close() error {
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()
	p.closed = true
	p.owner.order = append(p.owner.order, "presentation.close")
	return nil
}

type fakeCap struct {
	owner  *fakeRingClient
	events chan capsulad.CaptureEvent
	closed bool
}

func (c *fakeCap) Events() <-chan capsulad.CaptureEvent { return c.events }
func (c *fakeCap) Close() {
	c.owner.mu.Lock()
	defer c.owner.mu.Unlock()
	c.closed = true
	c.owner.order = append(c.owner.order, "capture.close")
}

type fakeRingClient struct {
	mu         sync.Mutex
	order      []string
	pres       *fakePres
	cap        *fakeCap
	captureErr error
	capPresID  string
	capMode    capsulad.CaptureMode
}

func newFakeRing() (*fakeRingClient, *fakePres, *fakeCap) {
	f := &fakeRingClient{}
	f.pres = &fakePres{owner: f}
	f.cap = &fakeCap{owner: f, events: make(chan capsulad.CaptureEvent, 4)}
	return f, f.pres, f.cap
}

func (f *fakeRingClient) OpenPresentation(ctx context.Context, resourceID string) (ringPresentation, error) {
	f.mu.Lock()
	f.order = append(f.order, "presentation")
	f.mu.Unlock()
	return f.pres, nil
}

func (f *fakeRingClient) OpenCapture(ctx context.Context, resourceID, presentationID string,
	mode capsulad.CaptureMode) (ringCapture, error) {
	f.mu.Lock()
	f.order = append(f.order, "capture")
	f.capPresID, f.capMode = presentationID, mode
	err := f.captureErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.cap, nil
}

func (f *fakeRingClient) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.order))
	copy(out, f.order)
	return out
}

func (f *fakeRingClient) frameCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pres.frames
}

func (f *fakeRingClient) firstFrameNonBlank() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pres.nonBlank
}

func (f *fakeRingClient) presentationClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pres.closed
}

type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) Contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), s)
}

func TestRingAcquiresCaptureAfterFirstFrame(t *testing.T) {
	client, _, _ := newFakeRing()
	var logs syncLog
	r := newRingIndicator(client, log.New(&logs, "", 0))

	if err := r.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Give the render goroutine a moment to emit at least one frame.
	waitFor(t, "first frame", func() bool { return client.frameCount() > 0 })
	got := client.sequence()
	want := []string{"presentation", "frame", "capture"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v; want %v", got, want)
	}
	if client.capPresID != "pres-1" || client.capMode != capsulad.CaptureModeUnspecified {
		t.Fatalf("capture presentation=%q mode=%v", client.capPresID, client.capMode)
	}
	if !client.firstFrameNonBlank() {
		t.Fatal("the first frame lights nothing; it is not a valid indication")
	}

	r.Close()
	got = client.sequence()
	if len(got) < 2 || got[len(got)-2] != "capture.close" || got[len(got)-1] != "presentation.close" {
		t.Fatalf("close order = %v; the capture must be released before the presentation", got)
	}
}

func TestRingOpenFailsWhenCaptureIsRefused(t *testing.T) {
	client, _, _ := newFakeRing()
	client.captureErr = context.DeadlineExceeded
	r := newRingIndicator(client, log.New(&syncLog{}, "", 0))

	if err := r.Open(context.Background()); err == nil {
		t.Fatal("Open succeeded without a capture")
	}
	if !client.presentationClosed() {
		t.Fatal("a refused capture left the presentation open")
	}
}

func TestRingReportsLostIndication(t *testing.T) {
	client, _, cap := newFakeRing()
	var logs syncLog
	r := newRingIndicator(client, log.New(&logs, "", 0))
	if err := r.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	cap.events <- capsulad.CaptureEvent{Selected: false, Reason: "muted"}
	waitFor(t, "lost indication log", func() bool { return logs.Contains("indication_lost") })
}

// waitFor is local to this package's tests.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
