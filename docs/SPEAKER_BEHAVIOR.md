# Speaker behavior and audit

This audit covers the physical-speaker experience and its control-plane lifecycle: clock/timers/alarms, input, assistant turns, audio, mute/volume/brightness, ring ownership, Bluetooth feedback, and application start/stop. It is not a claim that every repository defect or every hardware variant has been eliminated.

## Interaction contract

- **Idle:** the selected clock/ring provider supplies the baseline. Choosing no provider means a physically blank output, not a frozen last frame, and survives restarts. Only a genuinely new settings store is auto-seeded. An explicit user-selected ring effect resumes after an interruption; it is not forcibly switched off. A ring app active window (`schedule_enabled` with local `HH:MM` bounds) turns the baseline into an explicit blank frame outside those times — for a nightlight or a dark night — while volume, mute, alert and application captures keep priority; the selected effect returns at the next boundary without reopening the presentation.
- **Provider stop/recovery:** a killed, crashed or framework-disconnected selected default provider (display, ring or assistant) is a failure and is restarted immediately; later abnormal exits back off 1/2/4/8 s, and its manifest does not need `restart_on_failure`. Backoff resets only after 30 s of continuous READY, so a ready-then-crash loop keeps backing off. Asking to stop a selected provider is therefore a restart, shown as **Restart** in the Web UI. Deselecting it, choosing **none**, uninstalling it or shutting the daemon down is the durable off switch: selected-origin and forced recovery relaunches re-check selection before starting, regardless of `restart_on_failure`; a never-selected manifest restart keeps its ordinary contract. A manual or wakeword activation after recovery routes to the fresh ready run.
- **Timer:** the nearest running timer appears on the segment display and takes an orange ring capture (RGB 255/96/24). The ring starts full. Pixel 0 is at the top and indices increase clockwise; pixels extinguish in the order 0, N−1, N−2, …, 1. Another application's capture may interrupt it; the current countdown resumes when that capture ends.
- **Timer completion:** the display shows 00:00 with a blinking colon, the ring pulses orange, and the jingle repeats until dismissal. Cancelling a running timer releases its countdown. Timer and alarm alerts do not silently expire.
- **Stopwatch:** a running stopwatch shows elapsed time and holds an orange whole-ring pulse; the nearest timer countdown, when present, takes precedence. A single circle/play-pause or centre/panel tap pauses it and releases the ring so the selected baseline returns; another tap resumes it. The `stopwatch_reset_gesture` clock setting (`hold` by default, or `double_tap`) resets and dismisses it. This stopwatch contract is evaluated before media: while it owns the button a tap pauses/resumes rather than toggling a player. The gesture is consumed by the framework and never summons the assistant; a non-matching hold or double tap still does nothing rather than acting as a summon. Reset returns the display to the clock baseline.
- **Dismissal:** a primary-panel press, circle/talk-key press, neutral centre, or framework-owned tap/hold gesture dismisses sounding alerts. That same interaction cannot also summon the assistant or toggle media. While an assistant turn is in flight, the same keys dismiss that turn through its declared `dismiss` action; the press and its release are consumed, so a second turn cannot start on top of the first. Volume and microphone keys retain their own safety/control functions. Full input capture remains owned by the capturing application, and no framework dismissal, volume, mute, media or summon action runs in that mode.
- **Overlapping alerts:** alerts are handled oldest first rather than playing over each other. Recurring alarms do not enqueue duplicate alerts for an already-ringing source. Disabling/deleting an alarm dismisses its sounding alert. A physical dismissal clears sounding alerts; the service can dismiss one alert by ID.
- **Assistant:** physical talk keys are manual/touch activations, including when hands-free detection is disabled. The Mini talk key is the circle/play-pause key (south); the unmarked centre is neutral and never activates the assistant. A short talk-key press does not summon: it toggles current media. In hold-to-talk mode, a short primary-panel tap does not start an unpaired microphone turn either. The framework carries the per-turn rule on the touch activation (`hold_to_talk`), so an assistant does not read administrator-only device settings; a release is correlated with the activation and assistant that actually received its press, not a later wakeword activation or a newly selected assistant.
- **Panel keys and inferred gestures:** five discrete keys are mapped microphone mute (north), volume up (east), circle/play-pause (south), volume down (west) and neutral centre. Full swipes are opposite → centre → direction (W/C/E right, E/C/W left, S/C/N up, N/C/S down) or the minimum two-contact opposite-edge pair without the centre (W→E right, E→W left), where the first contact must still be down when the second is pressed — a physical overlap; sequential opposite taps and circle-then-microphone stay deliberate key actions, and the vertical two-contact pair is full-capture only. An unambiguous centre-first half swipe is accepted; a closed perimeter traversal from any corner is `circle_cw` (N→E→S→W→N) or `circle_ccw` (reverse). The user's map is authoritative (east volume up, west volume down) and direction is not inverted from a single passive trace. A crossing with zero or one reported contact is indistinguishable from a key press or no input and is not treated as a gesture. Gesture windows and the 700 ms hold use the kernel event timestamp converted to the daemon clock; only an invalid event older than 5 s falls back to receive time. A failed multi-contact sequence suppresses its prefix contacts — no mute, volume step or summon leaks — and only the newest contact may resolve as one single action; a standalone volume tap or held ramp still works. Contacts are deferred by up to 250 ms (1.5 s total budget) so a gesture cannot leak its constituent mute, volume or summon action. The cost is up to 250 ms added latency on a single-key action; the hold threshold uses the same converted press timestamp, and a timed-out prefix resolves exactly one action without duplicating a tap. Partial capture receives circle/centre key events and gestures exclusively, with volume and microphone still framework-owned; full capture receives every key and gesture and all system effects are suppressed. No raw coordinates are fabricated; raw samples are delivered only if the backend has them (`coordinates_available=false` on the Mini, `full_panel_available=true`).
- **Media control:** a short circle tap toggles current media and left/right swipes skip previous/next. Priority after alert/turn dismissal and the stopwatch contract is: a standard provider that is playing, then one that is paused, then a connected Bluetooth source via the public `BluetoothMediaService`, then another standard provider. Bluetooth uses only public BlueZ interfaces and the advertised device; a source that exports no `MediaPlayer1` is driven through that device's public `MediaControl1`, whose missing status is substituted by `MediaPlayer1.Status`, the A2DP transport `active` state, or the sink's 500 ms packet-receipt freshness, in that order. The evidence is read-only, per connection and reset on stop/error/new source; absent evidence sends Play rather than inventing Pause. This works with no media application installed; commands are serialized, and full capture suppresses them.
- **Listening:** teal tracks the framework's processed application microphone tap; blue chase covers the post-speech delay and reply. Speech onset and end are measured in sample time against a learned room-noise floor. An utterance ends after 700 ms in which the 300–3400 Hz band level has fallen clearly below the recent voiced level — a sustained voiced vowel is preserved, not treated as silence — and a reply needs 140 ms of modulated speech-band time or 400 ms of steady voiced level well above the floor. A cold-start guard keeps first speech out of the floor. No-speech turns cancel after the safety window, and incomplete/discontinuous speech must not become an immediate reply. This is a level endpointer, not a speech classifier: a steady loud noise at or above voice level that begins mid-turn is bounded by the 10 s cap and cancelled, a steady hiss/tone held above 2.5× the floor for 400 ms can be answered, and a sustained quiet vowel below that multiple is not confirmed. The canonical raw external-music + full-level-word check at actual input levels recovers 1 reliable, 1 marginal and 4 buried/rejected words — an accepted music/quiet-speech limitation, not an accuracy benchmark. Loud external music or lyrics can open or satisfy a turn, quiet speech can be missed, and there is no automatic music cancellation, AEC or speaker identification. The current activation mode is preserved; no hold-to-talk change is made. Successful playback drains its tail; cancellation discards it. Every exit releases capture, presentation, microphone, and conversation ownership.
- **Audio cues:** a volume step plays the embedded volume cue at the new level; the microphone key plays distinct mute and unmute cues. The startup jingle plays once on installation, then only when `startup_sound_enabled` is on (off by default). Cues are framework playback streams under the master gain: they never block boot or input handling, and each toggle/step produces exactly one cue.
- **Wi-Fi fallback and captive portal:** when no saved Wi-Fi network connects within the bounded startup attempt (or none is saved), the daemon raises the setup AP rather than leaving the speaker unreachable. The AP uses the live Bluetooth adapter alias as its SSID and WPA2 password `capsulad`; the authenticated Web UI shows the active network, known networks with Forget, and scans only when the user presses Scan. The network carrying the console session is never offered for Forget and there is no remote disconnect command; forgetting is limited to inactive saved networks after an explicit confirmation. The portal at `192.168.4.1` exposes only Wi-Fi status, Scan and Connect — no administrator login, device settings, app control or system mutation. It accepts requests only from the AP subnet, with Host/Origin checks and an HMAC token bound to the client address and valid for 15 minutes (reusable within that window, not single-use); it has no script execution. Existing stock `wpa_supplicant` networks are imported once on first use, and if the stock file cannot be read while the private store is empty the daemon leaves the stock association alone and reports an error. Scan is capability-driven: it can be unavailable in AP mode until a fallback scanner exists, and the UI then disables Scan while manual SSID entry remains. A successful connection tears the AP down and the Web UI updates to the joined network; a failed station connection restores the previous/saved association or raises the setup AP boundedly and shows the original error. Joining an AP does not imply that a phone's captive-browser detection or the stock driver will work; that remains a hardware gate.
- **Web UI live controls:** the authenticated console is a fresh daisyUI appliance UI with four
  destinations (Main, Settings, Access, Diagnostics), a top navigation bar and an application
  detail/settings dialog; Settings has section anchors and a filter with a no-results state. Selects,
  sliders, checkboxes and the microphone gate apply immediately after local validation as
  field-scoped updates (display default app/brightness/brightness mode; assistant app and touch
  behaviour; microphone mute; muted-indicator mode/delay; volume; startup sound; timezone;
  hostname) through one serialized compare-and-swap queue with one in-flight request per group and
  coalescing of rapid slider input. The microphone control is positive: ON means enabled, shown as
  “Microphone on/off”, with the accessible name “Enable microphone”. A visible pending state
  reports in-flight work; a failed update shows the error and re-reads the snapshot.
  External/hardware changes update the controls live.
  Text and selection drafts (timezone, network name, Wi-Fi passphrase) remain local until their
  explicit Save/Apply; a neighbouring live control never submits or validates them, and an incoming
  push never overwrites them. The muted-indication hide delay offers “Never (keep the light
  visible)” (the stored `none` mode: keep the indication until unmuted, the default), 1 minute,
  10 minutes and 30 minutes; a stored non-preset mode or delay is shown as a labelled current
  setting and loading the page never rewrites it. Timezone opens with the current setting, a
  phone/computer suggestion and UTC; a bounded city/region list appears only after a search, the
  browser zone is only a suggestion applied by an explicit Apply, and raw zone identifiers or
  firmware defaults stay under Technical details. Provider dropdowns are populated from manifest
  `provides` hints (`voice_assistant`, `segment_display_provider`, `led_ring_controller`) and always
  keep the explicit none choice; a saved app that is no longer eligible or installed remains the
  selected option, clearly marked, and is never rewritten automatically. Touch choices read Hold to
  summon (the assistant decides when speech has ended) and Hold to talk (release ends listening).
  DNS/mDNS and interface identifiers for the speaker name and Wi-Fi sit behind Technical details.
  The Wi-Fi console never scans on open, protects the active network from Forget/disconnect, and
  names the setup AP and its `capsulad` password. Diagnostics host metrics are read on Linux only: an
  unsupported host shows an explicit reason instead of zero measurements, and each domain is
  independently available. Partial log/metric frames never suppress an older-reserved full snapshot:
  full fields and metrics keep independent watermarks, logs are unioned, deduplicated, sorted and
  capped at 400 entries (200 while paused), and a within-connection resync never discards newer
  streamed entries; reconnecting starts a new monotonic epoch, clears the log and paused buffers and
  preserves the pause choice. The wakeword area embeds guided training and the real last 20 engine
  detections; manual, simulator and API activity are not detections. The custom app-settings frame
  follows the same draft and field-mask rule inside its own page.
