# Home Assistant bridge (`org.capsulad.homeassistant`)

## Using this app

The bridge is a background service that publishes the speaker to Home Assistant
over MQTT and applies Home Assistant's commands through framework services. The
rest of this page documents the entity set and implementation for maintainers.

- **Before selecting it:** configure *Broker host* and credentials, or leave the
  host empty with *Discover the broker with mDNS* enabled on a device that has
  Avahi. Keep the discovery prefix aligned with Home Assistant's MQTT
  integration (usually `homeassistant`).
- **Launch:** the package starts at boot and stays ready. Open
  **Apps → Home Assistant bridge → Details** to launch it manually after a stop;
  it declares no launch arguments.
- **Controls:** use the entities Home Assistant discovers. The detail page also
  offers *Republish discovery and state*, *Reconnect the broker* and *Release
  hardware*. *Release hardware* hands the ring and segment display back to the
  device's configured defaults and asks for confirmation.
- **Teardown:** *Stop* on the detail page ends the bridge until the next boot or
  manual launch. It is a background app, not a selected provider, so a stop does
  not restart it. Uninstalling keeps its settings; retained discovery messages
  may remain on the broker.
- **Troubleshooting:**
  - **No entities appear:** check the broker, user name/password, discovery
    prefix and retained messages. Use *Republish discovery and state* after
    clearing retained state or re-adding the MQTT integration.
  - **TLS fails:** there is no insecure mode. Add the broker's private CA to
    *Extra CA certificates*, and set *TLS server name* when the certificate
    names a different host.
  - **The ring or display stopped responding:** Home Assistant owns those
    surfaces while the bridge drives them. Use *Release hardware*; the
    device's configured defaults resume.
  - **Panel buttons no longer dismiss alerts:** *Expose the panel buttons* uses
    a partial touch capture. Leave it off unless Home Assistant should own those
    controls; volume and microphone gestures stay with the device.
  - **The broker reports the device offline after a crash:** the last will is
    retained. Reconnect or let the bridge reconnect; *Republish* refreshes the
    retained state.

Publishes a Capsula Mini to Home Assistant with MQTT discovery, and applies Home
Assistant's commands back to the device through the framework's normal services.

The app is an ordinary supervised native app. It reads device state from the
framework, owns the ring and segment display only while Home Assistant drives
them, and sends every MQTT byte through the shared native transport
(`capsulad.OpenTCP`). It links no TLS, HTTP or MQTT transport of its own, creates
no listening socket, and never touches a device node.

## What Home Assistant gets

| Entity | Domain | Direction | Notes |
|---|---|---|---|
| Ring | `light` (JSON schema) | state + control | Colour, brightness and the framework's built-in effects. OFF releases the capture, so the configured default ring app resumes. |
| Segment display overlay | `switch` | state + control | ON shows a face and owns the display; OFF releases it to the default app. |
| Segment display face | `select` | state + control | One option per built-in face reported by `ListPresets`. |
| Bluetooth playback | `sensor` (`device_class: enum`) | state | `playing`, `idle` or `off`, with title/artist/album/position as attributes. |
| Bluetooth title | `sensor` | state | The track title, or `none`. |
| Bluetooth transport | 4 × `button` | control | Play, pause, next, previous. |
| Panel button | `event` | state + control (opt-in) | `play_pause` and `centre` keys and completed swipes, via a partial touch capture. |
| Framework sensors | `sensor` | state | One entity per `SensorService` sensor, with its unit and (when unambiguous) a device class. |
| Uptime, master volume, microphone muted | `sensor`, `binary_sensor` | state | Diagnostic. Volume and mute are read-only: the framework reserves both writes for a local administrator. |
| Bridge status | `sensor` | state | `online`, `connecting`, `stopped` or `error`, with JSON attributes (entity count, counters, last error). |
| Republish / Reconnect / Release hardware | `button` | control | Diagnostic recovery actions. |

Availability is one retained topic, `<base_topic>/<node_id>/status`, carrying
`online` or `offline`. The MQTT last will is set to `offline` (retained, QoS 1),
so a crashed app is marked unavailable by the broker itself; a deliberate stop
publishes `offline` and then disconnects cleanly, which also discards the will.

