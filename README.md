# capsulad — Capsula Universal Framework (initial implementation)

`capsulad` is a native Go implementation of the local Capsula framework/HAL daemon described in
`_devdocs/ARCHITECTURE.md` (draft 0.5). It supervises native application processes, owns the
hardware abstraction, routes dynamic presentations and captures, mixes audio, serves a
self-signed HTTPS Web UI protected by an admin password and static API keys, and exposes exactly one local gRPC surface over a Unix socket.

This is an **initial vertical slice**, not a frozen v1. The wire schemas are the draft v0.5 protobufs;
the implementation is honest about what is verified on real hardware and what is not.

## What runs today

- **One Unix-socket gRPC API** (`/run/capsulad/hal.sock`) implementing all 15 draft services:
  device, settings, routing, presentation, audio, input, assistant, application,
  application-configuration, app-interface, app-bus, sensor, system, diagnostics, asset.
- **Supervised native applications** with manifest validation, unique run IDs, graceful stop,
  restart backoff, stdout/stderr collection and singleton invocation semantics (queued, bounded,
  expiring).
- **Reverse-RPC app bus**: one long-lived `Connect` stream per app; routed unary and streaming
  `Call` relays with deadlines, cancellation and bounded queues. Apps never listen on IPC sockets.
- **Shared native SDK**: a pure Rust `libcapsula` cdylib exposes a frozen C ABI v1 through one
  bounded `capsula_get_api` table. It owns the UDS gRPC transport, protobuf/JSON reflection for all
  framework unary, client-streaming, server-streaming and bidi calls, managed AppBus sessions and
  routed calls, and a raw-PCM audio fast path. Thin Go and Rust bindings load the library
  dynamically; C and C++ consumers use the installed `capsula/capsula.h` header directly. No Go
  runtime or per-language transport stack is linked into the library.
- **Dynamic presentations and captures** with exact first-content readiness, last-eligible-capture
  wins, baseline fallback, out-of-order release, force release and process-exit cleanup.
- **Touch partial/full modes** with mutually exclusive ownership, framework volume/microphone
  controls, raw/gesture delivery and clean cancellation on ownership changes.
- **Audio mixer/fan-out** with software mixing, conversation ducking, bounded capture queues,
  discontinuities, pre-roll for activations and the visible-indication microphone watchdog.
- **Four-page Web UI**: Overview (Bluetooth state and discoverability, now-playing metadata, volume,
  installed applications), Settings (default display/ring selection among installed apps, with an
  empty selection keeping the built-in providers; brightness, assistant and touch behaviour,
  microphone and muted indication, device timezone), Access (admin password, static API keys,
  public certificate download), Diagnostics (hardware, firmware, software, runs, resources, logs).
- **Built-in clock and ring providers**: the segment clock and LED-ring effects are daemon-internal,
  not installed packages, and remain the baseline whenever no external provider is selected. Their
  app-configuration keys are still available without a package: `capsula.clock` (`blink`,
  `timezone`) and `capsula.ring` (`mode`).
- **Media-player contract without a bundled provider**: the standard
  `capsula.contracts.media.v1.MediaPlayerService` app-bus plumbing remains available to external
  apps — unary commands plus a server-streaming `WatchPlayers` whose first event is a snapshot — but
  this repository no longer ships a sample media app. The Web UI and `capsula media list` show
  whatever providers an operator installs; no app listens on its own socket.
- **Optional dumb assistant** (`org.capsula.dumbassistant`): an ordinary first-party package, not a
  system app and not part of the default build. It lives in its own nested Go module and is built
  only by the explicit `dumbassistant` target. It claims `voice_assistant` and answers every
  activation with the same packaged WAV, either after a constant delay or a uniformly random delay
  in a configured range; while it handles a prompt the LED ring glows teal. It performs no
  recognition, and it is neither installed, selected nor started unless an operator chooses it.
- **Framework-owned boot service**: `deploy/rockchip-stock/S60capsulad` starts the daemon after
  mount, microphone-gate and PulseAudio init, gates on `capsulad -health`, and starts the preserved
  vendor chain if the daemon cannot become healthy. The stock assistant init scripts are retired on
  the deployed unit.
- **In-place daemon upgrades**: `capsula daemon install -binary PATH` proves the replacement runs,
  stages it beside the live binary, swaps by rename, restarts through the boot service and checks
  the framework API; any failure restores the previous binary. `capsula daemon rollback` restores
  the preserved one.
