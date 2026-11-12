# Implementation status (initial slice)

Target schemas: draft v0.5 (`proto/capsula/framework/v1/framework.proto` and the media contract).
Generated Go bindings are committed under `gen/go`. The example ring schema and its generated
bindings were retired with the built-in ring provider; the ring contract now ships as the `capsula.ring` system-app interface. The wire API is **not frozen**.

> **Deviation from draft 0.5:** at the project owner's request, the Web UI no longer requires
> client-certificate mTLS. It uses a self-signed HTTPS certificate plus an Argon2id admin password
> and static opaque API keys. The HAL/app bus remains Unix-socket only; this changes only the
> browser-facing surface.

## Speaker behavior audit (2026-11-12)

The physical-speaker lifecycle audit and fixes are documented in
[Speaker behavior and audit](SPEAKER_BEHAVIOR.md). Timer progress/repetition/dismissal,
assistant and output cleanup, saved no-provider choices, input ownership, audio cancellation,
feedback generations, and Bluetooth state convergence have regression coverage. The repaired
daemon, assistant 0.2.0, and canonical boot hook were deployed together on the Rockchip unit.
On-device evdev scenarios, rendered driver-frame traces, digital speaker-output capture,
microphone-gate transitions, and a final restart verified the covered paths. Original user
settings and the 2000–3500 ms reply delay were retained.

### Follow-up fixes (2026-11-13)

A killed or crashed default provider is restarted immediately instead of staying
unavailable (a lost framework connection is a failure, not a deliberate stop).
The circle/play-pause key dismisses an in-flight assistant turn through the app's
`dismiss` action. A running stopwatch owns the ring with an orange pulse and the
primary button with tap pause/resume and a configurable hold or double-tap reset
that never summons the assistant. A Bluetooth disconnect shows one pulse and no
`bt` glyph; the sink endpoint is repaired before connecting, PA modules are not
unloaded on a healthy connect, and the two audio profiles connect concurrently.
Dumb-assistant speech detection is relative to a learned room-noise floor.
These paths have unit and simulator-daemon regression coverage; they have not
been re-deployed to the Rockchip unit, and the acoustic-room, real-Mac Bluetooth
and listening gates below remain open.

This does not replace the remaining acoustic/room, browser, or real-peer Bluetooth gates below.

## Implemented and exercised

