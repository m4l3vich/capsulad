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
resource.

| Property | Value |
|---|---|
| App ID | `org.capsulad.pa` |
| Runtime | `micropython`, single instance, background, start on boot |
| Interface | `org.capsulad.pa.v1.SpeakerService`, version `v1` |
| Descriptor | `proto/pa.protoset` (published for JSON callers and the Web UI) |
| Endpoint | framework `audio.output.speaker` |
| Settings | `max_seconds` (1–60, default 30) |

## Play

`rpc Play(stream PlayRequest) returns (PlayResult)` — client-streaming.

The caller sends the file as **one or more** `PlayRequest` messages and
half-closes to mark the end of the file. Every message must carry **at least
one byte** of the file: a frame without WAV bytes is rejected with
`INVALID_ARGUMENT`, because a frame that makes no progress could otherwise hold
the call (and a queued second call) for the caller's whole deadline. Concatenating every `data` field in
order yields exactly one RIFF/WAVE file; the split points are arbitrary,
including inside the RIFF header. PCM is never base64 on the wire: `data`
carries raw file bytes.

The app parses the stream incrementally and starts playback as soon as the
`fmt ` and `data` chunk headers are complete, so a long file is never held in
memory. `frame_bytes` alignment is preserved across request frames.

A successful call returns one `PlayResult` after the daemon drains the queued
audio:

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
- The shared engine buffers at most **32** inbound request messages per call
  and its app-bus connection another 128. Send the file in reasonably sized
  frames (a handful, or slices of at least several KiB): a caller that bursts
  hundreds of tiny frames faster than the app reads them can have the call
  ended by the engine with `RESOURCE_EXHAUSTED`.
- The app writes PCM to the playback stream in 128 KiB chunks and holds no
  more than one chunk of its own, so it consumes request frames promptly and
  the call does not stall behind the mixer.
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
  ends the call with `DEADLINE_EXCEEDED` and discards the queued audio. Because
  every frame must carry at least one byte, a call cannot idle without making
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
replied, so a `PlayResult` always means the audio played.

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

Raw protobuf (works without a descriptor) from a native app:

```go
call, err := client.Stream(ctx, "org.capsulad.pa",
    "org.capsulad.pa.v1.SpeakerService", "Play", 90*time.Second)
if err != nil {
    return err
}
defer call.Close()
for off := 0; off < len(wav); off += 64 << 10 {
    end := min(off+64<<10, len(wav))
    // PlayRequest{data}: field 1, length-delimited. varint is protobuf's
    // base-128 encoding; Send carries raw protobuf, no descriptor needed.
    frame := append([]byte{0x0a}, varintBytes(len(wav[off:end]))...)
    if err := call.Send(append(frame, wav[off:end]...)); err != nil {
        return err
    }
}
if err := call.HalfClose(); err != nil {
    return err
}
reply, err := call.Recv() // one PlayResult, or the rejection status
```

A script app reaches it the same way over the app bus:

```python
import capsulad

call = capsulad.open_routed("org.capsulad.pa",
    "org.capsulad.pa.v1.SpeakerService", "Play", 90000)
call.send_bytes(frame)  # PlayRequest{data} for each slice of the WAV
call.half_close()
reply = call.recv_bytes(90000)  # None only if the app ended without a reply
call.cancel()
```

JSON callers register the packaged descriptor first
(`DescribeAppInterface` returns it as `fileDescriptorSet`) and then use the
canonical protobuf JSON form: `{"data":"<base64 WAV slice>"}` per request
message, `{"played":true,...}` for the reply.

## Interaction contract

| Facet | Behaviour |
|---|---|
| Trigger | The `Play` routed call. The app is already running (background, start on boot); no launch is needed. |
| Feedback | One `PlayResult` reply per accepted clip, one `pa:` log line per call (`played …` / `rejected …` / `stalled …` / `cancelled …`), and the daemon's own `playback stream=… opened/closed` records. |
| Ownership | The app owns one `AudioService.Playback` stream for the duration of a call and releases it on every terminal path. |
| Interruption | Cancelling the call discards queued audio and frees the speaker, including during the drain: the app keeps polling the call while it waits, so a cancel cuts the clip off instead of letting it play out. Stopping the run (`ApplicationService.Stop`, `capsulad-cli app stop`) or losing the bus tears the stream down through framework cleanup. |
| Dismissal | There is no separate stop RPC: the script host serves one call at a time, so only the caller that owns the call can stop it, by cancelling. A second caller waits until the current call ends - the clip plus, at most, the owner's remaining routed deadline. |
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
| Playback stream deadline | None in the app; the caller's routed deadline, the 15 s frame wait, the bounded drain and cancellation end it |
| Concurrent script calls | 1 (single-threaded host); further calls queue in the host and bus budgets |
| Filesystem / asset access | None: the MicroPython host has no filesystem, so a caller must supply the bytes (a native caller can read `/assets` and stream them here) |
