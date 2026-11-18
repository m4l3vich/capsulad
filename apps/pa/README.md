# PA Speaker (`org.capsulad.pa`)

## Using this app

PA Speaker is a background service, not an interactive app: it exposes
`org.capsulad.pa.v1.SpeakerService` and plays one WAV streamed by another local
app. It has no panel controls and no settings form beyond *Maximum clip length*.
The rest of this page documents the interface and build for maintainers.

- **Launch:** the package starts at boot and stays ready. Open
  **Apps → PA Speaker → Details** if you need to launch it manually after a stop;
  the launch form declares no arguments.
- **Controls:** use the `Play` RPC from another app (see *Calling it* below).
  The app has no physical buttons; volume and microphone keys remain framework
  controls.
- **Teardown:** a caller ends the stream by half-closing and waits for
  `PlayResult`, or cancels to discard queued audio. *Stop* on the detail page
  stops the service until the next boot or manual launch. It is a background
  app, not a selected provider, so a stop does not restart it.
- **Troubleshooting:**
  - **Launch fails:** the device needs the MicroPython host
    (`lib/capsulad-micropython`, built by `make interpreter-micropython`) and
    the shared runtime.
  - **`RESOURCE_EXHAUSTED`:** the clip is longer than *Maximum clip length*
    (1–60 s) or its declared length is unknown and it ran past the bound. Raise
    the setting only for a clip that needs it.
  - **`INVALID_ARGUMENT`:** the stream is not a supported WAV; the RPC detail
    names the rejected field.
  - **A second call waits or fails:** one playback stream is held at a time.
    Wait for the first `PlayResult` or cancel it.
  - **Nothing is heard:** the framework must have a speaker endpoint and the
    device must not be muted; this app submits PCM, it does not change master
    volume.

A background MicroPython app that turns the Capsula Mini into an addressable
speaker: another app streams one WAV file to it over the app bus and it plays
the file on the framework speaker. It holds no microphone, ring or touch
resource and needs no launch — it is ready from boot.

| | |
|---|---|
| App ID | `org.capsulad.pa` |
| Runtime | `micropython` (single instance, background, start on boot, restart on failure) |
| Interface | `org.capsulad.pa.v1.SpeakerService` / `Play` (`proto/pa.proto`, `proto/pa.protoset`) |
| Setting | `max_seconds` (1–60, default 30) |
| Contract | [docs/speaker.md](docs/speaker.md) |

`Play` is a client-streaming RPC. Each request carries the next slice of the
WAV (`PlayRequest.data`, raw bytes, at least one byte per frame), and the call
replies with `PlayResult` once the framework has drained the audio. Rejections end the call with
`INVALID_ARGUMENT`, `RESOURCE_EXHAUSTED`, `DEADLINE_EXCEEDED`,
`FAILED_PRECONDITION` or `CANCELLED` and a human-readable detail.

## Build, install, run

```sh
sh apps/pa/build.sh                     # stage + package into build/apps
# -> build/apps/org.capsulad.pa-0.1.0.tar.gz

capsulad-cli -data-dir /data/capsdemo app install build/apps/org.capsulad.pa-0.1.0.tar.gz
# restart the daemon (or use capsulad-cli deploy) so the registry sees it
capsulad-cli -data-dir /data/capsdemo app runs org.capsulad.pa
capsulad-cli -data-dir /data/capsdemo logs -app org.capsulad.pa -n 50
```

The device needs the MicroPython host (`lib/capsulad-micropython`, built by
`make interpreter-micropython` / `make interpreter`) and the shared runtime;
without the host the package installs but the launch fails. A bundle built
from a workspace where the hosts were built ships them in `lib/`.

`OUT=...` changes the output directory and `PACKAGE=0` stages the package
directory without writing an archive. A script package compiles nothing, so no
cross toolchain is involved; packaging needs a host `capsulad-cli`
(`CAPSULAD_CLI=/path/to/capsulad-cli` overrides the search). The descriptor is
regenerated with `protoc` when it is available.

## Calling it

From a native app (Go SDK), stream the file in slices:

```go
call, err := client.Stream(ctx, "org.capsulad.pa",
    "org.capsulad.pa.v1.SpeakerService", "Play", 90*time.Second)
if err != nil { return err }
defer call.Close()
for off := 0; off < len(wav); off += 64 << 10 {
    end := off + 64<<10
    if end > len(wav) { end = len(wav) }
    // playRequest(p) is PlayRequest{data:p}: 0x0a, varint len(p), p.
    if err := call.Send(playRequest(wav[off:end])); err != nil { return err }
}
if err := call.HalfClose(); err != nil { return err }
reply, err := call.Recv() // one PlayResult, or the rejection status
```

From a script app:

```python
call = capsulad.open_routed("org.capsulad.pa",
    "org.capsulad.pa.v1.SpeakerService", "Play", 90000)
call.send_bytes(b"\x0a" + varint(len(slice)) + slice)   # PlayRequest{data}
call.half_close()
reply = call.recv_bytes(90000)
call.cancel()
```

Cancelling the call is the stop: the app discards the queued audio and frees
the speaker. See [docs/speaker.md](docs/speaker.md) for the framing rules,
timeouts, accepted WAV formats, status codes and the interaction contract.

## Limits worth knowing

- 128 KiB per request frame, 8000–48000 Hz, 1–2 channels, 16-bit PCM or
  32-bit float, at most `max_seconds` of audio.
- The script host has no filesystem: the caller supplies the bytes. A native
  caller can read the shared `/assets` store and stream a file here.
- The routed-call deadline belongs to the caller (default 30 s, cap 5 min);
  size it to the clip plus its drain.
- One clip at a time. The host is single-threaded, so a `Play` in progress
  delays the next call; a caller that wants to stop cancels its own call.

## Verification

```sh
python3 apps/pa/tests/test_wav.py    # parser, protobuf codec and limit unit tests
python3 apps/pa/tests/test_app.py    # settings shapes, status mapping, call handler
sh apps/pa/tests/e2e.sh              # simulator end to end (about 20 s)
```

`test_wav.py` and `test_app.py` import the app with a scripted stand-in for the
injected `capsulad` host module (`tests/fake_capsulad.py`) and build their files
with an independent RIFF/protobuf reader (`tests/fixtures.py`), so a bug in the
app's own helpers cannot hide behind the fixtures.

The end-to-end runner needs `build/bin/capsulad`, `build/bin/capsulad-cli` and
`build/lib` (host `make capsulad capsulad-cli` and `make interpreter-micropython`);
it builds nothing itself. It plays accepted files, rejects every documented
failure class, proves interruption, a stalled upload, two queued calls and an
administrator-set `max_seconds` bound, and checks that one playback stream is
held at a time — API behaviour, routing, ownership and PCM submission on the
simulator, not acoustic output.
