# capsulad-installer

`capsulad-installer` is the cross-platform (macOS, Linux, Windows) terminal
installer for a Capsula Mini. It talks to the device over ADB on the jailbroken
stock images and over SSH on Alpine Linux, discovers the firmware rather than
trusting a menu choice, backs up the vendor userspace before changing anything,
deploys the release bundle detached, configures Wi-Fi and the administrator
password, and installs the first-party apps you select.

The installer is its own Go module (`capsulad/installer`, Go 1.22). It never
changes the device-side build outputs in the root module.

## Prerequisites

| Need | When |
|---|---|
| Go 1.22 or newer | building the installer |
| `adb` (Android platform-tools) | jailbroken stock images (Rockchip RK3308 or Amlogic A113X) |
| SSH reachability, root password or private key | Alpine Linux (Amlogic A113X) |
| Network access to GitHub Releases, or an offline release tree | obtaining artifacts |
| The deployer's 8 MiB data-partition reserve plus any daemon growth | the deployment; the installer reports the exact floor before starting |

The Capsula Mini must be reachable on the same network as the computer running
the installer. On stock, the device asks you to allow USB/network debugging; the
installer waits for that prompt. On Alpine, the first SSH connection shows a
SHA-256 host-key fingerprint and asks you to trust it.

## Build and run

```sh
make installer
./build/installer/capsulad-installer
```

`make installer` builds for the host. A direct build works too:

```sh
cd installer
go build -o ../build/installer/capsulad-installer .
```

### Flags

| Flag | Meaning |
|---|---|
| `-artifacts DIR` | offline release tree, either with `release.json` or discoverable |
| `-release-url URL` | override the release download base |
| `-release-tag TAG` | use `/releases/download/TAG/` on GitHub |
| `-adb PATH` | adb binary (default: resolves `adb`) |
| `-ssh-key PATH` | prefills the Alpine private-key field |
| `-config-dir DIR` | state root (default `<user config>/capsulad-installer`) |
| `-cache-dir DIR` | download cache (default `<user cache>/capsulad-installer`) |
| `-version` | print the installer version |

`-release-url` and `-release-tag` are mutually exclusive. Online mode uses
GitHub **Releases**, never GitHub **Packages**: arbitrary files cannot be
downloaded from Packages, so the manifest and artifacts live on a release.

## What the interface does

1. **Device** — Capsula Mini is offered; the original Capsula is disabled with
   the reason that no maintained firmware image exists.
2. **Firmware** — *Jailbroken stock (capsula-jb)* over ADB, or *Alpine Linux*
   over SSH. The board (Rockchip or Amlogic) is discovered, not chosen.
3. **Address** — ADB connects to `<address>:5555`; SSH defaults to `root` and
   accepts a hidden password, a private key, or both.
4. **Identity** — the installer reads `/proc/device-tree/serial-number`, then
   `getprop ro.serialno`, then `devinfo`, plus the hostname, and shows the result
   on the confirmation screen. After the deployment's reconnect it reads the
   identity again and aborts before sending Wi-Fi, app or token credentials if a
   serial that was present has changed. With no stable serial the confirmation
   carries a warning and requires an explicit acknowledgement (`x`); ADB itself
   cannot authenticate the peer.
5. **Discovery and backup** — after connecting, the installer reads
   `/etc/os-release`, `/proc/device-tree/compatible` and `uname -m`, maps them to
   `rockchip-stock`, `amlogic-stock` or `amlogic-alpine`, and refuses a
   contradiction with the firmware you chose.
   * Stock: the vendor userspace is streamed to the host as tar before any
     change and gzipped on the host, so the stored backup is `.tar.gz`. The
     archive holds `/data` (excluding `/data/lost+found`, `/data/capsdemo`, and
     the active platform data directory) plus `/etc/init.d/S97voice-agent.sh`,
     `/etc/init.d/S99sysmon.sh`, `/etc/init.d/S99btctl.sh`,
     `/etc/wpa_supplicant.conf` and `/etc/network` when present. It is written to
     `<config-dir>/backups/<host>-<UTC timestamp>.tar.gz`, validated, hashed and
     described by a `.manifest.json` beside it. The remote command emits a
     success marker on stderr only after `tar` exits; the installer requires that
     marker as well as gzip/tar validation.
   * Alpine: no vendor backup exists. The installer checks `df -Pk` on the data
     filesystem against the bundle, the apps the release carries and a 64 MiB
     reserve.
   * Stock free-space preflight: the installer reports the deployer's own floor
     (8 MiB + any positive staged-minus-installed daemon growth) with the
     available bytes. When short it names installed apps that are not running,
     but never stops or moves them. The deploy tool remains authoritative.
