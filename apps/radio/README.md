# Internet Radio (`org.capsulad.radio`)

One native Capsula Mini app that keeps a station list, opens the selected
HTTP(S) MP3 stream through the shared `libcapsulad` transport, decodes it in
this process, writes PCM to the framework speaker and serves
`capsulad.contracts.media.v1.MediaPlayerService`. It runs only while it has
audio or a live indication: a bare launch waits one minute for Play, a pause
or a playback Stop holds the pause sign for one minute, and a terminal error
holds its dashes for
its display window; each then ends the run. There is no companion app and no
app-to-app audio hop: the old `org.capsulad.radio.tuner` package is retired.

## Package contents

| Path | Purpose |
|---|---|
| `manifest.toml` | identity, native entrypoint, settings, actions, argument, settings-UI document, media interface |
| `bin/radio` | the native app |
| `ui/settings.html` | packaged station picker and station-list editor (sandboxed settings bridge) |
| `proto/media.protoset`, `proto/media.md` | published standard media descriptor and documentation |
| `README.md` | this guide |

The package is a regular `<id>-<version>.tar.gz`; settings and preferences live
outside it, so replacing the package keeps the station list, selected station,
autoplay choice and display preference under the same app ID.

## Build, install, run

Packaging happens on the build host; the device only installs the archive. The
app links only the Go SDK, which dlopens the shared native runtime injected by
the launcher. It never bundles `net/http`, `crypto/tls` or a gRPC stack.

```sh
make native                 # host libcapsulad + metadata (once)
make radio                  # -> build/apps/org.capsulad.radio-<version>.tar.gz
capsulad-cli app install build/apps/org.capsulad.radio-<version>.tar.gz
capsulad-cli app start org.capsulad.radio
capsulad-cli app start org.capsulad.radio station=1
capsulad-cli app runs org.capsulad.radio
capsulad-cli logs -app org.capsulad.radio -n 50
```

A device build is the same command with the target toolchain, for example:

```sh
GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc sh apps/radio/build.sh
```

The app is `background = false`, `start_on_boot = false` and
`restart_on_failure = false`: it is not started at boot, and one run at most is
alive. It is not a resident service. A launch with a `station` argument selects
and plays; a bare launch starts a run that waits one idle minute for Play and
then exits. While a run is alive, another launch is delivered as an invocation
to it (a bare invocation refreshes the idle wait). `autoplay` is off by default,
so a reboot does not start audio by itself. A clean self-exit is not relaunched;
launching the app again is the recovery.

### Migrating from the retired companion

The previous release was two packages: the MicroPython front end and the native
`org.capsulad.radio.tuner` companion. The single app replaces both. Settings for
`org.capsulad.radio` itself (`stations`, `station`, `autoplay`, `always_display`) survive package
replacement because they share the app ID.

Nothing is uninstalled automatically. On a device that still has the companion,
find its run, stop it by run id, and then remove the app. Repeat for the radio's
current run before replacing its package; `app install` refuses while a run is
live. If an app is not installed, `app runs` prints nothing and the remove step
does not apply. The radio exits itself when idle, so its stop step is unnecessary
when no run is listed.

```sh
capsulad-cli app runs org.capsulad.radio.tuner    # note the run_… id, if present
capsulad-cli app stop <run-id>                    # stop the companion run
capsulad-cli app remove org.capsulad.radio.tuner  # only when it is installed
capsulad-cli app runs org.capsulad.radio          # note the radio run_… id
capsulad-cli app stop <radio-run-id>
capsulad-cli app install build/apps/org.capsulad.radio-<version>.tar.gz
capsulad-cli app start org.capsulad.radio
```

The companion's `player_app` setting is framework-persisted form state and is not
used by the single app; uninstalling the companion does not claim to erase it.

Stale build artifacts are not deleted by any target. Remove the old staging
directory and archive explicitly before assembling a deployment bundle:

```sh
rm -rf build/apps/org.capsulad.radio.tuner build/apps/org.capsulad.radio.tuner-*.tar.gz
```

`make deploy-bundle` fails with that cleanup instruction if either is still
present, instead of silently shipping the retired package.

## User operation

### Station list (`stations`)

One station per line, `Name | URL`. Blank lines and lines starting with `#` are
ignored; a bare `http(s)://` URL is accepted with the URL as its name. At most
99 stations are kept. Only `http://` and `https://` stream URLs are accepted; an
invalid line is logged and skipped.

