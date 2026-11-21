# Audio

Audio is framework-owned. Apps open **streams** to named endpoints; the daemon owns the
capture source, the mixer and the physical devices. Playback streams are independent and mixed;
microphone capture is one physical source fanned out to bounded per-app queues. An app never
opens an ALSA or PulseAudio device.

| Endpoint | Direction | Typical format | Processing tap |
|---|---|---|---|
| `audio.input.microphone` | capture | 16 kHz mono `PCM_S16LE` | `framework.dsp.v1` when the DSP chain is active |
| `audio.input.microphone.raw` | capture | the backend's capture format | empty (physical vendor stream) |
| `audio.output.speaker` | playback | 48 kHz stereo `PCM_S16LE` | — |

`ListEndpoints` returns the endpoints the backend actually exposes, each with direction,
supported `AudioFormat`s and `processing_tap`. An empty tap is a normal physical/raw tap. Treat
the advertised format as authoritative; the simulator and the device can differ.

## Formats and chunking

| `AudioFormat` field | Meaning |
|---|---|
| `sample_format` | `PCM_S16LE` or `PCM_F32LE`. |
| `sample_rate_hz` | Frames per second. |
| `channels` | Interleaved channel count. |
| `channel_names` | Optional labels; do not rely on them being present. |

- Capture ignores `preferred_format` and `packet_ms`: the stream always uses the backend's
  capture format and delivers chunks sized for **20 ms** of that format (320 frames, 640 bytes,
  at 16 kHz mono S16LE). Read `started.actual_format` rather than assuming.
- Playback accepts `PCM_S16LE` or `PCM_F32LE` at any rate and channel count and resamples to the
  48 kHz stereo mixer. `PlaybackStarted.actual_format` reports the **output** format.
- Playback input is bounded at the mixer input. `PCM_F32LE` samples outside `[-1, 1]` are
  **silently clamped** (not rejected, so benign overshoot still plays), and `NaN`/`±Inf` samples
  are treated as silence. An amplitude above full scale cannot raise the output past the master
  volume; scale inside `[-1, 1]` if you want a predictable level.
- PCM is raw little-endian bytes. Do not send base64 or JSON audio.
- One playback chunk must not exceed 1 MiB.

## Microphone capture

`AudioService.Capture` is a server stream. Its first message is the request; then the daemon
streams events.

| Request field | Meaning |
|---|---|
| `endpoint_id` | `audio.input.microphone` (processed) or `audio.input.microphone.raw`. Empty selects the processed endpoint. |
| `preferred_format` | Accepted but currently ignored. |
| `packet_ms` | Accepted but currently ignored. |
| `activation_id` | Optional. Attaches the bounded pre-roll only when the framework issued this activation to the calling app. |
| `conversation_id` | Optional lifetime marker. |

```json
→ {"endpointId":"audio.input.microphone","activationId":"act_…","conversationId":"conv_…"}
← {"started":{"streamId":"mic_…","actualFormat":{"sampleFormat":"PCM_S16LE","sampleRateHz":16000,"channels":1},"indicationWatchdogMs":5000}}
← {"chunk":{"sequence":"1841","firstFrame":"588800","monotonicTimeNs":"19234000000","frameCount":320,"pcm":"…base64…","discontinuity":false,"droppedFramesBefore":"0"}}
← {"notice":{"code":"CLIENT_TOO_SLOW","detail":"audio consumer is not draining chunks"}}
```

| Event | Fields | Meaning |
|---|---|---|
| `CaptureAudioStarted` | `stream_id`, `actual_format`, `indication_watchdog_ms` | Sent once before any audio. `indication_watchdog_ms` is the grace period for the visible indication (5000 ms). |
| `AudioChunk` | `sequence`, `first_frame`, `monotonic_time_ns`, `frame_count`, `pcm`, `discontinuity`, `dropped_frames_before` | One block of PCM. `dropped_frames_before` counts frames withheld since the previous delivered chunk. |
| `AudioNotice` | `code`, `detail` | Non-audio condition. |

Notice codes the daemon emits:

| Code | Cause |
|---|---|
| `CLIENT_TOO_SLOW` | The app did not drain its bounded queue; chunks were dropped and the next delivered chunk carries `discontinuity`. |
| `WATCHDOG_EXPIRED` | No qualifying visible indication within the grace period; the capture is terminated. |
| `MICROPHONE_MUTED` | The global microphone mute was turned on; the capture ends. |
| `APP_EXITED` | The owning run is gone; the capture ends. |
| `DAEMON_SHUTDOWN` | The audio service is stopping. |
| `STREAM_CLOSED` | Normal end of the capture stream. |

