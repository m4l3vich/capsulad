# Application packaging — draft 0.5

Each installed app is supervised by `capsulad`, with its own immutable `/app` package and read-only `/assets` catalog. It connects to `unix:///run/capsulad/hal.sock` (script runners provide the bridge) and has no filesystem writes or per-app IPC listeners. Network client and TCP/UDP server sockets are allowed, but the Web UI mTLS configuration does **not** protect arbitrary app-owned network services.

## Package entrypoints

Use an **entrypoint table** in the manifest; no shell interpolation.

### Native

```toml
id = "org.example.weather"
name = "Weather"
version = "0.1.0"
single_instance = true
background = true
provides = ["segment_display_provider"]

[entrypoint]
runtime = "native"
path = "bin/weather"
architecture = "aarch64"
abi = "linux-musl"
sdk = "0.5"

[github_attestation]
subject = "bin/weather"
bundle = "provenance/build-bundle.jsonl"
repository = "example/weather"
workflow = ".github/workflows/release.yml"
```

### JavaScript (QuickJS)

```toml
id = "org.example.clock-js"
name = "Clock JS"
version = "0.1.0"
background = true

[entrypoint]
runtime = "quickjs"
path = "main.js"
sdk = "0.5"
```

### Python (MicroPython)

```toml
id = "org.example.clock-py"
name = "Clock Python"
version = "0.1.0"
background = true

[entrypoint]
runtime = "micropython"
path = "main.py"
sdk = "0.5"
```

The native framework runner loads scripts and supplies host bindings for lifecycle, HAL access, settings, logs and routed inter-app RPC. Scripts do **not** have to implement HTTP/2 or native gRPC. They still run in separate sandboxed OS processes. Bytecode/module writes and temp-file creation are disallowed. Provide tested interpreter version/module compatibility rather than claiming full Node.js or CPython support.

## Provenance semantics

`github_attestation` is optional and meaningful initially only for native binaries. The framework must verify the exact packaged binary digest against a signed attestation and trusted signing identity, not trust manifest claims. The UI displays the *computed* status, verified identity when available, and any verification errors. **Verified origin is not an assertion of software safety.**

## App contract publication

An app may include `.proto`, a compiled descriptor set, and Markdown method documentation. These are registered with the framework, which brokers app-to-app calls through the persistent `AppBusService.Connect` stream, never through an app-owned IPC socket.
