# Security boundaries

The framework launches every app process, gives it an identity, and decides what that identity
may do. The manifest is metadata, not a permission system: nothing an app declares in it grants
access. Access follows from the connection's identity and the sandbox the daemon established.

## Sandbox modes

The daemon probes the kernel at startup and reports what it actually achieved. It never claims
stronger isolation than the probe proved. Mode selection is `auto` by default; `capsulad
-sandbox auto|namespaces|mountns|uid|none` overrides it. The probe tries user namespaces, then
mount namespaces; `uid` applies only when neither is available, so a privileged host with mount
namespaces always runs `mountns` even if `-sandbox uid` was requested. `none` is explicit.

| Mode | Isolation | Typical use |
|---|---|---|
| `namespaces` | User + mount namespace, pivoted read-only root, distinct app UID when the daemon is privileged, optional seccomp deny list. | Kernels with unprivileged user namespaces. |
| `mountns` | Mount namespace and pivoted read-only root, distinct app UID, no user namespace. Requires a privileged daemon. | Stock Capsula Mini kernels. |
| `uid` | Credential drop only; no filesystem isolation. | Fallback when the host has no usable namespace mode. |
| `none` | The app runs with the daemon's credentials and filesystem view. | Explicit `-sandbox none` or a kernel with no usable namespaces. |

**The stock device runs `mountns` only.** Its kernel supports a mount namespace but not
unprivileged user namespaces, and its seccomp filter support is unavailable, so the deny list is
not installed. The app still gets a pivoted read-only root and its own host UID, but it does not
get a user-namespace boundary and it is not syscall-filtered. The simulator reports whatever its
host kernel supports; a host result does not describe the device.

`DeviceInfo.subsystem_backends` and the daemon's status output report the probed mode and any
limitation. Query them instead of assuming.

## Filesystem and socket view

In `namespaces` and `mountns`, the app is pivoted into a private root:

| Path | Contents | Access |
|---|---|---|
| `/app` | The installed package (`CAPSULAD_APP_DIR`) | Read-only |
| `/assets` | Shared asset store (`CAPSULAD_ASSETS`), when configured | Read-only |
| `/run/capsulad/hal.sock` | The one framework gRPC socket | Connect, read/write |
| `/capsulad/lib` | Shared native runtime and script hosts, when installed | Read-only |
| `/lib`, `/lib64`, `/usr/lib`, `/usr/lib64` | System runtime libraries | Read-only |
| `/etc/ld.so.cache`, `/etc/ssl`, `/etc/ca-certificates`, `/etc/resolv.conf`, `/etc/hosts`, `/etc/nsswitch.conf`, `/etc/localtime`, `/usr/share/zoneinfo` | Loader, TLS, resolver and timezone data | Read-only |
| `/dev/null`, `/dev/zero`, `/dev/random`, `/dev/urandom` | Basic devices | Read-only bind |
| `/proc` | Process table | Only with a PID namespace; otherwise absent |

There is no writable `/tmp`, no home directory, no writable app data directory and no writable
package tree. `/app` and `/assets` are remounted read-only after binding, so an app cannot replace
a path inside its own sandbox. In `uid` and `none` modes there is no namespace: the app sees the
host filesystem, and `CAPSULAD_ASSETS`/`CAPSULAD_APP_DIR` are host paths. Treat that as a
development mode, not a security boundary.

The framework socket is the only endpoint the app needs. There is no per-app socket, no network
gRPC listener and no app-to-app socket. An app-to-app call travels over the caller's
`AppBusService.Call` stream and is relayed to the target's existing `Connect` stream. Applications
never listen; a listening socket is a contract violation even where the sandbox would permit it.

`-native-lib-dir`/`CAPSULAD_LIBRARY` point at the shared runtime. Apps must not bundle a copy of
`libcapsulad` or an interpreter: the launcher mounts and names the platform runtime read-only, and
the injected library path is the only supported way to load it.