- **Brightness:** changing global brightness affects static frames too. Automatic mode retains the visible floor and discards stale cache entries when its mode changes. Manual zero remains explicit off.
- **Bluetooth:** connect shows three blue ring pulses and a two-second `bt` display. Disconnect shows exactly one blue pulse and no glyph; the selected display baseline is never covered by a disconnect notice. The native A2DP sink registration is verified for the current adapter generation before the adapter can be made connectable or discoverable; a lost registration is repaired before the next connect, and a failed one is reported as not ready rather than advertised. Enabling incoming connections or discoverability is refused while the audio profile is not advertised. **Incoming connections** off denies new inbound BR/EDR links, including from paired devices, through the public management API's connectable state; the adapter and BLE radio stay powered, and links already connected are not torn down. **Discoverable** on implies incoming connections and permits new devices to find, connect and pair; incoming off forces discoverable off. A source already routed through PulseAudio is not torn down to switch owners; the switch completes after it disconnects. If BlueZ autopowered the adapter before the daemon started, a paired source may already have opened an ACL in that window; the connect handler repairs the sink and requests the profiles over the same link, but a peer that already abandoned A2DP is device-dependent and a hardware gate. A connected audio source remains present without a player object. Disconnecting another device must not stop the active source. Slow event consumers resynchronize against current state rather than retaining a lost disconnect forever. One active native source remains the supported model.

