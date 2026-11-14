# Dumb Assistant

An optional first-party assistant package. It gives the `voice_assistant` role,
but performs no recognition: it listens to real microphone PCM, waits until the
user stops speaking, then plays the same packaged WAV reply after a configurable
delay. It is deliberately ordinary application code, not a system service. The
package is never installed or selected automatically; an operator installs it
like any third-party app and then picks it as the voice assistant.

The app is a standalone Go module (`capsula/apps/dumbassistant`) that depends
only on the `capsula/sdk/capsula` Go SDK. It does not import the daemon,
generated framework code, gRPC or protobuf. The SDK loads the shared Capsula
library (`libcapsula`); the app contains no transport implementation and the
package bundle contains no copy of the library. The launcher injects the
library path in `CAPSULA_LIBRARY`, and the manifest declares the optional
`native_sdk` ABI 1 requirement.

## Behaviour

- Wakeword, talk-button and API activations are queued and handled one at a time.
- A turn claims the ring presentation and capture, then opens the standard
  microphone stream with `OpenMic`. Application microphone capture is the
  framework's processed application tap (high-pass, conservative
  stationary-noise suppression and a peak limiter). The internal wakeword feed is
  a separate daemon path with its own documented tap; this app only ever sees the
  application tap. The activation pre-roll is used for the initial level display
  but is never mistaken for speech from this turn.
- While listening, every ring pixel is teal (`#008080`). Brightness follows the
  real captured PCM level (RMS mapped over -60..0 dBFS), with fast attack and a
  slower release. There is no synthetic level generator.
- Speech end comes from the captured PCM in the speech band (300-3400 Hz), not
  from full-band energy: a real room's fan, air conditioner or traffic is a loud
  low-frequency roar that no full-band threshold can tell from a voice. Speech is
  recognized by *modulation*: a voice moves the band level by tens of percent
  within a few hundred milliseconds (syllables, consonants), while hums, hisses
  and buffeted room noise hold their band level steadier. Onset needs 60 ms of
  sample time with the band above a floor learned from the room and modulated.
  An utterance ends when the band level has fallen clearly below the recent voice
  level and stayed there for 700 ms, so the same room returning after speech ends
  the turn while a sustained voiced vowel, which keeps its level, is preserved
  until it actually stops. A reply needs 140 ms of voiced utterance time with at
  least 40 ms of articulation, or 400 ms of steady voiced level well above the
  room (a held vowel). A short unmodulated room event (a bump, a door) carries no
  articulation and is rejected without a reply. Onset, end and noise rejection
  all use the same band level and floor, so they cannot disagree. Only the oldest
  part of the activation pre-roll (at least 400 ms before the activation)
  calibrates the floor, using a low percentile, so the wakeword itself cannot
  become the background. Without any pre-roll the cold floor stays at the
  digital-silence value and a level-only onset is allowed once, so a turn that
  opens on a voice already in progress is not swallowed by its own floor; a
  capture gap keeps the learned floor and only discards stream state.
  A 3 s no-speech window cancels the turn without a reply; only confirmed speech
  keeps that window open, so a rejected noise hypothesis cannot extend a turn.
  Continuous speech-like audio that reaches the 10 s listen cap also cancels the
  turn with a full cleanup instead of pretending the utterance ended, so a reply
  delay can never start while the user is still speaking. These are safety
  limits; the normal end-of-speech decision is not a fixed listen timer and is
  independent of transport chunk size. Thresholds are relative to the learned
  band floor, so a change in the framework tap's suppression depth or high-pass
  corner needs no retune.

  Measured limits (fixtures matched to the authorized capture's level statistics,
  plus backed-up word takes; not an acoustic-certification claim): a word whose
  300-3400 Hz band level is at least about 3x the room's band level is answered;
  at about 1.5-2x it is marginal, and below the room's band level it is
  physically buried. A room that begins mid-turn and whose band level never
  falls away from the speaker's is bounded by the no-speech window or the 10 s
  cap (cancel, no reply) rather than ended early. The one authorized ambient
  capture turned out to be music from external speakers, not a speech-free room;
  music and vocals are legitimately speech-like, so an endpointer responding to
  them is not a false-positive result, and this app adds no music cancellation.
  A perfectly steady tone or hiss cannot be separated from a held vowel by this
  tap and, if it holds and then stops, is answered like one — including when the
  turn started without any pre-roll; that limitation is pinned by a test.
