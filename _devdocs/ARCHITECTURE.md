# Capsula Universal Framework

**Architecture draft 0.5 — November 2026**  
**Status:** working design, not an implemented or stable API. Supersedes draft 0.4 where it differs.

> **Scope of this revision:** mandatory-by-default client-certificate mTLS for the **Web UI**; supervised native / QuickJS / MicroPython application packages; optional GitHub build-provenance attestations for native binaries; three initial Capsula Mini firmware/SoC targets; and synchronized updated Protobuf drafts. All v0.4 touch, sandbox, presentation, audio, and application-bus decisions remain in force.

## 1. Goal and principles

A cooperative, language-agnostic **local application framework and hardware abstraction layer** for jailbroken VK Capsula devices. Applications communicate using Protobuf/gRPC **only with the framework's Unix domain socket**. `capsulad` launches and supervises every managed app, owns physical hardware, routes/mixes apps' logical resource usage, and serves a straightforward browser-based settings UI.

The homebrew application ecosystem is cooperative. There is **no per-application permission system or RBAC**. Nevertheless, the runtime enforces a **uniform process and read-only filesystem sandbox** for reliability and isolation, and the framework documents resource conflicts instead of trying to infer developer intent. Apps may use TCP/UDP networking (including inbound listeners) even though inter-app IPC uses only the daemon.

Design principles:

1. **One local framework socket:** `unix:///run/capsulad/hal.sock`; no network gRPC API and no per-app IPC listeners. The **separate HTTPS Web UI** requires mTLS client certificates by default, not API keys.
2. **Supervised isolated processes:** every app is started by `capsulad` as a native binary, QuickJS script, or MicroPython script, runs unprivileged with an isolated read-only filesystem view, and connects to the single daemon socket (script apps via the framework runner).
3. **Dynamic presentations:** apps create display/ring presentations at runtime, with no declared output ports or stable output IDs. Presets and raw frames are *contents of presentations*, not resources.
4. **User-selected defaults:** choose an app for segment display, ring controller, and voice-assistant role; built-in fallbacks where appropriate.
5. **Temporary explicit captures:** last **frame-ready** capture wins for display/ring; release is explicit in normal operation, and cleanup follows process exit. Full and partial touch captures are mutually exclusive *modes* of the same touch resource.
6. **System owns hardware:** it mixes playback, fans out capture, interprets touch gestures when not fully captured, and selects visible outputs. Apps do not direct other apps' behavior.
7. **System wakeword detection:** the selected assistant stays running and idle, receives activation events, and may immediately open mic capture, subject to the visible-indication watchdog.
8. **Simple settings:** framework/device and app settings are typed fields, including masked text; app-defined actions are buttons. No user-written routing rules.
9. **Brokered app-to-app APIs:** all inter-app calls are relayed through `capsulad`, with descriptors and agent-readable docs; media player is the first optional standard interface.
10. **No app filesystem writes:** app code, shared assets, runtime dependencies, and other framework-provided files are readable but immutable to apps. Logging and persistent settings go through the daemon, not arbitrary file writes.
11. **Initially Capsula Mini:** support Rockchip jailbroken stock firmware, Amlogic jailbroken stock firmware, and Amlogic Alpine Linux firmware; defer larger Capsula/Neo interaction backends.
12. **Optional verified origin:** native packages may carry GitHub artifact attestation evidence; the framework verifies and displays provenance status, never equating it with a guarantee of safety.

## 2. Process and IPC architecture

```text
  Browser Web UI
       |
       v
  local UI backend -------+      Clock app     Weather app      Ring app
                          |           \          |           /
  Voice assistant --------+------------\---------+----------/
  AirPlay/media app -------+------------- gRPC client connections
                          |             over the SAME Unix socket
                          v
                 +-----------------------+
                 |       capsulad        |
                 |                       |
                 | process supervision   |
                 | app connection/bus    |
                 | settings + action UI  |
                 | resource routing      |
                 | audio mixer/fan-out   |
                 | wakeword detection    |
                 | logs/diagnostics      |
                 +-----------+-----------+
                             |
                 native backends / stock daemon bridges
                             |
                    physical Capsula hardware
```

Every application is started by the framework from its installed manifest, inside an unprivileged sandbox (see §8). The process connects outward to `capsulad`'s **single** UDS endpoint, opens an `AppBusService.Connect` stream, identifies the launcher-assigned `run_id`, and signals `READY`. The daemon associates that connection with the process it launched (using its process registry and local peer credentials); this is identity bookkeeping, **not an app permissions model**.

All app-to-app calls are routed through `AppBusService.Call`: caller → `capsulad` → callee's established `Connect` stream → `capsulad` → caller. The daemon does not interpret custom protobuf payloads, only routes them using method descriptors and the chosen destination `app_id`. One call can contain multiple request and response messages, so the optional standardized media `WatchPlayers` stream still fits. The bus must implement deadlines, cancellation, bounded buffering/flow control, and predictable termination on disconnect. Only a supervised, `READY` app is callable; when it restarts, in-flight calls fail and new calls can target the app's stable ID.

This means a third-party app does **not** run its own inter-application gRPC server. The framework's SDK adapts ordinary protobuf service handlers to reverse-RPC messages on `Connect`. Without that helper, an app can use the Protobuf envelope messages directly. It is not transparent standard gRPC-to-app-server forwarding; callers use the framework's routed-call interface.

