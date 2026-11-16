# Development and deployment

This is the working manual for building, packaging, testing and deploying capsulad. The
application contract is the [application manual](app_development/README.md); measured evidence
is in [VERIFICATION.md](VERIFICATION.md).

## Prerequisites

| Tool | Needed for |
|---|---|
| Go 1.22.5 or newer | daemon, CLI, tools, Go SDK |
| Rust 1.85 or newer with Cargo | `libcapsulad`, Rust binding |
| C compiler | Go SDK cgo shim, C/C++ example, script hosts |
| Node.js + npm | rebuilding the Web UI (`web/`) only; the built SPA is checked in |
| `protoc` | regenerating Go bindings and the embedded descriptor after schema changes |

## Build profiles and targets

`make` is the release profile (`-trimpath -s -w`, Cargo release); `make debug` keeps
symbols. Outputs land in `build/bin`, `build/lib`, `build/include`, `build/apps`,
`build/devapps` and `build/deploy-bundle`.

| Command | Builds |
|---|---|
| `make` / `make release` | daemon, CLI, Go/Rust bindings, native library and header |
| `make debug` | the same targets without stripping |
| `make daemon` | only `capsulad` and `capsulad-cli` (no native SDK, no cgo) |
| `make sdk` | `make sdk-go` and `make sdk-rust` |
| `make native` | `libcapsulad`, header, symlinks and `capsulad-native.json` |
| `make web` | `npm ci && npm run build` into `internal/webui/dist` |
| `make interpreter` | QuickJS and MicroPython hosts, staged in `build/lib` |
| `make devapps` / `make dumbassistant` / `make marusia` | development apps and the two optional assistants: package directories plus one `<id>-<version>.tar.gz` each, under `build/devapps` / `build/apps` |
| `make deploy-bundle` | self-contained bundle directory and `.tar.gz`, assembled from ready-made artifacts; `BUNDLE_DEVAPPS=1` adds `build/devapps` |

Go targets whose `GOOS` is `darwin` are linked with `-linkmode=external`, because the Go
internal linker emits Mach-O binaries that current dyld rejects (missing `LC_UUID`). The
Makefile keys this off the target `GOOS`, not the build host.

## Cross build for the device

The Capsula Mini is aarch64 with glibc 2.33; it does not use musl.

```sh
make daemon GOOS=linux GOARCH=arm64                         # daemon + CLI only
make release GOOS=linux GOARCH=arm64 \
  NATIVE_TARGET=aarch64-unknown-linux-gnu                   # plus native SDK and bindings
```

`NATIVE_TARGET` is mandatory whenever the Go target differs from the host; the native
target refuses to package a host library. The Go SDK uses cgo, so its cross build needs a
target C compiler:

```sh
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc go build -C sdk/go ./...
```

For a reproducible glibc baseline, build inside an arm64 Bullseye container:
`docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src golang:1.22-bullseye make
daemon-release GOOS=linux GOARCH=arm64` (the daemon) and the same form with
`rust:1.85-slim-bullseye` for `make native … NATIVE_TARGET=aarch64-unknown-linux-gnu`.

Check the result before installing anything; every symbol must be glibc 2.33 or older:

```sh
readelf --version-info build/bin/capsulad | grep -o 'GLIBC_[0-9.]*' | sort -u
readelf --version-info build/lib/libcapsulad.so.1.0.0 | grep -o 'GLIBC_[0-9.]*' | sort -u
```

## Web UI

```sh
make web        # npm ci && npm run build into internal/webui/dist
make capsulad   # re-embed the new assets
```

`make web` runs `npm ci`, which needs network access on a machine without a populated npm
cache. With `web/node_modules` already present, `cd web && npm run build` works offline.

The checked-in `internal/webui/dist` is embedded with `go:embed`, so an ordinary Go build
needs no Node toolchain. For frontend work, run Vite against a live simulator daemon; Vite
serves sources and proxies `/api`, `/healthz` and `/ws` to the daemon's real HTTPS surface.

