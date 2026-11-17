package hw

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"capsulad/apps/homeassistant/internal/ha"
	"capsulad/sdk/capsulad"
)

// presentTimeout bounds one presentation or capture acquisition. The daemon
// answers immediately on a healthy device; a bounded call keeps a Home
// Assistant command from hanging on a wedged backend.
const presentTimeout = 5 * time.Second

// ErrReleased reports that hardware ownership was released while a presentation
// was being acquired. The release wins: a command that raced it fails instead of
// resurrecting ownership the operator just dropped.
var ErrReleased = errors.New("hw: hardware ownership was released during setup")

// Controller owns every presentation and capture the bridge holds. Ownership
// is explicit and released on every terminal path: a Home Assistant OFF, the
// bridge's own cleanup, or the app exiting all end with the device's
// configured baseline presentation selected again.
//
// A release never forces a blank frame over that baseline. The user's chosen
// default ring or display effect resumes because the capture is gone, not
// because the bridge paints black.
//
// Acquisition (which blocks in the framework) never happens while the state
// mutex is held: a release must not queue behind a 5-second open. Instead each
// release bumps a generation, and an acquisition that finds the generation
// changed closes what it opened and reports ErrReleased.
type Controller struct {
	client *capsulad.Client
	logf   func(format string, args ...any)

	ringResource    string
	ringPixels      int
	segmentResource string
	touchResource   string

	mu          sync.Mutex
	generation  uint64
	ringPres    *capsulad.Presentation
	ringCap     *capsulad.Capture
	displayPres *capsulad.Presentation
	displayCap  *capsulad.Capture
	touchCap    *capsulad.Capture
}

// NewController returns a controller for the advertised resources. A zero
// pixel count falls back to a single pixel; the daemon clamps the frame to the
// real ring.
func NewController(client *capsulad.Client, ringResource string, ringPixels int,
	segmentResource, touchResource string, logf func(string, ...any)) *Controller {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if ringPixels <= 0 {
		ringPixels = 1
	}
	return &Controller{
		client:          client,
		logf:            logf,
		ringResource:    ringResource,
		ringPixels:      ringPixels,
		segmentResource: segmentResource,
		touchResource:   touchResource,
	}
}

// RingSetColor lights the whole ring with one colour at intensity 0..1.
func (c *Controller) RingSetColor(color ha.RGB, intensity float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), presentTimeout)
	defer cancel()

	pres, err := c.ringPresentation(ctx)
	if err != nil {
		return err
	}
	scaled := scaleRGB(color, intensity)
	pixels := make([]capsulad.RGB, c.ringPixels)
	for i := range pixels {
		pixels[i] = capsulad.RGB{R: scaled.R, G: scaled.G, B: scaled.B}
	}
	if err := pres.Ring(pixels); err != nil {
		c.dropRingIfCurrent(pres)
		return fmt.Errorf("write ring frame: %w", err)
	}
	return nil
}

// RingSetEffect shows one built-in ring effect.
func (c *Controller) RingSetEffect(presetID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), presentTimeout)
	defer cancel()

	pres, err := c.ringPresentation(ctx)
	if err != nil {
		return err
	}
	if err := pres.Effect(presetID, nil); err != nil {
		c.dropRingIfCurrent(pres)
		return fmt.Errorf("write ring effect %s: %w", presetID, err)
	}
	return nil
}

// RingRelease hands the ring back to the device's baseline.
func (c *Controller) RingRelease() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(c.dropRingLocked)
}

// DisplaySetFace shows one built-in segment face.
func (c *Controller) DisplaySetFace(presetID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), presentTimeout)
	defer cancel()

	pres, err := c.displayPresentation(ctx)
	if err != nil {
		return err
	}
	if err := pres.Face(presetID, nil); err != nil {
		c.dropDisplayIfCurrent(pres)
		return fmt.Errorf("write segment face %s: %w", presetID, err)
	}
	return nil
}

// DisplayRelease hands the display back to the device's baseline.
func (c *Controller) DisplayRelease() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(c.dropDisplayLocked)
}

// ReleaseRingAndDisplay is the operator-facing release: the two surfaces the
// manifest and the Home Assistant buttons promise. The touch capture is a
// configured exposure rather than an ad-hoc acquisition, so it is governed by
// expose_touch and the runtime restart that follows a settings change.
func (c *Controller) ReleaseRingAndDisplay() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.dropDisplayLocked()
	c.dropRingLocked()
}

// MediaCommand sends a transport command to the connected Bluetooth source.
func (c *Controller) MediaCommand(cmd string) error {
	framework := NewFramework(c.client)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return framework.MediaCommand(ctx, cmd)
}

// AcquireTouch takes (or returns) the partial-gesture touch capture. It is
// idempotent: a held capture is returned as is, and a capture whose stream
// ended is replaced.
func (c *Controller) AcquireTouch(ctx context.Context) (*capsulad.Capture, error) {
	c.mu.Lock()
	if c.touchCap != nil {
		select {
		case <-c.touchCap.Done():
			c.dropTouchLocked()
		default:
			capture := c.touchCap
			c.mu.Unlock()
			return capture, nil
		}
	}
	generation := c.generation
	c.mu.Unlock()

	capture, err := c.client.OpenCapture(ctx, c.touchResource, "", capsulad.CaptureModePartialGestures)
	if err != nil {
		return nil, fmt.Errorf("capture touch panel: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		capture.Close()
		return nil, ErrReleased
	}
	if c.touchCap != nil {
		capture.Close()
		return c.touchCap, nil
	}
	c.touchCap = capture
	return capture, nil
}

// ReleaseTouch drops the touch capture, returning the panel to the framework.
func (c *Controller) ReleaseTouch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(c.dropTouchLocked)
}

