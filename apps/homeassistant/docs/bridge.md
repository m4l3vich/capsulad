Home Assistant integration: MQTT discovery for the device's hardware and a local app interface to inspect and refresh it.

The integration publishes Home Assistant MQTT discovery payloads for the surfaces the
device advertises, publishes their state, and applies Home Assistant's commands
through the framework's normal services. This document is the complete external
contract: the topics and payloads Home Assistant sees, and the
`org.capsulad.homeassistant.v1.HomeAssistantService` interface other supervised
apps call on the app bus.

## Topics

```text
<discovery_prefix>/<component>/<node_id>/<entity>/config   retained discovery payload
<base_topic>/<node_id>/status                              retained "online"/"offline"
<base_topic>/<node_id>/<entity>/state                      entity state (not retained)
<base_topic>/<node_id>/<entity>/set                        entity command
<base_topic>/<node_id>/<entity>/attributes                 JSON attributes
```

`discovery_prefix` defaults to `homeassistant`, `base_topic` to `capsulad`, and
`node_id` is a slug derived from the framework device id unless it is configured.
`availability_topic` is the status topic with `payload_available: "online"` and
`payload_not_available: "offline"`; the broker's last will carries the offline
value, so a crashed app is marked unavailable by the broker.

Discovery topics are written retained at QoS 1 on every (re)connect and whenever
`PublishDiscovery` is called. State topics are not retained: the integration publishes
the full current state after each connect, and Home Assistant keeps the last value
it saw. A broker restart therefore leaves the entity available (status is
republished) with the value published at the next state refresh.

## Entities

| Component | Slug | Command payload | State payload |
|---|---|---|---|
| `light` | `ring` | JSON: `{"state":"ON","brightness":1-255,"color":{"r","g","b"},"effect":"<preset id>"}` or `{"state":"OFF"}` | JSON: `{"state":"ON","brightness":1-255,"color_mode":"rgb","color":{"r","g","b"},"effect":"<preset id>"}`, or `{"state":"OFF"}` |
| `switch` | `display` | `ON` / `OFF` | `ON` / `OFF` |
| `select` | `display_face` | One of the `options` values (a face preset id) | The active face preset id |
| `sensor` | `bluetooth_state` | — | `playing`, `idle` or `off` (`device_class: enum`) |
| `sensor` | `bluetooth_title` | — | The track title, or `none` |
| `button` | `bluetooth_play`, `bluetooth_pause`, `bluetooth_next`, `bluetooth_previous` | `PLAY`, `PAUSE`, `NEXT`, `PREVIOUS` | — |
| `event` | `touch` | — | `{"event_type":"<type>"}` |
| `sensor` | `uptime` | — | Seconds |
| `sensor` | `volume` | — | Percent, one decimal |
| `binary_sensor` | `microphone_muted` | — | `ON` / `OFF` |
| `sensor` | `bridge_status` | — | `online`, `connecting`, `stopped` or `error` |
| `sensor` | `<framework sensor id slug>` | — | The raw sample value |
| `button` | `bridge_republish`, `bridge_reconnect`, `bridge_release` | `PRESS` | — |