```sh
mkdir -p build/dev-web
./build/bin/capsulad-cli -data-dir build/dev-web auth init -hostname localhost \
  -ip 127.0.0.1 -password devpass123
./build/bin/capsulad -platform simulator -data-dir build/dev-web \
  -dev-admin build/dev-web/run/dev.sock -web-addr 127.0.0.1:8443
cd web && npm run dev            # https://localhost:5173 with HMR
```

`CAPSULAD_WEB_TARGET` overrides the proxy target (default `https://127.0.0.1:8443`). The
dev server reuses the daemon certificate (`build/dev-web/security`, or
`CAPSULAD_DEV_SECURITY_DIR`) and serves HTTPS because session cookies are
`Secure; SameSite=Strict`; set `CAPSULAD_DEV_HTTPS=0` only for browsers that accept Secure
cookies from `localhost`.

`sh tools/dev-media.sh build/dev-web` installs the stopped `dev.media` fixture, which the
Overview media bar can start through the normal routed contract.

## Application artifacts

An app package is a gzip-compressed tar with `manifest.toml` at the root, sorted entries,
zeroed timestamps/ownership and modes normalized to 0755 or 0444, so the same tree produces
identical bytes. Absolute paths, `..`, links, devices, FIFOs and duplicate paths are
rejected. Pinned limits: 48 MiB per file, 64 MiB unpacked, 4096 entries. The app-facing
contract for the artifact, manifest and install flow is
[app_development/packaging-deployment.md](app_development/packaging-deployment.md).

Packaging runs on the build host, never on the device. Each app build target compiles its tree
and then packages it with `capsulad-cli app package DIR`, which writes `<id>-<version>.tar.gz`
beside the package directory: `make dumbassistant` leaves
`build/apps/org.capsulad.dumbassistant-<version>.tar.gz`, `make marusia` leaves
`build/apps/org.capsulad.marusia-<version>.tar.gz`, and `make devapps` leaves one archive per
development app under `build/devapps`. `apps/devtools/build.sh` takes `PACKAGE=0` to stage
the directories without archives and `CAPSULAD_CLI=/path/to/capsulad-cli` to choose the packager.
A cross build packages with the host-executable `build/bin/capsulad-cli-host`
(`make capsulad-cli-host`), because the cross-target CLI cannot run on the build host.

The device receives archives only:

```sh
capsulad-cli app install build/apps/org.capsulad.dumbassistant-<version>.tar.gz
capsulad-cli app install build/deploy-bundle/apps/dev.hardware-<version>.tar.gz
capsulad-cli app list
capsulad-cli app start APP_ID [NAME=VALUE ...]
capsulad-cli app runs [APP_ID]
capsulad-cli app stop RUN_ID
capsulad-cli app remove APP_ID
```

`app install` accepts an archive and treats a package directory as a developer convenience, not
a separate format. It validates the manifest, the settings-UI document and any `[native_sdk]`
requirement, stages the package under `<data-dir>/apps`, and renames it into place. An
existing installation becomes an atomic `update`; a live run must be stopped first. A new
package becomes visible after the daemon restarts, because the registry is read at startup.
`app remove` also requires the app to be stopped. Built-in system apps have no package and
cannot be installed, removed or shadowed.

`app start` on a singleton app delivers an invocation instead of spawning a second process.

## Deployment bundles

`make deploy-bundle` writes `build/deploy-bundle/` and `build/deploy-bundle.tar.gz` from the
current build outputs. Assembly never compiles or packages an app: it copies the ready-made
`<id>-<version>.tar.gz` archives from `build/apps` and fails, naming the target to run, when a
staged package directory has no archive. `BUNDLE_DEVAPPS=1` also copies the archives that
`make devapps` staged in `build/devapps`. Build the app artifacts on the build host first when
they belong in the bundle. The bundle contains host-targeted binaries unless you built with
`GOOS`/`GOARCH` and `NATIVE_TARGET` for the device: a host bundle is for simulator work, and
installing it on the Capsula Mini fails the daemon's self-check (`-version` cannot execute
there).

```text
capsulad                    daemon binary
capsulad-cli                CLI binary
lib/                        native runtime, metadata and headers
apps/*.tar.gz               application packages
boot/S99btctl.sh            boot hook
```

