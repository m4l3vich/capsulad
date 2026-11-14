# Application API (current implementation)

This is the current, implementation-backed guide for writing an application that
runs under `capsulad`. It describes the protocol as it exists in this tree; the
wire API is review draft v0.5 and is **not frozen**. ABI v1 (the native C table)
is frozen within its major version.

Maintained sources, in priority order:

1. `proto/capsula/framework/v1/framework.proto` — framework services and
   messages.
2. `proto/capsula/contracts/*/v1/*.proto` — standard app contracts.
3. `internal/manifest/manifest.go` — the manifest schema and validation.
4. `sdk/native/include/capsula/capsula.h` and `sdk/native/` — the native ABI.
5. `sdk/go/capsula/`, `sdk/rust/`, `sdk/cpp/` — bindings and examples.
6. `docs/GLOSSARY.md` — terminology.

`_devdocs/` is a historical architecture draft. Do not write an app against it.

The transport is a local Unix-domain gRPC socket. Its host path defaults to
`<data-dir>/run/hal.sock` (for example `.capsula/run/hal.sock`); inside a
sandboxed app it is bind-mounted at `/run/capsulad/hal.sock`. Either way the
app sees it through the injected `CAPSULA_SOCKET`. There is no network gRPC
listener, no per-app IPC socket and no app-owned output port. All application
traffic is either a framework service call on that socket or a routed call
through `AppBusService`.

## 1. Lifecycle at a glance

```text
package directory + manifest.toml
        │  capsula app install DIR        (validates manifest, entrypoint metadata, SDK requirement)
        ▼
installed app (app_id) ──► launch/invocation (run_id)
        │                          │
        │                          ├─ child process in a probed sandbox
        │                          └─ or compiled-in system app
        ▼
process loads libcapsula ──► AppBusService.Connect
        │                          ├─ AppHello{run_id}  (verified against peer PID/SO_PEERCRED)
        │                          ├─ AppReady          (run becomes READY; interfaces available)
        │                          └─ events: invocations, settings, actions, activations, calls
        ▼
framework services (presentation/capture/audio/input/settings/config/…)
```

An app that loses its framework connection is treated as failed, not as a
deliberate stop. The supervisor restarts unfinished work according to the
manifest and the selected-provider policy. A selected default provider is
restarted even without `restart_on_failure`; deselecting it (including "none"),
uninstalling it or shutting the daemon down is the durable off switch.

## 2. Package and manifest

A non-built-in app is a directory containing `manifest.toml` at its root.
`capsula app install DIR` validates and installs it; `capsula app remove ID`
uninstalls it. Built-in system apps have no package and cannot be installed,
removed or shadowed.

```toml
id = "org.example.weather"          # ^[a-z0-9][a-z0-9._-]{0,127}$
name = "Weather"
version = "0.1.0"
description = "Shows the forecast on the segment display."
background = true
start_on_boot = true
restart_on_failure = true
single_instance = true
provides = ["segment_display_provider"]

[entrypoint]
runtime = "native"                  # native | quickjs | micropython | builtin
path = "bin/weather"                # relative to the package root; required except builtin
architecture = "aarch64"            # required for native
abi = "linux-gnu"                   # required for native
sdk = "0.5"                         # optional framework SDK version declaration

[native_sdk]                        # optional; native entrypoints only
abi_major = 1
min_minor = 0
required_capabilities = [
  "capsula.service.presentation",
  "capsula.service.routing",
  "capsula.service.audio",
  "capsula.audio.playback",
]

[[settings]]
key = "city"
label = "City"
type = "text"                       # text | text_masked | boolean | integer | number | select
default = "Moscow"
required = true
restart_required = false

[[settings]]
key = "units"
label = "Units"
type = "select"
default = "metric"
[[settings.choices]]
value = "metric"
label = "Celsius"
[[settings.choices]]
value = "imperial"
label = "Fahrenheit"

[[actions]]
id = "refresh"
label = "Refresh now"
confirm_before_run = false

[[arguments]]
name = "demo"
type = "boolean"                    # text | integer | number | boolean
required = false

[[interfaces]]
service = "org.example.weather.v1.WeatherService"
version = "v1"
descriptor = "proto/weather.protoset"   # optional, relative, regular file
docs = "docs/weather.md"                # optional, relative

[settings_ui]
html = "ui/settings.html"               # optional; self-contained document, ≤512 KiB

[github_attestation]                # optional, native binaries only
subject = "bin/weather"
bundle = "provenance/build-bundle.jsonl"
repository = "example/weather"
workflow = ".github/workflows/release.yml"
```

Manifest rules enforced at install/launch:

- Exactly one entrypoint runtime. `native` requires `architecture` and `abi`;
  `quickjs` requires a `.js` path; `micropython` requires a `.py` path;
  `builtin` forbids path/architecture/ABI and `native_sdk`.
- All paths are relative to the package root, cleaned, and rejected if absolute
  or escaping (`..`). Script entrypoints must resolve to a regular file inside
  the package.
- Setting keys are unique; `select` needs choices; numeric bounds must be
  ordered; defaults are validated against their type.
- Action IDs are unique. Launch argument names are unique enough for lookup and
  are typed as text, integer, number or boolean; they are never shell-parsed.
- Setting and action updates are typed and validated by the daemon; a manifest
  is metadata, not a permission or security policy.
- A `[settings_ui]` document must be a relative regular `.html` file inside the
  package root, valid UTF-8, at most 512 KiB, and is served as an untrusted
  opaque-origin iframe. The app must declare at least one `[[settings]]`; the
  document replaces the generic form while manifest actions remain host
  buttons. It cannot declare a command, service name or permission.
- Unknown manifest keys are rejected, so a typo cannot silently disable a
  feature.
- `native_sdk.required_capabilities` is preflighted against the installed
  `capsula-native.json` before install and launch; the binding re-checks the
  loaded library at startup.

See `docs/GLOSSARY.md` for `package`, `run`, `invocation`, `system app` and
`default provider`.

## 3. Startup, identity and environment

The supervisor launches the entrypoint itself. There is no shell, no app-owned
socket and no self-assigned identity. The launcher injects:

