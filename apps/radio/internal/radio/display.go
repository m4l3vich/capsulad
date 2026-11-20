package radio

import (
	"context"
	"sync"
	"time"

	"capsulad/sdk/capsulad"
)

const (
	presentationService = "capsulad.framework.v1.PresentationService"
	openDisplayTimeout  = 5 * time.Second

	// defaultDisplayWindow is how long the station number or error pattern
	// owns the segment display after a tuning attempt when "Always display
	// station number" is off. Closing the presentation afterwards releases
	// the overlay so the configured baseline returns; it never paints black
	// over a deliberately selected baseline and never touches audio or the
	// LED ring.
	defaultDisplayWindow = 10 * time.Second
)

var segmentDigits = [10]uint16{0x3F, 0x06, 0x5B, 0x4F, 0x66, 0x6D, 0x7D, 0x07, 0x7F, 0x6F}

const segmentDash = 0x40

// A paused radio shows a pause sign in the middle two cells: the right
// vertical pair of the left cell next to the left vertical pair of the right
// cell. The inter-digit gap separates two full-height bars, so the frame reads
// as `||` without lighting the colon or a decimal point.
const (
	segmentPauseLeft  = 0x06 // segments b and c
	segmentPauseRight = 0x30 // segments e and f
)

// segmentCell mirrors capsulad.framework.v1.SegmentCell. The app speaks the
// same public PresentationService JSON contract directly so the display frame
// stays independent of SDK convenience helpers.
type segmentCell struct {
	Mask         uint16 `json:"mask,omitempty"`
	DecimalPoint bool   `json:"decimalPoint,omitempty"`
}

type segmentFrameJSON struct {
	Cells []segmentCell `json:"cells,omitempty"`
	Colon bool          `json:"colon,omitempty"`
}

type segmentRequestJSON struct {
	Open *struct {
		ResourceID string `json:"resourceId,omitempty"`
	} `json:"open,omitempty"`
	Frame *struct {
		Segment *segmentFrameJSON `json:"segment,omitempty"`
	} `json:"frame,omitempty"`
	Close *struct{} `json:"close,omitempty"`
}

type segmentEventJSON struct {
	Ready *struct {
		PresentationID string `json:"presentationId"`
		ResourceID     string `json:"resourceId"`
	} `json:"ready,omitempty"`
	Visibility *struct {
		Visible bool   `json:"visible"`
		Reason  string `json:"reason"`
	} `json:"visibility,omitempty"`
}

type segmentPresentation struct {
	stream *capsulad.Stream
	id     string
	cancel context.CancelFunc
	doneCh chan struct{}
	once   sync.Once
}

// segmentSink is the display loop's view of one open presentation. It exists
// so the loop's ownership/release timing can be tested without a daemon; the
// production implementation is segmentPresentation.
type segmentSink interface {
	presentationID() string
	done() <-chan struct{}
	send(cells []segmentCell, colon bool) error
	close()
}

// captureSink is the owned routing capture held alongside a presentation.
type captureSink interface {
	Done() <-chan struct{}
	Close()
}

// displayHost opens the segment presentation and its routing capture. The
// production host talks to PresentationService; tests substitute a recorder.
type displayHost interface {
	open(ctx context.Context) (segmentSink, error)
	capture(ctx context.Context, presentationID string) (captureSink, error)
}

type clientDisplayHost struct {
	client *capsulad.Client
}

func (h clientDisplayHost) open(ctx context.Context) (segmentSink, error) {
	return openSegmentPresentation(ctx, h.client)
}

func (h clientDisplayHost) capture(ctx context.Context, presentationID string) (captureSink, error) {
	return h.client.OpenCapture(ctx, segmentResource, presentationID, capsulad.CaptureModeUnspecified)
}

