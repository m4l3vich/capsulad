# Internet Radio (`org.capsulad.radio`)

One native Capsula Mini app that keeps a station list, opens the selected
HTTP(S) MP3 stream through the shared `libcapsulad` transport, decodes it in
this process, writes PCM to the framework speaker and serves
`capsulad.contracts.media.v1.MediaPlayerService`. There is no companion app
and no app-to-app audio hop: the old `org.capsulad.radio.tuner` package is
retired.

## Package contents

| Path | Purpose |
|---|---|
| `manifest.toml` | identity, native entrypoint, settings, actions, argument, settings-UI document, media interface |
| `bin/radio` | the native app |
| `ui/settings.html` | packaged station picker and station-list editor (sandboxed settings bridge) |
| `proto/media.protoset`, `proto/media.md` | published standard media descriptor and documentation |
| `README.md` | this guide |

The package is a regular `<id>-<version>.tar.gz`; settings and preferences live
outside it, so replacing the package keeps the station list, selected station
and autoplay choice under the same app ID.

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

The app is `background = true`, `start_on_boot = true` and
`single_instance = true`: it is already running to answer actions and the media
contract, stays idle until a station is played, and a second launch is delivered
as an invocation to the existing run. `autoplay` is off by default, so a reboot
does not start audio by itself.

### Migrating from the retired companion

The previous release was two packages: the MicroPython front end and the native
`org.capsulad.radio.tuner` companion. The single app replaces both. Settings for
`org.capsulad.radio` itself (`stations`, `station`, `autoplay`) survive package
replacement because they share the app ID.

Nothing is uninstalled automatically. On a device that still has the companion,
find its run, stop it by run id, and then remove the app. Repeat for the radio's
current run before replacing its package; `app install` refuses while a run is
live. If an app is not installed, `app runs` prints nothing and the remove step
does not apply.

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
continuing a station that is no longer configured. It stays there until Stop or
Dismiss restores the baseline, Next/Previous re-selects and plays, or an
explicit pick chooses a valid slot that Play then starts. Unsubmitted textarea
edits are a draft: an incoming settings update from the picker, the autoplay
checkbox or another writer never overwrites them. The picker correlates each
save with the `requestId` returned by `CapsuladUI.patch`: only `result.ok` for
the newest save, while the textarea still holds that submitted text, clears the
draft. A predecessor's late result, a failure, or a newer edit leaves it intact.

### Selected station (`station`) and autoplay (`autoplay`)

`station` is the 1-based stored selection. Changing it switches an active
station; `next`/`previous`, a media `SkipNext`/`SkipPrevious`, and a launch
argument update the stored value with a compare-and-swap write. A value outside
the list is reported and does not start audio; the app never silently rewrites a
deliberate selection.

`autoplay` plays the selected station once, when the first settings snapshot
arrives after startup. It never restarts a radio the user stopped, and changing
it later does not start playback.

### Controls

| Control | Effect |
|---|---|
| `play` action / media `Play` | Open and play the selected station. While paused, re-dials it because pausing ends the network stream. |
| `pause` action / media `Pause` | Close the station stream and discard the queued tail. `PLAYBACK_PAUSED` is reported until play. |
| `stop` action | Stop, close the stream, abort queued audio and release the display so the configured baseline returns. |
| `next` / `previous` actions, media `SkipNext`/`SkipPrevious` | Step the selection with wrapping. A live (or paused/error) radio switches immediately; idle only selects. |
| `dismiss` action | Stop and return to the idle baseline. This is the action the device's circle key uses. |
| launch argument `station` | Select by 1-based number, exact case-insensitive name, or URL and play it. A bare launch only ensures the app is running. |

## Feedback

| State | Segment display |
|---|---|
| buffering / reconnecting | station number, colon dark |
| playing (audio queued) | station number, colon lit |
| paused | station number, last decimal point lit |
| error | four dashes, colon dark, until `stop`/`dismiss` or a new `play` |
| stopped | no presentation: the configured baseline is restored |

If the device advertises no `display.segment.front`, the radio plays without a
display and retries the presentation on the next state change. When it does
present, it holds a display capture so the station wins over the configured
default app; stopping closes the presentation and releases the capture. If the
device advertises no `audio.output.speaker`, play fails with
`no speaker endpoint on this device`.

## Standard media contract

`capsulad.contracts.media.v1.MediaPlayerService` is published with the standard
descriptor/docs. There is exactly one player, id `radio`:

| Field | Value |
|---|---|
| `playbackState` | `PLAYBACK_BUFFERING`, `PLAYBACK_PLAYING`, `PLAYBACK_PAUSED`, or `PLAYBACK_STOPPED` when idle or after a failure |
| `appName` | `Internet radio` |
| `sourceName` | selected station name |
| `mediaName` | ICY title when present, otherwise the station name |
| `title`, `artist` | ICY metadata |
| `supportedActions` | `MEDIA_PLAY` or `MEDIA_PAUSE` (by state), plus `MEDIA_SKIP_NEXT` and `MEDIA_SKIP_PREVIOUS` |
| `revision` | monotonic per observable change |

`ListPlayers` and `GetStatus` still expose the stopped entry so the selected
station remains discoverable and `Play` can start it. The Web UI's strict Now
Playing panel only lists actively playing entries; that is a consumer policy,
not a different contract. `GetStatus`/command calls require a non-empty
`playerId` (`INVALID_ARGUMENT`), and an unknown id is `NOT_FOUND`.
`WatchPlayers` sends a full snapshot first and then converges on the newest
status after each change; a slow watcher never blocks playback.

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
  connection. While active, a station switch replaces the owned playback; the
  previous session is cancelled and its late open/read/failure cannot clear the
  newer stream or selection.
- A stream read that goes idle for 10 s is treated as a stream failure. Up to
  three reconnects with 1/2/4 s backoff are attempted in the same session; a
  terminal failure shows the dash pattern and logs the reason. The app's
  autonomous watchdog bounds "no audio reached the speaker" at 60 s while
  buffering or playing, so a hung stream cannot leave `PLAYING` forever.
- `Close`/`SIGTERM` cancels the stream, aborts playback, closes the presentation
  and waits for the loop goroutines. The framework also releases a run's
  presentation and audio stream if the process is killed.

## Tests

The app's Go tests are host-only; they contact no network and need no daemon:

```sh
cd apps/radio && CGO_ENABLED=1 go test ./...
```

They cover the station grammar and selection, settings application and
out-of-range handling, media snapshots, chunk sizing, idle accounting and the
session-ownership rule that a stale failure/completion cannot clear a newer
session. The stream package tests cover chunked framing, ICY stripping,
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