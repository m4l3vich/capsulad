# capsulad — Capsula Universal Framework (initial implementation)

`capsulad` is a native Go implementation of the local Capsula framework/HAL daemon described in
`_devdocs/ARCHITECTURE.md` (draft 0.5). It supervises native application processes, owns the
hardware abstraction, routes dynamic presentations and captures, mixes audio, serves a
self-signed HTTPS Web UI protected by an admin password and static API keys, and exposes exactly one local gRPC surface over a Unix socket.

This is an **initial vertical slice**, not a frozen v1. The wire schemas are the draft v0.5 protobufs;
the implementation is honest about what is verified on real hardware and what is not.

## Documentation

- [docs/GLOSSARY.md](docs/GLOSSARY.md) — project terms (daemon, HAL, app, run, presentation,
  capture, AppBus, revision, native SDK, …) as this codebase uses them.
- [docs/APP_API.md](docs/APP_API.md) — current application API: packaging/manifest, startup and
  identity, AppBus events and routed calls, framework services and streaming shapes, presentations,
  captures, audio, input, settings, interfaces and assets.
- [docs/SPEAKER_BEHAVIOR.md](docs/SPEAKER_BEHAVIOR.md) — user-facing interaction contract and proof
  levels; [docs/STATUS.md](docs/STATUS.md) — implemented versus unverified matrix;
  [docs/HARDWARE.md](docs/HARDWARE.md) and [docs/WAKEWORD.md](docs/WAKEWORD.md) — hardware and
  wakeword evidence.
- [AGENTS.md](AGENTS.md) — repository working rules and verification gates.

## What runs today

- **One Unix-socket gRPC API** (`/run/capsulad/hal.sock`) implementing all 15 draft services:
  device, settings, routing, presentation, audio, input, assistant, application,
  application-configuration, app-interface, app-bus, sensor, system, diagnostics, asset.
- **Supervised native applications** with manifest validation, unique run IDs, graceful stop,
  immediate first restart then exponential backoff, stdout/stderr collection and singleton
  invocation semantics (queued, bounded, expiring). A process that loses its framework connection
  is a failure, not a deliberate stop. A **selected default provider** — display, ring or
  assistant — is recovery-managed regardless of its manifest: stopping, killing or disconnecting it
  relaunches it immediately. Later abnormal exits back off 1, 2, 4 and 8 s; the counter resets only
  after the run has stayed READY for 30 s, so a ready-then-crash loop keeps backing off instead of
  spinning. Stopping a selected provider is therefore a restart, not a switch-off; the Web UI labels
  it **Restart**. Deselecting it (including choosing **none**), uninstalling it or shutting the
  daemon down is the durable off switch: any selected-origin or forced relaunch re-checks selection
  before starting, regardless of `restart_on_failure`. A manifest-declared restart of an app that
  was never selected keeps its ordinary contract.
- **Reverse-RPC app bus**: one long-lived `Connect` stream per app; routed unary and streaming
  `Call` relays with deadlines, cancellation and bounded queues. Apps never listen on IPC sockets.
- **Shared native SDK**: a pure Rust `libcapsula` cdylib exposes a frozen C ABI v1 through one
  bounded `capsula_get_api` table. It owns the UDS gRPC transport, protobuf/JSON reflection for all
  framework unary, client-streaming, server-streaming and bidi calls, managed AppBus sessions and
  routed calls, and a raw-PCM audio fast path. Thin Go and Rust bindings load the library
  dynamically; C and C++ consumers use the installed `capsula/capsula.h` header directly. No Go
  runtime or per-language transport stack is linked into the library.
- **Dynamic presentations and captures** with exact first-content readiness, last-eligible-capture
  wins, baseline fallback, out-of-order release, force release and process-exit cleanup.
- **Touch partial/full modes** with mutually exclusive ownership and clean cancellation on
  ownership changes. The Mini panel is five discrete keys, not a coordinate panel: **N** microphone
  mute, **E** volume up, **S** circle/play-pause (the talk key), **W** volume down and **C** the
  unmarked neutral centre. A partial capture receives circle and centre key events plus inferred
  semantic gestures while volume and microphone remain framework controls; a full capture receives
  every key and gesture with no system volume, mute, media or assistant action. Swipe and circle
  gestures are inferred from ordered presses — no raw positions are invented. Raw coordinate
  samples are delivered only when the backend actually has a coordinate panel, so the Mini reports
  `coordinates_available=false` while `full_panel_available=true`.
- **Audio mixer/fan-out** with software mixing, conversation ducking, bounded capture queues,
  discontinuities, pre-roll for activations and the visible-indication microphone watchdog.
- **Application microphone DSP**: `audio.input.microphone` is a bounded streaming cleanup for the
  production S16LE mono 16 kHz source — 80 Hz high-pass, adaptive spectral suppression capped at
  12 dB, unity gain with no AGC, and a −1 dBFS peak limiter (256-sample frame / 128 hop, about
  15.9 ms). The noise estimate is learned from aggregate frame energy relative to a fast-down /
  slow-up floor and a very-slow-up average, so colored steady room noise is admitted; spectral
  flatness no longer gates learning. It has fixed state and output length, is chunk-boundary
  invariant, and resets on mute, source reopen or a capture gap. Wakeword inference and enrollment
  now consume this same processed tap (`framework.dsp.v1`), not the raw vendor stream; the
  unprocessed stream is available only at the explicit `audio.input.microphone.raw` endpoint.
  On the one authorized 10 s background recording — mostly music played through a Mac's own or
  external speakers, not the Capsula Bluetooth path — cleanup measured about 2.9 dB overall and
  2.7 dB in 80–300 Hz. That recording is unannotated and has no local playback reference to cancel
  the music, so it does not validate fan/traffic noise, music immunity, the new location, or a zero
  false-activation rate. The estimator also has a cold-start guard: a voice or word at 0 dB relative
  to the learned reference is not absorbed, while ordinary speech re-engages the floor.
- **daisyUI Web console with a realtime binary WebSocket API**: the browser is an independently
  built Vue 3 SPA (`web/`) with Tailwind CSS 3.4.17 and daisyUI 4.12.23, a local light-neutral
  muted-teal `capsula` theme and no CDN or external fonts, served by the daemon and embedded at
  build time. A top navigation bar exposes four destinations — Main, Settings, Access and
  Diagnostics — plus Sign out; application detail and its settings open over Main in a dialog.
  One session-only `/ws` carries `capsula.web.v1` protobuf frames with revisioned topic snapshots,
  bounded events and an explicit resync signal, plus a typed command allowlist. Main shows
  Bluetooth state, incoming connections and discoverability, now-playing metadata, volume,
  microphone state and installed applications including built-in system apps. Settings covers the
  voice assistant (default app or none from installed apps that declare `voice_assistant`,
  hold-to-summon or hold-to-talk touch behaviour, embedded guided wakeword training and the real
  last 20 engine detections), display/ring defaults from apps that declare
  `segment_display_provider`/`led_ring_controller` and brightness, microphone mute and
  muted-indication hiding, Bluetooth policy and paired devices, Wi-Fi, a timezone view that opens
  with the current setting, a phone/computer suggestion and UTC (matching cities appear only after
  a search; raw identifiers and firmware defaults stay under Technical details), network name,
  startup sound and master volume. A saved provider that no longer declares the matching role
  remains selected and is never rewritten automatically. Access covers admin password, static API
  keys and the public certificate; Diagnostics covers hardware, firmware, software, runs, resources,
  routing, audio, logs and read-only Linux host metrics with an explicit unsupported-platform state
  elsewhere. Per-application pages keep lifecycle, declared settings, actions, runs, provenance,
  interfaces, logs and clock/ring controls. There is no generic AppBus/Capture/Audio/Asset proxy:
  the browser cannot request PCM, download enrollment takes, read another app's logs or call an
  arbitrary service.
- **Custom application settings UIs**: an app may declare one self-contained
  `[settings_ui] html = "…"` document (≤512 KiB, relative, regular, valid UTF-8). The Web UI loads
  the session-authenticated document URL directly as an iframe `src` and renders it in an
  **opaque-origin iframe with `sandbox="allow-scripts"` only**, under a restrictive CSP that
  forbids network, forms, frames, navigation helpers and external resources. The host page allows
  only same-origin frame URLs (`frame-src 'self'`); normal host routes keep
  `X-Frame-Options: DENY`, and only the settings-document route omits it. Since browsers do not
  support CSP `navigate-to`, self-navigation is not claimed to be CSP-blocked: the bridge owner
  detects any iframe load after the initial ready, treats it as a violation, disposes the frame,
  points it at `about:blank` and never re-sends init/token/schema/values. A 10 s ready timeout
  reports a visible error and disposes the frame; retry creates a fresh frame and token. The daemon
  injects the local system UI kit and a bridge (`capsula.settings-ui/1`); the parent accepts only
  messages from that exact frame with its per-frame random channel token and size/schema limits,
  and forwards only declared setting-value patches for the hosting app through the normal
  revision-checked app-settings path. There is no generic command, AppBus, capture, audio or
  cross-app operation, and no host cookie/WebSocket/DOM access for the frame. Built-in Clock and
  Ring use this same route.
