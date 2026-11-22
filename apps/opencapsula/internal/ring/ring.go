// Package ring owns the LED-ring presentation an assistant turn needs. The
// presentation is also the visible indication the framework requires before it
// will deliver microphone audio, so it is opened before capture and released
// when the turn goes idle.
package ring

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"capsulad/sdk/capsulad"
)

// ResourceID is the primary ring on every supported speaker.
const ResourceID = "light.ring.main"

const (
	pixels    = 12
	frameRate = 33 * time.Millisecond
	levelRise = 70 * time.Millisecond
	levelFall = 240 * time.Millisecond
)

// State is what the ring shows.
type State int

const (
	StateIdle State = iota
	StateListening
	StateThinking
	StateSpeaking
	StateError
	StateDisconnected
)

func (s State) String() string {
	switch s {
	case StateListening:
		return "listening"
	case StateThinking:
		return "thinking"
	case StateSpeaking:
		return "speaking"
	case StateError:
		return "error"
	case StateDisconnected:
		return "disconnected"
	default:
		return "idle"
	}
}

var (
	teal  = capsulad.RGB{G: 128, B: 128}
	blue  = capsulad.RGB{G: 64, B: 255}
	green = capsulad.RGB{G: 192, B: 96}
	red   = capsulad.RGB{R: 220, G: 32, B: 32}
	amber = capsulad.RGB{R: 200, G: 120, B: 0}
)

// View is one ring presentation and its renderer. Methods are safe for
// concurrent use by the turn loop and the connection loop.
type View struct {
	client *capsulad.Client
	log    *log.Logger

	// opMu serializes open and close, which are slow service calls. mu
	// protects the renderer's snapshot and is never held across a wait.
	opMu sync.Mutex

	mu      sync.Mutex
	pres    *capsulad.Presentation
	capture *capsulad.Capture
	stop    chan struct{}
	done    chan struct{}
	state   State
	level   float64
	target  float64
	last    time.Time
	gen     uint64
	buf     []capsulad.RGB
}

// New creates a closed view. Nothing is opened until Show.
func New(client *capsulad.Client, logger *log.Logger) *View {
	if logger == nil {
		logger = log.Default()
	}
	return &View{client: client, log: logger, buf: make([]capsulad.RGB, pixels)}
}

// Generation changes on every Show, so a delayed release from an error state
// can tell that a newer turn has taken the ring.
func (v *View) Generation() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gen
}

// Show opens the ring when necessary and sets the desired state. It returns an
// error when the ring resource or the capture grant is unavailable; the caller
// must then abandon the turn rather than capture without an indication.
func (v *View) Show(ctx context.Context, state State) error {
	v.opMu.Lock()
	defer v.opMu.Unlock()

	v.mu.Lock()
	v.gen++
	if v.pres != nil {
		v.state = state
		v.mu.Unlock()
		return nil
	}
	v.state = state
	v.level, v.target = 0, 0
	v.last = time.Now()
	v.mu.Unlock()

	pres, err := v.client.OpenPresentation(ctx, ResourceID)
	if err != nil {
		return fmt.Errorf("ring presentation: %w", err)
	}
	// Write a lit frame before asking for capture: a blank or unselected
	// presentation is not an indication.
	v.mu.Lock()
	v.pres = pres
	frame := v.frameLocked(time.Now())
	v.mu.Unlock()
	if err := pres.Ring(frame); err != nil {
		_ = pres.Close()
		v.mu.Lock()
		v.pres = nil
		v.mu.Unlock()
		return fmt.Errorf("ring frame: %w", err)
	}
	capture, err := v.client.OpenCapture(ctx, ResourceID, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		_ = pres.Close()
		v.mu.Lock()
		v.pres = nil
		v.mu.Unlock()
		return fmt.Errorf("ring capture: %w", err)
	}

	v.mu.Lock()
	v.capture = capture
	v.stop = make(chan struct{})
	v.done = make(chan struct{})
	stop, done := v.stop, v.done
	v.mu.Unlock()
	go v.render(stop, done)
	return nil
}

