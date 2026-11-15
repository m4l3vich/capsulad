# Framework services

Every framework method lives on the single local Unix socket (`CAPSULA_SOCKET`, mounted at
`/run/capsulad/hal.sock` inside a sandbox). There is no network listener and no per-app port.
Services are grouped by concern; all types named here are `capsula.framework.v1` messages
unless a fully qualified contract name is given.

## Access model

Two kinds of caller exist:

- **App scope** — a supervised application, identified from its `Connect` stream and socket
  peer credentials. An app may address only itself: its own settings, runs, captures, logs and
  presentations.
- **Administrator** — a local daemon client (the CLI, the Web UI, `capsulad-cli`) with no app
  identity. A local administrator may address any app and the device surfaces.

An app that tries an administrator operation receives `PERMISSION_DENIED`. Omitting identity
never promotes a caller: a connection with a sandbox UID or a supervised PID is still an app.

| Service | App access |
|---|---|
| `DeviceService` | full |
| `SettingsService` (device settings) | administrator only |
| `RoutingService` | full for own captures; `ForceRelease` administrator only |
| `PresentationService` | full, app identity required |
| `AudioService` | full for streams and state reads; `SetMasterVolume`/`SetMicrophoneMuted` administrator only |
| `InputService` | own selected touch capture only |
| `AssistantService` | `ListWakewordModels`, `BeginConversation`; wakeword sample/training RPCs administrator only |
| `ApplicationService` | `ListApplications`, `ListRuntimes`, `GetBinaryProvenance`, own runs; `Launch`/`Stop` administrator only |
| `ApplicationConfigurationService` | `Describe` public; `Get`/`Update`/`TriggerAction` own app |
| `AppInterfaceService` | full |
| `AppBusService` | `Connect` (app identity), `Call` (any local caller) |
| `SensorService` | full (read-only) |
| `SystemService` | `GetStatus`, `GetBluetoothStatus`, `ListUnits` (empty) and `ControlUnit` (`UNIMPLEMENTED`) for any caller; the rest administrator only |
| `BluetoothMediaService` | full, peer address and name redacted |
| `DiagnosticsService` | `WatchLogs` own app only; `GetSnapshot` administrator only |
| `AssetService` | catalog only, never bytes |

## DeviceService

| RPC | Shape | Purpose |
|---|---|---|
| `GetInfo` | unary `DeviceInfo` | Identity, firmware and backend availability. |
| `ListResources` | unary `ListResourcesResponse` | Advertised resources with capabilities. |
| `WatchResources` | server stream `ResourceChange` | Resource availability changes. |

`GetInfoRequest` has no fields. `DeviceInfo`:

| Field | Type | Meaning |
|---|---|---|
| `device_id` | string | Stable device identity. |
| `model` | string | Board model name. |
| `board_revision` | string | Board revision. |
| `soc` | string | SoC identifier. |
| `kernel` | string | Kernel release. |
| `firmware` | string | Firmware description. |
| `daemon_version` | string | Running `capsulad` version. |
| `boot_id` | string | Boot identity; changes on restart. |
| `target_platform` | enum | `CAPSULA_MINI_ROCKCHIP_STOCK`, `CAPSULA_MINI_AMLOGIC_STOCK`, `CAPSULA_MINI_AMLOGIC_ALPINE`, or `TARGET_PLATFORM_UNSPECIFIED` for the simulator. The Amlogic values are defined by the protocol but no Amlogic backend exists in this build; see [README.md](README.md). |
| `subsystem_backends` | `BackendAvailability[]` | `{subsystem, backend_id, status, detail}`; status is `BACKEND_AVAILABLE`, `BACKEND_PARTIAL`, `BACKEND_UNAVAILABLE` or `BACKEND_UNVERIFIED`. |

`ResourceInfo`:

