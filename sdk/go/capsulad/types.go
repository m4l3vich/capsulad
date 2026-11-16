package capsulad

import (
	"encoding/json"
	"fmt"
)

// Env is the launcher-assigned application identity plus the optional shared
// library override.
//
// Library is CAPSULAD_LIBRARY when the launcher injected an absolute shared
// library path; empty means load the platform SONAME (libcapsulad.so.1 on
// Linux, libcapsulad.1.dylib on Darwin). DeviceSerial is the host SoC serial
// from CAPSULAD_DEVICE_SERIAL, and is empty when the daemon could not read it.
type Env struct {
	Library      string
	Socket       string
	RunID        string
	AppID        string
	SDK          string
	DeviceSerial string
}

// Enums are plain uint8 values. Their JSON encoding uses the canonical
// protobuf enum names so native descriptor reflection can decode them; the
// decoder also accepts the numeric wire value.
type ActivationOrigin uint8

const (
	ActivationOriginUnspecified ActivationOrigin = 0
	ActivationOriginWakeword    ActivationOrigin = 1
	ActivationOriginTouch       ActivationOrigin = 2
	ActivationOriginAPI         ActivationOrigin = 3
)

var activationOriginNames = map[ActivationOrigin]string{
	ActivationOriginUnspecified: "ACTIVATION_ORIGIN_UNSPECIFIED",
	ActivationOriginWakeword:    "ACTIVATION_WAKEWORD",
	ActivationOriginTouch:       "ACTIVATION_TOUCH",
	ActivationOriginAPI:         "ACTIVATION_API",
}

func (o ActivationOrigin) String() string { return enumString(o, activationOriginNames) }
func (o ActivationOrigin) MarshalJSON() ([]byte, error) {
	return marshalEnum(o, activationOriginNames)
}
func (o *ActivationOrigin) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, activationOriginNames, o)
}

type SampleFormat uint8

const (
	SampleFormatUnspecified SampleFormat = 0
	SampleFormatPCM16LE     SampleFormat = 1
	SampleFormatPCMF32LE    SampleFormat = 2
)

var sampleFormatNames = map[SampleFormat]string{
	SampleFormatUnspecified: "SAMPLE_FORMAT_UNSPECIFIED",
	SampleFormatPCM16LE:     "PCM_S16LE",
	SampleFormatPCMF32LE:    "PCM_F32LE",
}

func (f SampleFormat) String() string { return enumString(f, sampleFormatNames) }
func (f SampleFormat) MarshalJSON() ([]byte, error) {
	return marshalEnum(f, sampleFormatNames)
}
func (f *SampleFormat) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, sampleFormatNames, f)
}

// CaptureMode selects how a touch capture treats system gestures. It must be
// CaptureModeUnspecified for display, ring and assistant captures; touch
// captures require an explicit mode.
type CaptureMode uint8

const (
	CaptureModeUnspecified     CaptureMode = 0
	CaptureModePartialGestures CaptureMode = 1
	CaptureModeFullRaw         CaptureMode = 2
)

var captureModeNames = map[CaptureMode]string{
	CaptureModeUnspecified:     "TOUCH_CAPTURE_MODE_UNSPECIFIED",
	CaptureModePartialGestures: "TOUCH_PARTIAL_GESTURES",
	CaptureModeFullRaw:         "TOUCH_FULL_RAW",
}

func (m CaptureMode) String() string { return enumString(m, captureModeNames) }
func (m CaptureMode) MarshalJSON() ([]byte, error) {
	return marshalEnum(m, captureModeNames)
}
func (m *CaptureMode) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, captureModeNames, m)
}

type PresetKind uint8

const (
	PresetKindUnspecified PresetKind = 0
	PresetKindRingEffect  PresetKind = 1
	PresetKindSegmentFace PresetKind = 2
)

var presetKindNames = map[PresetKind]string{
	PresetKindUnspecified: "PRESET_KIND_UNSPECIFIED",
	PresetKindRingEffect:  "PRESET_RING_EFFECT",
	PresetKindSegmentFace: "PRESET_SEGMENT_FACE",
}

func (k PresetKind) String() string { return enumString(k, presetKindNames) }
func (k PresetKind) MarshalJSON() ([]byte, error) {
	return marshalEnum(k, presetKindNames)
}
func (k *PresetKind) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, presetKindNames, k)
}

type PlaybackCommand uint8

const (
	PlaybackCommandUnspecified PlaybackCommand = 0
	PlaybackFlush              PlaybackCommand = 1
	PlaybackPause              PlaybackCommand = 2
	PlaybackResume             PlaybackCommand = 3
)

var playbackCommandNames = map[PlaybackCommand]string{
	PlaybackCommandUnspecified: "PLAYBACK_COMMAND_UNSPECIFIED",
	PlaybackFlush:              "PLAYBACK_FLUSH",
	PlaybackPause:              "PLAYBACK_PAUSE",
	PlaybackResume:             "PLAYBACK_RESUME",
}

func (c PlaybackCommand) String() string { return enumString(c, playbackCommandNames) }
func (c PlaybackCommand) MarshalJSON() ([]byte, error) {
	return marshalEnum(c, playbackCommandNames)
}
func (c *PlaybackCommand) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, playbackCommandNames, c)
}

type BluetoothMediaCommand uint8

const (
	BluetoothMediaCommandUnspecified BluetoothMediaCommand = 0
	BluetoothMediaPlay               BluetoothMediaCommand = 1
	BluetoothMediaPause              BluetoothMediaCommand = 2
	BluetoothMediaToggle             BluetoothMediaCommand = 3
	BluetoothMediaNext               BluetoothMediaCommand = 4
	BluetoothMediaPrevious           BluetoothMediaCommand = 5
)

