# Marusia

## Using this app

Marusia is an optional voice assistant for the recovered VK Marusia service. It
sends microphone audio to the configured vendor endpoint and plays the returned
TTS reply. The rest of this page documents the implementation, wire protocol and
recovered evidence for maintainers.

- **Before selecting it:** provision a valid vendor session or `xtoken_id`, and
  check the *Service endpoint*. **Test 2** and a custom `http://` base send the
  session and microphone audio without TLS; use them only on a trusted LAN.
- **Select it:** open **Settings → Voice assistant** and choose Marusia.
  Selection starts the app. Choosing another assistant, or **none**, leaves it
  stopped.
- **Controls:** a wake word or talk-key press starts a turn. The ring shows the
  capture indication and returns to the baseline when the turn ends. Press the
  circle/play-pause key to dismiss the current turn; the process stays ready.
- **Launch and stop:** open **Apps → Marusia → Details**. *Launch* starts a run
  without speaking. When it is not the selected assistant, *Stop* is durable;
  when it is selected, the control is shown as *Restart* and deselecting is the
  off switch.
- **Teardown:** dismiss the turn, then stop or deselect. Uninstalling keeps its
  settings for a reinstall.
- **Troubleshooting:**
  - **401, expired session or attach failure:** set a current session or
    `xtoken_id`, or point *Vendor session file* at the provisioned
    `session.json` (its `device_settings.json` is read beside it).
  - **"keyword not confirmed":** keep *Keyword policy* at `no_kw`. `kw_params`
    is only valid when the captured phrase really begins with the wake word.
  - **A turn produces no audio:** check the logs for the phrase result, the
    returned `tts` stream URL, HTTP status and decode. WAV and `audio/l16` are
    supported; Ogg Opus, MP3, AAC, FLAC, 24/32-bit and float WAV replies are
    refused with one actionable line.
  - **A plaintext endpoint warning appears:** that is deliberate; the service
    and credentials are not protected on `http://`.
  - **It never starts:** installing and launching need a compatible native
    runtime; the package declares `[native_sdk]`.

A first-party optional voice assistant for Capsula. It implements the VK/Marusia
voice-agent turn recovered from the vendor binary shipped on this hardware:

```
activation → visible ring indication → resolve session (settings → vendor file → attach)
           → Phrase/Speech bidi stream: init → 80 ms WAV chunks → eos → terminal finish
           → GET /phrase/result (phrase_id, capabilities_hex, device_id, session_id)
           → commands (text, tts stream_id, listen) → GET /stream?stream_id=...
           → decode (WAV or audio/l16) → playback → release everything
```

It claims `voice_assistant` like `apps/dumbassistant`, but performs real
recognition: the microphone audio goes to the vendor's service and the reply is
whatever `tts` body that service returns.

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
| Client capability payload: the `capabilities_json` setting, default `{"capabilities_hex":"31c0255a004201d1408a235de880400000"}` | `SpeechRequestInit.client_options.capabilities.json` | the device capability token the shipped firmware advertised on `/connect`; the field the previous build omitted |
| Optional `client_data_json` (empty by default) | `SpeechRequestInit.client_options.client_data.json` | operator-provided vendor payload; unset unless configured |
| Timezone name and the keyword policy | `SpeechRequestInit` | vendor's own init surface |
| Phrase id, capability token, device id and session id | `GET /phrase/result` query, with the session bearer | retrieve the text/tts/listen commands the gRPC stream announces only with its terminal finish |
| The TTS `stream_id` from the phrase result | `GET /stream?stream_id=…`, with the session bearer | download the speech body the result command names |

The attach reply's account id is decoded for the log and is never sent, and
`DeviceService.GetInfo` is a local framework call over the Unix socket, not
egress; only the `device_id` it reports can be forwarded as the identity above.

