# Native runtime descriptor

`src/dynamic.rs` embeds `descriptors/capsulad-framework.protoset` with
`include_bytes!`. `CallJSON` and the other dynamic JSON codecs use that snapshot;
new protocol fields absent from it are unknown to the native runtime and can be
dropped while decoding replies.

Regenerate the framework descriptor from the canonical protocol tree after
schema changes with `make -C sdk/native descriptor`, which runs:

```sh
protoc -I proto --include_imports \
  --descriptor_set_out=sdk/native/descriptors/capsulad-framework.protoset \
  capsulad/framework/v1/framework.proto \
  capsulad/contracts/clock/v1/clock.proto \
  capsulad/contracts/media/v1/media.proto \
  capsulad/contracts/ring/v1/ring.proto
```

The `internal/nativedescriptor` Go test compares this committed descriptor's
message and field names with the generated Go framework descriptor. It catches
file drift, but it cannot inspect the library already installed on a device:
the native runtime must also be rebuilt and redeployed after regeneration for
apps to see new fields. Until then a deployed library can still lag and drop
fields such as `DeviceInfo.hardware_serial`; apps can use `CAPSULAD_DEVICE_SERIAL`
as a descriptor-free path to the host serial.
## Outbound HTTP, remote gRPC and primitives

The library also implements the outbound pieces apps otherwise bundle:

- `src/http.rs` — bounded HTTP/1.1 client (`Connection: close`, one request per
  call, no decompression, verified TLS 1.2+). Redirects follow the 301/302/303
  and 307/308 rules (301/302/303 keep `HEAD` as `HEAD`; everything else becomes
  a bodiless GET) and refuse `https` → `http` downgrades. Cross-origin hops drop
  caller headers. Bodies and header blocks are capped (`TOO_LARGE` past the
  cap), as are interim 1xx responses (8), chunked trailers (64 fields / 64 KiB)
  and response header control bytes (`TRANSPORT`). `connect_timeout_ms` and
  `read_timeout_ms` are per connect/read; `total_timeout_ms` (default 120 s, `0`
  means the default) bounds the whole exchange including redirects. Each request
  carries a unique caller id; `http_cancel(handle, id)` aborts only that exchange
  and is sticky across the registration race (a bounded set of 64 pre-registration
  cancels turns the matching request into `CANCELLED`; late cancels are no-ops).
  `http_close` cancels every in-flight request, waits for them unless called on a
  Tokio runtime thread (Arc snapshots keep the state valid), and invalidates the
  handle.
- `src/tls.rs` — rustls client configuration. Roots are the system PEM bundles
  (`/etc/ssl/certs/ca-certificates.crt`, `/etc/ssl/cert.pem`,
  `/etc/pki/tls/certs/ca-bundle.crt`) plus `webpki-roots`, webpki only, or `pem`
  (caller `ca_pem` only, no system or public roots; opening fails when it adds no
  certificates). There is no insecure mode. The connector is also used for gRPC,
  where ALPN is `h2`.
- `src/grpc_remote.rs` — a tonic channel over TLS/h2 or h2c prior knowledge.
  The resulting `ClientInner` reuses the existing call queues, `RawCodec` and
  metadata path; it has no run id and no app bus.
- `src/primitives.rs` — base64, RFC 3986 percent coding, SHA-256, getrandom
  bytes, UUIDv4 and absolute-URL decomposition.
- `src/headers.rs` — strict JSON string-map validation for HTTP headers and gRPC
  metadata (lowercasing, forbidden names, CR/LF, reserved protocol fields).
- `src/call.rs` — the outbound response backlog is bounded by both entries (512
  audio-sized messages) and bytes (16 MiB): a reservation that would exceed the
  byte budget fails the call with `BUSY`/13 instead of dropping data, and a
  remote client also caps one decoded message at 4 MiB.

ABI v1 keeps its 1.0 identity: the new function pointers are appended after
`handle_info` and `CAPSULAD_STATUS_TOO_LARGE` (15) is a new status value.

`cargo test --manifest-path sdk/native/Cargo.toml` covers the primitives and URL
parser, HTTP over real loopback sockets (content-length, chunked bodies,
301/302/303/307 redirect rules, `HEAD` preservation, refused https→http
downgrades, cross-origin header stripping, size caps, interim/trailer floods,
total and per-read timeouts, TLS verification with the embedded `testdata/` CA,
private-PEM trust, and cancellation races), metadata validation, the response
byte budget, and a remote-gRPC connection failure. The test certificate, key and
CAs in `testdata/` are fixtures only; they are not used by the shipped library.