- **Live Web UI controls with explicit saves**: selects, sliders, checkboxes and the microphone gate
  apply immediately after local validation as field-scoped updates (display default app, brightness
  and brightness mode; assistant app and touch behaviour; microphone mute; muted-indicator
  mode/delay; volume; startup sound; timezone; hostname) through one serialized compare-and-swap
  queue with one in-flight request per group and coalescing of rapid slider input. A visible
  pending state reports in-flight work, a failure shows the error and re-reads the snapshot instead
  of keeping an optimistic value, and external/hardware changes update the controls live without
  polling. Drafts (network name, timezone selection, Wi-Fi passphrase) stay local until their
  explicit Save/Apply; a live control in another group never submits or validates a dirty draft,
  and an incoming push never overwrites one. The muted-indication hide delay offers
  “Never (keep the light visible)” (the stored `none` mode: keep the indication until unmuted, the
  device default), 1 minute, 10 minutes and 30 minutes; a stored non-preset mode or delay is shown
  as a labelled current value and is never rewritten merely by loading the page or changing another
  control. Timezone is a searchable city/region chooser with a
  current UTC-offset/local-time preview; the browser zone is only a suggestion and is applied only
  after an explicit Apply. Diagnostics host metrics are Linux-only: an unsupported host shows an
  explicit state and reason rather than zero measurements, and each metric domain is independently
  marked available. Partial log/metric frames never suppress an older-reserved full snapshot: full
  fields and host metrics keep independent watermarks, logs are unioned, deduplicated, sorted and
  capped at 400 entries (200 while paused), and a within-connection resync never discards newer
  streamed entries; reconnecting starts a new monotonic epoch, clears the log and paused buffers and
  preserves the pause choice. Wakeword history lists the real last 20 engine activations, not
  manual-button, simulator or API events.
- **Wi-Fi with AP fallback provisioning**: a Linux Wi-Fi manager joins saved networks in the
  background in priority order with a bounded 25 s startup attempt (7 s per network); if none
  connect it brings up a WPA2 provisioning AP whose SSID is the live Bluetooth adapter alias (saved
  host name only when Bluetooth has no name) and whose password is the documented `capsulad`. On a
  first Linux run the manager imports existing stock `/etc/wpa_supplicant.conf` networks once into
  its private store (derived PSKs, `.stock-imported` marker, active network preferred; a private
  network wins a duplicate SSID). If the stock file is unreadable and the private store is empty,
  it leaves the stock association untouched and reports an error instead of killing it. The
  AP uses channel 1, `192.168.4.1/24`, DHCP `192.168.4.10–100`, DNS hijack and a portal on port 80;
  stock `wpa_supplicant` AP mode and a separate `dnsmasq` are used (no hostapd), with a best-effort
  firewall chain limiting the setup network to DHCP/DNS/portal. The portal has only Wi-Fi
  status/saved list, explicit Scan and Connect (no administrator login, no system/app mutation); it
  is restricted to the AP subnet, checks Host/Origin and an HMAC token bound to the client address
  and valid for 15 minutes (reusable within that window, **not** single-use), and uses
  a no-script CSP. The authenticated Web UI shows the active network, known networks with Forget
  (portal forget is not offered), and nearby networks only after an explicit Scan; subscribing or
  opening Settings never scans. `scan_supported` is capability-driven, not hardcoded: station mode
  reports it when `wpa_cli` is available, and AP mode disables Scan when the supplicant refuses and
  no `iw` fallback exists, while manual SSID entry still works. Supplicant control-socket
  CONNECTED/DISCONNECTED/roam events reconcile live state into the snapshot and Web UI without
  polling. A successful association tears the AP down; a failed station connect restores the
  previous/saved association, or raises the setup AP boundedly and reports the original error.
  Passphrases derive a PSK and are never returned or written in plaintext. This is
  source/HTTP-fixture and simulator coverage; physical RF association, stock AP-mode scanning,
  DHCP/firewall behavior, captive detection and the target's Wi-Fi drivers are not verified here.
- **Local-only privilege boundary.** The gRPC API is a Unix socket only; there is no network gRPC
  listener. The Web UI is a local administrator client of that socket, but its authenticated
  browser/API-key surface is not a second privilege plane: API keys stay confined to read-only
  status and certificate endpoints and cannot reach recording, diagnostics or credential mutation.
  Supervised apps are identified by the supervisor run ID plus Linux `SO_PEERCRED`; an app that
  omits its run ID is still rejected by its sandbox UID or supervised PID, and a built-in run ID
  additionally requires the daemon's per-boot token. Device settings, diagnostics/log history,
  forced capture release, microphone mute/volume, wakeword takes/training, Bluetooth
  incoming-connection/discoverable policy and unpair, application launch/stop and foreign-app
  configuration are
  administrator operations; per-app settings, actions and run listings are scoped to the calling
  app. Microphone capture requires both a selected visible non-blank ring/segment presentation at
  non-zero brightness and a microphone-live cue the mixer consumed and drained; the cue is the
  exclusive mixer source while it runs, and frames are withheld whenever either half is absent.
  Pre-roll follows only a framework-issued, one-shot activation for that app. Shared assets are
  listed only from regular non-symlink files inside the asset root, and the Bluetooth media view
  redacts the connected peer address/name for app callers.
- **Built-in clock and ring system apps** (`capsula.clock`, `capsula.ring`): compiled into
  `capsulad` but otherwise normal application identities. They appear in `ApplicationService`,
  the Web UI and default-provider selections, have a run lifecycle (`Launch`/`Stop`/`WatchRuns`),
  app settings through `ApplicationConfigurationService`, actions and hardware gestures. Only their
  packaging is special: no package directory, no child process, and no install/uninstall.
  They publish **public standard contracts** discoverable through `AppInterfaceService` and
  callable by any supervised app through `AppBusService.Call`: `capsula.contracts.clock.v1.ClockService`
  (state/watch, stopwatch start/pause/resume/reset, timers start/cancel/list, alarms
  list/add/delete/enable and alert dismissal) and `capsula.contracts.ring.v1.RingService` (state/watch,
  settings get/update/watch with compare-and-swap for every declared setting). The daemon's own Web UI
  uses the same path; there is no private token-only service. Clock alarms are stored in the app setting
  `alarms` as five-field dcron entries separated by a comma after the fifth field (and any suffixes),
  so comma lists inside fields such as `0,30 7 * * 1-5` remain one schedule; an entry may carry
  `@IANA-zone`, `!once` and `!disabled` suffixes, and the stable alarm ID is `expr [@zone] [!once]`.
  Migration preserves time, recurrence, explicit timezone, one-shot behavior and enabled/disabled
  state (`!once` persists `!disabled` after firing; alerts ring until dismissed). The settings UI
  edits friendly times/repeat days and serializes them. Ring `palette` is a canonical gradient
  (`linear-gradient(<angle>deg, #RRGGBB [pct%], …)`) or a solid `#RRGGBB`, and the `static` effect
  shows one solid colour without animation; `rainbow` rotates/interpolates the palette's actual
  samples at 0, 1/3 and 2/3 and invents no unrelated hues, so a mono palette produces a grayscale
  rotation. Named legacy palettes migrate to equivalent gradients. Clock settings also cover colon
  blink, timezone, display mode, timer duration and the stopwatch reset gesture; ring settings also
  cover brightness, speed and an optional wall-clock active window (for a nightlight, or to keep the
  ring dark at night). Legacy
  `org.capsula.clock` / `org.example.ring-controller` state migrates to these IDs. Rendering always
  goes through presentations/captures, so an external capture preempts a system app and its
  baseline resumes on release.
- **Media-player contract without a bundled provider**: the standard
  `capsula.contracts.media.v1.MediaPlayerService` app-bus plumbing remains available to external
  apps — unary commands plus a server-streaming `WatchPlayers` whose first event is a snapshot — but
  this repository no longer ships a sample media app. The Web UI and `capsula media list` show
  whatever providers an operator installs; no app listens on its own socket. The physical circle
  key and left/right swipes still control media with no app installed: a standard provider that is
  playing, then one that is paused, wins; with neither, a connected Bluetooth source is driven
  through the public `BluetoothMediaService`; only then does another standard provider (for
  example, stopped) receive the command. Bluetooth control uses only public BlueZ interfaces and
  the advertised connected device: `MediaPlayer1` when the source exports it, otherwise the
  deprecated-but-public `MediaControl1` on the same device. `MediaControl1` publishes no playback
  status, so toggle direction is resolved from `MediaPlayer1.Status` when present, then that
  device's A2DP transport `active` state, then the framework sink's own monotonic packet-receipt
  evidence for the current stream (500 ms freshness, per connection, cleared on stop/error/new
  source). Commands never write this evidence, and no state is invented when the evidence is
  absent — an unknown direction sends Play, never a false "pause" claim. A private-bus BlueZ
  fixture on Bullseye verified the actual `MediaControl1` object/method call on the wire; that path
  is in the daemon deployed on 2026-11-13, though it has not been re-exercised with a real peer
  since deployment.
- **Optional dumb assistant** (`org.capsula.dumbassistant`): an ordinary first-party package, not a
  system app and not part of the default build. It lives in its own nested Go module and is built
  only by the explicit `dumbassistant` target. It claims `voice_assistant`, consumes the framework's
  processed application microphone tap, paints the whole ring teal with brightness tracking input
  level, and ends the turn when the 300–3400 Hz band level has fallen clearly below the recent
  voiced level for 700 ms (a sustained voiced vowel keeps its level and is preserved), or on a
  hold-to-talk release; it then fades to a blue chase for the configured reply delay. A reply needs
  either 140 ms of modulated speech-band time or 400 ms of steady voiced level well above the
  learned floor; a cold-start guard keeps first speech out of the floor. This is a level-based
  endpointer, not a speech classifier: a new steady loud noise at or above voice level that starts
  mid-turn is bounded by the 10 s cap and cancelled; a steady hiss/tone held above 2.5× the floor
  for 400 ms can be answered; and a sustained quiet vowel below that multiple is not confirmed.
  The canonical raw check on external music plus a full-level word (one DSP pass, no normalization,
  calibrated at the actual input level) recovered 1 reliable, 1 marginal and 4 buried words — an
  accepted music/quiet-speech limitation, not an accuracy benchmark. Loud music or lyrics can open
  or satisfy a turn, quiet speech can be missed, and the app does not cancel music, perform AEC or
  identify speakers. The current activation mode is preserved; no hold-to-talk change is made.
  The circle key dismisses an in-flight
  turn through the app's declared `dismiss` action, which aborts listening, delay and playback and
  clears queued activations. `delay` accepts empty (0 ms), a constant, or an ascending inclusive
  range, 0–60000 ms; `reply_asset` is a path relative to the shared read-only app asset store (apps receive
  `CAPSULA_ASSETS=/assets` in namespace mode and the host store path otherwise; empty uses the
  packaged fallback). The daemon rejects absolute/traversal/symlink targets and files not registered
  in the shared asset manifest, synchronously and again in the app. It performs no speech recognition, and it is neither installed, selected nor started
  unless an operator chooses it.
