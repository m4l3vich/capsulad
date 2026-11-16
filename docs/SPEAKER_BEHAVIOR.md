# Speaker behavior

The physical-speaker contract of Capsula Mini: clock/timers/alarms, input, assistant turns, audio,
mute/volume/brightness, ring ownership, Bluetooth feedback, and app start/stop. It binds every layer
from routing to driver submission; acoustics and photons are separate, explicitly marked gates.

## Interaction contract

- **Idle baseline.** The selected clock/ring provider supplies the baseline. Choosing no provider
  means a physically blank output — an explicit all-zero frame, not a frozen last frame — and
  survives restarts; only a genuinely new settings store is auto-seeded. An explicitly selected
  ring effect resumes after an interruption and is never forcibly switched off. A ring app active
  window (`schedule_enabled` with local `HH:MM` bounds) turns the baseline into an explicit blank
  frame outside those hours while volume, mute, alert and application captures keep priority; the
  selected effect returns at the next boundary without reopening the presentation.
- **Provider stop and recovery.** A killed, crashed or framework-disconnected selected default
  provider (display, ring or assistant) is restarted immediately; later abnormal exits back off
  1/2/4/8 s and the manifest does not need `restart_on_failure`. Backoff resets only after 30 s of
  continuous READY, so a ready-then-crash loop keeps backing off. "Stop" on a selected provider is
  therefore a restart, shown as **Restart** in the Web UI. Deselecting it, choosing **none**,
  uninstalling it or shutting the daemon down is the durable off switch: selected-origin and forced
  recovery relaunches re-check selection before starting, regardless of `restart_on_failure`; a
  never-selected manifest restart keeps its ordinary contract. A manual or wakeword activation after
  recovery routes to the fresh ready run.
- **Timer.** The nearest running timer appears on the segment display and takes an orange ring
  capture (RGB 255/96/24). The ring starts full; pixel 0 is at the top, indices increase clockwise,
  and pixels extinguish in the order 0, N−1, N−2, …, 1. Another application's capture may interrupt
  it; the countdown resumes when that capture ends. Cancelling releases the countdown.
- **Timer completion.** 00:00 with a blinking colon, an orange ring pulse, and a jingle that repeats
  until dismissal. Timer and alarm alerts do not silently expire; the clock's blinking digits never
  reset the ring pulse phase.
- **Stopwatch.** A running stopwatch shows elapsed time and holds an orange whole-ring pulse; the
  nearest timer countdown takes precedence. A single circle/play-pause or centre/panel tap pauses it
  and releases the ring to the selected baseline; another tap resumes. The `stopwatch_reset_gesture`
  clock setting (`hold` by default, or `double_tap`) resets and dismisses it. This contract is
  evaluated before media: while the stopwatch owns the button a tap pauses/resumes rather than
  toggling a player, the gesture is consumed by the framework and never summons the assistant, and a
  non-matching hold or double tap does nothing rather than acting as a summon. Reset returns the
  display to the clock baseline.
- **Dismissal.** A primary-panel press, circle/talk-key press, neutral centre, or framework-owned
  tap/hold gesture dismisses sounding alerts, and that same interaction cannot also summon the
  assistant or toggle media. While a turn is in flight the same keys dismiss it through its declared
  `dismiss` action, resolving the selected assistant or the bound app that owns the capture,
  playback or ring resources; the press and its release are consumed, so a second turn cannot
  start on top of the first. Volume and microphone keys keep their safety/control functions. Full
  input capture belongs to the capturing app and no framework dismissal, volume, mute, media or
  summon action runs in that mode.
- **Overlapping alerts.** Handled oldest first rather than played over each other. A recurring alarm
  does not enqueue a duplicate alert for an already-ringing source; disabling or deleting an alarm
  dismisses its sounding alert. A physical dismissal clears sounding alerts; the service can dismiss
  one alert by ID.
- **Assistant.** Physical talk keys are manual/touch activations, including when hands-free detection
  is disabled. The Mini talk key is circle/play-pause (south); the unmarked centre is neutral and
  never activates the assistant. A short talk-key press does not summon — it toggles current media.
  In hold-to-talk mode a short primary-panel tap does not start an unpaired microphone turn either.
  The per-turn rule travels on the touch activation (`hold_to_talk`), so an assistant never reads
  administrator-only device settings, and a release is correlated with the activation and assistant
  that received its press, never a later wakeword activation or a newly selected assistant. A turn
  that ends with no recognised text and no command is reported as a no-answer with the terminal
  error indication, never as a completed turn; a turn whose session the service rejects says so,
  drops the credential and re-resolves it from the app's own settings before the next turn.
