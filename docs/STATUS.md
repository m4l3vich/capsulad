# Implementation status (initial slice)

Target schemas: draft v0.5 (`proto/capsula/framework/v1/framework.proto` and the standard app
contracts). Generated Go bindings are committed under `gen/go`. Predefined external service shapes
are the framework services plus `capsula.contracts.media.v1.MediaPlayerService`,
`capsula.contracts.clock.v1.ClockService` and `capsula.contracts.ring.v1.RingService`. In this source
tree the built-in clock and ring apps declare those contracts through `AppInterfaceService`, so
ordinary supervised apps can discover and call them over the app bus. The framework protoset and
Go bindings for both contracts were regenerated in the same source tree and the verifier observed
the final generated outputs; the change has not been deployed to a device. The wire API is **not
frozen**.

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
`bt` glyph; the sink endpoint is verified/repaired before connecting, PA endpoints are
not removed while a source is actively routed through them, and the two audio profiles
connect concurrently.
Dumb-assistant speech detection is relative to a learned room-noise floor; an utterance ends
after 700 ms in which the 300–3400 Hz band level has fallen clearly below the recent voiced
level, so a sustained voiced vowel is preserved rather than treated as silence.
The ring app also has an optional active window: local `HH:MM` bounds (device
timezone, crossing midnight allowed) present an explicit blank baseline frame
outside the window, with one timer per boundary instead of per-frame polling,
and boundary/timezone/settings changes republish the frame.
These paths have unit and simulator-daemon regression coverage. The stopwatch,
Bluetooth and speech-floor paths were not re-exercised on the Rockchip unit by
the deployment below (the ring active window was), and the acoustic-room,
real-Mac Bluetooth and listening gates remain open.

### Contract update (2026-11-13): recovery, panel/media, microphone, A2DP, mDNS

The current behavior contract changed in eight places. Focused unit/simulator checks
accompany each; the integrated verifier run, device, real-peer and acoustic gates are listed
separately and are not implied by simulator results.

> **Deployment note.** This section records the contract of the binary installed on 2026-11-13.
> Four later user-reported live failures produced source corrections — panel-swap media
> control, wakeword preprocessing, assistant endpointer and mDNS idempotence. They are summarized
> in [Follow-up corrections](#follow-up-corrections-2026-11-13-deployed), and the corrected stack
> is the one installed since 2026-11-13 16:19 MSK (see the dated deployment entry there). Where
> the two differ, the follow-up section is current.

- **Selected-provider recovery.** Stopping a selected default provider (display, ring or
  assistant) is a recovery event, not an off switch: `Stop` relaunches it immediately, and later
  abnormal exits back off 1/2/4/8 s, even when the manifest has `restart_on_failure=false`. The
  backoff resets only after a run stays READY for 30 s, so a READY-then-crash loop keeps backing
  off instead of restarting forever at zero delay. The supervisor policy is installed once at
  daemon initialization rather than on each launch. Selected-origin crash recovery and forced
  recovery stops re-check selection at the scheduled start, regardless of `restart_on_failure`, so
  deselection (including `none`), uninstall or shutdown during the stop or backoff suppresses the
  relaunch; a manifest-declared restart of an app that was never selected keeps its ordinary
  contract. Ownership is released on exit and a later manual or wakeword activation routes a fresh
  ready run. The Web UI labels the control **Restart** and explains that deselection is the off
  switch.
- **Panel mapping and gestures.** Capsula Mini is five discrete keys: microphone mute
  (north), volume up (east), circle/play-pause (south), volume down (west) and an unmarked
  neutral centre. Full swipes are opposite → centre → direction; unambiguous centre-first
  half swipes are accepted; a closed perimeter traversal from any corner is
  `circle_cw`/`circle_ccw`. Recognition allows 250 ms between contacts and 1.5 s total.
  Per-key effects are deferred until a sequence fails or completes, so a swipe/circle cannot
  leak mute, volume or a summon; a failed multi-key gesture drops framework effects but still
  delivers app-owned circle/centre key events to a partial capture. The tradeoff is up to
  250 ms latency on every single-key action; a hold is measured from the press event.
  Held/repeat presses are not new contacts, and a timed-out prefix yields exactly one action.
- **Partial/full ownership.** Partial capture receives circle/centre key events and inferred
  gestures exclusively, while volume and microphone stay framework controls. Full capture
  receives every key and gesture and suppresses every system side effect, including mute,
  volume, media transport and assistant activation. `coordinates_available=false` and
  `full_raw_available=false` on the Mini do not disable full-panel delivery: the backend
  advertises `full_panel_available=true`, and raw coordinate samples are never fabricated.
- **System media.** A short circle tap toggles current media; left/right swipes skip
  previous/next. Priority is: alert dismissal and assistant-turn dismissal first, then a
  running stopwatch's own gesture contract, then media. Media selection is a standard
  provider that is playing, then one that is paused, then a connected Bluetooth source via
  the public `BluetoothMediaService`, then any other standard provider. No external media
  app is required for a Bluetooth speaker. Commands are serialized; full capture suppresses
  them.
- **Application microphone DSP.** The processed tap (`audio.input.microphone`,
  `framework.dsp.v1`) runs only for S16LE mono 16 kHz: 80 Hz high-pass, adaptive spectral
  suppression capped at 12 dB, unity gain without AGC and a −1 dBFS limiter, using a
  256/128 STFT (about 15.9 ms). The endpoint advertises `framework.dsp.v1` only while that chain
  is active; a disabled or unsupported backend format advertises an empty processing tap instead.
  State, latency and output length are fixed; chunk boundaries
  do not change output; mute, source reopen and capture gaps reset the chain. The raw
  endpoint (`audio.input.microphone.raw`) and the internal wakeword/enrollment frame
  subscriptions were the untouched physical vendor `speex` tap, and both wakeword paths
  rejected processed frames to preserve installed-model calibration (superseded by the local
  follow-up corrections below).
- **Dumb-assistant VAD.** The optional assistant consumes the processed app tap and measures
  onset/end in sample time against a learned room-noise floor, so stationary fan/traffic
  noise neither starts nor sustains a turn, real speech is detected under room noise, and the
  initial speech is not learned as background. Chunk count does not affect onset or end.
  (Superseded: the user observed listening under substantial room noise; see the level-fallaway
  endpointer in the follow-up corrections below.)
- **A2DP ownership and readiness.** The native A2DP Sink endpoint is registered through the
  public BlueZ `Media1` APIs and its generation-scoped registration is verified before the
  adapter is powered for connectable/discoverable/pairable use or an API request enables
  those. A failed or lost registration is never reported ready; discoverability/pairability
  are refused with the reason and `BluetoothStatus.Detail` reports the missing audio profile.
  PulseAudio remains the fallback owner when native registration fails. PA endpoints are
  removed only after the native sink is armed, and never while an active source is routed
  through PA; that switch completes after disconnect. BlueZ restart or adapter power cycle
  invalidates the registration and triggers repair before the next connect.
- **mDNS host name.** A single-label `Hostname` (default `capsula`) is a CAS-validated typed
  setting advertised as `<label>.local`. On the stock Linux image the daemon called
  `org.freedesktop.Avahi.Server.SetHostName` on the system bus instead of binding a second
  5353 responder. The requested label/generation is re-applied when Avahi reaches RUNNING and
  after responder restarts; the monitor subscribes before its initial snapshot, a later save
  wins over an in-flight reapply, and Close removes the match and joins the monitor. Invalid
  labels keep the prior stored/advertised name; responder failures clear the advertised claim
  and carry the observed FQDN. The simulator and other unsupported platforms report mDNS as
  unavailable and never rename the host. (The unconditional same-name write in this build caused
  the redundant-operation warning; see the idempotent apply in the follow-up corrections below.)

Verification status for this section: the integrated focused run on darwin/arm64 used
`-ldflags '-linkmode=external'` and the real Mach-O native runtime from `build/lib` with
`CAPSULA_NATIVE_LIB_DIR`. It passed `-race` suites for `internal/input`, `internal/backend/rockchip`,
`internal/wakeword`, `internal/settings`, `internal/discovery`, `internal/audio`, the three focused
supervisor policy tests, seven Bluetooth registration/rearm tests, focused Web UI hostname/restart
wording, seven daemon lifecycle paths, eight daemon Bluetooth ordering/readiness paths, the three
final media tests (including `TestSystemMediaFallsBackToConnectedBluetooth`), and all eleven
`TestDumbAssistant` scenarios. One race-only fixture deadline (2 s → 20 s drain for the 120 s
burst; burst/drop/backpressure behavior unchanged) was resolved as test instrumentation, not a
product defect. Fresh host CLI/daemon smoke exercised health/status, simulated circle press,
empty `media list`, shutdown and persisted-hostname startup (simulator correctly logged mDNS
unsupported). A real daemon plus Chrome-headless browser session then observed Settings → Device:
default `capsula`, `Living-Room` saved as `living-room` and persisted across a daemon restart,
invalid names rejected by both the HTML pattern and the server with the prior value retained, the
simulator's "no responder" warning with no false `Advertised as` line, and the selected-provider
**Restart** label/title on Clock, Ring and Diagnostics. That session did not prove actual
Avahi/`.local` resolution. No physical device, real Bluetooth peer or real `.local` resolution is
included, so those remain hardware gates. Two lifecycle policy edges found after the first
integrated pass were then repaired: a READY-to-crash loop now keeps backoff until 30 s of continuous
READY, and selected-origin/forced recovery re-checks selection regardless of `restart_on_failure`.
Controlled-clock regressions `TestReadyThenCrashKeepsBackoffAndStableRunResets`,
`TestSelectedManifestRestartRechecksSelection` and `TestStopForRestartRechecksSelection` pass; the
full `go test -race ./internal/supervisor` suite is clean, and the integrated daemon lifecycle
recovery set passes 7/7 against the real native runtime (36.9 s). Stop-window origin capture and the
builtin teardown re-check are covered by
`TestDeselectDuringBlockedTerminationSuppressesManifestRestart` and
`TestBuiltinStopDoesNotResurrectAfterDeselectionDuringTeardown`, each failing before the fix and
passing under `-race`; the contract above is unchanged. The media fallback test now drives
the real input manager and asserts semantic NEXT/PREVIOUS through the public `Playing`/`Title`
state (three-track playlist, direction and wrap), rather than a revision-only oracle;
`TestMemoryPlaylistNextPrevious` and
`TestSystemMediaFallsBackToConnectedBluetooth` pass under `-race` with the external linker. Final
repaired-source verification also passed under `-race` with the external linker: the full
`internal/input` suite (26.3 s), six daemon media/endpoint/controller-shutdown cases (22.6 s)
covering the effective processing tap when enabled, disabled and unsupported, no-provider Bluetooth
plus the standard three-track Title next/previous oracle, and no queued client work after close,
and all eleven `TestDumbAssistant` scenarios after the sqrt-Hann window change (91.3 s); the six
targeted DSP race tests were already passing.

### Ring active window deployed (2026-11-13)

Commit `089f589` (daemon sha256 `e0dd1e1a…3453925a6`) was installed on the unit
through `/etc/init.d/S99btctl.sh` with `capsula daemon install`: the health gate
passed, the previous binary is retained as `capsulad.prev`, and the host backup
is `_capsula-mini-deploy-backup/20261112T181428Z-ring-schedule/`. The restart
preserved user revision 2198 and every choice: clock/ring/dumbassistant
providers, ring effect `rainbow`/`spectrum` at speed 4, `Asia/Shanghai`,
microphone muted, 5 % volume; the new keys appear as
`schedule_enabled=false`, `schedule_start="22:00"`, `schedule_end="07:00"`.
The installed binary was then exercised in a scratch simulator instance on the
unit: with a window starting the next minute, trace frames stopped at the
closing boundary, an all-zero frame held outside it (`dev state` 0/12 lit), the
pulse animation resumed on the opening boundary second (12/12 lit), and the
write stream stopped again at the closing boundary. This is digital-frame
evidence from the target binary, not a photometric measurement or a production
night-window trial; production settings were not changed.

This does not replace the remaining acoustic/room, real-device or real-peer Bluetooth gates below.

### Recovery/panel/media/mDNS/mic-DSP stack deployed (2026-11-13)

The eight-change contract above was installed on the unit at `192.168.1.50:5555`
(`Capsula-mini-03104B1D40E15C55`, RK3308, glibc 2.33) through the canonical
`/etc/init.d/S99btctl.sh` service. Daemon `ccde5d81789dfc4c571f60b6f3b7aa21cc803b829621fe6ce3281a6be1a76951`
(25,428,120 bytes, static aarch64, built from current source with
`CGO_ENABLED=0 make daemon-release GOOS=linux GOARCH=arm64`) replaced `e0dd1e1a…3453925a6`
with `capsula daemon install`; the health gate passed and the previous binary is
`capsulad.prev`. The optional assistant was rebuilt (glibc 2.31 Bullseye container,
`CGO_ENABLED=1`) because its speech-floor changes are not part of `make daemon`:
installed binary `0b5cbcb57a18281c8f5261a6bc5cc76ceaca33475f39dbe4c7e29567387a195f`
(2,181,720 bytes, GLIBC_2.17 max) replaces `886e104a…cc80a5`; the manifest is unchanged
and the user's on-device `reply.wav` (1,029,374 bytes) was restored after the reinstall.
The native runtime was rebuilt and replaced as well. `sdk/native/src` is unchanged, but
`dynamic.rs` embeds `descriptors/capsula-framework.protoset` at compile time, so the
installed `libcapsula.so.1.0.0` still carried the old app-facing wire metadata. A
device-side probe over the public C ABI (`SettingsService/GetSettings` as JSON through
the runtime) returned a snapshot without `hostname` and without `startupSoundEnabled`
before the update and with `{"startupSoundEnabled":true,"hostname":"capsula"}` after it.
The current runtime (`c6c06dfa9eb5684c82a2b468a99d7d0b522e5255d5d8b69f400b2d28ed7a1d32`,
4,109,688 bytes, GLIBC_2.30 max, SONAME `libcapsula.so.1` and the same NEEDED set,
metadata `capsula-native.json` byte-identical) was built in a Bullseye container with
`make native GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu` and
installed by atomic rename with the daemon stopped through the boot service; the
`libcapsula.so`/`.so.1` aliases are unchanged and the assistant maps the new file
(`/capsula/lib/libcapsula.so.1.0.0`).

Because stopping a selected provider is now a recovery event, the assistant package
was also swapped with the daemon stopped through the boot service instead of
`app stop`.

Verified after three normal service restarts (daemon install, assistant package swap,
runtime swap): health exit 0 each time, user revision 2259 and every choice preserved
(clock/ring/assistant defaults, `word.personal`, live microphone, `Asia/Shanghai`,
assistant delay `2000-3500`, ring 22:00–07:00 schedule), all three runs `APP_READY`,
the running assistant md5 equal to the built binary, "native A2DP SBC sink endpoint
registered" at each start, and real clock/ring frames written (all-zero ring baseline
outside the schedule window). The user then changed master volume from their own key
presses (revision 2282 at the end), which is their live choice, not a test change, and
demonstrates the panel volume path. The selection recovery path was observed on the
device: an `app stop` of the selected assistant was accepted and the supervisor
relaunched it. The new `hostname` setting seeded `capsula` for the pre-existing store,
was persisted on the next store write, and `avahi-resolve capsula.local` resolves to
192.168.1.50 (the system hostname is unchanged). Idle sample: 72 % CPU idle, daemon
RSS 77 MiB, assistant 8.9 MiB, 17.8 MiB free `/data`. The full backup (including the
pre-update runtime and its metadata) and the exact rollback recipe are in
`_capsula-mini-deploy-backup/20261113T094024Z-eight-changes/` (evidence.txt,
`probe-before.json`/`probe-after.json`).

