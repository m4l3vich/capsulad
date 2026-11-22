# MCP frontend

`capsulad` runs a **Model Context Protocol (MCP) server** as a second frontend of the
daemon. It is not an application: it has no app id, no manifest, no package and no
sandbox. It lives inside the daemon and is served by the same HTTPS listener as the Web
UI, at `https://<host>:8443/mcp`.

The MCP frontend exposes **allowlisted operations from the Web UI**. The `operator` scope
gets administrative tools, never browser-session management. The `assistant` scope gets
only [speaker controls](#tools) and reduced snapshots; `status` gets two reads. MCP does
not create a second control plane and has no private endpoints: every tool either dispatches one
typed `capsulad.web.v1.Command` through the daemon's allowlisted operation layer, or
performs one administrative file/read operation the browser performs over its own
routes. The shared boundary is documented in the source as `opSink`
(`internal/webui/ops.go`): the WebSocket frontend encodes the outcome as protobuf
frames and the MCP frontend collects it into a tool-call reply, but both drive the same
`dispatch` and operation validation, with separate scope checks before dispatch.

Related reading: [ARCHITECTURE.md](ARCHITECTURE.md) for the system architecture,
[docs/DEVELOPMENT.md](DEVELOPMENT.md) for builds and fixtures,
[docs/VERIFICATION.md](VERIFICATION.md) for proof levels.

## Enablement and key provisioning

There is no separate MCP listener, port, flag or process. The endpoint exists exactly
when the Web UI listener exists, which requires initialized credentials:

```sh
capsulad-cli -data-dir /var/lib/capsulad auth init -hostname capsula.local -ip 192.168.1.20
```

API keys are scoped. **A key minted without a scope argument records an explicit `status`
grant.** This does not grant speaker controls or administration. A credential record
without explicit scopes does not authenticate; an administrator must issue a new key.

```sh
# Read-only key: status + certificate only (this is also the default).
capsulad-cli auth key create probe --scope status

# Assistant key: speaker controls without administration.
capsulad-cli auth key create agent --scope assistant

# Operator key: the full MCP tool set, including administration.
capsulad-cli auth key create admin-automation --scope operator

capsulad-cli auth key list      # shows each key's scopes and revocation state
capsulad-cli auth key revoke agent
```

The plaintext key is printed once; only its SHA-256 hash is persisted. An operator
client can also mint keys with the `create_api_key` tool:

```json
{"name": "agent", "scopes": ["assistant"]}
```

That is deliberate delegation, not a default: only an `operator` key may call
`create_api_key`, and the `assistant` and `operator` grants must be named explicitly.
The browser Access page keeps minting status-only keys.

Key edits are picked up live: the daemon watches `credentials.json`, so a revocation or
a new key applies without a restart. Revocation is effective for every frontend
immediately.

Scopes:

| Scope | Grants |
|---|---|
| `status` | `GET /api/status`, `GET /api/cert.pem`, and the MCP tools `get_status` and `get_certificate`. Default for new keys. |
| `assistant` | Everything in `status`, plus a bounded set of MCP speaker controls and state reads. No administrative HTTP routes or browser-session privileges. |
| `operator` | Every MCP tool and full MCP snapshots, plus the operator-only PA Speaker HTTP relay. An API key still cannot obtain a browser session or change the administrator password. |

## Protocol

The transport is the MCP **Streamable HTTP** binding over the existing TLS listener.

| Property | Behaviour |
|---|---|
| Endpoint | `POST /mcp`, `DELETE /mcp`; `GET /mcp` answers `405 Method Not Allowed` because the server offers no server-initiated message stream (the binding allows this). |
| Content type | `POST` requires `application/json`; anything else is `415`. |
| Accept | `application/json`, `text/event-stream` or `*/*`. A client that accepts only `text/event-stream` receives one `event: message` carrying the same JSON-RPC response; everything else receives JSON. |
| Body limit | 16 MiB per request. A staged upload sends at most 1 MiB of decoded data per `upload_chunk`, so no single request buffers a large file. |
| Sessions | `initialize` returns an `Mcp-Session-Id` header; every later request must echo it. A session is bound to the key that created it, so another valid key can neither use it nor delete it. A missing header is `400`, an unknown, expired or foreign session is `404`; both carry a JSON-RPC error (`-32600`). `DELETE` with the header ends the session (`204`); an unknown session is `404`. |
| Notifications | A JSON-RPC message without an `id` (for example `notifications/initialized`) is acknowledged with an empty `202 Accepted`; an unknown notification method is ignored. `notifications/cancelled` stops the tool call whose JSON-RPC `requestId` it names, even when the notification arrives on another HTTP request. |
| Protocol version header | `MCP-Protocol-Version` is validated on every POST: an unsupported value is `400`. When it is absent the session's negotiated revision applies. |
| Batching | Version-dependent, because `2025-06-18` removed JSON-RPC batching: for a `2025-03-26` session a JSON array is accepted and answered with an array of request replies (a notifications-only batch is `202`); for a `2025-06-18` session an array is `400`. |
| Versions | `2025-06-18` (implemented) and `2025-03-26` (same Streamable HTTP transport, batching still part of the revision) are accepted and echoed. `2024-11-05` is deliberately not offered: that revision uses the older HTTP+SSE transport, which this server does not host. An unknown revision is answered with `2025-06-18` and the client decides whether to proceed. |
| Initialization | `initialize` is the first interaction; every other method except `ping` needs the session it returned. A repeated `initialize` that carries a live session id reuses that session instead of minting another. An unsolicited JSON-RPC *response* is accepted with `202` (this server sends no requests). |
| Capabilities | `tools` (`listChanged: false`). No resources, prompts or sampling. |
| Methods | `initialize`, `ping`, `tools/list`, `tools/call`, `notifications/*`. Anything else is `-32601`. |
| Deadlines | A POST raises both the listener's read deadline and its response write deadline for that request (up to 5 minutes), so a slow one-call upload body is delivered and answered instead of cut off. Each underlying device operation keeps its own daemon timeout. |

JSON-RPC errors:

| Code | Meaning |
|---|---|
| `-32700` | Malformed JSON body (`400`). |
| `-32600` | Invalid request, missing/unknown/foreign session (with HTTP `400` or `404`). |
| `-32601` | Unknown method. |
| `-32602` | Invalid params, unknown tool, malformed tool arguments. |
| `-32603` | Internal encoding/daemon failure. |
| `-32001` | Missing, unknown or revoked API key (`401`, with `WWW-Authenticate`). |
| `-32002` | The key is valid but does not permit the tool, requested field, or snapshot topic. |
| `-32003` | A session-scoped resource is unavailable right now (a staged-upload slot, an ended session). |

A **tool that ran and failed** is not a protocol error: it returns
`{"content": [...], "isError": true, "structuredContent": {"error": {"code": "...",
"message": "..."}}}`, carrying the shared operation's typed error code (for example
`ERROR_CODE_CONFLICT`, `ERROR_CODE_INVALID_ARGUMENT`).