var bluetoothMediaCommandNames = map[BluetoothMediaCommand]string{
	BluetoothMediaCommandUnspecified: "BLUETOOTH_MEDIA_COMMAND_UNSPECIFIED",
	BluetoothMediaPlay:               "BLUETOOTH_MEDIA_PLAY",
	BluetoothMediaPause:              "BLUETOOTH_MEDIA_PAUSE",
	BluetoothMediaToggle:             "BLUETOOTH_MEDIA_TOGGLE",
	BluetoothMediaNext:               "BLUETOOTH_MEDIA_NEXT",
	BluetoothMediaPrevious:           "BLUETOOTH_MEDIA_PREVIOUS",
}

func (c BluetoothMediaCommand) String() string {
	return enumString(c, bluetoothMediaCommandNames)
}
func (c BluetoothMediaCommand) MarshalJSON() ([]byte, error) {
	return marshalEnum(c, bluetoothMediaCommandNames)
}
func (c *BluetoothMediaCommand) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, bluetoothMediaCommandNames, c)
}

type TouchHoldMode uint8

const (
	TouchHoldModeUnspecified TouchHoldMode = 0
	TouchHoldSummon          TouchHoldMode = 1
	TouchHoldToTalk          TouchHoldMode = 2
)

var touchHoldModeNames = map[TouchHoldMode]string{
	TouchHoldModeUnspecified: "TOUCH_HOLD_MODE_UNSPECIFIED",
	TouchHoldSummon:          "TOUCH_HOLD_SUMMON",
	TouchHoldToTalk:          "TOUCH_HOLD_TO_TALK",
}

func (m TouchHoldMode) String() string { return enumString(m, touchHoldModeNames) }
func (m TouchHoldMode) MarshalJSON() ([]byte, error) {
	return marshalEnum(m, touchHoldModeNames)
}
func (m *TouchHoldMode) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, touchHoldModeNames, m)
}

type MutedIndicatorMode uint8

const (
	MutedIndicatorModeUnspecified MutedIndicatorMode = 0
	MutedIndicatorImmediate       MutedIndicatorMode = 1
	MutedIndicatorDelay           MutedIndicatorMode = 2
	MutedIndicatorUntilUnmuted    MutedIndicatorMode = 3
)

var mutedIndicatorModeNames = map[MutedIndicatorMode]string{
	MutedIndicatorModeUnspecified: "MUTED_INDICATOR_MODE_UNSPECIFIED",
	MutedIndicatorImmediate:       "MUTED_INDICATOR_IMMEDIATE",
	MutedIndicatorDelay:           "MUTED_INDICATOR_DELAY",
	MutedIndicatorUntilUnmuted:    "MUTED_INDICATOR_UNTIL_UNMUTED",
}

func (m MutedIndicatorMode) String() string { return enumString(m, mutedIndicatorModeNames) }
func (m MutedIndicatorMode) MarshalJSON() ([]byte, error) {
	return marshalEnum(m, mutedIndicatorModeNames)
}
func (m *MutedIndicatorMode) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, mutedIndicatorModeNames, m)
}

type AudioDirection uint8

const (
	AudioDirectionUnspecified AudioDirection = 0
	AudioDirectionCapture     AudioDirection = 1
	AudioDirectionPlayback    AudioDirection = 2
)

var audioDirectionNames = map[AudioDirection]string{
	AudioDirectionUnspecified: "AUDIO_DIRECTION_UNSPECIFIED",
	AudioDirectionCapture:     "AUDIO_CAPTURE",
	AudioDirectionPlayback:    "AUDIO_PLAYBACK",
}

func (d AudioDirection) String() string { return enumString(d, audioDirectionNames) }
func (d AudioDirection) MarshalJSON() ([]byte, error) {
	return marshalEnum(d, audioDirectionNames)
}
func (d *AudioDirection) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, audioDirectionNames, d)
}

func enumString[T ~uint8](v T, names map[T]string) string {
	if name, ok := names[v]; ok {
		return name
	}
	return fmt.Sprintf("enum(%d)", uint8(v))
}

func marshalEnum[T ~uint8](v T, names map[T]string) ([]byte, error) {
	if name, ok := names[v]; ok {
		return json.Marshal(name)
	}
	return json.Marshal(uint8(v))
}

func unmarshalEnum[T ~uint8](data []byte, names map[T]string, out *T) error {
	if len(data) > 0 && data[0] == '"' {
		var name string
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
		for value, candidate := range names {
			if candidate == name {
				*out = value
				return nil
			}
		}
		return fmt.Errorf("capsulad: unknown enum value %q", name)
	}
	var number uint64
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	if number > 255 {
		return fmt.Errorf("capsulad: enum value %d out of range", number)
	}
	*out = T(number)
	return nil
}

// RGB is one ring pixel in the application-facing color model.
type RGB struct{ R, G, B uint8 }

// MarshalJSON emits the framework's rgb pixel field names.
func (c RGB) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Red   uint32 `json:"red"`
		Green uint32 `json:"green"`
		Blue  uint32 `json:"blue"`
	}{uint32(c.R), uint32(c.G), uint32(c.B)})
}

// UnmarshalJSON accepts the framework's rgb pixel field names.
func (c *RGB) UnmarshalJSON(data []byte) error {
	var px struct {
		Red   uint32 `json:"red"`
		Green uint32 `json:"green"`
		Blue  uint32 `json:"blue"`
	}
	if err := json.Unmarshal(data, &px); err != nil {
		return err
	}
	if px.Red > 255 || px.Green > 255 || px.Blue > 255 {
		return fmt.Errorf("capsulad: rgb component out of range")
	}
	*c = RGB{R: uint8(px.Red), G: uint8(px.Green), B: uint8(px.Blue)}
	return nil
}
