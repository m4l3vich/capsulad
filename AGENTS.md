# Working on Capsula

These instructions apply throughout this repository. Treat the speaker as an interactive appliance, not a set of independently compiling services. A passing build or a "polishing" pass does not establish a coherent user experience.

## Start with the current contracts

- Read [README.md](README.md) for architecture, builds, SDK boundaries, and supported behavior; consult [docs/STATUS.md](docs/STATUS.md) for implemented versus unverified paths.
- For any user-facing behavior, read [docs/SPEAKER_BEHAVIOR.md](docs/SPEAKER_BEHAVIOR.md), especially its interaction contract and future-change gate. Read [docs/HARDWARE.md](docs/HARDWARE.md) before device work and [docs/WAKEWORD.md](docs/WAKEWORD.md) for detection/enrollment work.
- `_devdocs/` is a historical architecture draft, not current implementation guidance. Do not resurrect retired apps, obsolete settings, or draft authentication assumptions from it. Production protocol sources live under `proto/`, not `_devdocs/proto/`.
- When code contradicts a documented/user-reported expectation, investigate the implementation; do not redefine success to match the broken behavior. Raise consequential product ambiguities rather than silently changing the contract.

## Plan the interaction, not just the handler

Before changing a feature, describe its trigger, feedback, ownership, interruption, dismissal, and final state in task notes or a test description. Use the [future-change gate](docs/SPEAKER_BEHAVIOR.md#future-change-gate) to cover relevant edges. No new planning document is required for each change.

Essential UX is part of implementation even when a request does not enumerate it: progress, discoverable stop/dismiss, distinct active/error states, and recovery to the user's chosen baseline. Reuse established speaker patterns; do not invent a second interaction convention or equate extra animation with polish.

## Preserve lifecycle and output invariants

- Use the normal presentation/capture/audio services for apps, including compiled-in system apps. The daemon output loop is the physical writer; direct hardware writes are for backend bring-up, not an app-level workaround.
- No selected presentation means an explicit blank hardware frame, not "skip writing." Releasing an overlay restores the selected baseline; do not force black over a deliberately selected baseline effect.
- Release ownership on every terminal path: completion, dismissal, cancellation, timeout, mute, stream failure, disconnect, app stop/crash. Closing a presentation must remove dependent captures and end their streams. Publish terminal run events using the terminal snapshot, not a lookup after removal.
- Distinguish blank segment masks from the glyph for digit zero. Derive ring geometry from capabilities: pixel 0 is top, indices advance clockwise. Include brightness and all physically relevant fields in write deduplication. Do not restart an effect's phase because unrelated text/colon content changed.
- An invisible blank/black presentation is not a qualifying microphone indicator. Selected ownership alone is not visible feedback.
- Successful playback half-close drains queued audio; cancellation/abort discards it and unblocks writers. Bound every buffering stage, including resamplers, not just the front channel. Keep queue accounting synchronized with publication/consumption.
- Correlate input releases with the activation and recipient that received the press, not the latest global activation. Manual keys must retain manual semantics. A consumed dismissing press must also consume its release/hold/repeat; full/raw capture and microphone/volume safety controls retain their documented behavior.
- Guard timers, fades, and asynchronous completions with ownership/generation identity. An old callback must not stop a newer indication, route, or run. Avoid holding render/lifecycle locks across stream setup or other blocking I/O.
- Subscribe before taking an initial snapshot; account for queued older revisions. Bounded event queues need a convergence/resynchronization strategy when transitions are lost. Dropping the oldest event alone is insufficient if consumers still act only on event kind.
- Use monotonic time for elapsed durations and wall time/timezones for calendar schedules. A timezone/settings update must not destroy unrelated timers, stopwatch state, or active interactions.
- Existing settings are authoritative. Missing/empty provider selections can mean an explicit "none" and must survive restart; auto-seed only a genuinely new store. Preserve preferences through migrations, use atomic/CAS updates, and deep-copy mutable snapshot data.
- Keep high-frequency paths bounded and allocation-conscious. Do not add polling, copying, retries, or abstractions as a substitute for correct ownership and event ordering.

## Verify the experience at the right layer

- Treat a user's report as the repro. Add a regression for an uncovered consumer-visible failure; reuse existing coverage otherwise. Fix root ownership/state transitions rather than suppressing errors or adding symptom-specific resets.
- Check both the isolated rule and its integrated outcome where the boundary matters. Prefer actual backend frames, active streams, public API behavior, and persisted values over incidental internal fields or log wording.
- A gRPC send is asynchronous, not a physical-output acknowledgement. Wait for the expected observable state with a bounded deadline. Use controlled clocks, barriers, or an isolated slow consumer for timing/backpressure tests; an oversized PCM burst does not by itself prove a gap occurred. Always resume/clean up a paused test process on failure.
- Run the narrowest relevant checks after changes; use `-race` for touched shared lifecycle/state paths. Root Go tests do not cover nested modules. Existing targets are `make test`, `make test-dumbassistant`, and `make test-sdk`; use them as appropriate, not as a mandatory full-suite run for every edit. Standalone Go test commands on macOS need `-ldflags '-linkmode=external'`, as the Makefile/test build helpers already handle.
- Host daemon integration tests require a matching host native runtime and metadata (normally `build/lib`, or `CAPSULA_NATIVE_LIB_DIR`). Do not mistake a Linux library for a usable macOS test runtime.
- For device-facing changes, verify the reachable surface when device work is authorized. Use bounded input/routing/driver-write traces; `-output-trace` instructions are in the behavior document. An audio stream-open log is not proof of sound output: the speaker's output monitor can establish digital repetition/drain without recording the microphone.
- Keep proof levels explicit: internal state, backend submission, digital output, electrical state, and acoustic/optical measurement are different. Simulator success does not certify Linux sandboxing, physical timing, brightness/gamma, recognition accuracy, or peer-device behavior.

## Deploy the actual changed stack safely

- Device access/deployment permission is specific to the current task; historical audit approval is not standing permission. Discover the current connection and state rather than hard-coding an audit IP, device identity, or user preference.
- Inventory affected daemon, app binaries, manifests, assets, SDK runtime/metadata, and settings together. `make`/`make daemon` do not build the optional assistant; rebuilding a daemon does not update its installed app packages. Verify installed versions and ready runs, not just source or build timestamps.
- Back up affected binaries/packages/settings/boot configuration before deployment. Stage replacements atomically, preserve user choices, and validate settings migrations against the installed manifest. A daemon `.prev` binary is not a rollback of the complete stack.
- On Rockchip stock, use the canonical `deploy/rockchip-stock/S99btctl.sh` service and normal upgrade/health path. Do not revive the obsolete secondary boot service or manually kill the daemon/watchdog feeder. Check storage headroom before staging. Native target compatibility is aarch64 glibc 2.33 or older, not musl; consult the README build recipe.
- Verify a semantic scenario after the API health gate, then restart once when persistence/startup behavior is part of the change. Check the user's explicit no-provider choices, restored settings, ready apps, and idle resources after restart.
- Restore test-changed settings and disable high-rate tracing; remove temporary probes. Do not unpair devices, change credentials, reboot/flash firmware, or alter a source computer's settings without appropriate authorization. The stock kernel's mount-only sandbox is a documented limitation, not full native-code isolation.

## Finish with durable evidence

Update the maintained behavior contract/status when behavior changes. Keep regression tests in source; local `build/` evidence and backups are useful artifacts, not a substitute for durable instructions. Report what was actually checked and any remaining limits; never claim "all bugs fixed" from compilation or a simulator-only pass.