## Privacy and microphone interaction

The speaker treats supervised applications and authenticated Web UI automation as untrusted for
private data, even when they hold a valid API key or a supervised run identity. The daemon has one
local gRPC listener on the Unix socket; there is no network gRPC listener. The Web UI is a local
administrator client of that socket, but its authenticated browser/API-key surface is not a second
privilege plane: API keys remain limited to the read-only status/certificate endpoints and never
reach recording, diagnostics, or credential-mutating routes.

- **Microphone indication (visible and audible).** A supervised app receives microphone audio only
  while both halves of the indication hold: a selected, visible, non-blank ring/segment presentation
  for that app at non-zero effective brightness, and the framework microphone-live cue that the
  mixer actually consumed and drained to the output sink. The cue is the exclusive mixer source
  while it runs, so an application cannot mask it with concurrent audio; other playbacks are frozen,
  not stopped, and resume afterwards. A zero master volume, the global mute, a missing output
  device, an unselected/blank presentation, or a cue the mixer cannot open or drain denies the
  capture instead of recording silently. Display brightness is part of the indication; manual zero
  brightness is off, not hidden recording.
- **Continuous delivery checks.** Both halves are re-evaluated for every audio chunk, not just at
  open: losing the visible indication or the master volume stops frame delivery immediately.
  Chunks withheld during the watchdog grace are counted as dropped and the next delivered chunk
  carries the discontinuity; the grace only bounds how long the capture remains alive before the
  `WATCHDOG_EXPIRED` notice terminates it. An invisible blank presentation never qualifies.
