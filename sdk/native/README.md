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

## Outbound HTTP, streams, UDP, listeners, mDNS, remote gRPC and primitives

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
- `src/stream.rs` — outbound TCP/TLS streams (`tcp_open`, `stream_read`,
  `stream_write`, `stream_shutdown`, `stream_close`, `stream_info`). TLS reuses
  `tls.rs` (verified, no insecure mode); `nodelay` and `server_name` are
  options. Reads cap at 4 MiB (`0` = 64 KiB), writes may be partial, clean EOF
  is `END_OF_STREAM`, and the ABI timeout convention is `0` poll /
  `0xFFFFFFFF` wait / milliseconds. The connection is split into independent
  read and write halves with separate mutexes: one logical reader, but a read
  parked on an idle stream never blocks `stream_write` or
  `stream_shutdown(WRITE)`, which sends FIN/close_notify without waiting for
  the reader. A close broadcast aborts both halves with `CANCELLED`.
- `src/udp.rs` — connected or unconnected UDP, optional per-datagram peer JSON,
  datagrams capped at 65507, the same timeout convention, and close
  unblocking `udp_recv`. The SDK is outbound-only: `{bind,port}` must use an
  ephemeral port (`port` 0, and no embedded non-zero port in `bind`), because a
  fixed inbound port would bypass daemon policy; a non-zero port is
  `INVALID_ARGUMENT` with a message pointing at `listener_open`, the mediated
  path for fixed inbound UDP ports.
- `src/control.rs` — the inherited `CAPSULAD_CONTROL_FD` channel (one request
  JSON, one response JSON + one `SCM_RIGHTS` fd in one `sendmsg`, serialized,
  bounded, `CLOEXEC` on the received fd). Every request carries a monotonic
  numeric `id` the daemon must echo; replies with another id are stale (left
  by an earlier timeout), their descriptors are closed, and the request keeps
  reading until the matching reply or the deadline, so a late reply can never
  be returned for the wrong request. Invalid JSON fails immediately after
  closing received descriptors. `AF_UNIX` `SOCK_SEQPACKET` is the contract;
  `SOCK_DGRAM` is accepted where the platform has no `SEQPACKET` (macOS), since
  both preserve message boundaries. Missing/invalid channel is `UNSUPPORTED`.
- `src/listener.rs` — daemon-allocated listeners: `listener_open` requests
  `{id,op:listen,protocol,bind,port,backlog}` (the numeric `id` is added by
  `control.rs` and echoed by the daemon), adopts the returned fd, and
  `listener_accept` (TCP) / `listener_recv`/`listener_send` (UDP) operate on it.
  The daemon owns address/port/backlog policy; the app never binds or chooses a
  path.
- `src/mdns.rs` — `Publish`/`Unpublish` through the client unary JSON path and
  server-streaming `Browse` returning a normal call handle for
  `call_recv_json`. Request/response bodies are canonical protobuf JSON for
  `capsulad.framework.v1.DiscoveryService`, passed through unchanged. Remote
  refusals are classified from the gRPC code: `InvalidArgument` (3) becomes
  `INVALID_ARGUMENT`, `NotFound` (5) becomes `NOT_FOUND`, and everything else
  stays `GRPC`.

ABI v1 is minor 0 (`1.0.0`) and this tree is pre-release: the 19 networking
function pointers are appended after `http_cancel` and
`CAPSULAD_STATUS_UNSUPPORTED` (16) is appended to the status list without
changing `CAPSULAD_ABI_MINOR` (`0`) or the runtime metadata. Earlier bindings
still work because `capsulad_get_api` copies only their smaller table; the
macOS current and compatibility versions are both 1.0.0. The table stays
append-only at ABI 1.0 until the first external consumer or release.

`cargo test --manifest-path sdk/native/Cargo.toml` covers the primitives and URL
parser, HTTP over real loopback sockets (content-length, chunked bodies,
301/302/303/307 redirect rules, `HEAD` preservation, refused https→http
downgrades, cross-origin header stripping, size caps, interim/trailer floods,
total and per-read timeouts, TLS verification with the embedded `testdata/` CA,
private-PEM trust, and cancellation races), metadata validation, the response
byte budget, and a remote-gRPC connection failure. `tests/net.rs` adds TCP
round-trip/EOF/shutdown, read timeout plus close-unblocks, a parked-read
regression proving `stream_write` and `shutdown(WRITE)` do not wait for the
reader, TLS stream verification, UDP connected/unconnected send/recv and close,
the ephemeral-bind-port policy (including an embedded port in `bind`), listener
open/accept/recv/send through a real `socketpair` fake daemon (fd passed with
`SCM_RIGHTS`), listener policy errors, absent/invalid control channel, a stale
control reply whose descriptor is drained and closed and an invalid-JSON reply
whose descriptor is closed, mDNS argument validation and the ABI 1.0 full table
identity. The mDNS gRPC-code mapping has a unit test in `src/mdns.rs`. The test
certificate, key and CAs in `testdata/` are fixtures only; they are not used by
the shipped library.
