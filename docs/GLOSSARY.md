# Capsula glossary

Terms are defined as this repository uses them. Where a word has a looser
everyday meaning, the **Current usage** note records what the code and
contracts actually mean. The implementation and the maintained protobuf
sources under `proto/` are authoritative; `_devdocs/ARCHITECTURE.md` is a
historical draft and may use terms differently.

## Platform and process

**Capsula** is the product family and the local application framework described
by this repository. It is a speaker appliance, not a cloud service.

**Capsula Mini** is the supported physical target: an RK3308 speaker with a
five-key panel, a 12-pixel RGB ring and a 30-cell segment display. Other
backends (simulator, Amlogic targets) either emulate it or fail explicitly.

**capsulad** (the *daemon*) is the single long-running native process that owns
the hardware abstraction, supervises applications, routes presentations and
captures, mixes and fans out audio, persists settings, serves the HTTPS Web UI
and exposes the local gRPC API. It is the only process that writes physical
output.

**capsula** (lowercase, the *CLI*) is the local administration client used for
authentication, application install/list/start/stop, diagnostics, firmware
inspection and daemon upgrades. It talks to the daemon over the same Unix
socket as apps, with administrator identity.

**HAL** — hardware abstraction layer. In this project it means two related
things: the `internal/backend` interfaces through which the daemon drives
displays, rings, input, audio, sensors and Bluetooth; and, historically, the
local API socket (`/run/capsulad/hal.sock`). The HAL is not a separate
process, and apps never talk to a backend directly. They use framework
services, and the backend is an implementation detail reported only for
diagnostics.

**Framework** is the stable runtime contract presented to applications: the
supervisor, the Unix-socket gRPC API (`capsula.framework.v1`), the app bus,
presentation/capture routing, audio, input, settings and the application
registry. "The framework" also means the daemon acting in that role. Framework
services are the only supported way for an app to affect the device.

**Device settings** are the daemon-wide typed settings (volume, brightness,
timezone, hostname, provider defaults, Bluetooth policy, …) owned by
`SettingsService`. They are distinct from an app's settings.

**Target platform** is the backend family declared at startup
(`simulator`, `rockchip-stock`, …). A backend advertises only the resources and
capabilities it actually implements; unsupported platforms fail explicitly.

## Applications and packaging

**App / application** is a unit of executable behavior known to the daemon
under a stable `app_id` (for example `org.example.weather`). An application can
be a supervised native process, a QuickJS or MicroPython script, or a
compiled-in system app. Apps present output, capture input, expose interfaces
and receive events only through the framework.

**System app** (also **built-in app**) is an application compiled into
`capsulad`, identified by a normal `app_id` such as `capsula.clock` or
`capsula.ring`. It has the same registry entry, settings, run lifecycle,
default-provider role and app-bus endpoints as an installed app, but no package
directory, child process, install or uninstall operation. `system_app` in
`InstalledApplication` is server-owned metadata, not something inferred from
the ID.

**Package** is the installed on-disk form of a non-built-in app: an immutable
directory containing `manifest.toml`, the entrypoint and any declared
interfaces, descriptors or assets. The package root is mounted read-only in
namespace sandbox modes (`/app`); it is never a security policy by itself.

**Manifest** (`manifest.toml`) is declarative metadata validated and consumed
by the daemon: identity, exactly one entrypoint, lifecycle flags, `provides`
hints, typed settings, actions, launch arguments, custom interfaces, an
optional native-SDK requirement and optional provenance declarations. It
declares no output ports, permissions or shell commands.

**provides** is a non-exclusive discovery hint (`segment_display_provider`,
`led_ring_controller`, `voice_assistant`, `media_player`, or an app-defined
tag). It never grants ownership; actual selection comes from settings,
captures or the standard interface a provider implements.

**Entrypoint** is the one declared way to start an app: `native` (a binary
path with architecture and ABI), `quickjs` / `micropython` (a `.js` or `.py`
script) or `builtin` (no path; compiled into the daemon). There is no shell
interpolation.

