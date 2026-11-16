# Working on capsulad

These instructions apply throughout this repository. Treat the speaker as an interactive
appliance, not a set of independently compiling services. A passing build or a "polishing"
pass does not establish a coherent user experience.

## Start with the current contracts

- [README.md](README.md) is the architecture and capability overview.
  [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) is the build, packaging, deployment and test
  manual. [docs/VERIFICATION.md](docs/VERIFICATION.md) separates device evidence from
  simulator/host evidence and records the open gates.
- For any user-facing behavior, read [docs/SPEAKER_BEHAVIOR.md](docs/SPEAKER_BEHAVIOR.md),
  especially its interaction contract and future-change gate. Read
  [docs/HARDWARE.md](docs/HARDWARE.md) before device work and
  [docs/WAKEWORD.md](docs/WAKEWORD.md) for detection/enrollment work.
- The application contract is [docs/app_development/README.md](docs/app_development/README.md), a
  self-contained manual: [manifest](docs/app_development/manifest.md),
  [lifecycle](docs/app_development/lifecycle.md), [SDK](docs/app_development/sdk.md),
  [app bus](docs/app_development/app-bus.md), [services](docs/app_development/services.md),
  [output and input](docs/app_development/output-input.md), [audio](docs/app_development/audio.md),
  [settings and actions](docs/app_development/settings-actions.md),
  [assets](docs/app_development/assets.md),
  [standard contracts](docs/app_development/standard-contracts.md),
  [packaging and deployment](docs/app_development/packaging-deployment.md),
  [security](docs/app_development/security.md) and
  [troubleshooting](docs/app_development/troubleshooting.md). Protocol sources live under
  `proto/`. The administrative CLI is `capsulad-cli`.
- When code contradicts a documented or user-reported expectation, investigate the
  implementation; do not redefine success to match broken behavior. Raise consequential
  product ambiguities rather than silently changing the contract.

## Keep hardware specifics in the board profile (non-negotiable)

Board and firmware specifics belong to the data-only `internal/backend/profile`, applied by
`internal/backend/rockchip` and `deploy/rockchip-stock`. Hardware-agnostic packages
(`internal/wifi`, `internal/audio`, `internal/webui`, …) take configuration from the
composition root and must not import a board profile. The supported configurations are the
Rockchip Capsula Mini rev 4 stock profile and the neutral simulator profile; Amlogic has no
backend and its wire enum values stay reserved.

## Use the shared SDK for app networking (non-negotiable)

Native apps use the shared `libcapsulad` transport and network helpers — the UDS gRPC
engine, `capsulad.OpenHTTP`, `capsulad.OpenGRPC` and the encoding/hash/random/URL
primitives — instead of bundling Go's `net/http`, `crypto/tls` or a gRPC stack. Keep those
packages out of a first-party app's production binary (`go list -deps` is the check); a
test-only loopback server over the transport seam is fine. Add a missing capability to the
shared SDK instead of giving an app a second network stack.

## Keep the application manual in step (non-negotiable)

Any change to an application-facing API MUST include the corresponding update to
[docs/app_development/](docs/app_development/README.md) in the same change. That covers framework
protobuf services and messages, manifest fields or their semantics, the native SDK and script-host
API surface, app-visible environment variables, the artifact and package format, and any
presentation, capture, audio, input, settings or asset behavior an app can observe. An application
contract that is only in code, or in a document that lags the code, is an incomplete change.

This rule is non-negotiable and is never deferred to a follow-up: do not land the code first and
document it later, and do not treat the manual as a separate task. Update the affected page (and
its examples and tables) in the same commit; adding a new page requires adding it to the manual's
index.

## Plan the interaction, not just the handler

