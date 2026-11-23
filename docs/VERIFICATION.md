# Verification

This page records what is verified, at which proof level, and what remains open. It is a
current capability record, not a change log: a behavior change updates the affected row at the
proof level actually observed. A passing test, simulator frame or backend call is not physical
output.

## Proof levels

| Level | Meaning | Proves | Does not prove |
|---|---|---|---|
| Internal state | Daemon-owned state, persisted values, supervisor records | Ownership, routing decisions, CAS results | That a backend or device acted |
| Backend submission | A write was formed and submitted to the backend | Encoding, channel mapping, frame content | That the hardware emitted anything |
| Digital output | The backend's actual output stream or captured samples | Timing, repetition, gain, channel content | Electrical or acoustic behavior |
| Electrical | Measured voltages, currents, bus signals | Driver and gate behavior | Sound or light as perceived |
| Acoustic / optical | Microphone or photometer measurement of the real device | Audible level, emitted light, gamma | Nothing beyond that measurement |

A gRPC send is asynchronous and is never an output acknowledgement. "The stream opened" is not
proof of sound; "the frame was written" is not proof of light.

## Capability matrix

The strongest evidence achieved per area. "Device" means a Capsula Mini rev 4 (RK3308 stock
image) was exercised; host-only and image-only evidence never closes a device gate.