- **Bounded operational footprint**: the daemon log rotates at 512 KiB and keeps one backup, the
  negative-training corpus path is configurable and is not a runtime dependency, and new wakeword
  recordings or training are refused below 2 MiB of free `/data`.
- **Bluetooth through BlueZ**: the daemon owns adapter power, discoverability and pairability over
  the system D-Bus, lists paired devices and unpairs them from the Web UI (an administrator-only
  operation, refused for supervised apps). It loads PulseAudio's Bluetooth discovery so the adapter
  advertises A2DP Sink and accepts an audio source, then routes that source to the speaker.
- **Bluetooth media mode**: AVRCP play/pause/next/previous and absolute volume go to the connected
  source, and its track metadata is read when the source exposes it. A provider app publishes this
  through the standard `MediaPlayerService`, so the Web UI and other apps see a normal
  `bluetooth` player; a blue ring pulse marks connect and disconnect.
- **Physical controls**: the circle (play/pause) key is the talk button — hold it past 700 ms to summon
  the assistant, release to end the turn, short taps do nothing. Volume keys step 5 %, ramp while held
  (200 ms decaying to 60 ms) and stop at the bounds; each change plays a synthesized percussive tick at
  the new level and shows the percentage on the segment display for a second.
- **Personal wakeword training from the browser**: record on the device, then run waveform
  augmentation, pretrained speech embeddings, hard-negative training and independent quality gates
  on the device. The browser only displays progress; no Python, ONNX runtime or GPU is needed there.
- **Muted-microphone indication** on the ring: a red run on the bottom three pixels that fades in
  and out, independent of the mute state itself. The microphone key is a press-edge toggle; volume
  keys step once per press.
- **Device timezone** set in the Web UI, inherited by apps as `TZ`; the built-in clock provider
  follows it, and changing it restarts boot applications to apply the new zone.
- **Typed settings** with compare-and-swap revisions, partial updates, masked storage and delivery
  to the owning app only.
- **HTTPS Web UI** with a self-signed server certificate, Argon2id admin password with
  server-side sessions, static opaque API keys for automation, CSRF/origin defenses, local
  credential management and read-only security diagnostics.
  This intentionally replaces the draft's mandatory client-certificate mTLS at the project
  owner's request. API keys protect only the HTTP surface; the HAL/app bus remains Unix-socket
  only.
- **Optional GitHub artifact-attestation verification** for native binaries with an offline
  Sigstore trusted root (digest, DSSE signature, certificate chain, transparency log, workflow
  identity). Verified provenance is never presented as a safety guarantee.
- **Simulator backend** with virtual segment display, LED ring, contacts and audio, plus local
  development injection hooks.
- **Rockchip WS2812B ring and IS31FL3236 segment output** using the ioctl ABI and wiring
  permutations recovered from the stock `voice-agent`, with sysfs and in-memory fallbacks.
- **Rockchip device-tree-driven input and sensors**: all five gpio-keys (volume, microphone,
  play/pause, voice) plus ambient-light and SoC/logic thermal sensors.

## What is intentionally not claimed

- **QuickJS and MicroPython host runners are not implemented.** `ListRuntimes` reports them
  unavailable and the supervisor refuses those packages. No fake interpreter is shipped.
- **Rockchip vendor LED ABI is recovered, measured and partly validated.** The device tree
  describes a 36-channel WS2812B chain (`/dev/topleds`, 12 RGB ring pixels) and an IS31FL3236
  (`/dev/led_multi_ctrl`, 30 monochrome display cells); `GET_COUNT` returns exactly 36 and 30 on a
  live unit, the segment channel map came from `cmd/ledmap`, the clock face renders correctly, and
  the ring shows pixel 0 at 12 o'clock, travels clockwise and renders RGB correctly. Gamma is
  unmeasured (no photometer). Standard Linux `sysfs` LED class devices and evdev remain fallbacks.
- **Amlogic stock/Alpine backends are not implemented.** Selecting those platforms fails explicitly
  instead of pretending to be hardware.
- **The Web UI certificate is self-signed.** Browsers show a trust warning until the operator
  verifies and trusts the fingerprint. Password recovery is a local-console CLI operation.
- **Personal wakeword models require setup and evaluation.** Native incremental speech-embedding
  inference runs on Rockchip, but legacy mel models must be retrained. There is no bundled,
  speaker-independent Russian keyword head. See [wakeword setup and evidence](docs/WAKEWORD.md).
