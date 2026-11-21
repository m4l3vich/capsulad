package ha

import (
	"encoding/json"
	"testing"
)

func testTopics(t *testing.T) Topics {
	t.Helper()
	topics, err := NewTopics("", "", "capsula")
	if err != nil {
		t.Fatalf("NewTopics: %v", err)
	}
	return topics
}

func testDevice() Device {
	return Device{
		ID: "capsula-0001", NodeID: "capsula", Name: "Capsula Mini",
		Manufacturer: "capsulad", Model: "Capsula Mini rev 4", SWVersion: "daemon 0.5 / fw 1.0",
		HWVersion: "rev4 / RK3308",
	}
}

func decodePayload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("discovery payload is not JSON: %v", err)
	}
	return out
}

func TestBuildCatalogHonorsCapabilities(t *testing.T) {
	sensors := []SensorInfo{{ID: "sensor.ambient_light", Slug: "sensor_ambient_light", Name: "Ambient light",
		FrameworkUnit: "lux", Unit: "lx", DeviceClass: "illuminance", StateClass: true}}
	caps := Capabilities{SegmentDisplay: true, Ring: true, RingRGB: true, Touch: true, TouchPartial: true, Media: true}
	catalog := BuildCatalog(testTopics(t), testDevice(), caps, sensors, ringPresets(), facePresets(),
		BuildOptions{Ring: true, Display: true, Sensors: true, Bluetooth: true, Touch: true, Diagnostics: true})

	want := map[string]bool{
		"light/ring": true, "switch/display": true, "select/display_face": true,
		"event/touch": true,
		// Home Assistant has no MQTT media_player, so the Bluetooth source is
		// an enum sensor, a title sensor and four buttons.
		"sensor/bluetooth_state": true, "sensor/bluetooth_title": true,
		"button/bluetooth_play": true, "button/bluetooth_pause": true,
		"button/bluetooth_next": true, "button/bluetooth_previous": true,
		"sensor/sensor_ambient_light": true, "sensor/uptime": true, "sensor/volume": true,
		"sensor/bridge_status": true, "binary_sensor/microphone_muted": true,
		"button/bridge_republish": true, "button/bridge_reconnect": true, "button/bridge_release": true,
	}
	got := map[string]bool{}
	for _, entity := range catalog.Entities {
		got[entity.Component+"/"+entity.Slug] = true
	}
	for key := range want {
		if !got[key] {
			t.Errorf("catalog is missing %s", key)
		}
	}
	if len(got) != len(want) {
		t.Errorf("catalog has %d entities, want %d: %v", len(got), len(want), got)
	}
}

func TestBuildCatalogOmitsUnavailableSurfaces(t *testing.T) {
	// A device with no ring, no display and no media source must not publish
	// controls that could only fail.
	catalog := BuildCatalog(testTopics(t), testDevice(), Capabilities{}, nil, ringPresets(), facePresets(),
		BuildOptions{Ring: true, Display: true, Sensors: true, Bluetooth: true, Touch: true, Diagnostics: true})
	forbidden := map[string]bool{
		SlugRing: true, SlugDisplay: true, SlugDisplayFace: true, SlugTouch: true,
		SlugBluetoothState: true, SlugBluetoothTitle: true, SlugBluetoothPlay: true,
		SlugBluetoothPause: true, SlugBluetoothNext: true, SlugBluetoothPrev: true,
	}
	allowed := map[string]bool{SlugUptime: true, SlugVolume: true, SlugMicrophoneMute: true,
		SlugBridgeStatus: true, SlugRepublish: true, SlugReconnect: true, SlugRelease: true}
	for _, entity := range catalog.Entities {
		if forbidden[entity.Slug] {
			t.Fatalf("published %s.%s without the capability", entity.Component, entity.Slug)
		}
		if !allowed[entity.Slug] {
			t.Fatalf("unexpected diagnostic entity %s.%s", entity.Component, entity.Slug)
		}
	}
}

