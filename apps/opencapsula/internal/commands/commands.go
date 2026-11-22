// Package commands maps OpenCapsule actions onto the framework's audio and
// Bluetooth-media surfaces. The protocol carries percent volumes while the
// framework persists a 0..1 control position; the conversion lives here so
// every command reports the same level.
package commands

import (
	"fmt"
	"math"

	"capsulad/sdk/capsulad"
)

// Action names announced to the server. Keep this list identical to
// Hello.capabilities: an action that is not announced must not be executed,
// and an announced action has a handler.
const (
	ActionVolumeGet       = "volume.get"
	ActionMediaControl    = "media.control"
	ActionMediaStatus     = "media.status"
	ActionConversationEnd = "conversation.end"
)

// Capabilities is the exact Hello.capabilities list. Master-volume writes and
// Bluetooth discoverability are administrator-only framework operations, so
// they are not announced to a supervised assistant.
var Capabilities = []string{
	ActionVolumeGet,
	ActionMediaControl,
	ActionMediaStatus,
	ActionConversationEnd,
}

// Media operations accepted by media.control.
const (
	MediaPlay     = "play"
	MediaPause    = "pause"
	MediaToggle   = "toggle"
	MediaNext     = "next"
	MediaPrevious = "previous"
)

// VolumePercent converts the framework's 0..1 control position to the 0..100
// level the protocol reports.
func VolumePercent(master float32) int {
	value := float64(master)
	if math.IsNaN(value) || value <= 0 {
		return 0
	}
	if value >= 1 {
		return 100
	}
	return int(math.Round(value * 100))
}

// BluetoothCommand maps a media.control operation onto the AVRCP command the
// framework exposes. stop has no AVRCP equivalent in this SDK and is refused
// rather than silently treated as pause.
func BluetoothCommand(operation string) (capsulad.BluetoothMediaCommand, error) {
	switch operation {
	case MediaPlay:
		return capsulad.BluetoothMediaPlay, nil
	case MediaPause:
		return capsulad.BluetoothMediaPause, nil
	case MediaToggle:
		return capsulad.BluetoothMediaToggle, nil
	case MediaNext:
		return capsulad.BluetoothMediaNext, nil
	case MediaPrevious:
		return capsulad.BluetoothMediaPrevious, nil
	case "stop":
		return capsulad.BluetoothMediaCommandUnspecified,
			fmt.Errorf("media.control stop is not supported by the Bluetooth AVRCP bridge")
	default:
		return capsulad.BluetoothMediaCommandUnspecified,
			fmt.Errorf("unknown media operation %q", operation)
	}
}

// VolumeData is the result payload for volume.get. The framework has no output
// mute, so muted is always false; microphone mute is a capture property and is
// reported elsewhere.
func VolumeData(state capsulad.AudioState) map[string]any {
	return map[string]any{
		"level": VolumePercent(state.MasterVolume),
		"muted": false,
	}
}

// MediaData is the result payload for media.status and media.control. The
// source is always the Bluetooth AVRCP source; present=false is the honest
// answer when no phone or player is connected.
func MediaData(state capsulad.BluetoothMediaState) map[string]any {
	playing := state.Playing
	if state.DirectionKnown {
		playing = state.DirectionPlaying
	}
	if state.ObservedPlaying {
		playing = true
	}
	return map[string]any{
		"present":     state.Present,
		"playing":     playing,
		"title":       state.Title,
		"artist":      state.Artist,
		"album":       state.Album,
		"device_name": state.DeviceName,
		"source":      "bluetooth",
		"detail":      state.Detail,
	}
}
