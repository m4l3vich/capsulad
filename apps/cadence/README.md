# Cadence Client

## Using this app

Cadence Client is an optional voice assistant for a **self-hosted Cadence
server**. It streams microphone audio as Opus, plays the server's streamed reply
and server announcements, and uses the ring as its capture indication. The rest
of this page documents the protocol and implementation for maintainers.

- **Before selecting it:** set *Cadence server URL* and either an *Enrollment
  code* from the Cadence dashboard or a *Device token* (or a token file). Add a
  CA certificate if the server uses a private one, either pasted as PEM or as a
  path in the shared Assets store. The app has no insecure TLS mode.
- **First enrollment:** in the Cadence dashboard open **Devices → Enroll a
  speaker**, choose **VK Capsula Mini** (the recommended model for this
  speaker) and a name, and make the code. Set *Cadence server URL* and paste the
  code into *Enrollment code*; the app exchanges it, stores the returned device
  token, clears the code and links within a few seconds. The dashboard then
  shows the device online. If the dashboard's default **Simulator** model was
  left selected, set *Cadence board* to **Simulator** before applying the code,
  or make a new code for **VK Capsula Mini**.
- **Select it:** open **Settings → Voice assistant** and choose Cadence Client.
  Selection starts the app. Choosing another assistant, or **none**, leaves it
  stopped.
- **Controls:** a wake word or talk-key press starts a turn. The ring shows teal
  while listening, a blue chase while thinking and a green chase while speaking.
  Press the circle/play-pause key to dismiss the running turn or announcement;
  the process stays ready.
- **Announcements:** the server may speak an announcement outside a turn (a task
  event, an API announcement). It plays on the speaker, ducks other playback,
  and uses no microphone or ring state. A newer announcement cuts the older one.
- **Follow-up:** with *Follow-up windows* enabled, the microphone and indication
  stay open after a complete reply until the server's window closes.
- **Launch and stop:** open **Apps → Cadence Client → Details**. *Launch* starts
  a run without speaking. When it is not the selected assistant, *Stop* is
  durable; when it is selected, the control is shown as *Restart* and
  deselecting is the off switch.
- **Teardown:** dismiss the turn or announcement, then stop or deselect.
  Dismissal cancels the session, aborts reply or announcement playback and
  releases the microphone and ring.
- **Troubleshooting:**
  - **Connection or 401 errors:** check the server URL, device token and trust
    roots. `http://` is allowed for a LAN server but logs a warning and sends
    the token, the enrollment code and audio unencrypted.
  - **Enrollment refused:** the code is unknown, expired (30 minutes), already
    used, or was made for another board. Check the log's `enroll_refused`
    line, make a new code, and match *Cadence board* to the dashboard's model.
    **VK Capsula Mini** is the supported model; if the dashboard's default
    **Simulator** was used, set *Cadence board* to **Simulator** (or create the
    code for VK Capsula Mini).
  - **The app will not launch:** it needs `libopus.so.0` from the image and a
    compatible native runtime; nothing is bundled. An Alpine image must keep
    the `opus` package.
  - **A wake-word or button turn works but an automation activation does not:**
    this client deliberately refuses API activations because they are not
    evidence that the room's owner spoke.
  - **No reply:** check the server's close code and the app logs; a failed
    codec, uplink or playback path holds a red ring briefly and releases the
    capture.
  - **Follow-up starts unexpectedly:** turn off *Follow-up windows*; it is
    enabled by default.

