package capsula

// LaunchArgument is one manifest-typed launch argument.
type LaunchArgument struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// LaunchInvocation requests the running app to service a launch. The app
// reports the outcome with Client.NotifyInvocationResult.
type LaunchInvocation struct {
	ID        string           `json:"invocationId,omitempty"`
	Arguments []LaunchArgument `json:"arguments,omitempty"`
}

// SettingValue is one private application setting. ValueJSON is the raw
// JSON-encoded value; masked settings carry their unredacted text only on
// this app-private path.
type SettingValue struct {
	Key       string `json:"key,omitempty"`
	ValueJSON string `json:"valueJson,omitempty"`
}

// SettingsChanged reports an app-private settings snapshot.
type SettingsChanged struct {
	Revision uint64         `json:"revision,omitempty"`
	Values   []SettingValue `json:"values,omitempty"`
}

// ActionInvoked reports that the user pressed a manifest-declared action.
type ActionInvoked struct {
	ID       string `json:"actionInvocationId,omitempty"`
	ActionID string `json:"actionId,omitempty"`
}

// Activation is a framework wakeword, touch or API activation.
type Activation struct {
	ID                 string           `json:"activationId,omitempty"`
	Origin             ActivationOrigin `json:"origin,omitempty"`
	MatchedModelID     string           `json:"matchedModelId,omitempty"`
	MonotonicTimeNS    uint64           `json:"monotonicTimeNs,omitempty"`
	ExpiresMonotonicNS uint64           `json:"expiresMonotonicNs,omitempty"`
	// HoldToTalk is true for a touch activation when the device is in
	// hold-to-talk mode; the framework then sends InputReleased to end input.
	// It is false in summon mode, where the turn may outlive the hold.
	HoldToTalk bool `json:"holdToTalk,omitempty"`
}

// InputReleased reports the release in hold-to-talk mode.
type InputReleased struct {
	ActivationID string `json:"activationId,omitempty"`
}

// UserSettings is the device-wide settings snapshot.
type UserSettings struct {
	// DefaultAppByResource maps physical output resource IDs to app IDs.
	DefaultAppByResource     map[string]string        `json:"defaultAppByResource,omitempty"`
	DefaultAssistantAppID    string                   `json:"defaultAssistantAppId,omitempty"`
	Wakewords                []WakewordBinding        `json:"wakewords,omitempty"`
	WakewordEnabled          bool                     `json:"wakewordEnabled,omitempty"`
	DisplayBrightness        map[string]float32       `json:"displayBrightness,omitempty"`
	MasterVolume             float32                  `json:"masterVolume,omitempty"`
	MicrophoneMuted          bool                     `json:"microphoneMuted,omitempty"`
	TouchHoldMode            TouchHoldMode            `json:"touchHoldMode,omitempty"`
	MutedMicrophoneIndicator MutedMicrophoneIndicator `json:"mutedMicrophoneIndicator,omitempty"`
}

// WakewordBinding is one installed wake word's configuration. ModelID is the
// artifact id under <data-dir>/wakeword/models; AppID empty targets the selected
// assistant. An absent Wakewords list on UserSettings is the legacy state in
// which every installed word is live under WakewordEnabled.
type WakewordBinding struct {
	ModelID string `json:"modelId,omitempty"`
	AppID   string `json:"appId,omitempty"`
	Enabled bool   `json:"enabled,omitempty"`
}

// MutedMicrophoneIndicator controls only the visual muted indication.
type MutedMicrophoneIndicator struct {
	Mode    MutedIndicatorMode `json:"mode,omitempty"`
	DelayMS uint32             `json:"delayMs,omitempty"`
}

// AudioState is the system-owned mixer state.
type AudioState struct {
	MasterVolume             float32 `json:"masterVolume,omitempty"`
	MicrophoneMuted          bool    `json:"microphoneMuted,omitempty"`
	MicrophoneMuteIsHardware bool    `json:"microphoneMuteIsHardware,omitempty"`
}

// AudioFormat describes a PCM stream layout.
type AudioFormat struct {
	SampleFormat SampleFormat `json:"sampleFormat,omitempty"`
	SampleRateHz uint32       `json:"sampleRateHz,omitempty"`
	Channels     uint32       `json:"channels,omitempty"`
	ChannelNames []string     `json:"channelNames,omitempty"`
}

// DefaultAudioFormat is the framework's default playback layout.
func DefaultAudioFormat() AudioFormat {
	return AudioFormat{SampleFormat: SampleFormatPCM16LE, SampleRateHz: 48000, Channels: 2}
}

func (f AudioFormat) isZero() bool {
	return f.SampleFormat == 0 && f.SampleRateHz == 0 && f.Channels == 0 && len(f.ChannelNames) == 0
}

// AudioChunk is one captured or played PCM chunk. On capture, PCM is filled
// from the native binary frame path, never through base64 JSON.
type AudioChunk struct {
	Sequence            uint64 `json:"sequence,omitempty"`
	FirstFrame          uint64 `json:"firstFrame,omitempty"`
	MonotonicTimeNS     uint64 `json:"monotonicTimeNs,omitempty"`
	FrameCount          uint32 `json:"frameCount,omitempty"`
	PCM                 []byte `json:"pcm,omitempty"`
	Discontinuity       bool   `json:"discontinuity,omitempty"`
	DroppedFramesBefore uint64 `json:"droppedFramesBefore,omitempty"`
}

