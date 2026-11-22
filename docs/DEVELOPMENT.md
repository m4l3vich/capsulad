# Development and deployment

This is the working manual for building, packaging, testing and deploying capsulad. The
application contract is the [application manual](app_development/README.md); measured evidence
is in [VERIFICATION.md](VERIFICATION.md).

## Prerequisites

| Tool | Needed for |
|---|---|
| Go 1.22.5 or newer | daemon, CLI, tools, Go SDK |
| Rust 1.88 or newer with Cargo | `libcapsulad`, Rust binding |
| C compiler | Go SDK cgo shim, C/C++ example, script hosts |
| Node.js + npm | rebuilding the Web UI (`web/`) and the Simon and Soundboard contract tests (`node --test`); the built SPA is checked in |
| Python 3 | the PA unit tests (`make test-pa`), the MicroPython host prelude self-test (`make test-interpreter`), and regenerating the Web UI icon subset (a venv, see [Web UI](#web-ui)) |
| `protoc` | regenerating Go bindings and the embedded descriptor after schema changes |

## Build profiles and targets

The release build surface has two inputs:

| Variable | Values | Default |
|---|---|---|
| `LIBC` | `gnu`, `musl` | `gnu` |
| `PROFILE` | `release`, `debug` | `release` |

The three public targets are:

| Command | Builds |
|---|---|
| `make core LIBC=gnu\|musl` | daemon, CLI, `libcapsulad` + metadata, QuickJS and MicroPython hosts |
| `make apps LIBC=gnu\|musl` | all ten first-party app archives, including AirPlay |
| `make installer` | the terminal installer for darwin/linux/windows on amd64 and arm64 |

Outputs are separated by both inputs:

```text
build/<libc>/<profile>/bin/capsulad
build/<libc>/<profile>/bin/capsulad-cli
build/<libc>/<profile>/lib/...
build/<libc>/<profile>/apps/*.tar.gz
build/installer/<os>-<arch>/capsulad-installer[.exe]
```

`make core` and `make apps` run inside the matching pinned builder image
(`tools/images/gnu.Dockerfile` or `tools/images/musl.Dockerfile`) on an arm64 host. The
builder selects the compiler, Rust target, ABI and glibc ceiling; there are no `CC`,
`GOOS`, `GOARCH`, `TARGET_ABI`, `NATIVE_TARGET` or `BUNDLE_*` inputs in the release path.
AirPlay is still built by its own pinned container recipe inside `make apps`.

`tools/release.sh core|apps|installer` turns those outputs into the flat GitHub release
assets and `release.json`/`apps.json`. Platform and boot-hook selection lives in that
script, not in Make.

The older granular targets (`native`, `interpreter`, `deploy-bundle`, the individual app
targets and the `sdk` targets) remain available as development and implementation helpers.
They are not part of the release interface and are not used by CI.

`make test` and `make test-apps` run on the host and are not device builds. `make web`
rebuilds the checked-in Web UI assets. Go targets whose `GOOS` is `darwin` still use
`-linkmode=external`, and the CLI still builds with `-tags=grpcnotrace`.

CI is split the same way:

- `.github/workflows/ci.yml` runs on pull requests, `main` and as the release gate.
- `.github/workflows/release-core.yml` runs on `v*` tags and publishes `release.json`
  plus the three platform bundles as the repository's latest release.
- `.github/workflows/release-apps.yml` runs on `apps-*` tags and updates the rolling
  `apps` prerelease with `apps.json` and the libc-prefixed app archives.

Both release workflows build on native arm64 runners inside the pinned builder images.
`tools/release.sh` maps a platform to its libc and boot hook, assembles the bundles, and
stages the flat GitHub assets. The app channel is marked prerelease so `latest/download/`
continues to resolve the core release.

## Cross build for the device

The Capsula Mini rev 4 (RK3308) is aarch64 with glibc 2.33. The Amlogic A113X Mini is
aarch64 with **glibc 2.28** on its stock image and **musl** on Alpine; see
[Amlogic A113X targets](#amlogic-a113x-targets) for the musl build and its current limit.

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
`rust:1.88-bullseye` for `make native … NATIVE_TARGET=aarch64-unknown-linux-gnu`.

Check the result before installing anything; every symbol must be at or below the target's glibc (2.33 on RK3308, 2.28 on Amlogic stock):

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

`npm run build` also runs `npm run build:kit` (before Vite). It compiles the stylesheet injected
into application settings documents from `web/src/settings-kit.css` and
`web/tailwind.settings.config.js` (Tailwind 3.4.17 + daisyUI 4.12.23) into the checked-in
`internal/appui/assets/kit.css`, which `internal/appui` embeds. The build scans
`internal/appui/assets/*.html` and `apps/*/ui/settings.html` (class attributes and literal class
strings in the inline script), verifies every scanned class against the generated CSS, rejects
remote `url()`/`@import`/font references, and enforces a 160 KiB ceiling. `npm run check:kit`
fails when the checked-in copy is stale. The `theme.config.js` palette is shared with the SPA, so
the `capsulad` (light) and `capsulad-dark` themes are byte-for-byte the same tokens in both
outputs; add a class used by a settings document to the config's safelist when the checked-in
build must keep emitting it.

The checked-in `internal/webui/dist` is embedded with `go:embed`, so an ordinary Go build
needs no Node toolchain. For frontend work, run Vite against a live simulator daemon; Vite
serves sources and proxies `/api`, `/healthz` and `/ws` to the daemon's real HTTPS surface.
Every user-visible string lives in `web/src/locales/en-GB.json` (the source catalogue) and
`ru-RU.json`; add a locale by importing its catalogue, adding it to the `MESSAGES` map in
`src/lib/i18n.js`, and adding one `AVAILABLE_LOCALES` entry. The catalogues are JSON so
`@intlify/unplugin-vue-i18n` precompiles them at build time; the bundle uses the runtime-only
vue-i18n build, and `scripts/check-csp.mjs` fails the build if `new Function` or `eval` returns.
`npm test` checks key parity, placeholders and locale-sensitive formatting. The Access page
downloads the server certificate from `/api/cert.pem` and the same self-signed certificate as a
CA file from `/api/ca.pem`; a plaintext HTTP request to the HTTPS listener gets a guidance page
instead of a TLS error.
The served tree also carries `LICENSE-material-symbols.txt` and `THIRD-PARTY.md`, copied
verbatim from `web/public` by the Vite build and reachable at `/LICENSE-material-symbols.txt`
and `/THIRD-PARTY.md`, so a released binary ships the Apache-2.0 notice for the font it
serves. The generated settings-document kit adds `LICENSE-tailwindcss.txt` and
`LICENSE-daisyui.txt` (full MIT copyright and permission texts for the installed
`tailwindcss` and `daisyui` versions) and lists both in `THIRD-PARTY.md`; the generated
`internal/appui/assets/kit.css` banner repeats those notices so the embedded stylesheet is
self-describing. `web/src/assets/LICENSE-material-symbols.txt` stays the source-tree copy next
to the font; the web tests keep the two licence copies byte-identical.

The icon font is a checked-in derived asset, not a post-build edit. `web/src/assets` holds a
40,336-byte subset of Material Symbols Rounded (90 glyphs: 30 icon ligatures, their 20
`.fill` alternates, `.notdef`, and 39 ASCII components — space, period, underscore,
`digit_zero`–`digit_nine` and lowercase `a`–`z`, with uppercase mapped to the same glyphs;
FILL/GRAD/opsz/wght axes intact) and
`icon-font.json` with its glyph/fill lists, source hash and output hash. `npm run build` runs
`npm run check:icons` first: it verifies the hash, size limit and declared glyphs, extracts
every static `<Icon>` body plus the toast, Wi-Fi signal and settings-section lists in
`src/lib/iconNames.js`, and fails when an icon has no glyph, a `.fill` alternate is missing,
the subset is stale, or a source references the full `material-symbols` package — the bare
specifier, any `material-symbols/...` subpath (a stylesheet, the `.woff2` files, the scss
entry points) or a `url()` to its font. A `<Icon>` that cannot contribute a name also fails
with its source line: a self-closing `<Icon ... />` or a tag without `</Icon>` has no slot
text, so it would otherwise ship a missing glyph. After adding or
changing an icon name, regenerate the subset (a Python venv, not the SPA build):

```sh
cd web
python3 -m venv ../build/font-venv
../build/font-venv/bin/pip install -r scripts/requirements-fonts.txt
../build/font-venv/bin/python scripts/build-icon-font.py
```

`npm run fonts` runs the same script with the `python3` on `PATH`; use it only from an
environment that already has the pinned packages.

The generator pins `material-symbols` 0.48.3 and fontTools 4.68.0, rejects a name the font
does not provide, keeps every existing `<icon>.fill` alternate, shapes each icon at FILL 0 and
FILL 1 against the full font, rejects changed variable axes, and produces the same bytes on
two runs before writing. The `.fill` glyphs are not optional trimming: Material Symbols uses
GSUB FeatureVariations to swap the base ligature for its filled alternate when FILL ≥ 0.99,
so dropping them changes the rendered icon. The subset filename and `icon-font.json` must
change together.

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

`sh tools/dev-media.sh build/dev-web` installs the stopped `dev.media` fixture for exercising the
Overview "Now playing" card: it starts READY and stopped, so the card reads "Nothing is playing"
until playback actually starts (a simulator tap, the app's `Play` action or an operator media
command), and the row appears and disappears live without a reload. The card is a strict
projection — only `PLAYBACK_PLAYING` players, and a provider with none is omitted — so an idle or
paused provider is deliberately absent there while `capsulad-cli media list`, `ListPlayers` and
the explicit transport controls still report and reach it.

The same listener also serves the daemon's MCP frontend at `https://HOST:8443/mcp`: an
API-key surface on the same port and process. Use `assistant` for day-to-day voice control,
`operator` for administration, and `status` for read-only polling. The browser's Access page
creates status keys. See [MCP.md](MCP.md) for the exact grants.

```sh
./build/bin/capsulad-cli -data-dir build/dev-web auth key create agent --scope assistant
```

## Application artifacts

An app package is a gzip-compressed tar with `manifest.toml` at the root, sorted entries,
zeroed timestamps/ownership and modes normalized to 0755 or 0444, so the same tree produces
identical bytes. Absolute paths, `..`, links, devices, FIFOs and duplicate paths are
rejected. Pinned limits: 48 MiB per file, 64 MiB unpacked, 4096 entries. The app-facing
contract for the artifact, manifest and install flow is
[app_development/packaging-deployment.md](app_development/packaging-deployment.md).

Packaging runs on the build host, never on the device. A package may carry one optional root
`README.md` user guide — inert Markdown the Web UI serves from
`GET /api/apps/{appID}/guide` and renders as text in the app detail, for a running or a stopped
app alike (built-in apps use embedded guides; a package without one is reported as having no
guide). Every first-party build script stages its own, and the guide is capped at 64 KiB, must
be a regular file and must be valid UTF-8. See
[app_development/manifest.md](app_development/manifest.md#user-guide-readmemd).

Each app build target compiles its tree
and then packages it with `capsulad-cli app package DIR`, which writes `<id>-<version>.tar.gz`
beside the package directory: `make dumbassistant` leaves
`build/apps/org.capsulad.dumbassistant-<version>.tar.gz`, `make marusia` leaves
`build/apps/org.capsulad.marusia-<version>.tar.gz`, `make cadence` leaves
`build/apps/org.capsulad.cadence-<version>.tar.gz` (add `TARGET_ABI=linux-musl` for the
Alpine board, which also rewrites the staged manifest and checks the binary's loader), and
`make opencapsula` leaves `build/apps/org.capsulad.opencapsula-<version>.tar.gz`. The
OpenCapsula client is native and needs a built host runtime (`make native`) like the other
native apps; for the device build it with the target toolchain, for example
`GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc make opencapsula`. The product apps follow the same layout:
`make simon` leaves `build/apps/org.capsulad.simon-<version>.tar.gz`, `make pa`
`org.capsulad.pa-<version>.tar.gz` and `make radio` `org.capsulad.radio-<version>.tar.gz`.
`apps/simon/build.sh`, `apps/pa/build.sh`, `apps/radio/build.sh` and
`apps/homeassistant/build.sh` take `PACKAGE=0` to stage the directories without archives,
`CAPSULAD_CLI=/path/to/capsulad-cli` to choose the packager, and `OUT=`
(`apps/radio/Makefile` wraps that script for local use). Every first-party package stages its
own root `README.md` next to `manifest.toml`, because that file is the app's user guide in the
Web UI; a build script that cannot find it fails rather than packaging a guide-less app.
A cross build packages with the host-executable `build/bin/capsulad-cli-host`
(`make capsulad-cli-host`), because the cross-target CLI cannot run on the build host.

The device receives archives only:

```sh
capsulad-cli app install build/apps/org.capsulad.dumbassistant-<version>.tar.gz
capsulad-cli app install build/deploy-bundle/apps/org.capsulad.simon-<version>.tar.gz
capsulad-cli app list
capsulad-cli app start APP_ID [NAME=VALUE ...]
capsulad-cli app runs [APP_ID]
capsulad-cli app stop RUN_ID
capsulad-cli app remove APP_ID
```

`app install` accepts an archive and treats a package directory as a developer convenience, not
a separate format. It validates the manifest, the settings-UI document and any `[native_sdk]`
requirement, stages the package under `<data-dir>/apps`, and renames it into place. An
existing installation becomes an atomic `update`; a live run must be stopped first. Built-in
system apps have no package and cannot be installed, removed or shadowed.

Install and remove act on the **running** daemon. `app install` dials the socket first: when the
daemon answers, the CLI streams the archive to `ApplicationService/InstallApplication` and the
daemon installs it into the live registry, rescanning the catalog, dropping the cached
provenance for that id and publishing an invalidation to the Web UI, so a new or replaced
package is usable immediately and needs no restart. `app remove` likewise reaches
`ApplicationService/UninstallApplication`. The same operations are exposed to the
session-authenticated Web UI as `POST /api/apps` (raw `.tar.gz` body, 65 MiB cap — the 64 MiB
unpacked budget plus gzip slack) and `DELETE /api/apps/{appID}`, behind the Assets-style
admin session on the Apps page; API keys are refused. The Apps page also installs a package
from an http(s) URL through `POST /api/apps/url`, and reads storage figures from
`GET /api/apps/storage` and `GET /api/apps/{appID}/storage`.

Only when the socket is provably absent (`ENOENT`, `ECONNREFUSED`, `ENOTSOCK`) does the CLI fall
back to the previous on-disk path under `<data-dir>/apps` and print that the daemon must be
restarted to see the change. Any other dial failure (timeout, permission, transport) refuses
rather than writing the package tree behind a daemon that may be live, and a daemon that answers
with `UNIMPLEMENTED` is told to be redeployed or stopped, never silently downgraded. A streamed
upload is supervised — one storage admission at a time, 60 s idle and 30 min total, with a
free-space guard and a native-SDK preflight — so a stalled or oversized transfer is refused
(too slow, too large, no space, malformed) instead of occupying the package tree. A compiled-in
system app, a not-installed app and a **running** app are refused with their own message —
`app <id> is running (run <run-id>); stop it before replacing the package`, `… before removing`,
`app <id> is a built-in system application and cannot be replaced`/`… uninstalled`,
`app <id> is not installed`.

A fresh install is not started automatically (`start_on_boot` applies at daemon start only), an
update never resurrects an app the user stopped, and removal keeps the app's stored settings
under `<data-dir>/app-settings/<app-id>/settings.json` and never rewrites a persisted
default-provider selection. With the daemon stopped or with `deploy -restart never`, the
on-disk path above still applies: those packages become visible at the next start. (Host and
simulator evidence for the live install/remove path and the Apps page is recorded in
[VERIFICATION.md](VERIFICATION.md#verified-only-on-the-simulator-or-host).)

`app start` on a running app delivers an invocation instead of spawning a second process; every
app is a singleton. A launch while the existing run is stopping is refused with
`FAILED_PRECONDITION`.

## Terminal installer

`installer/` is a separate host Go module (`capsulad/installer`) for the cross-platform
terminal installer. It is never built into a device image: `make installer` builds
`build/installer/capsulad-installer` for the host and `make test-installer` runs its fake-device
and in-process TLS suites. The installer talks to stock over ADB and Alpine over SSH, discovers
the platform, streams a pre-change backup, deploys a release bundle (which must carry
`capsulad-cli`) detached, then configures credentials, Wi-Fi, first-party apps and Marusia. Its
release manifest, artifact layout, security model and limits are in
[installer/README.md](../installer/README.md).

## Deployment bundles

`make deploy-bundle` writes `build/deploy-bundle/` and `build/deploy-bundle.tar.gz` from the
current build outputs. Assembly never compiles or packages an app: it copies the ready-made
`<id>-<version>.tar.gz` archives from `build/apps` and fails, naming the target to run, when a
staged package directory has no archive. Build the app artifacts on the build host first when
they belong in the bundle. The bundle contains host-targeted binaries unless you built with
`GOOS`/`GOARCH` and `NATIVE_TARGET` for the device: a host bundle is for simulator work, and
installing it on the Capsula Mini fails the daemon's self-check (`-version` cannot execute
there). `BUNDLE_CLI=0` opts into a CLI-free device bundle; the default (`BUNDLE_CLI=1`) keeps
the documented adb flow. The target is rebuilt from an empty directory, so changing the mode
cannot leave a CLI from a previous bundle in the output.

```text
capsulad                    daemon binary
capsulad-cli                CLI binary (omitted with BUNDLE_CLI=0)
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
installing a copy of itself, and `BUNDLE_CLI=0` assembles such a bundle. Because the CLI is
then not a build prerequisite either, a CLI-free bundle can be assembled from the daemon and
app outputs alone. Flags:

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
7. Replace `capsulad-cli` when the bundle carries it, then `capsulad`. A staged file on the
   target filesystem is renamed into place, and the replaced binary is kept as `<name>.prev`.
   A staged file elsewhere (the normal case, since staging is on the temporary filesystem)
   cannot be renamed: the replaced binary is compressed into staging (~25 MiB for the 40 MiB
   daemon), the target is removed to release its space, the staged bytes are written directly
   to the target path and chmodded 0755. No sibling copy of the new binary is ever created on
   `/data`. A staged file inside the staging tree is deleted once it is written; a directory
   bundle is left alone unless it shares the target filesystem, where the rename consumes its
   entry — rebuild the bundle before repeating that path.
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

### CLI-free bundles

`make deploy-bundle BUNDLE_CLI=0` writes the same layout without `capsulad-cli`. Build the
daemon and the device CLI (`make daemon GOOS=linux GOARCH=arm64`, or the full cross recipe
above for the native runtime), assemble, then push the CLI to `/tmp`:

```sh
make daemon GOOS=linux GOARCH=arm64
make deploy-bundle BUNDLE_CLI=0 GOOS=linux GOARCH=arm64 \
  NATIVE_TARGET=aarch64-unknown-linux-gnu
adb -s <device> push build/bin/capsulad-cli /tmp/capsulad-cli
adb -s <device> push build/deploy-bundle /tmp/capsulad-bundle
adb -s <device> shell chmod 0755 /tmp/capsulad-cli
adb -s <device> shell /tmp/capsulad-cli -data-dir /data/capsdemo \
  deploy /tmp/capsulad-bundle -timeout 60s
```

The example assumes `build/lib` came from the cross recipe above; pass `BUNDLE_NATIVE=0` only
for a deliberately static/script-only bundle.

The pushed CLI is not installed: it lives on the tmpfs, consumes no `/data` blocks, and
disappears on reboot. The deployer only replaces files the bundle carries, so an already
installed `<data-dir>/capsulad-cli` is neither removed nor updated by a CLI-free bundle; it
remains the device's recovery copy but must be refreshed deliberately when the CLI changes.
A board without an installed CLI needs the `/tmp` push again after every reboot before the
next deploy or recovery. Do not run the CLI from `/data` while the same deployment replaces
it; run the pushed `/tmp` copy as above. An interrupted deployment is recovered exactly as
under [Over adb](#over-adb) (`S99btctl.sh start` and the installed-binary check); a CLI-free
bundle changes where the operator runs the CLI, not the rollback path.

Storage tradeoff: a CLI-free bundle removes the CLI's bytes from the tarball and from the
deployment write path, and removes the CLI-sized pre-deploy free-space requirement. It
reclaims `/data` only when no CLI is installed there (the pushed `/tmp` flow above); a
separately installed recovery CLI keeps consuming its own space and is not deleted. The saving
is the CLI binary's own size in the tarball and the pre-deploy free-space check.

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
interface.** On the Rockchip stock board the daemon owns the Wi-Fi station, so `deploy` stopping
it drops the link, which can kill an attached `adb shell` mid-deployment after the old daemon was
stopped and the binaries swapped but before the restart, leaving the board with no running
daemon. `nohup ... &` is not enough (the shell's process group is killed); use a script and the
init helper:

```sh
# /tmp/deploy.sh
umask 022
/tmp/capsulad-bundle/capsulad-cli -data-dir /data/capsdemo deploy /tmp/capsulad-bundle \
  -staging /run -timeout 180s > /tmp/deploy.log 2>&1
echo "exit=$?" >> /tmp/deploy.log
```
```sh
adb shell chmod +x /tmp/deploy.sh
adb shell start-stop-daemon -S -b -x /tmp/deploy.sh
# reconnect when the link returns, then read /tmp/deploy.log and the health gate
```
Keep `umask 022` for the service launch. The stock mount-namespace helper creates
app-visible directory components under the inherited umask; a restart under `077`
can pass daemon health while leaving apps unable to traverse those directories.
Verify the expected app runs reach `APP_READY` after the restart.

If a deployment is cut off between `stopped the daemon` and `restarted`, start the stack with
`/etc/init.d/S99btctl.sh start` (never by running the daemon by hand) and check the installed
binary's md5 before retrying. Staging defaults to `/tmp/capsulad-deploy-*`; the bundle itself
can live on `/run` when vendor files consume `/tmp`. Run the CLI from the pushed bundle so
the installed `/data/capsdemo/capsulad-cli` is not the executable being replaced. Keep a host
rollback copy of `capsulad` and `capsulad-cli` (`adb pull`) until the health gate and the
app/web checks pass; a reset during the direct daemon write can leave a truncated target.

### The watchdog window

`capsulad` owns `/dev/watchdog` while it runs and feeds it **only while the output loop keeps
stepping**. Both the keepalive cadence and the gate are derived from the interval the driver
actually reports (`WDIOC_GETTIMEOUT`): the cadence is a third of it, capped at 2 s, and the stall
window is half of it, capped at 10 s, so both stay strictly inside the timer the hardware runs —
a fixed window would either miss a short timer or reset a long one. A driver that reports nothing
falls back to the profile's declared interval and the state says so. If the output loop stops
completing steps, or a keepalive write fails, the supervisor stops feeding and leaves the armed
timer to reset the board. A live goroutine is not liveness — this is the case the hardware timer
exists for. Opening the device arms the timer, so the supervisor writes a keepalive before it
returns from the open. The loop then ticks at that same derived cadence — for a timer shorter than
the 500 ms probe interval it wakes faster than the probe cadence instead of rounding the keepalive
up to it — so the effective keepalive period is the cadence itself.

On this kernel the timer cannot be disarmed at all: the magic close (`'V'`) is ignored and
`WDIOC_SETOPTIONS(WDIOS_DISABLECARD)` returns `ENOTTY`. That is
`profile.Watchdog.Nowayout: true`, and it means `Close` is not a stop button. A graceful stop
(`SIGTERM`, `S99btctl.sh stop`) therefore hands the descriptor to a detached BusyBox `sh`
feeder (`run/watchdog.feeder`, `run/watchdog.handback`) instead of closing it.

The release rule is deliberately conservative and does not depend on that field: a close is only
proven to stop the timer on a driver measured to honour the magic close, and `Nowayout` records a
measured fact rather than a guess. Any driver whose disarm semantics are unmeasured — Amlogic
stock, until someone measures it — is handed off as well, so a wrong guess can never reset the
board during a planned restart or deployment. `Close` is used only when the platform exposes no
descriptor to transfer, and the outcome is reported as it is: `closed; the hardware timer is
disarmed`, or `closed, but this driver ignores the magic close; the timer is still armed and the
board will reset`. The same honesty applies to a failed handoff or close: the state goes
`healthy=false` and the detail says the board will reset instead of claiming a release happened.

The feeder writes a keepalive every 2 s for at most `feederWindow` (5 min), then stops feeding
and exits. That bound is deliberate, not a bug: a planned stop does not leave the speaker running
indefinitely — with no daemon back within the window, the armed timer expires and the board
resets, into a fresh boot that the boot service owns. The feeder runs `/bin/sh`, never a capsulad
image, so it pins no inode on `/data`; a feeder left by an older release is a copy of that daemon
and pins its 40 MiB inode.

`S99btctl.sh start` asks the feeder for the timer back before it launches the daemon, and the
daemon asks again before its first `open`, so the boot service never starts into an `EBUSY`
open. `disable` and the vendor fallback release the feeder before starting the vendor chain,
which then owns the timer again.

The release is ordered, not best-effort. `disarm` is idempotent *and blocking*: the first caller
performs the release and every other caller waits for it, and the daemon's shutdown calls
`disarm` and then waits for the supervisor goroutine, so the process cannot exit between spawning
a feeder and that feeder existing. An open that completes after the supervisor was released is
handed off the same way instead of being stored as an armed timer nobody feeds. Within the daemon
the paths are serialized: a release stops the loop from opening or feeding any further, and it
runs exactly once with every other caller waiting on it, so the device never has two owners inside
this process.

Deployment ordering: `deploy` first compresses the replaced binary into staging while the
feeder is still feeding, then asks it to hand the timer back (a shell feeder pins nothing and
is skipped; a daemon-copy feeder is asked), waits for `run/watchdog.feeder` to disappear — which
is what releases the replaced inode's space — and removes the marker. So the gzip does not
consume the window; only removing the old file, writing the new binary and restarting do, and
whatever owner arms the device next has the remaining armed timeout (~21 s) to do it. If the
feeder is still there after the wait, the deployment aborts before removing the daemon (its
space is pinned, so the write could not succeed), drops the useless backup and restarts the
previous stack. A same-filesystem rename lands instantly; the cross-filesystem write is the
slow part, and a watchdog reset during it leaves a truncated target. Re-run the deployment
after the board reboots rather than assuming the binary is complete.

Operating consequences:

- `S99btctl.sh stop` alone does not leave the speaker running indefinitely. The feeder covers a
  restart, an upgrade and a short inspection window (5 min); after that the board resets. To
  take the framework out of service, run `disable`, which releases the feeder and gives the
  timer back to the vendor chain.
- Never `kill -9` a feeder: on a nowayout timer the last feeder is the only thing keeping the
  board alive, and killing it is the reset path. `capsulad stop`, `S99btctl.sh` and the deploy
  tool all use the handback marker instead. `supersedeVendor` skips any process whose argv
  carries `capsulad-watchdog-feeder` (or the older `__capsulad_watchdog_feeder`) and asks it
  to release rather than signalling it.
- `run/watchdog.feeder` is on the persistent partition, so it survives a reboot or a hard kill.
  A file whose pid is gone (or whose pid now belongs to another process) is stale: the daemon,
  the boot script and the deploy tool clear it instead of waiting for a handback that cannot
  arrive.
- `-watchdog off` leaves the timer entirely to the vendor agent; `-watchdog /path` arms a
  different device. Both are diagnostic switches, not a supported deployment mode.

## Device layout and boot hook

```text
/data/capsdemo/capsulad
/data/capsdemo/capsulad-cli
/data/capsdemo/lib/               libcapsulad.so.1.0.0, capsulad-native.json, script hosts, include/
/data/capsdemo/apps/<app_id>/
/data/capsdemo/assets/
/data/capsdemo/run/hal.sock
/data/capsdemo/security/
/data/capsdemo/capsulad.log       active daemon log (500 KiB cap; older logs rotate to /tmp/capsulad-logs, 2 MiB total)
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

`auth init` creates the certificate and sets an admin password (printed once when
generated); open `https://127.0.0.1:8443/`. Without `-dev-admin`, the
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
`-single-app <app-id>`, `-dev-admin`, `-mem-limit-mb`, `-output-trace`,
`-wakeword-trace`, `-watchdog`.

`-single-app <app-id>` is a launch policy for a dedicated speaker deployment: the daemon
starts that application at boot — even when its manifest does not set `start_on_boot` or
`background` — and starts no other application, including selected providers,
framework-owned compiled-in helpers and other `background` apps. `Launch` of any other app
is refused. The Web UI and MCP surface keep their normal defaults; pass an empty
`-web-addr` to exclude them deliberately. Stop keeps the normal rule — a selected
provider restarts, an unselected app stays stopped — and the app's own
`restart_on_failure` contract still applies. An unknown ID fails startup instead of
serving an empty device.

Platform selection:

| `-platform` | Backend | Notes |
|---|---|---|
| `simulator` (default) | in-memory simulator | neutral `profile.Simulator`: no device paths or watchdog |
| `rockchip-stock` (alias `rockchip`) | RK3308 stock backend | hardware profile; the stock watchdog opener is wired by `cmd/capsulad` |
| `amlogic-stock` | Amlogic A113X stock backend | stock Buildroot, glibc 2.28; the clock panel's vendor ABI is unrecovered and reports `UNVERIFIED` |
| `amlogic-alpine` | Amlogic A113X Alpine backend | Alpine, musl; the panel and ring are probed, not assumed |

Any other `-platform` value fails with `unknown platform ... (simulator, rockchip-stock,
amlogic-stock, amlogic-alpine)`. See [Amlogic A113X targets](#amlogic-a113x-targets) for the
cross-build and boot-hook details.

## Tests

```sh
make test              # every Go module: root, sdk/go, the four assistants, the native Internet
                       # Radio, the Home Assistant integration and the AirPlay 2
                       # receiver, the Rust binding, the native crate, the C++ header
                       # compile and the terminal installer's host suite
make test-race         # root Go module with -race
make test-installer    # the nested installer module (host tool; no device or network)
make test-sdk          # ABI check, Go binding, Rust binding, native crate, C++ header compile
make test-marusia      # the apps/marusia module's tests from the repository root
make test-cadence      # the apps/cadence module: wire goldens, websocket framing, Opus, turn engine, announcements, enrollment, settings
make test-opencapsula  # the nested apps/opencapsula Go module
make test-radio        # the nested apps/radio Go module (stations, media, streaming)
make test-homeassistant # the nested apps/homeassistant Go module
make test-airplay      # the nested apps/airplay Go module (protocol, codecs, session)
```

`make test-cadence` needs no daemon, device or Cadence server: it decodes the upstream
`cadence.v2` golden frames as interoperability vectors, runs an in-process websocket peer and a
fake-codec turn and announcement state machine, exercises the enrollment exchange and its
compare-and-swap token write against fakes, and performs one real libopus encode/decode round
trip. The Opus test skips with a clear message when the host has no libopus; the device loads
`libopus.so.0` at run time, so the app has no link-time codec dependency.

`make test-airplay` needs neither a daemon nor a device: the protocol layers are pure Go and the
one end-to-end session test drives the receiver over in-memory transports. Its ALAC fixtures were
produced by ffmpeg and are checked in with their reference PCM, so the decoder is verified against
a real encoder rather than against itself.

The script-app suites need Node or CPython, so they are not part of `make test`;
`make test-apps` runs them together with the radio and bridge Go modules:

```sh
make test-apps         # test-simon, test-soundboard, test-hardwaretest, test-radio, test-homeassistant, test-pa and test-interpreter
make test-interpreter  # python3 interpreter/micropython/sdk_test.py (host prelude + timers)
make test-simon        # node --test apps/simon/test/*.test.mjs
make test-pa           # python3 apps/pa/tests/test_wav.py and test_app.py
make test-radio        # the nested apps/radio Go module from the repository root
```

`test-interpreter` needs no built host: the MicroPython prelude is pure Python, and its self-test
covers timer registration, the 16-callback cap, cancellation during a pass, clock safety,
exception isolation, the poll bound and stop-on-return.

`test-simon` loads the shipped `main.js` into a fake QuickJS host and asserts the app's
observable requests, ring/segment frames, exact packaged per-colour tone PCM, RIFF
boundary/padding regressions, input ordering and terminal cleanup; a packaging test runs
`build.sh` with `PACKAGE=0` and checks the staged `audio/` tree byte-for-byte. `test-pa`
exercises the shipped script's WAV
parser and protobuf helpers under CPython. `test-radio` is the app's own Go suite: station
grammar and selection, settings application, media snapshots, chunk sizing, idle accounting,
session ownership and the stream package's framing, ICY stripping, redirects, playlists and
codec detection. It contacts no network and needs no daemon.

The simulator end-to-end run lives with its app and needs `make capsulad capsulad-cli`,
`make native` and `make interpreter-micropython` first:

```sh
sh apps/pa/tests/e2e.sh        # install, boot, play, rejections and cancellation for the PA service
```

It uses a test-only caller (`apps/pa/tests/caller`) that is never installed on a device.

`make test` does not run the interpreters or device backends. Focused commands:

```sh
go test -ldflags '-linkmode=external' ./internal/routing ./internal/presentations
go test -race -ldflags '-linkmode=external' ./internal/daemon ./internal/audio
go test -run 'TestDumbAssistant|TestSupervisor' ./internal/daemon
(cd sdk/go && CGO_ENABLED=1 go test ./...)
(cd apps/dumbassistant && CGO_ENABLED=1 go test ./...)
(cd apps/marusia && go test ./...)        # no test reaches the network; TLS listeners are loopback
(cd apps/cadence && go test ./...)        # upstream golden frames, in-process peers, fake-codec turn/announcement state machine, enrollment
cargo test --manifest-path sdk/native/Cargo.toml
sdk/go/regenerate-abi.sh --check
```

`sh tools/check-deploy-bundle.sh` needs no Go build: it assembles the default and
`BUNDLE_CLI=0` bundles from stub build outputs with the real recipe, and checks the tarball
contents, the reused-directory behaviour, the absence of the CLI build/copy in the
omitted mode and the up-front rejection of an ambiguous `BUNDLE_CLI` value. The deploy-side
acceptance of a CLI-free bundle is covered by
`go test ./cmd/capsulad-cli -run TestDeployWithoutCLILeavesInstalledCLI`.

The networking surface has its own suites; every listener and peer is loopback and the mDNS
responder is a fake, so none of them needs a device or a network:

```sh
cargo test --manifest-path sdk/native/Cargo.toml --test net   # TCP/TLS, UDP, listener fd, mDNS
go test ./internal/appcontrol ./internal/discovery           # listener policy, normalize/manager
go test -run 'TestDiscovery|TestRemoveRunPublications' ./internal/daemon
go test -run 'TestControlChannel' ./internal/supervisor      # fd 3 + CAPSULAD_CONTROL_FD
cd sdk/go && CGO_ENABLED=1 go test -run 'TestTCP|TestUDP|TestListener|TestMDNS|TestNetwork|TestTLS' ./...
```

The Go binding and native-crate suites need a host `libcapsulad` and its metadata, exactly like
the other SDK tests: `make native` first, and `CAPSULAD_TEST_LIBRARY` for gated tests. The C ABI
identity itself is pinned by `make check-sdk-abi`, which `make test-sdk` runs first.

Standalone Go test commands against a Darwin target need `-ldflags '-linkmode=external'`; the
Makefile and app build scripts already handle it. Host daemon integration tests need a matching
host native runtime with metadata, normally `build/lib`, or the `CAPSULAD_NATIVE_LIB_DIR`
override; a target-platform library is not a usable host test runtime. Tests that start a daemon
also need the real host runtime and its `capsulad-native.json`.

The script-host smoke test covers ready, invocation, unary and routed calls, stream
cancellation, stop/crash cleanup and a daemon-crash disconnect:

```sh
make interpreter
sh interpreter/smoke/run.sh
```

## Interpreters

`make interpreter` builds both hosts and `make interpreter-quickjs` /
`make interpreter-micropython` one. They come from pinned sha256-verified sources
(quickjs-ng 0.18.0, MicroPython `1c6321…`); set `QUICKJS_SRC`/`MICROPYTHON_SRC` for
offline builds and note that they stage beside `libcapsulad`.

### Product apps

The first-party product packages use the same rule as the assistants: compile (when there is
anything to compile) and package on the build host, one `<id>-<version>.tar.gz` beside the
staged directory under `build/apps`. None of them is part of `all`, `release` or `debug`.

| Target | Package | What it is |
|---|---|---|
| `make simon` | `org.capsulad.simon` | Simon Says, a QuickJS colour-and-tone memory game: the LED ring replays a growing four-colour sequence (north red, east yellow, south green, west blue) with one packaged tone per colour, the four side keys repeat it, an optional four-digit segment display keeps the score, and holding the unmarked centre leaves the game. The package is `manifest.toml`, `main.js`, the app guide `README.md` and the pre-rendered `audio/*.wav` cues; the contract test suite stays out of it. It needs a QuickJS host with `Call.tryRecv` and `capsulad.readPackageFile`, so build `make interpreter-quickjs` and ship its `lib/capsulad-quickjs` — an older host makes the app log that it must be rebuilt and stop cleanly. |
| `make soundboard` | `org.capsulad.soundboard` | Soundboard, a QuickJS app: the centre key plays the active shared `/assets` WAV, left/right swipes switch sounds and briefly show the active name, and the ring lights the sound's fixed or random colour. Its package carries `manifest.toml`, `main.js`, `README.md` and `ui/settings.html`; user WAVs are assets, not package files. The host must provide `capsulad.readAssetFile` and `Call.tryRecv`; an older host logs that it must be rebuilt and stops. |
| `make hardwaretest` | `org.capsulad.hardwaretest` | Hardware Test, a native foreground app: `12:34` with the upper colon dot at 1 Hz and the lower at 2 Hz in ten-second phases, a clockwise rainbow chase, a 20% 440 Hz tone checked through the microphone with a red ring failure flash, and a full-panel capture showing 1-5 for the held key. Its package carries the binary, manifest and app guide; it needs the host native runtime and the rebuilt native SDK that understands `colonTop`/`colonBottom`. |
| `make pa` | `org.capsulad.pa` | PA Speaker, a background MicroPython service: another app routes one `SpeakerService/Play` (a streamed WAV) and the app plays it through the framework speaker, replying after the queued audio drains. |
| `make radio` | `org.capsulad.radio` | Internet Radio, a single native app: it keeps the station list, fetches one HTTP(S) MP3 stream through the shared `libcapsulad` transport, strips ICY metadata, decodes MP3 and writes paced PCM to the framework speaker in the same process, and serves the standard media contract. Its package carries the binary, the manifest, the `ui/settings.html` station picker, the media descriptor and the app guide. It installs as one package. |
| `make homeassistant` | `org.capsulad.homeassistant` | The Home Assistant integration, a native app: an MQTT client that publishes the speaker to Home Assistant through discovery, publishes state topics, and applies the commands it sends back to the ring, the segment display and the Bluetooth source. Its package carries the binary, the manifest, the descriptor and `docs/bridge.md`. |
| `make airplay` | `org.capsulad.airplay` | The AirPlay 2 receiver, a native app: HAP transient pairing, FairPlay handshake replies, an encrypted RTSP control channel, realtime ALAC/AAC-LC/PCM over RTP, the RAOP setup path, and the standard media contract for the live session. It uses daemon-mediated listeners for every inbound port — including the PTP multicast receiver — and the daemon's responder for `_airplay._tcp`/`_raop._tcp`, whose records carry the 32-byte ed25519 HomeKit key as `pk` inside a 63-byte instance label. AAC decoding links FFmpeg's `libavcodec`/`libavutil`: the build needs `pkg-config --exists libavcodec libavutil` on the host, and a device package needs the target libraries (see the app README). Its package carries the binary, the manifest, the media-contract descriptor, `docs/media.md`, the app guide `README.md` and `NOTICE`. |

```sh
make native                                            # the native apps link the shared SDK
# make airplay additionally needs FFmpeg's libavcodec/libavutil development files
make product-apps                                      # all five: simon, pa, radio, homeassistant, airplay
sh apps/radio/build.sh                                 # radio alone; OUT=/PACKAGE=/CAPSULAD_CLI= honored
capsulad-cli app install build/apps/org.capsulad.radio-<version>.tar.gz
```

The Simon package needs a QuickJS host with `Call.tryRecv` and `capsulad.readPackageFile`, so
run `make interpreter-quickjs` before `make deploy-bundle` when Simon ships in the bundle: the
bundle's `lib/` is what reaches the device, and an older host makes the app log that it has no
`readPackageFile` (or no `Call.tryRecv`) and stop rather than play.

`make airplay` additionally requires FFmpeg's `libavcodec`/`libavutil` development files. Its
`PACKAGE` default is `auto`: a Linux target packages, and a host build stages the directory only
(`PACKAGE=0`), because a Darwin binary is not a device package. `PACKAGE=1` on a host is refused
unless `AIRPLAY_ALLOW_HOST_PACKAGE=1` is set for a local packaging smoke test. For a device
package, pass `TARGET_ABI=linux-gnu` or `TARGET_ABI=linux-musl` together with
`AIRPLAY_FFMPEG_PREFIX` pointing at that target's FFmpeg: the app links a native decoder, so a
glibc-linked library can never be relabelled as a musl build. The build stamps the requested `abi`
into the staged manifest through `tools/stage-manifest-abi.sh`, runs
`tools/check-elf-interp.sh` on the staged binary before packaging, and fails without the target
prefix or a matching cross compiler.

Each app's own entrypoint is authoritative for its package contents:
`apps/simon/build.sh`, `apps/soundboard/build.sh`, `apps/hardwaretest/build.sh`,
`apps/pa/build.sh`, `apps/radio/build.sh`, `apps/homeassistant/build.sh` and
`apps/airplay/build.sh` — all of them honor `OUT=`,
`PACKAGE=0` and `CAPSULAD_CLI=` where they package. Simon, Soundboard and PA compile nothing; the
Hardware Test, radio, bridge and receiver are native and need the host native runtime
(`make native`) because their binaries link the shared SDK.

A native app's build host artifacts are **simulator** binaries; a device package is
the same script with the target toolchain, exactly like the other native apps:

```sh
GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/radio/build.sh
GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/homeassistant/build.sh
```

The internet radio is one package: it fetches, decodes and plays in its own supervised run, so
there is no second half to install, no app-to-app audio hop and no cross-app `Tune`
authorization. It refuses AAC/HE-AAC, Ogg/Opus/Vorbis, FLAC, MP4/M4A, WAV and HLS with an
explicit error, so the station list must be MP3 (MPEG-1/MPEG-2 Layer III). Its
`ui/settings.html` is packaged with it and renders the named station picker in the Web UI's app
detail; a selection writes the `station` setting through the sandboxed settings bridge. See
[`apps/radio/README.md`](../apps/radio/README.md) for the station-list format, the controls, the
display states and the streaming limits.

## Administration commands

```sh
capsulad-cli -data-dir build/demo status          # device, resources, runs, audio
capsulad-cli -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
capsulad-cli -data-dir build/demo auth set-password
capsulad-cli -data-dir build/demo auth key create automation
capsulad-cli -data-dir build/demo auth key create agent --scope operator
capsulad-cli -data-dir build/demo auth key list
capsulad-cli -data-dir build/demo auth key revoke automation
capsulad-cli -data-dir build/demo cert status
capsulad-cli -data-dir build/demo cert renew -days 365
capsulad-cli -data-dir build/demo settings show
capsulad-cli -data-dir build/demo settings app capsulad.clock blink=true
capsulad-cli -data-dir build/demo settings app capsulad.ring effect=static palette='#FF7828'
capsulad-cli -data-dir build/demo settings app org.example.assistant vk.token@- < token.txt
capsulad-cli -data-dir build/demo logs -app APP_ID -n 100
capsulad-cli -data-dir build/demo media list
capsulad-cli -data-dir build/demo wifi status
capsulad-cli -data-dir build/demo wifi scan
capsulad-cli -data-dir build/demo wifi connect 'Home Wi-Fi' -password-stdin -security wpa2
capsulad-cli -data-dir build/demo wifi forget 'Home Wi-Fi'
capsulad-cli platform inspect -root /path/to/extracted/rootfs
capsulad-cli platform ledtest -ring
```

Global flags precede the command: `-data-dir DIR`, `-socket PATH`, `-security-dir DIR`,
`-native-lib-dir DIR`, `-json`. `settings app APPID key=value` reads values as JSON when
they parse and as strings otherwise. `settings app APPID KEY@-` (or `KEY=-`) reads exactly
one line from stdin, strips the line ending, and stores it as a raw JSON string without
sniffing, so a numeric-looking token stays a string; it must be the only setting in the
invocation. Use this form for session tokens and other secrets so they never appear in the
device CLI's argv or `/proc/<pid>/cmdline`. `cert renew` requires a daemon restart to reload
the listener certificate; `auth` changes apply live because the daemon watches the security
store. A newly issued server certificate is self-signed with CA basic constraints and the
certificate-signing key usage, so it can be imported as the trust anchor the Web UI guidance
describes; the same certificate is still used for TLS. Certificates already on a device gain
this only through `cert renew` or a fresh `auth init`.

`auth init` and `auth set-password` accept `-password PASS` or `-password-stdin`; stdin reads
exactly one line and the two flags are mutually exclusive. With neither flag an interactive
terminal prompts, and a non-terminal `set-password` still generates a password. Use
`-password-stdin` from an installer so the password never appears in `ps` or shell history.

The `wifi` commands are the installer's station/provisioning surface. They call the local
HTTPS administration API on `-web-url` (default `https://127.0.0.1:8443`, as bound by the
device boot hooks) and are therefore available before the browser UI is initialised, as soon
as `capsulad-cli auth init` has created the certificate and administrator. Each command mints
a short-lived `cli-wifi-<random>` operator key from the local security store, presents it as
`Authorization: Bearer`, retries a 401 for up to three seconds while the daemon reloads the
store, and revokes the key before exiting. Only the device certificate at
`<security-dir>/server.crt` is trusted; the system roots are not added, so a lookalike
certificate for the same host cannot impersonate the daemon. There is no insecure mode.
A `-web-url` whose host is not covered by the certificate SANs fails with a hostname message,
so use `127.0.0.1` or renew the certificate with the intended name.

```sh
capsulad-cli wifi status [-web-url URL]
capsulad-cli wifi scan [-web-url URL]
capsulad-cli wifi connect SSID [-password PASS | -password-stdin] \
    [-security wpa2|wpa3|open|...] [-web-url URL]
capsulad-cli wifi forget SSID [-web-url URL]
```

`wifi status` never scans; `wifi scan` is the only command that energizes the radio.
`-security` defaults to WPA2, which requires a passphrase; pass `-security open` for an open
network. The installer supplies the mode explicitly rather than inferring it from a scan.
`-password-stdin` reads one passphrase line, and neither the passphrase nor the temporary key
is printed. Without `-json` the human output is a status block (state, SSID, IP, access point
and saved networks), a scan list (SSID, security, signal), or the daemon's connect/forget
message. With `-json` the CLI prints the daemon response object indented, preserving its
fields:

```json
{"wifi": {"available": true, "state": "connected", "interface": "wlan0", "ssid": "Home Wi-Fi",
  "ip_address": "192.168.1.20", "security": "wpa2", "detail": "", "ap_active": false,
  "ap_ssid": "", "ap_address": "", "ap_portal_url": "", "ap_password_hint": "",
  "scan_supported": true, "last_error": "",
  "known_networks": [{"ssid": "Home Wi-Fi", "security": "wpa2", "has_password": true, "connected": true}]}}
```

`wifi scan` adds `{"message": "...", "results": [{"ssid": "...", "security": "wpa2",
"signal_dbm": -52, "frequency_mhz": 2437, "connected": false}], "wifi": {...}}`; connect and
forget return `{"message": "...", "wifi": {...}}`. A non-2xx reply exits non-zero with the
daemon's `error` text. The routes live on the Web UI listener for CLI use; they are
administrative plumbing, not an application-facing API.

## Amlogic A113X targets

The Amlogic A113X board runs two supported userlands, and the same binaries serve both:
`-platform amlogic-stock` (jailbroken stock Buildroot, glibc 2.28) and
`-platform amlogic-alpine` (Alpine 3.24.1, musl 1.2.6, kernel 6.18). The profile carries the boot service, hook name,
audio path, panel naming and free-space floors; the composition root only wires
`profile.AmlogicA113XStock` / `profile.AmlogicA113XAlpine` into `backend/amlogic`.

```sh
# Daemon and CLI for either Amlogic userland (Go only).
make daemon GOOS=linux GOARCH=arm64

# Amlogic stock is glibc 2.28: a bullseye-era build links GLIBC_2.29+ and cannot
# load there. Build the runtime against buster (archived repos) with the helper;
# it verifies the symbol ceiling and writes build/lib + metadata.
sh tools/build-native-glibc228.sh
make deploy-bundle DEPLOY_PLATFORM=amlogic-stock BUNDLE_NATIVE=0   # lib/ already staged

# Native SDK: the Rust target follows the libc the image actually has.
# DEPLOY_PLATFORM also defaults NATIVE_TARGET (gnu for stock, musl for Alpine),
# so the two lines below are what the bundle targets do implicitly.
make native GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-gnu   # stock
make native GOOS=linux GOARCH=arm64 NATIVE_TARGET=aarch64-unknown-linux-musl  # Alpine

# Bundle with the matching boot hook, then deploy with the matching -platform.
make deploy-bundle DEPLOY_PLATFORM=amlogic-stock
capsulad-cli deploy build/deploy-bundle.tar.gz -platform amlogic-stock

make deploy-bundle DEPLOY_PLATFORM=amlogic-alpine
capsulad-cli deploy build/deploy-bundle.tar.gz -platform amlogic-alpine
```

`-platform` on `deploy` selects the boot service (`/etc/init.d/S99sysmon.sh` on stock,
`/etc/init.d/capsulad` on Alpine), the hook name inside `boot/` and the free-space floors. The
bundle layout, validation, rollback and health gate are unchanged. `make deploy-bundle` without
`DEPLOY_PLATFORM` still assembles the Rockchip hook.

A native SDK shared library is not the only libc-sensitive artifact: a native **app** package
carries its own binary, and a glibc-linked one cannot exec under musl however its manifest reads.
An app's manifest `entrypoint.abi` must therefore agree with the binary. `apps/cadence` builds to
`TARGET_ABI` (`linux-gnu` by default, `linux-musl`/`musl` for Alpine): it defaults the cross
compiler to `aarch64-linux-musl-gcc`, writes `abi = "linux-musl"` into the staged manifest, and
`make check-abi` reads the binary's ELF interpreter and refuses a package whose libc disagrees.

The bundle gate is libc-aware: with `DEPLOY_PLATFORM=amlogic-alpine` (or `amlogic-stock`) it
requires `lib/libcapsulad.so` and runs `tools/check-native-libc.sh`, which reads the ELF dynamic
section (`DT_NEEDED` via `readelf`, or an ELF-capable `objdump`) and rejects a runtime linked against
the other libc, or a static one with no libc dependency. It never guesses from a string inside the
file.

Building the musl runtime works; it needs one target-specific flag, already in the Makefile:

```sh
# arm64 host; the image is pulled into the local Docker store, no privileged change
docker run --rm --platform linux/arm64 -v "$PWD":/src -w /src/sdk/native rust:alpine \
  sh -c 'apk add --no-cache build-base; \
         RUSTFLAGS="-C target-feature=-crt-static" \
         cargo build --locked --release --target aarch64-unknown-linux-musl'
```

Rust musl targets default to `crt-static`, and a static runtime drops the `cdylib` crate type;
`-C target-feature=-crt-static` links the library against the dynamic musl and produces
`libcapsulad.so` (SONAME `libcapsulad.so.1`, `NEEDED libc.musl-aarch64.so.1`, `libgcc_s.so.1`).
`make native` applies that flag **only** for a `*-linux-musl` `NATIVE_TARGET`
(`NATIVE_RUSTFLAGS`), so glibc and Darwin builds are unchanged. The emitted
`capsulad-native.json` is the usual `{abi_major:1, abi_minor:0, library:"libcapsulad.so.1"}`.

Verified in an arm64 Alpine container: `tools/check-native-libc.sh` reports `libc=musl`, `ldd`
shows `/lib/ld-musl-aarch64.so.1`, and a C `dlopen("libcapsulad.so.1")` + `dlsym("capsulad_abi_major")`
smoke loads the runtime and resolves the symbol. Host proof only: no device ran it.

App packages are built per target: `make deploy-bundle DEPLOY_PLATFORM=amlogic-alpine` exports
`TARGET_ABI=linux-musl` (and `linux-gnu` otherwise), each native app build stages that value in its
manifest `abi`, and the bundle refuses an archive with `entrypoint.runtime = "native"` whose `abi`
does not name the target libc. QuickJS and MicroPython archives are libc-independent, so they pass
without an `abi`; an archive declaring an unknown runtime is refused rather than guessed at. The
packager never rewrites the field. `BUNDLE_NATIVE=0` remains available for a deliberately
runtime-free bundle (static Go/script apps), but it is not the Alpine default. The ABI remains
`1.0`.

Boot hooks:

- Stock: `deploy/amlogic-stock/S99sysmon.sh`, installed over the already-expanded vendor
  supervisor script of the same name. `rcS` expands the list before the overlay pivots, so a new
  overlay-only name never runs; replacing an expanded name does. `S99btctl.sh` (btctl) is untouched;
  `disable` restores the stock `S99sysmon.sh` byte-for-byte (embedded copy verified against the
  extracted image) and `enable` restores the wrapper.
- Alpine: `deploy/amlogic-alpine/capsulad` is an OpenRC service. Install it as
  `/etc/init.d/capsulad` and `rc-update add capsulad default`. The deploy CLI's
  `-init-script /etc/init.d/capsulad` uses OpenRC's `start`/`stop`/`restart`.

Data directories: `/data/local/capsulad` on stock (overlay-backed), `/var/lib/capsulad` on Alpine.

Offline inspection identifies the platform from the rootfs (stock init scripts, `alpine-release`/
`openrc-run`, Rockchip overlay/boot markers) and reports it in `capsulad-cli platform inspect`
before applying any board-specific reading; an unrecognised tree reports `platform unknown`
instead of Rockchip statuses.