| Variable | Meaning |
|---|---|
| `CAPSULA_SOCKET` | Framework gRPC Unix socket. In namespace modes this is the in-namespace bind path. |
| `CAPSULA_RUN_ID` | This run's unique identity. Required by `AppBusService.Connect`. |
| `CAPSULA_APP_ID` | The installed app ID. |
| `CAPSULA_SDK` | Framework SDK/protocol version (currently `0.5`). |
| `CAPSULA_LIBRARY` | Absolute path of the compatible `libcapsula` file, when a runtime is installed. |
| `CAPSULA_LIBRARY_DIR` | Directory for the library's own dependency resolution. |
| `CAPSULA_ASSETS` | `/assets` in namespace modes, otherwise the host asset-store path. Read-only. |
| `CAPSULA_APP_DIR` | `/app` in namespace modes, otherwise the host package path. |
| `CAPSULA_INTERPRETER` | Script hosts only: the engine executable. |
| `TZ` | Resolved device timezone, when configured. |

Identity is established from the connection, not from a claim:

- `AppHello{run_id}` must name a live supervised run. The daemon compares it
  with the socket peer's Linux `SO_PEERCRED` PID and rejects a mismatch.
- A compiled-in system app's run cannot be claimed by a supervised connection.
- A connection that omits its run ID but has a sandbox UID or supervised PID is
  still treated as an app and cannot become an administrator by silence.
- "Administrator" means the local daemon/CLI/Web UI caller (no app identity).
  "App scope" means a supervised app addressing its own state. A local
  administrator may address any app; an app may address only itself.

Sandbox modes are probed at startup and reported (`namespaces`, `mountns`,
`uid`, `none`). The stock RK3308 kernel supports only `mountns`; the daemon
never claims stronger isolation than it verified. Apps do not assume a backend
ABI; resource capabilities are discovered.

`AppReady` is sent once the app has opened the bus and is ready for traffic.
That is when `ApplicationService` reports READY and its declared interfaces
become `available`.

## 4. Native SDK and bindings

The normative C interface is `sdk/native/include/capsula/capsula.h`. The
shared library owns the transport; bindings are thin.

```c
capsula_api table = {0};
if (capsula_get_api(CAPSULA_ABI_MAJOR, sizeof table, &table) != CAPSULA_STATUS_OK) {
  /* UNSUPPORTED_ABI or INVALID_ARGUMENT; no table was written */
}
```

- ABI major must match; a newer minor may append table fields. The table is
  copied up to the caller's size and never written past it.
- Output bytes are library-owned `capsula_buf` values freed with
  `api->buf_free`. Input buffers are borrowed for the call only.
- The library must stay resident for the process lifetime; ABI v1 has no
  complete joined shutdown. Do not `dlclose` it after closing clients.
- Handles are thread-safe; a call has one logical reader. `cancel`, `release`,
  `bus_close` and `client_close` unblock a pending `recv`.

Go binding (`capsula/sdk/capsula`):

```go
env := capsula.FromEnv() // CAPSULA_LIBRARY, CAPSULA_SOCKET, CAPSULA_RUN_ID, CAPSULA_APP_ID
client, err := capsula.Connect(ctx, env)
if err != nil { return err }
defer client.Close()
client.SetHandlers(capsula.Handlers{
    Ready:        func() {},
    Invocation:   func(ctx context.Context, inv *capsula.LaunchInvocation) { /* … */ },
    Settings:     func(s *capsula.SettingsChanged) { /* own settings, including masked */ },
    Action:       func(ctx context.Context, a *capsula.ActionInvoked) { /* … */ },
    Activation:   func(ctx context.Context, a *capsula.Activation) { /* assistant only */ },
    TouchRelease: func(r *capsula.InputReleased) { /* hold-to-talk release */ },
    Disconnected: func(err error) { /* terminal */ },
})
if err := client.Start(ctx); err != nil { return err } // blocks until the bus ends
```

Rust (`capsula::Library`, `client.start_call`, `client.start_routed`,
`BusHandler`) and C++ (`dlopen`/`dlsym`, RAII handles) expose the same model.
The detailed ABI, memory, descendant threading, and loader rules are in
`README.md#native-sdk-c-abi-v1`.

### Calls and encoding

- `call_start(service, method, kind, timeout)` covers unary, server-streaming,
  client-streaming and bidi. `kind` declares intent; the stream path is shared.
- `call_send_json` / `call_send_proto` send one message; `call_half_close`
  closes the request direction; `call_recv_json` / `call_recv_proto` receive.
  `recv` returns `OK`, `END_OF_STREAM`, `GRPC`, `TIMEOUT` or `CANCELLED`; a
  timeout leaves the call usable.
- Canonical protobuf JSON means lowerCamelCase fields, enum names, base64
  bytes and 64-bit integers as strings. JSON requires a registered or embedded
  descriptor; unknown methods/descriptors fail explicitly. Raw protobuf works
  without a descriptor.
- `call_start_routed(target_app_id, service, method)` uses
  `AppBusService/Call` and accepts raw protobuf even when the target's custom
  descriptor is not registered locally.
- Queues are bounded (64–256 entries depending on direction); overflow returns
  `BUSY` or ends with `RESOURCE_EXHAUSTED` instead of growing without limit.
  Routed calls default to a 30 s deadline and cap at 5 minutes unless the
  caller supplies one.

Go helpers cover the common shapes: `OpenPresentation`, `OpenCapture`,
`WatchInput`, `OpenPlayback`, `OpenMic`, `BeginConversation`,
`BluetoothMediaState`/`WatchBluetoothMedia`, `ListPresets`, `Call`,
`CallJSON`, `Stream`, `OpenStream`, `OpenBidi`, and `Handle` for inbound
custom methods. `Start` blocks; `Close` is idempotent.

## 5. AppBus (reverse RPC)

Each app has exactly one long-lived `Connect` bidi stream.

**App → daemon (`AppOutbound`)**

| Message | Purpose |
|---|---|
| `AppHello{run_id}` | First message. Verified against the supervised process. |
| `AppReady` | Second message. Marks the run ready and interfaces available. |
| `AppCallReply{call_id, response_message}` | One reply to an inbound custom call. |
| `AppCallReply{call_id, end{grpc_status_code, detail}}` | Terminal reply. Code 0 = OK. |
| `AppInvocationResult{invocation_id, accepted, detail}` | Optional result for a launch/invocation. |
| `AppActionResult{action_invocation_id, succeeded, detail}` | Optional result for a triggered action. |

**Daemon → app (`AppInbound`)**

