# Packaging and deployment

An application package is built and assembled **on the build host**. The device never compiles
and never packages: it receives `.tar.gz` artifacts, installs them into its data directory and
starts the daemon. This page covers the artifact format, the host tooling, install and update
semantics, the deployment bundle that moves a complete stack onto a device, and the simulator
loop.

What runs where:

| Stage | Machine | Tool |
|---|---|---|
| Compile a native binary | build host | Go/Rust/C toolchain for the target ABI |
| Assemble and package a directory | build host | `capsulad-cli app package` |
| Include packages in a bundle | build host | `capsulad-cli deploy` input, `make deploy-bundle` |
| Install a package | device or simulator | `capsulad-cli app install` |
| Launch, supervise, restart | device or simulator | `capsulad` |

## Artifact format

A package is a gzip-compressed POSIX (PAX) tar whose root contains `manifest.toml`. Entries are
relative, slash-separated paths. Directories and regular files only.

| Property | Value |
|---|---|
| Name | `<id>-<version>.tar.gz` by default |
| Root entry | `manifest.toml` (required) |
| Modes | `0755` when any execute bit was set on the source file, otherwise `0444` |
| Timestamps and ownership | zeroed; `uid`/`gid` 0, no user/group names |
| Per-file limit | 48 MiB |
| Unpacked package limit | 64 MiB |
| Entry count limit | 4096 |
| Installed location | `<data-dir>/apps/<app-id>` |
| App view | `/app`, read-only |

Rejected while packaging and again while extracting:

- an absolute path, or a path that escapes the package root through `..`;
- symlinks, hardlinks, devices and FIFOs;
- duplicate paths;
- a missing root `manifest.toml`;
- a file above 48 MiB, a package above 64 MiB unpacked, or more than 4096 entries.

The writer sorts entries, zeroes metadata and applies the mode policy, so the same source tree
always produces byte-identical output. Every regular file below the directory is included,
including dotfiles: keep `.git`, editor state and scratch build trees out of the package
directory. The executable bit is the only mode bit the launcher reads; the package tree is
immutable once installed.

Packaging validates the manifest before writing. A manifest error therefore fails the package
step with the same message an install would produce; see [manifest.md](manifest.md).

## Packaging on the build host

```sh
capsulad-cli app package DIR [-out FILE]
```

`DIR` must contain `manifest.toml` at its root. Without `-out`, the artifact is written to the
current directory as `<id>-<version>.tar.gz`. The command prints `wrote <file> (<id> <version>)`.

```sh
capsulad-cli app package ./my-app -out build/apps/my-app.tar.gz
```

### Repository build targets

| Command | Result |
|---|---|
| `make dumbassistant` | Builds the first-party assistant for the selected `GOOS`/`GOARCH`, stages `build/apps/org.capsula.dumbassistant/` and runs `app package` there, producing `build/apps/org.capsula.dumbassistant-<version>.tar.gz`. |
| `make devapps` | Runs `apps/devtools/build.sh`, which builds and packages `dev.hardware`, `dev.quickjs` and `dev.micropython` under `build/devapps/`. |
| `APP=dev.hardware make devapps` | Builds one development app. |
| `OUT=/tmp/devapps make devapps` | Changes the output directory. |
| `PACKAGE=0 make devapps` | Stages package directories only, with no `.tar.gz`. Development convenience; the device path installs archives. |
| `make deploy-bundle` | Assembles `build/deploy-bundle/` and `build/deploy-bundle.tar.gz` from already-built artifacts. It builds or reuses the daemon, CLI and native runtime, but never packages an app. |
| `BUNDLE_DEVAPPS=1 make deploy-bundle` | Also copies staged development-app archives from `build/devapps/`. |

`make deploy-bundle` requires every app directory under `build/apps/` to have a matching
`<id>-*.tar.gz`; otherwise it fails and tells you which build target to run. The bundle contains
`capsulad`, `capsulad-cli`, `lib/` (the native runtime plus headers), `apps/*.tar.gz` and
`boot/S99btctl.sh`.

