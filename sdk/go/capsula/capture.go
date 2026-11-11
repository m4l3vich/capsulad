package capsula

import (
	"context"
	"fmt"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

type CaptureEvent struct {
	Selected bool
	Reason   string
}

type Capture struct {
	id       string
	resource string
	stream   frameworkv1.RoutingService_CaptureClient
	cancel   context.CancelFunc
	events   chan CaptureEvent
	done     chan struct{}
}

// OpenCapture acquires a runtime capture. mode must be UNSPECIFIED for
// display/ring/assistant captures and set for touch captures.
func (c *Client) OpenCapture(ctx context.Context, resourceID, presentationID string, mode frameworkv1.TouchCaptureMode) (*Capture, error) {
	runCtx, cancel := context.WithCancel(ctx)
	stream, err := frameworkv1.NewRoutingServiceClient(c.conn).Capture(runCtx, &frameworkv1.CaptureRequest{
		ResourceId: resourceID, PresentationId: presentationID, TouchMode: mode,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	cap := &Capture{resource: resourceID, stream: stream, cancel: cancel,
		events: make(chan CaptureEvent, 8), done: make(chan struct{})}
	ev, err := stream.Recv()
	if err != nil {
		cancel()
		return nil, err
	}
	granted := ev.GetGranted()
	if granted == nil {
		cancel()
		return nil, fmt.Errorf("capture was not granted")
	}
	cap.id = granted.Capture.CaptureId
	if granted.Capture.Selected {
		cap.events <- CaptureEvent{Selected: true, Reason: "granted"}
	}
	go func() {
		defer close(cap.events)
		defer close(cap.done)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			if ch := ev.GetChanged(); ch != nil {
				select {
				case cap.events <- CaptureEvent{Selected: ch.Selected, Reason: ch.Reason}:
				default:
				}
			}
		}
	}()
	return cap, nil
}

func (c *Capture) ID() string                  { return c.id }
func (c *Capture) Events() <-chan CaptureEvent { return c.events }

// Close releases the capture by ending its stream.
func (c *Capture) Close() {
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(time.Second):
	}
}

// UserSettings returns the device-wide settings snapshot.
func (c *Client) UserSettings(ctx context.Context) (*frameworkv1.UserSettings, uint64, error) {
	snap, err := frameworkv1.NewSettingsServiceClient(c.conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{})
	if err != nil {
		return nil, 0, err
	}
	return snap.Values, snap.Revision, nil
}

// AudioState returns the current mixer/mute state.
func (c *Client) AudioState(ctx context.Context) (*frameworkv1.AudioState, error) {
	return frameworkv1.NewAudioServiceClient(c.conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{})
}

// ListPresets discovers built-in effects/faces for a resource.
func (c *Client) ListPresets(ctx context.Context, resourceID string) ([]*frameworkv1.PresentationPresetInfo, error) {
	resp, err := frameworkv1.NewPresentationServiceClient(c.conn).ListPresets(ctx, &frameworkv1.ListPresentationPresetsRequest{ResourceId: resourceID})
	if err != nil {
		return nil, err
	}
	return resp.Presets, nil
}