- **Sandbox strength is probed, never assumed.** Linux user+mount namespace isolation, read-only
  mounts and a seccomp deny list are implemented, but the RK3308 stock kernel (4.4.167) offers only
  mount namespaces: `CLONE_NEWUSER`, `CLONE_NEWNET` and `PR_SET_SECCOMP` all fail on a live unit.
  The daemon probes both namespaces and the filter at startup, runs apps in a root mount namespace
  with a distinct UID, and reports `mode=mountns` rather than claiming isolation it does not have.
- Persistent app storage beyond typed settings, per-app RBAC, remote gRPC and large Capsula/Neo
  drivers remain out of scope, as the architecture specifies.

## Build and run (simulator)

```sh
make                 # release profile: daemon, CLI, Go/Rust SDK bindings, native library and header
make debug           # same targets, unstripped
mkdir -p build/demo
./build/bin/capsula -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
./build/bin/capsulad -platform simulator -data-dir build/demo -dev-admin build/demo/run/dev.sock
```

`make` is the release profile: Go programs are built with `-trimpath -s -w`, the native library
and Rust binding use Cargo's release profile, and the Go binding is built into its nested module.
`make debug` builds the same targets without stripping. Programs and the native runtime are written
to `build/bin`, `build/lib` and `build/include`; the Rust binding uses its own Cargo target
directory. Builds are local artifacts and nothing here deploys them to a device.

The clock face and LED-ring behaviour are internal to `capsulad`; they are not installable packages
and need no `app install`. Their settings remain on the app-configuration surface even though no
package exists, and an empty default display/ring selection means this internal baseline:

```sh
capsula -data-dir build/demo settings app capsula.clock blink=true timezone=Europe/Moscow
capsula -data-dir build/demo settings app capsula.ring mode=ambient
```

`capsula.clock` accepts `blink` (boolean, default true) and `timezone` (IANA name, default empty,
which follows the device timezone). `capsula.ring` accepts `mode=ambient|spinner|rainbow`. Legacy
defaults `org.capsula.clock`, `org.example.ring-controller` and `org.capsula.assistant` migrate to
the internal baseline at boot, and the old clock settings are copied to `capsula.clock` once.
`capsula settings show` and the Web UI default-provider dropdown list only installed packages; an
empty default keeps the built-in providers.

An upgrade quarantines previously installed legacy bundles
(`org.capsula.clock`, `org.example.ring-controller`, `org.capsula.assistant`, `org.capsula.media`):
they are not started, manually launched, selectable or listed, persisted defaults pointing at them
are cleared, and the daemon warns once at boot. Their files are retained non-destructively, so
upgrading the daemon suppresses the processes but does **not** reclaim their disk bytes. Seeing the
directories in a manual `ls` does not mean they are active. An operator may remove those bundle
directories explicitly to reclaim space; `capsulad` never auto-deletes installed app or operator
data. Quarantine affects only those retired sample IDs: custom/third-party providers and generic
interfaces are unaffected. The built-in baseline is internal (`capsula.builtin`), not an installed
application.

The optional dumb assistant is deliberately separate:

```sh
make dumbassistant
./build/bin/capsula -data-dir build/demo app install build/apps/org.capsula.dumbassistant
```

That nested project is not built, installed, selected or started by `make`, `make release`,
`make debug` or the simulator bootstrap.

The shared native SDK is emitted beside the binaries:

```text
build/lib/libcapsula.so.1.0.0  # Linux real file, SONAME libcapsula.so.1
build/lib/libcapsula.so.1      # symlink; libcapsula.so is the link-time alias
build/lib/libcapsula.1.dylib   # Darwin real file; libcapsula.dylib is the alias
build/lib/capsula-native.json
build/include/capsula/capsula.h
```

`capsula-native.json` records the ABI major/minor, the capability vocabulary and the exact library
file name. The daemon finds the runtime in `<exe-dir>/lib` or `<exe-dir>/../lib`, or from the
explicit `-native-lib-dir DIR` flag; the CLI also accepts `-native-lib-dir DIR` and
`CAPSULA_NATIVE_LIB_DIR`, and looks in `<data-dir>/lib` first. An installed device layout therefore
puts the library and metadata in `/data/capsdemo/lib` next to `/data/capsdemo/capsulad`. Apps that
declare `[native_sdk]` are refused when a compatible runtime is absent. Do not copy the library
into each app package.

C consumers include the header and link the versioned library; C++ consumers use the same header and
load it with `dlopen`. The ABI is plain C with integer handles and library-owned buffers, so no C++
class crosses the boundary.

For a Linux device cross-build, select the Go target explicitly so the linker choice follows the
target rather than the host:

```sh
make daemon GOOS=linux GOARCH=arm64
```