```text
# One station per line: Name | stream URL
SomaFM Groove Salad | https://ice1.somafm.com/groovesalad-128-mp3
Radio Paradise Main Mix | https://stream.radioparadise.com/mp3-192
```

The list is edited in the Web UI's app page. The custom settings document shows
the parsed stations as a picker, so selecting a station writes the `station`
setting through the sandboxed `capsulad.settings-ui/1` bridge; it has no fetch,
app-bus, audio or filesystem access.

Saving the station list writes only `stations`; it never rewrites `station`. If
the saved list no longer contains the selected slot, the picker keeps the
configured number and shows `Selected station N is not in the list. Pick one
above.`; choosing a station is the explicit recovery. A live stream whose
station was removed stops into the error state (four dashes) instead of
continuing a station that is no longer configured. With `always_display` on it
holds the dashes until Stop or Dismiss, Next/Previous re-selects and plays, an
explicit pick chooses a valid slot that Play then starts, or the one-minute
terminal hold ends; the run then exits. With the default off, the error uses the
same 10-second tuning window, then releases the display and exits the run. A run
whose station list or selection makes Play impossible ends the same way.
Unsubmitted textarea edits are a draft: an incoming settings update from the
picker, the autoplay checkbox or another writer never overwrites them. The
picker correlates each save with the `requestId` returned by
`CapsuladUI.patch`: only `result.ok` for the newest save, while the textarea
still holds that submitted text, clears the draft. A predecessor's late result,
a failure, or a newer edit leaves it intact.
The display-preference checkbox follows the same rule: it writes only
`always_display` through its own correlated patch, so it can be changed while
the station text is an unsaved draft, and an incoming snapshot for a save never
rewrites that draft.

### Selected station (`station`), autoplay (`autoplay`) and display (`always_display`)

`station` is the 1-based stored selection. Changing it switches an active
station; `next`/`previous`, a media `SkipNext`/`SkipPrevious`, and a launch
argument update the stored value with a compare-and-swap write. A write whose
RPC is cancelled or times out is unsettled: the app stops trusting its cached
stored station until a response or a newer snapshot settles the revision, so a
Dismiss flush fences the possible late commit with another compare-and-swap
instead of skipping the final selection. A value outside the list is reported
and does not start audio; the app never silently rewrites a deliberate
selection. Only an explicit next/previous step or a named launch
marks a selection for writing, and a write waits for the first settings
snapshot's revision: Dismiss racing startup therefore cannot replace the saved
station with the launch default, and a setting change during a playback Stop
hold updates the selection without starting audio. `Play` then uses the newest
selection when the user resumes.

`autoplay` plays the selected station once, when the first settings snapshot
arrives after a launch. It never restarts a radio the user stopped, and changing
it later does not start playback.

`always_display` is off by default and only changes a playing run. When off, a
tuning attempt shows the station number immediately and owns the segment
display for 10 seconds; the radio then closes the presentation so the selected
baseline returns. It never writes a blank or black frame, and releasing the
display leaves audio and the LED ring untouched. A new play/tune starts a new
10-second window. A pause never extends that window: it replaces the station
number with the pause sign and holds the display for the one-minute pause
window. The playback Stop action uses that same hold and reports
`PLAYBACK_STOPPED`; only the terminal Dismiss action and an administrative
process stop release the display immediately and end the run. An error
that occurs inside the tuning window shows four dashes until the window ends;
an error that occurs after the display was released exits the run without
reacquiring it. When `always_display` is on, a playing radio keeps the station
number; a paused radio still shows the pause sign only for its pause window, and
a terminal error holds the dashes until Stop/Dismiss, Next/Previous, or at most
one idle minute. Turning the setting on during a playing run shows the number
immediately; turning it off uses the deadline of the last tuning attempt rather
than a fresh 10 seconds. The station number, the pause sign and the error
pattern never light the colon: the colon belongs to the clock/volume
presentations. A run that never starts playback (a bare launch) owns no display
and exits after one idle minute; a step while idle refreshes that minute.

### Controls