- **Wakewords and app binding.** A wake word can be bound to a specific app. Saying a bound word
  starts that app's turn and, if the app is not ready, the activation is dropped with an explicit
  reason and a log entry — the device does not reroute the turn to the selected assistant or to
  another word, and saying a different word is the user's recovery path. Unbound words and the talk
  key target the selected assistant; an API activation targets whatever app the named word is bound
  to, exactly like a spoken detection, and an activation whose target app is not ready is dropped
  with an explicit reason rather than rerouted. The microphone-muted switch and the global detection
  switch still gate every word; a word's own switch pauses only that word. The
  circle/play-pause dismissal resolves the app that currently owns the capture, playback or ring
  resources among the selected assistant and the bound apps.
  A newly created wakeword is installed switched off and becomes live only when the operator enables
  it and (optionally) binds an app; retraining an existing word keeps its enabled state and binding.
  Installing, retraining or deleting any word restarts detection for all words, so the device is
  deaf to every wakeword for about two seconds — the setup dialog says so before it closes. At most
  eight wake words can be installed: training a ninth is refused before the fit starts with an
  actionable message, so the operator deletes a word first. Deleting a word never deletes the
  recordings, which are shared training data.
- **Panel keys and gestures.** Microphone mute (north), volume up (east), circle/play-pause (south),
  volume down (west), neutral centre. The panel is one `gpio-keys` node advertising `EV_SYN|EV_KEY`
  only (`EV=3`, no `EV_ABS`), so no coordinate ever exists and every gesture is inferred from
  ordered key edges. Raw `/dev/input/event0` captures measured on the unit show that a finger swipe
  over the whole panel emits **exactly one** press + SYN + release + SYN: full-panel up reports
  `volume_down` (0x72), right `mic_toggle` (0xf8), down `mic_toggle` and left `volume_up` (0x73),
  while a swipe anchored at the physical centre can report the centre key (0x2c0 or 0x246) instead.
  A bare volume edge is therefore indistinguishable from a deliberate volume tap, so the **centre
  key is the measured gesture evidence on a single contact**; a whole-pad crossing that reports only
  its two end-stop edges is recognised from their order and timing.
  **No contact is ever deferred.** Every press and release resolves its own action on its own edge,
  so a lone volume tap has zero added latency and a release stops the volume ramp as before. Three
  gesture forms are recognised:
  - *centre gate*: a `center` press followed by a `volume_up`/`volume_down` press → `swipe_right` /
    `swipe_left`. The panel reports one key at a time, so the usable form is a **centre tap followed
    by an edge within 700 ms of the centre's release**; a centre that is still held when the edge is
    pressed needs no press-gap allowance (only the 1500 ms total gesture budget applies). Only that
    edge is suppressed; the centre keeps its own action, and the gate costs no volume latency because
    nothing waits for it.
  - *full-panel crossing*: `volume_down` then `volume_up`, or `volume_up` then `volume_down`, with
    **no centre between them** and a press-to-press gap of at most 400 ms → `swipe_right` (west →
    east, next) / `swipe_left` (east → west, previous). The first edge keeps its immediate action;
    when the second edge resolves the gesture it is suppressed and the leading step is **reverted**
    to the master level captured immediately before it, through the same canonical volume path. The
    step is audible only for the length of the crossing, and a slow opposite pair (> 400 ms) or a
    lone tap is unchanged: two deliberate steps or one step, with no deferral and no revert.
    A crossing leaves **no volume side effect**: the volume overlay is closed rather than flashed for
    a level the gesture takes back, and the source's mirrored level never carries the transient step,
    because the mirror coalescer (450 ms) outlasts the 400 ms crossing window and only the restored
    level is written. This is the field report "a full-panel swipe triggers the volume control on
    both sides" turned into a rule.
  - *chain*: opposite → centre → direction (`W,C,E` right; `E,C,W` left; `S,C,N` up; `N,C,S` down)
    with each press gap at most 250 ms and at most 1500 ms in total. Only the direction contact is
    suppressed — because nothing is deferred, the leading opposite edge has **already** produced
    its own volume step or media toggle by the time the gesture resolves.
  The panel reports one key at a time, so the measured centre-anchored form is a **centre tap
  followed by an edge inside 700 ms of the centre's release**. A whole-panel swipe that reports one
  end-stop key per contact is the full-panel crossing above: the pair is accepted only inside the
  400 ms press window, and its leading step is reverted rather than held back. Everything else is
  plain keys, immediately: a bare volume edge is one step, opposite edges more than 400 ms apart are
  two steps, and a vertical pair or a perimeter traversal without the centre keeps its individual
  actions (the transport key's 700 ms hold still applies). The retained device log contains a real
  centre → edge contact pair — `center p+0ms r+115ms | gap 14ms volume_down p+130ms down`, a 130 ms
  press-to-press distance well inside the centre window — and the same log's previous heuristic
  scored a `volume_down … gap 123ms volume_up` pair as `swipe_right`: that 123 ms pair is now the
  full-panel crossing, with the leading step reverted. The `swipe_half.hex` capture reports one
  gesture as `mic_toggle` (`0xf8`) and the next as the centre key (`0x2c0`), and the `swipe_full.hex`
  capture holds no centre contact at all — a whole-panel crossing that emits only one key per
  gesture cannot be told from two quick opposite taps by its endpoints alone, which is why the
  400 ms window is the contract and why a lone end-stop key stays a volume tap. Circles
  (`circle_cw`/`circle_ccw`, one closed perimeter traversal
  from any key with each contact beginning while the previous is still down) are recognised **in
  full capture only**; a partial capture must keep its individual contacts, so the framework never
  turns a perimeter press into a circle. Recognition is a bounded shadow of at most five contacts:
  a press more than 250 ms after the previous one (400 ms for the opposite volume edge of a
  full-panel crossing), or a sixth contact, abandons it and starts fresh — except a volume edge
  that follows a centre tap, which has the 700 ms released window —
  and nothing is ever replayed. The shadow also carries the pre-step level of a volume contact, so
  the full-panel revert restores exactly one leading step and is cleared with the sequence.
  The user's map is authoritative (east volume up, west volume down) and direction is never inverted
  from a single passive trace. Gesture windows and the 700 ms hold use the kernel event timestamp
  converted to the daemon clock; only an invalid event older than 5 s falls back to receive time.
  An abandoned sequence produces no gesture and suppresses nothing, while a recognised gesture
  suppresses only its completing contact; a standalone volume tap or held ramp still works. Partial
  capture receives `play_pause`/`center` keys and recognized swipes, with volume and microphone
  framework-owned; full capture additionally shadow-recognizes circles while forwarding every key,
  with all system effects suppressed. Raw coordinates are delivered only when the backend
  advertises them (`coordinates_available=false` on the Mini, `full_panel_available=true`).
- **Media control.** A short circle tap toggles current media and left/right swipes skip
  previous/next. Priority after alert/turn dismissal and the stopwatch contract: a standard provider
  that is playing, then one that is paused, then a connected Bluetooth source via the public
  `BluetoothMediaService`, then another standard provider. No Bluetooth provider app is bundled and
  no app id is reserved for one: when no media app is playing or paused — the normal case for a
  connected phone — the circle key and swipes drive the source through that public service, and the
  Web UI "Now playing" panel lists the same source from the same service as a framework-owned
  provider with its PLAY/PAUSE/SKIP buttons mapped onto the same commands. Bluetooth uses
  only public BlueZ interfaces and the advertised device; a source without `MediaPlayer1` is driven
  through its public `MediaControl1`. A toggle's direction comes from the strongest available
  evidence, in this order: a direction this daemon commanded in the last two seconds (the next
  toggle inverts it, because the peer's notification has not arrived yet), then — while this daemon
  owns the acquired A2DP transport — the sink's own 500 ms packet-receipt freshness (a fresh receipt
  is playing, no receipt while the transport is owned is paused), then `MediaPlayer1.Status`, then
  the A2DP transport `active` state, then receipts again. Explicit Play/Pause/Next/Previous never
  consult that memory. The direction and the evidence (`memory|receipt|status|transport`) are logged
  at Debug; a failed AVRCP call is logged at Warn. That evidence is read-only, per connection, reset
  on stop/error/new source; absent evidence sends Play rather than inventing Pause. A recognised
  swipe reaches the current media through that same standard media contract over the app bus — a
  routed `MediaPlayerService.SkipNext`/`SkipPrevious` for an app provider, otherwise
  `BluetoothMediaService.Command` for the framework-owned source — and there is no private
  daemon→hardware path. Works with no media app installed; commands are serialized; full capture
  suppresses them.
- **Listening.** Teal tracks the framework's processed application microphone tap; blue chase covers
  the post-speech delay and reply. Onset and end are measured in sample time against a learned
  room-noise floor. An utterance ends after 700 ms in which the 300–3400 Hz band level has fallen
  clearly below the recent voiced level — a sustained voiced vowel is preserved, not treated as
  silence — and a reply needs 140 ms of modulated speech-band time or 400 ms of steady voiced level
  well above the floor. A cold-start guard keeps first speech out of the floor. No-speech turns
  cancel after the safety window and incomplete/discontinuous speech must not become an immediate
  reply. This is a level endpointer, not a speech classifier: steady loud noise at or above voice
  level beginning mid-turn is bounded by the 10 s cap and cancelled, a steady hiss/tone above 2.5×
  the floor for 400 ms can be answered, and a sustained quiet vowel below that multiple is not
  confirmed. The canonical external-music + full-level-word check recovers 1 reliable, 1 marginal
  and 4 buried/rejected words — an accepted music/quiet-speech limitation, not an accuracy
  benchmark. Loud music or lyrics can open or satisfy a turn, quiet speech can be missed, and there
  is no automatic music cancellation, AEC or speaker identification.
- **Audio cues.** A volume step plays the embedded volume cue at the new level; the microphone key
  plays distinct mute and unmute cues. The startup jingle plays once on installation, then only when
  `startup_sound_enabled` is on (off by default). Cues are framework playback streams under the
  master gain: they never block boot or input handling, and each toggle or step produces exactly one
  cue. The master volume is a ceiling on every app's contribution, the mixer holds its output below
  full scale with a soft limiter, and the PulseAudio **sink** is pinned at unity at startup and on
  volume changes, so the sink stage cannot rescale framework output. That pin covers the sink
  (device) volume only: `pacat`'s per-stream sink-input volume is a separate PulseAudio stage that
  the pin neither sets nor reads back, so a stream volume restored by PulseAudio's stream-restore
  module could still rescale the framework's output, and it is not part of this guarantee.
- **Wi-Fi fallback and captive portal.** When no saved network connects within the bounded startup
  attempt (or none is saved), the daemon raises the setup AP instead of leaving the speaker
  unreachable: SSID from the live Bluetooth adapter alias, WPA2 password `capsulad`, address
  `192.168.4.1`. The portal there exposes only Wi-Fi status, Scan and Connect — no administrator
  login, device settings, app control or system mutation — accepts requests only from the AP subnet
  with Host/Origin checks and an HMAC token bound to the client address and valid for 15 minutes,
  runs no script, and stops with the AP. The authenticated Web UI never offers the console's own
  network for Forget, has no remote disconnect, and forgets only inactive saved networks after
  explicit confirmation. Scan is capability-driven and may require manual SSID entry in AP mode. A
  successful join tears the AP down; a failed one restores the previous association or raises the AP
  boundedly and shows the error.
- **Web UI live controls.** Selects, sliders, checkboxes and the microphone gate apply immediately
  after local validation as field-scoped compare-and-swap updates with one in-flight request per
  group and coalesced slider input; the microphone control is positive (ON means enabled, labelled
  “Microphone on/off”). A failed update shows the error and re-reads the snapshot, and external
  changes update controls live. Unsubmitted text edits stay local until Save/Apply and are never
  submitted or overwritten by a neighbouring control or an incoming push. A stored non-default value
  is shown as a labelled current setting; provider dropdowns keep the explicit none choice and never
  rewrite a saved-but-ineligible app. Logs and metrics merge without discarding newer streamed entries, logs are
  capped at 400 entries (200 while paused), and a reconnect starts a new epoch. A request the daemon
  refuses (`PERMISSION_DENIED`, `NOT_FOUND`) is an inline error on that page plus a toast and
  leaves the socket, its live subscriptions and its epoch intact; only a real transport failure
  closes the socket and starts the reconnect. The wakeword area shows guided training and the real
  last 20 engine detections only.
- **Brightness.** Global brightness affects static frames too. Automatic mode retains the visible 5 %
  floor and discards stale cache entries when its mode changes; manual zero is explicit off. Ring
  frame deduplication keys on pixels **and** brightness, so a brightness-only change is still written.
- **Bluetooth.** Connect shows three blue ring pulses and a two-second `bt` display; disconnect shows
  exactly one blue pulse and no glyph, and the selected display baseline is never covered by a
  disconnect notice. The native A2DP sink is verified armed for the current adapter generation before
  the adapter can be made connectable or discoverable; a lost registration is repaired before the next
  connect and a failed one is reported as not ready. A connection is also verified to have an A2DP
  transport: within about five seconds of the profile request, a source without a
  `MediaTransport1` carrying the A2DP sink UUID is reported with the sentence "audio link is not
  A2DP" and the daemon asks that source for its A2DP source profile (`Device1.ConnectProfile`,
  UUID `0000110a`) once per source — it never disconnects the source to renegotiate, so a peer that
  will not bring A2DP up keeps its link and its verdict; the same source is not asked again for two
  minutes, and a newer connection or a disconnect cancels a pending verdict. Incoming connections
  or discoverability are refused while the audio profile is not advertised. **Incoming connections**
  off denies new inbound BR/EDR links, including from paired devices, with the adapter and BLE radio
  still powered and existing links left alone; **Discoverable** on implies incoming connections, and
  incoming off forces discoverable off. A source PulseAudio is actively streaming is not torn down
  to switch owners; the switch completes after it disconnects, while an idle PulseAudio module set is
  retired as soon as the native sink is armed even if a source is merely connected or paused. A
  paired source may already hold an ACL from the pre-daemon autopower window; the connect handler
  repairs the sink registration and requests the profiles over that link, and a peer that already
  abandoned A2DP is device-dependent. A connected audio source stays present without a player
  object, disconnecting another device never stops the active source, slow consumers resynchronize
  against current state rather than retaining a lost disconnect, and one active native source is the
  supported model. Volume sync is two way for display, one way for gain: a level the source reports
  becomes the framework master (phone slider → speaker), and a speaker-side change is written back to
  the source's AVRCP register so the phone's display follows. That write is a mirror, never a second
  gain stage: the local curve stays the only attenuation, because writing the register as an
  additional stage attenuated the same signal twice (10 % on both ends sounded near-silent) and
  compensating the local trim for the source's share made 5 % far too loud. A report within four
  AVRCP steps of a level written in the last five seconds is therefore treated as an echo and does
  not move the master.
