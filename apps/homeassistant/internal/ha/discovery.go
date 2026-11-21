package ha

import (
	"encoding/json"
	"sort"
)

// Entity is one Home Assistant entity the bridge publishes. Config holds the
// entity-specific discovery keys; the shared keys (name, unique id, object id,
// availability and the device block) are added when the payload is built.
type Entity struct {
	Component string
	Slug      string
	Name      string
	// Category is the Home Assistant entity_category ("diagnostic") or empty.
	Category string
	Config   map[string]any
}

// Catalog is the complete entity set for one device and one configuration.
type Catalog struct {
	Topics   Topics
	Device   Device
	Entities []Entity
}

// BuildOptions selects which surfaces are published.
type BuildOptions struct {
	Ring        bool
	Display     bool
	Sensors     bool
	Bluetooth   bool
	Touch       bool
	Diagnostics bool
}

// BuildCatalog assembles the entity set from the probed capabilities. Only
// entities backed by an advertised surface are produced: publishing a ring
// light on a device with no ring would offer Home Assistant a control that can
// only fail.
func BuildCatalog(topics Topics, device Device, caps Capabilities, sensors []SensorInfo,
	ringPresets, facePresets []Preset, opts BuildOptions) Catalog {
	catalog := Catalog{Topics: topics, Device: device}

	if opts.Diagnostics {
		catalog.Entities = append(catalog.Entities,
			Entity{
				Component: "sensor", Slug: SlugUptime, Name: "Uptime", Category: "diagnostic",
				Config: map[string]any{
					"state_topic":         topics.StateTopic(SlugUptime),
					"unit_of_measurement": "s",
					"device_class":        "duration",
					"state_class":         "total_increasing",
					"icon":                "mdi:timer-outline",
				},
			},
			Entity{
				Component: "sensor", Slug: SlugVolume, Name: "Master volume", Category: "diagnostic",
				Config: map[string]any{
					"state_topic":         topics.StateTopic(SlugVolume),
					"unit_of_measurement": "%",
					"state_class":         "measurement",
					"icon":                "mdi:volume-high",
				},
			},
			Entity{
				Component: "binary_sensor", Slug: SlugMicrophoneMute, Name: "Microphone muted",
				Category: "diagnostic",
				Config: map[string]any{
					"state_topic": topics.StateTopic(SlugMicrophoneMute),
					"payload_on":  PayloadOn,
					"payload_off": PayloadOff,
					"icon":        "mdi:microphone-off",
				},
			},
			Entity{
				Component: "sensor", Slug: SlugBridgeStatus, Name: "Integration status", Category: "diagnostic",
				Config: map[string]any{
					"state_topic":           topics.StateTopic(SlugBridgeStatus),
					"json_attributes_topic": topics.AttributesTopic(SlugBridgeStatus),
					"icon":                  "mdi:bridge",
				},
			},
			Entity{
				Component: "button", Slug: SlugRepublish, Name: "Republish discovery", Category: "diagnostic",
				Config: map[string]any{
					"command_topic": topics.SetTopic(SlugRepublish),
					"payload_press": PayloadPress,
					"icon":          "mdi:refresh",
				},
			},
			Entity{
				Component: "button", Slug: SlugReconnect, Name: "Reconnect broker", Category: "diagnostic",
				Config: map[string]any{
					"command_topic": topics.SetTopic(SlugReconnect),
					"payload_press": PayloadPress,
					"device_class":  "restart",
				},
			},
			Entity{
				Component: "button", Slug: SlugRelease, Name: "Release hardware", Category: "diagnostic",
				Config: map[string]any{
					"command_topic": topics.SetTopic(SlugRelease),
					"payload_press": PayloadPress,
					"icon":          "mdi:hand-back-right-outline",
				},
			},
		)
	}

	if opts.Ring && caps.Ring {
		// Home Assistant's modern JSON light schema takes colour support from
		// supported_color_modes; the legacy rgb/hs/xy flags were removed and are
		// dropped from a discovery payload. flash and transition have no
		// implementation here, so they are declared false rather than left at
		// their defaults (flash defaults to true).
		light := map[string]any{
			"schema":        "json",
			"state_topic":   topics.StateTopic(SlugRing),
			"command_topic": topics.SetTopic(SlugRing),
			"icon":          "mdi:led-strip-variant",
			"brightness":    true,
			"flash":         false,
			"transition":    false,
		}
		if caps.RingRGB {
			light["supported_color_modes"] = []string{"rgb"}
		} else {
			light["supported_color_modes"] = []string{"brightness"}
		}
		if len(ringPresets) > 0 {
			effects := make([]string, 0, len(ringPresets))
			for _, preset := range ringPresets {
				effects = append(effects, preset.ID)
			}
			light["effect"] = true
			light["effect_list"] = effects
		}
		catalog.Entities = append(catalog.Entities, Entity{
			Component: "light", Slug: SlugRing, Name: "Ring", Config: light,
		})
	}

	if opts.Display && caps.SegmentDisplay {
		catalog.Entities = append(catalog.Entities, Entity{
			Component: "switch", Slug: SlugDisplay, Name: "Segment display overlay",
			Config: map[string]any{
				"state_topic":   topics.StateTopic(SlugDisplay),
				"command_topic": topics.SetTopic(SlugDisplay),
				"payload_on":    PayloadOn,
				"payload_off":   PayloadOff,
				"state_on":      PayloadOn,
				"state_off":     PayloadOff,
				"icon":          "mdi:dots-horizontal",
			},
		})
		if len(facePresets) > 0 {
			options := make([]string, 0, len(facePresets))
			for _, preset := range facePresets {
				options = append(options, preset.ID)
			}
			catalog.Entities = append(catalog.Entities, Entity{
				Component: "select", Slug: SlugDisplayFace, Name: "Segment display face",
				Config: map[string]any{
					"state_topic":   topics.StateTopic(SlugDisplayFace),
					"command_topic": topics.SetTopic(SlugDisplayFace),
					"options":       options,
					"icon":          "mdi:emoticon-outline",
				},
			})
		}
	}

	if opts.Bluetooth && caps.Media {
		// Home Assistant's MQTT integration has no media_player platform, so the
		// Bluetooth source is published with components it does support: one
		// enum sensor for the resolved playback direction, one text sensor for
		// the track, and one button per transport command.
		catalog.Entities = append(catalog.Entities,
			Entity{
				Component: "sensor", Slug: SlugBluetoothState, Name: "Bluetooth playback",
				Category: "diagnostic",
				Config: map[string]any{
					"state_topic":           topics.StateTopic(SlugBluetoothState),
					"json_attributes_topic": topics.AttributesTopic(SlugBluetoothState),
					"device_class":          "enum",
					"options":               []string{MediaStatePlaying, MediaStateIdle, MediaStateOff},
					"icon":                  "mdi:bluetooth-audio",
				},
			},
			Entity{
				Component: "sensor", Slug: SlugBluetoothTitle, Name: "Bluetooth title",
				Config: map[string]any{
					"state_topic": topics.StateTopic(SlugBluetoothTitle),
					"icon":        "mdi:music-note",
				},
			},
			Entity{
				Component: "button", Slug: SlugBluetoothPlay, Name: "Bluetooth play",
				Config: map[string]any{
					"command_topic": topics.SetTopic(SlugBluetoothPlay),
					"payload_press": PayloadPlay,
					"icon":          "mdi:play",
				},
			},
			Entity{
				Component: "button", Slug: SlugBluetoothPause, Name: "Bluetooth pause",
				Config: map[string]any{
					"command_topic": topics.SetTopic(SlugBluetoothPause),
					"payload_press": PayloadPause,
					"icon":          "mdi:pause",
				},
			},
			Entity{
				Component: "button", Slug: SlugBluetoothNext, Name: "Bluetooth next",
				Config: map[string]any{
					"command_topic": topics.SetTopic(SlugBluetoothNext),
					"payload_press": PayloadNext,
					"icon":          "mdi:skip-next",
				},
			},
			Entity{
				Component: "button", Slug: SlugBluetoothPrev, Name: "Bluetooth previous",
				Config: map[string]any{
					"command_topic": topics.SetTopic(SlugBluetoothPrev),
					"payload_press": PayloadPrevious,
					"icon":          "mdi:skip-previous",
				},
			},
		)
	}

	if opts.Touch && caps.Touch && caps.TouchPartial {
		catalog.Entities = append(catalog.Entities, Entity{
			Component: "event", Slug: SlugTouch, Name: "Panel button",
			Config: map[string]any{
				// No value_template: the MQTT event platform renders the payload
				// through the template and then requires the result to be a JSON
				// object carrying "event_type". A template that extracts the type
				// makes every event an unparseable string, which Home Assistant
				// drops with a warning.
				"state_topic": topics.StateTopic(SlugTouch),
				"event_types": TouchEventTypes(),
				"icon":        "mdi:gesture-tap-button",
			},
		})
	}

	if opts.Sensors {
		for _, sensor := range sensors {
			if sensor.Slug == "" {
				continue
			}
			config := map[string]any{"state_topic": topics.StateTopic(sensor.Slug)}
			if sensor.Unit != "" {
				config["unit_of_measurement"] = sensor.Unit
			}
			if sensor.DeviceClass != "" {
				config["device_class"] = sensor.DeviceClass
			}
			if sensor.StateClass {
				config["state_class"] = "measurement"
			}
			if sensor.Unit == "" && sensor.DeviceClass == "" {
				config["icon"] = "mdi:gauge"
			}
			catalog.Entities = append(catalog.Entities, Entity{
				Component: "sensor", Slug: sensor.Slug, Name: sensor.Name, Config: config,
			})
		}
	}

	// Two entities on one topic would overwrite each other's discovery payload.
	// Sensor ids are slugged into entity names, so a pathological id set can
	// collide; the first declaration wins and the duplicate is dropped rather
	// than published twice.
	seen := make(map[string]bool, len(catalog.Entities))
	deduped := catalog.Entities[:0]
	for _, entity := range catalog.Entities {
		key := entity.Component + "/" + entity.Slug
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, entity)
	}
	catalog.Entities = deduped

	sort.SliceStable(catalog.Entities, func(i, j int) bool {
		if catalog.Entities[i].Component != catalog.Entities[j].Component {
			return catalog.Entities[i].Component < catalog.Entities[j].Component
		}
		return catalog.Entities[i].Slug < catalog.Entities[j].Slug
	})
	return catalog
}