| Field | Type | Meaning |
|---|---|---|
| `resource_id` | string | Stable identifier, for example `display.segment.front`. |
| `kind` | enum | `RESOURCE_SEGMENT_DISPLAY`, `RESOURCE_LED_RING`, `RESOURCE_TOUCH`, `RESOURCE_AUDIO_INPUT`, `RESOURCE_AUDIO_OUTPUT`, `RESOURCE_SENSOR`, `RESOURCE_VOICE_ASSISTANT_ROLE`, `RESOURCE_OTHER`. |
| `routing` | enum | `ROUTING_DEFAULT_WITH_CAPTURE`, `ROUTING_SHARED_STREAMS` or `ROUTING_READ_ONLY`. |
| `available` | bool | Usable now. |
| `features` | string[] | Backend and framework hints, for example `fanout`, `preroll`, `mixing`, `ducking`, `unit:°C`, `backend_unverified`. |
| `backend_name` | string | Diagnostics only; never assume its ABI. |
| `segment` / `ring` / `touch` | oneof details | `SegmentCapabilities{digit_count, supports_colon, supports_decimal_points}`, `RingCapabilities{pixel_count, supports_rgb}`, `TouchCapabilities{partial_gestures_available, full_raw_available, coordinates_available, pressure_available, semantic_gestures[], full_panel_available}`. |

`ListResourcesResponse` adds `revision` (uint64). `ResourceChange` carries `revision`,
`resource` and `removed`.

```text
# JSON request for a unary call
capsula.call("capsula.framework.v1.DeviceService", "ListResources", {})
```

## SettingsService

Device-wide settings. Administrator only; apps never read per-turn policy from here (an
activation carries what a turn needs).

| RPC | Shape | Purpose |
|---|---|---|
| `GetSettings` | unary `SettingsSnapshot` | Current values and revision. |
| `UpdateSettings` | unary `SettingsSnapshot` | Compare-and-swap partial update. |
| `WatchSettings` | server stream `SettingsSnapshot` | Snapshot first, then each change. |

`SettingsSnapshot` = `{revision: uint64, values: UserSettings}`.

`UserSettings`:

| Field | Type | Meaning |
|---|---|---|
| `default_app_by_resource` | map<string,string> | Resource ID → provider app ID. |
| `default_assistant_app_id` | string | Selected assistant. |
| `wakeword_model_id` | string | Selected wakeword model. |
| `wakeword_enabled` | bool | Framework wakeword detection. |
| `display_brightness` | map<string,float> | Per resource, `[0,1]`. |
| `master_volume` | float | `[0,1]`. |
| `microphone_muted` | bool | Global mute. |
| `touch_hold_mode` | enum | `TOUCH_HOLD_SUMMON` or `TOUCH_HOLD_TO_TALK`. |
| `muted_microphone_indicator` | `{mode, delay_ms}` | Indicator policy; `mode` is `MUTED_INDICATOR_IMMEDIATE`, `MUTED_INDICATOR_DELAY` or `MUTED_INDICATOR_UNTIL_UNMUTED` (`MUTED_INDICATOR_MODE_UNSPECIFIED` behaves as delay). `delay_ms` applies to the delay mode; `0` means 30000 ms. |
| `timezone` | string | IANA zone; empty follows the firmware. Apps inherit it as `TZ`. |
| `brightness_mode` | map<string,string> | `const` or `auto` per display/ring resource. |
| `bluetooth_incoming_connections` | bool | Accept inbound links. |
| `bluetooth_discoverable` | bool | Advertise and accept new devices; implies incoming connections. |
| `startup_sound_enabled` | bool | Replay the startup cue on every boot. |
| `hostname` | string | Single DNS label for `<hostname>.local`. |

`UpdateSettingsRequest` = `{expected_revision: uint64, values: UserSettings,
update_fields: string[]}`. Only the listed paths change; map fields are replaced as a whole.
A stale revision returns `ABORTED`. Validation failures (volume outside `[0,1]`, unknown
timezone, invalid hostname, unknown field) return `INVALID_ARGUMENT`.

