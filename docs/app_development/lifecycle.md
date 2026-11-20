# Lifecycle, identity and environment

`capsulad` owns the whole process lifecycle. An app never daemonizes, never forks a supervisor,
never creates or binds a listening socket and never assigns itself an identity. A fixed inbound
TCP or UDP port is a listener the daemon allocated and passed in (see
[network.md](network.md#daemon-mediated-listeners)); an app's own `udp_open` socket is ephemeral
and carries replies. It is started by the framework, told who it
is, connects to the framework socket, declares itself ready, serves events, and exits when its
run ends.

## Launch

The launcher performs these steps in order; a failure at any step ends the launch with an error
and no process:

1. Resolve the installed manifest and the configured `[native_sdk]` requirement.
2. Check binary compatibility: `architecture` must be `aarch64`/`arm64` and `abi` must describe
   a Linux ABI. Script runtimes must have their host installed.
3. Resolve the entrypoint inside the immutable package directory. A symlink that escapes the
   package is rejected; a native entrypoint must exist and must not be a directory.
4. Choose the probed sandbox mode (`namespaces`, `mountns`, `uid` or `none`) and build the
   command. Script runtimes run the installed host with the package script as its argument.
5. Build the environment (next section), spawn the process, and start collecting its stdout and
   stderr.
6. Record a run in `APP_STARTING` state with a fresh run ID. The run ID is newly generated for
   every start; it never repeats.

The simulator skips the architecture/ABI check so a host build runs there. Everything else is
identical, which is why an app that runs in the simulator runs on the device once built for
`aarch64`/`linux-gnu`.

## Identity

Identity comes from the connection, not from a claim:

- The app sends `AppHello{run_id}` on `AppBusService.Connect`. The daemon resolves that run ID
  to a supervised process and compares the process ID with the Unix-socket peer credentials.
  A mismatch, an unknown run, or a claim on a compiled-in system app's run is
  `PERMISSION_DENIED`.
- A connection with a sandbox UID or a supervised PID but no run ID is still treated as an app.
  It cannot become an administrator by silence.
- **Administrator** means a local daemon, CLI or Web UI caller with no app identity.
  **App scope** means a supervised app addressing its own state. An app may address only itself.
- `AppReady` marks the run READY; that is when `ApplicationService` reports `APP_READY` and the
  app's declared interfaces become `available`.

## Environment

The launcher injects these variables. Read them; never hard-code a path.

| Variable | Meaning |
|---|---|
| `CAPSULAD_SOCKET` | The framework's Unix socket. `/run/capsulad/hal.sock` in namespace modes; the host path otherwise. |
| `CAPSULAD_RUN_ID` | This run's identity. Required by `Connect`. |
| `CAPSULAD_APP_ID` | The installed app ID. |
| `CAPSULAD_SDK` | The framework SDK version the daemon reports for this platform. |
| `CAPSULAD_LIBRARY` | Absolute path of the compatible `libcapsulad`. `/capsulad/lib/libcapsulad.so.1` in namespace modes. |
| `CAPSULAD_LIBRARY_DIR` | Directory for the library's own dependency resolution. |
| `CAPSULAD_ASSETS` | The read-only asset store: `/assets` in namespace modes, the host path otherwise. |
| `CAPSULAD_DEVICE_SERIAL` | The SoC serial read by the daemon from the host (`/proc/cpuinfo` `Serial` on ARM), provided when known and omitted entirely when unknown. This is not a `device_id`. |
| `CAPSULAD_CONTROL_FD` | Reserved: the inherited descriptor number of this run's control channel, normally `3`. It carries listener allocation only. Never read, write, close or reuse it; `listener_open` is the only supported use. Present whenever the supervisor could create the channel; if creation failed the run still starts without it (one `WARN` in the daemon log) and every listener call returns `UNSUPPORTED`. |
| `CAPSULAD_APP_DIR` | Script hosts only (quickjs, micropython): the package root, `/app` in namespace modes and the host path otherwise. Native apps do not receive this variable. |
| `CAPSULAD_INTERPRETER` | Script runtimes only: the engine executable that is running the script. |
| `TZ` | The resolved device timezone, when one is configured. |

Do not assume a working directory: namespace modes start at `/`, `uid` mode inherits the
daemon's directory, and only `none` runs from the package root. Script apps read
`CAPSULAD_APP_DIR`; native apps locate package files relative to their own executable path,
since the launcher starts them by absolute path.

Inside a namespace sandbox the filesystem view is fixed:

```text
/app                   the installed package, read-only
/assets                the shared asset store, read-only
/run/capsulad/hal.sock the framework socket
/capsulad/lib           the native runtime and script hosts, read-only
```

In `uid` and `none` modes the same contract holds with host paths; the variables tell you
which.

The control channel is not a path: it is an inherited connected Unix descriptor, passed as
descriptor 3 with its number in `CAPSULAD_CONTROL_FD`. It appears nowhere in the filesystem
view, carries only listener allocation, and must never be used as a general-purpose channel.

## Run states

```text
APP_STARTING ──AppReady──► APP_READY ──stop or failure──► APP_STOPPING ──► APP_EXITED
      │                        │
      └────exit/exec error─────┴──────────────────────────────────────► APP_FAILED
```

| State | Meaning |
|---|---|
| `APP_STARTING` | A process exists; no `AppReady` yet. |
| `APP_READY` | The bus handshake completed. Interfaces are available and events are delivered. |
| `APP_STOPPING` | A stop was requested; the process is expected to exit. |
| `APP_EXITED` | The process ended cleanly: a framework-requested stop, or the app exited on its own with status 0. |
| `APP_FAILED` | The process ended with a non-zero status or a signal, or its framework connection was lost. |

`AppRun` carries `run_id`, `app_id`, `pid`, `state`, `started_monotonic_ns`, `exit_code`,
`status_detail` and `runtime`. An app can watch its own runs through
`ApplicationService.ListRuns`/`WatchRuns`; a local administrator can watch any app.

Exit codes follow the operating system: a signal is reported as `128 + signal` (`143` for
`SIGTERM`, `137` for `SIGKILL`), a normal exit is its own code, and a deliberate stop sets the
detail `stopped`.

## Invocations

Every app is a singleton: at most one live run exists per installed application. A launch
therefore does one of two things:

- **Spawn** — the app has no live run; a new supervised run is created. The app receives
  `AppLaunchInvocation` after it reports ready.
- **Invocation** — the app already has a live run; the launch is delivered to that run as
  `AppLaunchInvocation` instead of starting a second process.

`AppLaunchInvocation` = `{invocation_id, arguments:[{name, value}]}`. Arguments are validated
against the manifest before anything is spawned: undeclared names, wrong types and missing
required arguments fail the launch with `INVALID_ARGUMENT`. Values are never shell-parsed.

Delivery rules:

- A ready run receives the invocation immediately.
- A starting run holds it in a per-run queue bounded at **8**; a ninth returns an error and the
  launch fails. The invocation expires after **30 s** if the app never becomes ready, with a
  logged warning.
- A run that is stopping refuses the launch with `FAILED_PRECONDITION`; launch again after it
  exits to start a fresh run.
- Concurrent launches are serialised by the launcher. A burst against an app with no live run
  starts one process; further launches are delivered to it as invocations, subject to the queue
  bound above.
- `invocation_id` is also returned to the caller (`LaunchResult.invocation_id`), so it can
  correlate the outcome.

Report the outcome with `AppInvocationResult{invocation_id, accepted, detail}`. It is a
notification: the framework logs it and no error comes back if you skip it.

```go
Invocation: func(ctx context.Context, inv *capsulad.LaunchInvocation) {
    // apply inv.Arguments, then:
    _ = client.NotifyInvocationResult(inv.ID, true, "applied")
},
```

## Assistant activations

An assistant app is a normal app that claims the `voice_assistant` role. It becomes the device's
assistant when it is selected, or when a wake word is **bound** to it: the operator binds each
installed word to one app, so different words can start different apps. Turns arrive as
`AssistantActivation` on the app bus:

| Field | Meaning |
|---|---|
| `activation_id` | Identity of this turn. One-shot; never reused. |
| `origin` | `ACTIVATION_WAKEWORD`, `ACTIVATION_TOUCH` or `ACTIVATION_API`. |
| `matched_model_id` | The wakeword artifact id that matched. Empty for `ACTIVATION_TOUCH` — the wake-word manager's internal placeholder id is never delivered to an app — and for an API activation the daemon trigger did not name. Only `origin` is keyword evidence. |
| `monotonic_time_ns` | When the framework decided to activate. |
| `expires_monotonic_ns` | The window in which the turn and its pre-roll are valid. |
| `hold_to_talk` | True for a manual (touch/key) activation while the device is in hold-to-talk mode. |
| `app_id` | Always set: the app this activation was delivered to (the bound app, or the selected assistant when the word is unbound). |

Behaviour an assistant must implement:

- The framework does not launch the assistant per turn. It is already running and ready.
- A word bound to an app is delivered to that app only. If that app is not ready the activation is
  logged and dropped with an explicit reason; the framework never reroutes it to the selected
  assistant or to another word. Unbound words and manual keys target the selected assistant; an API
  activation of a bound word is delivered to that same app, exactly like a detection.
- Use `origin` — never `matched_model_id` — as keyword evidence. Only `ACTIVATION_WAKEWORD` means
  a word was heard; a manual (touch/key) activation is `ACTIVATION_TOUCH` with an **empty**
  `matched_model_id`, so an app that treats any non-empty model id as a keyword claims to have heard
  a word on a key press. `hold_to_talk` likewise belongs to the manual activation and is set from
  the device's touch mode.
- The app decides when to begin capture; beginning late loses the audio before it opened.
- Passing `activation_id` when opening the microphone attaches the bounded pre-roll for that
  turn. Only the app that received the activation can use it, and only once.
- With `hold_to_talk=true`, the release arrives as `AssistantInputReleased{activation_id}` on
  the same stream and ends input. In summon mode the turn may outlive the hold.
- The app is responsible for ending the turn: close the microphone and conversation streams.
- The circle/play-pause key dismisses the turn that currently owns a capture, playback or ring
  resource among the selected assistant and the bound apps, through the app's declared `dismiss`
  action. Detection pauses while any of those apps holds audio, so a bound word's reply cannot
  re-trigger detection either.

## Settings delivery

| Moment | What the app receives |
|---|---|
| Right after `AppReady` | `AppSettingsChanged{revision, values}` with the current snapshot, including unmasked `text_masked` values. |
| Any accepted update | `AppSettingsChanged` with the new revision and values. |
| Update while the app is not running | Nothing is queued; the value is stored and arrives at the next ready. |
| Update to a compiled-in system app | Not applicable; system apps read the settings store directly. |

The app-private event is the **only** surface that carries `text_masked` values in clear; the
generic `Get` and the Web UI redact them. Values are JSON-encoded strings
(`"text"`, `true`, `42`, `1.5`); a key with no stored value is simply absent, which means "use
the declared default".

## Actions

`ApplicationConfigurationService.TriggerAction` (or the Web UI) reaches a declared action:

- The app must be **ready**; triggering an action on a non-running app fails.
- The action ID must be declared in the manifest; anything else fails.
- The app receives `AppActionInvoked{action_invocation_id, action_id}`.
- `AppActionResult{action_invocation_id, succeeded, detail}` is optional and is a notification.

Actions are not settings: nothing about an action is persisted, and `confirm_before_run` is a
UI behaviour only.

## Stop, restart and backoff

### Deliberate stop

`ApplicationService.Stop` and `capsulad-cli app stop RUN_ID` ask the run to end:

1. The supervisor sends `SIGTERM` and moves the run to `APP_STOPPING`.
2. If the process is still alive after **5 s**, it is killed with `SIGKILL`.
3. The run becomes `APP_EXITED` with detail `stopped`.

A deliberate stop is not a failure and does not trigger a restart — **unless** the app is the
operator-selected default provider for a resource or the assistant role. In that case the
framework restarts it, because a selected provider must remain available; deselecting it,
uninstalling it or shutting the daemon down is the durable off switch.

Shutdown behaves the same way for every run (SIGTERM, then kill), with a 5 s total shutdown
budget; shutdown cancels the restart context first, so nothing is restarted.

### Failure

These are failures, not stops:

- a non-zero exit;
- death by an unhandled signal;
- losing the framework connection (the daemon stops the orphaned process and applies the
  restart policy).

A failed run is `APP_FAILED` and its restart depends on three things:

| Condition | Restarts? |
|---|---|
| `restart_on_failure = true` | yes |
| the app is the selected default provider or assistant | yes, even with `restart_on_failure = false` |
| neither | no; the run stays failed |

A process that exits by itself with status **0** is a normal exit, not a failure: it reports
`APP_EXITED` with exit code 0 and an empty detail. The `stopped` detail is reserved for a stop the
framework requested. `restart_on_failure` still governs a self-exit, because the supervisor
cannot distinguish an intentional idle exit from an early clean failure: with the flag **true**
the app is relaunched (subject to the backoff below), so an app designed to leave when it has
nothing to do must declare `restart_on_failure = false`. A selected default provider is relaunched
regardless, so an idle-exit design does not suit that role.

### Backoff

Repeated failures back off so a crash loop cannot spin the process table:

| Consecutive failure | Delay before restart |
|---|---|
| 1st | immediate |
| 2nd | 1 s |
| 3rd | 2 s |
| 4th | 4 s |
| 5th and later | 8 s |

A run that stays READY for **30 s** resets the counter, so an app that ran well and then crashed
restarts immediately. A run that reports ready and dies at once keeps accumulating. A scheduled
restart re-checks the app at fire time: if a launch started a run while the backoff was pending,
that run is the recovery and the scheduled start is skipped, so one app never has two live runs.

### Boot

On daemon start, framework-owned compiled-in helpers and selected compiled-in providers start
first, then every installed app with `start_on_boot = true` or `background = true`, then the
configured default provider for each resource and the selected assistant. A built-in provider is
started only when selected, regardless of its manifest's `start_on_boot` or `background` flags;
framework-owned helpers are marked explicitly and start independently of selection. An app that is
already running is not started twice.

For installed apps, `background = true` therefore has two effects: the app is started at boot, and
deselecting it does not stop it. `start_on_boot = true` gives the same protection from deselection.

## Graceful shutdown: what an app must do

Every terminal path — stop request, lost connection, crash — must leave the framework able to
clean up. An app is expected to:

1. Stop producing output and close presentations (`ClosePresentation`, then close the stream).
2. Release captures (`ReleaseCapture`, or close the capture stream).
3. Close audio streams: a playback `Close` drains, `Abort` discards; a capture close releases
   the microphone and its indication.
4. Close conversations.
5. End every inbound call (`AppCallReply{end}`), even on error.
6. Close the bus (`client.Close()`) and exit.

The framework also cleans up after a dead process: presentations, captures, audio streams,
pending invocations and touch ownership are released when the run ends. That cleanup is a
safety net, not a substitute — a stream held until the process dies keeps a resource occupied
while the app is still alive.

## What the app sees as logs

The framework collects the app's `stdout` and `stderr` as structured log records:

- component `app.stdout` or `app.stderr`, severity `info`, `app_id` set to the app;
- one record per line, each line truncated to 8000 characters;
- a single line longer than 64 KiB is a hard failure, not a split: the reader stops at that
  point and the rest of that stream (stdout or stderr, independently) is dropped for the life
  of the run. Once the pipe buffer fills, further writes by the app block.

Emit short lines ending in `\n`; never log one large blob. Chunk long payloads yourself (for
example every 1000 characters on separate lines) and write diagnostics to the stream you still
need.

`capsulad.log(message)` (Go: `log.Print`/`fmt.Fprintln`) writes to `stderr` with the prefix
`capsulad-script:`, so it appears as an `app.stderr` record. The Go and Rust bindings' log
callbacks are local hooks; they do not forward to the daemon.

An app can read **its own** records through `DiagnosticsService.WatchLogs` (pass its own
`app_id` or leave it empty). Any other app ID is `PERMISSION_DENIED`, and framework records stay
administrator-only. Administrators read the same stream with
`capsulad-cli logs -app ID -n N`.

## Assistant and provider lifecycle rules

- A provider is selected by app ID in device settings, never by a port or a stable output
  identifier.
- Selecting a provider starts it if it is installed and not running.
- Deselecting a provider stops it, unless it declares `start_on_boot` or `background`; a compiled-in provider is stopped unless explicitly framework-owned.
- Uninstalling a provider requires it to be stopped first, and removing the selection is the
  durable way to keep it off.
- A presentation is owned by one run; when that run ends, its presentations and captures are
  released and routing recomputes selection.