- **Built-in startup.** At boot only a needed compiled-in app starts. The clock is framework-owned
  and starts whenever the segment display capability exists, preserving timers, alarms and the
  segment baseline even when another display provider is selected. The ring built-in is a provider
  and starts only when the persisted settings select it for its resource; deselecting it stops its
  run without disturbing clock timers. The manifest's `start_on_boot`/`background` flags do not
  override this rule for a built-in, and a framework-owned helper starts without a user selection.
  Selecting a built-in starts it immediately, while the explicit none choice leaves its output blank
  and survives restart. `Launch` on a stopped built-in starts exactly one run under its stable
  `builtin:<app_id>` ID, and stopping an unselected built-in leaves it stopped.
- **Lifecycle invariants.** Every terminal path — completion, cancel, error, crash, disconnect,
  dismissal, mute, watchdog expiry, daemon shutdown — releases its presentations, captures,
  microphone and conversation ownership, and the next output is a successful backend write (for a
  released ring with no baseline, an all-zero frame), never a stale routing message. A successful
  playback drains its tail; a cancellation discards it. Timers, callbacks and events carry the
  generation that created them: an older completion, OSD, mute or route load never modifies a newer
  owner. Geometry and gesture acceptance come from backend capabilities, never from a hard-coded
  panel. Duplicate or stray key events cannot change a held control's owner.

