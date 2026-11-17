# Internet Radio (`org.capsulad.radio`)

A MicroPython internet radio for the Capsula Mini: it keeps the station list, owns the
user-visible radio state, draws the station on the segment display, plays the station through the
framework speaker and serves the standard media contract so the circle key, the swipes and the
Web UI "Now playing" panel control it.

## Why a companion tuner is required

A script app cannot fetch an internet stream on this platform, and the platform has no audio
decoder for apps:

- The QuickJS and MicroPython hosts expose no HTTP/TCP/UDP/listener functions
  (`docs/app_development/network.md`, "What is not available"). `libcapsulad` has
  `http_open`/`tcp_open`/`stream_*` in ABI 1.0, but they are not bound into the script hosts.
- `AudioService.Playback` accepts raw PCM only. Nothing in the daemon or `libcapsulad` decodes
  MP3/AAC/Ogg/FLAC; the only decoder in the tree is the A2DP SBC sink.
- The script host has no threads, so a script pumps a long-lived stream one event at a time;
  repeating work uses the host's run-loop callbacks (`capsulad.every`, added for this app).

So this app is the **player/front end** and a native **tuner** companion does the fetch and the
decode. The two halves talk over the app bus with one routed `Push` call per chunk: that shape
turns the missing event pump into the framework's own event stream, keeps Pause/Stop/Skip
responsive, and gives the tuner natural backpressure.

Contract: [`proto/radio.proto`](proto/radio.proto) (raw protobuf, no descriptor needed). It is
published as `org.capsulad.radio.v1.RadioPlayerService` (served by this app) and
`org.capsulad.radio.v1.RadioTunerService` (served by the tuner). The tuner is a separate package;
without one the app reports a distinct `error` state ("no tuner provider is running") instead of
pretending to play.

## Build, install, run

Packaging happens on the build host; the device only installs the `.tar.gz`.

```sh
make radio                      # repository root: stages and packages the app
# -> build/apps/org.capsulad.radio-<version>.tar.gz

capsulad-cli app install build/apps/org.capsulad.radio-<version>.tar.gz
capsulad-cli app install build/apps/org.capsulad.radio.tuner-<version>.tar.gz   # companion
capsulad-cli app start org.capsulad.radio station=1
capsulad-cli app runs org.capsulad.radio
capsulad-cli logs -app org.capsulad.radio -n 50
```

`app install` acts on a running daemon and rescans the registry, so a replaced package is usable
without restarting `capsulad`; the on-disk path that needs a daemon restart applies only when no
daemon socket is reachable (or with `capsulad-cli deploy -restart never`).

`make test-radio` (or `make -C apps/radio test`) runs the app's unit tests on the development
host. Two end-to-end scripts run the whole app against a simulator daemon:

| Command | What it proves |
|---|---|
| `make -C apps/radio e2e` (`tests/e2e.sh`) | The real daemon and MicroPython host with the test-only tuner fixture (`tests/fixture-tuner`): install, boot start, invocation, Tune/Stop, pushed PCM into the speaker stream, segment capture and rendered frames, live settings switch, circle-key pause/resume, clean stop. No network. |
| `make -C apps/radio e2e-tuner` (`tests/e2e-tuner.sh`) | The same handshake with the real native tuner (`apps/radio/tuner`) and a live MP3 station: sustained real-time playback, pause refusal, resume, clean stop. Needs network and a built tuner package. |

The app is `background = true` and `single_instance = true`: it starts at boot so it is already
running when someone presses the circle key, and a second launch is delivered as an invocation
to the existing run.

## User operation

### Station list (`stations`)

One station per line, `Name | URL`. Blank lines and lines starting with `#` are ignored, a bare
`http(s)://` URL is accepted on its own (the URL becomes the name), and at most 99 stations are
kept. Only `http://` and `https://` stream URLs are accepted; a line that is neither is logged
and skipped.

