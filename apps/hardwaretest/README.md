# Hardware Test (`org.capsulad.hardwaretest`)

A foreground test app for the physical Capsula Mini. It owns the segment
display, the LED ring and a full-panel touch capture until it is stopped.

| | |
|---|---|
| App ID | `org.capsulad.hardwaretest` |
| Runtime | `native` |
| Needs | four-digit segment display, RGB LED ring, full-panel touch capture, speaker and microphone for the tone check |
| Package | `manifest.toml`, `bin/hardwaretest`, this guide |

## What it does

1. **Segment display.** Shows `12:34`. For ten seconds the upper colon dot
   blinks once per second while the lower dot stays dark; for the next ten
   seconds the lower dot blinks twice per second while the upper dot stays dark.
   The two phases repeat. The colon phase is absolute, so showing a held-key
   digit does not restart the blink.
2. **Ring.** Runs a clockwise rainbow chase. Pixel 0 is the top and indices
   advance clockwise; the hue pattern takes two seconds per revolution. The
   ring is updated through the normal presentation and capture services, so the
   daemon's output loop remains the physical writer.
3. **Tone and microphone.** Three seconds after the chase starts the app opens
   the processed microphone endpoint and measures a short pre-tone floor. It
   then plays a soft 440 Hz tone at 20% of full scale (not a change to the
   device master volume) for 800 ms while recording the microphone. The
   recording is checked for the 440 Hz component against the pre-tone floor. If
   the tone is absent (or the microphone cannot open), the whole ring blinks red
   for three seconds and the rainbow chase resumes. The test runs once per app
   run.
4. **Touch.** A `TOUCH_FULL_RAW` capture makes the four side keys and the centre
   test input, with the framework's volume, mute, media and assistant actions
   suppressed. While a key is held the display shows its number and the colon
   dots are dark:

   | Held key | Position | Display |
   |---|---|---|
   | `mic_toggle` | north | `1` |
   | `volume_up` | east | `2` |
   | `play_pause` | south | `3` |
   | `volume_down` | west | `4` |
   | `center` | centre | `5` |

   Releasing the key returns the display to `12:34` at the current colon phase.

## Run and stop

```sh
make hardwaretest
capsulad-cli app install build/apps/org.capsulad.hardwaretest-0.1.0.tar.gz
capsulad-cli app start org.capsulad.hardwaretest
```

The app stays in the foreground until it is stopped. `capsulad-cli app stop
<run-id>` (or stopping the run in the Web UI) cancels the run; the app then
blanks and closes both presentations, releases the captures and the input
subscription, and returns the configured clock and panel behaviour. Losing a
presentation or capture stream is treated as a terminal interruption rather
than continuing with invisible output.

## Build

```sh
sh apps/hardwaretest/build.sh
OUT=/tmp/hardwaretest sh apps/hardwaretest/build.sh
PACKAGE=0 sh apps/hardwaretest/build.sh
GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/hardwaretest/build.sh
```

The native SDK is loaded by the launcher; it is never bundled into the
package. The device needs a matching `libcapsulad` and the rebuilt native
runtime that understands the `colonTop`/`colonBottom` segment fields.

## Verification

`go test ./...` in this directory covers the seven-segment glyphs, the absolute
colon schedule, the held-key map, the clockwise rainbow and red flash frames,
and the tone detector. Those are host-level assertions about values sent to the
framework; they are not optical, acoustic or electrical measurements. The
device proof levels remain the ones in `docs/VERIFICATION.md`.
