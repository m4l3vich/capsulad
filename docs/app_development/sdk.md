# SDK: native, Go, Rust, C, QuickJS, MicroPython

Every runtime talks to the same framework through the same shared library and the same
frameworks services. The library is a platform component: an app never bundles or ships it.

| Layer | What it is | Use from |
|---|---|---|
| `libcapsulad` | The shared engine. Owns the daemon transport, the app bus, reflection, the audio fast path, outbound HTTP/1.1, remote gRPC, outbound TCP/TLS streams, UDP, daemon-mediated listeners, mDNS calls and small primitives. Frozen C ABI v1 (1.0, pre-release). | any language that can call C |
| Go binding | `capsulad/sdk/capsulad`, cgo over the C ABI | native Go apps |
| Rust binding | crate `capsulad`, `libloading` over the C ABI | native Rust apps |
| C / C++ | the header plus `dlopen`/`dlsym` | native C/C++ apps |
| QuickJS host | the engine plus a JavaScript prelude exposing `capsulad.*` | `runtime = "quickjs"` |
| MicroPython host | the engine plus a Python prelude exposing `import capsulad` | `runtime = "micropython"` |

The daemon-to-app wire protocol may change together with the daemon; the C ABI is v1 (1.0,
pre-release) and is frozen within that major version. The stream, UDP, listener and mDNS entry
points are appended to the table in this tree; a binding built against an earlier revision
keeps working with its shorter table.

## The native SDK model

### One library, dynamically loaded

The launcher injects the absolute path of a compatible library in `CAPSULAD_LIBRARY` and its
directory in `CAPSULAD_LIBRARY_DIR` when a runtime is installed. When `CAPSULAD_LIBRARY` is
empty, load the platform SONAME: `libcapsulad.so.1` on Linux, `libcapsulad.1.dylib` on Darwin.
`CAPSULAD_LIBRARY_DIR` exists so the loader can resolve the library's neighbours (the script
hosts live beside it).

- **Do not link the library statically and do not vendor a copy.** The device installs it, the
  version is the device's, and replacing it is an administrator operation.
- **Keep it resident for the process lifetime.** ABI v1 has no complete joined shutdown; do not
  `dlclose` after closing clients. Let process exit release it.
- The library spawns its own threads and a small async runtime. It must be able to create them
  before you open a client.

### The frozen C ABI

```c
#include <capsulad/capsulad.h>

capsulad_api table;
memset(&table, 0, sizeof table);
int32_t st = capsulad_get_api(CAPSULAD_ABI_MAJOR /* 1 */, sizeof table, &table);
if (st != CAPSULAD_STATUS_OK) {
  /* UNSUPPORTED_ABI when the major differs, INVALID_ARGUMENT for a bad size.
     No table was written in either case. */
}
```

Rules that hold for every binding:

- `abi_major` must be `1`. Fields are never reordered or removed; in this pre-release tree the
  table is append-only at ABI 1.0, so new entry points extend it without changing the identity.
  `capsulad_abi_minor()` reports the library's minor; an app that uses the networking surface
  declares `[native_sdk] min_minor = 0` (or omits the requirement).
- The table is copied up to `min(table_size, capsulad_api_size())` and never written past
  `table_size`. `out->struct_size` reports the library's full table size: keep your table
  zero-initialized and require every function you call to be within that size and non-NULL.
- `CAPSULAD_API_V1_MIN_SIZE` is the stable prefix (through `struct_size`). A table built against
  an older minor with fewer pointers is still accepted.
- `capsulad_api_v1()` returns the read-only process-wide table; `capsulad_api_size()`,
  `capsulad_abi_major()`, `capsulad_abi_minor()` and `capsulad_version()` report identity.

The table covers: client connect/close, descriptor registration, client info, unary JSON and
unary protobuf, call start (direct, routed and with metadata), send JSON/protobuf, half-close,
recv JSON/protobuf, end, cancel, retain/release, call info/status, bus open/close/ready, bus
notification of invocation and action results, the four audio functions, `buf_free`,
`last_error`, `handle_info`, an outbound HTTP/1.1 client (open/request/cancel/close), a remote
gRPC client, encoding/crypto/URL primitives, then — appended in that order — the networking
surface: `tcp_open`, the six stream functions, the four UDP functions, the six listener
functions and the three mDNS functions. Append order after `http_cancel` is fixed, so a newly
added entry point can never shift an older pointer. The HTTP, remote gRPC, primitive and
networking pointers are appended after `handle_info`; a binding built against an earlier
revision sees them as absent (`capsulad_api_size()` is the authority, not a version check).

### Handles and lifetimes

| Handle | Created by | Released by |
|---|---|---|
| client | `client_connect(socket, run_id, app_id, timeout_ms)` | `client_close` |
| bus | `bus_open(client, callbacks)` | `bus_close` |
| call | `call_start`, `call_start_routed`, `mdns_browse_open`, `audio_capture_open`, `audio_playback_open`, or handed to `on_call` | `call_release` (or `call_end`/`call_cancel` on an outbound call) |
| stream | `tcp_open`, `listener_accept` | `stream_close` |
| udp | `udp_open` | `udp_close` |
| listener | `listener_open` | `listener_close` |

- Handles are process-local integers. `0` is never a valid handle.
- Handles are thread-safe. One call has **one logical reader**: only one thread may be inside
  `call_recv_*` for a given call at a time. Concurrent sends are allowed.
- A stream, UDP socket or listener has **one logical reader** as well: one thread in
  `stream_read`, `udp_recv`, `listener_accept` or `listener_recv` at a time. Writes are
  serialized inside the engine; a second writer blocks rather than interleaving.
- `stream_close`, `udp_close` and `listener_close` unblock a pending read, receive, accept or
  write with `CANCELLED`; using a released handle is `NOT_FOUND` or `CLOSED`.
- A call handle is reference-counted. Inbound calls arrive with one reference owned by the
  library and delivered to `on_call`; returning from `on_call` without ending the call ends it
  with `INTERNAL`. Use `call_retain` to keep working asynchronously, then `call_release`.
- `call_cancel`, `call_release`, `bus_close` and `client_close` unblock a pending
  `call_recv_*` with `CANCELLED`.
- `client_info`, `bus`, `call`, `call_status`, `stream_info` and `listener_info` each render a
  small JSON object; treat it as diagnostics, not as a stable schema beyond the documented
  fields.

Callbacks (`on_ready`, `on_disconnected`, `on_event`) are serialized per bus. `on_call` runs on
one dedicated thread per inbound call, so a handler may block in `call_recv_*`. Callbacks must
not unwind across the FFI boundary: catch every exception or panic inside them.

### Buffers

Output bytes are library-owned `capsulad_buf` values (`data`, `len`), freed exactly once with
`buf_free`. `len` excludes the library's trailing NUL for text buffers. An empty result is
`{data = NULL, len = 0}`. Input buffers are borrowed for the duration of the call only; the
library copies what it needs before returning.

### Status model

Every C function returns a `capsulad_status`. Local failures use this code; a call that ended
with a remote error additionally carries the canonical gRPC code through `call_status` and the
`AppCallEnd` frame.

