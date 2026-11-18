package webui

import (
	"context"
	"fmt"
	"strings"
	"time"

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
// commanded exactly as before; the overview lists it only for its playing
// players.
const (
	// frameworkBluetoothPlayerID is the single player a connected source
	// exposes. It is stable for the life of the source session.
	frameworkBluetoothPlayerID = "bluetooth"
	frameworkBluetoothName     = "Bluetooth"
)

// frameworkMediaProvider returns the connected Bluetooth source as a media
// provider only while playback is observed, and nil otherwise. A source that
// is connected but idle, paused or stopped is provider discovery, not Now
// Playing, so it adds no row; an unreadable state has no observed playback to
// claim either. Both are omitted instead of rendering an error or an empty
// card. `observed_playing` excludes the framework's command-memory direction,
// so an acknowledged Play that the peer did not act on cannot create a row;
// receipt and transport evidence keep a MediaControl1-only source visible
// while it is genuinely playing.
func (s *Server) frameworkMediaProvider(ctx context.Context) *webv1.MediaProviderView {
	state, err := frameworkv1.NewBluetoothMediaServiceClient(s.Conn).GetState(ctx, &frameworkv1.GetBluetoothMediaStateRequest{})
	if err != nil || !state.GetPresent() || !state.GetObservedPlaying() {
		return nil
	}
	return &webv1.MediaProviderView{
		Name: frameworkBluetoothName,
		Kind: webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA,
		Players: []*webv1.MediaPlayerView{{
			PlayerId:      frameworkBluetoothPlayerID,
			PlaybackState: mediav1.PlaybackState_PLAYBACK_PLAYING.String(),
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
func (s *Server) opFrameworkMedia(c opSink, reqID uint64, media *webv1.MediaCommand) {
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
