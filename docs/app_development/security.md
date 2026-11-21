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
`AppBusService.Call` stream and is relayed to the target's existing `Connect` stream.

**An app must not create or bind its own listening socket.** A fixed inbound TCP or UDP port is
available only through a listener the daemon allocated: `listener_open` performs the bind under
policy and passes the listening descriptor over the run's control channel. An app's own
`udp_open` socket binds an ephemeral port and receives replies to its own sends; it cannot
expose a service. A listening socket created in the app process is a contract violation even
where the sandbox would permit it — which it does in `uid` and `none` modes, where there is no
network isolation at all. See [network.md](network.md#daemon-mediated-listeners).

`-native-lib-dir`/`CAPSULAD_LIBRARY` point at the shared runtime. Apps must not bundle a copy of
`libcapsulad` or an interpreter: the launcher mounts and names the platform runtime read-only, and
the injected library path is the only supported way to load it.

## Networking

Egress and ingress have different rules.

**Egress.** A native app reaches the network through the shared SDK: outbound HTTP/1.1, remote
gRPC, TCP streams with optional TLS, UDP and the mDNS calls. There is no per-destination
allowlist — the app's own code is the control — so never proxy another app's traffic and never
treat network access as a way around identity checks. TLS is always verified (TLS 1.2 or newer;
`system`, `webpki` or caller-supplied PEM roots); there is no insecure mode, no plaintext
downgrade and no hostname-verification override. Do not bundle your own TLS, HTTP or gRPC stack.

**Ingress.** An app must not create or bind a listening socket, join a multicast group, or
enable broadcast. There are exactly two inbound paths: a daemon-allocated listener (TCP or UDP,
with the bind decided by the daemon), and datagrams addressed to the ephemeral port of a
`udp_open` socket, which is a reply socket rather than an exposed service. A fixed, known
inbound port is only ever a listener.

| Rule | Value |
|---|---|
| Bind address | A local unicast address: wildcard, loopback or an interface address. Multicast and broadcast are refused. |
| Port | `0` (ephemeral) or `1024..65535`. 5353 and the configured Web UI port are refused. |
| UDP socket port | Ephemeral only. A non-zero `udp_open` bind port is refused; use a UDP listener for a fixed port. |
| Backlog | TCP only, 1..128 (default 64). |
| Per-run cap | 16 listeners. |
| Audit | Every allocation is logged with app, run, protocol, address and port. |
| Exposure | A non-loopback bind is reachable from the LAN, not just the device. |

`CAPSULAD_CONTROL_FD` is **reserved**: an inherited `AF_UNIX` `SOCK_SEQPACKET` descriptor that
carries listener allocation and nothing else. Never read, write, close, duplicate over or reuse
that number, and never treat it as a general-purpose channel. It is present whenever the
supervisor could create the channel; if creation failed, the run still starts without it and
every listener call returns `UNSUPPORTED` rather than falling back to a self-made socket. There
is no API that creates a listening socket in the app process; doing it directly violates this
contract even where the sandbox permits it, because `uid` and `none` modes have no network
isolation.

**mDNS.** The daemon's Avahi responder owns multicast DNS and port 5353. An app can neither
bind it nor run its own responder, so advertisement and browsing go only through
`DiscoveryService` — a raw UDP datagram the app sends to a multicast address is not a
publication and reaches no responder. Publications are scoped to the calling run and removed on
unpublish, run exit and daemon shutdown, so a crashed app leaves no advertisement. A publication
is visible to the whole local link: it exposes the service type, instance name, port, TXT
records and the device's mDNS host name. Never put a credential, token or internal identifier in
a TXT record, and never publish a port you do not actually serve. Browse results are untrusted
network input.

The networking entry points are appended to ABI 1.0 in this pre-release tree; declare
`[native_sdk] min_minor = 0` (or omit the requirement). A runtime that predates them has no
stream, UDP, listener or mDNS entry points, and a listener call without a usable control
channel returns `UNSUPPORTED` rather than creating anything.

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

## Routed calls and trust

`AppBusService.Call` is a **local request with an identified peer, not an authorization
decision**. The daemon decides what is reachable: a supervised app may address another app's
*declared* interfaces (`[[interfaces]]`), and only the daemon's internal token reaches private or
system targets. That check is per service, not per method: the framework does not decide who may
invoke a method.

The peer is identified, though. The daemon derives `IncomingAppCall.caller_run_id` from the
connection's peer credentials and the optional `capsulad-run-id` claim: a supervised run's id
when the claim resolves to that run and its live process matches the socket peer, `pid:<pid>` for
a local caller with no supervised run (the CLI, the Web UI), or empty when neither is available.
A claim that contradicts peer credentials is `PERMISSION_DENIED`. The Go SDK exposes the value as
`ServerCall.CallerRunID()`; the Rust and C callbacks carry the same field. Because it is derived
by the daemon and not by the caller, a handler may use it for authorization.

What that identity is not:

- It is not a per-method permission: a declared interface is reachable by every ready local app,
  and the framework never decides whether a particular app may invoke a particular method.
- It is not administrator authority or user consent. A CLI or Web UI call arrives as `pid:<pid>`
  — a local process, not a person, and not an administrator credential. `pid:<pid>` and empty
  cannot be authorized as an application.
- A value inside the request payload is never identity.

A handler that serves a privileged local interface should therefore identify its caller and
refuse: match `CallerRunID()` against daemon-served run data — for example
`AppInterfaceService.ListInterfaces`, whose `run_id`/`app_id` rows come from the supervisor, not
from the caller — and end the call with `PERMISSION_DENIED` otherwise.

Two consequences follow. Both are accepted rather than mitigated:

- Without such a check, any **ready** local app can invoke any declared interface of another app
  with no per-method permission. A handler that acts on the framework's behalf (opening a
  capture, changing what a provider shows) is then reachable by every other local app.
