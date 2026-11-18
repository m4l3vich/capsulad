# Verification

This page records what is verified, at which proof level, and what remains unverified. A
passing test, simulator frame or backend call is not physical output. Keep the levels apart
when reading or extending this page.

## Proof levels

| Level | Meaning | Proves | Does not prove |
|---|---|---|---|
| Internal state | Daemon-owned state, persisted values, supervisor records | Ownership, routing decisions, CAS results | That a backend or device acted |
| Backend submission | A write was formed and submitted to the backend | Encoding, channel mapping, frame content | That the hardware emitted anything |
| Digital output | The backend's actual output stream or captured samples | Timing, repetition, gain, channel content | Electrical or acoustic behavior |
| Electrical | Measured voltages, currents, bus signals | Driver and gate behavior | Sound or light as perceived |
| Acoustic / optical | Microphone or photometer measurement of the real device | Audible level, emitted light, gamma | Nothing beyond that measurement |

A gRPC send is asynchronous and is never an output acknowledgement. "The stream opened" is
not proof of sound; "the frame was written" is not proof of light.

## Verified on Capsula Mini rev 4

The unit is a Capsula Mini rev 4: RK3308, aarch64, glibc 2.33, kernel 4.4.167, running the
stock Rockchip image.

### Platform, sandbox and boot

- The daemon starts from the single boot hook `/etc/init.d/S99btctl.sh` across power cycles,
  including after `/data` overlay setup; the hook's health gate returns 0.
- In-place upgrade replaces the daemon by rename, restarts through the boot service and
  passes the health gate. The replaced binary exists only as a transient `.prev` during that
  window: it restores the same bytes if the restart or gate fails, and it is deleted once the
  deployment is accepted, so the buffer covers only the deployment in flight.
- Sandbox probe on the live unit: `/proc/self/ns` exposes only `mnt`; `unshare(CLONE_NEWUSER
  | CLONE_NEWNET)` returns `EINVAL`; `PR_SET_SECCOMP` returns `EINVAL`. The daemon reports
  `mode=mountns`.
- A native app under that sandbox ran as a distinct UID (`/proc/<pid>/status` UID 21556) in a
  private mount namespace, mapped `/capsulad/lib` with ABI 1.0, reached `APP_READY`, and exited
  cleanly on stop.
- The optional `org.capsulad.dumbassistant` assistant and the native runtime were replaced while
  the daemon was stopped through the boot service; the service restart preserved settings revision
  and all user choices, and all three expected runs reported `APP_READY`.

### Display and ring

- `GET_COUNT` returns exactly 36 channels on `/dev/topleds` and 30 on `/dev/led_multi_ctrl`,
  matching the device tree (12 RGB ring pixels, 30 monochrome segment cells).
- Both channel maps render correctly on hardware: the clock face is correct, ring pixel 0 is
  at 12 o'clock, travel is clockwise, and RGB order is correct.
- The clock wrote a real segment frame (`cells=[6 7 6 91]`, brightness 0.05); with the ring's
  active window excluding the current local time, the ring wrote its explicit all-zero
  baseline frame.
- A scratch simulator instance on the unit started with a window opening one minute later:
  trace frames stopped at the closing boundary, an all-zero frame held outside it (`dev state`
  0/12 lit), the animation resumed at the opening boundary (12/12 lit), and the stream stopped
  again at the closing boundary. This is digital-frame evidence from the target binary, not a
  photometric measurement.
- Gamma is unmeasured: no photometer was applied.

### Panel input

- The Mini's only evdev node is `gpio-keys`; all five keys (microphone mute, volume up,
  play/pause, volume down, neutral centre) produce events on hardware.
- That same node advertises only `EV_SYN` and `EV_KEY`: there is no `EV_ABS` capability (`EV=3`),
  so the panel emits no coordinates and a crossing is observable only as ordered key transitions.
- Two raw `/dev/input/event0` captures (24-byte `input_event` records) settle the gesture shape:
  a full finger swipe over the panel emits exactly one key press + SYN + release + SYN, never a
  coordinate stream. `swipe_full.hex` holds three such gestures ~1 s apart — `0x72`
  (`KEY_VOLUMEDOWN`), then `0xf8` (`KEY_MICMUTE`), then `0x73` (`KEY_VOLUMEUP`), each a 57–95 ms
  press — which matches full-panel up → `volume_down`, right → `mic_toggle` and left → `volume_up`.
  `swipe_half.hex` holds one gesture reported as `0xf8` and the next as `0x2c0`
  (`BTN_TRIGGER_HAPPY1`), so a swipe anchored at the physical centre can report the centre key
  (`0x2c0`/`0x246`) instead of an edge. A bare single volume edge is therefore indistinguishable
  from a volume tap; the centre key is the gesture evidence a single contact adds, and the ordered
  opposite edge pair inside 400 ms is the full-panel crossing.
- The retained device log contains a real centre→edge contact pair, `center p+0ms r+115ms | gap
  14ms volume_down p+130ms down` (130 ms press-to-press), and its previous heuristic also recognised
  `volume_down … gap 123ms volume_up` as `swipe_right`. So the hardware shape supports both the
  centre-gated contract and the 400 ms full-panel crossing; that old pair lacked the window and the
  leading-step revert now required.
- A passive trace captured a 310 ms circle press, standalone volume taps, a requested swipe
  that produced no kernel contacts, and one overlapping centre→volume-down glide (95.7 ms
  apart) scored as `swipe_left` by the recognizer of that build. Contact order of the user's
  intended swipe is not established, so the trace neither proves nor refutes an inversion.
- The centre-gated echo, zero-deferral steps, full-capture circle and the 400 ms full-panel / 250 ms key / 700 ms centre windows
  are host-verified against synthetic timelines and the real input manager.
- **Device-confirmed after deployment (2026-11-16):** a centre tap followed by a volume edge 79 ms
  later resolved `panel gesture swipe_right resolved tier=centre press_gap=79ms window=700ms`, then
  `swipe_right: next media`, then `Bluetooth media BLUETOOTH_MEDIA_NEXT accepted for MacBook Pro`;
  `volume_down` 114 ms after a centre tap resolved `swipe_left` and
  `BLUETOOTH_MEDIA_PREVIOUS accepted`; both volume releases were consumed by the gestures (no
  volume step). Plain volume taps resolved as `tier=keys` on the press edge with no deferral, and
  opposite volume edges 1.5–3.3 s apart stayed steps. The pad reports **one key at a time**: a
  centre and an edge never overlap, so the released-centre window (700 ms) is the usable form. A
  full-panel crossing (west→east without the centre) leaves one or two bare edges and stays volume
  steps; recognising it needs either a settle delay or a volume write-back, which is an owner
  decision recorded in the behaviour contract, not an implementation gap.
- The microphone key toggles once per press and shows the red run on ring pixels 5–7 with a
  fade; volume steps change the persisted setting and the displayed percentage.

### Microphone gate and privacy indication

- GPIO 21 and the PulseAudio source mute follow the setting, and the gate cuts analog audio:
  capture RMS fell from **1977 to 38** with PulseAudio unmuted.
- Microphone capture requires both a selected visible non-blank presentation and an audible
  cue the mixer consumed; the visible-indication watchdog ended a capture 5 s after the last
  qualifying indication in a scripted hardware run.
- Supported by unit tests, not the physical unit: continuous withholding and per-chunk
  re-evaluation of both indication halves.

### Audio mixer

- A 1 kHz tone from an app measured on `master.monitor`: source RMS **6364**; **4454 at 70 %**
  master volume and **1908 at 30 %**, exactly 0.7 and 0.3 of the source, so software master
  gain is linear and exact.
- A conversation ducked that tone to 0.25× (measured **1113 → 477**) with about a 150 ms fade.
- Master-volume curve measured on the same path: 0 % silence, 1 % peak 58, 5 % peak 294,
  10 % peak 589, 20 % peak 1179.
- Application microphone audio arrives over the bus and stops on the indication watchdog.

### Bluetooth

- Adapter power, incoming-connections and discoverable policy operate over BlueZ D-Bus plus
  the Linux management socket on the live adapter; the two policy settings persist.
- A live MacBook source accepted Play and Next and changed playback state. Setting 50 % master
  volume wrote transport `Volume=64` of 127. A source that has not reported absolute volume yet is
  reported as such (`ErrNoAbsoluteVolume`, one Warn per session, the sentence in the media detail)
  instead of the write silently failing.
- The native A2DP SBC sink endpoint was registered and reported ready at daemon start; the
  registration is verified before the adapter is made connectable. That session used the earlier
  seven-octet capability blob, so it does not prove that a peer accepts the current conformant
  four-octet SBC capability IE (host-verified layout only).
- Connect plays three completed blue fade-in/out ring pulses and shows lowercase `bt` on the
  two rightmost segment cells for 2 s; disconnect plays exactly one pulse and no glyph, over
  the selected baseline. The connection cue (`question.wav`) added with this change has simulator
  digital-output evidence below but **no unit listening check**, so it is not part of this
  hardware-verified record (see Open gates 25).
- SBC decoding matches the system `sbcdec` and the Android reference sample-for-sample on the
  172-frame fixture, including cross-packet synthesis history (host fixture).
- With the PulseAudio fallback, source latency settled at **57–111 ms** (target 68 ms) and the
  loopback rate at **44.10–44.13 kHz**; a shared UART IRQ produced two `hci_uart`
  `Unexpected start frame` errors in a 30 s window until the boot hook moved the IRQ to CPU3.

### Discovery and Web UI

- Avahi on the unit reported `RUNNING`, `GetHostName=capsula` and
  `GetHostNameFqdn=capsula.local`; `avahi-resolve capsula.local` resolved to the unit, and
  startup logged the advertisement with no redundant-operation warning.
- A Chrome-headless session against a real simulator daemon observed the host-name default,
  normalized save (`Living-Room` → `living-room`), persistence across a restart, rejection of
  invalid labels by both the HTML pattern and the server with the prior value retained, the
  simulator's unsupported-mDNS status with no false advertisement, and the **Restart** label on
  Clock, Ring and Diagnostics.
- A second browser run drove the production Web UI, auth, TLS and settings path against a
  successful responder fixture: startup, two GETs and a same-name save made **zero**
  `SetHostName` calls with a clean "Advertised as capsula.local"; a changed label made exactly
  one; a collision stayed visible with no advertised claim; restoring the label recovered.
  Totals: 15 `GetHostName`/`GetHostNameFqdn` reads, four settings updates from POSTs, three
  `SetHostName` calls.
- On the unit the Web UI is reachable over the LAN address: `/healthz` 200, `/` serves the
  embedded SPA, served asset hashes equal the local build, and `/api/status` returns 401
  without a session.

### Script hosts

- `ListRuntimes` on the unit returned `quickjs-ng 0.18.0` and `micropython 1.29.0-preview`
  with shared ABI 1.0 from `/data/capsdemo/lib`.
- A QuickJS package ran `APP_READY` under `mountns`, read the model string through
  `DeviceService/GetInfo`, accepted an invocation, and exited 143 on Stop with no orphan host.
- No MicroPython package has been run on the unit; its host is installed and reported only.

### Personal wakewords

- Native on-device training completed in **670.6 s**. A replay on the device A35 scored 6/6
  word takes and 9/9 backgrounds. Installation evidence, not an accuracy benchmark. Those
  measurements cover the one word that is now stored as `models/word.personal.json`; they say
  nothing about another word's acceptance, and installing one word does not train any other.
- On the settled profile, a warm classifier replay of the unchanged raw-trained model (2.32 s
  of its own same-session ambient audio, the word, and a 1 s tail) fired **6/6 raw and 6/6
  processed**.
- The processed microphone chain was measured on one authorized 10 s background recording
  (mostly music through Mac speakers): cleanup averaged **2.9 dB overall and 2.7 dB in
  80–300 Hz**. It has no local playback reference, so it is not proof of fan/traffic immunity.
- Synthetic room-noise regressions measured **4.23, 8.33 and 3.42 dB** of suppression for
  pink, brown/rumble and amplitude-modulated noise (host fixtures).
- The cold-start guard preserved first speech at worst **0.896** of the reference (gate
  ≥0.85).

### Resources and storage

- A sampled idle state reported 72 % CPU idle, daemon RSS 77 MiB, assistant 8.9 MiB and
  17.8 MiB free on `/data`.
- The bounded log rotates at 512 KiB with one backup; wakeword recording and training refuse
  below 2 MiB free.

### 2026-11-17 second deployment: aarch64 build of the working tree

A coordinated `capsulad-cli deploy` of the current working tree (HEAD `33378ec` plus the
uncommitted app-install / Web UI work) was applied to the unit over ADB on 2026-11-17, replacing
the `77f5c85` stack recorded above. The artifacts were cross-built for aarch64 / glibc ≤ 2.33 with
a repo-local toolchain (Apple clang 21 front end, `rust-lld` linker, an extracted Debian bullseye
arm64 sysroot under `build/linux-arm64/sysroot/`); each ELF passed a `readelf` gate: highest
symbol version `GLIBC_2.30`, no `RPATH`/`RUNPATH`, and every `DT_NEEDED` present in the device's
248-soname inventory. A cross-built `hello world` executed on the unit before anything else was
staged.

- **Deployed stack (device md5s equal to the build host's).** `capsulad` 41,287,832 B
  `368635d1…`, `capsulad-cli` 11,731,096 B `21b7fa4f…`, `lib/libcapsulad.so.1.0.0` 6,498,328 B
  `765301c1…` (SONAME `libcapsulad.so.1`, `capsulad-native.json` ABI 1.0), `capsulad-quickjs`
  1,236,416 B `3ec13072…`, `capsulad-micropython` 418,736 B `23af60a7…`. The deploy printed
  `daemon healthy` and reinstalled `/etc/init.d/S99btctl.sh` byte-identically (`c1083ff9…`).
- **Runtime proof.** Both hosts were pushed to `/tmp` and ran on the unit against the new
  library (`quickjs-ng 0.18.0`, `micropython 1.29.0-preview`, exit 0) before the install, and
  took the library from `/data/capsdemo/lib` afterwards; every app binary's
  `LD_TRACE_LOADED_OBJECTS` resolved with zero missing `DT_NEEDED`. Gate 7 and gate 30 below
  record what that does and does not settle.
- **Apps.** Seven packages installed: `simon 1.0.0` (quickjs), `pa 0.1.0` and `radio 0.1.0`
  (micropython), `radio.tuner 0.1.0` and `homeassistant 0.1.0` (native aarch64), plus updated
  `dumbassistant 0.2.0` and `marusia 0.1.0`. `app list` reports the expected runtime per app, and
  runs of the native and MicroPython apps reached `APP_READY` on the unit.
- **User state preserved.** Settings revision 3635 unchanged (assistant `org.capsulad.marusia`,
  master volume 10 %, `Asia/Irkutsk`, wakeword switch on, display provider `capsulad.clock`) and
  the `<data-dir>/app-settings` files were not rewritten.
- **Storage.** `/data` is 78.5 MiB and holds 4,259 KB free with all seven packages, both script
  hosts and the new runtime. The first attempt aborted before replacing any binary with
  `write /data/capsdemo/lib/.deploy-…: no space left on device`; a deployment does not revert its
  package installs, so the retry ran after the stack was stopped through the boot service, which
  released the pinned blocks of a library that had been moved aside while running processes still
  mapped it. The canonical deploy then reported `deploy_rc=0` and passed its health gate. A bundle
  deploy needs ≥ 8 MiB free on `/data` before it will replace binaries.
- **tmpfs OOM during the deployment window.** With the 63 MiB bundle staged in `/run`, the
  restart that ended the deployment started five apps at once and the Go apps logged
  `fatal error: out of memory allocating heap arena map` (`radio.tuner`, `marusia` twice,
  `homeassistant`). The supervisor restarted them to `APP_READY`, but **two `org.capsulad.marusia`
  runs survived**. Removing the pushed bundle and its staging trees from tmpfs raised
  MemAvailable from 42 MiB to 106 MiB. These are observations; no cause is established here.
- **Loss of network after the follow-up restart.** One further restart through
  `/etc/init.d/S99btctl.sh restart` (run detached, because the daemon owns the Wi-Fi station) was
  the last device interaction. ADB then went offline and the unit did not answer again: ICMP
  100 % loss, `arp` entry `(incomplete)`, no host on the local /24 answering 5555, an empty ADB
  mDNS service list and no `capsula.local` resolution, against a healthy host LAN. The cause is
  not established — the same restart path had completed successfully minutes earlier — and no
  claim is made here about a watchdog reset, a failed start or the vendor fallback.
- **Unverified in this deployment.** Post-restart startup behavior of the installed stack; the
  sandbox mode of a healthy start (runs started during the OOM window reported `sandbox=none`,
  where earlier records report `mountns`); the two duplicate `marusia` runs; and an app-level
  QuickJS start/stop of `org.capsulad.simon`. Rollback material (`capsulad` `db1ad420…`,
  `capsulad-cli` `fd48698b…`, runtime `bf814012…`) is kept on the build host and in
  `/tmp/rollback-20261117/` on the unit;
  `build/linux-arm64/release-20261117b/verify-after-recovery.sh` re-runs the whole verification
  once the speaker is reachable. This is gate 31.

### 2026-11-18 full-stack deployment (all 12 apps, startup ring)

A coordinated `capsulad-cli deploy` of the 2026-11-18 working tree (HEAD `1c6bb20` plus the
startup-ring work and the size-optimised CLI/Web UI) was applied over ADB to the Capsula Mini
rev 4. Artifacts were cross-built for aarch64 / glibc ≤ 2.33 with the repo-local bullseye
sysroot toolchain; every ELF passed the version/SONAME/`DT_NEEDED` gates. No device reboot was
performed.

- **Deployed stack.** `capsulad` 28,639,384 B `4ddb9111…`, `capsulad-cli` 8,781,976 B
  `90420043…`, `lib/libcapsulad.so.1.0.0` 6,500,208 B `fa742e8f…` (SONAME `libcapsulad.so.1`,
  max GLIBC 2.30), `capsulad-native.json` `f4491337…` (ABI 1.0), `capsulad-quickjs`
  `f80ae271…`, `capsulad-micropython` `dfe2fbb0…`, `/etc/init.d/S99btctl.sh` `91440905…`.
  The installed files matched `SHA256SUMS.final` (`evidence/check-installed-hashes.sh`).
- **Packages.** All twelve installed and listed with the expected versions/runtimes:
  `simon 1.0.0` (quickjs), `pa 0.1.0`/`radio 0.1.0` (micropython), `radio.tuner 0.1.0`,
  `homeassistant 0.1.0`, `dumbassistant 0.2.0`, `marusia 0.1.0`, `cadence 0.1.0`,
  `airplay 0.1.0`, `dev.hardware 0.1.0` (native), `dev.quickjs 0.1.0` (quickjs),
  `dev.micropython 0.1.0` (micropython).
- **Runs and runtime.** `capsulad -health` exit 0; `S99btctl.sh status` 0. Runs: clock,
  marusia, airplay, homeassistant, pa, radio, radio.tuner all `APP_READY`, single runs. The
  four native runs' `/proc/<pid>/maps` resolve
  `/capsulad/lib/libcapsulad.so.1.0.0` with no `(deleted)` entry, so the restarted native apps
  load the updated runtime. A safe smoke started `dev.quickjs` and `dev.micropython` to
  `APP_READY` and stopped both; the non-dev run set was unchanged.
- **Service restarts and recovery.** Three canonical service restarts were required by the
  observed failures, not by a reboot: (1) the daemon/boot deploy's `-restart auto`; (2) a
  recovery `S99btctl.sh restart` after the live apps attempt refused; (3) the final
  apps + `-restart auto` that installed all twelve packages and started the full stack. The
  first attempted live app install (`-restart never`) refused because the daemon restarts the
  selected default assistant after a stop request (`org.capsulad.marusia … still running after
  a stop request`); the deploy restored the previous app tree (`restore_rc=0`) and the
  recovery restart returned health 0. The first apps space gate then refused because the old
  `libcapsulad` inode was still mapped by the five running apps/hosts (`(deleted)` maps), leaving
  12,849 KiB free and a 2,405 KiB projected final; stopping those replaced runs through the
  documented app lifecycle freed the inode and raised free to 19,218 KiB. The standard
  `-restart auto` apps deploy then installed all twelve packages with the daemon stopped and
  performed the final restart; this is the deployment finding recorded below. The automation
  now stages that stopped-daemon apps+restart path and stops the pinned replaced runs first, so
  a re-run does not repeat the live `-restart never` attempt. It performs a read-only projected
  free-space preflight before stopping any run (counting the pinned library and interpreter-host
  files), verifies the stopped-daemon stage script is fully staged and started, polls the actual
  free space after the stops (the selected assistant may restart on the new lib), and restores
  the baseline through the service and missing app starts on a start, gate or timeout failure.
  Stage logs are cleared with a verified size check before each start, the timeout branch always
  restarts the service, post-stage verification failures run the same baseline recovery, and a
  baseline-restore failure is a hard error rather than a warning. A deleted native-lib map or a
  non-ready airplay run forces the restart even when the five baseline apps are ready (airplay
  is start_on_boot but not part of `baseline_ready`); ring/marker/settings failures with a ready
  baseline do not restart.
- **Startup ring.** The final boot's per-boot context (`/run/startup-context.txt`, 144 lines)
  contains exactly one `startup ring introduction on light.ring.main (capture=…)` at
  `2026-11-17T22:38:11.747788107Z` and one
  `startup ring complete; selected ring baseline restored` at
  `22:38:14.314677206Z` (2.567 s log interval; the source/simulator measured 1.924 s — the
  device interval includes setup, acquisition and release scheduling). The whole-log trace
  retains the earlier boots' ring lines (22:32, 22:35, 22:38) while the per-boot context has
  only the final one. The physical fade-in/out is **not** optically measured; the evidence is
  the daemon's presentation lifecycle and the reviewed backend/simulator proof.
- **Startup audio (digital monitor only).** The final restart was captured on
  `alsa_playback.monitor` and `master.monitor` (16 kHz mono, gzip) with no microphone
  recording. The master monitor has a **10.800 s** non-silent span (peak 890) and the ALSA
  monitor a 4.500 s maximum span (peak 914) for the authored 11.413 s jingle. The captures were
  stopped by terminating `parec` so gzip wrote a valid trailer; the remaining file is 314 s of
  silence after the jingle. This is digital output evidence, not an acoustic or optical
  measurement.
- **Settings/user state.** The four pre-existing app-settings files are byte-identical
  (`capsulad.clock 66d8345f…`, `capsulad.ring f9c799ae…`, `org.capsulad.dumbassistant
  c3726d99…`, `org.capsulad.marusia f4190f1b…`); installing the new apps seeded
  new app-settings for `org.capsulad.airplay`, `dev.quickjs`, `dev.micropython` and
  `org.capsulad.radio`, and advanced the global settings revision 3800 -> 3808.
  The selected default assistant, display provider, startup-sound switch and
  `startup-sound.played` marker are unchanged. The master volume was moved 19.7 % -> 10 % by
  logged **physical panel volume key presses** (`key volume_down/up` at 22:36:07–22:36:14 UTC),
  not by the deployment; it was then restored to the pre-deployment value through the public
  `AudioService.SetMasterVolume` (`0.1968503892` float32, CLI reports 19 %), advancing the
  revision to 3809. The four pre-existing app-settings and all provider/boot choices are
  unchanged. The host driver therefore checks the pre-existing app-settings hashes and the
  user-visible settings other than volume/revision; the whole `settings.json` hash is not
  expected to match (app registration advances the revision, and new app-settings are seeded).
- **Storage.** `/data` free after the deployment was 8,649 KiB, `/run` was returned to 44 KiB
  (task staging/backups removed after evidence capture), `/tmp` returned to its pre-deployment
  baseline, and `MemAvailable` was 89,940 kB. The task-created `/tmp` shards, `/run` logs,
  captures, settings snapshots, backups and helpers were removed after evidence capture (the
  observed 44 KiB `/run` result followed the manual cleanup of this run; the driver now removes
  those `/run` probes automatically); no user file or log under `/data` was deleted or truncated.
- **Remaining gates.** The visible ring fade/gamma and the actual acoustic output are not
  measured on this unit; the double-rotation per-boot-boot trace edge remains theoretically
  possible (the whole-log lifecycle section still retains the lines); and the master-volume
  restoration used the public float32 API, so the stored value is the float32 rounding of the
  pre-deployment 19.685 % (the CLI and effective level are 19 %). Deployment finding for future
  changes: replacing a package set that includes the selected assistant requires the daemon to
  be stopped (use `-restart auto` or a service stop); a live `-restart never` replacement can
  race the assistant's automatic restart. A live `lib/` replacement also leaves the previous
  library inode mapped by running apps until the next restart, so app-space accounting must
  account for that pinned inode.

### 2026-11-18 deployment: Now Playing, AirPlay discovery, native Radio, Simon and app guides

The updated ARM64 stack was staged over ADB and installed through `capsulad-cli deploy` and
`S99btctl.sh`. The installed daemon (`0e8f808f…`), CLI (`61bef405…`), native runtime
(`401d35e5…`), runtime metadata, script hosts and boot hook matched the build. All 48 files
from the 11 app packages matched their archives. The app update exited 0 and its canonical
restart passed `capsulad -health` and the boot-service status check.

- Native Internet Radio 0.2.0 reached `APP_READY` at startup with autoplay still off. The
  retired tuner was stopped and uninstalled; no package or run remains. Simon 1.1.0 passed a
  launch/stop smoke and left the existing run set unchanged. The bundled guides were installed;
  the browser guide route and Now Playing interaction were not exercised on the device.
- Avahi resolved both `_airplay._tcp` and `_raop._tcp` to `capsula.local:7000`, repairing
  the previously missing RAOP record. AirPlay advertised a different device ID after deployment
  despite unchanged saved values. Pairing impact and a real sender session remain unverified.
- Global settings, Radio preferences, assets and wakeword data matched the pre-deployment
  snapshot. Only AirPlay's settings revision advanced; every saved value matched.
- A large temporary backup caused transient allocation failures before any install. Removing
  that staging restored the original service without a restart. The unit later rebooted
  unexpectedly during the core update; the stage log was lost and the cause is unconfirmed.
  Post-reboot hashes and health verified the complete core installation, not a clean core
  deploy exit. The subsequent app update did not reboot the unit. Temporary staging was
  removed; `/data` had 8,653 KiB free at the end.

Detailed checked hashes are in `build/candidate-20261118T112139Z/DEPLOY-EVIDENCE-20261118.md`;
local rollback artifacts are in `build/deploy-20261118T1925Z/`. No media was sent to the
speaker, and no acoustic or optical evidence was collected.

## 2026-11-18 current-change evidence: AirPlay advertisement, Simon 1.1.0, radio and app guides

This section records evidence for the current working tree only. It is not a deployment record,
and its parts sit at different proof levels. A container or a simulator is host infrastructure,
not the speaker; read the level stated in each part.

### AirPlay: field diagnosis and independent responder

- **Field evidence (read-only device logs).** On the Capsula Mini rev 4 the old production stack
  published `_airplay._tcp` and it resolved to `capsula.local` / `192.168.1.50` (and the unit's
  IPv6 address); `OPTIONS * RTSP/1.0` on TCP 7000 answered `200 OK` with
  `Server: AirTunes/366.0`. `_raop._tcp` was never advertised: `avahi-browse -rt _raop._tcp` on
  the unit and `dns-sd -B _raop._tcp` on the Mac both omitted the speaker, while the daemon log
  recorded `publish _raop._tcp: … add service: Invalid record` wrapped as "mDNS responder is not
  running". The app's `pk` TXT item was the base64 PKCS#1 RSA public key (about 364 bytes, item
  about 367), and Avahi rejects a TXT character-string over 255 bytes with
  `org.freedesktop.Avahi.InvalidRecordError`; this was an Avahi rejection, not daemon
  pre-validation. A later post-session `_airplay` `Local name collision` was diagnosed as an
  application-side concurrent-publish race: one successful `Unpublish` was followed ~30 ms later
  by an `AddService` collision, and a different publish call succeeded about 1 s later (the app
  has no retry). The fix uses the 32-byte ed25519 `pk` as 64 lowercase hex on `_raop` (with
  `am`, `ft`, `sf`), caps the `_airplay` instance at 63 UTF-8 bytes, projects the RAOP instance
  rune-safely after its 12-hex `@` prefix, pre-validates each TXT item at 255 bytes, maps invalid
  records to `INVALID_ARGUMENT` and collisions to `ALREADY_EXISTS`, serialises publish/close, and
  packages the app root `README.md`. `apps/airplay/README.md` documents the resulting wire
  contract; this page records only what was observed.

