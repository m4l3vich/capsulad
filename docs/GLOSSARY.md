# capsulad glossary

Terms as this repository uses them. The implementation and the protobuf sources under `proto/` are
authoritative; where a word has a looser everyday meaning, the definition here is the contract. The
application contract itself is the [application manual](app_development/README.md).

## Platform and process

**capsulad** — the project and the local appliance framework: a speaker appliance for the Capsula
Mini, not a cloud service.

**capsulad** (the *daemon*) — the single long-running native process that owns the hardware
abstraction, supervises applications, routes presentations and captures, mixes and fans out audio,
persists settings, serves the HTTPS Web UI and exposes the local gRPC API. It is the only process
that writes physical output.

**capsulad-cli** — the local administration client: authentication, application install/list/start/
stop, settings, diagnostics, media listing, firmware inspection, display bring-up and
deployment-bundle application. It reaches the daemon over the same Unix socket as apps, with
administrator identity.

**Capsula Mini** — the supported physical target: an RK3308 speaker with a five-key panel, a
12-pixel RGB ring and a 30-cell segment display, running the stock Rockchip firmware. Its
`rk3308-capsula-mini-rev4-stock` board profile is the only hardware configuration; the simulator
emulates the same contracts, and Amlogic has no backend.

**HAL** — hardware abstraction layer: the `internal/backend` interfaces through which the daemon
drives displays, rings, input, audio, sensors and Bluetooth. It is not a separate process, and apps
never talk to a backend directly — they use framework services, and the backend is reported only for
diagnostics.

**Framework** — the stable runtime contract presented to applications: the supervisor, the
Unix-socket gRPC API (`capsulad.framework.v1`), the app bus, presentation/capture routing, audio,
input, settings and the application registry. "The framework" also means the daemon acting in that
role; framework services are the only supported way for an app to affect the device.

**Device settings** — daemon-wide typed settings (volume, brightness, timezone, hostname, provider
defaults, Bluetooth policy, …) owned by `SettingsService`, distinct from an app's settings.

**Target platform** — the backend family declared at startup: `simulator` or `rockchip-stock`
(`rockchip` is accepted as an alias). Any other value, including `amlogic-stock` and
`amlogic-alpine`, is rejected as an unknown platform. A backend advertises only the resources
and capabilities it implements; unsupported platforms fail explicitly, and the historical
`CAPSULA_MINI_AMLOGIC_*` wire enum values are reserved.

**Board profile** — the inert data record in `internal/backend/profile` describing one
supported configuration: model and SoC fallbacks, serial location, audio topology, LED and
segment geometry, input paths, watchdog, stock Wi-Fi files, deployment budgets, architecture,
memory envelope and brightness curve. It has no I/O, no interfaces and no build tags. The
composition root selects it and passes values into generic packages; board paths and quirks
live in `internal/backend/rockchip` and `deploy/rockchip-stock`, and hardware-agnostic
packages such as `internal/wifi` must not import a profile. The supported profiles are
Rockchip stock (`rk3308-capsula-mini-rev4-stock`) and the neutral simulator profile.

**Sandbox mode** — the isolation level the daemon actually probed: `namespaces` (user+mount+PID,
distinct UID), `mountns` (root mount namespace, distinct UID), `uid` (distinct UID only) or `none`
(development). `auto` requests probing; the mode is reported, never assumed, and is `mountns` on the
RK3308 stock kernel.

## Applications and packaging

**App / application** — a unit of executable behavior known to the daemon under a stable `app_id`: a
supervised native process, a QuickJS or MicroPython script, or a compiled-in system app. Apps present
output, capture input, expose interfaces and receive events only through the framework.

**Built-in system app** — an application compiled into `capsulad`, with a normal `app_id` such as
`capsulad.clock` or `capsulad.ring`: same registry entry, settings, run lifecycle, provider role and
app-bus endpoints as an installed app, but no package directory, child process, install or uninstall.
`system_app` in `InstalledApplication` is server-owned metadata, not inferred from the ID.

**Package** — the installed on-disk form of a non-built-in app: an immutable directory with
`manifest.toml`, the entrypoint and any declared interfaces, descriptors or assets, mounted read-only
at `/app` in namespace sandbox modes. It is not a security policy by itself.

**Manifest** (`manifest.toml`) — declarative metadata validated and consumed by the daemon: identity,
exactly one entrypoint, lifecycle flags, `provides` hints, typed settings, actions, launch arguments,
custom interfaces, an optional native-SDK requirement and optional provenance declarations. It
declares no output ports, permissions or shell commands.

**provides** — a non-exclusive discovery hint (`segment_display_provider`, `led_ring_controller`,
`voice_assistant`, `media_player`, or an app-defined tag). It never grants ownership; selection comes
from settings, captures or the standard interface a provider implements.

**Entrypoint** — the one declared way to start an app: `native` (binary path with architecture and
ABI), `quickjs`/`micropython` (a `.js` or `.py` script) or `builtin` (compiled into the daemon). There
is no shell interpolation.