Apps may open outbound connections or listen on **network** TCP/UDP ports; this does not make them IPC servers for the framework. The browser cannot directly call a Unix socket, so a small UI backend speaks the same local API. The Web UI is deliberately a **different network surface** from the HAL RPC endpoint. It uses HTTPS with **mandatory client-certificate verification by default**, including when accessed over a LAN. A browser supplies a client certificate issued by a framework-trusted CA; this replaces API keys for the Web UI. TLS server authentication and client certificate verification are both required. There is **no remotely exposed gRPC HAL or app bus**.

### 2.1 Web UI mTLS and enrollment

- The HTTPS listener refuses clients without a valid trusted client certificate and validates the server certificate in browsers. Self-signed defaults without a trusted CA are not silently accepted.
- A **local bootstrap mechanism** (administrator CLI or physical provisioning flow, not an unauthenticated remote endpoint) initializes the CA/server credentials and issues the first client certificate. The resulting client certificate/private key must be installed into the browser/device securely; do not serve it over an unauthenticated Web UI route.
- Support explicit client certificate rotation and revocation/removal, server certificate renewal, and clear diagnostics for expired/rejected credentials. The exact CA lifecycle is an implementation decision; no API-key fallback is part of the default UX.
- Client certificates are **Web UI administrative access credentials**, not per-application permissions or a new application RBAC model. Session/cookie protections and CSRF defense remain necessary for browser actions; mTLS does not automatically eliminate browser-origin attacks.
- Trusted CA fingerprints and non-secret TLS status may be shown in diagnostics. Server and client private keys must never be returned through framework app APIs. The normal Web UI backend talks to the same local Unix socket as other clients.
- LAN reachability/firewall configuration and browser-certificate import UX should be tested with the actual expected browsers and clients before shipping.


## 3. Resources and initial ownership

| Resource | Initial presentation/handler | Sharing / capture semantics |
|---|---|---|
| Segment display | User-selected default app (bundled clock initially) | Explicit temporary captures select a different app's ready presentation; system applies global brightness |
| Voice assistant role | User-selected always-running assistant | Another assistant can temporarily capture activation destination; framework detects wakewords |
| Touch panel | Framework interprets volume, microphone toggle and primary/gesture input | **Partial** capture routes only non-system gestures to an app; **full** capture routes raw readings and disables all built-in touch interpretation |
| LED ring | Framework mic-state fallback, or user-selected ring controller | Apps temporarily capture ring; presentation contents may be frames or built-in effects |
| Speaker | No initial owner | Multiple playback streams mixed; background streams duck during an active assistant conversation |
| Microphone | Framework physical capture/wakeword service | App capture streams fan out where supported; visible indication watchdog bounds unindicated access |

Stable IDs: `app_id` identifies installed software; `resource_id` identifies a discoverable physical or logical resource; `run_id` identifies a running process. `presentation_id`, `capture_id`, `conversation_id`, and `invocation_id` are **runtime-only IDs**. No stable source or declared output port is necessary.

Defaults store `resource_id -> app_id` rather than `resource_id -> output_id`. Manifest `provides` hints (for example `segment_display_provider`, `led_ring_controller`, `voice_assistant`) populate settings dropdowns; they do not grant permissions or prohibit other app behaviors.

### 3.1 Touch panel: exactly two capture modes

Touch capture is **not** a collection of individually capturable volume, microphone and primary keys. It has **two modes**, both operating on the same panel:

| Mode | Data delivered to capturing app | Framework's ordinary touch controls |
|---|---|---|
| **Partial / gesture** | Semantic non-system gestures: primary circle/tap, swipe, center hold, double/triple tap, and other supported gestures | **Retained:** volume adjustment and microphone toggle continue to be interpreted by the framework |
| **Full / raw** | Raw touch readings, such as positions/coordinates, pressure/contact state or controller samples **where the actual device exposes them** | **Disabled for the duration of full capture:** the framework does not interpret *any* panel event for volume, mic toggle, summon or other built-ins |

On Capsula Mini, the circle button's tap maps to the semantic primary action; on a larger Capsula, a suitable touch-panel tap does. The precise gestures and raw sample representation are **capabilities discovered per hardware backend**. Some devices or stock-driver bridges may offer discrete events only; full capture must report that limitation rather than promise coordinates the hardware cannot provide.

- **No standalone capture API exists for volume controls or the microphone-toggle event.** Apps cannot take either of those individually. In partial mode, the app also does not receive volume/mute gestures as routed input; these are framework-handled. Optionally observing diagnostics is distinct from controlling them.
- Captures use **last-capture-wins** ordering on the panel, but each effective capture selects **one entire mode**, never a hybrid. A new full capture supersedes an older partial capture; on full release, the older partial capture may resume. Conversely, a newer partial capture supersedes an older full capture and **restores framework volume/mute gesture handling** while it is effective. No app receives selected raw touch events while another receives selected semantic events from the same panel.
- A contact/gesture already in progress is kept with the handler selected when that contact began, or cancelled cleanly on capture-mode transition. Never send an unmatched press to one owner and release to another.
- Full touch capture disables the *touch gesture* for microphone mute; it does **not** remove the framework's global microphone mute state or its Web UI/API control. The user can still use those controls, and disabling/muting the microphone affects all consumers.
- If raw input cannot be provided on a particular device, the capability advertises that and a full/raw capture fails explicitly or offers only the documented native raw events; it must not silently degrade into partial gesture capture.

## 4. Dynamic presentations, effects and routing

An app opens `PresentationService.Open(resource_id)`, receives a runtime presentation ID, and writes content to it. It may continue updating while hidden. A presentation becomes **eligible for physical selection only after its first valid content** (raw frame, recognized built-in effect, or display face). An explicit blank frame is valid content; merely creating a presentation does not blank hardware.

