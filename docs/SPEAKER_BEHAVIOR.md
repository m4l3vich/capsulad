# Speaker behavior and audit

This audit covers the physical-speaker experience and its control-plane lifecycle: clock/timers/alarms, input, assistant turns, audio, mute/volume/brightness, ring ownership, Bluetooth feedback, and application start/stop. It is not a claim that every repository defect or every hardware variant has been eliminated.

## Interaction contract

- **Idle:** the selected clock/ring provider supplies the baseline. Choosing no provider means a physically blank output, not a frozen last frame, and survives restarts. Only a genuinely new settings store is auto-seeded. An explicit user-selected ring effect resumes after an interruption; it is not forcibly switched off. A ring app active window (`schedule_enabled` with local `HH:MM` bounds) turns the baseline into an explicit blank frame outside those times — for a nightlight or a dark night — while volume, mute, alert and application captures keep priority; the selected effect returns at the next boundary without reopening the presentation.
- **Timer:** the nearest running timer appears on the segment display and takes an orange ring capture (RGB 255/96/24). The ring starts full. Pixel 0 is at the top and indices increase clockwise; pixels extinguish in the order 0, N−1, N−2, …, 1. Another application's capture may interrupt it; the current countdown resumes when that capture ends.
- **Timer completion:** the display shows 00:00 with a blinking colon, the ring pulses orange, and the jingle repeats until dismissal. Cancelling a running timer releases its countdown. Timer and alarm alerts do not silently expire.
- **Stopwatch:** a running stopwatch shows elapsed time and holds an orange whole-ring pulse; the nearest timer countdown, when present, takes precedence. A single circle/play-pause or panel tap pauses it and releases the ring so the selected baseline returns; another tap resumes it. The `stopwatch_reset_gesture` clock setting (`hold` by default, or `double_tap`) resets and dismisses it. The gesture is consumed by the framework and never summons the assistant; a non-matching hold or double tap still does nothing rather than acting as a summon. Reset returns the display to the clock baseline.
- **Dismissal:** a primary-panel press, circle/talk-key press, or framework-owned tap/hold gesture dismisses sounding alerts. That same interaction cannot also summon the assistant. While an assistant turn is in flight, the same keys dismiss that turn through its declared `dismiss` action; the press and its release are consumed, so a second turn cannot start on top of the first. Volume and microphone keys retain their own safety/control functions. Full/raw input capture remains owned by the capturing application.
- **Overlapping alerts:** alerts are handled oldest first rather than playing over each other. Recurring alarms do not enqueue duplicate alerts for an already-ringing source. Disabling/deleting an alarm dismisses its sounding alert. A physical dismissal clears sounding alerts; the service can dismiss one alert by ID.
- **Assistant:** physical talk keys are manual/touch activations, including when hands-free detection is disabled. A short talk-key press does not summon. In hold-to-talk mode, a short primary-panel tap does not start an unpaired microphone turn either. A release is correlated with the activation and assistant that actually received its press, not a later wakeword activation or a newly selected assistant.
- **Listening:** teal tracks live microphone PCM; blue chase covers the post-speech delay and reply. Speech onset and end are measured against a learned room-noise floor, so steady background noise cannot keep a turn open until the listen cap. No-speech turns cancel after the safety window, and incomplete/discontinuous speech must not become an immediate reply. Successful playback drains its tail; cancellation discards it. Every exit releases capture, presentation, microphone, and conversation ownership.
- **Audio cues:** a volume step plays the embedded volume cue at the new level; the microphone key plays distinct mute and unmute cues. The startup jingle plays once on installation, then only when `startup_sound_enabled` is on (off by default). Cues are framework playback streams under the master gain: they never block boot or input handling, and each toggle/step produces exactly one cue.
- **Temporary feedback:** volume overlays and mute fades expire only the generation that created them. An old callback cannot blank a newer indication. Changing the mute-indication policy takes effect while muted. Timezone changes preserve built-in timers and stopwatch state; timers use monotonic time.
- **Brightness:** changing global brightness affects static frames too. Automatic mode retains the visible floor and discards stale cache entries when its mode changes. Manual zero remains explicit off.
- **Bluetooth:** connect shows three blue ring pulses and a two-second `bt` display. Disconnect shows exactly one blue pulse and no glyph; the selected display baseline is never covered by a disconnect notice. A connected audio source remains present without a player object. Disconnecting another device must not stop the active source. Slow event consumers resynchronize against current state rather than retaining a lost disconnect forever. One active native source remains the supported model.

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
| Input/assistant | No physical alert-dismiss path; dismissal could also activate/release a turn; duplicate/stray key events and ownership changes mishandled held controls; physical keys were labelled as wakewords; later activations overwrote release identity; silent hold turns missed the no-speech limit; empty activation IDs matched an empty release sentinel; an active assistant turn could not be dismissed by any physical control; taps on the talk key had no framework meaning. |
| Audio | Cancelled RPCs retained mixer streams/queued audio; paused playback drained into an unbounded resampler queue; queue accounting raced publication; exact-ratio resampling could strand a final sample; cancelling enrollment countdowns left their audio playing. |
| Feedback/settings | Restarting re-enabled providers despite saved empty/absent selections; timezone changes stopped virtual clock runs and erased timer/stopwatch state; stale OSD/mute callbacks affected newer feedback; live mute policy changes were ignored; automatic-brightness mode reused an old level; brightness-mode maps escaped settings snapshots; ring startup could block the output loop or consume a signature without a presentation. |
| Ownership/diagnostics | Closing presentations retained phantom captures; capture RPCs outlived released ownership; invisible content counted toward the microphone-indication watchdog; untouched resources appeared with empty IDs; terminal run events disappeared after run removal; launch could report success during an in-progress built-in stop. |
| Bluetooth | Snapshot/subscription gap; overflow lost terminal changes; absent player objects hid connected sources; unordered player choice targeted the wrong source; disconnect omitted `bt`; unrelated-source disconnect stopped playback; a late route load could resurrect a disconnected route; pairing bypassed audio-profile advertisement. Connect/disconnect notices were symmetric (a disconnect covered the display with `bt` and pulsed three times); sequential profile connects added ~3 s before audio; PulseAudio Bluetooth modules were unloaded on every connect, which could tear down a source with no audio route; a lost endpoint registration was not repaired before connecting. |

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