## Authentication and security

- Credential: `Authorization: Bearer <key>` or `X-API-Key: <key>`. A missing or invalid
  credential is `401` with `WWW-Authenticate: Bearer realm="capsulad-mcp"`.
- MCP never accepts a browser session cookie or a CSRF token. It is an API-key surface,
  so it is not a way to impersonate a logged-in browser session, and the browser cannot
  use MCP with a session alone.
- The listener uses the same self-signed certificate as the Web UI. Trust it with
  `capsulad-cli cert status` (fingerprint) or fetch it with the `get_certificate` tool.
  There is no plaintext variant.
- A concrete cross-origin `Origin` must match the request host. Non-browser clients that
  send no `Origin` are accepted; the check exists to stop a random web page from driving
  a loopback MCP endpoint through the user's browser.
- Tool invocations are logged on the `mcp` component with the key name (never the
  secret). Rejected credentials are logged with the remote host.
- Secrets: API keys are stored as hashes and returned once; Wi-Fi passphrases are
  forwarded once and never stored or echoed; paired Bluetooth devices are addressed by
  the opaque `device_id`, never a raw address.
- Every request is authenticated, not just `initialize`: revoking a key stops new MCP
  requests immediately, including requests that carry an existing session id. Revocation
  also **ends the sessions that key created**: an API-key edit publishes the same access
  topic the daemon uses for the Web UI, and the listener drops every MCP session whose key
  is no longer active, cancelling its training run, staged uploads and in-flight tool
  calls. Session `DELETE`, idle expiry and listener shutdown run the same teardown.