A default display/ring app maintains at most **one implicit baseline presentation per resource**. The first eligible presentation from that app becomes the baseline; the app should keep it alive. Additional presentations may exist and be explicitly captured. If a provider unexpectedly creates multiple baseline candidates, report the contract error in logs; don't invent application-specific arbitration rules.

**Routing order:**

1. Latest acquired **eligible** live capture for the resource.
2. Otherwise the eligible baseline presentation of the user's configured default application.
3. Otherwise a safe framework fallback (e.g. system microphone-state ring presentation or blank segment display).

Captures are explicitly acquired against a live presentation and **explicitly released** in normal operation. Captures may end out of order. Closing a presentation destroys its captures, and process disconnection/exit also cleans them up. A stuck, still-running app that never releases remains selected until user force-release or stop; provide diagnostics rather than automatic non-mic capture timeouts.

For example, Clock sends its initial time frame and stays ready; Assistant opens a presentation, captures the segment display, supplies an animation, then releases. Clock's most recent frame returns without having stopped updating.

### 4.1 Framework-provided effects and faces are presentation values

Built-in LED ring effects and segment display faces are **predefined content values for an existing presentation**, not new services, apps, output sources, or capture types. At the SDK level the intended use is:

```python
ring = hal.open_presentation("light.ring.main")
ring.apply_effect(led_effects.spinner, speed=1.0, color="#0088ff")
capture = ring.capture()             # required only if taking temporary control
# ...
capture.release()                   # same explicit lifetime as raw frames

face = hal.open_presentation("display.segment.front")
face.apply_face(display_faces.neutral)   # e.g. "- . -"
```

These are **illustrative SDK calls**, not a frozen protobuf shape. The wire API should update the content of an existing `presentation_id` with a tagged value such as `RingFrame`, `SegmentFrame`, `BuiltinRingEffect` or `BuiltinSegmentFace`. Replacing an effect with a raw frame (or vice versa) **does not change presentation or capture ownership**. There is no `CreateEffectSession` or separate routing mechanism.

The framework executes built-in animations and adapts supported presets to the specific LED geometry/segment layout. Discoverable preset IDs and typed parameters might include `ring.spinner`, `ring.pulse`, `ring.breathe`, `ring.rainbow`, and `segment.face.neutral`, `segment.face.sleepy`, `segment.face.surprised`. Names are provisional; preset availability and parameter support are capabilities, not assumptions for all Capsula models. Unknown or unsupported presets return a clear error, with no silent substitutions. Arbitrary app-rendered raw frames remain supported.

A built-in effect can keep animating while its presentation is active, including when the app is not sending repeated frames. For microphone indication, only an actually **visible and perceptibly active** app-owned indication counts (see §5); a hidden effect is insufficient. A static face/frame may need periodic app refreshes to satisfy that watchdog.

**System brightness** is configured by the user and applied after selecting segment content, even if an app has captured the display. App presets cannot override global brightness. The ring's system microphone-state presentation and mute-transition indicator are rendered through the same general mechanism.

## 5. Audio and microphone indications

The framework captures each physical audio source once, fans out available microphone taps, and mixes independent PCM playback streams. Hardware capabilities describe available capture formats, processing taps, and output formats. Realtime audio workers do not wait on gRPC; bounded queues report discontinuities to slow clients. Playback streams ordinarily mix at normal relative gain.

**Automatic audio ducking is fixed built-in behavior**, not an app request or a user preference. The active assistant may open a lightweight conversation lifetime signal for the mixer. During it, its associated playback remains at normal relative gain; other streams are attenuated using a short fade. Restoring means removing the temporary gain multiplier, not resetting a saved master volume. Merely being the selected, always-running assistant must not duck media.

### 5.1 Wakeword detection and capture

Framework-provided wakeword detection continuously uses system-managed microphone capture when enabled. An activation event goes to the ready selected assistant through `AppBus.Connect`; the framework preserves a short bounded pre-roll tagged with the activation ID. The assistant can immediately open a microphone stream and request that pre-roll, but no mandatory assistant acknowledgement or interaction-state machine is imposed. Starting late may lose audio; there is no indefinite queue.

### 5.2 Microphone capture watchdog

For **app-owned mic capture**, the framework checks for a *currently visible* LED ring or segment display indication belonging to the same app, with fresh visible updates (or a continuously perceptible framework-executed effect). A newly started capture gets a short grace period. If the app provides no qualifying visible/audible fallback indication within the watchdog window, terminate its microphone stream and log why. A presentation hidden by another app, a turned-off/unavailable display or brightness zero does not count. A static frame requires explicit periodic refresh if the implementation uses frame freshness.

The purpose is a best-effort observable-listening safeguard, not proof that the visible content semantically means “microphone in use.” The watchdog is **not** satisfied by merely owning a display, and the safety check is independent of touch capture mode. Device variants without usable indications may require stricter mic-capture duration or a framework-managed audible indicator; timing and fallback values remain open for hardware testing. Internal wakeword capture is exempt.

The hardware/software **global microphone mute** is system-owned. Muting stops framework wakeword processing and app capture as far as supported, regardless of which app owns touch input or LED presentations.

### 5.3 Microphone-disabled indicator timeout (user setting)

The **microphone-disabled/muted indicator** and the **active app microphone-capture indicator** are different behaviors. Add one simple device setting, *Hide muted-microphone indication after*, with options such as *Immediately*, *10 s*, *30 s* (proposed default), *60 s*, and *Never*. The setting controls only how long the framework visibly indicates a change to **muted** state; the microphone stays muted indefinitely until explicitly unmuted. `Never` keeps the disabled indication shown as long as mute remains enabled, subject to capture/routing visibility.

