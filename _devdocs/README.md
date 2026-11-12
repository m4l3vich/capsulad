# Capsula Universal Framework — architecture draft 0.5

> **Historical design archive.** For work on the implemented project, start with
> [agent/contributor guidance](../AGENTS.md), [current status](../docs/STATUS.md),
> and [speaker behavior](../docs/SPEAKER_BEHAVIOR.md). The draft below records
> earlier intent; its app, authentication, and runtime assumptions are not current requirements.

A **draft specification**, not an implemented runtime or frozen API, for a local-only Protobuf/gRPC application framework and hardware abstraction layer for jailbroken VK Capsula Mini devices.

## Files

- `ARCHITECTURE.md` — complete design, decisions, and backend scope.
- `IMPLEMENTATION_READINESS.md` — what can be built now and what requires hardware validation.
- `proto/capsula/framework/v1/framework.proto` — primary framework API.
- `proto/capsula/contracts/media/v1/media.proto` — optional app-supplied media-player contract.
- `proto/capsula-framework-v0.5.protoset` — compiled descriptor bundle for dynamic tooling.
- `docs/APP_PACKAGING.md` — native/QuickJS/MicroPython entrypoints and provenance.
- `docs/WEB_UI_CERTIFICATES.md` — HTTPS mTLS/bootstrap expectations (NOT network gRPC).
- `docs/TARGETS.md` — three first-stage Capsula Mini platform targets.
- `docs/MEDIA_CONTRACT.md` — media contract semantics.
- `examples/` — sample custom LED ring controller interface.
- `VALIDATION.md` — protobuf compiler validation.
- `CHANGES.md` — changes from draft 0.4.

No applications connect directly to one another's IPC sockets. They connect to one framework Unix socket; native SDKs and script runners provide communication bindings. The Web UI is a separate HTTPS listener with mTLS client certificates required by default.
