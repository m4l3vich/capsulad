# Marusia

A first-party optional voice assistant for Capsula. It implements the VK/Marusia
voice-agent turn recovered from the vendor binary shipped on this hardware:

```
activation → visible ring indication → resolve session (settings → vendor file → attach)
           → Phrase/Speech bidi stream: init → 80 ms WAV chunks → eos
           → commands (play_url) → HTTPS fetch → decode → playback
           → release everything
```

It claims `voice_assistant` like `apps/dumbassistant`, but performs real
recognition: the microphone audio goes to the vendor's service and the reply is
whatever TTS URL that service returns.

## What leaves the device

A turn sends the following to the configured vendor endpoint:

| Data | Where it goes | Why |
|---|---|---|
| Microphone PCM, 16 kHz mono, reframed into 80 ms chunks | the `Phrase/Speech` gRPC stream only | recognition; the attach body carries no audio |
| SoC serial: the `serial` setting, else `CAPSULAD_DEVICE_SERIAL`, framework `hardware_serial`, then `/proc/cpuinfo` | attach `serial`, gRPC `x-device-sn`, `DeviceInfo.serial` | device identity the vendor expects |
| Device id: the `device_id` setting, else the framework's `DeviceService.GetInfo` `device_id`, else a per-process UUID | gRPC `x-device-id` | the identity the vendor expects |
| Client version (`44.91` by default) | gRPC `x-device-ver`, `DeviceInfo.version` | client identity |
| `xtoken_id` (the configured value, possibly empty) | attach body `{"serial", "with_secret", "xtoken_id"}` | provisioning credential; only used when no session is configured or readable |
| Session id and session secret from the `session_id`/`session_secret` settings, else the vendor session file, else the attach reply | gRPC metadata (`x-session-id`, `authorization: Bearer …`) | runtime credential, held in memory only |
| A fresh request UUID | gRPC `x-client-request-id` on every RPC | request correlation |
| Timezone name and keyword parameters | `SpeechRequestInit` | vendor's own init surface |

The attach reply's account id is decoded for the log and is never sent, and
`DeviceService.GetInfo` is a local framework call over the Unix socket, not
egress; only the `device_id` it reports can be forwarded as the identity above.

The reply path fetches the URL from `CommandPlayUrl` with
`User-Agent: voice-agent/1`. That URL is chosen by the server; the app does not
contact any other host. A vendor TTS URL carries its signature in the query
string, so every log line and error reports it as `scheme://host/path` only —
the query and the fragment never reach the log.

The transport is TLS for every `https://` endpoint, which is four of the five
recovered deployments. `test2` is plaintext `http://` in the vendor's own table;
selecting it (or a custom `http://` base) sends the session and the `xtoken_id`
without TLS, and the choice is logged as a warning at startup instead of being
silently downgraded — see [Plaintext endpoints](#plaintext-endpoints).

No vendor asset, model, animation frame or proprietary decoder is redistributed.
The protobuf schemas under `proto/` are transcriptions of the descriptor set in the
shipped binary; the generated bindings are built from them. The codec matrix is
implemented here, not borrowed.

## Authentication

The voice service refuses a new session without a session credential. On this
device the vendor agent's own session is still valid, so the app authenticates
with it and never attaches while a session exists. Resolution order, first hit
wins:

1. the masked settings `session_id` + `session_secret` (with `device_id`), applied
   live on every settings push without a restart — **this is the supported path**;
2. the vendor agent's state file at `session_file` (default
   `/data/voice-agent/session.json`, with `device_settings.json` read beside it for
   `general.id`). The device's directory is `root:root 0600`, so a sandboxed app
   normally cannot read it; the read is best-effort and a redacted reason is
   logged when it fails;
3. the provisioning attach (`POST /account/attach_xtoken`), which needs
   `xtoken_id`. A refusal with service code `4002` ("xtoken_id is required") is
   treated as *not provisioned*, not as transient: it is reported once with an
   actionable line and the same client never retries it.

