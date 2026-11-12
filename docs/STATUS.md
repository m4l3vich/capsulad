# Implementation status (initial slice)

Target schemas: draft v0.5 (`proto/capsula/framework/v1/framework.proto` and the media contract).
Generated Go bindings are committed under `gen/go`. The example ring schema and its generated
bindings were retired with the built-in ring provider. The wire API is **not frozen**.

> **Deviation from draft 0.5:** at the project owner's request, the Web UI no longer requires
> client-certificate mTLS. It uses a self-signed HTTPS certificate plus an Argon2id admin password
> and static opaque API keys. The HAL/app bus remains Unix-socket only; this changes only the
> browser-facing surface.

## Implemented and exercised

| Area | State | Evidence |
|---|---|---|
| All 15 framework gRPC services over one UDS | implemented | simulator run + tests |
| App supervision, manifests, restart/backoff, log capture | implemented | test apps run, crash test |
| Singleton launch invocations (queued/bounded/expiring) | implemented | delay-injection test |
| Reverse-RPC bus (`Connect`, routed unary/streaming `Call`) | implemented | routed-call test |
| Presentations (first-content readiness, in-place content) | implemented | routing tests + built-in clock provider |
| Built-in clock and ring providers (`capsula.clock`, `capsula.ring`) | implemented in the daemon, not as installed apps: clock keys `blink` (boolean, default true) and `timezone` (IANA name, empty follows the device); ring key `mode=ambient|spinner|rainbow` (default `ambient`). An empty default-provider selection keeps this baseline; legacy defaults `org.capsula.clock`, `org.example.ring-controller` and `org.capsula.assistant` migrate to the internal baseline at boot, and the old clock settings are copied to `capsula.clock` once. Previously installed legacy bundles (`org.capsula.clock`, `org.example.ring-controller`, `org.capsula.assistant`, `org.capsula.media`) are quarantined: not started, manually launched, selectable or listed, persisted defaults pointing at them are cleared, with one boot warning; they remain visible only in a manual `ls`. Their files remain on disk unless an operator removes them (the daemon never auto-deletes); operators may remove the bundle directories explicitly to reclaim space. Custom/third-party providers and generic interfaces are unaffected. The built-ins are internal (`capsula.builtin`), not installed apps. On the unit (2026-11-12 deployment) the retired clock, assistant and media bundles were removed after a host backup, the ring bundle was never installed, and the migrated app settings were preserved | internal/daemon builtin, upgrade-gap and settings-migration tests; live unit 2026-11-12 |
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
| Bluetooth audio routing and ring cue | implemented: connect loads `module-loopback` from `bluez_source.<addr>.a2dp_source` to `master` with a 250 ms buffer and disconnect unloads it; a blue ring pulse reuses the framework fallback presentation and restores it | live logs: route added/removed, loopback module present while connected; source latency settled at 72 ms against a 68 ms target with the loopback rate at ~44.1 kHz after the 60 → 250 ms change; cold boot re-registered endpoints and powered the adapter |
| Master volume and system cues | software mixer applies the master gain (measured 0 % → silence, 1 % → peak 58, 5 % → 294, 10 % → 589, 20 % → 1179). Bluetooth keeps the Mac's AVRCP volume and the Capsula master equal in both directions, but **does not trust AVRCP as the only attenuation**: the Mac can report a minimal transport volume while sending near full scale (observed: master 5 %, Mac minimal, audible ~90 %). The loopback therefore carries a local safety gain of master^1.5 (cube-root compensated because PA's sink-input volume is cubic; a linear master was still too hot at 5–7 % on this amplifier), and the source stays at a fixed 90 % trim. The local gain and source trim are re-asserted every 2 s so a PA reset cannot leave the path at full scale. The effective Bluetooth level is bounded by master^1.5. PA is decoupled (`avrcp_absolute_volume=0`, self-healed every 2 s because the discover module recreates it with defaults). The volume tick peaks at 0.22 | live capture, decoupling check (source 58 % left transport 114 unchanged), live module recreation caught and reloaded, loud-at-5 % report reproduced by the missing ceiling |
| Display/ring brightness: constant or ambient-light | implemented with the **stock mapping** recovered from the light handler: `trunc(log(0.75+0.05*lux)*19.9375)` clamped to [5,95] %, off below 5 lux, full above 32768 lux, with the vendor's 5-point deadband and an animated transition. At 7.3 lux the framework writes 13/255; the stock unit wrote 14/255 at 7.4 lux | disassembly of 0x27ca68-0x27cc24, LED-class brightness cross-check, `TestAmbientBrightnessMatchesStockCurve` |
| Device timezone (Web UI) with built-in clock override | implemented, validated end to end | Web UI round-trip restarts boot apps; the clock re-reads `TZ` (11:59 MSK ↔ 16:59 Asia/Shanghai) and `capsula.clock timezone` overrides the device zone |
| Rockchip DT key map (volume, mic, play/pause, voice) | implemented | device-tree parser + tests |
| Rockchip ambient-light and thermal sensors | implemented | IIO/thermal sysfs tests |
| Web UI pages (all settings, apps, media, diagnostics) | implemented | login/API-key page + CSRF form tests |
| Shared native SDK (`libcapsula`) | implemented as a pure Rust cdylib with frozen C ABI v1: bounded `capsula_get_api`/table copy, explicit statuses and buffer ownership, reflection JSON/protobuf for all framework unary/client/server/bidi calls, managed AppBus, routed and concurrent calls, cancellation/deadlines, raw-PCM audio, and runtime metadata with capability negotiation. Go/Rust bindings load it dynamically; C/C++ use the same header. Release builds strip debuginfo, debug builds do not. Integrated build, usage, lifetime and compatibility details are in the README’s Native SDK section | ABI/table sizing and lifetime tests; C/C++/Go/Rust consumer checks; physical device 2026-11-12: release cdylib + metadata installed in `/data/capsdemo/lib`, mapped at `/capsula/lib` under the mount namespace with `APP_READY` |
| Media-player standard contract | framework plumbing retained: unary commands and server-streaming `WatchPlayers` over the app bus; no sample provider is built or shipped in this repository | app-bus routed streaming tests |
| Optional dumb assistant (`org.capsula.dumbassistant`) | implemented as a separate optional nested Go project, built only by `make dumbassistant`: it claims `voice_assistant`, queues every activation, waits a constant or uniformly random configured delay, then streams the packaged 16-bit PCM WAV through the mixer inside a conversation while holding a full-ring teal indication and releasing it afterwards; it is optional and excluded from default builds, but on the unit it is installed and remains the selected default (2026-11-12) | `TestDumbAssistantDelayedStaticReply`; WAV parser, asset-path and delay unit tests; live activation on the unit 2026-11-12: teal ring → conversation → PCM_S16LE 44.1 kHz stereo playback → reply finished → ring release |
| Framework boot ownership on the stock image | deployed and verified across two power cycles before the built-in refactor: `/etc/init.d/S60capsulad` starts the daemon after mount, GPIO 21 and PulseAudio init; `S99btctl.sh` is the pivot-safe hook because rcS expands its script list before S23 pivots into the overlay; the vendor assistant init scripts are retired. That historical boot ran clock, assistant and media packages; current builds start with clock and ring internal to the daemon and no supervised sample apps, while legacy default IDs migrate to the internal baseline. The new build was not redeployed to the unit for this change; it was redeployed on 2026-11-12: canonical `S60capsulad` restart healthy, `capsulad -health` exit 0, `/dev/watchdog armed, timeout=21s`, boot scripts byte-identical, no reboot or power-cycle test run for this deployment | `boot.log` start/healthy lines, `capsulad -health`, three `APP_READY` lines at the historical deployment, `/dev/watchdog armed`, 2026-11-12 redeploy health/restart evidence |
| In-place daemon upgrade with rollback | implemented and exercised: executable check, staged rename beside the live binary, restart through the boot service, framework-API health gate, previous binary preserved; `capsula daemon rollback` is the inverse, but it restores the binary only: a full return to the old app-based clock/ring/assistant also needs the old bundles and manifests from the host backup, and the old daemon's strict manifest loader does not understand `[native_sdk]` | host simulator install+rollback drill, live install on the unit (21,954,712 → 21,889,176 bytes), 2026-11-12 live install to 22,610,180 bytes (sha256 `86d131ae…fe604`) with the 22,479,000-byte `.prev` retained, health after restart |
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
- **Boot ownership is deployed, with one non-obvious ordering constraint.** rcS expands
  `/etc/init.d/S??*` **before** `S23overlayfs.sh` pivots into the `/data` overlay, so a script that
  exists only in the overlay is never in that list. The framework is started from the stock
  `S99btctl.sh` (a vendor no-op) which `exec`s `/etc/init.d/S60capsulad start`; the vendor assistant
  scripts are whiteouted. `S60capsulad` remains the manual `start|stop|restart|status` entry point
  and is also what `capsula daemon install` restarts through. If the overlay is wiped (factory
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