| Control | Effect |
|---|---|
| `play` action / media `Play` | Open and play the selected station. While paused or in a Stop hold, re-dials it because both close the network stream, and cancels the expiry. On the Web UI's app page and overview actions menu, Play on a stopped app launches it through the public launch API with the saved `station`; it does not restore a resident process. |
| `pause` action / media `Pause` | Close the station stream and discard the queued tail. `PLAYBACK_PAUSED` is reported and the pause sign is held for one minute; the run then exits unless Play resumes it. |
| `stop` action | Stop playback, close the stream and abort queued audio. `PLAYBACK_STOPPED` is reported and the pause sign is held for one minute; the run then exits unless Play resumes it. A `station` or `stations` edit during the hold updates the selection but does not resume playback. Stopping the run from Run status (or Dismiss) is immediate. |
| `next` / `previous` actions, media `SkipNext`/`SkipPrevious` | Step the selection with wrapping. A live (or paused/error) radio switches immediately; idle only selects and refreshes the idle minute. |
| `dismiss` action | Stop, return to the idle baseline and end the run immediately. This is the action the device's circle key uses to dismiss the radio. |
| launch argument `station` | Select by 1-based number, exact case-insensitive name, or URL and play it. A bare launch starts a run that waits one idle minute for Play, then exits. |

## Feedback

| State | Segment display |
|---|---|
| buffering / reconnecting | station number, colon dark |
| playing (audio queued) | station number, colon dark |
| paused / playback stopped | pause sign (two vertical bars in the middle cells), colon dark; held for one minute, then the run exits |
| error | four dashes, colon dark; held for the tuning window (or up to one idle minute with `always_display`), then the run exits |
| stopped | no presentation: the configured baseline is restored and the run exits |

With `always_display` off (the default), playing and buffering own the display
for 10 seconds from the last tuning attempt; afterwards the presentation is
closed and the baseline returns. With it on, a playing radio keeps the number
until stop. A paused or playback-stopped radio always holds the pause sign for
its one-minute window, and a terminal error never outlives the run it is
ending. The radio
never lights the colon; that segment belongs to the clock/volume
presentations.

If the device advertises no `display.segment.front`, the radio plays without a
display and retries the presentation on the next state change. When it does
present, it holds a display capture so the station wins over the configured
default app; releasing the display closes the presentation and its capture,
which restores the selected baseline rather than forcing black. If the
device advertises no `audio.output.speaker`, play fails with
`no speaker endpoint on this device`.

## Standard media contract

`capsulad.contracts.media.v1.MediaPlayerService` is published with the standard
descriptor/docs. There is exactly one player, id `radio`, for as long as the run
is alive. The contract is not served between listens: a stopped radio has no
ready run, so it is not discoverable or commandable until the app is launched
again. While a run is alive — including the pause/Stop hold and an error's
display window — the player is reported:

| Field | Value |
|---|---|
| `playbackState` | `PLAYBACK_BUFFERING`, `PLAYBACK_PLAYING`, `PLAYBACK_PAUSED`, or `PLAYBACK_STOPPED` while a playback Stop holds for a resume or a Dismiss is closing the run |
| `appName` | `Internet radio` |
| `sourceName` | selected station name |
| `mediaName` | ICY title when present, otherwise the station name |
| `title`, `artist` | ICY metadata |
| `supportedActions` | `MEDIA_PLAY` or `MEDIA_PAUSE` (by state), plus `MEDIA_SKIP_NEXT` and `MEDIA_SKIP_PREVIOUS` |
| `revision` | monotonic per observable change |

`ListPlayers` and `GetStatus` expose the paused/stopped entry during its hold,
so the selected station remains discoverable and `Play` can resume it. The Web UI's
strict Now Playing panel only lists actively playing entries; that is a consumer
policy, not a different contract. `GetStatus`/command calls require a non-empty
`playerId` (`INVALID_ARGUMENT`), and an unknown id is `NOT_FOUND`.
`WatchPlayers` sends a full snapshot first and then converges on the newest
status after each change; a slow watcher never blocks playback. A provider with
no ready run cannot be called at all; a launch is the recovery, exactly as the
standard media contract's discovery rule says.

## Streaming behavior and limits

- MPEG-1/MPEG-2 Layer III over HTTP/1.1 only. AAC/HE-AAC, Ogg/Opus/Vorbis,
  FLAC, MP4/M4A, WAV and HLS are refused with an explicit error; there is no
  fake fallback and no second decoder.
- TLS, DNS and certificate verification are done by `libcapsulad`; redirects
  are bounded (5) and an HTTPS→HTTP downgrade is refused. One level of
  `.pls`/`.m3u` playlist is resolved (≤64 KiB). Gzip is not accepted.
- Shoutcast/Icecast ICY metadata is stripped from the audio and its
  `StreamTitle` is published as `title`/`artist`. Audio reaches the speaker as
  200 ms, 16-bit stereo PCM chunks; the daemon resamples to its 48 kHz mixer.