The framework implements the muted-state indication as a temporary system-owned ring presentation/capture over the selected baseline. It releases that temporary capture when the timer expires, restoring the selected ring controller automatically. A new mute transition can restart the timer. Other captures obey the same normal last-capture-wins semantics, so diagnostic logs should distinguish “indicator expired” from “indicator obscured.” The timer does **not** shorten or waive the app microphone-capture watchdog, and hiding a *muted* indication must never be interpreted as unmuting the microphone.

## 6. Voice assistant role

A selected/default assistant is a **long-running background application**, started and supervised by the framework. It stays connected and idle, receiving activation events for wakeword, touch gesture, or local API summons. The assistant handles its own listening, recognition, response, TTS and visual animations; the framework is intentionally hands-off.

- `role.voice_assistant` may be temporarily captured by another always-running assistant; last capture wins for new activations.
- App capture of microphone begins whenever the assistant calls the normal audio API. The assistant typically also captures ring and/or segment display while listening and submits a visible indication; the microphone watchdog checks that independently.
- Conversation lifetime is only a small framework-visible signal for audio ducking and optional association of playback streams; no enforced conversation state machine or required activation acknowledgement.
- Touch hold mode is a **simple settings choice** for framework-interpreted gestures (normal or partial touch mode; unavailable while full/raw touch is captured): `Hold to summon` sends one activation and lets conversation continue after release; `Hold to talk` sends activation on press/hold and a corresponding **release event**, which the assistant handles by stopping or finishing listening. The framework does not kill the assistant's process on touch release.
- Activations received during an existing conversation are handled by the assistant or ignored under a simple framework default; richer assistant routing is deferred.
- Pre-roll is bounded and best effort. The app's own performance determines whether it consumes it in time.

An app can be selected as default only once installed; selection/startup should make it ready at boot or whenever the preference changes. A crash triggers normal supervisor restart/backoff, but an activation missed while unavailable is simply logged/dropped, not replayed later.

## 7. Application installation, lifecycle and invocation

An installed package has a manifest with stable app ID, **runtime and relative entrypoint**, display name/version, background/start-on-boot and restart-on-failure flags, singleton flag, optional `provides` hints, typed launch arguments, typed settings, action buttons, and custom service descriptors/docs. There are **no static presentation/output ports**. Framework launches registered native executables or its own fixed script runner (no arbitrary shell command RPC) inside the standard sandbox in §8.

Application states: `STARTING` → `READY` → `STOPPING` → `EXITED`; `FAILED` represents abnormal exit/start failure. The daemon handles spawn, graceful stop, restart backoff, and cleanup for a terminated `run_id`. An app connects to the single daemon UDS and sends `AppReady` on its persistent bus connection. That is all the inter-app endpoint readiness needed.

Apps launch through a voice assistant, the Web UI, or the local API. Other apps can call `ApplicationService.Launch`. Launch arguments are named, typed and validated against the manifest, never interpolated into shell commands.

For a `single_instance` app:

- If not running, `Launch` spawns it and delivers the invocation once the process becomes ready.
- If already ready, `Launch` delivers `AppLaunchInvocation` with the new arguments to that *same* process rather than creating a second one.
- The app can asynchronously accept, reject or complete an invocation. Delivery alone does not mean its work completed.
- A bounded queue with expiry may hold invocations while the singleton starts. It never waits forever.
- Multi-instance apps can spawn additional supervised processes; default long-running display, ring and assistant providers should be single-instance for predictable defaults.

Apps can open/listen on **network sockets** as normal unprivileged Linux processes; the framework does not proxy network traffic. They do **not** run independent inter-app IPC listeners and must not open filesystem Unix sockets in their read-only sandbox. Inter-app APIs exclusively use the framework's reverse-RPC bus.

### 7.1 Background/always-running roles

The selected default segment provider, selected ring controller (if any), and selected assistant are typically always running. `background` is a lifecycle policy, not a special executable type or a list of app capabilities. Ordinary applications can run on demand. Optional standard media-player methods are described in §12.

### 7.2 Application package runtimes

A package chooses **exactly one entrypoint runtime**. Every package remains a supervised *isolated OS process*, regardless of whether its code is native or scripted:

| Runtime | Package entrypoint | IPC implementation | First-party developer support |
|---|---|---|---|
| `native` | Executable ELF for the correct CPU/ABI | Library connects to the single local gRPC UDS | **Go and Rust SDKs first** |
| `quickjs` | Read-only `.js` script | Framework-provided QuickJS runner/host bindings bridge to the daemon | Framework JS host API |
| `micropython` | Read-only `.py` / supported `.mpy` script | Framework-provided MicroPython runner/host bindings bridge to the daemon | Framework Python host API |

Scripts **do not need to implement gRPC HTTP/2 or Protobuf transport themselves**: their native interpreter runner owns the persistent local framework connection and exposes typed bindings for HAL, app-bus calls, activation, settings, presentation updates and launch events. All reverse-RPC custom-interface behavior must eventually be expressible through those bindings. Script code does not listen on a separate IPC socket. The interpreter runner is part of that app's sandboxed process, not a privileged shared interpreter for all apps.

Native packages declare CPU architecture and binary ABI (for example, `armv7`/`aarch64`, `linux-gnu`/`linux-musl`) so the daemon can reject incompatible binaries before spawning. Script runners also must be compiled for each supported firmware/architecture combination. A binary may have build-specific package variants; no automatic cross-architecture execution is implied.