```text
# One station per line: Name | stream URL
SomaFM Groove Salad | https://ice1.somafm.com/groovesalad-128-mp3
Radio Paradise Main Mix | https://stream.radioparadise.com/mp3-192
```

### Controls

| Control | Effect |
|---|---|
| `play` action / `Play` on the media contract | Tune the selected station and play it. While paused it resumes with a fresh tuner session. |
| `pause` action / `Pause` | Pause the speaker stream and make the tuner drop its session (pushes are refused with `paused`). |
| `stop` action | Stop, tell the tuner to drop the session, abort the queued audio and release the display so the configured baseline returns. |
| `next` / `previous` action, `SkipNext`/`SkipPrevious`, left/right swipes | Step the selection (wrapping) and play it when a session was live; the new number is persisted to the `station` setting with a compare-and-swap update. |
| `dismiss` action | The device's circle-key dismissal: stop and return to the idle baseline. |
| `station` launch argument | `capsulad-cli app start org.capsulad.radio station=<number|name|url>` selects and plays that station. |

The `station` setting (1-based) is authoritative: changing it in the Web UI switches a playing
station. `autoplay` plays the selected station once, when the first settings snapshot arrives
after startup; it never restarts a radio the user stopped.

### Feedback

| State | Segment display |
|---|---|
| tuning / buffering | station number, colon dark |
| playing | station number, colon lit |
| paused | station number, last decimal point lit |
| error | four dashes (`----`), colon dark, until `stop`/`dismiss` or a new `play` |
| stopped | no presentation: the configured baseline is restored |

`MediaPlayerService` requires a non-empty `playerId` on `GetStatus`/`Play`/`Pause`/`SkipNext`/
`SkipPrevious` (`INVALID_ARGUMENT` when missing, `NOT_FOUND` for an unknown id) and reports the
same state: one player `radio`, `playbackState`
(`PLAYBACK_BUFFERING` while the tuner dials, `PLAYBACK_PLAYING`, `PLAYBACK_PAUSED`,
`PLAYBACK_STOPPED` when idle or after a failure), `appName` = "Internet radio", `sourceName` =
station name, `mediaName`/`title`/`artist` from the tuner's ICY metadata, `supportedActions`
and a monotonic `revision`. `WatchPlayers` sends the snapshot first and then one update per
change. Failures are logged (`radio: error: …`) and shown as the dash pattern; the media contract
reports `PLAYBACK_STOPPED` because the contract has no error state.

If the device advertises no `display.segment.front` the app plays without a display; if it
advertises no `audio.output.speaker` a play attempt fails with "no speaker endpoint on this
device".

## Tuner contract (for the companion app)

Encoding is **raw protobuf** for the whole seam (no descriptor registration). Directions:

1. Player → tuner: `Tune(TuneRequest{session_id, url, station_name, player_app_id})` →
   `TuneResult{accepted, detail, session_id, content_type}`. `Stop(StopRequest{session_id,
   detail})` is best effort; a tuner may answer `UNIMPLEMENTED` and rely on refused pushes.
2. Tuner → player: `Push(PushRequest{sequence, session_id, pcm, sample_rate_hz, channels,
   sample_format, first_frame, discontinuity, dropped_frames, title, artist, finished, failed,
   detail})` → `PushResult{accepted, detail}`.

Rules the player depends on:

- **One `Push` call per chunk, and wait for `PushResult` before the next one.** The player writes
  the chunk into the speaker stream inside the call, so the reply is the backpressure.
- `accepted=false` means stop this session immediately; `detail` is `stale session`,
  `unauthorized caller`, `push authorization unavailable`, `paused`, `stopped`,
  `no playback stream` or `over capacity`. Do not retry.
- `session_id` is generated by the player and must be echoed **exactly**; a message without it or
  with another session is refused as stale and changes no state, so an aborted or superseded
  session can never leak audio or a terminal event into the current one.