The gRPC stream's terminal `finish result` does not carry the answer. The app
follows the shipped agent's `phrase_result_getter` path: `GET /phrase/result`
with the phrase id and identity, then `GET /stream?stream_id=…` for the `tts`
command's body. Both use `User-Agent: voice-agent/1`; the stream body also
carries the session bearer. Those URLs are chosen by the server; the app does
not contact any other host. The bearer is read when the stream body is
requested, so a first-turn provisioning attach is visible to the media fetch.
It is scoped to the exact configured scheme/host/port and the generated
`/stream` path: a server-chosen `play_url` on another origin (for example a
music provider, or the same host over plaintext HTTP) is fetched with only
`User-Agent`, never the vendor session. A vendor TTS URL carries its signature
in the query string, so every log line and error reports it as
`scheme://host/path` only — the query and the fragment never reach the log.

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
| `keyword_mode` | `no_kw` | `no_kw` asks the service to transcribe the command directly; `kw_params` is applied to wake-word activations only, the one origin whose capture starts with the spoken keyword (touch and API turns always send `no_kw`) |
| `keyword` | `marusya` | keyword name used when `keyword_mode=kw_params` |
| `hw_id` | `9F25725B` | keyword id used when `keyword_mode=kw_params` |
| `capabilities_json` | `{"capabilities_hex":"31c0255a004201d1408a235de880400000"}` | `RequestClientOptions.capabilities.json`; the default is the recovered Capsula Mini token, documented as a model default rather than a proven vendor schema |
| `client_data_json` | empty | `RequestClientOptions.client_data.json`; empty leaves the field unset |

Validation: `session_id` and `session_secret` must both be set or both be empty
(a half-supplied pair is rejected with a reason and the previous configuration is
kept); `session_file` may be empty to disable the file fallback; `endpoint_custom`
must be an absolute `http(s)://` URL without a path, query or fragment;
`audio_format` must be `wave` or `ogg_opus`; `client_version`, `keyword` and
`hw_id` fall back to their defaults when blank; `keyword_mode` must be `no_kw` or
`kw_params`; `capabilities_json` must be non-empty valid JSON (an empty value
falls back to the model default, a malformed one is rejected and the previous
configuration is kept), and `capabilities_hex`, when present, must be an even
number of hex digits. Nothing is trimmed into a credential: the values are used
exactly as stored.

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

Playback (`play_url` and `tts` bodies):

| Format | Support |
|---|---|
| RIFF/WAVE, linear PCM, 16-bit, mono or stereo, 8–192 kHz | yes; mixed to mono and resampled to 16 kHz |
| `audio/l16`, linear 16-bit, mono or stereo, 8–192 kHz, big- or little-endian | yes; converted to mono s16le and resampled. The live 2026-11-21 TTS stream returned `audio/l16;rate=24000;channels=1;endianness=big-endian` |
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

The service also sends an `eos` frame before the answer and the terminal
`finish result`; `eos` is an upload acknowledgement, not the end of the turn,
and the app keeps reading until `finish`. The terminal `finish result` is a
signal, not the answer. After it, the app fetches `GET /phrase/result`; the
returned `text` command is logged
and used as the turn text, `tts` is played through `/stream?stream_id=…`,
`play_url` is played directly, `listen` is logged as unsupported (this build
does not open a follow-up turn), `send_event` is logged as unsupported, and
preload/background-music commands are ignored. The result fetch has its own
deadline after the terminal finish, so a slow result request is reported as a
result-fetch failure, not as a missing terminal frame; its failures log only
the failure class and HTTP status, never the response body or a redirect
`Location`. If the result fetch fails or is empty, the turn is reported as
**no answer** with the fetch detail, never as a success. When the uploaded PCM
stayed below the silence floor, it is reported as **no audio** instead.
`Phrase/GetResult` remains `Unimplemented`; the HTTP
result endpoint is the recovered path the stock agent's `phrase_result_getter`
uses. Interactive skills, flowmode, music providers and VK account linking are
out of scope; the direct GENERAL init plus the result fetch works without the
stock control-plane `grpc_flowmode`/`grpc_mainflow` toggles.

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
service returned `finish result` with no text and no command. That empty finish
was not a recognition failure: the answer was behind the separate
`GET /phrase/result` endpoint, which this build now fetches. The counters still
prove capture and upload; they are not a recognition result by themselves.

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

