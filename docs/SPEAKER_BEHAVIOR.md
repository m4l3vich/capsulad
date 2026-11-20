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
  provider with its PLAY/PAUSE/SKIP buttons mapped onto the same commands. That panel is a strict
  now-playing projection rather than provider discovery: it lists only players that report
  `PLAYBACK_PLAYING`, and for the framework source only while playback is *observed* — the peer's
  own status, transport packet receipts or transport state, never this daemon's command memory, so
  an acknowledged Play the peer did not act on cannot create a row. A connected but idle, paused or
  stopped source therefore adds no row and no error card, while the physical key and the swipes
  still drive it, and so does `BluetoothMediaService.GetState`/`Command` directly — from a
  framework client on the socket or from the MCP `media_command` tool, neither of which needs a
  panel row. Its transport buttons return to the panel as soon as playback is observed again.
  Bluetooth uses
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
  plays distinct mute and unmute cues; a Bluetooth source that connects while the speaker accepts
  inbound links (speaker mode) plays the embedded connection cue (`question.wav`) once. The startup
  jingle plays once on installation, then only when `startup_sound_enabled` is on (off by default).
  The same installation/`startup_sound_enabled` decision shows the **startup ring introduction**: a
  short blue-teal (the aurora palette midpoint) run on the top of the ring that fades in from dark,
  holds, fades back down in steps and releases. It is a temporary framework capture on the ring,
  like the muted-microphone indication: the run length derives from the advertised pixel count
  (pixel 0 is the top, indices advance clockwise; the run is clamped to the preset's 12-pixel
  maximum on a larger ring), a newer system indication or app capture takes selection priority,
  and the selected ring baseline — including an explicit blank for a closed active window — is
  selected again on release instead of a forced black frame. An explicit no-ring-provider choice
  still permits this transient framework indication on the ring (as volume, mute and alert
  indications already do); its release returns the explicit blank frame, so the durable choice is
  a dark baseline, not a ban on framework feedback.
  The timeline is a 0.6 s fade-in inside a 1.5 s hold, then six 70 ms taper steps: about 1.9 s
  total, deliberately not the ten-second jingle. The physical writer starts before that lifetime
  begins, so a cold boot with slow boot-app spawning cannot consume the fade-in. The
  introduction mirrors the cue's own decision rather than a second rule: the installation marker is
  written only after the cue reaches the mixer, so a boot whose audio path cannot open does not
  consume the one-shot cue and retries both the jingle and the introduction on the next boot; once
  the marker exists, a boot with the startup sound off shows neither. Shutdown, cancellation and a
  superseded introduction invalidate the fade generation, so a late fade step cannot blank or
  release a newer owner (a newer indication merely wins selection while the introduction remains
  privately owned and is destroyed by its own release).
  Cues are framework playback streams under the master gain: they never block boot or input
  handling, and each toggle or step produces exactly one cue. The persisted 0–1 master position
  is mapped once to an exponential 60 dB taper (linear in dB, so 5% is about -57 dBFS) and is the
  ceiling on every app's contribution, the mixer holds its output below full scale with a soft limiter, and
  the PulseAudio **sink** is pinned at unity at startup and on volume changes, so the sink stage
  cannot rescale framework output. That pin covers the sink (device) volume only: `pacat`'s
  per-stream sink-input volume is a separate PulseAudio stage that the pin neither sets nor reads
  back, so a stream volume restored by PulseAudio's stream-restore module could still rescale the
  framework's output, and it is not part of this guarantee.
- **Wi-Fi fallback and captive portal.** When no saved network connects within the bounded startup
  attempt (or none is saved), the daemon raises the setup AP instead of leaving the speaker
  unreachable: SSID from the first 32 UTF-8 bytes (rune-aligned) of the live Bluetooth adapter
  alias, WPA2 password `capsulad`, address `192.168.4.1`. The portal there exposes only Wi-Fi
  status, Scan and Connect — no administrator login, device settings, app control or system
  mutation — accepts requests only from the AP subnet with Host/Origin checks and an HMAC token
  bound to the client address and valid for 15 minutes,
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
- **Web UI appearance.** The switch beside **Sign out** offers Light, Dark, and System.
  The choice is saved in the browser, not on the speaker. System is the default and follows
  the OS appearance live; Light and Dark stay fixed. Custom app settings use the embedded
  daisyUI kit inside their isolated frame. The palette updates without reloading that frame,
  discarding edits, or interrupting a save.
