package capsula

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const routingService = "capsula.framework.v1.RoutingService"

type captureGrantedJSON struct {
	Capture *CaptureInfo `json:"capture"`
}

type captureChangedJSON struct {
	Selected bool   `json:"selected"`
	Reason   string `json:"reason"`
}

type captureEventJSON struct {
	Granted *captureGrantedJSON `json:"granted"`
	Changed *captureChangedJSON `json:"changed"`
}

// Capture is one temporary routing selection.
type Capture struct {
	call *callHandle

	mu   sync.Mutex
	info CaptureInfo

	events chan CaptureEvent
	done   chan struct{}
	once   sync.Once
}

// OpenCapture acquires a capture. mode must be CaptureModeUnspecified for
// display/ring/assistant captures and explicit for touch captures.
func (c *Client) OpenCapture(ctx context.Context, resourceID, presentationID string,
	mode CaptureMode) (*Capture, error) {
	ctx = orBackground(ctx)
	request := struct {
		ResourceID     string      `json:"resourceId,omitempty"`
		PresentationID string      `json:"presentationId,omitempty"`
		TouchMode      CaptureMode `json:"touchMode,omitempty"`
	}{ResourceID: resourceID, PresentationID: presentationID, TouchMode: mode}

	call, err := c.startCall(ctx, "", routingService, "Capture", callKindServerStream, 0)
	if err != nil {
		return nil, err
	}
	if err := call.sendJSON(request); err != nil {
		call.release()
		return nil, err
	}
	if err := call.halfClose(); err != nil {
		call.release()
		return nil, err
	}
	payload, err := call.recvJSONTimeout(timeoutMillis(ctx, 10*time.Second))
	if err != nil {
		call.release()
		return nil, err
	}
	var event captureEventJSON
	if err := unmarshalProtoJSON(payload, &event); err != nil {
		call.release()
		return nil, err
	}
	if event.Granted == nil || event.Granted.Capture == nil {
		call.release()
		return nil, fmt.Errorf("capsula: capture was not granted")
	}

	cap := &Capture{
		call:   call,
		info:   *event.Granted.Capture,
		events: make(chan CaptureEvent, 8),
		done:   make(chan struct{}),
	}
	if cap.info.Selected {
		info := cap.info
		cap.events <- CaptureEvent{Selected: true, Reason: "granted", Info: &info}
	}
	go cap.readLoop()
	return cap, nil
}

func (c *Capture) readLoop() {
	defer close(c.events)
	defer close(c.done)
	defer c.call.release()
	for {
		payload, err := c.call.recvJSON()
		if err != nil {
			return
		}
		var event captureEventJSON
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			continue
		}
		if event.Changed == nil {
			continue
		}
		c.mu.Lock()
		info := c.info
		info.Selected = event.Changed.Selected
		c.mu.Unlock()
		select {
		case c.events <- CaptureEvent{Selected: event.Changed.Selected,
			Reason: event.Changed.Reason, Info: &info}:
		default:
		}
	}
}

// ID is the framework capture ID.
func (c *Capture) ID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.CaptureID
}

// Info is a copy of the latest capture description.
func (c *Capture) Info() CaptureInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Events reports selection changes; it closes when the capture ends.
func (c *Capture) Events() <-chan CaptureEvent { return c.events }

// Done closes when the capture ends.
func (c *Capture) Done() <-chan struct{} { return c.done }

// Close releases the capture. It is idempotent and bounded.
func (c *Capture) Close() {
	c.once.Do(func() {
		_ = c.call.release()
		select {
		case <-c.done:
		case <-time.After(time.Second):
		}
	})
}