## Tools

`tools/list` shows the exact variants usable with the caller's key. A `status` key sees
only the first two tools; an `assistant` key sees those plus the bounded entries below;
an `operator` key sees the full catalogue. Direct `tools/call` requests enforce the
same scope, fields and topics independently of listing.

Command tools accept exactly one `capsulad.web.v1` command payload as their argument
object. **The input schema is generated from the protobuf descriptor**, so it cannot
drift from the protocol: field names are the proto names, enums are proto enum names and
nested messages are expanded. The schema is also strict in the same way the server is:
`additionalProperties: false` (the decoder rejects unknown fields), a `required` list for
the fields the daemon enforces (for example `expected_revision` on `update_settings`, or
`app_id` on `launch_app`), and, for every `oneof`, one `allOf` entry holding an exactly-one
`oneOf` of its members' `required` lists. A message with several oneofs therefore gets one
constraint per group instead of a single flattened one, so "exactly one of" is
machine-checkable rather than prose. The hand-written schemas (the reads and the staged
uploads) also set `additionalProperties: false`. Assistant command schemas project the
same descriptors through a positive allowlist, including nested fields and oneof members.

| Tool | Scope | Shared operation |
|---|---|---|
| `get_status` | status | `DeviceService.GetInfo`/`ListResources`, `ApplicationService.ListRuns`, `SystemService.GetStatus` (the `/api/status` read). |
| `get_certificate` | status | `/api/cert.pem`. |
| `get_state` | assistant (limited), operator | A snapshot of `overview`, `settings` or `application` (+ `app_id`); operator also gets `diagnostics`, `wakeword`, `access`, `wifi` and unfiltered topic views. |
| `list_assets` | operator | the shared asset catalog, optionally filtered by `prefix`. |
| `list_api_keys` | operator | key names, prefixes, creation times, revocation state and scopes. |
| `update_settings` | assistant (limited), operator | `Command.settings`; requires `expected_revision`. Assistant may set master volume, display brightness/mode, startup sound and timezone only. |
| `launch_app` | assistant (limited), operator | `Command.launch`. Assistant can name only an installed app by `app_id`; operator can also submit declared launch arguments. |
| `stop_run` | assistant, operator | `Command.stop` (the run must belong to that app). |
| `trigger_action` | operator | `Command.action`, including actions that may enroll an app or change where credentials are sent. A declared action is not a safety grant. |
| `update_app_settings` | operator | `Command.app_settings`; requires `expected_revision` and declared keys. |
| `media_command` | assistant, operator | `Command.media`; play, pause, next and previous for app providers and the framework Bluetooth provider, including paused or idle ones. |
| `bluetooth_command` | operator | `Command.bluetooth`; discoverable, incoming connections, unpair, adapter name. |
| `wifi_command` | operator | `Command.wifi`; scan, connect, forget. |
| `record_sample` | operator | `Command.record_sample`; microphone indication gate applies. |
| `delete_sample` | operator | `Command.delete_sample`. |
| `train_wakeword` | operator | `Command.train` plus an optional bounded wait (see below). |
| `cancel_training` | operator | `Command.cancel_training`. |
| `delete_wakeword` | operator | `Command.delete_wakeword`. |
| `dismiss_detection` | operator | `Command.false_detection`. |
| `clock_command` | assistant, operator | Stopwatch, timers, alarms and active-alert dismissal through `Command.clock` (`app_id` must be `capsulad.clock`). |
| `create_api_key` | operator | `websecurity.Store.CreateAPIKeyWithScopes`; returns the secret once. |
| `revoke_api_key` | operator | `Command.revoke_api_key`. |
| `install_app` | operator | the same `InstallApplication` stream as `POST /api/apps` (one-call form, archive in a single POST). |
| `remove_app` | operator | the same `UninstallApplication` call as `DELETE /api/apps/{appID}`. |
| `upload_asset` | operator | `POST /api/assets` (one-call form, `path` + base64 body). |
| `begin_upload` | operator | starts a staged `asset` or `app` upload and returns `upload_id`/`max_bytes`. |
| `upload_chunk` | operator | appends one decoded-≤1 MiB chunk into the same sink (`assets.Write` / `InstallApplication`). |
| `complete_upload` | operator | ends the body and returns the asset entry or install result. |
| `abort_upload` | operator | discards the staged upload (idempotent). |
| `delete_asset` | operator | `DELETE /api/assets?path=...`. |