Not exercised: physical panel gestures, media transport, real-peer Bluetooth
connect/first-connection after BlueZ autopower, assistant speech-floor and microphone
DSP behavior (no acoustic capture), photometric output, and LAN IPv4 reachability of
the Web UI (the listener is `[::]:8443`; the same binding predates this deployment).

### Follow-up corrections (2026-11-13, deployed)

The user reported four live failures after the deployment above. The corrected daemon and optional
assistant were installed together on 2026-11-13 at 16:19 MSK (hashes in the dated entry below).
The deployment changed no production setting, pairing, Mac client or service. The user reports
remain reproduction observations, and the simulator/fixture results below are not device proof for
the physical paths; the limits recorded with each item still stand.

- **Physical media control.** The Bluetooth path uses only public BlueZ interfaces and the
  advertised connected device: `MediaPlayer1` when the source exports it, otherwise the same
  device's public `MediaControl1` (no private API and no second control plane). `MediaControl1`
  publishes no playback status, so a toggle's direction comes from `MediaPlayer1.Status` when
  present, then that device's A2DP transport `active`, then the framework sink's own monotonic
  packet-receipt evidence for the current stream (500 ms freshness, per connection, cleared on
  stop/error/new source). A command never writes that evidence; absent evidence sends Play rather
  than pretending the source is paused. The sink provider is connected-device scoped, so a second
  playing device cannot capture the toggle.
