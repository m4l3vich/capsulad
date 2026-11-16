package capsulad

import (
	"context"
	"sync"
)

const bluetoothMediaService = "capsulad.framework.v1.BluetoothMediaService"

// BluetoothMediaState reads the current connected-source state.
func (c *Client) BluetoothMediaState(ctx context.Context) (BluetoothMediaState, error) {
	var state BluetoothMediaState
	err := c.unaryJSON(ctx, bluetoothMediaService, "GetState", struct{}{}, &state)
	return state, err
}

// BluetoothMediaCommand sends one AVRCP transport command.
func (c *Client) BluetoothMediaCommand(ctx context.Context,
	command BluetoothMediaCommand) (BluetoothMediaState, error) {
	var state BluetoothMediaState
	err := c.unaryJSON(ctx, bluetoothMediaService, "Command", struct {
		Command BluetoothMediaCommand `json:"command,omitempty"`
	}{Command: command}, &state)
	return state, err
}

// BluetoothMediaStream is the app-facing view of the connected source's AVRCP
// state: the current state first, then one value per change.
type BluetoothMediaStream struct {
	// C closes when the stream ends or Close is called.
	C <-chan BluetoothMediaState

	call *callHandle
	done chan struct{}
	stop chan struct{}
	once sync.Once
}

// WatchBluetoothMedia subscribes to connect/disconnect and track changes.
func (c *Client) WatchBluetoothMedia(ctx context.Context) (*BluetoothMediaStream, error) {
	ctx = orBackground(ctx)
	call, err := c.startCall(ctx, "", bluetoothMediaService, "WatchState", callKindServerStream, 0)
	if err != nil {
		return nil, err
	}
	if err := call.sendJSON(struct{}{}); err != nil {
		call.release()
		return nil, err
	}
	if err := call.halfClose(); err != nil {
		call.release()
		return nil, err
	}
	states := make(chan BluetoothMediaState, 8)
	stream := &BluetoothMediaStream{C: states, call: call, done: make(chan struct{}),
		stop: make(chan struct{})}
	go func() {
		defer close(states)
		defer close(stream.done)
		defer call.release()
		for {
			payload, err := call.recvJSON()
			if err != nil {
				return
			}
			var state BluetoothMediaState
			if err := unmarshalProtoJSON(payload, &state); err != nil {
				continue
			}
			select {
			case states <- state:
			case <-stream.stop:
				return
			case <-call.ctx.Done():
				return
			}
		}
	}()
	return stream, nil
}

// Done closes when the watch stream ends.
func (s *BluetoothMediaStream) Done() <-chan struct{} { return s.done }

// Close stops the stream. It is safe to call more than once.
func (s *BluetoothMediaStream) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		_ = s.call.release()
	})
}
