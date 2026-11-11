package capsula

import (
	"context"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

// BluetoothMediaStream is the app-facing view of the connected audio source's
// AVRCP state. The first value is the current state, then one per change.
type BluetoothMediaStream struct {
	// C closes when the daemon stream ends or Close is called.
	C <-chan *frameworkv1.BluetoothMediaState

	cancel context.CancelFunc
}

// Close stops the stream. It is safe to call more than once.
func (s *BluetoothMediaStream) Close() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

// BluetoothMediaState reads the current connected-source state.
func (c *Client) BluetoothMediaState(ctx context.Context) (*frameworkv1.BluetoothMediaState, error) {
	return frameworkv1.NewBluetoothMediaServiceClient(c.conn).GetState(ctx, &frameworkv1.GetBluetoothMediaStateRequest{})
}

// WatchBluetoothMedia subscribes to connect/disconnect and track changes.
func (c *Client) WatchBluetoothMedia(ctx context.Context) (*BluetoothMediaStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := frameworkv1.NewBluetoothMediaServiceClient(c.conn).WatchState(streamCtx, &frameworkv1.WatchBluetoothMediaStateRequest{})
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan *frameworkv1.BluetoothMediaState, 8)
	go func() {
		defer close(out)
		defer cancel()
		for {
			state, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case out <- state:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	return &BluetoothMediaStream{C: out, cancel: cancel}, nil
}

// BluetoothMediaCommand sends one AVRCP transport command to the source.
func (c *Client) BluetoothMediaCommand(ctx context.Context, cmd frameworkv1.BluetoothMediaCommand) (*frameworkv1.BluetoothMediaState, error) {
	return frameworkv1.NewBluetoothMediaServiceClient(c.conn).Command(ctx, &frameworkv1.BluetoothMediaCommandRequest{Command: cmd})
}