`MARUSIA_PROBE_CAPABILITIES_JSON` overrides the init capability payload (default:
the recovered Capsula Mini token), `MARUSIA_PROBE_CLIENT_DATA_JSON` sets
`client_data` (empty by default), and `MARUSIA_PROBE_KEYWORD_MODE` defaults to
`no_kw` — the same policy the app's own turns send, so a probe run reproduces a
real turn. `MARUSIA_PROBE_SDK_RESULT=1` calls the production `Speech.Result`
after the stream ends and prints the recovered events; `MARUSIA_PROBE_RESULT=1`
also fetches `/phrase/result` and prints its status and byte count, and
`MARUSIA_PROBE_STREAM=1` fetches and decodes the authenticated `/stream` body
through the production media path. The raw result body is never logged.
`MARUSIA_PROBE_RESULT_OUT` explicitly captures it to a **new** 0600 file and
refuses to overwrite an existing path; treat that file as sensitive.
`MARUSIA_PROBE_ACTIVATION_TYPE` sets `client_options.activation_type`
and is empty by default, which is also what the app's own turns send; the vendor's
shipped client always names one of its four wire values (`listen_command`,
`keyword`, `button`, `flowmode`), so an operator reproducing that init sets the
knob explicitly:

```
MARUSIA_PROBE_ACTIVATION_TYPE=listen_command … \
  go run ./cmd/marusia-probe
```

For a prompt whose audio starts with the wake word (the ones in
`/tmp/marusia-prompts`: `Маруся, как дела?`, `Маруся, включи музыку`), set
`MARUSIA_PROBE_KEYWORD_MODE=kw_params` to exercise the wake-word path:

```
MARUSIA_PROBE_SESSION=… MARUSIA_PROBE_SECRET=… MARUSIA_PROBE_DEVICE=… \
MARUSIA_PROBE_SERIAL=… MARUSIA_PROBE_WAV="/tmp/marusia-prompts/Маруся, как дела?.wav" \
MARUSIA_PROBE_KEYWORD_MODE=kw_params \
  go run ./cmd/marusia-probe
```

It prints the keyword policy, the activation type and the capability/client-data
payload sizes before the turn, then the init QoS, the chunk accounting with the
service's own status counter, every `text_so_far`, every command, the `play_url`
(shortened to `scheme://host/path`, because the query is the signature), and with
`MARUSIA_PROBE_STREAM=1` the authenticated `/stream` `Content-Type`, size and
decoded duration. Credentials are never written to disk or to the log; the
optional raw result capture is a separate, explicit 0600 file. The probe is
refused outright without credentials. It is a diagnostic, not a test: it needs
network egress and a live session.

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
family, and that the answer arrives in the separate phrase result as a `tts`
command naming a stream id.

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

### Phrase result and TTS body (2026-11-21)

The gRPC stream alone was not the answer path. On the same device and session,
a GENERAL `Phrase/Speech` turn with `no_kw`, 80 ms WAV chunks and the recovered
capability payload returned only the terminal `finish result` with no text and
no command. The app now fetches the commands exactly as the stock agent's
`phrase_result_getter` does:

| Observation | Result |
|---|---|
| gRPC frame order | `eos` arrives before `text_so_far`/commands and the terminal `finish result`; `eos` is not treated as terminal |
| `GET /phrase/result?phrase_id=…&capabilities_hex=…&device_id=…&session_id=…` with the session bearer, redirects disabled | `200` with `result.commands`: `text`, `tts` (`stream_id`) and `listen` |
| `GET /stream?stream_id=…` with the session bearer | `200`, `audio/l16;rate=24000;channels=1;endianness=big-endian`, 349,200 bytes |
| `media.DecodeMedia` on that body | 7,275 ms of 16 kHz mono s16le |
| stock production turn on the same device/session (2026-11-21) | `phrase_result_getter` commands `text`/`tts`/`listen`, then `media_fetcher` fetched the `/stream` body |

