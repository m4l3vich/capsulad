package ha

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Fixed entity slugs. They are part of the bridge's published interface: a
// changed slug changes an entity's topic and object id.
const (
	SlugRing           = "ring"
	SlugDisplay        = "display"
	SlugDisplayFace    = "display_face"
	SlugBluetoothState = "bluetooth_state"
	SlugBluetoothTitle = "bluetooth_title"
	SlugBluetoothPlay  = "bluetooth_play"
	SlugBluetoothPause = "bluetooth_pause"
	SlugBluetoothNext  = "bluetooth_next"
	SlugBluetoothPrev  = "bluetooth_previous"
	SlugTouch          = "touch"
	SlugUptime         = "uptime"
	SlugVolume         = "volume"
	SlugMicrophoneMute = "microphone_muted"
	SlugBridgeStatus   = "bridge_status"
	SlugRepublish      = "bridge_republish"
	SlugReconnect      = "bridge_reconnect"
	SlugRelease        = "bridge_release"
)

// Bridge action payloads: the MQTT button entities' command payloads.
const (
	PayloadPress = "PRESS"
)

// Availability payloads.
const (
	PayloadOnline  = "online"
	PayloadOffline = "offline"
)

// Switch and binary-sensor payloads.
const (
	PayloadOn  = "ON"
	PayloadOff = "OFF"
)

// Bluetooth media command payloads. The transport is exposed as four button
// entities because Home Assistant's MQTT integration has no media_player
// platform (it is not in its SUPPORTED_COMPONENTS and has no module); each
// button publishes one of these on its own command topic.
const (
	PayloadPlay     = "PLAY"
	PayloadPause    = "PAUSE"
	PayloadNext     = "NEXT"
	PayloadPrevious = "PREVIOUS"
)

// Bluetooth playback states published on the enum sensor. They are the sensor's
// declared options, so Home Assistant validates the value.
const (
	MediaStatePlaying = "playing"
	MediaStateIdle    = "idle"
	MediaStateOff     = "off"
)

// TitlesNone is published when the connected source reports no title. A sensor
// cannot carry an empty state, and an explicit token is easier to automate on
// than "unknown".
const TitlesNone = "none"

// Bridge status sensor states.
const (
	BridgeStatusOnline      = "online"
	BridgeStatusConnecting  = "connecting"
	BridgeStatusStopped     = "stopped"
	BridgeStatusErrorPrefix = "error"
)

// RGB is one 24-bit colour.
type RGB struct{ R, G, B uint8 }

// Preset is one built-in ring effect or segment face discovered from the
// framework. Params documents the preset's parameters for diagnostics; the
// bridge passes defaults through and lets the framework validate.
type Preset struct {
	ID          string
	Kind        string // "PRESET_RING_EFFECT" or "PRESET_SEGMENT_FACE"
	DisplayName string
	Description string
}

// KindRingEffect and KindSegmentFace mirror the framework's preset kinds.
const (
	KindRingEffect  = "PRESET_RING_EFFECT"
	KindSegmentFace = "PRESET_SEGMENT_FACE"
)

// Device is the Home Assistant device block built from the framework's device
// identity.
type Device struct {
	// ID is the device node id: the topic level and the Home Assistant
	// identifier. It is derived from the framework's device_id.
	ID string
	// NodeID is the sanitized topic level actually used.
	NodeID       string
	Name         string
	Model        string
	Manufacturer string
	// SWVersion combines the daemon and firmware versions.
	SWVersion string
	// HWVersion is the board revision and SoC.
	HWVersion string
}

// SensorInfo is one discovered framework sensor, with its unit already mapped
// to what Home Assistant accepts.
type SensorInfo struct {
	// ID is the framework sensor id, for example "sensor.ambient_light".
	ID string
	// Slug is the entity-language slug derived from ID.
	Slug string
	// Name is the display name derived from ID.
	Name string
	// FrameworkUnit is the unit SensorService reports, e.g. "celsius".
	FrameworkUnit string
	// Unit is the published unit ("°C", "lx", …), empty when the framework unit
	// has no Home Assistant equivalent and should not be shown.
	Unit string
	// DeviceClass is the Home Assistant device class, empty when unknown.
	DeviceClass string
	// StateClass enables long-term statistics for a unit/class pair Home
	// Assistant validates.
	StateClass bool
}

// Capabilities describes which physical surfaces this device actually
// advertises, so the bridge only publishes entities that exist.
type Capabilities struct {
	SegmentDisplay bool
	Ring           bool
	RingPixels     int
	RingRGB        bool
	Touch          bool
	TouchFull      bool
	TouchPartial   bool
	Bluetooth      bool
	Media          bool
}

// MediaState is the Bluetooth source state the bridge publishes.
type MediaState struct {
	Present  bool
	Playing  bool
	Title    string
	Artist   string
	Album    string
	Duration uint64 // milliseconds
	Position uint64 // milliseconds
	Detail   string
}