## RoutingService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `GetRouting` | unary `RoutingState` | any local caller | Selection and captures for one resource. |
| `WatchRouting` | server stream `RoutingState` | any local caller | Revisions as they change. |
| `Capture` | request then server stream `CaptureEvent` | app identity | Acquire a temporary routing selection. |
| `ReleaseCapture` | unary `ReleaseCaptureResponse` | app identity, own capture | Release explicitly. |
| `ForceRelease` | unary `ForceReleaseResponse` | administrator | Release another owner's capture. |

`CaptureRequest`:

| Field | Type | Rule |
|---|---|---|
| `resource_id` | string | Required. |
| `presentation_id` | string | Required for display/ring; must be the caller's own presentation of the matching kind. Must be empty for touch and assistant-role. |
| `touch_mode` | enum | Required for touch; must be `TOUCH_CAPTURE_MODE_UNSPECIFIED` otherwise. `TOUCH_PARTIAL_GESTURES` keeps the system volume and microphone gestures; `TOUCH_FULL_RAW` suspends all default touch controls. |

`CaptureInfo` = `{capture_id, resource_id, app_id, run_id, presentation_id,
acquired_monotonic_ns, selected, ready, touch_mode}`.
`CaptureEvent` = oneof `granted{CaptureInfo}` or `changed{selected, reason}`.
`RoutingState` = `{resource_id, configured_default_app_id, selected_presentation_id,
selected_app_id, captures[], revision}`; `captures` is in acquisition order.

```text
# First message of the Capture stream (JSON)
{"resourceId": "light.ring.main", "presentationId": "prs_…"}
# Stream replies
{"granted": {"capture": {"captureId": "cap_…", "selected": true, "ready": true}}}
{"changed": {"selected": true, "reason": "capture"}}
```

Errors: unknown resource `NOT_FOUND`; missing mode, missing/foreign presentation or mode on a
non-touch resource `INVALID_ARGUMENT`; full capture on a panel without full support
`FAILED_PRECONDITION`.

## PresentationService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `Open` | bidi: `PresentationRequest` → `PresentationEvent` | app identity | Publish output on one resource. |
| `ListPresets` | unary `ListPresentationPresetsResponse` | any local caller | Built-in effects and faces. |

`PresentationRequest` is a oneof: `open{resource_id}`, `frame{...}`, `close{}`. The first
message must be `open`. `PresentationFrame` is a oneof `segment`, `ring`, `ring_effect`,
`segment_face`, `blank`. `PresentationEvent` is a oneof `ready{presentation_id, resource_id,
has_valid_frame}` or `visibility{visible, reason}`.

`ListPresets` takes `{resource_id}` and returns `presets[]` of
`{preset_id, kind, display_name, description, parameters[], supported_resource_ids}`; `kind` is
`PRESET_RING_EFFECT` or `PRESET_SEGMENT_FACE`, and each parameter is
`{name, description, value_type, default_value, min?, max?}`.

## AudioService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `ListEndpoints` | unary `ListAudioEndpointsResponse` | any local caller | Advertised endpoints and formats. |
| `Capture` | request then server stream `CaptureAudioEvent` | app identity, indication required | Microphone PCM. |
| `Playback` | bidi `PlaybackRequest` → `PlaybackEvent` | app identity | Speaker PCM. |
| `GetAudioState` | unary `AudioState` | any local caller | Mixer and mute state. |
| `SetMasterVolume` | unary `AudioState` | administrator | Persisted volume. |
| `SetMicrophoneMuted` | unary `AudioState` | administrator | Persisted mute. |

`AudioEndpointInfo` = `{endpoint_id, direction, supported_formats[], processing_tap}`.
`AudioFormat` = `{sample_format, sample_rate_hz, channels, channel_names[]}`; formats are
`PCM_S16LE` and `PCM_F32LE`. `AudioChunk` = `{sequence, first_frame, monotonic_time_ns,
frame_count, pcm, discontinuity, dropped_frames_before}`. `AudioNotice` = `{code, detail}`.

