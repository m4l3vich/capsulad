package radio

import (
	"context"
	"sync"
	"time"

	"capsulad/sdk/capsulad"
)

const (
	presentationService = "capsulad.framework.v1.PresentationService"
	openDisplayTimeout   = 5 * time.Second
)

var segmentDigits = [10]uint16{0x3F, 0x06, 0x5B, 0x4F, 0x66, 0x6D, 0x7D, 0x07, 0x7F, 0x6F}

const segmentDash = 0x40

// segmentCell mirrors capsulad.framework.v1.SegmentCell. The SDK's convenience
// Segment helper omits the decimal point, which is a physically relevant field
// for the paused state, so this app speaks the same public PresentationService
// JSON contract directly.
type segmentCell struct {
	Mask         uint16 `json:"mask,omitempty"`
	DecimalPoint bool   `json:"decimalPoint,omitempty"`
}

type segmentFrameJSON struct {
	Cells []segmentCell `json:"cells,omitempty"`
	Colon bool          `json:"colon,omitempty"`
}

type segmentRequestJSON struct {
	Open  *struct {
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
	done   chan struct{}
	once   sync.Once
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
				done:   make(chan struct{}),
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
	p.once.Do(func() { close(p.done) })
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
	case <-p.done:
	case <-timer.C:
	}
	_ = p.stream.Close()
	p.cancel()
}

type displayState struct {
	phase   Phase
	station int
}

func (a *App) displayState() displayState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return displayState{phase: a.phase, station: a.stationNumber}
}

func displayFrame(state displayState) ([]segmentCell, bool) {
	if state.phase == PhaseError {
		cells := make([]segmentCell, 4)
		for i := range cells {
			cells[i] = segmentCell{Mask: segmentDash}
		}
		return cells, false
	}
	number := state.station
	if number < 1 {
		number = 1
	} else if number > 99 {
		number = 99
	}
	tens := number / 10
	ones := number % 10
	cells := []segmentCell{
		{},
		{},
		{Mask: segmentDigits[tens]},
		{Mask: segmentDigits[ones], DecimalPoint: state.phase == PhasePaused},
	}
	return cells, state.phase == PhasePlaying
}

// displayLoop converges on the newest state. Only this goroutine touches the
// presentation and its routing capture, and every close path releases both.
func (a *App) displayLoop() {
	defer close(a.displayDone)
	var (
		presentation *segmentPresentation
		capture      *capsulad.Capture
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
		select {
		case <-a.ctx.Done():
			return
		case <-a.drawCh:
		}
		state := a.displayState()
		if state.phase == PhaseStopped {
			closePresentation()
			continue
		}
		if presentation != nil {
			select {
			case <-presentation.done:
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
			opened, err := openSegmentPresentation(a.ctx, a.client)
			if err != nil {
				a.logf("display open failed (will retry): %v", err)
				continue
			}
			presentation = opened
		}
		if capture == nil && presentation.id != "" {
			opened, err := a.client.OpenCapture(a.ctx, segmentResource, presentation.id, capsulad.CaptureModeUnspecified)
			if err != nil {
				a.logf("display capture failed: %v", err)
			} else {
				capture = opened
			}
		}
		cells, colon := displayFrame(state)
		if err := presentation.send(cells, colon); err != nil {
			a.logf("display frame failed: %v", err)
			closePresentation()
		}
	}
}