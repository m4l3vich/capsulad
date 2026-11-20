# Troubleshooting

Every failure below is observable from the CLI or the log. Start with the daemon's own view, then
narrow to the app.

```sh
D=/data/capsdemo                  # device; use run/demo for a simulator
capsulad-cli -data-dir $D status
capsulad-cli -data-dir $D app list
capsulad-cli -data-dir $D app runs <app-id>
capsulad-cli -data-dir $D logs -app <app-id> -n 100
```

`status` and `doctor` print the device, probed subsystem backends, resource availability, runs,
master volume and microphone state. `logs` is an administrator call, returns recent records only
and waits at most 700 ms for new ones.

## Manifest rejected

**Symptom** — `app package` or `app install` fails before anything is installed; the message names
a field.

| Cause | Fix |
|---|---|
| Unknown manifest key, often a typo such as `start_on_boot` misspelled | Remove or correct the key. Unknown keys are rejected, never ignored. |
| Invalid or missing `id`, blank `name`, empty `version` | Fix the top-level fields. The ID must match `^[a-z0-9][a-z0-9._-]{0,127}$`. |
| Path is absolute, escapes through `..`, or is `.` | Use a relative path inside the package root. |
| `native` without `architecture`/`abi`, or a script runtime with them | Match the runtime: native needs both; QuickJS needs a `.js` path; MicroPython needs a `.py` path. |
| Install/launch fails with `package ABI "…" does not match this target` | The package was built for the other libc. Rebuild with `TARGET_ABI=linux-gnu` (stock) or `linux-musl` (Alpine) so the manifest `abi` matches. |
| Script path not a regular file, or a symlink escaping the package | Store a regular file, not a link. |
| Duplicate/empty setting key, select without choices, `min > max`, bad default | Fix the setting definition; defaults are validated against their own type and bounds. |
| `[settings_ui]` missing, not `.html`, over 512 KiB, invalid UTF-8, or no settings declared | Fix the document and declare at least one `[[settings]]`. |
| `[native_sdk]` on a non-native runtime, `abi_major < 1`, negative `min_minor` | Fix the requirement block. |
| Unknown key inside `[github_attestation]` or missing `subject`/`bundle`/`repository` | Complete or remove the block. |