// DiscoveryPayload renders one entity's retained discovery configuration.
func (c Catalog) DiscoveryPayload(e Entity) ([]byte, error) {
	// object_id is deliberately absent: it was a discovery option and has been
	// removed, so an entity id is derived by Home Assistant from the device and
	// entity names. unique_id is what keeps an entity stable across renames.
	payload := map[string]any{
		"name":                  e.Name,
		"unique_id":             c.Topics.UniqueID(e.Slug),
		"availability_topic":    c.Topics.StatusTopic(),
		"payload_available":     PayloadOnline,
		"payload_not_available": PayloadOffline,
		"device":                c.deviceBlock(),
	}
	if e.Category != "" {
		payload["entity_category"] = e.Category
	}
	for key, value := range e.Config {
		payload[key] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, Errorf("encode discovery payload for %s.%s: %w", e.Component, e.Slug, err)
	}
	return encoded, nil
}

func (c Catalog) deviceBlock() map[string]any {
	device := map[string]any{
		"identifiers": []string{c.Device.NodeID},
		"name":        c.Device.Name,
	}
	if c.Device.Manufacturer != "" {
		device["manufacturer"] = c.Device.Manufacturer
	}
	if c.Device.Model != "" {
		device["model"] = c.Device.Model
	}
	if c.Device.SWVersion != "" {
		device["sw_version"] = c.Device.SWVersion
	}
	if c.Device.HWVersion != "" {
		device["hw_version"] = c.Device.HWVersion
	}
	return device
}