- For hold-to-talk touch turns, the framework's `TouchRelease` event ends the
  utterance. The touch activation itself carries `hold_to_talk`, so the app
  applies the per-turn mode without reading the administrator-only device
  settings service. A late release also ends a turn that was already using
  speech-end detection, so a mode change cannot wedge the capture.
- When speech ends, the ring smoothly transitions to a blue chase. The chase
  runs through the delay and the reply. The delay starts only after speech ends.
  The transition is time-based and independent of the delay: an empty or short
  delay neither skips it nor adds a wait; the blend can continue into playback.
- The app begins a conversation while listening so other mixer playback is
  ducked; the conversation closes with the turn.
- The manifest declares a `dismiss` action. The daemon invokes it when the
  circle/play-pause key is pressed during a turn; the app cancels listening, the
  reply delay or playback, drops activations queued behind it, and releases the
  ring, microphone, conversation and playback through the normal teardown. The
  process stays running and ready for the next activation.
- Global mute, microphone watchdog notices, daemon shutdown, a closed capture
  stream and unavailable audio all cancel the turn. The microphone, ring capture
  and ring presentation are always released; no capture is left claimed.
- The reply is a 16-bit PCM WAV. Empty `reply_asset` uses the packaged
  `assets/reply.wav`; a configured value uses an asset registered in the shared
  read-only `/assets` store. It must be PCM 16-bit WAV (any positive sample rate
  and channel count).
- If the package is not selected as the voice assistant, the daemon does not
  start it.

## Settings

| Key | Type | Default | Meaning |
|---|---|---|---|
| `delay` | text | `1000` | Empty waits no time. A single value waits that many milliseconds (`100` = 100 ms). A range like `200-1500` waits a uniformly random time between the bounds, inclusive. |
| `reply_asset` | text | empty | Path relative to the shared read-only `/assets` asset store, declared in its `manifest.json` catalog, for example `audio/assistant/reply.wav`. Empty uses the packaged `assets/reply.wav`. |

`delay` accepts only decimal milliseconds in `0..60000`. Empty is valid and
means zero. A range must ascend; `1500-200`, negatives, decimals, exponents,
missing bounds, extra hyphens, values above `60000` and other malformed input are
rejected. The app keeps the last valid value and logs the reason; the daemon's
assistant settings validator applies the same rules to
`UpdateAppConfiguration`, so the Web UI can show the error and the previous
value stays active.

`reply_asset` is scoped to the daemon's shared read-only app asset store. Apps
see it at `/assets`; the launcher passes the store root as `CAPSULA_ASSETS`
(`/assets` in the sandbox, the host assets directory for unsandboxed runs). The
value is relative to that root and must be declared in the store's
`manifest.json` catalog. Absolute paths, NUL bytes, any `..` component, symlink
components, missing or non-regular files, unregistered assets and non-WAV media
types are rejected. Empty always means the packaged `assets/reply.wav`, with no
store lookup. The app validates these rules when it loads the asset; the
daemon's assistant settings validator applies the same catalog and containment
checks to `UpdateAppConfiguration`, so the Web UI can show the error and the
previous value stays active.

### Shared asset store

The store is administratively managed and read-only to apps. A minimal
registration looks like:

```text
<assets-dir>/
  audio/assistant/reply.wav
  manifest.json
```

```json
{
  "assets": [
    {
      "id": "assistant.reply",
      "path": "audio/assistant/reply.wav",
      "media_type": "audio/wav"
    }
  ]
}
```

The daemon normalizes `path` to `/assets/audio/assistant/reply.wav`. The app
matches that canonical path exactly; it reads the manifest and the WAV through
the standard read-only store mount and never resolves host paths.

The obsolete `delay_mode`, `delay_ms`, `delay_min_ms` and `delay_max_ms` keys were
removed with no aliases. Existing stored values under those keys are ignored;
the new `delay` default is `1000` ms. Use an explicit `delay` value to keep a
different wait.