- **Web UI app guide.** An app detail page offers its user guide — the package root `README.md`, or
  an embedded document for a compiled-in app — over the session-authenticated
  `GET /api/apps/{appID}/guide`, independent of the app's run state, so a stopped app can still
  explain its controls, launch and recovery. The response is JSON with the Markdown as text: the
  SPA renders it with interpolated text and no markup, link target, script context or network
  primitive, and no package path is exposed. Reading it changes no setting, launches nothing and
  has no app-visible side effect. A package without a guide is reported as having no guide, and an
  unreadable one (wrong file type, over 64 KiB, invalid UTF-8, empty) is reported separately
  without blocking install, launch or the rest of the page.
- **Web UI now playing.** The Overview media card is a strict projection: only players reporting
  `PLAYBACK_PLAYING` are listed, and a provider with none is omitted entirely, so an idle, paused
  or unreadable provider never renders a row the user cannot act on and never an error card. It is
  a display policy, not a change to discovery or control: the Overview app list still shows a
  stopped or paused app with its launch control and its declared actions; a READY but idle or
  paused provider is still reported and addressable through
  `capsulad-cli media list`, `AppInterfaceService` and `ListPlayers`/`WatchPlayers`; and the
  Bluetooth source stays readable and commandable through
  `BluetoothMediaService.GetState`/`Command`, which need no panel row. A provider with no ready
  run is not routable at all, which is a discovery fact, not this filtering. With nothing
  playing the card says so. The card converges without
  polling or reload: the daemon follows each READY media provider's `WatchPlayers` stream and
  `BluetoothMediaService.WatchState`, renewing a feed below the app bus's own call bound and
  rebuilding the whole authoritative overview on an observed change, so a provider that starts
  from stopped appears in an already-open page and the row disappears when playback stops. Feeds
  exist only for READY providers, and a failed listing keeps the existing feeds rather than
  tearing down a working projection; position ticks and a hidden player's metadata change are not
  overview changes, so no timer polls a stopped app.
- **Brightness.** Global brightness affects static frames too. Automatic mode retains the visible 5 %
  floor and discards stale cache entries when its mode changes; manual zero is explicit off. Ring
  frame deduplication keys on pixels **and** brightness, so a brightness-only change is still written.
- **Bluetooth.** Connect shows three blue ring pulses, a two-second `bt` display and one play of the
  embedded connection cue (`question.wav`); a boot-restored source and a coalesced resync do not
  replay the cue. A disconnect shows exactly one blue pulse, no glyph and no cue, and the selected
  display baseline is never covered by a disconnect notice. The native A2DP sink is verified armed
  for the current adapter generation before the adapter can be made connectable or discoverable; a
  lost registration is repaired before the next connect and a failed one is reported as not ready.
  A connection is also verified to have an A2DP transport: within about five seconds of the profile
  request, a source without a
  `MediaTransport1` carrying the A2DP sink UUID is reported with the sentence "audio link is not
  A2DP" and the daemon asks that source for its A2DP source profile (`Device1.ConnectProfile`,
  UUID `0000110a`) once per source — it never disconnects the source to renegotiate, so a peer that
  will not bring A2DP up keeps its link and its verdict; the same source is not asked again for two
  minutes, and a newer connection or a disconnect cancels a pending verdict. Incoming connections
  or discoverability are refused while the audio profile is not advertised. **Incoming connections**
  off denies new inbound BR/EDR links, including from paired devices, with the adapter and BLE radio
  still powered and existing links left alone; **Discoverable** on implies incoming connections, and
  incoming off forces discoverable off. The advertised device name is an administrator-only Web UI
  setting (Settings → Bluetooth, pencil button): it is validated as 1..248 UTF-8 bytes with no
  control characters, persisted by BlueZ as the adapter alias and advertised without a power cycle.
  The provisioning AP SSID is derived from this name only when the AP is (re)started, and uses its
  first 32 UTF-8 bytes on a rune boundary (a Wi-Fi SSID cannot carry more), so a rename while the
  AP is up takes effect on the next raise. Apps cannot change adapter exposure or the advertised name:
  there is no framework RPC for either.
  A source PulseAudio is actively streaming is not torn down
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
  gain stage: the framework mixer's canonical taper stays the only attenuation on the native sink,
  and the PulseAudio fallback applies that same taper locally because it bypasses the mixer.
  Writing the register as an additional stage attenuated the same signal twice (10 % on both ends
  sounded near-silent) and compensating the local trim for the source's share made 5 % far too
  loud. A report within one AVRCP step of a level written in the last 900 ms is therefore treated
  as an echo and does not move the master.
