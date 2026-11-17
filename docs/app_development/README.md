# capsulad application development

This manual is the complete contract for writing an application that runs under `capsulad`.
It covers what the platform provides, how an app is packaged and installed, how it is launched
and identified, how it talks to the framework, and what it may and may not do. Everything you
need is in this directory; no other documentation is required.

## What the platform is

`capsulad` is the on-device supervisor and framework for **Capsula Mini**. It owns the
hardware and every application process:

- It launches and supervises every application, injects its identity, and restarts it
  according to its manifest.
- It owns the segment display, the LED ring, the touch panel and the audio devices. Apps never
  write hardware directly; they publish presentations, acquire routing captures, stream audio
  and receive input through framework services.
- It exposes one local Unix socket. Every gRPC method an app can call lives on that socket,
  including app-to-app calls, which are relayed through the framework. There is no network
  gRPC listener and no per-app socket.
- An app never creates or binds its own listening socket. When an app needs a fixed inbound
  TCP or UDP port, the daemon allocates and binds the listener under policy and passes the
  listening descriptor over the run's control channel; the app serves it and never owns the
  bind. `udp_open` sockets bind an ephemeral port only, so they carry replies, not services.
- It persists app settings declared in the app manifest, renders them in the Web UI, and
  delivers changes to the owning app.
- It discovers app-published interfaces and relays calls to them, so one app can serve another
  without listening on anything.
- Outbound TCP/UDP/TLS goes through the shared native SDK, and mDNS publication and browsing go
  through the daemon's Avahi responder. An app never runs its own TLS server, mDNS responder or
  listening socket.

An app is therefore a normal process — native binary, JavaScript or Python — that connects to
the framework socket, declares itself ready, serves the events it is given, and exits cleanly
when told. The platform handles installation, sandboxing, identity, routing and lifecycle.

## Supported targets

| Target | Platform value | Notes |
|---|---|---|
| Capsula Mini, Rockchip stock firmware | `rockchip-stock` | Primary device. aarch64, glibc 2.33. Kernel supports mount-namespace sandboxing only. |
| Host simulator | `simulator` | Full virtual segment display, LED ring, five-key panel, microphone and speaker. Runs on a development host, accepts every package a device accepts. |

**Amlogic is not supported in this build.** There is no Amlogic backend:
`-platform amlogic-stock` and `-platform amlogic-alpine` are rejected as unknown platforms
(`unknown platform "…" (simulator, rockchip-stock)`), not accepted and then refused. The
historical `CAPSULA_MINI_AMLOGIC_*` values remain reserved wire enums with no backend. Do
not test, document, plan a parity milestone or claim Amlogic support; an app that must run
on Amlogic has to wait for a backend and its own board profile.

The only supported hardware profile is the Rockchip Capsula Mini rev 4 stock configuration,
held as inert data in `internal/backend/profile`; board paths and quirks live in
`internal/backend/rockchip` and `deploy/rockchip-stock`. An app sees the same framework
contract on the simulator, whose profile names no device nodes.

A device advertises only what its backend implements: query `DeviceService.ListResources`
before assuming a display, ring, panel or audio path exists. The simulator reports
`targetPlatform` as unspecified.

Resource identifiers are stable across targets:

| Resource | Kind | Routing |
|---|---|---|
| `display.segment.front` | segment display | default app plus captures |
| `light.ring.main` | LED ring | default app plus captures |
| `touch.panel.main` | touch panel | default app plus captures |
| `audio.input.microphone` | microphone | shared fan-out streams |
| `audio.output.speaker` | speaker | shared mixed streams |
| `role.voice_assistant` | assistant role | default app plus captures |
| sensor IDs | sensors | read-only |

## Prerequisites

| Item | Needed for |
|---|---|
| `capsulad` and `capsulad-cli` | Running a simulator or a device; `capsulad-cli` packages, installs and controls apps. |
| The shared native runtime (`libcapsulad` plus its metadata file) in the platform `lib/` directory | Every runtime: native apps load it, script hosts embed it. |
| Go 1.22 or newer and a C compiler | Native apps written in Go. The Go binding uses cgo. |
| A C compiler, or Rust 1.75 or newer with Cargo | Native apps written in C/C++ or Rust. |
| The QuickJS host installed in the platform `lib/` directory | `runtime = "quickjs"` apps. |
| The MicroPython host installed in the platform `lib/` directory | `runtime = "micropython"` apps. |
| `protoc` (optional) | Producing a `FileDescriptorSet` for interfaces you publish. |

The native runtime is a platform component, not something an app ships. An app declares that
it needs it; the framework preflights that requirement and injects the library path.

Native binaries must be built for the device's ABI: `aarch64`, `linux-gnu`, glibc 2.33 or
older. Anything newer fails at startup on the device.

## Ten minutes from an empty directory

The example below is a complete native Go app that shows a value on the segment display.
Simulator and device use the same steps; only the transport to the machine differs.

```sh
mkdir -p hello-segment/bin
cd hello-segment
```

`manifest.toml`:

```toml
id = "com.example.hello"
name = "Hello Segment"
version = "0.1.0"
description = "Shows digits on the segment display."
single_instance = true

[entrypoint]
runtime = "native"
path = "bin/hello"
architecture = "aarch64"
abi = "linux-gnu"

[native_sdk]
abi_major = 1
min_minor = 0
```

`main.go`:

```go
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"

	"capsulad/sdk/capsulad"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	client, err := capsulad.Connect(ctx, capsulad.FromEnv())
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Close()

	ready := make(chan struct{})
	client.SetHandlers(capsulad.Handlers{
		Ready:        func() { close(ready) },
		Disconnected: func(err error) { log.Printf("disconnected: %v", err) },
	})

	// Start blocks until the bus ends, so output runs beside it.
	go func() {
		select {
		case <-ready:
		case <-ctx.Done():
			return
		}
		pres, err := client.OpenPresentation(ctx, "display.segment.front")
		if err != nil {
			log.Printf("presentation: %v", err)
			return
		}
		defer pres.Close()
		for i := 0; i < 10; i++ {
			if err := pres.Segment([]uint16{1 << uint(i%7)}, false); err != nil {
				log.Printf("frame: %v", err)
				return
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()

	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("start: %v", err)
	}
}
```

Build and package **on the build host**, then install:

```sh
go mod init hello-segment
go mod edit -require=capsulad/sdk@v0.0.0
go mod edit -replace=capsulad/sdk=/path/to/platform/capsulad-sdk
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  go build -trimpath -o bin/hello .

capsulad-cli app package . -out hello-segment.tar.gz
capsulad-cli -data-dir /data/capsdemo app install hello-segment.tar.gz
capsulad-cli -data-dir /data/capsdemo app start com.example.hello
capsulad-cli -data-dir /data/capsdemo app runs com.example.hello
capsulad-cli -data-dir /data/capsdemo logs -app com.example.hello -n 50
capsulad-cli -data-dir /data/capsdemo app stop <run-id>
capsulad-cli -data-dir /data/capsdemo app remove com.example.hello
```

On a development host the same commands work against a simulator daemon; build the binary for
the host, install the package, then start the daemon:

```sh
mkdir -p run/demo
capsulad-cli -data-dir run/demo auth init -hostname localhost -ip 127.0.0.1
CGO_ENABLED=1 go build -trimpath -o bin/hello .
capsulad-cli app package . -out hello-segment.tar.gz
capsulad-cli -data-dir run/demo app install hello-segment.tar.gz
capsulad -platform simulator -data-dir run/demo -dev-admin run/demo/run/dev.sock &
capsulad-cli -data-dir run/demo app start com.example.hello
capsulad-cli -data-dir run/demo app runs com.example.hello
capsulad-cli -data-dir run/demo logs -app com.example.hello -n 50
```

Two things to remember:

- **Packaging happens on the build host, never on the device.** The device receives `.tar.gz`
  artifacts and installs them, from the Web UI **Apps** page or with `capsulad-cli app install`.
- **Installing while the daemon runs is live; a stopped daemon activates the package at its next
  start.** With capsulad listening, an accepted install, update or removal is visible to
  `ListApplications` at once and `app install` says so; with it stopped, the package lands in
  `<data-dir>/apps` and the CLI prints a restart reminder. Installing, removing or replacing a
  package while its app is running is always refused; stop the run first.

The full walkthrough, including manifest fields, deployment bundles and rollback, is in
[packaging-deployment.md](packaging-deployment.md). A complete QuickJS app is in
[sdk.md](sdk.md#complete-quickjs-app).

## The manual

| Document | Contents |
|---|---|
| [manifest.md](manifest.md) | Every manifest field, type, default, constraint and validation rule; runtimes; settings, actions, arguments, interfaces; complete examples. |
| [lifecycle.md](lifecycle.md) | Launch, identity, environment variables, run states, restart and backoff, settings and action callbacks, shutdown, failures versus stops. |
| [sdk.md](sdk.md) | The native SDK model, Go/Rust/C usage, handles and threading, status model, JSON versus protobuf, deadlines, outbound HTTP(S)/TLS, remote gRPC (TLS/h2c), encoding/hash/random/URL primitives, script hosts. |
| [network.md](network.md) | Outbound TCP streams with verified TLS, UDP datagrams, daemon-mediated listeners (`CAPSULAD_CONTROL_FD`), and mDNS publish/browse through the daemon's Avahi responder: options, limits, errors, cleanup and examples. |
| [app-bus.md](app-bus.md) | `Connect`, routed `Call`, notifications, descriptor publication, encoding rules, one stream per app, error codes. |
| [services.md](services.md) | Every framework service and method with request/response fields. |
| [output-input.md](output-input.md) | Presentation messages, readiness, captures, routing priority, touch modes, keys, gestures, dismissal. |
| [audio.md](audio.md) | Endpoints, formats, capture and playback streams, pre-roll, ducking, indication, buffering, drain. |
| [settings-actions.md](settings-actions.md) | Typed settings, compare-and-swap, validation, redaction, actions, custom settings UI protocol. |
| [assets.md](assets.md) | The shared read-only asset store and how apps consume it. |
| [standard-contracts.md](standard-contracts.md) | `capsulad.contracts.clock.v1.ClockService`, `capsulad.contracts.ring.v1.RingService`, `capsulad.contracts.media.v1.MediaPlayerService`. |
| [packaging-deployment.md](packaging-deployment.md) | Artifact format, packaging, install/update/remove, deployment bundles, simulator, versioning, rollback. |
| [security.md](security.md) | Sandbox modes, filesystem view, sockets, what an app must never attempt, provenance, trust boundaries. |
| [troubleshooting.md](troubleshooting.md) | Symptom, cause and fix for the failures an app author actually hits. |

## Naming and conventions

- App IDs are stable, lowercase and dot-separated, for example `org.example.weather`
  (`^[a-z0-9][a-z0-9._-]{0,127}$`). The ID identifies the package, its settings file, its runs
  and its bus identity.
- A **run** is one supervised process; a **run ID** identifies it and is required on the app
  bus. A **capture** is a temporary routing selection; a **presentation** is app output on one
  resource.
- Manifest hints such as `provides` are UI vocabulary, not permissions. Nothing in a manifest
  grants access; access is decided by the framework from the connection's identity.
- The device clock is monotonic since boot for every `*_monotonic_ns` field, unless a field
  says otherwise (`*_unix_ms`, `*_rfc3339`).