Credentials are never logged. `session_secret`, `xtoken_id` and `device_id` are
declared `text_masked`, and the log names a source ("settings", "vendor session
file", "attach") plus shortened identifiers — an 8-character account prefix and,
on the reauth line, an 8-character session prefix — never the values
themselves. The reply URL is shortened the same way: `scheme://host/path`, with
the signed query and fragment stripped, in the completed-turn line and in every
media error.

A 401/403 from the service (gRPC `UNAUTHENTICATED`/`PERMISSION_DENIED`) drops the
session once; the next activation re-resolves it in the order above, so a
refreshed settings push or a refreshed file is picked up. The `reauth` action
validates the configured session (or attaches when there is none) and reports one
line.

### What expires

The vendor session is a bearer credential tied to the vendor account. It ends when
the vendor revokes it: a password/account change, a "log out of all devices", or
the session's own lifetime. When that happens the service answers `UNAUTHENTICATED`
and the app logs one line telling the operator to reload the session. Recovery is
always an operator action — there is nothing on the device that can mint a new
session, and `xtoken_id` is a manufacturing credential this unit does not hold.

## Settings

| Key | Default | Meaning |
|---|---|---|
| `endpoint` | `prod` | one of the five recovered deployments; `test2` is plaintext `http://` and triggers the startup warning |
| `endpoint_custom` | empty | overrides `endpoint`; `http(s)://host[:port]` with no path; an `http://` base triggers the same warning |
| `session_id` | empty | vendor session id; masked. Must be supplied together with `session_secret` |
| `session_secret` | empty | vendor bearer secret; masked, never logged |
| `session_file` | `/data/voice-agent/session.json` | best-effort fallback source; the sibling `device_settings.json` supplies `general.id` |
| `xtoken_id` | empty | manufacturing/provisioning credential, masked; only used by the attach path, i.e. when no session exists |
| `serial` | empty | optional override for the SoC serial; an explicit value wins over `CAPSULAD_DEVICE_SERIAL`, framework `hardware_serial`, and `/proc/cpuinfo`. If none is available, `x-device-sn` is empty and the vendor rejects the turn with a message naming this setting |
| `device_id` | empty | stable `x-device-id`, masked; also the HTTP identity |
| `audio_format` | `wave` | upload framing; `ogg_opus` is declared but refused |
| `client_version` | `44.91` | advertised client/firmware version |
| `keyword` | `marusya` | keyword name in `keyword_options.kw_params` |
| `hw_id` | `9F25725B` | keyword id in `keyword_options.kw_params` |

Validation: `session_id` and `session_secret` must both be set or both be empty
(a half-supplied pair is rejected with a reason and the previous configuration is
kept); `session_file` may be empty to disable the file fallback; `endpoint_custom`
must be an absolute `http(s)://` URL without a path, query or fragment;
`audio_format` must be `wave` or `ogg_opus`; `client_version`, `keyword` and
`hw_id` fall back to their defaults when blank. Nothing is trimmed into a
credential: the values are used exactly as stored.

The sandbox has no writable filesystem and the SDK does not write app settings,
so an empty `device_id` cannot be persisted by the app. The stable device id
comes from the framework instead: the app reads `DeviceService.GetInfo`
(app-readable, over the local socket) and uses its `device_id` for
`x-device-id`. Only if that call fails or reports nothing does the app fall
back to one UUID per process; restarting then changes the identity. The
`device_id` setting always wins and is masked: the app never logs its value.

The device serial is separate. It is resolved once per process in this order:
a non-empty `serial` setting, `CAPSULAD_DEVICE_SERIAL`, framework
`DeviceService.GetInfo.hardware_serial`, then local `/proc/cpuinfo` (the same
value the vendor agent sends). The selected source is logged once as
`device serial source=settings`, `device serial source=env`,
`device serial source=framework` or `device serial source=cpuinfo` — never the
value. The environment variable is injected by the launcher from the daemon's
host serial and is available even when an older native runtime drops the
framework field from JSON. If no source yields a serial, the request is still
sent but the vendor rejects it; the turn then reports that the device serial
is missing and names the `serial` setting instead of echoing the vendor error
alone. The serial is never `device_id`.

### Plaintext endpoints

Four of the five recovered deployments are `https://`. `test2`
(`http://voice-test2.ngx.openstack.go.devmail.ru`) is plaintext in the vendor's
own endpoint table, and it is kept as an explicit operator test choice rather
than removed or silently upgraded to TLS: upgrading would change where the
request goes, and refusing it would hide the vendor's own test surface.

The consequence is stated, once per process, instead of being implied. When the
selected base URL is `http://` and a credential would be sent over it, the app
logs:

```
marusia: WARN endpoint http://voice-test2.ngx.openstack.go.devmail.ru is plaintext http:// and the session is sent as an unencrypted bearer token; a passive observer on the path can read it. Use an https endpoint unless this is a deliberate test.
```

The same line names the `xtoken_id` attach body or the session file when that is
the credential source. An `http://` base with no credential source at all logs
nothing. The warning is emitted once per process: at startup when the initial
settings select a plaintext endpoint, otherwise on the first settings push that
does.

## Media formats

Playback (`CommandPlayUrl` bodies):

| Format | Support |
|---|---|
| RIFF/WAVE, linear PCM, 16-bit, mono or stereo, 8–192 kHz | yes; mixed to mono and resampled to 16 kHz |
| Ogg Opus, MP3, AAC, FLAC, 24/32-bit or float WAV, RF64 | no; the turn fails with one actionable log line and the ring shows the error state |
| `http`/`https` only, ≤ 10 MiB, ≤ 4 redirects, 30 s total | enforced |

Upload:

| Format | Support |
|---|---|
| `wave` — one self-contained RIFF/WAVE file per 80 ms chunk, 16 kHz mono s16le | yes, the default |
| `ogg_opus` — Opus packets in Ogg | no encoder is shipped; the setting is refused instead of silently sending another format |

Upload framing is fixed by live service behaviour, not by preference:

| Rule | Evidence |
|---|---|
| `chunk_num` starts at 0 and increments by one | the service replies "First incoming chunk must be with 0 index" otherwise |
| every chunk is a **self-contained WAV file** | raw PCM is refused with "audio data is not valid WAV" |
| the WAV header carries **real** sizes | a streaming `0xFFFFFFFF` header is refused, and a header that disagrees with the chunk's payload is refused with "PCM sizes mismatch <header> in header != <read> actually read" |
| chunks are 80 ms | the init QoS answers `chunk_length_ms_hint: 80`; the status frame counts `chunks × 80 ms` |
| `eos` does **not** half-close the request side | closing it makes the live service end the stream with "Internal server error" instead of the terminal frames |

Capture still arrives as 20 ms framework chunks; the uploader reframes them and
zero-pads the last 80 ms block, so the header size always matches the payload.

## Command surface

If a phrase ends cleanly with neither `text_so_far` nor a command, the turn is
reported as **no answer** — one line naming the terminal frame — and never as a
success. When the uploaded PCM stayed below the silence floor, it is reported as
**no audio** instead: the service had nothing to recognize, so an empty result
there is not evidence about the recognition gate. `play_url` plays; `show_text`
is logged; `send_event` is logged as unsupported; preload/background-music
commands are ignored. Interactive skills, flowmode, music providers, VK account
linking and `GetResult` are out of scope.

## Turn audio evidence

Every turn that opens a stream logs one INFO line with its audio evidence,
whatever the outcome:

```
marusia: turn audio uploaded_chunks=13 uploaded_ms=1040 rms_dbfs=-8.7 pre_open_dropped=0 service_chunks_received=13 service_audio_received_ms=1040 service_status_num=0 status_frames=1
```

`uploaded_chunks` and `uploaded_ms` are counted by the stream as it writes 80 ms
frames; `rms_dbfs` is the integrated RMS of exactly the PCM those frames carried;
`pre_open_dropped` is capture the queue discarded before the phrase opened; and
`service_*` echo the last `SpeechResponseStatus` frame the service sent (`-1`
when it sent none). A service count that matches `uploaded_*` within the 1 s
status interval proves the audio reached it as valid 80 ms framing; counters
stuck at zero or far below the upload are a transport or audio problem, not a
recognition result.

The first instrumented device turn proved that path end to end: 51 chunks /
4080 ms were uploaded, the PCM integrated at **-24.7 dBFS** (speech, not
silence), the service's own status frames counted **44 chunks / 3520 ms** —
everything up to its last 1 s status interval — and `pre_open_dropped=0`. The
audio therefore reached the service as valid 80 ms frames with the utterance
start intact, `status_num` was 2 (logged, still not interpreted), and the
service still returned `finish result` with no text and no command. That is the
vendor's recognition gate, named with counters instead of a hypothesis. The
remaining limit is scope: one captured turn proves this transport and capture
path and this gate, not that every future utterance will be gated the same way
or that the vendor will ever transcribe this unit.

The RMS is accumulated in the endpointer from the samples it already reads, so
measuring a turn adds no copy, no extra pass and no blocking to capture. Below
**-60 dBFS** the turn additionally logs `WARN` and reports **no audio**. A turn
that uploaded no chunk at all says so explicitly instead of blaming the vendor.

## Networking and build footprint

The application links no TLS, HTTP or gRPC stack. `internal/vkclient` and
`internal/media` define a small transport seam (HTTP `Do`, unary call, bidi
stream) and their production implementation is the shared `libcapsulad` SDK:
`capsulad.OpenHTTP` for the attach and media fetches, `capsulad.OpenGRPC` for
`Phrase/Speech` and `ClientProbe/AuthReady`. Protobuf-go still marshals messages;
grpc-go, net/http and crypto/tls appear only in `_test.go` files, which build a
throwaway in-process TLS server over that same seam. The opt-in `marusia-probe`
diagnostic uses the same native HTTP client. `go list -deps ./cmd/marusia` (and
`./cmd/marusia-probe`) is the authoritative check.

TLS is always verified. Production uses the system roots; the integration test
passes its throwaway CA explicitly, which is also how an operator would trust a
private deployment. No insecure switch exists.

Proof recorded from this tree:

```
$ go list -deps ./cmd/marusia | grep -E 'google.golang.org/grpc|^net/http$|^crypto/tls$'
# (no output)
$ ls -l marusia-old marusia-new
15875602  marusia-old   # HEAD: bundled grpc-go + net/http + crypto/tls
 6436386  marusia-new   # this tree: transport delegated to libcapsulad
```

A stripped release build of this tree is 4,249,810 bytes.

The 9,439,216-byte (9.0 MiB / 9.44 MB) reduction — 6,436,386 vs 15,875,602, a
59.4% smaller file — is the linked grpc-go/net-http/TLS machinery leaving the
binary, not a compression change. The low-level security fixes added 17,376
bytes over the pre-fix host build (6,419,010).

## Build and test

```
cd apps/marusia && go test ./...
cd apps/marusia && go vet ./...
make marusia        # package beside build/apps/org.capsulad.marusia/ as an archive
make test-marusia   # the module's tests from the repository root
```

`make marusia` needs `capsulad-cli` (the target builds the host copy when
cross-compiling). No test contacts an external host: the protocol tests use
in-process TLS listeners on loopback, and the turn tests use fake streams and
players.

One integration test drives the *production* native transport against those
in-process servers (HTTP attach + gRPC probe + bidi speech). It is skipped
unless a staged library is named, so an ordinary `go test` needs no Rust build:

```
make native   # from the repository root: stages build/lib/libcapsulad.1.dylib
CAPSULAD_TEST_LIBRARY=$PWD/build/lib/libcapsulad.1.dylib \
  go test ./internal/vkclient -run TestProductionSDKTransport -v
```

### Live probe (opt-in, never part of `go test`)

`cmd/marusia-probe` performs one real turn against the configured deployment with
credentials from the environment and nothing else:

```
MARUSIA_PROBE_SESSION=… MARUSIA_PROBE_SECRET=… MARUSIA_PROBE_DEVICE=… \
MARUSIA_PROBE_SERIAL=… MARUSIA_PROBE_WAV=/path/16k-mono.wav \
  go run ./cmd/marusia-probe
```

It prints the init QoS, the chunk accounting, every `text_so_far`, every command,
the `play_url` (shortened to `scheme://host/path`, because the query is the
signature), and then the HTTP status, `Content-Type`, size and first bytes of
that URL. No credential is written to disk or to the log; the probe is refused
outright without them. It is a diagnostic, not a test: it needs network egress and
a live session.

On this workstation the gRPC dial to the production host fails from a bare macOS
process (Happy-Eyeballs/DNS), and container DNS is intermittent; a Linux container
with the CA bundle mounted and the host pinned to an A record is what worked:

```
docker run --rm --platform linux/arm64 --entrypoint bash \
  --add-host vc.go.mail.ru:95.163.43.216 \
  -v "$PWD":/repo -v "$HOME/go/pkg/mod":/gomodcache -v /tmp/gocache:/gocache \
  -v /etc/ssl/cert.pem:/etc/ssl/cert.pem:ro -e SSL_CERT_FILE=/etc/ssl/cert.pem \
  -e GOMODCACHE=/gomodcache -e GOCACHE=/gocache -e GOPATH=/gopath -e GOFLAGS=-mod=mod \
  -w /repo/apps/marusia capsulad-bullseye:3 \
  -lc 'export PATH=/usr/lib/go-1.24/bin:$PATH; go run ./cmd/marusia-probe'
```

## What is proof and what is unproven

Proven from the recovered descriptors and string table: message shapes and field
numbers, the RPC path, the metadata header set and order, the attach JSON keys,
the 16 kHz mono capture rate, the `play_url`/`show_text`/`send_event` command
family, and that TTS arrives as a URL.

Proven live against the production deployment with the device's own session
(2026-11-15):

| Observation | Result |
|---|---|
| `ClientProbe/AuthReady` with the session metadata | accepted |
| `Phrase/Speech` init with `kw_params{keyword,hw_id,media_state}` and `qos.status_interval_ms=1000` | accepted; `phrase_id` returned, `qos{chunk_length_ms_hint:80, status_interval_ms:1000}` |
| init without `media_state` in `kw_params` | refused: "Invalid 'kw_params' param: unknown media state" |
| first `chunk_num` ≠ 0 | refused: "First incoming chunk must be with 0 index" |
| raw PCM chunk | refused: "audio data is not valid WAV" |
| streaming `0xFFFFFFFF` WAV sizes | refused: "Incoming chunk have not any data" |
| WAV header size ≠ payload size | refused: "PCM sizes mismatch … in header != … actually read" |
| 80 ms self-contained WAV chunks | accepted; `status{chunks=13, audio_received_ms=1040}` for a 1.015 s clip |
| `eos` followed by `CloseSend` | stream ends with "Internal server error" |
| `eos` with the request side left open | clean `finish` then end of stream |
| `Phrase/GetResult` | `Unimplemented` |
| the recovery session (`session.json`) on the gRPC surface | accepted, no attach performed |

Unproven, and therefore not claimed by the app:

- **Recognition itself.** Every phrase tried ended with a terminal `finish` but no
  `text_so_far`, no command and no `play_url`: `kw_params` aborts with "keyword
  not confirmed" (including for a real human recording of "Маруся" from the
  device's own wakeword samples), while `strict_kw`/`no_kw` answer `finish
  result` with zero text or abort "processing failed". Tried: 20/80/160 ms chunks,
  8 and 16 kHz, Ogg-Opus as one chunk and as Ogg pages, silence padding,
  `use_asrld_recognize`, and `activation_type` in {"", keyword, button, "listen
  command"}. A likely missing piece is the vendor's `ClientCapabilities.json` /
  `client_data` payload, whose schema the recon marked unrecoverable, or a
  keyword-confirmation exchange the stock client performs locally.
- **The TTS media type.** No `play_url` was ever returned, so the body was never
  fetched. The decoder therefore still accepts WAV PCM only and names whatever
  else arrives (Ogg, MP3, FLAC, MP4/AAC, RF64) in one line. Adding a decoder is
  the next step once the media type is observed.
- The service accepting an attach with an empty `xtoken_id`: it does not (4002
  "xtoken_id is required"). The attach path therefore exists only for a
  provisioned unit.
- `mode = GENERAL`, empty `brand`, empty `activation_type`, and no `client_data`,
  `player_data` or coordinates.
- Port 443 for `https` bases.
- Local end-of-speech is an energy gate (learned noise floor, 700 ms hangover,
  6 s no-speech, 15 s cap). Server `status_num` values are logged in the turn
  audio line but not interpreted.
- A failed or empty playback is terminal; `CommandSendEvent` is not sent back.
- Coordinates and the `x-geo-lat`/`x-geo-lon` metadata are omitted (there is no
  location source), as is `x-splits`.