package hw

import (
	"reflect"
	"testing"

	"capsulad/apps/homeassistant/internal/appcfg"
	"capsulad/apps/homeassistant/internal/ha"
)

// The set of watchers a bridge will actually run is derived from the published
// entity set — the shape of the bug that published uptime/volume/mute forever
// with diagnostics off, and that retried a Bluetooth watch forever on a device
// with no adapter. This asserts the loops themselves, not an intermediate flag.
func TestWatcherGatingFollowsTheCatalog(t *testing.T) {
	topics, err := ha.NewTopics("", "", "capsula")
	if err != nil {
		t.Fatalf("NewTopics: %v", err)
	}
	device := ha.Device{NodeID: "capsula", Name: "Capsula Mini"}
	sensors := []ha.SensorInfo{{ID: "sensor.ambient_light", Slug: "sensor_ambient_light",
		Name: "Ambient light", FrameworkUnit: "lux", Unit: "lx", DeviceClass: "illuminance", StateClass: true}}

	// A ring-only catalog: nothing else is published, so nothing else watches.
	bare := ha.BuildCatalog(topics, device, ha.Capabilities{Ring: true}, sensors, nil, nil,
		ha.BuildOptions{Ring: true})
	watchers := NewWatchers(nil, nil, appcfg.Config{}, sensors, nil, bare, nil)
	if names := loopNames(watchers.loops()); len(names) != 0 {
		t.Fatalf("a ring-only catalog started %v, want no watchers", names)
	}

	// The full exposure: each watcher's entity is published, so each runs.
	caps := ha.Capabilities{SegmentDisplay: true, Ring: true, RingRGB: true, Media: true,
		Touch: true, TouchPartial: true}
	full := ha.BuildCatalog(topics, device, caps, sensors, nil, nil,
		ha.BuildOptions{Ring: true, Display: true, Sensors: true, Bluetooth: true, Touch: true, Diagnostics: true})
	watchers = NewWatchers(nil, nil, appcfg.Config{}, sensors, nil, full, nil)
	want := []string{"device-state", "sensor:sensor_ambient_light", "media", "touch"}
	if got := loopNames(watchers.loops()); !reflect.DeepEqual(got, want) {
		t.Fatalf("loops %v, want %v", got, want)
	}
}

func loopNames(loops []loop) []string {
	names := make([]string, 0, len(loops))
	for _, l := range loops {
		names = append(names, l.name)
	}
	return names
}