Endpoints: `audio.input.microphone` (processed tap, advertises `framework.dsp.v1` when the DSP
chain is active), `audio.input.microphone.raw` (unprocessed vendor stream),
`audio.output.speaker`.

`CaptureAudioRequest` = `{endpoint_id, preferred_format, packet_ms, activation_id,
conversation_id}`. `PlaybackRequest` is a oneof `open{endpoint_id, format,
target_latency_ms, conversation_id}`, `chunk{AudioChunk}` or `control{PLAYBACK_FLUSH|
PLAYBACK_PAUSE|PLAYBACK_RESUME}`.

Errors: microphone capture without a satisfied indication is `FAILED_PRECONDITION`; an
unusable endpoint is `FAILED_PRECONDITION`; a missing audio backend yields an empty endpoint
list rather than an error.

The full stream contract is in [audio.md](audio.md).

## InputService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `WatchInput` | server stream `InputEvent` | own selected touch capture | Keys, gestures and raw reports. |

`WatchInputRequest` = `{resource_id, capture_id}`. `resource_id` may be empty or
`touch.panel.main`. `capture_id` must be the caller's **currently selected** touch capture;
anything else is `FAILED_PRECONDITION`, which is what prevents an app from subscribing to the
system volume or mute gestures.

`InputEvent` = `{resource_id, monotonic_time_ns, oneof key{key, pressed} |
gesture{gesture_id, phase} | raw{RawTouchReport}}`. `RawTouchReport` = `{contacts[],
backend_sample, backend_sample_format, coordinates_valid}`; each contact is `{contact_id,
x_normalized, y_normalized, pressure?, touching}`.

Details, including the five panel keys and gesture names, are in
[output-input.md](output-input.md).

## AssistantService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `ListWakewordModels` | unary | any local caller | Installed wakeword models. |
| `BeginConversation` | unary request → server stream `ConversationEvent` | any local caller | Marks a conversation lifetime for audio ducking. |
| `RecordWakewordSample` | unary | administrator | Record a training take. |
| `ListWakewordSamples` | unary | administrator | List takes. |
| `DeleteWakewordSample` | unary | administrator | Delete a take. |
| `TrainWakewordClassifier` | server stream `TrainWakewordEvent` | administrator | On-device training with quality gates. |

`WakewordModelInfo` = `{model_id, display_name, language, engine_id, available}`.

`BeginConversationRequest` = `{activation_id}`. The daemon mints a new `conversation_id`,
registers the conversation for the caller's identity (empty for a local administrator), replies
`ConversationStarted{conversation_id}`, and keeps it open until the caller closes the stream. It
is a ducking lifetime marker, not a priority or activation check: audio opened with that
`conversation_id` stays at normal gain while other playback is ducked.

