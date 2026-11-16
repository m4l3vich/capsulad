# Development apps (`apps/devtools`)

Development-only supervised apps that exercise the Capsula Mini's hardware and
software surfaces through the public app-facing APIs only: the Go SDK
(`capsulad/sdk`, `capsulad.Connect(ctx, capsulad.FromEnv())`) or the QuickJS and
MicroPython `capsulad` host modules. They never read daemon internals and never
open a private socket.

| App | Runtime | What it proves |
|---|---|---|
| `dev.hardware` | native | Device/audio/state reads, segment and ring presentations with explicit release, both touch capture modes, ~1 s microphone PCM, paced tone playback, app-settings CAS, AppBus echo |
| `dev.quickjs` | `quickjs` | Host environment, `DeviceService`/`AudioService`/`PresentationService` calls, settings CAS, action round trip, periodic resource heartbeat, raw-protobuf AppBus echo |
| `dev.micropython` | `micropython` | The same surface as `dev.quickjs` for the MicroPython host |

## Build

```sh
sh apps/devtools/build.sh              # all three apps, compiled and packaged
make devapps                           # the same script through the Makefile
APP=dev.hardware sh apps/devtools/build.sh
OUT=/tmp/devapps sh apps/devtools/build.sh
PACKAGE=0 sh apps/devtools/build.sh    # stage the directories, write no archives
CAPSULAD_CLI=build/bin/capsulad-cli sh apps/devtools/build.sh
```

Each app is compiled and packaged on the build host. The package directory and its
`<app-id>-<version>.tar.gz` archive land side by side under `build/devapps` (or `OUT`):

```text
dev.hardware/                     manifest.toml  devtools  proto/devtools.protoset  proto/devtools.proto
dev.hardware-<version>.tar.gz
dev.quickjs/                      manifest.toml  main.js
dev.quickjs-<version>.tar.gz
dev.micropython/                  manifest.toml  main.py
dev.micropython-<version>.tar.gz
```

The script requires the Go SDK (`sdk/go`) and a native runtime (`build/lib` with
`libcapsulad*` and `capsulad-native.json`; `make native`, or `make`/`make release`, produces it)
and fails with an actionable message when either is missing. Packaging uses
`capsulad-cli app package` with the repository CLI; set
`CAPSULAD_CLI=/path/to/capsulad-cli` to override the packager, which a cross build needs
because the cross-target CLI cannot run on the build host.
`PACKAGE=0` stops after staging the directories. `protoc` regenerates the echo descriptor
when available; otherwise the committed `devtools.protoset` is used. The native binary is
built with the same `GOOS`/`GOARCH`/`CGO_ENABLED`/`CC` knobs as `apps/dumbassistant`: the
default is the host, and a device build needs a matching cross compiler (for example
`CC=aarch64-linux-gnu-gcc`).

## Package, deploy, run

```sh
capsulad-cli app install build/devapps/dev.hardware-<version>.tar.gz
capsulad-cli app install build/devapps/dev.hardware     # directory: developer convenience

# Or a deployment bundle: apps/*.tar.gz beside the daemon, CLI, lib/ and boot/.
make deploy-bundle BUNDLE_DEVAPPS=1     # copies the archives staged in build/devapps
capsulad-cli deploy build/deploy-bundle # staging on the data filesystem needs ~162 MiB free
capsulad-cli deploy build/deploy-bundle -staging /path/on/another/filesystem

capsulad-cli app start dev.hardware              # runs the full self-test
capsulad-cli app start dev.hardware step=input   # one step
capsulad-cli app start dev.quickjs step=validate
capsulad-cli app runs
capsulad-cli logs -app dev.hardware -n 200
```

The device only ever installs an archive built on the host; `make deploy-bundle` never
packages anything, so run `make devapps` first. Installing a package does not hot-reload the
daemon's registry: restart `capsulad` (or use the `deploy` restart path) before starting a
newly installed app. A running instance must be stopped before its package is replaced.

Steps: `selftest`, `info`, `display`, `ring`, `audio`, `input`, `settings`,
`echo`. The same names are manifest actions; a launch invocation accepts one
`step` argument and defaults to `selftest`.

## Expected observable results (simulator)

```text
devtools: info gen=1 device="simulator-0001" model="Capsula Mini (simulator)" platform=unspecified ...
devtools: display gen=1 ... digits=1234 masks=0x0006,0x005b,0x004f,0x0066 colon=true
devtools: ring gen=1 ... pixels=12 frame=first=8,64,128 last=16,192,224
devtools: input gen=1 mode=partial ... selected=true keys=0 gestures=0 raw=0 events=0 window_ms=2500
devtools: audio gen=1 ... capture ... microphone[ok] PCM_S16LE/16000Hz/1ch ... captured_ms=1000 ... signal=silence; playback[ok] tone=440Hz/300ms ... drained=true
devtools: settings gen=1 key=note ... revision=1->2
devtools: echo gen=1 target=self mode=json reply="echo:selftest"
devtools: result step=selftest gen=1 status=ok duration_ms=... detail="steps=7 ok=7 unsupported=0 failed=0"
```

A step that the backend cannot support reports `status=unsupported` with the
reason instead of failing the run. Every step releases its presentations,
captures and streams; a terminal path cannot leave the ring or microphone
claimed.

The simulator panel only emits input through the development admin socket, so
run a bounded input step and inject during its 2.5 s windows:

```sh
capsulad-cli app start dev.hardware step=input
capsulad-cli dev key play_pause true
capsulad-cli dev gesture tap start
capsulad-cli dev raw 0.5 0.5 true
```

Microphone capture is gated by the daemon on a selected, visible, non-blank ring
presentation plus the live cue; the app opens that indication itself. The
simulator microphone is digital silence unless PCM is injected, so a successful
audio step reports `signal=silence`, not audio content.

Script apps log one `ready` line, settings revisions, a triggered action, three
one-second heartbeat lines and echo requests. MicroPython's heartbeat is a
timed `WatchResources` read (its host raises a catchable `OSError`); the QuickJS
host exposes no timer and its binding errors are not catchable from script, so
it emits a bounded spin heartbeat before entering the event loop. The native
`echo` step additionally calls the script apps' raw-protobuf echo handlers when
they are installed and ready (`peers=2`); otherwise it reports them as
`unavailable` and still succeeds on its own routed self-call.

## Required artifacts

- `sdk/go` (Go binding) and a working `go` toolchain with cgo for the native app.
- Native runtime in `build/lib` (or `CAPSULAD_NATIVE_LIB_DIR`): `libcapsulad`,
  `capsulad-native.json`, and, for the script apps, `capsulad-quickjs` and
  `capsulad-micropython` (`make interpreter`).
- The daemon must run with that runtime directory (`-native-lib-dir`).

## Proof-level caveats

- The simulator proves API behavior, routing/ownership, PCM plumbing and log
  flow. It does **not** prove acoustic output, microphone sensitivity, real
  brightness/gamma, panel timing or gesture recognition quality.
- `sandbox=none` on macOS only proves the app protocol path. It does not prove
  Linux namespace/uid sandboxing or the stock kernel's mount-only isolation.
- The simulator's `targetPlatform` is unspecified; device runs report a platform
  enum. Ring and segment frames are virtual write snapshots, not light.
- Script hosts do not expose a filesystem or timers. Their echo contract is
  deliberately descriptor-less (protobuf field 1); descriptor-backed JSON is
  covered by the native app.