- **Framework-owned boot service**: one self-contained hook,
  `deploy/rockchip-stock/S99btctl.sh`, starts the daemon after mount, microphone-gate and
  PulseAudio init; it pins the Bluetooth UART IRQ, preserves/retires and restores the vendor
  assistant init scripts, gates on `capsulad -health`, and starts the preserved vendor chain if
  the daemon cannot become healthy. The hook is removable: `S99btctl.sh disable` stops the daemon,
  restores the vendor scripts and moves the hook aside; `enable` reinstates it. The name is
  inherited from the vendor boot list because `rcS` expands its script list before the `/data`
  overlay pivot.
- **In-place daemon upgrades**: `capsula daemon install -binary PATH` proves the replacement runs,
  stages it beside the live binary, swaps by rename, restarts through the boot service and checks
  the framework API; any failure restores the previous binary. `capsula daemon rollback` restores
  the preserved one.
- **Bounded operational footprint**: the daemon log rotates at 512 KiB and keeps one backup, the
  negative-training corpus path is configurable and is not a runtime dependency, and new wakeword
  recordings or training are refused below 2 MiB of free `/data`.
- **Bluetooth through BlueZ**: the daemon owns adapter power, discoverability and pairing over
  the system D-Bus and the two policy controls, lists paired devices and unpairs them from the Web UI
  (an administrator-only operation, refused for supervised apps). **Incoming connections** (persisted
  as `bluetooth_incoming_connections`, default on) controls whether new inbound BR/EDR links are
  accepted, including from paired devices; turning it off denies inbound connections through the
  public Linux Bluetooth management socket `Set Connectable` and also clears the kernel action list
  (`Remove Device`), so BlueZ's known-device allow-list cannot bypass the deny. Connectability is
  not a D-Bus property (BlueZ 5.65 has no `Adapter1.Connectable`), while the adapter
  and BLE radio stay
  powered for applications, and links that are already connected are not torn down. **Discoverable**
  (`bluetooth_discoverable`, default off) is a D-Bus adapter property and lets new devices find,
  connect and pair; enabling it implies incoming connections, and turning incoming off clears it. A headless BlueZ pairing agent
  (NoInputNoOutput) lets a new source bond without the retired vendor `btctl`; new pairing
  authorization requires both incoming connections and discoverability, while an already-paired source may authorize services
  on an existing link. The obsolete `bluetooth_pairable` switch is migrated (true → incoming and
  discoverable both on; false or absent → incoming on, discoverable off). It registers a native
  A2DP Sink endpoint through the public BlueZ `Media1` APIs and verifies that the registration is
  still armed for the current adapter generation **before** the adapter is made connectable or
  discoverable, and before an API request can enable either. A
  failed or lost registration is never reported ready: both enabling actions are refused with
  the real reason, and `BluetoothStatus.Detail` says the audio profile is not advertised. If native
  registration fails, PulseAudio's Bluetooth discovery is the fallback owner. PulseAudio endpoints
  are removed only after the native sink is armed and never while an active source is routed
  through them; the switch completes when that source disconnects. A BlueZ restart or adapter power
  cycle invalidates the registration, which is repaired before the next connect. If BlueZ
  autopowers the adapter before `capsulad` starts, a paired source can still open the first ACL in
  that pre-daemon window; the incoming-connect handler requests the audio profiles over that same
  link, but recovery of a peer that already abandoned A2DP is device-dependent and remains a
  hardware gate. The two policy toggles, their persistence and observed status are implemented and
  covered by D-Bus-fixture tests; `Set Connectable`/pairing behavior on the target device is not
  physically verified here.
- **Bluetooth media mode**: AVRCP play/pause/next/previous and absolute volume go to the connected
  source, and its track metadata is read when the source exposes it. Volume is event-driven in both
  directions through the same `applyMasterVolume` path as the keys and application RPC; the daemon
  does not echo a transport value it just wrote. A provider app publishes this through the standard
  `MediaPlayerService`, so the Web UI and other apps see a normal `bluetooth` player; the physical
  controls reach AVRCP through the public `BluetoothMediaService` even when no provider app is
  installed. Connect plays exactly three completed blue fade-in/out ring pulses and shows `bt` on
  the two rightmost segment cells for two seconds; disconnect plays one pulse and no glyph, leaving
  the selected display baseline untouched. Both notices go through ordinary presentations and
  captures over the selected baseline.
- **Physical controls**: the Mini panel is five discrete keys — microphone mute at north, volume up
  at east, the circle/play-pause talk key at south, volume down at west, and the unmarked neutral
  centre. Hold the circle past 700 ms to summon the assistant; in hold-to-talk mode its release
  ends the turn. A short circle tap toggles the current media: a playing standard provider, then a
  paused one, then a connected Bluetooth source through the public `BluetoothMediaService`, then
  another standard provider. Pressing the circle while an alert or an assistant turn is in flight
  dismisses it through the framework/app `dismiss` action instead of starting a second action. The
  centre is neutral: it dismisses alerts and assistant turns and drives stopwatch gestures, but it
  never summons the assistant. While a stopwatch runs, a circle or centre tap pauses or resumes it
  and the configured hold or double tap resets it, never summoning the assistant.
  Swipes and circles are inferred from ordered presses, never from invented coordinates. The
  user's panel map is authoritative — east is volume up and west is volume down — and direction is
  not inverted from any single passive trace. A completed swipe may be the full
  opposite → centre → direction form (W/C/E right, E/C/W left, S/C/N up, N/C/S down) or the
  minimum two-contact opposite-edge pair without the centre (W then E = right, E then W = left),
  where the first contact must still be down when the second is pressed — a physical overlap.
  Sequential opposite taps and a circle-then-microphone pair remain deliberate key actions. The
  vertical two-contact pair is recognized only in full capture; partial and none modes keep
  circle and microphone as deliberate keys. An unambiguous centre-first half swipe is also
  accepted, and a closed perimeter traversal from any corner is `circle_cw`/`circle_ccw`
  (N→E→S→W→N or reverse). A crossing that reports zero or one contact is indistinguishable from a
  key press or from no input and is not treated as a gesture.
  When a multi-contact sequence fails, its prefix contacts are suppressed — no volume step, mute or
  summon leaks — and only the newest contact may still resolve as a single action, while a
  standalone volume tap or held ramp keeps its normal behavior. An unfinished multi-key gesture
  produces no framework effect; a partial capture may still receive its app-owned circle/centre key
  contacts. A partial capture receives circle/centre key events and completed gestures, while
  volume and microphone stay framework controls; a full capture receives every key and gesture
  with all system effects suppressed, including mute, volume, media and assistant activation. Gesture
  contacts are deferred by up to 250 ms (total gesture budget 1.5 s) so a swipe/circle cannot leak
  its constituent mute, volume or summon actions. This deliberately adds up to 250 ms latency to a
  single-key action; a hold and the gesture windows use the kernel event timestamp converted to the
  daemon clock (a receive-time fallback applies only to an invalid, >5 s stale event), and a
  timed-out prefix
  produces exactly one action with no duplicate tap. Volume keys step 5 %, ramp while held
  (200 ms decaying to 60 ms) and stop at the bounds; each change plays the embedded volume cue at
  the new level and shows the percentage on the segment display for a second. Key, application and
  Bluetooth volume all write the same persisted setting and apply the same mixer/transport path; the
  microphone key uses the same persistent mute path as `AudioService.SetMicrophoneMuted` and plays
  a distinct mute (warning) or unmute (info) cue.
- **Embedded UI cues**: the startup jingle, volume cue and microphone cues are the CC BY-SA
  `ubuntu-sounds` set compiled into the daemon from `internal/sounds` (with its `COPYING` preserved),
  not operator/app assets. The startup jingle plays once on the first boot after installation and
  afterwards only when `startup_sound_enabled` is on (off by default); it streams asynchronously and
  never delays boot. Every cue uses the framework playback path, so it follows the master volume.
- **Personal wakeword training from the browser**: record on the device, then run waveform
  augmentation, pretrained speech embeddings, hard-negative training and independent quality gates
  on the device. The browser only displays progress; no Python, ONNX runtime or GPU is needed there.
  Inference and enrollment consume the shared processed capture tap, and each take carries a
  processing-provenance sidecar; legacy raw takes are converted in memory under that declared
  policy rather than silently relabeled. The installed raw-trained model stays installed and
  selected on processed input and logs an explicit unverified-threshold warning; weights and
  selection are not rewritten. On the settled current profile, a warm classifier replay of the
  unchanged legacy model — 2.32 s of its own same-session ambient audio, the word, and a 1 s tail —
  fires 6/6 raw and 6/6 processed. That is legacy-model usability only, not calibration or an
  accuracy certificate. External `CNEG` embeddings are auxiliary off-policy examples: they cannot
  certify processed-audio quality, but a false activation on that broad corpus remains a hard
  acceptance veto, and only local processed held-out takes decide threshold calibration and the
  pass/fail certificate.
