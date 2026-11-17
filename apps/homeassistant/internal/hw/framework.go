// Package hw binds the bridge to the framework: typed reads of device state,
// presentation/capture ownership for the ring and display, the MQTT stream over
// the shared native transport, and the watchers that feed Home Assistant.
//
// Nothing here touches a device node. Output goes through PresentationService,
// input through InputService, media through BluetoothMediaService, and every
// network byte goes through the shared SDK (capsulad.OpenTCP, mDNS calls).
package hw

import (
	"context"
	"fmt"
	"time"

	"capsulad/apps/homeassistant/internal/ha"
	"capsulad/sdk/capsulad"
)

const (
	deviceService = "capsulad.framework.v1.DeviceService"
	sensorService = "capsulad.framework.v1.SensorService"
	systemService = "capsulad.framework.v1.SystemService"

	// callTimeout bounds one framework unary call. A hung daemon must not wedge
	// a command handler that Home Assistant is waiting on.
	callTimeout = 5 * time.Second

	// Resource ids are stable across targets; the bridge still resolves them
	// from ListResources by kind so a device that advertises a different id is
	// handled instead of hard-failing.
	segmentResource = "display.segment.front"
	ringResource    = "light.ring.main"
	touchResource   = "touch.panel.main"
)

// DeviceInfo is the subset of DeviceService.GetInfo the bridge uses.
type DeviceInfo struct {
	DeviceID          string `json:"deviceId"`
	Model             string `json:"model"`
	BoardRevision     string `json:"boardRevision"`
	SOC               string `json:"soc"`
	Kernel            string `json:"kernel"`
	Firmware          string `json:"firmware"`
	DaemonVersion     string `json:"daemonVersion"`
	HardwareSerial    string `json:"hardwareSerial"`
	TargetPlatform    string `json:"targetPlatform"`
	SubsystemBackends []struct {
		Subsystem string `json:"subsystem"`
		BackendID string `json:"backendId"`
		Status    string `json:"status"`
		Detail    string `json:"detail"`
	} `json:"subsystemBackends"`
}

// SegmentCapabilities mirrors the display's advertised geometry.
type SegmentCapabilities struct {
	DigitCount            uint32 `json:"digitCount"`
	SupportsColon         bool   `json:"supportsColon"`
	SupportsDecimalPoints bool   `json:"supportsDecimalPoints"`
}

// RingCapabilities mirrors the ring's advertised geometry.
type RingCapabilities struct {
	PixelCount  uint32 `json:"pixelCount"`
	SupportsRGB bool   `json:"supportsRgb"`
}

// TouchCapabilities mirrors the panel's advertised capture modes.
type TouchCapabilities struct {
	PartialGesturesAvailable bool     `json:"partialGesturesAvailable"`
	FullRawAvailable         bool     `json:"fullRawAvailable"`
	CoordinatesAvailable     bool     `json:"coordinatesAvailable"`
	FullPanelAvailable       bool     `json:"fullPanelAvailable"`
	SemanticGestures         []string `json:"semanticGestures"`
}

// Resource is one advertised resource.
type Resource struct {
	ResourceID string               `json:"resourceId"`
	Kind       string               `json:"kind"`
	Routing    string               `json:"routing"`
	Available  bool                 `json:"available"`
	Features   []string             `json:"features"`
	Segment    *SegmentCapabilities `json:"segment"`
	Ring       *RingCapabilities    `json:"ring"`
	Touch      *TouchCapabilities   `json:"touch"`
}

type resourcesResponse struct {
	Revision  uint64     `json:"revision"`
	Resources []Resource `json:"resources"`
}

// SensorInfo is one framework sensor.
type SensorInfo struct {
	SensorID string `json:"sensorId"`
	Unit     string `json:"unit"`
}

type sensorsResponse struct {
	Sensors []SensorInfo `json:"sensors"`
}

type systemStatus struct {
	UptimeMS      uint64 `json:"uptimeMs"`
	BootID        string `json:"bootId"`
	DaemonVersion string `json:"daemonVersion"`
}

type bluetoothStatus struct {
	Present bool   `json:"present"`
	Powered bool   `json:"powered"`
	Adapter string `json:"adapter"`
	Detail  string `json:"detail"`
	Name    string `json:"name"`
}

