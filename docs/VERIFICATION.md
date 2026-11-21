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
The per-application boot preference requires a target restart/power-cycle check; no device
evidence for this preference has been collected here.

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
  private mount namespace, mapped `/capsula/lib` with ABI 1.0, reached `APP_READY`, and exited
  cleanly on stop.
- The optional assistant and the native runtime were replaced while the daemon was stopped
  through the boot service; the service restart preserved settings revision and all user
  choices, and all three expected runs reported `APP_READY`.

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
- A passive trace captured a 310 ms circle press, standalone volume taps, a requested swipe
  that produced no kernel contacts, and one overlapping centre→volume-down glide (95.7 ms
  apart) scored as `swipe_left` by the installed recognizer. Contact order of the user's
  intended swipe is not established, so the trace neither proves nor refutes an inversion.
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
  volume wrote transport `Volume=64` of 127.
- The native A2DP SBC sink endpoint was registered and reported ready at daemon start; the
  registration is verified before the adapter is made connectable.
- Connect plays three completed blue fade-in/out ring pulses and shows lowercase `bt` on the
  two rightmost segment cells for 2 s; disconnect plays exactly one pulse and no glyph, over
  the selected baseline.
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

### Personal wakeword

- Native on-device training completed in **670.6 s**. A replay on the device A35 scored 6/6
  word takes and 9/9 backgrounds. Installation evidence, not an accuracy benchmark.
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

## Verified only on the simulator or host

- Complete virtual backend (segment display, RGB ring, contacts, keys and audio) and every
  Go test suite, including `-race` runs for input, routing, audio, settings, supervisor,
  Bluetooth ordering, discovery and web UI.
- Browser behaviour beyond the runs listed above: jsdom and headless runs for login/session,
  CSRF, API-key flows, live control coalescing, unsaved-edit preservation, diagnostics ordering and
  the settings-document bridge. The 390 px and 849 px surfaces were exercised through a
  throwaway loopback HTTP→TLS/WebSocket proxy in a native Tern browser; the managed WebKit
  build crashed in EGL and Chromium failed its Mach-port launch, so a full desktop viewport
  and a physical phone are unverified.
- The application modal was exercised against an isolated simulator in desktop Chromium: the
  selected clock showed its display-controller badge, running/restart controls, disabled boot
  checkbox, embedded settings document and nested default-provider and interface dialogs.
  Launch-argument and boot-preference changes have not been exercised on a physical speaker.
- Wi-Fi manager logic: stock `wpa_supplicant.conf` import, priority-ordered association,
  previous-network recovery, scan capability and token expiry against fixtures; captive
  portal host/origin/token/subnet behavior against `httptest`.
- Avahi apply ordering against a portable fake responder and a barrier-driven private-bus
  adapter fixture.
- BlueZ `MediaControl1` object/method calls on the wire against a private bus; AVRCP metadata
  parsing and the media fallback through the real input manager.
- QuickJS/MicroPython smoke coverage: ready, unary, events, routed unary, routed stream and
  cancellation, Stop/SIGKILL cleanup, and a daemon-crash phase delivering exactly one bounded
  terminal disconnect per host.
- Provenance verification against genuine Sigstore/GitHub fixtures; the settings-UI route
  returns both built-in documents with the expected headers and CSP.
- All packaging, manifest validation, extraction limits, settings CAS, log rotation and
  supervisor policy paths.

## Explicitly unavailable on this hardware

- **User, PID, network and IPC namespaces and seccomp filters.** The stock kernel offers mount
  namespaces only. Sandboxing is therefore a read-only mount namespace plus a distinct UID;
  native apps are trusted code.
- **Raw touch coordinates.** There is no coordinate panel, so `coordinates_available=false`
  and `full_raw_available=false`. Full-panel delivery remains available.
- **Amlogic stock and Alpine backends.** Platform selection fails explicitly.
- **Vendor system-service control.** `SystemService.ControlUnit` returns `UNIMPLEMENTED`.
- **A field-certified wakeword model for the connected room.** The installed model was
  trained on raw audio and runs on the processed tap, so its calibrated threshold is
  unverified; no speaker-independent keyword head ships.
- **Wi-Fi RF and provisioning on the target.** Physical association, stock AP-mode scanning,
  DHCP/DNS/firewall behavior and captive-portal detection are unproven; the target firmware
  has no `hostapd`/`iw`.
- **A2DP listening parity by ear and real-peer recovery.** The native endpoint registered and
  a transport was created, but no sustained listening test with the corrected transport
  selection has completed. Recovery of a peer that already abandoned A2DP before the daemon
  started is device-dependent.
- **Photometric brightness and gamma.** No photometer measurement exists.
- **Unpairing without disrupting the user's source.** The path is exercised on the simulator
  and rejects supervised callers; the user's paired source was deliberately not removed.

## Open gates

1. Measure emitted light and gamma for both output channels, including the 5 % auto-brightness
   floor, and compare the standby baseline with the vendor firmware.
2. Exercise the five-key panel with the current recognizer: single-key timing after the 250 ms
   window, 700 ms hold summon, overlapping edge-to-edge crossings without volume leakage,
   sequential opposite taps and circle-then-microphone as deliberate keys, and no effect from
   an unfinished sequence.
3. Listen to the native A2DP path with a playing source and compare it with the vendor stack
   under dropouts; verify `MediaTransport1.Acquire` and the SBC decoder under real jitter.
4. Verify the first incoming Bluetooth connection after BlueZ autopowers the adapter, and
   whether a peer that already abandoned A2DP recovers on the same link without reconnecting.
5. Repeat the mDNS browser proof on the device, including a real write-time collision and
   invalid-label rejection, and confirm `avahi-resolve` after an Avahi restart.
6. Evaluate the assistant endpointer and the wakeword model against real room noise, loud
   music and quiet speech over long durations; the acoustic gate cannot be certified by
   simulator PCM.
7. Run a MicroPython package on the unit, not just its host.
8. Exercise a real Wi-Fi association, the setup AP, DHCP/portal detection and AP teardown on
   the target; verify stock AP-mode scanning or the absence of a scan path.
9. Exercise a power cycle with the single boot hook, including `disable`/`enable` and the
   vendor fallback, and verify recovery from an overlay wipe.
10. Test the Amlogic stock and Alpine backends on hardware before claiming any parity.
11. Freeze protocol schemas only after generated Go/Rust bindings and script-host parity
    tests pass, including routed streaming and cancellation.