- **Muted-microphone indication** on the ring: a red run on the bottom three pixels that fades in
  and out, independent of the mute state itself. The microphone key is a press-edge toggle; volume
  keys step once per press.
- **Device timezone** set in the Web UI, inherited by apps as `TZ`; the built-in clock system app
  follows it, and changing it restarts boot applications to apply the new zone.
- **mDNS network name**: a single DNS label (default `capsula`), stored as a typed setting and
  advertised as `<label>.local`. On the stock Linux image Avahi remains the sole mDNS owner;
  `capsulad` reads `GetState`/`GetHostName` on the system bus and calls
  `org.freedesktop.Avahi.Server.SetHostName` only when the saved label is not already active,
  confirming an active label through `GetHostNameFqdn` with no redundant write. A zero-write
  success requires the responder to be RUNNING. After a write the daemon waits a bounded interval
  and confirms the name, and a coalesced worker replays the latest RUNNING edge instead of dropping
  it behind a blocking apply; Close cancels without publishing a health warning. A successful write
  whose FQDN cannot be read back is reported as applied-but-unconfirmed, never as a fabricated
  advertisement. A redundant-write reply, or a concurrent writer reaching the name first, is
  accepted only after the responder reports the requested label; write-time collisions, invalid
  labels and responder failures remain visible with the observed name instead of claiming an old
  registration is live. The latest saved
  label is reconciled at startup and when the responder restarts. `GET /settings` is read-only: it
  renders the stored status and never applies a name or saves configuration. The simulator and
  other unsupported platforms report mDNS as unavailable and never rename the host; the saved label
  is retained. No second responder or 5353 socket is opened. A browser proof drove the production
  Web UI/auth/TLS/settings path against a successful responder fixture (not the simulator's
  unsupported status): startup, two GETs and a same-name save issued **zero** `SetHostName` calls
  with a clean “Advertised as capsula.local” and no warning; changing the label issued exactly one
  setter call; a colliding label stayed visible with no advertised claim and no writes from the GET;
  and saving the original label recovered. That is fixture-backed browser evidence, not real-device
  `.local` resolution.
- **Typed settings** with compare-and-swap revisions, partial updates, masked storage and delivery
  to the owning app only.
- **HTTPS Web UI** with a self-signed server certificate, Argon2id admin password with
  server-side sessions, static opaque API keys for automation, CSRF/origin defenses, local
  credential management and read-only security diagnostics. The Vue SPA (built from `web/` and
  embedded by `go:embed`) drives the daemon exclusively through the typed `/ws` protocol; the
  socket requires a session cookie and a matching HTTPS origin, rejects non-binary or oversized
  frames, and closes on logout, revocation or session expiry. Overview, Settings, Application,
  Wakeword, Diagnostics and Access subscriptions carry authoritative revisioned views; wakeword
  catalog and API-key changes are pushed to every authenticated session, and a topic-less resync
  makes every subscription re-request its snapshot after an overflow. Credential changes made by
  the local `capsula auth` CLI in another process are watched and applied live; a password change
  revokes sessions and closes sockets. Static API keys remain
  limited to `GET /api/status` and the public certificate download, never the console or socket.
  This intentionally replaces the draft's mandatory client-certificate mTLS at the project
  owner's request. API keys protect only the HTTP surface; the HAL/app bus remains Unix-socket
  only.
- **Optional GitHub artifact-attestation verification** for native binaries with an offline
  Sigstore trusted root (digest, DSSE signature, certificate chain, transparency log, workflow
  identity). Verified provenance is never presented as a safety guarantee.
- **Simulator backend** with virtual segment display, LED ring, contacts and audio, plus local
  development injection hooks.
- **Rockchip WS2812B ring and IS31FL3236 segment output** using the ioctl ABI and wiring
  permutations recovered from the stock `voice-agent`, with sysfs and in-memory fallbacks.
- **Rockchip device-tree-driven input and sensors**: all five gpio-keys (microphone mute, volume up,
  play/pause, volume down, neutral centre) plus ambient-light and SoC/logic thermal sensors.

## What is intentionally not claimed

- **Privacy indication is a software boundary, not a measurement.** The audible microphone cue
  proves the framework mixer consumed a cue at the selected master gain and wrote it to the
  configured output sink; it does not measure perceived loudness, and audio already buffered in the
  hardware/PulseAudio sink can still overlap it. Visible brightness is a backend statement, not
  measured light. Zero brightness or zero volume denies capture rather than recording quietly.
  Root/physical access and `-sandbox none` development runs are outside the cooperative model.

- **The 2026-11-13 follow-up fixes are deployed; their physical paths are not certified.**
  Panel/media control, wakeword preprocessing, the assistant endpointer and mDNS idempotence are in
  the installed daemon (sha256 `ff3646bc…`) and assistant (`141da876…`) as of 2026-11-13 16:19 MSK.
  The health gate, ready clock/ring/assistant runs, preserved user settings, model, activation mode
  and reply asset, and a normal restart are verified on the unit. No microphone capture was made,
  so wakeword/noise behavior and the endpointer under the current room are not acoustically
  verified; physical gesture/media-recency behavior, real-peer Bluetooth, Avahi collisions and a
  power-cycle remain hardware gates. See the dated entry in [STATUS.md](docs/STATUS.md).
- **The 2026-11-14 public-contract/settings-UI/Wi-Fi stack plus the clock/ring wire correction are
  deployed; their physical paths are not certified.** Corrected daemon `fce5c506…`, runtime
  `394ed875…` (descriptor now includes the public clock/ring services), assistant `ee27cc0a…`,
  and the rebuilt embedded SPA are installed through the canonical boot service, with full
  host/on-device `.prev` backups and versioned backup directories (see the dated entries in
  [STATUS.md](docs/STATUS.md)). Health, ready clock/ring/assistant runs, public interface
  discovery, preserved settings/credentials/network/pairings, the new SPA bytes and 401-gated API
  were verified on the unit. An on-device native-SDK proof through the installed runtime now
  succeeds: clock `GetState`, a non-empty `CancelTimer`, ring `GetSettings` plus a same-value CAS
  update, and a snapshot-first `WatchState`/cancel all convert through
  `call_send_json`/`call_recv_json`, and the authenticated Web UI WebSocket serves the typed
  clock/ring application views. Physical Bluetooth `Set Connectable`/pairing, real-peer A2DP,
  Wi-Fi RF association, captive-portal detection, stock Wi-Fi drivers, targeted AP teardown, real
  hardware input, acoustics and a power cycle are not verified. A captive-portal browser proof or
  a Linux command/D-Bus fixture is not proof of RF or stock-driver behavior.
- **Script runtimes are implemented and now running on the unit.** `make
  interpreter` builds real quickjs-ng v0.18.0 and MicroPython 1.29 hosts from pinned sources and
  stages `capsula-quickjs`/`capsula-micropython` beside `libcapsula`. The hosts load the public C
  ABI, open the AppBus, and expose unary/proto calls, events, routed calls and stream cancellation.
  `ListRuntimes` reports the installed engines, and non-native packages are admitted only when the
  matching host and ABI are present. `interpreter/smoke/run.sh` passes READY, invocation, unary,
  routed unary, routed stream/cancel, stop/crash cleanup and a daemon-crash phase where both hosts
  receive the bounded terminal disconnect event and exit on their own; `capsula_host_test.c` pins
  the single-job queue behavior. Both hosts are installed in `/data/capsdemo/lib` on the Capsula
  Mini: `ListRuntimes` reports `quickjs-ng 0.18.0` and `micropython 1.29.0-preview` from the
  device, and a temporary QuickJS package ran `APP_READY` under `mountns`, read the public device
  API, accepted an invocation and stopped without an orphan host. The same host+library pair has
  not yet been exercised under MicroPython on the device, and script sandbox strength is still the
  documented mount-only kernel limit.
- **Rockchip vendor LED ABI is recovered, measured and partly validated.** The device tree
  describes a 36-channel WS2812B chain (`/dev/topleds`, 12 RGB ring pixels) and an IS31FL3236
  (`/dev/led_multi_ctrl`, 30 monochrome display cells); `GET_COUNT` returns exactly 36 and 30 on a
  live unit, the segment channel map came from `cmd/ledmap`, the clock face renders correctly, and
  the ring shows pixel 0 at 12 o'clock, travels clockwise and renders RGB correctly. Gamma is
  unmeasured (no photometer). Standard Linux `sysfs` LED class devices and evdev remain fallbacks.
- **Amlogic stock/Alpine backends are not implemented.** Selecting those platforms fails explicitly
  instead of pretending to be hardware.
- **The Web UI certificate is self-signed.** Browsers show a trust warning until the operator
  verifies and trusts the fingerprint. Password recovery is a local-console CLI operation.
- **Personal wakeword models require setup and evaluation.** Native incremental speech-embedding
  inference runs on Rockchip, but legacy mel models must be retrained. The installed model was
  trained on raw audio and now runs on the processed tap, so its calibrated threshold is unverified;
  it stays installed and selected with an explicit warning. There is no bundled,
  speaker-independent Russian keyword head. See [wakeword setup and evidence](docs/WAKEWORD.md).
- **Sandbox strength is probed, never assumed.** Linux user+mount namespace isolation, read-only
  mounts and a seccomp deny list are implemented, but the RK3308 stock kernel (4.4.167) offers only
  mount namespaces: `CLONE_NEWUSER`, `CLONE_NEWNET` and `PR_SET_SECCOMP` all fail on a live unit.
  The daemon probes both namespaces and the filter at startup, runs apps in a root mount namespace
  with a distinct UID, and reports `mode=mountns` rather than claiming isolation it does not have.
- Persistent app storage beyond typed settings, per-app RBAC, remote gRPC and large Capsula/Neo
  drivers remain out of scope, as the architecture specifies.

## Build and run (simulator)