func TestSensorWithoutAMappedUnitPublishesNoClassOrStatistics(t *testing.T) {
	sensors := []SensorInfo{{ID: "sensor.ambient_light", Slug: "sensor_ambient_light",
		Name: "Ambient light", FrameworkUnit: "lux-raw", Unit: "lux-raw"}}
	catalog := BuildCatalog(testTopics(t), testDevice(), Capabilities{}, sensors, nil, nil,
		BuildOptions{Sensors: true})
	if len(catalog.Entities) != 1 {
		t.Fatalf("catalog has %d entities, want 1", len(catalog.Entities))
	}
	payload, err := catalog.DiscoveryPayload(catalog.Entities[0])
	if err != nil {
		t.Fatalf("DiscoveryPayload: %v", err)
	}
	sensor := decodePayload(t, payload)
	if sensor["unit_of_measurement"] != "lux-raw" {
		t.Fatalf("unit not published verbatim: %v", sensor)
	}
	if _, present := sensor["device_class"]; present {
		t.Fatalf("an unknown unit got a device class: %v", sensor)
	}
	if _, present := sensor["state_class"]; present {
		t.Fatalf("an unknown unit got statistics: %v", sensor)
	}
}

func TestEventEntityHasNoValueTemplate(t *testing.T) {
	caps := Capabilities{Touch: true, TouchPartial: true}
	catalog := BuildCatalog(testTopics(t), testDevice(), caps, nil, nil, nil, BuildOptions{Touch: true})
	var event Entity
	for _, entity := range catalog.Entities {
		if entity.Slug == SlugTouch {
			event = entity
		}
	}
	if event.Slug == "" {
		t.Fatal("no touch event entity")
	}
	// The MQTT event platform renders the payload through a value template and
	// then requires a JSON object; a template that extracts the type would make
	// every event unparseable and Home Assistant would drop it.
	if _, present := event.Config["value_template"]; present {
		t.Fatal("the event entity declares a value_template, which breaks event parsing")
	}
	if event.Config["event_types"] == nil {
		t.Fatal("the event entity declares no event types")
	}
}

func TestTouchEntityRequiresPartialCapture(t *testing.T) {
	caps := Capabilities{Touch: true, TouchFull: true}
	catalog := BuildCatalog(testTopics(t), testDevice(), caps, nil, nil, nil,
		BuildOptions{Touch: true})
	for _, entity := range catalog.Entities {
		if entity.Component == "event" {
			t.Fatal("touch event entity published although partial capture is unavailable")
		}
	}
}

