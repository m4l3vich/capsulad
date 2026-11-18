package daemon

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/bluetooth"
)

type bluetoothMediaService struct {
	frameworkv1.UnimplementedBluetoothMediaServiceServer
	d *Daemon
}

func bluetoothMediaProto(m bluetooth.MediaState) *frameworkv1.BluetoothMediaState {
	return &frameworkv1.BluetoothMediaState{
		Present: m.Present, Playing: m.Playing,
		DeviceAddress: m.DeviceAddress, DeviceName: m.DeviceName,
		Title: m.Title, Artist: m.Artist, Album: m.Album,
		DurationMs: m.DurationMS, PositionMs: m.PositionMS,
		Revision: m.Revision, Detail: m.Detail,
	}
}

// bluetoothMediaAppView removes the connected source's address and name before
// a supervised app sees the state. Transport state and now-playing metadata are
// needed by a media provider/UI; the speaker's peer address is not.
func bluetoothMediaAppView(state *frameworkv1.BluetoothMediaState) *frameworkv1.BluetoothMediaState {
	if state == nil {
		return nil
	}
	out := proto.Clone(state).(*frameworkv1.BluetoothMediaState)
	out.DeviceAddress, out.DeviceName = "", ""
	return out
}

func (s *bluetoothMediaService) mediaView(ctx context.Context, state *frameworkv1.BluetoothMediaState) *frameworkv1.BluetoothMediaState {
	if appID, isApp := s.d.applicationCaller(ctx); isApp {
		s.d.logs.Debugf("bluetooth", appID, "media state redacted for an application caller")
		return bluetoothMediaAppView(state)
	}
	return state
}

// bluetoothPlaybackDirection resolves the displayed source's playback direction
// from the manager that owns the evidence. It is the single authority for that
// question: the manager's own toggle resolves the direction the same way, so a
// consumer that shows this value (the Web UI's now-playing transport buttons)
// can never disagree with the direction the next physical key press takes.
// A manager that cannot resolve one publishes its exact state instead (the
// simulator), which is not an inference that could be inverted.
func (d *Daemon) bluetoothPlaybackDirection(ctx context.Context, state bluetooth.MediaState) bluetooth.Direction {
	if d.bt == nil || !state.Present {
		return bluetooth.Direction{}
	}
	if resolver, ok := d.bt.(bluetooth.DirectionResolver); ok {
		direction, err := resolver.PlaybackDirection(ctx)
		if err == nil && direction.Present {
			return direction
		}
		if err != nil {
			d.logs.Debugf("bluetooth", "", "playback direction: %v", err)
		}
	}
	return bluetooth.Direction{Present: true, Playing: state.Playing, Evidence: bluetooth.EvidenceStatus}
}

// stateProto renders one manager state for a caller, including the resolved
// playback direction. `playing` stays the source-published AVRCP status; a
// source exposing only MediaControl1 publishes none, which is why the direction
// travels separately.
func (s *bluetoothMediaService) stateProto(ctx context.Context, state bluetooth.MediaState) *frameworkv1.BluetoothMediaState {
	out := bluetoothMediaProto(state)
	if direction := s.d.bluetoothPlaybackDirection(ctx, state); direction.Present {
		out.DirectionKnown, out.DirectionPlaying = true, direction.Playing
		// A now-playing surface must not treat a command this daemon just sent
		// as observed playback. Direction memory exists to make the next
		// toggle invert; the source-published status, transport state and sink
		// receipts are the evidence that playback is actually happening. A
		// MediaControl1-only source has no status, so receipt/transport
		// evidence is what keeps it visible while it is genuinely playing.
		// When a direction is resolvable, its evidence wins even over a cached
		// `playing`: a stale peer status that still says playing after this
		// daemon paused the source is not observed playback.
		observed := direction.Playing && direction.Evidence != bluetooth.EvidenceMemory
		out.ObservedPlaying = state.Present && observed
	} else {
		// A backend that publishes only its exact state has no richer evidence;
		// its `playing` is the observation.
		out.ObservedPlaying = state.Present && state.Playing
	}
	return out
}

func bluetoothStatusError(err error) error {
	switch {
	case errors.Is(err, bluetooth.ErrNoSource), errors.Is(err, bluetooth.ErrUnsupported):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, bluetooth.ErrUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (s *bluetoothMediaService) GetState(ctx context.Context, _ *frameworkv1.GetBluetoothMediaStateRequest) (*frameworkv1.BluetoothMediaState, error) {
	if s.d.bt == nil {
		return nil, status.Error(codes.Unavailable, "Bluetooth is unavailable on this platform")
	}
	state, err := s.d.bt.Media(ctx)
	if err != nil {
		return nil, bluetoothStatusError(err)
	}
	return s.mediaView(ctx, s.stateProto(ctx, state)), nil
}

// WatchState sends the current state, then one message per change. Connect and
// disconnect also produce a media event from the manager.
func (s *bluetoothMediaService) WatchState(_ *frameworkv1.WatchBluetoothMediaStateRequest, stream frameworkv1.BluetoothMediaService_WatchStateServer) error {
	if s.d.bt == nil {
		return status.Error(codes.Unavailable, "Bluetooth is unavailable on this platform")
	}
	ctx := stream.Context()
	// Subscribe before reading the snapshot. A change between the read and the
	// subscription would otherwise be lost and the watcher would show a stale
	// source forever; events already reflected in the snapshot are skipped by
	// revision below.
	events := s.d.bt.Watch(ctx)
	initial, err := s.d.bt.Media(ctx)
	if err != nil {
		return bluetoothStatusError(err)
	}
	appScoped := false
	if _, isApp := s.d.applicationCaller(ctx); isApp {
		appScoped = true
	}
	sendState := func(state *frameworkv1.BluetoothMediaState) error {
		if appScoped {
			state = bluetoothMediaAppView(state)
		}
		return stream.Send(state)
	}
	if err := sendState(s.stateProto(ctx, initial)); err != nil {
		return err
	}
	last := initial.Revision
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			// Connect/disconnect events carry the post-change state; the
			// manager may also emit a media event with the same revision.
			// Revisions only grow, so an event queued before the snapshot is
			// old state and is skipped too.
			if ev.Media.Revision <= last {
				continue
			}
			last = ev.Media.Revision
			if err := sendState(s.stateProto(ctx, ev.Media)); err != nil {
				return err
			}
		}
	}
}

func (s *bluetoothMediaService) Command(ctx context.Context, req *frameworkv1.BluetoothMediaCommandRequest) (*frameworkv1.BluetoothMediaState, error) {
	if s.d.bt == nil {
		return nil, status.Error(codes.Unavailable, "Bluetooth is unavailable on this platform")
	}
	var cmd bluetooth.Command
	switch req.Command {
	case frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PLAY:
		cmd = bluetooth.CommandPlay
	case frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PAUSE:
		cmd = bluetooth.CommandPause
	case frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_TOGGLE:
		cmd = bluetooth.CommandToggle
	case frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_NEXT:
		cmd = bluetooth.CommandNext
	case frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PREVIOUS:
		cmd = bluetooth.CommandPrevious
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown Bluetooth media command")
	}
	if err := s.d.bt.Command(ctx, cmd); err != nil {
		return nil, bluetoothStatusError(err)
	}
	state, err := s.d.bt.Media(ctx)
	if err != nil {
		return nil, bluetoothStatusError(err)
	}
	return s.mediaView(ctx, s.stateProto(ctx, state)), nil
}