- The player authorizes the pusher the same way the tuner authorizes `Tune`/`Stop`: the
  daemon-verified caller run id (`Call.info["caller_run_id"]`) must resolve, through
  `AppInterfaceService.ListInterfaces{runningOnly:true}`, to the tuner app this session tuned.
  A foreign app (or an unnamed caller) is refused with `unauthorized caller` before anything is
  written, so it cannot inject audio, spoof metadata or drive the radio into an error state. The
  lookup is cached per caller run id — one extra unary call per session, not per chunk.
  If the lookup itself fails, the push is lost either way (a refusal makes the tuner drop the
  session), so a push from the run that is serving the session ends it at once with
  `push authorization unavailable` and the error state; an unknown or foreign caller is only
  refused and can never end playback through a lookup failure (liveness and the dry queue still
  bound the real session). Resolutions are cached in a bounded `run → app` table (16 entries; a
  run id's app mapping is immutable), so a repeated foreign caller costs one lookup in total
  rather than one per push, and the tuned entry is never evicted by it.
- Chunks of 80–500 ms (200 ms recommended), `pcm` ≤ 256 KiB (hard framework cap 1 MiB), rate
  8000–192000 Hz, 1–2 channels, `sample_format` 1 = `PCM_S16LE` / 2 = `PCM_F32LE`. The daemon
  resamples to its 48 kHz stereo mixer.
- Finish with one `Push` carrying `finished=1` (the player drains the queued tail) or
  `failed=1` + `detail` (the player aborts the queue and shows the error). Empty metadata means
  "unchanged".
- One session at a time; a new `Tune` supersedes the old one.

Discovery: the app routes to the app id in its `tuner` setting when set, otherwise to the first
running app that declares `org.capsulad.radio.v1.RadioTunerService` (checked through
`AppInterfaceService.ListInterfaces`). A tuner named only by the setting must still declare that
interface: the declaration is what resolves its run for push authorization and for the liveness
watchdog, so a tuner without it is refused (fail-closed, the same coupling the tuner itself has).

Authorization: the tuner accepts `Tune`/`Stop` only from the configured player app's ready run. It
checks the daemon-verified caller run id (`ServerCall.CallerRunID()`, derived from peer
credentials and the supervised PID) against `AppInterfaceService.ListInterfaces{runningOnly:true}`
for its `player_app` setting (default `org.capsulad.radio`) and ends anything else with
`PERMISSION_DENIED` before dialing. `TuneRequest.player_app_id` is the push target, not the
authorization; the radio's own run id always resolves because it declares
`org.capsulad.radio.v1.RadioPlayerService`. A refusal surfaces in this app as
`tuner unavailable: …` and the radio shows its error state instead of playing; `Stop` is best
effort and only logs the refusal.

## Limits and integration requirements

- **No network and no decoder in the app.** The station audio can only come from a tuner app.
  Remaining missing platform capabilities, precisely: (1) script-host bindings for the ABI's
  `http_open`/`tcp_open`/`stream_*`; (2) an audio decoder reachable from an app. A third gap — no
  timer/idle hook for script apps — was closed for this app by adding the MicroPython host's
  repeating callbacks (`capsulad.every`, `capsulad.monotonic_ms`; documented in
  `docs/app_development/sdk.md`), which is what makes the stall watchdog autonomous. With (1) and
  (2) the fetch and decode could live in this app; today the tuner companion is the split.
- The app never binds a socket, never reads hardware and never writes the asset store.
- Pause is not a parked buffer: the tuner stops and resume is a new session; the parked tail is
  discarded rather than resumed and replayed. A tuner that keeps pushing while paused is
  throttled by refused pushes, not queued.
- One bound covers "no audio reached the speaker", in both phases: if no audio is accepted within
  **60 s** of entering `buffering`, or no audio has been queued for 60 s while `playing`, the
  session ends (`tuner did not start …` / `tuner stalled …`). 60 s sits above the tuner's own
  worst case for a legitimate retry window — 3 reconnects with 1/2/4 s backoff, a 12 s open and a
  10 s first-read idle timeout each, `(1+12)+(2+12)+(4+12) = 43 s` — so a recovering station is
  never pre-empted. A terminal `finished` that carries no audio at all ends the session as
  `stopped` directly rather than waiting for that bound.
- A natural end (`finished=1`) half-closes the speaker stream so the queued tail is heard, but the
  drain is serviced by the watchdog instead of blocking the loop: the phase stays `playing` while
  the tail plays out and the session becomes `stopped` when the daemon closes the event stream. A
  drain that has not finished after 30 s is aborted (the tail is discarded). A control frame after
  half-close is not legal, so `pause` during a drain is treated as a stop: the tail is discarded
  and the radio goes to `stopped` (never stranded in `paused` with no stream); `stop`/`dismiss`
  and a station change abort the drain the same way.
- A dead or stalled tuner does not leave the radio claiming `PLAYING`, even with no user
  activity. An autonomous watchdog runs on the MicroPython host's repeating callback
  (`capsulad.every(500, …)` + `capsulad.monotonic_ms()`, see `docs/app_development/sdk.md`) and
  uses two independent signals: (1) **liveness** — every ~2 s the *run id* that last pushed for
  this session must still be the ready run of the tuned tuner app in
  `AppInterfaceService.ListInterfaces`; the check is by run, not by app, because a tuner
  restarted by the supervisor is ready again as the same app id within a second. A killed or
  crashed tuner disappears and the session ends with `tuner stopped`. The lookup has an 800 ms
  budget so it cannot stall the event thread, and a lookup failure is never treated as death.
  (2) **dry queue** — the speaker stream's own `frames_queued` (not `frames_played`, which
  counts mixed output frames including silence) must show queued audio at least once every
  **60 s**. That bound sits above the tuner's documented worst case for a legitimate retry
  window — 3 reconnects with 1/2/4 s backoff and a 12 s open each, `(1+12)+(2+12)+(4+12) = 43 s`
  — so a recovering station is not killed, while a live-but-hung tuner is still bounded. An
  unexpected end-of-stream or `STREAM_CLOSED`/`DAEMON_SHUTDOWN` notice ends the session
  immediately.
