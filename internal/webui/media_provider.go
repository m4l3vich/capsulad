package webui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
)

// The daemon publishes no Bluetooth provider application. The connected AVRCP
// source appears in the browser's media surface as a framework-owned provider
// instead: it has no app identity, its players come from the public
// BluetoothMediaService on this same socket, and its commands are that service's
// Commands — exactly the path the physical circle key uses when no provider is
// playing or paused. An installed media provider keeps its app identity and is
// listed and commanded exactly as before.
const (
	// frameworkBluetoothPlayerID is the single player a connected source
	// exposes. It is stable for the life of the source session.
	frameworkBluetoothPlayerID = "bluetooth"
	frameworkBluetoothName     = "Bluetooth"
)

// frameworkMediaProvider returns the connected Bluetooth source as a media
// provider, or nil when no source is connected. Every failure that means "this
// platform has no Bluetooth source" yields nil: an absent manager, a stopped
// radio or a source that disconnected between the overview reads. A genuine
// failure is reported on the provider view instead of hiding the row, because
// the alternative is the defect this provider exists to remove — a speaker with
// a phone playing that shows no media at all.
func (s *Server) frameworkMediaProvider(ctx context.Context) *webv1.MediaProviderView {
	state, err := frameworkv1.NewBluetoothMediaServiceClient(s.Conn).GetState(ctx, &frameworkv1.GetBluetoothMediaStateRequest{})
	if err != nil {
		if unavailableMediaError(err) {
			return nil
		}
		return &webv1.MediaProviderView{
			Name:  frameworkBluetoothName,
			Kind:  webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA,
			Error: rpcMessage(err),
		}
	}
	if !state.GetPresent() {
		// Nothing is connected: no provider, no empty row.
		return nil
	}
	return &webv1.MediaProviderView{
		Name: frameworkBluetoothName,
		Kind: webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA,
		Players: []*webv1.MediaPlayerView{{
			PlayerId:      frameworkBluetoothPlayerID,
			PlaybackState: frameworkPlaybackState(state).String(),
			SupportedActions: mediaActions([]mediav1.MediaAction{
				mediav1.MediaAction_MEDIA_PLAY,
				mediav1.MediaAction_MEDIA_PAUSE,
				mediav1.MediaAction_MEDIA_SKIP_NEXT,
				mediav1.MediaAction_MEDIA_SKIP_PREVIOUS,
			}),
			AppName:    frameworkBluetoothName,
			SourceName: state.GetDeviceName(),
			Title:      state.GetTitle(),
			Artist:     state.GetArtist(),
			Album:      state.GetAlbum(),
			DurationMs: state.GetDurationMs(),
			PositionMs: state.GetPositionMs(),
			Revision:   state.GetRevision(),
		}},
	}
}

// frameworkPlaybackState maps the framework's resolved direction onto the
// standard media contract's playback states. `playing` is the source-published
// AVRCP status, which a source exposing only MediaControl1 never publishes; the
// resolved direction is what the manager's own toggle uses, so showing it keeps
// the browser's Play/Pause choice consistent with the physical key.
func frameworkPlaybackState(state *frameworkv1.BluetoothMediaState) mediav1.PlaybackState {
	playing := state.GetPlaying()
	if state.GetDirectionKnown() {
		playing = state.GetDirectionPlaying()
	}
	if playing {
		return mediav1.PlaybackState_PLAYBACK_PLAYING
	}
	return mediav1.PlaybackState_PLAYBACK_PAUSED
}

// unavailableMediaError reports whether an error means "no Bluetooth media
// source on this platform" rather than a failure of the framework surface.
func unavailableMediaError(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.Unimplemented, codes.FailedPrecondition, codes.NotFound:
		return true
	default:
		return false
	}
}

// frameworkBluetoothCommand maps one browser transport action onto the public
// BluetoothMediaService command. Explicit Play and Pause are used (not Toggle):
// the browser already knows the direction it displayed, so a stale peer status
// cannot invert it, and the service resolves the direction itself for anything
// that does need one.
func frameworkBluetoothCommand(action webv1.MediaAction) (frameworkv1.BluetoothMediaCommand, bool) {
	switch action {
	case webv1.MediaAction_MEDIA_ACTION_PLAY:
		return frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PLAY, true
	case webv1.MediaAction_MEDIA_ACTION_PAUSE:
		return frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PAUSE, true
	case webv1.MediaAction_MEDIA_ACTION_NEXT:
		return frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_NEXT, true
	case webv1.MediaAction_MEDIA_ACTION_PREVIOUS:
		return frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PREVIOUS, true
	default:
		return frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_COMMAND_UNSPECIFIED, false
	}
}

// opFrameworkMedia answers one browser command for the framework Bluetooth
// provider. It is separate from opMedia because there is no application to look
// up, authorize or route to: the command is the public BluetoothMediaService on
// this socket, the same surface the physical circle key uses.
func (s *Server) opFrameworkMedia(c *wsConn, reqID uint64, media *webv1.MediaCommand) {
	if playerID := strings.TrimSpace(media.GetPlayerId()); playerID != frameworkBluetoothPlayerID {
		c.sendError(reqID, errInvalid, fmt.Sprintf("unknown framework media player %q", playerID))
		return
	}
	command, ok := frameworkBluetoothCommand(media.GetAction())
	if !ok {
		c.sendError(reqID, errInvalid, "unknown media action")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	state, err := frameworkv1.NewBluetoothMediaServiceClient(s.Conn).Command(ctx,
		&frameworkv1.BluetoothMediaCommandRequest{Command: command})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	if s.Logs != nil {
		s.Logs.Debugf("webui", "", "Bluetooth media %s accepted for %s (playing=%t)",
			command, state.GetDeviceName(), state.GetDirectionPlaying())
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Media{Media: &webv1.MediaResult{Accepted: true}}})
}