6. **Confirmation** — the administrator password (generated with `crypto/rand`,
   24 characters, or your own, or the existing one when `auth status` says it is
   set), the one-time jingle (default **off**), and an explicit acknowledgement
   are shown before anything is deployed.
7. **Deployment** — the bundle and the device CLI are pushed to
   `/tmp/capsulad-installer/`. The bundle must carry `capsulad-cli`; a
   `BUNDLE_CLI=0` bundle is refused before the device is touched. The
   administrator password and certificate are then created or changed **before**
   the launch, because a fresh daemon only opens its HTTPS listener when
   credentials exist at process start. Any previous `deploy.log` is removed so a
   retry cannot read the last attempt's result. The deploy script sets
   `umask 022`, runs
   `capsulad-cli -data-dir <dir> deploy <bundle> -platform <platform> -staging /run -timeout 180s`
   through `start-stop-daemon -S -b -x` (falling back to `setsid`) because the
   stock daemon owns Wi-Fi, and writes `exit=N` to `deploy.log`. The installer
   reconnects with bounded retries and reads that line. Before deploying it
   writes `<data-dir>/startup-sound.played` when the jingle is off (creating the
   data directory first), or removes the marker when the jingle is on; the
   daemon's one-shot cue follows that choice on its first boot.
8. **Wi-Fi** — `-json wifi status`; a connected link is skipped. Otherwise the
   scan is deduplicated by SSID (strongest advertisement wins, the connected one
   is never hidden). Every connect passes an explicit `-security` value: `open`
   for an open network, otherwise the scanned mode; a manual secured entry with
   no chosen mode relies on the backend's WPA2 default. A passphrase travels on
   stdin only for a secured network, and the installer waits for
   `state=connected` with an IP. Scan-less firmware gets a manual SSID entry.
   Setup-AP details are surfaced when active.
9. **Administrator password** — shown on this screen, with OSC 52 copy, a
   "change now" key that runs `auth set-password` through stdin, and a required
   "stored" confirmation. Copy it before continuing. Recovery is
   `capsulad-cli auth set-password` in the device shell.
10. **Apps** — exactly ten first-party apps are offered: AirPlay 2, Cadence,
   Dumb Assistant, Home Assistant, Marusia, OpenCapsula, PA Speaker, Internet
   Radio, Simon Says, Soundboard. Hardware Test is deliberately absent. Each
   selected archive is pushed and installed in order, and its ID is verified in
   `app list`. A missing artifact is reported as a failed app, never silently
   omitted.
11. **Marusia** — on stock, an existing `/data/voice-agent/session.json` with
    `session` and `secret` is migrated into app settings; the vendor file is
    left in place. Otherwise (and on all Alpine installs) a hidden, optional VK
    access token is stored through stdin.
12. **Summary** — Web UI URL, certificate SHA-256 fingerprint, certificate
    downloads (`/api/cert.pem`, `/api/ca.pem`) and verification
    (`capsulad-cli cert status`), installed apps and failures, Wi-Fi state, backup
    path and the exact rollback command. It states that nothing was deleted.

Every step keeps a running log, and back/retry is available where returning is
safe. Ctrl-C during a deployment asks before aborting.

## Cleanup is retire-only

Nothing is deleted by the installer. The boot hook takes over the stock
assistant chain by retiring `S97voice-agent.sh` / `S99sysmon.sh` (pristine copies
are preserved by the hook) and the vendor files stay on disk. `S99btctl.sh
disable` (Rockchip), `S99sysmon.sh disable` (Amlogic stock) or stopping and
disabling the OpenRC service (Alpine) returns boot ownership to the image.

## Release layout and manifest

