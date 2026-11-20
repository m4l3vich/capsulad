package hw

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/sdk/capsulad"
)

// The ring is consumer-visible only while the app holds both halves of the
// framework contract: a presentation with content, and the routing capture
// that selects it. These tests prove the app's half — the same obligation
// apps/cadence pins for its microphone indication — plus the release order
// that restores the device's selected baseline.

type fakeRingPresentation struct {
	owner  *fakeRinger
	id     string
	pixels []capsulad.RGB
	closed bool
}

func (p *fakeRingPresentation) ID() string { return p.id }

func (p *fakeRingPresentation) Ring(pixels []capsulad.RGB) error {
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()
	p.pixels = append([]capsulad.RGB(nil), pixels...)
	// The first valid frame makes the presentation eligible; the capture is
	// what lets it win routing. A frame without a capture would leave the
	// configured baseline selected.
	if p.owner.captured {
		p.owner.selected = "airplay"
	}
	return nil
}

func (p *fakeRingPresentation) Close() error {
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()
	p.closed = true
	p.owner.order = append(p.owner.order, "presentation.close")
	return nil
}

type fakeRingCapture struct {
	owner  *fakeRinger
	closed bool
}

func (c *fakeRingCapture) Close() {
	c.owner.mu.Lock()
	defer c.owner.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.owner.order = append(c.owner.order, "capture.close")
	// Releasing the capture is what returns the ring to its configured
	// baseline; the presentation stays open until after this.
	c.owner.captured = false
	c.owner.selected = "baseline"
}

type fakeRinger struct {
	mu   sync.Mutex
	pres *fakeRingPresentation
	cap  *fakeRingCapture

	order          []string
	selected       string
	captured       bool
	captureErr     error
	resourceID     string
	presentationID string
	mode           capsulad.CaptureMode

	// captureWait blocks the grant until it closes, so a test can cancel the
	// acquisition while the framework is still considering it.
	captureWait    chan struct{}
	captureStarted chan struct{}
	captureOnce    sync.Once
}

func newFakeRinger() *fakeRinger {
	r := &fakeRinger{selected: "baseline"}
	r.pres = &fakeRingPresentation{owner: r, id: "prs_cover_art"}
	r.cap = &fakeRingCapture{owner: r}
	return r
}

func (f *fakeRinger) OpenPresentation(context.Context, string) (ringPresentation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "presentation")
	return f.pres, nil
}

func (f *fakeRinger) OpenCapture(ctx context.Context, resourceID, presentationID string,
	mode capsulad.CaptureMode) (ringCapture, error) {
	f.mu.Lock()
	f.order = append(f.order, "capture")
	f.resourceID, f.presentationID, f.mode = resourceID, presentationID, mode
	wait, started := f.captureWait, f.captureStarted
	f.mu.Unlock()
	if started != nil {
		f.captureOnce.Do(func() { close(started) })
	}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.captureErr != nil {
		return nil, f.captureErr
	}
	// The capture is a claim, not content: selection still waits for the first
	// eligible frame, exactly as the router's newest-capture rule does.
	f.captured = true
	return f.cap, nil
}

func (f *fakeRinger) snapshot() (order []string, selected string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...), f.selected
}

func TestOpenRingAcquiresTheRoutingCapture(t *testing.T) {
	fake := newFakeRinger()
	ring, err := openRing(context.Background(), fake)
	if err != nil {
		t.Fatalf("openRing: %v", err)
	}

	order, selected := fake.snapshot()
	if strings.Join(order, ",") != "presentation,capture" {
		t.Fatalf("acquisition order = %v, want presentation then capture", order)
	}
	if selected != "baseline" {
		t.Fatalf("selected app before the first frame = %q, want the baseline", selected)
	}
	if fake.resourceID != ringResource {
		t.Fatalf("capture resource = %q, want %q", fake.resourceID, ringResource)
	}
	if fake.presentationID != fake.pres.id {
		t.Fatalf("capture presentation = %q, want %q", fake.presentationID, fake.pres.id)
	}
	if fake.mode != capsulad.CaptureModeUnspecified {
		t.Fatalf("capture mode = %v, want unspecified for a ring", fake.mode)
	}

	pixels := make([]byte, 36)
	for i := range 12 {
		pixels[i*3] = byte(i)
		pixels[i*3+1] = byte(10 + i)
		pixels[i*3+2] = byte(20 + i)
	}
	if err := ring.Pixels(pixels); err != nil {
		t.Fatalf("Pixels: %v", err)
	}
	if len(fake.pres.pixels) != 12 {
		t.Fatalf("written pixels = %d, want 12", len(fake.pres.pixels))
	}
	if got := fake.pres.pixels[11]; got.R != 11 || got.G != 21 || got.B != 31 {
		t.Fatalf("pixel 11 = %+v, want the last frame triple", got)
	}
	if _, selected := fake.snapshot(); selected != "airplay" {
		t.Fatalf("selected app after the first frame = %q, want the cover-art presentation", selected)
	}

	if err := ring.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	order, selected = fake.snapshot()
	if strings.Join(order, ",") != "presentation,capture,capture.close,presentation.close" {
		t.Fatalf("release order = %v, want capture.close before presentation.close", order)
	}
	if !fake.cap.closed || !fake.pres.closed {
		t.Fatalf("close state = capture:%v presentation:%v, want both released", fake.cap.closed, fake.pres.closed)
	}
	if selected != "baseline" {
		t.Fatalf("selected app after release = %q, want the configured baseline", selected)
	}
}

func TestOpenRingClosesThePresentationWhenCaptureFails(t *testing.T) {
	fake := newFakeRinger()
	fake.captureErr = errors.New("routing refused")
	if _, err := openRing(context.Background(), fake); err == nil {
		t.Fatal("openRing succeeded without a capture")
	}
	_, selected := fake.snapshot()
	if !fake.pres.closed {
		t.Fatal("a failed capture left the presentation open")
	}
	if fake.cap.closed {
		t.Fatal("a capture that was never granted was closed")
	}
	if selected != "baseline" {
		t.Fatalf("selected app = %q, want the baseline", selected)
	}
}

// A session ending while the router is still considering the grant must not
// leave an open, unroutable presentation behind. Cancelling the acquisition
// context closes the presentation it already opened and never holds a capture,
// so the configured baseline stays selected.
func TestOpenRingCancellationClosesThePresentation(t *testing.T) {
	fake := newFakeRinger()
	fake.captureWait = make(chan struct{})
	fake.captureStarted = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := openRing(ctx, fake)
		result <- err
	}()

	select {
	case <-fake.captureStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the capture was never attempted")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("openRing succeeded after the acquisition was cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openRing ignored cancellation while the capture grant was pending")
	}

	order, selected := fake.snapshot()
	if strings.Join(order, ",") != "presentation,capture,presentation.close" {
		t.Fatalf("acquisition order = %v, want the presentation closed after the refused capture", order)
	}
	if !fake.pres.closed {
		t.Fatal("a cancelled capture left the presentation open")
	}
	if fake.cap.closed {
		t.Fatal("a capture that was never granted was closed")
	}
	if selected != "baseline" {
		t.Fatalf("selected app = %q, want the baseline", selected)
	}
}