- **Pre-roll belongs to the activation.** The one-second pre-roll is attached only when the supplied
  activation ID was issued by the framework to that same app, is still inside its delivery window,
  and has not been used before. An arbitrary activation ID yields live, indicated capture only; it
  cannot replay audio from around another app's turn. Microphone capture is still per-app scoped and
  released on stream end, dismissal, crash, mute, watchdog expiry, or daemon shutdown.
- **Enrollment feedback.** Wakeword takes are a local-administrator UI operation, never app-facing.
  Every take (word, phrase, and room/background) opens the red ring recording pulse and plays the
  audible start cue before capture begins; if the ring cannot be selected/rendered or the cue cannot
  be delivered, recording fails instead of proceeding silently. The browser receives samples and
  status only, never PCM or the stored takes.
- **Administrator-only surfaces.** Device settings (including hostname, volume and microphone mute),
  device-wide diagnostics and log history, forced capture release, wakeword enrollment/training,
  Bluetooth incoming-connection/discoverable policy and unpair, application launch/stop, and
  foreign-app configuration are local-administrator operations. A supervised app that omits its run
  ID still
  identifies as an app through its sandbox UID or supervised PID; it cannot become an administrator
  by silence. Per-app configuration, actions and run listings are scoped to the calling app.
- **Foreign data stays foreign.** Apps do not read another app's runs, settings or logs; the
  Bluetooth media view redacts the connected peer address/name for app callers while keeping the
  transport/now-playing fields a media provider needs; the shared asset catalog exposes only
  normalized `/assets/...` paths for regular, non-symlink files inside the asset root. The built-in
  clock and ring apps publish ordinary discoverable contracts through `AppInterfaceService`; a
  supervised app may call them through the app bus like any other declared interface, but cannot
  claim an in-process system-app run, spoof another caller, or reach an administrator operation.