- Control calls block the single-threaded event loop: `Tune` can take up to 25 s (the tuner dials
  and decodes the first frame) and the best-effort `Stop` up to 2 s. A `stop`/`dismiss`/`next`
  issued during a dial is applied when the dial returns; the display already shows the buffering
  state. The host has no threads, and the watchdog callbacks run on that same loop, so a long
  control call delays them too (their schedules slip rather than queue up).
- The segment display recovers: a failed frame closes the presentation and the next state change
  reopens it. An Open failure is treated as transient and retried, never as "this device has no
  display".
- The display is a station number, not a scrolling name; names are in the media contract and the
  logs.
- A deliberate stop (SIGTERM) kills the MicroPython host without a signal handler; the framework
  then releases the run's presentation and audio stream. The app's own cleanup runs on the
  disconnect path and on a normal exit (see `shutdown()` in `main.py`).

## Files

| Path | Purpose |
|---|---|
| `micropython/manifest.toml` | identity, settings, actions, arguments, published interfaces |
| `micropython/main.py` | the app: station list, state machine, display, speaker stream, media contract, tuner seam |
| `micropython/proto/radio.proto` | authoritative player/tuner wire contract |
| `micropython/proto/radio.protoset`, `proto/media.protoset` | published descriptors (`DescribeInterface`) |
| `micropython/proto/radio.md`, `proto/media.md` | interface documentation served with the descriptors |
| `tests/test_radio.py`, `tests/fake_capsulad.py`, `tests/pb.py` | host unit tests, scripted host double, independent protobuf codec |
| `tests/fixture-tuner/` | test-only MicroPython tuner used by `tests/e2e.sh` |