## Identity and UIDs

- Every run in `mountns` or `uid` mode, and in `namespaces` when the daemon is privileged, gets a
  stable host UID derived from the app ID, in the `-sandbox-uid-base` range (default 20000, up to
  8000 slots) and distinct from other apps. An unprivileged daemon in `namespaces` mode cannot
  map UIDs, so apps share its host UID; `none` does not drop credentials at all.
- In `namespaces`, the app sees UID 0 inside its user namespace; on the host it is the assigned
  UID when the daemon is privileged.
- In `mountns`, the helper starts as host root, pivots and mounts, then drops to the app UID
  before `exec`.
- The framework knows the app from the supervised process: `AppHello{run_id}` is resolved
  against the daemon's run table and compared with the Unix-socket peer PID (`SO_PEERCRED`). A
  mismatched or unknown run ID is `PERMISSION_DENIED`.
- A compiled-in system app's run cannot be claimed by a supervised connection.
- Omitting identity does not make a connection more privileged. A connection without an app
  identity is a local administrator; a connection with an app identity is confined to app scope.

"Administrator" means a local CLI, the daemon, or the Web UI session. "App scope" means a
supervised app addressing its own state. A local caller is treated as an administrator only when
it carries no app identity: no `capsulad-run-id` metadata, a UID below the sandbox base, and a PID
that is not supervised. A sandbox UID or a supervised PID is confined even without a run ID. App
confinement is implemented by identity checks on each method, not by hiding the socket.

## What apps must never do

- Open, read or write physical hardware directly: LED controllers, display device nodes,
  `/dev/input`, audio devices, GPIO, the watchdog or any vendor character device. Publish output
  and take input through the framework services.
- Open a listening socket, or attempt to receive a framework or app-to-app connection. Use the
  injected `CAPSULAD_SOCKET` and the app bus.
- Read or update device settings, change master volume or microphone mute, force another app's
  capture, read diagnostics or retained logs, record or train wakewords, manage paired Bluetooth
  devices, or launch/stop apps.
- Address another app's configuration, runs, logs, assets-by-path assumptions or captures.
  `ApplicationConfigurationService` and run queries are scoped to the caller.
- Claim another run or app, reuse another run's `run_id`, spoof the `capsulad-run-id` metadata, or
  try to acquire an administrator identity by omitting or forging identity.
- Replay an activation ID, request another app's pre-roll, or capture after the visible/audible
  microphone indication is gone.
- Treat `provides`, a `[native_sdk]` ABI requirement or a `[github_attestation]` as permissions.
  They are hints and requirements.
- Use the custom settings UI to attempt network access, frames, forms, popups, host DOM access,
  app-bus calls or another app's settings. The frame is opaque-origin and receives no session or
  socket.
- Read or change Wi-Fi configuration. There is no app-facing Wi-Fi service.
- Store a secret in a `text` setting, a log line or a presentation when `text_masked` exists.
  Masked values are redacted everywhere except the owning app's bus event.

## Administrator-only surfaces

These methods reject a supervised-app identity with `PERMISSION_DENIED`. The full method index is
in [services.md](services.md).

| Service | Administrator-only methods |
|---|---|
| `SettingsService` | `GetSettings`, `UpdateSettings`, `WatchSettings` |
| `RoutingService` | `ForceRelease` |
| `AudioService` | `SetMasterVolume`, `SetMicrophoneMuted` (`GetAudioState` is readable) |
| `AssistantService` | wakeword sample recording/listing/deleting, classifier training |
| `ApplicationService` | `Launch`, `Stop` (`ListApplications`, `ListRuntimes`, `GetBinaryProvenance` are readable; run listings are app-scoped) |
| `SystemService` | `SetBluetoothDiscoverable`, `SetBluetoothIncomingConnections`, `ListPairedDevices`, `UnpairDevice`, `GetWebUiSecurity` |
| `DiagnosticsService` | `GetSnapshot`, `WatchLogs` |