- **Built-in startup.** At boot only a needed compiled-in app starts. The clock is framework-owned
  and starts whenever the segment display capability exists, preserving timers, alarms and the
  segment baseline even when another display provider is selected. The ring built-in is a provider
  and starts only when the persisted settings select it for its resource; deselecting it stops its
  run without disturbing clock timers. The manifest's `start_on_boot`/`background` flags do not
  override this rule for a built-in, and a framework-owned helper starts without a user selection.
  Selecting a built-in starts it immediately, while the explicit none choice leaves its output blank
  and survives restart. `Launch` on a stopped built-in starts exactly one run under its stable
  `builtin:<app_id>` ID, and stopping an unselected built-in leaves it stopped.
- **Panel game (`org.capsulad.simon`).** A launch reads the device's advertised capabilities
  (`DeviceService.ListResources`, `AudioService.ListEndpoints`) and then owns exactly four things
  for the life of the run: an LED ring presentation on `light.ring.main`, an *optional* score
  presentation on `display.segment.front`, a full-panel capture on `touch.panel.main` and one
  speaker playback stream. The game is a colour-and-tone memory game, not a segment-display game:
  the ring is read as a clock face (pixel 0 at the top, indices advancing clockwise) and each side
  of the panel owns the 90° arc centred on its own direction, so the four colours are stable
  cardinals — north `mic_toggle` red, east `volume_up` yellow, south `play_pause` green, west
  `volume_down` blue — with one distinct tone per colour (392/523/659/784 Hz, a rising pentatonic
  figure) and a deliberately lower falling buzz (220→130 Hz) reserved for a wrong answer. The arc
  geometry derives from the advertised pixel count and needs at least four pixels and RGB support;
  nothing is keyed to a board name or a hard-coded ring size. A round replays the whole growing
  sequence in colour and tone at 320 ms on / 140 ms off; a correct press re-lights that arc, plays
  a short version of the same tone and raises the score; a completed round lights every arc for
  260 ms while side input is ignored; a wrong side key flashes the whole ring white, plays the
  error buzz and shows four dashes, then the final score with the colon off, and any side key
  starts a retry from zero. While the game runs *every* panel action belongs to it: side keys are
  game input and the framework's volume, mute, media and assistant handling is suppressed, and side
  input during a replay, a round break or the error display is ignored rather than queued.
  The score display is optional and never gates the game: with no `display.segment.front`, one with
  fewer than four digits, or one whose capture is refused, the ring, the tones and one log line per
  accepted press are the progress indicator, and the colon/decimal points are used only when the
  display advertises them. Holding the unmarked centre for at least 800 ms, measured from the input
  event's monotonic timestamp so the exit never waits for an animation, is the documented exit; the
  press immediately shows the hold hint (four dashes, colon on) and dims the ring to white, and a
  release before the threshold restores the current phase — including resuming a paused replay —
  while a release at or past it exits. A routing preemption of the ring or the panel, an ended
  capture stream or a speaker notice is a terminal interruption, not a degraded game: the run ends
  through the same release path instead of continuing with invisible output or a dead panel.
  On every terminal path the app cancels the speaker stream (discarding a queued tone), cancels its
  input subscription, releases the panel, display and ring captures through
  `RoutingService.ReleaseCapture`, closes both presentations and awaits the stream ends, so the
  configured clock/ring baseline returns for *both* resources and the framework's panel semantics
  come back. A timed read is a
  fatal error in that host, so the game needs one with
  `Call.tryRecv`/`tryRecvBytes`: ship the matching `lib/capsulad-quickjs` from
  `make interpreter-quickjs`, otherwise the app logs `unavailable: this QuickJS host has no
  Call.tryRecv; rebuild the interpreter host` and stops cleanly instead of playing. The host closes
  the app bus just before the process exits; the daemon's lost-connection handler
  (`onAppDisconnect` → `StopCrashed`) then marks the run stopping and sends SIGTERM. Depending on
  which wins, the run is recorded as 143 "stopped" (signal landed) or 0 "stopped" (process exited
  first). Cleanup has already completed either way; the crashed path sets `restartAfterStop`, but
  Simon declares `restart_on_failure=false` and is not a selected provider, so nothing relaunches
  it. Recorded timings for the centre-hold exit (a press surfacing within about 20 ms mid-replay,
  exit 0.5 ms after a 1018 ms release) and for `capsulad.clock` regaining `display.segment.front`
  are simulator evidence from the previous segment-first build; the ring/tone build's own device
  run — including the ring's emitted colours and the tones' acoustic output — is still a gate,
  since frame and PCM submission is not physical output.
