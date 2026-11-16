# capsulad — appliance daemon for the Capsula Mini

`capsulad` is the local appliance daemon and application framework for the Capsula Mini
speaker; `capsulad` is the project and the daemon, and "Capsula Mini" is only the hardware
name. It supervises application processes, owns the hardware abstraction, routes
dynamic presentations and captures, mixes audio, serves an HTTPS Web UI, and exposes one
local gRPC surface over a Unix socket. Applications are ordinary executables or scripts
that talk to that socket through the shared native SDK or a script host; they never listen
on a socket of their own or write hardware directly.

The supported hardware is the Capsula Mini rev 4 (Rockchip RK3308, aarch64 glibc 2.33).
A complete virtual backend (`-platform simulator`) implements the same contracts for
development and tests.

The administrative CLI is `capsulad-cli` (`capsulad-cli status`, `app`, `deploy`, `auth`,
`cert`, `settings`, `logs`, `media`, `platform`, `dev`). The daemon-to-app wire protocol may
change together with the daemon; the native C ABI is v1 and is frozen within its major
version.

## Architecture

```text
 native apps (Go/Rust/C/C++)  script apps (QuickJS, MicroPython)
          │ CAPSULA_LIBRARY             │ capsula-quickjs / capsula-micropython
          └──────── libcapsula ─────────┘
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

- **One Unix-socket gRPC API.** Sixteen framework services (device, settings, routing,
  presentation, audio, input, assistant, application, application configuration,
  app-interface, app-bus, sensor, system, Bluetooth media, diagnostics, asset) on one socket.
  There is no network gRPC listener and no per-app IPC socket.
- **AppBus reverse RPC.** Each app holds one long-lived `Connect` stream. Routed unary
  and streaming calls reach other apps through `AppBusService.Call`; no app accepts an
  inbound connection.
- **Shared native SDK.** `libcapsula` is a pure Rust cdylib exposing a frozen C ABI v1
  through one bounded `capsula_get_api` table. It owns the transport, protobuf/JSON
  reflection, AppBus sessions, routed calls and the raw-PCM audio path. Thin Go and Rust
  bindings load it dynamically; C and C++ use the installed header.
- **Presentations and captures.** Apps open output presentations and acquire routing
  captures. First valid frame makes a presentation eligible; the latest eligible capture
  wins; release, crash and app stop fall back to the selected default baseline.
- **Audio.** A software mixer makes master volume a ceiling on each app's contribution: hot
  float input is clamped, one app may hold four concurrent streams, and a soft limiter holds
  the bus below full scale. It fans out playback, ducks during a conversation, bounds capture
  queues and gates microphone capture on a visible, audible indication. The PulseAudio sink is
  pinned at unity so the mixer's master is the only gain stage.
- **Input.** Five discrete panel keys with partial/full capture modes, centre-gated swipe
  gestures and a step-then-cancel full-panel crossing of two opposite volume edges (a bare
  single volume edge is a volume step) plus full-capture
  circle detection. No contact is ever deferred, so every key acts on its own edge; a
  full-panel crossing reverts its already-applied leading step instead of delaying it. Raw
  coordinates only when the backend has a coordinate panel. A recognized swipe reaches current
  media through the standard media contract over the app bus — routed
  `MediaPlayerService.SkipNext`/`SkipPrevious` for an app provider, otherwise
  `BluetoothMediaService.Command` — never through a private daemon→hardware path.
- **Built-in clock and ring.** `capsula.clock` and `capsula.ring` are compiled into the
  daemon as normal registry identities with run lifecycle, settings, routing ownership
  and app-bus endpoints. Their functionality is published as the public contracts
  `capsula.contracts.clock.v1.ClockService` and `capsula.contracts.ring.v1.RingService`.
  The clock always runs because it owns the timers, alarms and stopwatch; the ring runs
  only while it is the selected ring/display provider, and neither manifest boot flag nor a
  restart overrides that rule.
- **Bluetooth media source, no bundled app.** The Web UI "Now playing" panel lists the
  connected AVRCP source from the framework's public `BluetoothMediaService` on the same
  socket — no system app and no reserved app id — and its transport buttons call that
  service's `Command`. The physical circle key and swipes use the same service when no media
  app is playing or paused, so a phone works with no media package installed.
- **HTTPS Web UI.** An embedded Vue SPA served by the daemon, authenticated with an
  Argon2id admin password and server-side sessions; static API keys are limited to status
  and certificate endpoints. Browser traffic uses the binary protobuf `/ws` protocol.
- **Boot ownership.** On the stock Rockchip image one removable hook,
  `/etc/init.d/S99btctl.sh`, prepares state, starts and health-gates the daemon, and
  manages the vendor assistant scripts.

## What is implemented

- **Supervision:** manifest validation, unique run IDs, singleton invocations (queued,
  bounded, expiring), graceful stop, stdout/stderr capture, immediate first restart then
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
  four-octet SBC capabilities. After every connection the daemon asserts that a
  `MediaTransport1` carrying the A2DP sink UUID exists and performs one disconnect/reconnect
  repair when it does not; a source that never reports absolute volume is reported as such
  instead of accepting a write that cannot reach it. PulseAudio is the fallback owner when
  native registration fails, and its module set is retired as soon as the native sink is
  armed unless it is actively streaming a connected source.
- **Wi-Fi:** a Linux manager over saved networks, stock `wpa_supplicant.conf` import,
  bounded startup association and a WPA2 setup AP with a restricted captive portal.
- **Discovery:** Avahi mDNS host name (`<label>.local`), applied idempotently and never
  written when the label is already active.
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
- **Web UI:** five destinations — Main, Settings, Assets, Access and Diagnostics. The Assets
  page lists `<data-dir>/assets` with size and SHA-256, uploads a file to a store-relative
  path and deletes one through the session-authenticated `/api/assets` routes; the same
  directory is mounted read-only at `/assets` for apps, and uploads are limited to 32 MiB.
  The other pages provide live controls with explicit saves, app detail pages and custom app
  settings documents in an opaque-origin iframe. The overview's Bluetooth speaker card shows a
  green `connected` chip beside the speaker-mode toggle when any `pairedDevices` row has
  `connected === true`; its tooltip and `aria-label` read `Connected: <names>`, and the chip
  disappears when no device is connected. This is snapshot state, not a Bluetooth-policy cache.
- **Operations:** in-place daemon upgrade through the boot service with a health gate,
  a 512 KiB rotating log, bounded storage guard, and deployment bundles applied by
  `capsulad-cli deploy`.
- **Simulator:** complete virtual segment display, LED ring, touch/keys and audio, plus
  development injection sockets.

## Application model

An application is a package directory installed under `<data-dir>/apps/<app_id>`, or one of the
built-in system apps (`capsula.clock`, `capsula.ring`). A package ships as a
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

At launch the supervisor allocates a run ID, starts the entrypoint in the probed sandbox
with the framework environment, and waits for the app to connect, send `AppHello{run_id}`
and then `AppReady`. Identity comes from the connection plus Linux `SO_PEERCRED`, never
from a claim. Declared interfaces become available only while the run is ready. A selected
default provider (display, ring or assistant) is restarted on failure; ordinary apps follow
`restart_on_failure`. Every output, capture, audio stream and conversation must be released
on completion, dismissal, cancellation, failure or process exit.

Settings are declared in the manifest and stored per app with compare-and-swap revisions;
actions are transient declarations, not persisted state. An app may also ship one
self-contained settings document, which the Web UI renders in an opaque-origin iframe with
no host access beyond validated own-setting patches.

## Deployment and boot

A deployment bundle is a directory or `.tar.gz` with exactly these top-level entries:
`capsulad`, `capsulad-cli`, `lib/` (native runtime and headers), `apps/*.tar.gz`, and
`boot/S99btctl.sh`. `make deploy-bundle` assembles that directory from ready-made host-built
archives — build them first with `make dumbassistant`, `make marusia` or `make devapps` — and
never packages an app. `capsulad-cli deploy PATH` validates the whole bundle before
changing installed state, stages it outside the data partition, stops the daemon, installs or
updates packages and managed runtime files, replaces both binaries, optionally installs the boot
hook, restarts through the boot service and gates on `capsulad -health`. A replaced binary is kept
recoverable only until the health gate passes — renamed aside when the staged file is on the data
filesystem, otherwise gzip-compressed in staging after the target was removed, since `/data` cannot
hold two 40 MiB daemons. A failure after the stop restores what was replaced and restarts the
previous stack.
On the device the data directory is `/data/capsdemo`:

```text
/data/capsdemo/capsulad          daemon
/data/capsdemo/capsulad-cli      administration CLI
/data/capsdemo/lib/              libcapsula, capsula-native.json, interpreter hosts, headers
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
(`build/apps/org.capsula.marusia-<version>.tar.gz`). It has no packaged assets and no
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
  PID, so a PID match alone never selects `capsula.clock` or `capsula.ring`; the Web UI's own
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
- **Web UI boundary.** Sessions use a self-signed HTTPS certificate, an Argon2id password
  and CSRF/origin checks. Static API keys reach only `GET /api/status` and the public
  certificate. There is no generic AppBus, capture, audio or asset proxy for the browser.
- **Asset store.** `<data-dir>/assets` is a plain directory catalog: every regular,
  non-symlink file below the root is an asset, addressed by its slash-relative path and
  visible read-only at `/assets`. Only the session-authenticated admin API writes it.
- **Provenance** is optional per app. A verified Sigstore/GitHub attestation describes the
  binary digest, signature, certificate chain, transparency log and workflow identity; it
  is evidence, not a runtime safety guarantee.
- **One first-party app talks to the internet.** The optional `apps/marusia` assistant is
  the only first-party component that opens outbound connections. While it is installed and
  selected as the assistant, a turn sends microphone PCM and the device identity (serial from
  the non-empty `serial` setting, otherwise `DeviceService.GetInfo.hardware_serial`, otherwise
  host-only local `/proc/cpuinfo`, otherwise empty; plus the distinct `device_id` value and the
  advertised client version) together with the vendor session credential it holds — the
  masked `session_id` and `session_secret` settings, or the session recovered from the
  vendor agent's root-only state file — to the configured vendor endpoint over HTTPS, and
  logs the selected serial source once per process without logging the serial value. The
  `Invalid 'x-device-sn'` rejection remains legible, directs operators to the `serial` setting
  and the framework-reported serial, and preserves the original gRPC status; when no source
  resolves, the warning names that setting. It fetches the HTTPS TTS URL that endpoint returns. The daemon, the framework plane and every
  other first-party app stay local-only. Nothing is sent when the app is not selected or
  idle. With a session configured the app validates it without an attach; only a device
  without a session sends the provisioning attach, which carries the device serial and the
  configured `xtoken_id` (empty when unset) and whose refusal is logged. A turn with no
  recognised text and no command is reported as a no-answer with the terminal error
  indication, never as a completed turn.

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
- **Amlogic stock and Alpine backends are not implemented.** Selecting them fails with an
  explicit message. `SystemService.ControlUnit` returns `UNIMPLEMENTED`.
- **No app-facing Wi-Fi service.** Wi-Fi is owned by the authenticated Web UI and the
  isolated captive portal; apps cannot read or change network configuration.
- **Bluetooth media has no provider app.** The daemon ships no Bluetooth media application and
  reserves no app id for one: the Web UI "Now playing" panel lists the connected AVRCP source
  as a framework-owned provider read from `BluetoothMediaService` (one player `bluetooth`
  while a source is connected, none otherwise), and its PLAY/PAUSE/SKIP_NEXT/SKIP_PREVIOUS
  commands are that service's `Command` — the same path as the physical circle key and
  swipes. An operator can install third-party providers; consumers discover every running
  provider through `AppInterfaceService`.
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
  warn until then. API keys never reach the console, AppBus, captures or audio.
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
| `internal/backend/simulator.go` | complete virtual backend |
| `internal/backend/rockchip` | RK3308 stock backend (vendor LED ABI recovered) |
| `internal/sandbox` | Linux namespace/UID launcher and runtime discovery |
| `internal/artifact`, `internal/manifest`, `internal/registry` | package format, manifest schema, installation |
| `internal/assets` | shared read-only asset store |
| `internal/websecurity` | self-signed TLS, Argon2id password, API keys, sessions |
| `internal/provenance` | Sigstore/GitHub attestation verification |
| `internal/webui`, `web` | HTTPS server, embedded SPA and its protobuf WebSocket |
| `internal/sounds` | embedded framework UI cues and their CC BY-SA `COPYING` |
| `sdk/native` | Rust shared engine behind the C ABI, header and C example |
| `sdk/go`, `sdk/rust`, `sdk/cpp` | bindings and examples over the shared library |
| `interpreter` | pinned QuickJS/MicroPython hosts, script SDKs, samples and smoke test |
| `apps/devtools`, `apps/dumbassistant`, `apps/marusia` | development apps and the two optional assistants |
| `deploy/rockchip-stock` | the single boot hook and bring-up scripts |
| `proto` | framework and standard contract schemas (media, clock, ring, web) |
| `docs` | application manual, development, verification, hardware, behavior, wakeword and glossary |

## Documentation

| Document | Read it for |
|---|---|
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | prerequisites, build targets, cross builds, packaging, deployment, simulator, tests, admin commands |
| [docs/app_development/README.md](docs/app_development/README.md) | the application manual: supported targets, prerequisites, a complete native app and the page index |
| [manifest](docs/app_development/manifest.md), [lifecycle](docs/app_development/lifecycle.md), [sdk](docs/app_development/sdk.md), [app bus](docs/app_development/app-bus.md) | artifact and manifest fields, launch/identity/restart, the native SDK and script hosts, routed calls between apps |
| [services](docs/app_development/services.md), [output & input](docs/app_development/output-input.md), [audio](docs/app_development/audio.md), [settings & actions](docs/app_development/settings-actions.md) | every framework service and method, presentations/captures/keys/gestures, audio streams and indication, typed settings and actions |
| [assets](docs/app_development/assets.md), [standard contracts](docs/app_development/standard-contracts.md), [packaging & deployment](docs/app_development/packaging-deployment.md), [security](docs/app_development/security.md), [troubleshooting](docs/app_development/troubleshooting.md) | the asset store, clock/ring/media contracts, install and deployment bundles, sandbox and trust boundaries, common failures |
| [docs/VERIFICATION.md](docs/VERIFICATION.md) | proof levels, measured device evidence, host-only evidence, unavailable paths and open gates |
| [docs/SPEAKER_BEHAVIOR.md](docs/SPEAKER_BEHAVIOR.md) | user-facing interaction contract, feedback ownership and the future-change gate |
| [docs/HARDWARE.md](docs/HARDWARE.md) | Capsula Mini rev 4 hardware evidence and firmware findings |
| [docs/WAKEWORD.md](docs/WAKEWORD.md) | detection, enrollment, training and model evidence |
| [docs/GLOSSARY.md](docs/GLOSSARY.md) | project terms as this codebase uses them |
| [AGENTS.md](AGENTS.md) | repository working rules, lifecycle invariants and verification gates |

## Quick start

```sh
make                                   # release build: daemon, CLI, native SDK, bindings
mkdir -p build/demo
./build/bin/capsulad-cli -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
./build/bin/capsulad -platform simulator -data-dir build/demo -dev-admin build/demo/run/dev.sock
# open https://127.0.0.1:8443/ and sign in with the printed admin password
```

`make test` runs the root Go module, `sdk/go` and both assistant modules, the Rust binding, the
native crate and the C++ example. See
[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for the full build, packaging and deployment workflow.