`ApplicationConfigurationService.Describe` is readable because it exposes manifest metadata only.
`Get`, `Update` and `TriggerAction` are app-scoped: an app may address itself, an administrator may
address any app. `AppInterfaceService` and `AssetService.ListAssets` are readable by apps.

One deliberate exception to the own-app rule: the built-in clock and ring publish public standard
contracts (`capsulad.clock`, `capsulad.ring`) that any supervised app may call, including writes.
Those are device-level services, not another app's private configuration. See
[standard-contracts.md](standard-contracts.md).

## Provenance and trust

A native package may declare GitHub build provenance:

```toml
[github_attestation]
subject = "bin/myapp"
bundle = "provenance/build-bundle.jsonl"
repository = "example/myapp"
workflow = ".github/workflows/release.yml"
```

The manifest only **declares** evidence. The framework computes the status independently and
reports it through `ApplicationService.GetBinaryProvenance` / `capsulad-cli app provenance ID`:

| Status | Meaning |
|---|---|
| `PROVENANCE_NOT_PROVIDED` | No declaration in the manifest. |
| `PROVENANCE_VERIFIED` | The executable digest, DSSE signature, certificate chain, transparency-log inclusion, timestamp and repository/workflow identity all verified against the configured offline Sigstore trusted root. |
| `PROVENANCE_VERIFICATION_FAILED` | Evidence exists but does not verify: bad bundle, digest mismatch, wrong repository/workflow, or no trusted root. |
| `PROVENANCE_UNVERIFIED` | A declaration exists but no trusted root is configured, so the evidence cannot be evaluated. |
| `PROVENANCE_UNSUPPORTED_RUNTIME` | The package is not a native binary (script packages). |

Verification uses an offline trusted root (`capsulad -trusted-root FILE`); it never depends on
GitHub being reachable. Verified origin attests build integrity under that policy. It is **not**
a statement that the code or its runtime behavior is safe, and it never changes what the app may
do. Script packages cannot carry provenance evidence in this contract.

## Native SDK ABI requirement

A native app that loads the shared SDK runtime declares an ABI requirement, not a grant:

```toml
[native_sdk]
abi_major = 1
min_minor = 0
```

- The requirement is preflighted at install and again at launch against the installed runtime's
  metadata (`capsulad-native.json`). A wrong major or a too-new minor is a hard install failure
  naming the mismatch.
- The binding independently re-checks the library it loaded at startup.
- The ABI requirement describes the shared library contract only. It does not enumerate
  per-method permissions; those are decided by the daemon from the caller's identity.

`ApplicationService.ListRuntimes` reports which runtimes are available, with an implementation
version and a detail string. A missing script host is a clear launch failure, not a silent
fallback.

## Web UI and API keys

The Web UI is HTTPS with a self-signed server certificate, an Argon2id administrator password and
static opaque API keys. There is no client-certificate requirement. Static API keys are limited to
`GET /api/status` and the public certificate; they cannot list apps, change settings, upload assets
or use the WebSocket. Browser sessions authenticate the administrator and are used by the settings
UI's document route.

The local Unix socket is not protected by an API key. A process that can reach it as an
un-supervised local caller is an administrator; apps are confined by their verified identity, not
by possession of the socket. Do not expose the socket or the Web UI beyond the device's local
administrative boundary.

## Checklist

- Publish output and read input only through the framework; never touch device nodes.
- Connect only to `CAPSULAD_SOCKET`; never listen.
- Verify every setting you receive, including values that bypass the generated form.
- Keep secrets in `text_masked` settings and never log them.
- Handle `PERMISSION_DENIED` as final; do not probe other identities or run IDs.
- Release every presentation, capture, audio stream and conversation on every exit path.
- Do not rely on the simulator's sandbox mode as evidence about the device.
- Build native binaries for the target ABI and declare the ABI requirement that matches it.