| Message | Purpose |
|---|---|
| `IncomingAppCall{call_id, caller_run_id, grpc_service, rpc_name, deadline_monotonic_ns}` | A routed custom call. Serve it, reply with `AppCallReply`, and always end it. |
| `IncomingCallData{call_id, request_message}` | Serialized protobuf request bytes; can repeat for client-streaming. |
| `IncomingCallHalfClose{call_id}` | No more request data. |
| `IncomingCallCancel{call_id}` | Caller cancelled or deadline expired. |
| `AppLaunchInvocation{invocation_id, arguments}` | Launch/singleton invocation with typed arguments. |
| `AppSettingsChanged{revision, values}` | Own settings only; this event carries `text_masked` values. Other surfaces redact them. |
| `AppActionInvoked{action_invocation_id, action_id}` | A declared action button was triggered. |
| `AssistantActivation{activation_id, origin, matched_model_id, monotonic_time_ns, expires_monotonic_ns, hold_to_talk}` | One assistant turn for the selected assistant. |
| `AssistantInputReleased{activation_id}` | Hold-to-talk release, correlated with that activation. |

The library sends `hello` and `ready`, then dispatches events on bounded
threads (4 workers, 32 queued calls, 64 live calls). Inbound custom calls get
`RESOURCE_EXHAUSTED` on overflow. A handler must finish its call; returning
without `call_end` ends it with `INTERNAL`. `bus_ready` returns after both
frames are flushed.

**Routed calls (`Call`)**

```text
start{target_app_id, grpc_service, rpc_name, timeout_ms}
→ request_message* → half_close
← response_message* → end{grpc_status_code, detail}
```

The target must be a ready supervised app or a reachable built-in target. The
daemon relays frames; the target's handler decides what it serves. A routed
call never opens a socket to the target. A disconnect fails in-flight calls
with `UNAVAILABLE` rather than replaying them to a new process. JSON is never
sent on this wire path; convert at the binding (`CallJSON`, as in Go) using a
registered descriptor.

The Web UI has no generic AppBus proxy. A malicious page cannot use the
browser as a routed-call client.

## 6. Framework services available to apps

Every gRPC method lives on the single Unix socket. "Admin" means the daemon
rejects a supervised-app identity with `PERMISSION_DENIED`.

| Service | Method | Shape | App access |
|---|---|---|---|
| `DeviceService` | `GetInfo`, `ListResources` | unary | yes |
| | `WatchResources` | server stream `ResourceChange` | yes |
| `SettingsService` | `GetSettings`, `UpdateSettings`, `WatchSettings` | unary / unary / server stream | admin only |
| `RoutingService` | `GetRouting`, `WatchRouting` | unary / server stream | yes |
| | `Capture` | server stream: request, then `CaptureEvent` | yes, own presentation/run |
| | `ReleaseCapture` | unary | yes, own capture |
| | `ForceRelease` | unary | admin only |
| `PresentationService` | `Open` | bidi: open/frame/close → events | yes |
| | `ListPresets` | unary | yes |
| `AudioService` | `ListEndpoints`, `GetAudioState` | unary | yes |
| | `Capture` | server stream after request | yes, indicated capture |
| | `Playback` | bidi: open/chunk/control → events | yes |
| | `SetMasterVolume`, `SetMicrophoneMuted` | unary | admin only |
| `InputService` | `WatchInput` | server stream | yes, selected capture only |
| `AssistantService` | `ListWakewordModels` | unary | yes |
| | `BeginConversation` | server stream `ConversationEvent` | yes, valid activation |
| | wakeword sample/training RPCs | unary / server stream | admin only |
| `ApplicationService` | `ListApplications`, `ListRuntimes`, `GetBinaryProvenance` | unary | yes |
| | `Launch`, `Stop` | unary | admin only |
| | `ListRuns`, `WatchRuns` | unary / server stream | app-scoped to own app |
| `ApplicationConfigurationService` | `Describe` | unary | yes (manifest metadata) |
| | `Get`, `Update`, `TriggerAction` | unary | app-scoped to own app |
| `AppInterfaceService` | `ListInterfaces`, `DescribeInterface` | unary | yes |
| `AppBusService` | `Connect` | bidi | app identity |
| | `Call` | bidi | any local caller; relay |
| `SensorService` | `ListSensors`, `ReadSensor` | unary | yes |
| | `WatchSensor` | server stream | yes |
| `SystemService` | `GetStatus`, `GetBluetoothStatus` | unary | yes (`BluetoothStatus` fields: `present`, `powered`, `discoverable`, `incoming_connections`, `adapter`, `detail`, `name`) |
| | `SetBluetoothIncomingConnections`, `SetBluetoothDiscoverable` | unary | admin only |
| | `ListPairedDevices`, `UnpairDevice`, `GetWebUiSecurity` | unary | admin only |
| | `ListUnits`, `ControlUnit` | unary | currently empty/unimplemented |
| `BluetoothMediaService` | `GetState` | unary | yes, peer address/name redacted |
| | `WatchState` | server stream | yes, redacted |
| | `Command` | unary | yes |
| `DiagnosticsService` | `GetSnapshot`, `WatchLogs` | unary / server stream | admin only |
| `AssetService` | `ListAssets` | unary | yes (catalog only, no bytes) |

Notes:

- `GetAudioState` is readable by apps, but changing volume/mute is an
  administrator operation (`SetMasterVolume`, `SetMicrophoneMuted`). Physical
  keys use the same persisted path.
- `ApplicationConfigurationService.Describe` reveals manifest metadata, which
  is not sensitive; `Get`/`Update`/`TriggerAction` are scoped with
  `requireAppScope` and reject cross-app targets.
- `ListRuns`/`WatchRuns` silently restrict an app caller to its own app and
  reject a foreign `app_id`.
- Paired-device management, force release, wakeword recording/training,
  diagnostics/log history, device settings and app launch/stop are all
  administrator surfaces. They are not available to a sandboxed app.
- `BluetoothStatus` exposes `present`, `powered`, `discoverable`,
  `incoming_connections`, `adapter`, `detail` and `name` (adapter alias/name,
  also the AP SSID). The old `pairable` field is removed. Incoming off denies
  new inbound BR/EDR links — including from paired devices — through the public
  Linux management `Set Connectable` **and** by clearing the kernel action list
  (`Remove Device`), so BlueZ's known-device allow-list cannot bypass the deny;
  the radio stays powered, BLE remains available and existing links are not
  torn down. Discoverable on implies incoming; incoming off clears
  discoverable. New pairing authorization requires both incoming connections
  and discoverability, not the removed pairable flag; enabling discoverability
  implies incoming.
- `ListAssets` returns IDs, normalized paths, media types, hashes, licenses and
  attribution; it never returns file contents.

## 7. Output: presentations, captures and input

### Presentation stream

First request must be `OpenPresentation{resource_id}`. The daemon responds with
`PresentationReady{presentation_id, resource_id, has_valid_frame}` and later
`PresentationVisibility{visible, reason}` events. Frames are one of:

```text
SegmentFrame{cells[{mask, decimal_point}], colon}   // segment display
RingFrame{pixels[{red, green, blue}]}               // direct RGB
RingEffect{effect_id, arguments[{name, PresetValue}]} // built-in effect
SegmentFace{face_id, arguments}                     // built-in face
BlankPresentation{}                                 // explicit blank; still content
```

A presentation becomes eligible for selection only after its first valid
frame. `BlankPresentation` counts as valid content (it is not "no frame").
Content changes inside one presentation do not create a new resource or
capture. `ClosePresentation` (or closing the stream) releases the presentation
and removes dependent captures; process exit does the same.

`ListPresets(resource_id)` returns `PresentationPresetInfo` entries
(`preset_id`, kind, display name, description, typed parameters with defaults
and optional min/max, supported resources). Presets are content, not ports.

### Captures and routing

`RoutingService.Capture` first message:

```text
resource_id, presentation_id, touch_mode
```

`touch_mode` is mandatory for touch and must be `UNSPECIFIED` elsewhere. The
stream starts with `CaptureGranted{capture}` (`capture_id`, app/run,
presentation, acquisition time, `selected`, `ready`, mode), then
`CaptureSelectionChanged{selected, reason}` transitions. `reason` values
include `granted`, `capture`, `default_baseline`, `system_fallback`, `none`
and `not_routable`; they are diagnostic, not a stable API for behavior.

Selection rules:

- For display/ring, the latest acquired *eligible* live capture wins.
- With no eligible capture, the configured default app's baseline presentation
  wins; with no default, the framework fallback or an explicit blank applies.
- There is no hidden provider tier. An app is selected only as the configured
  default, as a capture holder, or as the framework fallback.
- Release order is not necessarily LIFO; an out-of-order release recomputes
  selection. A closed presentation, ended stream, crash or app stop releases
  its capture.
- `ForceRelease` exists for administrators/diagnostics only.

Touch modes:

| Mode | Delivery | Framework keeps |
|---|---|---|
| `TOUCH_PARTIAL_GESTURES` | circle/play-pause and centre keys plus completed gestures | volume up/down and microphone mute |
| `TOUCH_FULL_RAW` | every key and gesture; raw reports only if the backend advertises coordinates | no system volume, mute, media or assistant action |

The Mini panel is five discrete keys (`mic_toggle`, `volume_up`,
`play_pause`, `volume_down`, `center`), not a coordinate panel. Raw coordinates
are never fabricated; `coordinates_available=false` while
`full_panel_available=true` is normal.

`InputService.WatchInput{resource_id, capture_id}` requires the caller's
currently selected touch capture, so it cannot be used as a bypass to read
system volume/mute gestures. Events are:

```text
KeyInput{key, pressed}
GestureInput{gesture_id, phase}     // e.g. tap/double_tap/hold,
                                    // swipe_left/right/up/down,
                                    // circle_cw/circle_ccw; phase start/update/end
RawTouchReport{contacts[...], backend_sample, backend_sample_format,
               coordinates_valid}
```

A consumed dismissal press also consumes its release/hold/repeat. Full capture
suppresses all system actions; alert/turn dismissal and the stopwatch contract
still run before media when the framework owns the input.

## 8. Audio

### Endpoints and format

`ListEndpoints` returns `endpoint_id`, direction (capture/playback), supported
`AudioFormat`s (`PCM_S16LE`, `PCM_F32LE`, rate, channels) and
`processing_tap`. An empty `processing_tap` is a normal physical/raw tap.

The production microphone endpoint is `audio.input.microphone` and advertises
`framework.dsp.v1` while the bounded DSP chain is active (80 Hz high-pass,
adaptive suppression capped at 12 dB, unity gain, −1 dBFS limiter, 256/128 STFT,
about 15.9 ms). The unprocessed vendor stream is only at
`audio.input.microphone.raw`. Wakeword and enrollment consume the processed
tap.

### Capture

First request `CaptureAudioRequest{endpoint_id, preferred_format, packet_ms,
activation_id, conversation_id}`; then the server streams
`CaptureAudioStarted{stream_id, actual_format, indication_watchdog_ms}`,
`AudioChunk{sequence, first_frame, monotonic_time_ns, frame_count, pcm,
discontinuity, dropped_frames_before}` and `AudioNotice{code, detail}`.
Notice codes include `XRUN`, `CLIENT_TOO_SLOW` and `WATCHDOG_EXPIRED`.

Application microphone audio is delivered only while the visible+audible
indication holds: a selected, visible, non-blank ring/segment presentation for
that app at non-zero brightness, plus a microphone-live cue the mixer consumed
and drained. Zero master volume, global mute, a missing sink, a blank/invisible
presentation, or a cue that cannot open/drain deny capture rather than record
quietly. Both halves are re-checked per chunk; withheld chunks increment the
drop count and the next delivered chunk carries `discontinuity`. The watchdog
bounds a capture that loses its indication. Pre-roll is attached only to a
framework-issued activation for the same app that has not been used.

### Playback

First request `OpenPlayback{endpoint_id, format, target_latency_ms,
conversation_id}`; then chunks or `PlaybackCommand{FLUSH|PAUSE|RESUME}`.
Server events are `PlaybackStarted{stream_id, actual_format}`,
`PlaybackProgress{frames_played, frames_queued}` and `AudioNotice`.

- A successful half-close drains queued audio.
- Cancel/abort discards it and unblocks writers.
- A paused stream's queue stays bounded.
- Playback opened with `conversation_id` stays at normal gain during its own
  assistant conversation; unrelated playback is ducked while a conversation
  is active.
- The Go fast path is `OpenPlayback` / `Play` / `Write` / `Close` / `Abort`;
  raw PCM never goes through JSON or base64.

## 9. Settings and actions

### Device settings (administrator)

`SettingsService.GetSettings` / `WatchSettings` / `UpdateSettings` are
administrator-only. `UserSettings` includes provider defaults, volume,
brightness and brightness mode per resource, microphone mute, touch hold mode,
muted-indicator policy, wakeword model/enabled, timezone, startup sound,
Bluetooth policy and hostname. Updates are compare-and-swap:
`expected_revision` + `update_fields` (only listed paths change; maps are
replaced as a whole). A stale revision returns `ABORTED`.

Apps do not read device settings for per-turn policy. For example, an assistant
gets `hold_to_talk` on the activation rather than reading administrator state.

### App settings (own app)

Declared in the manifest and served by `ApplicationConfigurationService`:

- `Describe(app_id)` — schema: key, label, description, type, default JSON,
  choices, required, restart_required, optional min/max. Public manifest
  metadata.