The stock control plane advertised `grpc_flowmode=1` and `grpc_mainflow=1`, but
the direct GENERAL probe above produced the same result commands without either
toggle. The recovered result endpoint is therefore the missing app step, not
flowmode or credentials.

### Capability payload and keyword policy (2026-11-17)

The previous build opened every phrase without
`RequestClientOptions.capabilities`, and every turn ended with a terminal
`finish`, no `text_so_far`, no command and no `play_url`. Two things changed,
both from recoverable evidence rather than a guess:

- **The capability token is recovered.** The device userdata image
  (`_capsula-mini-rockchip-backup/11_userdata.img`) contains the shipped agent's
  own telemetry URLs. Every `/connect` call carried
  `capabilities_hex=31c0255a004201d1408a235de880400000`, i.e. the exact
  capability advertisement of firmware 44.91. Disassembling the agent shows it
  is built by `sprintf("%llx%016llx%016llx", …)` over a capability bitmask
  (`voice-agent` offset `0x12fbb0`), so it is per-firmware, not per-unit. The
  app now sends it in `client_options.capabilities.json` as the documented
  model default
  `{"capabilities_hex":"31c0255a004201d1408a235de880400000"}`
  (`capabilities_json` overrides it).
- **The keyword policy was applied to the wrong activations.** `kw_params` makes
  the service confirm the keyword from the audio it receives. A wake-word
  capture does begin with the keyword, because the framework attaches up to 1 s
  of pre-activation audio to it (`internal/audio` `prerollMS`, emitted on the
  activation grant). Touch and API activations receive the same pre-roll, but
  for them it is room audio ahead of the command, so their phrase never starts
  with the keyword and claiming one can only end in "keyword not confirmed".
  The policy is now per-activation: `kw_params` is sent for wake-word
  activations when `keyword_mode=kw_params`, and every other origin always sends
  `no_kw`. The default stays `no_kw`, since the wake-word pre-roll path is the
  only unproven part of the claim.

The terminal `finish` these earlier turns observed is not a recognition verdict:
the answer is behind the separate phrase-result endpoint described above. The
capability and keyword work is init correctness, not the missing answer path.

What is *not* recovered: the exact JSON schema the vendor puts in
`capabilities.json` (`bytes json = 1`). The binary sets the field present but its
value comes from the control connection, and no schema string exists in the
image. The default above therefore carries the only recovered evidence under the
same key the vendor's `/connect` uses; it is a model default, not a claim about
the vendor's schema, and `capabilities_json` replaces it verbatim when the exact
payload is known.

Unproven, and therefore not claimed by the app:

- **Opus TTS.** The stock log's `tts` command carried `encoder:"opus"`; the
  2026-11-21 result used `encoder:null` and `/stream` returned `audio/l16`. This
  build plays WAV and `audio/l16`; an Opus body remains undecoded and fails with
  the Ogg line. The app does not send an encoder preference yet.
- **Follow-up listen.** The result's `listen` command (`forced_listen`,
  `listen_previous_intent`, `min_waiting_time`) is logged but not acted on; the
  app returns to idle instead of opening the follow-up turn.
- The service accepting an attach with an empty `xtoken_id`: it does not (4002
  "xtoken_id is required"). The attach path therefore exists only for a
  provisioned unit.
- `brand`, `activation_type` and `player_data` are not populated; the live
  GENERAL path did not need them, but their effect on other skills is
  unverified. `client_data` is only sent when `client_data_json` is set.
- Port 443 for `https` bases.
- Local end-of-speech is an energy gate (learned noise floor, 700 ms hangover,
  6 s no-speech, 15 s cap). Server `status_num` values are logged in the turn
  audio line but not interpreted.
- A failed or empty playback is terminal; `CommandSendEvent` is not sent back.
- Coordinates and the `x-geo-lat`/`x-geo-lon` metadata are omitted (there is no
  location source), as is `x-splits`.