Entity slugs: `ring`, `display`, `display_face`, `bluetooth_state`,
`bluetooth_title`, `bluetooth_play`, `bluetooth_pause`, `bluetooth_next`,
`bluetooth_previous`, `touch`, `uptime`, `volume`, `microphone_muted`,
`bridge_status`, `bridge_republish`, `bridge_reconnect`, `bridge_release`, and
`sensor_<framework sensor id>` for framework sensors.

**Entity set notes.** Home Assistant's MQTT integration has no `media_player`
platform, so the Bluetooth source is published as two sensors and four buttons
rather than a media player. The ring light declares `supported_color_modes`
(the legacy `rgb` flag was removed from Home Assistant's schema and is dropped
from a discovery payload) and its ON state names `color_mode`, which the modern
JSON schema requires before it applies a colour. `flash` and `transition` are
declared unsupported, but a command carrying either is still applied rather than
rejected. Sensor units are published in Home Assistant's vocabulary (`celsius` →
`°C`, `lux` → `lx`) with a device class and statistics only for pairs Home
Assistant validates; an unmapped unit such as `lux-raw` is published verbatim
with no class and no statistics. The entity set uses only platforms the MQTT
integration implements and was checked against its current source.

## Configuration

All settings are declared in `manifest.toml` and rendered in the Web UI.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `broker_host` | text | — | Broker host name or address. Empty + `broker_discover` on browses `_mqtt._tcp`. |
| `broker_port` | integer | 1883 | Broker port. |
| `broker_tls` | boolean | false | Verified TLS 1.2+ through the shared transport. No insecure mode. |
| `broker_tls_roots` | select | `system` | `system`, `webpki` or `pem` trust roots. |
| `broker_ca_pem` | text | — | Extra PEM certificates (private CA). Required by `pem`. |
| `broker_tls_server_name` | text | — | SNI / verified name. Empty uses the host. |
| `broker_username` | text | — | MQTT user name. |
| `broker_password` | text_masked | — | MQTT password. Never logged or published. |
| `broker_discover` | boolean | true | Browse `_mqtt._tcp` when no host is configured. |
| `keep_alive_seconds` | integer | 30 | MQTT keepalive; silence beyond 1.5 intervals drops the session. |
| `client_id` | text | — | Empty derives `capsulad-ha-<node_id>`. |
| `discovery_prefix` | text | `homeassistant` | Home Assistant discovery prefix. |
| `base_topic` | text | `capsulad` | Root of the state/command topics. |
| `node_id` | text | — | Topic level and device identifier; empty derives a slug from the framework device id. |
| `state_interval_seconds` | integer | 30 | Uptime/mixer republish cadence. |
| `sensor_interval_seconds` | integer | 60 | Requested minimum interval per sensor watch. |
| `expose_ring` | boolean | true | Publish the ring light. |
| `expose_display` | boolean | true | Publish the display switch and face select. |
| `expose_sensors` | boolean | true | Publish framework sensors. |
| `expose_bluetooth` | boolean | true | Publish the Bluetooth state/title sensors and the four transport buttons. |
| `expose_touch` | boolean | false | Publish the panel event entity (partial capture). |
| `expose_diagnostics` | boolean | true | Publish the diagnostic entities and buttons. |

### Broker behaviour

The bridge refuses to look healthy when it is not:

- If the CONNECT/CONNACK exchange (10 s), the SUBACK wait (10 s) or the initial
  discovery publish fails, the session does not count as usable: it publishes the
  retained `offline` marker, sends DISCONNECT and retries with a backoff that
  doubles from 1 s to at most 1 min.
- The backoff is reset only after a session has been usable for 30 s. A broker
  that keeps dropping a healthy session therefore also slows down instead of
  being retried once a second.
- `keep_alive_seconds` is the other liveness bound: a broker that stays silent for
  more than 1.5 intervals ends the session. At the 5 s minimum that can fire
  before the 10 s subscribe budget, which produces the same reconnect with one
  extra log line.
