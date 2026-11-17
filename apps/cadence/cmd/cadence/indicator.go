package main

import (
	"context"
	"log"
	"math"
	"sync"
	"time"

	"capsulad/sdk/capsulad"
)

// ringResource is the assistant's capture indication resource. The framework
// withholds microphone audio unless this app both publishes a visible, non-blank
// ring frame and holds the router's capture for that ring: the presentation is
// the content, the capture is the selection that makes it the indication.
const ringResource = "light.ring.main"

const (
	ringPixels = 12
	// ringPeriod is one rendered frame. The daemon treats content not updated
	// for two seconds as stale, so a live render is the indication.
	ringPeriod = 33 * time.Millisecond
	// chasePeriod is how long the head takes to travel once around the ring.
	chasePeriod = 1100 * time.Millisecond
	chaseHead   = 3.0
)

type ringMode int

const (
	ringListen ringMode = iota
	ringThinking
	ringSpeaking
	ringFailed
)

// ringIndicator owns the capture indication for one turn: the presentation, the
// routing capture that selects it, the rendered phase and the measured level.
// The turn reports phase and level; rendering and capture events stay here.
// ringClient, ringPresentation and ringCapture are the framework surface the
// indication needs. They exist so a unit test can prove the capture obligation
// (presentation, then its first frame, then the routing capture that selects
// it) without the framework runtime; production is sdkRing.
type ringClient interface {
	OpenPresentation(ctx context.Context, resourceID string) (ringPresentation, error)
	OpenCapture(ctx context.Context, resourceID, presentationID string, mode capsulad.CaptureMode) (ringCapture, error)
}

type ringPresentation interface {
	ID() string
	Ring(pixels []capsulad.RGB) error
	Close() error
}

type ringCapture interface {
	Events() <-chan capsulad.CaptureEvent
	Close()
}

// sdkRing adapts *capsulad.Client (concrete returns) to the testable seam.
type sdkRing struct{ c *capsulad.Client }

func (s sdkRing) OpenPresentation(ctx context.Context, resourceID string) (ringPresentation, error) {
	return s.c.OpenPresentation(ctx, resourceID)
}

func (s sdkRing) OpenCapture(ctx context.Context, resourceID, presentationID string,
	mode capsulad.CaptureMode) (ringCapture, error) {
	return s.c.OpenCapture(ctx, resourceID, presentationID, mode)
}

type ringIndicator struct {
	client ringClient
	log    *log.Logger

	mu    sync.Mutex
	pres  ringPresentation
	cap   ringCapture
	mode  ringMode
	level float64 // target 0..1, from the last captured chunk
	shown float64 // smoothed level actually rendered
	stop  chan struct{}
	done  chan struct{}
	// pixels is reused by the render goroutine; nothing else reads it.
	pixels []capsulad.RGB
}

func newRingIndicator(client ringClient, logger *log.Logger) *ringIndicator {
	return &ringIndicator{client: client, log: logger, pixels: make([]capsulad.RGB, ringPixels)}
}