**Run** is one live process (or one live compiled-in instance) of an
application, identified by a unique, non-reused `run_id`. Run state is
`STARTING`, `READY`, `STOPPING`, `EXITED` or `FAILED`. Ownership of
presentations, captures, streams and settings delivery is tied to the run, not
just to the app ID.

**Invocation** is one request to start or signal an app. For a singleton app,
an invocation can be queued and delivered to the already-running run instead of
spawning a second process. `LaunchResult.invocation_id` identifies it even when
no new process appeared. Invocations are bounded and can expire.

**Activation** is a framework-issued, one-shot assistant turn, sent over the
selected assistant's app-bus connection. It carries an activation ID, origin,
and whether hold-to-talk applies. An activation authorizes bound pre-roll
audio for that app only; a release is correlated with the activation that
received the press. An activation is not a launch of the assistant.

**Default provider / selected provider** is the app chosen in device settings
for a routing resource (display, ring or assistant). A display/ring default
owns the implicit *baseline* presentation. The *selected* presentation at a
given moment may instead be an explicit capture. A selected provider is
recovery-managed: stopping it relaunches it rather than switching it off.
Choosing "none" is an explicit, persisted choice and yields a blank output, not
a hidden fallback.

**Conversation** is one assistant speech exchange owned by a conversation ID.
`AssistantService.BeginConversation` streams conversation events; audio opened
with that conversation ID is held at normal gain during ducking. Ending,
cancelling or dismissing the turn releases its captures and streams.

**Wakeword model** is an installed personal detector. Training/enrollment is an
administrator Web UI operation; apps receive activations, not samples. A model
has a stable model ID used by settings and activation.

**Sandbox mode** is the isolation level the daemon actually probed:
`namespaces` (user+mount+PID, distinct UID), `mountns` (root mount namespace
with a distinct UID), `uid` (distinct UID only) or `none` (development).
`auto` requests probing. The mode is reported, never assumed; on the RK3308
stock kernel the verified mode is `mountns`.

**Asset catalog / shared assets** is the read-only, daemon-owned file store
described by `<assets-dir>/manifest.json`. Apps see normalized
`/assets/...` paths (`CAPSULA_ASSETS` in namespace modes). Only regular,
non-symlink files inside the asset root are published.

## Resources, output and input

**Resource** is one routable hardware capability discovered from the backend,
with a stable `resource_id`: segment display, LED ring, touch panel, audio
input, audio output, sensor or the voice-assistant role. A resource's
`ResourceRouting` says whether it is single-owner-with-capture, shared-stream
or read-only.

**Presentation** is a dynamic output stream opened by an app on a display or
ring resource. It becomes eligible for selection on its *first valid frame*
(text/segment, RGB pixels, a named effect/face, or an explicit `blank`). A
presentation is content, not a port and not a permission. Closing the stream or
exiting the app removes it; its dependent captures end too.