- **PA announcement (`org.capsulad.pa`).** A background service that is already running; the
  trigger is a routed `Play` call carrying one streamed RIFF/WAVE file. Feedback is one
  `PlayResult` per accepted clip — its `detail` names the WAV's own format (`played 3200 frames at
  16000 Hz/1 ch`) — plus one `pa:` log line per call (`played`/`rejected`/`stalled`/`cancelled`,
  carrying both the WAV and mixer formats) and the daemon's own playback open/close records; there
  is no presentation, no microphone and no permission to change the speaker's master volume.
  The app holds one playback stream per call at the current master gain, opens it without an
  internal deadline so a slow but conforming caller is not cut off, and releases it on every
  terminal path; the 15 s wait for the next request frame, the bounded drain and caller
  cancellation are the only terminators, and a closed framework stream is reported as
  `UNAVAILABLE`. A frame that carries no WAV bytes at all (an empty message, an empty `data`
  field, or only other fields) is not progress: it ends the call with `INVALID_ARGUMENT` and
  `request frame N carries no WAV bytes`, so one caller cannot hold the app — and a queued second
  caller — for its whole routed deadline by sending no-ops; a one-byte frame is still progress.
  A call is held for at most the caller's own routed deadline (30 s by default, 5 minutes at
  most, enforced by the daemon as a cancel), so a second caller waits for the current clip plus
  at most the owner's remaining deadline. The `max_seconds` bound (1–60, default 30) follows the
  stored setting — a raised or lowered value applies to the next clip (an earlier build silently
  reset it to the default on every boot) — and a clip over it is refused before it starts, while
  an unknown-length stream is stopped at the bound. One call is served at a time: a second caller
  waits, and only the caller that owns the call can end it, by cancelling. At most 1 MiB of RIFF
  chunks may precede the `data` chunk; beyond that the call ends `INVALID_ARGUMENT` with a detail
  naming the limit.
- **Internet radio (`org.capsulad.radio`).** One native app owns the station list, the display,
  the network stream, the decoder and the standard media contract: it fetches one HTTP(S) MP3
  stream through the shared `libcapsulad` transport, strips ICY metadata, decodes it and writes
  paced PCM to the framework speaker in its own process. There is no companion app, no app-to-app
  audio hop and therefore no cross-app push authorization; the retired MicroPython front end and
  `org.capsulad.radio.tuner` are replaced under the same app id, and the stored `stations`,
  `station` and `autoplay` settings survive the package replacement. The app is
  not a resident service: it is `background = false`, `start_on_boot = false` and
  `restart_on_failure = false`, so it is not started at boot and a clean self-exit
  is not relaunched; one run at most is alive. Play or a retune shows the station
  number with a dark colon, whether buffering or playing. Paused replaces the
  number with a pause sign — two full-height bars in the middle two cells, dark
  colon — and holds that state and the run for one minute; Play inside the
  minute cancels the stale expiry and re-dials, while the deadline passing
  reports the terminal state and exits. The **Always display station number**
  setting defaults off: after 10 s a playing radio releases its
  `display.segment.front` capture and presentation, restoring the selected
  baseline without affecting playback or the ring. A retune replaces that
  deadline. Enabling the setting while playing shows the number again;
  disabling it uses the current tuning deadline, not a new one. It never
  extends the pause hold: a paused radio always ends after its minute.
  Failure shows four dashes only while the app owns the display; with
  `always_display` off the dashes use the tuning window, and with it on they
  last until Stop/Dismiss, Next/Previous or at most one idle minute. A late
  failure after the display was released exits without bringing back the
  overlay. Stop, dismissal or a terminal expiry releases the display capture and
  presentation immediately, so the selected baseline returns, and ends the run.
  A device that advertises no segment display plays without one and retries on
  the next state change.
  A bare launch starts a run that only proves readiness and never starts audio,
  but it waits one idle minute for Play rather than staying resident; a second
  bare invocation while the run is alive refreshes that minute, and a station
  step while idle does too. `autoplay` is off by default and plays the selected
  station once, at the first settings snapshot after a launch, never restarting
  a radio the user stopped and never starting playback when the setting is
  changed later. A short circle tap follows the normal media contract (the app
  is a standard provider while its run is alive, so the Web UI "Now playing"
  panel and the swipes drive it), and the circle key's dismissal runs the app's
  `dismiss` action. Pause is not a parked buffer: the station stream is closed
  and the queued tail discarded, `PLAYBACK_PAUSED` is reported and the pause
  sign held for one minute, and resuming re-dials the station.
  Next/previous wrap the list and persist the new `station` number with a
  compare-and-swap update; a live, paused or errored radio switches station
  immediately, while an idle radio only selects and refreshes its idle minute —
  changing the stored `station` never implicitly starts audio. The packaged
  settings document renders the parsed station list as a named picker and writes
  `station` through the sandboxed `capsulad.settings-ui/1` bridge; an
  out-of-range stored selection is reported and does not start audio, and the
  app never silently rewrites a deliberate selection. A station switch while
  active replaces the owned playback: the previous session is cancelled and its
  late open, read or failure cannot clear the newer stream or selection, and a
  stale own-write echo cannot revert it.
  Streaming is bounded on every path: a read that goes idle for 10 s fails the stream, up to three
  reconnects with 1/2/4 s backoff run inside the same session, and a terminal failure reports
  `PLAYBACK_STOPPED` in the contract with the reason logged, then ends the run when the error's
  display window closes. A stream
  that never starts or goes silent cannot leave a stale `PLAYING` even with no user activity: an
  autonomous watchdog (500 ms ticks) bounds "no audio reached the speaker" at **60 s** while
  buffering or playing — ending as `radio did not start (no audio for 1m0s)` or `radio stalled (no
  queued audio for 1m0s)` — which covers the three-reconnect budget plus the first-chunk idle, so a
  legitimate retry window is never pre-empted. A finite file that ends is not a live station: its
  tail chunk is written, EOF triggers the same bounded reconnects and the app then ends in the
  error state, while a live stream stays playing. The media contract keeps exactly one player, id
  `radio`, for as long as the run is alive: `ListPlayers`/`GetStatus` expose the paused entry
  during its hold, so the selected station stays discoverable and `Play` can resume it (the strict
  Web UI Now Playing projection hides non-playing entries — a consumer policy, not a different
  contract), `GetStatus`/command calls require a non-empty `playerId` (`INVALID_ARGUMENT` when it
  is missing, `NOT_FOUND` for an unknown player), and `WatchPlayers` sends a full snapshot first
  and then converges on each change without a slow watcher blocking playback. After the idle exit
  there is no ready provider to discover or command, and a new launch is the recovery.
  `Close`/`SIGTERM` cancels the stream, aborts playback, closes the presentation and waits for its
  loops; a self-exit uses the same release path, and a killed process is covered by the framework,
  which releases the run's presentation, capture and audio stream.
- **AirPlay receiver (`org.capsulad.airplay`).** The trigger is a sender on the local link: it
  finds the receiver through the two mDNS records the daemon publishes and connects to the
  daemon-allocated control port. Opening a TCP connection, querying receiver information, or
  completing HomeKit pair-setup/pair-verify does not take ownership: pairing protects and keys
  that connection but is not a media session. A successful media-start request is required
  before a new connection can replace the active sender: an `ANNOUNCE` or a `SETUP` that stages
  a real audio stream; event-only and control-only `SETUP` forms configure channels but do not
  take the player. Failed pairing or stream setup leaves
  the active sender, its audio, and its group unchanged. RAOP and AirPlay 2 setup replies report
  the bound ports of the corresponding daemon-allocated audio listeners. Advertisement is what
  makes the speaker
  appear in a sender's output list, so it follows the DNS-SD limits the responder enforces rather
  than the saved setting verbatim: both `_airplay._tcp` and `_raop._tcp` carry the same 32-byte
  ed25519 HomeKit long-term public key as `pk` (64 lowercase hex characters), and the instance
  labels are a rune-safe projection of the saved receiver name into the 63-byte DNS-SD limit — the `_raop`
  instance keeps its `<DEVICEID>@` prefix and shortens only the display part. The saved name and
  `/info` keep the full value; the legacy 2048-bit RSA key is never advertised (its base64 form is
  about 360 bytes and a single TXT item over 255 bytes is refused by the responder) and is used
  only to decrypt an `ANNOUNCE` that carries `rsaaeskey`, so a sender that takes its key from the
  TXT record uses the unencrypted path. The advertised authentication surface is HomeKit pairing
  plus the `/fp-setup` handshake: feature bit 14 (Authentication4) is set by default, but it does
  not enable FairPlay-protected playback or claim MFi. Republishing is serialized: one publish
  withdraws and replaces both records atomically, and a close waits for an in-flight publish, so
  a session start/end, a group-id change, a settings change or a replaced sender cannot make two
  callers register the same instance and be told the name collides. Feedback is the standard media
  contract (buffering, then playing, with the sender's metadata) plus the ring: the cover-art
  palette with the audio envelope, or a dim neutral breath with no cover art, never an invented
  image. Modern AirPlay 2 now-playing arrives as the sender's `POST /command`
  `updateMRNowPlayingInfo` plist (the event channel remains the receiver-to-sender notification
  path): the receiver reads the `npi-text` record under `params.params` — title, artist, album,
  duration, elapsed time and `kMRMediaRemoteNowPlayingInfoArtworkData` — treats `replace` as a
  full record that clears omitted fields and artwork and `update` as a merge, and takes
  playing/paused/stopped from a separate `updateMRPlaybackState` command; a nested record
  without the exact `npi-text` marker is ignored instead of clearing the live record, and
  promotion replays a timeline-only record's duration and position. Type-103 playback stays
  buffering until decoded PCM reaches the sink, and an explicit paused/stopped report outranks
  PCM-derived playing without moving the transport. A pause, immediate flush or reconnected
  data channel retires the current PCM generation: an explicit playing report after that
  boundary stays buffering until fresh PCM arrives, and a late write completion from the
  retired generation cannot repaint the player. Evidence is per type-103 stream, and an
  immediate `FLUSHBUFFERED` retires every affected generation before publishing the pause,
  so consumers never observe PAUSED followed by a stale PLAYING. The record's informational
  playback rate never moves the audio transport either. The legacy DMAP text, image and
  `progress:` path remains for RAOP and senders that negotiate it. A pause (`rate 0`) flushes
  queued audio and reports `PLAYBACK_PAUSED`; the ring
  presentation, the routing capture that selects it and the playback stream are owned by the
  session and released on teardown, connection loss, takeover, idle timeout, app stop or a
  settings change; releasing the capture restores the selected ring baseline, and the playback
  stream is aborted rather than drained so a stopped session makes no late sound. An authenticated
  type-103 buffered stream is staged by `SETUP` but remains silent until a valid
  `SETRATEANCHORTI` or `SETRATEANCHORTIME` anchor and audio arrive; the reference literal and
  the field client's full spelling name the same plist-bodied anchor. Its timeline is matched to
  that sender's PTP source, and an absent or undisciplined match uses local arrival timing rather
  than another source's clock. Only decoded PCM reaching the sink changes the player to
  `PLAYBACK_PLAYING`. Immediate `FLUSHBUFFERED` stops and discards sequences below its endpoint,
  deferred ranges discard only `[from, until)`, and a fresh anchor is required to resume.
  Invalid or unauthenticated buffered setup, malformed flush or anchor requests, and a short
  `shk` are refused before changing listeners or the live sender.
  Panel gestures and the Web UI reach the sender through its event channel — the same standard
  media contract — and never through a private daemon→sender path. Volume has one control:
  the persisted speaker master. Connecting reports the existing level without resetting it.
  The authenticated sender that owns the media session can change it, including while paused;
  probes, event-only connections, unauthenticated senders and a retired owner cannot. Speaker-side
  changes are mirrored to the sender without a second PCM gain stage. Disconnect and takeover
  do not reset the master; the framework mixer and limiter still apply. See
  [VERIFICATION.md](VERIFICATION.md) for host and device evidence and the
  [AirPlay app guide](../apps/airplay/README.md) for protocol details.
- **Home Assistant bridge (`org.capsulad.homeassistant`).** The app is installed, started and
  configured with an MQTT broker (named, or discovered with mDNS when `broker_discover` is on);
  a launch while it runs is an invocation that forces a discovery/state republish. Feedback is a
  retained `online`/`offline` availability topic with a broker last will for the crash path, a
  diagnostic "Bridge status" sensor, and one state topic per published entity; every applied
  command republishes the state it changed, and a refused command is logged and reflected in the
  status sensor instead of changing anything. The bridge owns the ring and the segment display
  only while it is driving them: switching the light off or the display overlay off releases the
  capture so the configured default resumes, and the `release_hardware` action (or its MQTT
  button) hands both back immediately. A settings change stops the runtime, releases every
  presentation and capture, and starts again with the new configuration. A deliberate stop
  publishes the retained `offline` marker, then sends `DISCONNECT` (which also discards the
  broker's last will), releases every presentation and capture, and exits 0 with the device back
  on its baseline; an unexpected framework-connection loss is the failure path and exits 1, so
  `restart_on_failure` is honest. A broker that stops answering fails the session instead of
  leaving a half-online bridge: the bridge publishes `offline`, sends `DISCONNECT` and retries
  with a paced backoff (1 s, 2 s, 4 s … capped at 1 min, reset only after a usable session held
  30 s, so it does not flap), and its watchers run only for entities it has actually published, so
  a disabled exposure goes quiet rather than staying silently stale. The Bluetooth source is
  enumerated as an `enum` state sensor
  plus a title sensor with four transport buttons on the same public command path, not as a
  media-player entity. The optional touch capture is partial, so the framework's volume and
  microphone gestures keep working.
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
  Bluetooth incoming-connection/discoverable policy, advertised-name change and unpair, application
  launch/stop, and foreign-app configuration. An app that omits its run ID still identifies as an
  app through its sandbox UID or supervised PID; silence never grants administrator identity. A compiled-in app
  is instead an explicit identity: its virtual run shares the daemon's PID with the Web UI and
  every other in-process client, so a PID alone never selects `capsulad.clock` or `capsulad.ring`
  — only that run's stable ID plus the per-boot internal token does, and the Web UI's own reads
  stay administrator reads. Per-app configuration, actions and run listings are scoped to the
  calling app; the administrator run table lists each built-in once under its own app ID with
  its real state, so a running clock shows as running and not as a Launch candidate.
  The AirPlay receiver has a narrowly scoped exception for the master-volume setter only; its
  authenticated READY run can follow the active AirPlay sender without gaining access to device
  settings or microphone controls.
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
mixer — each app contributes at most the tapered master gain, an app cannot exceed that gain with
hot samples or extra streams, and the limiter keeps the bus below full scale. They say nothing about
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
| Volume/mixer change → output | Which gain stage changed? Does every source still pass the per-app ceiling, the tapered master, the limiter and the sink pin? Can an app exceed the configured gain with hot samples, extra streams or a second app? | Drive full-scale S16, hot F32 and overlapping apps through the public playback API; measure the sink PCM (≤ tapered master gain, ≤ limiter threshold), confirm hot F32 is clamped, and confirm the fifth stream is `RESOURCE_EXHAUSTED`. |

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
CAPSULAD_OUTPUT_TRACE=1 /etc/init.d/S99btctl.sh restart
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

The shipped app packages have their own host suites, run with `make test-apps`: the Simon
contract suite drives the real `main.js` inside a fake QuickJS host (ring geometry at several
advertised pixel counts, the colour/tone pair per side, input ordering, retry and centre
hold/tap, no-display degradation, preemption and the release order), the PA suite exercises the
real `main.py`'s WAV parser and protobuf helpers under CPython, and the Internet Radio and Home
Assistant bridge suites are nested Go modules. [VERIFICATION.md](VERIFICATION.md) still carries
the recorded runs of the earlier segment-first Simon build and of the retired two-package radio
(its CPython suite, simulator `e2e` with a fixture tuner, and live `e2e-tuner` against a real MP3
station); those entries are evidence about those builds, and the suites that produced them no
longer exist. None of it is a device measurement: a radio fetch/decode run is
host evidence, and app-package installation and runs on the unit are recorded separately with
their own proof levels.

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