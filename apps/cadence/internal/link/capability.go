package link

import (
	"fmt"
	"strings"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
)

// The client is a speaker: it advertises the one capability it can honour,
// audio.output. The server needs that capability's state to choose the reply
// stream and to allow announcements; it does not need media.player, which is a
// separate media-session role this client does not implement.
const audioOutputCapability = "audio.output"

// The level is optional in our schema and omitted: the app cannot receive
// framework volume changes, and upstream treats a missing level as unknown
// (volume display and the model's volume note are omitted). Channels and
// maxBitrate are the values the server needs to encode reply/announcement
// audio, so they are always present.
const audioOutputStateSchema = `{"type":"object","properties":{"channels":{"type":"integer"},"maxBitrate":{"type":"integer"}},"required":["channels","maxBitrate"]}`

// The framework speaker endpoint accepts stereo PCM at 48 kHz. The app can
// decode both channel counts the server offers; 128 kbps is the upstream
// device maximum.
const (
	audioOutputChannels   = 2
	audioOutputMaxBitrate = 128000
)

func audioOutputDescriptor() *cadencev2.Descriptor {
	return &cadencev2.Descriptor{
		Id:      audioOutputCapability,
		Version: 1,
		// The app has no delay trim or screen volume mode to configure. An
		// empty schema keeps the dashboard from offering a setting the app
		// cannot apply.
		ConfigSchema: `{"type":"object","properties":{},"additionalProperties":false}`,
		StateSchema:  audioOutputStateSchema,
	}
}

func audioOutputState() string {
	return fmt.Sprintf(`{"channels":%d,"maxBitrate":%d}`, audioOutputChannels, audioOutputMaxBitrate)
}

// configApplied acknowledges a ConfigSet on the connection that carried it.
// The descriptor's empty config schema means only {} is applicable; another
// config is refused explicitly instead of being reported as applied.
func configApplied(cs *cadencev2.ConfigSet) *cadencev2.Envelope {
	code := ""
	switch {
	case cs.GetCapability() != audioOutputCapability:
		code = "unknown_capability"
	case strings.TrimSpace(cs.GetJson()) != "" && strings.TrimSpace(cs.GetJson()) != "{}":
		code = "unsupported"
	}
	return &cadencev2.Envelope{Body: &cadencev2.Envelope_ConfigApplied{
		ConfigApplied: &cadencev2.ConfigApplied{
			Capability: cs.GetCapability(), Generation: cs.GetGeneration(), ErrorCode: code,
		},
	}}
}