| Code | Value | Meaning |
|---|---|---|
| `CAPSULAD_STATUS_OK` | 0 | Success. |
| `CAPSULAD_STATUS_INVALID_ARGUMENT` | 1 | Malformed argument, bad buffer, invalid JSON or an invalid descriptor set. |
| `CAPSULAD_STATUS_UNSUPPORTED_ABI` | 2 | `abi_major` is not 1. |
| `CAPSULAD_STATUS_NOT_CONNECTED` | 3 | The client or bus is not connected. |
| `CAPSULAD_STATUS_CLOSED` | 4 | The handle was closed or released. |
| `CAPSULAD_STATUS_DESCRIPTOR_NOT_FOUND` | 5 | The JSON path needs a descriptor for this service/method and none is registered. |
| `CAPSULAD_STATUS_INVALID_MESSAGE` | 6 | Bytes are not a valid message for the expected type. |
| `CAPSULAD_STATUS_TRANSPORT` | 7 | Socket or transport failure. |
| `CAPSULAD_STATUS_GRPC` | 8 | The remote returned a non-OK gRPC status; read `call_status` for the code and detail. |
| `CAPSULAD_STATUS_END_OF_STREAM` | 9 | No more messages. A successful unary reply is consumed first. |
| `CAPSULAD_STATUS_TIMEOUT` | 10 | The receive timeout elapsed; the call is still usable. |
| `CAPSULAD_STATUS_CANCELLED` | 11 | The call was cancelled, its deadline expired, or a handle it depends on closed. |
| `CAPSULAD_STATUS_NOT_FOUND` | 12 | Unknown handle or unknown target. |
| `CAPSULAD_STATUS_BUSY` | 13 | A bounded queue or the live-call budget is full. |
| `CAPSULAD_STATUS_INTERNAL` | 14 | Engine invariant or an unexpected runtime failure. |
| `CAPSULAD_STATUS_TOO_LARGE` | 15 | An HTTP body, header block, chunk, read length or datagram above its configured cap. |
| `CAPSULAD_STATUS_UNSUPPORTED` | 16 | The runtime has no usable `CAPSULAD_CONTROL_FD`, so daemon-mediated listeners are unavailable. Appended to the v1 status list in this pre-release tree. |

`last_error()` returns the library's human-readable message for the last failure on the
calling thread; it is diagnostic, not an error object.

### Deadlines and cancellation

- `call_start(..., timeout_ms)` sets the overall call deadline. `0` means no deadline. A
  deadline that expires cancels the call: pending and subsequent receives return `CANCELLED`.
- `call_recv_json` / `call_recv_proto` take their own bound: `0xFFFFFFFF` waits until data or a
  terminal event, `0` polls, any other value is a millisecond bound. A receive timeout returns
  `TIMEOUT` and leaves the call open, so a poll loop can keep reading.
- `call_cancel` cancels immediately and unblocks a reader. Cancelling after the call ended is a
  no-op.
- Routed calls default to a 30 s deadline and are capped at 5 minutes unless the caller passes
  a shorter timeout.