- **Panel gestures.** The user's map remains authoritative — east is volume up, west is volume
  down — and direction is not inverted from a single passive trace. Besides the full
  opposite → centre → direction swipe, a crossing may report the minimum two-contact opposite pair
  without the unmarked centre (W→E right, E→W left). That pair requires the first contact to still
  be down when the second is pressed, i.e. a physical overlap; sequential opposite taps and a
  circle-then-microphone pair remain deliberate key actions. The vertical two-contact pair is
  recognized only in full capture; none and partial modes keep circle and microphone as deliberate
  keys. The three canonical centre crossings are unchanged. A crossing that reports zero or one
  contact is indistinguishable from a key press or no input and is not inferred as a swipe. A
  failed multi-contact sequence suppresses its prefix contacts, so no volume step, mute or summon
  leaks, while the newest contact may still resolve as one single action; a standalone volume tap
  and a held ramp keep working. Gesture windows and the 700 ms hold use the event timestamp from
  the kernel clock (converted to the daemon's origin) rather than read-loop time; a receive-time
  fallback applies only to an invalid event older than 5 s. The captured trace
  (310 ms circle press; one overlapping center→volume_down glide 95.7 ms apart that the deployed
  recognizer scored as `swipe_left`; one requested swipe that produced no kernel contacts at all;
  standalone volume taps) is a passive observation, not a new run, and the user's swipe order is
  uncertain (right may have preceded left), so it does not prove or refute a mapping inversion.
- **Wakeword preprocessing.** Inference, enrollment and training now consume the shared processed
  capture tap (`framework.dsp.v1`); the raw vendor stream is reachable only at the explicit
  `audio.input.microphone.raw` endpoint, and frames with an undeclared tap are rejected. The
  colored-noise estimator learns from aggregate frame energy relative to a fast-down/slow-up floor
  and a very-slow-up average instead of gating on spectral flatness. A general cold-start guard
  keeps a voice or word at 0 dB relative to the learned reference out of the estimate and preserves
  it over the first 20 ms frames (first-vowel gate ≥0.85 of reference, worst 0.896); ordinary room
  re-engagement and a second speech segment after a pause are preserved. On the one authorized 10 s
  background recording — mostly music played through a Mac's own or external speakers, not the
  Capsula Bluetooth path, with no local playback reference to cancel it — cleanup measured 2.9 dB
  overall and 2.7 dB in 80–300 Hz. That is consumed background evidence, not fan/traffic
  validation, music immunity, a new-location profile, or a zero-FAR result. Synthetic pink,
  brown/rumble and amplitude-modulated room regressions measure 4.23, 8.33 and 3.42 dB. The
  installed raw-trained model remains installed, selected and unmodified; it loads on the
  processed tap with an explicit unverified-threshold warning. On the settled current profile, its
  warm classifier replay — 2.32 s of its own same-session ambient audio, the word, and a 1 s tail —
  fires 6/6 raw and 6/6 processed. This is legacy-model usability evidence only, not calibration
  or an accuracy benchmark, and it is distinct from the assistant endpointer result below.
  Recordings carry a processing-provenance sidecar; legacy raw takes are converted in memory under
  the declared policy, never silently relabeled. `CNEG` embeddings remain auxiliary off-policy
  examples: only local processed held-out takes decide threshold calibration and the pass/fail
  certificate, while a false activation on the broad `CNEG` corpus remains a hard acceptance veto
  and is not a processed-quality certificate. No forced retrain, weight rewrite or selection
  rewrite occurs.
- **Assistant endpointer.** The optional assistant still measures onset and end in sample time
  against a learned floor, but an utterance now ends after 700 ms in which the 300–3400 Hz band
  level has fallen clearly below the recent voiced level — not after any 700 ms of unmodulated
  audio. A sustained voiced vowel keeps its level and is preserved. A reply requires either 140 ms
  of modulated speech-band time or 400 ms of steady voiced level well above the floor with no dip
  below the tail threshold, so a room that starts mid-turn is not confirmed by one steady hold.
  The canonical raw endpointer check uses external music plus a full-level word at actual input
  levels — one DSP pass, no normalization, calibration on the actual level and evaluation on the
  warm and live ranges: 1 reliable, 1 marginal and 4 buried/rejected. That is an accepted
  music/quiet-speech limitation and a consumed small regression, not an accuracy benchmark. Loud
  external music or lyrics can open or satisfy a turn, quiet speech can be missed, and there is no
  promised automatic music cancellation, AEC or speaker identification. The current activation
  mode is preserved; no hold-to-talk change is made. The old 10 s Mac-speaker music recording is
  historical consumed evidence and not proof of non-speech-room behaviour or of this location.
  Remaining limits are explicit: a new steady loud noise at/above voice level that begins mid-turn
  is bounded by the existing 10 s cap and cancelled; a steady hiss/tone held above 2.5× the floor
  for 400 ms can be answered; and a sustained quiet vowel below that multiple is not confirmed.
  These are limitations of a level endpointer, not a claim that all noise or music is rejected.
  The earlier lost noisy-room assertion is not attributed to a particular cause; paced-negative,
  typed-PCM and counter-before-active/after-close checks replace the vacuous log it relied on, and
  a composed short-word regression that failed the old budget passes the new one.
- **mDNS idempotence.** Applying the saved label first reads `GetState`/`GetHostName`; when the
  responder is RUNNING and already owns that label, the daemon confirms `GetHostNameFqdn` and
  makes **no** `SetHostName` write. After a write it waits a bounded interval for the responder to
  report the requested label; a redundant-write reply (Avahi's invalid-operation error) or a
  concurrent writer winning the race is accepted only from that authoritative read, otherwise the
  original error and observed FQDN remain visible. Write-time collisions, invalid names and a
  missing responder remain failures. A successful write whose FQDN cannot be read back is reported
  as applied-but-unconfirmed, never as a fabricated advertisement or a bogus warning. A coalesced
  worker drains state changes during an apply and replays the latest RUNNING edge, so an overflow
  cannot drop the reapply; `Close` cancels without publishing a warning. `GET /settings` only
  renders the stored status and never applies a name or saves configuration.
  The read-only device snapshot showed Avahi `RUNNING`, `GetHostName=capsula` and
  `GetHostNameFqdn=capsula.local`; the portable fake-responder suite, a barrier-driven private-bus
  Linux adapter fixture and a real Web UI/auth/TLS/settings browser run against a successful
  responder fixture cover the ordering. In that browser run, startup + two GETs + a same-name save
  made **zero** `SetHostName`
  calls and rendered a clean “Advertised as capsula.local” without warnings; changing to `kitchen`
  made one setter call; a genuine collision remained visible with no advertised claim and the GET
  made no writes; restoring `capsula` recovered. Totals were 15 `GetHostName`/`GetHostNameFqdn`,
  four settings updates from POSTs only, and three `SetHostName` calls (`kitchen`, the collision,
  and the restore). This is fixture-backed browser evidence. The fixed build is deployed: the unit
  reports `GetHostNameFqdn=capsula.local`, `avahi-resolve capsula.local` resolves to it, and both
  starts after the 2026-11-13 deployment logged the advertisement with no redundant-operation
  warning. A real write-time collision and its Web UI rendering remain gates.

Final host verification (darwin/arm64) used a HEAD-built Mach-O runtime with its metadata and exact
embedded descriptor match in a unique temporary directory, because the pre-existing `build/lib`
candidate did not match; `build/lib` was not overwritten. With `-race` and
`-ldflags '-linkmode=external'`, the audio drop/frame/lifecycle subset passed (2.238 s), the entire
nested-assistant module passed (6.222 s), and the actual daemon passed all 14 `TestDumbAssistant`
scenarios (128.684 s), including noise baseline zero before capture, active zero and post-close zero
without playback, typed-level assertions, released/Ready ownership and splice cleanup. The final
review of those baseline/active-zero/stat comments is clear. The BlueZ `MediaControl1` private-bus
wire fixture, barrier-driven Avahi adapter fixture and fixture-backed Web UI browser run are the
separate evidence noted above. The follow-up stack was deployed on 2026-11-13 with a full host
backup, the canonical health gate and a normal restart; the dated deployment entry records the
installed hashes, the preserved choices and the remaining limits. Prior simulator, browser and
fixture results are not device proof.

#### Follow-up stack deployed (2026-11-13 16:19 MSK)

The unit was identified from the device itself (`Capsula-mini-03104B1D40E15C55`, RK3308, glibc
2.33, `boot_id` `b0d88563…`); the connected adb endpoint was `192.168.1.84:5555`. The requested
`192.168.1.84:555` was not an adb device, so the port was not rewritten silently: the only attached
device was verified by hostname. The unit had power-cycled shortly before the work; the canonical
hook recovered the daemon before any deployment step.

Installed from current source, with the full previous stack pulled to
`_capsula-mini-deploy-backup/20261113T131321Z-followup-input-wakeword-vad/` and verified
byte-for-byte against the device (settings, app settings, assistant package including the user's
`reply.wav`, wakeword model, runtime and metadata, boot hook, both daemon binaries):

- daemon `ff3646bc29acaf2515731ef94d21a4ef9217981bfd9029e636a9b1d2bebc8687`, 25,493,656 bytes,
  static aarch64, stripped (`CGO_ENABLED=0 make daemon-release GOOS=linux GOARCH=arm64`); the
  previous `ccde5d81…` is `capsulad.prev`;
- assistant `141da8765652e6c8f4e528074e407aeece91483cedefd7bb23fb145220613ada`, 2,190,200 bytes,
  built in Bullseye (`CGO_ENABLED=1`, GLIBC_2.17 max), manifest unchanged;
- the native runtime was not rebuilt: the deployed `libcapsula.so.1.0.0` (`c6c06dfa…`, ABI 1.0,
  35 capabilities, metadata `6294d52a…`) contains the current repo protoset (`2a77b190…`, 37,234
  bytes) byte-for-byte — a content check of the target binary, not an mtime or source assumption.

Deployment used `capsula daemon install` with the normal health gate (`boot.log` 16:18:25 →
16:18:26 healthy) and, because stopping a selected provider is a recovery event, an offline
package swap with the daemon stopped through `/etc/init.d/S99btctl.sh`: `app remove` →
`app install` → the user's `reply.wav` restored (sha256 `b56d0cc…`) → service start (16:18:58 →
16:19:00 healthy). `/data` was at 18.3 MiB free; the old `.prev` was removed only after its host
copy was verified, leaving 18.1 MiB free after installation.

Verified after the restart: health exit 0; `capsula.clock` and `capsula.ring` (builtin) plus
`org.capsula.dumbassistant` all `APP_READY`; the running assistant md5 `32e4024b…` equals the
built binary and maps the deployed runtime. Settings revision 2358 was unchanged — clock/ring/
assistant selections, `word.personal` enabled, 5 % volume, microphone live, `Asia/Shanghai`,
startup sound on, `bluetooth_pairable=false`, hostname `capsula`, and `touch_hold_mode=1`
(SUMMON, the user's preserved activation mode); assistant delay `2000-3500`, ring window
22:00–07:00. The wakeword model (`5f92202f…`, threshold 0.59) and the custom reply asset were
byte-identical before and after. Startup logged the mDNS advertisement with zero warnings,
`GetHostNameFqdn` returned `capsula.local`, `avahi-resolve capsula.local` resolved to the unit, and
the native A2DP SBC sink registered at both starts. A device-side probe over the deployed runtime's
public ABI returned `hostname` and `startupSoundEnabled` from the new daemon, confirming descriptor
compatibility.

Rollback: restore the backed-up daemon/packages/settings/model/runtime from the directory above;
daemon-only rollback is `capsula daemon rollback` (restores `ccde5d81…`).

Limits: no microphone capture or recording was made, so the shared-DSP wakeword path and the VAD
endpointer are not acoustically verified here; the old private external-music clip is historical
and does not qualify the current room, and quiet speech under loud external music remains the
documented user-accepted limitation. Physical gesture/media-recency behavior, real-peer Bluetooth
commands, Avahi write-time collisions, authenticated Web UI and a power-cycle were not re-exercised
after this deployment.

## RPC and microphone privacy boundary (2026-11-13)

The single Unix-socket gRPC API now enforces a least-privilege boundary for supervised apps and
authenticated automation. Administrator-only: device settings; device-wide diagnostics and log
history; forced capture release; microphone mute/master volume; wakeword record/list/delete/train;
Bluetooth discoverability/pairability/unpair; application launch/stop. Per-app scoping: app
configuration reads/writes, actions and run listings, plus log tails and capture release (an app
sees only its own records; framework and foreign log records are excluded). Bluetooth media state
redacts the connected peer address/name for app callers while keeping the transport/now-playing
fields a media provider needs. The built-in clock and ring apps publish their contracts through
`AppInterfaceService` and serve ordinary supervised-app callers through the app bus; a supervised
app still cannot claim a virtual system-app run, spoof another app's routed-call caller ID, or use
the reserved framework identity. Administrator operations remain gated as listed above. Assets are
listed only from contained, regular, non-symlink files. A touch assistant activation carries
`hold_to_talk`, so the optional assistant applies the per-turn release rule without calling the
administrator-only device-settings read.

Microphone capture requires both halves of an indication: a selected visible non-blank
ring/segment presentation at non-zero effective brightness and a microphone-live cue that the mixer
consumed and drained to the output sink. The cue is the exclusive mixer source while it runs, so an
application cannot mask it with concurrent playback; missing output, zero master volume, global
mute, blank/unselected indication or an undeliverable cue deny the capture. Both halves are
re-evaluated for every chunk, so delivery stops as soon as the visible indication or volume is
lost; withheld chunks are counted as dropped and the watchdog grace only bounds how long the
capture remains alive before `WATCHDOG_EXPIRED`. Pre-roll is attached only to a framework-issued,
one-shot activation for that same app. Enrollment takes always play the recording start cue and
fail when the ring indication cannot be selected/rendered.

Regressions: internal/audio indication denial and continuous withholding, internal/assets
traversal/absolute/symlink rejection, daemon administrator/app-scope gates, and virtual-run token
identity. The earlier private clock app-bus denial is superseded by the public clock/ring contract
tests: ordinary supervised callers can discover and use those services, while run-identity and
administrator boundaries still apply. `go test -race` passed for
internal/audio and internal/assets. Limitation: no device/microphone/acoustic run was authorized;
loudness and emitted light are not measured, and root/physical or `-sandbox none` access is outside
the cooperative model.

## Implemented and exercised

| Area | State | Evidence |
|---|---|---|
| All 15 framework gRPC services over one UDS | implemented | simulator run + tests |
| App supervision, manifests, restart/backoff, log capture | implemented: a lost framework connection is a failure, not a deliberate stop. A selected default provider is recovered even when `restart_on_failure=false`: immediate first restart, then 1/2/4/8 s backoff. The backoff resets only after 30 s of continuous READY, so a ready-then-crash loop cannot spin at zero delay. Selected-origin and forced recovery relaunches re-check selection regardless of `restart_on_failure`; deselection, `none`, uninstall and shutdown keep it stopped. | test apps run, crash test, `TestDefaultAppRestartsAfterLostConnection`, `TestReadyThenCrashKeepsBackoffAndStableRunResets`, `TestSelectedManifestRestartRechecksSelection`, `TestStopForRestartRechecksSelection`, `TestDeselectDuringBlockedTerminationSuppressesManifestRestart`, `TestBuiltinStopDoesNotResurrectAfterDeselectionDuringTeardown`, `TestStopForRestartRelaunchesProvider`, full `go test -race ./internal/supervisor`; focused daemon opt-out/deselection tests |
| Singleton launch invocations (queued/bounded/expiring) | implemented | delay-injection test |
| Reverse-RPC bus (`Connect`, routed unary/streaming `Call`) | implemented | routed-call test |
| Presentations (first-content readiness, in-place content) | implemented | routing tests + built-in clock system app |
| Built-in clock and ring system apps (`capsula.clock`, `capsula.ring`) | implemented as compiled-in applications with normal registry identity, `ApplicationService` list/launch/stop/run events, app settings, routing ownership and app-bus endpoints; only packaging is special (no directory, process, install or uninstall). Their features are public standard contracts (`capsula.contracts.clock.v1.ClockService`, `capsula.contracts.ring.v1.RingService`) discoverable through `AppInterfaceService` and callable by ordinary supervised apps over the app bus; the daemon's Web UI uses the same path. Clock exposes state/watch, stopwatch start/pause/resume/reset, timers start/cancel/list, alarms list/add/delete/enable-disable and alert dismissal. Ring exposes state/watch and every declared setting through get/update/watch with CAS. Clock settings: `blink`, `timezone`, `display_mode`, `timer_seconds`, `stopwatch_reset_gesture=hold|double_tap`, `alarms` (five-field dcron entries with optional `@IANA-zone`, `!once`, `!disabled`; `*/1`/full-domain fields count as unrestricted for DOM/DOW OR semantics) and `alarm_labels` (JSON map stable ID→display label, default `{}`; schedules stay solely in `alarms`). `Alarm.label`/`AddAlarm.label` are public. Legacy `alarm_label` migrates into `alarm_labels`; flat `<data-dir>/app-settings/capsula.clock.json`/`capsula.ring.json` files convert once atomically with the canonical directory form winning and the flat source retained. Recurring alarms dedupe by local wall-clock minute, so DST fall-back cannot ring twice. Explicit disable (`SetAlarmEnabled`, editor, or `!disabled` setting write) dismisses an active alert; a one-shot self-disarm after firing keeps its alert until dismissed. Ring settings: `effect=breathe|spinner|rainbow|pulse|comet|sparkle|static` (`rainbow` rotates/interpolates actual palette samples at 0, 1/3 and 2/3, no invented hues), `palette` as a canonical gradient or solid colour, `speed`, `brightness`, and an optional local-wall-clock active window `schedule_enabled` + `schedule_start`/`schedule_end` (`HH:MM`, device timezone, may cross midnight, equal bounds mean all day): outside the window the app presents an explicit blank baseline frame, so the ring is dark but still yields to system/app captures and the effect returns at the boundary, with one timer armed per boundary rather than per-frame polling. A running stopwatch holds an orange whole-ring pulse; a tap pauses/resumes it and the configured hold or double tap resets it without summoning the assistant. Both are seeded as defaults on a fresh install; an empty selection means none. Legacy `org.capsula.clock` / `org.example.ring-controller` settings and default IDs migrate, retired sample bundles stay quarantined, and legacy named palettes migrate to canonical gradients. Alarm migration preserves prior time, recurrence, explicit timezone, one-shot behavior and enabled/disabled state; entries may carry `@IANA-zone`, `!once` and `!disabled` suffixes, and `alarm_id` is `expr [@zone] [!once]`. External captures preempt and release normally. | simulator tests for list/get/default round-trip, stop/launch/run events, capture preemption and public routed clock/ring calls; the ring-schedule build (commit `089f589`) was installed on the unit on 2026-11-13 and its schedule transitions were exercised there in a scratch simulator instance (see the deployment note above). The public contracts, dcron alarms and gradient palettes are local source changes; the final generated outputs were rebuilt/observed by the verifier, and nothing has been deployed to the device |
| Captures (last eligible wins, baseline fallback, out-of-order release) | implemented | routing tests |
| Touch partial/full, panel keys and inferred gestures, cancellation | implemented: five discrete keys mapped N/E/S/W/C; partial capture owns circle/centre plus swipes/circles, full capture owns every key/gesture with all system effects suppressed; full swipes accept opposite→centre→direction or the two-contact opposite-edge pair only while the first contact is still down (sequential opposite taps and circle→mic stay deliberate keys); the vertical two-contact pair is full-capture only; a 0/1-contact glide is not inferred as a gesture; deferred recognition (250 ms/1.5 s) uses the kernel event timestamp converted to the daemon clock; a failed prefix suppresses its framework effects; ownership changes cancel a pending sequence | `internal/input`, panel gesture and backend key-map tests; injected-clock delayed-read oracle; full-panel capability advertised with `coordinates=false` on the Mini |
| Audio mixing, fan-out, ducking, pre-roll, mic watchdog | implemented | simulator test |
| Application capture DSP and provenance | implemented for S16LE mono 16 kHz: 80 Hz high-pass, energy-gated adaptive spectral suppression ≤12 dB, unity gain, −1 dBFS limiter, 256/128 STFT, fixed latency and chunk-invariance, reset on mute/reopen/gap; a cold-start guard keeps voice/word at 0 dB relative to the reference (first-vowel gate ≥0.85, worst 0.896) and preserves room re-engagement and a second speech segment; while the chain is active `audio.input.microphone` advertises `framework.dsp.v1`, otherwise endpoint metadata reports an empty tap; wakeword inference and enrollment consume the same processed tap, and the raw vendor stream is available only at the explicit `.raw` endpoint | `internal/audio` DSP and endpoint tests; format gate is exactly 16 kHz mono; 2.9 dB cleanup was measured on the consumed mostly-Mac-music background recording, not fan/traffic or new-location proof |
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
| Talk button / circle key, centre, volume keys | implemented: hold 700 ms summons (manual origins need no trained model); release ends the turn in hold-to-talk; a short circle tap toggles media (playing standard provider → paused → connected Bluetooth via public `BluetoothMediaService` → other standard provider; a Bluetooth source without `MediaPlayer1` is driven through the same device's public `MediaControl1`, with toggle direction from `MediaPlayer1.Status`, the transport `active` state, or sink packet-receipt freshness); an alert or assistant turn is dismissed first and its release consumed; the neutral centre dismisses and drives stopwatch gestures but never summons. A running stopwatch owns tap/hold/double-tap. 250 ms panel deferral adds latency to single-key actions but prevents gesture leakage. Volume taps ±5 %, held ramps accelerate 200→60 ms and stop at bounds, with a generated tick and a 1 s percentage overlay | `internal/input` and panel/mapping tests, `TestSystemMediaUsesStandardProvider`, `TestSystemMediaFallsBackToConnectedBluetooth`; historical `buttons5.log` hardware run exercised hold/release/volume before tap-media existed |
| Microphone key toggle + muted indication | implemented, verified on hardware | one mute per tap (`TestDiscreteKeysActOnPressOnly`), red run pixels 5–7 with fade-in/out |
| Web UI (Vue SPA + binary protobuf WebSocket) | implemented: `web/` builds an embedded Vue 3 SPA; `/ws` carries `capsula.web.v1` frames with revisioned topic snapshots/events, resync-on-overflow, session-only auth, exact HTTPS-origin/CSRF checks, a 64 KiB inbound frame bound and a 15-operation allowlist including explicit Wi-Fi commands (no generic AppBus/capture/audio/asset path). Overview, Settings, Application/Clock/Ring, Wakeword, Wi-Fi, Diagnostics and Access functionality retained; live selects/sliders/checkboxes/microphone gate apply immediately with per-group coalescing, one in-flight request and newest-revision CAS, while text/number drafts stay local until Save and survive external pushes. Application pages can mount a manifest-declared custom settings UI in an opaque-origin `allow-scripts` iframe through the authenticated `capsula.settings-ui/1` bridge; only declared own-app setting patches reach the normal CAS path. Wakeword and Access have true cross-session subscriptions fed by in-process store invalidations (no polling), and a topic-less resync refreshes every subscription after an overflow. Static API keys stay limited to `/api/status` and the certificate; logout/password-change close live sockets, and the daemon watches the credential file so an out-of-process `capsula auth` password change revokes sessions immediately and key edits invalidate Access without polling. Clock/ring use their public standard contracts through the same app-bus path as external apps. | `go test -race ./internal/webui` (auth/API-key boundary, cross-origin/unauth upgrade, frame limits, logout/revocation close, CAS conflict, training cancel, allowlist-vs-descriptor, two real socket sessions receiving an access change, hub fanout, credential-file convergence, global resync); dist regenerated from current `web/src` with the pinned Vite 4.5.5/Vue 3.5.13 toolchain and re-embedded; simulator browser run through a local TLS-forwarding verification proxy: all pages rendered, clock stopwatch reached RUNNING over the real app bus, no console errors; two live WebKit sessions on the proxy-configured console received a newly created API key on both sockets without a refresh. Chromium launch and trusted self-signed TLS remain host limits; direct self-signed browser trust remains manual |
| Custom application settings UIs | implemented: `[settings_ui] html` serves one package-contained document (≤512 KiB) through an authenticated HTTP route, but the document is untrusted and opaque-origin. The SPA loads that URL directly as the iframe `src` with `sandbox="allow-scripts"` only, so the child is opaque-origin; the host page allows `frame-src 'self'`, normal routes keep `X-Frame-Options: DENY`, and only the settings-document route omits it. CSP denies network/forms/frames/external resources. Self-navigation is not claimed to be CSP-blocked (`navigate-to` is unsupported); any iframe `load` after the initial ready is a violation: the host disposes the frame, points it at `about:blank`, invalidates its token and never re-sends init/schema/values. No ready within 10 s is a visible error with a disposed frame; retry creates a fresh frame/token. The injected local UI kit plus `capsula.settings-ui/1` bridge exposes only `CapsulaUI.patch` for declared own-app setting values. The parent validates exact frame source, opaque origin, a per-frame random 32-hex channel, 64 KiB message/64-change/8 KiB-value/32 KiB-total limits, and forwards through normal CAS app settings; unknown/undeclared keys, stale frames and foreign apps have no host effect. Built-in clock/ring use the identical route. | `TestSettingsUIHandlerServesSandboxedDocument`, `TestSettingsUIHandlerRejectsUnknownAndEscapingAssets`, `TestSettingsUIHandlerServesBuiltin`, `internal/appui` document/kit tests; the SPA bridge and iframe behavior are frontend code, not a live browser security certification |
| Wi-Fi manager, AP fallback and captive portal | implemented for a Linux target: first Linux use imports existing stock `/etc/wpa_supplicant.conf` networks once (derived PSKs only, `.stock-imported`, active network preferred, private duplicate wins); if the stock file cannot be read and the private store is empty, the daemon leaves the stock association untouched and reports an error. Background association over saved networks is priority-ordered and bounded to 25 s total (7 s per network); no saved network or any failure raises a WPA2-PSK AP with SSID = live BlueZ adapter alias (saved hostname only if Bluetooth has no name), password `capsulad`, 2.4 GHz channel 1, `192.168.4.1/24`, DHCP `192.168.4.10–100`, DNS hijack and a portal on port 80. Stock `wpa_supplicant` AP mode + a separate `dnsmasq` are used (no hostapd); a best-effort `CAPSULA_AP_PORTAL` firewall chain permits only DHCP/DNS/portal on the AP interface. The portal exposes status/saved-list, explicit `POST /scan` and `POST /connect` only, rejects non-AP-subnet/Host/Origin requests, requires an HMAC token bound to the client address and valid for 15 minutes (reusable, not single-use), never echoes a passphrase, and shows no admin/settings/WS route. The authenticated Web UI shows state/current SSID/IP, known networks with Forget (Web-UI-only), and scan results only after explicit Scan; snapshots/events never scan. `scan_supported` is capability-driven: station mode when `wpa_cli` exists; AP mode disables it after a supplicant refusal with no `iw` fallback, while manual SSID entry works. Supplicant control-socket CONNECTED/DISCONNECTED/roam events reconcile state/revision/KnownNetworks/scan-Connected/Web UI without polling, and long-lived children use the manager lifetime rather than the request context. Successful connect stops AP supplicant/dnsmasq/portal, flushes the AP address and starts station mode; a failed station connect restores the previous/saved association or raises the AP boundedly with the original error; Close stops children and removes the firewall chain. Passphrases are never stored or returned; only the derived PSK is kept in `<data-dir>/wifi/networks.json` (0700/0600, atomic). Simulator/non-Linux report unavailable. | `internal/wifi` tests: PSK vector, parser/store permissions/no plaintext, stock import/adoption, bounded startup/fallback, status/subscribe never scans, explicit scan, AP teardown/recovery, portal Host/Origin/CSRF/client-subnet/allowlist/connect-flush. Physical RF association, stock wpa_supplicant AP-mode scanning, DHCP/firewall behavior and captive detection remain device gates |
| Bluetooth adapter policy (BlueZ D-Bus + Linux management) | implemented: adapter power, discoverable and pairing over D-Bus; **Incoming connections** (`bluetooth_incoming_connections`, default on) via the public Linux management socket `Set Connectable`; **Discoverable** (`bluetooth_discoverable`, default off); paired-device list and unpair. Incoming off denies new inbound BR/EDR links, including from paired devices, through `Set Connectable` plus clearing the kernel action list (`Remove Device`) so the BlueZ known-device allow-list cannot bypass it; the adapter and BLE radio stay powered and existing links are not torn down. Discoverable implies incoming and permits new find/connect/pair; incoming off clears discoverable. The obsolete `bluetooth_pairable` value migrates true → both on, false/absent → incoming on/discoverable off. The native A2DP Sink endpoint is registered through public `Media1` APIs and verified generation-scoped before any connectable/discoverable state or API request; a failed/lost registration is never ready, so both enabling actions are refused with the real reason and `BluetoothStatus.Detail` reports "audio profile is not advertised". `BluetoothStatus` carries `name` (adapter alias) for the Wi-Fi AP SSID. PulseAudio discovery is the fallback owner only. The framework powers the adapter only once the sink is ready and repairs registration after BlueZ restarts/power cycles | live unit (2026-11-11): `hciconfig` UP RUNNING, `PSCAN ISCAN` after the former Web UI toggle, paired MacBook listed; migration, two-toggle transitions, public-mgmt-call and readiness-ordering source tests were added but not yet run in this docs slice. Physical `Set Connectable`/codec action-list/LE side effects and real-peer first-connection recovery remain hardware gates |
| Bluetooth AVRCP backend | implemented: play/pause/next/previous, absolute volume to the source and metadata when the source exposes it, scoped to the advertised device through public interfaces (`MediaPlayer1`, else that device's `MediaControl1`). The bundled sample media provider was removed, so a Web UI player still requires an operator-installed provider; physical circle/swipe controls do not, and use the public `BluetoothMediaService` when no playing/paused standard provider exists | live MacBook (2026-11-11): Play/Next accepted and playback state changed; 50 % master volume set transport `Volume=64` of 127; no bundled provider ships; simulator fallback test drives the real input manager; private-bus BlueZ fixture on Bullseye verified the `MediaControl1` object/method call on the wire (deployed in the 2026-11-13 16:19 MSK daemon; not re-exercised with a real peer after deployment) |
| Bluetooth notifications, routing and volume | implemented: connect plays three completed blue fade-in/out ring pulses and shows lowercase `bt` on the two rightmost segment cells for 2 s; disconnect plays exactly one pulse and no glyph, through ordinary `SystemAppID` presentations/captures. Native A2DP sink readiness is generation-scoped and verified before connecting, accepting inbound links or enabling discoverability; a lost registration is repaired first. If native registration fails, the PulseAudio fallback loads `module-bluetooth-discover` and routes the source with `module-loopback` at 250 ms, reloading `module-bluez5-device` with `avrcp_absolute_volume=0`. PA endpoints are never removed while a source is actively routed through them. Transport/AVRCP volume is event-driven through the shared `applyMasterVolume` path, with no poll, echo window or debounce; a headless NoInputNoOutput BlueZ agent permits first-time bonding only while incoming connections and discoverability are enabled. | simulator: pulse/glyph timing, ownership/preemption, lifecycle and registration ordering; hardware-unverified: real Mac pairing latency, A2DP audio, AVRCP absolute-volume behavior, physical light; earlier live logs covered routes and endpoints only |
| Master volume and system cues | one canonical control path (`applyMasterVolume`) is shared by volume keys, `AudioService.SetMasterVolume` and AVRCP changes, and `applyMicrophoneMute` is shared by the microphone key and `AudioService.SetMicrophoneMuted`; the settings watcher remains the single writer for the muted indicator. The software mixer applies the master gain (historical live measurement: 0 % -> silence, 1 % -> peak 58, 5 % -> 294, 10 % -> 589, 20 % -> 1179). Bluetooth keeps the transport and framework master equal, but does not trust AVRCP as the only attenuation: when PulseAudio is the fallback owner, the loopback carries a local safety gain of master^1.5 and the local bluez source stays at a fixed 90 % trim, re-applied on source/route events rather than by a poll. PA is decoupled with `avrcp_absolute_volume=0` on each fallback source connect. The volume tick peaks at 0.22 of full scale. | live capture and module/decoupling checks were run before the event-driven rewrite; simulator tests now cover ownership, timing and the shared volume path. The new AVRCP transport behavior, real Mac pairing/audio and the master^1.5 audible result remain hardware-unverified | UI cues are the embedded `internal/sounds` ubuntu-sounds set (CC BY-SA, `COPYING` preserved): `generic.wav` for a volume step, `warning.wav`/`info.wav` for mute/unmute, and `startup.wav` once on installation then only with `startup_sound_enabled` (off by default); all stream through the mixer at a -8 dB UI trim.
| Display/ring brightness: constant or ambient-light | implemented with the stock curve `trunc(log(0.75+0.05*lux)*19.9375)` clamped to [5,95] %, 5-point deadband and animated fade, plus an auto-only 5 % floor: 0 lux, malformed/NaN/±Inf/negative or stale samples never blank an enabled automatic display, and a manual 0 remains explicit off. 5 % survives backend quantization (13/255). | `TestAmbientBrightnessMatchesStockCurve` (floor, invalid samples, monotonicity), `TestEncodeMultiCtrlFrame` floor case; physical luminosity in darkness remains hardware-unverified |
| Device timezone (Web UI) with built-in clock override | implemented, validated end to end | Web UI round-trip restarts boot apps; the clock re-reads `TZ` (11:59 MSK ↔ 16:59 Asia/Shanghai) and `capsula.clock timezone` overrides the device zone |
| mDNS host name (Web UI/Settings) | implemented: single-label `Hostname` default `capsula`, CAS-validated, persisted; applied through public Avahi calls — `GetState`/`GetHostName` first, `SetHostName` only when the label is not active, then `GetHostNameFqdn`; a redundant reply is accepted only from an authoritative read after the responder is RUNNING, write-time collisions/invalid names/missing responder fail, a successful write whose FQDN cannot be read is applied-but-unconfirmed, and the coalesced worker replays the latest RUNNING edge without a warning on Close. Re-applied at startup and after responder restart (latest generation wins); `GET /settings` only renders the stored status and never applies or saves. Simulator/unsupported platforms cannot rename the host and report it unavailable. | portable fake-Avahi ordering tests, barrier-driven private-bus Linux adapter fixture, `GET /settings` purity test; read-only device snapshot observed Avahi `RUNNING`, `capsula`, `capsula.local`; a real Web UI/auth/TLS/settings browser run against a successful responder fixture exercised zero setter calls for startup/GET/same-name save, one for a change, a genuine collision and recovery; real-device `avahi-resolve` after save/restart remains the gate |
| Rockchip DT key map (microphone mute, volume up/down, play/pause, neutral centre) | implemented: `KEY_VOICECOMMAND` (0x246) and `BTN_TRIGGER_HAPPY1` (0x2c0) both name the unmarked neutral centre, not an assistant key; both DTBs and labels are parsed and merged over the default map | device-tree parser + key-map tests |
| Rockchip ambient-light and thermal sensors | implemented | IIO/thermal sysfs tests |
| Web UI pages (all settings, apps, media, diagnostics) | implemented; server-owned app records distinguish built-in system apps from installed packages, system-app pages expose lifecycle/packaging metadata and omit package provenance, default selectors include every provider with an explicit none choice, and clock/ring pages discover their app-bus services through AppInterfaceService and call them with binary protobuf (typed requests and decoded responses at the app-bus boundary; protobuf JSON remains only the browser/reflection encoding). Invalid app settings show the daemon validator message and keep the prior value. Device settings include the validated mDNS host name with advertised/collision status; a selected provider's stop control is labelled **Restart** with deselect guidance, and diagnostics use the same wording. | login/API-key + CSRF + settings/app-control tests; Chrome-headless browser session against a real simulator daemon observed the default/valid/invalid host names, save and restart persistence, no false advertisement claim, and Restart labels on Clock, Ring and Diagnostics; actual Avahi `.local` resolution remains a device gate |
| Shared native SDK (`libcapsula`) | implemented as a pure Rust cdylib with frozen C ABI v1: bounded `capsula_get_api`/table copy, explicit statuses and buffer ownership, reflection JSON/protobuf for all framework unary/client/server/bidi calls, managed AppBus, routed and concurrent calls, cancellation/deadlines, raw-PCM audio, and runtime metadata with capability negotiation. The per-call response backlog is bounded at 512 events, deliberately above the transport flow-control window: a brief consumer stall (or an operator-paused app) creates backpressure instead of failing a live audio capture. Go/Rust bindings load it dynamically; C/C++ use the same header. Release builds strip debuginfo, debug builds do not. Integrated build, usage, lifetime and compatibility details are in the README’s Native SDK section | ABI/table sizing and lifetime tests; C/C++/Go/Rust consumer checks; physical device 2026-11-12: release cdylib + metadata installed in `/data/capsdemo/lib`, mapped at `/capsula/lib` under the mount namespace with `APP_READY` |
| Media-player standard contract | framework plumbing retained: unary commands and server-streaming `WatchPlayers` over the app bus; no sample provider is built or shipped. Physical circle tap and left/right swipes control current media with no provider app: playing provider → paused provider → connected Bluetooth source via public `BluetoothMediaService` → other standard provider. | app-bus routed streaming tests; `TestSystemMediaUsesStandardProvider`, `TestSystemMediaFallsBackToConnectedBluetooth` |
| Optional dumb assistant (`org.capsula.dumbassistant`) | separate optional nested Go project built only by `make dumbassistant`: consumes the framework's processed application microphone tap, tracks input level on the whole teal ring, learns a room-noise floor in sample time with a cold-start guard (initial speech is not learned as background; a reply needs 140 ms of modulated speech-band time or 400 ms of steady voiced level well above the floor), ends turns on hold-to-talk release or after 700 ms in which the 300–3400 Hz band level has fallen clearly below the recent voiced level, so a sustained voiced vowel is preserved, cancels cleanly on mute/no-speech/capture end, and declares a `dismiss` action so the circle key aborts listening, delay and playback and drops queued activations, then fades to a blue chase for the delay and reply. `delay` accepts empty/0, a constant, or an ascending inclusive range 0–60000 ms. `reply_asset` is relative to the shared read-only app asset store (`/assets`; apps receive `CAPSULA_ASSETS`, empty uses the packaged fallback) and must be registered in the shared asset manifest; absolute, traversal, symlink and unregistered values are rejected synchronously by a daemon validator (`ApplicationConfigurationService.Update` -> InvalidArgument, no write) and again by the app. | simulator tests with injected PCM, ring snapshots, VAD/sample-time and Update rejection; canonical raw external-music + full-level-word run at actual levels yields 1 reliable, 1 marginal, 4 buried/rejected (accepted music/quiet-speech limit, not an accuracy benchmark); the old 10 s Mac-music recording is historical, not room-noise or new-location proof; real acoustic thresholds and LED gamma remain hardware gates |
| Framework boot ownership on the stock image | one self-contained removable hook, `/etc/init.d/S99btctl.sh` (repo `deploy/rockchip-stock/S99btctl.sh`), performs all capsulad-specific preparation and recovery: boot log cap, data/run dirs, Bluetooth UART IRQ affinity, daemon start/stop, health gate, vendor-script preservation/retirement and vendor fallback. `disable` stops the daemon, restores the vendor scripts, starts the vendor chain and moves the hook aside; `enable` restores it. It replaces the former S60capsulad + wrapper pair. Name/order are forced by rcS expanding `/etc/init.d/S??*` before the `/data` overlay pivot. | deployed: the single hook is the installed service on the unit (hash `75c3716f…`, used by every install since 2026-11-13; see the dated deployment entries below). The previous S60capsulad deployment was verified on 2026-11-12 (health 0, watchdog 21 s); a power-cycle and vendor-fallback exercise of the current single-hook build remains a gate |
| In-place daemon upgrade with rollback | implemented and exercised: executable check, staged rename beside the live binary, restart through the boot service, framework-API health gate, previous binary preserved; `capsula daemon rollback` is the inverse, but it restores the binary only: a full return to the old app-based clock/ring/assistant also needs the old bundles and manifests from the host backup, and the old daemon's strict manifest loader does not understand `[native_sdk]` | host simulator install+rollback drill, live install on the unit (21,954,712 → 21,889,176 bytes), 2026-11-12 live install to 22,610,180 bytes (sha256 `86d131ae…fe604`) with the 22,479,000-byte `.prev` retained, health after restart; 2026-11-12T12:26Z install of the working-tree build (23,003,288 bytes, sha256 `30c5c1e4…48363`) with the 22,610,180-byte `.prev` retained, byte-identical md5 on the unit, `S60capsulad restart` healthy |
| Bounded logs and storage guard | implemented: 512 KiB log file plus one rotated copy, configurable `-wakeword-negatives`, and a 2 MiB free-space floor for new takes and training | `internal/logbus` rotation tests; deployed `/data` at 93 % (earlier), 69 % used / 24.7 MB free after the 2026-11-12 deployment |
| Personal wakeword embedding engine and training | implemented: incremental Google/openWakeWord backbone, ARM64 NEON, shared-projection head, on-device training and atomic replacement; strict warm `CNEG0002` corpus with real same-split preroll; legacy mel models and cold corpora refused. Inference, enrollment and training use the shared processed capture tap with take-provenance sidecars and in-memory conversion of legacy raw takes; the installed raw-trained model stays selected with an unverified-threshold warning; CNEG is auxiliary off-policy (only local processed held-out takes certify) while a broad-CNEG false activation remains a hard acceptance veto; the earlier broad-negative guard is not weakened, and provenance/InputPolicy validation is closed. | ONNX/kernel parity, gradient checks, real Mini frontend 37.9 ms/output, production loader accepts the 872-stream gzip corpus; `word.personal` installed and selected after full native training (670.6 s; dated on-device A35 replay 6/6 words, 9/9 backgrounds — installation evidence, not accuracy) and a one-shot 237-clip fresh negative set with zero false activations; settled warm classifier replay (2.32 s own ambient + word + tail) fires 6/6 raw and 6/6 processed — legacy-model usability only, not calibration; user reports live use mostly works (informal, not an accuracy/daily-FAR certificate); personal trial, not field certified; see [WAKEWORD.md](WAKEWORD.md) |

### Fresh daisyUI console (2026-11-14; source implemented, locally verified)

The `web/` browser UI was rewritten from scratch on daisyUI 4.12.23 / Tailwind CSS 3.4.17 (pinned
Vue 3.5.13, Vite 4.5.5, PostCSS 8.4.49, autoprefixer 10.4.20) with a light-neutral/muted-teal
`capsula` theme, local-only assets and a top navigation bar for Main, Settings, Access and
Diagnostics. No markup, layout or CSS from the previous hand-styled UI was reused. The binary
`capsula.web.v1` WebSocket, the secure session HTTP boundary and the source-level command allowlist
are unchanged; there is still no generic AppBus/capture/asset proxy.

Implemented in source:

- Settings: header anchors, section filter and a no-results state; voice assistant default
  app/none filtered to apps that declare `voice_assistant` (a saved app that is no longer eligible
  or installed stays selected, marked, and is never rewritten) and Hold to summon / Hold to talk
  touch behaviour, with the embedded guided wakeword area (training dialog and the real last-20
  engine detections); display and ring default app/none filtered by
  `segment_display_provider`/`led_ring_controller` with the same saved-value preservation, plus an
  automatic-brightness checkbox that disables the brightness slider and is gated on the reported
  ambient capability; a positive microphone control (ON = enabled, visible
  “Microphone on/off”, aria label “Enable microphone”) with hide-delay presets “Never (keep the light
  visible)”/1/10/30 minutes (the stored value is the `none` mode; a stored
  non-preset mode or delay is surfaced as a labelled current value and is not rewritten on load);
  Bluetooth “Speaker mode”/“Pair new devices” policy plus paired-device unpair confirmation from
  the overview topic; Wi-Fi active-network protection, a manage-networks dialog that forgets
  inactive networks only, explicit Scan, manual hidden-SSID connect and the setup-AP note naming
  the device and `capsulad`; a small speaker-name row, startup-sound toggle, master volume and a
  timezone view that opens with only the current setting, phone/computer suggestion and UTC —
  matching cities appear only after a search, the browser zone needs an explicit Apply, and raw
  identifiers/firmware defaults stay under Technical details. Speaker-name and Wi-Fi interface
  technical language is likewise behind Technical details.
- Application detail and its declared settings open over Main in a dialog; the sandboxed custom
  settings document/bridge remains the only custom-UI path.
- Diagnostics host metrics are Linux-only (`/proc` based) and report an explicit
  unsupported-platform state with a reason elsewhere instead of zero measurements; each metric
  domain carries its own availability flag. Wakeword detection history is the daemon's real last
  20 engine activations, not manual-button, simulator or API events.
- Live controls keep field-scoped compare-and-swap patches, one serialized queue per tab, a visible
  pending/error state, preserved dirty drafts and no polling. Diagnostics partial log/metric frames
  never suppress an older-reserved full snapshot: full fields and metrics keep independent
  watermarks, logs are unioned, deduplicated, sorted and capped at 400 entries (200 while paused),
  and a within-connection resync never discards newer streamed entries; reconnecting starts a new
  monotonic epoch, clears the log and paused buffers and preserves the pause choice. The topic
  helper exposes the applied revision, which the page reads before writing snapshot data. Generated-
  frame regressions cover a full snapshot at revision 10 arriving after log revision 11 and metric
  revision 12 retaining the new partial and full state, plus the reconnect epoch. The per-domain
  ordering was proven by those generated-frame regressions and the final build; the fix changed no
  UI markup, so the earlier browser flows remain the proof surface.

Observed local verification (source and local runs, not a device deployment):

- Frontend: `node --test web/test/*.test.mjs` — 11/11 passed. The final Vite production build of the
  fresh SPA, the embedded host daemon build that serves it and the Linux arm64 crossbuild passed
  after all patches, including the final positive-microphone copy/inversion.
- Go: `go test -race ./internal/webui` — ok, 22.672 s. The focused wakeword real-origin last-20
  detection-history test passed with `-race`. The focused daemon install tests (precommit and
  commit-winning cancellation) plus the existing install/train tests passed with `-race` in 2.045 s
  using a matching host native runtime. The targeted `TestTraining` `-race` run (4.581 s) confirms
  that neither a local nor a generic cancel claims the installed model is unchanged; only an
  explicit daemon precommit status does.
- Native Tern browser through a throwaway loopback HTTP→TLS/WebSocket proxy exercised the 390 px
  and 849 px surfaces: login with live startup and no reload; live microphone changes plus an
  independent-client push that preserved an unsaved name; timezone city search with explicit Apply;
  the Clock app's sandboxed `allow-scripts` settings document reaching ready and a blink patch
  persisted through the public CLI; API-key create, clear and revoke; password change with
  reconnect; authenticated certificate PEM download as a 200 attachment; Settings filter
  no-results; wakeword room-take start and cancel with simulator routing restored to its default
  baseline and no captures; diagnostics pause with 10 new / resumed 27 logs and filtering; and a
  final main-toolbar Bluetooth/media/volume/microphone pass before the application list with no
  overflow at 849 px. The latest 390 px proxy run started with the microphone ON; switching it OFF
  showed “Microphone off” and the public CLI persisted `microphone_muted=true`, then switching it
  ON restored it. The final Main screenshot at 390 CSS px (`scrollWidth` 390) kept
  Bluetooth/media/volume/microphone before the application list with no overflow. Diagnostics
  Refresh returned the current runs/logs plus the honest host-metrics-unavailable state with zero
  JS errors after the domain-ordering patch, and sign-out succeeded. All final smoke services were
  stopped and the proxy was being removed again.

Limits of this evidence: the managed WebKit build crashed in EGL and Chromium failed its Mach-port
launch; Tern ignored resize emulation, so the viewport was dictated by the pane rather than a full
1440 px desktop or a physical phone. A direct native Tern load of the self-signed TLS endpoint with
`insecure:true` was still refused ("Unacceptable TLS certificate"), and certificate validation was
not weakened for testing; the throwaway HTTP→TLS proxy run remains the browser proof, and its smoke
services were stopped and proxy script deleted. The log/metric ordering fix changed no UI markup, so
the earlier browser flows remain the proof surface. No real Wi-Fi or Bluetooth peer, physical
temperature, acoustics, or full training-quality run was exercised, and nothing was deployed.
Wakeword training remains a single personal-model slot; a cancel that arrives as the final commit
is being written may still install the new model, so a generic cancel never promises the previously
installed model is unchanged. The WebKit/jsdom, simulator and fixture evidence recorded above
predates this rewrite and is retained as historical evidence, not as proof of it.

### Focused checks for the 2026-11-14 source change

Run after the integrated tree is built; no device access is implied. On macOS use the external
linker and a matching host native runtime (`CAPSULA_NATIVE_LIB_DIR`).

```sh
go test ./internal/dcron ./internal/ringpalette ./internal/appui ./internal/wifi
go test -race ./internal/webui ./internal/daemon -run 'SettingsUI|Wifi|Bluetooth|Clock|Ring|Builtin'
(cd sdk/go && CGO_ENABLED=1 go test ./...)
go build ./cmd/capsulad ./cmd/capsula
sdk/go/regenerate-abi.sh --check
```

The final integrated verifier should also regenerate the web bindings and the embedded framework
descriptor after all schema owners finish:
`go run ./web/protogen` and the `protoc ... clock.proto ring.proto` command in the README. These
commands are the recommended narrow set; they do not replace physical RF/Bluetooth/ap proof.
The verifier's close-out evidence after the final handoffs is recorded below as observed local
results, with the browser-engine and physical limits retained.

### Integrated verifier evidence (observed, after all owner handoffs)

Observed by the build verifier after the final owner handoffs and rebuild. Other limits below still
apply.

- Rebuild after the available handoffs: `make native` (host runtime embeds the label-bearing
  descriptor; repo and SDK protosets byte-identical), SPA with the pinned offline Vite toolchain,
  `make capsulad capsula`, `GOOS=linux GOARCH=arm64 go build ./cmd/capsulad ./cmd/capsula`, and
  `go build ./...` — all ok. Proof daemons/temp data/harness scripts/temporary fixtures and the
  `web/node_modules` symlink were removed; port 8443 is free.

- Generated artifacts were byte-identical to fresh output: framework/clock/ring/web Go bindings
  (`--go_out`/`--go-grpc_out` source-relative), `web/src/proto/web_pb.js`/`.d.ts`/`runtime.js`,
  the combined SDK descriptor (`framework+media+clock+ring`) against
  `sdk/native/descriptors/capsula-framework.protoset` and
  `proto/capsula-framework-v0.5.protoset`, and the embedded descriptor in the rebuilt macOS host
  runtime (37 capabilities including `capsule.service.bluetooth-media`).
- Focused Go checks on that intermediate tree:
  `go test -ldflags '-linkmode=external' ./internal/dcron ./internal/ringpalette ./internal/presets ./internal/registry ./internal/manifest ./internal/appui` — ok;
  `go test -race -ldflags '-linkmode=external' ./internal/bluetooth ./internal/settings ./internal/wifi ./internal/webui` — ok;
  `GOOS=linux GOARCH=arm64 go build ./cmd/capsulad ./cmd/capsula` — ok.
- SPA rebuilt with the pinned offline toolchain; `node --test web/test/*.test.mjs` — 4/4 bridge tests.
- Real built SPA executed in jsdom against a real simulator daemon with TLS/auth/WebSocket/protobuf:
  two sessions converged for volume, incoming-connections and microphone state without reload; a
  rapid slider sequence ended at the authoritative value; dirty hostname/timezone/delay drafts
  survived an external push without being submitted, then persisted only on explicit Save; clock
  stopwatch start/pause/resume/reset and timezone followed the setting.
- Settings-UI route/documents observed on a real current-build daemon: both built-ins returned
  HTTP 200 (clock 34,603 bytes, ring 31,019 bytes), `X-Frame-Options` absent, document CSP
  included `frame-ancestors 'self'` and `sandbox allow-scripts`; executing the served documents in
  jsdom ran the injected kit/bridge and rendered their editors. The SPA app page used
  `iframe src=/api/apps/<id>/settings-ui`, `sandbox=allow-scripts`, no `srcdoc`; the built dist
  contains no `srcdoc`.
- Wi-Fi available surface with a temporary simulator fixture: Settings showed the current/known
  networks and no scan results before Scan; pressing Scan produced results, Use filled the SSID,
  Connect reported success and cleared the passphrase field. A live captive-portal HTTP fixture
  could not bind because this host cannot assign `127.0.0.2`; portal proof remains the existing
  `httptest` handler tests. Physical AP/RF/captive behavior is still unproven.
- Targeted public supervised-identity/bus tests passed in the intermediate tree:
  `TestPublicAppBusTargetReachableByOrdinaryClient`,
  `TestInternalBusAuthorizerAllowsOnlyDeclaredInterfaces`,
  `TestAdministrativeRPCsRejectApplicationIdentity`,
  `TestAppScopeRejectsForeignApplication`, `TestVirtualRunIdentityRequiresDaemonPeer`.
- **Later targeted rerun after the AppsBackend fixes (tests updated by the owner): PASS.** `go test
  -ldflags '-linkmode=external' -run 'Builtin|Clock|Ring|Alarm|Migration|Watch|Fail|Stale|Label|DST|Atomic|Settings|AppBus|Identity|Inproc|SystemApp|Supervisor' ./internal/daemon` — ok, 8.489 s. The
  earlier nine failures (alarm race, minute-exact expectations, protojson decoding) were test
  causes; no backend contract change. `go test -race ./internal/webui` — ok 14.5 s after a
  test-only `HasMode:true` fix; targeted `-race` Bluetooth/A2DP/management/ordering/convergence/
  audio across `internal/daemon`/`internal/bluetooth` — ok; settings/race, dcron/settings, and
  manifest/appui/registry — ok.
- Runtime masks/clock surface on the real daemon with the real dist in jsdom: partial settings
  updates changed only the named `updateFields` path (display brightness 80→45 changed
  `display.segment.front` only, leaving `light.ring.main` at 0.80 and `brightness_mode` empty);
  muted indicator `mode=immediate` and `delay=50000` persisted together while hostname/timezone
  were untouched. Clock page stopwatch Start/Pause/Resume/Reset behaved correctly, a 2 s timer
  became `completed` with an active `timer/2s` alert, and Dismiss removed it — all through the
  public clock contract from an ordinary authenticated browser session.
- **Wi-Fi (final, after WifiProvisioning's handoff):** `go test -race -count=1 -ldflags
  '-linkmode=external' ./internal/wifi` — ok, 9.383 s on the final tree (child-lifetime context,
  stock import/adoption, previous-network recovery, ATTACH reconciliation, AP scan capability and
  token-expiry regressions included); `go test -count=1 -ldflags '-linkmode=external'
  ./internal/webui` — ok, 2.533 s; `GOOS=linux GOARCH=arm64 go build ./cmd/capsulad ./cmd/capsula`
  and `go build ./...` — ok; host `capsulad`/`capsula` rebuilt afterwards. The available-surface
  fixture (earlier provisional run, unchanged SPA and simulator wiring) showed no pre-Scan results,
  Scan/Use/Connect, passphrase clearing and saved networks. A live captive-portal HTTP fixture
  cannot bind on this host (macOS rejects `127.0.0.2`); portal evidence remains `httptest`
  host/origin/token/subnet/connect-flush only. Physical AP/RF/captive behavior remains unproven.
- Wi-Fi AP-mode scan capability limitation: `scan_supported` is computed from the station tooling
  (`wpa_cli`) and is reported false for the setup AP after a refusal without `iw`; the UI/portal
  disable Scan accordingly. Stock firmware has no `hostapd`/`iw`, so AP-mode scanning and physical
  association, DHCP/DNS/iptables and captive detection on the target remain hardware gates.
- Settings-UI/route recheck on the rebuilt daemon: `/healthz` ok; clock (34,603 B) and ring
  (31,019 B) documents returned 200 with no `X-Frame-Options` and CSP
  `frame-ancestors 'self'; sandbox allow-scripts`; both execute and post the bridge message; the
  SPA iframe uses the endpoint as `src`, `sandbox=allow-scripts`, no `srcdoc`.
- Limit: evidence is real-daemon/jsdom/httptest/simulator, **not a browser engine or radio**.
  Browser-engine launch is unavailable on this host, so iframe sandbox/CSP enforcement and visual
  layout are unproven; physical Bluetooth, Wi-Fi/RF, AP association, captive detection and optical
  behavior remain gates. The full non-targeted daemon suite was not rerun after the final fixes;
  the earlier full run predated the alarm-test fixes and is not a current result.

## Explicitly unavailable

- **User, PID, network and IPC namespaces, and seccomp filters, on the RK3308 stock kernel.** Measured
  on a live unit: `/proc/self/ns` exposes only `mnt`, `unshare(CLONE_NEWUSER|CLONE_NEWNET)` returns
  `EINVAL` and `PR_SET_SECCOMP` returns `EINVAL`. The daemon therefore runs apps in a root mount
  namespace with a distinct UID, reports `mode=mountns`, and drops the seccomp claim instead of
  failing the launch (`Seccomp` is probed by a helper process before any app starts).
- **Raw touch coordinates on the current unit.** Its only evdev node is `gpio-keys`; there is no
  coordinate panel, so `touch.panel.main` advertises `coordinates_available=false` and
  `full_raw_available=false`. This does not reduce the key surface: all five keys and the inferred
  swipes/circles are available, and `full_panel_available=true` lets full capture receive every
  key and gesture. Raw coordinate delivery on a real coordinate panel remains simulator-tested.
- **Real mDNS resolution and real-peer Bluetooth recovery.** Host-name ordering is tested against
  a portable fake Avahi responder and a real adapter over a private D-Bus daemon; a read-only
  device snapshot observed Avahi `RUNNING` with `capsula`/`capsula.local`. `avahi-resolve` on a
  device after a save and after an Avahi restart, real collision behavior, recovery of a real
  source that already abandoned A2DP in the pre-daemon window, and the browser proof against a
  successful responder still need device/peer access. The simulator reports mDNS unsupported and
  cannot rename the host. The Web UI field/status/save/persistence and Restart labels were observed
  in a real Chrome-headless browser against the simulator daemon; actual `.local` resolution
  remains the device gate.
- **QuickJS / MicroPython** host runners. Implemented and exercised on the simulator: real
  quickjs-ng 0.18.0 and MicroPython 1.29 embed hosts are built from pinned sources, staged beside
  `libcapsula`, discoverable through `ListRuntimes`, and admitted by the device gate when ABI 1 is
  installed. Both reach `APP_READY`, receive invocations/settings, make permitted unary framework
  calls, exchange routed unary calls, exchange a routed stream and cancel it, and survive Stop and
  SIGKILL without orphan hosts. A lost daemon closes the bus socket: each host queues exactly one
  bounded terminal disconnect job for its script handler and exits without a supervisor. The
  `capsula_host_test.c` queue test plus the daemon-crash phase of `interpreter/smoke/run.sh` are the
  reproducible proof. On the unit (2026-11-14) both hosts are installed in `/data/capsdemo/lib`:
  `ListRuntimes` returned `quickjs-ng 0.18.0` and `micropython 1.29.0-preview` with shared ABI 1.0,
  and a temporary QuickJS sample package ran `APP_READY` under `mountns`, read the model string from
  `DeviceService/GetInfo`, accepted an invocation, and exited 143 on Stop with no orphan host.
  Remaining: no MicroPython package was run on the device (its host is installed and reported
  only), and the kernel still provides mount-only isolation for script hosts.
- **Amlogic** stock/Alpine backends. Platform selection fails with an explicit message.
- **A field-certified personal wakeword model for the connected room.** A warm-corpus
  `word.personal` model is installed and selected, and it remains the raw-trained artifact: on the
  now-default processed tap its calibrated threshold is unverified (an explicit warning is logged,
  and no retrain is forced). Native training completed in 670.6 s and a dated on-device A35 replay
  scored 6/6 word takes and 9/9 backgrounds (installation evidence, not accuracy). On the settled
  current profile, the warm classifier replay (own same-session ambient + word + tail) fires 6/6 raw
  and 6/6 processed — legacy-model usability only, not calibration. It remains a
  small personal trial: long-duration false-activation/miss rates are unmeasured, no
  speaker-independent Russian head is bundled, and zero false activations over 40 minutes is not a
  daily-rate guarantee. See [WAKEWORD.md](WAKEWORD.md).
- **Vendor system-service control** (`SystemService.ControlUnit`). Returns `UNIMPLEMENTED`.

## Source-completeness gaps (2026-11-14 assessment)

Read-only assessment of the current working tree (HEAD `9574699` plus uncommitted changes) against
the documented contracts. Every item below was confirmed in source; the hardware/acoustic/RF gates
are listed separately under [Next gates](#next-gates-in-order) and are not repeated here. "Verified"
means read in the current tree, not re-run on a device.

### Correctness defects

1. **Native FFI buffer length/free contract.** `alloc_buf` reports `len` including the appended NUL
   while the header documents `len` as excluding it (`sdk/native/src/ffi.rs:148-165`;
   `sdk/native/include/capsula/capsula.h:108-110`), and `free_buf` reconstructs `len+1` bytes and
   drops that box (`ffi.rs:171-184`), so every `capsula_buf_free` overruns the allocation by one
   byte on both the JSON and raw paths. The Rust binding copies `buf.len` verbatim
   (`sdk/rust/src/lib.rs:289-296`) and feeds it to `serde_json::from_slice`
   (`sdk/rust/src/client.rs:295-300`), so library JSON with a trailing NUL fails to parse; Go
   tolerates it only because `unmarshalProtoJSON` uses a streaming decoder
   (`sdk/go/capsula/codec.go:307-313`). No `ffi.rs` test covers length or free.
2. **`ListRuns`/`WatchRuns` identity fallback.** Both use `callerIdentity` — app-scoped only while
   the run record is live — instead of `applicationCaller`/`requireAppScope`, which also checks the
   sandbox UID or supervised PID (`internal/daemon/services_app.go:109,125` vs
   `internal/daemon/appfacade.go:31-62`). A connection whose run record is gone but whose socket
   persists is treated as an administrator and can enumerate/watch all runs and name a foreign
   `app_id`, contradicting the documented "cannot become an administrator by silence" guarantee
   (`docs/APP_API.md`).
3. **`BeginConversation` ignores `activation_id`.** It opens a conversation/ducking context for any
   caller without binding the framework-issued activation, although the request field exists
   (`internal/daemon/services_mediaplane.go:891-910`; `proto/capsula/framework/v1/framework.proto:684`).
   Pre-roll remains activation-gated; ducking does not.

### Missing enforcement / workflow gaps

4. **App install requires a daemon restart, and the CLI says otherwise.** The registry is opened once
   at startup (`internal/daemon/daemon.go:378-380`); there is no rescan, install RPC or web route,
   and `capsula app install` prints "restart capsulad (or wait for its rescan)"
   (`cmd/capsula/main.go:306`). Install also does not validate entrypoint existence/type
   (`internal/manifest/manifest.go:236-239,365-383`); that only happens at launch
   (`internal/sandbox/sandbox.go:218-232`), so a package with a missing binary installs and fails on
   first start.
5. **Provenance is report-only, not a gate.** `provenance.Verify` is called only from
   `GetBinaryProvenance` (`internal/daemon/services_app.go:75`); neither install
   (`internal/registry/registry.go:291-315`) nor launch (`internal/daemon/lifecycle.go:28-114`)
   consults it, even though the known limits below designate provenance "the control that matters"
   on the stock kernel. The boot hook also starts the daemon without `-trusted-root`
   (`deploy/rockchip-stock/S99btctl.sh:209-210`), so attestation-bearing packages return
   `PROVENANCE_UNVERIFIED` on the deployed unit regardless.
6. **Asset catalog and interface examples are stale or empty.** `assets.Open` runs once at startup
   (`internal/daemon/daemon.go:405`) and `ListAssets` serves that snapshot
   (`internal/daemon/services_system.go:348-356`), so a newly registered asset is invisible until
   restart even though the assistant validator reads `manifest.json` fresh
   (`internal/daemon/assistant_settings.go:157-190`). `InterfaceInfo.request_example_json` /
   `response_example_json` (`framework.proto:944-945`) are never populated by the daemon.
7. **Generic app-settings `Update` misclassifies storage failures.** It string-matches "stale" and
   maps everything else to `INVALID_ARGUMENT` (`internal/daemon/services_app.go:239-247`), while
   `appSettingsStatus` exists to return `INTERNAL` for IO (`internal/daemon/builtin_status.go:12-35`)
   and is used by the built-ins.

### SDK / release-engineering gaps

8. **The frozen-ABI gate does not exercise the frozen ABI.** `make test-sdk` runs only `sdk/go` and
   `sdk/rust` unit tests (`Makefile:174-176`); the native table/prefix/descriptor tests
   (`sdk/native/src/api.rs:893-927`, `dynamic.rs:413-436`) are reachable only through
   `sdk/native/Makefile:52-53`, which no root target calls, and the C++ example is never compiled by
   a gate. The only Go test that loads `libcapsula` skips without `CAPSULA_TEST_LIBRARY`
   (`sdk/go/capsula/sdk_test.go:291-295`), so `make test-sdk` can pass with zero real-ABI coverage.
9. **Script hosts lack the audio fast path and device parity.** QuickJS/MicroPython expose
   calls/bus but no raw-PCM capture/playback entries (`interpreter/quickjs/host.c:729-761`;
   `interpreter/micropython/modcapsula.c:82-530`), although the frozen ABI and Go binding do
   (`capsula.h:248-268`; `docs/APP_API.md:263-265`). Both hosts are installed on the unit but only a
   QuickJS package was ever run there ([Next gates](#next-gates-in-order) item 9).
10. **No reproducible release artifact.** The deployed daemon was built from an uncommitted tree (see
    the Web UI rewrite deployment entry below); the version is still `0.1.0-dev`
    (`internal/version/version.go:7`) and there is no tag or artifact manifest tying source → daemon
    → SPA → native runtime → assistant hashes. `make native` invokes Cargo without `--locked`
    (`Makefile:134`) even though `Cargo.lock` is committed, and interpreter sources are downloaded at
    build time unless `*_SRC` is supplied (`interpreter/build.sh:75-86`).

### Documentation drift

The following current-state statements contradicted the source or the dated deployment records. The
present-tense ones marked "corrected" were fixed inline in this assessment; the dated historical
entries are left untouched, and source-comment/capability-string drift is tracked here rather than
silently rewritten:

- **Boot integration (corrected).** The "Implemented and exercised" matrix row said the single-hook
  build was "**not** deployed to the unit" and the known-limits section repeated
  "shell-checked/simulator-only for now", while three later dated entries install and restart through
  `/etc/init.d/S99btctl.sh`. `docs/HARDWARE.md` likewise said the unit still runs the historical
  two-file integration. All three present-tense statements were corrected to match the deployment
  records; a power-cycle/vendor-fallback exercise of the current build remains a gate.
- **Credential reload (corrected).** "A key created by the CLI is not accepted until the daemon
  restarts" was stale: the daemon watches `credentials.json` and reloads live
  (`internal/daemon/credentials_watch.go:12-78`), as `README.md` already states. The known-limits
  bullet was rewritten to the live-reload behavior.
- **Backend capability strings.** `internal/backend/rockchip/rockchip.go` still reports ring/segment
  "hardware validation pending" (`:104-105,:148-149`), audio "must be validated on hardware"
  (`:182-183`) and wakeword "stays unavailable until a detector is integrated and tested"
  (`:231-233`), although the maps/mixer have device evidence in this document and the framework
  ships an integrated detector (`internal/daemon/wakeword_engine.go:186-252`). Clients reading
  `BackendAvailability` therefore see stale PARTIAL/UNVERIFIED detail. (Source strings not yet
  changed; tracked here.)
- **Stale sizes (corrected).** `docs/HARDWARE.md` cited a 23,003,288-byte daemon; the latest deployed
  daemons are 26,935,556 / 27,132,164 bytes (deployment entries below). The HARDWARE operating
  guidance was updated to ~27 MB.
- **APP_API claims (corrected).** `docs/APP_API.md` stated the socket default is
  `/run/capsulad/hal.sock` (the host default is `<data-dir>/run/hal.sock`; that path is the
  in-sandbox mount), that `app install` "validates … entrypoint" (it validates entrypoint *metadata*;
  existence/type is checked at launch), and that an app cannot "access another app's settings"
  without carving out the public clock/ring contracts. All three were corrected, and a note
  documents the public-contract exception.
- **Comment drift.** `internal/daemon/bluetooth_audio.go:17-21` describes Bluetooth audio as
  PulseAudio-only ("not ducked or re-mixed"), true only of the fallback; the primary path decodes SBC
  into the mixer (`internal/daemon/bluetooth_a2dp.go:262-330`). `internal/daemon/systemapp.go:32-37`
  and `internal/bus/bus.go:31-37` still say "protobuf JSON" although the built-in wire is binary
  protobuf (`internal/daemon/builtin_native_wire_test.go:26-63`). (Comments not yet changed; tracked
  here.)

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
- **Panel key ownership is mode-dependent.** Partial capture owns circle/centre and receives
  inferred swipes/circles; volume and microphone remain framework controls. Full capture receives
  every key and gesture with all system actions suppressed. On the Mini, `KEY_VOICECOMMAND`/
  `BTN_TRIGGER_HAPPY1` is the neutral centre, not an assistant key; the talk key is `play_pause`.
  Raw coordinates are a separate capability and are absent on this unit.
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
  parses BlueZ `MediaPlayer1.Track` when present. A source that exports only the deprecated public
  `MediaControl1` publishes no playback status, so a toggle's direction falls back to its A2DP
  transport state or the sink's packet-receipt freshness; when none is available the command is
  Play, not an inferred Pause. HSP/HFP telephony is not implemented; only A2DP Sink and AVRCP.
- **Bluetooth volume: synced displays, local bound (PulseAudio fallback).** When native A2DP is
  unavailable and PulseAudio owns the source, the Mac's AVRCP volume and the Capsula master
  are kept equal in both directions, but the Mac is not trusted to attenuate: it can report a
  minimal transport volume while the captured stream is near full scale. The loopback therefore
  applies the master as a local gain (cube-root compensated for PA's cubic sink-input volume), with
  the source at a fixed 90 % trim. The effective level is bounded by the master and is at most
  ~0.73 × it. The PA source/loopback numbers are gain stages, not the listening volume; making all
  three stages numerically equal would either double-attenuate or, with no local ceiling, allow a
  “5 %” setting to play at full scale.
- **Bluetooth audio prefers a native A2DP SBC sink endpoint.** The stock `voice-agent`/`btctl`
  chain sounded better than the old PulseAudio source + loopback path, which added a resampler and
  a second volume stage. capsulad registers an `org.bluez.MediaEndpoint1` at
  `/org/capsula/a2dp/sbc` through `Media1.RegisterEndpoint`, verifies the registration is armed for
  the current adapter generation, and does so before the adapter is powered for
  connectable/discoverable use. It removes PA's bluez5 Bluetooth endpoints only after the
  native sink is armed, and never while an active source is routed through PA; that switch completes
  after disconnect. It obtains the transport fd through `MediaTransport1.Acquire`. BlueZ 5.65 calls
  `SelectConfiguration(ay)` and
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
  fixture. The stripped daemon binary was unchanged in size for that change (22,479,000 bytes; the
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
- **PulseAudio Bluetooth modules are a fallback owner, not the primary one.** When native endpoint
  registration fails, the daemon loads `module-bluetooth-discover` and routes the source with a
  250 ms loopback. A BlueZ restart or adapter power cycle invalidates the native registration; the
  next connect or explicit pairing change repairs it before requesting profiles. If PulseAudio is
  restarted while it is the fallback owner, no monitor reloads the modules immediately; readiness is
  re-established on the next connect or incoming/discoverable change. A source that was already routed through
  PA keeps that route and is not torn down to switch owners.
- **BlueZ may autopower the adapter before capsulad starts.** In that pre-daemon window a paired
  source can open an ACL without the framework's sink advertised. The incoming-connect handler
  repairs/verifies registration and requests A2DP/AVRCP over the same link (no second connection);
  whether a peer that already abandoned A2DP honors that request without reconnecting is
  device-dependent and remains a real-peer hardware gate. Preventing the window requires the daemon
  to own adapter power before BlueZ or the peer is active.
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
  single-hook version has been the installed service since 2026-11-13 (dated deployment entries
  below), while a power-cycle and vendor-fallback exercise of it remains a gate. If the overlay is
  wiped (factory reset), the device returns to the stock assistant and the deployment must be
  repeated. The daemon ignores SIGHUP and is started with `nohup`; the watchdog handoff is
  unchanged.
- **Local credential changes apply live.** `capsula auth …` and the API-key CLI write the same
  security store the daemon loads, and the daemon watches `credentials.json` and reloads it without
  a restart (`internal/daemon/credentials_watch.go`); a password change also closes live browser
  sessions. Keys created or revoked through the Web UI go through the running daemon directly. No
  restart is needed after CLI credential changes.
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

### Script runtimes, embedded SPA and private-contract stack deployed (2026-11-14)

> **Historical record (superseded for clock/ring).** This entry describes the binary installed on
> that date, which exposed clock/ring only through a private protocol. The current source replaces
> that with public standard contracts and does not revoke this dated evidence; it has not been
> deployed to the device.

The current source tree (private clock/ring protocol, native SDK descriptor activation field,
assistant hold-to-talk contract, embedded Vue SPA plus authenticated WebSocket, RPC credential
watch) was built for the unit and installed through the canonical boot service. The unit was
discovered as the only attached adb device (RK3308 `Capsula-mini-03104B1D40E15C55`, glibc 2.33,
`/lib/ld-linux-aarch64.so.1`); no IP, hostname or user choice was assumed from earlier sessions.

Build used the persistent `capsula-arm64-builder` container (Debian Bullseye arm64, glibc 2.31,
Go 1.22.12, rustc 1.100.0, gcc 10.2.1) and the repo Makefiles with
`GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu`; `internal/webui/dist` was the
current checked-in SPA build (newer than every `web/src` file, and the toolchain cannot be
reinstalled offline, so it was embedded as built rather than regenerated). `interpreter/build.sh`
needed one portability fix for this glibc: the MicroPython host link now passes `-ldl` on Linux
(the QuickJS link already did), because Bullseye still ships `dlopen` in `libdl`. All six ELFs pass
`check-target-compat.sh` against the extracted system image: `capsulad`/`capsula`/`dumbassistant`
require at most `GLIBC_2.17`, `libcapsula` `GLIBC_2.30`, both hosts `GLIBC_2.29`; every
`DT_NEEDED` resolves in the device rootfs and the interpreter is the device's loader.

Installed artifacts (host and device hashes verified equal):

- daemon `234c8d5280a4b6b153033142153ecdc4113e28458202ea9b3d05b3ef5f988e8d`, 26,345,732 bytes; the
  previous `ff3646bc…bc8687` is retained as `capsulad.prev`;
- CLI `c6f9c20812386257006cb353fefcb637204f2c2b36cfa2367de9589cf4ec5fce`, 11,337,988 bytes;
- native SDK `a33f5dddd2490dfb965e9e2a3eda2560d873c694cb7ab5f75b013a17f4433b21`, 4,109,688 bytes
  (ABI 1.0, unchanged capability vocabulary `6294d52a…`, descriptor set includes the
  `AssistantActivation.hold_to_talk` field);
- QuickJS host `607bf46762151d9b383106e5cfb0408c65e7d09abb3395cf8c1d33e44ad06884` (quickjs-ng
  0.18.0) and MicroPython host `b2519353d0e028a6c3ae916d59198660a8fc8f4fdece14395c00e0a241731fef`
  (micropython 1.29.0-preview), both staged beside the library in `/data/capsdemo/lib`;
- optional assistant `886e104a64e62592aac1583f9d8b3c61e0afd1f6b59f3ef071038f9ea2cc80a5`,
  2,169,432 bytes, manifest and `reply.wav` unchanged.

The full previous stack (daemon, `.prev`, library, metadata, assistant package, device settings,
both app-setting stores, credentials, wakeword model, boot hook, boot-dir contents, vendor-init
copies, boot log) was pulled to
`_capsula-mini-deploy-backup/20261113T161338Z-script-runtime-spa-stack/` before any mutation. The
old `capsulad.prev` was removed only after that host copy was hash-checked; `/data` then had
43.2 MiB free, enough for the staged rename.

Library, metadata, both hosts and the assistant binary were swapped atomically (`*.new` → rename)
while the daemon was stopped through `/etc/init.d/S99btctl.sh`; the daemon itself was installed by
`capsula daemon install`, which restarted it through the same hook and gated on `-health`
(`boot.log` 19:19:06 → 19:19:08 healthy). No watchdog or vendor chain was touched.

Verified after the restart:

- health exit 0, `capsula.clock` and `capsula.ring` (builtin) plus `org.capsula.dumbassistant`
  (`mountns`) all `APP_READY`; the builtin clock published its face through the private protocol
  (`output … cells=[63 63 91 63]`), so the internal client path works while an external SDK client
  is refused: `call_start_routed` to `capsula.internal.clock.v1.ClockService` returned
  `PermissionDenied`, "this app-bus service is not exposed to application callers". The same probe
  was refused `SettingsService/GetSettings` ("device settings are a local administrator operation")
  and used only public read APIs otherwise.
- `ListRuntimes` over the deployed library reports native, `quickjs-ng 0.18.0`, `micropython
  1.29.0-preview` and builtin as available with shared ABI 1.0.
- A temporary QuickJS sample package installed, ran `APP_READY` under `mountns` (pid verified),
  logged `sample: READY`, read `RK3308-based Capsula-mini` from `DeviceService/GetInfo`, accepted an
  invocation, and exited 143 on Stop; no `capsula-quickjs` process remained. The package was then
  removed and the registry restored by a canonical restart. No MicroPython package was run.
- Served SPA bytes match the embedded build exactly (`/`, `/assets/index-94dfe86a.js`,
  `/assets/index-5586fb90.css` sha256 equal to `internal/webui/dist`); `/api/status` and `/ws`
  return 401 without a session and the WebSocket route is present. Credentials were not read,
  changed or rotated, so the authenticated browser/WS session itself was not exercised.
- Settings are byte-identical to the pre-deployment snapshot (`settings.json` `afed1df8…`): revision
  2389, display provider `capsula.clock`, assistant `org.capsula.dumbassistant`, no ring provider
  (explicit none), wakeword `word.personal` enabled, master volume 5 %, microphone muted,
  `Asia/Shanghai`, brightness auto/const values, `startup_sound_enabled`, `touch_hold_mode=1`,
  `muted_indicator_mode=2`, `bluetooth_pairable=false`, hostname `capsula`. All four app-setting
  stores, the wakeword model (`5f92202f…`) and the three credential files are byte-identical to
  their backups. `/data` ended at 16.4 MiB free (79 % used) with the new `.prev` retained; `/tmp`
  staging and the probe were removed.

Not established by this deployment: authenticated browser/WebSocket behavior, a MicroPython
package run on the device, any microphone capture, wakeword/acoustic behavior, physical
gesture/panel behavior, real-peer Bluetooth and a power-cycle. Nothing here is acoustic or
photometric evidence.

### Public clock/ring contracts, settings UI and Wi-Fi stack deployed (2026-11-14)

The 2026-11-14 source change was built for the unit and installed through
`/etc/init.d/S99btctl.sh`, superseding the private-protocol entry above for clock/ring. The unit
was discovered on the LAN (ARP plus `adb connect`), not assumed from an earlier address:
`Capsula-mini-03104B1D40E15C55`, RK3308, glibc 2.33. The build ran in the persistent
`capsula-arm64-builder` (Debian Bullseye arm64) with `GOOS=linux GOARCH=arm64
`NATIVE_TARGET=aarch64-unknown-linux-gnu`. The embedded SPA was regenerated: the existing
`internal/webui/dist` predated `web/src/views/SettingsView.vue`, so it was rebuilt with the pinned Vite 4.5.5 /
`@vitejs/plugin-vue` 4.6.2 / Vue 3.5.13 toolchain (asset `index-ce23ab88.js` became
`index-a08fbc5e.js`). All deployed ELFs pass `check-target-compat.sh` against the device rootfs
(glibc ≤ 2.30, every `DT_NEEDED` resolves, device loader).

Installed artifacts (host and device sha256 equal):

- daemon `a847c805e110077d6e7e75286b2b93f5a6c4e9cf9aa9ffc00ea06e31641f5756`, 26,935,556 bytes;
  the previous `234c8d52…f988e8d` is retained as `capsulad.prev`;
- native runtime `394ed875b5cbbe5839278f0a7c6440edb64a9131ff06a758dde4cd922cd35a8d`, 4,117,880
  bytes; its descriptor set now includes the public clock/ring services. The previous
  `a33f5ddd…f4433b21` is retained as `libcapsula.so.1.0.0.prev`; `capsula-native.json` is
  byte-identical (`6294d52a…`), so the capability vocabulary did not change;
- optional assistant `ee27cc0aca78c7d8a2db690da749fb6c7130977fde782fe4519381e9c47f74fb`,
  2,182,008 bytes; the previous `886e104a…2cc80a5` is retained as `dumbassistant.prev`. Its
  manifest is unchanged and the user's `assets/reply.wav` was not touched;
- the CLI rebuild (`82f63e81…`) was used from `/tmp` for the upgrade and probes, then removed; the
  unit still has no persistent CLI, as before. The boot hook is byte-identical (`75c3716f…`), and
  both interpreter hosts plus their metadata were left installed unchanged.

Backup and rollback: `_capsula-mini-deploy-backup/20261113T203528Z-public-contracts-wifi-ui/`
contains the pre-deployment settings, all app-setting stores, the app package (including the user's
reply asset), library, daemon, boot hook, boot/vendor-init state, credentials, wakeword model,
`/var/lib/bluetooth`, both wpa_supplicant configurations, and before/after hashes plus semantic
logs. The host copies were hash-checked before the old `capsulad.prev` was removed to make room.
The new `.prev` files hold the complete previous binaries on-device. Rollback: stop
`/etc/init.d/S99btctl.sh`, restore the files from that backup (`capsula daemon rollback` restores
the daemon binary only), and start the service.

`/data` had 40.7 MB free after pruning the backed-up old `.prev` and 8.9 MB free after the
deployment with the three new `.prev` copies retained; `/tmp` staging and probes were removed.

Verified after two canonical service restarts (the daemon install, then the runtime/assistant swap
with the daemon stopped through the same service):

- health exit 0; `capsula.clock` and `capsula.ring` (builtin) plus `org.capsula.dumbassistant`
  (`mountns`) all `APP_READY`; the assistant maps the new `/capsula/lib/libcapsula.so.1.0.0`.
- `AppInterfaceService/ListInterfaces` returns both public contracts from the built-ins
  (`capsula.contracts.clock.v1.ClockService`, `capsula.contracts.ring.v1.RingService`); the same
  query returned `{}` before the swap.
- Raw routed state: clock `GetState` reports `Asia/Shanghai` local time and an idle stopwatch,
  `ListAlarms` is empty; ring `GetState`/`GetSettings` report `breathe`, the migrated gradient
  palette `linear-gradient(90deg, #FF7828 0%, #FF3C96 100%)`, brightness 1, speed 0.1 and the
  preserved 22:00–07:00 window.
- `settings.json` revision 2460 (from 2454) preserves the explicit no-ring-provider choice, clock
  display provider, assistant, `word.personal`, live microphone, `Asia/Shanghai`, brightness and
  hostname. Master volume is 5 % because the user pressed volume keys during verification. The
  only shape migration was `bluetooth_pairable=false` → `bluetooth_incoming_connections=true` +
  `bluetooth_discoverable=false`. The ring palette preset `sunset` was canonicalised to its
  gradient; the clock/ring/assistant app-setting revisions, the assistant `2000-3500` delay and the
  empty alarm list are preserved.
- Web UI: `/` serves the new `index-a08fbc5e.js` (bytes hash-equal to the embedded SPA) and
  `/api/status` returns 401 without a session. A browser at 192.168.1.3 completed an admin login
  and opened a realtime session against the new build. Credentials were not read or changed.
- Wi-Fi: the manager logged "adopted 2 stock Wi-Fi network(s)" on `wlan0`, reconnected to the same
  `ctOS AP` with the same 192.168.1.50/24 address, and left the overlay `wpa_supplicant.conf`
  hash unchanged; no scan was issued. The Bluetooth store and the paired device
  `BC:D0:74:32:51:DC` are hash-identical to the backup, the native A2DP SBC sink registered, and
  the adapter reports PSCAN (connectable) without ISCAN (not discoverable).
- Idle sample with the user's session active: about 44 % CPU idle, daemon RSS 77 MiB, assistant
  9 MiB, `/data` 8.9 MB free.

Found defect, **fixed in source after this deployment**; the installed binaries still carry the
broken wire. The built-ins replied with protojson bytes, while the native SDK
`call_send_json`/`call_recv_json` encode and decode protobuf. Empty requests succeed by accident
(`{}` serialises to zero bytes); a non-empty request (`CancelTimer{timer_id}`) was rejected by the
built-in with `proto: syntax error`, and `call_recv_json` failed on every built-in response. The
Web UI path worked only because it sent and decoded JSON itself. The source correction replaces
the app-bus codec with `proto.Unmarshal`/`proto.Marshal` for all clock/ring unary and watch-stream
messages, and makes the Web UI adapters marshal typed requests and decode typed responses; there
is no dual-format or JSON fallback. Regression coverage: `TestBuiltinPublicContractState` (binary
GetState/WatchState plus non-empty `StartTimer`/`CancelTimer` round-trips and JSON rejection),
`TestRingWatchSettingsStartsWithSnapshot`, the ring-schedule state helper and
`TestDecodeClockStateUsesBinaryProtobuf`. Public service names, descriptors, permissions and
cancellation are unchanged, so the SDK descriptor did not need regeneration. Independent native
`call_send_json`/`call_recv_json` proof lives in the separate consumer regression owned by that
verification task. The contracts are callable by an ordinary SDK app in source; the corrected
daemon was subsequently deployed and proved on the unit (next section).

Source-fix verification on 2026-11-14 (local host, no device access):
`go test -count=1 -race -ldflags '-linkmode=external' -run 'Builtin|Clock|Ring|Alarm|Migration|Watch|Fail|Stale|Label|DST|Atomic|Settings|AppBus|Identity|Inproc|SystemApp|Supervisor' ./internal/daemon`
— ok, 11.091 s; `go test -count=1 -race -ldflags '-linkmode=external' ./internal/webui` — ok,
14.658 s; `go vet ./internal/webui ./internal/daemon` — only the pre-existing
`services_bluetooth.go` lock-copy warning. This fix changed no `.proto`, generated binding or
descriptor file, so `sdk/native/descriptors/capsula-framework.protoset` is unaffected. This proves
the source wire and the Web UI adapter; it is not device or native-SDK evidence.

Not established: physical panel gestures, microphone capture/acoustics, real-peer Bluetooth
pairing/AVRCP, Wi-Fi RF association/AP fallback/captive portal, photometric output and a power
cycle. The deployment produced no microphone capture, pairing change, network change, credential
change or user-visible audio. During the intentional service stop the daemon logged one
`context canceled` built-in recovery line; it did not recur on the running stack.

### Corrective builtin clock/ring binary-wire deployment (2026-11-14T21:10Z)

The corrected daemon was built in `capsula-arm64-builder` from the fixed source (no `.proto`,
generated binding, descriptor, runtime, assistant or SDK rebuild) with `GOOS=linux GOARCH=arm64
NATIVE_TARGET=aarch64-unknown-linux-gnu`, embedding the existing SPA
(`internal/webui/dist/assets/index-a08fbc5e.js`). The installed ELF passes
`check-target-compat.sh` against the device rootfs (glibc max `GLIBC_2.17`, `DT_NEEDED` resolves)
and is 26,935,556 bytes. It was staged in `/tmp` and activated with
`capsula -data-dir /data/capsdemo daemon install -binary /tmp/capsulad.new`, which restarts through
`/etc/init.d/S99btctl.sh` and gates on the framework API; no manual daemon/watchdog kill was used.
Target rediscovered before deployment as `Capsula-mini-03104B1D40E15C55` on `192.168.1.50:5555`.

- daemon `fce5c50605bea63a2ecf60c0e29e07d0abde796690e2dd3938e592573f722316` installed (host and
  device hashes equal); the pre-fix `a847c805…f5756` is retained as `capsulad.prev`. The old
  `234c8d52…` `.prev` was removed first only after its host backup hash was re-verified, because
  `/data` had 8.9 MB free; `/data` is 8.3 MB free after the install.
- unchanged on device: `libcapsula.so.1.0.0` `394ed875…`, `dumbassistant` `ee27cc0a…`,
  `capsula-native.json` `6294d52a…`, `settings.json` `acec2184…` (revision 2460, same values),
  `security/credentials.json` `02e4d0af…`, server certificate/key, `/etc/init.d/S99btctl.sh`
  `75c3716f…`, `/etc/wpa_supplicant.conf` `53af0247…`, `wifi/networks.json` `5ba4372a…`, and the
  `/var/lib/bluetooth` store including `BC:D0:74:32:51:DC` (all hash-identical to the pre-deploy
  pull).
- native SDK C ABI proof on the unit, as an ordinary external client with the clock/ring descriptor
  set registered, using `call_send_json`/`call_recv_json`: `AppInterfaceService/ListInterfaces`
  returns both builtins `available=true`; clock `GetState` returns non-empty JSON
  (`2026-11-14T05:14:48+08:00`, `Asia/Shanghai`, idle stopwatch, revision 1); a non-empty
  `CancelTimer{timerId:"probe-does-not-exist"}` returns structured gRPC `NOT_FOUND` with detail
  `timer probe-does-not-exist is not active` instead of `proto: syntax error`; ring `GetSettings`
  returns the preserved `breathe` gradient, brightness 1, speed 0.1, 22:00–07:00 window; a CAS
  `UpdateSettings` with the same effect advanced revision 23→24 and changed no value; clock
  `WatchState` delivered a non-empty snapshot through `call_recv_json`, `call_cancel` returned 0,
  and `GetState` still answered afterwards. Probe result `failures=0`.
- after the final canonical restart: `capsulad -health` exit 0, `capsula.clock`, `capsula.ring`
  and `org.capsula.dumbassistant` all `APP_READY`; settings revision 2460 unchanged.
- Web UI typed adapters: `/healthz` 200; `/`, `/clock`, `/ring` serve the SPA shell with
  `index-a08fbc5e.js`; served JS/CSS hashes equal `internal/webui/dist` (375afb68…, 21f3dec7…).
  `/api/status` and `/ws` return 401 without a session. A short-lived proof-only admin password was
  set on the device to obtain a session, check `/api/status` (200) and request binary
  `TOPIC_APPLICATION` snapshots: `capsula.clock` returned `ApplicationView.clock`
  (`localTimeRfc3339`, timezone, 67 bytes) and `capsula.ring` returned `ApplicationView.ring`
  (`breathe`, gradient palette, `settings_revision` 23, 100 bytes). That password change was not
  requested or authorized by the user and was outside the stated preserve-credentials scope.
  `credentials.json` was then restored byte-identically (`02e4d0af…`), the proof cookie jar and
  client removed, and the daemon restarted through `/etc/init.d/S99btctl.sh`; the password-hash
  reload plus restart invalidated the proof session (sessions are in-memory only, never persisted).
  Side effect: setting and restoring the password revoked every pre-existing browser session, so
  any previously signed-in browser must sign in again with the unchanged password. No credential,
  password, API key, pairing or network state differs from the pre-deploy pull.
- backup/rollback: `_capsula-mini-deploy-backup/20261113T210514Z-builtin-binary-wire/`
  (`ROLLBACK.txt`, `sha256-current.txt`, `evidence-after.txt`, complete current settings,
  app-settings, boot, Wi-Fi, security and Bluetooth state plus the pre-fix daemon). On-device
  `capsula daemon rollback` restores `a847c805…`.

Still unproven: browser-engine rendering (the WebSocket check used a raw TLS/binary-protobuf
client, not a browser), optical ring/segment output, microphone capture/acoustics, real-peer
Bluetooth audio/pairing, Wi-Fi RF/AP fallback/captive portal, and a power cycle.

### Web UI rewrite daemon deployed (2026-11-14T09:12Z)

The current working tree (commit `9574699` plus its uncommitted changes, including the daisyUI
rewrite in `web/` and `internal/webui/`) was built in `capsula-arm64-builder` with
`make PROFILE=release daemon native GOOS=linux GOARCH=arm64
NATIVE_TARGET=aarch64-unknown-linux-gnu` and a `CGO_ENABLED=1` assistant build. Only the daemon and
CLI differ from the installed stack: the freshly built `libcapsula.so.1.0.0` (`394ed875…`),
`capsula-native.json` (`6294d52a…`) and `dumbassistant` (`ee27cc0a…`) are byte-identical to the
installed files, so the runtime, metadata, interpreters and assistant were not touched. The daemon
embeds `internal/webui/dist/assets/index-7ee84adb.js` and `index-2b554a7d.css`. ELF checks:
daemon/CLI/assistant `GLIBC_2.17`, runtime `GLIBC_2.30`, all ≤ the device's 2.33.

- daemon `91579cb7ab34d670f0b226d1fc436b3cde3a9dc5f6d97b73023ce51d3e4f181c`
  (27,132,164 bytes) installed from `/tmp` with
  `capsula -data-dir /data/capsdemo daemon install -binary /tmp/capsulad.new`; it restarted
  through `/etc/init.d/S99btctl.sh` and passed the health gate. `fce5c506…` is retained as
  `capsulad.prev`; the older `a847c805…` `.prev` was removed only after re-verifying its host
  archive, because `/data` had 8.3 MB free before staging (8.1 MB after). Host and device hashes
  are equal.
- settings are byte-identical to the pre-deploy pull (`settings.json` `9133f829…`, revision 2481,
  same values: clock display provider, assistant default, `word.personal`, live microphone,
  `Asia/Shanghai`, assistant delay `2000-3500`, ring 22:00–07:00). Application settings,
  credentials, certificate, Wi-Fi and pairing state were not written.
- after the restart: `capsulad -health` exit 0, `capsula.clock`, `capsula.ring` and
  `org.capsula.dumbassistant` all `APP_READY`, native A2DP SBC sink registered, daemon RSS
  73.7 MiB. The clock wrote a real frame (`cells=[6 7 6 91]`, brightness 0.05) and the ring wrote
  its explicit all-zero baseline because the current time is outside the stored schedule window.
- Web UI over the LAN IPv4 address: `/healthz` 200, `/` serves the new SPA shell, the served
  `index-7ee84adb.js` (`168501da…`) and `index-2b554a7d.css` (`2b554a7d…`) hashes equal the local
  `internal/webui/dist` files, and `/api/status` returns 401 without a session. No credential was
  changed for proof; the user's own browser at `192.168.1.3` logged in and opened a realtime
  session after the restart, so the new SPA ran against the deployed daemon.
- backup/rollback: `_capsula-mini-deploy-backup/20261114T090954Z-web-rewrite-daemon/`
  (`ROLLBACK.txt`, `sha256-current.txt`, the live `fce5c506…` binary, and tar archives of settings,
  app settings, boot, security, Wi-Fi, `S99btctl.sh`/`wpa_supplicant.conf` and `/var/lib/bluetooth`).
  On-device `capsula daemon rollback` restores `fce5c506…`.

Not proven by this deployment: browser-engine rendering of the rewritten console (only the
auth-free shell/assets and the user's live session), the authenticated API/WebSocket payloads, the
changed Wi-Fi/media/panel paths on hardware, optics/acoustics, real-peer Bluetooth and a power
cycle. The deployed daemon was also built from an uncommitted tree, so the source at the installed
hash must be committed before another rebuild is expected to reproduce it.

### Media transport labels (2026-11-14, source only)

The Overview media bar gates its Play/Pause/Previous/Next buttons on plain `PLAY`, `PAUSE`,
`SKIP_PREVIOUS` and `SKIP_NEXT` labels, but `mediaActions` trimmed a `MEDIA_ACTION_` prefix while
`media.proto` names the contract values `MEDIA_PLAY`…: every snapshot carried `MEDIA_PLAY` and no
transport button rendered for any provider. The prefix is now `MEDIA_`. The corrected labels were
observed over the real dev WebSocket against the `dev.media` fixture
(`PLAY,PAUSE,SKIP_NEXT,SKIP_PREVIOUS`, Play accepted → `PLAYBACK_PLAYING`, Next → `Borealis`), and
`TestSnapshotAndCommands` now asserts the wire spelling. Source only; the unit still runs the
previous daemon.

## Next gates (in order)

1. ~~Boot ownership, in-place upgrades and bounded storage~~ **done and verified on the unit**: the
   daemon starts from the boot hook after two power cycles, `capsula daemon install` replaced the
   binary through the boot service with a health gate, the log is capped, and the corpus moved off
   `/data`. The 2026-11-12 redeploy re-verified the canonical restart and health gate without a
   power-cycle test. Remaining: exercise pairing and unpair with a throwaway device, and write the
   restore procedure for the host-backed corpus and recordings into operator docs.
2. ~~Media-player standard contract~~ **retained for external apps and usable without one**: the
   routed unary/streaming plumbing remains, but the bundled `org.capsula.media` sample provider was
   removed. A Web UI player requires an operator-installed provider; the physical circle/swipes
   control a playing/paused standard provider, then a connected Bluetooth source through the public
   `BluetoothMediaService`, without any app.
3. Verify the native A2DP endpoint by ear with a playing source and compare it with the vendor
   stack; exercise `MediaTransport1.Acquire`, `sbcdec` and the mixer under dropouts. Do not change
   the source's volume/settings from tooling.
4. On a real source, verify the first incoming connection after BlueZ autopowers the adapter before
   `capsulad`, including whether a peer that already abandoned A2DP recovers on the same link; the
   pre-daemon window is a documented limit, not resolved by simulator ordering tests.
5. On the device, save a network name and run `avahi-resolve -n <name>.local`, then restart Avahi
   and repeat; verify collision and invalid-label status in the Web UI. Also watch
   `dbus-monitor --system "interface=org.freedesktop.Avahi.Server,member=SetHostName"` across a
   Settings GET and a daemon restart with the label already active: the fixed code must issue zero
   writes and show no redundant-operation warning. The portable fake, barrier-driven private-bus
   fixture and fixture-backed browser run pass. The fixed binary is now deployed: device startup
   logged no redundant-operation warning, `GetHostNameFqdn` reads back `capsula.local`, and
   `avahi-resolve capsula.local` resolves to the unit. A real write-time collision, invalid-label
   rejection on the device and the Web UI collision rendering remain gates.
6. ~~Observe the deployed Web UI in a browser~~ **observed against a real simulator daemon in
   Chrome headless**: host-name field default, valid save/normalization and restart persistence,
   invalid-label rejection at the HTML and server layers, simulator mDNS-unavailable status with no
   false advertisement, and selected-provider **Restart** labels on Clock, Ring and Diagnostics.
   The corrected mDNS code was then driven through the production Web UI/auth/TLS/settings path
   against a successful responder fixture: zero setter calls and clean “Advertised as
   capsula.local” for startup/GET/same-name save, one call for a real change, a visible collision
   with no advertised claim, and recovery on restoring the label. Remaining: repeat on the Linux
   device; actual `.local` resolution is covered by item 5.
7. Exercise the five-key panel on the unit with the corrected recognizer: a single-key tap resolves
   once after the 250 ms window, a held circle still summons at 700 ms, an overlapping
   edge-to-edge crossing is recognized without stepping volume, sequential opposite taps and
   circle-then-microphone stay deliberate key actions, a 0/1-contact glide is not treated as a
   gesture, full swipes/circles emit the documented gesture, and an unfinished multi-key sequence
   produces no mute/volume/summon. The physical trace taken so far is passive and cannot establish
   contact order.
8. Evaluate the optional assistant's VAD against real room noise and music, including loud
   stationary noise, external music/lyrics and quiet-speech onset under them. The canonical raw
   external-music + full-level-word check at actual levels yields 1 reliable, 1 marginal and
   4 buried/rejected: an accepted music/quiet-speech limit, not an accuracy benchmark. The old
   10 s Mac-music recording is historical consumed evidence, not room-noise or new-location proof;
   loud-noise onset mid-turn, steady-tone false confirmation and music cancellation are documented
   limits, not certified. Simulator PCM cannot certify the acoustic gate.
9. ~~Implement QuickJS and MicroPython host runners~~ **implemented and installed on the unit**:
   separate cgo-free hosts embed real quickjs-ng/MicroPython, load the public C ABI, and run the
   same AppBus contract in the normal sandbox. `make interpreter` builds and stages them;
   `interpreter/smoke/run.sh` proves READY, unary, events, routed calls, stream cancellation and
   stop/crash cleanup on the simulator. Both hosts are installed in `/data/capsdemo/lib`, the unit
   reports them through `ListRuntimes`, and a temporary QuickJS package ran READY under `mountns`
   with a public framework call and clean stop (2026-11-14). Remaining: a MicroPython package run,
   and stronger-than-mount sandboxing (kernel limit).
10. Test the Amlogic stock and Alpine targets on hardware; do not claim parity with Rockchip until
    each subsystem is probed. The backends still fail explicitly.
11. Evaluate the installed personal wakeword model over long real-room audio on its now-default
    processed tap (false activations and misses beyond the 40-minute trial in WAKEWORD.md); its
    threshold was calibrated on raw audio and remains unverified under processing. The settled warm
    classifier replay (own same-session ambient + word + tail) is 6/6 raw and 6/6 processed
    legacy-model usability, not calibration. It is a personal detector, not a
    speaker-independent Russian head.
12. Freeze v1 schemas only after generated Go/Rust SDK and script-host parity tests pass, including
    routed streaming and cancellation.

Gate 1's design decision stands: with no user, PID, network or IPC namespaces and no seccomp on the
stock kernel, the sandbox is mount-only. Either accept that and make provenance verification the
primary control for native apps, or investigate a vendor kernel rebuild — it must be stated, not
implied.