## Privacy and microphone interaction

Supervised applications and authenticated Web UI automation are untrusted for private data even with
a valid API key or supervised run identity. The daemon has one local gRPC listener on the Unix
socket; there is no network gRPC listener. The Web UI is a local administrator client of that
socket, but its authenticated browser/API-key surface is not a second privilege plane: API keys stay
limited to the read-only status and certificate endpoints and never reach recording, diagnostics or
credential-mutating routes.

- **Microphone indication, visible and audible.** A supervised app receives microphone audio only
  while both halves hold: a selected, visible, non-blank ring/segment presentation for that app at
  non-zero effective brightness, and the framework microphone-live cue that the mixer actually
  consumed and drained to the output sink. The cue is the exclusive mixer source while it runs, so an
  app cannot mask it; other playbacks freeze, not stop, and resume afterwards. Zero master volume,
  global mute, a missing output device, an unselected/blank presentation, or a cue the mixer cannot
  open or drain denies capture instead of recording silently. Manual zero brightness is off, not
  hidden recording.
- **Continuous delivery.** Both halves are re-evaluated for every audio chunk, not just at open:
  losing the visible indication or the master volume stops frame delivery immediately. Chunks withheld
  during the watchdog grace count as dropped and the next delivered chunk carries the discontinuity;
  the grace only bounds how long the capture stays alive before the `WATCHDOG_EXPIRED` notice
  terminates it. An invisible blank presentation never qualifies.