- Stream, UDP and listener calls share one convention: `0` polls once, `0xFFFFFFFF` waits
  indefinitely and any other value bounds that operation in milliseconds. A timeout returns
  `TIMEOUT` and leaves the handle usable. `tcp_open` and `udp_open` treat `0` as the documented
  30 s default because opening must complete or fail; `listener_open` treats `0` as its default
  control budget. See [network.md](network.md#availability-and-abi).
- Losing the framework connection fails every in-flight call with `UNAVAILABLE`. Calls are
  never replayed to a new process.

### Encoding: reflection JSON versus protobuf

Two payload paths exist on every call:

| Path | Requires | Use for |
|---|---|---|
| JSON (`call_send_json`, `call_recv_json`, `client_unary`) | a registered or embedded descriptor for the message type | readable code, discovery clients, script hosts |
| Protobuf (`call_send_proto`, `call_recv_proto`, `client_unary_proto`) | nothing | custom app interfaces, minimal payloads, raw AppBus relays |

Canonical protobuf JSON means lowerCamelCase field names, enum **names**, base64 `bytes` and
64-bit integers encoded as **strings**. Unknown or missing descriptors fail explicitly with
`DESCRIPTOR_NOT_FOUND`; there is no guessing. Raw protobuf always works, including for a
routed call whose target's descriptor is not registered locally.

For framework services, the descriptors are built into the library, so JSON works without
registration. For your own interface, register a `FileDescriptorSet` first:

```c
api->client_register_descriptors(client, set_bytes, set_len);
```

Script hosts expose the same call: `capsulad.registerDescriptors(bytes)`.

### Audio fast path

Audio does not travel through JSON or base64. `audio_capture_open` and
`audio_playback_open` return a call handle used with dedicated functions:

- `audio_capture_recv(call, timeout_ms, &event)` fills a `capsulad_pcm_event`: `kind`
  (`STARTED`, `CHUNK`, `NOTICE`), `sequence`, `first_frame`, `monotonic_time_ns`,
  `frame_count`, `discontinuity`, `dropped_frames_before`, plus `pcm` bytes for a chunk and
  `json` metadata for started/notice events. Free **both** buffers with `buf_free`.
- `audio_playback_write(call, pcm, len, frame_count, monotonic_time_ns, discontinuity, dropped)`
  enqueues one chunk. The engine copies the bytes before returning, so the caller may reuse the
  buffer. `frame_count` 0 and time 0 mean "derive from the length and the current clock".
- `audio_playback_control(call, CAPSULAD_PLAYBACK_FLUSH|PAUSE|RESUME)` controls a live stream.

See [audio.md](audio.md) for the stream contract.

### HTTP, remote gRPC and primitives

The same library also gives native apps outbound networking without bundling a TLS, HTTP or
gRPC stack. None of it is available from the QuickJS or MicroPython hosts.

**HTTP/1.1** (`http_open`, `http_request`, `http_cancel`, `http_close`) is one request per call with verified
TLS 1.2+, `Connection: close` and no automatic decompression. Options are a snake_case JSON
object; unknown keys are rejected:

| Key | Default | Notes |
|---|---|---|
| `roots` | `"system"` | `"system"` = system PEM bundle + Mozilla roots + `ca_pem`; `"webpki"` = Mozilla roots + `ca_pem`; `"pem"` = `ca_pem` only (no system bundle, no public roots). |
| `ca_pem` | — | Extra PEM certificates appended to the roots. Required by `roots: "pem"`, which fails to open without at least one CERTIFICATE. |
| `user_agent` | `capsulad-native/1.0` | Rejected if it contains control characters. |
| `max_body` | 32 MiB | Hard cap 64 MiB; exceeding it returns `TOO_LARGE`. |
| `follow_redirects` | 4 | 0 disables. 301/302/303 keep `HEAD` as `HEAD` and turn other methods into a bodiless GET; 307/308 preserve method and body. Cross-origin hops drop every caller header except `user-agent`, and an `https` → `http` hop is refused with `TRANSPORT` (no cleartext downgrade). |
| `connect_timeout_ms` | 15000 | DNS + TCP + TLS. |
| `read_timeout_ms` | 30000 | Each individual read and write on the connection. |
| `total_timeout_ms` | 120000 | The whole exchange, redirects included. Per-request `timeout_ms` overrides it. `0` selects the default; it never means "unlimited". |

Only absolute `http`/`https` URLs are accepted; userinfo and fragments are rejected. Caller
headers are a JSON string map; names are lowercased and `host`, `content-length`,
`transfer-encoding` and `connection` are rejected. The response header block must be ASCII:
names are validated as RFC 7230 tokens (lowercased) and values may contain visible ASCII
(`0x20`–`0x7e`) plus HTAB; obs-text, C1 controls and CR/LF/NUL are protocol failures and
return `TRANSPORT`. Response headers are a JSON object with
lowercase names and the last value winning. At most eight
interim `1xx` responses are read, and chunked trailers are bounded by both field count and
total bytes. A non-2xx response is still `OK` with
`out_status_code` set. DNS/TCP/TLS/parse failures are `TRANSPORT`, timeouts are `TIMEOUT`,
malformed input is `INVALID_ARGUMENT`.

`http_request` takes a caller-supplied, non-zero `request_id` that must be unique for the
lifetime of the client: an id is never reused, not even after its request returned (zero, or
an id already in flight, is `INVALID_ARGUMENT`). `http_cancel(http,
request_id)` aborts only that exchange; its `http_request` returns
`CAPSULAD_STATUS_CANCELLED`. Cancellation is sticky across the registration race: an id with no
registered request yet is remembered (bounded to 64 ids, oldest evicted) and the matching
`http_request` then fails with `CAPSULAD_STATUS_CANCELLED` before any network I/O. Cancelling
an id whose request already completed is a successful no-op: it neither arms a later
cancellation nor affects any other request, and a completed id is never used again. `http_cancel`
with id `0` is a no-op. Concurrent requests on one client are
independent. `http_close` cancels every in-flight request and then releases the handle: it
waits until no request can still use the TLS/root state, unless it is called on a Tokio runtime
thread, where it returns without blocking (requests hold `Arc` snapshots, so the state stays
valid). Either way the handle is unusable once it returns.

**Remote gRPC** (`grpc_open`, `call_start_meta`) returns a normal client handle. The URL is an
origin only (`https://host[:port]` or `http://host[:port]`): `https` verifies TLS with ALPN
`h2`; `http` speaks h2c with prior knowledge. Options: `roots` (`system` | `webpki` | `pem`,
same meaning as HTTP), `ca_pem`, `server_name`, and
`connect_timeout_ms` (default 10000). The handle supports `client_close`, `client_info`,
`client_register_descriptors`, `client_unary*`, `call_start`, `call_start_meta`, send/recv and
half-close. `bus_open` and `call_start_routed` return `INVALID_ARGUMENT` because a remote
channel has no app bus, and no `capsulad-run-id` metadata is sent. `client_unary` JSON needs a
descriptor registered with `client_register_descriptors`; `client_unary_proto` and raw calls
need none. `call_start_meta` validates lowercase ASCII metadata (no CR/LF, no `-bin` keys) and
rejects names owned by the protocol — `content-type`, `te`, `trailer`, `grpc-timeout`,
`grpc-encoding`, `grpc-accept-encoding`, `user-agent`, `host` — plus the SDK's
`capsulad-run-id`. A remote client also accepts at most 4 MiB per decoded response message.

**Primitives** mirror the small helpers apps repeatedly need: `base64_encode`/`base64_decode`
(`CAPSULAD_BASE64_STANDARD`, `_URL_SAFE`, `_NO_PADDING`, OR-able; decode is strict),
`percent_encode`/`percent_decode` (RFC 3986 unreserved set; malformed `%` rejected),
`sha256` (32 raw bytes), `random_bytes` (getrandom), `uuid_v4` (lowercase canonical) and
`url_parse` (`{"scheme","host","port","path","query"}`, effective port).

```c
const char *options = "{\"roots\":\"system\",\"follow_redirects\":4}";
capsulad_handle http;
if (table.http_open((const uint8_t *)options, strlen(options), 0, &http)
    != CAPSULAD_STATUS_OK) { /* last_error() */ }
int32_t status;
capsulad_buf headers, body;
/* The request id is caller-assigned, non-zero and unique while in flight. */
int32_t st = table.http_request(http, 1, "GET", 3, "https://example.com/", 20,
                                NULL, 0, NULL, 0, 5000, &status, &headers, &body);
/* From another thread, while request 1 is still in flight:
 *   table.http_cancel(http, 1);   // that http_request returns CANCELLED
 * Cancelling an id that already finished is a successful no-op. */
if (st == CAPSULAD_STATUS_OK) {
  table.buf_free(headers);
  table.buf_free(body); /* status is the HTTP code */
}
table.http_close(http);
```

### Outbound TCP/TLS streams and UDP

This pre-release tree appends outbound TCP with optional verified TLS and connected/unconnected
UDP to the ABI 1.0 table. The complete contract — options, policy, limits, statuses and cleanup
— is [network.md](network.md). Signatures:

```c
int32_t tcp_open(const char *host, size_t host_len, uint16_t port,
                 const uint8_t *options_json, size_t options_len,
                 uint32_t timeout_ms, capsulad_handle *out_stream);
int32_t stream_read(capsulad_handle stream, size_t max_len, uint32_t timeout_ms,
                    capsulad_buf *out_data);
int32_t stream_write(capsulad_handle stream, const uint8_t *data, size_t len,
                     uint32_t timeout_ms, size_t *out_written);
int32_t stream_shutdown(capsulad_handle stream, int32_t how);
int32_t stream_close(capsulad_handle stream);
int32_t stream_info(capsulad_handle stream, capsulad_buf *out_json);

int32_t udp_open(const char *host, size_t host_len, uint16_t port,
                 const uint8_t *options_json, size_t options_len,
                 uint32_t timeout_ms, capsulad_handle *out_udp);
int32_t udp_send(capsulad_handle udp, const uint8_t *peer_json, size_t peer_len,
                 const uint8_t *data, size_t len, uint32_t timeout_ms,
                 size_t *out_sent);
int32_t udp_recv(capsulad_handle udp, size_t max_len, uint32_t timeout_ms,
                 capsulad_buf *out_data, capsulad_buf *out_peer_json);
int32_t udp_close(capsulad_handle udp);
```

`tcp_open` options are `tls`, `roots`, `ca_pem`, `server_name` and `nodelay`; TLS always
verifies a chain with TLS 1.2+ and there is no insecure mode. `stream_read` returns
`END_OF_STREAM` on a clean remote close, `stream_write` may write fewer bytes than requested and
rejects an empty buffer, and `max_len` 0 means 64 KiB with a 4 MiB cap. UDP datagrams are capped
at 65507 bytes, `udp_send` names a peer unless the socket is connected, and a peer address must
be an IP literal — peer JSON is not resolved. `udp_open` needs both host and port or neither,
and binds an ephemeral local port only: a fixed inbound UDP port is a UDP listener, never a
`udp_open` socket.

Bindings: Go `OpenTCP`/`TCPStream` and `OpenUDP`/`UDPConn` with `TCPOptions`/`UDPOptions`
(Rust `net::Stream`, `net::UdpSocket`, `TcpOptions`, `UdpOptions`). Complete examples are in
[network.md](network.md#examples).

### Daemon-mediated listeners

An app never creates or binds a listening socket. `listener_open` asks the daemon over the
inherited control channel and receives the already-bound descriptor:

```c
int32_t listener_open(const uint8_t *options_json, size_t options_len,
                      uint32_t timeout_ms, capsulad_handle *out_listener,
                      capsulad_buf *out_info);
int32_t listener_accept(capsulad_handle listener, uint32_t timeout_ms,
                        capsulad_handle *out_stream, capsulad_buf *out_peer_json);
int32_t listener_recv(capsulad_handle listener, size_t max_len,
                      uint32_t timeout_ms, capsulad_buf *out_data,
                      capsulad_buf *out_peer_json);
int32_t listener_send(capsulad_handle listener, const uint8_t *peer_json,
                      size_t peer_len, const uint8_t *data, size_t len,
                      uint32_t timeout_ms);
int32_t listener_info(capsulad_handle listener, capsulad_buf *out_json);
int32_t listener_close(capsulad_handle listener);
```

Options are `protocol` (`tcp`/`udp`), `bind`, `port` and `backlog`; `port` `0` is ephemeral,
explicit ports must be `1024..65535`, and 5353 plus the configured Web UI port are refused.
`out_info` carries the bound `{"protocol","address","port"}`. There are at most 16 listeners per
run, and the daemon closes any un-taken listener and the control channel when the run ends.
`CAPSULAD_CONTROL_FD` is a reserved inherited descriptor: never read, write, close or reuse it,
and there is no other way to obtain a listener. Without the channel every listener call returns
`UNSUPPORTED`. Bindings: Go `OpenListener`/`Listener` with `Accept`/`Receive`/`Send`/`Info`;
Rust `listener::Listener` with the same operations.

### mDNS discovery

mDNS goes through the daemon's Avahi responder, not through the app:

```c
int32_t mdns_publish(capsulad_handle client, const uint8_t *request_json,
                     size_t request_len, uint32_t timeout_ms,
                     capsulad_buf *out_json);
int32_t mdns_unpublish(capsulad_handle client, const char *publication_id,
                       size_t publication_id_len, uint32_t timeout_ms);
int32_t mdns_browse_open(capsulad_handle client, const uint8_t *request_json,
                         size_t request_len, uint32_t timeout_ms,
                         capsulad_handle *out_call);
```

Requests and responses are canonical protobuf JSON for
`capsulad.framework.v1.DiscoveryService`. `Browse` returns a normal call handle: read each
`DiscoveredService` with `call_recv_json` and finish with `call_release` or `call_cancel`.
`Publish` and `Unpublish` release their internal call handle for you and map a remote
`INVALID_ARGUMENT` to `CAPSULAD_STATUS_INVALID_ARGUMENT` and `NOT_FOUND` to
`CAPSULAD_STATUS_NOT_FOUND`; every other non-OK gRPC status arrives as `GRPC` (8) with the reason
in the message, so there is no `call_status` to read afterwards.
Publications are scoped to the caller's run and removed on unpublish, run exit and daemon
shutdown; a build with no responder ends the call with `FAILED_PRECONDITION` (surfaced as
`GRPC`, never as a false success).
Bindings: Go `Client.PublishMDNS`/`UnpublishMDNS`/`BrowseMDNS` with `MDNSService`,
`MDNSPublication`, `MDNSEvent`; Rust `Client::publish_mdns`/`unpublish_mdns`/`browse_mdns` with
`MdnsService`, `MdnsPublication`, `DiscoveredService`, `Browse`. Scripts use the framework call
path (`capsulad.call` and `capsulad.stream`). Fields, access and errors are in
[services.md](services.md#discoveryservice); the full publication and browse contract is in
[network.md](network.md#mdns-publication-and-browsing).

## Go binding

Module `capsulad/sdk`, package `capsulad`. It uses cgo, so a build needs a C compiler and, for
the device, a target cross compiler.

### Adding the module

The binding is distributed with the platform SDK. Keep it beside your project and point the
module at that copy; there is no public module proxy:

```sh
go mod init example.com/thermostat
go mod edit -require=capsulad/sdk@v0.0.0
go mod edit -replace=capsulad/sdk=/path/to/platform/capsulad-sdk
```

### Connect, start, close

```go
env := capsulad.FromEnv() // CAPSULAD_LIBRARY, CAPSULAD_SOCKET, CAPSULAD_RUN_ID, CAPSULAD_APP_ID, CAPSULAD_SDK, CAPSULAD_DEVICE_SERIAL
client, err := capsulad.Connect(ctx, env)   // loads the library, opens the client
if err != nil { return err }
defer client.Close()

client.SetHandlers(capsulad.Handlers{ /* see below */ })
err = client.Start(ctx)                    // opens the bus, sends hello + ready, then blocks
```

- `Connect` fails immediately if `CAPSULAD_RUN_ID` is empty or the library cannot be loaded.
- `Start` sends `AppHello{run_id}` and `AppReady`, waits for both to be handed to the transport,
  calls `Ready`, then blocks until the bus ends or `ctx` is cancelled. It returns the terminal
  error: `ErrClosed` after `Close`, the context error after cancellation, or the bus failure.
- `Close` is idempotent and unblocks everything. `Done()` reports the bus ending; `Err()` is
  the terminal error.
- A handler must not call `Close`/`Start` recursively. Work that outlives a callback belongs in
  your own goroutine.

### Handlers

| Field | Called when | Notes |
|---|---|---|
| `Ready` | hello and ready were flushed | output can be opened here |
| `Invocation` | a launch or singleton invocation arrived | report with `NotifyInvocationResult` |
| `Settings` | own settings changed, or the initial snapshot after ready | masked values are in clear here |
| `Action` | a declared action was triggered | report with `NotifyActionResult` |
| `Activation` | the selected assistant received a turn | assistant apps only |
| `TouchRelease` | hold-to-talk release | correlate with the activation ID |
| `Disconnected` | the bus ended | terminal; exit or let the supervisor restart you |

Inbound custom calls are registered separately:

```go
client.Handle("org.example.thermostat.v1.ThermostatService", "GetTarget",
    func(ctx context.Context, call *capsulad.ServerCall) {
        req, err := call.Recv()          // raw protobuf bytes
        if err != nil && !errors.Is(err, io.EOF) { call.End(13, err.Error()); return }
        _ = req
        _ = call.Send(replyBytes)        // zero or more responses
        _ = call.End(0, "")              // always end, including errors
    })
```

`ServerCall` exposes `ID()`, `Service()`, `Method()`, `Context()`, `Recv()`, `RecvJSON(v)`,
`Send()`, `SendJSON(v)` and `End(code, detail)`. Returning without `End` makes the caller see
`INTERNAL`. The inbound budget is 64 live calls with a 32-entry dispatch queue; overflow is
rejected with `RESOURCE_EXHAUSTED`.

### Helpers

| Helper | Purpose |
|---|---|
| `OpenPresentation(ctx, resourceID)` → `*Presentation` | publish output; `Segment`, `Ring`, `Effect`, `Face`, `Blank`, `Close`, `ID`, `Visible` |
| `ListPresets(ctx, resourceID)` | built-in effects and faces with typed parameters |
| `OpenCapture(ctx, resourceID, presentationID, mode)` → `*Capture` | routing capture; `Events()`, `Info()`, `Close()` |
| `WatchInput(ctx, captureID)` → `*InputStream` | touch/gesture stream; `Events()`, `Done()`, `Close()` |
| `OpenPlayback(ctx, endpoint, format, conversationID)` → `*Playback` | `Write`, `Control`, `Close` (drain), `Abort` |
| `OpenMic(ctx, activationID, conversationID)` → `*Mic` | `Chunks()`, `Close`; PCM arrives as bytes |
| `BeginConversation(ctx, activationID)` → `*Conversation` | assistant turn lifetime |
| `AudioState`, `ListAudioEndpoints`, `UserSettings` | read-only state helpers |
| `BluetoothMediaState`, `WatchBluetoothMedia`, `BluetoothMediaCommand` | Bluetooth media bridge |
| `Call`, `CallJSON`, `Stream`, `OpenStream`, `OpenBidi` | routed calls and framework streams |
| `RegisterDescriptors(set)` | enable the JSON path for your own messages |
| `NotifyInvocationResult`, `NotifyActionResult` | acknowledgements |

`Call(ctx, targetAppID, service, rpc, request, timeout)` takes serialized protobuf bytes;
`CallJSON(ctx, targetAppID, service, rpc, request, response, timeout)` takes and returns Go
values. Both take a trailing `time.Duration` call timeout; `0` means no deadline beyond the
context. `Stream`/`OpenStream`/`OpenBidi` return a `*Stream` with `Send`, `SendJSON`,
`HalfClose`, `Recv`, `RecvJSON`, `Close`. `Recv` returns `io.EOF` at the end of a stream.

`WatchInput` takes no resource argument: the Go binding always subscribes to
`touch.panel.main`. The framework models exactly one touch resource, and the daemon rejects any
other `resource_id` with `NOT_FOUND`, so a device that ever exposes several panels cannot be
addressed through this helper — the raw service request carries the same restriction.

### Outbound HTTP, remote gRPC and primitives

```go
httpClient, err := capsulad.OpenHTTP(env, capsulad.HTTPOptions{
    Roots: "system", UserAgent: "thermostat/1", MaxBody: 8 << 20,
    FollowRedirects: 4, ConnectTimeout: 10 * time.Second,
    ReadTimeout: 30 * time.Second, TotalTimeout: 2 * time.Minute,
})
if err != nil { return err }
defer httpClient.Close()

response, err := httpClient.Do(ctx, capsulad.HTTPRequest{
    Method: "POST", URL: "https://api.example.com/v1/state",
    Header: map[string]string{"Content-Type": "application/json"},
    Body:   body, Timeout: 15 * time.Second,
})
// response.StatusCode, response.Header (lowercase names), response.Body

grpcClient, err := capsulad.OpenGRPC(ctx, env, "https://voice.example.com",
    capsulad.GRPCOptions{Roots: "system", ConnectTimeout: 10 * time.Second})
if err != nil { return err }
defer grpcClient.Close()

reply, err := grpcClient.Unary(ctx, "example.v1.Service", "Do", map[string]string{
    "authorization": "Bearer token",
}, requestProto, 5*time.Second)

stream, err := grpcClient.NewStream(ctx, "example.v1.Service", "Watch",
    map[string]string{"x-client": "1"}, capsulad.CallBidi, 0)
if err == nil {
    defer stream.Close()
    _ = stream.Send(requestProto)
    payload, recvErr := stream.Recv() // io.EOF at a clean end
    _ = payload
    _ = recvErr
}

digest, _ := capsulad.SHA256([]byte("abc"))
uuid, _ := capsulad.UUIDv4()
encoded, _ := capsulad.Base64Encode([]byte{0xff}, capsulad.Base64URLSafe|capsulad.Base64NoPadding)
parsed, _ := capsulad.ParseURL("https://example.com/a?b=1") // parsed.Port is effective
```

`OpenHTTP`/`OpenGRPC` load the shared library and fail with `LibraryError`/`ABIError` like
`Connect`. `HTTPClient.Do` allocates a native request id per call. Cancelling `ctx` cancels only
that id; a cancel that lands before the native request is registered is remembered (bounded to
64 ids) and fails the request with `CANCELLED` before any network I/O. When the interrupted
exchange reports cancellation, `Do` waits for the native call to return and then returns
`ctx.Err()` if the context is still done, otherwise a `*StatusError`. Concurrent `Do` calls on
one client are fully independent, and a request that completes just as its watcher fires
returns its response. A request started after a cancelled one proceeds normally.
`HTTPClient.Close` cancels every native request and waits for them before releasing the handle
(the native engine skips the wait only when it runs on a Tokio runtime thread; Go calls it from
an application goroutine, so it waits). `*capsulad.StatusError` carries `NativeStatus` so callers can distinguish
a native transport status (7/10/...) from the numerically overlapping canonical gRPC code.

### Networking

The binding wraps the appended ABI v1 networking surface; the contract is [network.md](network.md).

| Call | Purpose |
|---|---|
| `OpenTCP(ctx, env, host, port, TCPOptions) (*TCPStream, error)` | Verified TLS when `TLS` is set. Only a `ctx` **deadline** bounds DNS + TCP + TLS; with no deadline the native 30 s default applies and cancelling the context does not abort the dial. |
| `(*TCPStream) Read(ctx, p)`, `Write(ctx, p)`, `Shutdown(ShutdownWrite\|ShutdownRead\|ShutdownBoth)`, `Close()`, `Info()` | `Read` returns `io.EOF` on a clean close; `Write` may accept fewer bytes than `len(p)` and rejects empty input; `Close` unblocks I/O. Reads and writes wait forever when `ctx` has no deadline. |
| `OpenUDP(ctx, env, host, port, UDPOptions) (*UDPConn, error)` | Connected only when host **and** port are both set; ephemeral local port only. `Send(ctx, *UDPAddr, data)` and `Receive(ctx, maxLen)`. |
| `OpenListener(ctx, env, ListenerOptions) (*Listener, error)` | The daemon binds; `Accept(ctx) (*TCPStream, *UDPAddr, error)`, `Receive`, `Send`, `Info`, `Close`. |
| `(*Client) PublishMDNS`, `UnpublishMDNS`, `BrowseMDNS` | Avahi-backed publication and browse; `BrowseMDNS` returns an iterator whose `Next` returns `io.EOF` at the end. |

Networking errors are `*capsulad.StatusError`; when `NativeStatus` is true, `Code` is an ABI
status (`10` timeout, `16` unsupported, …), not a gRPC code. `ErrClosed` means the handle was
released.

### Complete native Go app

`manifest.toml`:

```toml
id = "org.example.thermostat"
name = "Thermostat"
version = "1.0.0"
description = "Shows a target temperature and serves a small status interface."
single_instance = true
restart_on_failure = true
start_on_boot = true

[entrypoint]
runtime = "native"
path = "bin/thermostat"
architecture = "aarch64"
abi = "linux-gnu"

[native_sdk]
abi_major = 1
min_minor = 0

[[settings]]
key = "target"
label = "Target temperature"
type = "integer"
default = 21
min = 5
max = 30

[[settings]]
key = "colon"
label = "Show the separator"
type = "boolean"
default = true

[[actions]]
id = "redraw"
label = "Redraw the display"

[[arguments]]
name = "target"
type = "integer"
required = false
```

`go.mod`:

```text
module example.com/thermostat

go 1.22

require capsulad/sdk v0.0.0

replace capsulad/sdk => ../capsulad-sdk
```

`main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"log"
	"os/signal"
	"sync"
	"syscall"

	"capsulad/sdk/capsulad"
)

const resource = "display.segment.front"

var (
	mu     sync.Mutex
	target = int64(21)
	colon  = true
	pres   *capsulad.Presentation
)

// digitMask is the seven-segment mask used by the device: bit i selects
// segment i of the digit. 0x7f lights the whole digit.
func digitMask(d int64) uint16 {
	switch d {
	case 0:
		return 0x3f
	case 1:
		return 0x06
	case 2:
		return 0x5b
	case 3:
		return 0x4f
	case 4:
		return 0x66
	case 5:
		return 0x6d
	case 6:
		return 0x7d
	case 7:
		return 0x07
	case 8:
		return 0x7f
	case 9:
		return 0x6f
	}
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	client, err := capsulad.Connect(ctx, capsulad.FromEnv())
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Close()

	draw := func() {
		mu.Lock()
		p, t, c := pres, target, colon
		mu.Unlock()
		if p == nil {
			return
		}
		cells := []uint16{digitMask(t / 10), digitMask(t % 10)}
		if err := p.Segment(cells, c); err != nil {
			log.Printf("frame: %v", err)
		}
	}

	apply := func(s *capsulad.SettingsChanged) {
		mu.Lock()
		for _, v := range s.Values {
			switch v.Key {
			case "target":
				var n int64
				if json.Unmarshal([]byte(v.ValueJSON), &n) == nil {
					target = n
				}
			case "colon":
				var b bool
				if json.Unmarshal([]byte(v.ValueJSON), &b) == nil {
					colon = b
				}
			}
		}
		mu.Unlock()
	}

	ready := make(chan struct{})
	client.SetHandlers(capsulad.Handlers{
		Ready: func() { close(ready) },
		Invocation: func(_ context.Context, inv *capsulad.LaunchInvocation) {
			for _, a := range inv.Arguments {
				if a.Name == "target" {
					var n int64
					if json.Unmarshal([]byte(a.Value), &n) == nil {
						mu.Lock()
						target = n
						mu.Unlock()
					}
				}
			}
			draw()
			_ = client.NotifyInvocationResult(inv.ID, true, "display updated")
		},
		Settings: func(s *capsulad.SettingsChanged) {
			apply(s)
			draw()
		},
		Action: func(_ context.Context, a *capsulad.ActionInvoked) {
			draw()
			_ = client.NotifyActionResult(a.ID, true, "redrawn")
		},
		Disconnected: func(err error) { log.Printf("disconnected: %v", err) },
	})

	// Start blocks until the bus ends; the display lives beside it.
	go func() {
		select {
		case <-ready:
		case <-ctx.Done():
			return
		}
		p, err := client.OpenPresentation(ctx, resource)
		if err != nil {
			log.Printf("presentation: %v", err)
			return
		}
		defer p.Close()
		mu.Lock()
		pres = p
		mu.Unlock()
		draw()
		<-ctx.Done()
	}()

	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("bus: %v", err)
	}
}
```

Build, package and install:

```sh
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  go build -trimpath -o bin/thermostat .
capsulad-cli app package . -out thermostat.tar.gz
capsulad-cli -data-dir /data/capsdemo app install thermostat.tar.gz
/etc/init.d/S99btctl.sh restart          # the registry is read at startup
capsulad-cli -data-dir /data/capsdemo app start org.example.thermostat target=23
capsulad-cli -data-dir /data/capsdemo app runs org.example.thermostat
```

## Rust binding

Crate `capsulad`. It loads the library dynamically with `libloading`; it never links it.

```rust
use std::sync::Arc;
use capsulad::{CallKind, Library};

struct Handler;

impl capsulad::BusHandler for Handler {
    fn on_event(&self, event: capsulad::BusEvent) {
        if let Some(value) = event.value() {
            // Canonical protobuf JSON for the message named by event.kind.
            eprintln!("event {:?}: {value}", event.kind);
        }
    }

    fn on_call(&self, call: capsulad::InboundCall<'_>, info: capsulad::CallInfo) {
        let _ = info;                 // call_id, caller_run_id, service, method, deadline
        let _ = call.recv_proto(None); // request bytes; None waits forever
        let _ = call.send_proto(&[0x0a, 0x02, b'k', b'o']);
        let _ = call.end(0, "");       // always end the call
    }
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let library = Library::load()?;                 // or Library::open(path)
    let client = library.connect_env()?;            // CAPSULAD_SOCKET + CAPSULAD_RUN_ID
    let bus = client.bus(Arc::new(Handler))?;
    bus.ready(Some(std::time::Duration::from_secs(10)))?;

    let call = client.start_call(
        "capsulad.framework.v1.DeviceService",
        "GetInfo",
        CallKind::Unary,
        Some(std::time::Duration::from_secs(5)),
    )?;
    call.send_json(b"{}")?;
    call.half_close()?;
    if let Some(reply) = call.recv_json(None)? {
        println!("{}", String::from_utf8_lossy(&reply));
    }

    // Routed call to another app, raw protobuf on the wire.
    let routed = client.start_routed(
        "capsulad.ring", "capsulad.contracts.ring.v1.RingService", "GetState", None,
    )?;
    routed.send_proto(&[])?;
    routed.half_close()?;
    let _ = routed.recv_proto(None)?;

    Ok(())
}
```

`Library` reports `version()`, `abi_major()`, `abi_minor()`, `path()`. `Client` exposes
`connect`, `connect_env`, `from_env`, `unary_json`, `unary_proto`, `start_call`,
`start_routed`, `register_descriptors`, `bus` and `info`. `Call` exposes `send_json`,
`send_proto`, `half_close`, `recv_json`, `recv_proto`, `end`, `cancel`, `status`, `retain`.
`Error` carries `status()`, `grpc_code()`, `is_cancelled()`, `is_timeout()` and
`is_end_of_stream()`.

### Outbound HTTP, remote gRPC and primitives

```rust
use capsulad::{HttpOptions, HttpRequest, RemoteOptions};
use std::time::Duration;

let http = capsulad::HttpClient::open(&library, &HttpOptions {
    roots: Some("system".into()),
    follow_redirects: Some(4),
    total_timeout: Some(Duration::from_secs(60)),
    ..HttpOptions::default()
})?;
let response = http.request(&HttpRequest {
    method: "GET".into(),
    url: "https://example.com/".into(),
    timeout: Some(Duration::from_secs(5)),
    ..HttpRequest::default()
})?;
println!("{} {}", response.status_code, response.body.len());

let remote = capsulad::Client::open_remote(&library, "https://voice.example.com",
    &RemoteOptions { server_name: Some("voice.example.com".into()), ..RemoteOptions::default() },
    Some(Duration::from_secs(10)))?;
let call = remote.start_call_meta("example.v1.Service", "Do", capsulad::CallKind::Unary,
    &[("authorization", "Bearer token")], Some(Duration::from_secs(5)))?;
call.send_proto(&[0x0a, 0x00])?;
call.half_close()?;
let _reply = call.recv_proto(None)?;

let digest = library.sha256(b"abc")?;
let uuid = library.uuid_v4()?;
let encoded = library.base64_encode(&[0xff], capsulad::BASE64_URL_SAFE | capsulad::BASE64_NO_PADDING)?;
let url = library.parse_url("https://example.com/a?b=1")?;
```

`HttpClient::request` picks its own request id; `HttpClient::request_with_id(id, request)` lets
another thread cancel exactly that exchange with `HttpClient::cancel(id)`. A cancel that lands
before the request is registered is remembered natively (bounded to 64 ids) and the request
fails with `CANCELLED` before any network I/O; cancelling an id that already finished is a
no-op, and other concurrent requests are unaffected. `HttpOptions::total_timeout` bounds the
whole exchange (native default 120 s; `None` or `Some(Duration::ZERO)` selects that default)
and a per-request `timeout` overrides it. Sub-millisecond option timeouts are encoded as 1 ms,
never 0. Dropping the client cancels every in-flight request and waits for them when not called
from a Tokio worker (inside a runtime it returns without blocking, because Arc snapshots keep
the state valid). `Client::open_remote` returns a normal client: the
existing `Call` methods work, `Client::bus` and `Client::start_routed` fail with
`INVALID_ARGUMENT`, and `Client::start_call_meta` applies validated metadata. The primitives
(`base64_encode`, `base64_decode`, `percent_encode`, `percent_decode`, `sha256`,
`random_bytes`, `uuid_v4`, `parse_url`) live directly on `Library` and copy/free the native
buffer once.

### Networking

The crate wraps the appended ABI v1 networking surface; the contract is [network.md](network.md).
`net::Stream` (`open`, `read`, `write`, `shutdown`, `info`, `close`), `net::UdpSocket`
(`open`, `send`, `recv`, `close`), `listener::Listener` (`open`, `accept`, `recv`, `send`,
`info`, `close`) and `Client::publish_mdns`/`unpublish_mdns`/`browse_mdns` with `MdnsService`,
`MdnsPublication`, `DiscoveredService` and `Browse`. `Stream::read` returns `Ok(None)` on a
clean close and a timeout is an `Error` whose status leaves the handle usable; dropping a
stream, socket or listener closes it. Option structs are `TcpOptions`, `UdpOptions` and
`ListenerOptions`; `PeerAddr` is the datagram peer.

## C and C++

```c
#include <capsulad/capsulad.h>
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>

typedef int32_t (*get_api_fn)(uint32_t, size_t, capsulad_api *);

int main(void) {
  const char *path = getenv("CAPSULAD_LIBRARY");
  if (path == NULL) path = "libcapsulad.so.1";

  void *lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  if (lib == NULL) { fprintf(stderr, "dlopen: %s\n", dlerror()); return 1; }

  get_api_fn get_api = (get_api_fn)dlsym(lib, "capsulad_get_api");
  if (get_api == NULL) { fprintf(stderr, "no capsulad_get_api\n"); return 1; }

  capsulad_api api;
  memset(&api, 0, sizeof api);
  int32_t st = get_api(CAPSULAD_ABI_MAJOR, sizeof api, &api);
  if (st != CAPSULAD_STATUS_OK) { fprintf(stderr, "abi: %d\n", st); return 1; }

  const char *socket = getenv("CAPSULAD_SOCKET");
  const char *run_id = getenv("CAPSULAD_RUN_ID");
  const char *app_id = getenv("CAPSULAD_APP_ID");
  if (socket == NULL) socket = "/run/capsulad/hal.sock";

  capsulad_handle client = 0;
  st = api.client_connect(socket, strlen(socket), run_id, strlen(run_id),
                          app_id ? app_id : NULL, app_id ? strlen(app_id) : 0,
                          10000, &client);
  if (st != CAPSULAD_STATUS_OK) { fprintf(stderr, "connect: %d\n", st); return 1; }

  capsulad_buf out = {0};
  st = api.client_unary(client, "capsulad.framework.v1.DeviceService",
                        strlen("capsulad.framework.v1.DeviceService"),
                        "GetInfo", strlen("GetInfo"),
                        (const uint8_t *)"{}", 2, 5000, &out);
  if (st == CAPSULAD_STATUS_OK) {
    printf("%.*s\n", (int)out.len, (const char *)out.data);
    api.buf_free(out);
  }

  api.client_close(client);
  /* Keep the library resident; do not dlclose it after closing clients. */
  return 0;
}
```

Compile against the header directory the platform distributes:

```sh
cc -std=c11 -I/path/to/platform/include app.c -ldl -o bin/app
```

In C++ the same calls work; wrap `capsulad_handle` and `capsulad_buf` in RAII types whose
destructors call `call_release` / `buf_free`, and keep the `capsulad_api` table alive for the
process. Never throw across a callback: catch inside the callback body.

## QuickJS host

`runtime = "quickjs"`, entrypoint path ending in `.js`. The host evaluates your file as a
**classic script** with an injected global `capsulad`. ES modules are not advertised; do not use
`import`/`export`.

Limits and environment:

- 64 MiB JS heap, 1 MiB stack, script file at most 8 MiB.
- No timers (`setTimeout`/`setInterval`), no threads, no filesystem access, no `require`.
  `Date.now()` and plain computation work.
- The process exits when `capsulad.run()` returns (bus ended or `capsulad.stop()`).

Structure of a script:

```js
capsulad.on("call", function (call) { /* ... */ });
capsulad.start();        // required before the script returns
// synchronous setup here
capsulad.run();          // pump events until the bus ends
```

If the script returns without calling `capsulad.start()`, the host exits with an error.

### Module surface

| Member | Signature | Notes |
|---|---|---|
| `capsulad.start(timeoutMs?)` | — | Opens the bus and sends hello + ready. Default 10000 ms. |
| `capsulad.run()` | — | Blocks, dispatching events and calls, until the bus ends. Returns `{status, detail}`. |
| `capsulad.stop()` | — | Requests the loop to stop after the current iteration. |
| `capsulad.on(kind, fn)` | `"invocation"`, `"settings_changed"`, `"action"`, `"activation"`, `"touch_released"`, `"call"`, `"disconnect"` | One handler per kind; `"call"` and `"disconnect"` are special. |
| `capsulad.call(service, method, request?, timeoutMs?)` | → decoded JSON | Unary JSON call. Default timeout 30000 ms; throws on error. |
| `capsulad.callBytes(service, method, bytes, timeoutMs?)` | → `Uint8Array` | Unary raw protobuf. |
| `capsulad.open(service, method, kind?, timeoutMs?)` | → `Call` | Starts a call; `kind` 0 unary, 1 server-stream (default), 2 client-stream, 3 bidi. |
| `capsulad.openRouted(appId, service, method, timeoutMs?)` | → `Call` | Routed call through the app bus. |
| `capsulad.stream(service, method, request?, timeoutMs?)` | → `Call` | `open` + send + half-close. |
| `capsulad.routed(appId, service, method, request?, timeoutMs?)` | → decoded JSON | One-shot routed unary. |
| `capsulad.routedStream(appId, service, method, request?, timeoutMs?)` | → `Call` | Routed stream with the request already sent. |
| `capsulad.notifyInvocation(id, accepted, detail?)` | — | Reports an invocation outcome. |
| `capsulad.notifyAction(id, succeeded, detail?)` | — | Reports an action outcome. |
| `capsulad.registerDescriptors(bytes)` | — | Enables the JSON path for your own messages. |
| `capsulad.info()`, `capsulad.handleInfo(handle)` | → object | Diagnostics. |
| `capsulad.state()`, `.ready()`, `.connected()`, `.disconnected()` | — | Bus state. |
| `capsulad.env` | object | `{socket, runId, appId, sdk, library, appDir}`. |
| `capsulad.log(message)` | — | Structured app log line. |
| `capsulad.WAIT_FOREVER` | `0xFFFFFFFF` | Pass as a receive timeout. |

`Call` objects expose:

| Member | Meaning |
|---|---|
| `send(value)` | Send one JSON message. |
| `sendBytes(bytes)` | Send one raw protobuf message. |
| `halfClose()` | Close the request direction. |
| `recv(timeoutMs?)` | Next JSON message, or `null` at end of stream. Default 30000 ms. |
| `recvBytes(timeoutMs?)` | Next protobuf message, or `null` at end of stream. |
| `end(code?, detail?)` | End a call you serve (default `0`). |
| `cancel()` | Cancel the call. |
| `status()` | `{code, detail}` of the terminal gRPC status. |
| `info` | Inbound calls only, set at construction: `{call_id, caller_run_id, grpc_service, rpc_name, deadline_monotonic_ns}`. The relay leaves `deadline_monotonic_ns` at `0`; rely on `cancel` instead. `null` for calls you opened. |
| `callInfo()` | `{kind, direction, service, method, target_app_id, routed, call_id, grpc_code, ended}` describing the call object itself. |

All keys on both objects are snake_case, exactly as the engine emits them.

Handlers receive the **inner message** as decoded JSON:
`{invocationId, arguments:[{name,value}]}`, `{revision, values:[{key,valueJson}]}`,
`{actionInvocationId, actionId}`, `{activationId, origin, matchedModelId, monotonicTimeNs,
expiresMonotonicNs, holdToTalk}`, `{activationId}`. The `"call"` handler receives a `Call`
whose `info` carries the inbound metadata above; it must call `end()` or `cancel()`.

An activation's `origin` is the only keyword evidence: a manual (touch/key) turn arrives as
`ACTIVATION_TOUCH` with an empty `matchedModelId` and carries `holdToTalk` when the device is in
hold-to-talk mode, while only `ACTIVATION_WAKEWORD` carries the id of the word that matched. Read
`origin` rather than treating a non-empty model id as proof a word was heard.

Error handling: `recv` returns `null` only at end of stream. A timeout or transport failure
throws a JS error carrying `.status`; catch it if you poll with a short timeout.

### Complete QuickJS app

`manifest.toml`:

```toml
id = "org.example.ringpaint"
name = "Ring Paint"
version = "1.0.0"
description = "Paints the LED ring and echoes a string over the app bus."
single_instance = true
restart_on_failure = true

[entrypoint]
runtime = "quickjs"
path = "main.js"

[[settings]]
key = "color"
label = "Colour"
type = "text"
default = "#00a0ff"

[[actions]]
id = "repaint"
label = "Repaint the ring"

[[arguments]]
name = "color"
type = "text"
required = false

[[interfaces]]
service = "org.example.ringpaint.v1.PaintService"
version = "v1"
```

`main.js`:

```js
"use strict";

var PRESENTATION = "capsulad.framework.v1.PresentationService";
var CONFIG = "capsulad.framework.v1.ApplicationConfigurationService";
var RING = "light.ring.main";

var color = [0, 160, 255];
var presentationId = null;
var call = null;

function log(message) {
  capsulad.log("ringpaint: " + message);
}

function parseHex(text) {
  var value = String(text || "").replace("#", "");
  if (value.length !== 6) return null;
  var rgb = [];
  for (var i = 0; i < 6; i += 2) {
    var part = parseInt(value.substring(i, i + 2), 16);
    if (isNaN(part)) return null;
    rgb.push(part);
  }
  return rgb;
}

function pixels(rgb, count) {
  var out = [];
  for (var i = 0; i < count; i++) {
    out.push({ red: rgb[0], green: rgb[1], blue: rgb[2] });
  }
  return out;
}

function paint() {
  if (call === null) return;
  try {
    call.send({ frame: { ring: { pixels: pixels(color, 12) } } });
  } catch (err) {
    log("frame failed: " + err.message);
  }
}

function open() {
  if (call !== null) return;
  call = capsulad.open(PRESENTATION, "Open", 3, 10000);
  call.send({ open: { resourceId: RING } });
  var event = call.recv(10000);
  if (event && event.ready) {
    presentationId = event.ready.presentationId;
    log("presentation " + presentationId + " ready");
    paint();
  }
}

function applySettings(settings) {
  var values = (settings && settings.values) || [];
  for (var i = 0; i < values.length; i++) {
    if (values[i].key !== "color") continue;
    var rgb = parseHex(JSON.parse(values[i].valueJson));
    if (rgb) color = rgb;
  }
}

capsulad.on("invocation", function (event) {
  var args = (event && event.arguments) || [];
  for (var i = 0; i < args.length; i++) {
    if (args[i].name === "color") {
      var rgb = parseHex(args[i].value);
      if (rgb) color = rgb;
    }
  }
  paint();
  capsulad.notifyInvocation(event && event.invocationId, true, "painted");
});

capsulad.on("settings_changed", function (event) {
  applySettings(event);
  paint();
});

capsulad.on("action", function (event) {
  paint();
  capsulad.notifyAction(event && event.actionInvocationId, true, "repainted");
});

capsulad.on("call", function (inbound) {
  var info = inbound.info || {};
  if (info.rpc_name !== "Echo") {
    inbound.end(12, "only Echo is implemented");
    return;
  }
  try {
    var request = inbound.recvBytes(5000);
    if (request === null) {
      inbound.end(0, "empty request");
      return;
    }
    inbound.sendBytes(request);   // echo the exact protobuf message back
    inbound.end(0, "ok");
  } catch (err) {
    inbound.end(13, String(err && err.message || err));
  }
});

capsulad.on("disconnect", function (event) {
  log("bus ended status=" + event.status + " detail=" + event.detail);
});

// --- startup ---------------------------------------------------------------
capsulad.start();
log("ready app=" + capsulad.env.appId + " run=" + capsulad.env.runId);

open();

try {
  var snapshot = capsulad.call(CONFIG, "Get", { appId: capsulad.env.appId }, 5000);
  applySettings({ values: snapshot.values });
  paint();
} catch (err) {
  log("settings read failed: " + err.message);
}

capsulad.run();
```

Package and install:

```sh
capsulad-cli app package . -out ringpaint.tar.gz
capsulad-cli -data-dir /data/capsdemo app install ringpaint.tar.gz
/etc/init.d/S99btctl.sh restart          # the registry is read at startup
capsulad-cli -data-dir /data/capsdemo app start org.example.ringpaint color=#22cc66
```

A script app fails to start when the QuickJS host is not installed on the target; check
`ApplicationService.ListRuntimes` or `capsulad status`. On a device the host lives in the
platform `lib/` directory.

## MicroPython host

`runtime = "micropython"`, entrypoint path ending in `.py`. The prelude is executed first and
publishes its surface on the injected `capsulad` module; your script uses `import capsulad`.

Limits and environment:

- 4 MiB heap, script file at most 8 MiB.
- No filesystem access: the embed port has no `open`, `json` or `io`. The prelude carries its
  own JSON encoder/decoder and `capsulad.call` uses it.
- No timers or threads. The process exits when `capsulad.run()` returns.

Structure:

```python
import capsulad

capsulad.on("invocation", handler)
capsulad.start()      # required before the script returns
# synchronous setup
capsulad.run()
```

The surface mirrors the QuickJS host with Python names:

| Member | Notes |
|---|---|
| `capsulad.start(timeout=10000)`, `run()`, `stop()` | bus lifecycle |
| `capsulad.on(kind, fn)` | same kinds as QuickJS, including `"call"` and `"disconnect"` |
| `capsulad.call(service, method, request=None, timeout=30000)` | unary JSON, returns a decoded object |
| `capsulad.call_bytes(service, method, request, timeout=30000)` | unary protobuf, returns `bytes` |
| `capsulad.open(service, method, kind=1, timeout=30000)` | `Call` object |
| `capsulad.open_routed(app_id, service, method, timeout=30000)` | routed `Call` |
| `capsulad.stream(...)`, `capsulad.routed(...)`, `capsulad.routed_stream(...)` | convenience wrappers |
| `capsulad.notify_invocation(id, accepted, detail="")` | acknowledgement |
| `capsulad.notify_action(id, succeeded, detail="")` | acknowledgement |
| `capsulad.register_descriptors(data)`, `capsulad.info()`, `capsulad.handle_info(h)`, `capsulad.state()`, `capsulad.log(message)` | as in QuickJS |
| `capsulad.env` | dict with `socket`, `runId`, `appId`, `sdk`, `library`, `appDir` |
| `capsulad.WAIT_FOREVER` | `0xFFFFFFFF` |

`Call` methods use snake_case: `send`, `send_bytes`, `half_close`, `recv`, `recv_bytes`, `end`,
`cancel`, `status`, `call_info`. The `info` attribute carries the same inbound metadata as
QuickJS. `recv` returns `None` at end of stream; other failures raise
`OSError`, so a timed read is catchable and a poll loop is safe.

```python
import capsulad

def on_invocation(event):
    capsulad.log("invocation " + str(event.get("invocationId")))
    capsulad.notify_invocation(event.get("invocationId"), True, "logged")

def on_call(call):
    info = call.info or {}
    if info.get("rpc_name") != "Ping":
        call.end(12, "only Ping is implemented")
        return
    request = call.recv_bytes(5000)
    call.send_bytes(request or b"")
    call.end(0, "ok")

capsulad.on("invocation", on_invocation)
capsulad.on("call", on_call)
capsulad.start()
capsulad.log("ready " + capsulad.env["appId"])
capsulad.run()
```

## Writing an assistant

An assistant is an app that claims the `voice_assistant` role and is selected as the device's
assistant. The framework delivers turns as `AssistantActivation` on the normal bus; there is no
separate channel and no activation-time launch. The app decides when to open the microphone,
and late capture loses audio. See [lifecycle.md](lifecycle.md#assistant-activations) and
[audio.md](audio.md).