Cadence Client is an optional first-party assistant for capsulad and a **client
of a self-hosted [Cadence](https://git.shino.boo/devilreef/cadence/) server**.
It claims the `voice_assistant` role and drives the normal assistant turn: the
framework's wake/touch activation opens the microphone, the ring is the visible
capture indication, the microphone is streamed to the Cadence server as Opus,
and the server's streamed Opus reply, and any server announcement, is played
through the framework's speaker endpoint.

It is ordinary supervised application code, not a system app: nothing installs,
starts or selects it automatically. It bundles no Cadence code and no
upstream source. The protobuf files here are an independently written subset of
the upstream `cadence.v2` definitions (the same field numbers, names and enum
values, which is what interoperability requires). The `.binpb` files under
`internal/proto/testdata/upstream` are upstream-generated golden frames, copied
verbatim as interoperability **test vectors** from the upstream `main` commit
`f9571ad0d89acd7326414adf04a85ea1c9dec131`; they are data, not code, and are not
part of the installed package. The enrollment board ids and the announcement
flow were checked against upstream `main` at
`9a6b257fd6e21b8f0e725461af332489fb11e383`, where `vk-capsula-mini` is the
speaker model in `server/internal/enroll/boards.go`; that addition is why this
client reports an upstream id instead of the framework's human-readable model
name.

## What it is not

The client is a **voice-only speaker** on the Cadence link. It advertises the
`audio.output` capability only (empty config schema; the server uses its state
to choose reply/announcement audio), so the dashboard can list the speaker and
the server can speak announcements. The state always carries the stereo/128 kbps
channels and bitrate; it deliberately omits `level`, because the app has no live
framework volume-change subscription and a connect-time level would go stale.
Upstream treats a missing level as unknown: the dashboard hides its volume
control and the model leaves the volume out of its note, while channels and
bitrate still drive reply/announcement encoding. It does not advertise `media.player`, a
screen, cards or tasks: those are separate device roles. The server pushes at
most the empty `audio.output` config; the client acknowledges it and rejects a
config it cannot apply. Reply/announcement streams use the framework speaker
endpoint, with the reported stereo/128 kbps ceiling:

- **Announcements are voice-only.** A server `STREAM_PURPOSE_ANNOUNCE` stream
  plays on the speaker outside any session, with no microphone, ring or session;
  when it asks to duck, it gets its own conversation so other playback drops
  behind it. It is not a card, screen or task; the client speaks the Opus
  packets and ends with the server's `StreamEnd`. A newer announcement cuts an
  older one, including a tail still draining.
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
- **No media-player role.** The dashboard's **Media** card shows a device
  `MediaSessionState`/`media.player` session (music, video, a handoff), and its
  volume slider requires `audio.output`'s `set_level` command. This client has
  no media session and cannot set the framework master volume (a supervised app
  is denied), so it advertises no `media.player` and no `audio.output` commands.
  Assistant replies and announcements are streamed TTS, not media sessions;
  **Media** therefore stays "Nothing is playing", which is expected. Cards,
  screens, RPC and tasks are separate roles too.
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
| device → server | `CapabilityReport{descriptors: [audio.output], hash}` | full report |
| device → server | `StateDelta{capability:audio.output, json:{channels,maxBitrate}}` | after the report |
| server → device | `ConfigSet{capability:audio.output, generation, json}` | acknowledged with `ConfigApplied` |
| device → server | `SessionStart{session_id, trigger, wake}` | activation / follow-up |
| device → server | `MicAudio{session_id, seq, opus}` | 20 ms 16 kHz mono frames |
| device → server | `SessionCancel{session_id, reason}` | dismiss, timeouts, capture loss |
| server → device | `SessionEvent{speech_started, listening, thinking}` | turn phases |
| server → device | `StreamStart{stream_id, session_id, purpose:REPLY, codec:OPUS, channels}` | reply begins |
| server → device | `AudioDown{stream_id, seq, opus}` | 20 ms reply packets |
| server → device | `StreamEnd{stream_id}` | reply complete |
| server → device | `StreamStart{stream_id, session_id:"", purpose:ANNOUNCE, codec:OPUS, duck:true}` | announcement begins |
| server → device | `AudioDown{stream_id, seq, opus}` / `StreamEnd{stream_id}` | announcement packets and end |
| server → device | `SessionClose{session_id, code, follow_up_allowed, follow_up_window_ms, failure}` | turn end |
| server → device | `Duck`, `VoiceLevel`, `MicOpen` | advisory/other roles; parsed, not acted on |

The announcement contract was checked against upstream source, not assumed from
the enum: `server/internal/voice/announce.go` sends `StreamStart` with
`Purpose: ANNOUNCE`, an empty `session_id`, `Duck: true`, then paced `AudioDown`
packets and one `StreamEnd`; `device/internal/session/controller.go` plays it
outside a session, refuses one that carries a session id, and cuts an older
announcement when a newer starts. Upstream `main` at
`9a6b257fd6e21b8f0e725461af332489fb11e383` adds only optional `TraceContext`
fields to these messages, which this client skips as unknown fields.

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
`ca_pem` appended to the system store) performs the link connection,
`capsulad.OpenHTTP` performs the one-time enrollment exchange, and this app's
RFC 6455 client (`internal/websocket`) frames the link. The app links no
`net/http`, `crypto/tls` or gRPC stack.

## Interaction contract