func TestDiscoveryPayloads(t *testing.T) {
	caps := Capabilities{SegmentDisplay: true, Ring: true, RingRGB: true, Media: true}
	catalog := BuildCatalog(testTopics(t), testDevice(), caps,
		[]SensorInfo{{ID: "sensor.x", Slug: "sensor_x", Name: "X", FrameworkUnit: "celsius",
			Unit: "°C", DeviceClass: "temperature", StateClass: true}},
		ringPresets(), facePresets(),
		BuildOptions{Ring: true, Display: true, Sensors: true, Bluetooth: true, Touch: true, Diagnostics: true})

	for _, entity := range catalog.Entities {
		payload, err := catalog.DiscoveryPayload(entity)
		if err != nil {
			t.Fatalf("DiscoveryPayload(%s): %v", entity.Slug, err)
		}
		decoded := decodePayload(t, payload)
		if decoded["unique_id"] != "capsula_"+entity.Slug {
			t.Fatalf("%s: unique_id = %v", entity.Slug, decoded["unique_id"])
		}
		if _, present := decoded["object_id"]; present {
			t.Fatalf("%s: object_id is no longer a discovery option", entity.Slug)
		}
		if decoded["availability_topic"] != "capsulad/capsula/status" {
			t.Fatalf("%s: availability_topic = %v", entity.Slug, decoded["availability_topic"])
		}
		if decoded["payload_available"] != PayloadOnline || decoded["payload_not_available"] != PayloadOffline {
			t.Fatalf("%s: availability payloads missing", entity.Slug)
		}
		device, ok := decoded["device"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no device block", entity.Slug)
		}
		if ids, ok := device["identifiers"].([]any); !ok || len(ids) != 1 || ids[0] != "capsula" {
			t.Fatalf("%s: device identifiers = %v", entity.Slug, device["identifiers"])
		}
		if device["name"] != "Capsula Mini" || device["model"] != "Capsula Mini rev 4" {
			t.Fatalf("%s: device block %v", entity.Slug, device)
		}
	}

	bySlug := map[string]map[string]any{}
	for _, entity := range catalog.Entities {
		payload, err := catalog.DiscoveryPayload(entity)
		if err != nil {
			t.Fatalf("DiscoveryPayload(%s): %v", entity.Slug, err)
		}
		bySlug[entity.Slug] = decodePayload(t, payload)
	}

	light := bySlug[SlugRing]
	if light["schema"] != "json" || light["brightness"] != true || light["effect"] != true {
		t.Fatalf("light payload %v", light)
	}
	// The legacy rgb flag is dropped by Home Assistant's schema: colour support
	// comes from supported_color_modes, and flash/transition are declared false
	// because this bridge implements neither.
	if _, present := light["rgb"]; present {
		t.Fatalf("light still advertises the removed rgb flag: %v", light)
	}
	modes, _ := light["supported_color_modes"].([]any)
	if len(modes) != 1 || modes[0] != "rgb" {
		t.Fatalf("light supported_color_modes %v", light["supported_color_modes"])
	}
	if light["flash"] != false || light["transition"] != false {
		t.Fatalf("light advertises flash/transition it does not implement: %v", light)
	}
	if light["state_topic"] != "capsulad/capsula/ring/state" || light["command_topic"] != "capsulad/capsula/ring/set" {
		t.Fatalf("light topics %v", light)
	}
	effects, _ := light["effect_list"].([]any)
	if len(effects) != 2 || effects[0] != "system.ring.pulse" {
		t.Fatalf("effect_list %v", light["effect_list"])
	}

	face := bySlug[SlugDisplayFace]
	options, _ := face["options"].([]any)
	if len(options) != 1 || options[0] != "system.display.face.neutral" {
		t.Fatalf("select options %v", face["options"])
	}

	sw := bySlug[SlugDisplay]
	if sw["payload_on"] != PayloadOn || sw["state_off"] != PayloadOff {
		t.Fatalf("switch payload %v", sw)
	}

	if bySlug[SlugBluetoothPlay]["payload_press"] != PayloadPlay ||
		bySlug[SlugBluetoothPrev]["payload_press"] != PayloadPrevious {
		t.Fatalf("bluetooth button payloads %v", bySlug[SlugBluetoothPlay])
	}
	options, _ = bySlug[SlugBluetoothState]["options"].([]any)
	if len(options) != 3 || options[1] != MediaStateIdle {
		t.Fatalf("bluetooth state options %v", bySlug[SlugBluetoothState]["options"])
	}

	sensor := bySlug["sensor_x"]
	if sensor["unit_of_measurement"] != "°C" || sensor["device_class"] != "temperature" ||
		sensor["state_class"] != "measurement" {
		t.Fatalf("sensor payload %v", sensor)
	}
	if event := bySlug[SlugTouch]; event != nil {
		t.Fatalf("touch entity present in a fixture without the capability: %v", event)
	}

	status := bySlug[SlugBridgeStatus]
	if status["name"] != "Integration status" {
		t.Fatalf("integration status display name = %v", status["name"])
	}
	if status["json_attributes_topic"] != "capsulad/capsula/bridge_status/attributes" {
		t.Fatalf("bridge status payload %v", status)
	}
	if status["entity_category"] != "diagnostic" {
		t.Fatalf("bridge status is not diagnostic: %v", status)
	}
}