### Cross builds

Only the Go and native toolchains care about the target; the packager must run on the build
host. The Makefile therefore builds a host-executable `build/bin/capsulad-cli-host` when
`GOOS`/`GOARCH` differ from the host, and the development-app script falls back to it (or to
`CAPSULAD_CLI=/path/to/capsulad-cli`).

```sh
# Device target: aarch64, glibc 2.33 or older.
GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc make dumbassistant

# Native runtime for the same target must be built for that ABI.
GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu make deploy-bundle
```

The device rejects a native entrypoint whose `architecture` is not `aarch64`/`arm64` or whose
`abi` does not contain `linux`. The simulator accepts host binaries, so a `darwin` or `x86_64`
build works there and fails on the device. Build native binaries with the target ABI and with a
compatible glibc; a newer glibc fails at process startup.

## Install, update and remove

```sh
capsulad-cli -data-dir /data/capsdemo app install PATH
capsulad-cli -data-dir /data/capsdemo app remove ID
```

`PATH` is a `.tar.gz` or, for development convenience, an unpacked package directory.

- Installation stages the package on the same filesystem, validates the manifest, preflights a
  declared `[native_sdk]` requirement against the installed runtime, sets the previous
  installation aside, renames the staged tree into `<data-dir>/apps/<app-id>`, and only then
  removes the old tree. A failed install leaves the previous installation intact.
- Replacing, removing or installing over a **running** app is refused:
  `app <id> is running (run <run-id>); stop it before replacing the package`. Stop the run first
  with `app stop <run-id>`.
- A newly installed package becomes visible after `capsulad` restarts. The registry is read at
  startup, and `app install` prints a reminder. `capsulad-cli deploy` performs that restart when
  it is allowed to.
- The install prints `installed <id> (<version>) into <dir>` or `updated …`.
- Removing an app deletes its package directory. App settings live in
  `<data-dir>/app-settings/<app-id>/settings.json` and are not removed by uninstall; reinstalling
  the same ID finds its stored settings again.
- Built-in system apps have no package directory and cannot be installed, replaced or removed.

## Deployment bundle

A deployment bundle moves a whole stack onto an existing device. It is a directory or a
`.tar.gz` with this layout:

| Entry | Meaning |
|---|---|
| `capsulad` | Daemon binary; installed as `<data-dir>/capsulad`, mode `0755`. |
| `capsulad-cli` | CLI binary; installed as `<data-dir>/capsulad-cli`, mode `0755`. |
| `lib/` | Shared native runtime and headers. Replaces managed runtime files in `<data-dir>/lib`. |
| `apps/*.tar.gz` | Application packages. Installed or atomically updated. Other names are reported and ignored. |
| `boot/S99btctl.sh` | Boot hook. Installed only when bundled, and only when the daemon is restarted. |

Unknown top-level entries are reported and ignored. `._*` files from BSD tar are ignored.
Symlinks anywhere in the bundle are rejected. The bundle itself is capped at 64 MiB per file,
128 MiB unpacked and 8192 entries; extraction reserves 32 MiB of free space for the daemon,
apps, settings and logs.

```sh
capsulad-cli -data-dir /data/capsdemo deploy build/deploy-bundle.tar.gz
capsulad-cli -data-dir /data/capsdemo deploy build/deploy-bundle -dry-run
```

| Flag | Default | Meaning |
|---|---|---|
| `-init-script PATH` | `/etc/init.d/S99btctl.sh` | Boot service invoked to restart the daemon. |
| `-restart auto\|always\|never` | `auto` | See below. |
| `-dry-run` | off | Validate and print the plan; change nothing. |
| `-timeout DURATION` | `45s` | Time allowed for the restarted daemon to report healthy. |

`-restart` values:

| Value | Behavior |
|---|---|
| `auto` | Restart if the bundle carries a daemon, `lib/` or any app package. A boot-hook-only bundle does not restart. |
| `always` | Always restart. |
| `never` | Never restart. The boot hook is not installed and packages become active at the next manual daemon restart. |