// Framework is the typed view of the framework services the bridge uses.
type Framework struct {
	client *capsulad.Client
}

// NewFramework wraps a connected client.
func NewFramework(client *capsulad.Client) *Framework {
	return &Framework{client: client}
}

// Client exposes the underlying SDK client for calls that already have a typed
// helper (AudioState, WatchBluetoothMedia, OpenPresentation, …).
func (f *Framework) Client() *capsulad.Client { return f.client }

// DeviceInfo reads the device identity.
func (f *Framework) DeviceInfo(ctx context.Context) (DeviceInfo, error) {
	var info DeviceInfo
	err := f.client.CallJSON(ctx, "", deviceService, "GetInfo", struct{}{}, &info, callTimeout)
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("device info: %w", err)
	}
	return info, nil
}

// Resources lists the advertised hardware resources.
func (f *Framework) Resources(ctx context.Context) ([]Resource, error) {
	var response resourcesResponse
	if err := f.client.CallJSON(ctx, "", deviceService, "ListResources", struct{}{}, &response, callTimeout); err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	return response.Resources, nil
}

// Sensors lists the readable framework sensors.
func (f *Framework) Sensors(ctx context.Context) ([]ha.SensorInfo, error) {
	var response sensorsResponse
	if err := f.client.CallJSON(ctx, "", sensorService, "ListSensors", struct{}{}, &response, callTimeout); err != nil {
		return nil, fmt.Errorf("list sensors: %w", err)
	}
	sensors := make([]ha.SensorInfo, 0, len(response.Sensors))
	for _, sensor := range response.Sensors {
		if sensor.SensorID == "" {
			continue
		}
		unit := ha.DescribeUnit(sensor.Unit)
		sensors = append(sensors, ha.SensorInfo{
			ID:            sensor.SensorID,
			Slug:          ha.SensorSlug(sensor.SensorID),
			Name:          ha.SensorName(sensor.SensorID),
			FrameworkUnit: sensor.Unit,
			Unit:          unit.Unit,
			DeviceClass:   unit.DeviceClass,
			StateClass:    unit.StateClass,
		})
	}
	return sensors, nil
}

// UptimeSeconds reads the daemon-reported device uptime.
func (f *Framework) UptimeSeconds(ctx context.Context) (uint64, error) {
	var status systemStatus
	if err := f.client.CallJSON(ctx, "", systemService, "GetStatus", struct{}{}, &status, callTimeout); err != nil {
		return 0, fmt.Errorf("system status: %w", err)
	}
	return status.UptimeMS / 1000, nil
}

// BluetoothPresent reports whether a Bluetooth adapter is available. A platform
// without a backend reports false, and the bridge then publishes no media
// player.
func (f *Framework) BluetoothPresent(ctx context.Context) (bool, error) {
	var status bluetoothStatus
	if err := f.client.CallJSON(ctx, "", systemService, "GetBluetoothStatus", struct{}{}, &status, callTimeout); err != nil {
		return false, fmt.Errorf("bluetooth status: %w", err)
	}
	return status.Present, nil
}

// Capabilities probes the advertised surfaces.
func (f *Framework) Capabilities(ctx context.Context) (ha.Capabilities, error) {
	resources, err := f.Resources(ctx)
	if err != nil {
		return ha.Capabilities{}, err
	}
	var caps ha.Capabilities
	for _, resource := range resources {
		if !resource.Available {
			continue
		}
		switch resource.Kind {
		case "RESOURCE_SEGMENT_DISPLAY":
			caps.SegmentDisplay = true
		case "RESOURCE_LED_RING":
			caps.Ring = true
			if resource.Ring != nil {
				caps.RingPixels = int(resource.Ring.PixelCount)
				caps.RingRGB = resource.Ring.SupportsRGB
			}
		case "RESOURCE_TOUCH":
			caps.Touch = true
			if resource.Touch != nil {
				caps.TouchPartial = resource.Touch.PartialGesturesAvailable
				caps.TouchFull = resource.Touch.FullRawAvailable
			}
		}
	}
	present, err := f.BluetoothPresent(ctx)
	if err != nil {
		return caps, err
	}
	caps.Bluetooth = present
	caps.Media = present
	return caps, nil
}