`make daemon` builds only `capsulad` and the CLI, so it needs no native SDK or cgo cross toolchain.
To cross-build the shared native SDK too, set `NATIVE_TARGET` to a Rust triple for the intended ABI
and install that target plus a matching linker/sysroot; for example:

```sh
make release GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu
```

The native target fails actionably if `NATIVE_TARGET` is unset or Cargo does not produce the
library, rather than packaging the host library. The Go SDK binding uses cgo, so its cross build
needs the same C toolchain. These commands produce local build artifacts; they are not a complete
device deployment.

### Hardware ABI and reproducible native release

The Capsula Mini runs glibc 2.33 with `/lib/ld-linux-aarch64.so.1`; it does **not** use musl. Build
the hardware shared library against glibc 2.33 or older, in an arm64 Debian Bullseye (glibc 2.31)
container, not Ubuntu 22.04/24.04:

```sh
# Daemon and CLI, glibc 2.31 baseline.
docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src golang:1.22-bullseye \
  make daemon-release GOOS=linux GOARCH=arm64

# Shared native SDK, same glibc baseline and the device's gnu ABI.
# Replace the Rust tag with the newest Bullseye-based image that satisfies the
# current Cargo dependencies; a bookworm/newer-libc image would raise the floor.
docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src rust:1.85-slim-bullseye \
  make native GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu
```

Inspect the resulting version requirements before installing anything:

```sh
readelf --version-info build/bin/capsulad | grep -o 'GLIBC_[0-9.]*' | sort -u
readelf --version-info build/lib/libcapsula.so.1.0.0 | grep -o 'GLIBC_[0-9.]*' | sort -u
```

Every listed `GLIBC_*` symbol must be 2.33 or older. A `dlopen` smoke test in a
`debian:bullseye` arm64 container is the loader-level check; neither command deploys to the device.
Native app packages that use this SDK must be built for glibc 2.33 or older as well.

`auth init` creates the self-signed certificate, sets an admin password (generated and printed
once unless `-password` is supplied) and issues a bootstrap API key. Open
`https://127.0.0.1:8443/`, accept the certificate after verifying the fingerprint shown by
`capsula cert status`, and sign in with the admin password. Automation can use the API key:

```sh
curl -k -H "X-API-Key: capsula_…" https://127.0.0.1:8443/api/status
```

Manage credentials locally:

```sh
capsula -data-dir build/demo auth set-password
capsula -data-dir build/demo auth key create automation
capsula -data-dir build/demo auth key list
capsula -data-dir build/demo auth key revoke automation
capsula -data-dir build/demo cert status
capsula -data-dir build/demo settings show
capsula -data-dir build/demo settings app capsula.clock blink=false
capsula -data-dir build/demo settings app capsula.ring mode=spinner
```

The rest of the CLI uses the framework Unix socket as before:

```sh
./build/bin/capsula -data-dir build/demo status
./build/bin/capsula -data-dir build/demo dev wakeword            # simulator activation
./build/bin/capsula -data-dir build/demo dev state               # virtual display/ring frames
./build/bin/capsula -data-dir build/demo app list
./build/bin/capsula -data-dir build/demo media list              # providers installed by the operator
./build/bin/capsulad -health -socket build/demo/run/hal.sock     # exit 0 when the API answers
```

On the device the same CLI performs staged upgrades through the boot service:

```sh
capsula -data-dir /data/capsdemo daemon install -binary /tmp/capsulad.new
capsula -data-dir /data/capsdemo daemon rollback
```

`capsulad -health` is the readiness probe used by the boot service and the upgrade path. Exit 0
means the framework API answers, exit 1 means it does not, and a pre-upgrade binary without the
flag exits 2, which the boot service treats as "running but not probeable".

Go targets whose `GOOS` is `darwin` build with `-linkmode=external` because the internal Go linker
emits Mach-O binaries that current dyld rejects (missing `LC_UUID`). The Makefile keys this off the
target `GOOS`, not the build host, so a Linux cross-build keeps the default internal linker even on
macOS.

Production paths are supplied explicitly, for example:

```sh
capsulad -platform rockchip-stock \
  -data-dir /var/lib/capsulad -socket /run/capsulad/hal.sock \
  -apps-dir /var/lib/capsulad/apps -assets-dir /var/lib/capsulad/assets \
  -native-lib-dir /var/lib/capsulad/lib \
  -trusted-root /etc/capsulad/trusted_root.json \
  -web-addr 0.0.0.0:8443 -sandbox auto
```

## Native SDK (C ABI v1)