`lib/` carries regular files only: the real runtime library (`libcapsulad.so.1.0.0` on the
device), `capsulad-native.json` and any host extras. `make deploy-bundle` copies regular files
and never dereferences the build tree's `libcapsulad.so -> libcapsulad.so.1 -> libcapsulad.so.1.0.0`
links, so one library is 4 MiB in the bundle instead of 12 MiB. The deployer rebuilds the ABI
layout after installing the files: it reads `library` from the installed `capsulad-native.json`,
links that SONAME to the matching `libcapsulad.so.1.0.0`, and links `libcapsulad.so` to the
SONAME, relative to the lib directory, refusing an absolute or escaping target and leaving a
conflicting regular file alone with a warning. A hand-assembled bundle follows the same rule —
ship the real file, not the links — and the preflight resolves the declared library from the
bundle's regular files or from the installed layout. A runtime file whose installed copy is
byte-identical is skipped rather than rewritten, so redeploying an unchanged runtime needs no
free space on the data partition and cannot fail there.

`capsulad-cli deploy PATH [-init-script PATH] [-restart auto|always|never] [-dry-run]
[-staging DIR] [-timeout DURATION]` accepts that directory or the tarball. `capsulad-cli`
itself is optional in a device bundle: the running CLI installs a daemon without
installing a copy of itself. Flags:

- `-dry-run` stages and validates the bundle and prints the plan; nothing durable changes.
- `-init-script` selects the boot service (default `/etc/init.d/S99btctl.sh`).
- `-restart auto` (default) restarts when the bundle carries a daemon, `lib/` or apps.
  `always` restarts unconditionally. `never` installs without stopping or restarting the
  daemon and without installing the boot hook; replacing the daemon binary is refused while a
  daemon still answers on the framework socket (stop it first, or use `auto`), and a daemon
  that is already stopped stays stopped until `S99btctl.sh start` or the next boot.
- `-staging DIR` puts staging below `DIR`; the default is the operating system temporary
  directory (`/tmp/capsulad-deploy-*`). Staging inside `<data-dir>` is always rejected.
  Symlinks are resolved before the check, and staging on the data *filesystem* is allowed
  only when it can hold the largest staged tree beside both reserves (128 MiB unpacked +
  26 MiB + 8 MiB). A host or simulator data directory on one big volume therefore keeps
  working; the 78.5 MiB device partition never qualifies, and `/tmp` is a separate tmpfs on
  the Capsula Mini anyway.
- `-timeout` bounds the health gate (default 45 s).

Nothing is staged inside `<data-dir>`. The 78.5 MiB data partition already holds the ~40 MiB
daemon, so a second copy of it does not fit: an archive bundle is unpacked into staging, the
replaced binaries are backed up there compressed, and `<data-dir>` only ever holds one copy
of each binary. An archive preflight requires the unpacked tree (about twice the archive
size) plus a 26 MiB reserve for the compressed backup on the staging filesystem; the
operator's archive itself stays where it was, and a directory bundle is inspected in place
and skips the preflight. A second preflight, run before the daemon is stopped, requires
enough free space on `<data-dir>` for the binary writes that cannot free their own space —
the full staged CLI size when the running executable *is* the installed CLI, and the
difference between the staged and installed daemon — plus an 8 MiB reserve.

The steps, in order:

1. Unpack and validate the whole bundle. Top-level entries other than the five above (and
   macOS `._*` metadata) are reported and ignored; `apps/` accepts only `.tar.gz`/`.tgz`.
   `lib/` must contain regular files only.
2. Check each incoming app's `[native_sdk]` requirement against the runtime the deployment
   would leave installed: the bundle's own `lib/`, otherwise `<data-dir>/lib`.
3. Prove the staged daemon runs here and prints `capsulad …` from `-version`; validate the
   boot hook with `sh -n`. The bundle's own file is probed in place (its mode is raised to
   executable when needed), not copied again.