- **Independent responder (real Avahi in a container).** A cross-compiled `linux/arm64` test
  binary ran in `debian:bullseye` against a real `avahi-daemon` 0.8 and system D-Bus.
  `avahi-browse -rt _raop._tcp` then listed and resolved the new record set: instance
  `AABBCCDDEEFF@Scratch Speaker`, port 7001, TXT `am=… pk=<64 hex> ft=… sf=… cn=0,1,2 et=0,1
  md=0,1,2 tp=UDP,TCP vn=65537 vs=366.0 da=true`. Sending a 364-byte `pk` directly to Avahi still
  produced `add service: Invalid record`, mapped to `ErrInvalidRequest`; a duplicate instance
  produced a real collision, mapped to `ErrNameInUse`. So the old production build did not
  announce `_raop`, while the new container build publishes and resolves it. This is a genuine
  Avahi D-Bus responder, not a fake or a host loopback; it is still not the speaker.

- **Independent sender (loopback).** `TestPyatvFullFlowStreamsPCM` with pyatv 0.18.0 passed again
  on the current tree: the receiver delivered **611,072 bytes of PCM** to the framework sink.
  This is the legacy RAOP path over loopback. It is neither an Apple sender on the user's LAN nor
  proof that the fixed advertisement is accepted by one.

- **Final host checks.** The final `-race` run of the advertisement ownership, session-state and
  RAOP-label tests passed on the real files. A final real-tree Docker run,
  `go test ./internal/discovery -run 'TestAvahi|TestNormalizeServiceTXTItemLimit'`, also passed;
  that removes the earlier overlay caveat for the discovery package (the container proof above
  used overlays for a missing `strings` import and for an unrelated `media_watch.go` compile
  error, and remains an independent result).
- **Open and not claimed.** No rebuilt AirPlay package has been deployed to the speaker, and no
  Apple sender, acoustic output or post-fix device advertisement is verified. The 611,072-byte
  pyatv loopback is a sender-library proof only.

### Simon Says 1.1.0 (isolated simulator)

- `node --test apps/simon/test/*.test.mjs` reported **31/31 passing** (`tests 31`, `pass 31`,
  `fail 0`). This supersedes the earlier 22/22 contract count recorded for `simon 1.0.0`; that
  older result stays as history for that package, not for 1.1.0.
- `sh apps/simon/build.sh` built `org.capsulad.simon-1.1.0.tar.gz` with the root `README.md`,
  which was installed in an isolated simulator (fresh data directory, simulator platform) and
  started to `APP_READY`.
- On that random run the simulator backend received north-side red frames on ring pixels **0, 1
  and 11**; injecting the corresponding north panel key through the simulator admin advanced the
  score display to `0001`. Holding the centre for **900 ms** and releasing restored the
  configured Clock and Ring baselines, released the panel capture and left no Simon run. The app
  log confirmed the playback stream closed.
- Limits: the ring/segment frames are backend submission, not emitted light; the PCM tone shape
  asserted by the host suite is host fixture evidence only, not an acoustic measurement. No
  device, optical or listening check is recorded here.

### Internet Radio (simulator browser and nested suite)

- **Build/install and guard.** `org.capsulad.radio-0.2.0.tar.gz` (sha256
  `ca4334b319032fe8bdd29e398448d1ee02186302d8e9894ff1faec67fad7cf4c`) was built from
  `apps/radio`, installed live after stopping the old run (`reinstalled org.capsulad.radio
  (0.2.0) … the daemon reloaded its application registry`), and started once to `APP_READY`;
  logs showed `radio ready` and
  `settings ready revision=6 stations=2 station=2 autoplay=false`. The scoped scratch bundle
  gate refused the old tuner directory/archive as intended.
- **Nested suite and dependency boundary.** `CGO_ENABLED=1 go test -race -count=1 -ldflags
  '-linkmode=external' ./...` passed from `apps/radio`. For both `darwin/arm64` and
  `linux/arm64`, `go list -deps ./cmd/radio` excluded `net/http`, `crypto/tls` and gRPC.
- **Browser acceptance (PASS).** In one Chrome document against the authenticated simulator,
  idle Internet Radio was `Running` with Now Playing `Nothing is playing.` An external
  `capsulad-cli app start … station=1` made `North tone` appear without a reload
  (`performance.timeOrigin` unchanged); the named picker then selected `East tone`, switching
  Now Playing live on the same document (`revision 9 station=2`); Pause changed it to
  `Nothing is playing.` with the pause control removed and the log `state paused …/east.mp3`.
  Closing and reopening the detail kept `Selected: 2 — East tone` with option 2
  `aria-selected="true"`, so the station choice survived the reopen.
- **Fast local origin / prior queue-full.** The final origin sustained playback for about
  **29.6 s** (`17:48:48.337` → `17:49:17.904`) to a finite EOF, reconnected (`reconnect 1/3`),
  and a second EOF/reconnect cycle also succeeded while the UI stayed on the station.
  Queue-full occurrences in the final run were **0**; the earlier
  `playback write failed (13): outbound call queue is full` is superseded by this fix, not an
  open gate.
- **Settings follow-up (PASS, rebuilt 0.2.0).** The follow-up used a rebuilt
  `org.capsulad.radio-0.2.0.tar.gz` with sha256
  `3955d538552d50c23edeb340528ffa0ba4d24ad84c56e0565c993cbc7db21cce`; the earlier acceptance
  above used `ca4334b3…`, so both artifacts are recorded. It was installed live after stopping
  the previous run and started once to `APP_READY` (`revision=11 stations=2 station=1`), then
  `app start … station=2` made `East fast` active. In the real settings iframe, shortening the
  textarea from the two-station list to the one-station `North fast` and clicking Save reported
  `Saved.` while the app setting **stayed `station 2`** (`revision 13`: `stations=1`,
  `station=2`); the picker showed `Selected station 2 is not in the list. Pick one above.` with
  class `notice err`, Now Playing removed the playing row live on the same document
  (`Nothing is playing.`, no reload), and the app log recorded
  `error: station 2 is not in the 1 configured stations`. An unsaved
  `Draft | http://127.0.0.1:18766/east.mp3` textarea survived autoplay toggled
  `false → true → false` exactly as typed (each update `Saved.`), while the daemon stayed
  `revision 15` with the one-station list, `station=2`, `autoplay=false`: the draft was never
  persisted. Clicking picker `North fast` selected `1 — North fast`; closing the detail and
  using Actions → Play logged `session 2 playing station="North fast"` and made Now Playing
  appear live on the same document (`revision 16 station=1`), then cleanup paused it.
- **Scope of that follow-up.** It was a settings-document-only change, so the pacing proof was
  not rerun and no active switch/Pause retest beyond the shrink-stop and recovery-Play paths was
  performed. That pacing result remains evidence for the same radio logic, not a separate
  rerun. The durable UI test `cd web && node --test test/radio-settings.test.mjs` passed **2/2**:
  it runs the packaged script with the actual injected bridge, covers an accepted older save
  plus a newer rejected save and a persisted value repeated before the older commit, and
  confirms only the matching `requestId` can acknowledge a draft — artificial-ordering test
  evidence, not a browser or physical result.
- **Limits.** These are backend and browser evidence on the simulator; no sound was measured
  and no device or real station was used.

### App guide

- The Web UI guide-render suite (`web/test/guide-render.test.mjs`) passed **4/4** node tests, and
  the Go race run over `internal/manifest` and `internal/appui` passed its guide read and
  built-in-guide tests.
- A subsequent coordinated race run over `./internal/webui` and `./internal/daemon` with
  `-run 'TestGuide|TestOverview|TestFrameworkMediaCommand|TestDiscovery(Publish|Auth|Browse)|TestBluetoothMedia'`
  passed in both packages, covering the guide access/usability handlers plus the Now Playing and
  Bluetooth snapshot/stream checks; `npm run build` passed and rebuilt the embedded Web UI.
- In Chrome against the rebuilt authenticated simulator, Apps → stopped Simon → **User guide**
  displayed "How to play" and "392 Hz", and **Back** returned to the application; the built-in
  Clock guide displayed "Stopwatch".
- Archive checks found a root `README.md` in each package built here: Simon, Radio, PA, Home
  Assistant, AirPlay (its host-only package override), DumbAssistant, Marusia, `dev.hardware`,
  `dev.quickjs` and `dev.micropython`. Cadence was packaged in the cached arm64 Bullseye
  container with the glibc loader and also contained `README.md`. Built-in guide embedding was
  covered independently, and authentication plus rendering were verified in real Chrome.
- These remain simulator/host/browser results; no device run of the guide path is recorded here.

## Verified only on the simulator or host