| | Behavior |
|---|---|
| **Trigger** | `AssistantActivation` with `ACTIVATION_WAKEWORD` → `TRIGGER_WAKE` (the matched model id is sent as `wake.engine`); `ACTIVATION_TOUCH` → `TRIGGER_BUTTON`; `ACTIVATION_API` refused with an actionable log (an automation trigger is not a physical owner signal). Up to 8 activations queue, expired ones drop, and one turn runs at a time. |
| **Feedback** | The ring presentation is opened, its **first frame is written, then the routing capture is acquired** — the content and the selection the daemon's indication check requires before any microphone opens. Listening is teal, brightness from the real captured RMS over −50…−10 dBFS (fast attack, slow release; a dark ring is never shown); the level only modulates the listening phase, so a still-open microphone cannot repaint thinking/speaking. Thinking is a blue chase, speaking a green chase. A local failure (codec, encode, uplink, unsupported capture) holds a red ring for 300 ms; mute and a lost indication deliberately leave the framework's own state alone. |
| **Ownership** | One turn owns one Cadence session at a time, one microphone capture, one conversation (so other playback ducks), one encoder, and a bounded reply-stream writer per server stream. |
| **Interruption** | A newer activation cancels the running turn (`superseded`). A hold-to-talk release ends input, then streams `hold_release_tail_ms` of paced digital silence so the recognizer closes the utterance. A mute, indication watchdog, capture close, link loss, listen cap (30 s) or no-speech window (8 s) cancels the turn. |
| **Dismissal** | The manifest `dismiss` action (the circle/play-pause key) sends `SessionCancel{dismissed}`, aborts reply audio, releases microphone and ring, and drops queued activations. |
| **Final state** | Every terminal path releases the microphone, the ring capture, the presentation, the conversation, playback and encoder (the capture before the presentation). A successful `SessionClose{code:"ok"}` drains the reply tail (bounded 5 s) before the conversation closes; any failure or cancellation discards queued audio immediately. |
| **Announcement** | A `StreamStart{purpose:ANNOUNCE}` with no session opens one playback stream, plus its own ducking conversation when `duck` is set, and writes no ring or microphone state. It ends with `StreamEnd`, drains its tail (bounded 5 s), then releases the conversation. A newer announcement cuts the older stream, a draining tail included; `dismiss`, link loss and shutdown abort it at once. An announcement that carries a session id, or a codec other than Opus, is refused with one log line. It never opens a follow-up window. |

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

The app is ready with no server or no token: it logs `link_unconfigured` once,
activates nothing, and waits. A settings change re-dials immediately (`Wake`).
When a server and an enrollment code are present but no token is, the app
exchanges the code first; the link opens only after a token exists.

## Settings

| Key | Type | Default | Meaning |
|---|---|---|---|
| `server_url` | text | empty | Base URL; the link is `<server_url>/v2/device` and enrollment is `<server_url>/v2/enroll`. A path prefix is allowed (reverse proxy). http:// is allowed but warns: token, enrollment code and audio travel unencrypted. |
| `board` | select | `auto` | Upstream board id reported in `Hello` and used in the enrollment fingerprint: `auto`, `vk-capsula-mini` or `sim`. Auto reports `vk-capsula-mini` on the speaker and `sim` on the simulator. Set `sim` only when the dashboard enrolled the hardware as its default Simulator. |
| `enroll_code` | text_masked | empty | One-time code from the Cadence dashboard. With no device token, the app exchanges it once at `/v2/enroll`, stores the returned token and clears this value. Single-use, 30-minute lifetime. Never logged. |
| `device_token` | text_masked | empty | Bearer token of the enrolled device. Never logged. `device_token_file` and a successful enrollment both feed it. |
| `device_token_file` | text | empty | File holding the token, re-read on every settings change. Wins over `device_token`; an unreadable or empty file is rejected and the previous config kept. |
| `ca_pem` | text | empty | Extra PEM certificate(s) for a self-hosted CA, appended to the system store. Either pasted PEM or a path in the shared Assets store (`cadence/ca.pem`, or the canonical `/assets/cadence/ca.pem` shown by the Assets page). A path is validated with the store's grammar, read fresh on each settings change, and must contain a CERTIFICATE block. There is no insecure mode. |
| `hold_release_tail_ms` | integer 0…5000 | 1000 | Silence streamed after a hold-to-talk release. Dead air the user hears as latency. |
| `follow_up` | boolean | true | Honor the server's follow-up windows. |