4. Check the data filesystem can take the binary writes (see above), then, when a restart is
   scheduled, stop the daemon with `<init-script> stop` and verify it: a daemon that still
   answers on the socket, or whose `run/capsulad.pid` still names a live process, fails the
   deployment before anything is installed. Without a restart (`-restart never`) live runs of
   the apps this bundle replaces are stopped instead, and the deployment refuses to continue
   if one stays alive.
5. Install or update packages, then replace managed files in `<data-dir>/lib` (each file
   written beside its target and renamed) — with the daemon stopped, so a selected provider
   app is replaced without a stop-for-restart round trip.
6. Install the boot hook when the bundle carries it and a restart is scheduled: the existing
   `/etc/init.d/S99btctl.sh` is copied to `<init-script>.prev` and the new hook is written in
   place. Both steps copy instead of renaming because the hook lives on the overlayfs root,
   where renaming a lower-layer file fails with `EINVAL`. Doing this before the binaries keeps
   the post-handback window free of everything but the daemon.
7. Replace `capsulad-cli`, then `capsulad`. A staged file on the target filesystem is renamed
   into place, and the replaced binary is kept as `<name>.prev`. A staged file elsewhere (the
   normal case, since staging is on the temporary filesystem) cannot be renamed: the replaced
   binary is compressed into staging (~25 MiB for the 40 MiB daemon), the target is
   removed to release its space, the staged bytes are written directly to the target path and
   chmodded 0755. No sibling copy of the new binary is ever created on `/data`. A staged file
   inside the staging tree is deleted once it is written; a directory bundle is left alone
   unless it shares the target filesystem, where the rename consumes its entry — rebuild the
   bundle before repeating that path.
8. Run `<init-script> restart` with `CAPSULAD_DATA_DIR` exported, then poll
   `capsulad -health -socket <socket>` until it exits 0 (exit 2 falls back to a framework
   API probe).
9. On success, remove the transient backups. On failure, restore the replaced binaries and
   boot hook and restart the previous stack (package installs are not reverted); report the
   original failure and any step that could not be undone. If a compressed backup cannot be
   restored, the staging tree is kept and the error names the backup: it is the only remaining
   copy of that binary.

A bundle without `capsulad`, `capsulad-cli`, `lib/`, `apps/` or `boot/S99btctl.sh` is
rejected as having no actionable payload. Nothing is written outside `<data-dir>` except
the boot hook.

### Over adb

The normal device flow does not need a copy of the CLI in `/data` and does not unpack the
bundle there:

```sh
# Build for the device, then assemble the bundle on the build host.
adb -s <device> push build/linux-arm64/<build>/bundle /tmp/capsulad-bundle
adb shell /tmp/capsulad-bundle/capsulad-cli -data-dir /data/capsdemo \
  deploy /tmp/capsulad-bundle -timeout 60s
adb shell /data/capsdemo/capsulad -health -socket /data/capsdemo/run/hal.sock
```

The bundle may be pushed as a directory or as the `.tar.gz`; a directory avoids a second
unpacked copy in the tmpfs. **Run the deployment detached when the daemon owns the network
interface.** On the Rockchip stock board the daemon owns the Wi-Fi station, so `deploy` stopping it
drops the link, which kills an attached `adb shell` mid-deployment — observed three times in one
session, once after the old daemon had been stopped and the binaries swapped but before the restart,
leaving the board with no running daemon. `nohup ... &` is not enough (the shell's process group is
killed); use a script and the init helper:

```sh
# /tmp/deploy.sh
/tmp/capsulad-bundle/capsulad-cli -data-dir /data/capsdemo deploy /tmp/capsulad-bundle \
  -staging /run -timeout 180s > /tmp/deploy.log 2>&1
echo "exit=$?" >> /tmp/deploy.log
```
```sh
adb shell chmod +x /tmp/deploy.sh
adb shell start-stop-daemon -S -b -x /tmp/deploy.sh
# reconnect when the link returns, then read /tmp/deploy.log and the health gate
```
If a deployment is cut off between `stopped the daemon` and `restarted`, start the stack with
`/etc/init.d/S99btctl.sh start` (never by running the daemon by hand) and check the installed
binary's md5 before retrying. Staging defaults to `/tmp/capsulad-deploy-*`; the bundle itself
can live on `/run` when vendor files consume `/tmp`. Run the CLI from the pushed bundle so
the installed `/data/capsdemo/capsulad-cli` is not the executable being replaced. Keep a host
rollback copy of `capsulad` and `capsulad-cli` (`adb pull`) until the health gate and the
app/web checks pass; a reset during the direct daemon write can leave a truncated target.