- **Bluetooth scanning.** There is no BLE scan RPC. Apps cannot change the persisted Bluetooth
  incoming-connection or discoverable policy, so a consumer cannot both override the user's
  exposure choice and initiate scanning; adapter exposure remains an administrator action with the
  existing audio-profile-readiness invariant.
- **Captive portal is not a second admin surface.** The unauthenticated portal has only Wi-Fi
  status, Scan and Connect. It is bound to the AP address/subnet, rejects a foreign Host/Origin,
  requires an HMAC token bound to the client address and valid for 15 minutes for POST, runs no script and sets a no-store/no-referrer
  policy. It cannot read the admin session, cookies, WebSocket, app settings, logs, credentials or
  system controls, and it is stopped when the AP is torn down. Wi-Fi passphrases are used to derive
  the PSK and are never returned; the derived key lives in a 0600 daemon store.

Honest limits: the audible cue proves the mixer consumed a cue at the selected master gain and
wrote it to the configured output sink; it does not measure perceived loudness, and audio already
buffered in the hardware/PulseAudio sink or a full-scale concurrent source already in that buffer
can still overlap it. Mixer-level exclusivity cannot reach into the sink's own queue. Visible
brightness is a digital/back-end statement; photons, gamma and panel failure are not measured. A
root/physical compromise or `-sandbox none` development run is outside this cooperative model.
Zero brightness and zero volume are treated as denial, not as an opportunity to record quietly.



## Future-change gate

Use this gate for behavior changes and polishing/review work, not only for reported bugs. State the intended interaction in task notes or tests before editing; update this document when that contract changes. A user's examples are starting evidence, not an exhaustive list of affected transitions. Apply the relevant rows rather than requiring unrelated hardware exercises for a documentation-only change.

| Transition | Questions to resolve | Proof to collect |
|---|---|---|
| Idle → start | What input starts it? Is there a hold threshold? What immediate feedback tells the user it worked? | Exercise the real handler/API path and observe the first selected/rendered output. |
| Active/progress | What does each light, glyph, and sound mean? Is progress direction/geometry correct? Are volume/brightness and safety controls respected? | Inspect frames/PCM and time-based progress, including static content with changing settings. |
| Active → interrupted | What happens when another capture, activation, settings change, or device event arrives? | Prove preemption and correct resumption; an older completion must not modify a newer owner. |
| Complete → dismiss | Does an alert repeat until its documented stop condition? Which physical action dismisses it? Is the matching release consumed? | Observe repetition, then drive dismissal through input and verify that it does not also activate another action. |
| Finish/cancel/error → baseline | Which streams, captures, presentations, timers, and conversations must end? What replaces them? | Check resource cleanup plus the next successful backend output; test normal audio drain separately from abort. |
| Stop/crash/disconnect → recover | Can an old callback or lost event leave a phantom owner or stale route? Can the feature be used again? | Exercise an abnormal exit and a fresh interaction; verify terminal events and convergence to current state. |
| Save → restart/upgrade | Which user choices and in-flight state must persist? Are all affected package/settings versions compatible? | Restart through the supported service when authorized; re-read choices and exercise the deployed package, not merely its API health. |

For each relevant path, name the expected terminal owner/output and the allowed time to reach it. Distinguish deliberate product limits from missing behavior. Do not silently introduce a timeout, change a gesture, or reset user preferences merely to make a scenario terminate.

When reviewing in parallel, partition implementation ownership and share this contract first. Cross-component integration belongs to one coordinator; agent reports are leads until their evidence and observable outcomes have been checked. Build/test the integrated change rather than independently declaring each subsystem polished.


## Confirmed defects repaired

