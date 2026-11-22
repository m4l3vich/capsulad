# Architecture

`capsulad` is the local appliance daemon and application framework for Capsula Mini. It
supervises application processes, owns the hardware abstraction, routes dynamic presentations and
captures, mixes audio, serves an HTTPS Web UI and exposes one local gRPC surface over a Unix
socket. Applications are ordinary executables or scripts that talk to that socket through the
shared native SDK or a script host. They never create or bind a listening socket of their own and
never write hardware directly: outbound networking and mDNS run through the shared SDK and the
daemon's responder, and a fixed inbound TCP or UDP port exists only as a listener the daemon
allocated and handed to the app.

This page is the system-level overview. The application contract is the
[application manual](app_development/README.md); the build and deployment manual is
[DEVELOPMENT.md](DEVELOPMENT.md); measured evidence is in [VERIFICATION.md](VERIFICATION.md).

```text
 native apps (Go/Rust/C/C++)  script apps (QuickJS, MicroPython)
          │ CAPSULAD_LIBRARY            │ capsulad-quickjs / capsulad-micropython
          └──────── libcapsulad ────────┘
                          │
        app bus (reverse RPC) + framework calls
                          │
              Unix socket: <data-dir>/run/hal.sock
                          │
   ┌────────────────── capsulad ──────────────────┐
   │ supervision · routing · mixer · settings     │
   │ wakeword · Bluetooth · Wi-Fi · discovery     │
   │ built-in system apps: clock, ring            │
   └───────┬───────────────────────────┬──────────┘
           │                           │
   backend (simulator | Rockchip |    HTTPS Web UI + MCP (:8443)
   Amlogic)                          self-signed TLS, Argon2id sessions,
   display · ring · audio · input    static API keys, /ws protobuf stream
   sensors · Bluetooth · GPIO
```

## One local API

Seventeen framework services — device, settings, routing, presentation, audio, input, assistant,
application, application configuration, app-interface, app-bus, sensor, system, Bluetooth media,
diagnostics, asset and discovery — share one Unix-socket gRPC API. There is no network gRPC
listener and no per-app IPC socket. Inbound TCP/UDP is not a service on that socket: the daemon
allocates and binds the listening socket, applies the address and port policy, and passes the
descriptor over the run's inherited control channel (`CAPSULAD_CONTROL_FD`). An app therefore
serves a listener it never created.

Each app holds one long-lived `AppBusService.Connect` stream. Routed unary and streaming calls
reach other apps through `AppBusService.Call`; no app accepts an inbound bus connection, and a
routed call never opens a connection to the target.

## Shared native SDK

`libcapsulad` is a pure Rust cdylib exposing a frozen C ABI v1 through one bounded
`capsulad_get_api` table. It owns the Unix-socket gRPC engine, protobuf/JSON reflection, AppBus
sessions, routed calls, the raw-PCM audio path, outbound HTTP/1.1 over verified TLS, remote gRPC
over TLS or h2c, outbound TCP/TLS streams, UDP, daemon-mediated listeners, mDNS calls, and the
encoding/hash/random/URL primitives (base64, percent coding, SHA-256, getrandom, UUIDv4,
absolute-URL parsing).

ABI 1 keeps its identity (1.0, pre-release): the HTTP, remote-gRPC, primitive, stream, UDP,
listener and mDNS entry points are appended to the table, and `CAPSULAD_STATUS_UNSUPPORTED` is
appended to the status list. The table stays append-only at ABI 1.0 until the first external
consumer or release. Thin Go and Rust bindings load it dynamically; C and C++ use the installed
header. Native apps should not bundle their own HTTP, TLS or gRPC stack.

Outbound streams verify TLS 1.2+ with system, webpki or caller PEM roots and have no insecure
mode. mDNS publication and browsing go through the daemon's Avahi responder, never an app-owned
responder: publications are scoped to the calling run, removed on run exit, and surfaced through
`DiscoveryService` and the SDK's mDNS calls. The daemon validates a publication against the
DNS-SD limits — a 63-byte instance label, at most 16 TXT records and 1024 bytes total, and no
single `key=value` TXT item over 255 bytes — and returns `ALREADY_EXISTS` rather than renaming a
registered name.