Deployment order: every bundle entry is validated first; live runs of apps the bundle replaces
are stopped through the daemon (if it is unreachable, package management continues offline);
packages are installed or updated; `lib/` is synced; binaries and the boot hook are activated;
the daemon is restarted through the boot service; and `capsulad -health` must succeed within the
timeout. Replaced files are kept as a transient `<file>.prev` only until the health gate passes,
then removed. If the restart or health gate fails, the previous binaries and boot hook are
restored and the previous stack is restarted. There is no durable backup.

`-dry-run` never executes bundle content. A real deployment probes the incoming daemon only
after it has been installed.

## Device paths

| Path | Contents |
|---|---|
| `/data/capsdemo` | Data directory used by the stock-device boot hook. |
| `/data/capsdemo/apps/<app-id>` | Installed package. |
| `/data/capsdemo/app-settings/<app-id>/settings.json` | App settings and revision. |
| `/data/capsdemo/assets` | Shared asset store. |
| `/data/capsdemo/lib` | Native runtime (`libcapsula`, `capsula-native.json`, script hosts). |
| `/data/capsdemo/run/hal.sock` | Framework Unix socket (host path). |
| `/run/capsulad/hal.sock` | The same socket inside an app's mount namespace. |
| `/data/capsdemo/capsulad.log` | Daemon log. |

## Simulator

The simulator runs the daemon on the development host with virtual display, ring, panel,
microphone and speaker. It accepts the same packages.

```sh
mkdir -p run/demo
capsulad-cli -data-dir run/demo auth init -hostname localhost -ip 127.0.0.1
capsulad -platform simulator -data-dir run/demo -dev-admin run/demo/run/dev.sock &

CGO_ENABLED=1 go build -trimpath -o bin/hello .
capsulad-cli app package . -out hello.tar.gz
capsulad-cli -data-dir run/demo app install hello.tar.gz
capsulad-cli -data-dir run/demo app start com.example.hello
```

`auth init` creates the HTTPS certificate, an admin password (generated when `-password` is
empty) and a bootstrap API key. The daemon listens on `<data-dir>/run/hal.sock`; every CLI
command resolves that path from `-data-dir`. `-dev-admin` adds a development-only admin socket
for simulator tooling. `capsulad-cli dev …` injects wakeword, gesture, key, raw and state events into a running
simulator.

A host build loads the host `libcapsula`; the simulator also accepts `quickjs` and `micropython`
packages when their hosts are installed beside it. The sandbox mode is whatever the host kernel
probes; on a stock device it is normally `mountns`, so do not assume the simulator's isolation
proves the device's.

## Versioning and replacement

`version` is free-form and compared for equality only. There is no implicit downgrade or upgrade
rule: installing a package with the same ID replaces the installed tree atomically, whatever its
version string. Settings survive replacement because they are stored outside the package tree.
Rollback means keeping the previous `.tar.gz` and installing it again; the deployment bundle's
`.prev` files are not a rollback mechanism.

## End-to-end checklist

```sh
# 1. Build and package on the build host.
capsulad-cli app package ./my-app -out build/apps/my-app.tar.gz

# 2. Install on the target and restart the daemon so the registry sees it.
capsulad-cli -data-dir /data/capsdemo app install build/apps/my-app.tar.gz

# 3. Start and observe.
capsulad-cli -data-dir /data/capsdemo app start org.example.myapp
capsulad-cli -data-dir /data/capsdemo app runs org.example.myapp
capsulad-cli -data-dir /data/capsdemo logs -app org.example.myapp -n 50
```

Limits to remember:

- The device never packages; every `.tar.gz` comes from a build host.
- A package above the size or entry budget is rejected before it is installed.
- A running app cannot be replaced or removed.
- `app install` does not restart the daemon; use `deploy` or restart `capsulad`.
- The boot hook is installed only by a bundle that carries it and only with a restart.