- Complete virtual backend (segment display, RGB ring, contacts, keys and audio). The Go
  suites for this tree were run here: `make test` and `make test-race` reported 33 packages
  ok, with the single known `internal/daemon`
  `TestDumbAssistantEndsAfterSpeechOverColoredRoom` failure (see
  [Known pre-existing failures](#known-pre-existing-failures)); the clean race run reported
  **0 data races**. `make test-sdk` exited 0; its per-suite counts and the gated Go SDK result
  (37 pass, 0 fail, 1 skip) are recorded in the networking bullet below.
- **Startup ring introduction (simulator backend, 2026-11-18).** Proof level: backend
  submission — frames the output loop wrote into the simulator's ring, not emitted light.
  `go test -race -ldflags '-linkmode=external' -run 'TestStartup' -v ./internal/daemon`
  passed (`ok`, 18.466 s) with
  `TestStartupRingGeometryFollowsRingCapabilities`,
  `TestStartupRingFadesAndRestoresBaseline`,
  `TestStartupRingFollowsStartupCueDecision` (installation-boot, explicit-no-provider and
  `startup_sound_enabled` sub-cases), `TestStartupRingStaleFadeCannotTouchNewerOwner`,
  `TestStartupRingRendersBeforeSlowBootApps`, `TestStartupCueMarkerConsumesTheIntroduction` and
  `TestStartupRingReleasedOnShutdown`.
  The ordering regression blocks the only boot app's `Launch` on `pkgMu` (explicit no-provider
  settings keep `applySettings` from launching anything earlier): the introduction's frames still
  reached the backend while the boot was blocked, with a near-dark minimum blue level and the full
  217 peak. Reverting to the old order (`go d.outputLoop()` after `startBootApps`) made that test
  fail with `the introduction produced no physical frame while boot app startup was blocked`; the
  writer now starts before `maybePlayStartupCue`. The marker test waits for
  `startup-sound.played` to appear only after the cue reached the simulator mixer, then confirms a
  second decision with the off-by-default setting starts no introduction; the retry side is the
  documented marker intent (a failed audio open does not consume the one-shot cue) and remains
  pinned by `TestStartupCueFollowsMarkerAndSetting`. The shutdown test cancels the daemon with an
  active introduction and asserts the capture/presentation are released and the capture is no
  longer selected. The geometry test covers 39- and 48-pixel rings, which now keep the preset's
  12-pixel top arc instead of silently dropping the introduction.
  The explicit-no-provider sub-case forces the installation cue decision while
  `light.ring.main` is an explicit none: the transient introduction still took the ring (as the
  muted-microphone indication does), released at its bounded lifetime, and left
  `selected_presentation`/`selected_capture_id` empty with an all-zero backend frame — the durable
  blank baseline survived. `TestExplicitNoRingProviderSurvivesRestart` and
  `TestFrameworkOwnedClockStartsWithNoProviderSelections` still pass; the restart fixture now
  records the startup marker so it asserts the persisted baseline after a later boot rather than
  the one-time installation indication, and `TestOutputClearsUnownedDisplayAndRing` passes.
  The live simulator daemon (`-platform simulator`, fresh data directory, `-output-trace`,
  `-dev-admin`, `/state` polled about every 60 ms) wrote the derived four-pixel top run
  (pixel count 12 → count 4, pixel 0 = top): the first submitted frame was
  `{2,10,12}`-per-pixel (near dark), the fade-in sampled 119 → 155 → 180 → 216 → 217 on the
  authored blue channel (`#28AFD9`), the full run held under `brightness=0.8`, and the
  fade-out steps measured 181 → 145 → 109 → 72 → 36, each one sixth of the peak. The
  introduction logged at `21:05:56.132` and `startup ring complete; selected ring baseline
  restored` at `21:05:58.056` (1.924 s), after which `light.ring.main` selected
  `capsulad.ring` with no capture and the breathe baseline was lit again on all 12 pixels —
  no forced black frame. A post-ordering-fix run of the final code reproduced it: first submitted
  frame `{2,10,13}`-per-pixel, sampled blue 143 → 179 → 216 → 217, tail 181 → 145 → 72 → 36 → 0,
  introduction `22:07:16.697` → `startup ring complete; selected ring baseline restored`
  `22:07:18.623` (1.926 s), then the breathe baseline lit all 12 pixels again. A second start on
  the same data directory (marker present,
  `startup_sound_enabled` false) logged no `startup ring` line and left the ring on
  `capsulad.ring`/`default_baseline` with no capture. A third run reproduced the deployed unit's
  persisted shape — marker present, `startup_sound_enabled` true, `default_app_by_resource`
  holding only `display.segment.front` and no ring key, so `capsulad.ring` is not a run — and the
  introduction still appeared over the unowned ring: 4/12 pixels lit, sampled blue levels
  144 → 179 → 203 → 217, then 145 → 109 → 72 → 0; log
  `21:14:26.899` → `startup ring complete; selected ring baseline restored` at `21:14:28.824`
  (1.925 s), after which `light.ring.main` had empty `selected_app_id`/`selected_capture_id` and an
  all-zero frame. The transient indication therefore reaches a unit that never selected a ring
  provider while the durable blank baseline survives. The full
  `go test -ldflags '-linkmode=external' ./internal/daemon` package run completed with only the
  documented environmental `TestDumbAssistantEndsAfterSpeechOverColoredRoom` failure (the same
  failure the behavior document records for this host), so the boot-time capture added no other
  package regression. Not observed here: emitted light,
  gamma/brightness as seen, real 12-LED timing, and the device's post-deployment boot order;
  those remain gates of the deployment task.
- **App and bundle ABI gates (host, 2026-11-18).** Two release-pipeline blockers found while
  preparing the device bundle were fixed and narrowly proved without compiling the daemon.
  (1) `make radio-tuner` failed after compiling because `apps/radio/tuner/build.sh` searched
  `$OUT/$APP_ID` with `find -maxdepth 1 -perm -u+x`, which never sees the manifest's
  `bin/tuner`; the command exited 2 with `error: no staged executable to ABI-check`. The script
  now ABI-checks that explicit path. With `GOOS=linux GOARCH=arm64
  DEPLOY_PLATFORM=rockchip-stock TARGET_ABI=linux-gnu CC=<aarch64 wrapper>
  CAPSULAD_NATIVE_LIB_DIR=build/artifacts-20261118/lib
  CAPSULAD_CLI=build/artifacts-20261118/bin/capsulad-cli-host make radio-tuner`, the same
  command exited 0, printed `interpreter=/lib/ld-linux-aarch64.so.1`, and wrote
  `org.capsulad.radio.tuner-0.1.0.tar.gz` (993,875 B) whose manifest declares
  `runtime = "native"`, `path = "bin/tuner"`, `architecture = "aarch64"`, `abi = "linux-gnu"`.
  (2) `make deploy-bundle` with `NATIVE_TARGET=aarch64-unknown-linux-gnu` rejected the three
  script apps because the libc gate demanded an `abi` from every archive, while the QuickJS and
  MicroPython manifests carry none. The bundle now applies the libc/ABI check only to
  `runtime = "native"` archives, accepts QuickJS/MicroPython runtimes, and still refuses an
  unknown runtime. Narrow proof (stub daemon and CLI, no Go compile):
  `make -o capsulad -o capsulad-cli deploy-bundle BUNDLE_NATIVE=0 BUNDLE_DEVAPPS=1
  GOOS=linux GOARCH=arm64 DEPLOY_PLATFORM=rockchip-stock
  NATIVE_TARGET=aarch64-unknown-linux-gnu …` bundled all 12 archives (9 apps + 3 devapps);
  a fabricated `org.capsulad.fakenative` archive (`runtime = "native"`, no `abi`) still failed
  with `declares abi "", but rockchip-stock needs gnu`, and a `runtime = "wasm"` archive failed
  with `declares runtime "wasm"; the rockchip-stock ABI gate needs native, quickjs or
  micropython`. `sh tools/check-deploy-bundle.sh` still passes. Host proof only: the stub
  daemon/CLI were shell scripts and no device executed these artifacts.
- **Host builds and target compile (this macOS host, 2026-11-17).** `make daemon` produced
  `capsulad` 41,095,522 bytes and `capsulad-cli` 11,792,818 bytes (0.1.0-dev, SDK 0.5);
  `make native` produced `libcapsulad.1.dylib` 5,270,288 bytes with metadata ABI 1.0, which the
  appended networking surface keeps (pre-release);
  `make sdk-go` and `make sdk-rust` passed (596,400-byte rlib); a fresh `WORK` directory built
  `capsulad-quickjs` 1,120,416 bytes and `capsulad-micropython` 352,520 bytes; `make devapps`
  built the sample set (`dev.hardware` 964,863 bytes); `make dumbassistant` produced a
  1,093,495-byte tarball and `make marusia` a 1,636,371-byte tarball (4,249,810-byte stripped
  binary). `make daemon GOOS=linux GOARCH=arm64` produced static aarch64 ELF binaries
  (40,894,616 and 11,600,024 bytes), so the Go-only daemon cross-compiles. The native SDK
  aarch64 cross build was **not** run: `make native NATIVE_TARGET=aarch64-unknown-linux-gnu`
  is skipped on this host because the Rust target/core and the cross linker are not installed.
  `go mod tidy`, `gofmt` and `cargo fmt` reported no diff; two web builds were byte-identical
  and `npm test` passed 39/39.
- **Deployment bundle CLI omission (host, 2026-11-18).** `sh tools/check-deploy-bundle.sh`
  assembled both bundle modes from stub build outputs with the real `deploy-bundle` recipe
  (no Go compile): the default output and tarball contained `capsulad`, `capsulad-cli` and
  `boot/S99btctl.sh`; `BUNDLE_CLI=0` contained no `capsulad-cli`, a stale `capsulad-cli`
  placed in the reused bundle directory was removed, and switching back to the default on the
  same directory restored it. `make -n deploy-bundle` listed the
  `./cmd/capsulad-cli` build and copy for the default, and neither for `BUNDLE_CLI=0`;
  `BUNDLE_CLI=yes` failed before any recipe line with `BUNDLE_CLI must be 0 or 1, got 'yes'`.
  `go test -ldflags '-linkmode=external' ./cmd/capsulad-cli -run
  'TestDeployWithoutCLILeavesInstalledCLI' -count=1` passed (`ok`, 0.058 s): a daemon-only
  bundle deployed over a data directory holding a sentinel `<data-dir>/capsulad-cli` left
  that file byte-identical and printed no `cli:` plan line. These are host assembly and
  deploy-logic proofs only; no adb push, tmpfs recovery, device deployment or on-device
  `/data` measurement was performed. With the current CLI measured below, a CLI-free bundle
  omits 8,781,976 B from the directory and about 3,233,000 B from the gzip tarball; `/data`
  reclaims that only when no separately installed copy remains.
- **CLI size build (host cross-compile, 2026-11-18).** The current `linux/arm64` release
  artifact is 8,781,976 bytes (8.38 MiB) against the 11,993,240-byte (11.44 MiB) baseline:
  −3,211,264 B (−3.06 MiB, 26.8%). `go version -m` records `-tags=grpcnotrace`,
  `-trimpath=true`, `CGO_ENABLED=0`, `GOOS=linux`, `GOARCH=arm64`; `CGO_ENABLED=0` is the
  Linux cross default, not a Makefile pin (ambient cgo is used otherwise). Its gzip sizes
  (host GNU gzip `-n`, no stored name/time; these are implementation-dependent and vary by a
  few bytes) are about 3,233,000 B at level 6 (`tar -czf`'s default compressor) versus about
  4,348,500 B for the baseline, and about 3,219,000 B at `gzip -9` versus about 4,327,900 B.
  The tag removes 7 dependencies for the documented
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` target (273 → 266; host `darwin` counts 275 → 268),
  including
  `golang.org/x/net/trace` and the template stack. The development admin path now uses
  `http.NewRequest`/`http.ReadResponse` over `net.Dial` instead of `http.Client`;
  `go test -ldflags '-linkmode=external' ./cmd/capsulad-cli -run 'TestDev|TestRunDev'` passed
  10/10 (`ok`, 0.124 s) for chunked bodies, error statuses, request semantics, dial failure,
  deadline, `/state` output and the key command, and the whole `cmd/capsulad-cli` package
  passed. Against a real simulator daemon, `capsulad-cli dev state` was byte-identical to
  `curl --unix-socket … /state` (2,660 B, chunked, no framing in the output) and key/wakeword
  worked; a fake-UDS gRPC smoke returned correct `status`, `app list`, `app runs` and three
  streamed `logs` records. This is the host cross-build; no device executed the artifact.
- **Icon font subset and embedded dist (host, 2026-11-18).** `web/src/assets` now carries a
  40,336-byte Material Symbols Rounded subset: 90 glyphs made of 30 icon ligatures, their 20
  `.fill` alternates, `.notdef` and 39 ASCII components (space, period, underscore,
  `digit_zero`–`digit_nine`, lowercase `a`–`z`; uppercase maps to the lowercase glyphs), with
  FILL/GRAD/opsz/wght intact.
  `icon-font.json` pins source package 0.48.3 and its SHA-256, the output SHA-256/size, the
  glyph and fill lists, and the tool versions. `cd web && npm run check:icons` printed
  `icon font ok: 30 icons (20 filled alternates), 90 glyphs, 40336 bytes (sha256 934d0bc5…)`;
  `npm test` passed 56/56 with 0 skipped, run with `CAPSULAD_FONT_PYTHON` pointing at a venv
  built from `scripts/requirements-fonts.txt` (fontTools 4.68.0); without that toolchain the
  generator test skips with an explicit reason instead of passing silently. The suite covers
  fontkit one-glyph shaping, the fill-alternate source-set helper, a test invoking the real
  `checkIconFont()` gate, axes, the `fluorescent`-missing rejection, the full-family guard
  (bare specifier, every `material-symbols/…` subpath, `require()`/dynamic `import()` and
  `@font-face url()` forms, with the local subset still allowed), the self-closing `<Icon />`
  and unclosed-tag rejections, commented-out template markup being ignored, the byte-identical
  licence copies, and `build-icon-font.py --check --require no_such_material_symbol_glyph`
  exiting 1 with the name in the error while the font and manifest stay byte-identical
  (the icon-font file alone: 13 tests; 1 more in `icon-font-generator.test.mjs`).
  `npm run check:icons` also enforces the pinned package version and source/output hashes,
  every declared glyph, and missing/stale `.fill` alternates against the source font.
  `npm ci && npm run build` emitted one WOFF2 and reproduced the previous hashed filenames
  (`index-80ec97ca.css`, `index-e83816c1.js`, `TimezonePicker-ba7a5968.js`). The checked-in
  `internal/webui/dist` dropped from 13,907,163 to 985,202 bytes
  (12,921,961 B = 12.32 MiB, 92.9%): the 972,557-byte asset tree plus
  `LICENSE-material-symbols.txt` (11,358 B, Apache-2.0, sha256 cfc7749b…) and
  `THIRD-PARTY.md` (1,287 B), copied from `web/public`, so the embedded tree a release ships
  carries the font notice and serves it at `/LICENSE-material-symbols.txt` and
  `/THIRD-PARTY.md`. `index-80ec97ca.css` references
  `material-symbols-rounded-subset-934d0bc5.woff2`, the outlined/sharp families are gone, and
  `TestEmbeddedDistServesFontLicense` fetches the licence through the SPA handler and
  byte-compares it with the embedded copy.
  A static `linux/arm64` daemon built with `CGO_ENABLED=0 -trimpath -ldflags '-s -w'` is
  still 28,639,384 bytes — the same size as before the notice was embedded. Measured by
  building with and without the two new dist files: the 12,645 B of notice text is absorbed by
  existing section padding, so only the SHA-256 changes (`c6524993…` with the notice, against
  the 41,549,976-byte baseline, −12,910,592 B = 12.31 MiB). Headless WebKit compared
  all 30 names at FILL 0 and FILL 1 against the full 0.48.3 rounded font with ImageMagick
  AE = 0 and RMSE = 0; a harness loading the real built CSS and subset rendered 30/30 spans,
  byte-identical to the subset reference, and its server log requested only the subset CSS and
  WOFF2. Retaining the `.fill` glyphs is required: GSUB FeatureVariations swaps in the filled
  alternate at FILL ≥ 0.99, and a base-only subset renders differently. Not verified:
  Chromium/Android WebView (the managed Chromium screenshot path hung), an on-device browser,
  daemon boot/runtime, gzip/transfer size and a native glibc build.
- **Simulator smoke (fresh data directory).** `auth init` completed, `capsulad -health`
  reported healthy, and `capsulad-cli status` named Capsula Mini with 6 backends and 7
  resources. The Web UI served `/` with 200 and `<title>capsulad</title>`, `/healthz` returned
  200, the unauthenticated `/api/status`, `/api/cert.pem` and `/ws` returned 401, and SIGTERM
  exited 0.
- **Linux-only paths not executed here.** The BlueZ D-Bus fixture
  (`TestBlueZDbusMediaControlFixture`) and the Wi-Fi Linux paths were not run on this darwin
  host, and no device or radio was exercised. They add no host confirmation of the adapter,
  AVRCP, setup AP or scan paths; the device evidence above stands on its own.
- Browser behaviour beyond the runs listed above: jsdom and headless runs for login/session,
  CSRF, API-key flows, live control coalescing, unsaved-edit preservation, diagnostics ordering and
  the settings-document bridge. The 390 px and 849 px surfaces were exercised through a
  throwaway loopback HTTP→TLS/WebSocket proxy in a native Tern browser; the managed WebKit
  build crashed in EGL and Chromium failed its Mach-port launch, so a full desktop viewport
  and a physical phone are unverified.
- Wi-Fi manager logic: stock `wpa_supplicant.conf` import, priority-ordered association,
  previous-network recovery, scan capability and token expiry against fixtures; captive
  portal host/origin/token/subnet behavior against `httptest`.
- Avahi apply ordering against a portable fake responder and a barrier-driven private-bus
  adapter fixture.
- BlueZ `MediaControl1` object/method calls on the wire against a private bus; AVRCP metadata
  parsing and the media fallback through the real input manager.
- The overview header's green `connected` chip is host-tested only, in
  `web/test/overview-badge.test.mjs`: *badge is present and names both connected paired devices*,
  *badge is absent when no paired device is connected or the snapshot is unavailable*, *only an
  explicit connected flag counts, in snapshot order*, *unnamed connected devices fall back to their
  id, then to a placeholder*, and *tooltip text joins names with `, ` and is empty for no names*.
  The derivation lives in `web/src/stores/overviewBluetooth.js`: it reads `pairedDevices` from the
  overview snapshot only, requires `connected === true` (a `"yes"`, `1` or missing flag does not
  count), renders a `<span role="status">` chip next to the speaker-mode toggle, and titles it
  `Connected: <names>`. A badge rendered against a real phone in a real overview is
  **still unobserved**.
- The device serial wire path: `TestParseHardwareSerial` (real `Serial` line, spaces,
  `Serial Number`, missing field, empty value) and `TestGetInfoReportsCachedHardwareSerial`, with
  `TestReadHardwareSerialUnreadable` for a host that cannot report it. Launcher injection and
  omission are covered by `TestLaunchEnvCarriesHardwareSerial` and
  `TestLaunchEnvOmitsUnknownHardwareSerial`; Marusia precedence is pinned by
  `TestEnvSerialWinsOverFrameworkSerial`, `TestSerialSettingWinsOverEnvSerial`,
  `TestFrameworkSerialWinsOverLocalCPUInfo` and `TestSerialSettingWinsOverFrameworkSerial`.
  `TestSerialSourceLoggedOnceWithoutValue` checks source-only logging. The descriptor drift test is
  `TestEmbeddedFrameworkDescriptorCoversGeneratedFields`; `TestDescriptorGuardDetectsMissingField`
  removes `DeviceInfo.hardware_serial` from an in-memory stale descriptor and confirms the guard
  reports it. Apps on older native runtimes must use `CAPSULAD_DEVICE_SERIAL`; the deployed library
  can lag even when the repository's embedded descriptor is current.
- QuickJS/MicroPython smoke coverage: ready, unary, events, routed unary, routed stream and
  cancellation, Stop/SIGKILL cleanup, and a daemon-crash phase delivering exactly one bounded
  terminal disconnect per host.
- Provenance verification against genuine Sigstore/GitHub fixtures; the settings-UI route
  returns both built-in documents with the expected headers and CSP.
- All packaging, manifest validation, extraction limits, settings CAS, log rotation and
  supervisor policy paths.
- The master-volume ceiling through the public API (`CGO_ENABLED=0 go test ./internal/daemon`,
  simulator sink, startup cue marker present so the jingle cannot overlap): full-scale S16 app
  playback measured **16383 at master 0.50**, **6553 at 0.20** and **3276 at 0.10** (exactly
  master × full scale). At master **1.00** the same stream peaks **31128 (0.950 FS)** — the
  limiter threshold, not the int16 rail. A documented `PCM_F32LE` chunk at **amplitude 10** and
  master **0.10** measures **3276**, so out-of-range float input is clamped instead of defeating
  the master. Two different apps playing full scale at master **0.90** measure **31128
  (0.950 FS)**, not the 1.000 FS that post-sum clipping produced; the fifth concurrent playback
  stream of one app returns `RESOURCE_EXHAUSTED`. At the mixer level, four concurrent full-scale
  streams from one app at master 0.60 peak at the 0.60 ceiling. The architecture guard
  (`TestOnlyMixLoopWritesPCM`) parses every Go file and fails on a `WritePCM(` call outside
  `mixLoop`.
- The packaged `apps/dumbassistant/assets/reply.wav` was re-encoded at the framework cue's
  asset level: 44.1 kHz stereo S16, RMS **−18.90 dBFS** (was −13.17), peak **−10.66 dBFS**
  (was −4.94). This is an asset measurement, not a listening test.
- The PulseAudio sink pin runs against a scripted `pactl` placed first on `PATH`: it sets
  `master` to 100 %, reads every channel back, and makes one more attempt; a persistent mismatch
  warns exactly once through the backend's `Warnf` hook (so it lands in the rotated log/Web UI as
  component `audio`) and a later volume change retries the pin, success logs one Info line, and a
  missing `pactl` degrades with one warning. The `get-sink-volume` parser is covered by its own
  test. No real PulseAudio sink was changed.
- Bluetooth host coverage: the local SBC capability IE is asserted to be exactly four octets
  with decodable frequency/mode/block/subband/allocation nibbles, bitpool 2..53 and
  `min ≤ max`, and `SelectConfiguration` is exercised against vendor SEPs and against the
  local blob. The session assertion is pure over a fabricated object map: a device with no
  `MediaTransport1` reports `audio link is not A2DP`, an idle/pending/active A2DP transport
  verifies, and the profile request runs once per source, is skipped for a healthy or non-audio
  link, honours the connection generation, waits for the profile request to finish, is bounded by
  the two-minute window, and stops after a failed request with the source still connected
  (`TestBluetoothSessionProfileRequestRunsOncePerSource`,
  `TestBTSessionGuardAdmitsOneRequestPerWindow`). The volume target prefers the transport this sink
  acquired, falls back to the object-tree transport and then to the source's A2DP transport, and
  reports `ErrNoAbsoluteVolume` exactly once per source session. A source-reported level never
  moves the master, and a master change is still forwarded to the transport
  (`TestBluetoothVolumeSourceReportNeverMovesTheMaster`). Toggle direction is covered against a
  stale `MediaPlayer1.Status` with and without owned-transport receipts, against the two-second
  command memory (per device, bounded, expiring, never consulted by explicit commands), and
  through the real D-Bus command path on a private bus.
- The Bluetooth connection cue is exercised end to end on the simulator:
  `TestBluetoothConnectionCue` (run under `-race` with `-ldflags '-linkmode=external'`) proves the
  mixer sink carries a non-zero tail after a source connects, the cue playback stream drains, and a
  disconnect opens no playback stream. The embedded cue set also decodes as non-silent 16-bit PCM
  (`TestEmbeddedCuesDecode`, which includes `question.wav`, under the same flags). This is
  simulator digital output, not acoustic level, and no real source was used.
- The centre-gated contract is covered by pure `panelPress`/`panelRelease` timelines and by the
  real input manager: `TestVolumeStepResolvesOnPressWithoutDeferral` pins that a bare volume press
  steps immediately with no deferral; `TestCentreEvidenceSelectsTheSwipe` selects the swipe from
  the centre; `TestBareVolumeTapsStaySteps` keeps opposite taps farther apart than the full-panel
  window as two steps. The full-panel crossing has its own coverage:
  `TestFullPanelSwipeRightRevertsTheLeadingStep` and `TestFullPanelSwipeLeftRevertsTheLeadingStep`
  prove the gesture dispatches one media command, suppresses the completing edge and restores
  exactly the level captured before the leading step; `TestFullPanelSwipeAcceptsASlowHandCrossing`
  proves a 330 ms real-hand crossing still resolves `tier=full` inside the 400 ms window with
  exactly one revert; `TestRepeatedDirectionPrefixStaysSteps` proves two same-direction taps plus
  the opposite edge never resolve as a crossing;
  `TestSlowOppositePairStaysTwoSteps` pins the 400 ms boundary; `TestCentreBetweenEdgesKeepsTheCentreGate`
  proves a centre contact between the edges keeps the centre-gated semantics with no revert; and
  `TestFullPanelSwipeWithoutAnAppliedStepStillDispatches` proves an unrevertable step (a bound)
  still dispatches exactly once. `TestFullPanelSwipeRevertsThroughMasterVolume` drives the real
  daemon and confirms the revert is persisted through `applyMasterVolume` to the pre-step value.
  An edge pressed while the centre is held (no press-gap allowance) or within 700 ms of its release
  resolves `swipe_right` with one media command and zero volume steps while the centre keeps its own
  action (`TestCentreGatedSwipeSuppressesTheEdgeStep`, `TestCentreGateBoundary`,
  `TestHeldCentreGatesDelayedVolumeEdges`, `TestHeldCentreCannotExceedGestureBudget`,
  `TestHeldCentreAcceptsBothVolumeEdges`, `TestReleasedCentreGateMeasuresFromTheRelease`,
  `TestCentreChainStaysOneGesture`, `TestConsecutiveChainsDoNotDoubleFire`, `TestPartialSwipeDeliveredWithoutSystemEffects`,
  `TestFullCaptureForwardsKeysAndCentreGesture`); `TestVerticalPairStaysDeliberate`,
  `TestPerimeterTraversalWithoutCentreIsKeys` and `TestPartialCaptureNeverInfersCircles` keep
  non-gesture contacts as keys; circles are resolved only by the full-capture shadow
  (`TestFullCaptureInfersCircleTraversal`); `TestCentreDecisionLoggedAtInfo` pins the Info-level
  `tier=centre|full|overlap|chain|keys` lines and the 250 ms `window=`. The full-capture recognizer
  delivers the same gesture alongside the raw keys, and `TestDelayedReadKeepsPhysicalGap` /
  `TestDelayedPressKeepsPhysicalHoldDeadline` keep kernel-timestamp windows. On-device recognition
  by the deployed resolver is still to be confirmed by physical presses.
- The Web UI's Bluetooth provider is exercised on the simulator without any provider app: with
  a source connected, the overview renders one framework-owned provider with player
  `bluetooth`, `PLAYBACK_PLAYING` from the framework's resolved direction (the source
  publishes no AVRCP status in the fixture), its metadata and
  PLAY/PAUSE/SKIP_NEXT/SKIP_PREVIOUS; the browser's command reaches
  `BluetoothMediaService.Command`; with no source the overview lists no Bluetooth provider at
  all. The daemon side proves the same provider is not an application: it has no entry in
  `ListApplications`, and the physical key path with no
  provider still drives the source through the public service. The rendered Overview was not
  observed against a physical phone. `TestOverviewRendersConnectedBluetoothSource`,
  `TestOverviewOmitsBluetoothProviderWithoutSource` and `TestFrameworkMediaCommandUsesBluetoothService`
  pin the provider kind, the single `bluetooth` player and the command path.
- The caller-identity matrix is covered by host tests. `TestVirtualRunIdentityRequiresDaemonPeer`
  proves a virtual run is claimed only by a peer that sends its stable run ID together with the
  per-boot internal token: the daemon's own connection passes, and a peer presenting the same run ID
  from another PID fails through the existing `SO_PEERCRED` check. `TestAdministrativeRPCsRejectApplicationIdentity`
  and `TestRunListingRejectsLapsedApplicationIdentity` keep app scope and administrator scope apart,
  and `TestBuiltinWatchReportsStoppedRun` plus `TestLaunchDoesNotSucceedDuringBuiltinStop` cover a
  stopped built-in's run state and the one-run Launch path. On the browser side,
  `web/test/socket-request-error.test.mjs` proves a refused request or subscription settles only
  itself, keeps its inline error, and leaves the socket state and its epoch open with no reconnect.
- The built-in boot rule is covered directly. `startSystemApps` starts a registered built-in only when
  it is explicitly framework-owned (the clock) or selected in persisted settings (the ring), so
  `TestFrameworkOwnedClockStartsWithNoProviderSelections` boots a store with no resource selections and
  asserts the clock run is READY while the unselected ring has no run and the segment backend receives
  the all-zero blank frame. `TestExplicitNoRingProviderSurvivesRestart` boots and restarts from a store
  whose ring entry is missing or explicitly empty and asserts the ring stays unowned with an all-zero
  frame, plus that selecting the ring after boot starts exactly one run and the clock keeps its single
  run. `TestDeselectingRingStopsOnlyRingAndReselectionStartsIt` deselects a selected ring and asserts
  its run disappears while the clock run and a running timer survive, then reselects it and gets one
  fresh READY run. The clock's stream-failure recovery is now generation-scoped rather than a
  `sync.Once` reset: `TestBuiltinClockFailureIsScopedToRunGeneration` proves a late callback from an
  older generation leaves the newer run untouched, a legitimate failure restarts it once, and a
  duplicate callback for the same generation records nothing. There is no separate host test that
  asserts the administrator run table omits an unselected ring run, so the run-table half of the rule
  rests on the code path plus open gate 19.
- The Bluetooth volume rule is covered at the daemon and was settled by device listening:
  `TestBluetoothVolumeSyncIsOneWay` feeds three source reports and asserts the master follows each,
  then applies a master change and asserts the local gain moved while the register was never
  written. Two earlier designs were rejected on the unit, not in theory: a two-way mirror made
  10 % + 10 % near-silent, and compensating the local trim for the peer's share made 5 % far too
  loud because the peer reports the written value back without attenuating the stream.
- Multi-word wakeword detection has host evidence only: the model store and its legacy
  migration (rename, byte-identical artifact, invalid artifacts reported not replaced),
  per-word enablement and binding resolution including the legacy `nil` semantics,
  ensemble↔single-model score parity on a stored embedding fixture, deterministic
  arbitration and the single global cooldown, delivery to a bound app with no rerouting when
  it is unready, and install/delete isolation (other artifacts byte-identical, new word
  installed disabled, takes kept, a fresh install into the legacy state materializes the new word
  as disabled, an explicit empty list survives write/read/re-encode, an unreadable legacy artifact
  is advertised as needing attention, and an activation is delivered end to end through the app
  bus; a detection keeps its matched artifact id while a manual activation is delivered as
  `ACTIVATION_TOUCH` with an empty matched model id
  (`TestDeliverManualActivationCarriesNoMatchedModel`)). Open device gates: per-word acoustic
  acceptance on the target hardware, the real ensemble cadence on the RK3308 (one frontend + N
  heads) and the roughly two-second all-word warm-up that follows an install or delete. Nothing
  here proves recognition accuracy or a daily false-alarm rate for any individual word.
- `apps/marusia`: the recovered schemas regenerate from the vendored `.proto` files; the
  attach request shape (method, path, `Accept`/`Content-Type`, the three JSON fields), the
  per-call metadata set with a fresh request UUID, the init→chunk⁰…chunkⁿ→eos sequence,
  abort on dismissal, a server status error and stream teardown are exercised against
  in-process TLS servers. The turn state machine is exercised against fake streams and
  players for ordering, hold-to-talk release, no-speech, server abort and player failure. No
  test contacts an external host; the protocol tests use loopback TLS listeners. The session
  credential path has its own host coverage: `TestResolveSessionPrecedence` pins the order
  (masked `session_id`/`session_secret` settings, then the vendor session file, then no session),
  `TestAuthorizeUsesSessionWithoutAttach` proves a configured session is validated with no attach,
  `TestAuthorizeDoesNotReplaceRejectedSession` and `TestTurnMarksRejectedCredentials` cover a
  rejected credential being reported rather than swapped for another, `TestUploaderFraming` asserts
  the one-self-contained-WAV-per-80-ms-chunk framing, `TestEOSLeavesTheRequestSideOpen` pins that
  `eos` does not close the request side, `TestBuildInitKeywordMediaState` pins the required
  `kw_params.media_state`, `TestOpenSpeechRefusesUnimplementedOpus` keeps `ogg_opus` an explicit
  error, `TestTurnReportsNoAnswerInsteadOfSuccess` keeps a clean phrase with no text and no command a
  no-answer, and `TestPumpMicLogsFormatOncePerStream` pins the once-per-stream microphone-format log.
  `TestEndpointerAccumulatesIntegratedRMS` pins that the integrated level is computed from the
  samples the gate already reads (digital silence at the floor, a square wave analytically, mixed
  energy by 3 dB); `TestTurnLogsAudioSummary` pins the per-turn `turn audio` line, including the
  uploaded chunk/duration count, pre-open loss and the service's `chunks_received`,
  `audio_received_ms` and `status_num`; `TestTurnNoAudioOutcome` pins that silent PCM and a turn that
  uploaded nothing become **no audio** with a `WARN`, not a recognition no-answer; and
  `TestPumpMicReportsDroppedPreOpenChunks` pins the pre-open loss counter that line reports.
  `TestSpeechOptionsClaimAKeywordOnlyForAWakeword` pins that only `ACTIVATION_WAKEWORD` claims the
  vendor keyword: a touch activation — even one carrying a stale match — and an API activation send
  no keyword, so a key press cannot be reported as a heard word.

  Proof level: request bytes, session resolution and protocol ordering only; the diagnostic line's
  numbers are host-test evidence, not device measurements.

- **`apps/cadence`: voice-assistant client for a Cadence server (host only).** The app speaks
  the upstream `cadence.v2` device link over the shared SDK's verified TLS stream: `Hello`
  (protocol 2) → `Welcome` → an empty `CapabilityReport`, then `SessionStart` / `MicAudio` /
  `SessionCancel` up and `SessionEvent` / `StreamStart` / `AudioDown` / `StreamEnd` /
  `SessionClose` down. It is a voice-only device: it advertises no capability; it refuses
  announcements (they play outside any activation), capsulad API activations and a server
  `MicOpen`, each with an explicit log. `cadence.v2` vouches owner privileges only for a wake
  word or a press, and an automation trigger (or another screen's press) is neither, so sending
  `TRIGGER_BUTTON` for them would fabricate physical owner evidence — the conservative choice
  was made deliberately and is documented in apps/cadence/README.md. Interoperability is
  pinned by the upstream golden frames, copied verbatim from
  `proto/fixtures/v2/*.binpb` (upstream `main` commit `f9571ad`) into
  `internal/proto/testdata/upstream/`: all 17 decode with this client's generated messages and all 17
  re-encode byte-for-byte to the upstream bytes, so field numbers, wire types and enum values are the
  real wire, not a guess. The `.binpb` files are data test vectors, not code; the proto subset
  here is independently written to the same field numbers, and no upstream source is vendored. The websocket
  framing (client handshake accept key, 401, no extensions, masked client frames, fragmentation,
  ping/pong, close frames, the 16 MiB cap) is exercised against an in-process `net.Pipe` peer, and
  the real `link.Client` plus websocket upgrade is driven end to end against an **independent
  loopback fake upstream** (HTTP upgrade, masked frames, Hello→Welcome→empty report, then a server
  `SessionClose` dispatched to the handler) — no remote service is involved.
  The Opus codec is a real libopus round trip — 16 kHz mono 20 ms encode, 48 kHz decode, the
  frame size enforced — measured with Homebrew libopus 1.6.1 on this host; on the device the
  codec is `dlopen`ed (`libopus.so.0` is present in the stock system image's `/usr/lib`), so the
  app has no link-time codec dependency. The turn state machine is driven against fake
  link/audio/indication/codec seams: wake and touch triggers, mic uplink and sequence, reply
  playback and drain versus abort, follow-up reuse of the microphone, the hold-release silence
  tail, dismiss, error close, link loss, API refusal and expired activations. The capture
  obligation has its own seam test: presentation → non-blank first frame → `OpenCapture` with the
  presentation id and `CaptureModeUnspecified` → capture released before the presentation, and a
  refused capture closes the presentation; the daemon side (selection, freshness, visible
  content) is the framework's `internal/daemon/indication_test.go`. A local failure (codec,
  encode, uplink, unsupported capture) shows a distinct red ring for 300 ms; mute and a lost
  indication deliberately leave the framework's own state instead.

  **Integrated host proof (real app + real daemon + loopback fake upstream).**
  `internal/daemon/cadence_e2e_test.go::TestCadenceCaptureAndMicOverLocalServer` builds and
  installs the actual package, selects it as the assistant, points `server_url` at a loopback
  websocket that answers the upgrade and a hand-encoded `Welcome`, triggers a hold-to-talk touch
  activation, and asserts, in order: the app logs `link_up`; `router.Current("light.ring.main").SelectedAppID`
  is the app (the **capture grant**); the simulator backend shows a **non-blank teal ring frame**;
  and `inputOpen(app)` is true (the microphone stream opened, i.e. `OpenMic` succeeded). It then
  calls the manifest `dismiss` action and asserts the microphone is released, the capture is no
  longer selected, and `turn_end activation=` is logged, with no `mic_failed`. Run:
  `go test -race -ldflags '-linkmode=external' ./internal/daemon -run TestCadenceCaptureAndMicOverLocalServer`
  (passes here, 6.25 s). This is the runtime boundary the earlier defect crossed; it is a
  permanent regression test. Proof level: host simulator with the real app, daemon and SDK — it
  does not prove device audio, LED gamma or a real Cadence server.

  Checks actually run in this tree: `make test-cadence` (8 test packages, 0 failures) and
  `go test -race` on `internal/turn`, `internal/link` and `internal/websocket` (0 races) on the
  host. Genuine Linux arm64 package proof was produced in arm64 containers, not on the darwin
  host: `golang:1.22-bullseye` (glibc 2.31) with `libopus0` built and tested the app and staged
  `abi = "linux-gnu"`; the artifact's interpreter is `/lib/ld-linux-aarch64.so.1`, its highest
  required symbol is **`GLIBC_2.17`** (`objdump -T`, so ≤ 2.33), and its `DT_NEEDED` entries are
  only `libdl.so.2`, `libpthread.so.0` and `libc.so.6` — Opus is `dlopen`ed, never linked.
  Offline inventory of the supplied images confirms the dlopen prerequisite: Rockchip stock and
  Amlogic stock carry `/usr/lib/libopus.so.0.8.0`, Alpine 3.24 carries the `opus` apk 1.6.1-r0 at
  `/usr/lib/libopus.so.0`; the Amlogic stock glibc is 2.28, above our `GLIBC_2.17` requirement. The
  package was written by the host `capsulad-cli` (`build/evidence/…-linux-gnu-arm64.tar.gz`), so
  the packager never rewrote the manifest. The darwin host binary is refused by `make package`
  outright: the manifest declares a Linux ABI, and the mislabeled host-staged directory was
  removed. `tools/check-elf-interp.sh` is the gate.
  The musl counterpart was built the same way in `golang:1.22-alpine` with `gcc`, `musl-dev` and
  `make`, and tested with the **actual device codec**: the Alpine 3.24.1 rootfs's
  `libopus.so.0.11.1` was bind-mounted and selected with `CAPSULAD_OPUS_LIBRARY`, so the Opus
  suite ran against the image's library (no skip). `make check-abi` printed
  `interpreter=/lib/ld-musl-aarch64.so.1` and staged `abi = "linux-musl"`; the binary's only
  `DT_NEEDED` is `libc.musl-aarch64.so.1` (musl 1.2.5; no `libcapsulad`, no `libopus`), and the
  full suite passed. The archive was written by the host `capsulad-cli` to
  `build/evidence/org.capsulad.cadence-0.1.0-linux-musl-arm64.tar.gz` (1,527,837 bytes) and the
  extracted content re-checked (`ELF aarch64`, `/lib/ld-musl-aarch64.so.1`,
  `abi = "linux-musl"`). `go list -deps ./cmd/cadence` names neither `net/http`,
  `crypto/tls` nor `google.golang.org/grpc`; the only transport is `capsulad.OpenTCP` plus this
  app's RFC 6455 framing. Proof level: protocol bytes, codec DSP and the client state machine on
  the host. **Not** established: behaviour against a real Cadence server (none runs here), the
  framework's real capture indication and physical audio, LED gamma, and acoustic echo during a
  follow-up. Those need an enrolled device and a deployed server.

- **Native SDK network helpers and primitives (host only).** `libcapsulad` now owns the
  outbound path: `src/http.rs` (bounded HTTP/1.1 with verified TLS 1.2+, one request per call
  and per-request `http_cancel`), `src/tls.rs` (system PEM roots plus webpki, no insecure
  mode), `src/grpc_remote.rs` (a tonic channel over TLS/h2 or h2c prior knowledge),
  `src/primitives.rs` (base64, RFC 3986 percent coding, SHA-256, getrandom, UUIDv4 and
  absolute-URL decomposition) and `src/headers.rs` (strict header and metadata maps). The C
  ABI appends the new entry points and `CAPSULAD_STATUS_TOO_LARGE` without reordering the
  table, so a binding built against the older prefix keeps its identity. Coverage:
  `cargo test --manifest-path sdk/native/Cargo.toml` exercises the primitives, the URL
  parser, HTTP over real loopback sockets (content length, chunked bodies, redirects,
  cross-origin header stripping, size caps, timeouts, TLS verification against the
  `testdata/` CA) and a remote-gRPC connection failure; the Go binding adds
  `TestHTTPOptionsEncode`, `TestGRPCOptionsEncode`, `TestPrimitivesAgainstNativeLibrary` and
  `TestHTTPCancelIsPerRequest`. Every listener is a loopback fixture; no external host is
  contacted. Proof level: request bytes, cancellation and TLS verification on the host, not
  device DNS, clock or trust-store behavior. These suites were run for this verification:
  `make test-sdk` exercised the Rust primitives, HTTP, TLS and remote-gRPC coverage and the
  Go binding tests above, and `sdk/go go test ./...` passed 22 tests with
  `CAPSULAD_TEST_LIBRARY` set (0 skips) at that point; the current networking run reports its own
  counts below.

- **Marusia delegates networking to the shared SDK (host builds).** `internal/vkclient`'s
  production transport calls `capsulad.OpenHTTP` and `capsulad.OpenGRPC`;
  `go list -deps ./cmd/marusia` names neither `google.golang.org/grpc`, `net/http` nor
  `crypto/tls` (observed in this tree). The recorded host builds of `cmd/marusia` are
  15,875,602 bytes at HEAD and 6,436,386 bytes here: the 9,439,216-byte (~59.4 %) difference is
  the removed grpc-go/net-http/TLS machinery, not a compression change. A stripped release build
  of the same tree is 4,249,810 bytes. The production transport
  is exercised only by the gated integration tests `TestProductionSDKTransport` and
  `TestProductionSDKTransportCancelsSlowAttach`, which run against in-process loopback TLS
  servers and are skipped unless `CAPSULAD_TEST_LIBRARY` names a staged `libcapsulad`:

  ```sh
  make native
  cd apps/marusia
  CAPSULAD_TEST_LIBRARY=$PWD/../../build/lib/libcapsulad.1.dylib \
    go test ./internal/vkclient -run TestProductionSDKTransport -v
  ```

  Proof level: host dependency graph, host file sizes and loopback protocol bytes. It does
  not prove that the sandbox's outbound TLS reaches the vendor endpoint (open gate 16), and
  the size figure is a host binary, not the packaged aarch64 app. Both gated tests were run
  here with `CAPSULAD_TEST_LIBRARY` pointing at the staged host library and passed; the plain
  and gated Marusia suites also passed under `-race` (5 packages, 0 races). On HEAD `0727796`,
  `go test ./...` and `go test -race ./...` in `apps/marusia` passed **7 packages** with
  **0 data races**.

- **Native SDK streams, UDP, listeners and mDNS (host only).** `libcapsulad` carries the
  networking surface in its **ABI 1.0 pre-release** table (`tcp_open`, the stream functions,
  UDP, the daemon-mediated listener functions and the mDNS calls) plus
  `CAPSULAD_STATUS_UNSUPPORTED = 16`. The ABI identity is not bumped: `make native` produced a
  **536-byte** `capsulad_api` table, `capsulad_version()` reports **`1.0.0`**,
  `capsulad_abi_major()` is **1**, `capsulad_abi_minor()` is **0** and the emitted runtime
  metadata names **minor 0**. The new pointers are appended after `http_cancel`; a table sized
  for the pre-networking v1.0 prefix is still accepted
  (`api_reports_v1_0_and_old_tables_still_load`) and a major-2 request is rejected with
  `UNSUPPORTED_ABI` without writing the table. This is the append-only rule in `AGENTS.md`, not
  a minor bump.
  `sdk/native/tests/net.rs` covers, against loopback and fixture sockets:
  `api_reports_v1_0_and_old_tables_still_load`, `tcp_round_trip_eof_and_shutdown`,
  `tcp_timeout_leaves_stream_usable_and_close_unblocks`, `tcp_open_validates_arguments`,
  `tls_stream_verifies_the_embedded_ca`, `udp_unconnected_send_recv_and_timeout`,
  `udp_connected_send_recv_and_close_unblocks`,
  `udp_rejects_bad_peer_and_oversized_datagrams`, and a fake daemon over a real `AF_UNIX`
  `SOCK_SEQPACKET` pair for `listener_open_accept_over_real_control_channel`,
  `listener_udp_recv_and_send_over_control_channel`,
  `listener_open_without_control_channel_is_unsupported`,
  `listener_open_with_invalid_fd_is_unsupported`, `listener_policy_error_is_surfaced`,
  `listener_close_unblocks_accept`, `listener_options_are_validated` and
  `mdns_validates_handles_and_requests`. The daemon side is covered by
  `internal/appcontrol` (`TestTCPListenerFDIsUsable`, `TestUDPListenerFDIsUsable`,
  `TestPolicyRejections` for 5353, the Web UI port, multicast/broadcast and the port range,
  `TestMaxListenersPerRun`, `TestOversizedRequestIsRejected`,
  `TestCloseRunReleasesSocketAndAllowsRelaunch`, `TestSendErrorClosesDupWithoutLeak`) and
  `internal/supervisor` (`TestControlChannelPassesFDAndEnv` proves the descriptor is fd 3 with
  `CAPSULAD_CONTROL_FD=3`; `TestControlChannelAbortOnFailedSpawn` proves a failed spawn releases
  it). Discovery is covered with a fake responder: `internal/discovery`
  (`TestNormalizeService`, `TestNormalizeQuery`, `TestServiceManagerPublishesAndBrowses`,
  `TestUnsupportedServicesAreExplicit`) and `internal/daemon`
  (`TestDiscoveryPublishIsScopedToRun`, `TestDiscoveryAuthAndFailureMapping`,
  `TestDiscoveryBrowseStreamsEvents`, `TestRemoveRunPublicationsOnlyRunOwned`). The Go binding
  adds option/status mapping (`TestTCPOptionsEncode`, `TestUDPAndListenerOptionsEncode`,
  `TestMDNSCanonicalJSON`, `TestNetworkStatusMapping`) and loopback suites
  (`TestTCPLoopbackEcho`, `TestUDPLoopbackEcho`, `TestTLSLoopbackEcho`) that are gated on
  `CAPSULAD_TEST_LIBRARY`; `TestListenerPolicyUnsupported` and `TestMDNSPublishBrowse` need an
  environment without a control channel, and `CAPSULAD_TEST_MDNS=1` with a supervised run id and
  a live Avahi, respectively, so they are not host evidence. The Go binding
  identity is checked by `sdk/go/regenerate-abi.sh --check`, which pins ABI minor 0 and status
  16.

  Runs recorded for this verification (host only, no device):
  `cargo test --manifest-path sdk/native/Cargo.toml` passed **88** tests (lib 40, http 23,
  net 25); `sdk/rust` passed **25** (lib 21, network 3, doc-test 1); the gated Go SDK suites with
  `CAPSULAD_TEST_LIBRARY` reported **37 pass, 0 fail, 1 skip** (`TestMDNSPublishBrowse`, as
  above); the focused `internal/appcontrol`, `internal/discovery`, `internal/supervisor` and
  `internal/sandbox` suites passed **85** tests; the daemon discovery subset
  (`go test ./internal/daemon -run 'TestDiscovery|TestRemoveRunPublications'`) passed **5/5**;
  the full `internal/daemon` package passed **211** with the one known failure below.
  `make test-sdk` exited **0**; `make test-go` reported **33 packages ok** with that same single
  failure; `make test-race` also reported **33 packages ok**, **0 data races**, and the same
  single failure.

  Every listener and peer is a loopback fixture or a fake daemon; the responder is a fake, not
  Avahi. Proof level: descriptor transfer, policy enforcement, stream/datagram bytes, timeout and
  close behavior, ABI table negotiation and TLS verification on the host. It does **not** prove
  device DNS, the device trust store, LAN reachability or Avahi visibility — see gates 22–24.

- **Bluetooth advertised name (unit, simulator and WebSocket).** `ValidateAdapterName` is the
  single rule shared by the real manager, the simulator and the Web UI: trim, non-empty,
  valid UTF-8, at most 248 bytes, no control characters. Unit coverage:
  `TestValidateAdapterName`, `TestMemorySetAdapterName`, and `TestSetBluetoothAdapterName`
  at the WebSocket command layer — the trimmed name reaches the manager, the result carries
  the observed adapter status and detail, empty/whitespace/control/over-limit names are
  invalid arguments that never reach the manager, an unreadable status is `UNAVAILABLE`, and
  a missing manager is `UNAVAILABLE`. A headless-Chromium run against a freshly built
  simulator daemon (Web UI on `127.0.0.1:9443`, admin session, no console/page errors)
  observed the Settings → Bluetooth "Device name" row with the simulator alias `Capsula`, a
  pencil button named "Change Bluetooth device name", the "Change device name" modal with a
  `7 / 248 bytes` counter (Save disabled at `0 / 248`), and a save of
  `Living Room Capsula` closing the modal, updating the row and showing
  "Device name changed to Living Room Capsula.". The setup-AP boundary is unit-tested by
  `TestWifiAPNameTruncatesLongAdapterAlias`: 32 bytes unchanged, 33+ bytes cut to the first
  32 bytes on a rune boundary, a straddling multi-byte rune dropped, and a 248-byte alias
  still accepted for Bluetooth. The BlueZ `Adapter1.Alias` write itself is covered only by
  the cross-compiled D-Bus fixture (`TestBlueZDbusMediaControlFixture`); it is not a
  physical-adapter observation unless the docker/D-Bus harness was actually run. No Wi-Fi
  radio was exercised, so AP SSID truncation on a real radio remains unobserved.

- **Wi-Fi scan result unwrapping (WebSocket-level).** The Web UI helpers in
  `web/src/lib/wifi.js` unwrap the daemon's `Result` → `WifiResult` → `WifiView` nesting and
  treat a missing view as a protocol error instead of rendering an empty network list.
  `web/test/wifi-scan.test.mjs` drives the shared `socket.handleFrame` with real encoded
  `ServerFrame`/`Result` messages: scan, connect and forget resolve to the `WifiView` with
  `scanResults` intact, and a result without a view rejects. The test passed in this tree
  (`node --test web/test/wifi-scan.test.mjs`, 4/4). Proof level: WebSocket frame handling
  and helper return values; no physical radio and no rendered scan list, because the
  simulator reports that Wi-Fi is unavailable on this speaker.

- **Hardware profile separation (host/simulator).** Board literals moved into the data-only
  `internal/backend/profile` package: `TestRK3308Rev4StockLiterals` pins every field the
  framework previously hard-coded and `TestSimulatorHasNoHardwarePaths` keeps the simulator
  free of device nodes and stock files. `internal/wifi` takes supplicant paths, control
  directories, restore file and stock adoption through `wifi.Config`
  (`TestConfigDefaultsAreNeutral`, `TestConfigExplicitStockInjection`,
  `TestStockAdoptionDisabledSkipsImport`), and the daemon and CLI feed those values from the
  selected profile; `internal/wifi` imports no board profile. The offline inspector moved to
  `internal/backend/rockchip/inspect`; `TestInspectGolden` pins its JSON report and
  `TestInspectRejectsNonDirectory` its error contract. Proof level: host unit and golden
  tests plus simulated behavior-preserving suites (`make test`, with the known pre-existing
  failure below). No device access, and none of these tests exercises a hardware path:
  Rockchip device nodes, the vendor LED ABI or the watchdog remain device-only paths.

- **Hardware watchdog supervision (host, macOS arm64, Go 1.22.6, no device).** The supervisor's
  decisions are pinned with a fake timer and the real feeder process, all with `-ldflags
  '-linkmode=external'`, and the daemon cases with `-race`: `TestWatchdogFeedsWhileTheDaemonMakesProgress`,
  `TestWatchdogStopsFeedingWhenTheDaemonStalls` (a stalled progress gauge gets **zero**
  keepalives, the state turns unhealthy, and the later graceful stop neither hands the timer to
  a feeder nor closes it), `TestWatchdogStopsFeedingWhenTheKeepaliveFails` (breakage is not a
  `TestWatchdogHandsOffOnGracefulStopUnderNowayout` (the descriptor reaches a
  detached `/bin/sh` feeder and keeps receiving keepalives until the handback marker arrives),
  `TestWatchdogHandsOffAnUnmeasuredTimer` (a driver whose disarm semantics are unmeasured is
  handed off, never closed), `TestWatchdogClosesWhenNoDescriptorCanBeTransferred`,
  `TestWatchdogReportsACloseThatLeavesTheTimerArmed`, `TestWatchdogReportsACloseFailure` and
  `TestWatchdogReportsAHandoffFailure` (a release that did not stop the timer is never reported
  as a disarm), `TestWatchdogDoesNotReopenAfterDisarm`, `TestWatchdogRetriesALateDevice`,
  `TestWatchdogTimingStaysInsideTheArmedTimeout` (the derived cadence and gate stay strictly
  inside the armed interval for 2 s to 1 h, including the declared and assumed fallbacks),
  `TestWatchdogFeedsImmediatelyOnArming`, `TestWatchdogReportsAnUnreportedTimeout`,
  `TestWatchdogTickFollowsTheDerivedCadence` (the loop ticks at the derived cadence, not the
  probe grid), `TestWatchdogDisarmIsABarrier`, `TestWatchdogOpenResultAfterRetirementIsHandedOff`,
  `TestWatchdogKeepaliveFailureDuringReleaseIsNotBreakage`,
  `TestWatchdogWaitReturnsForADisabledSupervisor` and
  `TestWatchdogReportsAHandoffFailureOnALiveSupervisor` (release ordering, driven by channels
  rather than sleeps),
  `TestWatchdogBusyDeviceIsRetriedAndAskedToRelease`,
  `TestWatchdogCleansAStaleFeederPIDFile`, `TestWatchdogCleansAReusedFeederPID`, `TestFeederPIDParsing`,
  `TestFeederTicksBoundTheWindow`, `TestHandOffWatchdogShellFeeder` and
  `TestFeederStopsFeedingWhenItsWindowExpires` (a feeder with no daemon stops feeding and drops
  its pid file on its own, so it cannot mask a dead framework) in `./internal/daemon`, and
  `TestReleaseLegacyFeederRequestsHandbackOnlyForTheLegacyFeeder`,
  `TestReleaseLegacyFeederClearsADeadFeeder` and `TestReleaseLegacyFeederClearsAReusedPID`
  in `./cmd/capsulad-cli`. The procfs-dependent cases (`TestWatchdogCleansAReusedFeederPID`,
  both reused/dead deploy cases) skip on macOS and were run on Linux instead
  (`golang:alpine`, `go test`, no race detector): all passed, together with the feeder and
  handoff tests above.
  `TestOutputLoopReportsWatchdogLiveness` runs a full daemon and fails if the liveness gate is
  not fed by the real output loop, which is the wiring error that would reset every board.
  Proof level: supervisor state transitions, feeder process lifetime, handback/staleness
  handling, the derived timing and the `EBUSY` retry path. It does **not** prove a real reset:
  there is no `/dev/watchdog` access here, the RK3308 nowayout facts (ignored magic close,
  `WDIOC_SETOPTIONS` → `ENOTTY`, 21 s armed timeout) are the device probe results above and are
  not re-measured, and no host test can show that the ungated timer actually fires. The timing
  rule (`cadence = timeout/3 ≤ 2 s`, `window = timeout/2 ≤ 10 s`) is a design bound checked as
  arithmetic, not a measurement of any driver.

- **Live application install and removal (host tests and simulator, no device).** The live path is
  exercised end to end. Focused host tests on macOS arm64 (Go 1.22.6, all with
  `-ldflags '-linkmode=external'`): `./internal/artifact`
  `TestOpenArchiveRoundTrip`, `TestRejectsOversize`, `TestOpenReader*` passed;
  `./internal/webui` `TestAppsRoutesRequireBrowserSession`,
  `TestAppInstallStreamsArchiveAndReportsOutcome`, `TestAppInstallKeepsDaemonRefusalMessage`,
  `TestAppInstallRejectsArchiveOverTheTransportBound`, `TestAppRemoveAddressesOneApp` and
  `TestAppInstallHidesUnexpectedFailureDetail` passed; `./internal/daemon`
  `TestInstallPackageStreamActivatesInLiveRegistry`, `TestInstallAndRemovalRefuseRunningApp`,
  `TestInstallPackageRefusesBuiltinAndInvalidArchive`, `TestUninstallPackageRefusesBuiltinAndUnknown`,
  `TestInstallPackageDropsCachedProvenance`, `TestInstallApplicationServiceStreamsChunks`,
  `TestInstallApplicationRequiresOpenFrame`, `TestInstallApplicationRejectsOversizedStream` and
  `TestPackageRPCsRejectApplicationIdentity` passed; the `-race` run of the install/remove/RPC set
  reported `ok capsulad/internal/daemon 21.284s` with no race; `./internal/nativedescriptor`
  passed. Against a throwaway simulator daemon
  (`-platform simulator -sandbox none -watchdog off -supersede off`) with a test-only
  `test.smoke` package: a fresh install printed `installed test.smoke (1.0.0) into …` and
  `active now: the daemon reloaded its application registry`, and `app list` showed it immediately
  with **no restart**; reinstalling the same archive printed `reinstalled …` the same way, and a
  package *directory* source behaved identically; installing or removing while the app ran failed
  with exit 1 and `capsulad-cli: app test.smoke is running (run run_…); stop it before replacing
  the package` / `… before removing`; after `app stop` both succeeded; `app remove` printed
  `removed test.smoke` and `app list` then showed only the built-ins. A headless-Chromium session
  against that daemon (admin session, `https://127.0.0.1:18443`) observed the `/apps` page, an
  upload plus `Install` → toast `Installed Smoke app 1.0.0.` and a third table row, `Remove` → the
  dialog `Remove Smoke app (test.smoke) from this speaker?` and the row disappearing, an install
  over a running app → the inline alert `app test.smoke is running (run run_…); stop it before
  replacing the package`, and a live row update to `Native Running` after a CLI `app start`, with
  that row's Remove button disabled (`Stop the app on Overview before removing its package`) — no
  page reload and no console errors at any read. The offline fallback was exercised only in its
  no-daemon form: `installed … restart capsulad to activate the installed package`, and
  `app remove` after the daemon was killed. The simulator lifecycle and the browser session were
  both re-run after the later upload-supervision and route-hardening changes, with the same
  results. A real defect this run found — the live path dialed
  the raw `-socket` flag instead of resolving the data-directory socket — was fixed before the
  recorded results. Limits: no device run; `-sandbox none`, so this is no evidence about Linux
  namespaces or mounts; uploads were local and ~4 KiB, so no slow-link timing and no real 65 MiB
  browser upload (the bound refusal is unit-tested, including that zero archive bytes reach the
  daemon); and neither "install while the daemon is stopped, then restart and see it active" nor
  the "cannot tell whether capsulad is running" refusal (a socket that fails for a reason other
  than absence) was exercised.

## First-party app packages

The six product app packages (`org.capsulad.simon`, `org.capsulad.pa`, `org.capsulad.radio`,
`org.capsulad.radio.tuner`, `org.capsulad.homeassistant`, `org.capsulad.airplay`) are
host/simulator evidence only. No app package has been installed or run on the unit in this record.

- **Packaging (build host, observed here).** Each app's entrypoint stages a package directory and
  writes `<id>-<version>.tar.gz` beside it, with the pinned artifact shape (root `manifest.toml`,
  normalized modes, epoch-zero timestamps). The archives in `build/apps` were listed directly,
  and their inventories match their manifests:
  - `org.capsulad.simon-1.0.0.tar.gz` holds exactly `main.js` and `manifest.toml` (mode 0444) —
    the contract test suite is deliberately not in the package.
  - `org.capsulad.radio-0.1.0.tar.gz` (20,026 bytes, sha256
    `49b6e7a7cc32132b822b45bf56dea094390c7de3c093d00b90ec99f5a3e54d3a`) holds `main.py`
    (54,409 bytes), `manifest.toml` (3,352 bytes) and
    `proto/{radio.proto,radio.protoset,radio.md,media.protoset,media.md}`.
  - `org.capsulad.pa-0.1.0.tar.gz` holds `main.py`, `manifest.toml`,
    `proto/{pa.proto,pa.protoset}` and `docs/speaker.md`.
  - `org.capsulad.radio.tuner-0.1.0.tar.gz` (1,016,671 bytes, sha256
    `95a9fcd959ea02a2afac93ac26732e67c8cbd61768e866da9250e95615c62a90`, matching the current file
    on disk) holds `manifest.toml` (1,528 bytes) and `bin/tuner` (2,382,370 bytes, **Mach-O 64-bit
    arm64**). The tuner's sources and rebuilds changed repeatedly during this work; this is the
    final artifact.
  - `org.capsulad.homeassistant-0.1.0.tar.gz` (1,077,352 bytes, sha256
    `7058114af739a950ad82165acdbf11803a7c2e131dde6af19f38eb684906ea30`) holds `homeassistant`
    (2,531,282 bytes, sha256 `4c84497e3d29e3f9e98c6427bd36f26a0eac0cd0ac6104e13da08b8e52a1bbff`,
    host binary), `manifest.toml` (9,647 bytes), `proto/homeassistant.{proto,protoset}` and
    `docs/bridge.md` (9,619 bytes).
  Both native binaries are simulator artifacts: no aarch64 cross toolchain was present, so neither
  a device binary nor a device install is claimed for any of these packages. Byte counts and
  digests move with each app edit; the inventories above are the stable part. Packaging itself is
  deterministic — `internal/artifact` sorts entries, writes epoch tar mtimes and a zero gzip
  header mtime with normalized modes, and the headers of these archives carry `00 00 00 00` — so
  an archive digest identifies its packaged bytes rather than the time it was packed. In this tree
  the tuner's `8ec997c5…` → `95a9fcd9…` step is exactly that kind of content delta, and it is
  verified entry by entry: the archive has three entries, the tuner source and the packaged
  `manifest.toml` are identical in both (source sha256 `855f5270…`), and the only entry that
  differs is `bin/tuner`, which embeds Go's `vcs.revision` — HEAD moved `77f5c85` → `33378ec`
  between the two builds, so the current binary (sha256 `606a5c21…`) matches a fresh build at
  HEAD while functionality is unchanged. Native app binaries therefore stamp the revision: two
  consecutive builds at one commit reproduce byte-for-byte, the digests above were taken at HEAD
  `33378ec`, and only `-buildvcs=false` would make them stable across commits (the repository
  convention, shared with the devtools app and the assistant Makefile targets, is to leave
  stamping on). Script packages such as the radio front end are unaffected because they compile
  nothing.
- **Tuner unit tests (reported by the app's author, run in `apps/radio/tuner`).**
  `go test ./...` → `? cmd/tuner [no test files]`, `ok internal/stream 0.010s`,
  `ok internal/tuner 0.010s`, exit 0; `go test -race ./...` and `go build ./...` pass, `gofmt -l`
  is clean. `go list -deps ./cmd/tuner` reported 71 packages with **no** match for `net/http`,
  `crypto/tls` or `google.golang.org/grpc`, which is the shared-SDK rule for a first-party app.
- **Tuner fetch and decode against real stations (host).** Through `capsulad.OpenTCP` plus the
  native verified-TLS path, `https://ice1.somafm.com/groovesalad-128-mp3` returned
  `content-type="audio/mpeg"` with codec MP3, `icy-name="Groove Salad: … [SomaFM]"`, metaint
  45000, 44100 Hz and a 35,280-byte 200 ms chunk; `https://stream.radioparadise.com/mp3-192`
  returned `audio/mpeg` (MP3, icy-name `Radio Paradise (192k mp3)`, metaint 16000, 44100 Hz);
  both default stations played again after the review fixes, and the playlist URL
  `somafm.com/groovesalad.pls` resolved; `https://ice1.somafm.com/groovesalad-128-aac` returned
  `audio/aac` and was refused as `unsupported codec: AAC/HE-AAC`. The temporary smoke test was
  deleted afterwards. Proof level: real network transport and local MP3 decode on the build host —
  **not** a device run.
- **Tuner caller authorization (reported by the app's author).** `Tune` and `Stop` accept only the
  configured `player_app`'s currently ready run. The handler compares the daemon-relayed caller
  run id against the run ids `AppInterfaceService.ListInterfaces{runningOnly:true}` reports for
  that app, and ends every other call with gRPC `PERMISSION_DENIED`, logged as
  `radio-tuner: refusing <Service>/<Method> from run "<id>": …`. That run id is
  `ServerCall.CallerRunID()`: the caller's `capsulad-run-id` claim, checked against the socket
  peer credentials where the transport provides them (a mismatch is rejected before relay, and
  the value is never taken from a request field), `pid:<pid>` for a caller with no supervised run,
  and empty when unknown. The request's `player_app_id` is validated but is not the authorization,
  and a caller with no supervised run of the player app (empty identity or the `pid:` fallback) is
  refused rather than trusted. Evidence: `TestServerCallExposesCallerRunID` in `sdk/go`
  (`go build` and `go test` pass; no C ABI change), and the tuner's tests covering accept, wrong
  app, `pid:` caller, empty identity, not-ready player, unknown run, lookup failure and a custom
  `player_app`. This supersedes the earlier residual-assumption wording: the routed call is no
  longer an arbitrary-egress primitive for anything but the configured player. The general model
  is in [security.md](app_development/security.md#routed-calls-and-trust), and the manual documents
  `CallerRunID()` with the ListInterfaces authorization pattern. The live `e2e-tuner` run against
  the current bytes (`95a9fcd9…`) exercised the accepted path end to end (authorized `Tune`/`Stop`,
  a live station switch, pause/resume and the tuner-kill watchdog case) with zero `refusing …`,
  `PERMISSION_DENIED` or `not acknowledged` lines; the earlier authorizing build's run recorded
  authorized `Tune` for
  both stations, and an authorized `Stop` on a station change acknowledged inside the player's 2 s
  budget, likewise with zero refusal lines across both
  authorizing runs; the refusal paths themselves are unit-tested, not seen in a live run.
- **Simon contract tests (reported by the app's author).** `node --test
  apps/simon/test/*.test.mjs` → `tests 22`, `pass 22`, `fail 0`. The suite loads the shipped
  `main.js` in a `vm` context against a fake host that implements the documented QuickJS surface
  (ordered JSON frames, blocking recv, `WAIT_FOREVER`) and asserts the app's observable requests,
  frames and terminal cleanup; the test file is not packaged. Node ≥ 18 is required. Host smoke
  runs of the new capability printed `SMOKE-TRYRECV quickjs events=7 empty=true ms=0`,
  `SMOKE-TRYRECV-BYTES quickjs first=true empty=true`,
  `SMOKE-TRYRECV-NEG quickjs empty=true ms=0` and `SMOKE-TRYRECV-OK quickjs`.
- **Simon needs the rebuilt QuickJS host.** Current `main.js` calls `Call.tryRecv`/`tryRecvBytes`,
  so the deployment must carry the matching `lib/capsulad-quickjs` from `make interpreter-quickjs`
  (or `make interpreter`). Against an older host the app logs
  `unavailable: this QuickJS host has no Call.tryRecv; rebuild the interpreter host` and stops
  cleanly instead of playing — that path was observed on the host, not on the unit.
- **Simon on the simulator (reported by the app's author).** With `-platform simulator`, the
  staged `build/lib` runtime including `capsulad-quickjs`, and the packaged `tar.gz` installed:
  startup reached `APP_READY`, both captures (segment presentation and full-panel touch) were
  selected and the rendered segment frames were observed, correct and wrong presses scored as
  designed, the centre tap hint and its restore worked, and no orphan run was left behind. The
  documented exit was measured: a centre press 18 ms into round 2 froze the replay and showed the
  dashes, and the release 1018 ms later exited 0.5 ms after it was seen. `sandbox=none` on that
  host. Limits: segments only (no ring or audio), no device, optical or timing measurement beyond
  those host timestamps.
- **Home Assistant bridge tests and simulator lifecycle (reported by the app's author).** The
  suite was reported race-clean (`CGO_ENABLED=1 go test -race -count=1 ./...` ok for
  `cmd/homeassistant` and `internal/{appcfg,ha,hw,mqtt}`) and re-verified against the final
  artifact (`CGO_ENABLED=1 go test -count=1 ./...` ok, `go vet ./...` clean, `gofmt -l .` empty);
  `sh apps/homeassistant/build.sh` wrote the archive.
  `go list -deps ./cmd/homeassistant` matched none of `net`, `net/http`, `crypto/tls`,
  `crypto/x509` or `google.golang.org/grpc` (observed here): the MQTT 3.1.1 framing and the TLS
  session run over `capsulad.OpenTCP`, and a local host:port helper replaced `net.JoinHostPort`
  so the resolver, interface and poll packages stay out of the app's graph. Against a private
  simulator daemon
  (`-platform simulator -sandbox none`), the packaged final archive was installed and run: it
  reached `APP_READY` and logged
  `bridge node="simulator_0001" entities=17 surfaces=ring:true display:true sensors:1 bluetooth:true touch:false`,
  an invocation reported `disconnected from mDNS discovery as node "simulator_0001"; 17
  entities; 0 commands applied; status connecting`, and
  `app stop` answered `stop requested: true`, then `bridge stopped; hardware released` and
  `exited code=0 state=EXITED`. The review changed what Home Assistant
  observes: there is **no** `media_player` entity (its MQTT integration has no such platform —
  the Bluetooth source is `sensor/bluetooth_state` (`enum`: playing/idle/off),
  `sensor/bluetooth_title` and four `button/bluetooth_{play,pause,next,previous}`), sensor units
  are published in Home Assistant's vocabulary (`celsius`→`°C`, `lux`→`lx`, `fahrenheit`→`°F`,
  `kelvin`→`K`; anything else verbatim with no device class, because an unknown unit/class pair
  could fail setup), the ring publishes `supported_color_modes` with `color_mode: "rgb"` on ON
  states and declares `flash`/`transition` false (a command carrying them is still applied),
  `object_id` is no longer published (`unique_id` carries identity), `PublishEvent` answers
  `INVALID_ARGUMENT` for a malformed/undeclared request and `FAILED_PRECONDITION` without a
  runtime/published event entity/session, `Status.broker` names the endpoint in use including a
  discovered broker, and every exchange is now bounded so a broker that accepts a connection and
  then stops answering cannot leave a half-online bridge: CONNECT/CONNACK 10 s, SUBACK 10 s
  (`TestSessionBoundsTheSubscribeExchange`), and writes through a capacity-1 channel that honours
  the caller's context — the connect deadline, a 2 s context plus a 2 s write deadline for
  `DISCONNECT`, and 5 s for client-originated PUBACK/PINGREQ/PINGRESP writes — so the offline
  marker and `DISCONNECT` cannot queue behind a wedged writer
  (`TestWedgedWriteDoesNotBlockOtherWriters`, `TestDisconnectDoesNotQueueBehindAWedgedWriter`,
  `TestDisconnectBoundsItsWrite`). A failed session publishes `offline`, sends `DISCONNECT` and
  retries with backoff, but the backoff is now *paced*, which is the visible part: a session counts
  as usable only once CONNECT, the discovery publish and the command subscriptions have all
  succeeded, and the delay is reset only when a usable session then held for at least 30 s. So a
  broker that answers CONNACK but refuses or ignores SUBSCRIBE, one that drops immediately after
  becoming usable, and a dial/CONNECT failure all retry at 1 s, 2 s, 4 s … capped at 1 min instead
  of resetting to a ~1 Hz flap that republishes the whole retained discovery set once a second;
  a deliberate reconnect (button or action) is still immediate
  (`TestAdvanceBackoff`, `TestBackoffDecaysForABrokerThatNeverAcceptsCommands` — exactly two dial
  attempts by 2.4 s). Two caveats belong with that: a healthy session that drops *inside* the 30 s
  window also decays towards 1 minute (deliberate — the reset needs 30 s of usability, so a
  flapping broker slows down instead of being retried once a second), and at
  `keep_alive_seconds = 5` the 1.5×-keepalive watchdog can end a session before the 10 s subscribe
  budget does (same reconnect, one extra log line; both bounds are in the app README's "Broker
  behaviour" section). Watcher activity is derived from the published catalog rather than the
  exposure settings (`TestWatcherGatingFollowsTheCatalog`, which asserts the loop set a bridge runs
  — `device-state`, `sensor:<id>`, `media`, `touch`): the device-state, media and touch
  loops and each framework-sensor watcher run only while their entity is published, the title
  sensor is truncated rune-safely at Home Assistant's 255-byte limit with the full value in
  attributes, and the MQTT release button calls the same
  `Hardware.ReleaseRingAndDisplay()` as the app action. The focused suites reported those tests
  PASS (`internal/hw` `ok …`, `internal/mqtt` `ok …`; `TestSessionBoundsTheSubscribeExchange`
  0.20 s, `TestAdvanceBackoff` 0.00 s, `TestBackoffDecaysForABrokerThatNeverAcceptsCommands`
  2.40 s, `TestWatcherGatingFollowsTheCatalog` 0.00 s), and the full suite was
  race-clean on the shipped tree. Limits: no real MQTT broker
  and no Home Assistant instance was contacted; mDNS broker discovery could not be exercised
  because this build's daemon answered `DiscoveryService/Browse` with "unknown service" (the app
  logged one clear failure and kept retrying); the packaged binary is darwin/arm64, so a device
  package needs the aarch64 toolchain.
- **Internet Radio tests and simulator end-to-end (reported by the app's author, after the review
  fixes).** `make -C apps/radio test` (equivalent to `python3 -m unittest discover -s
  apps/radio/tests` from the repository root, which the author verified) printed
  `Ran 57 tests in 0.020s` and `OK`; the prelude self-test
  (`python3 interpreter/micropython/sdk_test.py`) printed `Ran 12 tests in 0.005s` and `OK` for the
  timer surface (cap, cancel-during-pass, clock safety, exception isolation, poll bound,
  stop-on-return). The app suite now covers the timer and watchdog directly —
  `test_watchdog_is_registered_at_startup`,
  `test_dead_tuner_is_detected_by_the_watchdog_without_any_activity`,
  `test_liveness_is_by_run_not_by_app`, `test_liveness_accepts_the_same_run`,
  `test_liveness_lookup_uses_a_short_budget`,
  `test_liveness_lookup_failure_does_not_kill_playback`,
  `test_watchdog_detects_a_dry_queue_on_a_live_tuner` (50 s survives, 60 s trips),
  `test_a_written_chunk_restarts_the_dry_queue_clock`,
  `test_repeating_callback_limit_is_enforced`, `test_speaker_stream_end_becomes_error`,
  `test_push_authorization_lookup_failure_ends_the_serving_session`,
  `test_a_foreign_caller_cannot_end_the_session_on_a_lookup_failure`,
  `test_foreign_callers_cost_one_lookup_each`,
  `test_no_audio_bound_covers_the_tuner_retry_window`,
  `test_finished_without_audio_returns_to_stopped`,
  `test_pause_during_the_drain_discards_the_tail_and_stops`,
  `test_finished_session_drains_without_blocking_then_stops` and
  `test_drain_timeout_discards_the_tail` — plus
  the earlier authorization, display-recovery and streaming cases. `make -C apps/radio e2e` — the
  deterministic simulator run, whose tuner fixture is now a paced producer (one 200 ms chunk per
  host timer tick, `Tune` replying first) and which waits for the new session's first accepted
  chunk before the circle-key tap, so it covers pause/resume during playback and the tuner-kill
  watchdog case in about 10 s, with the real
  MicroPython host and a MicroPython tuner fixture speaking the raw-protobuf
  contract — printed `PASS` and logged `radio: push authorized
  app=org.capsulad.radio.testtuner run=…`; it covers install, boot start, the capabilities probe,
  a launch invocation, tuner discovery, `Tune`, pushed PCM reaching the speaker stream, ICY
  metadata, drain, a second station, a live station switch through the settings API, circle-key
  pause via `MediaPlayerService`, resume, the media-provider listing, a display capture with the
  daemon's rendered frames, and a clean stop.
  `make -C apps/radio e2e-tuner` (live: native tuner, real MP3 station, needs network and the
  tuner package) printed `e2e-tuner: PASS (simulator, native tuner, live MP3 station)` against the
  final tuner artifact, exercising authorized `Tune`/`Stop`, a station switch, pause/resume and a
  clean stop with zero refusal or stall lines; the run against the authorizing build logged
  `session radio-1-28b2b5` accepting `SomaFM Groove Salad` (codec MP3),
  `speaker stream pbs_acd7e0c6b4695ea66694e open 44100 Hz 2 ch format=1 actual=1/48000/2`,
  `radio: session radio-1-28b2b5 playing 50 chunks`, pause as `refused by player: paused`, a live
  station switch answered by the authorized `Stop` (`session radio-2-28b2b5 stopped (station
  changed)`, inside the player's 2 s budget), `session radio-3-28b2b5` accepting
  `Radio Paradise Main Mix`, `radio: state playing station audio` and a clean stop. Both e2e
  scripts now also contain the autonomous-watchdog phase: they stop the tuner's run with no user
  interaction and require the app to log `radio: playback failed: tuner stopped` and
  `radio: state error tuner stopped`, asserting `watchdog left PLAYING after the tuner died, with
  no user activity`. The live run on the current pairing (player `49b6e7a7…`, tuner
  `95a9fcd9…`) logged `radio: push authorized app=org.capsulad.radio.tuner
  run=run_8784a72fe4e95d8f6f438`, with authorized `Tune`/`Stop`, a live station switch,
  circle-key pause/resume and the tuner-kill watchdog case (`radio: playback failed: tuner
  stopped (run is no longer ready)` → `radio: state error tuner stopped (run is no longer
  ready)`), then a clean stop — and `e2e-tuner: PASS (simulator, native tuner, live MP3
  station)`. The reviewed behaviour this covers: symmetric caller
  authorization on `Push`
  (a foreign or unnamed pusher is refused `unauthorized caller` with no state change, and a
  non-empty exact `session_id` is required), the **autonomous watchdog** (registered as
  `capsulad.every(500, watchdog)`; liveness by **run identity** — every four ticks ≈2 s the run
  that last pushed for this session must still be the ready run of the tuned tuner app, so a
  supervisor-restarted tuner counts as gone — with an **800 ms** lookup budget and a lookup error
  never treated as death, detail `tuner stopped (run is no longer ready)`; a dry-queue detector
  requiring `frames_queued` in `PlaybackProgress` at least once per **60,000 ms** while playing,
  derived from the tuner's three-reconnect budget, `(1+12)+(2+12)+(4+12) = 43 s`, detail
  `tuner stalled (no queued audio for 60000 ms)`, reset by every successful write; immediate end
  on end-of-stream, `STREAM_CLOSED`/`DAEMON_SHUTDOWN`), the push-refusal ownership rule (a failed
  push-authorization lookup ends the *serving* run's session as `push authorization unavailable`,
  while an unknown or foreign caller is only refused `unauthorized caller` and can never end
  playback through a lookup failure; the resolution cache is a bounded 16-entry `run → app` table,
  so a repeated foreign caller costs one lookup ever), the single 60 s no-audio bound for both
  phases (`tuner did not start (no audio for 60000 ms)` while buffering — `Tune` returns only
  after the first decoded frame — and `tuner stalled (no queued audio for 60000 ms)` while
  playing; the bound follows the tuner's retry budget, `(1+12)+(2+12)+(4+12) = 43 s` worst case
  plus a 10 s first-chunk idle), the terminal `finished` that carries no audio ending directly as
  `stopped` (`station stream ended`), and the non-blocking natural end (`finished=1` half-closes
  and drains while the phase stays `playing`; `Pause` during that drain is a stop — `accepted=true,
  detail="stream ended"`, tail discarded, `PLAYBACK_STOPPED` — `Stop`/`dismiss`/a station change
  abort it the same way, and a drain unfinished after 30 s is aborted), display recovery after a
  failed frame with
  a retried open, resume discarding the paused tail (no `PLAYBACK_RESUME`), the non-empty
  `playerId` rule on the media contract, and the 25 s `Tune` / 2 s `Stop` blocking bounds. The
  terminal state is `phase=error`, four dashes (`0x40` ×4, colon off) and `PLAYBACK_STOPPED`, with
  the reason logged as `radio: error: <detail>` and served as the command `detail`; recovery is
  only `play`/`stop`/`dismiss`. The timer capability is new platform surface, documented in the
  application manual (`sdk.md`: the limits bullet, the member table and the lifecycle section): the
  MicroPython host exposes `capsulad.monotonic_ms()` (the only clock in that host),
  `capsulad.every(interval_ms, fn)` → token and `capsulad.cancel_timer(token)` → bool. Callbacks
  run on the event thread, between dispatched events and — when the bus is idle — from the poll
  timeout bounded by the nearest due timer, so they fire with no inbound event or call at all; a
  callback's next due time is `now + interval_ms` after it returns and only when it actually fires
  (slow callbacks slip, with no catch-up storm), exceptions are logged as `timer handler failed: …`
  without ending the run and the schedule is kept, a timer cancelled by another callback in the
  same pass no longer fires even if it was due at the start of that pass, and a clock fault never
  ends the run: a raising `monotonic_ms()` skips that pass and leaves the poll at its 1 s default,
  while a backwards jump simply leaves a timer not yet due (`_run_timers` iterates a snapshot and
  re-checks registration; `_poll_timeout` returns its default on the same exception). At most 16
  repeating callbacks are allowed per run (`ValueError` beyond that and for an interval below
  1 ms), tokens are per-process and never reused, and every callback stops when `run()` returns —
  the manual's "Cancellation and clock faults" bullet states the same. QuickJS still has no
  timers, so Simon's model is unchanged. `docs/app_development/audio.md` also now defines
  `frames_played` (mixed output frames, silence included — not usable as producer progress) and
  `frames_queued`, and states there is no playback underrun notice. Proof level: the daemon log
  shows routing
  selecting the radio's capture and
  `display.segment.front wrote presentation=… cells=[…]`, and the speaker stream accepted real
  44.1 kHz MP3-decoded PCM — digital output, not electrical or acoustic, and no device run.
- **PA Speaker tests and simulator run (reported by the app's author, after the review fixes).**
  `python3 apps/pa/tests/test_wav.py` → `Ran 29 tests in 0.081s`, `OK`; the new
  `python3 apps/pa/tests/test_app.py` (settings shapes, status mapping, handler) →
  `Ran 28 tests in 0.081s`, `OK`. `sh apps/pa/tests/e2e.sh` (real simulator daemon, real
  MicroPython host, real audio service) printed
  `PASS: install, boot, play, split play, rejections, no-progress frames, stall, cancellation, queued calls, setting bound`
  in about 21 s (phase counters `DONE main ok=15 fail=0`, `DONE settings ok=3 fail=0`): it
  installs the archive, waits for the app to start at boot, streams a mono
  16 kHz and a stereo 48 kHz WAV (whole and split across request frames), rejects
  non-WAV/8-bit/96 kHz/over-length/truncated/oversized-frame with the documented statuses, proves
  a frame carrying no WAV bytes ends `INVALID_ARGUMENT` (`request frame N carries no WAV bytes`),
  a 15 s stall ends `DEADLINE_EXCEEDED`, discards a 3 s clip when the caller cancels (the
  playback stream closed ~0.2 s after the cancel), serves two queued calls strictly one after
  another (open/close alternation asserted) and honours an administrator-set bound
  (`max_seconds=1` accepts exactly 1 s and rejects one frame over). A call is held for at most
  the caller's own routed deadline (30 s default, 5 minutes maximum, enforced by the daemon as a
  cancel), so a queued caller waits for the current clip plus at most the owner's remaining
  deadline. The review fixed a real bug
  here: `max_seconds` had been silently forced back to its default on every boot because the app
  misread the app-private settings event; the run above shows `pa: settings revision=2
  max_seconds=1` followed by `the WAV holds 32002 bytes of audio; max_seconds=1 allows 32000`.
  Proof level: API behaviour, routing/ownership and PCM submission — not acoustic output. No
  device run; the script host has no filesystem, so a caller supplies the bytes.

## Verified against the live vendor service

The Marusia assistant was exercised against the real vendor service, not only against loopback
fixtures.

- **Session authentication.** With the masked `session_id`/`session_secret` settings configured, the
  app validates that session and sends no provisioning attach; the service accepted the credential
  and the per-call metadata set. A rejected session is not silently replaced by an attach — the app
  reports the rejection, drops the session and re-resolves it on the next activation.
- **Speech-stream framing.** The service accepted the init (`kw_params` carrying `media_state`), the
  80 ms chunk frames — each a self-contained canonical WAV, numbered from 0 — and the `eos` frame,
  without a format or framing error. Leaving the request side open after `eos` is what the service
  needs in order to return its terminal frames instead of an internal error.
- **Recognition produced no text.** The streamed utterance yielded no recognised text, so no reply
  was synthesised and no `play_url` body was fetched; the TTS body codec is still unobserved.
- **Turn audio evidence and the recognition gate.** One instrumented touch turn on the device
  logged `uploaded_chunks=51 uploaded_ms=4080 rms_dbfs=-24.7 pre_open_dropped=0
  service_chunks_received=44 service_audio_received_ms=3520 service_status_num=2 status_frames=2`.
  The integrated level is real speech, not silence; 51 chunks / 4080 ms were written, and the
  service's own status frames counted 44 chunks / 3520 ms — exactly the audio up to its last 1 s
  status interval — so the upload arrived as valid 80 ms framing and the utterance start was not
  truncated. The service still produced no text and no command. That names the vendor recognition
  gate with numbers rather than a hypothesis: it is no longer a capture or framing question. The
  remaining limit is scope: one captured turn establishes this transport path and this gate, not a
  recognition failure mode for every utterance or a permanent vendor behaviour.

Proof level: the vendor service accepted the session credential and the upload framing. Not proven:
recognition of the captured speech, the codec of a returned `play_url` body, playback of that body,
any latency figure, and one complete spoken turn. Those are open gate 16.

### 2026-11-17 deployment and probe

Deployment proof and live probes recorded on 2026-11-17, at their separate proof levels.

- **Deployed stack.** HEAD `77f5c85` was built for aarch64 / glibc ≤ 2.33 and deployed to the
  Capsula Mini rev 4 over ADB (`/data/capsdemo`). The boot hook's health gate returned 0; the
  installed daemon reports `0.1.0-dev` and the apps report `org.capsulad.marusia 0.1.0` and
  `org.capsulad.dumbassistant 0.2.0`. The default assistant was migrated to `org.capsulad.marusia`
  and existing credentials and settings were preserved. The deployed build is HEAD `77f5c85` and
  did not include the unrelated uncommitted peer changes in the working tree.
- **Device serial (identity hygiene).** The serial setting was corrected from
  `11bfcbc5…` to the unit's real serial `03104B1D…` (hostname/backup evidence). The
  probe with the correct serial still produced no text, so this is identity hygiene, not a
  recognition fix.
- **Live probe matrix.** Four prompts with `no_kw` all ended `finish result` with zero
  `text_so_far`, no commands and no `play_url`; the service accepted the init/QoS and counted
  13 chunks / 1040 ms, then 25 chunks / 2000 ms (status_num 1, 2). Both `Маруся,` prompts with
  `kw_params` aborted with "keyword not confirmed". Extra controls were identical in outcome:
  activation_type `listen_command` / `button` / `keyword`, an independent Milena TTS utterance,
  and an open `/connect` WebSocket changed nothing. `/connect` returned 101 with zero pushes.
- **Vendor ground truth.** The vendor packer does not set `use_asrld_recognize` or `kws_context`,
  and the device control plane supplies no `client_data`, `grpc_mainflow` or `grpc_flowmode`. The
  vendor's older session `b1903f9f…` is Unauthenticated; the only accepted session `f775c474…`
  belongs to a different account than the unit's historical pairing. The remaining blocker is
  service-side provisioning / account pairing, not a missing app init field; recognition is not
  shown to work.

Proof level: the vendor service accepted the supplied app-settings session and the upload framing.
Not proven: recognition, TTS, and any `play_url` body (none observed). The probes ran through the
host native SDK against `https://vc.go.mail.ru`; there is no device acoustic measurement and no
claim that the app is fixed. The safe next action is to re-link/provision the unit under the
accepted session's account, then re-run the probe.

## Explicitly unavailable on this hardware

- **User, PID, network and IPC namespaces and seccomp filters.** The stock kernel offers mount
  namespaces only. Sandboxing is therefore a read-only mount namespace plus a distinct UID;
  native apps are trusted code.
- **Raw touch coordinates.** There is no coordinate panel, so `coordinates_available=false`
  and `full_raw_available=false`. Full-panel delivery remains available.
- **Amlogic.** Both Amlogic A113X platforms are implemented: `-platform amlogic-stock` and
  `-platform amlogic-alpine` select `profile.AmlogicA113XStock` / `profile.AmlogicA113XAlpine`
  and `internal/backend/amlogic`, and the daemon reports
  `CAPSULA_MINI_AMLOGIC_STOCK` / `CAPSULA_MINI_AMLOGIC_ALPINE`. Nothing on that board was probed
  for this change: the host proof is limited to selection, capability reporting, frame encoding,
  panel-probe fallbacks and cross compilation (see the Amlogic entry in this document's platform
  section). The stock clock panel's vendor ioctl ABI is unrecovered and is reported `UNVERIFIED`;
  no parity claim with the RK3308 is made.
- **Vendor system-service control.** `SystemService.ControlUnit` returns `UNIMPLEMENTED`.
- **Field-certified wakewords for the connected room.** Every installed word was trained
  from its own recordings and runs on the processed tap, so each word's calibrated threshold is
  unverified for this room; no speaker-independent keyword head ships.
- **Wi-Fi RF and provisioning on the target.** Physical association, stock AP-mode scanning,
  DHCP/DNS/firewall behavior and captive-portal detection are unproven; the target firmware
  has no `hostapd`/`iw`.
- **A2DP listening parity by ear and real-peer recovery.** The native endpoint registered and
  a transport was created, but no sustained listening test with the corrected (conformant
  four-octet SBC capability) SEP has completed. Whether a peer accepts that SEP, and whether a
  source that abandoned A2DP honors the one profile request without being disconnected, are
  device gates: the request is exercised only against a scripted manager and the D-Bus fixture runs
  the command path, not a real AVRCP session.
- **Photometric brightness and gamma.** No photometer measurement exists.
- **Unpairing without disrupting the user's source.** The path is exercised on the simulator
  and rejects supervised callers; the user's paired source was deliberately not removed.

## Known pre-existing failures

- `internal/daemon` `TestDumbAssistantEndsAfterSpeechOverColoredRoom` fails in this environment
  and reproduces on pristine HEAD ("speech end 1 not detected"). It is an
  environmental/timing failure, not a regression from the changes recorded above. The failure
  fired at **11.10 s** here against **11.08 s, 11.28 s and 11.83 s** on pristine HEAD, so the
  timing is in the same band. `make test-go` and `make test-race` reported 33 packages ok with
  this one test the only failure, and the race run reported **0 data races**.
- No settings race is recorded. An earlier settings-race report came from a race run whose
  staged host library had been removed by a failed native cross-build; the clean full
  `make test-race` run reported **0 data races**, so that report was a broken-build artifact,
  not a confirmed race. Gate 21 records the re-run.

## Open gates

Item 10 is withdrawn rather than removed, so the numbering used by cross-references in this
document stays stable.

1. Measure emitted light and gamma for both output channels, including the 5 % auto-brightness
   floor, and compare the standby baseline with the vendor firmware.
2. Exercise the five-key panel with the deployed resolver on the unit: a bare volume tap stepping
   exactly once with no added latency, the centre gate both held and released-then-edge, the
   opposite→centre→direction chain
   (whose leading edge has already acted), the ≤ 400 ms full-panel crossing resolving once with its
   completing edge suppressed and its leading step reverted, a 330 ms hand crossing still
   resolving it, two same-direction taps plus an opposite edge staying three steps, a slower
   opposite pair stepping twice and a lone end-stop key staying a step, the 700 ms hold summon,
   overlapping edge-to-edge crossings without volume leakage,
   circle-then-microphone as deliberate keys, full-capture-only circles, and no further effect from
   an unfinished sequence. The resolver and its windows are host-verified against synthetic
   timelines and the real input manager, and the retained device log already shows a real
   `center p+0ms r+115ms | gap 14ms volume_down p+130ms down` pair, but the **new resolver's
   on-device behaviour after deployment is unconfirmed** and needs physical presses. Whether the
   panel reports the centre on every intentional swipe is still open: `swipe_half.hex` shows one `0xf8` gesture and one centre-key `0x2c0` gesture, and the node declares only `EV_SYN|EV_KEY`, so the Info-level contact trace
   (press/release offsets and the signed gap) is the only field evidence a swipe can produce. A
   rendered overview badge with a real phone is likewise unobserved.
3. Listen to the native A2DP path with a playing source and compare it with the vendor stack
   under dropouts; verify `MediaTransport1.Acquire` and the SBC decoder under real jitter.
4. Verify the first incoming Bluetooth connection after BlueZ autopowers the adapter; whether a
   peer that already abandoned A2DP recovers on the same link when its A2DP source profile is
   requested, without a disconnect; and record the `Warn` that a session is not A2DP together with
   the one profile request it triggers (`profiles up` / `peer advertises` / the request outcome)
   plus `busctl tree org.bluez` for the resulting `MediaTransport1`.
5. Repeat the mDNS browser proof on the device, including a real write-time collision and
   invalid-label rejection, and confirm `avahi-resolve` after an Avahi restart.
6. Evaluate the assistant endpointer and each wakeword against real room noise, loud music and
   quiet speech over long durations; the acoustic gate cannot be certified by simulator PCM.
7. Run a MicroPython package on the unit, not just its host. **Partly closed on 2026-11-17:**
   `org.capsulad.pa` and `org.capsulad.radio` ran on the unit through the installed
   `capsulad-micropython` (418,736 B `23af60a7…`) and reached `APP_READY`. What a script app does
   after readiness on the device (audio path, station playback) is still open.
8. Exercise a real Wi-Fi association, the setup AP, DHCP/portal detection and AP teardown on
   the target; verify stock AP-mode scanning or the absence of a scan path.
9. Exercise a power cycle with the single boot hook, including `disable`/`enable` and the
   vendor fallback, and verify recovery from an overlay wipe.
10. **Amlogic A113X bring-up.** Run `-platform amlogic-stock` and `-platform amlogic-alpine` on a
    unit. Record the ring channel count and one rendered frame, which clock panel is fitted
    (IS31FL3236A or AW21036) and which of the three panel interfaces exists, the real PulseAudio
    sink/source names (stock), the ALSA mixer control (Alpine), capture latency, whether the stock
    `/dev/watchdog` can be disarmed, and whether the stock init list really starts an overlay-only
    hook. On Alpine, confirm the `Master Playback Volume` softvol exists only after the first
    playback open, that the framework's pin eventually lands (`SetMasterVolume` retries because a
    failed pin leaves `pinDone=false`), and that the framework level is not multiplied by a
    non-unity softvol in the meantime. Disarming is a fact-gathering gate only: a planned stop is already safe because the
    supervisor hands any armed timer to a feeder rather than trusting `Close`. Watchdog facts are
    now image-derived, with no unit involved: the stock DT (`device.dtb`) has
    `/watchdog@0xffd0f0d0` with `compatible = "amlogic, meson-wdt"` (a **vendor** driver — note the
    space after the comma, which defeats a naive `amlogic,[a-z0-9-]*wdt` grep), `status = okay` and
    vendor properties `default_timeout = 10`, `firmware_timeout = 6`, `suspend_timeout = 6`,
    `shutdown_timeout = 10`, `reset_watchdog_time`, `reset_watchdog_method`. Those property
    **units and whether this driver parses them at all are unknown** (the vendored tree has no
    mainline match; the property name matches an unrelated mainline module parameter), so
    `default_timeout` is *not* treated as a fact: the stock profile keeps its previous 30 s
    declared interval, and the supervisor derives the keepalive cadence and the stall window from
    the interval the driver actually reports (`WDIOC_GETTIMEOUT`), using the declared value only
    when the driver reports nothing. In the Alpine image (`vk-mini-a113x.dtb`) the node is
    `/soc/bus@ffd00000/watchdog@f0d0` with the mainline
    `compatible = "amlogic,meson-gxbb-wdt"` and no `status` property (enabled), and `config_alpine`
    (kernel 6.18.44) sets `CONFIG_MESON_GXBB_WATCHDOG=m`, `CONFIG_WATCHDOG_CORE=m`,
    `CONFIG_WATCHDOG_SYSFS=y`, `CONFIG_WATCHDOG_HANDLE_BOOT_ENABLED=y`,
    `CONFIG_WATCHDOG_OPEN_TIMEOUT=0` and leaves `CONFIG_WATCHDOG_NOWAYOUT` **unset**, with
    `meson_gxbb_wdt.ko` present in the rootfs image. The Alpine profile therefore wires
    `/dev/watchdog` (30 s default, `amlogic.syswatchdog`): if the module is not loaded the node is
    absent and the supervisor retries and reports `no watchdog device yet`, so a missing timer is
    reported rather than silently unprotected. The stock profile keeps its disarm facts unmeasured:
    a vendor driver's magic-close/disable behaviour cannot be inferred from the Alpine/mainline
    evidence, and `/sys/class/watchdog/watchdog*/nowayout` is readable only if the stock kernel
    enables `CONFIG_WATCHDOG_SYSFS`. Offline items still open: which mechanism loads the Alpine
    module at boot (mdev modalias coldplug, `/etc/modules` or an init script), and the stock
    rootfs' watchdog holder/service. Until a unit is probed, the Amlogic panel and audio remain
    `PARTIAL`/`UNVERIFIED` and no parity with
    the RK3308 is claimed.
11. Freeze protocol schemas only after generated Go/Rust bindings and script-host parity
    tests pass, including routed streaming and cancellation.
12. Measure acoustic loudness at several master settings, including the re-encoded assistant
    reply and the framework cues, and confirm by ear that the per-app ceiling is audible as "no
    app exceeds the configured level" rather than as a mixer measurement. The host proves emitted
    PCM (ceiling, clamp, limiter) and the asset RMS, not perceived level; speaker efficiency,
    enclosure and room response are unmeasured.
13. Verify the real PulseAudio sink state on the unit: `SetMasterVolume` should pin `master` at
    100 % and read it back, warn once and retry boundedly when it is not unity, and degrade with
    one warning when `pactl` is absent. The pin proves the **sink** gain only — `pacat`'s
    sink-input volume is a separate stage that is neither set nor read back, so also compare
    `pactl list sink-inputs` volumes for the framework stream when the whole chain matters. The
    warning lands in the daemon log bus (rotated log / Web UI) through the backend's warning
    hook, not the process's standard logger, so this check can be read from the Web UI log.
    Only the `pactl` volume-output parser and a scripted `pactl` were exercised on the host; no
    real PulseAudio sink was changed.
14. Measure the aarch64 cost of the per-app scratch pass and the limiter on the RK3308
    (four 1.008 GHz A35, 240 MiB RAM). The steady-state mixer path allocates nothing per tick,
    but the host gives no CPU or heap proxy for the target.
15. Decide the product question left open by this change: whether the linear master should become
    a global loudness curve (the report's `master^1.5` proposal) for every source. The deferred
    curve would change the loudness of every cue, alert and app at every setting and redefine
    what `master_volume` means to users; it needs an acoustic decision on the device, not a
    simulator measurement. The linear master and the existing Bluetooth `master^1.5`/trim
    behaviour are intentionally unchanged.
16. Run one complete spoken turn on the device: the live-service evidence above settles that the
    service accepts the session credential and the WAV chunk framing, but not recognition of the
    captured speech, the codec of the returned `play_url` body, playback of that body, latency, or
    an unprovisioned `xtoken_id` attach (which was not exercised, because a session was configured).
    It also settles whether the sandbox's outbound TLS reaches the vendor endpoint from inside the
    app's mount namespace.
17. Measure the real multi-word ensemble on the RK3308: one frontend plus N heads at the 80 ms
    hop, the ~two-second all-word warm-up after an install or delete, and per-word acoustic
    acceptance with each word's own calibrated threshold. The host proves the policy, storage and
    score parity, not recognition quality or a daily false-alarm rate.
18. Record real AVRCP PDUs across a source session with `btmon`: play/pause from a circle tap,
    `SetAbsoluteVolume` (0x50) after a master change, and next/previous from a swipe. Confirm the
    peer's own absolute-volume report creates `MediaTransport1.Volume`, that the master re-assert
    after the transport appears lands once (and that a later source report triggers no write back),
    and that `btmon` shows the PDU a D-Bus write only *claims* to have sent.
19. Re-run the Web UI identity scenario on the unit with the deployed binary: open Settings for
    an app that is not running, open the running clock, and stop/launch the ring. The expectation
    is that the daemon log shows no `denied cross-application access` from the Web UI's own
    connection, the stopped app's settings and Launch action render, the clock is listed as
    running (no Launch button), and a refused read stays an inline error without the "disconnected"
    banner or a reconnect loop. The same session also settles the built-in run table after the boot
    change: the unselected ring offers Launch and has no running entry, while the clock is Running.
    The source fix is proven by host tests only (`TestVirtualRunIdentityRequiresDaemonPeer` plus the
    SPA transport tests); no device run of this fix is recorded yet.
20. Observe the Bluetooth rename on the unit: change the advertised name in the Web UI,
    confirm `Adapter1.Alias` and the name a paired phone sees, restart BlueZ and confirm the
    alias persists, then let the setup AP start and confirm its SSID is the first 32 UTF-8
    bytes of that name on a rune boundary. Host evidence covers validation, the WebSocket
    command, the simulator UI and the D-Bus property write; no physical adapter or Wi-Fi
    radio was exercised.
21. **Closed on this host (2026-11-17).** The clean full `make test-race` run exercised the
    daemon settings service under concurrent load and reported **0 data races**. An earlier
    settings-race report came from a race run against a staged host library that a failed
    native cross-build had removed; it was not a confirmed race. Re-open this gate only if
    settings concurrency changes.
22. **Real Avahi publish/browse visibility.** On the unit, publish `_example._tcp` through
    `DiscoveryService` from a supervised test app, confirm a second mDNS browser on the LAN
    sees the instance with the expected host, addresses, port and TXT, confirm a colliding
    instance is renamed rather than duplicated, unpublish removes it, run exit removes it, and
    `avahi-browse -rt` still resolves after an Avahi or daemon restart. No device run of the
    app-facing publication path is recorded; the host evidence uses a fake responder.
23. **Listener LAN reachability and port policy.** On the unit, allocate an ephemeral TCP
    listener, reach it from another host on the LAN, confirm 5353 and the configured Web UI port
    are refused with the documented code, that a port already in use is `TRANSPORT`, that the
    16-listener cap holds under the deployed supervisor, that a UDP listener receives and
    replies, and that no listener survives run exit. All current listener evidence is loopback
    or a fake daemon; nothing proves the device's interfaces or firewall behavior.
24. **TLS roots and DNS on the device.** From inside the app sandbox, dial a public host by name
    and verify the device trust store accepts it, dial with `roots: "pem"` and a private CA,
    observe the failure modes for a name mismatch and a missing CA, and confirm that the sandbox
    resolver (`/etc/resolv.conf`, `/etc/hosts`) works. The host proves verification against the
    embedded test CA and loopback sockets only; device DNS, trust-store contents and outbound
    reachability are unverified.
25. **Bluetooth connection cue on the unit.** Simulator digital output now covers the cue:
    `TestBluetoothConnectionCue` passed under `-race` with `-ldflags '-linkmode=external'`
    (non-zero sink tail on connect, the cue stream drains, no playback stream on disconnect), and
    `TestEmbeddedCuesDecode` passed under the same flags for the embedded `question.wav` PCM. Still
    open: connect and disconnect a phone on the unit and confirm the cue plays once at the current
    master gain with no cue on disconnect, at an audible level. The host test proves emitted PCM,
    not acoustic level, and no real source was exercised.

26. **Closed on this host (2026-11-17).** Every app-level suite now has reported output: the Simon
    contract suite (22/22) and its simulator run, the PA unit tests (29 + 28, `OK`) and `e2e.sh`
    (`PASS: install, boot, play, split play, rejections, no-progress frames, stall, cancellation,
    queued calls, setting bound`; `DONE main ok=15 fail=0`, `DONE settings ok=3 fail=0`), the
    Internet Radio unit tests (`Ran 57 tests … OK`, eighteen of them
    timer/watchdog/authorization
    cases) plus the
    host prelude self-test (`Ran 12 tests … OK`), its deterministic `e2e`
    (`PASS`, authorizing push logged) and its live `e2e-tuner` (`PASS` against the final tuner,
    zero refusal/stall lines), both e2e scripts including the autonomous-watchdog phase (tuner
    killed with no user interaction → `tuner stopped` error state), and the bridge's Go suite plus
    its simulator lifecycle. `make test-apps` remains the
    entrypoint; re-open this gate only if an app's suites change.
27. **Close the remaining live-install gaps.** Simulator evidence now covers a fresh install, a
    replace, the running-app refusal, removal, the browser upload/remove/live-status session and
    the no-daemon offline fallback (see the host section). Still unexercised: installing while the
    daemon is stopped and then restarting to see the package active, the "cannot tell whether
    capsulad is running" refusal (a socket that fails for a reason other than absence), a real
    65 MiB browser upload, upload timing over a slow link, and any device run. A real 65 MiB
    upload and the device are the material ones.
28. **Play a station on the unit.** The host side is settled, including the authorization: the live
    `e2e-tuner` PASS was re-run against the authorizing tuner package with an authorized `Tune`, an
    authorized `Stop` on a station change and zero refusal lines. Still open: the same path on the
    device — the tuner's outbound TLS/DNS from inside the mount namespace, decoded PCM reaching the
    device mixer, and the display states under real key presses.
29. **Exercise the Home Assistant bridge against a real broker.** Point it at an MQTT broker (or
    a Home Assistant MQTT integration), confirm the retained discovery payloads and one state
    topic per entity, apply a command to the ring and the display and see the state republish,
    kill the app to see the broker last will flip availability, and use `release_hardware`. All
    bridge evidence today is host unit tests; no broker, no Home Assistant and no device was
    contacted.
30. **Package and run the native apps on the unit.** Build `org.capsulad.radio.tuner` and
    `org.capsulad.homeassistant` with the aarch64 toolchain and install them, so the tuner's
    outbound TLS/DNS path and the bridge's MQTT session are exercised from inside the mount
    namespace. **Partly closed on 2026-11-17:** both apps were built for aarch64 / glibc ≤ 2.33,
    installed on the unit and their runs reached `APP_READY` (see the second deployment above).
    Their network paths — tuner outbound TLS/DNS from inside the mount namespace, bridge MQTT
    session against a real broker — remain unexercised, and no audio was measured.

31. **Confirm the 2026-11-17 second deployment's post-restart state.** Open because the unit left
    the network during the follow-up restart and could not be reached afterwards (see that entry
    for the observations; no cause is established): startup behavior of the installed stack, one
    run per app (two `org.capsulad.marusia` runs survived the tmpfs OOM restart storm), the
    sandbox mode of a healthy start (`sandbox=none` was reported by runs started during the OOM
    window, against `mountns` in the earlier record), and an app-level start/stop of the QuickJS
    app `org.capsulad.simon`. `build/linux-arm64/release-20261117b/verify-after-recovery.sh` runs
    those checks once the speaker is reachable.
32. **Verify the watchdog contract on the unit**, which no host test can reach: (a) `kill -STOP`
    the daemon and confirm the board resets about 19–21 s later — SIGSTOP freezes the supervisor
    too, so the stall window never runs and the reset is just the tail of the 21 s armed
    timeout after the last keepalive — with `dw_wdt: keepalive missed, machine will reset` on the
    console; (b) with a throwaway build that wedges the **output loop** while the supervisor keeps
    running, confirm the *health gate* causes that reset too, i.e. the log shows
    `no core progress for 10s` (the window is `min(timeout/2, 10 s)`, so the message on this unit
    says 10 s) while the process is still alive, and the board resets at window + ≤2 s (last
    keepalive) + the armed timeout later (≈33 s here; the host tests prove the decision, not the
    reset); (c) `S99btctl.sh stop` keeps the board up inside the 5 min feeder window and
    `start` reclaims the timer with no reset and no `EBUSY` in the boot log — note the reset after
    that window is by design, not a failure; (d) `disable` releases the feeder so the vendor chain
    owns `/dev/watchdog` again, and the boot log shows no vendor watchdog open failure. Record the
    timeout the driver reports (`WDIOC_GETTIMEOUT`) while there: the cadence and the window are
    derived from it, so a unit that arms a shorter timer than the profile assumed is fed faster and
    gated sooner. Confirm the `nowayout` facts the profile records while there (magic close
    ignored, `WDIOC_SETOPTIONS` → `ENOTTY`, no `/sys/class/watchdog`); they only choose the wording
    of the no-descriptor release path now, because the supervisor hands any transferable descriptor
    to a feeder regardless, so a different result does not change the handoff decision.

No device evidence exists for the networking surface described in
[app_development/network.md](app_development/network.md): outbound streams, UDP, listener
allocation and mDNS publication are host-verified only, and these three gates must close before
any device support is claimed. Do not read a host loopback fixture or the fake responder as a
device result, and do not claim Avahi visibility, LAN reachability or device TLS/DNS behavior
from this record.

## MCP frontend (2026-11-17)

The daemon's MCP frontend (`/mcp` on the Web UI listener) is **host-verified only**; no
device was accessed for this change. `go test ./internal/webui -run TestMCP` exercises the
real HTTPS endpoint: `initialize`, scope-filtered `tools/list`, a `tools/call` driving the
shared operation layer against the in-process fake daemon, protocol and tool errors, a
status-scoped key refused on an operator tool, explicit key-scope provisioning, asset
upload/list/delete, package install/remove, content negotiation, cross-origin rejection
and session DELETE, plus version-dependent protocol
cases (batching refused for 2025-06-18 and accepted for 2025-03-26, `MCP-Protocol-Version`
rejection, session reuse on a repeated `initialize`, `notifications/cancelled` ending a
live device recording). `-race` covers the session registry and lifecycle teardown: session
`DELETE`, listener shutdown and API-key revocation (directly and through the access-topic
watcher) cancel the daemon-side wakeword run, staged uploads and in-flight tool calls.
Staged multi-chunk asset and package uploads are exercised end to end, with `max_bytes`
equal to the store's 32 MiB and the artifact archive limit. Concurrency and ordering have
dedicated regressions: completion waits for a chunk already in the pipe (no truncated
publish), an abort releases a blocked chunk, two interleaved staged uploads publish their
own bytes, a paused upload does not stall `list_assets`, finished uploads are reaped from
the session's slots, and expiry reasons stay legible as failed tool calls. The asset store
has direct regressions for the staging budget (a streaming body holds its reservation, a
concurrent over-budget body is refused before staging anything, a failed body releases
its reservation, a replacement needs staging headroom so a full-store replacement is
refused, the physical published-plus-staged footprint measured on disk stays within the
budget during a concurrent replacement, and published bytes equal the budget) and for
delete-wins (a delete that lands during a replacement
succeeds and the replacement is refused at both catalog and MCP level, while a later
upload recreates the asset). The schema generator is covered with a two-oneof descriptor
built in the test. A slow-body test proves the MCP request read deadline is extended past
the listener's short timeout, and the shared asset store copies bodies outside its lock
(preparation and the budget-checked rename stay serialized). The browser proof for the Access change is the WebSocket `TOPIC_ACCESS`
snapshot carrying each key's scopes, the node test for the view's scope label helper and
the rebuilt embedded bundle; no screenshot of a running speaker was taken. `go test
./internal/websecurity -run TestAPIKeyScopes` covers explicit scopes,
operator-implies-status, unknown-scope rejection and a persisted legacy (no-scope) key
staying read-only across a reload. Not verified: a third-party MCP client or conformance
suite, a maximum-size staged upload on the real 240 MiB board, and any MCP behavior
against a device. See [MCP.md](MCP.md).

## AirPlay 2 receiver (host proof only)

`org.capsulad.airplay` was implemented without a device and without an iPhone, iPad or Mac
sender. Everything below is host evidence; the sender-negotiation and acoustic gates are open.

- **Protocol and crypto.** `make test-airplay` (also `cd apps/airplay && go test ./...` with cgo
  and the host external linker) covers: SRP-6a/setup and pair-verify against a test-only HAP
  client with matching channel keys, a wrong proof and an unknown controller refused; HAP block
  framing round-trips including a tampered block; binary/XML plist and TLV8 codecs; RSA-OAEP
  (SHA-1, the reference's mode) key exchange with an OpenSSL-generated fixture; RTSP parsing with
  pipelining, LF-only input and size caps; DMAP metadata and progress.
- **Codecs.** ALAC is verified bit-exactly against five checked-in fixtures produced by ffmpeg
  (16-bit stereo 44.1 kHz, 16-bit mono 48 kHz, 24-bit stereo, the verbatim path and a silence
  run), each with its reference PCM; the 24-bit case compares against `reference >> 8`. RTP
  decryption uses the sender's ChaCha20-Poly1305 construction (trailing 8-byte nonce, RTP header
  4..12 as AAD) and AES-128-CBC is checked against the NIST SP 800-38A vector; reordering,
  duplicates, wrap, NACK and RTCP reports are covered.
- **Session lifecycle.** One end-to-end test acts as a sender over in-memory transports:
  pair-verify, encrypted control channel, `SETUP` of a realtime PCM stream, encrypted RTP audio,
  sender-volume gain applied to the PCM the framework sink receives, DMAP metadata visible in the
  media contract as canonical protobuf JSON, ring frames, then `TEARDOWN` releasing the player,
  the ring presentation and aborting playback.
- **AAC-LC through FFmpeg.** The receiver links `libavcodec`/`libavutil` (`-tags ffmpeg`, the
  build script refuses to package without them), matching upstream, which links the same decoder
  for its AAC-LC buffered formats. `go test -tags ffmpeg ./internal/aac` decodes fixtures made by
  ffmpeg from two encodings (44.1 kHz stereo, 48 kHz mono) against ffmpeg's own decode of the same
  file: the mono fixture is sample-for-sample identical and the stereo fixture is within one LSB,
  the difference between two PCM quantisations. `go test -tags ffmpeg ./internal/session -run
  TestAACStreamPlays` drives the full path — pair-verify, encrypted `SETUP` with the
  `AAC-LC/44.1K/F24/2` mask, encrypted RTP carrying the fixture's real access units, PCM at the
  framework sink — and `SETUP` advertises feature bit 20 and RAOP `cn=0,1,2`.
  Verified as upstream-advertised: `rtsp.c` `bufferStreamFormats` lists AAC-LC 44.1K/48K F24
  stereo (and 5.1/7.1, which this receiver does not accept). AAC-ELD is decodable by the same
  FFmpeg decoder but is **not advertised**, because upstream advertises no ELD format either.
  The packaged binary's `otool -L` records `libavcodec.63.dylib`/`libavutil.61.dylib`, so the
  shipped artifact really contains the linked decoder rather than a stub path.
- **Full independent pyatv loopback flow (fixed two wire defects).**
  `PYATV_PYTHON=<venv> go test ./internal/session -run TestPyatvFullFlowStreamsPCM` starts the
  receiver on a real loopback transport and lets pyatv 0.18 drive its own stack
  (`AirPlayService`/`RaopService` → `pyatv.connect` → `atv.stream.stream_file`). It now passes:
  pyatv performs ANNOUNCE and SETUP, streams the tone as RTP, and this receiver delivers
  **611,072 bytes of PCM** to the framework sink before teardown. Reaching that required two
  fixes the independent client exposed:
  1. **Binary-plist dictionary layout.** `internal/plist` emitted dictionary object references
     interleaved (key, value, key, value); the format requires all key references and then all
     value references. Python `plistlib` rejected every multi-key document
     (`unhashable type: 'list'` while reading a key), so `/info`, SETUP responses and event
     documents were invalid for real senders. Encoder, decoder and the hand-written fixture now
     use the spec layout, and `internal/plist/plistlib_test.go` (gated on `PLISTLIB_PYTHON`)
     validates an /info-shaped, a SETUP-shaped and an `updateInfo` document with Python's
     plistlib — an independent parser, not a round trip through our own decoder.
  2. **Legacy RAOP transport choice.** pyatv sends
     `RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=…;timing_port=…` and expects a
     UDP `server_port`/`control_port` reply; the receiver now prefers the UDP path whenever the
     Transport header contains `udp` instead of treating the presence of `interleaved` as TCP.
     The earlier `control_port=0` finding (R1) is covered by the same path.
  Both tests are gated on environment variables, so the default suite cannot report a skipped
  external proof as a pass: run them explicitly with
  `PLISTLIB_PYTHON=<python with plistlib> PYATV_PYTHON=<python with pyatv> go test ./internal/plist ./internal/session -run 'TestPlistlib|TestPyatv'`
  (pyatv needs `srptools==1.0.1` for the current release). A CI environment should set both
  variables, otherwise these proofs exist only when run by hand.
- **Independent pyatv interop (A1/A2).**- **Independent pyatv interop (A1/A2).** `PYATV_PYTHON=<venv python> go test
  ./internal/session -run TestPyatvTransientPairingKeys` runs **pyatv's own** SRP client against
  this receiver's pair-setup server: pyatv computes A and the client proof from the receiver's M2,
  the receiver accepts it and completes a transient setup at M4, the two sides' SRP session keys
  (SHA-512(S), 64 bytes) are identical, and the accessory control-channel keys derived by
  `hapcrypt.ControlChannelKeys` equal pyatv's independent HKDF derivation (outbound
  Control-Read, inbound Control-Write). Scope recorded honestly: this proves the pairing/key
  interop with a real third-party implementation; it is not a full pyatv HTTP session. The test
  skips when `PYATV_PYTHON` is unset, so it cannot be mistaken for a default suite result.
- **Transient pairing and channel keys (review A1/A2).** `go test ./internal/session -run
  TestTransientPairingDrivesTheWholeSession` performs the exact pyatv sequence — `/pair-pin-start`,
  M1..M4 with the transient flag, then every request and the audio channel keyed from the SRP
  session key — and proves the channel is usable end to end (encrypted `/info`, `SETUP`,
  encrypted RTP, PCM at the sink). `TestPairSetupTransientCompletesAtM4` pins the M4 completion
  and that an M5 afterwards is refused. `internal/hapcrypt` now returns named `BlockKeys`
  (outbound/inbound) per channel, and `TestKeys` pins both directions against HKDF-SHA512
  vectors generated independently in Python: the accessory seals control with
  `Control-Read-Encryption-Key` and events with `Events-Write-Encryption-Key`, the asymmetry
  Shairport Sync documents. A pair-verify after an established channel never re-keys it (upstream
  comments out that path); the non-transient path, for which upstream has no working cipher,
  keys from the ECDH secret as a documented deviation.
- **UDP realtime audio and the AP2 control plane (review A3/A4).** Realtime streams now bind a
  daemon UDP data socket, and one session-wide UDP control socket is echoed in every descriptor.
  `TestAP2ControlAnchorResendAndResponse` builds the sender's datagrams from the reference layout
  (not from this package's encoder): a `0xD7` anchor whose `frame_2 - frame_1` latency (77,175
  frames) and anchored frame (`frame_1 - 11,025`) are read back from the stream; a missing
  sequence produces one `80 D5 00 01 <first> <count>` request on the control socket after the
  reorder grace; and a `0xD6` response, whose payload starts at the RTP sequence number, is
  decrypted and played by the normal pipeline. RTCP is no longer part of the AirPlay path: no SR,
  RR or generic NACK is sent or expected.
- **PTP is consumed, not just parsed.** A `0xD7` anchor plus a disciplined clock converts the
  sender's master-clock instant to local time for each frame; without a clock the receiver paces
  from arrival. The PTP feature bits are advertised only when a clock exists, so the
  advertisement cannot promise timing nothing uses.
- **Lifecycle and SDK fixes (review C1-C7, D1/D2).** Declared actions are handled and report
  results; a closed listener is removed from the runtime registry (no growth across restarts); a
  settings restart withdraws the old advertisement before publishing the new one and restores the
  previous configuration when the new one cannot start; a session is capped at four audio
  streams; board writes are generation-guarded (`internal/media` tests include a stale-write and
  a subscriber/unsubscribe race, and a copy of the old publish path was shown to fail `-race`);
  the native SDK parses `join_group` independently of JSON key order and refuses `listener_send`
  on a multicast listener (Rust tests in `listener.rs`).
- **Pairing ownership and administration.** `go test ./internal/session -run TestPairSetup` drives
  the full controller side of pair-setup with the receiver's setup code (default `3939`, the value
  upstream's accessory uses when no AirPlay password is configured, `pair_homekit.c`
  `server_setup_new`), including a refused wrong code (`kTLVError_Authentication`, no controller
  stored). Over the encrypted channel it then exercises `pair-list` (only the administrator, with
  its public key and permission), `pair-add` of a non-admin controller, `pair-remove` of that
  controller and of an unknown id (`kTLVError_Unknown`), a demoted-controller refusal, and removal
  of the session's own pairing ending the session. `./internal/pair` covers the store: permissions,
  the legacy combined format, split `device_identity`/`pairings` serialisation and the
  compare-and-swap persistence path. Upstream keeps pairings in memory with no permissions and no
  session check (`rtsp.c` `struct pairings`, `server_list` hard-codes admin and ignores the
  request); this receiver is deliberately stricter, and the app README records the difference.
- **FairPlay handshake.** `POST /fp-setup` now serves the four fixed protocol replies and the
  header echo with upstream's selection rule (`rtsp.c` `handle_fp_setup`), including the
  `application/octet-stream` content type, so a sender's handshake completes. No DRM content is
  decrypted by either implementation; the constants are reproduced under Shairport Sync's MIT
  notice, recorded in `README.md`.
- **Group advertisement.** A sender's `gid` from `SETUP` is recorded and republished as mDNS `gid`
  with `gcgl=0`, matching `bonjour_strings.c`; no cross-receiver playback synchronisation is
  implemented, and none is claimed.
- **SDK extension (multicast PTP).** A PTP clock needs the sender's multicast stream. The daemon
  accepts a narrow `join_group` listener option: UDP only, ports 319 and 320 only, the group must
  be exactly 224.0.0.1, the bind must be the wildcard, and the socket is receive-only. The daemon
  (root) binds the privileged port once with `SO_REUSEADDR` (plus best-effort `SO_REUSEPORT`) and
  joins the group on every up, multicast-capable, non-loopback interface; membership lives on the
  socket, so it ends with the transferred descriptor and run exit, and the allocation is counted
  against the existing per-run listener cap and logged. `go test ./internal/appcontrol` pins the
  refusals and the interface-selection rule. Nothing else about the listener contract changed and
  ABI 1.0 is untouched (the option is a JSON key, not a table entry).
- **Target packages (musl/glibc).** The delivered packages were built in arm64 containers from a
  pinned working-tree snapshot: both are dynamically linked against their target libc
  (`interpreter /lib/ld-musl-aarch64.so.1` with `NEEDED libc.musl-aarch64.so.1` for musl;
  `/lib/ld-linux-aarch64.so.1` with `libdl/libm/libpthread/libresolv/libc` and a maximum of
  `GLIBC_2.28` for the stock glibc ceiling), and the AAC decoder is linked **statically**
  (`libavcodec.a`/`libavutil.a`; no `libavcodec.so*`/`libavutil.so*` in `DT_NEEDED`). Because the
  decoder is static, `apps/airplay/NOTICE` §6's relink package (objects archive, FFmpeg source
  URL+sha256, configure line, relink procedure) is a **distribution precondition**, not optional.
  The musl loader smoke with the real musl `libcapsulad` proves exactly: the loader resolved the
  binary's `DT_NEEDED` closure and `dlopen` of the injected SDK succeeded, then execution stopped
  at our own `run_id is required`. It does not prove a framework session, the app bus, an AirPlay
  session, decode or output. Rebuilt artifacts from the final source, with these `DT_NEEDED` and
  link-mode facts recorded in the same report, are tracked separately.
- **Packaging policy.** `make airplay` stages a development build on the host and writes no
  archive (`PACKAGE=auto`); `PACKAGE=1` on a non-Linux target is refused unless
  `AIRPLAY_ALLOW_HOST_PACKAGE=1` is set for a packaging smoke test, so a Darwin binary can never
  be shipped with a Linux ABI. A Linux build stamps `abi` through `tools/stage-manifest-abi.sh`,
  runs `tools/check-elf-interp.sh` on the staged binary before packaging, and fails when the
  interpreter does not match the manifest's libc. The host package that earlier rounds recorded
  is therefore gone; the target artifacts and their ELF evidence are recorded below once built.

Proof limits (open gates):

- The pyatv proof is library-level (SRP/HKDF), not a full HTTP session; a live pyatv end-to-end
  run over a loopback daemon seam remains the one unexecuted external proof. `pyatv` needs a
  compatible `srptools` version (1.0.1 was required for the installed release).
- No sender was available, so the advertisement set (`srcvers`, feature bits) has not been
  confirmed to make a current iOS release choose a stream this build decodes. Both values are
  app settings so a device test can settle it; the app README lists the values that were
  considered and why.
- The recorded package is a **host** build: no aarch64 `libavcodec`/`libavutil` and no cross
  toolchain were available, so the device package with the AAC decoder was not produced here.
  The build's ABI guards were exercised: `GOOS=linux` without `AIRPLAY_FFMPEG_PREFIX` fails with
  the prefix instruction, and with a prefix but no cross compiler it fails rather than using the
  host compiler; the staged-manifest rewrite was checked to produce exactly one
  `abi = "linux-musl"` line, and the packaged host build still declares the source default
  `linux-gnu`.
  A release needs the target FFmpeg built as upstream builds it (`--disable-everything
  --enable-decoder=aac --enable-parser=aac`, static) and pointed at with `PKG_CONFIG_PATH` or
  `CGO_CFLAGS`/`CGO_LDFLAGS`; `apps/airplay/build.sh` fails early without it.
- The pyatv flow covers the legacy RAOP path (pyatv's RAOP client chooses `AirPlayV1` for this
  service advertisement). The AP2 pairing/key layer is covered by the pyatv SRP/HKDF test above;
  an AP2 realtime SETUP driven by pyatv itself is not exercised (its AirPlay-v2 stream protocol is
  reached only through its own mDNS/feature negotiation, which the loopback config does not
  reproduce).
- AAC-ELD is not advertised (upstream does not advertise it either); if a sender sends ELD the
  FFmpeg decoder accepts it, but no ELD fixture could be produced — ffmpeg has no ELD encoder —
  so that path is unverified.
- Normal pairing and the administration endpoints are verified against this repository's test
  client, not against a HomeKit administrator app: the wire format is covered, the HomeKit
  accessory-database semantics beyond add/remove/list are not implemented.
- FairPlay-protected content and MFi remain out of reach: only the handshake is answered, which
  is all upstream does.
- PTP is implemented as a slave clock over the new multicast listener, but its lock quality,
  the sender's actual PTP profile and its effect on multi-room sync are untested; without a
  disciplined clock the session paces from packet arrival, which is what the end-to-end test
  exercises.
- The four reverse transport commands over the sender's event channel are implemented to the
  reference layout but have never been accepted by a real sender in this record.
- Acoustic latency, ring colour fidelity on the panel and behaviour under the 240 MiB memory
  bound are device gates.

## Amlogic A113X platforms (host proof only)

Both Amlogic platforms were added without a device: no Amlogic unit was connected, so every line
below is host proof. The physical facts they depend on come from the published survey and the
third-party projects cited in [HARDWARE.md](HARDWARE.md#amlogic-a113x-stock-buildroot-and-alpine-linux);
they are not measurements.

What was checked:

- `go test ./internal/backend/amlogic` — profile→config selection (stock=pulse, Alpine=alsa,
  `default` PCM, 48 kHz stereo playback / 16 kHz mono capture), capability reporting per
  userland, the ring's vendor-node/`UNVERIFIED`/`UNAVAILABLE` paths, the three panel paths
  (per-segment LED class, line display, unrecovered vendor node), the fallback when one segment
  device is missing, and `SoC` not being replaced by the model-probe fallback.
- `go test ./internal/backend/linuxdev` — the moved board-agnostic layer: keymap semantics,
  kernel-timestamp decoding, IIO/thermal probes, LED-class classification/trigger claim, WS2812B
  gamma/permutation encoding, brightness quantization.
- `go test ./internal/backend/rockchip ./internal/backend/rockchip/inspect` — the refactor that
  moved evdev/keymap/sensors/LED-class/WS2812 into `linuxdev` did not change the Rockchip
  backend's behaviour, and the offline inspector now reports `platform` and identifies an
  Amlogic rootfs instead of applying Rockchip readings.
- `go test ./internal/daemon -run 'TestProfileOrDefault|TestPlatformProto'` — platform string →
  profile and backend platform → wire enum for both Amlogic targets.
- `go test ./cmd/capsulad-cli -run 'TestSelectDeployProfile|TestPlatformFlag'` — the `-platform`
  deploy flag selects the boot service, hook name and space floors for all four selections and
  rejects anything else.
- `GOOS=linux GOARCH=arm64 go build ./...` — both binaries and the CLI cross-compile for the
  target (this is a compile check, not a run).

Proof limits:

- No frame reached a ring, panel, mixer or watchdog on an Amlogic board. `PARTIAL` there means the
  interface is driven and the mapping is recovered, not that the device rendered it.
- The stock clock panel is driven through the `clock-led1..30` LED class array with the channel
  table published in the board's own MicroPython hardware layer (`STOCK_MAP`/`LAYOUT`, colon 16/15);
  host tests pin the mapping and the frame writes. The private `/dev/is31fl32xx` ioctl path is not
  used, and when the LED class array is absent no segment resource is published at all (the
  subsystem reports `UNVERIFIED`), so a frame can never reach a no-op sink.
- The glibc stock runtime was built against glibc 2.28 (Debian buster, rustc 1.100,
  `tools/build-native-glibc228.sh`): 6,313,560 B, sha256 `620297dd…`, `DT_NEEDED`
  libgcc_s/libpthread/libm/libdl/libc, max `GLIBC_2.28`. On a buster image it loads and the app
  reaches `connect failed (7)` for a nonexistent socket; a 2.30 build fails the loader at
  `libm.so.6: version GLIBC_2.29 not found`. Proof level: host container load only, no device.
- The musl native runtime was built and loaded on the host: `cargo build --release --target
  aarch64-unknown-linux-musl` with `-C target-feature=-crt-static` in an arm64 Alpine container
  produced `libcapsulad.so` (SONAME `libcapsulad.so.1`; `NEEDED libc.musl-aarch64.so.1`,
  `libgcc_s.so.1`), `tools/check-native-libc.sh` accepted it as musl from the ELF dynamic section,
  and a C `dlopen`/`dlsym("capsulad_abi_major")` smoke resolved the symbol under
  `/lib/ld-musl-aarch64.so.1`. The metadata is the usual three-field JSON. No Alpine device was
  used, so service start, app launch and audio remain device gates.

- The stock Amlogic ambient-brightness curve is unmeasured; the profile uses the framework policy
  curve and claims no vendor parity.
- Stock autostart depends on an init-list/overlay fact that was not tested on firmware 44.90; the
  hook's `start|stop|restart` path is what a deployment exercises in the current boot.
- The native SDK was not cross-built for `aarch64-unknown-linux-musl`/`-gnu` here; only the Go
  binaries were.
