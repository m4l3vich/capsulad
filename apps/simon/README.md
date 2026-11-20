# Simon Says (`org.capsulad.simon`)

A physical Simon Says game for the Capsula Mini. The speaker's LED ring shows a
growing colour sequence and plays one distinct tone per colour; the player
repeats it with the four keys around the panel. The four-digit segment display
keeps the score. The game owns the ring, the score display, the speaker
playback stream and a full-panel touch capture only while a run is active, and
releases all four on every exit so the configured clock/ring baseline and the
normal panel controls return.

| | |
|---|---|
| App ID | `org.capsulad.simon` |
| Runtime | `quickjs` (foreground, no settings or actions) |
| Needs | an RGB LED ring with at least 4 pixels, a full-panel touch capture, a speaker; an optional four-digit segment display |
| Package | `manifest.toml`, `main.js`, this `README.md` |

## Build and install

```sh
make capsulad-cli                 # host packager
make simon                        # -> build/apps/org.capsulad.simon-1.1.0.tar.gz

# with the daemon running, the CLI applies the package live:
capsulad-cli app install build/apps/org.capsulad.simon-1.1.0.tar.gz
capsulad-cli app start org.capsulad.simon
```

`sh apps/simon/build.sh` stages the package directly; `OUT=...` changes the
output directory and `PACKAGE=0` skips the archive. The test suite stays out of
the installed artifact.

The target needs the QuickJS script host (`make interpreter-quickjs`, installed
as `lib/capsulad-quickjs`). A host without `Call.tryRecv` is rejected with a
clear log line; rebuild the host rather than running a blocking game.

## How to play

The ring is read as a clock face: pixel 0 is the top, and indices advance
clockwise. Each side of the panel owns a 90-degree arc centred on its
direction (a boundary pixel goes to the next side clockwise), so the ring is
divided into four coloured arcs centred on the physical keys:

| Panel side | Key | Colour | Tone |
|---|---|---|---|
| North | `mic_toggle` | Red | 392 Hz |
| East | `volume_up` | Yellow | 523 Hz |
| South | `play_pause` (marked circle) | Green | 659 Hz |
| West | `volume_down` | Blue | 784 Hz |

1. The game flashes the first colour on its ring arc and plays its tone.
2. Repeat it by pressing the matching side key. A correct press keeps that arc
   lit, plays the same tone briefly and increments the score.
3. Every completed round adds one random colour and replays the whole growing
   sequence before accepting input again. Side keys are ignored during a
   replay, while a round-complete display is shown, and during the error
   display; only the centre is meaningful then.
4. A wrong side key ends the round: the whole ring flashes white, a low falling
   tone plays, the display shows four dashes, and then the final score. Press
   any side key to retry from score zero, or hold the centre to leave.
5. Hold the unmarked centre for about 0.8 s to exit at any time. A shorter
   centre tap shows the hold hint and pauses a replay; releasing it resumes the
   replay where it stopped. The hold time is measured from the press, so the
   exit does not wait for the animation.

### Score and progress

The segment display shows the score zero-padded to four digits. **Score counts
accepted symbols**, so it rises with every correct press and always equals the
number of correct responses in the run. The colon is on while it is your turn,
off during playback and at the final score. All four decimal points light
briefly when a round is complete. The initial `0000` is the ready state.

If the device has no segment display, or the display cannot be captured, the
game runs without one: the growing ring sequence, its tones and the run length
are the progress indicator, and the score is written to the app log at every
accepted press and at the end. The panel controls and exit are unchanged. A
display with fewer than four digits is not used, because it cannot show the
whole score.

## Controls and ownership

While the game runs it holds a `TOUCH_FULL_RAW` capture of `touch.panel.main`,
so the four side keys and the centre are game input and the framework's volume,
mute, media and assistant actions pause. All four panel keys are the game's
symbols. The centre remains the documented way out.

On exit, failure or preemption the app:

- cancels the speaker playback stream, discarding any queued tone;
- cancels the touch input subscription;
- releases the panel, display and ring captures through
  `RoutingService.ReleaseCapture`;
- closes the ring and segment presentations.

That restores the configured baseline presentation for both resources and the
framework's normal panel semantics. A routing preemption (another owner taking
the ring or panel) is treated as a terminal interruption rather than continuing
with invisible output or a dead panel.

## Device requirements

The app reads capabilities instead of assuming a board:

- `DeviceService.ListResources` must advertise `light.ring.main` with
  `pixel_count >= 4` and `supports_rgb`, and `touch.panel.main` with
  `full_panel_available`;
- `AudioService.ListEndpoints` must advertise `audio.output.speaker`;
- `display.segment.front` is used only when it is available and advertises at
  least four digits. Colon and decimal points are used only when advertised.

Ring brightness follows the device's ring presentation brightness, and output
volume follows the master volume; the app never writes hardware or changes
either setting.

## Verification

```sh
node --test apps/simon/test/*.test.mjs
```

The contract suite loads the shipped `main.js` into a fake QuickJS host and
checks ring geometry at several advertised pixel counts, the colour/tone pair
for each side, the growing sequence and score, input ordering (including
playback-period input being ignored), wrong-answer/retry, centre hold and tap,
no-display degradation, preemption, setup failures and the full release order.
It is host evidence for requests, frames and PCM, not a physical LED or acoustic
measurement; see `docs/VERIFICATION.md` for the device proof levels.