// SegmentResourceID and RingResourceID resolve the advertised resource id by
// kind, falling back to the documented stable id.
func (f *Framework) SegmentResourceID(ctx context.Context) (string, int) {
	return f.resourceByKind(ctx, "RESOURCE_SEGMENT_DISPLAY", segmentResource)
}

// RingResourceID resolves the ring resource id and its pixel count.
func (f *Framework) RingResourceID(ctx context.Context) (string, int) {
	return f.resourceByKind(ctx, "RESOURCE_LED_RING", ringResource)
}

// TouchResourceID resolves the touch panel resource id.
func (f *Framework) TouchResourceID(ctx context.Context) string {
	id, _ := f.resourceByKind(ctx, "RESOURCE_TOUCH", touchResource)
	return id
}

func (f *Framework) resourceByKind(ctx context.Context, kind, fallback string) (string, int) {
	resources, err := f.Resources(ctx)
	if err != nil {
		return fallback, 0
	}
	for _, resource := range resources {
		if resource.Kind != kind {
			continue
		}
		pixelsOrDigits := 0
		if resource.Ring != nil {
			pixelsOrDigits = int(resource.Ring.PixelCount)
		}
		if resource.Segment != nil {
			pixelsOrDigits = int(resource.Segment.DigitCount)
		}
		return resource.ResourceID, pixelsOrDigits
	}
	return fallback, 0
}

// Presets lists the built-in presets of one kind for a resource.
func (f *Framework) Presets(ctx context.Context, resourceID, kind string) ([]ha.Preset, error) {
	info, err := f.client.ListPresets(ctx, resourceID)
	if err != nil {
		return nil, fmt.Errorf("list presets for %s: %w", resourceID, err)
	}
	presets := make([]ha.Preset, 0, len(info))
	for _, preset := range info {
		if preset.Kind.String() != kind {
			continue
		}
		presets = append(presets, ha.Preset{
			ID:          preset.PresetID,
			Kind:        preset.Kind.String(),
			DisplayName: preset.DisplayName,
			Description: preset.Description,
		})
	}
	return presets, nil
}

// MediaState reads the connected Bluetooth source state.
func (f *Framework) MediaState(ctx context.Context) (ha.MediaState, error) {
	state, err := f.client.BluetoothMediaState(ctx)
	if err != nil {
		return ha.MediaState{}, fmt.Errorf("bluetooth media state: %w", err)
	}
	return convertMediaState(state), nil
}

// WatchMedia subscribes to Bluetooth source changes.
func (f *Framework) WatchMedia(ctx context.Context) (*capsulad.BluetoothMediaStream, error) {
	return f.client.WatchBluetoothMedia(ctx)
}

// MediaCommand sends one transport command.
func (f *Framework) MediaCommand(ctx context.Context, command string) error {
	var native capsulad.BluetoothMediaCommand
	switch command {
	case ha.PayloadPlay:
		native = capsulad.BluetoothMediaPlay
	case ha.PayloadPause:
		native = capsulad.BluetoothMediaPause
	case ha.PayloadNext:
		native = capsulad.BluetoothMediaNext
	case ha.PayloadPrevious:
		native = capsulad.BluetoothMediaPrevious
	default:
		return fmt.Errorf("unsupported media command %q", command)
	}
	if _, err := f.client.BluetoothMediaCommand(ctx, native); err != nil {
		return fmt.Errorf("bluetooth media command %s: %w", command, err)
	}
	return nil
}

func convertMediaState(state capsulad.BluetoothMediaState) ha.MediaState {
	// direction_playing is the framework's resolved answer; `playing` is the
	// source-published AVRCP status that a MediaControl1-only source never
	// publishes.
	playing := state.Playing
	if state.DirectionKnown {
		playing = state.DirectionPlaying
	}
	return ha.MediaState{
		Present:  state.Present,
		Playing:  playing,
		Title:    state.Title,
		Artist:   state.Artist,
		Album:    state.Album,
		Duration: state.DurationMS,
		Position: state.PositionMS,
		Detail:   state.Detail,
	}
}
