package daemon

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
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
	return s.mediaView(ctx, bluetoothMediaProto(state)), nil
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
	if err := sendState(bluetoothMediaProto(initial)); err != nil {
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
			if err := sendState(bluetoothMediaProto(ev.Media)); err != nil {
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
	return s.mediaView(ctx, bluetoothMediaProto(state)), nil
}