| Area | State | Evidence |
|---|---|---|
| All 15 framework gRPC services over one UDS | implemented | simulator run + tests |
| App supervision, manifests, restart/backoff, log capture | implemented: a lost framework connection is a failure, not a deliberate stop, so `restart_on_failure` apps restart; the first restart after a healthy run is immediate, later failures back off exponentially | test apps run, crash test, `TestDefaultAppRestartsAfterLostConnection` |
| Singleton launch invocations (queued/bounded/expiring) | implemented | delay-injection test |
| Reverse-RPC bus (`Connect`, routed unary/streaming `Call`) | implemented | routed-call test |
| Presentations (first-content readiness, in-place content) | implemented | routing tests + built-in clock system app |
| Built-in clock and ring system apps (`capsula.clock`, `capsula.ring`) | implemented as compiled-in applications with normal registry identity, `ApplicationService` list/launch/stop/run events, app settings, routing ownership and app-bus endpoints; only packaging is special (no directory, process, install or uninstall). Clock service `capsula.contracts.clock.v1.ClockService` covers stopwatch, timers and alarms (unary); ring service `capsula.contracts.ring.v1.RingService` exposes state. Clock settings: `blink`, `timezone`, `display_mode`, `timer_seconds`, `stopwatch_reset_gesture=hold|double_tap`, alarm label/hour/minute/days/timezone/enabled. A running stopwatch holds an orange whole-ring pulse; a tap pauses/resumes it and the configured hold or double tap resets it without summoning the assistant. Ring settings: `effect=breathe|spinner|rainbow|pulse|comet|sparkle`, `palette`, `speed`, `brightness`; legacy `mode` is read once for migration and hidden from new settings UIs. Both are seeded as defaults on a fresh install; an empty selection means none. Legacy `org.capsula.clock` / `org.example.ring-controller` settings and default IDs migrate, and retired sample bundles stay quarantined. External captures preempt and release normally. | simulator tests for list/get/default round-trip, stop/launch/run events, capture preemption and clock service calls; the new build was not redeployed to the unit |
| Captures (last eligible wins, baseline fallback, out-of-order release) | implemented | routing tests |
| Touch partial/full, system controls, cancellation | implemented | routing/input tests |
| Audio mixing, fan-out, ducking, pre-roll, mic watchdog | implemented | simulator test |
| Typed app settings, CAS, masked redaction | implemented | settings tests |
| HTTPS Web UI: self-signed TLS, Argon2id password sessions, static API keys, CSRF | implemented | websecurity + webui auth tests |
| Certificate + credential bootstrap (self-signed, password, API keys) | implemented | websecurity tests |
| Provenance verification against an offline Sigstore root | implemented | genuine fixture test |
| Shared read-only asset catalog | implemented | asset service |
| Simulator backend | complete | dev admin + tests |
| Rockchip backend probing, evdev, PulseAudio, GPIO mute | daemon runs on a live unit | `capsula status` on RK3308, app launch in mountns sandbox |
| Rockchip WS2812B ring + IS31FL3236 segment output | both channel maps validated on hardware: clock face correct, ring pixel 0 at the top with smooth clockwise travel and correct RGB order; gamma still unmeasured | `ledmap.json`, `capsula platform ledtest -ring`, `TestEncodeMultiCtrlFrameClockPattern` |
| Rockchip microphone hardware mute (GPIO 21) | verified end to end: `gpio21` and the PulseAudio source mute track the setting, and the gate cuts audio (capture RMS 1977 → 38 with PulseAudio unmuted) | acoustic loop test on the unit |
| Rockchip sandbox (root mount namespace, distinct app UID) | implemented, validated on hardware | `/proc/<pid>/status` UID 21556, private `mnt` namespace; 2026-11-12: native app mapped `/capsula/lib` (ABI 1.0) and reported `APP_READY` |
| Audio through the mixer (playback, volume, ducking, mic capture) | verified on hardware: a 1 kHz tone from an app measured on `master.monitor` gives RMS 4454 at 70 % volume and 1908 at 30 % (exactly 0.7 / 0.3 of the source's 6364, so software master volume is linear and exact); a conversation ducks it to 0.25 × (1113 / 477) with a ~150 ms fade; microphone capture arrives over the bus and the visible-indication watchdog terminates it 5 s after the last qualifying indication | scripted harness run on the unit |
| Talk button (circle key) + volume keys | implemented and exercised on the unit: hold 700 ms activates (manual origins do not require a trained keyword model), release ends the turn, short taps do nothing; volume taps ±5 %, held ramps accelerate 200→60 ms and stop at bounds, with a generated tick and a 1 s percentage overlay | `buttons5.log`: activation → conversation → capture → input released |
| Microphone key toggle + muted indication | implemented, verified on hardware | one mute per tap (`TestDiscreteKeysActOnPressOnly`), red run pixels 5–7 with fade-in/out |
| Web UI restructure (overview, settings, access, diagnostics) | implemented and exercised over HTTPS on the unit: all four pages render, volume/settings round-trip, API-key create+revoke works, certificate downloads as PEM | live checks against the daemon |
| Bluetooth adapter control (BlueZ D-Bus) | implemented: power, discoverable, pairable (persisted as `bluetooth_pairable`), paired-device list and unpair. The daemon loads PulseAudio's Bluetooth discovery so the adapter advertises A2DP Sink (0x110B) and Headset (0x1108); the old `hciconfig piscan` error was an adapter nobody powered, and the framework now powers it at startup and after a BlueZ restart | live unit: `hciconfig` UP RUNNING, `PSCAN ISCAN` after the Web UI toggle, BlueZ Pairable toggled/persisted, paired MacBook listed |
| Bluetooth AVRCP backend | implemented: play/pause/next/previous, absolute volume to the source and metadata when the source exposes it. The bundled sample media provider was removed, so a Web UI player requires an operator-installed provider | live MacBook: Play/Next accepted and playback state changed; 50 % master volume set transport `Volume=64` of 127; no bundled provider ships |
| Bluetooth notifications, routing and volume | implemented: connect plays three completed blue fade-in/out ring pulses and shows lowercase `bt` on the two rightmost segment cells for 2 s; disconnect plays exactly one pulse and no glyph, through ordinary `SystemAppID` presentations/captures; the sink endpoint is re-registered before connecting if BlueZ forgot it, and PulseAudio Bluetooth modules are never unloaded while a source is connected; connect loads `module-loopback` from `bluez_source.<addr>.a2dp_source` to `master` with a 250 ms buffer and disconnect unloads it. Transport/AVRCP volume is event-driven through the shared `applyMasterVolume` path, with no poll, echo window or debounce; a headless NoInputNoOutput BlueZ pairing agent permits first-time bonding only while `bluetooth_pairable` is enabled; a sink endpoint is advertised before pairable is enabled and re-registered after BlueZ restart; audio profiles connect concurrently with one retry, removing the ~3 s sequential connection delay. Pairable application can now fail if the agent cannot register; the daemon logs `pairable=...`, leaves the recorded state unchanged, and retries on the next settings change. | simulator: pulse/glyph timing, ownership/preemption, lifecycle; hardware-unverified: real Mac pairing latency, A2DP audio, AVRCP absolute-volume behavior, physical light; earlier live logs covered routes and endpoints only |
| Master volume and system cues | one canonical control path (`applyMasterVolume`) is shared by volume keys, `AudioService.SetMasterVolume` and AVRCP changes, and `applyMicrophoneMute` is shared by the microphone key and `AudioService.SetMicrophoneMuted`; the settings watcher remains the single writer for the muted indicator. The software mixer applies the master gain (historical live measurement: 0 % -> silence, 1 % -> peak 58, 5 % -> 294, 10 % -> 589, 20 % -> 1179). Bluetooth keeps the transport and framework master equal, but does not trust AVRCP as the only attenuation: the loopback carries a local safety gain of master^1.5 and the local bluez source stays at a fixed 90 % trim, re-applied on source/route events rather than by a poll. PA is decoupled with `avrcp_absolute_volume=0` on each source connect. The volume tick peaks at 0.22 of full scale. | live capture and module/decoupling checks were run before the event-driven rewrite; simulator tests now cover ownership, timing and the shared volume path. The new AVRCP transport behavior, real Mac pairing/audio and the master^1.5 audible result remain hardware-unverified | UI cues are the embedded `internal/sounds` ubuntu-sounds set (CC BY-SA, `COPYING` preserved): `generic.wav` for a volume step, `warning.wav`/`info.wav` for mute/unmute, and `startup.wav` once on installation then only with `startup_sound_enabled` (off by default); all stream through the mixer at a -8 dB UI trim.
| Display/ring brightness: constant or ambient-light | implemented with the stock curve `trunc(log(0.75+0.05*lux)*19.9375)` clamped to [5,95] %, 5-point deadband and animated fade, plus an auto-only 5 % floor: 0 lux, malformed/NaN/±Inf/negative or stale samples never blank an enabled automatic display, and a manual 0 remains explicit off. 5 % survives backend quantization (13/255). | `TestAmbientBrightnessMatchesStockCurve` (floor, invalid samples, monotonicity), `TestEncodeMultiCtrlFrame` floor case; physical luminosity in darkness remains hardware-unverified |
| Device timezone (Web UI) with built-in clock override | implemented, validated end to end | Web UI round-trip restarts boot apps; the clock re-reads `TZ` (11:59 MSK ↔ 16:59 Asia/Shanghai) and `capsula.clock timezone` overrides the device zone |
| Rockchip DT key map (volume, mic, play/pause, voice) | implemented | device-tree parser + tests |
| Rockchip ambient-light and thermal sensors | implemented | IIO/thermal sysfs tests |
| Web UI pages (all settings, apps, media, diagnostics) | implemented; server-owned app records distinguish built-in system apps from installed packages, system-app pages expose lifecycle/packaging metadata and omit package provenance, default selectors include every provider with an explicit none choice, and clock/ring pages discover their app-bus services through AppInterfaceService and call them with protobuf JSON. Invalid app settings show the daemon validator message and keep the prior value. | login/API-key + CSRF tests; browser round-trip verification pending for this change |
| Shared native SDK (`libcapsula`) | implemented as a pure Rust cdylib with frozen C ABI v1: bounded `capsula_get_api`/table copy, explicit statuses and buffer ownership, reflection JSON/protobuf for all framework unary/client/server/bidi calls, managed AppBus, routed and concurrent calls, cancellation/deadlines, raw-PCM audio, and runtime metadata with capability negotiation. The per-call response backlog is bounded at 512 events, deliberately above the transport flow-control window: a brief consumer stall (or an operator-paused app) creates backpressure instead of failing a live audio capture. Go/Rust bindings load it dynamically; C/C++ use the same header. Release builds strip debuginfo, debug builds do not. Integrated build, usage, lifetime and compatibility details are in the README’s Native SDK section | ABI/table sizing and lifetime tests; C/C++/Go/Rust consumer checks; physical device 2026-11-12: release cdylib + metadata installed in `/data/capsdemo/lib`, mapped at `/capsula/lib` under the mount namespace with `APP_READY` |
| Media-player standard contract | framework plumbing retained: unary commands and server-streaming `WatchPlayers` over the app bus; no sample provider is built or shipped in this repository | app-bus routed streaming tests |
| Optional dumb assistant (`org.capsula.dumbassistant`) | separate optional nested Go project built only by `make dumbassistant`: consumes real microphone PCM, tracks input level on the whole teal ring, learns a room-noise floor for speech onset/end (steady fan/traffic noise no longer keeps a turn open), ends turns on hold-to-talk release or 700 ms post-speech silence, cancels cleanly on mute/no-speech/capture end, and declares a `dismiss` action so the circle key aborts listening, delay and playback and drops queued activations, then fades to a blue chase for the delay and reply. `delay` accepts empty/0, a constant, or an ascending inclusive range 0–60000 ms. `reply_asset` is relative to the shared read-only app asset store (`/assets`; apps receive `CAPSULA_ASSETS`, empty uses the packaged fallback) and must be registered in the shared asset manifest; absolute, traversal, symlink and unregistered values are rejected synchronously by a daemon validator (`ApplicationConfigurationService.Update` -> InvalidArgument, no write) and again by the app. | simulator tests with injected PCM, ring snapshots and Update rejection (2026-11-12); real acoustic thresholds, LED gamma and talk-button timing remain hardware-unverified |
| Framework boot ownership on the stock image | one self-contained removable hook, `/etc/init.d/S99btctl.sh` (repo `deploy/rockchip-stock/S99btctl.sh`), performs all capsulad-specific preparation and recovery: boot log cap, data/run dirs, Bluetooth UART IRQ affinity, daemon start/stop, health gate, vendor-script preservation/retirement and vendor fallback. `disable` stops the daemon, restores the vendor scripts, starts the vendor chain and moves the hook aside; `enable` restores it. It replaces the former S60capsulad + wrapper pair. Name/order are forced by rcS expanding `/etc/init.d/S??*` before the `/data` overlay pivot. | pending verification: shell syntax and simulator startup harness; the single-hook build was **not** deployed to the unit. The previous S60capsulad deployment was verified on 2026-11-12 (health 0, watchdog 21 s); hardware power-cycle/fallback of the new script remains a gate |
| In-place daemon upgrade with rollback | implemented and exercised: executable check, staged rename beside the live binary, restart through the boot service, framework-API health gate, previous binary preserved; `capsula daemon rollback` is the inverse, but it restores the binary only: a full return to the old app-based clock/ring/assistant also needs the old bundles and manifests from the host backup, and the old daemon's strict manifest loader does not understand `[native_sdk]` | host simulator install+rollback drill, live install on the unit (21,954,712 → 21,889,176 bytes), 2026-11-12 live install to 22,610,180 bytes (sha256 `86d131ae…fe604`) with the 22,479,000-byte `.prev` retained, health after restart; 2026-11-12T12:26Z install of the working-tree build (23,003,288 bytes, sha256 `30c5c1e4…48363`) with the 22,610,180-byte `.prev` retained, byte-identical md5 on the unit, `S60capsulad restart` healthy |
| Bounded logs and storage guard | implemented: 512 KiB log file plus one rotated copy, configurable `-wakeword-negatives`, and a 2 MiB free-space floor for new takes and training | `internal/logbus` rotation tests; deployed `/data` at 93 % (earlier), 69 % used / 24.7 MB free after the 2026-11-12 deployment |
| Personal wakeword embedding engine and training | implemented: incremental Google/openWakeWord backbone, ARM64 NEON, shared-projection head, on-device training and atomic replacement; strict warm `CNEG0002` corpus with real same-split preroll; legacy models and cold corpora refused | ONNX/kernel parity, gradient checks, real Mini frontend 37.9 ms/output, production loader accepts the 872-stream gzip corpus; `word.personal` installed and selected after full native training (670.6 s; on-device A35 replay 6/6 words, 9/9 backgrounds) and a one-shot 237-clip fresh negative set with zero false activations; user reports live use mostly works (informal, not an accuracy/daily-FAR certificate); personal trial, not field certified; see [WAKEWORD.md](WAKEWORD.md) |

## Explicitly unavailable

- **User, PID, network and IPC namespaces, and seccomp filters, on the RK3308 stock kernel.** Measured
  on a live unit: `/proc/self/ns` exposes only `mnt`, `unshare(CLONE_NEWUSER|CLONE_NEWNET)` returns
  `EINVAL` and `PR_SET_SECCOMP` returns `EINVAL`. The daemon therefore runs apps in a root mount
  namespace with a distinct UID, reports `mode=mountns`, and drops the seccomp claim instead of
  failing the launch (`Seccomp` is probed by a helper process before any app starts).
- **Raw touch coordinates on the current unit.** Its only evdev node is `gpio-keys`; there is no
  panel, so `touch.panel.main` advertises `coordinates=false` and full/raw capture stays
  simulator-tested.
- **QuickJS / MicroPython** host runners. `ListRuntimes` reports unavailable; packages are refused.
- **Amlogic** stock/Alpine backends. Platform selection fails with an explicit message.
- **A field-certified personal wakeword model for the connected room.** A warm-corpus
  `word.personal` model is installed and selected. Native training completed in 670.6 s, and an
  on-device A35 replay scored 6/6 word takes and 9/9 backgrounds. It remains a small personal trial:
  long-duration false-activation/miss rates are unmeasured, no speaker-independent Russian head is
  bundled, and zero false activations over 40 minutes is not a daily-rate guarantee. See
  [WAKEWORD.md](WAKEWORD.md).
- **Vendor system-service control** (`SystemService.ControlUnit`). Returns `UNIMPLEMENTED`.

## Known design limits in this slice

- **Sandbox strength on Rockchip stock is mount-only.** No user, PID, network or IPC namespaces and no
  seccomp filter are available in kernel 4.4.167 as shipped, so an app cannot be contained beyond a
  read-only private mount namespace plus a distinct UID. Native apps are consequently trusted code:
  provenance verification is the control that matters, not runtime isolation.
- **No ABI compatibility rejection on the simulator** (host builds are accepted for development).
  On Rockchip, native packages must declare `aarch64` and a Linux glibc 2.33-compatible ABI.
- **LED vendor ABI is recovered, not documented**: ioctl numbers, record layout and physical
  permutations come from the stock `voice-agent` binary for this firmware build. `GET_COUNT` is
  confirmed on a live unit (36 channels on `/dev/topleds`, 30 on `/dev/led_multi_ctrl`); the segment
  map, ring pitch/order and RGB order are validated on the unit, while gamma is unmeasured (no
  photometer). The vendor assistant is retired at boot, so concurrent writes to the LED nodes no
  longer occur.
- **Play/pause and voice keys are backend-level key events.** The framework input manager
  consumes volume, microphone and primary; `play_pause` and `voice` reach an application only
  while it holds a full input capture.
- **Provenance trust policy**: the Sigstore trusted root is supplied offline; verification uses
  transparency log + observer timestamps + SCTs via `sigstore-go`. A missing root yields
  `PROVENANCE_UNVERIFIED`, never a false positive. Root refresh policy remains an operator task.
- **Self-signed certificate trust** is an operator decision. Browsers warn until the fingerprint
  is trusted; server renewal requires a daemon restart. Credential bootstrap must exist before the
  Web UI starts.
- **Credential recovery** is local-console only: `capsula auth set-password` and API-key CLI
  commands operate on the daemon's security store. API keys are shown once and stored as hashes.
- **API keys protect the HTTP Web UI/API**, not the local Unix-socket gRPC plane, which remains
  filesystem-local and requires supervised-run identity for app-scoped calls. Revocation is serial-list based, not a CRL/OCSP responder.
- **Bluetooth media is source-dependent.** Control and transport state worked against the test
  MacBook, but that source exposed no `Track` attributes, so title/artist stayed empty. The reader
  parses BlueZ `MediaPlayer1.Track` when present. HSP/HFP telephony is not implemented; only A2DP
  Sink and AVRCP.
- **Bluetooth volume: synced displays, local bound.** The Mac's AVRCP volume and the Capsula master
  are kept equal in both directions, but the Mac is not trusted to attenuate: it can report a
  minimal transport volume while the captured stream is near full scale. The loopback therefore
  applies the master as a local gain (cube-root compensated for PA's cubic sink-input volume), with
  the source at a fixed 90 % trim. The effective level is bounded by the master and is at most
  ~0.73 × it. The PA source/loopback numbers are gain stages, not the listening volume; making all
  three stages numerically equal would either double-attenuate or, with no local ceiling, allow a
  “5 %” setting to play at full scale.
- **Bluetooth audio now uses a native A2DP SBC sink endpoint.** The stock `voice-agent`/`btctl`
  chain sounded better than the old PulseAudio source + loopback path, which added a resampler and
  a second volume stage. capsulad now unloads PA's bluez5 Bluetooth modules, registers an
  `org.bluez.MediaEndpoint1` at `/org/capsula/a2dp/sbc`, and obtains the transport fd through
  `MediaTransport1.Acquire`. BlueZ 5.65 calls `SelectConfiguration(ay)` and
  `SetConfiguration(o, a{sv})`; the transport has no `Endpoint` property, so the path from
  `SetConfiguration` is retained and UUID/Codec is the fallback. Only a `pending` or `active`
  transport is acquired: an idle transport is configured but silent, and blocking on its fd was
  observed as a silent stream followed by `EOF` while the source played on a different transport.
  RTP is depacketized and decoded in-process by `internal/sbc`, a pure-Go 8-subband SBC decoder
  (Apache-2.0 Android reference, with attribution). It is fed every A2DP payload, keeps its
  synthesis history across packets, and writes directly to the mixer, so there is no decoder
  subprocess, temp file, chunk boundary or warm-up. The A2DP media header stores the frame count
  in bits 3-0 (bits 7/6/5 are F/S/L); a one-frame packet has a zero high nibble, which previously
  discarded the stream. A raw-SBC packet with no RTP headers is also accepted. The first transport
  packet is logged in hex so a silent source can be distinguished from a parse fault. The decoder
  matches both the system `sbcdec` and the Android reference sample-for-sample on the 172-frame
  fixture. The `pacat` output latency is 120 ms and the PA Bluetooth-module scan is throttled to
  10 s. The stripped daemon binary was unchanged in size for that change (22,479,000 bytes; the
  2026-11-12 deployment installs 22,610,180 bytes). PA still owns app playback and the
  microphones. End-to-end listening is the remaining gate: the endpoint reached CONFIGURED/OPEN
  and a transport was created, but no
  listening test with the corrected transport selection has completed, so no codec/quality parity
  is claimed. The Mac ↔ Capsula AVRCP volume sync is unchanged; manual tooling must not alter the
  Mac's settings.
- **The earlier flatness was PulseAudio applying the AVRCP value twice.** With the default
  `avrcp_absolute_volume=1`, PA mirrored the transport volume onto the local bluez source; at 34 %
  the cubic source gain was 3.9 % (−28 dB), so the stream sounded thin. The decoupled module and
  the fixed 90 % source trim remove that second attenuation.
- **A2DP jitter needed buffer and IRQ tuning.** The first loopback target was 60 ms; the source
  buffer swung by hundreds of milliseconds, the adaptive resampler moved between 44.03 and 44.4 kHz,
  and short gaps appeared. With 250 ms the source latency settles at 57–111 ms (target 68 ms) and
  the loopback rate at ~44.10–44.13 kHz. Separately, `hci_uart` reported
  `Unexpected start frame` errors (2 in one 30 s window) while the Bluetooth UART IRQ shared CPU0
  with the daemon; the boot service now moves that IRQ to CPU3. Long captures are confounded by the
  source pausing, so sustained field listening is still the acceptance test.
- **PulseAudio Bluetooth modules are loaded by the daemon at startup.** If PulseAudio is restarted
  while the daemon runs, the A2DP endpoints disappear until the daemon restarts (or the modules are
  loaded by hand).
- **Unpairing is administrator-only and simulator-tested.** The Web UI and local CLI pass; the RPC
  rejects supervised app callers by run ID and sandbox UID. The live test deliberately did not
  unpair the user's MacBook, so real `Adapter1.RemoveDevice` remains a hardware gate.
- **Boot ownership is one removable hook, with one non-obvious ordering constraint.** rcS expands
  `/etc/init.d/S??*` **before** `S23overlayfs.sh` pivots into the `/data` overlay, so a script that
  exists only in the overlay is never in that list. The single deployed file is therefore the stock
  `S99btctl.sh` path, whose content is the complete capsulad service
  (`deploy/rockchip-stock/S99btctl.sh`): preparation, daemon start/stop, health gate, vendor script
  retirement/restoration and fallback. `S99btctl.sh disable` stops the daemon, restores the vendor
  init scripts, starts the vendor chain and moves the hook to
  `/data/capsdemo/boot/S99btctl.sh.disabled`; running that copy with `enable` reinstates it.
  `capsula daemon install` restarts `/etc/init.d/S99btctl.sh` by default. The previous unit
  deployment used `S60capsulad` plus an `exec` wrapper and was verified on 2026-11-12; the
  single-hook version is shell-checked/simulator-only for now. If the overlay is wiped (factory
  reset), the device returns to the stock assistant and the deployment must be repeated. The daemon
  ignores SIGHUP and is started with `nohup`; the watchdog handoff is unchanged.
- **Local credential changes are read at daemon start.** `capsula auth …` and the API-key CLI write
  the same security store the daemon loads at startup. A key created by the CLI is not accepted
  until the daemon restarts; keys created or revoked in the Web UI go through the running daemon and
  apply immediately. Restart the daemon after CLI credential changes.
- **Memory and persistent storage are the binding constraints**: 240 MiB total with no swap, and
  `/data` is only 78.5 MB (22,610,180-byte daemon plus a retained 22,479,000-byte `.prev` on the
  unit after 2026-11-12). The daemon defaults to an
  80 MiB soft heap limit (`-mem-limit-mb`). The kernel OOM-killed `capsulad` during the wakeword
  workflow before that limit. The assistant and media sample packages are no longer built, so the
  historical 27.6 MB app footprint does not apply to a current install; clock and ring are internal.
  The 2026-11-12 deployment removed this unit's retired clock, assistant and media bundles after a
  host backup (the ring bundle was never installed); other upgraded devices may still hold
  quarantined old bundles: upgrading the daemon suppresses their processes and listings but does not
  reclaim their file bytes. Remove those bundle directories explicitly to reclaim space; the daemon
  never auto-deletes installed apps or operator data.
  After the earlier boot/upgrade/media/Bluetooth deployment the unit had about 4.3 MB free: the
  24.4 MB warm corpus, its sidecar and the earlier recordings were moved to the host backup
  (`_capsula-mini-data-backup/`, git-excluded), and the negative-corpus path is configurable
  (`-wakeword-negatives`) because it is training input only. The log is capped at 512 KiB plus one
  rotated copy; new recordings and training are refused below 2 MiB free. After the 2026-11-12
  deployment `/data` reported 24.7 MB free (69 % used) with the `.prev` retained.
- **Web UI binds TCP**: set `-web-addr` explicitly and use firewall rules; the UI is not exposed on
  the Unix socket.
- **Audio master volume** is applied in software by the mixer; the PulseAudio sink is left at unity
  to avoid double attenuation. Vendor daemons bypassing the mixer do not follow it.
- **Mic watchdog timing** (5 s grace, 1 s poll, 2 s frame freshness) is a defensible default, not a
  hardware-tuned value. The grace is measured from the last qualifying indication.
- **Auto brightness** replicates the stock light handler: brightness % =
  `trunc(log(0.75 + 0.05·lux) × 19.9375)`, clamped to [5, 95], 0 below 5 lux and 100 above
  32768 lux, re-applied only when it moves by more than 5 points (the vendor's deadband), animated at
  0.06 per 100 ms tick. With no ambient sensor it falls back to the constant value and warns once.

## Next gates (in order)

1. ~~Boot ownership, in-place upgrades and bounded storage~~ **done and verified on the unit**: the
   daemon starts from the boot hook after two power cycles, `capsula daemon install` replaced the
   binary through the boot service with a health gate, the log is capped, and the corpus moved off
   `/data`. The 2026-11-12 redeploy re-verified the canonical restart and health gate without a
   power-cycle test. Remaining: exercise pairing and unpair with a throwaway device, and write the
   restore procedure for the host-backed corpus and recordings into operator docs.
2. ~~Media-player standard contract~~ **retained for external apps**: the routed unary/streaming
   plumbing remains, but the bundled `org.capsula.media` sample provider was removed. A Web UI
   player now requires an operator-installed provider.
3. Verify the native A2DP endpoint by ear with a playing source and compare it with the vendor
   stack; exercise `MediaTransport1.Acquire`, `sbcdec` and the mixer under dropouts. Do not change
   the source's volume/settings from tooling.
4. Implement QuickJS and MicroPython host runners with the same bus bindings, in isolated
   processes and read-only runtime images. The engine strategy is still an owner decision: the
   daemon is cgo-free, so a separate interpreter runner with a framed host bridge is the
   architecture-faithful option; a pure-Go engine would be a documented deviation from the runtime
   names in the manifest.
5. Test the Amlogic stock and Alpine targets on hardware; do not claim parity with Rockchip until
   each subsystem is probed. The backends still fail explicitly.
6. Evaluate the installed personal wakeword model over long real-room audio (false activations and
   misses beyond the 40-minute trial in WAKEWORD.md); it is a personal detector, not a
   speaker-independent Russian head.
7. Freeze v1 schemas only after generated Go/Rust SDK and script-host parity tests pass, including
   routed streaming and cancellation.

Gate 1's design decision stands: with no user, PID, network or IPC namespaces and no seccomp on the
stock kernel, the sandbox is mount-only. Either accept that and make provenance verification the
primary control for native apps, or investigate a vendor kernel rebuild — it must be stated, not
implied.