All runtimes inherit the **same** no-filesystem-writes contract (§8). Disable Python bytecode caches, QuickJS/MicroPython runtime file output and other implicit writes. Runtime dependencies and imported modules must be packaged read-only or supplied by a documented read-only runtime image. Network client and TCP/UDP listener capability remains supported wherever the interpreter's host bindings provide it. Do not promise parity with CPython, Node.js, or all native extension modules. Publish a tested runtime feature matrix and SDK versions.

Illustrative manifest entries:

```toml
[entrypoint]
runtime = "native"          # native | quickjs | micropython
path = "bin/weather"        # relative to immutable /app/
architecture = "aarch64"    # native only
abi = "linux-musl"          # native only
sdk = "0.5"

# Alternative scripted package:
# [entrypoint]
# runtime = "quickjs"
# path = "main.js"
```

### 7.3 Optional GitHub build provenance for native binaries

A native package **may** provide a GitHub Artifact Attestations/Sigstore bundle and expected repository/workflow identity. The declaration does not itself count as proof. The framework verifies the evidence against the **actual shipped executable's cryptographic digest**, signing identity, expected source repository/workflow (and repository ID where available), and suitable trusted roots; it records a computed verification result. A manifest cannot declare itself verified.

```toml
[github_attestation]
subject = "bin/weather"
bundle = "provenance/build-bundle.jsonl"  # packaged read-only evidence
repository = "example/weather"            # expected signing source
workflow = ".github/workflows/release.yml" # optional stricter matching
```

Display a non-deceptive status in the Web UI: **Verified provenance / Verification failed / Not supplied / Could not verify**. Show the verified source repository, workflow/commit and subject digest where available, but avoid a generic “safe” or “official developer” badge. An attestation confirms build origin/integrity under the chosen trust policy, **not** that source code is harmless, a developer account is uncompromised, or runtime behavior is safe. Verification failure should be visible and logged; enforcement/blocking is not part of the initial cooperative-app model.

Evidence may be packaged with the app and verified offline with an up-to-date trusted root; alternatively a distribution tool may fetch evidence on install. Preserve clear provenance verification errors and do not depend on GitHub being reachable at application startup. Initially this feature is **native-binary-only**; script packages report unsupported/not supplied, not an artificial verified status.

References: [GitHub artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations), [offline verification](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/verify-attestations-offline).

## 8. Application sandbox and filesystem contract

**Apps cannot write to the filesystem.** This includes the installed app tree, shared assets, host filesystem, and private writable directories: **there is no writable `/data` or `/tmp` mount** and no generic filesystem write-proxy API in v0.5. Apps remain fully capable of in-memory work, network communication, and framework RPC.

The daemon uses per-app (ideally per-process) unprivileged identities and an isolated mount namespace. Do **not** put every app under the same shared `nobody` UID: distinct identities prevent accidental cross-process interference. A possible filesystem view is:

| Path inside sandbox | Availability | Semantics |
|---|---|---|
| `/app/` | Read-only | App's executable, code and bundled resources; may include libraries shipped with that app |
| `/assets/` | Read-only | Framework-provided shared assets (§11) |
| `/run/capsulad/hal.sock` | Connect only | One daemon-owned Unix socket for HAL and app-bus communications |
| Minimal runtime libraries, loader, trust/DNS config | Read-only, where necessary | Needed for execution and outbound network connections; exposed only as a deliberate runtime image |
| Minimal virtual kernel interfaces | Restricted | e.g. safe `/dev/null`, `/dev/urandom`, limited `/proc`; no direct physical hardware nodes |
| `/tmp`, `/var/tmp`, `/home`, app writable storage | No writable filesystem | No general-purpose file persistence or scratch files |

The daemon **may** write on an app's behalf for features it explicitly owns:

- **Persistent settings:** manifest-declared typed settings, stored by the daemon and delivered over the app bus (§9).
- **Logging:** stdout/stderr collected by the supervisor and/or structured `Log` RPC, with the daemon writing its own log storage. Log messages are not a general file-writing channel.
- **Framework-managed assets:** installed/updated by the framework or administrator, always read-only to apps (§11).

This is intentional: applications needing databases, file caches, temporary disk files, downloads, or arbitrary persistent state cannot use them directly in v0.5. A future *explicit* framework-managed storage API may be considered once a concrete use case exists; do not silently grant `/tmp` or writable home directories. An application needing a temporary working area should use memory when possible; runtimes that mandate temporary files will need adaptation or will be incompatible.

**Network:** allow outbound connections and listening on TCP/UDP ports, including ordinary DNS/TLS dependencies. Applications may use normal socket APIs and exchange bytes over network connections, but they may not write Unix-domain socket pathname files or any other filesystem object. This does not create a network-facing HAL API. Actual port reachability depends on host firewall/network setup; privileged ports and raw sockets need not be available.

**Enforcement:** use available mount/user namespaces and other kernel controls as verified per board/firmware (e.g. seccomp and read-only bind mounts); do not claim stronger isolation than the target kernel provides. Maintain a fallback/compatibility table for devices without required namespace features. Sandboxing is a uniform runtime contract, **not a per-app permission/approval workflow**. The cooperative app threat model remains unchanged.

## 9. Typed application settings and buttons

As apps have **no writable data directory**, the framework persists an app's configuration, validates it using a **simple manifest-defined schema**, renders it as a form in the Web UI, and forwards changes to its managed app. No custom frontend is required.

Supported setting types for the initial API:

| Type | UI | Example |
|---|---|---|
| `text` | text field | Weather city name |
| `text_masked` | obscured text field | Weather service API key |
| `boolean` | toggle | Clock blinking colon |
| `integer` / `number` | numeric field or slider with bounds | Update interval |
| `select` | dropdown with fixed choices | Time format |

Optional fields: label, description, default value, required, valid range/choices, restart required. `text_masked` is a **display and accidental-disclosure convention**, not a security boundary between mutually trusted homebrew processes. The Web UI and app-catalog APIs never return the stored key value; they return only `has_value`. The app receives its own configured key via its framework connection. The daemon stores local configuration with restrictive file permissions in its **own** filesystem context, and never put secret values in descriptors, logs, or event histories. An unchanged masked field does not reset the stored secret; clearing requires an explicit action.

**Pushbuttons** are separately declared **actions**, not persistent settings. For example `Reload`, `Disconnect`, and `Reconnect` produce an `AppActionInvoked` event on that app's connection. They can optionally request confirmation and return an asynchronous result/status. The framework does not try to understand the meaning of arbitrary app actions.

Example (illustrative TOML manifest schema):

```toml
id = "org.example.weather"
name = "Weather"
background = true
single_instance = true
provides = ["segment_display_provider"]

[entrypoint]
runtime = "native"
path = "bin/weather"
architecture = "aarch64"
abi = "linux-musl"

[[settings]]
key = "city"
label = "City"
type = "text"
default = "Kazan"

[[settings]]
key = "api_key"
label = "API key"
type = "text_masked"

[[settings]]
key = "refresh_minutes"
label = "Refresh every (minutes)"
type = "integer"
default = 15
min = 1
max = 120

[[actions]]
id = "reload"
label = "Reload"
```

Settings changes use compare-and-swap revisions; a partial patch avoids overwriting unrelated fields. Changes are forwarded via `AppBus.Connect`. The daemon maintains the persistent state whether or not the app is running; the app never writes it to a file. This mechanism is for modest typed configuration, not an arbitrary application database. The app can indicate that some settings require restart; the UI can show that information without silently restarting it.

## 10. Built-in presentation preset catalog

The framework provides a **discoverable catalog** of supported LED ring effects and segment display faces with stable identifiers, descriptions and parameter schemas. These are values passed to an **existing presentation update** (§4), not independently capturable resources, applications, or background tasks.

- Ring effect parameters can be typed (e.g. speed, color, intensity, direction), with ranges and device support reported by the backend.
- Segment faces (e.g. neutral `- . -`) map onto hardware-supported segment glyphs/masks; the framework reports unavailable glyphs/presets rather than pretending every model has identical geometry.
- Applications can switch content in-place: a presentation may emit raw frames, then `apply_effect(spinner)`, then a raw frame, with no new capture and no ownership change.
- The daemon runs built-in animations so the app doesn't need to stream every animation frame. It stops rendering that effect when the presentation is replaced/closed (or remains logically animated but hidden if the router selects another presentation, depending on renderer optimization).
- Additions/changes to the built-in catalog should be backward compatible; missing optional presets yield a typed `UNSUPPORTED`/`NOT_FOUND` result.

App-specific custom effects remain possible through ordinary raw frame updates. The framework does not impose an app-facing rendering DSL.

## 11. Framework-provided shared read-only assets

Apps can read common media (primarily short audio jingles/SFX), without having to ship duplicate copies in every app. The daemon installs and manages a shared **read-only `/assets/` tree** inside each sandbox, optionally mirrored by a metadata listing API for discovery.

Illustrative layout (not a frozen asset list):

```text
/assets/
  audio/
    notifications/success.wav
    notifications/error.wav
    assistant/listening.wav
    system/startup.wav
  manifest.json
```

Document asset IDs/paths, media format, channel layout and sample rate, intended semantics, licensing/attribution, and versioning guarantees. Apps may directly decode and play audio bytes using the existing `AudioService`; **there is no need for a separate “play jingle” resource service**. Neither the app directory nor shared assets are writable by apps, and asset management remains a daemon/admin operation. Missing or unsupported optional assets should fail clearly without breaking application startup.

## 12. Inter-application communication and standard contracts

Applications declare their **custom service contracts** in manifests as protobuf descriptor sets (including imports) and human-readable Markdown documentation. `AppInterfaceService` exposes each known service, version, method docs and serialized descriptor. No endpoint URI is published: callers use `AppBusService.Call(target_app_id, grpc_service, rpc_name, serialized_request)` and consume serialized responses. The framework forwards those messages to a ready app over its persistent `Connect` stream. Generic callers can dynamically encode/decode protobuf using descriptors; generated SDKs can make this transparent in ordinary languages.

For agent-readability, documentation should describe method purpose, argument meanings/units, examples, state effects, error cases, and return values. Standard Protobuf descriptors describe structure but not always enough semantics for an agent to call methods well. The framework catalog need not interpret or enforce the custom semantics.

The **optional standard media-player contract**, `capsula.contracts.media.v1.MediaPlayerService`, retains `ListPlayers`, `WatchPlayers`, `GetStatus`, `Play`, `Pause`, `SkipNext`, `SkipPrevious`, metadata (`app_name`, `source_name`, optional `media_name`/title/artist/album), playback state and supported controls. AirPlay and Bluetooth apps may implement it. LED ring controllers, assistants and UI all discover and call the same contract via the bus. An ordinary audio playback app need not implement the media contract.

Ring controllers and display providers can expose arbitrary app-specific methods (`SetMode`, etc.); the framework should not force a fixed controller API too early. Custom methods configure the app, not the resource router. Standard media service definitions remain normal protobuf contracts even though the **transport is a reverse-RPC relay, not a per-app gRPC listening service**.

