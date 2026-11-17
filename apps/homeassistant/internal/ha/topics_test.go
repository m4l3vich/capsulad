package ha

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSlugIsStableAndUsable(t *testing.T) {
	cases := map[string]string{
		"capsula":               "capsula",
		"simulator-0001":        "simulator_0001",
		"AA:BB:CC":              "aa_bb_cc",
		"  leading spaces  ":    "leading_spaces",
		"sensor.ambient_light":  "sensor_ambient_light",
		"---":                   "",
		"Ünïcode device":        "n_code_device",
		strings.Repeat("a", 80): strings.Repeat("a", maxSlugLength),
	}
	for input, want := range cases {
		if got := Slug(input); got != want {
			t.Fatalf("Slug(%q) = %q, want %q", input, got, want)
		}
	}
	// Determinism matters: a restarted app must keep the same entity ids.
	if Slug("Simulator-0001") != Slug("simulator-0001") {
		t.Fatal("Slug is not case-stable")
	}
}

func TestNewTopicsValidatesInput(t *testing.T) {
	topics, err := NewTopics("", "", "simulator-0001")
	if err != nil {
		t.Fatalf("NewTopics: %v", err)
	}
	if topics.DiscoveryPrefix != DefaultDiscoveryPrefix || topics.BaseTopic != DefaultBaseTopic {
		t.Fatalf("defaults not applied: %+v", topics)
	}
	if got, want := topics.StatusTopic(), "capsulad/simulator-0001/status"; got != want {
		t.Fatalf("StatusTopic = %q, want %q", got, want)
	}
	if got, want := topics.StateTopic(SlugRing), "capsulad/simulator-0001/ring/state"; got != want {
		t.Fatalf("StateTopic = %q, want %q", got, want)
	}
	if got, want := topics.SetTopic(SlugRing), "capsulad/simulator-0001/ring/set"; got != want {
		t.Fatalf("SetTopic = %q, want %q", got, want)
	}
	if got, want := topics.DiscoveryTopic("light", SlugRing),
		"homeassistant/light/simulator-0001/ring/config"; got != want {
		t.Fatalf("DiscoveryTopic = %q, want %q", got, want)
	}
	if got, want := topics.UniqueID(SlugRing), "simulator-0001_ring"; got != want {
		t.Fatalf("UniqueID = %q, want %q", got, want)
	}
	// object_id is a removed discovery option: only unique_id is published.
	if got, want := topics.UniqueID(SlugBluetoothState), "simulator-0001_bluetooth_state"; got != want {
		t.Fatalf("UniqueID = %q, want %q", got, want)
	}

	bad := []struct {
		name                  string
		discovery, base, node string
	}{
		{"empty node", "", "", ""},
		{"node with a wildcard", "", "", "capsula/+"},
		{"node with a hash", "", "", "capsula#"},
		{"node with a slash", "", "", "capsula/mini"},
		{"prefix with a wildcard", "homeassistant/#", "", "capsula"},
		{"base with a wildcard", "", "capsulad/+", "capsula"},
		{"empty base level", "", "capsulad//x", "capsula"},
		{"slash only node", "", "", "/"},
	}
	for _, tc := range bad {
		if _, err := NewTopics(tc.discovery, tc.base, tc.node); err == nil {
			t.Fatalf("%s: NewTopics accepted %q/%q/%q", tc.name, tc.discovery, tc.base, tc.node)
		}
	}
}

func TestValidTopic(t *testing.T) {
	valid := []string{"a", "a/b", "homeassistant/light/x/config", "a-b_c.1"}
	for _, topic := range valid {
		if !ValidTopic(topic) {
			t.Fatalf("ValidTopic(%q) = false", topic)
		}
	}
	invalid := []string{"", "+", "#", "a/+", "a/#", "/a", "a/", "a//b", "a\x00b", strings.Repeat("a", maxTopicLength+1)}
	for _, topic := range invalid {
		if ValidTopic(topic) {
			t.Fatalf("ValidTopic(%q) = true", topic)
		}
	}
}

