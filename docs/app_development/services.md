# Framework services

Every framework method lives on the single local Unix socket (`CAPSULAD_SOCKET`, mounted at
`/run/capsulad/hal.sock` inside a sandbox). There is no network gRPC listener and no per-app
framework port. App-facing inbound TCP/UDP is a separate, daemon-allocated listener descriptor,
not a service on this socket; see [network.md](network.md#daemon-mediated-listeners).
Services are grouped by concern; all types named here are `capsulad.framework.v1` messages
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
The one identity that is never derived from the socket peer is a compiled-in system app: it
runs inside the daemon, so its run records the daemon's own PID — the same PID as every
connection the daemon opens to its own socket, including the Web UI's. A virtual run is
claimed only by its stable run ID together with the daemon's per-boot internal token, which
only the daemon itself holds; a peer that merely shares the daemon PID (the Web UI,
`capsulad-cli`, a framework cue) is an administrator, never `capsulad.clock` or `capsulad.ring`.

| Service | App access |
|---|---|
| `DeviceService` | full |
| `SettingsService` (device settings) | administrator only |
| `RoutingService` | full for own captures; `ForceRelease` administrator only |
| `PresentationService` | full, app identity required |
| `AudioService` | full for streams and state reads; `SetMasterVolume`/`SetMicrophoneMuted` administrator only |
| `InputService` | own selected touch capture only |
| `AssistantService` | `ListWakewordModels`, `BeginConversation`; wakeword sample/training RPCs administrator only |
| `ApplicationService` | `ListApplications`, `ListRuntimes`, `GetBinaryProvenance`, own runs; `Launch`/`Stop`/`InstallApplication`/`UninstallApplication` administrator only |
| `ApplicationConfigurationService` | `Describe` public; `Get`/`Update`/`TriggerAction` own app |
| `AppInterfaceService` | full |
| `AppBusService` | `Connect` (app identity), `Call` (any local caller) |
| `SensorService` | full (read-only) |
| `SystemService` | `GetStatus`, `GetBluetoothStatus`, `ListUnits` (empty) and `ControlUnit` (`UNIMPLEMENTED`) for any caller; the rest administrator only |
| `BluetoothMediaService` | full, peer address and name redacted |
| `DiagnosticsService` | `WatchLogs` own app only; `GetSnapshot` administrator only |
| `AssetService` | catalog only, never bytes |
| `DiscoveryService` | `Publish`/`Unpublish`/`Browse` require a supervised app identity; publications are scoped to the caller's run |

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
| `target_platform` | enum | `CAPSULA_MINI_ROCKCHIP_STOCK`, `CAPSULA_MINI_AMLOGIC_STOCK` or `CAPSULA_MINI_AMLOGIC_ALPINE`, or `TARGET_PLATFORM_UNSPECIFIED` for the simulator. The Amlogic values are reported when the daemon runs `-platform amlogic-stock` or `-platform amlogic-alpine`; each board's subsystem backends and statuses differ, so read `subsystem_backends` rather than inferring capability from this enum. |
| `subsystem_backends` | `BackendAvailability[]` | `{subsystem, backend_id, status, detail}`; status is `BACKEND_AVAILABLE`, `BACKEND_PARTIAL`, `BACKEND_UNAVAILABLE` or `BACKEND_UNVERIFIED`. |
| `hardware_serial` | string | The SoC serial as the host kernel reports it (`/proc/cpuinfo` `Serial` on ARM), read once by the daemon at startup. Empty when the host cannot report it. It is not a secret and it is not the vendor-compatible `device_id`. |

An app sandbox mounts `/proc` only when the daemon gives it a PID namespace, and the stock
device does not, so `/proc/cpuinfo` is unreadable inside an app. `hardware_serial` is
therefore the supported way for an app to obtain the SoC serial the framework can read;
never parse `/proc` yourself, and never substitute `device_id` for a serial value. `hardware_serial`
is authoritative in the protocol, but apps whose native runtime predates this field must use
`CAPSULAD_DEVICE_SERIAL`.

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
capsulad.call("capsulad.framework.v1.DeviceService", "ListResources", {})
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
| `wakewords` | `WakewordBinding[]` | Per-word configuration: `{model_id, app_id, enabled}`. Absent means the legacy state (every installed word is live under `wakeword_enabled` and targets the selected assistant); present means authoritative, one entry per installed word, and a word without an entry is disabled. `app_id` empty targets the selected assistant. |
| `wakeword_enabled` | bool | Global framework wakeword detection switch. A word activates only when this switch, its own `enabled` and its availability all hold. |
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
update_fields: string[]}`. Only the listed paths change; map fields and `wakewords` are replaced as a
whole. A repeated field cannot distinguish an absent list from an empty one on the wire, so an update
that owns `wakewords` with no entries states the explicit "no word is live" list and leaves every
installed word disabled; an update that does not own the field leaves the current state alone. A stale
revision returns `ABORTED`. Validation failures (volume outside `[0,1]`, unknown timezone, invalid
hostname, unknown field, more than eight wake words, a duplicate `model_id`) return
`INVALID_ARGUMENT`.

The retired `wakeword_model_id` field (tag 3) is reserved, not reused. It was a selector that never
gated a detection, so it carried no behavior to preserve: per-word enablement and the app binding are
`wakewords`, and a client that still writes the old field is ignored.

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
| `TrainWakewordClassifier` | server stream `TrainWakewordEvent` | administrator | On-device training with quality gates, for a new word or one existing word in place. |
| `DeleteWakewordClassifier` | unary | administrator | Remove one trained word's artifact and settings entry. Recordings are kept. |

`WakewordModelInfo` = `{model_id, display_name, language, engine_id, available, metrics[],
threshold, enabled, app_id, detail}`. `metrics` is the gate certificate recorded when that model was
trained and is empty for an artifact that predates it; `threshold` is the calibrated threshold, `0`
when unknown. Both are read from the installed artifact and are never recomputed by a client.
`enabled` is the **effective** state (global switch, the word's own setting and availability already
combined by the daemon), `app_id` is the app that word activates (empty means the selected
assistant) and `detail` explains why a listed word cannot be used. The list is readable by any local
caller and is decorated by the daemon, so a client never joins the registry and the settings itself.
`model_id` is the artifact id and the file name stem under `<data-dir>/wakeword/models`.

`WakewordGateMetrics` = `{split, word_recordings, detected, recall, false_activations}`. `split` is
`tune`, `final_test`, `auxiliary_tune` or `auxiliary_final_test`. The auxiliary splits are the
external CNEG corpus: off-policy measurements that veto installation on a false activation but are
not the processed-audio certificate.

`TrainWakewordClassifierRequest` = `{display_name, language, seed, word_sample_ids,
room_sample_ids, speech_sample_ids, false_sample_ids, model_id}`. Only the submitted IDs are
trained; an omitted take is never pulled in implicitly. `model_id` empty creates a new word with a
fresh id; an installed id replaces exactly that artifact and leaves every other word, its
enablement, its binding and its gate untouched. A newly installed word is switched off until the
word's own `enabled` is set. Training a new word when eight artifacts are already installed is
refused before the fit starts (`configuration`, so `FAILED_PRECONDITION`): the settings layer accepts
at most eight entries, so a ninth artifact could never be named by a binding.

`DeleteWakewordClassifierRequest` = `{model_id}`. It removes the artifact and, when `wakewords` is
present, the entry; it never deletes recordings, which are shared training data. Deleting the last
word leaves the explicit empty list, so the next installed word starts switched off. It returns
`FAILED_PRECONDITION` while a training run is active, `INVALID_ARGUMENT` for an id that cannot
name an artifact and `INTERNAL` for a store fault. Installing or deleting any word restarts detection
for all words, which costs about two seconds of warm-up before any word can score again.

`TrainWakewordEvent` = `{stage, fraction, log, classifier, metrics[], failure}`. The final success
event sets `classifier` (`WakewordClassifierInfo`) and `metrics`. A refused run emits one terminal
`stage = "failed"` event with `failure` and then ends the stream with a gRPC error status, so a
client that only waits for the stream result does not hang:

`WakewordTrainingFailure` = `{causes[], takes[], splits[], threshold, detail}`.
`causes` holds distinct stable strings, most significant first: `invalid_request`, `configuration`,
`too_few_word_takes`, `too_few_room_takes`, `too_few_speech_takes`, `take_too_short`,
`unusable_take`, `duplicate_takes`, `short_context`, `no_positive_windows`, `no_negative_windows`,
`insufficient_heldout`, `tune_gate_failed`, `final_gate_failed`, `auxiliary_gate_failed`,
`corpus_missing`, `corpus_unusable`, `fit_failed` or `internal`. `WakewordFailingTake` =
`{partition, index, sample_id, cause, detail}`, where `partition` is `word`, `room`, `speech` or
`false_activation` and `index` is the 1-based position in the submitted selection for that
partition (`sample_id` is empty; the trainer reports positions).

Error codes: `INVALID_ARGUMENT` for an unusable or insufficient selection, `FAILED_PRECONDITION`
for gate, policy, corpus or configuration refusals, `INTERNAL` for a trainer or artifact fault,
`CANCELED` when the request was cancelled before the atomic commit, and `DEADLINE_EXCEEDED` when the
call exceeded its deadline. A refused run installs nothing and leaves the installed model untouched.

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
| `InstallApplication` | client stream `InstallApplicationRequest` → `InstallApplicationResult` | administrator | Install or atomically replace one `.tar.gz` package. |
| `UninstallApplication` | unary `UninstallApplicationRequest` → `UninstallApplicationResult` | administrator | Remove one installed package. |

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
| `system_app`, `can_stop`, `can_uninstall` | bool — server-owned, never inferred from the ID. `can_uninstall` is true for a supervised package and false for a compiled-in system app; a running app still refuses removal at the call. |

`RuntimeAvailability` = `{runtime, available, implementation_version, detail}`.

Package administration is part of this service because the browser backend and `capsulad-cli`
use the same local control plane as every other client; there is no second administration
channel. A supervised app calling either RPC is `PERMISSION_DENIED` before a byte is read or a
path is touched.

`InstallApplicationRequest` is a client stream whose **first** message is
`InstallApplicationOpen{file_name}` (diagnostics only — the daemon never installs from a
client-named location) and whose remaining messages carry `content` bytes of one gzip-compressed
tar. Chunk boundaries carry no meaning; the concatenation is the artifact described in
[packaging-deployment.md](packaging-deployment.md#artifact-format). The compressed stream is
bounded at 65 MiB and the unpacked tree at the pinned package budget, so a client cannot make the
device buffer an unbounded upload.

The daemon ends an upload with no progress for 60 s (no message received and no buffered bytes
consumed), or one that exceeds 30 minutes in total, with `DEADLINE_EXCEEDED`. A timeout is
reported only before the install reaches its commit point, so nothing has been swapped,
"nothing was installed" is true and the staging slot is released; the package may already be
unpacked and verified at that point. Once the commit begins (the validated package is being
swapped in), the call no longer times out: it waits for and returns the real result, because a
committed install cannot be reported as abandoned. Only one upload is unpacked at a time (the
78.5 MiB data partition cannot hold two packages beside the installed set), so a second upload
waits up to 30 s and is then refused with `RESOURCE_EXHAUSTED`.

`InstallApplicationResult` = `{application, replaced, previous_version}`; `previous_version` is
empty for a fresh install and otherwise the version that installation carried before the call, so
a caller can distinguish an update from a reinstall of the same version.

Semantics an app author can rely on:

- The artifact is validated (root `manifest.toml`, relative regular files and directories only,
  no links, per-file/total/entry limits) before anything installed is touched.
- A declared `[native_sdk]` requirement is checked against the installed runtime; a mismatch is
  `FAILED_PRECONDITION`.
- Replacing or removing a package whose app is **running** is `FAILED_PRECONDITION`
  (`app <id> is running (run <run-id>); stop it before …`). Stop the run first.
- A compiled-in system app ID cannot be installed over, replaced or removed
  (`FAILED_PRECONDITION`); `can_uninstall` is false for those entries.
- The swap is atomic: a failed install leaves the previous installation intact and never exposes
  a partial package. An accepted install or removal is visible to `ListApplications` immediately
  — the daemon reloads its registry in place — and a newly installed or replaced app is never
  started implicitly.
- Removal keeps the app's stored settings (`<data-dir>/app-settings/<app-id>/settings.json`) and
  never rewrites a stored default-provider selection; reinstalling the same app ID finds them
  again.
- The daemon serialises install/remove against `Launch`, so a process is never started from a
  package tree that is being replaced or removed.

`UninstallApplicationRequest` = `{app_id}`; `UninstallApplicationResult` = `{app_id}`. An unknown
app is `NOT_FOUND`.

`LaunchRequest` = `{app_id, arguments[]}` with `LaunchArgument{name, value}`. `LaunchResult` =
`{run_id, spawned_new_process, invocation_id}`; `invocation_id` is also set when a singleton app
received an invocation instead of spawning.

`AppRun` = `{run_id, app_id, pid, state, started_monotonic_ns, exit_code, status_detail,
runtime}`; `state` is `APP_STARTING`, `APP_READY`, `APP_STOPPING`, `APP_EXITED` or
`APP_FAILED`. `pid` is the supervised child process and is `0` for a compiled-in run, which has
no child process of its own (`runtime = RUNTIME_BUILTIN`); never treat `pid` as an app identity.
`AppRunEvent` wraps one `AppRun`.

An app calling `ListRuns`/`WatchRuns` for another `app_id` gets `PERMISSION_DENIED`. An
administrator listing is the complete run table: every live run appears once under its own
`app_id` with its real `state`, and the compiled-in `capsulad.clock` and `capsulad.ring` runs are
ordinary entries with their stable `builtin:<app_id>` run IDs. A run whose state is not
`APP_READY` (starting, stopping, exited, failed) is never "running the app": it cannot serve its
declared interfaces, and a Web UI provider needs `APP_READY`.

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

A routed `Call` is a local request with a daemon-derived caller identity, but no per-method
authorization: the daemon decides which declared interfaces are reachable, and the target's
handler receives `IncomingAppCall.caller_run_id` (Go: `ServerCall.CallerRunID()`) — a supervised
run id, `pid:<pid>` for a non-app local caller, or empty. A handler that serves a privileged
interface must authorize that identity against framework run data and refuse otherwise. See
[security.md](security.md#routed-calls-and-trust).

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
duration_ms, position_ms, revision, detail, direction_known, direction_playing}`. For a supervised
app caller `device_address` and `device_name` are empty; metadata fields remain.

