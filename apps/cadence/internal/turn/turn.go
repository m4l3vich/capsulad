// Package turn runs one assistant activation at a time against the Cadence
// device link: it opens the visible capture indication, captures and Opus
// encodes the microphone into the session's uplink, plays the server's reply
// streams, honors the follow-up window, and releases every resource on
// completion, dismissal, cancellation, link loss or error.
//
// The framework delivers activations; this package never touches the network
// directly. Audio, indication, codecs and the link are seams so the state
// machine is exercised against fakes without the framework runtime.
package turn

import (
	"context"
	"log"
	"time"

	"capsulad/sdk/capsulad"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/settings"
)

// Sender is the link's outbound surface.
type Sender interface {
	Send(env *cadencev2.Envelope) error
	Connected() bool
}

// MicStream is one framework microphone capture.
type MicStream interface {
	Chunks() <-chan capsulad.MicChunk
	Close() error
}

// Playback is one framework playback stream. Close drains queued audio;
// Abort discards it and unblocks writers.
type Playback interface {
	Write(pcm []byte) error
	Close() error
	Abort() error
}

// Conversation is the framework's ducking lifetime for one activation.
type Conversation interface {
	ID() string
	Close()
}

// Audio is the framework audio surface the turn uses.
type Audio interface {
	OpenMic(ctx context.Context, activationID, conversationID string) (MicStream, error)
	OpenPlayback(ctx context.Context, endpoint string, format capsulad.AudioFormat, conversationID string) (Playback, error)
	BeginConversation(ctx context.Context, activationID string) (Conversation, error)
}

// Encoder encodes one 20 ms 16 kHz mono frame to one Opus packet.
type Encoder interface {
	Encode(pcm []byte) ([]byte, error)
	Close()
}

// Decoder decodes one reply stream's Opus packets to 48 kHz PCM.
type Decoder interface {
	Decode(packet []byte) ([]byte, error)
	Close()
}

// Codecs opens the uplink encoder and one decoder per reply stream.
type Codecs interface {
	NewEncoder() (Encoder, error)
	NewDecoder(channels int) (Decoder, error)
}

// Indicator is the visible capture indication and turn state on the ring. Its
// implementation owns the presentation; the turn only reports state. It must
// be open before the microphone is opened: the framework withholds capture
// without a visible, non-blank indication.
type Indicator interface {
	Open(ctx context.Context) error
	// EnterListen sets the listening phase; Level only modulates the measured
	// brightness within it, so a still-open microphone during the reply cannot
	// repaint the thinking/speaking state.
	EnterListen()
	Level(level float64)
	Thinking()
	Speaking()
	Failed()
	Close()
}

// Trigger is one queued activation.
type Trigger struct {
	ActivationID string
	Origin       capsulad.ActivationOrigin
	ModelID      string
	HoldToTalk   bool
	// Expires bounds the turn's validity from QueuedAt; zero means unbounded.
	Expires  time.Duration
	QueuedAt time.Time
}

// Defaults mirror cadence-device's own bounds where they exist.
const (
	DefaultNoSpeechTimeout = 8 * time.Second
	DefaultListenTimeout   = 30 * time.Second
	DefaultStreamQueue     = 32
	// maxFollowUpWindow is the server's own bound on a follow-up window.
	maxFollowUpWindow = 15 * time.Second
	// maxTailDrain bounds how long the conversation (and its duck) outlives a
	// finished turn while the reply's tail drains.
	maxTailDrain = 5 * time.Second
	// pushTimeout bounds how long the controller waits for room in a slow reply
	// stream before dropping the packet as lost.
	pushTimeout = 500 * time.Millisecond
)

// Deps are the controller's collaborators.
type Deps struct {
	Link     Sender
	Audio    Audio
	Ring     Indicator
	Codecs   Codecs
	Log      *log.Logger
	Settings func() settings.Config
	NewID    func() (string, error)
	Now      func() time.Time

	NoSpeechTimeout time.Duration
	ListenTimeout   time.Duration
	StreamQueue     int
}

// TriggerOrigin maps a framework activation origin to the Cadence trigger.
//
// cadence.v2 vouches a turn as the owner's only for TRIGGER_WAKE and
// TRIGGER_BUTTON; the server grants volume, alarm and integration writes on
// that basis. A capsulad ACTIVATION_API is a daemon/automation trigger and is
// not evidence that the owner spoke or pressed anything, so it is refused:
// sending BUTTON would fabricate a physical press and escalate an unattended
// trigger to owner privileges. The refusal is logged with an actionable line;
// a wake word or a talk-key press is the supported path.
func TriggerOrigin(o capsulad.ActivationOrigin) (cadencev2.Trigger, bool) {
	switch o {
	case capsulad.ActivationOriginWakeword:
		return cadencev2.Trigger_TRIGGER_WAKE, true
	case capsulad.ActivationOriginTouch:
		return cadencev2.Trigger_TRIGGER_BUTTON, true
	default:
		return cadencev2.Trigger_TRIGGER_UNSPECIFIED, false
	}
}