```text
release.json
rockchip-stock/deploy-bundle.tar.gz
rockchip-stock/apps/<id>-<version>.tar.gz
amlogic-stock/deploy-bundle.tar.gz
amlogic-stock/apps/<id>-<version>.tar.gz
amlogic-alpine/deploy-bundle.tar.gz
amlogic-alpine/apps/<id>-<version>.tar.gz
```

Only the selected platform is downloaded. `release.json` schema 1:

```json
{
  "schema": 1,
  "version": "0.4.0",
  "firmwares": {
    "rockchip-stock": {
      "label": "Jailbroken stock (Rockchip RK3308)",
      "bundle": {
        "path": "rockchip-stock/deploy-bundle.tar.gz",
        "size": 40123456,
        "sha256": "<64 lowercase hex characters>"
      },
      "apps": [
        {
          "id": "org.capsulad.simon",
          "version": "0.1.0",
          "path": "rockchip-stock/apps/org.capsulad.simon-0.1.0.tar.gz",
          "size": 123456,
          "sha256": "<64 lowercase hex characters>"
        }
      ]
    }
  }
}
```

Every `path` is relative to the release root, uses forward slashes and may not
escape it. `firmwares` may carry any subset of the three platform keys. The
installer refuses an unknown schema, an empty version, duplicate app IDs,
malformed hashes and unsafe paths before fetching anything.

### Offline trees

With `-artifacts DIR`, a `release.json` at the root is used and every artifact is
re-hashed before use. If `release.json` is absent the installer discovers the
tree: each known platform directory must hold `deploy-bundle.tar.gz` (or exactly
one other tarball) and may hold `apps/<id>-<version>.tar.gz`. Sizes and SHA-256
digests are computed locally; the resulting manifest is the same shape.

### Staging a release

`cmd/stage-release` is a build-host tool. It copies the bundle and apps, hashes
them, and merges one platform into `release.json` without dropping other
platforms:

```sh
cd installer
go run ./cmd/stage-release \
  -out ../build/release \
  -version 0.4.0 \
  -platform rockchip-stock \
  -bundle ../build/deploy-bundle.tar.gz \
  -apps ../build/apps/org.capsulad.simon-0.1.0.tar.gz,../build/apps/org.capsulad.pa-0.1.0.tar.gz
```

Build the bundle with the device CLI included (`make deploy-bundle BUNDLE_CLI=1`,
the default) when staging a release the installer can deploy. The bundled CLI
must support the secret-safe `settings app APPID KEY@-` form (and
`-password-stdin`); build the bundle from the matching CLI revision. `stage-release`
copies files; it never compiles or packages an app. `-apps` may be omitted for a
bundle-only platform; the manifest then carries an empty app list.

## Security model

- **TLS with normal verification.** Manifest and artifact downloads use HTTPS and
  the host's trust store. Loopback HTTP is accepted only for a local test server.
  A custom `-release-url` must be HTTPS unless it is loopback.
- **Integrity, not provenance.** SHA-256 digests come from the same release
  channel as the files. They detect corruption, truncation and a swapped cache
  file; they do not make a compromised upstream trustworthy. Releases are not
  signed yet.
- **Size caps and atomic writes.** Bundle 512 MiB, each app 256 MiB. Downloads
  are streamed to a temporary file, hashed, checked against the declared size,
  fsynced and renamed. Cached files are re-hashed before reuse.
- **Downloaded host files are never executed.** Bundles are read with Go's tar
  and gzip readers. The device CLI is extracted as data and pushed; only the
  device runs it.
- **Hardened staging.** `/tmp/capsulad-installer` is created or verified as a
  real directory owned by the current (root) user, mode 0700, with symlinks and
  foreign owners refused before the bundle, CLI, script or log is written.
- **SSH trust on first use.** The first key is shown as a SHA-256 fingerprint and
  pinned in `<config-dir>/known_hosts.json` (mode 0600). Any later mismatch
  fails the connection; the pin is never overwritten.
- **Secrets stay off host argv and logs.** Admin passwords and Wi-Fi
  passphrases use `-password-stdin`; Marusia session values and VK tokens use
  `settings app APPID KEY@-`. The device CLI reads the value on stdin, so only
  the key name is in argv. The host argv, installer logs and error messages never
  contain the value. The generated password is shown on this screen; copy it
  before continuing.
