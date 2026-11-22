# Settings and actions

Apps declare typed settings and action buttons in the manifest. The framework persists settings,
renders them in the Web UI, validates the stored type, versions every change with a revision and
delivers changes to the owning app. Actions are transient button presses, not persisted values.

The manifest field tables live in [manifest.md](manifest.md#settings); this page covers validation,
revisions, redaction, delivery and the optional custom settings UI.

## Declaring settings

```toml
[[settings]]
key = "units"
label = "Units"
description = "How temperatures are shown."
type = "select"
default = "metric"

[[settings.choices]]
value = "metric"
label = "Celsius"
[[settings.choices]]
value = "imperial"
label = "Fahrenheit"

[[settings]]
key = "refresh_minutes"
label = "Refresh interval"
type = "integer"
default = 15
min = 1
max = 240

[[settings]]
key = "api_key"
label = "Service key"
type = "text_masked"
```

| Type | JSON value | Notes |
|---|---|---|
| `text` | string | Stored and returned verbatim. |
| `text_masked` | string | Never returned by `Get`, the Web UI or the settings-UI bridge. Delivered in clear only to the owning app. Clearing needs an explicit `clear`. |
| `boolean` | `true` / `false` | |
| `integer` | number without a fraction | Bounds apply to the manifest default and are published to UIs. |
| `number` | number | Bounds apply to the manifest default and are published to UIs. |
| `select` | string | Choices are required and non-empty in the manifest. |

`default` is optional. A missing default means "no stored value until an administrator sets
one"; `has_value=false` reports that state. `required` and `restart_required` are UI hints:
the framework does not enforce a required value and does not restart the app.

Keys are unique, non-empty and at most 128 bytes on the UI surfaces. A malformed default,
`min > max`, a select without choices, an unknown type or a duplicate key makes the package
invalid — see [manifest.md](manifest.md#validation-summary).

## Configuration service

| Method | Shape | Access |
|---|---|---|
| `Describe{app_id}` | unary `AppConfigurationSchema` | Any caller; manifest metadata only. |
| `Get{app_id}` | unary `AppConfigurationSnapshot` | Own app, or an administrator. |
| `Update{app_id, expected_revision, changes}` | unary snapshot | Own app, or an administrator. |
| `TriggerAction{app_id, action_id}` | unary `{action_invocation_id}` | Own app, or an administrator. |

A supervised app may address only its own `app_id`; a foreign ID is `PERMISSION_DENIED`. A local
administrator with no app identity may address any app.

### Schema

`Describe` returns, for every declared setting: `key`, `label`, `description`, `type`,
`default_value_json`, `choices[{value,label}]`, `required`, `restart_required`, `has_min`/`min`,
`has_max`/`max`; and for every action: `action_id`, `label`, `description`,
`confirm_before_run`. It never returns stored values. Use it to build a form.

### Snapshot and revision

```json
{
  "appId": "org.example.weather",
  "revision": "7",
  "values": [
    {"key": "units", "valueJson": "\"metric\"", "hasValue": true},
    {"key": "refresh_minutes", "valueJson": "15", "hasValue": true},
    {"key": "api_key", "valueJson": "", "hasValue": true}
  ]
}
```

- `value_json` is a JSON document encoded as a string (`"\"metric\""`, `"true"`, `"42"`, `"1.5"`).
- `text_masked` entries always have an empty `value_json`; `has_value` says whether a secret is
  stored.
- Non-masked settings with a declared default report the default value with `has_value=true`
  even when nothing is stored.
- The revision identifies the snapshot. It starts at 1 and increases by exactly one per accepted
  update. A rejected update does not advance it.
- App settings survive package replacement and uninstall/reinstall of the same app ID; they live
  outside the package tree.

## Updating settings

```json
→ {"appId":"org.example.weather","expectedRevision":"7","changes":[
     {"key":"units","valueJson":"\"imperial\""},
     {"key":"refresh_minutes","valueJson":"30"},
     {"key":"api_key","clear":true}
   ]}
← {"appId":"org.example.weather","revision":"8","values":[ … ]}
```

Rules:

- Each change is exactly one of a value (`value_json`) or `clear:true`. A non-empty value on a
  masked field replaces the secret; an empty string is rejected — clearing requires `clear`.
- The key must be declared by the app's manifest.
- The value must decode as JSON of the declared type: string for `text`/`text_masked`/`select`,
  bool for `boolean`, exact integer for `integer`, number for `number`.
- `expected_revision` is compare-and-swap. The update succeeds only when it equals the current
  revision. A mismatch is `ABORTED` with a message such as
  `app settings revision 6 is stale (current 8)`. Re-read and retry.
- `expected_revision=0` **skips** the check. It is honored for compatibility, and it is unsafe
  for concurrent writers. Use the revision you read.
- A successful update pushes `AppSettingsChanged` to the owning app and returns the redacted
  snapshot.

Error mapping:

| Condition | gRPC code |
|---|---|
| Malformed JSON, undeclared key, wrong type, empty masked value without `clear` | `INVALID_ARGUMENT` |
| Stale revision | `ABORTED` |
| Storage write failure | `INTERNAL` |
| Cross-app access by a supervised app | `PERMISSION_DENIED` |
| Unknown app | `NOT_FOUND` |

### What is and is not enforced

The framework enforces the stored **type**, key declaration and masked-clear rule. It publishes
`choices`, `min` and `max` to UIs but does not re-check them on a direct `Update`: a
`select` value outside the declared choices, or a number outside `min`/`max`, is stored if its
JSON type is valid. The Web UI renders a `<select>` for choices and a bounded number input, and
the custom settings UI receives the same schema, but that is a UI constraint, not a server
guarantee.

Therefore an app must validate its own settings on receipt and behave defensively when a stored
value is out of range. If a value is unusable, log it, fall back to a safe default in memory, and
surface the problem through an action result or a status presentation. Do not assume the value
came from the generated form.

Direct gRPC updates are bounded only by the 8 MiB framework message limit. The Web UI enforces
64 changes per request, 128-byte keys and 8 KiB per value; the settings-UI bridge enforces 64
changes, 8 KiB per value and 32 KiB per patch.

## Delivery and redaction

`AppSettingsChanged` is sent:

- immediately after the app sends `AppReady`, as an initial snapshot;
- after every accepted update to that app.

```json
← {"settingsChanged":{"revision":"8","values":[
     {"key":"units","valueJson":"\"imperial\""},
     {"key":"refresh_minutes","valueJson":"30"},
     {"key":"api_key","valueJson":"\"s3cr3t\""}
   ]}}
```

- The event is app-private. It contains every declared setting with its stored value or its
  default, and `text_masked` values in clear.
- `Get`, the Web UI and the custom settings UI see masked values redacted. Only this bus event
  carries them in clear; the settings-UI bridge receives redacted values.
- Every `ready` is followed by an initial snapshot; each accepted update while the app is
  running is pushed to it.
- If the app was not running when an update happened, it sees the newest state on its next ready.
- The event is a snapshot, not a delta: replace your in-memory configuration with it, keyed by
  `revision`. A later revision always wins; an older revision must not overwrite a newer one.
- Never log a `text_masked` value, and never include it in a presentation, log message or
  interface response.

## Actions

```toml
[[actions]]
id = "refresh"
label = "Refresh now"

[[actions]]
id = "factory_reset"
label = "Reset to defaults"
confirm_before_run = true
```

| Step | Behavior |
|---|---|
| Trigger | `ApplicationConfigurationService.TriggerAction{app_id, action_id}` returns an `action_invocation_id`. |
| Delivery | The owning app receives `AppActionInvoked{action_invocation_id, action_id}` on its bus. |
| Acknowledgement | Optional. `AppActionResult{action_invocation_id, succeeded, detail}` is recorded and logged; no caller receives an error if it is omitted. |

Rules:

- The action must be declared; an unknown action is `FAILED_PRECONDITION` with
  `application <id> declares no action "<action-id>"`.
- The app must be installed and **ready**. Triggering an action for a stopped or starting app is
  `FAILED_PRECONDITION` with `application <id> is not ready`. Actions are not queued.
- The Web UI respects that rule. The shipped Internet Radio package is the one explicit
  exception: on its application page and in the overview actions menu, a stopped `play` action
  becomes a launch shortcut. The UI calls the public `ApplicationService.Launch` with the radio's
  declared `station` launch argument seeded from the saved `station` setting (or that setting's
  schema default). The separate Launch form is not an input: a value typed there belongs only to
  that form's own launch. The launch is ordinary: it starts the app for that request and does not
  make a non-resident app resident. Other actions stay disabled until a run reports `APP_READY`,
  with that explanation shown; the UI never sends an action it cannot route.
- The overview listing carries no per-app settings values, so its cold Play requests the same
  application snapshot the detail page uses before launching. If no saved selection can be
  resolved, the UI reports the reason instead of sending a launch without it.
- `confirm_before_run` is a UI hint. The Web UI asks for confirmation before sending the trigger.
  The framework does not enforce it for other callers, so a destructive action must verify its
  own preconditions and may refuse by reporting `succeeded=false` with a detail.
- Actions are not persisted and do not change the settings revision.
- Every action should finish its work or report failure. A long action blocks nothing in the
  daemon, but it must not leave the user without feedback: send `AppActionResult` when the
  outcome is known.

### Shipped assistant setup actions

Marusia and Cadence Client both ship a `[settings_ui]` document, so their setup panels
are app-owned pages rendered inside the application detail, not sections of the system
Settings page. The document is the only settings surface an operator needs; declared actions
remain in the application's **Actions** list below it, and the document must name them instead
of trying to invoke them.

- **Marusia** (`[settings_ui] html = "ui/settings.html"`) shows an always-empty password field
  for a VK access token or full OAuth redirect URL. Submitting it writes only the masked
  `vk_access_token` setting; the app detects a token that changed after its initial snapshot,
  runs the same validated exchange as `reauth`, stores `session_id`/`session_secret`, clears
  the one-shot token and writes a short non-secret `last_auth_status`. The page derives the
  session, pending-token and session-file state from redacted snapshot flags, never from the
  stored secret, and offers a clear-pending control. Every other declared setting is generated
  from `CapsuladUI.schema` inside a collapsed `<details>` area.
- **Cadence Client** (`[settings_ui] html = "ui/settings.html"`) walks an unenrolled device
  through server URL, a TLS CA asset path when the URL is `https://` (default
  `/assets/cadence/ca.pem`), the `test_connection` action, the one-time enrollment code, live
  enrollment confirmation and MCP setup advice. `test_connection` resolves DNS, opens TCP and
  performs TLS with the app's `ca_pem` trust roots, logs `test_ok` or `test_failed`, and writes
  the same non-secret outcome to `last_test_status` for the page. Once a device token is
  present the page shows the server/status plus live `board`, `hold_release_tail_ms` and
  `follow_up` controls and a confirm-then-reset that clears every declared setting on the
  speaker without touching the Cadence server.

Neither document can call an action, read logs, fetch, use storage or reach the app bus: the
bridge exposes only the schema, revision, redacted values, theme, `onState`, `onResult`,
`patch` and `resize`. That is why test and retry controls stay as manifest actions and the app
writes its own status text.

## Custom settings UI

An app may replace the generated form with one self-contained HTML document.

```toml
[settings_ui]
html = "ui/settings.html"
```

| Constraint | Value |
|---|---|
| Path | Relative, regular `.html` file inside the package root. |
| Size | At most 512 KiB, valid UTF-8. |
| Requires | At least one declared `[[settings]]` entry. |
| Network | Forbidden by CSP: no fetch, XHR, WebSocket, media, frames, forms or external fonts. |
| Asset URL | `GET /api/apps/<app-id>/settings-ui`, fetched with the administrator's session. |

### Hosting and isolation

The Web UI loads that URL as the iframe's `src` and mounts it with `sandbox="allow-scripts"` only.
The document therefore has an **opaque origin**: no same-origin access, cookies, storage, popups,
forms, downloads, pointer lock, host DOM or API. The session cookie authenticates the document
request; the document receives no token, host cookie access or WebSocket.

The response and the injected wrapper apply:

```text
default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:;
font-src 'none'; connect-src 'none'; media-src 'none'; object-src 'none'; frame-src 'none';
child-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'; navigate-to 'none';
frame-ancestors 'self'; sandbox allow-scripts
```

The response also sends `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: no-referrer`, COOP/CORP `same-origin` and `Vary: Cookie`, and never sets a
cookie. The wrapper injects the local UI kit CSS and the bridge bootstrap **before** the app's
markup. The app supplies no external script or stylesheet URL.

UI kit: the wrapper injects a locally built Tailwind + daisyUI 4 stylesheet (compiled into the
daemon; no CDN and no external font or image request). Wrap the content in
`<main class="capsulad-ui">` and compose the document from daisyUI classes with explicit
Tailwind layout: `card` + `card-body` + `card-title`, `btn` (`btn-primary`, `btn-ghost`,
`btn-sm`, `btn-active`), `input input-bordered`, `select select-bordered`,
`textarea textarea-bordered`, `checkbox`, `toggle`, `range`, `alert alert-error` /
`alert-success`, `badge` (`badge-ghost`, `badge-success`, …), `label` + `label-text` /
`label-text-alt`, and the layout utilities (`flex`, `grid`, `flex-col`, `flex-wrap`,
`items-{start,center,end}`, `justify-{start,between}`, `gap-*`, `p-*`, `mt-*`, `w-*`,
`text-{xs,sm,lg}`, `font-mono`). The supported set is fixed when the daemon is built;
`web/tailwind.settings.config.js` is the authoritative safelist. Do not add a stylesheet link,
a style import, or a second component kit.

The kit carries two themes, `capsulad` (light) and `capsulad-dark`, with the system colour
scheme on each. The bridge sets `data-theme` on the document element from the administrator's
Light/Dark/System choice and updates it live through the `theme` message below. A theme change
is an attribute swap: drafts, focus, scroll position and pending patches are preserved; nothing
is reloaded or replayed. Style with daisyUI variables (`--p`, `--b1`, `--bc`, …) or its classes,
not fixed page colours. On mount the host opens the document with a reserved
`#capsulad-theme=light|dark` fragment so the first paint already matches; the bootstrap consumes
it before the app script runs.

Compatibility aliases for the first-kit CSS variables (`--surface`, `--fg`, `--muted`, `--accent`,
`--ok`, `--warn`, `--err`, `--radius`, `--sans`, `--mono`) map to the active daisyUI theme. There
are no first-kit component styles: a packaged document that uses the old `stack`, `row`, `spread`,
`field`, `check`, `pill`, `notice`, `actions`, `mono` or `empty` classes still loads and works
through the bridge but is unstyled until migrated to the daisyUI set above. The protocol, patches,
CSP and response headers are unchanged.

### `capsulad.settings-ui/1:` protocol

The injected bootstrap exposes `window.CapsuladUI`:

```js
CapsuladUI.apiVersion   // 1
CapsuladUI.appId
CapsuladUI.schema       // { settings: [...] } from the manifest
CapsuladUI.revision     // current app-settings revision
CapsuladUI.theme        // 'light' or 'dark' (the resolved palette)
CapsuladUI.onState(fn)  // snapshot/state events
CapsuladUI.onResult(fn) // patch results, each carrying requestId
CapsuladUI.patch(changes) // [{key, valueJson}] or [{key, clear:true}];
                          // returns a 1-based request id (0 when ignored)
CapsuladUI.resize()
```

Every message has a `type` prefixed with `capsulad.settings-ui/1:` and a per-frame `channel`
token.

| Direction | Type | Payload |
|---|---|---|
| parent → frame | `init` | `{channel, appId, apiVersion:1, schema, revision, values, theme}`. Exactly one. |
| parent → frame | `theme` | `{channel, appId, theme: 'light'\|'dark'}`. Live palette change; no reload. |
| frame → parent | `ready` | The bootstrap accepted init. |
| frame → parent | `patch` | `{requestId, changes:[{key, valueJson}|{key, clear}]}` |
| frame → parent | `resize` | `{height}` (20–4000 px). |
| parent → frame | `state` | `{revision, schema, values}` after an update. |
| parent → frame | `result` | `{requestId, ok, revision, message?, error?}`. |

`theme` is one-way: the frame applies it by setting `data-theme` on `<html>`. It is accepted
only with this frame's channel, is never answered, and has no effect on drafts, patches,
`revision` or the persisted settings. A frame cannot request or dictate a palette; a child
message of type `theme` is rejected as unknown.

`patch` returns a per-frame, monotonically increasing request ID, starting at 1. It returns 0
when the call is ignored locally (empty or non-array `changes`). The matching `result` echoes
that ID as `requestId`; ids are never reused for the document's lifetime. Patches are serialized
by the host (one in flight, queue of 16), but a frame may have several submitted patches
outstanding, so **correlate each result with the patch that produced it**. A `result` is not a
generic acknowledgement of "the latest edit": a predecessor's late result must not clear a newer
draft or claim the newest save.

After a patch reaches the daemon, the host sends that patch's `result` and then the `state`
snapshot for the resulting revision, in that order on the frame's channel. `state` carries the
authoritative persisted values but is not tied to one request; use `result.requestId` for the
acknowledgement and treat snapshots as convergence data.

A form that submits text should keep `{id, text}` for the newest submission and clear its dirty
flag only when `result.ok` arrives for exactly that id **and** the control still holds the
submitted text. A failed result, a result for an older id, a newer edit, or an unrelated
snapshot must leave the draft untouched.

Host rules:

- A message is accepted only when `event.source` is that exact iframe's `contentWindow` and
  `event.origin` is the opaque `null`. A claimed real origin is rejected.
- The channel is a random 32-hex token, fresh per mount. A stale or unmounted frame has no
  effect.
- The child message is limited to **64 KiB**, 1–64 changes, **8 KiB** per value and **32 KiB**
  total; keys must be declared. A request ID is required for patches. Unknown types and
  undeclared keys are rejected.
- Patches are serialized: one in flight, queue of **16**. Overflow returns
  `too many pending updates; retry after the next state`.
- Accepted patches go through the normal `Update` path, so the daemon re-checks key, type,
  masked/clear and revision. The result is returned to the frame; on failure the host refreshes
  the authoritative snapshot and sends `state`.
- No generic app bus, capture, audio, asset or filesystem proxy is available to the frame.

### Failure handling

| Failure | Behavior |
|---|---|
| No `ready` within 10 s | Visible error; the frame is disposed; retry re-fetches settings and mounts a fresh frame. |
| Invalid message, wrong channel or undeclared key | Reported as an error and the frame is disposed; no host effect. |
| Navigation or reload after init | Detected by a second `load`; the frame is pointed at `about:blank`, the token is invalidated and the user must retry. |
| Rejected patch (stale revision, bad value) | `result{ok:false, error, revision}`; the host re-syncs and the frame may retry after `state`. |
| Initial revision unknown | `Get` is fetched before mounting; a frame never starts with revision 0. |

Browsers do not implement `navigate-to`, so self-navigation is not claimed to be CSP-blocked. The
host detects it instead.

### Complete example

`ui/settings.html`:

```html
<!doctype html>
<html><body>
<main class="capsulad-ui p-3">
  <div class="card bg-base-100 border border-base-300">
    <div class="card-body gap-3">
      <h2 class="card-title">Units</h2>
      <label class="form-control">
        <span class="label-text">Units</span>
        <select id="units" class="select select-bordered select-sm"></select>
      </label>
      <label class="form-control">
        <span class="label-text">Refresh interval (minutes)</span>
        <input id="refresh" class="input input-bordered input-sm w-32" type="number" min="1" max="240" step="1">
      </label>
      <div class="alert" id="status" role="status"></div>
    </div>
  </div>
</main>
<script>
  const units = document.getElementById('units');
  const refresh = document.getElementById('refresh');
  const status = document.getElementById('status');

  CapsuladUI.onState((state) => {
    const choices = state.schema.settings.find((s) => s.key === 'units').choices;
    units.replaceChildren(...choices.map((c) => new Option(c.label, c.value)));
    const values = Object.fromEntries(state.values.map((v) => [v.key, v.valueJson]));
    units.value = JSON.parse(values.units ?? '"metric"');
    refresh.value = JSON.parse(values.refresh_minutes ?? '15');
  });

  units.addEventListener('change', () =>
    CapsuladUI.patch([{ key: 'units', valueJson: JSON.stringify(units.value) }]));
  refresh.addEventListener('change', () =>
    CapsuladUI.patch([{ key: 'refresh_minutes', valueJson: String(Number(refresh.value)) }]));

  CapsuladUI.onResult((result) => {
    status.textContent = result.ok ? 'Saved.' : result.error;
    status.className = result.ok ? 'alert alert-success' : 'alert alert-error';
  });
  CapsuladUI.resize();
</script>
</body></html>
```

Keep variant classes as complete literal strings (`'alert alert-success'`, not
`'alert alert-' + kind`): the kit is built from the checked-in documents and the safelist, and
a concatenated suffix is not a scanned candidate.

The document is trusted only for its own rendering. It cannot read a masked secret, call another
app, open the app bus or escape the frame; its only write is a validated patch for its own
manifest-declared settings.