**Artifact / deployment bundle** — an *artifact* is the canonical `.tar.gz` app package
(`manifest.toml` at the archive root); the app build targets package it on the build host with
`capsulad-cli app package DIR`, and `app install` takes the artifact (a directory is a developer
convenience). A *deployment bundle* is the directory or `.tar.gz` applied by
`capsulad-cli deploy`: `capsulad`, `capsulad-cli`, `lib/`, `apps/*.tar.gz` and `boot/S99btctl.sh`,
with unknown top-level entries reported and ignored.

**Run** — one live process (or one live compiled-in instance) of an application, identified by a
unique, non-reused `run_id`, with state `STARTING`, `READY`, `STOPPING`, `EXITED` or `FAILED`.
Ownership of presentations, captures, streams and settings delivery belongs to the run, not just the
app ID.

**Activation** — a framework-issued, one-shot assistant turn sent over the selected assistant's
app-bus connection: activation ID, origin, and whether hold-to-talk applies. It authorizes bound
pre-roll audio for that app only, and a release is correlated with the activation that received the
press. An activation is not a launch.

**Default / selected provider** — the app chosen in device settings for a routing resource (display,
ring or assistant). A display/ring default owns the implicit *baseline* presentation; the *selected*
presentation at a given moment may instead be an explicit capture. A selected provider is
recovery-managed: stopping it relaunches it rather than switching it off. Choosing "none" is an
explicit persisted choice yielding a blank output, not a hidden fallback.

**Conversation** — one assistant speech exchange owned by a conversation ID.
`AssistantService.BeginConversation` streams its events; audio opened with that ID stays at normal
gain during ducking, and ending, cancelling or dismissing the turn releases its captures and streams.

**Wakeword model** — an installed personal detector (format version 3) with a stable model ID used by
settings and activation. Enrollment and training are local-administrator Web UI operations; apps
receive activations, never samples.

## Assets and resources

**Asset store** — the read-only, daemon-owned directory `<data-dir>/assets`, mounted at `/assets` for
apps and passed as `CAPSULAD_ASSETS`. The directory is the catalog: every regular, non-symlink file
below the root is an asset, its ID is the slash-relative path and its canonical app path is
`/assets/<id>`. The catalog is enumerated per call, so a new file is visible without a daemon restart;
uploads are staged and renamed atomically; one file is capped at 32 MiB.

**Resource** — one routable hardware capability discovered from the backend with a stable
`resource_id`: segment display, LED ring, touch panel, audio input, audio output, sensor or the
voice-assistant role. `ResourceRouting` says whether it is single-owner-with-capture, shared-stream
or read-only.

**Presentation** — a dynamic output stream an app opens on a display or ring resource, eligible for
selection on its *first valid frame* (text/segment, RGB pixels, a named effect/face, or an explicit
`blank`). It is content, not a port and not a permission; closing the stream or exiting the app
removes it and its dependent captures.

