PA speaker service: one WAV file in, playback on the framework speaker.

# SpeakerService (`org.capsulad.pa.v1`)

The PA Speaker app is a background MicroPython service. Another app on the
device reaches it through `AppBusService.Call` (a routed call) and streams one
RIFF/WAVE file; the app feeds the framework audio service and replies when the
device has finished playing it.

- `Play`: stream one WAV file to the speaker and reply when it has played, been rejected or cancelled.

Nothing else is exposed: there is no volume control (master volume is
administrator-only), no recording, no playlist and no persistent state. The app
declares no provider role and holds no presentation, microphone or touch
resource. The daemon separately exposes one authenticated HTTP relay for the
same `Play` operation; it is described under [HTTP API](#http-api).

| Property | Value |
|---|---|
| App ID | `org.capsulad.pa` |
| Runtime | `micropython`, background, start on boot |
| Interface | `org.capsulad.pa.v1.SpeakerService`, version `v1` |
| Descriptor | `proto/pa.protoset` (published for JSON callers and the Web UI) |
| Endpoint | framework `audio.output.speaker` |
| HTTP route | `POST /api/apps/org.capsulad.pa/play` (authenticated relay to `Play`; see [HTTP API](#http-api)) |
| Settings | `max_seconds` (1–60, default 30) |

## Play

`rpc Play(stream PlayRequest) returns (stream PlayEvent)` — bidirectional and
credit-based.

The caller sends the file as **one or more** `PlayRequest` messages and
half-closes to mark the end of the file. Every message must carry **at least
one byte** of the file: a frame without WAV bytes is rejected with
`INVALID_ARGUMENT`, because a frame that makes no progress could otherwise hold
the call (and a queued second call) for the caller's whole deadline. Concatenating every `data` field in
order yields exactly one RIFF/WAVE file; the split points are arbitrary,
including inside the RIFF header. PCM is never base64 on the wire: `data`
carries raw file bytes.

`PlayEvent` carries either a **credit** or the final **result**. The app grants
an initial credit of **two request frames** before it reads, then one more
frame after it has fully consumed each frame. A caller must not have more
frames in flight than its credits. This is the flow-control contract for both
app-bus and HTTP callers; it is what keeps the native SDK's 32-message inbound
queue from filling when the app is slow or another call is playing.

The app parses the stream incrementally and starts playback as soon as the
`fmt ` and `data` chunk headers are complete, so a long file is never held in
memory. `frame_bytes` alignment is preserved across request frames.

A successful call ends with one `PlayEvent.result` after the daemon drains the
queued audio:

```json
{"played":true,"frames":48000,"sampleRateHz":16000,"channels":1,
 "detail":"played 48000 frames at 16000 Hz/1 ch"}
```

`frames` counts interleaved PCM frames accepted for playback; `sampleRateHz`,
`channels` and `detail` always describe the **WAV's own** format. The device
resamples to its mixer format, which is reported separately in the app's log
line (`played call=… wav=16000 Hz/1 ch mixer=48000 Hz/2 ch`).

### Request framing and backpressure

- Each `PlayRequest` must be at most **128 KiB**; a larger frame is rejected
  with `RESOURCE_EXHAUSTED`.
- Honor the credits. The app starts at **2** and grants **1** after each fully
  consumed frame. The shared engine buffers 32 inbound request messages per
  call; sending ahead of credits can end the call with `RESOURCE_EXHAUSTED`.
  Both native/script callers and the daemon's HTTP relay use this window, so a
  direct caller playing a clip and an HTTP caller share the app's single-thread
  consumer fairly and cannot overflow it.
- A caller that ignores credits and fills the app's outbound credit queue can
  end the call with `UNAVAILABLE` (`caller is not reading playback credits`).
- The app writes PCM to the playback stream in 128 KiB chunks and holds no
  more than one chunk of its own. Credits are granted only after a frame has
  been parsed and any PCM handed to playback, so a slow mixer naturally slows
  the producer instead of filling a queue.
- The tail of a clip smaller than one chunk is written when the request stream
  ends. A stream that is rejected or truncated before that therefore plays
  nothing that was still buffered; chunks already handed to the mixer are
  discarded by the abort that ends the call.

### Timeouts

The routed call's deadline belongs to the caller:

- `AppBusService.Call.timeout_ms` defaults to 30 s and is capped at 5 minutes.
  Size it to the clip **plus its drain** (the app waits for the framework to
  finish draining before replying).
- The app itself waits at most 15 s for the next request frame; after that it
  ends the call with `DEADLINE_EXCEEDED` and discards the queued audio. A frame
  may be any size up to the limit, so a steady slow producer is not mistaken
  for a stall: each completed read is forwarded as its own frame. Because every
  frame must carry at least one byte, a call cannot idle without making
  progress; the total bytes are bounded (1 MiB of chunks before `data`, the
  audio bound, 64 KiB after it).
- A call is therefore held for at most the caller's own routed deadline (30 s
  by default, 5 minutes maximum, enforced by the daemon as a cancel). A caller
  that keeps a call open after its audio is queued delays a queued second call
  for no longer than that deadline.
- The app's own playback stream has **no deadline**: only the caller's routed
  deadline, that 15 s frame wait, the bounded drain and caller cancellation end
  it. An upload that is slow but within those bounds is not cut off mid-clip.
- If the caller cancels the call, the app discards the queued audio and ends
  the call; no reply is delivered.

### Rejections

Failures end the call with a gRPC status and a detail string; nothing is
replied, so a `PlayEvent.result` always means the audio played.

| Status | Cause |
|---|---|
| `INVALID_ARGUMENT` (3) | Not RIFF/WAVE; no `fmt ` or `data` chunk; `data` before `fmt `; unsupported encoding, channel count or rate; a stream that carries no audio at all; a frame-misaligned data chunk; a truncated file; a request frame with no WAV bytes; more than 64 RIFF chunks; more than 1 MiB of chunks before `data`; more than 64 KiB after the audio chunk; a malformed `PlayRequest`. |
| `RESOURCE_EXHAUSTED` (8) | A request frame above 128 KiB, or audio longer than `max_seconds`. A declared-length file is rejected before anything plays; an unknown-length file is stopped at the bound. |
| `DEADLINE_EXCEEDED` (4) | No request frame for 15 s, or the framework did not confirm the drain inside its window. |
| `FAILED_PRECONDITION` (9) | The speaker stream could not be opened (for example no output endpoint). |
| `UNAVAILABLE` (14) | The framework connection or the playback stream was lost. |
| `CANCELLED` (1) | The caller cancelled or its deadline expired. |
| `UNIMPLEMENTED` (12) | Any method other than `Play`. |
| `INTERNAL` (13) | An unexpected script or transport failure. |

### Accepted WAV files

| Property | Accepted |
|---|---|
| Container | `RIFF`/`WAVE`, chunk walk with padding; unknown chunks (`LIST`, `fact`, …) are skipped |
| Encoding | `WAVE_FORMAT_PCM` with 16-bit samples, or `WAVE_FORMAT_IEEE_FLOAT` with 32-bit samples |
| Channels | 1 or 2 |
| Sample rate | 8000–48000 Hz |
| Length | Declared `data` size within `max_seconds`; `0`/`0xFFFFFFFF` means “runs to the end of the stream” and is bounded while streaming |
| Trailing bytes | At most 64 KiB after the audio chunk |
| Chunks before `data` | At most 1 MiB of declared chunk sizes (`fmt ` plus anything skipped) |

MP3, Ogg, FLAC, ADPCM, extensible-subformat, 8/24-bit and >48 kHz files are
rejected with an explicit message instead of being played as noise.

## Calling it

Raw protobuf (works without a descriptor) from a native app. `PlayEvent.credit`
is field 1 (`PlayCredit.frames` field 1); `PlayEvent.result` is field 2. Read
the initial credit before sending, and keep at most the granted frames in
flight:

```go
call, err := client.Stream(ctx, "org.capsulad.pa",
    "org.capsulad.pa.v1.SpeakerService", "Play", 90*time.Second)
if err != nil {
    return err
}
defer call.Close()
credit := 0
for credit == 0 {
    event, err := call.Recv() // PlayEvent
    if err != nil {
        return err
    }
    credit += eventCreditFrames(event) // PlayEvent.credit.frames
}
for off := 0; off < len(wav); off += 64 << 10 {
    for credit == 0 {
        event, err := call.Recv()
        if err != nil {
            return err
        }
        credit += eventCreditFrames(event)
    }
    end := min(off+64<<10, len(wav))
    // PlayRequest{data}: field 1, length-delimited. Send raw protobuf.
    frame := append([]byte{0x0a}, varintBytes(len(wav[off:end]))...)
    if err := call.Send(append(frame, wav[off:end]...)); err != nil {
        return err
    }
    credit--
}
if err := call.HalfClose(); err != nil {
    return err
}
var result []byte
for result == nil {
    event, err := call.Recv() // credits continue, then PlayEvent.result
    if err != nil {
        return err
    }
    result = eventResultPayload(event) // field 2, or nil for a credit
}
```

A script app reaches it the same way over the app bus:

```python
import capsulad

call = capsulad.open_routed("org.capsulad.pa",
    "org.capsulad.pa.v1.SpeakerService", "Play", 90000)
credit = 0
while credit == 0:
    credit += event_credit(call.recv_bytes(90000))  # waits for initial grant
for frame in frames:
    while credit == 0:
        credit += event_credit(call.recv_bytes(90000))
    call.send_bytes(frame)  # PlayRequest{data}
    credit -= 1
call.half_close()
while True:
    event = call.recv_bytes(90000)
    result = event_result(event)
    if result is not None:
        break
call.cancel()
```

`eventCreditFrames`/`eventResultPayload` (and the Python `event_credit`/
`event_result`) are the two small field readers for `PlayEvent.credit.frames`
and `PlayEvent.result`; [../tests/caller/main.py](../tests/caller/main.py) is a
complete credit-aware caller. JSON callers register the packaged descriptor
first (`DescribeAppInterface` returns it as `fileDescriptorSet`) and then use
the canonical protobuf JSON form: `{"data":"<base64 WAV slice>"}` per request
message, `{"credit":{"frames":1}}` per credit event and `{"result":{...}}` for
the terminal success payload. A credit event is part of flow control, not a
completion reply.

## HTTP API

The daemon exposes one authenticated HTTP route for automation that has no
app-bus client. It is an app-specific relay, not a second audio path and not a
generic AppBus, audio or asset proxy: the daemon streams the request body to
this app over `AppBusService.Call`, and the app remains the sole owner of the
`AudioService.Playback` stream.

```
POST /api/apps/org.capsulad.pa/play
```

Only `POST` is registered; any other method returns `405 Method Not Allowed`.

The request body is exactly one raw RIFF/WAVE file, streamed. It is never
base64 and `Content-Type` is not interpreted. `Content-Length` may be omitted
(chunked uploads work). The route splits the body into request frames of up to
127 KiB (the app's 128 KiB message limit minus protobuf framing) and
half-closes when the body ends. On HTTP/1.1 each response carries `Connection: close`: a
clip is a one-shot request, and not reusing the connection is what lets the
daemon flush an early rejection without draining a stalled upload.

### Authentication

The route accepts either credential the daemon already supports:

- **Operator API key** (automation). Present it as
  `Authorization: Bearer <key>` or `X-API-Key: <key>`. A `status` key is
  refused; create the key with the explicit operator scope:
  `capsulad-cli -data-dir /data/capsdemo auth key create pa-operator --scope operator`.
- **Administrator browser session**. Send the session cookie plus the
  `X-Capsulad-CSRF` header from `GET /api/session`, with a same-origin request.

An unauthenticated request is `401`. A valid status key is `403`, as is a
browser request without the CSRF proof or with a mismatched concrete `Origin`.

```sh
# -k is the self-signed first-run bootstrap; trust the fingerprint from
# `capsulad-cli cert status` instead when the certificate is installed.
curl -k -sS \
  -H "Authorization: Bearer $CAPSULAD_OPERATOR_KEY" \
  -H "Content-Type: audio/wav" \
  --data-binary @announcement.wav \
  https://capsula.local:8443/api/apps/org.capsulad.pa/play
```

A successful call returns `200` with the same fields as `PlayResult`:

```json
{"played":true,"frames":48000,"sampleRateHz":16000,"channels":1,
 "detail":"played 48000 frames at 16000 Hz/1 ch"}
```

`frames` counts interleaved PCM frames; `sampleRateHz`, `channels` and `detail`
describe the WAV's own format. The device's mixer format appears only in the
app's log line, as for a routed call.

### Failures

The app's terminal gRPC status maps to one HTTP status. The `error` string is
the app's own detail, so it names the rejected WAV field or limit.

| HTTP | Cause |
|---|---|
| `400` | The body is not a supported WAV, is truncated, or is not `RIFF/WAVE`; a frame carries no data; the request body ended early. |
| `401` | No credential, or an unknown/revoked API key. |
| `403` | A `status` API key, a missing/invalid CSRF token, or a mismatched concrete `Origin`. |
| `404` | The app reported `NOT_FOUND` (not expected for this route). |
| `409` | The playback call was cancelled while the HTTP client was still connected. |
| `413` | The body exceeds the route's 32 MiB HTTP cap, or the app rejected the clip as longer than `max_seconds` (`RESOURCE_EXHAUSTED`). |
| `500` | The app reported an internal failure. |
| `501` | The app does not implement `Play`. |
| `502` | The app bus failed or the app returned a malformed result. |
| `503` | The PA app is not ready, has no output endpoint, or the app bus is unavailable. |
| `504` | The 5 minute route deadline expired; a start/send/recv app-bus call reported `DEADLINE_EXCEEDED`; or the app reported a drain timeout. |

The route does not launch the app. If `org.capsulad.pa` is stopped, a request
returns `503`; start it again (`capsulad-cli app start org.capsulad.pa`) or let
the next boot start it. A retry plays the clip again: `POST` is not idempotent.

### Ownership and interruption

- The app opens exactly one playback stream per HTTP request and releases it
  on every terminal path. The daemon never opens, drains or abandons a
  `Playback` stream on the route's behalf, so the route cannot become a second
  owner of the clip.
- Disconnecting the HTTP request cancels the routed call. The app discards
  queued audio and frees the speaker, including during the drain; this is the
  same stop path as cancelling an app-bus `Play` call.
- One HTTP request is admitted at a time. A second request waits at the
  relay's admission gate; it does not open its routed call or forward its body
  until the active clip has finished. Its wait is bounded by its own 5 minute
  routed deadline. There is no separate HTTP stop endpoint: close the request
  connection to stop the clip that request owns.
- The relay honors the app's frame credits. It reads one HTTP chunk, waits for
  a credit, sends that frame, and repeats; it starts with no credit and never
  sends a frame ahead of the app's grants. A direct caller playing a clip and
  an HTTP relay therefore share the single-threaded consumer without filling
  the SDK's 32-frame queue. Partial reads are forwarded as their own frames,
  so a steady slow producer is not mistaken for a stalled one.
- The route sets a 5 minute routed deadline (the daemon's maximum) and raises
  this request's HTTP read/write budget by 30 s. A slow upload is bounded by
  that deadline and by the app's 15 s wait between request frames. Size the
  HTTP client's own timeout to the clip plus its drain; the response arrives
  only after the app has played and released the audio.

## Interaction contract

| Facet | Behaviour |
|---|---|
| Trigger | The `Play` routed call, or `POST /api/apps/org.capsulad.pa/play` with an operator API key or an administrator session. The app is already running (background, start on boot); no launch is needed. |
| Feedback | One `PlayEvent.result` per accepted clip plus flow-control `PlayEvent.credit` events, one `pa:` log line per call (`played …` / `rejected …` / `stalled …` / `cancelled …`), and the daemon's own `playback stream=… opened/closed` records. The HTTP route relays that same `PlayResult` as JSON. |
| Ownership | The app owns one `AudioService.Playback` stream for the duration of a call and releases it on every terminal path. The HTTP route only relays a routed call; it never owns or writes the playback stream. |
| Interruption | Cancelling the call discards queued audio and frees the speaker, including during the drain: the app keeps polling the call while it waits, so a cancel cuts the clip off instead of letting it play out. Closing an HTTP request cancels its routed call, so the same path applies. Stopping the run (`ApplicationService.Stop`, `capsulad-cli app stop`) or losing the bus tears the stream down through framework cleanup. |
| Dismissal | There is no separate stop RPC: the script host serves one call at a time, so only the caller that owns the call can stop it, by cancelling. An HTTP client stops its clip by closing the request connection; another client's request cannot stop it. A second caller waits until the current call ends - the clip plus, at most, the owner's remaining routed deadline. |
| Final state | No playback stream, no buffered audio, no pending reply: the app is ready for the next `Play`. |

## Limits

| Limit | Value |
|---|---|
| WAV slice per request message | 128 KiB |
| Audio length | `max_seconds` (1–60, default 30) |
| Request frames in flight (framework) | 32 per call in the engine, 128 on the bus connection |
| Chunk written to the mixer | 128 KiB |
| Concurrent clips | 1 per app run; a second `Play` is served after the first (the app never holds two playback streams) |
| WAV bytes per request frame | At least 1; a frame without WAV bytes ends the call with `INVALID_ARGUMENT` |
| RIFF chunk bytes before `data` | 1 MiB of declared sizes |
| HTTP body | 32 MiB transport cap; the clip bound remains `max_seconds` |
| HTTP request slice | Up to 127 KiB per `PlayRequest` frame |
| HTTP admission | One request at a time; a second waits up to its routed deadline |
| Request-frame credits | Initial 2; one more after each fully consumed frame |
| HTTP deadline | 5 minutes for the routed call; 30 s extra HTTP read/write budget for the response |
| Playback stream deadline | None in the app; the caller's routed deadline, the 15 s frame wait, the bounded drain and cancellation end it |
| Concurrent script calls | 1 (single-threaded host); further calls queue in the host and bus budgets |
| Filesystem / asset access | None: the MicroPython host has no filesystem, so a caller must supply the bytes (a native caller can read `/assets` and stream them here) |
