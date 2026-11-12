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
  microphone stream with `OpenMic`. The framework's activation pre-roll is used
  for the initial level display but is never mistaken for speech from this turn.
- While listening, every ring pixel is teal (`#008080`). Brightness follows the
  real captured PCM level (RMS mapped over -60..0 dBFS), with fast attack and a
  slower release. There is no synthetic level generator.
- Speech end comes from the captured PCM: sustained energy above a learned
  room-noise floor starts an utterance, 700 ms of quiet near that floor ends it,
  and a blip shorter than 140 ms is rejected as noise. The activation pre-roll
  calibrates the floor before speech is classified, and it keeps adapting while
  listening. The absolute thresholds remain the floor for a quiet room; a steady
  fan or traffic raises the onset and release thresholds instead of holding the
  turn open until the listen cap. A 3 s no-speech window cancels the turn without a
  reply. Continuous speech that reaches the 10 s listen cap also cancels the
  turn with a full cleanup instead of pretending the utterance ended, so a
  reply delay can never start while the user is still speaking. These are
  safety limits; the normal end-of-speech decision is not a fixed listen timer.
- For hold-to-talk touch turns, the framework's `TouchRelease` event ends the
  utterance. A late release also ends a turn that was already using speech-end
  detection, so a mode change cannot wedge the capture.
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
# App unit tests: delay grammar, store containment/catalog and speech tracker.
cd apps/dumbassistant && go test ./...

# Root-level contract tests: real package, real daemon, simulator PCM capture.
cd ../.. && go test ./internal/daemon -run 'TestDumbAssistant' -count=1 -v
```

The daemon contract tests install the real package, select it as the assistant,
inject real 16 kHz PCM through the simulator microphone, and assert: no reply
before speech, real teal level frames, speech-end detection before the delay,
blue chase frames during the delay, the configured store asset, synchronous
rejection of malformed/inverted/overflow delays and unsafe asset paths, and
clean release on no-speech and mute.

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