For code or UX changes, start with [AGENTS.md](AGENTS.md) and the
[speaker behavior change gate](docs/SPEAKER_BEHAVIOR.md#future-change-gate).

```sh
make                 # release profile: daemon, CLI, Go/Rust SDK bindings, native library and header
make debug           # same targets, unstripped
mkdir -p build/demo
./build/bin/capsula -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
./build/bin/capsulad -platform simulator -data-dir build/demo -dev-admin build/demo/run/dev.sock
```

`make` is the release profile: Go programs are built with `-trimpath -s -w`, the native library
and Rust binding use Cargo's release profile, and the Go binding is built into its nested module.
`make debug` builds the same targets without stripping. Programs and the native runtime are written
to `build/bin`, `build/lib` and `build/include`; the Rust binding uses its own Cargo target
directory. Builds are local artifacts and nothing here deploys them to a device.

`make interpreter` is opt-in because it builds third-party engines from pinned sources
(quickjs-ng v0.18.0 and MicroPython 1c6321181…, both sha256-pinned in
`interpreter/build.sh`). It stages `capsula-quickjs` and `capsula-micropython` into `build/lib`
beside `libcapsula`; `make interpreter-quickjs` / `make interpreter-micropython` build one engine.
Offline builds may set `QUICKJS_SRC` / `MICROPYTHON_SRC` to already extracted matching trees.
The runnable samples are `interpreter/samples/`; the end-to-end proof is
`sh interpreter/smoke/run.sh`.

The Web UI is a separate Vue 3 + Vite project in `web/` with Tailwind CSS 3.4.17 and daisyUI
4.12.23. Its built output is checked in under `internal/webui/dist` and embedded by `capsulad`, so a
normal `make`/`go build` needs no Node toolchain. After changing frontend sources, rebuild it and
then the daemon:

```sh
make web          # npm ci && npm run build into internal/webui/dist
make capsulad     # re-embed the new assets
```

For frontend work, run the SPA against a live simulator daemon instead of re-embedding assets on
every edit. The daemon keeps its real HTTPS/session surface; Vite only serves the sources:

```sh
mkdir -p build/dev-web
./build/bin/capsula -data-dir build/dev-web auth init -hostname localhost -ip 127.0.0.1 \
  -password devpass123
./build/bin/capsulad -platform simulator -data-dir build/dev-web \
  -dev-admin build/dev-web/run/dev.sock -web-addr 127.0.0.1:8443
cd web && npm run dev            # https://localhost:5173, HMR
```

`vite` proxies `/api`, `/healthz` and the binary `/ws` socket to the daemon
(`CAPSULA_WEB_TARGET`, default `https://127.0.0.1:8443`) with `changeOrigin` off, so the daemon's
Origin/Host check still matches. The dev server reuses the daemon's certificate
(`build/dev-web/security`, or `CAPSULA_DEV_SECURITY_DIR`) and serves HTTPS, because the session and
CSRF cookies are `Secure; SameSite=Strict` and Safari drops them on plain `http://localhost`;
accept the self-signed certificate once, or set `CAPSULA_DEV_HTTPS=0` for browsers that accept
Secure cookies from localhost. `capsula -data-dir build/dev-web dev <state|wakeword|gesture|key|raw>`
and the `-dev-admin` Unix socket drive input, wakeword and rendered frames
(`curl --unix-socket build/dev-web/run/dev.sock http://localhost/state`).

The Overview media bar needs a provider. `sh tools/dev-media.sh build/dev-web` builds the real
media-contract fixture (`internal/daemon/testdata/mediafixture`) for the host, installs it as the app
`dev.media` and leaves it stopped; restart the daemon and Play starts it through the same routed
contract the panel keys use, so transport and now-playing render exactly as they would for an
installed provider. The fixture is a development package, not a product app.

The checked-in dist is generated from the current `web/src` with the pinned Vue 3.5.13 / Vite 4.5.5 /
PostCSS 8.4.49 / autoprefixer 10.4.20 toolchain (the offline flow symlinks the staged package tree
into `web/node_modules` and runs `vite build`; do not hand-edit minified output). The fresh daisyUI
console replaced the previous hand-styled UI and loads only local assets, with no CDN or external
font. Observed local verification (not a device deployment): the frontend's 11/11 node tests passed;
the final Vite production build, the embedded host daemon build and the Linux arm64 crossbuild
passed (after all patches); `go test -race ./internal/webui` passed (22.672 s); the wakeword
real-origin last-20 history focused `-race` test passed; the daemon
install/cancellation/install-and-train focused `-race` tests passed (2.045 s with a matching host
native runtime); and the targeted `TestTraining` `-race` run (4.581 s) confirms that neither a local
nor a generic cancel claims the installed model is unchanged — only an explicit daemon precommit
status does. A native Tern browser session through a throwaway loopback HTTP→TLS/WebSocket proxy
exercised the 390 px and 849 px surfaces: login with live startup and no reload, live microphone
changes plus an independent-client push that preserved an unsaved name, timezone city search with
explicit Apply, the Clock app's `allow-scripts` settings document reaching ready with a blink patch
persisted through the public CLI, API-key create/clear/revoke, password change with reconnect, an
authenticated certificate PEM 200 attachment, Settings filter no-results, wakeword room-take start
and cancel with simulator routing restored to its default baseline and no captures, diagnostics
pause with 10 new/resumed 27 logs and filtering, and a final main-toolbar Bluetooth/media/volume/microphone pass before the apps
without overflow at 849 px. Limits: the managed WebKit build crashed in EGL and Chromium failed its
Mach-port launch; a direct Tern load of the self-signed TLS endpoint with `insecure:true` was still
refused ("Unacceptable TLS certificate") and certificate validation was not weakened, so the proxy
run remains the browser proof; Tern ignored resize emulation, so the viewport was dictated by the
pane rather than a full 1440 px desktop or physical phone; no real Wi-Fi/Bluetooth peer, physical
temperature, acoustics or full training-quality run, and no deployment. All temporary smoke services
were stopped and the proxy script deleted. Historical evidence retained as such: an
earlier WebKit build through a temporary plain-HTTP TLS-forwarding proxy showed two live sessions
receiving a cross-session Access change without refresh, with the resync client handling exercised
independently.

The browser schema lives in `proto/capsula/web/v1/web.proto`. Regenerate the Go bindings with
`protoc` and the browser module with `go run ./web/protogen` whenever that schema changes. The
WebSocket is HTTPS-only and speaks binary protobuf; there is no JSON-over-bytes fallback.

The clock face and LED-ring behaviour are built-in **system apps** compiled into `capsulad`.
They are not installable packages and need no `app install`, but they are otherwise normal apps:
`capsula app list` and the Web UI list them, `capsula app start capsula.clock` starts a stopped
one, `Launch`/`Stop` and run events work, their settings use the standard app-configuration
surface, and their services are routed over the app bus. Only packaging (no directory, process or
uninstall) is special. On a fresh install each is the default provider for its resource; the Web UI
can select another installed provider or none, and there is no hidden fallback.

```sh
capsula -data-dir build/demo settings app capsula.clock blink=true timezone=Europe/Moscow
capsula -data-dir build/demo settings app capsula.clock alarms='0 7 * * 1-5, 30 9 * * 6,7'
capsula -data-dir build/demo settings app capsula.ring effect=static palette='#FF7828'
capsula -data-dir build/demo settings app capsula.ring effect=breathe \
  palette='linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)'
capsula -data-dir build/demo app runs capsula.ring
capsula -data-dir build/demo app stop <run-id>
capsula -data-dir build/demo app start capsula.ring
```

`capsula.clock` settings cover `blink` (boolean), `timezone` (IANA name; empty follows the device),
`display_mode`, the timer duration, the stopwatch reset gesture (`hold` or `double_tap`) and
`alarms`, a string of five-field dcron entries separated by a comma after the fifth field and any
suffixes. Entries may carry `@IANA-zone`, `!once` and `!disabled`; the stable ID is
`expr [@zone] [!once]`. `alarm_labels` is a JSON map from that stable ID to a display label
(schedules stay solely in `alarms`); `Alarm.label`/`AddAlarm.label` expose it, and legacy
`alarm_label` migrates into the map. Older flat `app-settings/capsula.clock.json` /
`capsula.ring.json` files are converted once atomically; the canonical directory form wins and the
flat source is retained. Recurring alarms deduplicate by local wall-clock minute, so a DST fall-back
does not ring twice. Comma lists inside a field stay in that field (`0,30 7 * * 1-5`), so the
list is parsed five whitespace-separated fields (plus optional suffixes) at a time rather than split
on every comma; the Web UI edits friendly times and repeat days with timezone, one-shot and
enabled/disabled controls, and preserves untouched entries byte-for-byte on save. `capsula.ring` settings are `effect` (`breathe|spinner|rainbow|pulse|comet|sparkle|static`),
`palette` (canonical `linear-gradient(...)` or solid `#RRGGBB`), `speed`, `brightness` and an optional
wall-clock active window
(`schedule_enabled`, `schedule_start`/`schedule_end` as 24-hour `HH:MM` in the device timezone; the
window may cross midnight and equal bounds mean all day). Outside the window the app presents an
explicit blank frame on its normal baseline presentation, so the ring is dark but still yields to
volume, mute, alert and application captures, and the selected effect returns at the boundary. The
`static` effect shows one solid palette colour and does not animate, while `rainbow`
rotates/interpolates the palette's actual samples at 0, 1/3 and 2/3 without inventing unrelated hues. Legacy named palettes migrate to
equivalent gradients with their colours preserved; the legacy `mode` key is migrated to `effect`
and no longer offered. Legacy defaults `org.capsula.clock`,
`org.example.ring-controller` and `org.capsula.assistant` migrate to these IDs at boot, and the old
clock settings are copied to `capsula.clock` once. The Web UI default-provider dropdowns list
installed packages and system apps; an empty selection means no default provider.

Timers show the nearest countdown on the display and an orange ring that empties counterclockwise
from the top. Completion displays 00:00 and repeats the alert until dismissed; a framework-owned
panel/circle/talk press dismisses without also summoning the assistant. A running stopwatch shows
its elapsed time and pulses the ring orange; a tap pauses (releasing the ring) and resumes, and the
configured reset gesture — holding the button by default, or two quick taps — resets and dismisses
it without summoning the assistant. Temporary outputs return to the selected baseline, or physically
blank when none is selected. See
[speaker behavior and audit](docs/SPEAKER_BEHAVIOR.md) for the interaction contract, regression
coverage, and `-output-trace` hardware diagnostics.

Brightness for an auto-managed display is floored at 5 %: darkness and malformed/stale sensor
samples can never blank an enabled automatic display, while a manual brightness of 0 remains an
explicit off.

An upgrade quarantines previously installed legacy bundles
(`org.capsula.clock`, `org.example.ring-controller`, `org.capsula.assistant`, `org.capsula.media`):
they are not started, manually launched, selectable or listed, persisted defaults pointing at them
are cleared, and the daemon warns once at boot. Their files are retained non-destructively, so
upgrading the daemon suppresses the processes but does **not** reclaim their disk bytes. Seeing the
directories in a manual `ls` does not mean they are active. An operator may remove those bundle
directories explicitly to reclaim space; `capsulad` never auto-deletes installed app or operator
data. Quarantine affects only those retired sample IDs: custom/third-party providers and generic
interfaces are unaffected. The default providers are the built-in system apps `capsula.clock` and
`capsula.ring`, which are real registry entries with a run lifecycle, not a hidden baseline.

The optional dumb assistant is deliberately separate:

```sh
make dumbassistant
./build/bin/capsula -data-dir build/demo app install build/apps/org.capsula.dumbassistant
```

That nested project is not built, installed, selected or started by `make`, `make release`,
`make debug` or the simulator bootstrap.

The shared native SDK is emitted beside the binaries:

```text
build/lib/libcapsula.so.1.0.0  # Linux real file, SONAME libcapsula.so.1
build/lib/libcapsula.so.1      # symlink; libcapsula.so is the link-time alias
build/lib/libcapsula.1.dylib   # Darwin real file; libcapsula.dylib is the alias
build/lib/capsula-native.json
build/include/capsula/capsula.h
```

`capsula-native.json` records the ABI major/minor, the capability vocabulary and the exact library
file name. The daemon finds the runtime in `<exe-dir>/lib` or `<exe-dir>/../lib`, or from the
explicit `-native-lib-dir DIR` flag; the CLI also accepts `-native-lib-dir DIR` and
`CAPSULA_NATIVE_LIB_DIR`, and looks in `<data-dir>/lib` first. An installed device layout therefore
puts the library and metadata in `/data/capsdemo/lib` next to `/data/capsdemo/capsulad`. Apps that
declare `[native_sdk]` are refused when a compatible runtime is absent. Do not copy the library
into each app package.

C consumers include the header and link the versioned library; C++ consumers use the same header and
load it with `dlopen`. The ABI is plain C with integer handles and library-owned buffers, so no C++
class crosses the boundary.

For a Linux device cross-build, select the Go target explicitly so the linker choice follows the
target rather than the host:

```sh
make daemon GOOS=linux GOARCH=arm64
```

`make daemon` builds only `capsulad` and the CLI, so it needs no native SDK or cgo cross toolchain.
To cross-build the shared native SDK too, set `NATIVE_TARGET` to a Rust triple for the intended ABI
and install that target plus a matching linker/sysroot; for example:

```sh
make release GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu
```

The native target fails actionably if `NATIVE_TARGET` is unset or Cargo does not produce the
library, rather than packaging the host library. The Go SDK binding uses cgo, so its cross build
needs the same C toolchain. These commands produce local build artifacts; they are not a complete
device deployment.

### Hardware ABI and reproducible native release

The Capsula Mini runs glibc 2.33 with `/lib/ld-linux-aarch64.so.1`; it does **not** use musl. Build
the hardware shared library against glibc 2.33 or older, in an arm64 Debian Bullseye (glibc 2.31)
container, not Ubuntu 22.04/24.04:

```sh
# Daemon and CLI, glibc 2.31 baseline.
docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src golang:1.22-bullseye \
  make daemon-release GOOS=linux GOARCH=arm64

# Shared native SDK, same glibc baseline and the device's gnu ABI.
# Replace the Rust tag with the newest Bullseye-based image that satisfies the
# current Cargo dependencies; a bookworm/newer-libc image would raise the floor.
docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src rust:1.85-slim-bullseye \
  make native GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu
```

Inspect the resulting version requirements before installing anything:

```sh
readelf --version-info build/bin/capsulad | grep -o 'GLIBC_[0-9.]*' | sort -u
readelf --version-info build/lib/libcapsula.so.1.0.0 | grep -o 'GLIBC_[0-9.]*' | sort -u
```

Every listed `GLIBC_*` symbol must be 2.33 or older. A `dlopen` smoke test in a
`debian:bullseye` arm64 container is the loader-level check; neither command deploys to the device.
Native app packages that use this SDK must be built for glibc 2.33 or older as well.

`auth init` creates the self-signed certificate, sets an admin password (generated and printed
once unless `-password` is supplied) and issues a bootstrap API key. Open
`https://127.0.0.1:8443/`, accept the certificate after verifying the fingerprint shown by
`capsula cert status`, and sign in with the admin password. Automation can use the API key:

```sh
curl -k -H "X-API-Key: capsula_…" https://127.0.0.1:8443/api/status
```

Manage credentials locally:

```sh
capsula -data-dir build/demo auth set-password
capsula -data-dir build/demo auth key create automation
capsula -data-dir build/demo auth key list
capsula -data-dir build/demo auth key revoke automation
capsula -data-dir build/demo cert status
capsula -data-dir build/demo settings show
capsula -data-dir build/demo settings app capsula.clock blink=false
capsula -data-dir build/demo settings app capsula.ring effect=spinner \
  palette='linear-gradient(90deg, #00B4FF 0%, #0046D2 100%)'
```

The rest of the CLI uses the framework Unix socket as before:

```sh
./build/bin/capsula -data-dir build/demo status
./build/bin/capsula -data-dir build/demo dev wakeword            # simulator activation
./build/bin/capsula -data-dir build/demo dev state               # virtual display/ring frames
./build/bin/capsula -data-dir build/demo app list
./build/bin/capsula -data-dir build/demo media list              # providers installed by the operator
./build/bin/capsulad -health -socket build/demo/run/hal.sock     # exit 0 when the API answers
```

On the device the same CLI performs staged upgrades through the boot service:

```sh
capsula -data-dir /data/capsdemo daemon install -binary /tmp/capsulad.new
capsula -data-dir /data/capsdemo daemon rollback
```

`capsulad -health` is the readiness probe used by the boot service and the upgrade path. Exit 0
means the framework API answers, exit 1 means it does not, and a pre-upgrade binary without the
flag exits 2, which the boot service treats as "running but not probeable".

Go targets whose `GOOS` is `darwin` build with `-linkmode=external` because the internal Go linker
emits Mach-O binaries that current dyld rejects (missing `LC_UUID`). The Makefile keys this off the
target `GOOS`, not the build host, so a Linux cross-build keeps the default internal linker even on
macOS.

Production paths are supplied explicitly, for example:

```sh
capsulad -platform rockchip-stock \
  -data-dir /var/lib/capsulad -socket /run/capsulad/hal.sock \
  -apps-dir /var/lib/capsulad/apps -assets-dir /var/lib/capsulad/assets \
  -native-lib-dir /var/lib/capsulad/lib \
  -trusted-root /etc/capsulad/trusted_root.json \
  -web-addr 0.0.0.0:8443 -sandbox auto
```

## Native SDK (C ABI v1)

`sdk/native` builds `libcapsula`, a pure-Rust cdylib that owns the UDS gRPC transport, protobuf
reflection, managed AppBus session and raw-PCM audio path. Go, Rust, C and C++ consumers load it
dynamically; they never link a second transport stack. The normative interface is
[`sdk/native/include/capsula/capsula.h`](sdk/native/include/capsula/capsula.h), and the runnable
examples are [`sdk/native/examples/c/device_info.c`](sdk/native/examples/c/device_info.c),
[`sdk/cpp/example.cpp`](sdk/cpp/example.cpp),
[`sdk/rust/examples/`](sdk/rust/examples) and the optional
[`apps/dumbassistant`](apps/dumbassistant) Go project. Header and package comments plus those
examples are normative; this section is the integrated summary.

### Artifacts and build

`make native` builds and stages the runtime for the Go target, or run Cargo directly:

```sh
cargo build --release --manifest-path sdk/native/Cargo.toml
cargo test  --manifest-path sdk/native/Cargo.toml
```

| Platform | Real file | SONAME / install name | Metadata `library` |
|---|---|---|---|
| Linux | `libcapsula.so.1.0.0` | `libcapsula.so.1` | `libcapsula.so.1` |
| macOS | `libcapsula.1.dylib` | `@rpath/libcapsula.1.dylib` | `libcapsula.1.dylib` |

`make native` also writes the symlinks, `build/lib/capsula-native.json`, and the single public header
`build/include/capsula/capsula.h`. The metadata is produced by
`sdk/native/emit-runtime-metadata.sh <out.json> <library-filename>`; the library filename is always
relative to the runtime directory. Cargo package is `capsula-native`, `[lib] name = "capsula"`,
`crate-type = ["cdylib"]`. `build.rs` sets the SONAME/install name. Release strips debuginfo only;
exported ABI symbols remain.

The hardware path is `aarch64-unknown-linux-gnu`, built against glibc 2.33 or older (the Bullseye
recipe above). The pure-Rust dependencies need no TLS, OpenSSL or sysroot libraries. Cross-build
compiler requirements and commands are in the contract.

### Loading and ABI negotiation

Only these symbols are exported: `capsula_api_size`, `capsula_abi_major`, `capsula_abi_minor`,
`capsula_version`, `capsula_api_v1` and `capsula_get_api`. Everything else is reached through the
function table.

```c
capsula_api table = {0};
if (capsula_get_api(CAPSULA_ABI_MAJOR, sizeof table, &table) != CAPSULA_STATUS_OK) {
  /* UNSUPPORTED_ABI or INVALID_ARGUMENT; the table was not written */
}
```

- `capsula_get_api` accepts a table of at least `CAPSULA_API_V1_MIN_SIZE` (the stable
  `abi_major`/`abi_minor`/`struct_size` prefix; 16 bytes on 64-bit), copies
  `min(table_size, capsula_api_size())`, never writes past `table_size`, and reports the library's
  full current size in `struct_size`.
- An unsupported major returns `CAPSULA_STATUS_UNSUPPORTED_ABI` without writing. `abi_major` must
  match; a newer minor may append table fields.
- `capsula_api_v1()` returns the read-only process-wide table for consumers that can use the full
  current layout.
- Handles are opaque `uint64_t` values shared by clients, calls and buses; `0` is invalid.

Both the Rust and Go loaders zero the table, ask for their compiled table size, require major 1 and
a `struct_size` covering every field they call, then reject NULL required pointers. A runtime
metadata preflight in the launcher is separate: an app's `[native_sdk]` requirement checks ABI
major/minor and capabilities at install and launch, while the binding still verifies the loaded
library at startup.

### Memory, lifetime and threading

- Output data is returned in library-owned `capsula_buf { data, len }` values and released exactly
  once with `api->buf_free`. Empty output is `{NULL, 0}`. Never free with `free`, `delete` or a Go
  finalizer.
- Input buffers are borrowed for the duration of the call only; strings are explicit
  `(pointer, length)` UTF-8 and need not be NUL-terminated.
- Errors are `int32_t capsula_status`; `capsula_last_error()` is thread-local and valid until the
  next SDK call on that thread. gRPC failures also expose the canonical code/detail through
  `call_status`.
- One bounded process-wide Tokio runtime (two workers, two blocking threads) is created lazily by
  the first `client_connect`. There is no runtime or thread pool per client.
- **The loaded library must remain resident for the process lifetime.** The global runtime owns
  library threads, so Go/Rust bindings retain a successful loader handle after the last
  client/call/bus closes and the C++ example never `dlclose`s it. Closing handles releases native
  resources, but it does not make unloading the library safe. ABI v1 has no complete joined
  shutdown, so do not unload `libcapsula` after draining clients.
- Handles are safe from arbitrary threads. A call has one logical reader: concurrent receives are
  serialized, but callers should not depend on which wins. `send`, `half_close`, `cancel`, `end`
  and `release` may run while another thread blocks in `recv`; `cancel`, release, `bus_close` and
  `client_close` unblock it.
- Callbacks run on library threads and must not unwind across the C boundary. Callback-initiated
  close of the same bus returns `BUSY` instead of deadlocking.
- `client_close` cancels and releases its children; `bus_close` ends the session, unblocks pending
  inbound reads, delivers `on_disconnected` exactly once (including an intentional close) and joins
  dispatch threads. `user_data` must outlive `bus_close`.

### Calls, streaming and AppBus

- `call_start(service, method, kind, timeout)` supports unary, server-streaming,
  client-streaming and bidi calls through the same wire path; `kind` declares intent. It returns
  immediately and the transport task streams in the background.
- Send one or more messages with `call_send_json` / `call_send_proto`, close the request direction
  with `call_half_close`, and receive with `call_recv_json` / `call_recv_proto`. `recv` returns
  `OK` (message), `END_OF_STREAM`, `GRPC`, `TIMEOUT` or `CANCELLED`; a timeout leaves the call
  usable. `call_end`, `call_cancel`, `call_retain` and `call_release` manage completion and
  references.
- JSON uses canonical protobuf JSON and requires an embedded or registered descriptor; unknown
  methods/descriptors fail explicitly. Raw protobuf always works.
- `call_start_routed` targets another supervised app through `AppBusService/Call` and accepts raw
  protobuf even when no descriptor is registered for that app's custom service.
- `bus_open` starts the managed `AppBusService/Connect` session, sends `AppHello{run_id}` followed
  by `AppReady`, and delivers `on_ready`, `on_disconnected` and `on_event` callbacks. Event kinds
  cover invocations, settings changes, actions, assistant activations and input release; payloads
  are canonical protobuf JSON except raw-PCM audio. A touch activation carries `hold_to_talk`, so
  the assistant knows whether a release ends input without reading administrator-only device
  settings.
- `bus_ready` waits until the two frames are flushed and `on_ready` has returned. Inbound custom
  calls arrive on `on_call` on a bounded pool (4 workers, 32 queued calls, 64 live calls; overflow
  ends the call with `RESOURCE_EXHAUSTED`). The library owns the callback reference; retain the call
  for asynchronous work. A handler must call `call_end`; returning without it ends the call with
  `INTERNAL`.
- Bounded per-call/request queues (64–256 entries depending on direction) return `BUSY` or a gRPC
  `RESOURCE_EXHAUSTED` end instead of growing without limit.

### Audio fast path

`audio_capture_open`/`audio_capture_recv` and `audio_playback_open`/`audio_playback_write` move raw
PCM and frame metadata without JSON or base64 on the chunk path. Format and conversation metadata
travel once in the open control message. `frame_count == 0` derives frames from the byte length;
`monotonic_time_ns == 0` fills `CLOCK_MONOTONIC`. Playback control sends FLUSH/PAUSE/RESUME;
`call_half_close` drains playback, while cancel does not.

### Go binding

`sdk/go` is the nested module `capsula/sdk` (package `capsula/sdk/capsula`). It has zero
third-party dependencies (standard library plus cgo) and never links `libcapsula`: the cgo shim
`dlopen`s `CAPSULA_LIBRARY` (absolute path from the launcher) or the platform SONAME
(`libcapsula.so.1`, Darwin `libcapsula.1.dylib`), validates ABI major 1, and keeps the successfully
loaded engine resident for process lifetime. Closing a client frees client/bus/call handles only;
the native process-wide runtime stays loaded. A failed pre-initialization load is unloaded.

It has no gRPC/protobuf Go imports and keeps no generated framework types in its API. Go values are
converted to canonical protobuf JSON (lowerCamelCase, enum names, base64 bytes, 64-bit integers as
strings) or passed as raw protobuf bytes; PCM always uses the binary fast path.

```go
env := capsula.FromEnv() // CAPSULA_LIBRARY, socket, run ID and app ID
client, err := capsula.Connect(ctx, env)
if err != nil { return err }
defer client.Close()
client.SetHandlers(capsula.Handlers{/* Ready, Activation, Settings, … */})
if err := client.Start(ctx); err != nil { return err } // blocks until the bus closes
```

`Call`/`CallJSON`/`Stream`/`OpenStream`/`OpenBidi` expose all framework and routed call shapes;
`Handle` serves custom routed calls; DTO-based helpers cover presentations, captures, input, audio,
conversations, Bluetooth media, settings and audio state. `Connect` copies every native buffer into
Go memory and releases the native allocation before returning; `Playback.Write` copies its input.
`Start` blocks and context cancellation ends the bus; `Close` is idempotent. `Playback.Close`
half-closes and waits for the framework to drain queued audio; `Playback.Abort` discards it
immediately. Typed errors include `ErrLibraryUnavailable`, `LibraryError`, `ABIError`,
`CapabilityError`, `StatusError` and `ErrClosed`.

Build with `make sdk-go`; the dumbassistant project is the runnable example. cgo is required
(`CGO_ENABLED=1`) and a C compiler must be available. On Linux the shim links only `-ldl` (libc and
pthread come from the cgo runtime); Darwin needs no extra libraries. A cross build needs a target C
compiler:

```sh
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  go build -C sdk/go ./...
```

The module vendors the ABI declarations at `sdk/go/capsula/abi.h`. If the canonical
`sdk/native/include/capsula/capsula.h` changes, run `sdk/go/regenerate-abi.sh` and gate CI or
`make test-sdk` on `sdk/go/regenerate-abi.sh --check`. The root module carries
`require capsula/sdk v0.0.0` plus `replace capsula/sdk => ./sdk/go` only for daemon test fixtures;
the nested module is excluded from root `go build ./...`.

### Rust binding

`sdk/rust` is crate `capsula`, a thin `libloading`/serde wrapper. It has no tonic/prost/native-crate
dependency and never links the engine.

```rust
use capsula::Library;

let library = Library::load()?;
let client = library.connect_env()?; // CAPSULA_SOCKET / CAPSULA_RUN_ID / CAPSULA_APP_ID
let reply = client.unary_json(
    "capsula.framework.v1.DeviceService",
    "GetInfo",
    b"{}",
    Some(std::time::Duration::from_secs(2)),
)?;
```

`Library::load()` resolves `CAPSULA_LIBRARY` (absolute path) first, then
`CAPSULA_LIBRARY_DIR` joined with the platform soname, then the soname itself. `Library::open(path)`
bypasses the environment. `Client::start_call` covers all four call kinds;
`Client::start_routed` uses the app-bus routed path. `Call`, `Client` and `Bus` are `Send + Sync`;
`recv_*` returns `Ok(None)` for a clean end, and `TIMEOUT` leaves the call usable. AppBus handlers
implement `BusHandler`; `InboundCall` is borrowed and the library owns its callback reference, so
retain it for asynchronous work and always finish it with `end(0, "")`. Build with
`make sdk-rust`; run the real-daemon smoke clients with
`cargo run --manifest-path sdk/rust/Cargo.toml --example smoke_unary -- …` (also `smoke_stream` and
`smoke_bus`), using `CAPSULA_LIBRARY`, `CAPSULA_SOCKET` and a supervised `CAPSULA_RUN_ID`.

### C and C++

C consumers include `<capsula/capsula.h>` from `build/include`. The C example links the library
directly (the loader resolves the SONAME at runtime):

```sh
cc -I build/include sdk/native/examples/c/device_info.c -L build/lib -lcapsula \
  -o build/bin/capsula-c-example
LD_LIBRARY_PATH=$PWD/build/lib CAPSULA_RUN_ID=run-... ./build/bin/capsula-c-example
```

The C++ example uses `<capsula/capsula.h>`, `dlopen`/`dlsym` of `capsula_get_api`, RAII wrappers
for handles and buffers, and no link against `libcapsula`:

```sh
c++ -std=c++17 -pthread -I build/include sdk/cpp/example.cpp \
    -o build/capsula-cpp-example -ldl
# macOS: omit -ldl; the CMake target links Threads::Threads and CMAKE_DL_LIBS
```

### Runtime metadata, capabilities and descriptors

`sdk/native/emit-runtime-metadata.sh <out.json> <library>` writes the launcher metadata with
`abi_major: 1`, `abi_minor: 0`, the capability array and the relative library filename. The
capability vocabulary covers `capsula.abi.v1`, transport modes, reflection/raw protobuf, AppBus
connect/events/notifications/routed calls, audio capture/playback/PCM fast path, descriptor
registration, deadlines/cancel/concurrent handles, and `capsula.service.<name>` for every framework
service. Apps declare only what they need:

```toml
[native_sdk]
abi_major = 1
min_minor = 0
required_capabilities = ["capsula.service.audio", "capsula.audio.playback"]
```

The launcher preflights this before starting the app and injects `CAPSULA_LIBRARY`,
`CAPSULA_LIBRARY_DIR` and the loader path when a runtime is discovered. The binding then performs
its own ABI check.

Framework descriptors are embedded in the library. Custom app contracts are not; register a
serialized `FileDescriptorSet` with `client_register_descriptors` (for example the bytes from
`AppInterfaceService.DescribeInterface`) before using JSON for that service. Regenerate the embedded
set when framework protos change:

```sh
protoc -I proto --include_imports \
  --descriptor_set_out=sdk/native/descriptors/capsula-framework.protoset \
  proto/capsula/framework/v1/framework.proto \
  proto/capsula/contracts/media/v1/media.proto \
  proto/capsula/contracts/clock/v1/clock.proto \
  proto/capsula/contracts/ring/v1/ring.proto
```

### Compatibility policy

ABI, SDK API and framework protocol versions are separate: ABI v1 describes the C table,
`CAPSULA_SDK` currently tracks the framework protocol (0.5), and binding source compatibility is
each module's own semver concern. Within ABI major 1, table fields are only appended, existing
signatures/positions/constants and ownership/threading guarantees never change, and new required
behavior starts a new major. Compatibility failures are explicit (`UNSUPPORTED_ABI`,
`ABIError`/`CapabilityError`, launcher rejection) rather than a loader crash or silent fallback.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/capsulad` | daemon entrypoint |
| `cmd/capsula` | local admin CLI (certs, apps, logs, firmware inspection, display bring-up) |
| `cmd/ledmap` | interactive display-channel calibration over adb (bring-up tool) |
| `internal/daemon` | wiring, gRPC services, supervision, routing/output loops |
| `internal/bus` | reverse-RPC app bus |
| `internal/presentations`, `internal/routing` | dynamic outputs and capture stack |
| `internal/audio`, `internal/input`, `internal/wakeword` | realtime services |
| `internal/bluetooth` | BlueZ D-Bus facade (power, pairing, AVRCP, absolute volume) |
| `internal/backend/simulator.go` | complete virtual backend |
| `internal/backend/rockchip` | RK3308 stock-firmware backend (vendor LED ABI recovered) |
| `internal/sandbox` | Linux read-only namespace launcher + seccomp |
| `internal/websecurity` | self-signed TLS, Argon2id password, API keys, sessions |
| `internal/provenance` | Sigstore/GitHub attestation verification |
| `internal/webui` | HTTPS Web UI server, session auth, SPA assets, protobuf WebSocket |
| `web` | independently built Vue 3 SPA, browser protobuf bindings and their generator |
| `internal/sounds` | embedded framework UI cue audio and its CC BY-SA `COPYING` |
| `sdk/native` | Rust shared engine behind the stable C ABI (`libcapsula.so.1` / `.dylib`), header and C example |
| `sdk/go` | nested Go module `capsula/sdk`; thin binding over the shared native library |
| `sdk/rust` | Rust binding that dynamically loads the shared native library, with real-daemon smoke examples |
| `sdk/cpp` | C++ `dlopen` example and CMake target for the C ABI |
| `interpreter` | pinned QuickJS/MicroPython embed hosts, script SDKs, samples and the end-to-end `smoke/run.sh` |
| `apps/dumbassistant` | optional first-party nested Go module; built only on request |
| `deploy/rockchip-stock` | single removable boot hook and bring-up scripts for the RK3308 stock image |
| `proto/` | draft v0.5 framework and standard app-contract schemas (media, clock, ring); built-in system apps publish contracts from here |
| `docs/` | glossary, current app API, speaker behavior, implementation status, hardware and wakeword evidence |

## Verification

```sh
make test      # daemon, framework and routing tests
make test-sdk  # Go/Rust binding tests and vendored ABI drift check

cargo test --manifest-path sdk/native/Cargo.toml
sdk/go/regenerate-abi.sh --check
(cd sdk/go && CGO_ENABLED=1 go build ./... && \
  CGO_ENABLED=1 go test -c -o /tmp/capsula-go.test ./capsula)
cargo build --manifest-path sdk/rust/Cargo.toml --examples
cc  -I build/include sdk/native/examples/c/device_info.c -L build/lib -lcapsula \
    -o build/bin/capsula-c-example
c++ -std=c++17 -pthread -I build/include sdk/cpp/example.cpp \
    -o build/bin/capsula-cpp-example -ldl
```

Tests cover routing/capture semantics, preset validation, touch mapping/gesture inference/system
suppression and cancellation, settings CAS and secret redaction, self-signed TLS, password sessions,
API-key accept/revoke and CSRF, genuine Sigstore fixture verification, bounded log rotation, and an
end-to-end simulator run (supervision, selected-provider recovery and deselection, singleton
invocation, routed unary/streaming calls, standard-provider and Bluetooth-fallback media control,
touch partial/full capture, crash cleanup, wakeword capture/release, mic watchdog and masked
settings). The 2026-11-13 focused integrated run (macOS arm64, `-ldflags '-linkmode=external'`,
real Mach-O native runtime from `build/lib` via `CAPSULA_NATIVE_LIB_DIR`) passed race-enabled suites
for input/backend/wakeword/settings/discovery/audio, focused supervisor and daemon lifecycle
recovery, Bluetooth registration/ordering, Web UI hostname and Restart wording, the final
standard-provider/Bluetooth-fallback media tests, and all eleven assistant scenarios; fresh host
CLI/daemon smoke covered health/status, simulated circle press, empty media list, shutdown and
persisted-hostname startup with simulator mDNS correctly unsupported. Two lifecycle policy edges
found after the first pass were then repaired: a READY-to-crash loop keeps backoff until 30 s of
continuous READY, and selected-origin/forced recovery re-checks selection regardless of
`restart_on_failure`; controlled-clock regressions and the full `-race` supervisor suite pass, and
the integrated daemon lifecycle recovery set passes 7/7 against the real native runtime. The Web UI
is additionally tested through login/session, CSRF and static API-key flows. A Chrome-headless browser session
against a real HTTPS simulator daemon observed the host-name default, normalized save and restart
persistence, invalid-label rejection at the HTML and server layers, the simulator's unsupported
mDNS warning with no false advertisement claim, and the **Restart** labels on Clock, Ring and
Diagnostics. A second browser run drove the corrected apply through the production Web
UI/auth/TLS/settings path against a successful responder fixture: startup, two GETs and a
same-name save made zero `SetHostName` calls with a clean advertised status; a changed label made
one call; a collision remained visible with no advertised claim; restoring the label recovered.
Real Bluetooth peer recovery and the acoustic/VAD room gate remain hardware gates, not simulator
or browser results; device `.local` resolution and a warning-free mDNS startup were observed after
the 2026-11-13 deployment. Final host checks used a fresh HEAD-built
Mach-O runtime with matching metadata and the exact embedded descriptor in a temporary directory
(the pre-existing `build/lib` candidate did not match): under `-race` with the external linker, the
audio drop/frame/lifecycle subset passed (2.238 s), the nested assistant module passed (6.222 s),
and the actual daemon passed all 14 assistant scenarios (128.684 s), including zero pre-capture /
active / post-close noise baseline and typed-level/no-playback assertions. The BlueZ
`MediaControl1` private-bus wire fixture and the barrier-driven Avahi fixture plus Web UI browser
run are separate local evidence. The follow-up daemon and assistant were deployed on 2026-11-13
after a full host backup, the canonical health gate and a normal restart; the dated entry in
[STATUS.md](docs/STATUS.md) records the installed hashes, preserved choices and remaining limits.

## Status

See `docs/STATUS.md` for the implementation matrix and the remaining hardware gates, and
`docs/HARDWARE.md` for the evidence extracted from the supplied Rockchip firmware dump.