## Board profiles and backends

Three hardware configurations are supported. The Capsula Mini rev 4 runs the stock Rockchip
firmware (RK3308, aarch64 glibc 2.33); the Capsula Mini on Amlogic A113X runs either its
jailbroken stock Buildroot image (aarch64 glibc 2.28) or Alpine Linux (aarch64 musl). Board
specifics are inert data in `internal/backend/profile`; the backends and deployment layers
(`internal/backend/rockchip`, `internal/backend/amlogic`, `deploy/rockchip-stock`,
`deploy/amlogic-stock`, `deploy/amlogic-alpine`) apply them, and hardware-agnostic packages take
the values they need from the composition root. Device knowledge that is not a knob is shared by
both boards through `internal/backend/linuxdev` and `internal/backend/syswatchdog`. A complete
virtual backend (`-platform simulator`, neutral profile) implements the same contracts for
development and tests.

Platforms are selected with `-platform simulator|rockchip-stock|amlogic-stock|amlogic-alpine`
(`rockchip` is an alias for `rockchip-stock`). The Amlogic stock clock panel's vendor ABI is
unrecovered and is reported `UNVERIFIED` rather than guessed. See [HARDWARE.md](HARDWARE.md) for
the measured hardware facts and [DEVELOPMENT.md](DEVELOPMENT.md#amlogic-a113x-targets) for the
Amlogic build recipes.

## Output, input and audio

Apps open output presentations and acquire routing captures. The first valid frame makes a
presentation eligible; the latest eligible capture wins; release, crash and app stop fall back to
the selected default baseline. No selected presentation means an explicit blank hardware frame,
not "skip writing".

Five discrete panel keys support partial and full capture modes, with centre-gated swipe gestures,
a step-then-cancel full-panel crossing of two opposite volume edges (a bare single volume edge is
a volume step), and full-capture circle detection. No contact is ever deferred, so every key acts
on its own edge; a full-panel crossing reverts its already-applied leading step instead of
delaying it. A recognized swipe reaches current media through the standard media contract over
the app bus — routed `MediaPlayerService.SkipNext`/`SkipPrevious` for an app provider, otherwise
`BluetoothMediaService.Command` — never through a private daemon→hardware path.

A software mixer makes master volume a ceiling on each app's contribution: the persisted 0–1
volume is a control position mapped once to an exponential 60 dB taper (linear in dB, so 5% is
about −57 dBFS) rather than used as a linear amplitude. Hot float input is clamped, one app may
hold four concurrent streams, and a soft limiter (0.95 threshold, 5 ms attack, 150 ms release)
holds the bus below full scale. The mixer fans out playback, ducks during a conversation, bounds
capture queues and gates microphone capture on a visible, audible indication. The PulseAudio sink
is pinned at unity so the mixer's tapered master is the only gain stage.

The microphone path is S16LE mono 16 kHz with a bounded DSP chain (`framework.dsp.v1`: 80 Hz
high-pass, adaptive suppression capped at 12 dB, unity gain, −1 dBFS limiter, 256/128 STFT); the
raw vendor stream exists only at `audio.input.microphone.raw`. Details and app-facing limits are
in [app_development/audio.md](app_development/audio.md) and
[app_development/output-input.md](app_development/output-input.md).

## Built-in system apps

`capsulad.clock` and `capsulad.ring` are compiled into the daemon as normal registry identities
with run lifecycle, settings, routing ownership and app-bus endpoints. Their functionality is
published as the public contracts `capsulad.contracts.clock.v1.ClockService` and
`capsulad.contracts.ring.v1.RingService`.

The clock starts at every normal boot when the segment display capability exists, because it owns
the timers, alarms and stopwatch; the ring runs only while it is the selected ring/display
provider, and neither manifest boot flag nor a restart overrides that rule. `-single-app` is the
one mode that starts neither unless it names it. See
[app_development/standard-contracts.md](app_development/standard-contracts.md) and
[SPEAKER_BEHAVIOR.md](SPEAKER_BEHAVIOR.md) for the interaction contract.

## Bluetooth media and discovery

The Web UI "Now playing" panel lists the connected AVRCP source from the framework's public
`BluetoothMediaService` on the same socket — no system app and no reserved app id — and its
transport buttons call that service's `Command`. The physical circle key and swipes use the same
service when no media app is playing or paused, so a phone works with no media package installed.
The panel is a strict now-playing projection: it lists only players reporting
`PLAYBACK_PLAYING`, so a source that is connected but idle, paused or stopped adds no row (and no
error card) even though it stays discoverable and controllable through the same service.

The Bluetooth stack is native: BlueZ adapter power, incoming-connections and discoverable policy,
pairing-agent support, paired-device listing and unpair, AVRCP transport and absolute volume, and
an A2DP Sink endpoint with a pure-Go SBC decoder. After every connection the daemon asserts that
a `MediaTransport1` carrying the A2DP sink UUID exists and performs one repair when it does not;
a source that never reports absolute volume is reported as such. PulseAudio is the fallback owner
when native registration fails, and its module set is retired as soon as the native sink is armed
unless it is actively streaming a connected source.

Wi-Fi is a Linux manager over saved networks with stock `wpa_supplicant.conf` import, bounded
startup association and a WPA2 setup AP with a restricted captive portal. The AP takes the live
Bluetooth advertised name when it starts and shortens it to the first 32 UTF-8 bytes on a rune
boundary, because that is the SSID limit; the saved host name is only a fallback. There is no
app-facing Wi-Fi service.

## Web UI and MCP

An embedded Vue SPA is served by the daemon over HTTPS, authenticated with an Argon2id admin
password and server-side sessions. Static API keys carry explicit scopes: `status` reads status
and the certificate, `assistant` controls the speaker through MCP, and `operator` also
administers the daemon and can use the PA Speaker raw-WAV relay. Browser traffic uses the binary
protobuf `/ws` protocol; the Access page shows each key's grant and offers the certificate.

The daemon also serves a Model Context Protocol server at `/mcp` on the same HTTPS listener — no
app, no extra port or process. It uses API keys, not browser sessions. An `assistant` key is a
deliberately limited grant for common speaker controls; operators use the same shared operation
boundary for administration. A new key defaults to explicit `status`; credential records without
scopes are not accepted. See [MCP.md](MCP.md).

## Lifecycle, packaging and boot

An app package is a deterministic `.tar.gz` with `manifest.toml` at the root, installed under
`<data-dir>/apps/<app_id>` or compiled in as a built-in. Install and update are atomic, a live
run must be stopped before its package is replaced, and a launch of an already-running app is
delivered as a queued, bounded, expiring invocation instead of a second process. The supervisor
allocates a run ID, starts the entrypoint in the probed sandbox, and waits for `AppHello{run_id}`
and `AppReady`; identity comes from the connection plus Linux `SO_PEERCRED`, never from a claim.

Repeated failures back off immediately, then 1/2/4/8 s; the backoff resets only after 30 s of
continuous READY. Selected display/ring/assistant providers are recovery-managed regardless of
`restart_on_failure`, and deselecting (including "none"), uninstalling or shutting down is the
durable off switch. Boot ownership on the stock Rockchip image is the single hook
`/etc/init.d/S99btctl.sh`, which prepares state, starts and health-gates the daemon, and manages
the vendor assistant scripts.

Packaging and deployment are described in
[app_development/packaging-deployment.md](app_development/packaging-deployment.md) and
[DEVELOPMENT.md](DEVELOPMENT.md#deployment-bundles).

## Security posture

The framework plane is local-only: the gRPC API is a Unix socket, and the browser and MCP
frontends are local administrator clients of that socket. Supervised apps are identified by run
ID, peer credentials, sandbox UID and supervised PID; a built-in run additionally requires the
daemon's per-boot token. Microphone capture requires a visible, non-blank presentation at non-zero
brightness and an audible cue the mixer consumed and drained.

On the RK3308 stock kernel the sandbox is mount-only: a mount namespace with a distinct UID, no
user or network namespace, and no seccomp. Native apps are trusted code; provenance verification
is evidence, not runtime isolation. Apps share the device network stack, so there is no
per-destination egress allowlist. See [app_development/security.md](app_development/security.md)
for the full boundary and [VERIFICATION.md](VERIFICATION.md) for the proof levels and open gates.