`sdk/native` builds `libcapsula`, a pure-Rust cdylib that owns the UDS gRPC transport, protobuf
reflection, managed AppBus session and raw-PCM audio path. Go, Rust, C and C++ consumers load it
dynamically; they never link a second transport stack. The normative interface is
[`sdk/native/include/capsula/capsula.h`](sdk/native/include/capsula/capsula.h), and the runnable
examples are [`sdk/native/examples/c/device_info.c`](sdk/native/examples/c/device_info.c),
[`sdk/cpp/example.cpp`](sdk/cpp/example.cpp),
[`sdk/rust/examples/`](sdk/rust/examples) and the optional
[`apps/dumbassistant`](apps/dumbassistant) Go project. Header and package comments plus those
examples are normative; this section is the integrated summary.

### Artifacts and build

`make native` builds and stages the runtime for the Go target, or run Cargo directly:

```sh
cargo build --release --manifest-path sdk/native/Cargo.toml
cargo test  --manifest-path sdk/native/Cargo.toml
```

| Platform | Real file | SONAME / install name | Metadata `library` |
|---|---|---|---|
| Linux | `libcapsula.so.1.0.0` | `libcapsula.so.1` | `libcapsula.so.1` |
| macOS | `libcapsula.1.dylib` | `@rpath/libcapsula.1.dylib` | `libcapsula.1.dylib` |

`make native` also writes the symlinks, `build/lib/capsula-native.json`, and the single public header
`build/include/capsula/capsula.h`. The metadata is produced by
`sdk/native/emit-runtime-metadata.sh <out.json> <library-filename>`; the library filename is always
relative to the runtime directory. Cargo package is `capsula-native`, `[lib] name = "capsula"`,
`crate-type = ["cdylib"]`. `build.rs` sets the SONAME/install name. Release strips debuginfo only;
exported ABI symbols remain.

The hardware path is `aarch64-unknown-linux-gnu`, built against glibc 2.33 or older (the Bullseye
recipe above). The pure-Rust dependencies need no TLS, OpenSSL or sysroot libraries. Cross-build
compiler requirements and commands are in the contract.

### Loading and ABI negotiation

Only these symbols are exported: `capsula_api_size`, `capsula_abi_major`, `capsula_abi_minor`,
`capsula_version`, `capsula_api_v1` and `capsula_get_api`. Everything else is reached through the
function table.

```c
capsula_api table = {0};
if (capsula_get_api(CAPSULA_ABI_MAJOR, sizeof table, &table) != CAPSULA_STATUS_OK) {
  /* UNSUPPORTED_ABI or INVALID_ARGUMENT; the table was not written */
}
```

- `capsula_get_api` accepts a table of at least `CAPSULA_API_V1_MIN_SIZE` (the stable
  `abi_major`/`abi_minor`/`struct_size` prefix; 16 bytes on 64-bit), copies
  `min(table_size, capsula_api_size())`, never writes past `table_size`, and reports the library's
  full current size in `struct_size`.
- An unsupported major returns `CAPSULA_STATUS_UNSUPPORTED_ABI` without writing. `abi_major` must
  match; a newer minor may append table fields.
- `capsula_api_v1()` returns the read-only process-wide table for consumers that can use the full
  current layout.
- Handles are opaque `uint64_t` values shared by clients, calls and buses; `0` is invalid.

Both the Rust and Go loaders zero the table, ask for their compiled table size, require major 1 and
a `struct_size` covering every field they call, then reject NULL required pointers. A runtime
metadata preflight in the launcher is separate: an app's `[native_sdk]` requirement checks ABI
major/minor and capabilities at install and launch, while the binding still verifies the loaded
library at startup.

### Memory, lifetime and threading

- Output data is returned in library-owned `capsula_buf { data, len }` values and released exactly
  once with `api->buf_free`. Empty output is `{NULL, 0}`. Never free with `free`, `delete` or a Go
  finalizer.
- Input buffers are borrowed for the duration of the call only; strings are explicit
  `(pointer, length)` UTF-8 and need not be NUL-terminated.
- Errors are `int32_t capsula_status`; `capsula_last_error()` is thread-local and valid until the
  next SDK call on that thread. gRPC failures also expose the canonical code/detail through
  `call_status`.
- One bounded process-wide Tokio runtime (two workers, two blocking threads) is created lazily by
  the first `client_connect`. There is no runtime or thread pool per client.
- **The loaded library must remain resident for the process lifetime.** The global runtime owns
  library threads, so Go/Rust bindings retain a successful loader handle after the last
  client/call/bus closes and the C++ example never `dlclose`s it. Closing handles releases native
  resources, but it does not make unloading the library safe. ABI v1 has no complete joined
  shutdown, so do not unload `libcapsula` after draining clients.