`playing` is the source-published AVRCP status. A source that exposes only `MediaControl1`, or
whose player does not publish `Status`, reports `playing=false` even while audio is flowing, so it
is not a reliable "is it playing" answer. The framework resolves that question itself, from the same
evidence its own media key and toggle use — a command this daemon sent in the last two seconds, the
owned A2DP transport's packet receipts, `MediaPlayer1.Status`, then the transport state — and
publishes the result as `direction_playing`, with `direction_known` reporting whether a direction
could be resolved at all (`false` when no source is connected, or for a backend that exposes only
its exact state). A consumer that shows a Play/Pause choice should use `direction_playing` when
`direction_known` is true and fall back to `playing` otherwise; that is what the daemon's Web UI
does. Neither field changes the meaning of `Command`: explicit `PLAY`, `PAUSE`, `NEXT` and
`PREVIOUS` are applied as sent, and only `TOGGLE` consults the resolved direction.


`detail` is bounded display text, not playback state and not an error: it explains why a field is
missing. A connected source without an A2DP transport reports `audio link is not A2DP`, and a source
that does not expose AVRCP absolute volume reports `the source does not expose AVRCP absolute
volume` (it cannot report a level, so its slider drives nothing; the speaker's master still applies).
Clients must treat the string as opaque.

This service exposes no volume control, and that is deliberate: listening level is the framework
master. Volume is two-way for display only. A level the source reports is applied through the
canonical master path, and a change made on the speaker is written back to the source's AVRCP
register so the source's own level display can follow it — but that write is a best-effort mirror
(BlueZ accepts it; whether the peer shows or applies it is the peer's decision), and the local
attenuation stays the only gain stage: writing the register as an additional gain stage attenuated
the same signal twice on this hardware. Because a source reports the level it just received, a report
within one AVRCP step of what the daemon wrote in the last 900 ms is suppressed as an echo; a report
further away is the user's slider and is adopted at once, even while a write is in flight, so a phone
slider drag moves the speaker immediately rather than when the drag ends. The memory is bounded to
the last four writes and is cleared on connect and disconnect. A source without a writable register
changes nothing for the speaker: the refused write is logged once per session and the level stays
local. An app that needs the current level reads `AudioService.GetAudioState`, and setting it is
administrator-only.

The daemon also publishes this source in the browser protocol (`capsulad.web.v1`) as a media
provider, and that provider is framework-owned rather than an app: `MediaProviderView.kind` is
`MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA` with an empty `app_id`, one player `bluetooth`
while a source is connected and none otherwise, and its commands are this service's `Command`.
An installed or compiled-in app that declares `capsulad.contracts.media.v1.MediaPlayerService`
is listed as `MEDIA_PROVIDER_KIND_APP` with its `app_id`. The kind is additive: a client that
predates it reads `MEDIA_PROVIDER_KIND_APP` and addresses providers by `app_id`. `BluetoothMediaService`
itself is unchanged by that view — it has no app identity, no run and no declared interface, so an
app calling it never sees a provider kind.

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
file from the mount named by `CAPSULAD_ASSETS`.

See [assets.md](assets.md).

## DiscoveryService

mDNS publication and browsing through the daemon's Avahi responder. Avahi owns multicast DNS
and port 5353; the daemon never binds it and an app never runs its own responder. All three
methods require a supervised app identity verified from the connection — there is no
administrator path in this contract. Field-level limits, publication lifetime and browse event
shape are in [network.md](network.md#mdns-publication-and-browsing).

| RPC | Shape | Access | Purpose |
|---|---|---|---|
| `Publish` | unary `PublishServiceResponse` | supervised app | Advertise one service instance. |
| `Unpublish` | unary `UnpublishServiceResponse` | supervised app, own publication | Remove one registration. |
| `Browse` | server stream `DiscoveredService` | supervised app | Resolved services of one type. |

`PublishServiceRequest`:

| Field | Type | Rule |
|---|---|---|
| `service_type` | string | `_name._tcp` or `_name._udp`, optionally with a domain suffix; the name starts with `_` and uses only letters, digits and hyphens. |
| `instance_name` | string | Display instance, ≤63 UTF-8 bytes, no NUL. |
| `port` | uint32 | `1..65535`. |
| `txt` | map<string,string> | ≤16 records, ≤1024 bytes total; keys non-empty. |
| `domain` | string | Empty means `local`; labels are lowercased. |

`PublishServiceResponse` = `{publication_id, service_type, instance_name, domain, host, port}`.
`host` is the responder's fully-qualified name; an app cannot choose it. Avahi may rename a
colliding instance on the link, so the effective advertised name can differ from the requested
one; the response reports the values you requested.

`UnpublishServiceRequest` = `{publication_id}`; the response is `{removed}`. An unknown id, or
one owned by another run, is `NOT_FOUND` — the daemon never reveals or removes another app's
registration.

`BrowseServicesRequest` = `{service_type, domain}`. Each `DiscoveredService` carries `kind`
(`EVENT_KIND_ADDED` or `EVENT_KIND_REMOVED`), `name`, `service_type`, `domain`, `host`,
`addresses[]`, `port` and `txt`. An `ADDED` event carries one resolved address; a `REMOVED`
event names the instance and carries no address. The stream ends normally by closing it. One
stream queues at most 64 events and drops the oldest (logged) when the consumer falls behind, so
treat it as a convergent view and re-browse after a gap.

Publications are removed on unpublish, run exit (including a crash) and daemon shutdown. A
build or platform without a responder returns `FAILED_PRECONDITION` with the reason — never a
false success. The simulator has no Avahi responder, so publish and browse are unsupported
there.

Errors: an invalid service type, instance, port or TXT set is `INVALID_ARGUMENT`; an unknown
publication is `NOT_FOUND`; a missing responder or a responder that is not yet answering is
`FAILED_PRECONDITION`; a responder lost during a browse stream is `UNAVAILABLE`; cancellation
and deadline expiry are `CANCELED` and `DEADLINE_EXCEEDED`.

### Daemon-mediated listeners are not a service

Inbound TCP and UDP do not appear on this socket. An app obtains a listening socket from the
daemon through the per-run control channel described in
[network.md](network.md#daemon-mediated-listeners) (`CAPSULAD_CONTROL_FD`); the daemon binds it
and passes the descriptor. There is no gRPC method that opens a listener, because the daemon
must own the bind decision and the descriptor transfer is not a request/response message.

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

// DiscoveryService.Publish: mDNS through the daemon's Avahi responder
{"serviceType": "_example._tcp", "instanceName": "Capsula Demo", "port": 8080,
 "txt": {"path": "/status"}, "domain": "local"}
// → {"publicationId": "mdns_…", "serviceType": "_example._tcp",
//    "instanceName": "Capsula Demo", "domain": "local",
//    "host": "capsula.local", "port": 8080}

// DiscoveryService.Unpublish
{"publicationId": "mdns_…"}

// DiscoveryService.Browse: server stream, one event per resolved address
{"serviceType": "_example._tcp"}
// → {"kind": "EVENT_KIND_ADDED", "name": "Capsula Demo",
//    "serviceType": "_example._tcp", "domain": "local",
//    "host": "capsula.local", "addresses": ["192.168.1.42"], "port": 8080,
//    "txt": {"path": "/status"}}
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