func TestCatalogTopicsAndLookup(t *testing.T) {
	caps := Capabilities{Ring: true, RingRGB: true}
	catalog := BuildCatalog(testTopics(t), testDevice(), caps, nil, ringPresets(), nil,
		BuildOptions{Ring: true, Diagnostics: true})

	discoveryTopics := catalog.DiscoveryTopics()
	if len(discoveryTopics) != len(catalog.Entities) {
		t.Fatalf("DiscoveryTopics returned %d topics for %d entities", len(discoveryTopics), len(catalog.Entities))
	}
	for i, topic := range discoveryTopics {
		entity := catalog.Entities[i]
		if topic != "homeassistant/"+entity.Component+"/capsula/"+entity.Slug+"/config" {
			t.Fatalf("discovery topic %q for %s", topic, entity.Slug)
		}
	}

	commandTopics := catalog.CommandTopics()
	if len(commandTopics) == 0 {
		t.Fatal("no command topics published")
	}
	for _, topic := range commandTopics {
		if !ValidTopic(topic) {
			t.Fatalf("command topic %q is not valid", topic)
		}
		entity, ok := catalog.EntityByTopic(topic)
		if !ok {
			t.Fatalf("no entity subscribes to %q", topic)
		}
		if _, hasCommand := entity.Config["command_topic"]; !hasCommand {
			t.Fatalf("%s has no command topic", entity.Slug)
		}
	}
	if _, ok := catalog.EntityByTopic("homeassistant/light/capsula/ring/config"); ok {
		t.Fatal("a discovery topic resolved as a command topic")
	}
}

func TestBuildCatalogDeduplicatesCollidingSlugs(t *testing.T) {
	// "sensor.a_b" and "sensor.a.b" slug to the same topic. Publishing both
	// would make one discovery payload overwrite the other.
	sensors := []SensorInfo{
		{ID: "sensor.a_b", Slug: "sensor_a_b", Name: "A b", Unit: "lx"},
		{ID: "sensor.a.b", Slug: "sensor_a_b", Name: "A b again", Unit: "lx"},
	}
	catalog := BuildCatalog(testTopics(t), testDevice(), Capabilities{}, sensors, nil, nil,
		BuildOptions{Sensors: true})
	count := 0
	for _, entity := range catalog.Entities {
		if entity.Slug == "sensor_a_b" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d entities on one slug, want 1", count)
	}
}

func TestLightEffectListIsOmittedWithoutPresets(t *testing.T) {
	caps := Capabilities{Ring: true, RingRGB: true}
	catalog := BuildCatalog(testTopics(t), testDevice(), caps, nil, nil, nil, BuildOptions{Ring: true})
	payload, err := catalog.DiscoveryPayload(catalog.Entities[0])
	if err != nil {
		t.Fatalf("DiscoveryPayload: %v", err)
	}
	light := decodePayload(t, payload)
	if _, present := light["effect"]; present {
		t.Fatalf("effect advertised without a preset list: %v", light)
	}
	modes, _ := light["supported_color_modes"].([]any)
	if len(modes) != 1 || modes[0] != "rgb" {
		t.Fatalf("colour support not advertised: %v", light)
	}
	// Without RGB support the entity must not offer a colour picker.
	plain := BuildCatalog(testTopics(t), testDevice(), Capabilities{Ring: true}, nil, nil, nil,
		BuildOptions{Ring: true})
	payload, err = plain.DiscoveryPayload(plain.Entities[0])
	if err != nil {
		t.Fatalf("DiscoveryPayload: %v", err)
	}
	modes, _ = decodePayload(t, payload)["supported_color_modes"].([]any)
	if len(modes) != 1 || modes[0] != "brightness" {
		t.Fatalf("a non-RGB ring advertises %v", modes)
	}
}
