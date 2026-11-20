# The app bus

The app bus is the app's reverse-RPC connection to the framework. It is one long-lived
`AppBusService.Connect` stream per app run, on the same local Unix socket every other framework
call uses. It carries:

- framework-to-app lifecycle events (invocations, settings changes, actions, assistant
  activations);
- framework-to-app custom calls, which other local callers reach through `AppBusService.Call`;
- app-to-framework replies, notifications and call results.

There is no per-app socket and no app-to-app direct transport. A routed call never opens a
connection to the target; the daemon relays frames over the target's existing `Connect` stream.
The bus is also not an inbound network endpoint: an app that needs one asks the daemon for a
listener and serves it directly, outside this stream (see
[network.md](network.md#daemon-mediated-listeners)).

```text
caller ──Call stream──► capsulad ──AppInbound──► target app's Connect stream
   ▲                       │                          │
   └──RoutedCallResponse───┘◄────AppCallReply─────────┘
```

## Connect: handshake and identity

1. The app opens `Connect` and sends `AppHello{run_id}` as the **first** message.
2. The daemon resolves `run_id` to a supervised run and compares the resulting PID with the
   Unix-socket peer credentials (`SO_PEERCRED`). A mismatch is `PERMISSION_DENIED`.
3. An unknown `run_id` is `PERMISSION_DENIED`. A `Connect` that claims a compiled-in system
   app's run is refused.
4. The app sends `AppReady`. Only then is the run marked READY, its declared interfaces become
   `available`, and queued invocations and the initial settings snapshot are delivered.

Rules:

- The first message must be `AppHello`; anything else is `INVALID_ARGUMENT`.
- A second `Connect` for the same run replaces the first; the older stream is closed. An app
  therefore has exactly one live bus.
- A connection that ends is treated as a **failure**, not a stop: the supervisor stops the
  orphaned process and applies the restart policy. A clean exit requires closing the app
  normally without losing the bus, or being stopped deliberately.
- `AppReady` is idempotent; a duplicate `AppHello` on an established connection is ignored with
  a warning.

## Messages app → daemon (`AppOutbound`)

| Message | Fields | Purpose |
|---|---|---|
| `AppHello` | `run_id` (string) | First message; verified against the supervised process. |
| `AppReady` | — | Second message; marks the run ready. |
| `AppCallReply` | `call_id` (string) + one of `response_message` (bytes) or `end` (`AppCallEnd`) | One reply, or the terminal frame, for an inbound custom call. |
| `AppInvocationResult` | `invocation_id` (string), `accepted` (bool), `detail` (string) | Optional outcome of a launch invocation. |
| `AppActionResult` | `action_invocation_id` (string), `succeeded` (bool), `detail` (string) | Optional outcome of a triggered action. |

`AppCallEnd` carries `grpc_status_code` (int32, `0` = OK, otherwise the canonical gRPC code)
and `detail` (string). A reply stream is: zero or more `response_message` frames, then exactly
one `end`.

## Messages daemon → app (`AppInbound`)

| Message | Fields | Purpose |
|---|---|---|
| `IncomingAppCall` | `call_id`, `caller_run_id`, `grpc_service`, `rpc_name`, `deadline_monotonic_ns` (uint64) | A routed custom call. Serve it, reply, and always end it. |
| `IncomingCallData` | `call_id`, `request_message` (bytes) | One serialized protobuf request message; repeats for client streaming. |
| `IncomingCallHalfClose` | `call_id` | No more request data. |
| `IncomingCallCancel` | `call_id` | Caller cancelled, its deadline expired, or the caller stream closed. |
| `AppLaunchInvocation` | `invocation_id`, `arguments` (`[{name, value}]`) | Launch invocation with typed arguments; a launch of a running app arrives on its existing run. |
| `AppSettingsChanged` | `revision` (uint64), `values` (`[{key, value_json}]`) | Own settings only; carries unmasked `text_masked` values. |
| `AppActionInvoked` | `action_invocation_id`, `action_id` | A declared action was triggered. |
| `AssistantActivation` | `activation_id`, `origin` (enum), `matched_model_id`, `monotonic_time_ns`, `expires_monotonic_ns`, `hold_to_talk` (bool) | One assistant turn for the selected assistant. A manual (touch/key) activation is `ACTIVATION_TOUCH` with an empty `matched_model_id`; only `ACTIVATION_WAKEWORD` is keyword evidence. |
| `AssistantInputReleased` | `activation_id` | Hold-to-talk release correlated with that activation. |

`deadline_monotonic_ns` is reserved for a per-call deadline; the relay does not populate it, so
read it as `0` and rely on `IncomingCallCancel` and your own timeouts.

Each `call_id` is scoped to one `Connect` stream. Frames for the same call are ordered; frames
for different calls may interleave.

## Notifications

`AppInvocationResult` and `AppActionResult` are **notifications**, not RPCs: the framework
records them and logs them, and no error comes back if the app never sends one. Send them when
the outcome matters to the caller or the operator:

- an invocation is acknowledged so a launch shows `accepted=true` (or the app explains a
  refusal);
- an action result shows whether the button had an effect.

They are delivered on the same stream, in order, after the event that caused them.

## Routed calls (`Call`)

`AppBusService.Call` is a bidirectional stream:

```text
caller → daemon:  RoutedCallStart{target_app_id, grpc_service, rpc_name, timeout_ms}
caller → daemon:  request_message*            (bytes)
caller → daemon:  half_close
daemon → caller:  response_message*           (bytes)
daemon → caller:  end{grpc_status_code, detail}
```

Rules and limits:

- `RoutedCallStart` must be the only start message and the first one. A missing target, service
  or RPC is `INVALID_ARGUMENT`.
- The target must be a **ready** supervised app or a reachable built-in target. A target that is
  not ready ends immediately with `UNAVAILABLE` and the detail `target app is not ready`.
- `timeout_ms` 0 or absent selects the default deadline of **30 s**; larger values are capped at
  **5 minutes**. On expiry the caller receives `DEADLINE_EXCEEDED` and the target receives
  `IncomingCallCancel`.
- The daemon relays frames in both directions; it does not interpret payloads. What the target
  serves is entirely the target's decision, including unknown methods.
- A target disconnect fails in-flight calls with `UNAVAILABLE`; calls are never replayed to a new
  process.
- The caller's identity is derived from the socket peer and the optional `capsulad-run-id`
  metadata. A claim that contradicts peer credentials is `PERMISSION_DENIED`. The verified
  result is delivered to the target as `IncomingAppCall.caller_run_id`: a supervised run's id
  when the claim matched that run's process, `pid:<pid>` for a caller with no supervised run
  (the CLI, the Web UI), or empty when neither is available.
- The daemon authorizes which services are reachable, not who may invoke a method: an inbound
  call is a **local request, not an authenticated one**, and a handler must never treat a field
  inside the request as the caller's identity. A handler that serves a privileged local
  interface identifies the caller from `caller_run_id` and must match it against daemon-served
  data — for example `AppInterfaceService.ListInterfaces`, whose `run_id`/`app_id` rows come
  from the supervisor — before acting. Refuse with `PERMISSION_DENIED` when the caller is not
  the app the interface is for; a caller that appears as `pid:<pid>` or empty has no app
  identity and cannot be authorized this way. See
  [security.md](security.md#routed-calls-and-trust).
- Built-in system apps answer in-process through an internal handler. Their wire shape is
  identical: one request frame, optional half-close, then responses and `end`. Bidirectional
  request streaming to a system app is `UNIMPLEMENTED`.

Response ordering: responses queued before the target's end frame are always flushed first, so
a streaming reply is never reordered or dropped by the end.

### Buffering

Bounded on every hop:

| Budget | Value | Overflow |
|---|---|---|
| Inbound control queue per connection | 128 messages | enqueue fails; the routed call ends `UNAVAILABLE` |
| Queued responses per routed call | 32 frames | the relay fails the call; the app sees a cancelled call |
| Inbound call dispatch queue (library) | 32 | the call is ended with `RESOURCE_EXHAUSTED` |
| Live inbound calls (library) | 64 | `RESOURCE_EXHAUSTED` |
| Outbound frame queue (library) | 256 | `BUSY` from the sending call |

Slow consumer, huge reply and fan-in storms therefore surface as explicit codes, not as
unbounded memory.

## Encoding on the wire

- `IncomingCallData.request_message` and `AppCallReply.response_message` are **serialized
  protobuf messages**. JSON never travels on this path.
- The SDK converts at the edges: `send_json`/`recv_json`, `call.send(obj)` in scripts,
  `CallJSON` in Go. The JSON conversion needs a registered or embedded descriptor; a JSON call
  to an unregistered method fails with `DESCRIPTOR_NOT_FOUND` rather than guessing.
- Raw protobuf works without any descriptor, including when the target's custom descriptor is
  not registered locally. This is why a routed call always has a working path.
- Canonical protobuf JSON means lowerCamelCase field names, enum names, base64 `bytes` and
  64-bit integers encoded as strings.

## Publishing an interface

An app that wants to serve custom methods declares them in its manifest:

```toml
[[interfaces]]
service = "org.example.weather.v1.WeatherService"
version = "v1"
descriptor = "descriptors/weather.protoset"   # optional
docs = "docs/weather.md"                # optional
```

and registers a handler for each method (see [sdk.md](sdk.md)). Discovery is served by
`AppInterfaceService`:

```text
ListInterfaces{app_id?, running_only?}
  → [{app_id, run_id, grpc_service, interface_version, available, summary}]

DescribeInterface{app_id, grpc_service}
  → {info, file_descriptor_set, documentation_markdown,
     methods:[{rpc_name, summary, semantics_markdown}]}
```

- `available` is true only while the app's selected run has sent `AppReady`; `run_id` is empty
  otherwise.
- The descriptor is a serialized `FileDescriptorSet` including its imports. Without one, the
  interface is a name and a version only; raw protobuf callers still work.
- An interface is a name and a schema, not an endpoint. Callers always go through
  `AppBusService.Call`.

## Error codes

The canonical gRPC codes an app sees or sends on this path:

| Code | Value | Typical cause |
|---|---|---|
| `OK` | 0 | Success. |
| `INVALID_ARGUMENT` | 3 | Malformed start frame or message. |
| `DEADLINE_EXCEEDED` | 4 | Routed deadline elapsed. |
| `NOT_FOUND` | 5 | Unknown app, service or method (target's decision). |
| `PERMISSION_DENIED` | 7 | Identity mismatch, or an administrator-only operation attempted by an app. |
| `RESOURCE_EXHAUSTED` | 8 | A bounded queue is full. |
| `FAILED_PRECONDITION` | 9 | The app or target is in the wrong state (for example a stopped run). |
| `ABORTED` | 10 | A compare-and-swap revision was stale. |
| `UNIMPLEMENTED` | 12 | The target does not implement the method. |
| `INTERNAL` | 13 | The handler returned without ending the call, or an internal failure. |
| `UNAVAILABLE` | 14 | Target not ready, target disconnected, or daemon shutting down. |
| `CANCELLED` | 1 | Caller cancelled or the caller stream closed. |

A handler that returns without an explicit end makes the daemon observe `INTERNAL`; that is a
contract violation, not a transport failure.

## What is not available

- No browser path: the Web UI has no generic AppBus proxy. A page cannot act as a routed-call
  client.
- No cross-device path: the bus is local to one daemon.
- No broadcast: every call names one target app.
- No app authentication tokens: identity is the connection itself, verified from the operating
  system's peer credentials.