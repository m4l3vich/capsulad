package ha

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

// maxCommandPayload bounds an inbound command payload. Home Assistant light
// commands are a few dozen bytes; a megabyte is a misconfiguration or an
// attack, and it is refused before it is parsed.
const maxCommandPayload = 4096

// LightCommand is one parsed Home Assistant light command (schema "json").
type LightCommand struct {
	// State is "ON", "OFF" or "" when the command does not change it.
	State string
	// HasBrightness and Brightness (0..255) are set when the command carries
	// a brightness.
	HasBrightness bool
	Brightness    uint8
	// HasColor and Color are set when the command carries an RGB colour.
	HasColor bool
	Color    RGB
	// HasEffect and Effect are set when the command names an effect. Home
	// Assistant sends the literal "None" to clear an effect.
	HasEffect bool
	Effect    string
}

// lightCommandJSON mirrors the MQTT light "json" schema command payload.
// Pointers distinguish an absent key from a zero value.
type lightCommandJSON struct {
	State      *string         `json:"state"`
	Brightness *float64        `json:"brightness"`
	Color      *lightColorJSON `json:"color"`
	Effect     *string         `json:"effect"`
	ColorMode  *string         `json:"color_mode"`
	Transition *float64        `json:"transition"`
	// Flash is Home Assistant's own option. The discovery payload declares
	// flash: false, so a well-formed client never sends it; an automation that
	// sets it anyway must not turn the whole command into an error, so it is
	// accepted and ignored (it is a duration in seconds).
	Flash *float64 `json:"flash"`
}

type lightColorJSON struct {
	R *float64 `json:"r"`
	G *float64 `json:"g"`
	B *float64 `json:"b"`
}

// EffectNone is the value Home Assistant sends to clear an effect.
const EffectNone = "None"

// ParseLightCommand decodes one light command strictly: every key is known,
// every value is in range, and an unknown key is an error rather than a silent
// no-op.
func ParseLightCommand(payload []byte) (LightCommand, error) {
	var out LightCommand
	if err := checkPayloadSize(payload); err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var raw lightCommandJSON
	if err := dec.Decode(&raw); err != nil {
		return out, fmt.Errorf("light command is not valid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return out, fmt.Errorf("light command has trailing content")
	}

	if raw.State != nil {
		switch *raw.State {
		case PayloadOn, PayloadOff:
			out.State = *raw.State
		default:
			return out, fmt.Errorf("light state %q is not %q or %q", *raw.State, PayloadOn, PayloadOff)
		}
	}
	if raw.Brightness != nil {
		brightness := math.Round(*raw.Brightness)
		if brightness < 0 || brightness > 255 || math.IsNaN(brightness) {
			return out, fmt.Errorf("light brightness %.3f is outside 0..255", *raw.Brightness)
		}
		out.HasBrightness = true
		out.Brightness = uint8(brightness)
	}
	if raw.Color != nil {
		r, err := channel("r", raw.Color.R)
		if err != nil {
			return out, err
		}
		g, err := channel("g", raw.Color.G)
		if err != nil {
			return out, err
		}
		b, err := channel("b", raw.Color.B)
		if err != nil {
			return out, err
		}
		out.HasColor = true
		out.Color = RGB{R: r, G: g, B: b}
	}
	if raw.Effect != nil {
		out.HasEffect = true
		out.Effect = *raw.Effect
	}
	if raw.ColorMode != nil {
		switch *raw.ColorMode {
		case "rgb", "onoff", "brightness":
		default:
			return out, fmt.Errorf("light colour mode %q is not supported", *raw.ColorMode)
		}
	}
	if raw.Transition != nil && (*raw.Transition < 0 || math.IsNaN(*raw.Transition)) {
		return out, fmt.Errorf("light transition must not be negative")
	}
	if raw.Flash != nil && (*raw.Flash < 0 || math.IsNaN(*raw.Flash)) {
		return out, fmt.Errorf("light flash duration must not be negative")
	}
	if out.State == "" && !out.HasBrightness && !out.HasColor && !out.HasEffect {
		return out, fmt.Errorf("light command carries no state, colour, brightness or effect")
	}
	return out, nil
}

func channel(name string, value *float64) (uint8, error) {
	if value == nil {
		return 0, fmt.Errorf("light colour is missing %q", name)
	}
	rounded := math.Round(*value)
	if math.IsNaN(rounded) || rounded < 0 || rounded > 255 {
		return 0, fmt.Errorf("light colour %s %.3f is outside 0..255", name, *value)
	}
	return uint8(rounded), nil
}

// ParseOnOff decodes a switch payload ("ON"/"OFF").
func ParseOnOff(payload []byte) (bool, error) {
	if err := checkPayloadSize(payload); err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(payload)) {
	case PayloadOn:
		return true, nil
	case PayloadOff:
		return false, nil
	default:
		return false, fmt.Errorf("payload %q is not %q or %q", truncate(payload), PayloadOn, PayloadOff)
	}
}

// ParseFaceCommand validates a segment face command against the preset list
// the framework actually offers: an option that is not in the list is refused
// rather than forwarded to the device.
func ParseFaceCommand(payload []byte, options []Preset) (string, error) {
	if err := checkPayloadSize(payload); err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(payload))
	if id == "" {
		return "", fmt.Errorf("face command is empty")
	}
	for _, option := range options {
		if option.ID == id {
			return id, nil
		}
	}
	return "", fmt.Errorf("face %q is not one of the %d offered presets", truncate(payload), len(options))
}

// ParseEffectCommand validates a ring effect id, or reports that the effect
// should be cleared.
func ParseEffectCommand(payload []byte, options []Preset) (string, bool, error) {
	if err := checkPayloadSize(payload); err != nil {
		return "", false, err
	}
	value := strings.TrimSpace(string(payload))
	if value == "" || strings.EqualFold(value, EffectNone) {
		return "", true, nil
	}
	for _, option := range options {
		if option.ID == value {
			return value, false, nil
		}
	}
	return "", false, fmt.Errorf("effect %q is not one of the %d offered presets", truncate(payload), len(options))
}

// ParseMediaCommand validates a Bluetooth transport payload, which the four
// transport buttons publish.
func ParseMediaCommand(payload []byte) (string, error) {
	if err := checkPayloadSize(payload); err != nil {
		return "", err
	}
	value := strings.ToUpper(strings.TrimSpace(string(payload)))
	switch value {
	case PayloadPlay, PayloadPause, PayloadNext, PayloadPrevious:
		return value, nil
	default:
		return "", fmt.Errorf("media command %q is not PLAY, PAUSE, NEXT or PREVIOUS", truncate(payload))
	}
}

// ParseButtonCommand validates a button press payload.
func ParseButtonCommand(payload []byte) error {
	if err := checkPayloadSize(payload); err != nil {
		return err
	}
	if strings.TrimSpace(string(payload)) != PayloadPress {
		return fmt.Errorf("button payload %q is not %q", truncate(payload), PayloadPress)
	}
	return nil
}

func checkPayloadSize(payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("command payload is empty")
	}
	if len(payload) > maxCommandPayload {
		return fmt.Errorf("command payload of %d bytes exceeds %d", len(payload), maxCommandPayload)
	}
	return nil
}

// truncate bounds a value echoed into an error message, so a hostile payload
// cannot flood the log.
func truncate(payload []byte) string {
	const limit = 48
	if len(payload) <= limit {
		return string(payload)
	}
	return string(payload[:limit]) + "…"
}