- **Pre-roll belongs to the activation.** The one-second pre-roll is attached only when the supplied
  activation ID is one the framework issued to that same app, still inside its delivery window,
  and has not been used. An arbitrary activation ID yields live, indicated capture only and cannot
  replay audio around another app's turn. Capture is per-app and released on stream end, dismissal,
  crash, mute, watchdog expiry or daemon shutdown.
- **Enrollment feedback.** Wakeword takes are a local-administrator UI operation, never app-facing.
  Every take (word, phrase, room/background) opens the red ring recording pulse and plays the audible
  start cue before capture begins; if the ring cannot be selected/rendered or the cue cannot be
  delivered, recording fails instead of proceeding silently. The browser receives samples and status
  only, never PCM or stored takes.
- **Administrator-only surfaces.** Device settings (including hostname, volume, microphone mute),
  device-wide diagnostics and log history, forced capture release, wakeword enrollment/training,
  Bluetooth incoming-connection/discoverable policy and unpair, application launch/stop, and
  foreign-app configuration. An app that omits its run ID still identifies as an app through its
  sandbox UID or supervised PID; silence never grants administrator identity. A compiled-in app
  is instead an explicit identity: its virtual run shares the daemon's PID with the Web UI and
  every other in-process client, so a PID alone never selects `capsula.clock` or `capsula.ring`
  — only that run's stable ID plus the per-boot internal token does, and the Web UI's own reads
  stay administrator reads. Per-app configuration, actions and run listings are scoped to the
  calling app; the administrator run table lists each built-in once under its own app ID with
  its real state, so a running clock shows as running and not as a Launch candidate.