Assistant activations themselves do not arrive here. They arrive as `AssistantActivation` on
the app bus; see [lifecycle.md](lifecycle.md#assistant-activations).

## ApplicationService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `ListApplications` | unary | any local caller | Installed apps with lifecycle metadata. |
| `ListRuntimes` | unary | any local caller | Which runtimes the daemon can run. |
| `GetBinaryProvenance` | unary | any local caller | Computed provenance status. |
| `Launch` | unary | administrator | Start a run or deliver an invocation. |
| `Stop` | unary | administrator | Graceful stop of one run. |
| `ListRuns` | unary | app scope (own app) or administrator | Runs. |
| `WatchRuns` | server stream | app scope (own app) or administrator | Run state changes. |

`InstalledApplication`:

| Field | Type |
|---|---|
| `app_id`, `display_name`, `version` | string |
| `start_on_boot`, `single_instance`, `background`, `restart_on_failure` | bool |
| `launch_arguments` | `AppArgumentInfo[]` = `{name, description, value_type, required}` |
| `provides` | string[] (UI hints) |
| `interfaces` | `DeclaredAppInterface[]` = `{grpc_service, interface_version, summary}` |
| `entrypoint` | `{runtime, relative_path, cpu_architecture, binary_abi, framework_sdk_version}` |
| `github_attestation` | `{bundle_path, subject_path, expected_repository, expected_workflow}` |
| `system_app`, `can_stop`, `can_uninstall` | bool — server-owned, never inferred from the ID |

`RuntimeAvailability` = `{runtime, available, implementation_version, detail}`.

`LaunchRequest` = `{app_id, arguments[]}` with `LaunchArgument{name, value}`. `LaunchResult` =
`{run_id, spawned_new_process, invocation_id}`; `invocation_id` is also set when a singleton app
received an invocation instead of spawning.

`AppRun` = `{run_id, app_id, pid, state, started_monotonic_ns, exit_code, status_detail,
runtime}`; `state` is `APP_STARTING`, `APP_READY`, `APP_STOPPING`, `APP_EXITED` or
`APP_FAILED`. `AppRunEvent` wraps one `AppRun`.

An app calling `ListRuns`/`WatchRuns` for another `app_id` gets `PERMISSION_DENIED`.

`BinaryProvenanceResult` = `{app_id, status, detail, subject_sha256_hex, verified_repository,
verified_repository_id, verified_workflow, verified_commit_sha, verified_at_unix_ms,
trusted_root_current}`. Status values are `PROVENANCE_NOT_PROVIDED`,
`PROVENANCE_VERIFIED`, `PROVENANCE_VERIFICATION_FAILED`, `PROVENANCE_UNVERIFIED` and
`PROVENANCE_UNSUPPORTED_RUNTIME`.

## ApplicationConfigurationService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `Describe` | unary `AppConfigurationSchema` | any local caller | Declared schema from the manifest. |
| `Get` | unary `AppConfigurationSnapshot` | own app or administrator | Values and revision. |
| `Update` | unary `AppConfigurationSnapshot` | own app or administrator | Compare-and-swap changes. |
| `TriggerAction` | unary `TriggerAppActionResult` | own app or administrator | Fire a declared action. |

`AppSettingDefinition` = `{key, label, description, type, default_value_json, choices[],
required, restart_required, has_min, min, has_max, max}`; `type` is `SETTING_TEXT`,
`SETTING_TEXT_MASKED`, `SETTING_BOOLEAN`, `SETTING_INTEGER`, `SETTING_NUMBER` or
`SETTING_SELECT`.

`AppConfigurationSnapshot` = `{app_id, revision, values[]}` with
`AppSettingState{key, value_json, has_value}`. `text_masked` values are always redacted here
(`value_json` empty).

`UpdateAppConfigurationRequest` = `{app_id, expected_revision, changes[]}`; each
`AppSettingChange` is `{key, value_json, clear}`. A stale revision is `ABORTED`, an undeclared
key or wrong type is `INVALID_ARGUMENT`, and clearing a masked value requires `clear: true`.
A successful update pushes `AppSettingsChanged` to the owning app.

`TriggerAppActionRequest` = `{app_id, action_id}`; the result carries
`action_invocation_id`, which also arrives at the app as `AppActionInvoked`. An unknown action
is `FAILED_PRECONDITION`.

Details are in [settings-actions.md](settings-actions.md).

## AppInterfaceService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `ListInterfaces` | unary | any local caller | Declared interfaces across apps. |
| `DescribeInterface` | unary `AppInterfaceDescription` | any local caller | Descriptor and documentation. |

`ListAppInterfacesRequest` = `{app_id?, running_only}`.
`AppInterfaceInfo` = `{app_id, run_id, grpc_service, interface_version, available, summary}`;
`available` is true only while the selected run has sent `AppReady`.

`DescribeAppInterfaceRequest` = `{app_id, grpc_service}`.
`AppInterfaceDescription` = `{info, file_descriptor_set (bytes), documentation_markdown,
methods[]}`; each method is `{rpc_name, summary, semantics_markdown, request_example_json,
response_example_json}`.

## AppBusService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `Connect` | bidi `AppOutbound` → `AppInbound` | app identity | The app's single long-lived channel. |
| `Call` | bidi `RoutedCallRequest` → `RoutedCallResponse` | any local caller | Relay one call to a target app. |

See [app-bus.md](app-bus.md) for the message tables, identity rules, deadlines and limits.

## SensorService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `ListSensors` | unary `ListSensorsResponse` | any local caller | `SensorInfo{sensor_id, unit}` list. |
| `ReadSensor` | unary `SensorSample` | any local caller | One reading. |
| `WatchSensor` | server stream `SensorSample` | any local caller | Periodic readings. |

`WatchSensorRequest` = `{sensor_id, minimum_interval_ms}`; a zero interval means one second.
`SensorSample` = `{sensor_id, value, unit, monotonic_time_ns}`. An unknown sensor is
`NOT_FOUND`; a platform without sensors returns an empty list and `NOT_FOUND` on read.

## SystemService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `GetStatus` | unary `SystemStatus` | any local caller | `{uptime_ms, boot_id, daemon_version}`. |
| `GetBluetoothStatus` | unary `BluetoothStatus` | any local caller | Adapter state. |
| `SetBluetoothDiscoverable` | unary `BluetoothStatus` | administrator | Advertise and accept new devices. |
| `SetBluetoothIncomingConnections` | unary `BluetoothStatus` | administrator | Accept inbound links. |
| `ListPairedDevices` | unary | administrator | `PairedDevice{address, name, connected, trusted}`. |
| `UnpairDevice` | unary | administrator | `{removed}`. |
| `GetWebUiSecurity` | unary | administrator | TLS and API-key state. |
| `ListUnits` | unary | any local caller | Backend system units; returns an empty list. |
| `ControlUnit` | unary | any local caller | `UNIMPLEMENTED` on every backend. |

`BluetoothStatus` = `{present, powered, discoverable, adapter, detail, incoming_connections,
name}`. Discoverable implies incoming connections; turning incoming connections off also clears
discoverability. A platform without a Bluetooth backend reports `present=false`.

`WebUiSecurityStatus` = `{https_enabled, client_certificate_required, bootstrap_complete,
listen_addresses[], server_certificate_expiry_unix_ms, server_certificate_sha256[],
active_api_keys, admin_password_set}`. Secrets and key material are never returned.

## BluetoothMediaService

Exposes the connected A2DP source's AVRCP state to apps.

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `GetState` | unary `BluetoothMediaState` | any local caller | Current state. |
| `WatchState` | server stream | any local caller | State first, then each change. |
| `Command` | unary `BluetoothMediaState` | any local caller | `PLAY`, `PAUSE`, `TOGGLE`, `NEXT`, `PREVIOUS`. |

`BluetoothMediaState` = `{present, playing, device_address, device_name, title, artist, album,
duration_ms, position_ms, revision, detail}`. For a supervised app caller
`device_address` and `device_name` are empty; metadata fields remain.

Errors: an absent adapter is `UNAVAILABLE`; an unknown command value is `INVALID_ARGUMENT`;
no connected source is `FAILED_PRECONDITION`.

## DiagnosticsService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `GetSnapshot` | unary `DiagnosticsSnapshot` | administrator | Device-wide state. |
| `WatchLogs` | server stream `LogRecord` | own app or administrator | Structured logs. |

`WatchLogsRequest` = `{app_id}`. An app may pass its own ID or empty (which is treated as its
own); any other ID is `PERMISSION_DENIED`, and framework records stay administrator-only.

`LogRecord` = `{monotonic_time_ns, component, app_id, severity, message}`.
`DiagnosticsSnapshot` = `{routed_resources[] (RoutingState), runs[] (AppRun),
audio_streams[] ({stream_id, app_id, endpoint_id, input, indication_active,
opened_monotonic_ns}), monotonic_time_ns}`.

`capsulad-cli logs -app ID -n N` is the same stream with a short read window.

## AssetService

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `ListAssets` | unary `ListAssetsResponse` | any local caller | Catalog of the shared asset store. |

`ListAssetsRequest` = `{prefix}`; an empty prefix lists everything and a `/assets/…` prefix is
accepted as an alias. `SharedAssetInfo` = `{asset_id, path, media_type, sha256_hex, size_bytes}`;
`path` is the canonical `/assets/<asset_id>` location. Contents are never returned: read the
file from the mount named by `CAPSULA_ASSETS`.

See [assets.md](assets.md).

## Request examples

These are canonical protobuf JSON payloads: lowerCamelCase names, enum **names**, base64
`bytes`, and 64-bit integers as strings. Raw protobuf callers send the same messages encoded
normally. `updateFields` is the exception: it carries **proto field paths** in snake_case —
`master_volume`, `microphone_muted`, `touch_hold_mode`, `muted_microphone_indicator` — not the
JSON spelling used inside `values`.

```json
// SettingsService.UpdateSettings (administrator): only listed paths change
{"expectedRevision": "3", "updateFields": ["master_volume"], "values": {"masterVolume": 0.4}}

// PresentationService.Open: first message, then frames on the same stream
{"open": {"resourceId": "display.segment.front"}}
{"frame": {"segment": {"cells": [{"mask": 63}, {"mask": 6}], "colon": true}}}
{"frame": {"ringEffect": {"effectId": "system.ring.spinner", "arguments": []}}}

// RoutingService.Capture: first message of the stream
{"resourceId": "light.ring.main", "presentationId": "prs_…"}
{"resourceId": "touch.panel.main", "touchMode": "TOUCH_PARTIAL_GESTURES"}

// InputService.WatchInput: must be the caller's selected capture
{"resourceId": "touch.panel.main", "captureId": "cap_…"}

// AudioService.Capture: first message; the fast path then delivers raw PCM
{"endpointId": "audio.input.microphone", "packetMs": 20,
 "preferredFormat": {"sampleFormat": "PCM_S16LE", "sampleRateHz": 48000, "channels": 2}}

// AudioService.Playback: open, then chunks, then close the request side to drain
{"open": {"endpointId": "audio.output.speaker", "targetLatencyMs": 80,
          "format": {"sampleFormat": "PCM_S16LE", "sampleRateHz": 48000, "channels": 2}}}

// ApplicationService.Launch (administrator)
{"appId": "org.example.thermostat", "arguments": [{"name": "target", "value": "23"}]}

// ApplicationConfigurationService.Update: compare-and-swap on the app's revision
{"appId": "org.example.thermostat", "expectedRevision": "7",
 "changes": [{"key": "target", "valueJson": "23"}]}

// ApplicationConfigurationService.TriggerAction
{"appId": "org.example.thermostat", "actionId": "redraw"}

// SensorService.ReadSensor: the id comes from ListSensors; the simulator
// reports sensor.ambient_light
{"sensorId": "sensor.ambient_light"}

// BluetoothMediaService.Command
{"command": "BLUETOOTH_MEDIA_PLAY"}

// AssetService.ListAssets
{"prefix": "sounds/"}
```

## Calling conventions

- Unary calls are ordinary request/response; server streams send one message per event.
- The first message on a stream is always the open/start request (`OpenPresentation`,
  `CaptureAudioRequest`, `RoutedCallStart`).
- A stream ends normally by closing it. A presentation is released by sending
  `ClosePresentation` or by closing its stream; a capture is released by `ReleaseCapture`, by
  closing its stream, or when the owning run ends.
- `ABORTED` means a stale revision, `FAILED_PRECONDITION` means a wrong state, `UNAVAILABLE`
  means a missing target or shutting-down daemon. Never retry `INVALID_ARGUMENT` unchanged.
- All `*_monotonic_ns` values are monotonic since boot; `*_unix_ms` and `*_rfc3339` are wall
  clock.