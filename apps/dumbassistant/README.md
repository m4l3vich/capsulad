# Dumb Assistant

An optional first-party assistant package. It gives the `voice_assistant` role,
but performs no recognition: every activation gets the same packaged audio file
after a configurable delay. It is deliberately ordinary application code, not a
system service. The package is never installed or selected automatically; an
operator installs it like any third-party app and then picks it as the voice
assistant.

The app is a standalone Go module (`capsula/apps/dumbassistant`) that depends
only on the `capsula/sdk/capsula` Go SDK. It does not import the daemon,
generated framework code, gRPC or protobuf. The SDK loads the shared Capsula
library (`libcapsula`); the app contains no transport implementation and the
package bundle contains no copy of the library. The launcher injects the
library path in `CAPSULA_LIBRARY`, and the manifest declares the optional
`native_sdk` ABI 1 requirement.

## Behaviour

- Wakeword and talk-button activations are queued and answered one at a time.
- While a reply is being handled, the whole LED ring glows teal (#008080) with a
  short fade-in. The ring capture and presentation are released when the reply
  finishes, restoring the previous ring selection.
- The delay runs before the conversation and playback open, so media keeps
  playing at full level until the reply actually starts.
- The reply is `assets/reply.wav`, shipped inside the package. It must be PCM
  16-bit WAV (any positive sample rate and channel count).
- The reply ducks other mixer playback while it plays.
- If the package is not selected as the voice assistant, the daemon does not
  start it.

## Settings

| Key | Meaning |
|---|---|
| `delay_mode` | `constant` uses `delay_ms`; `random` picks uniformly from `delay_min_ms`..`delay_max_ms` |
| `delay_ms` | Constant wait before the reply, in milliseconds |
| `delay_min_ms` | Lower random bound, in milliseconds |
| `delay_max_ms` | Upper random bound, in milliseconds; a reversed pair is swapped at use time |

All delays are clamped to 0..60000 ms.

## Build and test standalone

From this directory, with no root-module setup:

```sh
make build   # writes ../../build/apps/org.capsula.dumbassistant/
make test    # go test ./...
```

`make build` compiles the release binary with `-trimpath` and strips it, then
copies `manifest.toml` and `assets/reply.wav` into the package directory. The
resulting package contains only the app binary, manifest and assets.

The module resolves the SDK through `replace capsula/sdk => ../../sdk/go`.
Downstream users copying this app out of the repository can drop that replace
and depend on a published `capsula/sdk` version instead.

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

## Replace the reply

Replace `assets/reply.wav` with another 16-bit PCM WAV, rebuild, and reinstall
the package. The binary rejects missing, truncated, non-PCM, non-16-bit or
frame-misaligned files at startup instead of playing silence.