- **Foreign data stays foreign.** Apps do not read another app's runs, settings or logs; the
  Bluetooth media view redacts the connected peer address/name for app callers while keeping the
  transport/now-playing fields a media provider needs; the shared asset store exposes only normalized
  `/assets/...` paths for regular, non-symlink files inside the asset root. The built-in clock and
  ring apps publish ordinary discoverable contracts through `AppInterfaceService`; a supervised app
  may call them through the app bus like any declared interface, but cannot claim an in-process
  system-app run, spoof another caller or reach an administrator operation.
- **Bluetooth scanning.** There is no BLE scan RPC. Apps cannot change the persisted
  incoming-connection or discoverable policy, so a consumer cannot both override the user's exposure
  choice and initiate scanning; adapter exposure stays an administrator action under the
  audio-profile-readiness invariant.
- **The portal is not a second admin surface.** The unauthenticated portal has only Wi-Fi status,
  Scan and Connect. It is bound to the AP address/subnet, rejects a foreign Host/Origin, requires an
  HMAC token bound to the client address and valid for 15 minutes for POST, runs no script and sets a
  no-store/no-referrer policy. It cannot read the admin session, cookies, WebSocket, app settings,
  logs, credentials or system controls, and it stops when the AP is torn down. Wi-Fi passphrases
  derive the PSK and are never returned; the derived key lives in a 0600 daemon store.

Honest limits: the audible cue proves the mixer consumed a cue at the selected master gain and wrote
it to the configured sink; it does not measure perceived loudness. The ceiling, per-app clamp,
limiter and sink pin are **digital-output** guarantees about PCM submitted through the framework
mixer — each app contributes at most the master level, an app cannot exceed that level with hot
samples or extra streams, and the limiter keeps the bus below full scale. They say nothing about
acoustic level, speaker efficiency or room response. Audio already buffered in the hardware/PA sink
or written by a non-framework path can still overlap the mixer's output — mixer-level exclusivity
and the sink pin cannot reach into the sink's own queue, a sink whose volume cannot be read back
cannot be proven at unity, and a non-unity per-stream sink-input volume (a separate stage the pin
neither sets nor reads back) is outside the pinned guarantee. Visible brightness is a digital/backend
statement; photons, gamma and panel failure are not measured. A root or physical compromise, or a
`-sandbox none` development run, is outside this cooperative model. Zero brightness and zero volume
are treated as denial, not as an opportunity to record quietly.

## Future-change gate

Use this gate for behavior changes and for polish/review work, not only for reported bugs. State the
intended interaction in task notes or tests before editing; update this document when the contract
changes. A user's examples are starting evidence, not an exhaustive list of affected transitions,
and a documentation-only change needs only the relevant rows.

| Transition | Questions to resolve | Proof to collect |
|---|---|---|
| Idle → start | What input starts it? Is there a hold threshold? What immediate feedback tells the user it worked? | Exercise the real handler/API path and observe the first selected/rendered output. |
| Active/progress | What does each light, glyph and sound mean? Is progress direction/geometry correct? Are volume/brightness and safety controls respected? | Inspect frames/PCM and time-based progress, including static content with changed settings. |
| Active → interrupted | What happens when another capture, activation, settings change or device event arrives? | Prove preemption and correct resumption; an older completion must not modify a newer owner. |
| Complete → dismiss | Does an alert repeat until its documented stop condition? Which physical action dismisses it? Is the matching release consumed? | Observe repetition, drive dismissal through input, verify it does not also activate another action. |
| Finish/cancel/error → baseline | Which streams, captures, presentations, timers and conversations must end? What replaces them? | Check resource cleanup plus the next successful backend output; test normal audio drain separately from abort. |
| Stop/crash/disconnect → recover | Can an old callback or lost event leave a phantom owner or stale route? Can the feature be used again? | Exercise an abnormal exit and a fresh interaction; verify terminal events and convergence to current state. |
| Save → restart/upgrade | Which user choices and in-flight state must persist? Are all affected package/settings versions compatible? | Restart through the supported service; re-read choices and exercise the installed package, not merely its API health. |
| Volume/mixer change → output | Which gain stage changed? Does every source still pass the per-app ceiling, the limiter and the sink pin? Can an app exceed the configured level with hot samples, extra streams or a second app? | Drive full-scale S16, hot F32 and overlapping apps through the public playback API; measure the sink PCM (≤ master, ≤ limiter threshold), confirm hot F32 is clamped, and confirm the fifth stream is `RESOURCE_EXHAUSTED`. |

For each relevant path, name the expected terminal owner/output and the allowed time to reach it.
Distinguish deliberate product limits from missing behavior. Do not silently introduce a timeout,
change a gesture, or reset user preferences merely to make a scenario terminate. When reviewing in
parallel, partition implementation ownership and share this contract first; cross-component
integration belongs to one coordinator, and agent reports are leads until their evidence and
observable outcomes are checked.

## Text-observable behavior