// State is one Home Assistant state value plus optional JSON attributes.
type State struct {
	Payload    string
	Attributes map[string]any
}

// FrameworkUnit describes how one framework sensor unit is published.
type FrameworkUnit struct {
	// Unit is the Home Assistant unit string; empty publishes no unit.
	Unit string
	// DeviceClass is the Home Assistant device class; empty publishes none.
	DeviceClass string
	// StateClass enables long-term statistics. It is only set for units whose
	// device class and unit pair is one Home Assistant accepts.
	StateClass bool
}

// DescribeUnit maps the framework's sensor unit vocabulary
// (internal/backend/rockchip: "celsius", "lux", "lux-raw") to Home Assistant.
//
// Home Assistant validates the pair: a device class restricts the allowed
// units, and `state_class: measurement` requires a unit it knows for
// statistics. A wrong pair is not a cosmetic problem — the sensor entity is
// rejected at setup. Anything unrecognised is therefore published with the
// framework's unit verbatim, no device class and no statistics, which Home
// Assistant always accepts.
func DescribeUnit(unit string) FrameworkUnit {
	switch unit {
	case "celsius":
		return FrameworkUnit{Unit: "°C", DeviceClass: "temperature", StateClass: true}
	case "fahrenheit":
		return FrameworkUnit{Unit: "°F", DeviceClass: "temperature", StateClass: true}
	case "kelvin":
		return FrameworkUnit{Unit: "K", DeviceClass: "temperature", StateClass: true}
	case "lux":
		return FrameworkUnit{Unit: "lx", DeviceClass: "illuminance", StateClass: true}
	default:
		// "lux-raw" is the uncalibrated illuminance reading: it is not lux, so
		// it gets no illuminance class and no statistics.
		return FrameworkUnit{Unit: unit}
	}
}

// SensorName turns a framework sensor id into a display name:
// "sensor.ambient_light" becomes "Ambient light".
func SensorName(id string) string {
	trimmed := id
	if idx := lastIndexByte(id, '.'); idx >= 0 {
		trimmed = id[idx+1:]
	}
	out := make([]rune, 0, len(trimmed))
	first := true
	for _, r := range strings.ToLower(trimmed) {
		if r == '_' || r == '-' || r == '.' {
			out = append(out, ' ')
			continue
		}
		if first && r >= 'a' && r <= 'z' {
			r = r - ('a' - 'A')
		}
		first = false
		out = append(out, r)
	}
	return string(out)
}

// SensorSlug turns a framework sensor id into a stable entity slug:
// "sensor.ambient_light" becomes "sensor_ambient_light".
func SensorSlug(id string) string {
	slug := Slug(id)
	if slug == "" {
		return "sensor"
	}
	return slug
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// MediaAttributes renders the JSON attributes for the Bluetooth state sensor.
func MediaAttributes(m MediaState) map[string]any {
	attrs := map[string]any{"state": MediaStatePayload(m)}
	if m.Title != "" {
		attrs["title"] = m.Title
	}
	if m.Artist != "" {
		attrs["artist"] = m.Artist
	}
	if m.Album != "" {
		attrs["album"] = m.Album
	}
	if m.Duration > 0 {
		attrs["duration_s"] = m.Duration / 1000
	}
	attrs["position_s"] = m.Position / 1000
	if m.Detail != "" {
		attrs["detail"] = m.Detail
	}
	return attrs
}

// MediaStatePayload maps the device state onto the enum sensor's vocabulary.
func MediaStatePayload(m MediaState) string {
	switch {
	case !m.Present:
		return MediaStateOff
	case m.Playing:
		return MediaStatePlaying
	default:
		return MediaStateIdle
	}
}

// MaxStateLength is Home Assistant's state length limit for a text sensor
// (mqtt/util.check_state_too_long); a longer state is replaced with "unknown"
// and logged, which is worse than a truncated one.
const MaxStateLength = 255

// MediaTitlePayload renders the title sensor's state: the track title, or an
// explicit token when the source reports none. A long title is truncated
// rune-safely; the untruncated value stays in the entity's attributes.
func MediaTitlePayload(m MediaState) string {
	if m.Title == "" {
		return TitlesNone
	}
	return truncateState(m.Title)
}

// truncateState shortens a state to MaxStateLength bytes on a rune boundary.
func truncateState(state string) string {
	if len(state) <= MaxStateLength {
		return state
	}
	limit := MaxStateLength - 3
	for limit > 0 && !utf8.RuneStart(state[limit]) {
		limit--
	}
	return state[:limit] + "..."
}

// Errorf formats a bridge error with the package prefix.
func Errorf(format string, args ...any) error {
	return fmt.Errorf("ha: "+format, args...)
}