- A wedged peer cannot stall the app: every write path (including the offline
  marker and DISCONNECT) is bounded, and a write that exceeds its budget closes
  the stream, which the session recovers from by dialling again.

A rejected settings snapshot keeps the previous configuration: the app logs the
reason and never starts with a half-applied configuration. Applying a changed
configuration stops the running bridge first (publishing offline and releasing
every presentation and capture) and then starts the new one.

### Topics

```text
<discovery_prefix>/<component>/<node_id>/<entity>/config   retained discovery payload
<base_topic>/<node_id>/status                              retained "online" / "offline"
<base_topic>/<node_id>/<entity>/state                      entity state
<base_topic>/<node_id>/<entity>/set                        entity command
<base_topic>/<node_id>/<entity>/attributes                 JSON attributes (media, bridge status)
```

The full payload contract is in [docs/bridge.md](docs/bridge.md).

## Interface

The app publishes `org.capsulad.homeassistant.v1.HomeAssistantService` on the app
bus (`proto/homeassistant.proto`):

| RPC | Purpose |
|---|---|
| `GetStatus` | Configuration, connection state, entity count, counters and the last error. |
| `PublishDiscovery` | Rewrite every retained discovery payload and the current state; returns how many were written. |
| `PublishEvent` | Raise one of the event entity's declared panel event types. |

## Ownership and safety

- The bridge owns a surface only while it is driving it. A light OFF or a
  display switch OFF releases the capture and presentation; the device's
  configured default app resumes. The bridge never paints a black frame over a
  baseline the operator chose.
- Enabling `expose_touch` takes the panel's `play_pause` and `centre` controls
  from the framework while the capture is held: a sounding framework alert or an
  assistant turn can no longer be dismissed with them. The volume and microphone
  gestures stay with the device. This is why the feature is off by default.
- Every inbound command is size-bounded, strictly parsed and validated before it
  reaches the device. An unknown topic, unknown key, out-of-range value or
  unknown preset is refused and reported; the device does not change state.
- The broker password and the CA bundle are never logged, never included in a
  discovery payload and never returned by `GetStatus`.

## Build, install, run

```sh
sh apps/homeassistant/build.sh                 # host build + package
GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/homeassistant/build.sh
capsulad-cli app install build/apps/org.capsulad.homeassistant-0.1.0.tar.gz
capsulad-cli app start org.capsulad.homeassistant
capsulad-cli logs -app org.capsulad.homeassistant -n 100
```

Tests: `cd apps/homeassistant && CGO_ENABLED=1 go test ./...` (the MQTT, catalog
and settings packages are pure Go; the framework and main packages need cgo for
the SDK shim).

## Known gaps (framework capabilities this app does not have)

- **No media player.** Home Assistant has no MQTT `media_player` platform, so the
  Bluetooth source is two sensors plus four buttons. Album art, seeking, volume
  and a Home Assistant `media_player.play_media` target are not available; the
  app also has no audio decoder and no way to serve a file to Home Assistant, so
  TTS or URL playback through this bridge would need a decoder in the app.
- **No segment text rendering.** The framework exposes segment cells (raw masks)
  and built-in faces, no font. The bridge therefore publishes a face selector,
  not a text entity. Arbitrary text would require a font in the app.
- **No master volume or microphone-mute control.** `AudioService.SetMasterVolume`
  and `SetMicrophoneMuted` are administrator-only, so both are exposed read-only.
- **No display/ring brightness settings.** They live in administrator-only
  `SettingsService`; the ring entity's own brightness is a presentation frame,
  not the device setting.
- **mDNS discovery is device-only.** The simulator has no Avahi responder, so
  `broker_discover` cannot find anything there; configure `broker_host` instead.
- **Amlogic panels may render nothing.** On `amlogic-stock` the clock panel's vendor ABI is
  unrecovered, so a segment presentation reports `UNVERIFIED` and never reaches the panel; on
  `amlogic-alpine` the panel works only when the kernel line display or per-segment LED class
  devices are present. Ring, keys, audio and sensors are implemented on both Amlogic targets and
  report their own status, but no Amlogic unit has been probed, so they are not device-verified.