- Handles are safe from arbitrary threads. A call has one logical reader: concurrent receives are
  serialized, but callers should not depend on which wins. `send`, `half_close`, `cancel`, `end`
  and `release` may run while another thread blocks in `recv`; `cancel`, release, `bus_close` and
  `client_close` unblock it.
- Callbacks run on library threads and must not unwind across the C boundary. Callback-initiated
  close of the same bus returns `BUSY` instead of deadlocking.
- `client_close` cancels and releases its children; `bus_close` ends the session, unblocks pending
  inbound reads, delivers `on_disconnected` exactly once (including an intentional close) and joins
  dispatch threads. `user_data` must outlive `bus_close`.

### Calls, streaming and AppBus

- `call_start(service, method, kind, timeout)` supports unary, server-streaming,
  client-streaming and bidi calls through the same wire path; `kind` declares intent. It returns
  immediately and the transport task streams in the background.
- Send one or more messages with `call_send_json` / `call_send_proto`, close the request direction
  with `call_half_close`, and receive with `call_recv_json` / `call_recv_proto`. `recv` returns
  `OK` (message), `END_OF_STREAM`, `GRPC`, `TIMEOUT` or `CANCELLED`; a timeout leaves the call
  usable. `call_end`, `call_cancel`, `call_retain` and `call_release` manage completion and
  references.
- JSON uses canonical protobuf JSON and requires an embedded or registered descriptor; unknown
  methods/descriptors fail explicitly. Raw protobuf always works.
- `call_start_routed` targets another supervised app through `AppBusService/Call` and accepts raw
  protobuf even when no descriptor is registered for that app's custom service.
- `bus_open` starts the managed `AppBusService/Connect` session, sends `AppHello{run_id}` followed
  by `AppReady`, and delivers `on_ready`, `on_disconnected` and `on_event` callbacks. Event kinds
  cover invocations, settings changes, actions, assistant activations and input release; payloads
  are canonical protobuf JSON except raw-PCM audio.
- `bus_ready` waits until the two frames are flushed and `on_ready` has returned. Inbound custom
  calls arrive on `on_call` on a bounded pool (4 workers, 32 queued calls, 64 live calls; overflow
  ends the call with `RESOURCE_EXHAUSTED`). The library owns the callback reference; retain the call
  for asynchronous work. A handler must call `call_end`; returning without it ends the call with
  `INTERNAL`.
- Bounded per-call/request queues (64–256 entries depending on direction) return `BUSY` or a gRPC
  `RESOURCE_EXHAUSTED` end instead of growing without limit.

### Audio fast path

`audio_capture_open`/`audio_capture_recv` and `audio_playback_open`/`audio_playback_write` move raw
PCM and frame metadata without JSON or base64 on the chunk path. Format and conversation metadata
travel once in the open control message. `frame_count == 0` derives frames from the byte length;
`monotonic_time_ns == 0` fills `CLOCK_MONOTONIC`. Playback control sends FLUSH/PAUSE/RESUME;
`call_half_close` drains playback, while cancel does not.

### Go binding

`sdk/go` is the nested module `capsula/sdk` (package `capsula/sdk/capsula`). It has zero
third-party dependencies (standard library plus cgo) and never links `libcapsula`: the cgo shim
`dlopen`s `CAPSULA_LIBRARY` (absolute path from the launcher) or the platform SONAME
(`libcapsula.so.1`, Darwin `libcapsula.1.dylib`), validates ABI major 1, and keeps the successfully
loaded engine resident for process lifetime. Closing a client frees client/bus/call handles only;
the native process-wide runtime stays loaded. A failed pre-initialization load is unloaded.

It has no gRPC/protobuf Go imports and keeps no generated framework types in its API. Go values are
converted to canonical protobuf JSON (lowerCamelCase, enum names, base64 bytes, 64-bit integers as
strings) or passed as raw protobuf bytes; PCM always uses the binary fast path.

```go
env := capsula.FromEnv() // CAPSULA_LIBRARY, socket, run ID and app ID
client, err := capsula.Connect(ctx, env)
if err != nil { return err }
defer client.Close()
client.SetHandlers(capsula.Handlers{/* Ready, Activation, Settings, … */})
if err := client.Start(ctx); err != nil { return err } // blocks until the bus closes
```