- `Get(app_id)` — own `revision` and `values[{key, value_json, has_value}]`.
  `text_masked` values are always redacted here.
- `Update(app_id, expected_revision, changes[{key, value_json, clear}])` —
  own settings only, CAS, typed validation against the manifest, including
  masked clear. A successful update pushes `AppSettingsChanged` to the owning
  app with the new revision (and masked values, for that app only).
- `TriggerAction(app_id, action_id)` — own declared action only; returns an
  `action_invocation_id` that also arrives as `AppActionInvoked`. Actions are
  not persisted settings.

A missing value with `has_value=false` means "use the declared default". Values
are JSON-encoded (`"text"`, `true`, `42`, `0.5`). Unknown keys, wrong types,
out-of-range numbers and undeclared select values fail with `INVALID_ARGUMENT`.
The revision identifies a snapshot; a concurrent writer is rejected rather than
silently overwritten.

Device-wide settings and another app's configuration are never readable or
writable by a supervised app. The Web UI is an administrator client and uses
the same services.

### Custom settings UI (untrusted iframe)

An app may ship one self-contained settings document:

```toml
[settings_ui]
html = "ui/settings.html"   # relative, regular .html inside the package, ≤512 KiB, valid UTF-8
```

The Web UI loads
`GET /api/apps/{appID}/settings-ui` directly as the iframe's `src` with the
administrator's session, and the document itself is **untrusted**:

- The response is an opaque-origin document. The browser renders it in a
  sandboxed iframe with `sandbox="allow-scripts"` only — no same-origin, forms,
  popups, top navigation, downloads, pointer lock or storage privileges. The
  session cookie authenticates this document request; the document receives no
  session token, host cookie access, WebSocket or DOM access.
- The response and the injected wrapper impose
  `default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline';
  img-src data:; font-src 'none'; connect-src 'none'; media-src 'none';
  object-src 'none'; frame-src 'none'; child-src 'none'; worker-src 'none';
  form-action 'none'; base-uri 'none'; navigate-to 'none'; frame-ancestors
  'self'; sandbox allow-scripts`. The document has no
  network access, no external fonts/images/imports, and no host cookie,
  WebSocket, DOM or API access.
- The host page's CSP allows `frame-src 'self'`, so only same-origin document
  URLs can be framed. Normal host routes retain `X-Frame-Options: DENY` and a
  `frame-ancestors 'none'` CSP; the settings-document route deletes
  `X-Frame-Options` and overrides the host CSP with the restrictive document
  policy above. It also sends `Cache-Control: no-store`, `X-Content-Type-Options:
  nosniff`, `Referrer-Policy: no-referrer`, COOP/CORP `same-origin`,
  `Vary: Cookie` and no `Set-Cookie`.
- `navigate-to` is present in the document policy but browsers do not implement
  it, so this documentation does **not** claim CSP blocks self-navigation. The
  frame URL is bounded by the host `frame-src 'self'`; the bridge owner detects
  navigation instead: any iframe `load` after the initial ready is a violation.
  It disposes the frame, points it at `about:blank`, and never re-sends
  init/token/schema/values to the navigated document. A new mount or retry gets
  a fresh token and a fresh frame.
- The parent gives the frame 10 seconds to reach `ready`. No ready means a
  visible error and a disposed frame; retry re-fetches the snapshot and creates
  a fresh frame rather than re-initializing the old one.
- The daemon injects the system UI kit (local CSS) and the bridge bootstrap
  (local JavaScript) before the app's own markup. The app supplies no external
  script or stylesheet URL.

The kit: wrap content in `<main class="capsula-ui">` and use the documented
classes — `card`, `stack`, `row`, `spread`, `actions`, `field`, `check`, `pill`
(`ok|warn|err`), `notice`, `mono`, `grow`, `wrap`, `sr-only`, plus the CSS
variables (`--bg`, `--surface`, `--fg`, `--muted`, `--accent`, `--ok`, `--warn`,
`--err`, `--radius`, `--sans`, `--mono`, …). Form controls are styled from
`input`/`select`/`textarea`; `table`, `alarm-list`, `alarm-item`, `days`,
`swatches`, `gradient-preview` and `stop-row` are available for compatible
forms. The kit is versioned locally and performs no I/O.

#### Bridge protocol (version 1)

The injected bootstrap exposes `window.CapsulaUI`:

```js
CapsulaUI.apiVersion              // 1
CapsulaUI.appId
CapsulaUI.schema                  // { settings: [...] } from the app manifest
CapsulaUI.revision                // current app-settings revision
CapsulaUI.onState(fn)             // host state/snapshot events
CapsulaUI.onResult(fn)            // patch results and errors
CapsulaUI.patch(changes)          // [{ key, valueJson }] or [{ key, clear: true }]
CapsulaUI.resize()
```

Messages use the prefix `capsula.settings-ui/1:`:

| Direction | Type | Payload |
|---|---|---|
| parent → frame | `init` | `{channel, appId, apiVersion:1, schema, revision, values}`. Exactly one; 32 lowercase hex channel. |
| frame → parent | `ready` | The bootstrap accepted init. |
| frame → parent | `patch` | `{requestId, changes:[{key, valueJson}|{key, clear}]}`. |
| frame → parent | `resize` | `{height}` for iframe sizing (20–4000 px). |
| parent → frame | `state` | `{revision, schema, values}` after an external/app update. |
| parent → frame | `result` | `{requestId, ok, revision, message?, error?}` for the matching patch. |

Parent rules:

- Accept only messages whose `event.source` is that exact iframe's
  `contentWindow`, whose origin is the opaque `null` (or empty), and whose
  channel equals the random token generated for that frame. A frame gets a
  fresh token per mount; a stale/unmounted frame and a foreign app's reference
  have no effect. A child message that claims a real origin is rejected.
- Enforce the 64 KiB child-message bound, a valid request ID, 1–64 changes,
  ≤8 KiB per value and ≤32 KiB total, and schema/JSON validity. Unknown
  message types and undeclared keys are reported as errors and have no host
  effect.
- Only setting-value patches for the app whose page hosts the frame are
  forwarded, through the normal `ApplicationConfigurationService.Update` path:
  the daemon re-checks that every key is declared, the value type/bounds,
  masked/clear rules, the expected revision (CAS) and the existing
  `maxAppSettingChanges` (64), key length (128 bytes) and value length
  (8 KiB) limits. Patches are serialized (one in flight, bounded queue of 16);
  a conflict or validation error is returned to the frame as a `result` and
  the host refreshes the authoritative state. No generic AppBus, capture,
  audio, asset or filesystem proxy exists in this path.

