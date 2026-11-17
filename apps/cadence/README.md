# Cadence

An optional first-party assistant for capsulad that is a **client of a
self-hosted [Cadence](https://git.shino.boo/devilreef/cadence/) server**. It
claims the `voice_assistant` role and drives the normal assistant turn: the
framework's wake/touch activation opens the microphone, the ring is the visible
capture indication, the microphone is streamed to the Cadence server as Opus,
and the server's streamed Opus reply is played through the framework's speaker
endpoint.

It is ordinary supervised application code, not a system app: nothing installs,
starts or selects it automatically. It bundles no Cadence code and no
upstream source. The protobuf files here are an independently written subset of
the upstream `cadence.v2` definitions (the same field numbers, names and enum
values, which is what interoperability requires). The `.binpb` files under
`internal/proto/testdata/upstream` are upstream-generated golden frames, copied
verbatim as interoperability **test vectors** from the upstream `main` commit
`f9571ad0d89acd7326414adf04a85ea1c9dec131`; they are data, not code, and are not
part of the installed package.

## What it is not

The client is a **voice-only device** on the Cadence link. It advertises no
capability descriptor, so the server pushes no config and no capability
command, uses its default reply output (mono, 64 kbps), and the client never
speaks for any other Cadence feature:

- **No announcements.** `STREAM_PURPOSE_ANNOUNCE` arrives outside an assistant
  activation and would speak with no turn; each one is refused with one log line.
- **API activations are refused.** `cadence.v2` treats a turn as the owner's
  only for `TRIGGER_WAKE` and `TRIGGER_BUTTON` (that is what authorises volume,
  alarm and integration writes). A capsulad `ACTIVATION_API` is a
  daemon/automation trigger, not evidence that the owner spoke or pressed
  anything, so this client refuses it with an actionable log instead of
  fabricating a press and escalating an unattended trigger to owner
  privileges. This is the deliberate, conservative side of that trust choice;
  a wake word or a talk-key press is the supported path. The same reasoning
  refuses a server `MicOpen` (a screen press in the room): it carries no
  physical owner signal for this device, so it is logged and not answered.
- **No cards, screens, RPC, media or tasks.** Those are separate device roles.
- **Runtime prerequisite.** The app `dlopen`s `libopus.so.0` at run time and
  ships no copy: the image provides it. Verified paths from the supplied
  rootfs images: Rockchip stock `/usr/lib/libopus.so.0.8.0`; Amlogic stock
  `/usr/lib/libopus.so.0.8.0`; Alpine 3.24 `opus` apk 1.6.1-r0 →
  `/usr/lib/libopus.so.0`. The bundle cannot install system packages, so the
  Alpine image must keep the `opus` package. No `LD_LIBRARY_PATH` or `$ORIGIN`
  rpath is involved because nothing is bundled.
- **No echo cancellation of its own.** During the reply the microphone stays
  open for the follow-up window, as cadence-device does; the server's own echo
  guard covers a reply heard back through the microphone.

## Wire protocol

Each link is one WebSocket at `<server_url>/v2/device` with
`Authorization: Bearer <device token>`, carrying one binary protobuf `Envelope`
per frame (the upstream `pkg/wire` framing: 16 MiB cap, unknown arms skipped).

| Direction | Message | When |
|---|---|---|
| device → server | `Hello{protocol:2, daemon_version, board, capabilities_hash}` | first frame |
| server → device | `Welcome{server_version, send_full_report, device_info}` | handshake reply |
| device → server | `CapabilityReport{descriptors: [], hash}` | full (empty) report |
| device → server | `SessionStart{session_id, trigger, wake}` | activation / follow-up |
| device → server | `MicAudio{session_id, seq, opus}` | 20 ms 16 kHz mono frames |
| device → server | `SessionCancel{session_id, reason}` | dismiss, timeouts, capture loss |
| server → device | `SessionEvent{speech_started, listening, thinking}` | turn phases |
| server → device | `StreamStart{stream_id, session_id, purpose:REPLY, codec:OPUS, channels}` | reply begins |
| server → device | `AudioDown{stream_id, seq, opus}` | 20 ms reply packets |
| server → device | `StreamEnd{stream_id}` | reply complete |
| server → device | `SessionClose{session_id, code, follow_up_allowed, follow_up_window_ms, failure}` | turn end |
| server → device | `Duck`, `VoiceLevel`, `MicOpen` | advisory/other roles; parsed, not acted on |

The client answers the server's pings, sends its own every 20 s, and drops a link
whose pong does not arrive within 10 s. Reconnects use cadence-device's policy:
1 s doubling to 60 s with ±20 % jitter, restarting at 1 s after a link that
stayed up a minute.

Opus is bound at run time with `dlopen` (`libopus.so.0` on the device's stock
rootfs, or `CAPSULAD_OPUS_LIBRARY`), so the app has no link-time codec
dependency. The encoder mirrors cadence-audiod: 16 kHz mono, 20 ms, 24 kbps,
voip, complexity 5, no DTX or in-band FEC. Reply packets are decoded to 48 kHz
for the framework's playback endpoint. A device without libopus fails its first
turn with one explicit error and keeps running.

All transport goes through the shared SDK: `capsulad.OpenTCP` (verified TLS 1.2+,
`ca_pem` appended to the system store) performs the connection, and this app's
RFC 6455 client (`internal/websocket`) frames it. The app links no `net/http`,
`crypto/tls` or gRPC stack.

## Interaction contract

| | Behavior |
|---|---|
| **Trigger** | `AssistantActivation` with `ACTIVATION_WAKEWORD` → `TRIGGER_WAKE` (the matched model id is sent as `wake.engine`); `ACTIVATION_TOUCH` → `TRIGGER_BUTTON`; `ACTIVATION_API` refused with an actionable log (an automation trigger is not a physical owner signal). Up to 8 activations queue, expired ones drop, and one turn runs at a time. |
| **Feedback** | The ring presentation is opened, its **first frame is written, then the routing capture is acquired** — the content and the selection the daemon's indication check requires before any microphone opens. Listening is teal, brightness from the real captured RMS over −50…−10 dBFS (fast attack, slow release; a dark ring is never shown); the level only modulates the listening phase, so a still-open microphone cannot repaint thinking/speaking. Thinking is a blue chase, speaking a green chase. A local failure (codec, encode, uplink, unsupported capture) holds a red ring for 300 ms; mute and a lost indication deliberately leave the framework's own state alone. |
| **Ownership** | One turn owns one Cadence session at a time, one microphone capture, one conversation (so other playback ducks), one encoder, and a bounded reply-stream writer per server stream. |
| **Interruption** | A newer activation cancels the running turn (`superseded`). A hold-to-talk release ends input, then streams `hold_release_tail_ms` of paced digital silence so the recognizer closes the utterance. A mute, indication watchdog, capture close, link loss, listen cap (30 s) or no-speech window (8 s) cancels the turn. |
| **Dismissal** | The manifest `dismiss` action (the circle/play-pause key) sends `SessionCancel{dismissed}`, aborts reply audio, releases microphone and ring, and drops queued activations. |
| **Final state** | Every terminal path releases the microphone, the ring capture, the presentation, the conversation, playback and encoder (the capture before the presentation). A successful `SessionClose{code:"ok"}` drains the reply tail (bounded 5 s) before the conversation closes; any failure or cancellation discards queued audio immediately. |

### Follow-up

When `SessionClose{code:"ok", follow_up_allowed:true, follow_up_window_ms:N}`
arrives after a reply that was heard whole (at least one packet actually handed
to playback, no lost stream, and no envelope dropped for the turn), and `follow_up` is on, and the microphone is still open, the client
starts a **new** session with `TRIGGER_FOLLOW_UP` and keeps the same
microphone, ring and conversation. The window is capped at the server's own
15 s. If nobody speaks, the turn's own window expires and the client cancels with
`follow_up_silence` (that is the device's cancel reason, not a server code). A hold-to-talk turn never follows up:
its release already ended input.

### Unconfigured

The app is ready with no server: it logs `link_unconfigured` once, activates
nothing, and waits. A settings change re-dials immediately (`Wake`).

## Settings

| Key | Type | Default | Meaning |
|---|---|---|---|
| `server_url` | text | empty | Base URL; the link is `<server_url>/v2/device`. A path prefix is allowed (reverse proxy). http:// is allowed but warns: token and audio travel unencrypted. |
| `device_token` | text_masked | empty | Bearer token of the enrolled device. Never logged. |
| `device_token_file` | text | empty | File holding the token, re-read on every settings change. Wins over `device_token`; an unreadable or empty file is rejected and the previous config kept. |
| `ca_pem` | text | empty | Extra PEM certificate(s) for a self-hosted CA; appended to the system store. There is no insecure mode. |
| `hold_release_tail_ms` | integer 0…5000 | 1000 | Silence streamed after a hold-to-talk release. Dead air the user hears as latency. |
| `follow_up` | boolean | true | Honor the server's follow-up windows. |

A malformed value is rejected, logged with a reason, and the previous
configuration is kept. Every log line passes through a redactor that scrubs the
current token, and a URL is shortened (no query or fragment) before it is
echoed.

### Enrolling the device

The token is the one the Cadence server issued for this device
(`device/cmd/cadence-install enroll`, or a device token created in the Cadence
dashboard). The client sends `Hello.protocol = 2`; a server that speaks another
protocol refuses the link with a protocol close and the client backs off.

## Build

The app requires cgo (the SDK) but not libopus at build time: the codec is
loaded at run time.

```sh
make native            # once, for the shared engine if not already built
make cadence           # package directory + build/apps/org.capsulad.cadence-<version>.tar.gz
```

Standalone, `make build` here writes the package directory, `make` or
`make package` also writes the archive beside it, and `make test` runs this
module's tests.

**`make package` refuses a non-Linux host build.** The manifest declares a Linux
ABI, so a darwin binary is for `make test`/inspection only; the guard runs
before packaging. The supported way to produce a device package is an arm64
Linux container (or a target toolchain):

```sh
# arm64 glibc <= 2.33: Debian bullseye (glibc 2.31)
docker run --rm --platform linux/arm64 -v "$PWD/../..":/src -w /src/apps/cadence \
  golang:1.22-bullseye sh -c 'apt-get update -qq && apt-get install -y -qq gcc libc6-dev libopus0 \
    && make check-abi GOOS=linux GOARCH=arm64 CC=gcc CGO_ENABLED=1'

# arm64 musl: Alpine (build needs only gcc/musl-dev; Opus is dlopen'd at run time)
docker run --rm --platform linux/arm64 -v "$PWD/../..":/src -w /src/apps/cadence \
  golang:1.22-alpine sh -c 'apk add --no-cache gcc musl-dev \
    && make check-abi GOOS=linux GOARCH=arm64 CC=gcc CGO_ENABLED=1 TARGET_ABI=linux-musl'
```

To run the codec test against the real device codec instead of the image's
package, mount the extracted rootfs's library and name it:

```sh
-v /path/to/extracted/root/usr/lib/libopus.so.0.11.1:/opus/libopus.so.0:ro \
-e CAPSULAD_OPUS_LIBRARY=/opus/libopus.so.0
```

`check-abi` builds, stages the manifest, and proves the binary's ELF interpreter
matches `TARGET_ABI` through the repository's shared
`tools/check-elf-interp.sh`. Package the staged directory on the build host with
`capsulad-cli app package build/apps/org.capsulad.cadence`; the packager never
rewrites `abi`. The musl container recipe requires `apk add build-base opus`
inside the image; on a host whose Alpine package mirror is slow or blocked,
install those in the builder image ahead of time.

`TARGET_ABI` selects the libc ABI the package declares and is checked against:

- `linux-gnu` (default) — the Rockchip stock board (aarch64 glibc 2.33) and the
  Amlogic stock board (glibc 2.28). The bullseye-built artifact requires no
  symbol above `GLIBC_2.17`, so it runs on both.
- `linux-musl` (alias `musl`) — the Amlogic Alpine board. It defaults `CC` to
  `<triple>-linux-musl-gcc` and writes `abi = "linux-musl"` into the staged
  manifest:

  ```sh
  make package GOOS=linux GOARCH=arm64 TARGET_ABI=linux-musl
  ```

A glibc-linked binary cannot run under musl however its manifest describes it,
so packaging runs `check-abi`: `tools/check-elf-interp.sh` reads the binary's
PT_INTERP (`readelf`, else the `.interp` string) and fails unless it names the
loader `TARGET_ABI` requires (`ld-linux-*` or `ld-musl-*`; static binaries are
accepted for either). The staged manifest and the binary therefore always agree.
`make package` uses the host-executable packager (`make capsulad-cli-host` in
the repository root) when cross-building.

## Install and select

```sh
capsulad-cli -data-dir build/demo app install \
  build/apps/org.capsulad.cadence-<version>.tar.gz
capsulad-cli -data-dir build/demo app list
```

Select it in the Web UI under **Settings → Voice assistant**, then set
`server_url` and the token. Selecting it starts the package; another assistant
or **none** leaves it stopped. The slot is a selected default provider, so an
abnormal exit is recovered with backoff; deselecting, uninstalling or shutting
the daemon down is the off switch.

## Verification and proof limits

```sh
cd apps/cadence && go test -ldflags '-linkmode=external' ./...
cd ../.. && make test-cadence
```

What the tests establish:

- **Wire compatibility**: the generated messages decode all 17 upstream
  `proto/fixtures/v2/*.binpb` golden frames and re-encode every one
  byte-for-byte (`Deterministic: true`, as upstream generates them), so field
  numbers, wire types and enum values are the upstream wire.
- **Framing**: an in-process peer exercises the RFC 6455 handshake (accept key,
  401, no extensions), masked client frames, ping/pong, fragmentation, close
  frames and the size cap.
- **Transport**: the real link client and websocket upgrade run against an
  independent loopback fake upstream that speaks the cadence.v2 device link
  (HTTP upgrade → Hello → Welcome → empty report → a dispatched SessionClose),
  with no remote service.
- **Codec**: a real libopus encode/decode round trip at 16 kHz mono → 48 kHz,
  with the frame size enforced. It skips only when libopus is absent.
- **State machine**: a fake link/audio/indication/codec drives wake and touch
  turns, mic uplink and sequence, reply playback and drain, follow-up reuse,
  hold-release silence, an early release that arrives before its turn,
  dismiss, error close, link loss, API refusal and expired activations.
- **Integrated daemon proof** (root module, not `make test`): `internal/daemon`
  `TestCadenceCaptureAndMicOverLocalServer` runs the packaged app under the simulator daemon
  against a loopback fake Cadence link and proves the real daemon grants the ring capture
  (`router` selection), shows a non-blank backend frame, opens the microphone, then dismisses
  and releases the microphone and capture. Run
  `go test -race -ldflags '-linkmode=external' ./internal/daemon -run TestCadenceCaptureAndMicOverLocalServer`.
- **Capture obligation**: a seam test proves the indication is presentation →
  non-blank first frame → `OpenCapture(resource, presentationID,
  CaptureModeUnspecified)` → (release) capture before presentation, and that a
  refused capture closes the presentation. The daemon side of the same contract
  (selection, freshness, visible content) is covered by the framework's
  `internal/daemon/indication_test.go`.
- **ABI honesty**: `make check-abi` rejects a binary whose ELF interpreter does
  not match `TARGET_ABI`, so a glibc build cannot be packaged as `linux-musl`
  (or the reverse). On a darwin host the check is skipped.

Both device packages are built and checked on arm64 Linux: a glibc artifact from Debian
bullseye (interpreter `/lib/ld-linux-aarch64.so.1`, highest required symbol `GLIBC_2.17`,
staged `abi = "linux-gnu"`) and a musl artifact from Alpine (interpreter
`/lib/ld-musl-aarch64.so.1`, only `libc.musl-aarch64.so.1` in `DT_NEEDED`, staged
`abi = "linux-musl"`; its Opus test ran against the Alpine rootfs's own `libopus.so.0`). Neither
binary links `libcapsulad` or `libopus`.

What is **not** established here: a real Cadence server's behaviour (no server
is reachable in this repository's tests), physical audio and LED gamma on the
device, and acoustic echo during a follow-up. Those need an enrolled device and
a running Cadence deployment; they are reported at the level actually observed,
never assumed.