func openSegmentPresentation(ctx context.Context, client *capsulad.Client) (*segmentPresentation, error) {
	callCtx, cancel := context.WithCancel(ctx)
	stream, err := client.OpenBidi(callCtx, presentationService, "Open")
	if err != nil {
		cancel()
		return nil, err
	}
	timer := time.AfterFunc(openDisplayTimeout, cancel)
	request := segmentRequestJSON{
		Open: &struct {
			ResourceID string `json:"resourceId,omitempty"`
		}{ResourceID: segmentResource},
	}
	if err := stream.SendJSON(request); err != nil {
		timer.Stop()
		cancel()
		_ = stream.Close()
		return nil, err
	}
	var event segmentEventJSON
	for {
		if err := stream.RecvJSON(&event); err != nil {
			timer.Stop()
			cancel()
			_ = stream.Close()
			return nil, err
		}
		if event.Ready != nil {
			timer.Stop()
			p := &segmentPresentation{
				stream: stream,
				id:     event.Ready.PresentationID,
				cancel: cancel,
				doneCh: make(chan struct{}),
			}
			go p.readLoop()
			return p, nil
		}
	}
}

func (p *segmentPresentation) readLoop() {
	defer p.closeDone()
	for {
		var event segmentEventJSON
		if err := p.stream.RecvJSON(&event); err != nil {
			return
		}
	}
}

func (p *segmentPresentation) closeDone() {
	p.once.Do(func() { close(p.doneCh) })
}

func (p *segmentPresentation) send(cells []segmentCell, colon bool) error {
	return p.stream.SendJSON(segmentRequestJSON{
		Frame: &struct {
			Segment *segmentFrameJSON `json:"segment,omitempty"`
		}{Segment: &segmentFrameJSON{Cells: cells, Colon: colon}},
	})
}

func (p *segmentPresentation) close() {
	p.closeDone()
	_ = p.stream.SendJSON(segmentRequestJSON{Close: &struct{}{}})
	_ = p.stream.HalfClose()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-p.doneCh:
	case <-timer.C:
	}
	_ = p.stream.Close()
	p.cancel()
}

func (p *segmentPresentation) presentationID() string { return p.id }
func (p *segmentPresentation) done() <-chan struct{}  { return p.doneCh }

type displayState struct {
	phase         Phase
	station       int
	alwaysDisplay bool
	displayUntil  time.Time
	// exitAt is the armed run-exit deadline. A terminal error with "Always
	// display" on may hold its dashes only until then; a paused state uses
	// displayUntil, which is the same pause deadline.
	exitAt time.Time
}

type displayOutput struct {
	cells []segmentCell
	colon bool
	// hold is false when the display loop must release its presentation so
	// the configured baseline returns. cells is then nil: releasing is not a
	// request to paint a blank or black frame.
	hold bool
}

func (a *App) clockNow() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *App) displayState() displayState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return displayState{
		phase:         a.phase,
		station:       a.stationNumber,
		alwaysDisplay: a.alwaysDisplay,
		displayUntil:  a.displayUntil,
		exitAt:        a.exitAt,
	}
}

// beginDisplayRunLocked starts the transient display window for one tuning
// attempt. It is called under a.mu. A newer attempt replaces the deadline, so
// a timer armed for the previous attempt can never release the newer one: the
// loop always recomputes visibility from the current deadline.
func (a *App) beginDisplayRunLocked() {
	window := a.displayWindow
	if window <= 0 {
		window = defaultDisplayWindow
	}
	a.displayUntil = a.clockNow().Add(window)
}

// displayFrame builds the segment frame for a visible radio state. The colon
// belongs to the clock/volume presentations and is never lit by the station
// number, the pause sign or the error pattern.
func displayFrame(state displayState) []segmentCell {
	switch state.phase {
	case PhaseError:
		cells := make([]segmentCell, 4)
		for i := range cells {
			cells[i] = segmentCell{Mask: segmentDash}
		}
		return cells
	case PhasePaused:
		return []segmentCell{
			{},
			{Mask: segmentPauseLeft},
			{Mask: segmentPauseRight},
			{},
		}
	}
	number := state.station
	if number < 1 {
		number = 1
	} else if number > 99 {
		number = 99
	}
	tens := number / 10
	ones := number % 10
	return []segmentCell{
		{},
		{},
		{Mask: segmentDigits[tens]},
		{Mask: segmentDigits[ones]},
	}
}

