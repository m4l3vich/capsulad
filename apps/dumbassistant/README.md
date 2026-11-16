# Dumb Assistant

An optional first-party assistant package for capsulad. It claims the
`voice_assistant` role but performs no speech recognition: it listens to real
microphone PCM, decides when the user has stopped speaking, then plays a WAV
reply after a configurable delay. It is ordinary supervised application code, not
a system app. Nothing installs, starts or selects it automatically.

## Runtime and requirements

The app is a standalone Go module (`capsulad/apps/dumbassistant`) that depends only
on the `capsulad/sdk/capsulad` Go SDK, with no daemon, gRPC or protobuf imports. The
SDK loads the shared engine `libcapsulad` through cgo; the launcher injects
`CAPSULAD_LIBRARY` and the package contains no copy of the library.

The manifest declares:

- `runtime = "native"`, `architecture = "aarch64"`, `abi = "linux-gnu"` for the
  device; a host build substitutes the host architecture;
- `[native_sdk] abi_major = 1` and the capability groups the app uses: audio
  capture/playback and the PCM fast path, presentations, routing, assistant,
  settings, and AppBus connect/events/notifications;
- `provides = ["voice_assistant"]`, `single_instance = true`,
  `restart_on_failure = true`, and no `start_on_boot`: the daemon starts it only
  while it is the selected assistant.

A missing or incompatible native runtime rejects the package at install and
launch. The app also needs a ring presentation, ring capture and the processed
microphone endpoint; without the ring it cancels the turn instead of listening
invisibly.

## Manifest contract

`[[actions]]` declares `dismiss` ("Dismiss the current turn"). The daemon invokes
it when the circle/play-pause key is pressed during a turn; the app cancels
listening, the reply delay or playback and drops activations queued behind it.
The process stays running and ready.

| Setting | Type | Default | Meaning |
|---|---|---|---|
| `delay` | text | `1000` | Empty waits no time; one decimal value waits that many milliseconds; `200-1500` waits a uniformly random time in the inclusive range. Bounds are `0…60000`; a range must ascend. |
| `reply_asset` | text | empty | Path relative to the shared asset store, for example `audio/assistant/reply.wav`. Empty uses the packaged `assets/reply.wav`. |

Settings are delivered live as typed app-settings pushes. The app validates a
change, keeps the previous value and logs the reason when it is malformed. The
daemon applies the same delay grammar and asset checks when the setting is saved,
so the Web UI shows the error instead of silently accepting it.

### Reply asset rule

The shared store is a plain directory (`<data-dir>/assets`, exposed read-only as
`/assets` and named by `CAPSULAD_ASSETS`). There is no asset manifest and no
registration step: any regular file under the root is an asset, addressed by its
slash-relative path.

A configured `reply_asset` must:

- be relative, with no NUL byte, no absolute path and no `..` component;
- end in `.wav` (the extension is the media-type contract);
- resolve inside the store without passing through a symlink component;
- be a regular file that parses as PCM WAV.

The reply decoder accepts only 16-bit PCM WAV with a positive sample rate and
channel count and a frame-aligned data chunk. Empty always means the packaged
`assets/reply.wav` with no store lookup. An invalid or disappeared asset cancels
the turn; the app never plays silence as a fallback. Upload alternates through
the Web UI Assets page (`POST /api/assets?path=audio/assistant/reply.wav`) or
copy them into the store directly; only the daemon's admin API writes it.

## Build

From the repository root:

```sh
make dumbassistant      # package directory + build/apps/org.capsulad.dumbassistant-<version>.tar.gz
```

That target builds the Go module with cgo, copies `manifest.toml` and
`assets/reply.wav`, stages `build/apps/org.capsulad.dumbassistant`, and packages it on
the build host with `capsulad-cli app package` into
`build/apps/org.capsulad.dumbassistant-<version>.tar.gz`. Standalone,
`make build` here writes the package directory, `make` or `make package` also writes
the archive beside it, and `make test` runs the unit tests; the module resolves the
SDK through `replace capsulad/sdk => ../../sdk/go`. A cross build packages with the
host-executable `build/bin/capsulad-cli-host` (`make capsulad-cli-host` in the
repository root), because the cross-target CLI cannot run on the build host.