Normal logs include routing identity changes and **successful backend writes**, including
presentation/capture IDs, global brightness, segment masks/colon and ring RGB pixels. Failed driver
writes are logged as failures, not successful output. Clock logs include countdown pixel counts and
alert/audio lifecycle; input logs identify handled, consumed and cancelled controls.

Panel recognition logs at Info so a user's swipe can be measured in the field. A resolved gesture
logs `panel gesture <id> resolved tier=<tier> press_gap=<ms> window=<bound>ms from <trace>`, where the
tier is one of exactly five names: `chain` (opposite → centre → direction), `centre` (the centre
gate), `full` (the full-panel crossing of two opposite volume edges), `overlap` (two contacts inside
one touch) or `keys` (individual key effects); a full-capture
resolution logs the same fields with `in full capture` inserted after `resolved`, which is also the
only mode that can resolve a circle. `press_gap` is the measured centre→edge or previous→newest
press distance, `window` is the bound that applied — 700 ms for the centre gate, 400 ms for the
full-panel crossing, 250 ms for the chain and every other shadow — and `-` means unknown (an out-of-order
read). A reverted full-panel step logs `panel full-panel swipe reverted the leading volume step`
just before the gesture action. A pair outside the crossing window logs
`panel full-panel crossing rejected tier=keys decision=keys reason=contact gap press_gap=<ms>
window=400ms; both edges stay volume steps` and produces the two steps with no gesture and no revert.
A centre that misses
its bound logs `panel centre -> <key> p+<ms> decision=keys tier=centre reason=contact gap
press_gap=<ms> window=700ms from <trace>`, and any other abandoned shadow logs `panel gesture
rejected tier=<tier> decision=keys reason=<reason> press_gap=<ms> window=<ms>: <trace>` with its
measured `press_gap` and the applied window. Delivery and framework action are logged separately
(`panel gesture swipe_right delivered to capture cap-1 (mode=full_raw)`, `panel gesture swipe_right:
next media`, `panel gesture swipe_left: previous media`). `<trace>` renders each contact as `key
p+<ms> r+<ms>` (or `key p+<ms> down` while the contact is still held) with offsets from the first
press, `gap <ms>` for the signed release-to-press distance between contacts, and an `overlap`
marker; it is bounded to five contacts and carries no PCM or presentation metadata.

Animated trace on the Rockchip service:

```sh
CAPSULA_OUTPUT_TRACE=1 /etc/init.d/S99btctl.sh restart
tail -f /data/capsdemo/capsulad.log
/etc/init.d/S99btctl.sh restart   # end high-rate tracing
```

A direct daemon launch uses `-output-trace`. Animation samples are limited to four successful writes
per second per resource; ownership/brightness transitions are recorded separately. Logs rotate at
512 KiB with one backup. Traces contain output/control metadata, not microphone PCM.

A released ring with no baseline must be followed by a successful all-zero ring write. A countdown
must have a contiguous orange arc whose extinguished prefix follows the counterclockwise order.
Audio open/close records must agree with diagnostic active streams. These checks assess software
decisions and driver submission; they do **not** measure emitted photons, perceived loudness,
microphone acoustics or firmware behavior outside the framework.

## Automated verification

Race-enabled checks cover `internal/audio`, `internal/input`, `internal/routing`,
`internal/settings`, `internal/bluetooth`, `internal/discovery`, `internal/wakeword`, both assistant
modules and the daemon integration scenarios: timer persistence/repetition/dismissal, countdown
geometry and preemption, stopwatch arbitration, actual backend blanking, static-frame brightness,
visible-indication rejection, cancelled versus finite playback, held input and intervening activation
IDs, assistant dismissal through the circle key, restart after a lost framework connection, the
level-fallaway endpointer with colored/steady-noise and sustained-vowel regressions, mute/OSD
generations, timezone preservation and Bluetooth overflow recovery. The mixer is pinned to its
contract: full-scale S16 and hot `PCM_F32LE` through the public playback API stay at or below the
master, one app's ceiling survives extra streams, its fifth stream is `RESOURCE_EXHAUSTED`, the
limiter holds the bus below its 0.95 threshold, the sink-pin path runs against a scripted `pactl`,
and an architecture test fails on a `WritePCM(` call outside `mixLoop`. Bluetooth coverage includes
the four-octet SBC capability IE, the pure object-tree session assertion with its one profile
request per source (bounded by the two-minute window and cancelled by a newer connection), the
acquired-transport-first volume target, `ErrNoAbsoluteVolume` reported once per session, a toggle
direction resolved from a stale `MediaPlayer1.Status` and from the two-second command memory, the
centre-gated panel crossing and the 400 ms full-panel crossing (one media command with only the
completing contact suppressed, the leading full-panel step reverted through the canonical master
path, a lone volume edge stepping immediately with zero deferral, and two deliberate volume steps
for a slow opposite pair), the framework Bluetooth provider in the Web UI rendering one `PLAYBACK_PLAYING`
player from a simulated connected source and no provider at all without one, and the source-volume
rule: a report from the peer never moves the framework master while the master still reaches the
transport. Multi-word detection is covered at the model store (legacy migration,
per-word enablement and binding), the ensemble (score parity with the single-model engine,
deterministic arbitration, one global cooldown) and delivery (a bound app, no rerouting when it is
unready). Integration assertions observe asynchronous backend
updates rather than treating a gRPC send as an acknowledgement, and the audio-gap scenario overflows
the real assistant transport with voiced PCM so a fast consumer receiving an intact silence burst is
not mistaken for a dropped-audio test.