| Boundary | Defects |
|---|---|
| Physical output | No-owner selection left old LEDs/display lit; ring write deduplication ignored brightness; same-app presentation changes were missing from routing logs. |
| Clock | One-shot alert audio; premature expiry; no countdown ring; blank digit masks instead of 00:00 at completion; recurring alert duplication; disable/delete orphaned alerts; blinking digits reset the ring pulse phase; stopwatch owned no ring and accepted no physical control. |
| Input/assistant | No physical alert-dismiss path; dismissal could also activate/release a turn; duplicate/stray key events and ownership changes mishandled held controls; physical keys were labelled as wakewords; later activations overwrote release identity; silent hold turns missed the no-speech limit; empty activation IDs matched an empty release sentinel; an active assistant turn could not be dismissed by any physical control; taps on the talk key had no framework meaning. The unmarked centre key was treated as a voice/assistant key; short taps did not control media; partial capture could not receive circle/centre or gestures while full capture required raw coordinates; gesture prefixes could leak mute/volume/summon effects or drop a legitimate tap. Current contract: neutral centre never summons, short tap toggles media, partial/full ownership follows panel capability rather than coordinates, and contacts defer up to 250 ms so a swipe/circle is recognized once. |
| Audio | Cancelled RPCs retained mixer streams/queued audio; paused playback drained into an unbounded resampler queue; queue accounting raced publication; exact-ratio resampling could strand a final sample; cancelling enrollment countdowns left their audio playing. |
| Feedback/settings | Restarting re-enabled providers despite saved empty/absent selections; timezone changes stopped virtual clock runs and erased timer/stopwatch state; stale OSD/mute callbacks affected newer feedback; live mute policy changes were ignored; automatic-brightness mode reused an old level; brightness-mode maps escaped settings snapshots; ring startup could block the output loop or consume a signature without a presentation. |
| Ownership/diagnostics | Closing presentations retained phantom captures; capture RPCs outlived released ownership; invisible content counted toward the microphone-indication watchdog; untouched resources appeared with empty IDs; terminal run events disappeared after run removal; launch could report success during an in-progress built-in stop. |
| Privacy/authorization | App captures received audio without an audible cue and could pass an arbitrary activation ID to replay the pre-roll; enrollment room takes recorded without an audible cue and continued when the ring indication failed; unauthenticated force-release; settings, device-wide diagnostics/logs, wakeword takes/training, Bluetooth exposure changes and app launch/stop were app-callable; apps could read/update foreign app configuration and logs; Bluetooth media state exposed the peer MAC and now-playing data to apps; a supervised package could squat the framework identity or claim a built-in run; the app bus accepted a routed call with a spoofed caller run ID; asset manifests could publish traversal/symlink paths. |
| Bluetooth | Snapshot/subscription gap; overflow lost terminal changes; absent player objects hid connected sources; unordered player choice targeted the wrong source; disconnect omitted `bt`; unrelated-source disconnect stopped playback; a late route load could resurrect a disconnected route; pairing bypassed audio-profile advertisement. Connect/disconnect notices were symmetric (a disconnect covered the display with `bt` and pulsed three times); sequential profile connects added ~3 s before audio; PulseAudio Bluetooth modules were unloaded on every connect, which could tear down a source with no audio route; a lost endpoint registration was not repaired before connecting. Readiness is now generation-scoped and verified before pairing/discoverability/API requests; PA endpoints are not removed while a source is actively routed; a connected source keeps its PA route until disconnect. |

## Text-observable hardware behavior

Normal logs now include routing identity changes and **successful backend writes**, including presentation/capture IDs, global brightness, segment masks/colon, and ring RGB pixels. Failed driver writes are logged as failures, not successful output. Clock logs include countdown pixel counts and alert/audio lifecycle; input logs identify handled, consumed, and cancelled controls.

For an animated trace on the deployed Rockchip service:

```sh
CAPSULA_OUTPUT_TRACE=1 /etc/init.d/S99btctl.sh restart
tail -f /data/capsdemo/capsulad.log
# End high-rate tracing after the exercise:
/etc/init.d/S99btctl.sh restart
```

For a direct daemon launch use `-output-trace`. Animation samples are limited to four successful writes per second per resource; ownership/brightness transitions are recorded separately. Logs retain the existing 512 KiB rotation with one backup. Traces contain output/control metadata, not microphone PCM.

A released ring with no baseline must be followed by a successful all-zero ring write. A countdown must have a contiguous orange arc whose extinguished prefix follows the counterclockwise order. Audio open/close records must agree with diagnostic active streams. These checks assess software decisions and driver submission; they do **not** measure emitted photons, perceived loudness, microphone acoustics, or firmware behavior outside the framework.

## Automated verification