For an `assistant` key:

- `overview` shows master volume, installed app identity, run identity/state and Now
  Playing transport and track metadata. Only `PLAYBACK_PLAYING` players are shown; the
  projection omits arbitrary source/media names (which can contain a configured radio
  URL or Bluetooth peer name), adapter and pairing state, and diagnostic details.
- `settings` shows the revision, volume, brightness and mode, startup sound, timezone,
  and display/ring resource information. `update_settings` changes only those writable
  preferences. Reuse `expected_revision` as returned by `get_state` (a decimal string);
  a JSON integer is also accepted. A `display` patch requires a nonempty `update_fields`
  list of `brightness` and/or `brightness_mode` and an explicit value for each field
  named in it. An omitted or empty mask would otherwise also write the default provider.
  Hostname, provider selections, microphone/privacy
  settings and assistant/wakeword configuration remain operator-only.
- `application` shows app identity and runs and the built-in clock/ring state when
  applicable. Clock state includes local time, timers, stopwatch, alarms and active
  alerts. App setting values and definitions, logs, interface descriptions and
  provenance are not returned. The launch tool does not accept any launch arguments,
  even when a manifest declares them; an app requiring an argument will reject a bare launch.
  An accepted launch runs the app's usual startup behavior with any existing settings.
- Generic app configuration and actions remain operator-only: declaration and type checks
  cannot say whether they enroll a client, change a credential destination or perform an
  unrelated operation. Use `media_command` and `clock_command` for explicit controls.
  `media_command` reports acceptance or a typed error without provider-authored text,
  which can contain a configured stream URL.
  Bluetooth adapter and Wi-Fi administration, training/samples/detections, diagnostics,
  keys, package installation/removal, and every asset or staged-upload operation are
  operator-only. The PA raw-WAV relay is restricted to operator keys and browser sessions.

## State, streams and files

- **Reads are snapshots.** There is no live event stream in MCP. `get_state` returns a
  snapshot (restricted for assistant keys); call it again to observe change. A mutation
  event the browser receives (for example a new settings view) is not pushed to MCP.
- **Wakeword training is the only streaming operation.** `train_wakeword` returns a
  `training_id` immediately. With `wait_seconds > 0` (clamped to 300 seconds) the call
  waits up to that long for the terminal stage and reports `status` (`installed`, `error` or `running`), the last
  stage, the log line, `fraction`, gate metrics and the training failure detail. Without
  a wait, poll `get_state` with `topic: "wakeword"`, or call `cancel_training`. One
  training run may be active per MCP session; ending the session, revoking its key, or
  stopping the daemon cancels it.
