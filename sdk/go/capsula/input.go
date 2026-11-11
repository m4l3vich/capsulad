package capsula

import (
	"context"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

// InputStream delivers touch events for one selected capture.
type InputStream struct {
	events chan *frameworkv1.InputEvent
	done   chan struct{}
}

func (c *Client) WatchInput(ctx context.Context, captureID string) (*InputStream, error) {
	stream, err := frameworkv1.NewInputServiceClient(c.conn).WatchInput(ctx, &frameworkv1.WatchInputRequest{
		ResourceId: "touch.panel.main", CaptureId: captureID,
	})
	if err != nil {
		return nil, err
	}
	in := &InputStream{events: make(chan *frameworkv1.InputEvent, 16), done: make(chan struct{})}
	go func() {
		defer close(in.events)
		defer close(in.done)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case in.events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return in, nil
}

func (i *InputStream) Events() <-chan *frameworkv1.InputEvent { return i.events }
func (i *InputStream) Done() <-chan struct{}                  { return i.done }