Race-enabled checks exercised `internal/audio`, `internal/input`, `internal/routing`, `internal/settings`, `internal/bluetooth`, `internal/discovery`, the assistant module, and the affected daemon integration scenarios. Coverage includes timer persistence/repetition/dismissal, countdown geometry/preemption, stopwatch orange pulse and tap/hold/double-tap arbitration, actual backend blanking, static-frame brightness, visible-indication rejection, cancelled/finite playback, held input and intervening activation IDs, assistant dismissal through the circle key, restart after a lost framework connection, level-fallaway speech endpointer with colored/steady-noise and sustained-vowel regressions, mute/OSD generations, timezone preservation, Bluetooth overflow recovery and the single-pulse disconnect notice, terminal lifecycle events, selected-provider restart regardless of `restart_on_failure` with deselection during stop/backoff, five-key mapping and swipe/circle inference (including the two-contact opposite pair and failed-prefix suppression) with deferred delivery, standard-provider and Bluetooth-fallback media control with MediaControl1/transport/receipt direction evidence, 16 kHz capture DSP chunk-invariance/reset and tap-declaration rejection, BlueZ registration ordering/generation readiness, and mDNS validation plus idempotent apply/skip-write and host-name Web UI wording. These are unit/simulator checks; device and browser gates are listed in [STATUS.md](STATUS.md). The 2026-11-13 integrated focused run on darwin/arm64 (`-ldflags '-linkmode=external'`, real `build/lib` native runtime) passed the input/backend/wakeword/settings/discovery/audio race suites, the three focused supervisor policy tests, seven Bluetooth registration/rearm tests, focused Web UI hostname/restart checks, seven daemon lifecycle paths, eight daemon Bluetooth ordering/readiness paths, the three final media tests and all eleven `TestDumbAssistant` scenarios; one race-only fixture deadline was corrected (2 s → 20 s drain for a 120 s burst) without changing burst/drop/backpressure behavior. Two lifecycle edges found after that pass were repaired: backoff now resets only after 30 s of continuous READY, and selected-origin/forced recovery re-checks selection regardless of `restart_on_failure`; the controlled-clock regressions and full `-race` supervisor suite pass, and the integrated daemon lifecycle recovery set passes 7/7 against the real native runtime.

The integration assertions observe asynchronous backend updates rather than assuming a gRPC send is an acknowledgement. The audio-gap scenario pauses the real assistant process and overflows its actual transport with voiced PCM; a fast consumer receiving an intact silence burst is not falsely treated as a dropped-audio test.

For the fresh daisyUI console, the frontend's 11/11 node tests passed; the final Vite production
build, the embedded host daemon build and the Linux arm64 crossbuild passed (after all patches);
`go test -race ./internal/webui` passed (22.672 s); the wakeword real-origin last-20 history focused
`-race` test passed; the daemon install precommit/commit-winning cancellation plus install/train
focused `-race` tests passed (2.045 s with a matching host native runtime); and the targeted
`TestTraining` `-race` run (4.581 s) confirms that neither a local nor a generic cancel claims the
installed model is unchanged; only an explicit daemon precommit status does. A native Tern browser
through a throwaway loopback HTTP→TLS/WebSocket proxy exercised the 390 px and 849 px surfaces:
login with live startup and no reload, live microphone changes plus an independent-client push
preserving an unsaved name, timezone city search with explicit Apply, the Clock app's
`allow-scripts` settings document ready with a blink patch persisted via the public CLI, API-key
create/clear/revoke, password change with reconnect, an authenticated certificate PEM 200
attachment, Settings filter no-results, wakeword room-take start and cancel with simulator routing
restored to its default baseline and no captures, diagnostics pause 10 new/resume 27 logs with
filtering, and a final main-toolbar Bluetooth/media/volume/microphone pass before the apps at 849 px. Limits: the managed
WebKit build crashed in EGL and Chromium failed its Mach-port launch; a direct Tern load of the
self-signed TLS endpoint with `insecure:true` was still refused ("Unacceptable TLS certificate") and
certificate validation was not weakened, so the proxy run remains the browser proof; Tern ignored
resize emulation, so the viewport was pane-driven rather than a full 1440 px desktop or physical
phone. All temporary smoke services were stopped and the proxy script deleted. No real Wi-Fi or
Bluetooth peer, physical temperature, acoustics, full training-quality run or deployment was
exercised. Wakeword training keeps its single personal-model slot; a cancel that races the final
atomic commit may still install the new model.