- **Uploads** arrive as base64 in JSON. Two forms exist, and both end in the same sink
  the browser uses, so the API limits are preserved rather than re-stated:
  - **Staged (full limits, bounded memory):** `begin_upload` → repeated `upload_chunk` →
    `complete_upload` (or `abort_upload`). Chunks carry at most 1 MiB decoded (the encoded
    size is checked before decoding), are written into a pipe the asset store's streaming
    writer or the package `InstallApplication` stream consumes, and are backpressured by
    that sink. **Send chunks in order, one at a time.** Completion stops accepting chunks
    and waits for one already in flight before closing the body, so a completed upload is
    never silently truncated: a chunk that races completion is refused with a clear
    error. The store's 32 MiB per-file limit, its 48 MiB aggregate budget and the
    daemon's package-archive limit apply exactly as they do for the Web UI's raw upload;
    `begin_upload` reports the applicable `max_bytes`. At most two uploads may be staged
    per session; a finished (failed, aborted or expired) upload no longer counts, and the
    next `begin_upload` reaps it.
    - **Staging is budgeted.** A staged body reserves its bytes in the shared asset
      store, so *published bytes plus every in-flight reservation* — including the bytes
      about to be staged — stay within the store budget. Several sessions staging large
      files therefore cannot fill the data partition before any of them publishes; an
      upload that no longer fits is refused instead of consuming disk it can never keep.
      The reservation is released on success, failure, abort, expiry and session
      teardown. Because the atomic rename frees the old file only at publication,
      **replacing an asset needs staging headroom equal to the new body on top of the
      published tree**: at a full store the replacement is refused with "asset store is
      full" (delete the target or free space first). An empty body stages nothing and is
      always accepted — and an empty replacement therefore truncates an existing
      asset to zero bytes (the contract the browser route always had). "Asset store is
      full" can also mean "an in-flight upload still holds the budget"; it clears when
      that upload publishes, aborts or expires (the 15-minute lifetime bounds it).
      Note the headroom rule is conservative: a replacement is refused with "asset
      store is full" *even though* the rename would free the old bytes, so an operator
      replacing a large asset in a nearly full store must delete it (or free space)
      first. The Web UI shows the same 413 text for that case.
    - **A delete wins over a replacement in flight.** If an administrator deletes a path
      while a replacement of it is still streaming, the delete succeeds and the in-flight
      write is refused ("asset was deleted while it was being written") instead of
      silently recreating the asset. A write started *after* the delete is a deliberate
      recreation and publishes normally.
    - A staged upload is bound to its session: `abort_upload`, session delete, key
      revocation, session expiry, listener shutdown or a 15-minute lifetime ends it,
      removes the partial file, and reports the specific reason ("expired", "aborted",
      "session ended", "stalled") on a later chunk or completion. An upload that has
      already ended is reported as a **failed tool call** (`isError: true`, with the
      reason), consistently with every other failed operation, not as a protocol error.
  - **One call (convenience):** `upload_asset` / `install_app` carry the whole body in a
    single POST, so they are bounded by the 16 MiB request limit. Use them for small
    files; use the staged form for anything larger. No path truncates silently: an
    oversized request is a `413`, an oversized staged upload is an error before the
    extra bytes reach the sink.
- **Downloads**: the certificate is returned as PEM text. Asset *contents* are not
  downloadable, matching the Web UI, which lists id, sandbox path, media type, SHA-256
  and size but does not serve asset bytes. Package archives are never downloadable. An
  application's custom settings document (`/api/apps/{appID}/settings-ui`) is browser-only
  — it is untrusted HTML rendered in an opaque-origin iframe — and `get_state` with
  `topic: "application"` carries the machine-readable schema and values for operators only.
- **No live asset invalidations.** Asset writes have no browser invalidation topic, so a
  Files page that is already open shows an MCP upload or delete after a reload or
  navigation (use `list_assets` for the authoritative view). Settings, apps, Bluetooth
  and API-key changes do publish the topics the Web UI subscribes to, so those mutations
  reach an open browser immediately.
- Sessions are bounded (64 live, 30 minutes idle). Dirty sessions are pruned on the
  next request; the least recently used session is evicted when the cap is reached.

## Deliberately not exposed

Operator keys have the whole `webv1.Command` allowlist, all snapshot topics, package
install/remove, asset upload/delete and the certificate. Browser presentation and
session-management surfaces are deliberately not MCP tools:

| Browser route | Why MCP does not expose it |
|---|---|
| `POST /api/login`, `POST /api/logout`, `GET /api/session` | MCP has no browser session. It authenticates every request with the key itself, never a cookie, so it cannot mint or impersonate one. |
| `POST /api/password` | A static key must never be able to change the administrator password: key compromise would become account takeover. Use an authenticated browser session or `capsulad-cli auth set-password`. |
| `GET /api/apps/{appID}/settings-ui` | Untrusted HTML rendered in an opaque-origin iframe; it is browser presentation, not data. An operator can read machine-readable schema and values via `get_state` topic `application`. |
| `/ws` event stream | MCP is request/response by design; `get_state` reads snapshots and there is no server-initiated stream (`GET /mcp` is `405`). |
| asset bytes | The Web UI does not serve asset contents either; both list metadata (id, path, media type, SHA-256, size). |

Only an administrator browser session (or the local CLI) can change the administrator
password. Even an operator API key cannot impersonate or obtain a browser session.
The app-specific PA Speaker raw audio route remains HTTP-only and is not an MCP tool.

## Worked example

