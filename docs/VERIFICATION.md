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
  private mount namespace, mapped `/capsula/lib` with ABI 1.0, reached `APP_READY`, and exited
  cleanly on stop.
- The optional `org.capsula.dumbassistant` assistant and the native runtime were replaced while
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
  reports it. Apps on older native runtimes must use `CAPSULA_DEVICE_SERIAL`; the deployed library
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

## Explicitly unavailable on this hardware

- **User, PID, network and IPC namespaces and seccomp filters.** The stock kernel offers mount
  namespaces only. Sandboxing is therefore a read-only mount namespace plus a distinct UID;
  native apps are trusted code.
- **Raw touch coordinates.** There is no coordinate panel, so `coordinates_available=false`
  and `full_raw_available=false`. Full-panel delivery remains available.
- **Amlogic stock and Alpine backends.** Platform selection fails explicitly.
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

## Open gates

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
7. Run a MicroPython package on the unit, not just its host.
8. Exercise a real Wi-Fi association, the setup AP, DHCP/portal detection and AP teardown on
   the target; verify stock AP-mode scanning or the absence of a scan path.
9. Exercise a power cycle with the single boot hook, including `disable`/`enable` and the
   vendor fallback, and verify recovery from an overlay wipe.
10. Test the Amlogic stock and Alpine backends on hardware before claiming any parity.
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