`XRUN` is reserved for a source underrun. A current daemon reports source read failures by
setting `discontinuity` on the next chunk and resetting its DSP state rather than emitting
`XRUN`.

### The indication requirement

An app receives microphone audio only while the microphone is visibly and audibly indicated.
Both halves are checked when the capture opens and re-checked for every chunk:

1. **Visible**: the app has a selected, visible, non-blank ring or segment presentation at
   non-zero effective brightness. The rendered content must actually light something (a lit
   segment, colon or non-black pixel); a blank frame, a zero-brightness resource, or content
   that has not been updated for 2 seconds (except a self-animating ring effect) does not
   qualify.
2. **Audible**: the framework can play and drain its microphone-live cue through the speaker.
   Zero master volume, global mute, a missing output, or a cue that cannot be opened or drained
   denies the capture.

`StartCapture` fails with `FAILED_PRECONDITION` when either half is missing. During capture, a
lost visible indication or a volume/mute transition withholds audio immediately: the withheld
frames are counted, the next delivered chunk carries `discontinuity` and
`dropped_frames_before`, and the watchdog terminates the stream after the advertised grace
period. There is no silent recording.

While the indication cue plays it is the **exclusive** mixer source: other app playbacks are
frozen, not mixed under it, so a flood of audio cannot mask the cue. They resume when the cue
ends.

### Pre-roll

The framework keeps a rolling **1 second** of capture history.

- `activation_id` attaches that history only when the ID was issued to the calling app by the
  framework, is still inside its activation window, and has not been consumed. An unknown,
  foreign or expired ID is ignored with a warning and live capture still proceeds.
- A grant is consumed only after the capture opens successfully, and it is one-shot: the same
  ID cannot replay the buffer.
- Pre-roll chunks are emitted synchronously before live audio. They are still the pre-activation
  audio, so consecutive `sequence` numbers and `first_frame` continue into the live stream.
- If the pre-roll cannot be queued, the missing frames are disclosed through
  `discontinuity`/`dropped_frames_before` on the next chunk; the daemon does not pretend the
  whole history was delivered.

### Buffering and backpressure

- Each capture has a bounded event queue (64 entries, enough for the 1-second pre-roll plus
  live chunks). A consumer that stops reading loses audio and is told so with
  `CLIENT_TOO_SLOW`; hardware capture never blocks on an app.
- The capture subscription is per run. When the run exits or its bus connection drops, the
  stream ends with `APP_EXITED` and the queue is discarded.
- The app must read `pcm` promptly. Buffering minutes of audio inside the app does not increase
  the daemon queue and does not create pre-roll.

### Complete Go example

```go
mic, err := client.OpenMic(ctx, activationID, conversationID)
if err != nil {
    log.Fatalf("microphone: %v", err) // FAILED_PRECONDITION names the missing half
}
defer mic.Close()

for ev := range mic.Chunks() {
    switch {
    case ev.Format != nil:
        log.Printf("capture %s: %d Hz, %d ch", ev.StreamID, ev.Format.SampleRateHz, ev.Format.Channels)
    case ev.Notice != nil:
        log.Printf("audio notice %s: %s", ev.Notice.Code, ev.Notice.Detail)
    case ev.Chunk != nil:
        if ev.Chunk.Discontinuity {
            log.Printf("gap: %d frames dropped", ev.Chunk.DroppedFramesBefore)
        }
        handlePCM(ev.Chunk.PCM)
    }
}
```

## Playback

`AudioService.Playback` is bidirectional. The first message **must** be `OpenPlayback`; then the
app sends chunks or control commands. The server streams started, progress and notice events.

The master volume is applied **after** the per-app mix: each app's concurrent streams are summed,
clamped to full scale, added to the master mix, and only then multiplied by the tapered master
gain. `master_volume` is a 0–1 **control position**, not an amplitude: the mixer converts it once
per volume change to an exponential gain with a 60 dB range that is linear in dB. The gain is about
**-57 dBFS at 5%**, **-30 dBFS at 50%** and unity at 100%; 0 is exact silence and 1 is exact
unity. The conversion is cached on the volume update, so the per-sample path never computes a
power. One app therefore cannot exceed the configured level by sending hot samples or opening more
streams. A soft limiter follows the master gain (threshold 0.95, 5 ms attack window, 150 ms
release) so overlapping apps or a framework cue stay below full scale instead of clipping. At most
**4** concurrent playback streams may be open per app; the fifth open fails with
`RESOURCE_EXHAUSTED` before any audio is accepted.

