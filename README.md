# capsulad — appliance daemon for the Capsula Mini

`capsulad` is the local appliance daemon and application framework for the Capsula Mini
speaker; `capsulad` is the project and the daemon, and "Capsula Mini" is only the hardware
name. It supervises application processes, owns the hardware abstraction, routes
dynamic presentations and captures, mixes audio, serves an HTTPS Web UI, and exposes one
local gRPC surface over a Unix socket. Applications are ordinary executables or scripts
that talk to that socket through the shared native SDK or a script host. They never create or
bind a listening socket of their own and never write hardware directly: outbound networking and
mDNS run through the shared SDK and the daemon's responder, and a fixed inbound TCP or UDP port
exists only as a listener the daemon allocated and handed to the app.

Three hardware configurations are supported. The Capsula Mini rev 4 runs the stock Rockchip
firmware (RK3308, aarch64 glibc 2.33); the Capsula Mini on Amlogic A113X runs either its
jailbroken stock Buildroot image (aarch64 glibc 2.33) or Alpine Linux (aarch64 musl). Their board
specifics are inert data in `internal/backend/profile`; the backends and deployment layers
(`internal/backend/rockchip`, `internal/backend/amlogic`, `deploy/rockchip-stock`,
`deploy/amlogic-stock`, `deploy/amlogic-alpine`) apply them, and hardware-agnostic packages take
the values they need from the composition root. Device knowledge that is not a knob is shared by
both boards through `internal/backend/linuxdev`. A complete virtual backend (`-platform
simulator`, neutral profile) implements the same contracts for development and tests. Amlogic
targets are selected with `-platform amlogic-stock` and `-platform amlogic-alpine`; the stock
clock panel's vendor ABI is unrecovered and is reported `UNVERIFIED` rather than guessed.

The administrative CLI is `capsulad-cli` (`capsulad-cli status`, `app`, `deploy`, `auth`,
`cert`, `settings`, `logs`, `media`, `platform`, `dev`). The daemon-to-app wire protocol may
change together with the daemon; the native C ABI is v1 and is frozen within its major
version.

## Architecture

```text
 native apps (Go/Rust/C/C++)  script apps (QuickJS, MicroPython)
          │ CAPSULAD_LIBRARY            │ capsulad-quickjs / capsulad-micropython
          └──────── libcapsulad ────────┘
                          │
        app bus (reverse RPC) + framework calls
                          │
              Unix socket: /run/capsulad/hal.sock
                          │
   ┌────────────────── capsulad ──────────────────┐
   │ supervision · routing · mixer · settings     │
   │ wakeword · Bluetooth · Wi-Fi · discovery     │
   │ built-in system apps: clock, ring            │
   └───────┬───────────────────────────┬──────────┘
           │                           │
   backend (simulator | rockchip)   HTTPS Web UI (:8443)
   display · ring · audio · input   self-signed TLS, Argon2id sessions,
   sensors · Bluetooth · GPIO       static API keys, /ws protobuf stream
```

- **One Unix-socket gRPC API.** Seventeen framework services (device, settings, routing,
  presentation, audio, input, assistant, application, application configuration,
  app-interface, app-bus, sensor, system, Bluetooth media, diagnostics, asset, discovery) on one
  socket. There is no network gRPC listener and no per-app IPC socket. Inbound TCP/UDP is not a
  service on that socket: the daemon allocates and binds a listening socket, applies the address
  and port policy, and passes the descriptor over the run's inherited control channel
  (`CAPSULAD_CONTROL_FD`). An app therefore serves a listener it never created.
- **AppBus reverse RPC.** Each app holds one long-lived `Connect` stream. Routed unary
  and streaming calls reach other apps through `AppBusService.Call`; no app accepts an inbound
  bus connection. A routed call never opens a connection to the target.
- **Shared native SDK.** `libcapsulad` is a pure Rust cdylib exposing a frozen C ABI v1
  through one bounded `capsulad_get_api` table. It owns the Unix-socket gRPC engine,
  protobuf/JSON reflection, AppBus sessions, routed calls, the raw-PCM audio path,
  outbound HTTP/1.1 over verified TLS, remote gRPC over TLS or h2c, outbound TCP/TLS streams,
  UDP, daemon-mediated listeners, mDNS calls, and the
  encoding/hash/random/URL primitives (base64, percent coding, SHA-256, getrandom,
  UUIDv4, absolute-URL parsing). ABI 1 keeps its identity (1.0, pre-release): the HTTP,
  remote-gRPC, primitive, stream, UDP, listener and mDNS entry points are appended to the table,
  and `CAPSULAD_STATUS_UNSUPPORTED` is appended to the status list. The table stays append-only
  at ABI 1.0 until the first external consumer or release. Thin Go and Rust bindings load it
  dynamically; C and C++ use the installed header. Native apps should not bundle their
  own HTTP, TLS or gRPC stack.
- **Egress and discovery.** Outbound streams verify TLS 1.2+ with system, webpki or caller PEM
  roots and have no insecure mode. mDNS publication and browsing go through the daemon's Avahi
  responder, never an app-owned responder: publications are scoped to the calling run, removed
  on run exit, and surfaced through `DiscoveryService` and the SDK's mDNS calls.
- **Presentations and captures.** Apps open output presentations and acquire routing
  captures. First valid frame makes a presentation eligible; the latest eligible capture
  wins; release, crash and app stop fall back to the selected default baseline.
- **Audio.** A software mixer makes master volume a ceiling on each app's contribution: the
  persisted 0–1 volume is a control position mapped once to an exponential 60 dB taper (linear
  in dB, so 5% is about -57 dBFS) rather than used as a linear amplitude; hot float input is
  clamped, one app may hold four concurrent streams, and a soft limiter holds the bus below full
  scale. It fans out playback, ducks during a conversation, bounds capture queues and gates
  microphone capture on a visible, audible indication. The PulseAudio sink is pinned at unity so
  the mixer's tapered master is the only gain stage.
- **Input.** Five discrete panel keys with partial/full capture modes, centre-gated swipe
  gestures and a step-then-cancel full-panel crossing of two opposite volume edges (a bare
  single volume edge is a volume step) plus full-capture
  circle detection. No contact is ever deferred, so every key acts on its own edge; a
  full-panel crossing reverts its already-applied leading step instead of delaying it. Raw
  coordinates only when the backend has a coordinate panel. A recognized swipe reaches current
  media through the standard media contract over the app bus — routed
  `MediaPlayerService.SkipNext`/`SkipPrevious` for an app provider, otherwise
  `BluetoothMediaService.Command` — never through a private daemon→hardware path.