func TestDescribeUnit(t *testing.T) {
	cases := []struct {
		framework   string
		unit        string
		deviceClass string
		stateClass  bool
	}{
		// The framework reports "celsius"/"lux"; Home Assistant knows "°C"/"lx".
		{"celsius", "°C", "temperature", true},
		{"fahrenheit", "°F", "temperature", true},
		{"kelvin", "K", "temperature", true},
		{"lux", "lx", "illuminance", true},
		// The raw illuminance reading is not lux: no class and no statistics,
		// because Home Assistant rejects an unknown class/unit pair.
		{"lux-raw", "lux-raw", "", false},
		{"", "", "", false},
		{"ppm", "ppm", "", false},
	}
	for _, tc := range cases {
		got := DescribeUnit(tc.framework)
		if got.Unit != tc.unit || got.DeviceClass != tc.deviceClass || got.StateClass != tc.stateClass {
			t.Fatalf("DescribeUnit(%q) = %+v, want unit %q class %q stats %v",
				tc.framework, got, tc.unit, tc.deviceClass, tc.stateClass)
		}
	}
}

func TestSensorNaming(t *testing.T) {
	cases := []struct {
		id   string
		name string
		slug string
	}{
		{"sensor.ambient_light", "Ambient light", "sensor_ambient_light"},
		{"SENSOR.SOC_TEMP", "Soc temp", "sensor_soc_temp"},
		{"temperature", "Temperature", "temperature"},
	}
	for _, tc := range cases {
		if got := SensorName(tc.id); got != tc.name {
			t.Fatalf("SensorName(%q) = %q, want %q", tc.id, got, tc.name)
		}
		if got := SensorSlug(tc.id); got != tc.slug {
			t.Fatalf("SensorSlug(%q) = %q, want %q", tc.id, got, tc.slug)
		}
	}
}

func TestMediaStateHelpers(t *testing.T) {
	if got := MediaStatePayload(MediaState{}); got != MediaStateOff {
		t.Fatalf("absent source state = %q, want %q", got, MediaStateOff)
	}
	if got := MediaStatePayload(MediaState{Present: true, Playing: true}); got != MediaStatePlaying {
		t.Fatalf("playing state = %q, want %q", got, MediaStatePlaying)
	}
	if got := MediaStatePayload(MediaState{Present: true}); got != MediaStateIdle {
		t.Fatalf("connected-idle state = %q, want %q", got, MediaStateIdle)
	}
	// The enum sensor's options are exactly these three words.
	for _, value := range []string{MediaStateOff, MediaStatePlaying, MediaStateIdle} {
		if value == "" || strings.ToLower(value) != value {
			t.Fatalf("enum option %q is not lower case", value)
		}
	}
	if got := MediaTitlePayload(MediaState{}); got != TitlesNone {
		t.Fatalf("absent title = %q, want %q", got, TitlesNone)
	}
	if got := MediaTitlePayload(MediaState{Title: "Song"}); got != "Song" {
		t.Fatalf("title payload = %q", got)
	}

	attrs := MediaAttributes(MediaState{
		Present: true, Playing: true,
		Title: "Song", Artist: "Artist", Album: "Album", Duration: 210000, Position: 5000,
	})
	if attrs["state"] != MediaStatePlaying || attrs["title"] != "Song" ||
		attrs["artist"] != "Artist" || attrs["album"] != "Album" {
		t.Fatalf("unexpected attributes %+v", attrs)
	}
	if attrs["duration_s"] != uint64(210) || attrs["position_s"] != uint64(5) {
		t.Fatalf("durations not converted to seconds: %+v", attrs)
	}
	if _, present := MediaAttributes(MediaState{})["title"]; present {
		t.Fatal("empty metadata produced a title attribute")
	}
}

func TestMediaTitleStateStaysWithinHomeAssistantLimit(t *testing.T) {
	short := MediaTitlePayload(MediaState{Title: "Song"})
	if short != "Song" {
		t.Fatalf("short title = %q", short)
	}
	long := strings.Repeat("ü", 400) // 800 bytes of multi-byte runes
	got := MediaTitlePayload(MediaState{Title: long})
	if len(got) > MaxStateLength {
		t.Fatalf("state is %d bytes, want at most %d", len(got), MaxStateLength)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncation broke the encoding: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated state %q does not mark the truncation", got)
	}
	// The full value is still what the attributes advertise.
	attrs := MediaAttributes(MediaState{Title: long})
	if attrs["title"] != long {
		t.Fatal("attributes lost the untruncated title")
	}
}