`Call`/`CallJSON`/`Stream`/`OpenStream`/`OpenBidi` expose all framework and routed call shapes;
`Handle` serves custom routed calls; DTO-based helpers cover presentations, captures, input, audio,
conversations, Bluetooth media, settings and audio state. `Connect` copies every native buffer into
Go memory and releases the native allocation before returning; `Playback.Write` copies its input.
`Start` blocks and context cancellation ends the bus; `Close` is idempotent. `Playback.Close`
half-closes and waits for the framework to drain queued audio; `Playback.Abort` discards it
immediately. Typed errors include `ErrLibraryUnavailable`, `LibraryError`, `ABIError`,
`CapabilityError`, `StatusError` and `ErrClosed`.

Build with `make sdk-go`; the dumbassistant project is the runnable example. cgo is required
(`CGO_ENABLED=1`) and a C compiler must be available. On Linux the shim links only `-ldl` (libc and
pthread come from the cgo runtime); Darwin needs no extra libraries. A cross build needs a target C
compiler:

```sh
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  go build -C sdk/go ./...
```

The module vendors the ABI declarations at `sdk/go/capsula/abi.h`. If the canonical
`sdk/native/include/capsula/capsula.h` changes, run `sdk/go/regenerate-abi.sh` and gate CI or
`make test-sdk` on `sdk/go/regenerate-abi.sh --check`. The root module carries
`require capsula/sdk v0.0.0` plus `replace capsula/sdk => ./sdk/go` only for daemon test fixtures;
the nested module is excluded from root `go build ./...`.

### Rust binding

`sdk/rust` is crate `capsula`, a thin `libloading`/serde wrapper. It has no tonic/prost/native-crate
dependency and never links the engine.

```rust
use capsula::Library;

let library = Library::load()?;
let client = library.connect_env()?; // CAPSULA_SOCKET / CAPSULA_RUN_ID / CAPSULA_APP_ID
let reply = client.unary_json(
    "capsula.framework.v1.DeviceService",
    "GetInfo",
    b"{}",
    Some(std::time::Duration::from_secs(2)),
)?;
```

`Library::load()` resolves `CAPSULA_LIBRARY` (absolute path) first, then
`CAPSULA_LIBRARY_DIR` joined with the platform soname, then the soname itself. `Library::open(path)`
bypasses the environment. `Client::start_call` covers all four call kinds;
`Client::start_routed` uses the app-bus routed path. `Call`, `Client` and `Bus` are `Send + Sync`;
`recv_*` returns `Ok(None)` for a clean end, and `TIMEOUT` leaves the call usable. AppBus handlers
implement `BusHandler`; `InboundCall` is borrowed and the library owns its callback reference, so
retain it for asynchronous work and always finish it with `end(0, "")`. Build with
`make sdk-rust`; run the real-daemon smoke clients with
`cargo run --manifest-path sdk/rust/Cargo.toml --example smoke_unary -- …` (also `smoke_stream` and
`smoke_bus`), using `CAPSULA_LIBRARY`, `CAPSULA_SOCKET` and a supervised `CAPSULA_RUN_ID`.

### C and C++

C consumers include `<capsula/capsula.h>` from `build/include`. The C example links the library
directly (the loader resolves the SONAME at runtime):

```sh
cc -I build/include sdk/native/examples/c/device_info.c -L build/lib -lcapsula \
  -o build/bin/capsula-c-example
LD_LIBRARY_PATH=$PWD/build/lib CAPSULA_RUN_ID=run-... ./build/bin/capsula-c-example
```

The C++ example uses `<capsula/capsula.h>`, `dlopen`/`dlsym` of `capsula_get_api`, RAII wrappers
for handles and buffers, and no link against `libcapsula`:

```sh
c++ -std=c++17 -pthread -I build/include sdk/cpp/example.cpp \
    -o build/capsula-cpp-example -ldl
# macOS: omit -ldl; the CMake target links Threads::Threads and CMAKE_DL_LIBS
```

### Runtime metadata, capabilities and descriptors

`sdk/native/emit-runtime-metadata.sh <out.json> <library>` writes the launcher metadata with
`abi_major: 1`, `abi_minor: 0`, the capability array and the relative library filename. The
capability vocabulary covers `capsula.abi.v1`, transport modes, reflection/raw protobuf, AppBus
connect/events/notifications/routed calls, audio capture/playback/PCM fast path, descriptor
registration, deadlines/cancel/concurrent handles, and `capsula.service.<name>` for every framework
service. Apps declare only what they need:

```toml
[native_sdk]
abi_major = 1
min_minor = 0
required_capabilities = ["capsula.service.audio", "capsula.audio.playback"]
```

