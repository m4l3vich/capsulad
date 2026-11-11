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
| Bluetooth status and discoverability | implemented via `hciconfig` (`hci0`), verified On/Off flips PSCAN; pairing/profiles remain owned by the vendor stack | `hciconfig hci0` before/after the toggle |
| Display/ring brightness: constant or ambient-light | implemented with the **stock mapping** recovered from the light handler: `trunc(log(0.75+0.05*lux)*19.9375)` clamped to [5,95] %, off below 5 lux, full above 32768 lux, with the vendor's 5-point deadband and an animated transition. At 7.3 lux the framework writes 13/255; the stock unit wrote 14/255 at 7.4 lux | disassembly of 0x27ca68-0x27cc24, LED-class brightness cross-check, `TestAmbientBrightnessMatchesStockCurve` |
| Device timezone (Web UI) with per-app override | implemented, validated end to end | Web UI round-trip restarts boot apps; clock re-reads `TZ` (11:59 MSK ↔ 16:59 Asia/Shanghai) |
| Rockchip DT key map (volume, mic, play/pause, voice) | implemented | device-tree parser + tests |
| Rockchip ambient-light and thermal sensors | implemented | IIO/thermal sysfs tests |
| Web UI pages (all settings, apps, media, diagnostics) | implemented | login/API-key page + CSRF form tests |
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
- **Media player sample app**. The Web UI can consume the standard contract; no provider sample is
  bundled.

## Known design limits in this slice

- **Sandbox strength on Rockchip stock is mount-only.** No user, PID, network or IPC namespaces and no
  seccomp filter are available in kernel 4.4.167 as shipped, so an app cannot be contained beyond a
  read-only private mount namespace plus a distinct UID. Native apps are consequently trusted code:
  provenance verification is the control that matters, not runtime isolation.
- **No ABI compatibility rejection on the simulator** (host builds are accepted for development).
  On Rockchip, native packages must declare `aarch64` and a Linux ABI.
- **LED vendor ABI is recovered, not documented**: ioctl numbers, record layout and physical
  permutations come from the stock `voice-agent` binary for this firmware build. `GET_COUNT` is
  confirmed on a live unit (36 channels on `/dev/topleds`, 30 on `/dev/led_multi_ctrl`); the
  `MULTI_SET` frame path, segment-bit order, glyph faces and gamma are still unverified, and the two
  vendor processes hold both nodes open, so writes race the stock agent.
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
- **Memory and persistent storage are the binding constraints**: 240 MiB total with no swap, and
  `/data` is only 78.5 MB (21.9 MB daemon + 18.4 MB apps). The daemon defaults to an 80 MiB soft heap
  limit (`-mem-limit-mb`). The kernel OOM-killed `capsulad` during the wakeword workflow before that
  limit. The expanded cold negative corpus (21.7 MB) plus its uncompressed sidecar left 4,292 KiB
  free. The warm cutover installed a 24.4 MB gzip corpus, a 514 KB gzip sidecar and a 133 KB model;
  `/data` now has 3,788 KiB free.
- **The vendor stack is superseded at startup, runtime only**: `-supersede=auto` stops `sysmon` and
  `voice-agent` (they hold `/dev/input/event0` and `/dev/watchdog`), then any other watchdog holder,
  and arms the timer itself; a reaper stops respawns. Init scripts are untouched, so boot
  integration — starting capsulad *instead of* the vendor stack — is still outstanding. The daemon
  ignores SIGHUP and must be started with `nohup`; killing the vendor watchdog holder by hand resets
  the board after 21 s.
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

1. ~~Install and run on the RK3308 stock image~~ **done, and the vendor agent is superseded**: the
   daemon runs on the unit, probes six backends, launches `org.capsula.clock` in a mount-namespace
   sandbox with a distinct UID, and the clock takes `display.segment.front`. `voice-agent`, `btctl`
   and `tgclient` are stopped without a reset because `capsulad` takes over `/dev/watchdog` and hands
   the feed to a detached feeder on graceful stop. The binaries stay installed; only their autostart
   is retired. The LED ring/segment display, PulseAudio playback/capture through the mixer and the
   GPIO 21 microphone gate are verified on the unit; boot integration remains separate work.
2. ~~Calibrate the display channel order~~ **done**: the map is measured (`multiCtrlChannelMap`,
   digits a–g plus colon), the clock face renders correctly, and the timezone is a clock-app setting.
   Ring pixel order is verified; gamma on the WS2812B chain remains unmeasured.
3. Implement QuickJS and MicroPython host runners with the same bus bindings, in isolated
   processes and read-only runtime images.
4. Add a media-player sample app implementing `MediaPlayerService` and a routed streaming test.
5. Tune watchdog/ducking/volume behavior, then test the Amlogic stock and Alpine targets separately.
6. Freeze v1 schemas only after generated Go/Rust SDK and script-host parity tests pass.

Gate 1 depends on a design decision that the probe forced: with no user, PID, network or IPC
namespaces and no seccomp on the stock kernel, the sandbox is mount-only. Either accept that and make
provenance verification the primary control for native apps, or investigate a vendor kernel rebuild —
it must be stated, not implied.