| Request | Fields | Rules |
|---|---|---|
| `OpenPlayback` | `endpoint_id`, `format`, `target_latency_ms`, `conversation_id` | `endpoint_id` is `audio.output.speaker` or empty. A zero or incomplete `format` selects the backend playback format. `target_latency_ms` is accepted but currently ignored. |
| `AudioChunk` | `pcm` | Raw PCM in the opened format. At most 1 MiB per frame. |
| `PlaybackCommand` | `PLAYBACK_FLUSH`, `PLAYBACK_PAUSE`, `PLAYBACK_RESUME` | Flush discards queued audio and resets the resampler; pause stops consuming, resume continues. |

| Event | Fields | Meaning |
|---|---|---|
| `PlaybackStarted` | `stream_id`, `actual_format` | Sent once, before any progress. |
| `PlaybackProgress` | `frames_played`, `frames_queued` | About every 250 ms. `frames_played` counts **mixed output frames the mixer consumed for this stream**, silence included: it advances on every mixer tick for any open, unpaused playback, so it is not evidence that the producer is still feeding the stream. `frames_queued` is this stream's own backlog in frames. |
| `AudioNotice` | `code`, `detail` | `STREAM_CLOSED` when the stream finishes; `DAEMON_SHUTDOWN` on shutdown; `APP_EXITED` when the owning run is gone. There is no playback underrun notice: a starved live stream keeps contributing mixed silence instead of ending, so an app that must notice a dead producer has to use its own signal (queue depth plus producer liveness) rather than `frames_played`. |

### Drain on success, discard on cancel

- **Half-close after a successful write**: the daemon drains everything already queued,
  including the resampler tail, then ends the event stream. Use this when the last words matter.
- **Cancel or abort**: queued audio is discarded immediately, writers are unblocked, and the
  event stream ends. Use this for barge-in, stop buttons and errors.
- **Pause**: the queue stays bounded; writers experience backpressure. A paused stream is not a
  place to park an unbounded buffer.
- **FLUSH** clears the queue without closing the stream. It is not a substitute for
  pause/abort when you need an immediate audible stop, because the chunk already handed to the
  mixer may still play.

Backpressure is real: `Write` blocks when the 32-chunk playback queue is full. A stalled client
blocks its own writer, not the mixer. Chunks larger than 1 MiB are rejected
(`RESOURCE_EXHAUSTED`).

### Complete Go example

```go
const rate, channels = 48000, 2
playback, err := client.OpenPlayback(ctx, "audio.output.speaker",
    capsulad.AudioFormat{SampleFormat: capsulad.SampleFormatPCM16LE, SampleRateHz: rate, Channels: channels}, "")
if err != nil {
    log.Fatalf("playback: %v", err)
}

pcm := loadWAVPCM() // S16LE stereo at 48 kHz
for off := 0; off < len(pcm); off += 4096 {
    end := off + 4096
    if end > len(pcm) {
        end = len(pcm)
    }
    if err := playback.Write(pcm[off:end]); err != nil {
        log.Printf("write: %v", err)
        break
    }
}
if err := playback.Close(); err != nil { // half-close: drain the tail
    log.Printf("close: %v", err)
}
// playback.Abort() would discard the queue instead.
```

## Ducking and conversations

`AssistantService.BeginConversation` starts a **ducking lifetime** and returns a
framework-generated `conversation_id`. The server stream must stay open for the lifetime of the
turn; cancelling it ends the conversation.

- Playback opened with that `conversation_id` stays at normal gain during its own conversation.
- Every other stream is ducked to gain **0.25** with a **150 ms** fade while any conversation is
  active, and restored afterward.
- A stream with an empty or stale `conversation_id` is treated as unrelated.
- The microphone indication cue is stronger than ducking: while it plays, unrelated streams are
  frozen so the cue is never masked.

`BeginConversation` is not a priority API and grants no capture rights. It does not validate its
`activation_id` argument, and the conversation ID it returns is unguessable; treat the ID as the
only handle to the ducking exemption. Do not use a conversation to silence other apps for
longer than the actual turn.

