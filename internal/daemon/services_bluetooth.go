package daemon

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	return bluetoothMediaProto(state), nil
}

// WatchState sends the current state, then one message per change. Connect and
// disconnect also produce a media event from the manager.
func (s *bluetoothMediaService) WatchState(_ *frameworkv1.WatchBluetoothMediaStateRequest, stream frameworkv1.BluetoothMediaService_WatchStateServer) error {
	if s.d.bt == nil {
		return status.Error(codes.Unavailable, "Bluetooth is unavailable on this platform")
	}
	ctx := stream.Context()
	initial, err := s.d.bt.Media(ctx)
	if err != nil {
		return bluetoothStatusError(err)
	}
	if err := stream.Send(bluetoothMediaProto(initial)); err != nil {
		return err
	}
	last := initial.Revision
	events := s.d.bt.Watch(ctx)
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
			if ev.Media.Revision == last {
				continue
			}
			last = ev.Media.Revision
			if err := stream.Send(bluetoothMediaProto(ev.Media)); err != nil {
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
	return bluetoothMediaProto(state), nil
}