- Pause is not a parked buffer: the stream is closed and resume is a new
  connection. A pause or a playback Stop keeps the run and its pause sign for
  one minute, then the app exits; Play within that minute cancels the expiry
  and re-dials. While active, a station switch replaces the owned playback; the previous session is
  cancelled and its late open/read/failure cannot clear the newer stream or
  selection.
- No persistent idle run. Every non-playing state is bounded: a bare launch
  waits one idle minute for Play, a pause or playback Stop holds for its
  minute, and a terminal error lasts for its display window (the 10-second tuning window, or up to one
  idle minute with `always_display`). The expiry is generation-checked, so a
  resume, retune or newer terminal state always wins over an old timer. At the
  end the run publishes its terminal state, closes the segment capture and
  presentation so the selected baseline returns, and exits cleanly; because
  `restart_on_failure` is off, the supervisor does not relaunch it.
- A stream read that goes idle for 10 s is treated as a stream failure. Up to
  three reconnects with 1/2/4 s backoff are attempted in the same session; a
  terminal failure shows the dash pattern and logs the reason, then ends the run
  when the pattern's window closes. The app's autonomous watchdog bounds "no
  audio reached the speaker" at 60 s while buffering or playing, so a hung
  stream cannot leave `PLAYING` forever.
- `Close`/`SIGTERM` cancels the stream, aborts playback, closes the presentation
  and waits for the loop goroutines. A self-exit uses the same path, and the
  framework also releases a run's presentation and audio stream if the process
  is killed.

## Tests

The app's Go tests are host-only; they contact no network and need no daemon:

```sh
cd apps/radio && CGO_ENABLED=1 go test ./...
```

They cover the station grammar and selection, settings application and
out-of-range handling, media snapshots, chunk sizing, idle accounting and the
session-ownership rule that a stale failure/completion cannot clear a newer
session. Display tests use a controlled clock and a recording presentation host
to prove the 10-second release drops the dependent capture before closing the
presentation without sending a blank frame, that the future deadline alone wakes
the wait with no draw available, that retuning replaces the deadline instead of
letting the old one release the new display, that turning `always_display`
on/off during a run uses the tuning deadline, and that the station, pause and
error frames never light the colon. Idle-exit tests prove the pause sign is held
for the pause window and then the run signals its exit, that a resume cancels
the stale expiry (an old timer cannot end the newer session), that a step on an
idle run refreshes its window, that a playback Stop holds and then exits while
Dismiss ends the run immediately, that a terminal error exits when its display window closes (including with `always_display`
on), and that the exit path releases the capture before the presentation so the
baseline returns. Persistence tests prove that an early Dismiss makes no write
and keeps the launch default away from the saved station, that an explicit
selection made before the first snapshot waits for its compare-and-swap
revision, that a write which committed while its RPC was cancelled is fenced by
the shutdown flush with a fresh compare-and-swap instead of trusting the stale
cached station, that a snapshot predating that unsettled write cannot clear the
selection, that reconcile retries stay inside the flush deadline, and that a
settings edit during a playback Stop hold neither starts
audio nor moves the hold. Settings-retune tests prove the queued goroutine
re-checks the snapshot and playback generations, so a Stop or a newer snapshot
wins before publication while a valid edit still retunes.
The stream package tests cover chunked framing, ICY stripping,
redirects/playlists, malformed response heads, ID3 bounds and codec detection.

For an end-to-end run, build a simulator stack and install the package:

```sh
make capsulad capsulad-cli
capsulad-cli -data-dir run/radio auth init -hostname localhost -ip 127.0.0.1
capsulad-cli -data-dir run/radio app install build/apps/org.capsulad.radio-<version>.tar.gz
capsulad -platform simulator -data-dir run/radio -dev-admin run/radio/run/dev.sock &
capsulad-cli -data-dir run/radio app start org.capsulad.radio station=<n|name|url>
capsulad-cli -data-dir run/radio media list
capsulad-cli -data-dir run/radio app runs org.capsulad.radio
```

Use `-output-trace` on the daemon (see `docs/SPEAKER_BEHAVIOR.md`) when the
question is whether the speaker output actually repeated frames; an open audio
stream is not proof of sound.

## Deprecated/removal notes

Removed in this revision: the MicroPython app, the native tuner source and
build script, `org.capsulad.radio.v1.RadioPlayerService`/`RadioTunerService`,
the `tuner` setting, the fixture tuner and the Python test harness. They were
specific to the two-package transfer and have no meaning to the single app.