The frame's own live controls may call `patch` immediately; text/number drafts
remain local until the app chooses to submit them (for example on a Save
button). A host `state` message does not overwrite a draft the user is editing.
The built-in Clock and Ring settings pages use this identical route; their
HTML is embedded in the daemon only because they have no package directory.
The embedded clock editor accepts the full alarm grammar
(`<five-field-dcron> [@IANA-zone] [!once] [!disabled]`), preserves untouched
entries byte-for-byte when saving, shows disabled entries with an Enable
control, and offers timezone and one-shot controls. An external custom UI still
uses only setting patches — the host never exposes structured alarm commands.

## 10. Interfaces, discovery and contracts

An app may publish one or more interfaces:

```toml
[[interfaces]]
service = "org.example.weather.v1.WeatherService"
version = "v1"
descriptor = "proto/weather.protoset"
docs = "docs/weather.md"
```

The descriptor must be a serialized `FileDescriptorSet` including imports. Docs
are Markdown. Neither is executed.

Discovery:

```text
AppInterfaceService.ListInterfaces{app_id?, running_only?}
  → AppInterfaceInfo{app_id, run_id, grpc_service, interface_version,
                     available, summary}
AppInterfaceService.DescribeInterface{app_id, grpc_service}
  → AppInterfaceDescription{info, file_descriptor_set,
                            documentation_markdown, methods[
                              rpc_name, summary, semantics_markdown,
                              request_example_json, response_example_json]}
```

`available` is true only when the selected run has sent `AppReady`. `run_id` is
empty when the app is not running. An interface is a name and a schema, not a
network endpoint: callers use `AppBusService.Call` / `CallJSON`. A target that
is not ready yields `UNAVAILABLE`; an unknown service/method is the target's
decision, and a handler must finish the call explicitly.

Standard contracts currently shipped:

- `capsula.contracts.media.v1.MediaPlayerService` (`ListPlayers`,
  server-streaming `WatchPlayers` whose first event is a snapshot, `GetStatus`,
  `Play`, `Pause`, `SkipNext`, `SkipPrevious`). A provider declares it in
  `[[interfaces]]` and adds the `media_player` provide hint.
- `capsula.contracts.clock.v1.ClockService` — exposed by the built-in
  `capsula.clock` system app.
- `capsula.contracts.ring.v1.RingService` — exposed by the built-in
  `capsula.ring` system app.

These are source-tree contracts. The built-in apps declare them for
`AppInterfaceService`; the framework protoset and Go bindings were regenerated
to include them. The integrated build still needs to confirm the generated
outputs and SDK descriptor resolution; raw protobuf calls do not depend on
descriptor registration.

The two built-in contracts are ordinary public interfaces: they appear in
`AppInterfaceService` (with their `FileDescriptorSet` and method docs) and any
local caller reaches them through `AppBusService.Call`, with the same routed
call path, deadline and cancellation rules as any other app. The daemon's own
Web UI is just another caller; there is no private token path. Caller identity
is still verified, and administrator-only framework operations remain
unavailable to supervised apps.

### Clock service

Target app ID: `capsula.clock`; service:
`capsula.contracts.clock.v1.ClockService`.

| RPC | Shape | Behavior |
|---|---|---|
| `GetState` | unary `ClockState` | Local time, timezone, stopwatch, timers, alarms, active alerts, `revision`, canonical `alarms_setting`. |
| `WatchState` | server stream `ClockState` | Full snapshot first; a new full snapshot whenever an observable field changes. Consumers converge on the latest snapshot; a missed intermediate transition is not authoritative. |
| `StopwatchStart/Pause/Resume/Reset` | unary `StopwatchState` | Start at zero when idle (no-op while running); pause/resume preserve accumulated monotonic elapsed time; reset returns to idle. |
| `StartTimer{duration_ms,label}` | unary `Timer` | Starts a timer that alerts until explicitly dismissed. |
| `CancelTimer{timer_id}` | unary `Timer` | Cancels a running timer; a completed timer is removed by dismissing its alert, not by cancelling. |
| `ListTimers` | unary | All live/completed timers known to the app. |
| `ListAlarms` | unary | Alarms with schedule, next/last trigger, `ringing`, alert ID, explicit/effective timezone, `enabled` and `one_shot`. |
| `AddAlarm{schedule, expected_revision, timezone, one_shot, disabled, label}` | unary `Alarm` | Adds one normalized five-field dcron expression plus optional `@zone`/`!once`/`!disabled` modifiers. An empty `label` keeps an existing label, or uses `"Alarm"` for a new entry. Adding an existing normalized identity is idempotent. `expected_revision` is the app-settings revision; `0` skips CAS. |
| `DeleteAlarm{alarm_id, expected_revision}` | unary `DeleteAlarmResponse` | `alarm_id` is the stable entry identity `expr [@zone] [!once]`; CAS applies the same way. |
| `SetAlarmEnabled{alarm_id, enabled, expected_revision}` | unary `Alarm` | Disables or re-enables a configured alarm without losing it; disabling dismisses that alarm's active alert and does not change its identity. |
| `DismissAlert{alert_id}` | unary `dismissed` count | Stops one alert; empty `alert_id` dismisses all current timer/alarm alerts. |

Alarm identity is the stable entry identity `expr [@zone] [!once]`, not a
private index, label or the enabled flag. `Alarm` fields: `alarm_id`,
`schedule`, `next_trigger_rfc3339`, `next_trigger_unix_ms`,
`last_triggered_unix_ms`, `ringing`, `alert_id`, `effective_timezone`,
`enabled`, `one_shot`, `timezone` (explicit zone or empty for the device zone),
`label` (display name from the `alarm_labels` map).

Disabling an alarm **explicitly** — through `SetAlarmEnabled`, the friendly
settings editor, or writing `!disabled` via `ApplicationConfigurationService` —
dismisses that alarm's active alert. A one-shot that disarms itself after
firing is different: it keeps its alert ringing until the user dismisses it.
Restarting the clock app (`Stop` + `Launch`, or provider recovery) reloads the
unchanged settings revision without requiring another write; alarms and their
next trigger survive, as do ring settings. An unexpected internal
presentation/capture stream death fails the owning built-in run, and the
supervisor applies its ordinary selected-provider restart/backoff; a normal
close is not treated as a failure. Startup setup runs outside the app's render
locks, so a slow playback-format probe cannot stall the output loop.

`Timer` fields: `timer_id`, `label`, `duration_ms`, `remaining_ms`,
`status` (`RUNNING|COMPLETED|CANCELLED`), `started_unix_ms`,
`finished_unix_ms`.