| Area | Strongest evidence | Current limit | Authoritative detail |
|---|---|---|---|
| Platform, sandbox, boot | Device: probe, boot, health gate and in-place upgrade | Power-cycle and overlay-wipe recovery open | [HARDWARE.md](HARDWARE.md#sandbox-constraints), [HARDWARE.md](HARDWARE.md#boot-service) |
| Hardware watchdog | Device: driver probe; host: supervisor decisions | A real stall- or crash-triggered reset is unobserved | [HARDWARE.md](HARDWARE.md#hardware-watchdog-ownership) |
| Segment display and LED ring | Device: digital frames; optical for "88:88" and ring orientation | Photometry, gamma and panel failure unmeasured | [HARDWARE.md](HARDWARE.md#displays-and-led-ring) |
| Ambient-light brightness | Device: stock curve and LED-class readback | Dark-room 5 % floor not photometered | [HARDWARE.md](HARDWARE.md#ambient-light-brightness) |
| Panel input and gestures | Device: evdev shape and centre→edge delivery | Full-capture circles, hold summon and full-panel crossing unconfirmed with the deployed resolver | [HARDWARE.md](HARDWARE.md#touch-and-keys), [SPEAKER_BEHAVIOR.md](SPEAKER_BEHAVIOR.md#interaction-contract) |
| Microphone gate and mute | Device: electrical GPIO state plus digital capture | Acoustic isolation beyond the measured loop unmeasured | [HARDWARE.md](HARDWARE.md#microphone-gate-and-mute) |
| Audio routing and mixer | Device: digital output (`master.monitor`) | Acoustic level, the 60 dB taper and room response unmeasured | [HARDWARE.md](HARDWARE.md#audio), [SPEAKER_BEHAVIOR.md](SPEAKER_BEHAVIOR.md#interaction-contract) |
| Bluetooth, A2DP and AVRCP | Device: D-Bus state and one real source | Conformant SBC SEP acceptance, sustained listening, pre-daemon incoming connection, real AVRCP PDUs | [HARDWARE.md](HARDWARE.md#bluetooth) |
| mDNS / Avahi | Device: read-only status; host: responder fixture | Real write-time collision and post-restart resolution on the device | [HARDWARE.md](HARDWARE.md#mdns-and-avahi) |
| Web UI and HTTP API | Device: TLS/SPA/CA download/plaintext guidance; host: EN/RU browser switch and locale formatting | Real client CA import and physical output | [SPEAKER_BEHAVIOR.md](SPEAKER_BEHAVIOR.md#interaction-contract) |
| Wi-Fi control (CLI/API) | Device: station status through the new local HTTPS admin API; host: route, scope and certificate-pinning tests | Real association, setup AP, DHCP and portal behaviour | [SPEAKER_BEHAVIOR.md](SPEAKER_BEHAVIOR.md#interaction-contract) |
| MCP frontend | Host only | No third-party client, no device run, no maximum-size staged upload | [MCP.md](MCP.md#proof-level-and-limits) |
| Script hosts | Device: QuickJS run; MicroPython reached `APP_READY` | What a MicroPython app does after readiness on the unit | [app_development/manifest.md](app_development/manifest.md) |
| Personal wakewords | Device: native training and replay | Per-word acoustic acceptance, daily false-alarm rate, multi-word cadence on the RK3308 | [WAKEWORD.md](WAKEWORD.md#numerical-and-device-evidence) |
| Apps, packaging and deployment | Device: package install/replace, runs reaching `APP_READY` | Radio playback, real MQTT broker, install-while-stopped | [app_development/packaging-deployment.md](app_development/packaging-deployment.md) |
| AirPlay 2 receiver | Host only | No real sender, no acoustic output, no device run | [apps/airplay/README.md](../apps/airplay/README.md) |
| Networking SDK | Host only | Device DNS, trust store, LAN listeners and mDNS visibility | [app_development/network.md](app_development/network.md) |
| Amlogic A113X | Image and host only | No Amlogic unit probed | [HARDWARE.md](HARDWARE.md#amlogic-a113x-stock-buildroot-and-alpine-linux) |

## Verified on Capsula Mini rev 4

The unit is a Capsula Mini rev 4: RK3308, aarch64, glibc 2.33, kernel 4.4.167, running the
jailbroken stock Rockchip image. Generic device observations and their proof levels live in
[HARDWARE.md](HARDWARE.md); this section keeps only the integrated outcomes.

### Platform, sandbox and boot

- The daemon starts from the single boot hook `/etc/init.d/S99btctl.sh`, including after `/data`
  overlay setup; the hook's health gate returns 0.
- In-place upgrade replaces the daemon by rename, restarts through the boot service and passes the
  health gate. A replaced binary exists only as a transient `.prev` during that window: it restores
  the same bytes if the restart or gate fails and is deleted once the deployment is accepted.
- Sandbox probe on the live unit: `/proc/self/ns` exposes only `mnt`; `unshare(CLONE_NEWUSER |
  CLONE_NEWNET)` returns `EINVAL`; `PR_SET_SECCOMP` returns `EINVAL`. The daemon reports
  `mode=mountns`.
- A native app under that sandbox ran as a distinct UID in a private mount namespace, mapped
  `/capsulad/lib` with ABI 1.0, reached `APP_READY`, and exited cleanly on stop.
- Replacing the optional assistant and native runtime while the daemon was stopped through the boot
  service preserved the settings revision and user choices; the expected runs reported
  `APP_READY`.

### Display, ring and brightness

- Channel counts and both channel maps match the device tree and render correctly on hardware: the
  clock face is correct, ring pixel 0 is at the top, travel is clockwise, and RGB order is correct.
  All 30 segment channels lit produce "88:88" (optical). The maps are in
  [HARDWARE.md](HARDWARE.md#displays-and-led-ring).
- The clock wrote a real segment frame; the ring's active window outside the current local time
  produced its explicit all-zero baseline frame, and a bounded window opened and closed the
  animation on the target binary (digital frames, not photometry).
- The stock ambient-light curve and the framework's 5 % auto floor were confirmed by LED-class
  brightness readback within one LSB; emitted light and gamma were not measured.

### Panel input

- The Mini's only evdev node is `gpio-keys`; all five keys produce events on hardware with no
  coordinate capability. Raw `/dev/input/event0` captures show one press + SYN + release + SYN per
  full-panel swipe; the key and edge mapping is in [HARDWARE.md](HARDWARE.md#touch-and-keys).
- The deployed resolver delivered a centre tap followed by a volume edge 79 ms later as
  `swipe_right` (`tier=centre`, `press_gap=79ms`, `window=700ms`) and sent
  `BLUETOOTH_MEDIA_NEXT`; a 114 ms centre→`volume_down` pair sent `BLUETOOTH_MEDIA_PREVIOUS`.
  Both volume releases were consumed by the gestures. Plain volume taps resolved as `tier=keys`
  on the press edge with no deferral, and opposite volume edges 1.5–3.3 s apart stayed steps.
- The pad reports one key at a time, so a centre and an edge never overlap; the released-centre
  window is the usable form. Recognition of a full-panel crossing, full-capture circles and the
  700 ms hold summon remains open; see [Open gates](#open-gates).

### Microphone gate and privacy indication

- GPIO 21 and the PulseAudio source mute follow the setting, and the gate cuts analog audio
  (electrical gate plus digital capture; measured levels are in
  [HARDWARE.md](HARDWARE.md#microphone-gate-and-mute)).
- Microphone capture requires both a selected visible non-blank presentation and an audible cue
  the mixer consumed; the visible-indication watchdog ended a capture 5 s after the last
  qualifying indication in a scripted hardware run.
- Continuous withholding and per-chunk re-evaluation of both indication halves are covered by unit
  tests on the host, not by a separate physical run.

### Audio mixer and volume

- A 1 kHz tone from an app reached `master.monitor`; a conversation ducked it to 0.25× with about a
  150 ms fade (digital output).
- The canonical master taper is host-pinned: exact silence at 0 and unity at 1, 5 % → −57 dB,
  50 % → −30 dB, equal dB steps per position step; full-scale S16 and hot F32 through the real mix
  loop stay at or below the canonical gain, and the limiter holds the bus below 0.95 FS. These are
  digital-output measurements; perceived loudness is unmeasured.
- The PulseAudio sink pin and the fallback's cube-root compensation are host-tested; the real sink
  state and per-stream sink-input volume on the unit remain open (see [Open gates](#open-gates)).

### Speaker output and interaction

Observed on the rev 4 hardware; these are integrated device outcomes, not host or simulator runs.

| Scenario | Device-observed outcome |
|---|---|
| Countdown | Driver frames carried 12 → 1 orange ring pixels in the contract's extinguishing order. |
| Completion and repetition | Segment masks `[63 63 63 63]` (00:00); the alert stayed active past ten seconds and opened eight clock playback streams in the trace. |
| Jingle digital output | `master.monitor` carried three repeated three-beep groups; the half second after dismissal was all-zero PCM. Mixer output, not acoustic loudness. |
| Physical dismissal | A one-second circle-key press dismissed the alert, stopped clock audio and did not summon the assistant. |
| Cancellation and stopwatch | Cancelling a countdown released the ring; stopwatch start/pause/resume/reset preserved the expected elapsed-time transitions. |
| Assistant lifecycle | Summon and hold-to-talk used the real play/pause-key path; the summon opened real microphone capture, and completed turns left no assistant audio streams or ring ownership. |
| Volume | A physical tap raised the level once and did not repeat after release; the original level was restored. |
| Brightness submission | Ring-driver writes carried global levels 0.25 and 0.8 during mute indication; the original level was restored. |
| Final output | A no-owner ring write contained twelve all-zero RGB pixels, not merely a routing-release message. |
| Restart and restoration | A normal service restart retained provider selections and settings, the assistant ready, and no active app audio; the kernel boot ID was unchanged. |

### Bluetooth

- Adapter power, incoming-connections and discoverable policy operate over BlueZ D-Bus plus the
  Linux management socket on the live adapter; the policy settings persist.
- A real laptop source accepted Play and Next and changed playback state; a speaker-side master
  change wrote the transport's absolute-volume register. A source that has not reported absolute
  volume yet is reported as such (`ErrNoAbsoluteVolume`, one Warn per session) instead of the write
  silently failing.
- The native A2DP SBC sink endpoint was registered and reported ready at daemon start, and the
  registration is verified before the adapter is made connectable. A hardware A2DP transport was
  created, but acceptance of the current conformant four-octet SBC capability IE by a peer is not
  proven.
- Connect plays three completed blue fade-in/out ring pulses and shows lowercase `bt` on the two
  rightmost segment cells for 2 s; disconnect plays exactly one pulse and no glyph, over the
  selected baseline. The connection cue has simulator digital-output evidence but no unit listening
  check.
- With the PulseAudio fallback, source latency settled at **57–111 ms** (target 68 ms) and the
  loopback rate at **44.10–44.13 kHz**. Moving the Bluetooth UART IRQ to CPU3 removed the
  `hci_uart` `Unexpected start frame` errors observed while it shared CPU0 with the daemon and
  wakeword inference.

### Discovery and Web UI

- Avahi on the unit reported `RUNNING`, `GetHostName=capsula` and
  `GetHostNameFqdn=capsula.local`; `avahi-resolve capsula.local` resolved to the unit, and startup
  logged the advertisement with no redundant-operation warning.
- The unit's Web UI is reachable over the LAN: `/healthz` 200, `/` serves the embedded SPA, served
  asset hashes equal the local build, and `/api/status` returns 401 without a session.
- The daemon and CLI were replaced in place over ADB; the boot hook restarted the stack, the
  health gate returned 0, and the served SPA asset contains both the English (United Kingdom) and
  Russian catalogues. The production SPA uses the runtime-only vue-i18n build with JSON
  catalogues precompiled at build time; the login page and the EN→RU switch render under the
  daemon's `script-src 'self'` CSP with no console or `unsafe-eval` violation. `npm run build`
  fails if the bundle contains `new Function` or `eval`. A plain-HTTP request to the HTTPS
  listener returns the guidance page (sign in, then download the CA from Access) instead of
  `net/http`'s bare TLS error.
- `capsulad-cli -json wifi status` on the unit returned `state=connected`, the station SSID and
  IP through the new local HTTPS admin API. The CLI minted a temporary operator key, retried the
  daemon's credential reload, and revoked it; a status-scoped key fetched `/api/ca.pem` with
  `Content-Disposition: attachment; filename="capsulad-ca.crt"` and the same self-signed
  certificate used for TLS.
- A browser run against a real simulator daemon observed the host-name default, normalized save
  (`Living-Room` → `living-room`), persistence across a restart, invalid-label rejection with the
  prior value retained, the simulator's unsupported-mDNS status with no false advertisement, and
  the **Restart** label on Clock, Ring and Diagnostics.
- A production-UI browser run against a successful responder fixture made zero `SetHostName` calls
  for startup, GET and a same-name save, one for a real change; a collision stayed visible with no
  advertised claim and restoring the label recovered. This is fixture-backed browser evidence, not
  device proof.

### Script hosts, wakewords and resources

- `ListRuntimes` on the unit returned `quickjs-ng 0.18.0` and `micropython 1.29.0-preview` with
  shared ABI 1.0 from `/data/capsdemo/lib`. A QuickJS package ran `APP_READY` under `mountns`,
  answered an invocation, and exited 143 on Stop with no orphan host. MicroPython apps reached
  `APP_READY`; their behavior after readiness remains a gate.
- Native wakeword training completed on the device and a device A35 replay of the installed artifact
  passed the runtime two-score and cooldown policy. That is installation evidence, not an accuracy
  benchmark; the measurements are in [WAKEWORD.md](WAKEWORD.md#numerical-and-device-evidence).
- A sampled idle state reported 72 % CPU idle, daemon RSS 77 MiB, assistant 8.9 MiB and 17.8 MiB
  free on `/data`. The daemon log is capped at 500 KiB with older logs rotating to
  `/tmp/capsulad-logs` up to 2 MiB; wakeword recording and training refuse below 2 MiB free.

## Verified only on the simulator or host

No line here is a device result. Host tests exercise the real framework code, fake backends,
loopback transports and fixtures; they prove decisions and digital content, not Linux sandboxing,
physical timing, brightness/gamma, recognition accuracy or peer-device behavior.

- The complete virtual backend covers segment display, RGB ring, contacts, keys and audio. The Go
  suites run with `-race` for the root module and the nested modules; the harness pinning and
  focused entry points are in [DEVELOPMENT.md](DEVELOPMENT.md#tests).
- The terminal installer (`installer/`, a separate host module) has host tests for the release
  manifest/fetch/hash path, ADB/SSH transports, streamed backup validation, deploy ordering,
  credentials-before-launch, identity continuity, secret stdin handling and terminal-text
  sanitisation. A read-only run against the unit detected the platform, streamed and validated a
  1.8 MiB / 256-entry vendor backup, and reported the stock data-partition shortfall honestly. A
  complete install through the TUI has not been performed; the daemon/CLI on the unit were
  deployed with `capsulad-cli deploy`. The unit's trimmed stock BusyBox has no `id`/`whoami`;
  the staging root check therefore reads the effective uid from `/proc/self/status` and was
  exercised under an Alpine 3.24 BusyBox 1.37 container with `id` removed from `PATH`.
- A browser run against a simulator daemon verified the EN/RU language switch, locale-sensitive
  dates and numbers, and the Access page's certificate and CA download controls. The CDP harness
  cancels browser downloads, so the download endpoint, filename header and button were verified
  rather than a saved client-side file.
- Audio, input, routing, settings, Bluetooth, discovery, wakeword, both assistant modules and the
  daemon integration scenarios are race-covered. They pin timer persistence, countdown geometry and
  preemption, stopwatch arbitration, backend blanking, static-frame brightness, visible-indication
  rejection, cancelled versus finite playback, held input and activation IDs, assistant dismissal,
  restart after a lost framework connection, endpointer regressions, mute/OSD generations, timezone
  preservation and Bluetooth overflow recovery.
- The mixer contract is pinned: full-scale S16 and hot `PCM_F32LE` stay at or below the master, one
  app's ceiling survives extra streams, its fifth stream is `RESOURCE_EXHAUSTED`, the limiter holds
  the bus below 0.95 FS, the sink pin runs against a scripted `pactl`, and an architecture test
  rejects `WritePCM(` outside `mixLoop`.
- Bluetooth coverage includes the four-octet SBC capability IE, the pure object-tree session
  assertion with one profile request per connection, the two-minute request window, the
  acquired-transport volume target, `ErrNoAbsoluteVolume` once per session, toggle direction from a
  stale `MediaPlayer1.Status` and from the two-second command memory, the centre-gated and 400 ms
  full-panel gestures, the framework Bluetooth provider with and without a playing source, and the
  echo-suppressed display mirror.
- Multi-word detection is covered at the model store (import, per-word enablement and binding), the
  ensemble (single-model score parity, deterministic arbitration, one global cooldown) and delivery
  (a bound app, no rerouting when it is unready).
- A word compiled into the daemon is seeded byte-identically into a fresh model store, advertised
  live and unbound, refused when the store already exists, suppressed by an explicit `wakewords`
  list or a legacy artifact, and not resurrected after deletion and restart. These are host tests;
  no device run of the bundled artifact has been recorded.
- Live package install, replace and remove through the daemon, and the browser Apps page, are
  exercised on the simulator/host, including the running-app refusal and the stopped-daemon
  on-disk fallback. Device installation is recorded above with its own proof level.
- The frontend node tests, Vite production build, embedded daemon build, Linux arm64 crossbuild,
  Web UI race tests and wakeword history/cancellation tests pass. A native browser through a
  loopback HTTP→TLS proxy exercised the 390 px and 849 px surfaces (login, live settings with
  unsaved-edit preservation, timezone search, app settings document, API keys, password change,
  certificate download, diagnostics pause/resume).
- The shipped app packages have their own host suites via `make test-apps`: the Simon contract
  suite drives the real `main.js` in a fake QuickJS host, the PA suite exercises the real
  `main.py` WAV parser and protobuf helpers under CPython, and the Internet Radio and Home
  Assistant bridge suites are nested Go modules. App-package installation and runs on the unit are
  recorded separately above with their own proof level.
- The native SDK's transport, HTTP, remote gRPC, stream, UDP, listener, mDNS and primitive paths
  are covered on the host, including TLS verification against an embedded test CA and loopback
  sockets. Device DNS, trust-store contents and LAN reachability are unverified.
- AirPlay 2 host coverage includes protocol and crypto vectors, bit-exact ALAC, RTP decryption,
  session lifecycle, AAC-LC through FFmpeg and an independent pyatv loopback flow. No real sender
  or device was used; see [apps/airplay/README.md](../apps/airplay/README.md).
- MCP host coverage exercises the real HTTPS endpoint, scopes, protocol errors, staged uploads and
  session teardown. See [MCP.md](MCP.md#proof-level-and-limits).
- Both Amlogic platforms have profile, capability, frame-encoding and panel-probe coverage plus a
  Linux arm64 cross-compile. No Amlogic unit was probed; the stock clock-panel vendor ioctl is
  unrecovered and reported `UNVERIFIED`. See
  [HARDWARE.md](HARDWARE.md#amlogic-a113x-stock-buildroot-and-alpine-linux).

## Environmental limits and unavailable paths

- **Namespaces and seccomp.** The stock kernel offers mount namespaces only. Sandboxing is a
  read-only mount namespace plus a distinct UID; native apps are trusted code.
- **Raw touch coordinates.** There is no coordinate panel: `coordinates_available=false` and
  `full_raw_available=false`. Full-panel delivery remains available.
- **Amlogic bring-up.** Both Amlogic A113X platforms are implemented, but nothing on that board was
  probed. The stock clock panel's vendor ABI is unrecovered, so it is reported `UNVERIFIED`; no
  parity with the RK3308 is claimed.
- **Vendor system-service control.** `SystemService.ControlUnit` returns `UNIMPLEMENTED`.
- **Field-certified wakewords.** Every installed word is trained from its own recordings and runs
  on the processed tap; no speaker-independent keyword head ships, and each threshold is unverified
  for a new room.
- **Wi-Fi RF and provisioning.** Physical association, stock AP-mode scanning, DHCP/DNS/firewall
  behavior and captive-portal detection are unproven; the target firmware has no `hostapd`/`iw`.
- **A2DP listening parity.** The native endpoint registered and created a transport, but no
  sustained listening test with the conformant four-octet SBC SEP has completed, and no real AVRCP
  session has been recorded.
- **Photometry and acoustics.** No photometer or calibrated acoustic measurement exists.
- **Unpairing a user's source.** The path is exercised on the simulator and rejects supervised
  callers; a paired user source was deliberately not removed.
- **Resources.** 239,840 kB RAM with no swap and a 78.5 MiB `/data` partition bound daemon heap,
  logs and training; `/data` cannot hold two 40 MiB daemons, which is why a deployment keeps only
  one transient replacement.
- **Host test environment.** Host daemon integration tests need a matching host native runtime and
  metadata (`build/lib` or `CAPSULAD_NATIVE_LIB_DIR`); a target-platform library is not a usable
  host test runtime. Root Go tests do not cover nested modules. A failure that reproduces on
  pristine HEAD is environmental or pre-existing: record that comparison instead of treating it as
  a regression.

## Open gates

Each gate needs evidence beyond a host-only pass; a simulator or unit-test result never closes a
device, acoustic or optical gate.

1. **Optical output and brightness.** Measure emitted light and gamma for the ring and segment
   display, including the 5 % auto-brightness floor, and compare the standby baseline with the
   vendor firmware.
2. **Panel resolver on the unit.** With physical presses, confirm a bare volume tap steps exactly
   once with no added latency; the centre gate both held and released-then-edge; the
   opposite→centre→direction chain after its leading edge has acted; the ≤ 400 ms full-panel
   crossing resolving once with its completing edge suppressed and leading step reverted; a slow
   opposite pair staying two steps; a lone end-stop key staying a step; the 700 ms hold summon;
   full-capture-only circles; and that an unfinished sequence produces no further effect. Windows
   must follow the kernel timestamp even when the read loop is delayed. The centre→edge form is
   device-confirmed; the rest is not.
3. **Watchdog behavior on the unit.** `kill -STOP` the daemon and confirm the board resets about
   19–21 s later with `dw_wdt: keepalive missed, machine will reset`; with a throwaway build that
   wedges the output loop, confirm the health gate causes a reset after `no core progress for 10s`;
   confirm `S99btctl.sh stop` keeps the board up inside the 5 min feeder window and `start`
   reclaims the timer without `EBUSY`; and confirm `disable` returns `/dev/watchdog` to the vendor
   chain. Record the driver-reported timeout and re-check the profile's `nowayout` facts while
   there.
4. **Power cycle and boot fallback.** Exercise a cold power cycle with the single boot hook,
   including `disable`/`enable`, the vendor fallback, recovery from an overlay wipe, and the
   per-application boot preference. Decide the boundary for vendor helpers that keep running
   (`okcall`, `stereoctl`, `telemetryd`, `snapclient`).
5. **Bluetooth A2DP and AVRCP.** Confirm the conformant four-octet SBC SEP is accepted; listen to
   the native path under real jitter; verify the first incoming connection after BlueZ autopowers
   the adapter and whether a peer that abandoned A2DP recovers on the same link after the one
   profile request; confirm PA-module removal timing around an active source; and record real
   AVRCP PDUs (`btmon`) for play/pause, `SetAbsoluteVolume` and next/previous. Confirm the
   connection cue once on the unit at an audible level.
6. **mDNS on the device.** Save a label, confirm `.local` resolution, restart Avahi through the
   device init system and confirm re-adoption; verify a real write-time collision keeps the prior
   name with a visible error, an invalid label is rejected, and a Settings GET plus restart produces
   zero `SetHostName` calls.
7. **Acoustic assistant and wakeword behavior.** Evaluate the endpointer and each wakeword against
   real room noise, loud music and quiet speech over long durations; measure the multi-word
   ensemble (one frontend plus N heads at the 80 ms hop), the ~two-second all-word warm-up, and
   each word's acceptance at its own threshold. No window-accuracy or short-run result is a daily
   false-alarm rate.
8. **MicroPython on the unit.** Run a MicroPython package and observe its actual audio, display and
   network behavior after `APP_READY`; readiness alone is not enough.
9. **Wi-Fi and the setup AP.** Exercise a real association, the setup AP, DHCP/portal detection,
   AP teardown and a failed join; verify stock AP-mode scanning or prove its absence.
10. **Networking on the device.** From inside the app sandbox, dial a public host by name and
    verify the trust store, dial with a private CA, observe name-mismatch and missing-CA failures,
    confirm the sandbox resolver, allocate a LAN-reachable TCP/UDP listener (including the 5353 and
    Web UI port refusals and the 16-listener cap), and publish/browse an mDNS instance visible to a
    second LAN browser.
11. **Amlogic A113X bring-up.** Run `-platform amlogic-stock` and `-platform amlogic-alpine` on a
    unit; record the ring channel count and one rendered frame, which clock panel is fitted
    (IS31FL3236A or AW21036) and which panel interface exists, the real sink/source and mixer
    names, capture latency, the stock ambient-brightness response, whether `/dev/watchdog` can be
    disarmed, and whether the stock init list starts an overlay-only hook.
12. **Acoustic loudness and cost.** Listen at 5 / 10 / 30 / 60 / 100 % master, including cues,
    assistant replies and overlapping apps, and confirm the per-app ceiling audibly; measure the
    aarch64 cost of the per-app scratch pass and limiter on the RK3308.
13. **PulseAudio sink pin and per-stream volume.** On the unit, confirm `SetMasterVolume` pins the
    `master` sink at 100 % and reads every channel back to unity, and inspect `pactl list
    sink-inputs` for the framework stream. The pin covers the sink only; a non-unity per-stream
    sink-input volume is a separate stage that the pin neither sets nor reads back, so record
    whether stream-restore or another writer rescales framework output.
14. **One complete spoken assistant turn on the device.** Settle recognition of captured speech,
    the codec of the returned `play_url` body, playback, latency, an unprovisioned `xtoken_id`
    attach, and outbound TLS from inside the mount namespace.
15. **Radio and Home Assistant on the device.** Play a station on the unit (TLS/DNS inside the
    mount namespace, decoded PCM reaching the mixer, display states under real keys); point the
    Home Assistant bridge at a real MQTT broker and observe discovery, state republish, last-will
    availability and `release_hardware`.
16. **Packaging edge cases.** Install while the daemon is stopped and restart into the package; test
    a socket that fails for a reason other than absence; perform a real 65 MiB browser upload and a
    slow-link upload; and run `-single-app` on a dedicated deployment to confirm exactly one run
    reaches `APP_READY`, another app's launch is refused, Stop keeps the selected-provider restart
    rule, and the Web UI still serves.
17. **Web UI identity and Bluetooth rename on the device.** Open settings for a stopped app and the
    running clock, stop/launch the ring, and confirm no `denied cross-application access` and no
    reconnect loop; change the advertised name, confirm `Adapter1.Alias` and the phone-visible
    name, restart BlueZ and confirm persistence, then confirm the setup AP SSID derivation.
18. **Protocol freeze.** Do not freeze protocol schemas until the generated Go/Rust bindings and
    the script-host parity tests pass, including routed streaming and cancellation.
19. **Import the speaker CA on real clients.** On Firefox, iOS/iPadOS and Android, import the
    downloaded `capsulad-ca.crt` as a trust anchor and confirm the browser warning disappears and
    the Web UI opens without an exception. The certificate is now issued with CA basic constraints;
    existing certificates gain this only through `cert renew` or a fresh `auth init`.
20. **Complete a terminal-installer run on a unit.** Exercise the TUI end to end on a fresh data
    directory (ADB detection, backup, credentials before launch, Wi-Fi, first-party apps, Marusia
    migration/token, jingle choice) and the ADB identity-mismatch abort path. The host suites and a
    read-only detect/backup run pass; no full install through the TUI is recorded.