// Open publishes the first frame and then acquires the ring capture. Both steps
// are required: without the capture the daemon's indication check fails and
// every microphone open is refused.
func (r *ringIndicator) Open(ctx context.Context) error {
	r.mu.Lock()
	if r.pres != nil || r.cap != nil {
		r.mu.Unlock()
		return nil
	}
	r.mode = ringListen
	r.level, r.shown = 0, 0
	r.mu.Unlock()

	pres, err := r.client.OpenPresentation(ctx, ringResource)
	if err != nil {
		return err
	}
	// Content before capture: a capture over a blank presentation is not a
	// valid indication.
	if err := pres.Ring(r.buildFrame(0)); err != nil {
		_ = pres.Close()
		return err
	}
	capture, err := r.client.OpenCapture(ctx, ringResource, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		_ = pres.Close()
		return err
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	r.mu.Lock()
	r.pres, r.cap = pres, capture
	r.stop, r.done = stop, done
	r.mu.Unlock()

	go r.render(pres, stop, done)
	go r.watchCapture(capture, stop)
	return nil
}

// EnterListen marks the phase in which capture level drives brightness.
func (r *ringIndicator) EnterListen() {
	r.mu.Lock()
	r.mode = ringListen
	r.mu.Unlock()
}

// Level updates the measured capture level. It never changes the phase: while
// the reply plays the microphone is still open for a follow-up, and its room
// level must not repaint the speaking/thinking indication.
func (r *ringIndicator) Level(level float64) {
	r.mu.Lock()
	r.level = level
	r.mu.Unlock()
}

func (r *ringIndicator) Thinking() {
	r.mu.Lock()
	r.mode = ringThinking
	r.mu.Unlock()
}

func (r *ringIndicator) Speaking() {
	r.mu.Lock()
	r.mode = ringSpeaking
	r.mu.Unlock()
}

func (r *ringIndicator) Failed() {
	r.mu.Lock()
	r.mode = ringFailed
	r.mu.Unlock()
}

// Close stops rendering, releases the capture and then closes the
// presentation. The order matters: the capture owns the selection.
func (r *ringIndicator) Close() {
	r.mu.Lock()
	pres, capture, stop, done := r.pres, r.cap, r.stop, r.done
	r.pres, r.cap, r.stop, r.done = nil, nil, nil, nil
	r.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	if capture != nil {
		capture.Close()
	}
	if pres != nil {
		if err := pres.Close(); err != nil {
			r.log.Printf("indication_close: %v", err)
		}
	}
}

// watchCapture follows the router's selection. A lost selection means the
// indication is no longer valid (for example the framework's muted indicator
// took the ring); the daemon then withholds audio and ends the capture, which
// the turn sees as a capture notice. The event is logged here so the cause is
// visible.
func (r *ringIndicator) watchCapture(capture ringCapture, stop chan struct{}) {
	lost := false
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-capture.Events():
			if !ok {
				return
			}
			if !ev.Selected && !lost {
				lost = true
				r.log.Printf("indication_lost reason=%s", ev.Reason)
			}
		}
	}
}

func (r *ringIndicator) render(pres ringPresentation, stop, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(ringPeriod)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			r.advance()
			if err := pres.Ring(r.buildFrame(time.Since(start).Seconds())); err != nil {
				// The stream can end under us (daemon shutdown, mute, app stop);
				// the turn's own teardown reports the reason.
				return
			}
		}
	}
}

// advance smooths the level: fast attack so speech is visible immediately, slow
// release so the ring does not flicker between syllables.
func (r *ringIndicator) advance() {
	r.mu.Lock()
	defer r.mu.Unlock()
	const attack, release = 0.6, 0.18
	if r.level > r.shown {
		r.shown += (r.level - r.shown) * attack
	} else {
		r.shown += (r.level - r.shown) * release
	}
}

// buildFrame renders one frame into the reused pixel buffer.
func (r *ringIndicator) buildFrame(elapsed float64) []capsulad.RGB {
	r.mu.Lock()
	mode := r.mode
	shown := r.shown
	pixels := r.pixels
	r.mu.Unlock()

	for i := range pixels {
		pixels[i] = capsulad.RGB{}
	}
	switch mode {
	case ringThinking:
		chase(pixels, elapsed/chasePeriodless(), rgb{0x20, 0x40, 0xFF}, 0.55)
	case ringSpeaking:
		chase(pixels, elapsed/chasePeriodless(), rgb{0x00, 0xC0, 0x30}, 0.8)
	case ringFailed:
		solid(pixels, rgb{0xC0, 0x10, 0x10}, 0.85)
	default:
		// Teal listening state; a fully dark ring is not a valid indication, so
		// the floor keeps the ring visibly lit even in silence.
		solid(pixels, rgb{0x00, 0x80, 0x80}, 0.10+0.90*clamp01(shown))
	}
	return pixels
}

func chasePeriodless() float64 { return chasePeriod.Seconds() }

type rgb struct{ r, g, b uint8 }

func solid(pixels []capsulad.RGB, c rgb, level float64) {
	level = clamp01(level)
	p := capsulad.RGB{R: scale(c.r, level), G: scale(c.g, level), B: scale(c.b, level)}
	for i := range pixels {
		pixels[i] = p
	}
}

// chase lights a moving head with a short tail; intensity is the peak
// brightness of the head.
func chase(pixels []capsulad.RGB, phase float64, c rgb, intensity float64) {
	head := math.Mod(phase, 1) * float64(len(pixels))
	for i := range pixels {
		d := math.Abs(float64(i) - head)
		if d > float64(len(pixels))/2 {
			d = float64(len(pixels)) - d
		}
		if d > chaseHead {
			continue
		}
		fade := (1 - d/chaseHead) * intensity
		pixels[i] = capsulad.RGB{R: scale(c.r, fade), G: scale(c.g, fade), B: scale(c.b, fade)}
	}
}

func scale(v uint8, f float64) uint8 {
	f = clamp01(f)
	return uint8(float64(v) * f)
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