`ClockState` includes `local_time_rfc3339`, `timezone`, `stopwatch`, `timers`,
`alarms`, `active_alerts` (`alert_id`, kind `TIMER|ALARM`, `source_id`, label,
start), a monotonically increasing `revision`, and `alarms_setting` exactly as
persisted.

#### `alarms` grammar (five-field dcron)

The clock's only alarm storage is the app setting `alarms`, a string. Each
entry is a dcron expression; the canonical list separator is a comma directly
after the completed fifth field and followed by whitespace (`", "`).

```text
alarms  = [ entry { ", " entry } ]
entry   = minute SP hour SP day-of-month SP month SP day-of-week
          [ SP "@" IANA-zone ] [ SP "!once" ] [ SP "!disabled" ]
field   = item { "," item }
item    = "*" | "*" "/" step | number | number "-" number |
          number "-" number "/" step
```

- Modifiers are optional and appear after the fifth field in this order:
  `@Asia/Tokyo` (explicit IANA zone; empty/absent follows the device zone),
  `!once` (fires at the next match and is then persisted as disabled), and
  `!disabled` (stays configured but does not fire). Suffixes never contain
  commas. Canonical example:
  `30 6 * * * @Asia/Tokyo !once, 0 7 * * * !disabled`.

- Numbers are decimal; no month/weekday names, no `L/W/#`, no 7/8-bit
  extensions. Fields accept `*`, `N`, `A-B`, lists `a,b,c` and steps `*/n` or
  `a-b/n`.
- Ranges: minute 0–59, hour 0–23, day-of-month 1–31, month 1–12,
  day-of-week 0–7 (0 and 7 both Sunday).
- Fields are whitespace separated. A comma **inside** a field is never
  followed by whitespace (`0,30 7 * * 1,3,5` is one expression).
- An entry separator is a comma after the completed fifth field and any
  suffixes, canonically followed by one space:
  `0 7 * * 1, 30 8 * * 2-5` is two alarms. Commas inside a field are never
  followed by whitespace; suffixes contain no commas.
- A trailing separator or an empty entry is rejected; an empty string means no
  alarms. Input may omit the space, but the writer always emits `", "`.
- A comma followed by whitespace is always an entry separator, never a field
  list; do not split on every comma.
- When both day-of-month and day-of-week are restricted, classic cron OR
  semantics apply: a minute matches if either field matches. A full-domain
  field such as `*/1` (or `1-31`, `0-7`) is treated as unrestricted for this
  rule, not as a restriction.
- Alarm RPC errors are typed: a stale revision is `ABORTED`, malformed
  schedule/modifiers or values are `INVALID_ARGUMENT`, and storage/write
  failure is `INTERNAL`.
- Leading/trailing whitespace is ignored; expressions are normalized with
  single spaces between fields and field contents preserved.

Examples:

```text
0 7 * * 1                          Mondays at 07:00
30 6 * * 1-5 @Asia/Tokyo           weekdays at 06:30 in Tokyo
0,30 7 * * *                      07:00 and 07:30 daily (minute list)
30 6 * * * @Asia/Tokyo !once, 0 7 * * * !disabled
                                    two entries: a one-shot Tokyo alarm and a disabled 07:00 alarm
```

A naive split on every comma is wrong: it splits `0,30 7 * * *` into two
invalid fragments. Parse five whitespace-separated fields plus optional
suffixes at a time and treat the following comma as the bridge to the next
entry.

The app setting `alarm_labels` is a JSON object mapping each stable alarm
identity to a display label (default `{}`). Schedule semantics stay solely in
`alarms`: `alarm_labels` never creates, deletes or enables an alarm, and an
entry is exposed as `Alarm.label`. `AddAlarm.label` updates the map for that
identity; an empty label keeps an existing label or uses `"Alarm"`.

Legacy settings (`alarm_label`, `alarm_hour`, `alarm_minute`, `alarm_days`,
`alarm_timezone`, `alarm_enabled`) are migrated once to `alarms`/`alarm_labels`
and then removed. The migration preserves all prior semantics: recurrence, time
of day, explicit timezone, one-shot behavior, enabled/disabled state and the
display label.

- `alarm_label` is preserved into `alarm_labels` for the migrated identity; it
  is not dropped.
- `alarm_timezone` → `@Zone`; an invalid zone is logged and falls back to the
  device zone, as before.
- `alarm_days=0` (old one-shot) → a daily expression plus `!once`; it fires at
  the next occurrence, then is persisted as `!disabled` and its alert rings
  until dismissed, matching the old one-shot behavior.
- `alarm_enabled=false` → a `!disabled` entry, preserving the configured alarm
  rather than dropping it.
- The Monday-first weekday bitmask maps to dcron day numbers 1–6/0.

A failed or stale explicit disable does not clear a one-shot's self-disarm
marker; the two transitions are independent. Recurring-alarm firing is
deduplicated by the local wall-clock minute, so a DST fall-back does not ring
the same recurring alarm twice.

Upgrading from an older layout converts flat
`<data-dir>/app-settings/capsula.clock.json` / `capsula.ring.json` files once,
atomically, to the canonical `<data-dir>/app-settings/<id>/settings.json`.
The canonical file always wins when both exist; the flat source is retained,
not deleted.

`DescribeInterface` documentation and method summaries are per-RPC: both
built-in services expose a heading for each RPC (`## RpcName: summary`) plus
the serialized descriptor, not one generic summary for the whole interface.


### Ring service

Target app ID: `capsula.ring`; service:
`capsula.contracts.ring.v1.RingService`.

| RPC | Shape | Behavior |
|---|---|---|
| `GetState` | unary `RingState` | Effective rendered values, owned presentation, `active`, settings revision, schedule, pixel count. |
| `WatchState` | server stream `RingState` | Snapshot first, then one per observable change. |
| `GetSettings` | unary `RingSettingsSnapshot` | Every declared ring setting plus its revision. |
| `UpdateSettings{expected_revision, values, update_fields}` | unary | Partial update; same validation as generic app settings; stale revision returns `ABORTED`. |
| `WatchSettings` | server stream `RingSettingsSnapshot` | Snapshot first, then converges on the newest accepted revision; intermediate revisions may be coalesced for a slow consumer. |

`RingSettings` fields: `effect`, `palette`, `brightness` `[0,1]`, `speed`
`[0.1,4]`, `schedule_enabled`, `schedule_start`, `schedule_end` (`HH:MM` in the
device timezone; the window may cross midnight and equal bounds mean all day).