### The watchdog window

`S99btctl.sh stop` hands `/dev/watchdog` to a detached feeder, so the hardware timer stays
armed but is only fed for about 21 s after the last keepalive. A feeder left behind by an older
daemon is a copy of that daemon and pins its 40 MiB inode on `/data`. The order matters:
`deploy` first compresses the replaced binary into staging while the feeder is still feeding,
then asks it to hand the timer back (`run/watchdog.handback`), waits for
`run/watchdog.feeder` to disappear — which is what releases the replaced inode's space — and
removes the marker. So the gzip does not consume the window; only removing the old file,
writing the new binary and restarting do. If the feeder is still there after the wait, the
deployment aborts before removing the daemon (its space is pinned, so the write could not
succeed), drops the useless backup and restarts the previous stack. A same-filesystem rename lands instantly; the
cross-filesystem write is the slow part, and a watchdog reset during it leaves a truncated
target. Re-run the deployment after the board reboots rather than assuming the binary is
complete.

## Device layout and boot hook

```text
/data/capsdemo/capsulad
/data/capsdemo/capsulad-cli
/data/capsdemo/lib/               libcapsulad.so.1.0.0, capsulad-native.json, script hosts, include/
/data/capsdemo/apps/<app_id>/
/data/capsdemo/assets/
/data/capsdemo/run/hal.sock
/data/capsdemo/security/
/data/capsdemo/capsulad.log       plus one rotated copy (512 KiB cap)
/etc/init.d/S99btctl.sh
```

The hook prepares state, pins the Bluetooth UART IRQ, retires/restores the vendor assistant
scripts, starts the daemon, gates on `capsulad -health`, and falls back to the vendor chain.
`S99btctl.sh disable` stops the daemon, restores the vendor scripts, starts the vendor
chain and moves the hook to `/data/capsdemo/boot/S99btctl.sh.disabled`; running that copy
with `enable` reinstates it. The stock path name is required: `rcS` expands
`/etc/init.d/S??*` before the `/data` overlay pivot.

## Simulator

```sh
make
mkdir -p build/demo
./build/bin/capsulad-cli -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
./build/bin/capsulad -platform simulator -data-dir build/demo \
  -dev-admin build/demo/run/dev.sock
```

`auth init` creates the certificate, sets an admin password (printed once when generated)
and issues a bootstrap API key; open `https://127.0.0.1:8443/`. Without `-dev-admin`, the
simulator accepts no injected input.

```sh
./build/bin/capsulad-cli -data-dir build/demo status
./build/bin/capsulad-cli -data-dir build/demo dev state
./build/bin/capsulad-cli -data-dir build/demo dev key play_pause true
./build/bin/capsulad-cli -data-dir build/demo dev gesture swipe_left end
./build/bin/capsulad-cli -data-dir build/demo dev wakeword
./build/bin/capsulad-cli -data-dir build/demo media list
./build/bin/capsulad -health -socket build/demo/run/hal.sock
```

Useful `capsulad` flags: `-platform`, `-data-dir`, `-socket`, `-web-addr` (empty
disables the UI), `-native-lib-dir`, `-sandbox auto|namespaces|mountns|uid|none`,
`-dev-admin`, `-mem-limit-mb`, `-output-trace`, `-wakeword-trace`, `-watchdog`.

Platform selection:

| `-platform` | Backend | Notes |
|---|---|---|
| `simulator` (default) | in-memory simulator | neutral `profile.Simulator`: no device paths or watchdog |
| `rockchip-stock` (alias `rockchip`) | RK3308 stock backend | hardware profile; backend factory and watchdog opener are wired by `cmd/capsulad` |