// displayDeadline is when the current indication must stop owning the display.
// Playing and buffering use the tuning window unless "Always display station
// number" keeps them; paused always ends at the pause hold, even with the
// setting on; a terminal error uses its tuning window, or the armed run exit
// while "Always display" is on. The second result is false when the indication
// is not time-bounded by this state alone.
func displayDeadline(state displayState) (time.Time, bool) {
	switch state.phase {
	case PhasePaused:
		return state.displayUntil, true
	case PhaseError:
		deadline := state.displayUntil
		if state.alwaysDisplay {
			deadline = state.exitAt
		} else if !state.exitAt.IsZero() && state.exitAt.Before(deadline) {
			deadline = state.exitAt
		}
		return deadline, true
	case PhasePlaying, PhaseBuffering:
		if state.alwaysDisplay {
			return time.Time{}, false
		}
		return state.displayUntil, true
	default:
		return time.Time{}, false
	}
}

// displayOutputFor decides whether the radio currently owns the segment
// display. With "Always display station number" on, a playing state owns it
// until stop. With it off, ownership ends at the deadline set by the last
// tuning attempt; the loop then closes the presentation, restoring whatever
// baseline the user selected instead of forcing a frame. A paused radio shows
// the pause sign for its whole hold even with the setting off, and a terminal
// error never holds past the run it is already ending.
func displayOutputFor(state displayState, now time.Time) displayOutput {
	if state.phase == PhaseStopped {
		return displayOutput{}
	}
	if deadline, timed := displayDeadline(state); timed && !now.Before(deadline) {
		return displayOutput{}
	}
	return displayOutput{cells: displayFrame(state), colon: false, hold: true}
}

func (a *App) waitForDraw() bool {
	select {
	case <-a.ctx.Done():
		return false
	case <-a.drawCh:
		return true
	}
}

// waitForDisplayChange blocks until a state change or the current indication's
// deadline. Paused and terminal-error states wait for their own deadline even
// with "Always display station number" on, because the run itself ends then.
func (a *App) waitForDisplayChange(state displayState) bool {
	deadline, timed := displayDeadline(state)
	if !timed {
		return a.waitForDraw()
	}
	remaining := deadline.Sub(a.clockNow())
	if remaining <= 0 {
		return true
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-a.ctx.Done():
		return false
	case <-a.drawCh:
		return true
	case <-timer.C:
		return true
	}
}

// displayLoop converges on the newest state. Only this goroutine touches the
// presentation and its routing capture, and every close path releases both.
func (a *App) displayLoop() {
	defer close(a.displayDone)
	var (
		presentation segmentSink
		capture      captureSink
	)
	closePresentation := func() {
		if capture != nil {
			capture.Close()
			capture = nil
		}
		if presentation != nil {
			presentation.close()
			presentation = nil
		}
	}
	defer closePresentation()

	for {
		state := a.displayState()
		output := displayOutputFor(state, a.clockNow())
		if !output.hold {
			closePresentation()
			if !a.waitForDraw() {
				return
			}
			continue
		}
		if presentation != nil {
			select {
			case <-presentation.done():
				closePresentation()
			default:
			}
		}
		if capture != nil {
			select {
			case <-capture.Done():
				capture.Close()
				capture = nil
			default:
			}
		}
		if presentation == nil {
			opened, err := a.display.open(a.ctx)
			if err != nil {
				a.logf("display open failed (will retry): %v", err)
				if !a.waitForDraw() {
					return
				}
				continue
			}
			presentation = opened
		}
		if capture == nil && presentation.presentationID() != "" {
			opened, err := a.display.capture(a.ctx, presentation.presentationID())
			if err != nil {
				a.logf("display capture failed: %v", err)
			} else {
				capture = opened
			}
		}
		// Opening a presentation or capture can take seconds. Re-read the
		// state so a stop, a retune or an expired window during that I/O
		// never publishes a stale frame or keeps ownership too long.
		state = a.displayState()
		output = displayOutputFor(state, a.clockNow())
		if !output.hold {
			closePresentation()
			if !a.waitForDraw() {
				return
			}
			continue
		}
		if err := presentation.send(output.cells, output.colon); err != nil {
			a.logf("display frame failed: %v", err)
			closePresentation()
			if !a.waitForDraw() {
				return
			}
			continue
		}
		if !a.waitForDisplayChange(state) {
			return
		}
	}
}