## Volume and mute

| Operation | App access |
|---|---|
| `GetAudioState` | Readable. Returns `master_volume` (0–1 control position), `microphone_muted`, `microphone_mute_is_hardware`. |
| `SetMasterVolume` | Administrator, plus the installed AirPlay 2 receiver's authenticated READY run. Every other supervised app gets `PERMISSION_DENIED`. |
| `SetMicrophoneMuted` | Administrator only; a supervised app gets `PERMISSION_DENIED`. |

`master_volume` is a **ceiling per app**, not a post-sum gain shared by everyone. The mixer
starts at the `0.6` position and follows the persisted device setting; physical volume keys and the
microphone key use the same persisted settings as the administrator surfaces, so an app must
tolerate volume and mute changing at any time. The Web UI, the physical keys and the Bluetooth and
AirPlay mirrors all exchange the same 0–1 position and display it as a percentage; the framework
converts the position to gain once per volume change. Scaling PCM by `master_volume` inside an app
therefore applies a second, linear factor and is not equivalent to the device level.

The AirPlay exception is fixed daemon policy, not a manifest feature. A phone or Mac changing
the sender volume is controlling the speaker's physical level, so the installed AirPlay 2 receiver
follows an authenticated session that owns the active media on the same persisted `master_volume`
the keys and the Web UI use. Attaching, a probe request or an event-only connection does not move
the level, and a retired sender cannot keep writing it. No package can opt in by declaring an
interface, `provides`, a native-SDK requirement or any other manifest field: the daemon
recognizes only the installed `org.capsulad.airplay` receiver, and only its live READY run whose
Unix-socket peer PID matches the supervised process. A host that cannot report peer credentials
(the simulator) refuses the app path. Trust in the receiver comes from the administrator
installing the package; the run identity is not a request field.

The receiver does not fold the sender level into its PCM. The master gain is applied once by
the framework mixer — using the same 0–1 position and logarithmic taper as the volume keys —
after app streams are summed, and the soft limiter follows it, so a sender slider moves the same
device level instead of stacking a second gain stage.

The gain stages, in order:

| Stage | Value | App-reachable? |
|---|---|---|
| Per-stream trim | `Playback.SetTrim`, daemon-internal; framework cues use a fixed `0.4` authored-level trim | No: not exposed by the app API or the SDK |
| Conversation ducking | `0.25` with a 150 ms fade while a conversation is active | Indirectly, through `conversation_id` |
| Master volume | `master_volume` (0–1 control position), converted once to the 60 dB logarithmic taper and applied after each app's streams are summed and clamped | Read from `GetAudioState`; administrator, plus the installed AirPlay 2 receiver's authenticated READY run |
| Soft limiter | threshold 0.95, 5 ms peak-hold attack window, 150 ms release, after the master gain | No |

- Volume 0 or global mute denies a new microphone capture.
- Turning global mute on ends existing app captures with `MICROPHONE_MUTED`.
- A hardware mute is reported separately: `microphone_mute_is_hardware=true` means the physical
  gate is closed, not merely the software flag.
- Volume 0 is the only output silence; there is no separate output mute (the mute flag controls
  capture). Playback at volume 0 is silent but still drains normally.
- Scaling PCM yourself inside the app changes your own level, never the ceiling: the master
  volume, the per-app clamp and the limiter still apply.

## Limits

| Limit | Current behavior |
|---|---|
| Custom capture format or packet size | Not supported; the backend format and 20 ms chunks are used. |
| Multiple microphone captures per app | Allowed, each a separate fan-out stream; each must satisfy the indication. |
| Capture without indication | Denied; there is no silent capture mode. |
| Global wakeword capture | Framework-owned; apps cannot subscribe to it or read its pre-roll. |
| Pre-roll outside an activation | Not available. |
| Playback chunk size | 1 MiB maximum. |
| Playback sample amplitude | Clamped to full scale at the mixer input; out-of-range `PCM_F32LE` is not an error. |
| Concurrent playback streams per app | 4; the fifth open fails with `RESOURCE_EXHAUSTED`. |
| Playback queue | 32 chunks with backpressure; a paused writer blocks. |
| Latency hint | `target_latency_ms` is accepted and ignored. |
| Recording to disk | No framework recording service and no writable app data directory in the sandbox contract. Keep only bounded in-memory state, or persist small values as app settings. |