- **Built-in clock and ring.** `capsulad.clock` and `capsulad.ring` are compiled into the
  daemon as normal registry identities with run lifecycle, settings, routing ownership
  and app-bus endpoints. Their functionality is published as the public contracts
  `capsulad.contracts.clock.v1.ClockService` and `capsulad.contracts.ring.v1.RingService`.
  The clock always runs because it owns the timers, alarms and stopwatch; the ring runs
  only while it is the selected ring/display provider, and neither manifest boot flag nor a
  restart overrides that rule.
- **Bluetooth media source, no bundled app.** The Web UI "Now playing" panel lists the
  connected AVRCP source from the framework's public `BluetoothMediaService` on the same
  socket — no system app and no reserved app id — and its transport buttons call that
  service's `Command`. The physical circle key and swipes use the same service when no media
  app is playing or paused, so a phone works with no media package installed. The panel is a
  strict now-playing projection: it lists only players reporting `PLAYBACK_PLAYING`, so a
  source that is connected but idle, paused or stopped adds no row (and no error card) even
  though it stays discoverable and controllable through the same service.
- **HTTPS Web UI.** An embedded Vue SPA served by the daemon, authenticated with an
  Argon2id admin password and server-side sessions; static API keys carry explicit scopes
  (`status` is read-only; `operator` is full daemon administration, including the MCP tools),
  and the Access page now shows each key's grant. Browser traffic uses the binary protobuf
  `/ws` protocol.
- **MCP frontend.** The daemon also serves a Model Context Protocol server at `/mcp` on the
  same HTTPS listener — no app, no extra port or process. It is an API-key surface that
  exposes the same allowlisted operations and snapshots as the browser, through one shared
  frontend operation boundary, not a second control plane. Keys carry explicit scopes:
  `status` keeps the legacy read-only grant and `operator` is full administration; a key
  minted before scopes existed is never promoted.
- **Boot ownership.** On the stock Rockchip image one removable hook,
  `/etc/init.d/S99btctl.sh`, prepares state, starts and health-gates the daemon, and
  manages the vendor assistant scripts.

## What is implemented

- **Supervision:** manifest validation, unique run IDs, one live run per app (a launch of a
  running app is delivered as a queued, bounded, expiring invocation), graceful stop,
  stdout/stderr capture, immediate first restart then
  1/2/4/8 s backoff. A lost framework connection is a failure, not a deliberate stop.
  Selected display/ring/assistant providers are recovery-managed regardless of
  `restart_on_failure`; the backoff resets only after 30 s of continuous READY, and every
  recovery relaunch re-checks selection. Deselecting (including "none"), uninstalling or
  shutting down is the durable off switch.
- **Built-in startup:** only a needed compiled-in app starts at boot. A provider built-in
  starts when the persisted settings select it and is left stopped when they do not, so an
  explicit "none" display or ring choice survives a restart as a blank output rather than a
  silently running provider; deselecting a provider built-in stops its run. The clock is the
  exception and always runs, because its timers, alarms and stopwatch are its own state, and
  its output still follows the selected baseline. A stopped built-in can be launched on
  demand and keeps its single stable `builtin:<app_id>` run.
- **Packaging:** a pinned `.tar.gz` artifact built and packaged on the build host, with
  `manifest.toml` at the root, regular files and directories only, deterministic bytes. Install
  and update are atomic; a live run must be stopped before its package is replaced.
- **App contract:** typed settings with compare-and-swap revisions, actions, launch
  arguments, declared interfaces with descriptor and docs, native SDK requirements,
  provenance evidence and an optional custom settings UI, documented in
  [docs/app_development/](docs/app_development/README.md).
- **Output and input:** segment display and RGB ring presentations, presets and effects,
  partial/full touch capture, panel gestures, dismissal and stopwatch ownership.
- **Audio:** S16LE mono 16 kHz microphone path with a bounded DSP chain (`framework.dsp.v1`:
  80 Hz high-pass, adaptive suppression capped at 12 dB, unity gain, −1 dBFS limiter,
  256/128 STFT), raw vendor stream only at `audio.input.microphone.raw`, application
  playback with ducking, pre-roll and a visible-indication watchdog. Master volume bounds
  each app's contribution: float samples outside `[-1, 1]` are clamped and non-finite ones
  become silence at decode, an app holds at most four concurrent playback streams, a soft
  limiter (0.95 threshold, 5 ms attack, 150 ms release) holds the bus below full scale, and
  the PulseAudio sink is pinned at unity at startup and on every master change so the sink
  stage cannot rescale framework output.
- **Bluetooth:** BlueZ adapter power, incoming-connections and discoverable policy,
  pairing-agent support, paired-device listing and unpair, AVRCP transport and absolute
  volume, and a native A2DP Sink endpoint with a pure-Go SBC decoder, advertising the spec's
  four-octet SBC capabilities. The Settings page edits the advertised Bluetooth name with a
  pencil button and a modal that validates 1–248 UTF-8 bytes with no control characters; the
  save writes BlueZ `Adapter1.Alias` without power-cycling the adapter or touching its
  policy. After every connection the daemon asserts that a
  `MediaTransport1` carrying the A2DP sink UUID exists and performs one disconnect/reconnect
  repair when it does not; a source that never reports absolute volume is reported as such
  instead of accepting a write that cannot reach it. PulseAudio is the fallback owner when
  native registration fails, and its module set is retired as soon as the native sink is
  armed unless it is actively streaming a connected source.
- **Wi-Fi:** a Linux manager over saved networks, stock `wpa_supplicant.conf` import,
  bounded startup association and a WPA2 setup AP with a restricted captive portal. The AP
  takes the live Bluetooth advertised name when it starts and shortens it to the first 32
  UTF-8 bytes on a rune boundary, because that is the SSID limit; the saved host name is
  only a fallback for a device without Bluetooth.
- **Discovery:** Avahi mDNS host name (`<label>.local`), applied idempotently and never
  written when the label is already active, plus app-facing mDNS publication and browsing
  through `DiscoveryService` and the SDK's `mdns_*` calls. A publication is scoped to the
  calling run and removed on unpublish, run exit and daemon shutdown; a build without a
  responder (the simulator) rejects the call instead of faking success. The daemon validates a
  publication against the DNS-SD limits before handing it to Avahi — a 63-byte instance label,
  at most 16 TXT records and 1024 bytes total, and no single `key=value` TXT item over 255 bytes
  — so an oversized record is an explicit `INVALID_ARGUMENT` rather than a responder failure
  reported as "not running". A name already registered on the link is `ALREADY_EXISTS`: the
  daemon never renames a publication behind the caller's back.