// AudioNotice reports a capture or playback backend condition.
type AudioNotice struct {
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// AudioEndpoint describes one system audio endpoint.
type AudioEndpoint struct {
	EndpointID       string         `json:"endpointId,omitempty"`
	Direction        AudioDirection `json:"direction,omitempty"`
	SupportedFormats []AudioFormat  `json:"supportedFormats,omitempty"`
	ProcessingTap    string         `json:"processingTap,omitempty"`
}

// MicChunk is one captured audio chunk, format announcement or notice.
type MicChunk struct {
	Chunk    *AudioChunk  `json:"chunk,omitempty"`
	Format   *AudioFormat `json:"format,omitempty"`
	StreamID string       `json:"streamId,omitempty"`
	Notice   *AudioNotice `json:"notice,omitempty"`
}

// PresetValue is one typed preset argument value. Exactly one field is set.
type PresetValue struct {
	Text    *string  `json:"text,omitempty"`
	Boolean *bool    `json:"boolean,omitempty"`
	Integer *int64   `json:"integer,omitempty"`
	Number  *float64 `json:"number,omitempty"`
	Color   *RGB     `json:"color,omitempty"`
}

// PresetArgument is a named value passed to a built-in effect or face.
type PresetArgument struct {
	Name  string       `json:"name,omitempty"`
	Value *PresetValue `json:"value,omitempty"`
}

// PresetParameterDefinition documents one preset parameter.
type PresetParameterDefinition struct {
	Name         string       `json:"name,omitempty"`
	Description  string       `json:"description,omitempty"`
	ValueType    string       `json:"valueType,omitempty"`
	DefaultValue *PresetValue `json:"defaultValue,omitempty"`
	Min          *float64     `json:"min,omitempty"`
	Max          *float64     `json:"max,omitempty"`
}

// PresetInfo describes a built-in effect or face.
type PresetInfo struct {
	PresetID             string                      `json:"presetId,omitempty"`
	Kind                 PresetKind                  `json:"kind,omitempty"`
	DisplayName          string                      `json:"displayName,omitempty"`
	Description          string                      `json:"description,omitempty"`
	Parameters           []PresetParameterDefinition `json:"parameters,omitempty"`
	SupportedResourceIDs []string                    `json:"supportedResourceIds,omitempty"`
}

// CaptureInfo describes an acquired capture.
type CaptureInfo struct {
	CaptureID           string      `json:"captureId,omitempty"`
	ResourceID          string      `json:"resourceId,omitempty"`
	AppID               string      `json:"appId,omitempty"`
	RunID               string      `json:"runId,omitempty"`
	PresentationID      string      `json:"presentationId,omitempty"`
	AcquiredMonotonicNS uint64      `json:"acquiredMonotonicNs,omitempty"`
	Selected            bool        `json:"selected,omitempty"`
	Ready               bool        `json:"ready,omitempty"`
	TouchMode           CaptureMode `json:"touchMode,omitempty"`
}

// CaptureEvent reports a capture grant or selection change.
type CaptureEvent struct {
	Selected bool         `json:"selected,omitempty"`
	Reason   string       `json:"reason,omitempty"`
	Info     *CaptureInfo `json:"info,omitempty"`
}

// KeyInput is a physical key event.
type KeyInput struct {
	Key     string `json:"key,omitempty"`
	Pressed bool   `json:"pressed,omitempty"`
}

// GestureInput is a recognized touch gesture phase.
type GestureInput struct {
	GestureID string `json:"gestureId,omitempty"`
	Phase     string `json:"phase,omitempty"`
}

// RawTouchContact is one raw touch contact.
type RawTouchContact struct {
	ContactID   uint32   `json:"contactId,omitempty"`
	XNormalized float32  `json:"xNormalized,omitempty"`
	YNormalized float32  `json:"yNormalized,omitempty"`
	Pressure    *float32 `json:"pressure,omitempty"`
	Touching    bool     `json:"touching,omitempty"`
}

// RawTouchReport is a full-raw touch report.
type RawTouchReport struct {
	Contacts            []RawTouchContact `json:"contacts,omitempty"`
	BackendSample       []byte            `json:"backendSample,omitempty"`
	BackendSampleFormat string            `json:"backendSampleFormat,omitempty"`
	CoordinatesValid    bool              `json:"coordinatesValid,omitempty"`
}

// InputEvent is one touch input event. Exactly one of Key, Gesture or Raw is
// set.
type InputEvent struct {
	ResourceID      string          `json:"resourceId,omitempty"`
	MonotonicTimeNS uint64          `json:"monotonicTimeNs,omitempty"`
	Key             *KeyInput       `json:"key,omitempty"`
	Gesture         *GestureInput   `json:"gesture,omitempty"`
	Raw             *RawTouchReport `json:"raw,omitempty"`
}

// BluetoothMediaState is the connected audio source's AVRCP state.
type BluetoothMediaState struct {
	Present       bool   `json:"present,omitempty"`
	Playing       bool   `json:"playing,omitempty"`
	DeviceAddress string `json:"deviceAddress,omitempty"`
	DeviceName    string `json:"deviceName,omitempty"`
	Title         string `json:"title,omitempty"`
	Artist        string `json:"artist,omitempty"`
	Album         string `json:"album,omitempty"`
	DurationMS    uint64 `json:"durationMs,omitempty"`
	PositionMS    uint64 `json:"positionMs,omitempty"`
	Revision      uint64 `json:"revision,omitempty"`
	Detail        string `json:"detail,omitempty"`
	// DirectionKnown reports whether the framework resolved a playback
	// direction for the displayed source.
	DirectionKnown bool `json:"directionKnown,omitempty"`
	// DirectionPlaying is that direction. Playing above is the source-published
	// AVRCP status, which a MediaControl1-only source never publishes; prefer
	// this field when DirectionKnown is true.
	DirectionPlaying bool `json:"directionPlaying,omitempty"`
}