A device binary is aarch64 glibc 2.33, not musl. The SDK's cgo shim means a cross
build needs a target-compatible compiler and libc no newer than 2.33:

```sh
cd apps/dumbassistant
make package GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc CGO_ENABLED=1
```

`make package` builds the directory and writes
`../../build/apps/org.capsulad.dumbassistant-<version>.tar.gz` beside it, using the
host-executable CLI (`make capsulad-cli-host` in the repository root). Build in an arm64
Bullseye container when no suitable cross toolchain is installed, and check the result imports
no symbol newer than `GLIBC_2.33`.

## Package, install and select

```sh
capsulad-cli -data-dir build/demo app install \
  build/apps/org.capsulad.dumbassistant-<version>.tar.gz
capsulad-cli -data-dir build/demo app list
capsulad-cli -data-dir build/demo app runs org.capsulad.dumbassistant
```

`make dumbassistant` already produced that archive on the build host, so the device
only installs it. Install requires the app to be stopped; a replacement with the same
ID is an atomic update, and the daemon must restart before the new package is
visible. To include it in a coordinated rollout, run `make dumbassistant` and then
`make deploy-bundle`, which copies `build/apps/*.tar.gz` into
`build/deploy-bundle/apps/`; apply that bundle with `capsulad-cli deploy
build/deploy-bundle`. Bundle assembly never packages an app.

Select it in the Web UI under **Settings → Voice assistant**: selecting it starts
the package, while another assistant or **none** leaves it stopped. The slot is a
selected default provider, so an abnormal exit is recovered with backoff;
deselecting, uninstalling or shutting the daemon down is the off switch.

## Observable behavior

- Wakeword, talk-button and API activations are queued (8 deep) and handled one
  at a time; expired queued activations are dropped.
- A turn claims a ring presentation and capture, opens a conversation (so other
  playback is ducked), and opens the processed microphone stream.
- While listening, all 12 ring pixels glow teal `#008080`; brightness follows the
  real captured RMS over −60…0 dBFS with fast attack and slower release. There is
  no synthetic level.
- Speech is detected in the 300–3400 Hz band by modulation against a room floor
  learned from the pre-activation audio. Onset needs 60 ms of sample time above
  the floor; an utterance ends after 700 ms in which the band level has fallen
  clearly below the recent voiced level.
- A reply needs 140 ms of voiced utterance time with at least 40 ms of
  articulation, or 400 ms of steady voiced level well above the room; a short
  unmodulated room bump is rejected without a reply.
- A 3 s no-speech window cancels the turn; confirmed speech keeps it open.
  Reaching the 10 s listen cap also cancels with a full cleanup.
- A hold-to-talk touch turn ends on the framework release event. A release that
  arrives while the turn is still queued is matched by activation ID.
- At speech end the ring transitions to a blue chase for the delay and reply; the
  delay starts after speech ends and the transition never adds a wait.
- Global mute, microphone watchdog notices, daemon shutdown, a closed stream or
  an unavailable audio path cancel the turn. Ring, microphone, conversation and
  playback are always released.

These are level and modulation rules, not a speech classifier. A perfectly steady
tone or hiss above the room, held for 400 ms, is answered like a sustained vowel;
a room that starts mid-turn and never falls away is bounded by the no-speech
window or the 10 s cap rather than ended early; quiet speech buried under loud
music can be missed. The app does not cancel music, perform AEC or identify
speakers.

## Verification

```sh
cd apps/dumbassistant && go test ./...

cd ../.. && go test -ldflags '-linkmode=external' ./internal/daemon \
  -run 'TestDumbAssistant' -count=1
```

The daemon contract tests install the real package, select it, feed simulator
PCM and assert: no reply before speech, real teal level frames, speech end before
the delay, blue chase during it, the configured store asset, synchronous
rejection of malformed delays and unsafe asset paths, and clean release on
no-speech and mute. Physical acoustics, LED gamma and real-button timing require
device measurement rather than captured PCM and the virtual ring.