- **Personal wakewords:** on-device training and inference over the shared processed
  microphone tap, with take-provenance sidecars and atomic model replacement. Each installed
  word is its own artifact under `<data-dir>/wakeword/models/<id>.json`, can be enabled or
  paused on its own, and can be bound to a specific app so word A starts app A and word B
  starts app B. All words share one pretrained embedding frontend and one 80 ms runner; each
  keeps its own calibrated threshold and consecutive-window streak, and one global
  2.9-second cooldown plus margin-based arbitration gives at most one activation per
  utterance. A word bound to an app that is not ready is dropped with an explicit reason,
  never rerouted. Unbound words, manual keys and an API activation of an unbound word target
  the selected assistant; an API activation of a bound word is delivered to that same app,
  exactly like a detection. The delivery target is always reported in
  `AssistantActivation.app_id`. See [docs/WAKEWORD.md](docs/WAKEWORD.md#multiple-wakewords-and-app-binding).
- **Shipped app packages:** optional first-party apps are built and packaged on the build host
  (`make simon`, `make pa`, `make radio`, `make homeassistant`, `make airplay`) and installed
  like any other package. `org.capsulad.simon` is a QuickJS colour-and-tone memory game that
  owns the LED ring, an optional score display, a full-panel capture and one speaker stream
  while it runs; `org.capsulad.pa` is a background MicroPython
  service that plays one streamed WAV per routed `Play` call and replies after the queued audio
  drains; `org.capsulad.radio` is a single native internet-radio app that keeps the station
  list, fetches and decodes the selected HTTP(S) MP3 stream in-process through the shared
  `libcapsulad` transport, runs only while it has audio or a live indication, shows the station
  number (or the pause sign) and serves the standard media contract —
  there is no companion package and no app-to-app audio hop; and
  `org.capsulad.homeassistant` is a native MQTT bridge that publishes the speaker to Home
  Assistant through discovery, publishes its state and applies Home Assistant's commands to the
  ring, the display and the Bluetooth source; `org.capsulad.airplay` is a native AirPlay 2 audio
  receiver — the daemon allocates its RTSP, audio and event listeners, Avahi publishes
  `_airplay._tcp` and `_raop._tcp`, sender audio is decoded by the app and mixed by the
  framework, the ring shows the cover-art palette, and the standard media contract carries the
  household controls back to the sender; and `apps/cadence` — optional voice-assistant client
  for a self-hosted Cadence server: framework wake-word and talk-key activation only (an
  API/automation activation is refused rather than fabricating a physical press, because
  cadence.v2 grants owner privileges only for a heard wake word or a press), ring capture
  indication, Opus mic uplink, streamed Opus reply playback, follow-up, dismiss/cancel/error
  cleanup; credentials in masked settings and never logged. None is compiled into the daemon, and none
  is installed by default.
- **Web UI:** six destinations — Overview, Apps, Settings, Assets, Access and Diagnostics. The Apps
  page lists installed packages with their running/system state and installs, updates or removes
  one through the session-authenticated `/api/apps` routes (a raw `.tar.gz` upload, refused while
  that app is running); a change it makes is live, with no daemon restart. The Assets
  page lists `<data-dir>/assets` with size and SHA-256, uploads a file to a store-relative
  path and deletes one through the session-authenticated `/api/assets` routes; the same
  directory is mounted read-only at `/assets` for apps, and uploads are limited to 32 MiB.
  The other pages provide live controls with explicit saves, app detail pages and custom app
  settings documents in an opaque-origin iframe. An app detail page also shows that app's
  optional user guide — the package root `README.md`, or an embedded document for a compiled-in
  app — through the session-authenticated `GET /api/apps/{appID}/guide`. The guide is inert
  Markdown rendered as text by the SPA (no markup, link target, script context or network
  primitive), it is readable whether the app is running or stopped, and a package without one
  is reported as having no guide rather than as a broken page.
  The Overview's "Now playing" card is a strict projection: it lists only players that report
  `PLAYBACK_PLAYING`, from ready application providers and from the framework Bluetooth
  provider, and omits a provider that has none. An idle, paused or unreadable provider adds no
  row and no error card; the app list on the same page still shows a stopped or paused app with
  its launch control and declared actions, and a ready provider is still reported and commanded
  through `capsulad-cli media list`, `ListPlayers` and the framework's Bluetooth media service,
  so this is a display policy, not a discovery or control restriction. The card
  converges live without polling: the daemon subscribes to each ready media provider's
  `WatchPlayers` stream and to `BluetoothMediaService.WatchState`, and rebuilds the overview on
  an observed change.
  The overview's Bluetooth speaker card shows a
  green `connected` chip beside the speaker-mode toggle when any `pairedDevices` row has
  `connected === true`; its tooltip and `aria-label` read `Connected: <names>`, and the chip
  disappears when no device is connected. This is snapshot state, not a Bluetooth-policy cache.
  The Access page labels each static key "status only" or "full administration".
- **MCP:** one Streamable HTTP JSON-RPC endpoint at `https://HOST:8443/mcp` with
  `initialize`/`tools/list`/`tools/call`/`DELETE` sessions, scope-filtered tools and the same
  operations as the Web UI (settings, apps, media, Bluetooth, Wi-Fi, wakeword training, clock,
  assets and package install/remove). Input schemas are generated from the protobuf
  descriptors. See [docs/MCP.md](docs/MCP.md).
- **Operations:** in-place daemon upgrade through the boot service with a health gate,
  a 512 KiB rotating log, bounded storage guard, and deployment bundles applied by
  `capsulad-cli deploy`.
- **Simulator:** complete virtual segment display, LED ring, touch/keys and audio, plus
  development injection sockets.

## Application model

An application is a package directory installed under `<data-dir>/apps/<app_id>`, or one of the
built-in system apps (`capsulad.clock`, `capsulad.ring`). A package ships as a
deterministic `.tar.gz` whose root entry is
`manifest.toml`, produced on the build host by the app's build target; `capsulad-cli app
install` validates the archive and installs it atomically, and `capsulad-cli app remove` deletes
it. A directory argument is a developer convenience, not a different package format.

```toml
id = "org.example.weather"
name = "Weather"
version = "0.1.0"

[entrypoint]
runtime = "native"          # native | quickjs | micropython | builtin
path = "bin/weather"
architecture = "aarch64"
abi = "linux-gnu"

[[settings]]
key = "city"
label = "City"
type = "text"
default = "Moscow"

[[interfaces]]
service = "org.example.weather.v1.WeatherService"
version = "v1"
descriptor = "proto/weather.protoset"
```

A launch of an app that already has a live run is delivered to that run as an invocation
instead of starting a second process. Otherwise the supervisor allocates a run ID, starts the
entrypoint in the probed sandbox with the framework environment, and waits for the app to
connect, send `AppHello{run_id}` and then `AppReady`. Identity comes from the connection plus
Linux `SO_PEERCRED`, never from a claim. Declared interfaces become available only while the
run is ready. A selected default provider (display, ring or assistant) is restarted on failure;
ordinary apps follow `restart_on_failure`. Every output, capture, audio stream and conversation
must be released on completion, dismissal, cancellation, failure or process exit.

Settings are declared in the manifest and stored per app with compare-and-swap revisions;
actions are transient declarations, not persisted state. An app may also ship one
self-contained settings document, which the Web UI renders in an opaque-origin iframe with
no host access beyond validated own-setting patches. A package may in addition carry one
optional root `README.md` user guide — operator-facing help, not a manifest field and not an
interface's `docs` Markdown — which the Web UI renders as inert text in the app detail. See
[docs/app_development/manifest.md](docs/app_development/manifest.md#user-guide-readmemd).

## Deployment and boot

A deployment bundle is a directory or `.tar.gz` with these top-level entries:
`capsulad`, `lib/` (native runtime and headers), `apps/*.tar.gz`, `boot/S99btctl.sh` and,
optionally, `capsulad-cli`. `make deploy-bundle` includes the CLI by default;
`make deploy-bundle BUNDLE_CLI=0` assembles a CLI-free bundle that leaves a separately
installed `<data-dir>/capsulad-cli` untouched (neither removed nor updated), so on-device
deployment and recovery then need a CLI pushed separately, for example through the
temporary directory. The bundle is assembled from ready-made host-built
archives — build them first with `make dumbassistant`, `make marusia`, `make product-apps` (the
five product apps) or `make devapps` — and never packages an app. `deploy-bundle` refuses to
assemble while `build/apps` still holds a staged directory or archive for the retired
`org.capsulad.radio.tuner` companion, printing the `rm -rf` cleanup instead of shipping it; a
fresh bundle never stages that package. `capsulad-cli deploy PATH`
validates the whole bundle before changing installed state, stages it outside the data partition,
stops the daemon, installs or updates packages and managed runtime files, replaces the binaries
the bundle carries (one or both),
optionally installs the boot hook, restarts through the boot service and gates on
`capsulad -health`. A replaced binary is kept
recoverable only until the health gate passes — renamed aside when the staged file is on the data
filesystem, otherwise gzip-compressed in staging after the target was removed, since `/data` cannot
hold two 40 MiB daemons. A failure after the stop restores what was replaced and restarts the
previous stack.
On the device the data directory is `/data/capsdemo`:

```text
/data/capsdemo/capsulad          daemon
/data/capsdemo/capsulad-cli      administration CLI
/data/capsdemo/lib/              libcapsulad, capsulad-native.json, interpreter hosts, headers
/data/capsdemo/apps/<app_id>/    installed application packages
/data/capsdemo/assets/           shared asset store, mounted read-only at /assets
/data/capsdemo/run/hal.sock      framework socket
/data/capsdemo/security/         certificate and credential store
/data/capsdemo/capsulad.log      bounded daemon log
/etc/init.d/S99btctl.sh          boot hook
```

`-restart never` skips the restart and the boot-hook install. The `.prev` files are removed
once the health gate passes; they are a transient buffer for the deployment in flight.

`make marusia` builds and packages the second optional assistant
(`build/apps/org.capsulad.marusia-<version>.tar.gz`). It has no packaged assets and no
embedded credentials: the reply is a TTS URL fetched at turn time. Its credential is a vendor
session supplied by the operator: the masked `session_id` and `session_secret` app settings (with
`device_id` also masked) are the supported path, applied on every settings push without an app
restart, and the app never logs them. The vendor agent's root-only
`/data/voice-agent/session.json` is only a best-effort fallback for a development run outside the
sandbox. A configured session is validated and never replaced by an attach; the provisioning
`xtoken_id` attach runs only when no session is available, and an empty `xtoken_id` is still sent —
the service, not the app, decides whether an unprovisioned device is acceptable. A refusal with
service code `4002` is a provisioning state, not a transient failure — it is not retried per turn and
is reported in the app log and on the ring. Vendor sessions expire when the vendor revokes them
(account change, sign-out of all devices, or the session's own lifetime), so recovery is an operator
action: store the new `session_id`/`session_secret` through the settings API; nothing on the device
can mint a replacement. A turn that receives `UNAUTHENTICATED`/`PERMISSION_DENIED` drops its session,
logs one actionable line and re-resolves the credential — settings first, then the fallback file —
before the next turn.

## Security model

- **Local-only framework plane.** The gRPC API is a Unix socket. There is no network gRPC
  listener; the browser Web UI is a local administrator client of that socket.
- **Identity, not assertions.** Supervised apps are identified by run ID, peer credentials,
  sandbox UID and supervised PID. A built-in run additionally requires the daemon's
  per-boot token, because every in-process client — the Web UI included — shares the daemon's
  PID, so a PID match alone never selects `capsulad.clock` or `capsulad.ring`; the Web UI's own
  connection stays an administrator. The administrator run table lists each built-in once under
  its own app id with its real state, and launch/stop act on the run of the requested app.
  Omitting a run ID does not grant administrator rights.
- **Administrator operations** include device settings, diagnostics and log history,
  forced capture release, microphone mute and master volume, wakeword records and training,
  Bluetooth policy and unpair, and application launch/stop. Per-app settings, actions and
  run listings are scoped to the calling app.
- **Microphone privacy indication** requires both a selected, visible, non-blank ring or
  segment presentation at non-zero brightness and a microphone-live cue the mixer consumed
  and drained. Missing output, zero volume, mute, a blank indication or an undeliverable cue
  deny capture rather than record quietly.
- **Web UI and MCP boundary.** Sessions use a self-signed HTTPS certificate, an Argon2id
  password and CSRF/origin checks. Static API keys carry explicit scopes: `status` reaches
  only `GET /api/status`, the public certificate and the two matching MCP read tools;
  `operator` reaches the MCP administration tools. The browser's Access page mints
  `status` keys, and a key created before scopes existed stays read-only. MCP accepts only an
  API key (never a session cookie) and checks Origin. The Access page lists every key with its
  scope, so a status key and an administration key can be told apart; the page itself mints
  status-only keys, and an `operator` key is created on the device with
  `capsulad-cli auth key create NAME --scope operator`. There is no generic AppBus, capture,
  audio or asset proxy for either frontend.
- **Asset store.** `<data-dir>/assets` is a plain directory catalog: every regular,
  non-symlink file below the root is an asset, addressed by its slash-relative path and
  visible read-only at `/assets`. Only the session-authenticated admin API writes it.
- **Provenance** is optional per app. A verified Sigstore/GitHub attestation describes the
  binary digest, signature, certificate chain, transparency log and workflow identity; it
  is evidence, not a runtime safety guarantee.
- **Which first-party apps reach the network.** The daemon, the framework plane and every other
  first-party component stay local-only. Three optional packages open outbound connections, each
  only after the operator configured it or asked it to play:
  - `apps/marusia`, the assistant. While it is installed and
    selected as the assistant, a turn sends microphone PCM and the device identity (serial from
    the non-empty `serial` setting, otherwise `DeviceService.GetInfo.hardware_serial`, otherwise
    host-only local `/proc/cpuinfo`, otherwise empty; plus the distinct `device_id` value and the
    advertised client version) together with the vendor session credential it holds — the
    masked `session_id` and `session_secret` settings, or the session recovered from the
    vendor agent's root-only state file — to the configured vendor endpoint over HTTPS, and
    logs the selected serial source once per process without logging the serial value. The
    `Invalid 'x-device-sn'` rejection remains legible, directs operators to the `serial` setting
    and the framework-reported serial, and preserves the original gRPC status; when no source
    resolves, the warning names that setting. It fetches the HTTPS TTS URL that endpoint returns.
    Nothing is sent when the app is not selected or
    idle. With a session configured the app validates it without an attach; only a device
    without a session sends the provisioning attach, which carries the device serial and the
    configured `xtoken_id` (empty when unset) and whose refusal is logged. A turn with no
    recognised text and no command is reported as a no-answer with the terminal error
    indication, never as a completed turn.
  - `apps/homeassistant`, the bridge. It is an MQTT client for the broker named in its
    `broker_host`/`broker_port` settings, or for the broker it finds by browsing `_mqtt._tcp`
    when `broker_discover` is on. It publishes its discovery payloads, its availability and one state
    topic per published entity, and applies the commands Home Assistant sends back to the ring,
    the segment display and the Bluetooth source; `broker_tls` performs a verified TLS 1.2+
    handshake through the shared transport with no insecure mode (as a plain MQTT session it
    sends everything in clear on the local network), and the username/password settings are the
    broker credential. It publishes device state to whatever broker the operator points it at, so
    the broker is as trusted as the operator makes it; nothing is sent while the app is stopped
    or unconfigured, and it never opens a listening socket.
  - `apps/radio`, the internet radio. It opens one outbound HTTP/1.1 connection per play to the
    station URL from the operator's station list (verified TLS for `https://`, plain for
    `http://`), follows bounded redirects and one playlist level, refuses an HTTPS→HTTP
    downgrade, strips ICY metadata and decodes the MP3 in-process. It sends the HTTP request and
    nothing else — no credential, no device identity — and it plays only the station the user
    selected. Nothing is fetched while the radio is stopped or paused, and a pause ends the
    network stream, so a resume re-dials. There is no companion app and therefore no app-to-app
    audio path: the fetch, decode and speaker write happen in one supervised run, and its
    packaged settings document can only submit patches for its own declared settings through the
    sandboxed `capsulad.settings-ui/1` bridge. See
    [routed calls and trust](docs/app_development/security.md#routed-calls-and-trust).
  - `apps/airplay`, the AirPlay 2 receiver. It is the one first-party app with inbound
    network surface, and every part of it is daemon-mediated: the RTSP control port, each audio
    channel, the event channel and the PTP multicast receiver are listeners the daemon allocated
    and bound under its policy (PTP group joins are restricted to 224.0.0.1 on ports 319/320),
    and `_airplay._tcp`/`_raop._tcp` are published by the daemon's Avahi responder, which
    validates the 63-byte instance label and the 255-byte per-item TXT limit before registering.
    The app never binds a socket and never reaches the internet: it writes RTCP reports and
    remote-control commands only to the sender that connected to it, and it decodes the sender's
    audio locally. Both records publish the same 32-byte ed25519 HomeKit long-term public key as
    `pk`; the legacy 2048-bit RSA key is never advertised (its base64 form exceeds the TXT item
    limit) and is used only to decrypt an `ANNOUNCE` that carries `rsaaeskey`.
    A sender on the local link is as trusted as the operator's network makes it; there is no
    password and no pairing code, matching the upstream receiver's transient-pairing behaviour.

## Current limits

- **Sandbox strength on the RK3308 stock kernel is mount-only.** The kernel exposes only
  a mount namespace; `CLONE_NEWUSER`, `CLONE_NEWNET` and `PR_SET_SECCOMP` fail. The daemon
  probes the capabilities, runs each app in a root mount namespace with a distinct UID,
  and reports `mode=mountns`. Native apps are trusted code; provenance verification is
  the control that matters, not runtime isolation.
- **No raw touch coordinates on the Mini.** Its only evdev node is `gpio-keys` and it advertises
  `EV_SYN|EV_KEY` only, so `coordinates_available=false` and `full_raw_available=false`; all five
  keys and inferred gestures remain available with `full_panel_available=true`. Because there is no
  absolute axis, a crossing is only visible as ordered key transitions: a swipe that reports a single
  end-stop key is indistinguishable from a volume tap and acts as one, while two opposite volume
  edges within 400 ms are the full-panel crossing (the leading step is reverted). Raw captures show
  a swipe emits exactly one key press/release pair, so skip gestures depend on either the centre key
  (`0x2c0`/`0x246`) appearing in the sequence or the ordered opposite edge pair; the gesture windows
  are host-verified and their
  on-device recognition after deployment is still to be confirmed by physical presses.
- **Script hosts inherit the mount-only sandbox.** QuickJS and MicroPython are sandboxed
  exactly like native apps; there is no stronger isolation available.
- **Apps share the device's network stack.** `CLONE_NEWNET` is unavailable, so there is no
  per-app network namespace and no per-destination egress allowlist. Listener binds are
  daemon-policed, but a compromised app can still reach anything the device routes. Treat
  native apps as trusted code.
- **The Amlogic A113X clock panel is unverified.** Both Amlogic targets are implemented
  (`-platform amlogic-stock`, `-platform amlogic-alpine`, wire enums
  `CAPSULA_MINI_AMLOGIC_STOCK` / `CAPSULA_MINI_AMLOGIC_ALPINE`). The stock vendor panel char device
  (`/dev/is31fl32xx`, ISSI IS31FL3236A or Awinic AW21036 depending on the batch) has no recovered
  ioctl ABI, so segment frames stay in memory and the subsystem reports `UNVERIFIED`; on Alpine the
  panel is driven through the kernel line display or per-segment LED class devices when present.
  Amlogic audio and brightness behaviour are host-verified only, and the Amlogic **watchdog
  timer** is unverified — pending image inspection, with the supervisor's release behaviour
  host-verified rather than the driver's; see
  [docs/HARDWARE.md](docs/HARDWARE.md#amlogic-a113x-stock-buildroot-and-alpine-linux). No parity
  with the Rockchip board is claimed.
- **`SystemService.ControlUnit` returns `UNIMPLEMENTED`.**
- **No app-facing Wi-Fi service.** Wi-Fi is owned by the authenticated Web UI and the
  isolated captive portal; apps cannot read or change network configuration.
- **Bluetooth media has no provider app.** The daemon ships no Bluetooth media application and
  reserves no app id for one: the Web UI "Now playing" panel lists the connected AVRCP source
  as a framework-owned provider read from `BluetoothMediaService` — one player `bluetooth`
  while playback is *observed* (the peer's own status, transport receipts or transport state,
  never this daemon's command memory), and no row for a connected but idle, paused or stopped
  source — and its PLAY/PAUSE/SKIP_NEXT/SKIP_PREVIOUS commands are that service's `Command`,
  the same path as the physical circle key and swipes. An operator can install third-party
  providers; consumers discover every running provider through `AppInterfaceService`, and a
  source the panel does not list stays readable and controllable through that same service —
  `BluetoothMediaService.GetState` and `Command`, reachable from an API-key client or the MCP
  `media_command` tool — with its transport buttons returning to the panel as soon as playback is
  observed again.
- **Bluetooth media is source-dependent.** AVRCP and absolute volume require a connected
  source. BlueZ creates `MediaTransport1.Volume` only from a peer report and refuses a write
  that would create it, so a source that never reports absolute volume is reported as such
  (one warning per source session plus a sentence in the status detail); the configured
  master level is re-asserted once when the first report arrives. A source exporting only
  `MediaControl1` publishes no playback status, so a toggle direction comes from this
  daemon's own last command, the owned transport's packet receipts, `MediaPlayer1.Status`
  and the transport state, in that order. HSP/HFP telephony is not implemented.
- **Native A2DP has a PulseAudio fallback.** Registration is verified before the adapter
  is made connectable; if it fails or is lost, PulseAudio owns routing. After a connection
  the daemon requires an A2DP `MediaTransport1` for the source and performs one
  disconnect/reconnect repair when it is missing. A BlueZ restart or adapter power cycle
  invalidates the registration until the next connect repairs it. With the native sink
  armed, PulseAudio's Bluetooth modules are retired as soon as they are not actively
  streaming a connected source, so the next connection cannot land on its Headset endpoint.
- **The Web UI binds TCP and uses a self-signed certificate.** Set `-web-addr`
  deliberately and trust the fingerprint shown by `capsulad-cli cert status`; browsers
  warn until then. API keys never reach the console, AppBus, captures or audio; `-web-addr`
  also exposes the MCP endpoint, which requires an `operator`-scoped key for anything beyond
  status reads.
- **Memory and `/data` are the binding constraints.** The board has 240 MiB RAM without
  swap, the data partition is 78.5 MB, and the daemon defaults to an 80 MiB soft heap
  limit. A package may be at most 64 MiB unpacked with no file above 48 MiB; one asset may
  be at most 32 MiB; wakeword recording and training are refused below 2 MiB free.
- **Boot ownership is one hook with an ordering constraint.** `rcS` expands
  `/etc/init.d/S??*` before the `/data` overlay pivot, so the hook must live at the stock
  `S99btctl.sh` path. Wiping the overlay returns the device to the stock assistant and the
  deployment must be repeated.
- **Provenance verification is optional and offline.** A missing Sigstore trusted root
  yields `PROVENANCE_UNVERIFIED`, never a false positive; refreshing the root is an
  operator task.
- **Wakeword models are personal, one per word.** No speaker-independent keyword head
  ships, and a new word is only as good as its own recordings: training one word teaches
  nothing to another, and each word keeps its own calibrated threshold. A word trained on
  raw audio runs on the processed tap with an explicit unverified-threshold warning.
  Per-word acoustic acceptance on the target hardware remains a device gate.
- **The Marusia assistant needs a vendor session (or an accepted provisioning attach) and
  network egress.** The optional `apps/marusia` turn sends microphone PCM, its session
  credential and device identity to its configured vendor endpoint over HTTPS and fetches the
  returned TTS URL; the masked `session_id`/`session_secret` settings are the supported
  credential path and take effect on the next turn without a restart, while a device with no
  session falls back to the provisioning attach with the configured `xtoken_id`. A refusal with
  code `4002` means the device was never provisioned: it is not transient and is not retried per
  turn. Sessions expire when the vendor revokes them, and only an operator can store a new one;
  a rejected session is dropped and re-resolved from the settings, then the vendor session file,
  before the next turn. Upload framing is one self-contained canonical WAV per 80 ms chunk, and
  `audio_format=ogg_opus` is not implemented in this build, so a non-wave upload format refuses
  turns until it is set back. Its reply playback supports RIFF/WAVE linear PCM only, so an Opus or
  MP3 body is an explicit actionable error and error ring state, not silence. A clean phrase with
  no recognised text and no command is a no-answer: the app says so and shows the terminal error
  indication instead of reporting a completed turn.
- **The internet radio is MP3-only.** `org.capsulad.radio` is a single native app (the
  MicroPython front end and the `org.capsulad.radio.tuner` companion are retired): it fetches
  and decodes in its own process, so there is no second package to install and no app-to-app
  audio path to authorize. It accepts MPEG-1/MPEG-2 Layer III over HTTP/1.1 only — AAC/HE-AAC,
  Ogg/Opus/Vorbis, FLAC, MP4/M4A, WAV and HLS are refused with an explicit error rather than a
  fake fallback or a second decoder — accepts no gzip, follows at most five redirects with an
  HTTPS→HTTP downgrade refused, and resolves at most one `.pls`/`.m3u` playlist level (≤64 KiB).
  Pause ends the network stream by design, so resuming re-dials the station, and a reconnect is a
  new session with a marked discontinuity, not a seamless resume. A pause is a bounded state: the
  segment display holds a pause sign for one minute, and the run exits if it is not resumed. The
  app is not a resident service — a bare launch waits one idle minute and a terminal error holds
  its dashes for the tuning window (or up to one idle minute with **Always display**), then each
  releases the display to the selected baseline and exits cleanly; a new launch is the recovery.
  A stream that never starts or
  goes silent never leaves a stale `PLAYING`: a 10 s read-idle bound fails the stream, up to
  three reconnects with 1/2/4 s backoff are attempted inside the same session, and an autonomous
  watchdog bounds "no audio reached the speaker" at 60 s while buffering or playing, moving the
  app to its error state with no user activity.
  A station switch while active replaces the owned playback, so a superseded session's late
  open, read or failure cannot clear the newer stream or selection.
- **The Home Assistant bridge needs a reachable MQTT broker.** A named broker, or an
  mDNS-discoverable one (`broker_discover` browses `_mqtt._tcp`; on a build whose daemon has no
  responder — the simulator answered `DiscoveryService/Browse` with `unknown service` — a broker
  must be named). Without one it publishes nothing and says so in the status sensor. It exports
  the surfaces the device advertises (ring, segment display, Bluetooth source) plus the sensor,
  touch and diagnostics entities that are switched on; neither master volume nor microphone mute
  is writable by the bridge, so both are read-only entities, and the display is a face selector
  rather than text because the framework ships no segment font. The Bluetooth source is enumerated
  (`sensor` with `playing`/`idle`/`off`, a title sensor) with four transport buttons rather than a
  `media_player`, because Home Assistant's MQTT integration has no media-player platform. Sensor
  units are published in Home Assistant's vocabulary (`°C`, `lx`) and anything else verbatim
  without a device class, and entity ids are Home Assistant's own: the bridge publishes
  `unique_id`, not `object_id`. A broker that is plaintext on the LAN, a wrong credential, an
  unauthorised topic or an unknown command is reported, never guessed or retried into a state
  change; a broker that accepts a connection and then stops answering (no CONNACK or SUBACK, or a
  write that wedges) cannot leave a half-online bridge: both exchanges are bounded at 10 s, writes
  are bounded and context-aware (2 s for `DISCONNECT`, 5 s for client control writes), so the
  session fails, publishes `offline` and retries with a paced backoff — 1 s, 2 s, 4 s … capped at
  1 min, reset only after a session became usable and held for 30 s — so even a healthy session
  that drops inside that window backs off further, deliberately. A broker it cannot really use
  therefore retries slowly instead of flapping online/offline about once a second and republishing
  the whole retained discovery set each time; a deliberate reconnect is still immediate.
- **Simon Says takes the whole panel and the ring while it runs.** Its capture is full-panel, so
  the framework's volume, mute, media and assistant actions are game input for as long as the
  game is on the panel; holding the unmarked centre for at least 800 ms is the only documented
  way back. The game also owns the LED ring presentation, so the selected ring baseline is
  replaced for the duration, and a routing preemption of the ring or the panel ends the run
  rather than continuing with invisible output or a dead panel.
- **AirPlay has no live-sender verification.** The receiver's wire behaviour is implemented from
  the protocol facts and checked against fixtures, the reference receiver's packet layouts and an
  independent sender loopback; no stream from an Apple sender has been captured and no acoustic
  output is claimed. Field evidence covers the advertisement reaching a sender's mDNS browser and
  one `RTSP/1.0 200 OK`. See [apps/airplay/README.md](apps/airplay/README.md#interoperability-gate)
  for the open items.
- **Persistent app storage beyond typed settings is not provided.** Apps own typed
  settings and their read-only package; there is no general per-app writable volume.
- **The daemon-to-app wire protocol may change with the daemon.** Schema and message
  compatibility guarantees are limited to the native C ABI's major version.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/capsulad` | daemon entrypoint and flags |
| `cmd/capsulad-cli` | administrative CLI: apps, deploy, certificates, settings, logs, platform tools |
| `cmd/ledmap`, `cmd/waketrain`, `cmd/wakeword-negatives` | bring-up and training tools |
| `internal/daemon` | wiring, gRPC services, supervision, routing and output loops |
| `internal/bus` | reverse-RPC app bus |
| `internal/presentations`, `internal/routing` | dynamic outputs and capture stack |
| `internal/audio`, `internal/input`, `internal/wakeword` | realtime services |
| `internal/bluetooth`, `internal/wifi`, `internal/discovery` | BlueZ, Wi-Fi and mDNS integrations |
| `internal/backend/profile` | data-only board profiles: Rockchip Capsula Mini rev 4 stock and the neutral simulator |
| `internal/backend/simulator.go` | complete virtual backend |
| `internal/backend/rockchip` | RK3308 stock backend: board paths, quirks and vendor LED ABI |
| `internal/backend/rockchip/inspect` | offline stock-firmware inspector |
| `internal/sandbox` | Linux namespace/UID launcher and runtime discovery |
| `internal/artifact`, `internal/manifest`, `internal/registry` | package format, manifest schema, installation |
| `internal/assets` | shared read-only asset store |
| `internal/websecurity` | self-signed TLS, Argon2id password, API keys, sessions |
| `internal/provenance` | Sigstore/GitHub attestation verification |
| `internal/webui`, `web` | HTTPS server, MCP frontend, embedded SPA and its protobuf WebSocket |
| `internal/sounds` | embedded framework UI cues and their CC BY-SA `COPYING` |
| `sdk/native` | Rust shared engine behind the C ABI: UDS gRPC, HTTP/TLS, remote gRPC, primitives, header and C example |
| `sdk/go`, `sdk/rust`, `sdk/cpp` | bindings and examples over the shared library |
| `interpreter` | pinned QuickJS/MicroPython hosts, script SDKs, samples and smoke test |
| `apps/devtools`, `apps/dumbassistant`, `apps/marusia` | development apps and the two optional assistants |
| `apps/simon`, `apps/pa`, `apps/radio`, `apps/homeassistant`, `apps/airplay` | first-party product app packages: Simon Says (QuickJS), PA Speaker (MicroPython), Internet Radio (native, single package), the Home Assistant bridge, and the AirPlay 2 receiver. Each package root `README.md` is also the app's packaged user guide |
| `deploy/rockchip-stock` | the single boot hook and bring-up scripts |
| `proto` | framework and standard contract schemas (media, clock, ring, web) |
| `docs` | application manual, development, verification, hardware, behavior, wakeword and glossary |

## Documentation

| Document | Read it for |
|---|---|
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | prerequisites, build targets, cross builds, packaging, deployment, simulator, tests, admin commands |
| [docs/MCP.md](docs/MCP.md) | the daemon's MCP frontend: enablement, key scopes and provisioning, protocol and error surface, the tool catalogue, upload limits and proof level |
| [docs/app_development/README.md](docs/app_development/README.md) | the application manual: supported targets, prerequisites, a complete native app and the page index |
| [manifest](docs/app_development/manifest.md), [lifecycle](docs/app_development/lifecycle.md), [sdk](docs/app_development/sdk.md), [network](docs/app_development/network.md), [app bus](docs/app_development/app-bus.md) | artifact and manifest fields, launch/identity/restart, the native SDK including HTTP/TLS, remote gRPC, streams, UDP, listeners and primitives, the `<label>.local`/mDNS surface, and script hosts, routed calls between apps |
| [services](docs/app_development/services.md), [output & input](docs/app_development/output-input.md), [audio](docs/app_development/audio.md), [settings & actions](docs/app_development/settings-actions.md) | every framework service and method, presentations/captures/keys/gestures, audio streams and indication, typed settings and actions |
| [assets](docs/app_development/assets.md), [standard contracts](docs/app_development/standard-contracts.md), [packaging & deployment](docs/app_development/packaging-deployment.md), [security](docs/app_development/security.md), [troubleshooting](docs/app_development/troubleshooting.md) | the asset store, clock/ring/media contracts, install and deployment bundles, sandbox and trust boundaries, common failures |
| [docs/VERIFICATION.md](docs/VERIFICATION.md) | proof levels, measured device evidence, host-only evidence, unavailable paths and open gates |
| [docs/SPEAKER_BEHAVIOR.md](docs/SPEAKER_BEHAVIOR.md) | user-facing interaction contract, feedback ownership and the future-change gate |
| [docs/HARDWARE.md](docs/HARDWARE.md) | Capsula Mini rev 4 hardware evidence and firmware findings |
| [docs/WAKEWORD.md](docs/WAKEWORD.md) | detection, enrollment, training and model evidence |
| [docs/GLOSSARY.md](docs/GLOSSARY.md) | project terms as this codebase uses them |
| [apps/radio/README.md](apps/radio/README.md), [apps/simon/README.md](apps/simon/README.md), [apps/homeassistant/README.md](apps/homeassistant/README.md), [apps/pa/README.md](apps/pa/README.md), [apps/pa/docs/speaker.md](apps/pa/docs/speaker.md), [apps/airplay/README.md](apps/airplay/README.md), [apps/airplay/docs/media.md](apps/airplay/docs/media.md) | each first-party app's own manual, which is also the guide packaged with it: the internet radio's station list, named picker, controls, display states, streaming limits and the migration off the retired tuner companion; Simon Says' colour/tone mapping, score and exit; the Home Assistant bridge's entities, settings and known gaps; the PA Speaker's `Play` contract, request framing, rejections and limits; the AirPlay receiver's supported sender flows, its advertisement records, its explicit AAC/FairPlay/PTP gaps, settings and the unproven device gate; and the media players they provide |
| [AGENTS.md](AGENTS.md) | repository working rules, lifecycle invariants and verification gates |

## Attribution

- **capsula-mini-airplay2** (https://github.com/ryysel/capsula-mini-airplay2) — the AirPlay
  receiver behaviour this app reimplements: the compact-speaker model string, the stock-profile
  latency intent, the cover-art ring rendering and the single-volume transport interaction.
  Upstream packages Shairport Sync 5.6.2 and NQPTP 1.2.9 plus its own installers; none of that
  is included or redistributed here, and `apps/airplay` is an independent Go implementation.
  **Attribution to the upstream author is explicitly pending exact agreement with that
  author**; this repository claims no affiliation with or endorsement by the project.
- **Hardware sources** — the Amlogic A113X facts come from cmp18's published survey
  (debianlive.ru, 2026-08-23), `ryysel/capsula-mini-airplay2`, `ryysel/marusya-hw` and
  `byrbyrka/capsula-mini-tweaker`; [docs/HARDWARE.md](docs/HARDWARE.md) names them and marks what
  is third-party evidence rather than a measurement in this repository.
- **Shairport Sync** (https://github.com/mikebrady/shairport-sync) and **NQPTP**
  (https://github.com/mikebrady/nqptp) — the reference for AirPlay wire behaviour (RTSP method
  handling, HAP pairing key derivation, RTP/RTCP and timing packet layouts, PTP message layout).
  The AirPlay receiver reproduces the fixed FairPlay handshake reply constants from
  `rtsp.c` (`handle_fp_setup`), which is MIT-licensed: "Copyright (c) James Laird 2013;
  modifications copyright (c) Mike Brady 2014–2026. Permission is hereby granted, free of
  charge, to any person obtaining a copy of this software and associated documentation files
  (the 'Software'), to deal in the Software without restriction … The above copyright notice
  and this permission notice shall be included in all copies or substantial portions of the
  Software." No other code from either tree is included; NQPTP is GPLv2 and nothing GPL is
  linked or copied.
- **FFmpeg** (https://ffmpeg.org) — the AirPlay receiver links `libavcodec`/`libavutil` for AAC
  decoding, the same decoder the upstream receiver links. FFmpeg is LGPL-2.1-or-later; the app
  uses only the AAC decoder and links the libraries, and the deployment ships them as separate
  shared objects where the platform does not already provide them.
- **Canonical** — the `ubuntu-sounds` 0.3 set used for the framework's UI cues
  (`internal/sounds`), licensed CC BY-SA. The licence text is
  [internal/sounds/COPYING](internal/sounds/COPYING) and the files are unmodified.
- **Material Symbols Rounded** (https://fonts.google.com/icons) — the Web UI serves a
  generated subset of the Material Symbols Rounded variable font (90 glyphs; axes intact),
  Apache-2.0, copyright Google LLC. The font is unmodified apart from subsetting; the source
  package, exact glyph list and hashes are recorded in `web/src/assets/icon-font.json`. The
  licence text is [web/src/assets/LICENSE-material-symbols.txt](web/src/assets/LICENSE-material-symbols.txt)
  and is also shipped with the embedded Web UI assets, which serve it at
  `/LICENSE-material-symbols.txt` with the notices in `/THIRD-PARTY.md`.
- **station-wakeword** (https://github.com/tangenx/station-wakeword) — reference material for
  the station wake-word work that the personal wake-word pipeline was developed and validated
  against. No source from that project is vendored here; the detection and training evidence
  is in [docs/WAKEWORD.md](docs/WAKEWORD.md).
- **Cadence** — the voice-assistant platform the `apps/cadence` client speaks to (device link
  protocol `cadence/v2`: WebSocket `GET /v2/device`, protobuf `Envelope` frames, bearer device
  token, Opus 16 kHz mono uplink, Opus reply streams). Source:
  https://git.shino.boo/devilreef/cadence/ (self-hosted, GitLab). The client implements the
  wire protocol against that upstream definition; no upstream source code is vendored and no
  vendor binary is redistributed. Its protobuf files are an independently written subset of
  cadence.v2 (same field numbers and enums), and its `.binpb` test vectors are upstream-generated
  data from upstream `main` commit `f9571ad0d89acd7326414adf04a85ea1c9dec131`.

## Quick start

```sh
make                                   # release build: daemon, CLI, native SDK, bindings
mkdir -p build/demo
./build/bin/capsulad-cli -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
./build/bin/capsulad -platform simulator -data-dir build/demo -dev-admin build/demo/run/dev.sock
# open https://127.0.0.1:8443/ and sign in with the printed admin password
```

`make test` runs the root Go module, `sdk/go`, the nested app Go modules (the three assistants,
the Internet Radio, the Home Assistant bridge and the AirPlay receiver), the Rust binding, the
native crate and the C++ example; `make test-apps` adds the Node/CPython app suites. See
[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for the full build, packaging and deployment workflow.