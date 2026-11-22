// Package protocol is the OpenCapsule v1 device stream: JSON text messages on
// the websocket control channel and raw PCM binary frames for audio.
package protocol

import (
	"encoding/json"
	"fmt"
)

// Version is the only wire protocol this app speaks.
const Version = 1

// Type discriminates the payload of a Message.
type Type string

const (
	TypeHello         Type = "hello"
	TypeReady         Type = "ready"
	TypeTurnStart     Type = "turn_start"
	TypeTurnEnd       Type = "turn_end"
	TypeTurnCancel    Type = "turn_cancel"
	TypeListenEnd     Type = "listen_end"
	TypeTranscript    Type = "transcript"
	TypeCommand       Type = "command"
	TypeCommandResult Type = "command_result"
	TypeInterrupted   Type = "interrupted"
	TypeTurnComplete  Type = "turn_complete"
	TypeError         Type = "error"
)

// Encoding is a raw PCM encoding. The protocol only defines LINEAR16 for the
// device path in this app.
type Encoding string

// EncodingLinear16 is signed 16-bit little-endian PCM.
const EncodingLinear16 Encoding = "LINEAR16"

// AudioFormat describes a raw PCM stream.
type AudioFormat struct {
	Encoding        Encoding `json:"encoding"`
	SampleRateHertz int      `json:"sample_rate_hertz"`
	Channels        int      `json:"channels"`
}

// DeviceInputFormat is the microphone format this app sends.
var DeviceInputFormat = AudioFormat{Encoding: EncodingLinear16, SampleRateHertz: 16000, Channels: 1}

// Validate reports whether this app can carry the format.
func (f AudioFormat) Validate() error {
	if f.Encoding != EncodingLinear16 {
		return fmt.Errorf("unsupported encoding %q; only LINEAR16 is supported", f.Encoding)
	}
	if f.SampleRateHertz != 16000 && f.SampleRateHertz != 24000 {
		return fmt.Errorf("unsupported sample rate %d Hz; only 16000 and 24000 are supported", f.SampleRateHertz)
	}
	if f.Channels != 1 {
		return fmt.Errorf("unsupported channel count %d; only mono is supported", f.Channels)
	}
	return nil
}

// BytesPerSecond is the byte rate of a 16-bit mono stream.
func (f AudioFormat) BytesPerSecond() int {
	return f.SampleRateHertz * f.Channels * 2
}

// Hello identifies the device and the actions it can execute.
type Hello struct {
	ProtocolVersion int         `json:"protocol_version"`
	DeviceID        string      `json:"device_id"`
	InputFormat     AudioFormat `json:"input_format"`
	Capabilities    []string    `json:"capabilities"`
	Language        string      `json:"language,omitempty"`
	Location        string      `json:"location,omitempty"`
	TimeZone        string      `json:"timezone,omitempty"`
	AgentVersion    string      `json:"agent_version,omitempty"`
}

// Ready is the server's acceptance of Hello.
type Ready struct {
	SessionID    string      `json:"session_id"`
	OutputFormat AudioFormat `json:"output_format"`
	ServerVAD    bool        `json:"server_vad,omitempty"`
}

// TranscriptRole identifies the author of a transcript fragment.
type TranscriptRole string

const (
	RoleUser  TranscriptRole = "user"
	RoleModel TranscriptRole = "model"
)

// Transcript is a piece of recognized or generated text.
type Transcript struct {
	Role  TranscriptRole `json:"role"`
	Text  string         `json:"text"`
	Final bool           `json:"final,omitempty"`
}

// Command asks the device to execute an action. Args is schema-less JSON.
type Command struct {
	ID     string         `json:"id"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args,omitempty"`
}

// String returns a named string argument.
func (c *Command) String(key string) (string, bool) {
	if c == nil || c.Args == nil {
		return "", false
	}
	value, ok := c.Args[key].(string)
	return value, ok
}

// CommandResult answers a Command.
type CommandResult struct {
	ID    string         `json:"id"`
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error string         `json:"error,omitempty"`
}

// Error is a non-fatal server error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Message is the envelope of every JSON text frame. Exactly one payload
// matching Type is expected to be set.
type Message struct {
	Type    Type   `json:"type"`
	TurnID  string `json:"turn_id,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`

	Hello         *Hello         `json:"hello,omitempty"`
	Ready         *Ready         `json:"ready,omitempty"`
	Transcript    *Transcript    `json:"transcript,omitempty"`
	Command       *Command       `json:"command,omitempty"`
	CommandResult *CommandResult `json:"command_result,omitempty"`
	Error         *Error         `json:"error,omitempty"`
}

// Validate checks that the payload matches the message type.
func (m *Message) Validate() error {
	if m == nil {
		return fmt.Errorf("nil message")
	}
	var ok bool
	switch m.Type {
	case TypeHello:
		ok = m.Hello != nil
	case TypeReady:
		ok = m.Ready != nil
	case TypeTranscript:
		ok = m.Transcript != nil
	case TypeCommand:
		ok = m.Command != nil && m.Command.ID != "" && m.Command.Action != ""
	case TypeCommandResult:
		ok = m.CommandResult != nil && m.CommandResult.ID != ""
	case TypeError:
		ok = m.Error != nil
	case TypeTurnStart, TypeTurnEnd, TypeTurnCancel, TypeListenEnd, TypeInterrupted, TypeTurnComplete:
		ok = true
	default:
		return fmt.Errorf("unknown message type %q", m.Type)
	}
	if !ok {
		return fmt.Errorf("message of type %q has no payload", m.Type)
	}
	return nil
}

// Encode marshals and validates one message.
func Encode(m *Message) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", m.Type, err)
	}
	return payload, nil
}

// Decode parses and validates one text frame.
func Decode(payload []byte) (*Message, error) {
	var message Message
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	if err := message.Validate(); err != nil {
		return nil, err
	}
	return &message, nil
}