Before changing a feature, describe its trigger, feedback, ownership, interruption,
dismissal, and final state in task notes or a test description. Use the
[future-change gate](docs/SPEAKER_BEHAVIOR.md#future-change-gate) to cover relevant edges. No
new planning document is required for each change.

Essential UX is part of implementation even when a request does not enumerate it: progress,
discoverable stop/dismiss, distinct active/error states, and recovery to the user's chosen
baseline. Reuse established speaker patterns; do not invent a second interaction convention
or equate extra animation with polish.

## Preserve lifecycle and output invariants

- Use the normal presentation/capture/audio services for apps, including compiled-in system
  apps. The daemon output loop is the physical writer; direct hardware writes are for backend
  bring-up, not an app-level workaround.
- No selected presentation means an explicit blank hardware frame, not "skip writing."
  Releasing an overlay restores the selected baseline; do not force black over a deliberately
  selected baseline effect.
- Release ownership on every terminal path: completion, dismissal, cancellation, timeout,
  mute, stream failure, disconnect, app stop/crash. Closing a presentation must remove
  dependent captures and end their streams. Publish terminal run events using the terminal
  snapshot, not a lookup after removal.
- Distinguish blank segment masks from the glyph for digit zero. Derive ring geometry from
  capabilities: pixel 0 is top, indices advance clockwise. Include brightness and all
  physically relevant fields in write deduplication. Do not restart an effect's phase
  because unrelated text/colon content changed.
- An invisible blank/black presentation is not a qualifying microphone indicator. Selected
  ownership alone is not visible feedback.
- Successful playback half-close drains queued audio; cancellation/abort discards it and
  unblocks writers. Bound every buffering stage, including resamplers, not just the front
  channel. Keep queue accounting synchronized with publication/consumption.
- Correlate input releases with the activation and recipient that received the press, not the
  latest global activation. Manual keys must retain manual semantics. A consumed dismissing
  press must also consume its release/hold/repeat; full/raw capture and microphone/volume
  safety controls retain their documented behavior.
- Guard timers, fades, and asynchronous completions with ownership/generation identity. An old
  callback must not stop a newer indication, route, or run. Avoid holding render/lifecycle
  locks across stream setup or other blocking I/O.
- Subscribe before taking an initial snapshot; account for queued older revisions. Bounded
  event queues need a convergence/resynchronization strategy when transitions are lost.
  Dropping the oldest event alone is insufficient if consumers still act only on event kind.
- Use monotonic time for elapsed durations and wall time/timezones for calendar schedules. A
  timezone/settings update must not destroy unrelated timers, stopwatch state, or active
  interactions.
- Existing settings are authoritative. Missing/empty provider selections can mean an explicit
  "none" and must survive restart; auto-seed only a genuinely new store. Preserve preferences
  when their storage changes, use atomic/CAS updates, and deep-copy mutable snapshot data.
- Keep high-frequency paths bounded and allocation-conscious. Do not add polling, copying,
  retries, or abstractions as a substitute for correct ownership and event ordering.

## Verify the experience at the right layer

- Treat a user's report as the repro. Add a regression for an uncovered consumer-visible
  failure; reuse existing coverage otherwise. Fix root ownership/state transitions rather than
  suppressing errors or adding symptom-specific resets.
- Check both the isolated rule and its integrated outcome where the boundary matters. Prefer
  actual backend frames, active streams, public API behavior, and persisted values over
  incidental internal fields or log wording.
- A gRPC send is asynchronous, not a physical-output acknowledgement. Wait for the expected
  observable state with a bounded deadline. Use controlled clocks, barriers, or an isolated
  slow consumer for timing/backpressure tests; an oversized PCM burst does not by itself prove
  a gap occurred. Always resume/clean up a paused test process on failure.
- Run the narrowest relevant checks after changes; use `-race` for touched shared
  lifecycle/state paths. Root Go tests do not cover nested modules. `make test` runs the root
  module, `sdk/go`, the Rust binding, the native crate, the C++ example and the optional
  assistant; `make test-race` adds the race detector to the root module. Focused commands are in
  [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md#tests). Standalone Go test commands on macOS need
  `-ldflags '-linkmode=external'`, as the Makefile and app build helpers already handle.
  `internal/daemon` `TestDumbAssistantEndsAfterSpeechOverColoredRoom` fails on this host and
  reproduces on pristine HEAD; treat it as environmental, not as a regression.
- Host daemon integration tests require a matching host native runtime and metadata
  (`build/lib`, or `CAPSULAD_NATIVE_LIB_DIR`). Do not mistake a Linux library for a usable macOS
  test runtime.
- For device-facing changes, verify the reachable surface when device work is authorized. Use
  bounded input/routing/driver-write traces; `-output-trace` instructions are in the behavior
  document. An audio stream-open log is not proof of sound output: the speaker's output monitor
  can establish digital repetition/drain without recording the microphone.
- Keep proof levels explicit: internal state, backend submission, digital output, electrical
  state, and acoustic/optical measurement are different. Simulator success does not certify
  Linux sandboxing, physical timing, brightness/gamma, recognition accuracy, or peer-device
  behavior.

## Package and deploy the actual changed stack

- Device access/deployment permission is specific to the current task; approval given for one
  task is not standing permission. Discover the current connection and state rather than hard-coding
  an address, device identity, or user preference.
- An application package is the pinned `.tar.gz` artifact: root `manifest.toml`, regular files
  and directories only, 48 MiB per file, 64 MiB unpacked, 4096 entries. Build targets compile
  and package on the build host — `make dumbassistant` and `make devapps` write
  `<id>-<version>.tar.gz` beside each package directory with `capsulad-cli app package` — and the
  device only ever installs that archive. `capsulad-cli app install|remove` refuses while the app
  is running; a new or replaced package is visible only after the daemon restarts.
- `make deploy-bundle` assembles a bundle from ready-made artifacts and never packages anything:
  it copies `apps/*.tar.gz` from `build/apps`, adds the `build/devapps` archives when
  `BUNDLE_DEVAPPS=1`, and fails when a staged app directory has no archive. Run
  `make dumbassistant`/`make devapps` first, then assemble.
- The shared asset store is the plain directory `<data-dir>/assets`, mounted read-only at
  `/assets`. There is no asset manifest. Every regular, non-symlink file is an asset addressed
  by its slash-relative path. Only the session-authenticated admin API writes it; apps never
  do.
- Inventory affected daemon, CLI, app packages, native runtime and metadata, assets and
  settings together. `make`/`make daemon` do not build the optional assistant; rebuilding the
  daemon does not update installed app packages. Verify installed versions and ready runs, not
  just source or build times.
- Prefer `capsulad-cli deploy PATH` for a coordinated stack: it validates the whole bundle
  before writing, checks native SDK requirements, stages outside `/data`, stops the daemon
  through the boot service, installs packages and runtime files while it is stopped, replaces
  the binaries, restarts through the boot service and gates on `capsulad -health`. A replaced
  binary is recoverable only until the health gate passes: as a sibling `.prev` when the staged
  file is on the data filesystem, otherwise as a gzip copy in staging after the target was
  removed, because `/data` cannot hold two 40 MiB daemons. A failed deployment restores what it
  replaced and restarts the previous stack.
- On Rockchip stock, use the canonical `deploy/rockchip-stock/S99btctl.sh` service and the
  normal health path. Do not revive a second boot service or manually kill the daemon or its
  watchdog feeder. Check storage headroom before staging. Native target compatibility is
  aarch64 glibc 2.33 or older, not musl; use the development manual's build recipe.
- Verify a semantic scenario after the health gate, then restart once when persistence or
  startup behavior is part of the change. Check explicit no-provider choices, restored
  settings, ready apps, and idle resources after restart.
- Restore test-changed settings and disable high-rate tracing; remove temporary probes. Do not
  unpair devices, change credentials, reboot/flash firmware, or alter a source computer's
  settings without appropriate authorization. The stock kernel's mount-only sandbox is a
  documented limitation, not full native-code isolation.

## Finish with durable evidence

Update the behavior contract and [docs/VERIFICATION.md](docs/VERIFICATION.md) when behavior
changes, at the proof level actually observed. Keep regression tests in source; local build
artifacts are useful during the task, not a substitute for durable instructions. Report what
was actually checked and any remaining limits; never claim "all bugs fixed" from compilation
or a simulator-only pass.