**Baseline** is the implicit presentation that a selected default display or
ring provider keeps open for its resource. When no capture owns the resource,
the baseline is what the user sees. A baseline can deliberately be blank (for
example the ring's night window), which is different from "no selection at
all".

**Overlay** is an informal term for a temporary framework-owned presentation
that takes a capture above the selected baseline — for example the volume OSD,
the mute indication, a timer/alert ring, or a Bluetooth connect notice. It is
not a separate API object and does not bypass routing: it opens a normal
presentation and capture, then releases them so the baseline returns. Old
overlay callbacks must not terminate a newer indication.

**Capture** is a temporary claim by an app on a resource, usually referencing
one of its presentations. For display/ring resources the newest eligible live
capture wins; releasing it (or a crash/disconnect) restores the previous
eligible capture or the baseline. For touch, a capture is *partial*
(`TOUCH_PARTIAL_GESTURES`: circle/centre keys and gestures, while volume and
microphone remain framework controls) or *full* (`TOUCH_FULL_RAW`: every key
and gesture, all system effects suppressed). A capture is routing priority, not
a security permission.

**Routing state** is the authoritative selection for one resource: configured
default app, selected presentation/app, ordered captures and a revision.
Selection reasons include `default_baseline`, `capture`, `system_fallback` and
`none`.

**Input event** is one key press/release, inferred semantic gesture, or raw
touch report. Raw coordinates exist only when the backend advertises them;
gestures are inferred from ordered contacts rather than invented positions.

**Indication / watchdog**: microphone capture is delivered only while both
halves of a visible+audible indication hold — a selected, visible, non-blank
presentation at non-zero brightness for that app, and a microphone-live cue
that the mixer actually drained. The watchdog bounds how long a capture may
survive a lost indication before it is terminated with `WATCHDOG_EXPIRED`.

**Pre-roll** is the bounded audio retained before an assistant activation,
delivered only when the activation ID was issued to that app and has not been
used.

## APIs, discovery and contracts

**AppBus** is the reverse-RPC transport: each app keeps exactly one long-lived
`AppBusService.Connect` bidi stream to the daemon, over which it sends
`hello`/`ready`/replies and receives routed calls, invocations, settings
changes, actions, activations, release events and disconnects. Callers use
`AppBusService.Call` to make a routed call to another app. Apps never listen on
their own IPC sockets.

**Interface** is a service an app declares in its manifest: a fully-qualified
gRPC service name, version, optional descriptor file and docs. It is
discoverable through `AppInterfaceService` (with `available` only when the
selected run has sent `AppReady`) and callable through `AppBusService.Call`.
An interface is a name and a schema, not a network endpoint.

**Contract** is a published protobuf service and its documented behavior.
Framework contracts live under `proto/capsula/framework/v1`; standard app
contracts live under `proto/capsula/contracts/` (for example the media
contract). A contract becomes useful to apps when it is either a framework
service or a declared app interface. "Public" means discoverable and callable
through the normal SDK/app-bus path without private daemon privileges.

**App interface description** is the `AppInterfaceService.DescribeInterface`
response: interface metadata, a serialized `FileDescriptorSet` including
imports, Markdown documentation and per-method examples. It is how a caller
can construct and decode custom calls without a compile-time dependency.

**Settings** are typed key/value state with declared defaults, choices and
bounds. *Device settings* are global and administrator-only; *app settings*
belong to one app, are declared in its manifest, are read/updated
compare-and-swap, and are delivered to the owning app (including masked values
that are redacted everywhere else). Buttons are actions, not settings.

**Revision** is a monotonically increasing version for a snapshot (device
settings, app configuration, routing state, a watched state stream). Updates
carry an expected revision and fail with `ABORTED` on a stale value. A revision
identifies a snapshot; it is not a timestamp.

**Snapshot** is the complete authoritative state at one revision, sent first on
subscriptions. Events after it are incremental; consumers that lose events
must resynchronize by re-subscribing or requesting a fresh snapshot rather
than assuming they saw everything.

**Stream** is a gRPC stream, not a media file. Directions matter:
server-streaming (one request, many responses, e.g. `WatchRuns`), client-streaming,
and bidi (e.g. `PresentationService.Open`, `AppBusService.Connect`,
`AudioService.Playback`). The first message on a stream is the
open/hello/start request. Streams are bounded and are cancelled by their
context, by `cancel`, by app disconnect or by daemon shutdown.

**Watch** is the project's convention for a server-streaming subscription:
send the current snapshot first, then one event per change, with a revision or
monotonic counter to detect loss. A watch is not a poll.

**CAS** (compare-and-swap) is the update discipline for settings and other
revisioned state: the caller supplies the expected revision and the daemon
rejects a stale writer instead of silently overwriting a concurrent change.

**Native SDK** is the shared `libcapsula` runtime (Rust cdylib, C ABI v1) that
owns the app's transport: UDS gRPC, protobuf reflection, AppBus sessions, routed
calls and the raw-PCM fast path. Thin Go and Rust bindings and direct C/C++
consumers load it; no app links a second transport stack or bundles the
library.

**ABI** is the stable C function-table contract of the native SDK: major/minor
versions, the `capsula_get_api` negotiation, opaque handles, library-owned
buffers, status codes and threading guarantees. ABI major mismatch is an
explicit error, not a best-effort load. The *framework protocol* version
(`0.5`) and the *SDK/package* versions are separate from the ABI version.

**Binding** is a language wrapper over the native SDK (Go `capsula/sdk`,
Rust crate `capsula`, or the C/C++ header examples). A binding never
reimplements the transport.

**Script runtime / host** is the engine process (`capsula-quickjs`,
`capsula-micropython`) that loads a script package and exposes the framework
through the same native library. Scripts are admitted only when the matching
host and ABI are installed.

**Provenance** is optional GitHub/Sigstore attestation evidence that the
daemon verifies independently of the manifest claim. `verified` means the
digest, signature, certificate chain, log and workflow identity checked out; it
is not a safety guarantee and is unsupported for built-in/script runtimes.

## Web, network and new surfaces

**Web UI** is the separately built Vue 3 SPA served by `capsulad` over HTTPS.
It is an administrator client of the local gRPC API, not a second privilege
plane.

**Browser API / WebSocket** is the session-only binary `/ws` transport
(`capsula.web.v1`): revisioned topic snapshots, bounded events, an explicit
resync signal and a typed command allowlist. Static API keys are limited to
read-only status and certificate endpoints.

**Live control** means a Web UI control whose change is applied to the daemon
immediately after local client-side validation, without waiting for the form's
**Save**. Text/number drafts that are not yet valid or submitted remain local
and are never submitted by a live control in the same form. Hardware or
external changes update live controls but must not erase an unsaved draft.

**Custom settings UI** is an app-supplied local HTML/JS asset rendered in a
sandboxed, opaque-origin iframe under the application settings page. It may use
the provided system UI kit, but its only accepted output is validated
setting-value patches for the app whose page hosts it, sent to the parent
through the authenticated bridge.

**Settings bridge** is the parent↔iframe protocol: the parent validates exact
`event.source`, a per-frame random channel token, message size and schema, and
applies only setting-value patches through the normal revision-checked
own-app settings path. It is not an AppBus proxy and accepts no arbitrary
commands.

**Captive portal** is the restricted HTTP surface served while the Wi-Fi
fallback AP is up. It exposes only Wi-Fi scanning/connection so a new network
can be configured without administrator authentication; it is isolated from
the admin Web UI and its credentials.

**AP fallback** is the Wi-Fi access point the daemon brings up when it cannot
join a known network within its bounded startup attempt. Its SSID follows the
live Bluetooth adapter alias (saved host name only if Bluetooth has no name),
its password is the documented WPA2 provisioning password `capsulad`, and it
runs on 2.4 GHz channel 1 at `192.168.4.1/24` with DHCP/DNS and a captive
portal on port 80. It serves only Wi-Fi provisioning — it is not a general
management network — and it is torn down when a station connection succeeds.

**dcron** is the five-field cron expression syntax used by the clock app's
stored `alarms` setting (minute, hour, day of month, month, day of week).
Comma-separated lists are valid *inside* a field (`0,30 7 * * 1-5`), so the
persisted list of schedules separates completed entries with a comma followed
by whitespace (`", "` canonical), never by splitting on every comma. An entry
may carry optional `@IANA-zone`, `!once` and `!disabled` suffixes; its stable
alarm identity is `expr [@zone] [!once]`. The Web UI edits schedules as
friendly times and serializes them to dcron.

**Palette / gradient** is the ring's stored color definition. It is a canonical
gradient string (`linear-gradient(<angle>deg, #RRGGBB [pct%], …)`) or a solid
`#RRGGBB`, not a named preset; named presets in the settings UI are conveniences
that serialize to the same canonical form. The renderer samples colors for each
effect, and the `static` effect displays a single solid color. The `rainbow`
effect rotates/interpolates the palette's actual samples at 0, 1/3 and 2/3 rather than
inventing unrelated hues. Legacy named
palettes migrate to equivalent gradient definitions without changing the
rendered colors.
