# Application manifest

Every package contains exactly one root `manifest.toml`. It declares the app's identity, its
single entrypoint, its lifecycle flags, its provider hints, its typed settings and actions, its
launch arguments, the interfaces it publishes, an optional custom settings document and
optional provenance evidence. A package may also contain one optional root `README.md` user
guide; it is a package file, not a manifest field, and is described under
[User guide](#user-guide-readmemd).

The manifest is **declarative metadata for the supervisor, the UI and discovery**. It is not a
permission or security policy: it cannot declare an output port, a service endpoint, a listening
socket, an mDNS advertisement or a capability grant. Access is decided by the framework from the
connection's identity, and a listener or publication is requested at runtime from the daemon, not
declared here.

Validation runs at packaging (`capsulad-cli app package`), at install and at launch. Unknown
keys are rejected, so a typo cannot silently disable a feature. There is no per-app instance
policy field: every installed app has at most one live run, and a launch while it is running is
delivered as an invocation (see [lifecycle.md](lifecycle.md#invocations)). A manifest that still
declares the removed `single_instance` key is rejected as unknown.

## Top-level fields

| Field | Type | Default | Constraint |
|---|---|---|---|
| `id` | string | — | Required. `^[a-z0-9][a-z0-9._-]{0,127}$`. Stable identity for the package, its settings file, its runs and its bus identity. |
| `name` | string | — | Required, must not be blank. Display name. |
| `version` | string | — | Required, must not be empty. Free-form; compared for equality only. |
| `description` | string | `""` | Shown in listings. |
| `background` | bool | `false` | The app is started at boot and is not stopped when it is deselected as a provider. |
| `start_on_boot` | bool | `false` | Start a run when the daemon starts; deselecting the app does not stop it. |
| `restart_on_failure` | bool | `false` | Restart after an exit the framework did not request — including a clean self-exit — or after a lost framework connection. An app that exits itself when idle must leave this false. |
| `provides` | array of string | `[]` | Nonexclusive UI hints. Every entry must be non-empty after trimming. |
| `entrypoint` | table | — | Required. See below. |
| `settings` | array of table | `[]` | Typed app settings. See below. |
| `actions` | array of table | `[]` | Buttons. See below. |
| `arguments` | array of table | `[]` | Typed launch arguments. See below. |
| `interfaces` | array of table | `[]` | Published interfaces. See below. |
| `settings_ui` | table | absent | Custom settings document. See below. |
| `native_sdk` | table | absent | Native runtime requirement. Native entrypoints only. |
| `github_attestation` | table | absent | Provenance declaration for a native binary. |

`provides` values are recognized by the UI for provider selection. Any non-empty string is
accepted; the framework's own vocabulary is `segment_display_provider`, `media_player`,
`led_ring_controller` and `voice_assistant`. A hint never grants anything and never makes the
app a default — an administrator selects the default app for a resource.

## `[entrypoint]`

| Field | Type | Default | Constraint |
|---|---|---|---|
| `runtime` | string | — | Required. `native`, `quickjs` or `micropython`. |
| `path` | string | — | Required for `native`, `quickjs` and `micropython`. Relative to the package root, cleaned, never absolute and never escaping through `..`. |
| `architecture` | string | `""` | Required for `native`, forbidden for script runtimes. The device accepts `aarch64` (or `arm64`). |
| `abi` | string | `""` | Required for `native`, forbidden for script runtimes. Must describe a Linux ABI, for example `linux-gnu`. |
| `sdk` | string | `""` | Optional informational SDK version, surfaced as `framework_sdk_version` in discovery. Not enforced. |

Rules:

- **Exactly one runtime.** `native` requires `architecture` and `abi`. `quickjs` requires a
  path ending in `.js`; `micropython` requires a path ending in `.py`. Script runtimes must not
  declare `architecture` or `abi`.
- There is a fourth value, `builtin`, for applications compiled into `capsulad`. It is not
  available to package authors: a `builtin` manifest has no package directory, cannot be
  installed or removed, and its package is rejected if it declares a path, architecture, ABI or
  `[native_sdk]`.
- A script entrypoint must resolve, at launch, to a regular file inside the package root. A
  symlink that escapes the package is rejected.
- The native executable must exist and must not be a directory. The launcher runs it directly;
  there is no shell and no argument interpolation.

The device validates the binary's compatibility before spawning anything: `architecture` must
be `aarch64`/`arm64`, `abi` must contain `linux`, and the running native runtime must satisfy
`[native_sdk]` when declared. The simulator accepts host builds, so a `darwin` or `x86_64`
binary works there and is rejected on the device.

## `[native_sdk]`

Optional requirement metadata for a native app that loads the shared SDK runtime.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `abi_major` | integer | — | At least 1. Must equal the installed runtime's major version. |
| `min_minor` | integer | `0` | Not negative. The installed runtime's minor must be at least this value. |

- Valid only with `runtime = "native"`.
- The requirement is preflighted at install and again at launch against the installed runtime's
  metadata; the binding independently re-checks the loaded library at startup. A failure names
  the version mismatch.
- This is a requirement, not a negotiation. Declaring it is what makes "the SDK is missing" a
  clear install-time error instead of a runtime crash.
- Packages built before the capability requirement was removed still carry
  `native_sdk.required_capabilities`; they load normally and that key is ignored.

## `[[settings]]`

One table per setting. Settings are persisted by the framework, typed by the manifest, exposed
to administrators through the Web UI and `ApplicationConfigurationService`, and delivered to
the owning app as `AppSettingsChanged`.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `key` | string | — | Required, unique, non-empty. The Web UI rejects a key longer than 128 bytes. |
| `label` | string | `""` | Display label. |
| `description` | string | `""` | Help text. |
| `type` | string | — | Required. `text`, `text_masked`, `boolean`, `integer`, `number` or `select`. |
| `default` | matching TOML type | absent | Validated against `type`, `min`, `max` and `choices` when present. Absent means "no stored value" until an administrator sets one. |
| `choices` | array of table | `[]` | Required and non-empty when `type = "select"`. |
| `required` | bool | `false` | UI hint: the administrator should provide a value. |
| `restart_required` | bool | `false` | UI hint: the app should be restarted after the value changes. The framework does not restart it. |
| `min` | float | absent | Only meaningful for `integer` and `number`. Must not exceed `max`. |
| `max` | float | absent | Only meaningful for `integer` and `number`. Must not be below `min`. |

### `[[settings.choices]]`

| Field | Type | Default | Constraint |
|---|---|---|---|
| `value` | string | — | Stored value; must be one of these for `select`. |
| `label` | string | — | Display label. |

### Type semantics

| Type | Stored representation | Validation |
|---|---|---|
| `text` | JSON string | Must be a string. |
| `text_masked` | JSON string | Must be a string. Always redacted on `Get`, in the Web UI and in the settings UI snapshot; delivered in clear only to the owning app. Clearing requires an explicit `clear`, not an empty value. |
| `boolean` | JSON `true`/`false` | Must be a bool. |
| `integer` | JSON number without a fraction | Must be an exact integer inside `[min, max]`. |
| `number` | JSON number | Must be inside `[min, max]`. |
| `select` | JSON string | Must equal one of the declared `choices` values. |

Setting keys are unique across the manifest; an empty or duplicate key is rejected. A default
that does not match its declared type, bounds or choices is rejected at validation time.

## `[[actions]]`

Actions are buttons, not persisted settings.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `id` | string | — | Required, unique, non-empty. |
| `label` | string | `""` | Display label. |
| `description` | string | `""` | Help text. |
| `confirm_before_run` | bool | `false` | The UI asks for confirmation before triggering. |

A trigger returns an `action_invocation_id` and arrives at the app as `AppActionInvoked` with
the same ID. The app may report a result with `AppActionResult`; an unreported action is not an
error.

## `[[arguments]]`

Launch arguments are typed, delivered as strings inside `AppLaunchInvocation`, and never
shell-parsed.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `name` | string | — | Required, non-empty. |
| `description` | string | `""` | UI help. |
| `type` | string | — | Required; `text`, `integer`, `number` or `boolean`. |
| `required` | bool | `false` | A launch without the argument is rejected. |

Validation happens before a run is spawned: an undeclared name, a non-integer `integer`, a
non-numeric `number`, or a `boolean` that is not exactly `true` or `false` fails the launch.
`capsulad-cli app start ID NAME=VALUE ...` passes values straight through this validation.

## `[[interfaces]]`

An app may publish one or more gRPC interfaces that other local callers reach through
`AppBusService.Call`. Publishing declares a name and a schema; it does not open a port or a
socket.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `service` | string | — | Required. Fully qualified protobuf service name, for example `org.example.weather.v1.WeatherService`. |
| `version` | string | `""` | Interface version label, for example `v1`. |
| `descriptor` | string | `""` | Optional relative path to a serialized `FileDescriptorSet` (`.protoset`) including its imports. |
| `docs` | string | `""` | Optional relative path to Markdown documentation. |

- A descriptor must decode as a `FileDescriptorSet` with at least one named file, and the
  declared service must be present in it. When it does not, the daemon skips that interface
  and logs a warning; the package still installs and the rest of the app is unaffected.
- When a descriptor is present, its method list is published. Callers may reach an interface
  without a descriptor using raw protobuf, but they cannot use the JSON path for it.
- With docs, the first non-empty line becomes the interface summary and each RPC's heading
  section is served as per-method documentation.
- `available` is true only while the app's selected run has completed its handshake; the
  reported `run_id` is empty while it is not running.

Publishing an interface does not require declaring it anywhere else. Serving it does: the app
must register a handler for the service and method, and must end every inbound call explicitly.

## `[settings_ui]`

One self-contained HTML document that replaces the generated settings form in the Web UI.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `html` | string | — | Required. Relative, regular `.html` file inside the package root; at most 512 KiB; valid UTF-8. |

The app must declare at least one `[[settings]]` entry. The document is untrusted: it runs in
an opaque-origin sandboxed iframe with `sandbox="allow-scripts"` and a restrictive CSP, and can
only submit validated patches for its own app's declared settings. The protocol is in
[settings-actions.md](settings-actions.md).

## User guide (`README.md`)

A package may include one operator-facing user guide at its root, named exactly `README.md`.
It is not a manifest field: the name is fixed, and an existing package without it remains valid.

| Property | Value |
|---|---|
| Name | `README.md` at the package root (nested README files are not the guide) |
| Required | No |
| Format | Markdown, UTF-8, non-empty |
| Size limit | 64 KiB |
| File type | Regular file; symlinks, directories, devices and FIFOs are refused |
| Rendering | Inert text in the authenticated Web UI application detail |
| Availability | Any installed app, running or stopped; built-in system apps use embedded guides |

- The guide is **not** interface documentation. `[[interfaces]].docs` describes one gRPC service
  for callers; `README.md` explains the app to the person using the speaker.
- The daemon serves it only through the session-authenticated Web UI. It is never executed,
  framed, granted a script context, or used as a navigation target, and the response contains no
  package path.
- A guide should cover the app's controls, how to launch and stop it, teardown or hand-over
  steps, and the failures a user can act on. Repository-only links are not a substitute for
  those instructions.
- A missing guide is a normal state and is reported as "no user guide". An unreadable file
  (wrong type, oversized, invalid UTF-8 or empty) is reported separately in the app detail; it
  does not block packaging, installation or launch.

## `[github_attestation]`

Optional provenance declaration for a native binary.

| Field | Type | Default | Constraint |
|---|---|---|---|
| `subject` | string | — | Required. Relative path to the native executable inside the package. |
| `bundle` | string | — | Required. Relative path to the Sigstore bundle file inside the package. |
| `repository` | string | — | Required. Expected repository identity, for example `org/repo`. |
| `workflow` | string | `""` | Optional expected workflow identity. |

The manifest only *declares* evidence. Verified status is computed independently by the
framework and reported through `ApplicationService.GetBinaryProvenance`, which returns
`PROVENANCE_NOT_PROVIDED`, `PROVENANCE_VERIFIED`, `PROVENANCE_VERIFICATION_FAILED`,
`PROVENANCE_UNVERIFIED` or `PROVENANCE_UNSUPPORTED_RUNTIME`. Declaring an attestation never
changes what an app may do.

## Paths

Every path in a manifest is relative to the package root and is cleaned before use. Absolute
paths, `.` as a whole path, and any component that escapes through `..` are rejected. The
package tree is immutable once installed; the launcher treats only the executable bit as
meaningful.

## Validation summary

Rejected at packaging, install and load:

- an invalid or missing `id`, a blank `name`, an empty `version`;
- an unknown key anywhere in the document;
- an unknown runtime, or a runtime that contradicts `path`/`architecture`/`abi`;
- a `.js`/`.py` mismatch for a script entrypoint;
- any absolute or escaping path;
- a duplicate or empty setting key, an unknown setting type, a `select` without choices,
  `min > max`, or a default that fails its own validation;
- a duplicate or empty action ID;
- a launch argument without a name, or with a type outside the four allowed;
- an interface without a service name, or a descriptor/docs path that is absolute or escaping;
- a `settings_ui` document that is missing, not `.html`, not a regular file, larger than
  512 KiB, invalid UTF-8, or declared without at least one setting;
- `[native_sdk]` on a non-native runtime, `abi_major < 1` or `min_minor < 0`;
- an empty `provides` entry;
- a `github_attestation` missing `subject`, `bundle` or `repository`.

## Complete example: native app

```toml
id = "org.example.clockface"
name = "Clock Face"
version = "1.2.0"
description = "Shows the time on the segment display and serves one custom method."
background = true
start_on_boot = true
restart_on_failure = true
provides = ["segment_display_provider"]

[entrypoint]
runtime = "native"
path = "bin/clockface"
architecture = "aarch64"
abi = "linux-gnu"
sdk = "0.5"

[native_sdk]
abi_major = 1
min_minor = 0

[[settings]]
key = "format"
label = "Format"
description = "How the clock separates hours and minutes."
type = "select"
default = "colon"
[[settings.choices]]
value = "colon"
label = "12:34 (colon)"
[[settings.choices]]
value = "blink"
label = "12 34 (blinking separator)"

[[settings]]
key = "brightness_cap"
label = "Brightness cap"
description = "Never render brighter than this fraction of the ring budget."
type = "number"
default = 0.5
min = 0.1
max = 1.0

[[settings]]
key = "api_key"
label = "Service API key"
type = "text_masked"
required = false

[[actions]]
id = "sync"
label = "Resync now"
confirm_before_run = false

[[actions]]
id = "factory_reset"
label = "Reset to defaults"
confirm_before_run = true

[[arguments]]
name = "face"
description = "Initial face: digits or bars."
type = "text"
required = false

[[arguments]]
name = "verbose"
type = "boolean"
required = false

[[interfaces]]
service = "org.example.clockface.v1.FaceService"
version = "v1"
descriptor = "descriptors/face.protoset"
docs = "docs/face.md"

[settings_ui]
html = "ui/settings.html"

[github_attestation]
subject = "bin/clockface"
bundle = "provenance/build-bundle.jsonl"
repository = "example/clockface"
workflow = ".github/workflows/release.yml"
```

## Complete example: script app

```toml
id = "org.example.counter"
name = "Counter"
version = "0.3.1"
description = "Counts invocations and shows the count on the ring."
restart_on_failure = true

[entrypoint]
runtime = "quickjs"
path = "main.js"

[[settings]]
key = "step"
label = "Step"
type = "integer"
default = 1
min = 1
max = 60

[[actions]]
id = "reset"
label = "Reset counter"

[[arguments]]
name = "amount"
type = "integer"
required = false

[[interfaces]]
service = "org.example.counter.v1.CounterService"
version = "v1"
docs = "docs/counter.md"
```

```toml
id = "org.example.sensorlog"
name = "Sensor log"
version = "0.1.0"
description = "Logs readable sensors once per invocation."
background = true

[entrypoint]
runtime = "micropython"
path = "main.py"

[[settings]]
key = "unit"
label = "Display unit"
type = "text"
default = "c"

[[interfaces]]
service = "org.example.sensorlog.v1.SensorLogService"
version = "v1"
```

Both script manifests are valid without a descriptor: raw protobuf callers can reach the
declared service, and a JSON caller must supply a registered descriptor of its own.