**Baseline** — the implicit presentation a selected default display or ring provider keeps open: what
the user sees when no capture owns the resource. A baseline may deliberately be blank (for example
the ring's night window), which differs from "no selection at all".

**Capture** — a temporary claim by an app on a resource, usually referencing one of its
presentations. For display/ring resources the newest eligible live capture wins; releasing it (or a
crash/disconnect) restores the previous capture or the baseline. For touch a capture is *partial*
(`TOUCH_PARTIAL_GESTURES`: circle/centre keys and gestures, volume and microphone framework-owned) or
*full* (`TOUCH_FULL_RAW`: every key and gesture, all system effects suppressed). A capture is routing
priority, not a security permission.

**Routing state** — the authoritative selection for one resource: configured default app, selected
presentation/app, ordered captures and a revision. Selection reasons are `default_baseline`,
`capture`, `system_fallback` and `none`.

**Input event** — one key press/release, inferred semantic gesture, or raw touch report. Raw
coordinates exist only when the backend advertises them; gestures come from ordered contacts, never
from invented positions.

**Indication / watchdog** — microphone capture is delivered only while both halves of a
visible+audible indication hold: a selected, visible, non-blank presentation at non-zero brightness
for that app, and a microphone-live cue the mixer actually drained. The watchdog bounds how long a
capture survives a lost indication before `WATCHDOG_EXPIRED` terminates it.

**Pre-roll** — the bounded audio retained before an assistant activation, delivered only when the
activation ID is one issued to that app and has not been used.

## APIs, discovery and contracts

**AppBus** — the reverse-RPC transport: each app keeps exactly one long-lived
`AppBusService.Connect` bidi stream carrying `hello`/`ready`/replies out and routed calls,
invocations, settings changes, actions, activations, release events and disconnects in. Callers use
`AppBusService.Call` for routed calls to another app; apps never listen on their own IPC sockets.

**Interface / contract** — an *interface* is a service an app declares in its manifest: qualified
gRPC service name, version, optional descriptor file and docs, discoverable through
`AppInterfaceService` (`available` only when the selected run sent `AppReady`) and callable through
`AppBusService.Call` — a name and a schema, not an endpoint. A *contract* is a published protobuf
service and its documented behavior (framework contracts under `proto/capsulad/framework/v1`,
standard app contracts under `proto/capsulad/contracts/`); "public" means callable through the normal
SDK/app-bus path without private daemon privileges.

**Settings** — typed key/value state with declared defaults, choices and bounds. *Device settings* are
global and administrator-only; *app settings* belong to one app, are declared in its manifest, are
read/updated compare-and-swap, and are delivered to the owning app (including masked values redacted
everywhere else). Buttons are actions, not settings.

**Revision** — a monotonically increasing version of a snapshot (device settings, app configuration,
routing state, a watched state stream). Updates carry an expected revision and fail with `ABORTED` on
a stale value. A revision identifies a snapshot; it is not a timestamp.

**Snapshot / watch / stream** — a snapshot is the complete authoritative state at one revision, sent
first on every subscription; later events are incremental, so a consumer that loses one re-subscribes
or requests a fresh snapshot. A *watch* is that server-streaming convention (snapshot, then one event
per change, with a revision or monotonic counter to detect loss), not a poll. A *stream* is a gRPC
stream, never a media file — server-streaming (`WatchRuns`), client-streaming or bidi
(`PresentationService.Open`, `AppBusService.Connect`, `AudioService.Playback`) — whose first message
is the open/hello/start request.

**CAS** (compare-and-swap) — the update discipline for settings and other revisioned state: the
caller supplies the expected revision and the daemon rejects a stale writer instead of silently
overwriting a concurrent change.

**Native SDK / ABI** — the shared `libcapsulad` runtime (Rust cdylib, C ABI v1) owns an app's
transport: UDS gRPC, protobuf reflection, AppBus sessions, routed calls, the raw-PCM fast path,
outbound HTTP/1.1 over verified TLS, remote gRPC over TLS or h2c, and the
encoding/hash/random/URL primitives (base64, percent coding, SHA-256, getrandom, UUIDv4,
absolute-URL parsing). Go and Rust bindings (`capsulad/sdk`, crate `capsulad`) and direct
C/C++ consumers load it and never reimplement the transport; an app must not bundle its own
`net/http`, `crypto/tls` or gRPC stack. The *ABI* is its stable C function-table contract —
major/minor versions, `capsulad_get_api` negotiation, opaque handles, library-owned buffers,
status codes and threading guarantees — where a major mismatch is an explicit error, not a
best-effort load. New entry points are appended within ABI 1.0. The SDK/package version
(`0.5`, also exported to apps as `CAPSULAD_SDK`) is separate from the ABI version.

## Web, network and display settings

**Web UI / live control** — the separately built Vue 3 SPA served by `capsulad` over HTTPS, an
administrator client of the local gRPC API rather than a second privilege plane. A *live control* is
one whose change reaches the daemon immediately after local validation, without waiting for the form's
**Save**; invalid or unsubmitted text edits stay local, are never submitted by a neighbouring control and
are never erased by a hardware or external update. Settings → Bluetooth also edits the **advertised
device name** through a pencil button and a validated modal: 1–248 UTF-8 bytes, trimmed, no control
characters, written to BlueZ `Adapter1.Alias` without power-cycling the adapter.

**Custom settings UI and settings bridge** — an app-supplied local HTML/JS asset in a sandboxed,
opaque-origin iframe under the application settings page. Its only accepted output is validated
setting-value patches for the hosting app, sent to the parent through the authenticated bridge, which
validates exact `event.source`, a per-frame random channel token, message size and schema and applies
patches through the revision-checked own-app settings path. It is not an AppBus proxy.

**Captive portal / AP fallback** — the Wi-Fi access point raised when no known network joins within
the bounded startup attempt, with SSID taken from the live Bluetooth adapter alias when it starts
(shortened to the first 32 UTF-8 bytes on a rune boundary, because that is the SSID limit; the saved
host name is only a fallback for a device without Bluetooth, and an empty answer fails the AP rather
than inventing a name), WPA2 password `capsulad`, 2.4 GHz channel 1 at `192.168.4.1/24`,
DHCP/DNS and a portal on port 80. The portal exposes only Wi-Fi scanning and connection so a network
can be configured without administrator authentication, is isolated from the admin Web UI and its
credentials, and the AP is torn down when a station connection succeeds.

**Palette** — the ring's stored color definition: a canonical gradient string
(`linear-gradient(<angle>deg, #RRGGBB [pct%], …)`) or a solid `#RRGGBB`, not a named preset. Named
presets in the settings UI serialize to that form; `static` shows one solid color, `rainbow` rotates
the palette's actual samples at 0, 1/3 and 2/3 without inventing hues, and a stored named palette
resolves to its equivalent gradient with unchanged rendered colors.

**dcron** — the five-field cron syntax of the clock app's stored `alarms` setting (minute, hour, day
of month, month, day of week). Commas may appear *inside* a field (`0,30 7 * * 1-5`), so the
persisted schedule list separates entries with a comma followed by whitespace (`", "` canonical) and
never splits on every comma. Entries may carry `@IANA-zone`, `!once` and `!disabled` suffixes; the
stable identity is `expr [@zone] [!once]`. The Web UI edits friendly times and serializes them.