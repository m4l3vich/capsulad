# Implementation status (initial slice)

Target schemas: draft v0.5 (`proto/capsula/framework/v1/framework.proto`, media contract, example
ring contract). Generated Go bindings are committed under `gen/go`. The wire API is **not frozen**.

> **Deviation from draft 0.5:** at the project owner's request, the Web UI no longer requires
> client-certificate mTLS. It uses a self-signed HTTPS certificate plus an Argon2id admin password
> and static opaque API keys. The HAL/app bus remains Unix-socket only; this changes only the
> browser-facing surface.

## Implemented and exercised

| Area | State | Evidence |
|---|---|---|
| All 15 framework gRPC services over one UDS | implemented | simulator run + tests |
| App supervision, manifests, restart/backoff, log capture | implemented | sample apps run, crash test |
| Singleton launch invocations (queued/bounded/expiring) | implemented | delay-injection test |
| Reverse-RPC bus (`Connect`, routed unary/streaming `Call`) | implemented | routed-call test |
| Presentations (first-content readiness, in-place content) | implemented | routing tests + clock |
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
| Rockchip sandbox (root mount namespace, distinct app UID) | implemented, validated on hardware | `/proc/<pid>/status` UID 21556, private `mnt` namespace |
| Audio through the mixer (playback, volume, ducking, mic capture) | verified on hardware: a 1 kHz tone from an app measured on `master.monitor` gives RMS 4454 at 70 % volume and 1908 at 30 % (exactly 0.7 / 0.3 of the source's 6364, so software master volume is linear and exact); a conversation ducks it to 0.25 × (1113 / 477) with a ~150 ms fade; microphone capture arrives over the bus and the visible-indication watchdog terminates it 5 s after the last qualifying indication | scripted harness run on the unit |
| Talk button (circle key) + volume keys | implemented and exercised on the unit: hold 700 ms activates (manual origins do not require a trained keyword model), release ends the turn, short taps do nothing; volume taps ±5 %, held ramps accelerate 200→60 ms and stop at bounds, with a generated tick and a 1 s percentage overlay | `buttons5.log`: activation → conversation → capture → input released |
| Microphone key toggle + muted indication | implemented, verified on hardware | one mute per tap (`TestDiscreteKeysActOnPressOnly`), red run pixels 5–7 with fade-in/out |
| Web UI restructure (overview, settings, access, diagnostics) | implemented and exercised over HTTPS on the unit: all four pages render, volume/settings round-trip, API-key create+revoke works, certificate downloads as PEM | live checks against the daemon |
| Bluetooth adapter control (BlueZ D-Bus) | implemented: power, discoverable, pairable (persisted as `bluetooth_pairable`), paired-device list and unpair. The daemon loads PulseAudio's Bluetooth discovery so the adapter advertises A2DP Sink (0x110B) and Headset (0x1108); the old `hciconfig piscan` error was an adapter nobody powered, and the framework now powers it at startup and after a BlueZ restart | live unit: `hciconfig` UP RUNNING, `PSCAN ISCAN` after the Web UI toggle, BlueZ Pairable toggled/persisted, paired MacBook listed |
| Bluetooth media through the app contract | implemented: AVRCP play/pause/next/previous and absolute volume to the source, metadata when the source exposes it; the media app publishes a `bluetooth` player only while a source is connected | live MacBook: Play/Next accepted and playback state changed; 50 % master volume set transport `Volume=64` of 127; UI showed `PLAYBACK_PLAYING`; `TestMediaProviderRoutedStreaming` |
| Bluetooth audio routing and ring cue | implemented: connect loads `module-loopback` from `bluez_source.<addr>.a2dp_source` to `master` with a 250 ms buffer and disconnect unloads it; a blue ring pulse reuses the framework fallback presentation and restores it | live logs: route added/removed, loopback module present while connected; source latency settled at 72 ms against a 68 ms target with the loopback rate at ~44.1 kHz after the 60 → 250 ms change; cold boot re-registered endpoints and powered the adapter |
| Master volume and system cues | software mixer applies the master gain (measured 0 % → silence, 1 % → peak 58, 5 % → 294, 10 % → 589, 20 % → 1179). Bluetooth keeps the Mac's AVRCP volume and the Capsula master equal in both directions, but **does not trust AVRCP as the only attenuation**: the Mac can report a minimal transport volume while sending near full scale (observed: master 5 %, Mac minimal, audible ~90 %). The loopback therefore carries a local safety gain of master^1.5 (cube-root compensated because PA's sink-input volume is cubic; a linear master was still too hot at 5–7 % on this amplifier), and the source stays at a fixed 90 % trim. The local gain and source trim are re-asserted every 2 s so a PA reset cannot leave the path at full scale. The effective Bluetooth level is bounded by master^1.5. PA is decoupled (`avrcp_absolute_volume=0`, self-healed every 2 s because the discover module recreates it with defaults). The volume tick peaks at 0.22 | live capture, decoupling check (source 58 % left transport 114 unchanged), live module recreation caught and reloaded, loud-at-5 % report reproduced by the missing ceiling |
| Display/ring brightness: constant or ambient-light | implemented with the **stock mapping** recovered from the light handler: `trunc(log(0.75+0.05*lux)*19.9375)` clamped to [5,95] %, off below 5 lux, full above 32768 lux, with the vendor's 5-point deadband and an animated transition. At 7.3 lux the framework writes 13/255; the stock unit wrote 14/255 at 7.4 lux | disassembly of 0x27ca68-0x27cc24, LED-class brightness cross-check, `TestAmbientBrightnessMatchesStockCurve` |
| Device timezone (Web UI) with per-app override | implemented, validated end to end | Web UI round-trip restarts boot apps; clock re-reads `TZ` (11:59 MSK ↔ 16:59 Asia/Shanghai) |
| Rockchip DT key map (volume, mic, play/pause, voice) | implemented | device-tree parser + tests |
| Rockchip ambient-light and thermal sensors | implemented | IIO/thermal sysfs tests |
| Web UI pages (all settings, apps, media, diagnostics) | implemented | login/API-key page + CSRF form tests |
| Media player sample provider (`org.capsula.media`) | implemented and exercised on the unit: the app advertises `capsula.contracts.media.v1.MediaPlayerService`, the Web UI discovers its one player, unary Play/Pause round-trip through the app bus, and `WatchPlayers` streams a snapshot first and updates on every command; the mixer logged stream `pbs_…` opening and closing | `TestMediaProviderRoutedStreaming`; live UI play/pause over HTTPS |
| Framework boot ownership on the stock image | deployed and verified across two power cycles: `/etc/init.d/S60capsulad` starts the daemon after mount, GPIO 21 and PulseAudio init; `S99btctl.sh` is the pivot-safe hook because rcS expands its script list before S23 pivots into the overlay; the vendor assistant init scripts are retired | `boot.log` start/healthy lines, `capsulad -health`, three apps `APP_READY` after reboot, `/dev/watchdog armed` |
| In-place daemon upgrade with rollback | implemented and exercised: executable check, staged rename beside the live binary, restart through the boot service, framework-API health gate, previous binary preserved; `capsula daemon rollback` is the inverse | host simulator install+rollback drill, live install on the unit (21,954,712 → 21,889,176 bytes), health after restart |
| Bounded logs and storage guard | implemented: 512 KiB log file plus one rotated copy, configurable `-wakeword-negatives`, and a 2 MiB free-space floor for new takes and training | `internal/logbus` rotation tests; deployed `/data` at 93 % |
| Personal wakeword embedding engine and training | implemented: incremental Google/openWakeWord backbone, ARM64 NEON, shared-projection head, on-device training and atomic replacement; strict warm `CNEG0002` corpus with real same-split preroll; legacy models and cold corpora refused | ONNX/kernel parity, gradient checks, real Mini frontend 37.9 ms/output, production loader accepts the 872-stream gzip corpus; `word.personal` installed and selected after full native training (670.6 s; on-device A35 replay 6/6 words, 9/9 backgrounds) and a one-shot 237-clip fresh negative set with zero false activations; personal trial, not field certified; see [WAKEWORD.md](WAKEWORD.md) |

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
  On Rockchip, native packages must declare `aarch64` and a Linux ABI.
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
  `SetConfiguration` is retained and UUID/Codec is the fallback. RTP is depacketized and decoded by
  the system `sbcdec` (the same libsbc PulseAudio used), then fed to the mixer at the stream rate.
  PA still owns app playback and the microphones. End-to-end listening is the remaining gate: the
  endpoint reached CONFIGURED/OPEN and a transport was created in hardware testing, but the final
  `Acquire` + decode path has not been exercised with a playing source, and no codec/quality parity
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
  `/data` is only 78.5 MB (22.3 MB daemon, 22.3 MB previous daemon, 27.6 MB apps). The daemon
  defaults to an 80 MiB soft heap limit (`-mem-limit-mb`). The kernel OOM-killed `capsulad` during
  the wakeword workflow before that limit. After the boot/upgrade/media/Bluetooth deployment the unit has
  about 4.3 MB free: the 24.4 MB warm corpus, its sidecar and the earlier recordings were moved to
  the host backup (`_capsula-mini-data-backup/`, git-excluded), and the negative-corpus path is now
  configurable (`-wakeword-negatives`) because it is training input only. The log is capped at
  512 KiB plus one rotated copy; new recordings and training are refused below 2 MiB free.
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
   `/data`. Remaining: exercise pairing and unpair with a throwaway device, and write the restore
   procedure for the host-backed corpus and recordings into operator docs.
2. ~~Media-player standard contract~~ **done**: `org.capsula.media` implements the provider side,
   the routed streaming test covers snapshot-then-update, and the Web UI played and paused it on the
   unit. Follow-ups are cosmetic (render `PLAYING` as "Playing") and belong to the UI track.
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