`CAPSULA_MINI_AMLOGIC_STOCK` and `CAPSULA_MINI_AMLOGIC_ALPINE` remain in the wire enum as
defined-but-unsupported/reserved values. This build has no internal platform constant, no
selection case and no backend for them, so any other `-platform` value fails with
`unknown platform ... (simulator, rockchip-stock)`.

## Tests

```sh
make test          # root Go module, sdk/go, Rust binding, native crate, C++ example, both assistants
make test-race     # root Go module with -race
make test-marusia  # the apps/marusia module's tests from the repository root
```

`make test` does not run the interpreters or device backends. Focused commands:

```sh
go test -ldflags '-linkmode=external' ./internal/routing ./internal/presentations
go test -race -ldflags '-linkmode=external' ./internal/daemon ./internal/audio
go test -run 'TestDumbAssistant|TestSupervisor' ./internal/daemon
(cd sdk/go && CGO_ENABLED=1 go test ./...)
(cd apps/dumbassistant && CGO_ENABLED=1 go test ./...)
(cd apps/marusia && go test ./...)        # no test reaches the network; TLS listeners are loopback
cargo test --manifest-path sdk/native/Cargo.toml
sdk/go/regenerate-abi.sh --check
```

Standalone Go test commands on macOS need `-ldflags '-linkmode=external'`; the Makefile
and app build scripts already handle it. Host daemon integration tests need a matching host
native runtime with metadata, normally `build/lib`, or the `CAPSULAD_NATIVE_LIB_DIR`
override; a Linux library is not a usable macOS test runtime. Tests that start a daemon
also need the real Mach-O runtime and its `capsulad-native.json`.

The script-host smoke test covers ready, invocation, unary and routed calls, stream
cancellation, stop/crash cleanup and a daemon-crash disconnect:

```sh
make interpreter
sh interpreter/smoke/run.sh
```

## Interpreters and development apps

`make interpreter` builds both hosts and `make interpreter-quickjs` /
`make interpreter-micropython` one. They come from pinned sha256-verified sources
(quickjs-ng 0.18.0, MicroPython `1c6321…`); set `QUICKJS_SRC`/`MICROPYTHON_SRC` for
offline builds and note that they stage beside `libcapsulad`.

```sh
make devapps        # dev.hardware, dev.quickjs, dev.micropython, one .tar.gz each
capsulad-cli -data-dir build/demo app install \
  build/devapps/dev.hardware-<version>.tar.gz
```

`make devapps` compiles and packages on the build host and needs a built native runtime
(`make native`) plus, for cross builds, a target C compiler. See [DEV_APPS.md](DEV_APPS.md) for
what each development app does, and for `PACKAGE=0`, `APP=`, `OUT=` and `CAPSULAD_CLI=`.

## Administration commands

```sh
capsulad-cli -data-dir build/demo status          # device, resources, runs, audio
capsulad-cli -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
capsulad-cli -data-dir build/demo auth set-password
capsulad-cli -data-dir build/demo auth key create automation
capsulad-cli -data-dir build/demo auth key list
capsulad-cli -data-dir build/demo auth key revoke automation
capsulad-cli -data-dir build/demo cert status
capsulad-cli -data-dir build/demo cert renew -days 365
capsulad-cli -data-dir build/demo settings show
capsulad-cli -data-dir build/demo settings app capsulad.clock blink=true
capsulad-cli -data-dir build/demo settings app capsulad.ring effect=static palette='#FF7828'
capsulad-cli -data-dir build/demo logs -app APP_ID -n 100
capsulad-cli -data-dir build/demo media list
capsulad-cli platform inspect -root /path/to/extracted/rootfs
capsulad-cli platform ledtest -ring
```

Global flags precede the command: `-data-dir DIR`, `-socket PATH`, `-security-dir DIR`,
`-native-lib-dir DIR`, `-json`. `settings app APPID key=value` reads values as JSON when
they parse and as strings otherwise. `cert renew` requires a daemon restart to reload the
listener certificate; `auth` changes apply live because the daemon watches the security
store.