The same validation runs at packaging, install and launch. Fix the source tree; do not patch the
installed copy. See [manifest.md](manifest.md#validation-summary).

## App ID mismatch

**Symptom** — a package appears under one name but is listed under another, or the app cannot be
started or removed by the directory name.

- The manifest `id` is authoritative. If a package directory is named differently, the registry
  logs `package directory "<dir>" does not match app id "<id>"; using manifest id` and keeps the
  manifest ID. Fix: install through `capsulad-cli app install`, which always creates
  `<data-dir>/apps/<manifest-id>`.
- `application <id> is not installed` immediately after `app install`: while capsulad is
  reachable the install is applied live (the daemon reloads its registry), so a missing app means
  the install did not reach that daemon — read the command's own error, and check
  `capsulad-cli app list`. The restart rule only applies to the offline path: if the daemon was
  provably stopped when the package was written to `<data-dir>/apps`, it becomes visible at the
  next start (see [packaging-deployment.md](packaging-deployment.md#install-update-and-remove)).
- `PERMISSION_DENIED: run_id does not match the connecting process` or `unknown run_id` during
  `AppBusService.Connect` means the process sent a stale or foreign `CAPSULAD_RUN_ID`. Read
  `CAPSULAD_RUN_ID` from the environment of the current process; never cache it across runs.
- A package that reuses a built-in system app's ID is ignored or refused. Built-in IDs are
  reserved; choose another ID.

## Native SDK requirement unmet

**Symptom** — install or launch fails with `requires native SDK ABI …`.

| Message | Cause | Fix |
|---|---|---|
| `native SDK runtime is not installed` | No `capsulad-native.json` beside the runtime, or the runtime directory is wrong. | Install the shared runtime in `<data-dir>/lib` or `<daemon-dir>/lib`, or pass `-native-lib-dir`. |
| `requires native SDK ABI major N but the installed runtime provides major M` | ABI major mismatch. | Ship a runtime and binding from the same major version. |
| `requires native SDK ABI N.M or newer but the installed runtime provides …` | The runtime is older than declared. | Lower `min_minor` or install a newer runtime. |
| `app requires native SDK ABI … : no native SDK runtime directory is configured` | The CLI cannot find the runtime for its preflight. | Use `-native-lib-dir` or install the runtime in `<data-dir>/lib`. |

The binding re-checks the library it loads, so a runtime that passes the install preflight can
still fail at startup if its metadata and its exported ABI disagree.

## Missing runtime

**Symptom** — a Python or JavaScript package fails at launch with
`runtime quickjs is unavailable: …` or `runtime micropython is unavailable: …`; a native package
fails with a loader error for `libcapsulad`.

| Cause | Fix |
|---|---|
| Script host not installed beside the runtime | Install `capsulad-quickjs` or `capsulad-micropython` in `<data-dir>/lib`. `make interpreter` builds them. |
| Script host not executable | Check the mode; a non-executable host is reported as unavailable. |
| Native runtime absent and the app declares `[native_sdk]` | Install `libcapsulad` plus `capsulad-native.json`. |
| Native app does not declare `[native_sdk]` but calls the SDK anyway | No preflight is done, so a missing runtime fails at startup instead of install. Declare the requirement. |
| App bundles its own `libcapsulad` | Remove it; the launcher injects the shared runtime path. |

The Web UI shows which runtimes are available and their detail line; a client can call
`ApplicationService.ListRuntimes`. A launch failure names the missing host explicitly.

## App never ready

**Symptom** — `app runs <id>` shows `APP_STARTING` forever, or the run exits and restarts.
`AppReady` never arrives, so interfaces stay `available=false` and actions fail with
`application <id> is not ready`.

| Cause | Fix |
|---|---|
| The process never calls `AppBusService.Connect` | Connect before serving or presenting. |
| It connects to the wrong socket | Use `CAPSULAD_SOCKET`; in a sandbox the host path does not resolve. |
| It sends `AppHello` with a run ID that is not its own | Use `CAPSULAD_RUN_ID` exactly. |
| It sends something else first | The first message must be `AppHello{run_id}`, the second `AppReady`. |
| It crashes before ready | Check the captured stdout/stderr in `logs -app`. |
| A library load fails at startup | Fix the ABI or install the runtime. |

`app start` reports `run=… invocation=… spawned=…`. If `spawned=false`, an existing run received
an invocation instead of a second process; inspect that run. If the launch fails with
`FAILED_PRECONDITION`, the existing run is stopping — launch again after it exits.

## Presentation never visible

**Symptom** — frames are accepted, `visibility.visible` stays false, or the resource is dark.

1. **No valid content yet.** The presentation is eligible only after its first frame. A segment
   mask of `0` and a black ring frame are valid content but invisible; `BlankPresentation` is
   valid and deliberately blank.
2. **The app is not the configured default and holds no capture.** Set the default app for the
   resource in the Web UI, or acquire a capture referencing this presentation.
3. **Another capture is newer.** Display and ring select the newest acquired capture whose
   presentation is ready. Release yours or accept the selection change.
4. **Brightness is zero.** Check the resource's brightness and `brightness_mode`. A zero effective
   brightness also disables microphone indication.
5. **Wrong resource kind.** Segment frames require `display.segment.front`; ring frames require
   `light.ring.main`. A mismatched payload is `INVALID_ARGUMENT`.
6. **A newer capture is unready.** An unready newer capture is skipped; the previous ready capture
   stays selected. Fix content readiness rather than reordering captures.
7. **The run exited.** Process exit closes every presentation it owned.

`GetRouting` / `WatchRouting` shows `selected_app_id`, `selected_presentation_id`, captures and
their `ready`/`selected` flags. There is no CLI subcommand for routing; use the Web UI or a
client call.

## Capture denied

| Error | Cause | Fix |
|---|---|---|
| `touch captures require a capture mode` | `touch_mode` was `TOUCH_CAPTURE_MODE_UNSPECIFIED` on `touch.panel.main`. | Send `TOUCH_PARTIAL_GESTURES` or `TOUCH_FULL_RAW`. |
| `touch_mode must be unset for … captures` | A mode was sent for display, ring or assistant role. | Clear `touch_mode`. |
| `presentation … not found` / `not owned by` / `not valid for` | The capture references a foreign, closed or wrong-kind presentation. | Capture your own ready presentation on the same resource. |
| `resource … does not support captures` | Resource is a sensor, audio or unknown ID. | Capture only display, ring, touch or assistant role. |
| `this touch backend cannot provide a full-panel stream` | `full_panel_available=false`. | Use partial capture. |
| `this touch backend provides no semantic gestures` | `partial_gestures_available=false`. | Use full capture. |
| `this RPC requires a supervised application connection` | No verified app identity. | Run inside the supervised process and connect with `CAPSULAD_RUN_ID`. |
| `capture_id must be the caller's currently selected touch capture` | `WatchInput` with a stale or foreign capture. | Watch only after `CaptureSelectionChanged{selected:true}`; re-subscribe after selection changes. |
| `microphone is globally muted` | Global mute is on. | Unmute from the Web UI or the microphone key. |
| `microphone capture requires a selected visible ring or segment indication` | No qualifying visible output. | Show a non-blank, non-zero-brightness presentation that is currently selected. |
| `microphone indication is not audible: …` | Zero master volume, mute, missing sink or a cue that cannot open/drain. | Fix the named condition; there is no silent capture. |

## Audio gaps and underruns

**Symptom** — `AudioChunk.discontinuity=true`, growing `dropped_frames_before`, or a
`CLIENT_TOO_SLOW` / `WATCHDOG_EXPIRED` notice.

| Cause | Evidence | Fix |
|---|---|---|
| The app does not drain its capture queue | `CLIENT_TOO_SLOW`, increasing drops | Read PCM continuously; do not block the reader on network or disk I/O. |
| Visible indication lapsed, blank or too dim | `discontinuity` then `WATCHDOG_EXPIRED` | Keep the selected presentation non-blank, lit and updated (content older than 2 s stops counting; ring effects are exempt). |
| Volume zero or global mute | Chunks withheld; `MICROPHONE_MUTED` on mute | Restore volume or unmute. |
| Source read failure | `discontinuity=true` without client drops | Recoverable; resynchronize on the next chunk. Report the device log if it persists. |
| Playback gaps | Progress stalls, `frames_queued` oscillating | Feed the playback stream steadily; `Write` blocks when the 32-chunk queue is full, which is expected backpressure. |
| Playback stops early | Stream closed before the tail | Half-close (`Close`) drains; `Abort` discards. Use `Close` when the last words matter. |
| No audio at all | `GetAudioState.master_volume` is 0 or `microphone_muted=true` | Fix the device state. |

There is no app-facing XRUN event in the current daemon; source underruns appear as
`discontinuity` on the next chunk. Do not treat `sequence` as a contiguous counter after a gap.

## Settings rejected or stale

| Error | Cause | Fix |
|---|---|---|
| `ABORTED: app settings revision N is stale (current M)` | Another writer won. | `Get` again, merge, retry with the returned revision. |
| `INVALID_ARGUMENT: setting <key> is not declared by <app>` | Typo or a setting removed from the manifest. | Use the `Describe` schema. Reinstall the manifest if it changed. |
| `INVALID_ARGUMENT: setting <key> must be …` | `value_json` encoded the wrong type (`"42"` for an integer is a JSON string). | Send `42`, `true`, `"text"`. |
| `INVALID_ARGUMENT: … clearing a masked value requires an explicit clear` | Empty string sent to `text_masked`. | Set `clear:true`. |
| `INVALID_ARGUMENT` from a malformed JSON body | `value_json` is not valid JSON. | Encode with a JSON serializer, then pass the string. |
| `PERMISSION_DENIED` | App addressed another app's configuration. | Address only your own `app_id`. |
| Value accepted but app sees an out-of-range number or unknown choice | The daemon enforces type, not `min`/`max`/`choices`, on direct updates. | Validate in the app; the generated UI constrains input but is not a server guarantee. |

`capsulad-cli settings app <id>` performs read-modify-write for you. The Web UI requires a
non-zero revision; a direct `Update` with `expected_revision=0` skips CAS and can silently
overwrite a concurrent change.

## Asset not found

| Cause | Fix |
|---|---|
| Joined the catalog `path` onto `CAPSULAD_ASSETS` | `path` already includes `/assets/…`. Join the **host root** with `asset_id`. |
| Used the host store path inside a namespace sandbox | Use `CAPSULAD_ASSETS` (`/assets`). Host paths do not resolve. |
| File was copied into a directory reached through a symlink | Symlinks are never followed or published. Copy the real file. |
| File name begins `.capsulad-upload-` | Reserved for in-flight uploads; rename it. |
| Catalog entry exists but read fails | Check the file mode (published files are `0644`, the store `0755`) and that the component is not a symlink. |
| Upload returned 413 | The file is over 32 MiB or the 48 MiB store budget is full. Remove unused assets. |
| Extension not in the table | Media type is `application/octet-stream`; rename to the correct extension. |
| File added but not listed | Listing is live; check you wrote to `<data-dir>/assets`, not a package directory. |

## Exit and restart loop

A lost framework connection is a **failure**, not a stop. The supervisor stops the orphaned
process and applies the restart policy: `restart_on_failure`, or the app being the selected
default provider (which is restarted even without the flag).

The backoff is immediate for the first failure, then 1, 2, 4 and 8 seconds; every further
failure restarts after 8 seconds. There is no longer delay and no 30-second cap — the delay
plateaus at 8 seconds. A run that stays ready for 30 seconds resets the sequence. A run that
reports ready and dies immediately keeps accumulating, so the loop becomes visible rather than
spinning.

`restart_on_failure` also covers a clean self-exit (status 0): the supervisor cannot distinguish
an intentional idle exit from an early clean failure, so it relaunches the app. An app that exits
itself when idle must leave the flag false; otherwise it comes back on its own and the loop looks
like a crash.

| Cause | Fix |
|---|---|
| The app exits nonzero after a successful handshake | Fix the bug; an unclean exit is treated as a failure. |
| The app exits 0 on its own and comes back | It declares `restart_on_failure = true`. Set it false for an idle-exit app; the flag governs clean self-exits too. |
| `AppHello` rejected or the bus stream dropped | See "App ID mismatch" and "App never ready". |
| Missing runtime or library discovered only at startup | Fix the runtime; consider declaring `[native_sdk]` so install fails early. |
| The app deliberately stops itself without closing the bus | Close the client and exit cleanly, or stay running; a deliberate framework stop comes from `app stop`, and a clean self-exit is still relaunched when `restart_on_failure` is true. |

To break a loop: `app stop <run-id>` (a deliberate stop stays stopped), then fix and restart it.
If the app is the selected default, deselect it or select another provider before expecting it to
stay stopped. Uninstalling a looping app requires stopping its run first.

## TCP dial, TLS and DNS fail

**Symptom** — `tcp_open` (or `http_open`/`grpc_open`) fails, or a stream stalls before any data.

| Status / message | Cause | Fix |
|---|---|---|
| `TRANSPORT` naming a host or address | DNS failed, the name has no record, or the host is unreachable from the device's network | Resolve the name yourself first; use an IP literal to isolate DNS; check the device's link. The sandbox mounts `/etc/resolv.conf` and `/etc/hosts` read-only. |
| `TRANSPORT` naming a certificate or host name | Untrusted chain, expired certificate, or the certificate does not match `server_name` | Use the right `server_name`, install the CA with `ca_pem`, or fix the server certificate. There is no verification override. |
| `TRANSPORT` right after `tcp_open` succeeds | The peer closed or reset before the first read | Treat it as a failed exchange; do not retry in a tight loop. |
| `TIMEOUT` | The deadline covered DNS, TCP and TLS together and was too small, or the peer accepted but never completed the handshake | Raise `timeout_ms` (`0` selects 30 s) and retry once. |
| `INVALID_ARGUMENT` mentioning an option | A mistyped key such as `root` or `tls_verify` | Use only `tls`, `roots`, `ca_pem`, `server_name`, `nodelay`. Unknown keys are rejected on purpose. |
| `INVALID_ARGUMENT` for the host or port | Empty host, host over 255 bytes, or port `0` | Pass a real name or IP and a port in `1..65535`. |
| `stream_write` returns fewer bytes than sent | Normal short write | Loop until everything is written; never assume one write drains the buffer. |
| `INVALID_ARGUMENT` on a zero-length write | `stream_write` rejects an empty buffer | Write only when you have bytes; an empty write is a caller bug, not a flush. |
| `stream_read` returns `END_OF_STREAM` | The peer closed cleanly | Stop reading and close the stream. It is not an error, and it is not a zero-byte frame. |
| `CLOSED` on write | `CAPSULAD_SHUTDOWN_WRITE` was already applied | Stop writing; close the stream when the read side is done. |

## UDP send or receive fails

| Status / message | Cause | Fix |
|---|---|---|
| `INVALID_ARGUMENT` "needs both host and port, or neither" | `udp_open` got a host with port `0`, or an empty host with a port | Pass both to connect, or neither to stay unconnected. |
| `INVALID_ARGUMENT` on the local port | `udp_open` binds an ephemeral port only | Leave the bind port at `0`; a fixed inbound UDP port is a UDP listener. |
| `INVALID_ARGUMENT` "is not an IP literal" | Peer JSON carried a DNS name | Resolve the name yourself and send to an IP literal. |
| `NOT_CONNECTED` on send | Unconnected socket with empty peer JSON | Name the peer on every send. |
| `TOO_LARGE` | Payload over 65507 bytes, or a receive `max_len` above it | Chunk at the application level; a datagram cannot exceed the protocol maximum. |
| Received fewer bytes than sent | `max_len` was smaller than the datagram | UDP truncates: the remainder of that datagram is lost, not buffered. |

## mDNS publication does nothing

**Symptom** — `Publish` fails, or a browser on the LAN never sees the service.

| Status / message | Cause | Fix |
|---|---|---|
| `FAILED_PRECONDITION` naming the responder | Simulator or a build without Avahi, or Avahi is not running/registering after a restart | mDNS is device-only; retry on the unit after `avahi-daemon` is up. Never treat this as a successful publication. |
| `PERMISSION_DENIED` | The call was made without a supervised app identity | Connect with `CAPSULAD_RUN_ID` from inside the run; an administrator connection cannot publish. |
| `INVALID_ARGUMENT` about the service type | `example._tcp`, `_example.tcp`, or a name with an underscore inside | Use `_example._tcp` or `_example._udp`; a domain suffix may follow. |
| `INVALID_ARGUMENT` about instance or TXT | Instance over 63 UTF-8 bytes, more than 16 TXT records, over 1024 bytes total, or a `key=value` record over 255 bytes | Shorten the instance and TXT set. The daemon rejects an oversized record instead of splitting it across TXT strings. |
| `ALREADY_EXISTS` | Another registration on the link already owns the instance name; the daemon does not auto-rename | Withdraw the old publication and retry after it clears, or choose a distinct instance name. |
| `NOT_FOUND` on unpublish | Wrong id, or a publication owned by another run | Keep the `publicationId` returned by `Publish` and unpublish from the same run. |
| `Browse` emits nothing | The service type is wrong, the peer is on another link, or the browser is not resolved yet | Verify the type on the publisher, and allow for resolve latency. One `ADDED` event arrives per resolved address. |
| `UNAVAILABLE` on an open browse stream | The responder stopped or the daemon restarted | Resubscribe. |

Publications disappear on their own when the run exits; that is cleanup, not a bug.

## Listener refused, port in use or accept stalls

**Symptom** — `listener_open` fails, or `listener_accept` never returns.

| Status / message | Cause | Fix |
|---|---|---|
| `UNSUPPORTED` | No usable `CAPSULAD_CONTROL_FD`: the process was not started by the supervisor, the supervisor could not create the channel (one `WARN` in the daemon log, and the run still starts), or the runtime predates the appended networking surface | Run as a supervised app with a runtime that carries the appended table; check the daemon log for `control channel unavailable`. Never create the socket yourself. |
| `INVALID_ARGUMENT` naming 5353 or the Web UI port | The port is reserved | Use `0` (ephemeral) or a port in `1024..65535`. |
| `INVALID_ARGUMENT` naming the bind address | The address is multicast, a broadcast address or not a local interface address | Use `0.0.0.0`, `::`, a loopback address or an address the device actually owns. |
| `TRANSPORT` saying the port is in use | The explicit port is already bound on the device | Request `0` and read the port from `out_info`. |
| `INVALID_ARGUMENT` naming the 16-listener cap | The run leaked listener handles | Close every listener on every exit path. |
| `accept` returns `TIMEOUT` | No client connected within the bound | Expected; the listener stays usable. `0` polls once instead of waiting. |
| `accept` returns `CANCELLED` | The listener was closed from another thread | Reopen if the service should continue. |
| A peer cannot reach the service | The bind is loopback-only, or a firewall/router is in the way | Bind a specific interface address or the wildcard; then treat the service as LAN-exposed. |
| Accepted stream reads stall | The peer connected but sends nothing | Bound every read with a timeout and close idle connections. |

## Simulator versus device differences

| Area | Simulator | Device |
|---|---|---|
| Binary ABI | Host binary accepted | `aarch64`/`arm64`, ABI containing the target libc (`linux` on stock, `musl` on Alpine), glibc 2.33 or older on RK3308 and 2.28 or older on Amlogic stock |
| Native library | Host `libcapsulad` | Target `libcapsulad` and `capsulad-native.json` |
| Sandbox | Whatever the host kernel probes | `mountns` on stock firmware; no user namespace, no seccomp |
| Framework socket | `<data-dir>/run/hal.sock`, host path | Host path `<data-dir>/run/hal.sock`; `/run/capsulad/hal.sock` in the sandbox |
| Asset path | Host asset directory via `CAPSULAD_ASSETS` in `uid`/`none`; `/assets` in namespaces | `/assets` |
| Touch | Virtual panel with optional coordinate injection | Five keys, no raw coordinates |
| Audio | Virtual 16 kHz mono capture, 48 kHz stereo output | Backend capture format (normally the same), vendor DSP tap |
| Brightness and cues | Virtual, always present | Hardware dependent; a backend may be partial or absent |
| Network egress | The host's network stack, DNS and trust store | The device's own link, DNS and trust store; no per-destination allowlist in either |
| Listening sockets | Daemon-allocated exactly as on the device; reachable from the host's LAN | Daemon-allocated under the same policy; reachable from the device's LAN |
| mDNS | Unsupported — no Avahi responder; `Publish`/`Browse` return `FAILED_PRECONDITION` | Avahi over the system D-Bus; visible to the local link |

If it works only on the simulator, check the architecture first, then the runtime, then the
sandbox mode. A host `darwin` build fails on the device with
`package architecture "…" is not executable on this aarch64 target`. A host test never proves the
device's sandbox or hardware backends.

## Where to look

| Place | What it shows |
|---|---|
| `capsulad-cli -data-dir $D status` | Device, probed backends, resources, runs, volume/mute. |
| `capsulad-cli -data-dir $D doctor` | Same with backend diagnostics. |
| `capsulad-cli -data-dir $D app list` | Installed apps, versions and runtimes. |
| `capsulad-cli -data-dir $D app runs <id>` | Run IDs, states (`APP_STARTING`, `APP_READY`, `APP_EXITED`, `APP_FAILED`), PIDs and detail. |
| `capsulad-cli -data-dir $D logs -app <id> -n 100` | Recent structured records for the app and supervisor. |
| `capsulad-cli -data-dir $D settings app <id>` | Current app settings and revision (masked values redacted). |
| `capsulad-cli -data-dir $D settings show` | Device settings, including default apps and microphone state. |
| `capsulad-cli -data-dir $D app provenance <id>` | Computed native-binary provenance status and detail. |
| `$D/capsulad.log` | Full daemon log. |
| `$D/app-settings/<app-id>/settings.json` | Stored settings and revision. |
| `$D/apps/<app-id>` | Installed package tree. |
| `$D/boot.log` | Boot hook and daemon start/stop history on the device. |

On the stock device `D=/data/capsdemo`; on a simulator `D` is the directory passed to
`capsulad -data-dir`. If the CLI reports `daemon is not reachable on …`, check that `capsulad` is
running and that `-data-dir` (or `-socket`) points at the same instance.