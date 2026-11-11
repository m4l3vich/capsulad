# Changes from draft 0.4 to draft 0.5

- **HTTPS Web UI:** mTLS required by default, browser client certificates replace API keys. Local bootstrap and certificate lifecycle required; the Unix-socket gRPC HAL remains local only. Read-only TLS diagnostics in `SystemService`; no cert issuance RPC for apps.
- **Application runtimes:** native Go/Rust SDK target, QuickJS, MicroPython. All are separate supervised processes in immutable sandboxes, with script host bridges to the single framework connection. `ApplicationRuntime`, `ApplicationEntrypoint`, `ListRuntimes` added.
- **Provenance:** optional GitHub Artifact Attestations evidence for native executable. `GetBinaryProvenance`, declared evidence metadata and **framework-computed** verification states added. Avoid calling verified provenance a safety guarantee.
- **Initial hardware scope:** exactly three Capsula Mini profiles (Rockchip stock, Amlogic stock, Amlogic Alpine). Backend/resource availability is discoverable; big Capsula/Neo deferred.
- **v0.4 catch-up in Protobuf:** partial/full touch capture; controller raw samples; presentation effect/face values and preset discovery; mute-indication policy and partial global settings patch; shared read-only assets.
- **Engineering readiness:** new `IMPLEMENTATION_READINESS.md` distinguishes implement-now simulator milestones from ship-time validation gates.

**Status:** Draft, compatibility not frozen. All three `.proto` schemas compiled successfully using local `protoc` 3.13 with `--experimental_allow_proto3_optional`; generated Go/Rust clients and end-to-end tests remain outstanding.