## Build and test standalone

From this directory, with no root-module setup:

```sh
make build   # writes ../../build/apps/org.capsula.dumbassistant/
make test    # go test ./...
```

`make build` compiles the release binary with `-trimpath` and strips it, then
copies `manifest.toml` and `assets/reply.wav` into the package directory. The
resulting package contains the app binary, manifest and default reply; extra
configured replies live in the shared asset store, not the app package.

The module resolves the SDK through `replace capsula/sdk => ../../sdk/go`.
Downstream users copying this app out of the repository can drop that replace
and depend on a published `capsula/sdk` version instead.

### Verification

```sh
# App unit tests: delay grammar, store containment/catalog and the speech-band
# detector against deterministic speech, colored-room and steady-noise fixtures.
cd apps/dumbassistant && go test ./...

# Root-level contract tests: real package, real daemon, simulator PCM capture.
cd ../.. && go test -ldflags "-linkmode=external" ./internal/daemon \
  -run 'TestDumbAssistant' -count=1 -v
```

The daemon contract tests install the real package, select it as the assistant,
inject real 16 kHz PCM through the simulator microphone, and assert: no reply
before speech, real teal level frames, speech-end detection before the delay,
blue chase frames during the delay, the configured store asset, synchronous
rejection of malformed/inverted/overflow delays and unsafe asset paths, and
clean release on no-speech and mute. `TestDumbAssistantEndsAfterSpeechOverColoredRoom`
feeds a synthetic colored, level-swinging room around a modulated speech fixture
and requires the turn to end and reply once; `TestDumbAssistantColoredRoomNoiseProducesNoReply`
requires the same room alone to reach a safety exit with no reply, and
`TestDumbAssistantRoomSpliceDoesNotReply` forces a producer stall so the
simulator's silent-read splice is exercised end to end. The injector is
consumption-driven, not wall-clock paced: it keeps a bounded four-frame lead,
refills on each frame the capture pipeline consumes, and blocks when the lead is
full, so a loaded host cannot splice silence into the room by starving a timer
(it can only stall the producer, which the splice test covers).

### Hardware target

The device runtime is **aarch64 glibc 2.33** (`/lib/ld-linux-aarch64.so.1`),
not musl; the manifest declares `architecture = "aarch64"` and
`abi = "linux-gnu"`. The SDK loads the shared engine through cgo, so hardware
binaries must be linked by a target-compatible aarch64 glibc toolchain or
container whose libc is no newer than 2.33. A plain host Go cross build without
that compiler/sysroot is not sufficient. For example:

```sh
make build GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc CGO_ENABLED=1
```

or run `make build` inside the glibc <= 2.33 target build container. Confirm the
result imports no `GLIBC_2.34`-or-newer symbol (`readelf -V` on the binary).

Microphone thresholds, perceived ring brightness and the blue chase were
validated through captured PCM and the simulator's virtual 12-pixel ring. The
actual acoustic threshold, physical LED gamma and real talk-button timing remain
unverified on hardware.

## Install and select

Install and select it like a third-party package; nothing installs or selects it
for you:

```sh
cd ../.. && make capsula
build/bin/capsula -data-dir build/demo app install build/apps/org.capsula.dumbassistant
```

Then open **Settings → Voice assistant** and choose **Dumb Assistant**. Selecting
it starts the package on demand; selecting another assistant leaves it stopped.
Reinstalling or removing it never affects the system apps or other assistants.

The example data directory's shared store is `build/demo/assets`; register any
alternate reply there as described above before setting `reply_asset`.

## Replace the reply

For the default, replace `assets/reply.wav` with another 16-bit PCM WAV, rebuild,
and reinstall the package. For a configured reply, place the WAV under the shared
store (for example `<assets-dir>/audio/assistant/reply.wav`), add or update its
`manifest.json` entry, and set `reply_asset` to the relative path. The store is
read-only to the app; the daemon/admin owns registration. The binary rejects
missing, truncated, non-PCM, non-16-bit, frame-misaligned, unregistered,
traversal or symlink-escaped assets instead of silently playing silence.