// DiscoveryTopics returns every discovery topic this catalog publishes, in the
// same order as Entities.
func (c Catalog) DiscoveryTopics() []string {
	topics := make([]string, 0, len(c.Entities))
	for _, entity := range c.Entities {
		topics = append(topics, c.Topics.DiscoveryTopic(entity.Component, entity.Slug))
	}
	return topics
}

// CommandTopics returns every command topic this catalog subscribes to.
func (c Catalog) CommandTopics() []string {
	topics := make([]string, 0, len(c.Entities))
	for _, entity := range c.Entities {
		if _, ok := entity.Config["command_topic"]; !ok {
			continue
		}
		topics = append(topics, c.Topics.SetTopic(entity.Slug))
	}
	return topics
}

// EntityByTopic finds the entity whose set topic matches, so an inbound
// command can be routed without a second lookup table.
func (c Catalog) EntityByTopic(topic string) (Entity, bool) {
	for _, entity := range c.Entities {
		if c.Topics.SetTopic(entity.Slug) == topic {
			return entity, true
		}
	}
	return Entity{}, false
}

// TouchEventTypes lists the event types the touch event entity can publish.
// Partial touch capture delivers the play_pause and center keys and completed
// swipes; the framework keeps the volume and microphone gestures.
func TouchEventTypes() []string {
	return []string{
		"play_pause_press", "play_pause_release",
		"center_press", "center_release",
		"swipe_left", "swipe_right", "swipe_up", "swipe_down",
	}
}