- **Web UI trust anchor.** After `auth init`, `/healthz` is polled with the
  device's `<data-dir>/security/server.crt` as the only root and normal hostname
  verification. `InsecureSkipVerify` is never set.
- **ADB peer identity is best-effort.** ADB has no cryptographic device
  authentication. The installer records the serial (device-tree, `ro.serialno`,
  `devinfo`) plus hostname before deploying, shows it, and re-reads it after the
  restart; a changed serial aborts before further secrets are sent. With no
  serial it warns and requires an explicit acknowledgement. A LAN impostor that
  answers at the same address can still be accepted when no serial exists or
  before the first check; use SSH or a trusted network for that guarantee.

## Rollback

| Platform | Command |
|---|---|
| Rockchip stock | `/etc/init.d/S99btctl.sh disable` |
| Amlogic stock | `/etc/init.d/S99sysmon.sh disable` |
| Amlogic Alpine | `rc-service capsulad stop && rc-update del capsulad` |

On stock, `disable` stops the daemon, restores the vendor init scripts, starts
the vendor assistant chain and moves the hook aside. The pre-change backup is a
host-side copy for inspection; rollback does not need it and nothing on the
device is deleted by the installer.

## Known limits

- The original **Capsula is disabled**: no maintained firmware image exists for
  this installer.
- Stock installation requires **ADB**; there is no stock SSH path.
- A deployment interruption is recovered by reconnecting and reading
  `/tmp/capsulad-installer/deploy.log`. The detached script may still be running
  after the interface is gone. Start the stack with the platform's init script,
  not by running the daemon by hand.
- `app list` is plain text with the app ID as the first column; the installer
  parses that column rather than expecting JSON.
- `auth status` is parsed from the real `admin password set: true|false` line;
  unrecognised output is treated as no existing password, so a fresh `auth init`
  runs.
- Keeping an existing password keeps its existing server certificate. If the
  device address changed since that certificate was created, the pinned health
  check can fail until the certificate is regenerated with `auth init`.
- Wi-Fi scan support is firmware-dependent; the manual SSID path is used when
  the device reports `scan_supported: false`.
- Encrypted SSH private keys are not supported; decrypt one for the installer or
  use the password path.
- The stock backup uses `adb exec-out` when the device adbd supports it. Some
  Rockchip stock images reject both `exec-out` and non-pty shells, so the
  installer falls back to a raw pty (`stty raw -echo`) and requires the remote
  success marker plus gzip/tar validation. A dropped link is never accepted as a
  complete backup.
- The stock preflight uses the bundle's declared daemon size and the installed
  daemon size. The deployer re-checks at deploy time and remains authoritative.
- Nothing is moved or deleted to make space; when the check fails, the user
  decides whether to abort.

## Troubleshooting

| Symptom | Action |
|---|---|
| `adb` not found | install Android platform-tools or pass `-adb` |
| `unauthorized` | accept the on-device debugging prompt; the installer waits |
| SSH host key mismatch | re-flash or remove only that host's pin from `known_hosts.json`; never accept a changed key blindly |
| Device disappears after deploy starts | normal; wait, then re-enter the same IP, or join the setup AP (portal `http://192.168.4.1`) |
| Deploy refused for free space | the deploy log in the error screen shows the deployer's numbers; nothing was deleted |
| `BUNDLE_CLI=1` error | rebuild the bundle with the device CLI included |
| Web UI TLS error | download `/api/cert.pem` and install it as trusted, or verify it with `capsulad-cli cert status` |

## Tests

```sh
cd installer
go test ./...
go test -run TestFakeFlow ./internal/install
go vet ./...
```

`TestFakeFlow*` covers the full stock happy path (detection, backup, detached
deploy, auth, pinned health check, Wi-Fi, ordered app install, Marusia
migration) and the failure paths (deploy exit ≠ 0, auth failure, missing
artifact, unconfirmed deploy, token handling). Tests use fake devices and
in-process TLS servers; no real device or external network is needed.

On macOS the test suite needs cgo enabled (the default). Go 1.22's internal
linker omits `LC_UUID` from the test binary on current macOS, so
`internal/linkext` imports a cgo package and forces the external linker.
