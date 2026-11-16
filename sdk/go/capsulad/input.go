package capsulad

import (
	"context"
	"sync"
)

const inputService = "capsulad.framework.v1.InputService"

// InputStream delivers touch input events for one selected capture.
type InputStream struct {
	events chan InputEvent
	done   chan struct{}
	stop   chan struct{}
	call   *callHandle
	once   sync.Once
}

// WatchInput subscribes to input for captureID.
func (c *Client) WatchInput(ctx context.Context, captureID string) (*InputStream, error) {
	ctx = orBackground(ctx)
	request := struct {
		ResourceID string `json:"resourceId,omitempty"`
		CaptureID  string `json:"captureId,omitempty"`
	}{ResourceID: "touch.panel.main", CaptureID: captureID}

	call, err := c.startCall(ctx, "", inputService, "WatchInput", callKindServerStream, 0)
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
	in := &InputStream{
		events: make(chan InputEvent, 16),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		call:   call,
	}
	go in.readLoop()
	return in, nil
}

func (in *InputStream) readLoop() {
	defer close(in.events)
	defer close(in.done)
	defer in.call.release()
	for {
		payload, err := in.call.recvJSON()
		if err != nil {
			return
		}
		var event InputEvent
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			continue
		}
		select {
		case in.events <- event:
		case <-in.stop:
			return
		case <-in.call.ctx.Done():
			return
		}
	}
}

// Events streams input events until the subscription ends.
func (in *InputStream) Events() <-chan InputEvent { return in.events }

// Done closes when the subscription ends.
func (in *InputStream) Done() <-chan struct{} { return in.done }

// Close cancels the subscription. It is idempotent.
func (in *InputStream) Close() error {
	var err error
	in.once.Do(func() {
		close(in.stop)
		err = in.call.release()
	})
	return err
}