```sh
# 1. create an assistant key on the device (prints the secret once)
capsulad-cli auth key create agent --scope assistant

# 2. initialize; keep the returned Mcp-Session-Id
curl -sk https://capsula.local:8443/mcp \
  -H 'Authorization: Bearer capsulad_…' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cli","version":"1"}}}'

# 3. list the tools this key may call (Mcp-Session-Id from step 2)
curl -sk https://capsula.local:8443/mcp \
  -H 'Authorization: Bearer capsulad_…' \
  -H 'Mcp-Session-Id: <session>' \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'

# 4. launch an app through the same operation the Web UI uses
curl -sk https://capsula.local:8443/mcp \
  -H 'Authorization: Bearer capsulad_…' \
  -H 'Mcp-Session-Id: <session>' \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"launch_app","arguments":{"app_id":"capsulad.clock"}}}'

# 5. end the session
curl -sk -X DELETE https://capsula.local:8443/mcp \
  -H 'Authorization: Bearer capsulad_…' -H 'Mcp-Session-Id: <session>'
```

For a self-hosted Cadence server, a Cadence administrator registers this endpoint as a
[`mcp-http` provider](https://git.shino.boo/devilreef/cadence/-/blob/9a6b257fd6e21b8f0e725461af332489fb11e383/deploy/server/README.md#L1301-L1339)
with an `Authorization: Bearer <assistant-key>` header, adds the provider to the server's
`mcp` list, and explicitly enables the desired tools for the device. A Cadence device-link
token does not register providers, and the speaker client does not register one on enrollment.

## Proof level and limits

Host evidence only; no device measurement is recorded.

- `go test ./internal/webui -run TestMCP` covers the real HTTP surface: `initialize`
  (version negotiation, capabilities, `Mcp-Session-Id`), scope-filtered `tools/list`, a
  `tools/call` that drives the shared operation and the fake daemon behind it, protocol
  and tool errors, a status key refused on an operator tool, key minting with explicit
  scopes, asset upload/list/delete, one-call and staged package install/remove, content
  negotiation, origin rejection, session DELETE, session/key binding, `DELETE`/shutdown
  teardown and concurrent session lifecycle. Protocol cases cover the version-dependent
  batching rule, the `MCP-Protocol-Version` rejection/acceptance, repeated `initialize`
  reusing its session, accepted unsolicited responses and a `notifications/cancelled`
  stopping a live device recording. Upload cases cover multi-chunk assets and packages,
  the 1 MiB chunk cap (encoded and decoded), the per-session slot limit and its reap of
  finished uploads, abort idempotency, expiry reasons, completion waiting for an
  in-flight chunk (no truncated publish), an abort releasing a blocked chunk, two
  interleaved staged uploads through the shared store, a paused upload not stalling
  `list_assets`, and a reported `max_bytes` equal to the store/artifact limits. The store
  has direct regressions for the budget invariant (a concurrent body holds its
  reservation, an over-budget concurrent body is refused before staging, a failed body
  releases everything, a replacement needs staging headroom so a full-store replacement
  is refused, the physical published+staged footprint stays within the budget during a
  concurrent replacement, and published bytes equal the budget) and for delete-wins at
  both the catalog and
  MCP levels (the in-flight replacement is refused with `isError` and the asset stays
  deleted; a later upload recreates it). The schema generator is tested with a
  two-oneof descriptor built in the test, and ended-upload outcomes are asserted to be
  `isError` tool results. A slow-body test proves the request read deadline is extended.
  The Access view's scope labels have a node test (`web/test/access-scopes.test.mjs`)
  over the same helper the view uses.
- `go test -race ./internal/webui -run TestMCP` covers the session registry, the
  training-slot lifecycle, staged-upload teardown, request cancellation and revocation
  (direct reconcile and the access-topic watcher), including the daemon-side effect of a
  cancelled run or recording.
- `go test ./internal/websecurity -run TestAPIKey` selects scope regressions for
  explicit status defaults, scope inheritance and fail-closed handling of missing or
  unknown recorded grants.
- Not verified: an MCP conformance suite, third-party MCP clients, and a maximum-size
  (32 MiB asset / 65 MiB package) staged upload on the real 240 MiB board — the streaming
  path is exercised with multi-chunk transfers on the host, and the size limits are the
  store's and artifact layer's own, not a re-statement. The browser proof for the Access view is the
  WebSocket snapshot test plus the built bundle; no screenshot of a running speaker was taken. The
  protocol surface documented here is the subset above; there is no resources/prompts/sampling
  support and no server-initiated stream.