## Rockchip speaker verification

The daemon, assistant 0.2.0, and canonical boot hook were deployed together. The previously installed assistant was 0.1.0 despite the newer daemon. Its old random-delay fields were converted once to `delay="2000-3500"`; the selected assistant, clock, explicit no-ring baseline, wakeword model, and other user settings were retained.

The first hardware check exposed the provider re-seeding bug above; it was fixed and the original no-ring choice restored before continuing. Input exercises inject the real device's Linux evdev codes, not simulator admin events.

| Scenario | Observed evidence |
|---|---|
| Countdown direction and geometry | Successful driver frames contained 12, 11, …, 1 orange pixels in the specified counterclockwise order. |
| Completion and repetition | Driver segment masks were `[63 63 63 63]` (00:00); eight clock playback opens were retained in the trace; the alert remained active beyond the former ten-second expiry. |
| Digital jingle output | The speaker's `master.monitor` capture contained three repeated three-beep groups; the final half-second after dismissal was all-zero PCM. This measures mixer output, not acoustic loudness. |
| Physical dismissal | A one-second circle-key press dismissed the alert, stopped clock audio, and did not summon the assistant. |
| Cancellation and stopwatch | Cancelling a countdown released the ring; stopwatch start/pause/resume/reset preserved the expected elapsed-time transitions. |
| Assistant lifecycle | Both summon and hold-to-talk modes exercised the actual play/pause-key path (the historical log called it the voice-key path; on the Mini that unmarked key is the neutral centre); the summon opened real microphone capture; completed turns left no assistant audio streams or ring ownership. |
| Physical microphone key | GPIO 21 read 0 after mute and 1 after unmute. No microphone/acoustic recording was required for this check. |
| Volume | A physical tap increased volume once and did not continue repeating after release; original volume was restored. |
| Brightness submission | Successful ring-driver writes included global levels 0.25 and 0.8 during mute indication; the user's original level was restored. |
| Final output | A successful no-owner ring write contained twelve all-zero RGB pixels, rather than merely a routing-release message. |
| Restart and restoration | A final normal service restart retained the original provider selections and settings, with the assistant ready and no active app audio. The kernel boot ID was unchanged. High-rate tracing was disabled after the exercise. |

Local evidence and rollback material are retained under `build/speaker-audit/`: scenario results, rendered-output logs/assertions, target binary hashes, and the pre-deployment daemon/package/settings/boot-hook backup. The previous redundant `S60capsulad` service is retained off the boot path on the device; `S99btctl.sh` is now the single canonical service.

These exercises do not certify acoustic recognition thresholds, subjective LED brightness/gamma, long-room wakeword false-positive rates, real Bluetooth re-pairing/source-device behavior, or unavailable hardware variants. Bluetooth changes were exercised with the simulator and real manager state-machine tests; no user pairing was removed or changed. The five-key gesture inference (including the overlap-only two-contact pair, vertical full-capture
pair, preserved sequential taps and failed-prefix suppression), kernel-timestamp windows, 250 ms
deferred delivery, tap/swipe media control with the `MediaControl1`/transport/receipt fallback,
mDNS apply idempotence and the revised processed-tap endpointer have local unit/fixture coverage;
the final host race pass covered the audio drop/frame/lifecycle subset and all nested-assistant
tests, and the real daemon passed all 14 assistant scenarios. None of these follow-up fixes is
deployed, so device/acoustic checks remain gates. Passive device traces showed event shape, not
transport success or contact order. A Chrome-headless browser session against a real simulator
daemon observed the host-name default/save/restart-persistence, invalid-label rejection, no false
mDNS advertisement claim, and the **Restart** labels on Clock, Ring and Diagnostics; a real
Web UI/auth/TLS/settings browser run against a successful responder fixture exercised the corrected
apply (zero setter calls for startup, GET and same-name save; one for a real change; genuine
collision with no advertised claim; recovery). Actual Avahi `.local` resolution/collision behavior
on the device and deployment of the fixed binary remain gates. Historical log labels calling the
unmarked centre a "voice key" are retained as dated evidence, not as the current mapping.