Race-enabled checks exercised `internal/audio`, `internal/input`, `internal/routing`, `internal/settings`, `internal/bluetooth`, the assistant module, and the affected daemon integration scenarios. Coverage includes timer persistence/repetition/dismissal, countdown geometry/preemption, stopwatch orange pulse and tap/hold/double-tap arbitration, actual backend blanking, static-frame brightness, visible-indication rejection, cancelled/finite playback, held input and intervening activation IDs, assistant dismissal through the circle key, restart after a lost framework connection, adaptive speech detection in steady room noise, mute/OSD generations, timezone preservation, Bluetooth overflow recovery and the single-pulse disconnect notice, and terminal lifecycle events.

The integration assertions observe asynchronous backend updates rather than assuming a gRPC send is an acknowledgement. The audio-gap scenario pauses the real assistant process and overflows its actual transport with voiced PCM; a fast consumer receiving an intact silence burst is not falsely treated as a dropped-audio test.

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
| Assistant lifecycle | Both summon and hold-to-talk modes exercised the actual voice-key path; the summon opened real microphone capture; completed turns left no assistant audio streams or ring ownership. |
| Physical microphone key | GPIO 21 read 0 after mute and 1 after unmute. No microphone/acoustic recording was required for this check. |
| Volume | A physical tap increased volume once and did not continue repeating after release; original volume was restored. |
| Brightness submission | Successful ring-driver writes included global levels 0.25 and 0.8 during mute indication; the user's original level was restored. |
| Final output | A successful no-owner ring write contained twelve all-zero RGB pixels, rather than merely a routing-release message. |
| Restart and restoration | A final normal service restart retained the original provider selections and settings, with the assistant ready and no active app audio. The kernel boot ID was unchanged. High-rate tracing was disabled after the exercise. |

Local evidence and rollback material are retained under `build/speaker-audit/`: scenario results, rendered-output logs/assertions, target binary hashes, and the pre-deployment daemon/package/settings/boot-hook backup. The previous redundant `S60capsulad` service is retained off the boot path on the device; `S99btctl.sh` is now the single canonical service.

These exercises do not certify acoustic recognition thresholds, subjective LED brightness/gamma, long-room wakeword false-positive rates, real Bluetooth re-pairing/source-device behavior, or unavailable hardware variants. Bluetooth changes were exercised with the simulator and real manager state-machine tests; no user pairing was removed or changed.