// ReleaseAll drops every owned presentation and capture. It is safe to call
// more than once and is the shutdown path for the bridge.
func (c *Controller) ReleaseAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.dropTouchLocked()
	c.dropDisplayLocked()
	c.dropRingLocked()
}

// releaseLocked bumps the generation so an in-flight acquisition is abandoned,
// then drops what the caller names.
func (c *Controller) releaseLocked(drop func()) {
	c.generation++
	drop()
}

// ringPresentation returns a live ring presentation, opening one when needed.
func (c *Controller) ringPresentation(ctx context.Context) (*capsulad.Presentation, error) {
	c.mu.Lock()
	if c.ringPres != nil {
		select {
		case <-c.ringPres.Done():
			c.dropRingLocked()
		default:
			pres := c.ringPres
			c.mu.Unlock()
			return pres, nil
		}
	}
	generation := c.generation
	c.mu.Unlock()

	pres, capture, err := c.open(ctx, c.ringResource, "ring")
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.installLocked(generation, pres, capture, &c.ringPres, &c.ringCap)
}

// displayPresentation returns a live segment presentation, opening one when
// needed.
func (c *Controller) displayPresentation(ctx context.Context) (*capsulad.Presentation, error) {
	c.mu.Lock()
	if c.displayPres != nil {
		select {
		case <-c.displayPres.Done():
			c.dropDisplayLocked()
		default:
			pres := c.displayPres
			c.mu.Unlock()
			return pres, nil
		}
	}
	generation := c.generation
	c.mu.Unlock()

	pres, capture, err := c.open(ctx, c.segmentResource, "segment")
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.installLocked(generation, pres, capture, &c.displayPres, &c.displayCap)
}

// open performs the blocking framework calls: a presentation and the capture
// that makes it routable. A frame alone is not routable, so the capture is part
// of acquisition.
func (c *Controller) open(ctx context.Context, resource, what string) (*capsulad.Presentation, *capsulad.Capture, error) {
	pres, err := c.client.OpenPresentation(ctx, resource)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s presentation: %w", what, err)
	}
	capture, err := c.client.OpenCapture(ctx, resource, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		_ = pres.Close()
		return nil, nil, fmt.Errorf("capture %s: %w", resource, err)
	}
	return pres, capture, nil
}

// installLocked publishes a freshly opened presentation, honouring a release
// that happened while it was opening.
func (c *Controller) installLocked(generation uint64, pres *capsulad.Presentation,
	capture *capsulad.Capture, presSlot **capsulad.Presentation, capSlot **capsulad.Capture) (*capsulad.Presentation, error) {
	if c.generation != generation {
		capture.Close()
		_ = pres.Close()
		return nil, ErrReleased
	}
	if *presSlot != nil {
		// Another acquisition won the race; this one is redundant.
		capture.Close()
		_ = pres.Close()
		return *presSlot, nil
	}
	*presSlot = pres
	*capSlot = capture
	return pres, nil
}

// dropRingIfCurrent releases the ring only when the given presentation is the
// one installed, so a frame failure cannot drop a newer presentation.
func (c *Controller) dropRingIfCurrent(pres *capsulad.Presentation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ringPres == pres {
		c.generation++
		c.dropRingLocked()
	}
}

func (c *Controller) dropDisplayIfCurrent(pres *capsulad.Presentation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.displayPres == pres {
		c.generation++
		c.dropDisplayLocked()
	}
}

// dropRingLocked releases the capture first so routing recomputes to the
// baseline while the presentation is still alive, then closes the
// presentation. Closing either one also releases dependent captures; doing
// both explicitly keeps the accounting obvious.
func (c *Controller) dropRingLocked() {
	if c.ringCap != nil {
		c.ringCap.Close()
		c.ringCap = nil
	}
	if c.ringPres != nil {
		_ = c.ringPres.Close()
		c.ringPres = nil
	}
}

func (c *Controller) dropDisplayLocked() {
	if c.displayCap != nil {
		c.displayCap.Close()
		c.displayCap = nil
	}
	if c.displayPres != nil {
		_ = c.displayPres.Close()
		c.displayPres = nil
	}
}

func (c *Controller) dropTouchLocked() {
	if c.touchCap != nil {
		c.touchCap.Close()
		c.touchCap = nil
	}
}

// scaleRGB applies an intensity to a colour, clamping to 0..255.
func scaleRGB(color ha.RGB, intensity float64) ha.RGB {
	scale := func(value uint8) uint8 {
		scaled := math.Round(float64(value) * intensity)
		if scaled <= 0 {
			return 0
		}
		if scaled >= 255 {
			return 255
		}
		return uint8(scaled)
	}
	return ha.RGB{R: scale(color.R), G: scale(color.G), B: scale(color.B)}
}
