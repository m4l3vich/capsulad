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
- **Dynamic presentations and captures** with exact first-content readiness, last-eligible-capture
  wins, baseline fallback, out-of-order release, force release and process-exit cleanup.
- **Touch partial/full modes** with mutually exclusive ownership, framework volume/microphone
  controls, raw/gesture delivery and clean cancellation on ownership changes.
- **Audio mixer/fan-out** with software mixing, conversation ducking, bounded capture queues,
  discontinuities, pre-roll for activations and the visible-indication microphone watchdog.
- **Four-page Web UI**: Overview (Bluetooth state and discoverability, now-playing metadata, volume,
  installed applications), Settings (default display/ring app with constant or light-sensor
  brightness, assistant and touch behaviour, microphone and muted indication, device timezone),
  Access (admin password, static API keys, public certificate download), Diagnostics (hardware,
  firmware, software, runs, resources, logs).
- **Bluetooth facade** through `hciconfig`: adapter presence, power and discoverability, with the
  vendor stack still owning pairing and profiles.
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
- **Device timezone** set in the Web UI, inherited by apps as `TZ`, with an optional per-app
  override (the clock app exposes one); changing it restarts boot applications to apply it.
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
- **Rockchip vendor LED ABI is recovered and partly validated.** The device tree describes a
  36-channel WS2812B chain (`/dev/topleds`, 12 RGB ring pixels) and an IS31FL3236
  (`/dev/led_multi_ctrl`, 30 monochrome display cells); the recovered `GET_COUNT` ioctl returns
  exactly 36 and 30 on a live unit, which confirms the ioctl family. The `MULTI_SET` frame path,
  face parity and segment bit order still require visual validation on a unit. Standard Linux
  `sysfs` LED class devices and evdev remain fallbacks.
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
make all apps
mkdir -p build/demo
./build/bin/capsula -data-dir build/demo app install build/apps/org.capsula.clock
./build/bin/capsula -data-dir build/demo app install build/apps/org.capsula.assistant
./build/bin/capsula -data-dir build/demo app install build/apps/org.example.ring-controller
./build/bin/capsula -data-dir build/demo auth init -hostname localhost -ip 127.0.0.1
./build/bin/capsulad -platform simulator -data-dir build/demo -dev-admin build/demo/run/dev.sock
```

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
capsula -data-dir build/demo settings app org.capsula.clock timezone=Asia/Shanghai
```

The rest of the CLI uses the framework Unix socket as before:

```sh
./build/bin/capsula -data-dir build/demo status
./build/bin/capsula -data-dir build/demo dev wakeword            # simulator activation
./build/bin/capsula -data-dir build/demo dev state               # virtual display/ring frames
./build/bin/capsula -data-dir build/demo app provenance org.capsula.clock
```

On macOS this project builds with `-linkmode=external` because the local Go linker currently emits
Mach-O binaries that current dyld rejects (missing `LC_UUID`). The Makefile handles that; Linux
targets use the default internal linker.

Production paths are supplied explicitly, for example:

```sh
capsulad -platform rockchip-stock \
  -data-dir /var/lib/capsulad -socket /run/capsulad/hal.sock \
  -apps-dir /var/lib/capsulad/apps -assets-dir /var/lib/capsulad/assets \
  -trusted-root /etc/capsulad/trusted_root.json \
  -web-addr 0.0.0.0:8443 -sandbox auto
```

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
| `internal/backend/simulator.go` | complete virtual backend |
| `internal/backend/rockchip` | RK3308 stock-firmware backend (vendor LED ABI recovered) |
| `internal/sandbox` | Linux read-only namespace launcher + seccomp |
| `internal/websecurity` | self-signed TLS, Argon2id password, API keys, sessions |
| `internal/provenance` | Sigstore/GitHub attestation verification |
| `internal/webui` | password/API-key HTML interface |
| `sdk/go/capsula` | Go SDK (bus, routed calls, presentations, audio, captures) |
| `apps/{clock,assistant,ring}` | sample supervised applications |
| `proto/` | draft v0.5 schemas plus the example ring contract (copies add only `go_package` options for codegen) |
| `docs/` | hardware findings, implementation status, security notes |

## Verification

```sh
make test
```

Tests cover routing/capture semantics, preset validation, touch delivery/system suppression and
cancellation, settings CAS and secret redaction, self-signed TLS, password sessions, API-key
accept/revoke and CSRF, genuine Sigstore fixture verification, and an end-to-end simulator run (supervision, singleton invocation, routed
unary/streaming calls, touch partial/full capture, crash cleanup, wakeword capture/release, mic
watchdog and masked settings). The Web UI is additionally tested through login/session, CSRF and
static API-key flows.

## Status

See `docs/STATUS.md` for the implementation matrix and the remaining hardware gates, and
`docs/HARDWARE.md` for the evidence extracted from the supplied Rockchip firmware dump.