A malformed value is rejected, logged with a reason, and the previous
configuration is kept. A stale settings revision is ignored, so an older
snapshot cannot undo the token this app's own enrollment wrote. Every log line
passes through a redactor that scrubs the current token and enrollment code; a
URL is shortened (no query or fragment) before it is echoed.

### Enrolling the device

The default path is the Cadence dashboard:

1. In the dashboard open **Devices → Enroll a speaker**, choose **VK Capsula
   Mini**, name the speaker and create the code. The dashboard's list includes
   `vk-capsula-mini` and `sim`; this client is a speaker, so `vk-capsula-mini`
   is the right model.
2. In the capsulad Web UI open **Apps → Cadence Client → Settings**. Set
   *Cadence server URL* and paste the code into *Enrollment code*. Leave
   *Cadence board* on **Automatic** for the recommended choice.
3. The app sends the upstream contract
   `POST <server_url>/v2/enroll` with
   `{"code":"…","fingerprint":{"board":"vk-capsula-mini"}}`. The server checks
   the code's model against the fingerprint, returns `deviceId` and `token`
   once, and the app writes the token to its own `device_token` setting,
   clears `enroll_code`, and links.
4. If the code was made for the dashboard's default **Simulator** on real
   hardware, set *Cadence board* to **Simulator** before applying it (or make a
   new code for VK Capsula Mini). The board must match; a mismatch is refused
   as `enroll_refused`, because the upstream server only redeems a code for the
   model it was created with.

The dashboard's "Run the command" line is for Cadence's own `cadenced` device
daemon, not for a capsulad package. On this speaker the code is used by the app
as above (or by the manual exchange below); there is no `cadenced` install step.

A refused code is logged once (`enroll_refused`); it is unknown, expired (30
minutes), already used, or made for another board. A transport failure is
logged as `enroll_failed` and leaves the code set, so saving the settings again
retries it. The token never appears in a log. The token and the spent-code
clear are committed in one compare-and-swap at the settings revision that
started the exchange; if the server, board, code or CA changed while the
request was in flight, or another settings edit advances the revision before
the commit, the answer is discarded and the code needs a new one.
A settings event carrying the committed token is applied idempotently. The success line names the
board only: the server-supplied device id is untrusted text and is not logged.

The manual path remains available: exchange the code with the server and paste
the returned token into *Device token*, or write it to a file named by *Device
token file* (which wins over the stored setting).

```sh
curl -fsS -X POST https://cadence.example.com/v2/enroll \
  -H 'Content-Type: application/json' \
  -d '{"code":"K7M4QX","fingerprint":{"board":"vk-capsula-mini"}}'
```

The client sends `Hello.protocol = 2` and the resolved board id; a server that
speaks another protocol refuses the link with a protocol close and the client
backs off.

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
`server_url` and an enrollment code (or a token). Selecting it starts the
package; another assistant or **none** leaves it stopped. The slot is a
selected default provider, so an abnormal exit is recovered with backoff;
deselecting, uninstalling or shutting the daemon down is the off switch.

## Verification and proof limits

**Live diagnosis (2026-11-21).** A running Cadence deployment showed this
device online (`link_open`, no close) but with no capabilities, and dashboard
**Say** attempts failed with server code `no_audio`; upstream requires an
`audio.output` state for announcements. The source now advertises
`audio.output` and sends that state. The host tests below exercise the fixed
source; **the live speaker was not rebuilt, redeployed or otherwise touched for
this diagnosis**, so the deployed package still has the old behavior until an
explicitly authorized install/restart. Dashboard **Media** remains a separate
media-player role (see above), not part of this fix.

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
- **Capability advertisement**: the real handshake advertises one `audio.output`
  descriptor (empty config schema, `{channels,maxBitrate}` state schema,
  no commands), its hash is the report hash, and the following `StateDelta`
  carries the stereo/128 kbps channels and bitrate and omits `level` (no
  connect-time snapshot and no synthesized value). A `ConfigSet` is
  acknowledged on its own connection (never the controller queue), and a
  non-empty config is refused explicitly.
- **Transport**: the real link client and websocket upgrade run against an
  independent loopback fake upstream that speaks the cadence.v2 device link
  (HTTP upgrade → Hello → Welcome → audio.output report/state → a dispatched SessionClose),
  with no remote service.