- A handler that performs **egress on a caller-supplied destination** without checking the caller
  is an unauthenticated arbitrary-egress primitive for any ready local app: any app that is ready
  could make it dial an address of its choosing. Any handler that fetches, connects or forwards
  to a caller-supplied destination must perform the check above before dialing; an interface that
  does so is not an egress primitive, and the repository README records which first-party apps
  are in that state.

The posture remains: **apps are trusted local code, and the app-bus plane is local only** (one
framework Unix socket, no network listener, no per-app sockets). Keep administrator or per-user
meaning out of app-bus handlers, authorize with the daemon-derived caller run id where an
interface is privileged, and constrain egress destinations inside the handler that performs them.

## What apps must never do

- Open, read or write physical hardware directly: LED controllers, display device nodes,
  `/dev/input`, audio devices, GPIO, the watchdog or any vendor character device. Publish output
  and take input through the framework services.
- Open a listening socket yourself, join a multicast group, or attempt to receive a framework or
  app-to-app connection. A fixed inbound TCP or UDP port is only a listener the daemon allocated;
  an app's own `udp_open` socket is ephemeral and carries replies, not a service. Use the
  injected `CAPSULAD_SOCKET` and the app bus for everything else.
- Read, write, close or reuse `CAPSULAD_CONTROL_FD`, or use it for anything but the SDK's
  listener allocation.
- Publish a credential or internal identifier in an mDNS TXT record, or advertise a service you
  do not serve.
- Read or update device settings, change master volume or microphone mute, force another app's
  capture, read diagnostics or retained logs, record or train wakewords, manage paired Bluetooth
  devices, or launch/stop apps. The installed AirPlay 2 receiver is the single
  framework-authorized exception for master volume; no manifest field can request it.
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

These methods reject a supervised-app identity with `PERMISSION_DENIED`, with the one
master-volume exception described below. The full method index is in [services.md](services.md).

| Service | Administrator-only methods |
|---|---|
| `SettingsService` | `GetSettings`, `UpdateSettings`, `WatchSettings` |
| `RoutingService` | `ForceRelease` |
| `AudioService` | `SetMasterVolume` (administrator or the installed AirPlay 2 receiver's authenticated READY run), `SetMicrophoneMuted` (`GetAudioState` is readable) |
| `AssistantService` | wakeword sample recording/listing/deleting, classifier training |
| `ApplicationService` | `Launch`, `Stop`, `InstallApplication`, `UninstallApplication` (`ListApplications`, `ListRuntimes`, `GetBinaryProvenance` are readable; run listings are app-scoped) |
| `SystemService` | `SetBluetoothDiscoverable`, `SetBluetoothIncomingConnections`, `ListPairedDevices`, `UnpairDevice`, `GetWebUiSecurity` |
| `DiagnosticsService` | `GetSnapshot`, `WatchLogs` |

The `SetMasterVolume` exception is fixed daemon policy, not a permission a package can request.
The installed AirPlay 2 receiver (`org.capsulad.airplay`) follows an authenticated active sender's
volume on the same persisted master the device keys and the Web UI control. The daemon accepts
it only from that app's live READY run, and only when the Unix-socket peer PID (`SO_PEERCRED`) is
the supervised process the daemon launched. A run-id claim, a request field or a manifest
declaration is not enough; an unknown, stale, starting, stopping or virtual run is refused. On a
host without peer credentials (the simulator) the app path fails closed. The trust that the
receiver is the first-party package comes from the administrator installing it; the exception is
compiled in and no other app ID can obtain it.

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
static opaque API keys. There is no client-certificate requirement. Static API keys carry a
scope. A `status` key is limited to `GET /api/status` and the public certificate; it cannot list
apps, change settings, upload assets or use the WebSocket. An `operator` key is full daemon
administration, including the MCP tools. The one non-MCP mutation route that accepts an API key
is `POST /api/apps/org.capsulad.pa/play`: an operator key may ask the PA Speaker to play one WAV,
and the route relays the app's `Play` call. It is not a generic AppBus, audio, capture or asset
proxy, and a `status` key is refused. An administrator browser session may also call that route,
with the same `X-Capsulad-CSRF` and same-origin proof as the other browser mutations. Browser
sessions authenticate the administrator and are used by the settings UI's document route.

The local Unix socket is not protected by an API key. A process that can reach it as an
un-supervised local caller is an administrator; apps are confined by their verified identity, not
by possession of the socket. Do not expose the socket or the Web UI beyond the device's local
administrative boundary.

## Checklist

- Publish output and read input only through the framework; never touch device nodes.
- Connect only to `CAPSULAD_SOCKET`; never create or bind a listening socket. Ask the daemon for
  a listener through `listener_open`, and treat `CAPSULAD_CONTROL_FD` as untouchable.
- Verify every setting you receive, including values that bypass the generated form.
- Keep secrets in `text_masked` settings and never log them; never put one in an mDNS TXT record.
- Keep TLS verification at the default; never disable it, override `server_name` to bypass a
  name mismatch, or ship a private CA when the system roots suffice.
- Bind listeners to loopback or a specific interface address, and validate every accepted
  connection and datagram as untrusted input.
- Unpublish mDNS services and close streams, UDP sockets and listeners on every exit path;
  process exit is the fallback, not the plan.
- Handle `PERMISSION_DENIED` as final; do not probe other identities or run IDs.
- Release every presentation, capture, audio stream and conversation on every exit path.
- Do not rely on the simulator's sandbox mode as evidence about the device.
- Build native binaries for the target ABI and declare the ABI requirement that matches it;
  declare `min_minor = 0` (or omit it) when you use streams, UDP, listeners or mDNS.