## 13. Normal user experience

The basic Web UI is an **mTLS-protected HTTPS** settings page (browser client certificate, no API key); it shows ordinary settings as forms, without rule engines or configurable priority matrices:

- **Display:** default segment display app; system-wide segment brightness.
- **LED ring:** default controller (System or installed app).
- **Assistant:** default always-running assistant, wakeword selector/toggle, Hold-to-summon versus Hold-to-talk.
- **Microphone:** global mute control; **Hide muted-microphone indication after** dropdown (immediately / short delay / never). This changes the visual indication only, not mute state or watchdog.
- **Device:** master volume.
- **Applications:** installed/running status, start/stop/restart, runtime/architecture compatibility, verified or unverified native-binary provenance, launch arguments, app-specific typed form including `text_masked`, action buttons, documented methods and logs.
- **Media:** optional current player controls and metadata from installed media contract implementations.

Ordinary users need not know what a capture stack is. The framework applies first-frame readiness, last-capture-wins, restore-previous, automatic audio ducking, microphone indication checks and sandboxing. Advanced diagnostics expose selected app, capture and presentation IDs, last frame/effect, mic watchdog, app run IDs and IPC failures. Touch capture is requested by apps; users do not select individual touch controls or configure volume/mute handling as per-app rules.

## 14. Example interactions

**Clock → assistant → clock:** Clock is the configured default segment app and sends its first frame. The assistant creates a new presentation, applies a built-in face or effect *to that presentation*, captures the display, and becomes visible. When finished, it **explicitly releases** the capture. The current Clock frame reappears. A stuck assistant remains selected until stopped or manually force-released; the logs identify it.

**Touch capture, partial:** A game captures the panel in partial/gesture mode. Circle tap / primary tap and swipes go to the game. Physical touch gestures for volume and microphone mute remain interpreted by the framework. A double tap or hold is delivered only if the hardware backend supports the gesture.

**Touch capture, full:** A touch-driven drawing app captures raw mode. The framework stops interpreting **all** touch gestures (including volume and microphone toggle); the app gets raw samples/coordinates where supported. Releasing raw mode restores the previous partial capture or the default system touch handlers. Master volume/microphone mute still work through Web UI and API.

**AirPlay → assistant:** AirPlay opens an output stream and publishes media metadata. The framework detector sends activation to the always-ready assistant. The assistant opens a mic stream and requests pre-roll; its conversation lifetime causes automatic speaker ducking. It captures the ring/display and applies a visible listening indication. Mic capture is terminated if its qualifying visible indication ceases long enough. On completion the app explicitly releases its presentations and audio resources, and media returns to normal gain.

**Mic muted indication expires:** The framework receives a microphone mute toggle, shows its system muted indication temporarily on the ring, then removes that temporary capture after the user-selected delay; the user's default ring controller returns. The microphone **remains muted**. With the “Never” setting, the disabled indication stays logically active until unmuted.

**Application without filesystem writes:** Weather reads code/config resources from `/app` and its API key from framework settings, makes an outbound HTTPS call, and emits a segment frame through the daemon socket. The app logs errors to stdout/structured log RPC; it cannot create a cache file in `/tmp` or write an API response to disk.

**Launch already-running Weather:** Assistant launches `org.example.weather` with `city=Kazan`. For a singleton already `READY`, the daemon forwards `AppLaunchInvocation` with typed arguments; Weather handles it in-process. Launching alone does not capture the display. A Web UI Reload button similarly emits `AppActionInvoked` and receives a result via the app bus.

## 15. Failure/diagnostic expectations

- A crashed/stopped app loses presentations, captures, conversations and open audio streams; the router immediately selects the next ready source.
- A stuck but live capture stays selected until explicit user force-release or the app is stopped; log app ID, run ID, capture age, last frame, resource and reason.
- A capture without a first valid frame must not blank the previous presentation.
- Live custom RPC calls fail `UNAVAILABLE` if the callee disconnects; queued data is bounded and never replayed into a new run automatically. Callers can retry deliberately.
- A `TEXT_MASKED` value is redacted in ordinary GET responses and all logs. It is transmitted to the owning app only over its runtime connection.
- The microphone watchdog includes log reason, time since last qualifying visible update/effect, capture stream ID, and actual selected display/ring ownership. Muted-indicator expiry is logged distinctly from mic watchdog revocation.
- Wakeword detections while no selected assistant is ready are dropped/logged, not queued indefinitely.
- A slow audio consumer receives explicit discontinuity information rather than blocking hardware capture.
- Full/raw touch capture advertises backend limitations (including absence of coordinates); unsupported raw capture fails explicitly. A pending gesture is not split between handlers on capture changes.
- Apps attempting filesystem writes receive normal OS errors; logs clearly distinguish sandbox denials from app crashes. The daemon retains logging capability.
- Built-in presets are content values; unsupported effect IDs or face glyphs fail with a descriptive capability error, never silently steal capture ownership.
- Vendor-owned drivers may require bridge integrations; actual device capabilities are discovered rather than assumed.

## 16. API layout and implementation plan

The **v0.5 Protobuf drafts** (`proto/capsula/framework/v1/framework.proto`, optional `capsula/contracts/media/v1/media.proto`) now reflect the architectural decisions from v0.4 and v0.5 **at the message/service-definition level**:

- Two exclusive touch capture modes and backend-dependent raw readings; non-system semantic gestures only in partial mode.
- In-place presentation contents: raw segment/ring frames, built-in effects/faces, and an optional preset catalog.
- Global microphone-muted indicator timeout distinct from active capture indication/watchdog.
- Shared read-only asset catalog, typed app settings and action buttons, reverse-RPC app bus.
- Runtime/entrypoint declarations for native / QuickJS / MicroPython, per-target runtime availability, and native-binary attestation verification result.
- Device target platform identification, backend availability, and a read-only Web UI mTLS status report. Certificate enrollment is deliberately **not** an app-facing RPC.
- The media-player standard remains a separate optional app-owned interface, relayed by the framework app bus.

These are **review APIs, not yet a stable v1**. The framework/media/example schemas have been **syntax-compiled with `protoc` 3.13** (with its legacy `--experimental_allow_proto3_optional` flag) and a combined descriptor set has been generated. Generated Go/Rust SDK and integration tests remain outstanding before freezing wire compatibility. Script-host bindings, sandbox implementation, actual hardware availability, certificates, and attestation verification all require working implementations and device tests. See `IMPLEMENTATION_READINESS.md`.

Suggested implementation order:

1. **Simulator first**, then supervised app runner with verified read-only sandbox, distinct unprivileged identities, one UDS socket and reverse-RPC app bus; test log collection and prohibited filesystem writes. Implement native SDK path first, followed by QuickJS and MicroPython host adapters.
2. Mock segment/ring backends with dynamic presentations, first-content readiness, in-place raw/effect/face content updates, and explicit capture/release; simulate Clock → Assistant → Clock.
3. Touch partial/full capture on an emulator/backend: verify system volume/mute in partial mode, suppression in full mode, and clean gesture transitions.
4. HTTPS Web UI with mTLS provisioning and client-certificate enrollment; typed app settings/actions and global microphone-muted indicator timeout; catalog and read-only mount for shared assets. Add native-package provenance verification status and CLI diagnostics.
5. PCM mixing/fan-out, microphone watchdog, media player contract and hardware mute; test AirPlay → Assistant → AirPlay with correct visible indication.
6. Framework wakeword engine, always-running assistant activation and pre-roll; then **three Capsula Mini target profiles** and real-hardware compatibility checks. Leave large Capsula/Neo interaction decisions for later.
7. Compile/test the updated Protobuf schemas and SDKs (including routed streaming and script bridge parity) before freezing any v1 interfaces.

## 17. Remaining open questions and implementation gates

### 17.1 Initial supported platforms

The initial hardware scope is **Capsula Mini**, in three target environments:

| Target platform | Firmware | Scope |
|---|---|---|
| `CAPSULA_MINI_ROCKCHIP_STOCK` | Jailbroken stock firmware | Initial backend target |
| `CAPSULA_MINI_AMLOGIC_STOCK` | Jailbroken stock firmware | Initial backend target |
| `CAPSULA_MINI_AMLOGIC_ALPINE` | Alpine Linux firmware | Initial backend target |

All three must be tested for audio I/O, touch event surface, LED ring, segment display, audio DSP availability, sandbox mechanisms, interpreter portability and dependency/runtime ABI. **Do not assert parity until tested.** App binaries may need different architecture/ABI builds; QuickJS/MicroPython host runners need native builds per environment. Large Capsula and Neo are **explicitly deferred** for interaction-backend decisions, even if their high-level public contract is likely reusable.

### 17.2 Blockers to a production release (but not to beginning development)

- **Web UI bootstrap and recovery:** owner client-certificate issuance/import, CA/server-key storage, renewal/revocation and lost-browser-certificate recovery without unauthenticated remote enrollment.
- **Script runtime host ABI:** final QuickJS/MicroPython bindings to the gRPC/Protobuf transport and reverse-RPC bus, including streaming, cancellation, and error translation; runtime-specific module/network compatibility.
- **Sandbox feasibility on each target:** user/mount namespace and seccomp availability, no-write-filesystem enforcement, interpreter/runtime loader dependencies; explicit behavior if a kernel lacks required features.
- **Actual hardware integration:** board probing, safe ownership of stock firmware daemons, raw touch capability, display/ring/mic APIs and audio latency on each of the three Mini environments.
- **Build provenance verification policy:** expected identity/repository constraints, trust-root updates and offline verification implementation; exact default UI badge language.
- **SDK conformance:** `protoc` syntax/descriptor compilation passed; still test generated Go/Rust clients, scripts, and capture/reverse-RPC stream lifecycle before considering API frozen.

### 17.3 Details that can evolve during implementation

- **Raw touch capability:** exact controller readings and gesture recognition on each Capsula/Neo variant; support is device-specific. Define a minimal raw sample schema after hardware inspection.
- **Mic watchdog timing:** startup grace, refresh threshold, visible continuing effects, brightness-zero behavior, and fallback on hardware without suitable visible indication; tune on real devices.
- **Default presentation edge cases:** how to report an app inadvertently creating multiple candidate baseline presentations (current rule: first ready baseline plus diagnostic).
- **App-bus transport details:** bounded queues, method streaming framing, cancellation, deadlines, SDK reverse-RPC adapters; no extra sockets needed.
- **Sandbox compatibility:** which Linux kernels/firmware images support the required namespace/read-only isolation and distinct unprivileged identities; specify fallback explicitly. Some app runtimes assume writable temporary directories and will need adaptation.
- **Persistent app state beyond typed settings:** intentionally unsupported in v0.5. Revisit only for a concrete real app requiring a database/cache or large state.
- **Asset catalog stability:** decide which SFX are guaranteed by the base runtime and how optional catalogs are versioned/licensed.
- **Launch invocation results:** precise user-visible outcomes when an already-running singleton defers, fails or rejects an invocation.

