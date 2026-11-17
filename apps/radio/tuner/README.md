# Internet Radio tuner (`org.capsulad.radio.tuner`)

Native companion for the MicroPython `org.capsulad.radio` app. The script host
cannot open an outbound connection or decode audio, so this app fetches one
HTTP(S) radio stream through the shared `libcapsulad` transport, strips ICY
metadata, decodes MP3 and pushes bounded PCM chunks to the radio over the app
bus with one routed `Push` call per chunk (the reply is the backpressure).

## Contract

- Serves `org.capsulad.radio.v1.RadioTunerService` (`Tune`, `Stop`) as raw
  protobuf; the authoritative `.proto` is
  `apps/radio/micropython/proto/radio.proto`.
- The only allowed push target is the app named by the `player_app` setting
  (default `org.capsulad.radio`); a `Tune` naming any other player is refused.
- One session at a time. A newer `Tune` supersedes an in-flight one; `Stop`
  cancels the matching session. Chunks are 200 ms of stereo `PCM_S16LE` at the
  stream's sample rate, with `discontinuity`/`dropped_frames` after a network
  reconnect and one final `failed=1` push when the stream cannot continue. A
  Push call that cannot complete (transport error, timeout or refusal) means
  the session is over: the tuner stops pushing and a cancelled/failed push is
  terminal, not a retry hint.
- Runs at boot (`start_on_boot`) so the radio can discover it through
  `AppInterfaceService.ListInterfaces` without a manual start.

## Caller authorization

`Tune` and `Stop` are restricted to the configured player app's currently ready run. The
handler reads the daemon-verified caller identity from `ServerCall.CallerRunID()`: the caller's
`capsulad-run-id` claim, checked against the connection's socket peer credentials where the
transport provides them (the framework Unix socket), and rejected with `PERMISSION_DENIED` when
it contradicts them. The handler matches that run id against the run ids the daemon itself
publishes in `AppInterfaceService.ListInterfaces`. The request's
`player_app_id` field is validated against the `player_app` setting but is never the
authorization: it is caller-supplied. A caller the framework cannot name (empty, or the
`pid:<pid>` fallback for the CLI/Web UI) is refused with `PERMISSION_DENIED` before any URL is
dialed. That removes the earlier arbitrary-egress hole where any ready local app could make
this process fetch a URL.

Because the run-to-app mapping comes from `ListInterfaces`, the configured `player_app` must
declare at least one interface in its manifest while it runs; the radio app declares
`RadioPlayerService` and `MediaPlayerService`, so this holds. An allowed app that declared no
interface could not be identified by its run id.

Two scope notes. A supersede is not instantaneous: the shared SDK does not abort an in-flight
native dial on context cancellation, so a superseded `Tune` can hold its socket for up to the
5 s dial timeout before its next step notices; it never starts a session. And the direction is
one-way — the player gates the tuner's `Push` calls by session id, not by the tuner's run id,
so `Push` is not caller-authorized the way `Tune`/`Stop` are.

The general model is documented in
[`docs/app_development/security.md#routed-calls-and-trust`](../../../docs/app_development/security.md#routed-calls-and-trust).

## Build, test, package

```sh
sh apps/radio/tuner/build.sh                       # host/simulator package
GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  sh apps/radio/tuner/build.sh                     # device package
cd apps/radio/tuner && go test ./...               # unit tests
go list -deps ./cmd/tuner | grep -E 'net/http|crypto/tls|google.golang.org/grpc'  # must be empty
```

Package: `build/apps/org.capsulad.radio.tuner-<version>.tar.gz`, containing
`manifest.toml` and `bin/tuner`. A newly installed package is visible to the
launcher only after `capsulad` restarts.

## Supported streams and limits

- MP3 (MPEG-1/MPEG-2 Layer III) only, over HTTP/1.1 with verified TLS.
  AAC/HE-AAC, Ogg/Opus/Vorbis, FLAC, MP4/M4A, WAV and HLS are refused at
  `Tune` with `unsupported codec: …`; go-mp3 additionally rejects MPEG-2.5 and
  free-format MP3.
- `.pls` and `.m3u` playlists are resolved one level from at most 64 KiB;
  `.m3u8`/HLS is refused.
- Redirects are capped at five and an `https` → `http` downgrade is refused.
  Responses with `Content-Encoding` other than identity are refused, as is a
  `Transfer-Encoding` other than plain `chunked`.
- A leading ID3v2 tag above 64 KiB is refused rather than allocated by the
  decoder.
- Stream failure triggers up to three reconnects in a 30 s window (1/2/4 s
  backoff) inside the same session; there is no seamless resume.

## Configuration

`player_app` — app id the tuner may push decoded PCM to (default
`org.capsulad.radio`). A Tune request naming another player is refused.
