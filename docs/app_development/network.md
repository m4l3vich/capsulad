# Networking: TCP, UDP, listeners and mDNS

An app has four network capabilities beyond the framework socket: outbound TCP with optional
verified TLS, UDP datagrams, daemon-allocated listening sockets, and mDNS publication and
browsing through the daemon's Avahi responder. All four are provided by `libcapsulad` behind the
frozen C ABI; an app never bundles its own TLS, HTTP or mDNS stack and never talks to the
network behind the SDK's back.

Two rules frame everything on this page:

- **An app never creates or binds a listening socket.** Inbound TCP exists only through a
  listener the daemon allocated; inbound UDP arrives either on a daemon-allocated UDP listener
  or, as replies, on the ephemeral socket `udp_open` created for you. `listener_open` asks the
  daemon, which allocates the bound socket, applies the policy and passes the listening
  descriptor over the inherited control channel (`CAPSULAD_CONTROL_FD`). The app serves the
  accepted connections and datagrams; it does not decide the bind, and a fixed, known inbound
  port is only ever a listener.
- **TLS is client-side only and always verified.** Outbound TLS requires a verified chain with
  TLS 1.2 or newer. There is no insecure mode, no plaintext downgrade and no TLS server
  termination in this contract.

## Availability and ABI

The capabilities are part of ABI **1.0** (`capsulad_abi_minor()` returns `0`, version `1.0.0`);
their entry points are appended to the function table after `http_cancel`, in the order below,
and the table stays append-only at ABI 1.0. C ABI negotiation, buffer ownership and the
one-logical-reader rule are in [sdk.md](sdk.md#the-frozen-c-abi). A native app that uses any of
the networking capabilities declares the major requirement:

```toml
[native_sdk]
abi_major = 1
min_minor = 0
```

- A binding built against the pre-networking prefix copies its shorter table and sees these
  pointers as absent; the library never reorders or removes a pointer.
- Every entry point returns a `capsulad_status`. Handles are `uint64_t` and `0` is never valid.
  Output buffers are library-owned `capsulad_buf` values freed with `buf_free`.
- Handles are process-local and thread-safe with **one logical reader**: one thread may be in
  `stream_read`, `listener_accept` or `listener_recv` for a given handle at a time. Writes are
  serialized inside the engine, so a second writer blocks rather than interleaving bytes.
- Closing a handle from another thread unblocks pending I/O with `CANCELLED`; using a released
  handle returns `NOT_FOUND` or `CLOSED`.
- Data-path timeouts are uniform: `0` polls once, `0xFFFFFFFF` waits indefinitely, any other
  value bounds the call in milliseconds. Opening calls differ — `tcp_open` and `udp_open` use a
  30 s default at `0`, `listener_open` uses its control-exchange default, and an mDNS call with
  `0` has no deadline. `TIMEOUT` leaves a stream, socket or listener usable.
- The QuickJS and MicroPython hosts expose no stream, UDP or listener functions. mDNS is
  reachable from a script through the normal framework call path; see
  [Script hosts](#script-hosts).

| Group | Entry points (append order) |
|---|---|
| TCP stream | `tcp_open`, `stream_read`, `stream_write`, `stream_shutdown`, `stream_close`, `stream_info` |
| UDP | `udp_open`, `udp_send`, `udp_recv`, `udp_close` |
| Listener | `listener_open`, `listener_accept`, `listener_recv`, `listener_send`, `listener_info`, `listener_close` |
| mDNS | `mdns_publish`, `mdns_unpublish`, `mdns_browse_open` |

## Outbound TCP with optional TLS

```c
int32_t tcp_open(const char *host, size_t host_len, uint16_t port,
                 const uint8_t *options_json, size_t options_len,
                 uint32_t timeout_ms, capsulad_handle *out_stream);
```

`tcp_open` performs DNS resolution, the TCP connect and, when `tls` is true, the TLS handshake
under **one deadline**. `host` is a DNS name or an IP literal of at most 255 bytes; `port` is
`1..65535`. Options are a snake_case JSON object; unknown keys are rejected, so a mistyped
option can never silently weaken verification.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `tls` | bool | `false` | Perform a verified TLS 1.2+ handshake. |
| `roots` | string | `"system"` | `"system"` = system PEM bundle + Mozilla roots + `ca_pem`; `"webpki"` = Mozilla roots + `ca_pem`; `"pem"` = `ca_pem` only (no system bundle, no public roots). |
| `ca_pem` | string | — | Extra PEM certificates appended to the trusted set; an empty string is ignored. |
| `server_name` | string | the host | SNI and the name verified in the certificate; must not be empty. |
| `nodelay` | bool | `true` | Set `TCP_NODELAY`. |

`timeout_ms` bounds DNS + TCP + TLS together; `0` selects the default of **30 s**. Failures are
`TRANSPORT` (DNS, connect, handshake, certificate verification), `TIMEOUT` (the deadline) and
`INVALID_ARGUMENT` (malformed host, port or options). There is no way to skip verification, and
`roots: "pem"` never falls back to a public root.

### Reading, writing and closing

| Call | Rule |
|---|---|
| `stream_read(stream, max_len, timeout_ms, &data)` | One read. `max_len` `0` means 64 KiB; the cap is **4 MiB** and a larger request is `TOO_LARGE`. A clean remote close returns `END_OF_STREAM` with an empty buffer — not `OK` with zero bytes. A timeout returns `TIMEOUT` and the stream stays usable. |
| `stream_write(stream, data, len, timeout_ms, &written)` | One write. `written` is `1..len`: **short writes are normal**, so loop until everything is sent. An empty write is rejected with `INVALID_ARGUMENT`; zero is never a successful result. |
| `stream_shutdown(stream, how)` | `CAPSULAD_SHUTDOWN_WRITE` (`0`) sends FIN or TLS `close_notify`; `CAPSULAD_SHUTDOWN_READ` (`1`) makes further reads return `END_OF_STREAM`; `CAPSULAD_SHUTDOWN_BOTH` (`2`) does both. Writing after `SHUTDOWN_WRITE` returns `CLOSED`. |
| `stream_close(stream)` | Releases the handle and unblocks a pending read or write with `CANCELLED`. Always close on every exit path. |
| `stream_info(stream, &json)` | Diagnostics: `kind`, `host`, `port`, `tls`, `server_name`, `peer`, `closed`, `read_shutdown`, `write_shutdown`. Treat it as diagnostics, not a stable schema beyond those fields. |

A stream is created by `tcp_open` or `listener_accept` and released by `stream_close`. The
framework does not clean up an app's streams for it while the process lives; process exit closes
the descriptors in the kernel.

Reads and writes follow the timeout convention above: `0` polls once, `0xFFFFFFFF` waits, and
anything else bounds that operation in milliseconds. A `TIMEOUT` is not a broken stream — a poll
loop can keep reading — while a `TRANSPORT` error means the connection is unusable.

## UDP

```c
int32_t udp_open(const char *host, size_t host_len, uint16_t port,
                 const uint8_t *options_json, size_t options_len,
                 uint32_t timeout_ms, capsulad_handle *out_udp);
```

`udp_open` binds `{bind, port}` (default `0.0.0.0:0`) and **connects** when both a non-empty
host and a non-zero port are given. A partial pair — a host with port `0`, or an empty host with
a port — is `INVALID_ARGUMENT`: both or neither. With neither, the socket is unconnected and
every send must name a peer.

A UDP socket is an **ephemeral, outbound-first** socket: its local port must be `0`. A fixed,
known inbound UDP port exists only through a daemon-allocated UDP listener
(`listener_open`), so an app cannot expose a UDP service with `udp_open` — it can send, receive
replies to its own requests, and receive datagrams sent to the ephemeral port it was handed.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `bind` | string | `"0.0.0.0"` | Local address to bind; must not be empty. |
| `port` | integer | `0` | Local port. Must be `0` (ephemeral); a non-zero value is `INVALID_ARGUMENT`. Use a UDP listener for a fixed port. |

`timeout_ms` bounds resolution, bind and connect; `0` selects 30 s.

| Call | Rule |
|---|---|
| `udp_send(udp, peer_json, data, len, timeout_ms, &sent)` | One datagram. `peer_json` is `{"address":"1.2.3.4","port":53}`: **`address` must be an IP literal** (a DNS name is `INVALID_ARGUMENT`) and both fields are required together. Empty `peer_json` uses the connected peer and fails with `NOT_CONNECTED` on an unconnected socket. A payload over **65507** bytes is `TOO_LARGE`. `sent` is the datagram size; a datagram is never partial. |
| `udp_recv(udp, max_len, timeout_ms, &data, &peer_json)` | One datagram, truncated to `max_len` (`0` means 65507; a larger request is `TOO_LARGE`), plus peer JSON `{"address","port"}`. A timeout leaves the socket usable. |
| `udp_close(udp)` | Releases the handle and unblocks a pending receive with `CANCELLED`. |

UDP is datagram-bounded: a short read loses the remainder of that datagram by design, and
datagrams can arrive out of order, duplicated or not at all. The SDK does not reassemble,
retransmit or order anything. It also does not filter destinations: a multicast or broadcast
destination is sent as given. What an app cannot do is *join* a group and receive multicast —
there is no group-join API — so inbound traffic is whatever is addressed to the ephemeral port.

## Daemon-mediated listeners

The daemon owns every listening socket. The supervisor creates one `AF_UNIX`
`SOCK_SEQPACKET` control channel per supervised run, passes the child end as inherited
descriptor 3 and records its number in `CAPSULAD_CONTROL_FD`. If the channel cannot be created
the run still starts — without the variable, one `WARN` in the daemon log — and every listener
call returns `UNSUPPORTED`. `listener_open` sends one JSON request over that channel; the daemon
validates the policy, binds, and returns the listening descriptor with `SCM_RIGHTS` in the same
reply message. The app then owns the descriptor.

```c
int32_t listener_open(const uint8_t *options_json, size_t options_len,
                      uint32_t timeout_ms, capsulad_handle *out_listener,
                      capsulad_buf *out_info);
```

| Key | Type | Default | Rule |
|---|---|---|---|
| `protocol` | string | `"tcp"` | `"tcp"` or `"udp"`. |
| `bind` | string | `"127.0.0.1"` | A local unicast address: loopback, an interface address, or the IPv4/IPv6 wildcard. Multicast and broadcast are refused. The SDK sends `127.0.0.1` when the option is omitted; pass `"0.0.0.0"` (or `"::"`) explicitly for a LAN-facing listener. |
| `port` | integer | `0` | `0` requests an ephemeral port; otherwise `1024..65535`. **5353** (mDNS/Avahi) and the configured Web UI port are refused. |
| `backlog` | integer | `64` | TCP only, `1..128`; ignored for UDP. |
| `join_group` | string | — | UDP only. IPv4 multicast group to join for a receive-only socket. The daemon accepts it only with `bind` `"0.0.0.0"` and `port` `319` or `320` (the PTP event and general ports), which is the AirPlay PTP clock's single use; see [Multicast receive for PTP](#multicast-receive-for-ptp). |

`out_info` receives the bound socket as JSON, for example
`{"protocol":"tcp","address":"127.0.0.1","port":54321}` — that is how an app learns the
ephemeral port it was given.

| Rule | Value |
|---|---|
| Listener policy | Only a supervised run can allocate a listener; the inherited control channel is the proof. |
| Concurrency | At most **16** listeners per run. |
| Refused ports | 5353, the configured Web UI port, and any port outside `0` or `1024..65535` — except a `join_group` request, which may use only the PTP ports 319/320. |
| Multicast | Refused for an ordinary listener. `join_group` is the only multicast path and it is receive-only on the PTP ports. |
| Cleanup | The app owns each transferred descriptor: run exit closes it in the kernel. The daemon closes an un-taken descriptor, the control channel and its own accounting; it cannot close a listener it already transferred. |
| Audit | The daemon logs every allocation with app, run, protocol, address and port. |
| Release | A successful reply carries an `allocation` id. The SDK sends one `close` control request for it when the listener is closed; the daemon then frees its retained descriptor and decrements the run's live-listener count. Closing is idempotent — an unknown or already-released allocation is a no-op — so every exit path can release without checking. The cap counts **live** listeners, so opening and closing listeners across sessions does not exhaust a run's budget. |

| Call | Rule |
|---|---|
| `listener_accept(listener, timeout_ms, &stream, &peer_json)` | TCP only. Returns a normal stream handle plus peer JSON `{"address","port"}`; the accepted stream follows the TCP rules above. A timeout leaves the listener usable. |
| `listener_recv(listener, max_len, timeout_ms, &data, &peer_json)` | UDP only. One datagram (max 65507) and its peer JSON. |
| `listener_send(listener, peer_json, data, len, timeout_ms)` | UDP only. `peer_json` is required: a listener has no connected peer. |
| `listener_info(listener, &json)` | Bound JSON plus `closed`. |
| `listener_close(listener)` | Releases the listener; pending accept, receive and send return `CANCELLED`. |

### Multicast receive for PTP

A multicast group receiver is a distinct, receive-only socket: the daemon binds the wildcard
address, joins the IPv4 group and transfers the descriptor exactly like any other listener. It
exists for one purpose — the IEEE 1588 clock an AirPlay 2 sender publishes on `224.0.0.1`
ports 319 (event) and 320 (general) — so the policy is deliberately narrow:

- `protocol` must be `"udp"`, `bind` must be `"0.0.0.0"` and `port` must be `319` or `320`.
  Any other combination is `INVALID_ARGUMENT` with the rule named.
- The group must be exactly `224.0.0.1`, the 802.1AS/AirPlay PTP group; any other multicast
  group is `INVALID_ARGUMENT`. The daemon binds the privileged port once (as root) with
  `SO_REUSEADDR` and best-effort `SO_REUSEPORT`, then joins the group on every interface that is
  up, multicast-capable and not loopback. Membership is socket state, so it ends when the
  transferred descriptor and the daemon's own handle are closed — i.e. on app exit and daemon
  shutdown — and the allocation is counted and logged like any other listener (the *16 listeners
  per run* cap applies).
- Receiving on a joined group is not a bind: a datagram socket bound to `0.0.0.0:320` alone
  does not see the sender's Sync/Follow_Up traffic, which is why the join is a daemon
  operation rather than something an app can fake.
- **Receive-only is an SDK contract, not a kernel boundary.** `listener_send` refuses a
  group listener, but the transferred descriptor is an ordinary UDP socket and an app that
  speaks the control protocol directly could send from it. The daemon's guarantee is the
  narrow group/port/interface policy and the authenticated control channel, not a
  per-syscall filter; treat native apps as trusted code, as the sandbox section says.
- There is no group-join API for an arbitrary port. If a future protocol needs multicast
  on another port, the policy is extended deliberately, not by widening this option.

```c
int32_t listener_open_json(const char *json, size_t len, uint32_t timeout_ms,
                           capsulad_handle *out, capsulad_buf *out_info);
/* json: {"protocol":"udp","bind":"0.0.0.0","port":320,"join_group":"224.0.0.1"} */
```

**`CAPSULAD_CONTROL_FD` is reserved.** It is an inherited, connected Unix descriptor owned by
the runtime; an app must never read, write, close, duplicate over or reuse that number. It
carries listener allocation only. A process that was not started by the supervisor (or a
runtime that did not provide the channel) gets `UNSUPPORTED` from every listener call — never a
silently created socket.

## mDNS publication and browsing

The daemon's Avahi responder owns multicast DNS: Avahi holds port 5353, and neither the app nor
the daemon binds it. `mdns_publish`, `mdns_unpublish` and `mdns_browse_open` act on a
**connected supervised client** and speak canonical protobuf JSON for
`capsulad.framework.v1.DiscoveryService`: lowerCamelCase field names, enum **names**, and
64-bit integers as strings. The service itself, including its access model, is documented in
[services.md](services.md#discoveryservice).

### Publishing

All three calls take a `timeout_ms`: `0` means no deadline, any other value bounds the call. A
publication is owned by the calling run: keep the returned id to unpublish it, and close or
unpublish on every exit path.

Request (`PublishServiceRequest`):

| Field | Type | Rule |
|---|---|---|
| `serviceType` | string | Required. `_name._tcp` or `_name._udp`; the name starts with `_` and contains only letters, digits and hyphens (at most 63 bytes with the underscore). A trailing domain suffix (`_name._tcp.local.`) is accepted and must agree with `domain`. |
| `instanceName` | string | Required display instance, at most **63** UTF-8 bytes, no NUL. |
| `port` | uint32 | Required, `1..65535`. |
| `txt` | map<string,string> | At most **16** records and **1024** bytes in total, and at most **255** bytes per `key=value` record; keys are non-empty and values may be empty; no NUL in either. The daemon rejects, rather than splits, an oversized item. |
| `domain` | string | Empty means `local`. Labels are lowercased; letters, digits and hyphens only. |

Response (`PublishServiceResponse`) carries `publicationId`, `serviceType`, `instanceName`,
`domain`, `host` and `port`. `host` is the responder's fully-qualified host name; an app cannot
choose it.

- **Publication lifetime.** A registration is scoped to the calling run. It ends on
  `mdns_unpublish`, on run exit (including a crash) and on daemon shutdown. The daemon also
  removes a run's registrations when the run's process is cleaned up, so a dead app cannot leave
  an advertisement behind.
- **`mdns_unpublish`** takes the publication id and removes that registration. Unpublishing an
  unknown id, or one owned by another run, returns `NOT_FOUND` — the daemon never reveals or
  removes another app's registration.
- **Collisions.** The daemon publishes with Avahi's no-rename policy. An instance name already
  registered on the link fails the call with `ALREADY_EXISTS`; the daemon never renames it
  behind the app's back, so choose another instance name, or withdraw the old publication and
  retry after it clears.
- **Exposure.** A publication is visible to the whole local link: service type, instance name,
  port, TXT records and the device's mDNS host name. Never put a credential, token or private
  identifier in a TXT record.
- **Availability.** Without a responder the daemon rejects the call with
  `FAILED_PRECONDITION`, which the SDK surfaces as `GRPC` (8) with the daemon's reason in the
  message. A publish is never reported as successful when it was not registered. The simulator
  has no Avahi responder, so mDNS is unsupported there.

### Browsing

`mdns_browse_open(client, request, timeout_ms, &call)` starts the server-streaming `Browse` call
and sends the request. Read events with `call_recv_json`; each is a `DiscoveredService`. The call
ends with `END_OF_STREAM`; finish by releasing it with `call_release` (or cancelling it).

Request (`BrowseServicesRequest`): `serviceType` (same rules as publishing) and `domain`
(empty means `local`).

Event (`DiscoveredService`):

| Field | Meaning |
|---|---|
| `kind` | `EVENT_KIND_ADDED` or `EVENT_KIND_REMOVED`. |
| `name` | Instance name. |
| `serviceType`, `domain` | The browsed service and its domain. |
| `host` | Target host name. |
| `addresses` | Resolved addresses. **One `EVENT_KIND_ADDED` per resolved address.** |
| `port` | Advertised port. |
| `txt` | TXT records. |

`EVENT_KIND_REMOVED` names the instance that disappeared and carries no address. One browse
stream keeps at most 64 queued events; when the consumer falls behind, the daemon **drops the
oldest** queued event and logs the drop. There is no backpressure and no replay, so treat a
browse stream as a convergent view rather than a reliable event log: if a gap matters, re-browse
after a reconnect or a quiet period. A closed or cancelled stream releases its browser and any
Avahi entry groups.

## Status codes

Local failures use the SDK status. An mDNS call that ended with a remote gRPC error maps
`INVALID_ARGUMENT` and `NOT_FOUND` onto the matching SDK status; every other gRPC status arrives
as `GRPC` (8) with the daemon's reason in the message (see [sdk.md](sdk.md#status-model)).

| Status | Value | When |
|---|---|---|
| `CAPSULAD_STATUS_OK` | 0 | Success. |
| `CAPSULAD_STATUS_INVALID_ARGUMENT` | 1 | Malformed options, host or port; invalid mDNS request; a listener policy refusal (reserved port, non-local bind, listener cap). An mDNS call rejected remotely with `INVALID_ARGUMENT` also lands here. |
| `CAPSULAD_STATUS_NOT_CONNECTED` | 3 | `udp_send` without a peer on an unconnected socket. |
| `CAPSULAD_STATUS_CLOSED` | 4 | The handle was released, or the write side was shut down. |
| `CAPSULAD_STATUS_TRANSPORT` | 7 | DNS, TCP, TLS, socket or protocol failure; a listener bind failure such as a port already in use. |
| `CAPSULAD_STATUS_GRPC` | 8 | An mDNS call ended with a non-OK gRPC status other than `INVALID_ARGUMENT`/`NOT_FOUND` — `FAILED_PRECONDITION` when there is no responder, `ALREADY_EXISTS` when the instance name is taken, `UNAVAILABLE` when the responder was lost. The message carries the reason. |
| `CAPSULAD_STATUS_END_OF_STREAM` | 9 | Clean stream end or the read side was shut down. |
| `CAPSULAD_STATUS_TIMEOUT` | 10 | The bound elapsed; the handle is still usable. |
| `CAPSULAD_STATUS_CANCELLED` | 11 | A close unblocked the operation, or the call was cancelled. |
| `CAPSULAD_STATUS_NOT_FOUND` | 12 | Unknown or released handle; an unknown publication id, or one owned by another run. |
| `CAPSULAD_STATUS_INTERNAL` | 14 | Engine invariant or an unexpected runtime failure. |
| `CAPSULAD_STATUS_TOO_LARGE` | 15 | A read or datagram request above the cap; an oversized mDNS record set is `INVALID_ARGUMENT`. |
| `CAPSULAD_STATUS_UNSUPPORTED` | 16 | The runtime has no usable `CAPSULAD_CONTROL_FD`, so listeners are unavailable. |

`mdns_publish` and `mdns_unpublish` release their internal call handle before returning, so
there is nothing to inspect afterwards: branch on the status and the message. `mdns_browse_open`
hands the call to you, and there `call_status` still reports the terminal gRPC code of the
stream.

A daemon policy refusal (reserved port, non-local bind, listener cap) fails the open with
`INVALID_ARGUMENT` and names the rule that refused it; a bind the kernel rejects (port already in
use, permission) is `TRANSPORT`. Neither is ever downgraded to a best-effort bind.

## Limits

| Limit | Value |
|---|---|
| TCP host | 1..255 bytes |
| TCP/UDP port argument | `tcp_open` 1..65535; `udp_open` both host and port, or neither |
| Stream read | `max_len` 0 = 64 KiB; hard cap 4 MiB |
| UDP socket | ephemeral local port only; a fixed inbound port is a UDP listener |
| UDP datagram | 65507 bytes |
| Listener protocol | `tcp` or `udp` |
| Listener bind | local unicast only (wildcard, loopback, interface address) |
| Listener port | `0` (ephemeral) or `1024..65535`; 5353 and the Web UI port refused |
| Listener backlog | 1..128, default 64 |
| Multicast group receiver | UDP only, PTP ports 319/320, IPv4 multicast group, wildcard bind, receive-only |
| Listeners per run | 16 |
| mDNS instance | ≤63 UTF-8 bytes |
| mDNS TXT | ≤16 records, ≤1024 bytes total, ≤255 bytes per `key=value` record |
| mDNS browse queue | 64 events per stream; overflow drops the oldest and logs it |
| Control channel | one per run when the supervisor could create it; number in `CAPSULAD_CONTROL_FD` |

## Security and exposure

- **Egress.** Outbound TCP and UDP are available to every supervised app; there is no
  per-destination allowlist. The framework plane and the device's own services stay on the local
  Unix socket. Do not proxy another app's traffic, and do not treat outbound access as a
  substitute for the framework's identity checks.
- **TLS.** Always verified, TLS 1.2 or newer, with the configured roots; `roots: "pem"` trusts
  only the certificates you pass. A hostname mismatch or an untrusted chain is `TRANSPORT`; there
  is no override. Do not ship a private CA bundle in the package when the system roots suffice.
- **Inbound.** TCP arrives only through a daemon-allocated listener; UDP arrives on a UDP
  listener, or as replies to the ephemeral socket `udp_open` created — that socket cannot expose
  a service on a fixed port. The daemon decides address, port and backlog and refuses 5353 and
  the Web UI port, so an app cannot shadow the responder or the administrator UI.
- **Egress filtering.** Destinations are not filtered: the SDK will send a datagram to a
  multicast or broadcast address if you ask. That is not a way to publish a service — only the
  daemon's responder does that — but it is a reason to keep egress under your own control.
- **Exposure.** A listener on `0.0.0.0` or an interface address is reachable from the LAN, not
  just the device. Prefer loopback or a specific interface address, and treat every accepted
  connection and datagram as untrusted input: bound reads, validate framing and never allocate
  from an attacker-controlled length.
- **mDNS.** Publications are link-visible and removed when the run ends; browse results are
  untrusted network input. Do not put secrets in TXT, and do not publish a port you are not
  actually serving.
- **Reserved descriptor.** `CAPSULAD_CONTROL_FD` is runtime-owned. Reading, writing, closing or
  reusing it is a contract violation, and a forged allocation request cannot succeed without a
  supervised run's channel.

## What is not available

- **No app-created listening socket.** There is no API that binds a listening socket in the app
  process, and doing it directly is a contract violation even where the sandbox would permit it.
- **No fixed local UDP port.** `udp_open` binds an ephemeral port only; a service that must be
  reachable on a known UDP port uses a UDP listener.
- **No TLS server.** The library terminates TLS as a client only. An app that needs an encrypted
  inbound protocol must implement its own handshake over an accepted stream — and, if it does,
  it owns that protocol's verification rules.
- **No DTLS or other datagram security.** UDP is plain datagrams, including on a listener.
- **No app-owned mDNS responder.** Apps cannot bind 5353, join the mDNS group and receive
  queries, or advertise anything themselves; `mdns_publish` is the only publication path.
  Sending a raw datagram to a multicast address is not filtered, but it reaches no responder and
  is not a publication.
- **No HTTP server, WebSocket server or framework proxy.** The Web UI is administrator-only and
  has no generic proxy to an app. The sole app-specific HTTP operation is the administrator-facing
  PA Speaker play relay described in
  [apps/pa/docs/speaker.md](../../apps/pa/docs/speaker.md#http-api); it is not a proxy an app can
  extend.
- **No raw sockets, packet capture, multicast group membership, broadcast enable or arbitrary
  bind.** Listener binds are restricted to local unicast addresses.
- **No network access from the script hosts** beyond framework calls; mDNS is the only network
  capability a QuickJS or MicroPython app can reach, and only through `DiscoveryService`.

## Examples

### C: TCP with verified TLS

```c
capsulad_handle stream = 0;
const char *opts = "{\"tls\":true,\"roots\":\"system\",\"server_name\":\"api.example.com\"}";
int32_t st = api->tcp_open("api.example.com", strlen("api.example.com"), 443,
                           (const uint8_t *)opts, strlen(opts), 10000, &stream);
if (st != CAPSULAD_STATUS_OK) { /* last_error(): TRANSPORT/TIMEOUT/INVALID_ARGUMENT */ }

const char *request = "GET /v1/state HTTP/1.1\r\nHost: api.example.com\r\n"
                      "Connection: close\r\n\r\n";
size_t off = 0, n = 0;
while (off < strlen(request)) {
  st = api->stream_write(stream, (const uint8_t *)request + off,
                         strlen(request) - off, 5000, &n);
  if (st != CAPSULAD_STATUS_OK) break;   /* n is 1..len; loop for short writes */
  off += n;
}
api->stream_shutdown(stream, CAPSULAD_SHUTDOWN_WRITE);

capsulad_buf chunk = {0};
while (st == CAPSULAD_STATUS_OK &&
       (st = api->stream_read(stream, 0, 5000, &chunk)) == CAPSULAD_STATUS_OK) {
  fwrite(chunk.data, 1, chunk.len, stdout);
  api->buf_free(chunk);
}
/* st is END_OF_STREAM on a clean close, TIMEOUT if you bound the wait. */
api->stream_close(stream);
```

### C: a daemon-allocated listener

```c
capsulad_handle listener = 0;
capsulad_buf info = {0};
const char *opts = "{\"protocol\":\"tcp\",\"bind\":\"127.0.0.1\",\"port\":0,\"backlog\":64}";
int32_t st = api->listener_open((const uint8_t *)opts, strlen(opts), 10000,
                                &listener, &info);
if (st == CAPSULAD_STATUS_UNSUPPORTED) { /* no control channel in this run */ }
if (st == CAPSULAD_STATUS_OK) {
  /* {"protocol":"tcp","address":"127.0.0.1","port":54321} */
  api->buf_free(info);
}

for (;;) {
  capsulad_handle conn = 0;
  capsulad_buf peer = {0};
  st = api->listener_accept(listener, 1000, &conn, &peer);
  if (st == CAPSULAD_STATUS_TIMEOUT) continue;   /* still usable */
  if (st != CAPSULAD_STATUS_OK) break;           /* CANCELLED on close */
  api->buf_free(peer);
  /* serve conn as an ordinary stream; close it when the exchange ends */
  api->stream_close(conn);
}
api->listener_close(listener);
```

### C: publish a service

```c
/* client is the connected supervised client from client_connect. */
const char *req = "{\"serviceType\":\"_example._tcp\",\"instanceName\":\"Capsula Demo\","
                  "\"port\":8080,\"txt\":{\"path\":\"/status\"},\"domain\":\"local\"}";
capsulad_buf out = {0};
int32_t st = api->mdns_publish(client, (const uint8_t *)req, strlen(req), 10000, &out);
if (st == CAPSULAD_STATUS_OK) {
  /* out holds {"publicationId":"...","serviceType":"_example._tcp",...}; read the
   * publicationId and keep it for the unpublish below. */
  api->buf_free(out);
}
/* later, on every exit path (publication_id comes from the reply above): */
api->mdns_unpublish(client, publication_id, strlen(publication_id), 5000);
```

### Go

```go
// Outbound TCP with verified TLS. Close it on every exit path.
// Only ctx's deadline bounds the dial; with no deadline the native 30 s default applies.
stream, err := capsulad.OpenTCP(ctx, env, "api.example.com", 443, capsulad.TCPOptions{
    TLS: true, Roots: "system", ServerName: "api.example.com",
})
if err != nil { return err }
defer stream.Close()

if _, err := stream.Write(ctx, request); err != nil { return err } // may be short: loop
if err := stream.Shutdown(capsulad.ShutdownWrite); err != nil { return err }
buf := make([]byte, 64<<10)
for {
    n, err := stream.Read(ctx, buf)
    if errors.Is(err, io.EOF) { break } // clean remote close
    if err != nil { return err }
    out.Write(buf[:n])
}
```

```go
// A daemon-allocated listener: the app never binds the socket.
listener, err := capsulad.OpenListener(ctx, env, capsulad.ListenerOptions{
    Protocol: "tcp", Port: 0, // 0 = ephemeral
})
if err != nil { return err } // a StatusError with native code 16 means UNSUPPORTED
defer listener.Close()

info, err := listener.Info() // info.Port is the port the daemon actually bound
if err != nil { return err }
_ = info
for {
    conn, peer, err := listener.Accept(ctx)
    if err != nil {
        var st *capsulad.StatusError
        if errors.As(err, &st) && st.NativeStatus && st.Code == 10 { // TIMEOUT
            continue // the listener is still usable
        }
        return err // CANCELLED after Close
    }
    go func(conn *capsulad.TCPStream, peer *capsulad.UDPAddr) {
        defer conn.Close()
        _ = peer
        // serve conn; every Read returns io.EOF at a clean end
    }(conn, peer)
}
```

```go
// Unconnected UDP: every send names a peer. UDPAddr.Address must be an IP
// literal — peer JSON is not resolved — and the local port is ephemeral.
sock, err := capsulad.OpenUDP(ctx, env, "", 0, capsulad.UDPOptions{})
if err != nil { return err }
defer sock.Close()

if _, err := sock.Send(ctx, &capsulad.UDPAddr{Address: "1.1.1.1", Port: 53}, query); err != nil {
    return err
}
data, peer, err := sock.Receive(ctx, 0) // 0 = native 65507-byte maximum
```

```go
publication, err := client.PublishMDNS(ctx, capsulad.MDNSService{
    ServiceType:  "_example._tcp",
    InstanceName: "Capsula Demo",
    Port:         8080,
    TXT:          map[string]string{"path": "/status"},
}, 10*time.Second)
if err != nil { return err }
defer client.UnpublishMDNS(ctx, publication.PublicationID, 5*time.Second)

browse, err := client.BrowseMDNS(ctx, "_example._tcp", "")
if err != nil { return err }
defer browse.Close()
for {
    event, err := browse.Next(ctx)
    if errors.Is(err, io.EOF) { break }
    if err != nil { return err }
    // event.Kind, event.Name, event.Addresses, event.Port, event.TXT
}
```

### Rust

```rust
let stream = capsulad::Stream::open(&library, "api.example.com", 443,
    &capsulad::TcpOptions { tls: true, ..Default::default() },
    Some(std::time::Duration::from_secs(10)))?;
stream.write(request, Some(std::time::Duration::from_secs(5)))?;
stream.shutdown(capsulad::Shutdown::Write)?;
while let Some(chunk) = stream.read(0, Some(std::time::Duration::from_secs(5)))? {
    out.extend_from_slice(&chunk); // None is a clean end of stream
}
```

```rust
let listener = capsulad::Listener::open(&library,
    &capsulad::ListenerOptions { protocol: Some("tcp".into()), port: Some(0),
                                 ..Default::default() }, None)?;
let info = listener.info()?;            // the port the daemon bound
let (conn, peer) = listener.accept(Some(std::time::Duration::from_secs(1)))?;
```

```rust
let socket = capsulad::UdpSocket::open(&library, "", 0,
    &capsulad::UdpOptions::default(), None)?; // ephemeral local port only
let peer = capsulad::PeerAddr::new("1.1.1.1", 53); // address must be an IP literal
socket.send(Some(&peer), query, Some(std::time::Duration::from_secs(2)))?;
let (data, from) = socket.recv(0, Some(std::time::Duration::from_secs(2)))?;
```

```rust
let publication = client.publish_mdns(&capsulad::MdnsService {
    service_type: "_example._tcp".into(),
    instance_name: "Capsula Demo".into(),
    port: 8080,
    ..Default::default()
}, None)?;
let browse = client.browse_mdns("_example._tcp", None, None)?;
while let Some(event) = browse.next(Some(std::time::Duration::from_secs(2)))? {
    // event.kind, event.name, event.addresses, event.port, event.txt
}
```

Both bindings load the shared library and fail like `Connect` — `LibraryError` or `ABIError` —
and a closed handle maps to `ErrClosed`. A `*capsulad.StatusError` (Go) or `Error` (Rust)
carries the native status, so `UNSUPPORTED` (16) and `TIMEOUT` (10) are distinguishable from a
gRPC code. See [sdk.md](sdk.md#go-binding) and [sdk.md](sdk.md#rust-binding).

### Script hosts

Both script hosts reach mDNS through the framework call surface, never through the stream, UDP
or listener entry points:

```js
// QuickJS: unary Publish, then a browse stream.
var published = capsulad.call("capsulad.framework.v1.DiscoveryService", "Publish", {
  serviceType: "_example._tcp", instanceName: "Capsula Demo", port: 8080
}, 10000);

var events = capsulad.stream("capsulad.framework.v1.DiscoveryService", "Browse",
  { serviceType: "_example._tcp" }, 10000);
for (var event = events.recv(5000); event !== null; event = events.recv(5000)) {
  capsulad.log(event.kind + " " + event.name);
}
```

There is no script binding for `tcp_open`, `udp_open` or `listener_open`; an app that needs them
is a native app.

## See also

- [sdk.md](sdk.md) — the ABI, handles, status model, deadlines and the language bindings.
- [services.md](services.md#discoveryservice) — `DiscoveryService` fields, access and errors.
- [security.md](security.md) — sandbox, egress and exposure rules.
- [troubleshooting.md](troubleshooting.md) — dial, TLS, DNS, mDNS and listener failures.
- [VERIFICATION.md](../VERIFICATION.md) — host evidence and the open device gates for this
  surface.