The launcher preflights this before starting the app and injects `CAPSULA_LIBRARY`,
`CAPSULA_LIBRARY_DIR` and the loader path when a runtime is discovered. The binding then performs
its own ABI check.

Framework descriptors are embedded in the library. Custom app contracts are not; register a
serialized `FileDescriptorSet` with `client_register_descriptors` (for example the bytes from
`AppInterfaceService.DescribeInterface`) before using JSON for that service. Regenerate the embedded
set when framework protos change:

```sh
protoc -I proto --include_imports \
  --descriptor_set_out=sdk/native/descriptors/capsula-framework.protoset \
  proto/capsula/framework/v1/framework.proto \
  proto/capsula/contracts/media/v1/media.proto
```

### Compatibility policy

ABI, SDK API and framework protocol versions are separate: ABI v1 describes the C table,
`CAPSULA_SDK` currently tracks the framework protocol (0.5), and binding source compatibility is
each module's own semver concern. Within ABI major 1, table fields are only appended, existing
signatures/positions/constants and ownership/threading guarantees never change, and new required
behavior starts a new major. Compatibility failures are explicit (`UNSUPPORTED_ABI`,
`ABIError`/`CapabilityError`, launcher rejection) rather than a loader crash or silent fallback.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/capsulad` | daemon entrypoint |
| `cmd/capsula` | local admin CLI (certs, apps, logs, firmware inspection, display bring-up) |
| `cmd/ledmap` | interactive display-channel calibration over adb (bring-up tool) |
| `internal/daemon` | wiring, gRPC services, supervision, routing/output loops |
| `internal/bus` | reverse-RPC app bus |
| `internal/presentations`, `internal/routing` | dynamic outputs and capture stack |
| `internal/audio`, `internal/input`, `internal/wakeword` | realtime services |
| `internal/bluetooth` | BlueZ D-Bus facade (power, pairing, AVRCP, absolute volume) |
| `internal/backend/simulator.go` | complete virtual backend |
| `internal/backend/rockchip` | RK3308 stock-firmware backend (vendor LED ABI recovered) |
| `internal/sandbox` | Linux read-only namespace launcher + seccomp |
| `internal/websecurity` | self-signed TLS, Argon2id password, API keys, sessions |
| `internal/provenance` | Sigstore/GitHub attestation verification |
| `internal/webui` | password/API-key HTML interface |
| `sdk/native` | Rust shared engine behind the stable C ABI (`libcapsula.so.1` / `.dylib`), header and C example |
| `sdk/go` | nested Go module `capsula/sdk`; thin binding over the shared native library |
| `sdk/rust` | Rust binding that dynamically loads the shared native library, with real-daemon smoke examples |
| `sdk/cpp` | C++ `dlopen` example and CMake target for the C ABI |
| `apps/dumbassistant` | optional first-party nested Go module; built only on request |
| `deploy/rockchip-stock` | boot service and bring-up scripts for the RK3308 stock image |
| `proto/` | draft v0.5 framework and media contract schemas; the example ring schema was retired with the built-in ring provider |
| `docs/` | hardware findings, implementation status, security notes |

## Verification

```sh
make test      # daemon, framework and routing tests
make test-sdk  # Go/Rust binding tests and vendored ABI drift check

cargo test --manifest-path sdk/native/Cargo.toml
sdk/go/regenerate-abi.sh --check
(cd sdk/go && CGO_ENABLED=1 go build ./... && \
  CGO_ENABLED=1 go test -c -o /tmp/capsula-go.test ./capsula)
cargo build --manifest-path sdk/rust/Cargo.toml --examples
cc  -I build/include sdk/native/examples/c/device_info.c -L build/lib -lcapsula \
    -o build/bin/capsula-c-example
c++ -std=c++17 -pthread -I build/include sdk/cpp/example.cpp \
    -o build/bin/capsula-cpp-example -ldl
```

Tests cover routing/capture semantics, preset validation, touch delivery/system suppression and
cancellation, settings CAS and secret redaction, self-signed TLS, password sessions, API-key
accept/revoke and CSRF, genuine Sigstore fixture verification, bounded log rotation, and an
end-to-end simulator run (supervision, singleton invocation, routed unary/streaming calls,
Bluetooth player and admin gating, touch partial/full capture, crash cleanup, wakeword
capture/release, mic watchdog and masked settings). The Web UI is additionally tested through
login/session, CSRF and static API-key flows.

## Status

See `docs/STATUS.md` for the implementation matrix and the remaining hardware gates, and
`docs/HARDWARE.md` for the evidence extracted from the supplied Rockchip firmware dump.