The frontend node tests, Vite production build, embedded daemon build, Linux arm64 crossbuild, `go
test -race ./internal/webui`, the wakeword last-20 history test and the install/train cancellation
tests pass; a targeted `TestTraining` run confirms only an explicit daemon precommit status proves
the installed model unchanged. A native browser through a loopback HTTP→TLS proxy exercised the 390
px and 849 px surfaces (login, live settings with unsaved-edit preservation, timezone search, app
settings document, API keys, password change, certificate download, diagnostics pause/resume).
Limits: these exercises cover no real Wi-Fi or Bluetooth peer, physical temperature, acoustics or
full training run, and the self-signed TLS endpoint is reached through the proxy without weakening
validation. Wakeword training writes one artifact per word: installing a word restarts detection
for all words, retraining keeps that word's enabled state and binding, and a cancel racing the final
atomic commit may still install that word's new model.

## Rockchip speaker verification

Exercises drive the real device's Linux evdev codes, not simulator admin events.

| Scenario | Observed evidence |
|---|---|
| Countdown direction and geometry | Driver frames carry 12, 11, …, 1 orange pixels in the specified counterclockwise order. |
| Completion and repetition | Driver segment masks `[63 63 63 63]` (00:00); eight clock playback opens in the trace; the alert stays active past ten seconds. |
| Digital jingle output | `master.monitor` contains three repeated three-beep groups; the final half second after dismissal is all-zero PCM. Mixer output, not acoustic loudness. |
| Physical dismissal | A one-second circle-key press dismisses the alert, stops clock audio and does not summon the assistant. |
| Cancellation and stopwatch | Cancelling a countdown releases the ring; stopwatch start/pause/resume/reset preserve the expected elapsed-time transitions. |
| Assistant lifecycle | Summon and hold-to-talk both use the real play/pause-key path; the summon opens real microphone capture; completed turns leave no assistant audio streams or ring ownership. |
| Physical microphone key | GPIO 21 reads 0 after mute and 1 after unmute; no acoustic recording needed. |
| Volume | A physical tap increases volume once and does not repeat after release; the original level is restored. |
| Brightness submission | Ring-driver writes carry global levels 0.25 and 0.8 during mute indication; the original level is restored. |
| Final output | A successful no-owner ring write contains twelve all-zero RGB pixels, not merely a routing-release message. |
| Restart and restoration | A normal service restart retains provider selections and settings, the assistant ready and no active app audio; the kernel boot ID is unchanged. |

Scenario results, rendered-output logs/assertions and binary hashes are kept under
`build/speaker-audit/`.

These exercises do not certify acoustic recognition thresholds, subjective LED brightness/gamma,
long-room wakeword false-positive rates, real Bluetooth re-pairing or source-device behaviour, or
unverified hardware variants. Bluetooth behavior is exercised with the simulator and real manager
state-machine tests; no user pairing is touched. Gesture inference (centre-gated
centre→edge swipes inside the 700 ms released window, the opposite→centre→direction chain,
the 400 ms full-panel crossing of opposite volume edges with its leading-step revert,
a slow opposite pair staying two deliberate volume steps, zero-deferral volume steps,
full-capture-only circles, preserved sequential taps, suppression of only the completing contact),
kernel-timestamp windows, tap/swipe media control with the
command-memory/receipt/`Status`/transport evidence order and the `MediaControl1` fallback, the
four-octet SBC capability IE, the session assertion with its one profile request per connection,
`ErrNoAbsoluteVolume`, the framework Bluetooth media provider and the two-way display mirror with
echo suppression, multi-word detection and delivery, mDNS apply idempotence and the processed-tap endpointer
have local unit and fixture coverage, and the real daemon passes all 14 assistant scenarios; device
and acoustic checks for the gesture, Bluetooth-profile, multi-word and endpointer changes remain
gates. Passive device traces show event shape, not transport success or contact order. A browser
session against a real simulator daemon observed host-name default/save/restart persistence,
invalid-label rejection, no false mDNS advertisement claim and the **Restart** labels on Clock, Ring
and Diagnostics; a browser run against a successful responder fixture exercised the corrected Avahi
apply (zero setter calls for startup, GET and same-name save; one for a real change; genuine
collision with no advertised claim; recovery). Real Avahi `.local` resolution and collision behaviour
on the device remain gates.