// Set changes the state of an open ring. It is a no-op when the ring is closed.
func (v *View) Set(state State) {
	v.mu.Lock()
	v.state = state
	v.mu.Unlock()
}

// Observe publishes the latest input or output level in [0,1].
func (v *View) Observe(level float64) {
	v.mu.Lock()
	v.target = clamp01(level)
	v.mu.Unlock()
}

// Release stops the renderer and releases the presentation and capture. A
// later Show opens a fresh one. Releasing an already closed ring is a no-op.
func (v *View) Release() {
	v.opMu.Lock()
	defer v.opMu.Unlock()

	v.mu.Lock()
	stop, done := v.stop, v.done
	pres, capture := v.pres, v.capture
	v.stop, v.done, v.pres, v.capture = nil, nil, nil, nil
	if stop != nil {
		close(stop)
	}
	v.mu.Unlock()

	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			v.log.Printf("opencapsula: ring renderer did not stop promptly")
		}
	}
	if capture != nil {
		capture.Close()
	}
	if pres != nil {
		if err := pres.Close(); err != nil {
			v.log.Printf("opencapsula: ring release: %v", err)
		}
	}
}

func (v *View) render(stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(frameRate)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			v.mu.Lock()
			pres := v.pres
			frame := v.frameLocked(time.Now())
			v.mu.Unlock()
			if pres == nil {
				return
			}
			if err := pres.Ring(frame); err != nil {
				if !errors.Is(err, context.Canceled) {
					v.log.Printf("opencapsula: ring frame: %v", err)
				}
				go v.Release()
				return
			}
		}
	}
}

// frameLocked returns the reused pixel buffer. v.mu must be held.
func (v *View) frameLocked(now time.Time) []capsulad.RGB {
	dt := now.Sub(v.last)
	if dt <= 0 || dt > time.Second {
		dt = frameRate
	}
	v.last = now
	tau := levelFall
	if v.target > v.level {
		tau = levelRise
	}
	alpha := 1 - math.Exp(-dt.Seconds()/tau.Seconds())
	v.level = clamp01(v.level + (v.target-v.level)*alpha)
	level := v.level

	switch v.state {
	case StateListening:
		brightness := 0.08 + 0.92*level
		pixel := scale(teal, brightness)
		for i := range v.buf {
			v.buf[i] = pixel
		}
	case StateSpeaking:
		brightness := 0.18 + 0.82*level
		pixel := scale(green, brightness)
		for i := range v.buf {
			v.buf[i] = pixel
		}
	case StateThinking:
		phase := math.Mod(float64(now.UnixNano())/1e9/1.1, 1)
		for i := range v.buf {
			distance := math.Abs(float64(i)/pixels - phase)
			if distance > 0.5 {
				distance = 1 - distance
			}
			head := 0.0
			if limit := 3.0 / pixels; distance < limit {
				head = 1 - distance/limit
			}
			v.buf[i] = scale(blue, 0.14+0.86*head*head)
		}
	case StateError:
		brightness := 0.35 + 0.65*(0.5+0.5*math.Sin(float64(now.UnixNano())/1e9*4))
		pixel := scale(red, brightness)
		for i := range v.buf {
			v.buf[i] = pixel
		}
	case StateDisconnected:
		brightness := 0.18 + 0.42*(0.5+0.5*math.Sin(float64(now.UnixNano())/1e9*1.6))
		pixel := scale(amber, brightness)
		for i := range v.buf {
			v.buf[i] = pixel
		}
	default:
		for i := range v.buf {
			v.buf[i] = capsulad.RGB{}
		}
	}
	return v.buf
}

func scale(color capsulad.RGB, brightness float64) capsulad.RGB {
	brightness = clamp01(brightness)
	return capsulad.RGB{
		R: uint8(float64(color.R)*brightness + 0.5),
		G: uint8(float64(color.G)*brightness + 0.5),
		B: uint8(float64(color.B)*brightness + 0.5),
	}
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