- **Codec**: a real libopus encode/decode round trip at 16 kHz mono → 48 kHz,
  with the frame size enforced. It skips only when libopus is absent.
- **State machine**: a fake link/audio/indication/codec drives wake and touch
  turns, mic uplink and sequence, reply playback and drain, follow-up reuse,
  hold-release silence, an early release that arrives before its turn,
  dismiss, error close, link loss, API refusal and expired activations.
- **Announcements**: the same seams prove an announce stream plays outside a
  turn with no microphone or ring capture, writes its packets, drains on
  `StreamEnd` and releases its conversation; a newer announcement aborts the
  older one, including a tail whose playback is deliberately stalled; dismiss
  and link loss abort it, and an aborted announcement cancels its SDK playback
  context; a playback failure reports `playback_failed` and releases its
  conversation without waiting for another frame; `StreamEnd` logs the drained
  outcome, not an early `ok`; completions carry a local owner token, so a reused
  server stream id cannot clear its replacement; a session and an announcement
  stay independent. A session-bearing announcement and a non-Opus codec are
  refused without opening playback. Frames are tied to the connection
  generation, so a frame queued before a disconnect or left from an older link
  is dropped. Link lifecycle and terminal envelopes (`StreamEnd`,
  `SessionClose`) wait for room rather than being dropped by a saturated
  controller queue. Successful, failed and aborted announcements release the
  writer/playback context when they finish; they do not retain it until app
  shutdown.
- **Enrollment**: the exchange is tested against a fake HTTP client for the
  exact `POST /v2/enroll` body (code and `fingerprint.board`), 403 refusal,
  malformed 200 answers, transport failure and the path-prefixed URL. The
  commit is tested through a fake configuration service as **one**
  compare-and-swap carrying the token and the spent-code clear; a stale
  revision, changed server/board/code/CA, or a superseded attempt is discarded
  without touching the live link. Settings and enrollment publish through one
  revision-aware path, so an older enrollment result cannot overwrite a newer
  settings callback or change the shared log redactor away from the published
  configuration. Each attempt's context is cancelled on success, failure or
  supersession. Board mapping (`auto` on a Capsula Mini →
  `vk-capsula-mini`, simulator → `sim`) and the plan/cancel/keep behavior have
  their own tests.
- **CA asset path**: inline PEM and an asset-store path both produce trust
  roots; traversal, absolute paths, backslashes, dot components, symlinks,
  missing files and an asset without a CERTIFICATE block are rejected. Before a
  new CA is published the SDK parses it as a root store, so a CERTIFICATE label
  with unusable bytes keeps the previous working link. The link test proves an
  enrollment code alone is not a bearer token, and that `Hello.board` follows
  `SetBoard` after a board setting change.
- **Integrated daemon proof** (root module, not `make test`): `internal/daemon`
  `TestCadenceCaptureAndMicOverLocalServer` runs the packaged app under the simulator daemon
  against a loopback fake Cadence link and proves the real daemon grants the ring capture
  (`router` selection), shows a non-blank backend frame, opens the microphone, then dismisses
  and releases the microphone and capture.
  `TestCadenceEnrollmentFromDashboardCode` proves the real app spends a dashboard code at a
  loopback `/v2/enroll`, links with the issued token, and leaves the real app-settings store
  with the token stored and the code cleared. `TestCadenceAnnouncementReachesSimulatorPlayback`
  proves an announce stream reaches the simulator's speaker sink as non-zero PCM without
  opening a microphone or taking the ring capture, then ends and releases playback.
  `TestCadenceInvalidCAKeepsTheWorkingLink` proves an unusable CA asset is rejected by the SDK
  before the working link is replaced. `TestCadenceTLSEnrollmentAndRestart` writes a private CA
  to the shared asset store, enrolls over HTTPS, links over WSS, then restarts the selected
  assistant and proves the persisted token is reused without a second code redemption. Run all
  five with `go test -ldflags '-linkmode=external' ./internal/daemon -run 'TestCadence'`
  after a host runtime is built (`CAPSULAD_NATIVE_LIB_DIR` may point at it; the harness reads
  that variable).
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

What is **not** established here: a live Cadence server's behaviour (no server
is reachable in this repository's tests; the announcement and enrollment
contracts above are source-verified against upstream, not observed against a
deployment), physical audio and LED gamma on the device, and acoustic echo
during a follow-up. Those need an enrolled device and a running Cadence
deployment; they are reported at the level actually observed, never assumed.