There is no `media_player` entity: Home Assistant's MQTT integration does not
implement one (it is absent from the integration's supported components), so the
Bluetooth source is published with components it does support — an enum sensor
for the resolved playback direction, a sensor for the track, and one button per
transport command. Each button accepts only its own payload.

The Bluetooth attributes are published on the `bluetooth_state` attributes topic:
`state`, `title`, `artist`, `album`, `duration_s`, `position_s` and `detail` (the
framework's explanation of a missing field). The `bridge_status` attributes are
`entities`, `published`, `commands`, `command_errors`, `publish_errors`,
`states`, `events`, `ring_on`, `display_on`, `ring_effect`, `display_face` and
`last_error`.

The `light` entity declares `schema: "json"`, `supported_color_modes` (`["rgb"]`
when the ring reports RGB, otherwise `["brightness"]`), `brightness`, `flash:
false`, `transition: false`, and `effect_list` with one entry per built-in ring
effect. `flash` and `transition` are declared false because the integration implements
neither, but a command that carries them anyway is still applied: the whole point
is that a flash option must not make a light command fail. An ON state payload
names its `color_mode`, which the modern JSON schema requires before it applies a
colour. The
`select` options and `effect_list` are read from `PresentationService.ListPresets`
at startup, so a device that gains or loses a preset changes its discovery
payload; call `PublishDiscovery` after such a change to rewrite it.

### Effect and colour rules

- `{"effect":"<id>"}` applies a built-in effect and counts as ON.
- `{"effect":"None"}` clears the effect and restores the last static colour.
- `{"state":"ON"}` with no colour or brightness re-applies the last colour at the
  last brightness; the initial default is white at full brightness.
- `{"state":"OFF"}` releases the presentation and capture. The device's
  configured default ring app resumes; the integration does not hold a dark ring.
- A colour, brightness or effect command implies ON.
- A brightness of 0 is raised to 1: Home Assistant ignores a zero brightness in a
  state payload, and the ring has no separate dark mode (OFF releases it).
- On a ring without RGB support the entity advertises `["brightness"]`, so Home
  Assistant offers no colour picker and never sends a colour.

### Display rules

- `switch` ON shows the last selected face, or the first advertised face.
- `switch` OFF releases the presentation and capture; the configured default
  display app resumes.
- Selecting a face while the overlay is off turns it on.
- `display_face` accepts only ids returned by `ListPresets`; anything else is
  refused and reported.

### Touch events

The `event` entity exists only when `expose_touch` is on and the panel advertises
partial gestures. While its partial capture is held, the framework stops acting
on `play_pause` and `centre`, so a framework alert or an assistant turn can no
longer be dismissed with them; the volume and microphone gestures stay with the
device. Declared event types: `play_pause_press`, `play_pause_release`,
`center_press`, `center_release`, `swipe_left`, `swipe_right`, `swipe_up`,
`swipe_down`.

## `GetStatus`

Returns the accepted configuration, the live connection state, the published
entity count, the ring and display state the integration believes it set, the status
string, the last error, and the session counters (`published`, `commands`,
`failures`). `broker` is the endpoint in use, so it also reports a broker that
was found by mDNS discovery rather than configured.

A caller that is not connected sees `connected: false` with the last error in
`last_error`. `configured` is false until a settings snapshot has been accepted.
The response never contains the broker password or the CA bundle.

## `PublishDiscovery`

Rewrites every retained discovery payload, the availability marker and the
current state, then returns how many discovery payloads were written. It is the
recovery path after clearing retained messages on the broker, after re-adding the
Home Assistant MQTT integration, or after an administrator changed the topic
layout. It fails when no integration runtime is running (no accepted configuration or
a failed device probe).

## `PublishEvent`

Raises one event on the `event` entity. `event_type` must be one of the declared
types listed above; anything else is refused with `INVALID_ARGUMENT`, because
Home Assistant discards undeclared event types. `payload_json` is an optional JSON
object of at most 4 KiB that is merged into the published payload.

This is how another supervised app (for example an assistant that reacts to a
panel press) can leave a record in Home Assistant's automation engine without
speaking MQTT itself.

## Errors

The interface uses canonical gRPC codes: `INVALID_ARGUMENT` for a malformed
request, an undeclared event type or an oversized payload; `FAILED_PRECONDITION`
when the request cannot be served by the running integration — no integration runtime yet,
no event entity published (`expose_touch` off, or the panel does not advertise
partial gestures), or no connected broker session; `INTERNAL` when a response
cannot be encoded. Commands arriving over MQTT are never answered with a gRPC
error: a refused command is logged, counted in the `bridge_status` attributes and
reflected in the status sensor.

## Home Assistant compatibility

The entity set uses only platforms the MQTT integration implements: `light`
(JSON schema, `supported_color_modes`), `switch`, `select`, `sensor` (including
`device_class: enum` with `options`), `binary_sensor`, `button` and `event`. It
targets current Home Assistant releases; the payload and schema keys were checked
against the integration's own source, not only its documentation. Sensor units
are published in Home Assistant's vocabulary (`celsius` → `°C`, `lux` → `lx`) with
a device class and statistics only for those pairs Home Assistant validates;
anything else is published verbatim with no class and no statistics, which Home
Assistant always accepts.

## Security

The MQTT transport is the shared native transport: verified TLS 1.2+ with the
configured roots, no insecure mode and no verification override. The broker
password is a masked app setting and is never logged, published or returned by
this interface. The device address and name of a Bluetooth source are redacted by
the framework for app callers and never published. No credential or internal
identifier is published in mDNS TXT records; the integration only browses `_mqtt._tcp`.