Effects: `breathe`, `spinner`, `rainbow`, `pulse`, `comet`, `sparkle` and
`static`. `static` is a solid colour and does not animate. `rainbow` passes the
palette's actual colors sampled at 0, 1/3 and 2/3 (`color`, `color_b`, `color_c`)
to the built-in `system.ring.rainbow` preset and rotates/interpolates those
colors around the ring; it invents no unrelated hues, so a mono palette yields
a grayscale rotation and the legacy `spectrum` palette remains rainbow-like.

`palette` is a canonical gradient string, not a preset name:

```text
linear-gradient([<angle>deg,] <stop> [, <stop> ...])
#RGB | #RRGGBB
```

- A bare hex colour is a one-stop gradient, i.e. a solid palette, and is the
  form used by `static`. A gradient is also accepted for `static` and renders
  as its first stop.
- The angle is optional, `0`–`360` degrees, and defaults to 90.
- A stop is `#RGB` or `#RRGGBB` with an optional percentage. At least one and
  at most eight stops are accepted. Positions must be nondecreasing (0–100 %);
  omitted positions are distributed evenly between their specified neighbours,
  with the first defaulting to 0 % and the last to 100 %.
- No CSS variables, colour functions, repeating gradients, URLs or other CSS
  functions are accepted.
- Named legacy palettes (`aurora`, `ocean`, `sunset`, `ember`, `mono`,
  `spectrum`) migrate once to these exact gradients, preserving their colors:
  `aurora` → `linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)`, `ocean` →
  `#00B4FF`/`#0046D2`, `sunset` → `#FF7828`/`#FF3C96`, `ember` →
  `#FF2814`/`#FFAA14`, `mono` → `#FFFFFF`/`#969696`, `spectrum` →
  `#FF46D2`/`#46C8FF`. Presets in the settings UI are UI conveniences that
  serialize to the same canonical form; they are not stored by name.
- The app samples colors per effect from the parsed definition. Parsing happens
  when the setting/presentation changes, not per animation frame; `static`
  shows one solid color and does not animate.

`RingState.active` is false while the configured active window excludes the
current local time; the app then presents an explicit blank baseline frame,
and other captures (volume, mute, alerts) still preempt it. Changing ring
settings never resets baseline presentation ownership.

Clock and ring actions remain available through
`ApplicationConfigurationService.TriggerAction`; the public services are
additive, not a replacement for generic app settings/actions.

## 11. Assets

The daemon owns a read-only asset catalog described by
`<assets-dir>/manifest.json`:

```json
{
  "assets": [
    {
      "id": "reply",
      "path": "/assets/reply.wav",
      "media_type": "audio/wav",
      "sha256": "…",
      "license": "CC0-1.0",
      "attribution": "…",
      "required_base_asset": false
    }
  ]
}
```

- Apps access files through `CAPSULA_ASSETS` (`/assets` in namespace modes),
  never by embedding a host path.
- `AssetService.ListAssets{prefix}` publishes only declared, regular,
  non-symlink files inside the asset root: `asset_id`, canonical `/assets/...`
  path, media type, SHA-256, license, attribution, required flag. It does not
  serve bytes; decode/play through the normal audio services or read the file
  from the read-only mount.
- Absolute paths, traversal, symlinks and files outside the catalog/root are
  rejected. An app that references another asset validates the normalized path
  and registration again.

## 12. Security boundaries (app view)

An app can:

- discover resources, capabilities and installed interfaces;
- open presentations, capture routing resources and release its own captures;
- capture microphone audio only with the indication satisfied, and play audio;
- watch input for its own selected capture;
- read its own settings and actions, update them CAS, and serve routed calls;
- list shared assets and read-only sensors, and query public Bluetooth media
  state (peer identity redacted).

An app cannot:

- listen on a per-app IPC socket, receive a network gRPC connection, or write
  physical hardware directly;
- read/update device settings, change master volume or microphone mute, force
  another capture, read diagnostics/log history, record/train wakewords,
  manage paired devices, launch/stop apps, or access another app's
  configuration, runs, logs or capture through the app-configuration path;
- claim another run/app, claim a built-in run from a supervised process, or
  become an administrator by omitting identity;
- receive microphone audio without the visible+audible indication, request
  another app's pre-roll, or replay an activation ID;
- use the browser WebSocket as a generic AppBus/capture/audio/asset proxy;
- read or change Wi-Fi configuration: there is no app-facing Wi-Fi service. The
  authenticated Web UI and the isolated captive portal own it, and neither
  exposes an app command path;
- escape the settings-UI iframe: the frame is opaque-origin and may only emit
  validated own-app setting patches to its host page, never a command, app bus
  call or another app's settings.

One deliberate exception: the built-in clock and ring publish **public standard
contracts** (`capsula.contracts.clock.v1.ClockService`,
`capsula.contracts.ring.v1.RingService`) that any supervised app may call over
the app bus, including writes such as `RingService.UpdateSettings` and
clock alarm add/delete/enable. These are device-level services, not another
app's private configuration, so they are intentionally reachable; the
"cannot access another app's configuration" rule above applies to the
app-configuration path (`ApplicationConfigurationService`), which stays
own-app scoped.

Static API keys are limited to `GET /api/status` and the public certificate.
The Web UI uses administrator sessions and the same local gRPC socket.

## 13. Packaging and verification checklist

```sh
# Build a package directory with manifest.toml at its root.
capsula -data-dir build/demo app install ./my-app
capsula -data-dir build/demo app list
capsula -data-dir build/demo app start my.app
capsula -data-dir build/demo app runs my.app
capsula -data-dir build/demo app stop <run-id>
capsula -data-dir build/demo app remove my.app
```

Checklist before calling an app ready:

- Manifest validates (`capsula app install` is the first check); every setting
  and action is intentional and typed.
- The entrypoint is the only one, paths stay inside the package, and a native
  app declares architecture/ABI (and `[native_sdk]` if it loads the library).
- The app sends `AppHello{run_id}` then `AppReady`, and handles
  `Disconnected` by exiting cleanly.
- Every inbound routed call ends with `AppCallReply{end}`; every presentation,
  capture, audio stream and conversation has a terminal close/release path.
- Settings updates use revisions and handle `ABORTED` by re-reading; masked
  values are never logged.
- The app's declared interfaces match what it serves, with a descriptor and
  method docs, and discovery reports `available` only while ready.
- Mic capture has a real selected, visible, non-blank indication and handles
  `WATCHDOG_EXPIRED`/`discontinuity`.
- The package is built for the target ABI (aarch64 glibc 2.33 or older for the
  Capsula Mini), and native runtime compatibility is checked at install/launch.

Tests/builds are the integrator's step; this document records contracts, not a
claim that a specific package was exercised on hardware.
