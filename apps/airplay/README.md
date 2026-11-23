# AirPlay 2 receiver (`org.capsulad.airplay`)

An AirPlay 2 audio receiver for the Capsula Mini (Rockchip rev 4 and both Amlogic targets —
the app is board-agnostic and uses only framework services). An iPhone, iPad or Mac sees the
speaker in its AirPlay output list, the sender's audio is decoded by this app and
played through the framework mixer, the ring shows the cover art's colours, and
the household controls (media contract, panel gestures, Web UI "Now playing")
reach the sender.

The app is a normal supervised package. It never binds a socket: the daemon
allocates every listener (the RTSP control port, each audio channel and the
event channel) and passes the descriptor over the run's control channel. It
never talks to hardware: audio goes to `audio.output.speaker`, the ring is a
`light.ring.main` presentation with the routing capture that selects it, and
the daemon's Avahi responder publishes the mDNS records. The ring capture is
acquired beside playback and owned by the sender session, so a grant waiting
on routing never delays `RECORD` or the first buffered PCM block. No HTTP, TLS
or gRPC stack is bundled; the AirPlay protocol, its pairing crypto and its
codecs are implemented in this package on top of the Go standard library and
`golang.org/x/crypto`.

## Supported sender flows

| Flow | Status | Notes |
|---|---|---|
| AirPlay 2 transient pair-setup | Implemented | Ends at M4 exactly as upstream's `server_setup_response2` and pyatv's `hap_transient.py` do; the M4 reply is plaintext and the control/event channels are then keyed from the **SRP session key** (SHA-512 of S) with the standard salts. A later `/pair-verify` is accepted but never re-keys an established channel (upstream's verify→cipher block is commented out). |
| Pair-verify (normal pairing's secure channel) | Implemented | SRP-6a 3072/SHA-512, Curve25519, ed25519 controller identity, HKDF-SHA512. For the non-transient path — which upstream leaves without a working cipher — this receiver keys the channel from the ECDH secret after M4; a deliberate, documented deviation. Channels are direction-safe: the accessory seals the control channel with `Control-Read-Encryption-Key` and the event channel with `Events-Write-Encryption-Key`, the asymmetry upstream documents. |
| Normal (non-transient) pair-setup | Implemented | The setup code is the `pairing_pin` setting, default `3939` — the fixed code upstream's receiver uses when no AirPlay password is configured. Transient pairing (the HAP flag) is accepted and uses the same code, so both flows a sender can take succeed; a wrong code is refused with `kTLVError_Authentication`. |
| Pairing administration (`/pair-add`, `/pair-remove`, `/pair-list`, `/pair-pin-start`) | Implemented, with ownership and permissions | Pairings persist in the masked `pairings` setting (compare-and-swap), each with its public key and admin permission. The endpoints require the encrypted session of a paired **administrator**; a non-admin or unpaired session is refused with `kTLVError_Authentication`, and removing the session's own controller ends that session. `/pair-pin-start` answers an empty `200` like upstream. |
| AirPlay 2 realtime stream (type 96) | Implemented | ALAC, AAC-LC and raw PCM, 44.1/48 kHz, up to 2 channels; RTP **over UDP** to the daemon-allocated `dataPort`, decrypted with the sender's `shk` (ChaCha20-Poly1305, per-packet nonce in the trailing 8 bytes, RTP header bytes 4..12 as AAD). |
| AP2 control plane (0xD4/0xD5/0xD6/0xD7) | Implemented | One session-wide UDP control socket, echoed as `controlPort`; the sender's sentinel/leftover filter matches upstream. `0xD5` resend requests are sent to the source address of the last control datagram, after a 100 ms reorder grace and at most every 250 ms per stream; `0xD6` resend responses are decrypted and played through the normal pipeline; `0xD7` provides the PTP anchor and stream latency; `0xD4` is accepted and recorded. No RTCP is sent or expected. |
| PTP-driven playout | Implemented when the runtime grants the multicast listener | The 0xD7 anchor's master-clock instant is converted to local time with the PTP offset to schedule each frame; without a disciplined clock the receiver paces from packet arrival. The PTP feature bits are advertised only when a clock exists, so the advertisement cannot promise timing nothing consumes. |
| AAC-LC decoding | Implemented (FFmpeg libavcodec) | The same decoder the upstream receiver links. Both `SETUP` paths accept upstream's AAC-LC/44.1K/F24/stereo and AAC-LC/48K/F24/stereo masks on FFmpeg builds. A build without the decoder withdraws only the buffered AAC formats and AAC feature bit; feature bit 40 and the pure-Go buffered ALAC format remain unless a feature override disables them. The AudioSpecificConfig is built from the format mask, and the decoder is verified against ffmpeg-encoded fixtures (sample-for-sample, mono bit-exact, stereo within 1 LSB of the encoder's own quantisation). |
| AAC-ELD decoding | Decoder available, not advertised | FFmpeg's AAC decoder handles ELD, so the eight ELD format masks are accepted if a sender sends one. Upstream does **not** advertise any ELD format (its RTSP format list is AAC-LC only), and neither does this receiver, so no parity claim is made beyond the decoder. |
| FairPlay handshake | Implemented, no DRM | `POST /fp-setup` serves the same fixed protocol replies as upstream (Shairport Sync `handle_fp_setup`), so a sender's handshake completes. Those constants decrypt nothing: FairPlay-encrypted content is not playable by either implementation. Feature bit 14 (Authentication4) is advertised by default and claims only this handshake, never DRM content or MFi. |
| AirPlay 2 buffered stream (type 103) | Implemented | Available only on the authenticated control channel, with a mandatory 32-byte `shk`. The daemon allocates the TCP `dataPort` and `SETUP` returns its actual 8 MiB bounded queue (and deliberately no `streamID`). Packets use the length-prefixed encrypted RTP layout and select ALAC 48 kHz/24-bit stereo (`0x15000000`, always available) or AAC-LC 44.1/48 kHz stereo (`0x16000000`/`0x17000000`, FFmpeg builds). Audio is buffered after `SETUP` until a valid `SETRATEANCHORTI` or the field client's `SETRATEANCHORTIME` anchor names `rtpTime` and network time; the anchor's timeline ID selects that sender's PTP source, and an absent or undisciplined source falls back to local arrival time rather than another source's offset. `FLUSHBUFFERED` implements immediate and deferred sequence/timestamp ranges, and pause, teardown, disconnect and takeover discard obsolete output. Playback state becomes PLAYING only after decoded PCM reaches the sink. |
| Legacy RAOP (ANNOUNCE + SETUP + RTP) | Implemented, without the timing exchange | SDP parsing, `a=rsaaeskey` decrypted with the receiver's RSA key (RSA-OAEP/SHA-1, the reference's mode), AES-128-CBC audio, UDP transport with receiver-chosen data **and control** ports (the control socket is drained; retransmission recovery is not implemented because no packet cache is kept), or interleaved TCP RTP (`$` frames on the control connection). The RAOP NTP-style timing exchange is not implemented and the SETUP reply advertises `timing_port=0`, so a legacy sender gets no receiver clock service. The RSA key is not published in `_raop._tcp` (see [advertisement records](#transport-and-lifecycle-details)), so a sender that takes the key from the TXT record cannot use the encrypted RSA path; the legacy SETUP response body still carries the RSA public key. |
| Metadata | Implemented | AirPlay 2 now-playing commands arrive as `POST /command` plists on the main encrypted RTSP control channel (`updateMRNowPlayingInfo` with an `npi-text` record: title/artist/album, duration, elapsed time, playback rate and embedded JPEG/PNG artwork), plus the legacy RAOP DMAP text and `progress:` parameter; both reach the media contract and the ring palette. Inbound commands on the event channel are also accepted as a tolerance, though Apple's sender and the reference receiver carry them on the control channel. |
| Remote control | Implemented | The sender's event channel is bidirectional: the receiver posts `sendMediaRemoteCommand` (play 0, pause 1, next 4, previous 5) with the standard `params` dictionary. |
| Group membership advertisement | Implemented to upstream's level | A `gid` sent with `SETUP` is recorded and republished as the mDNS `gid` with `gcgl=0`, which is what upstream does. Neither implementation synchronises playback across a group, so multi-room audio is *not* claimed. |
| Volume | Implemented; one persisted master | `SET_PARAMETER volume: dB` (text) and a binary-plist `volume` take the same path: only the promoted HAP-keyed active media owner can move the speaker master. Pause keeps ownership; candidates, event-only/type-130 connections, legacy plaintext RAOP and retired sessions get the reference's `200` no-op. The reference transfer is linear over the valid slider: -30..0 dB maps to master 0..1 (`(dB+30)/30`), -144 dB and values at or below -30 map to 0, and positive/non-finite/out-of-contract values are ignored rather than raising the speaker. `/info` `initialVolume`, `updateInfo`, `GET_PARAMETER` and `dvlc` all derive from that master; sender PCM is never scaled by it. | Shairport Sync `utilities/general_utilities.c:30-36` (transfer), `rtsp.c:3219-3247` (principal-only SET), `rtsp.c:1208` + `player.c:3398-3404` (initialVolume), `rtsp.c:3313-3322` (GET), `player.c:5420-5422` (persistence). |
| Device volume pushed to the sender | Implemented as a notification, never an echo | Local controls, the Web UI, Bluetooth master changes and AirPlay itself update the same persisted framework master; when that master changes from the receiver side, the app posts one authoritative `dvlc` media-remote command on the event channel after `updateInfo`, at the actual master unit and bound to the channel generation. It does not emit artificial intermediate values; a delayed echo is suppressed only by exact/quantization-aware correlation with a value actually staged on that channel, and a newer genuine SET keeps its display state; if a superseded dvlc lands after that SET, the next pass repairs the sender presentation to the authoritative master without writing it. While that notification token remains armed, a corrective mirror is deferred; echo consumption or the bounded expiry releases the next regular pass. A sender's own `SET_PARAMETER` records the applied level, so the watcher sends no `dvlc` for it. Without a readable master, `/info`, `updateInfo` and `GET_PARAMETER` report -144 dB (mute) and nothing is posted; connecting never writes the master. | Upstream posts the same message from `remote/remote.c` (`ap2_event_send_unit_volume_notification`, payload 334-380; its local-change loop is 383-410) and never from its RTSP SET path; `player.c:3823-3828` re-applies the reported initial volume at playback start. The framework write is the existing `AudioService.SetMasterVolume`; it persists through `applyMasterVolume` and keeps the Bluetooth master gain in step. |
| Timed playout (PTP) | Implemented when the runtime grants it | A PTP slave clock (`internal/ptp`) consumes the sender's Sync/Follow_Up stream through a daemon-allocated multicast listener; without it the receiver paces from the first packet's arrival. |

## Not implemented (and what it means)

| Gap | Consequence | Evidence / reason |
|---|---|---|
| AAC decoding in a build without the FFmpeg tag | Such a build refuses AAC at `SETUP` with an explicit reason and does not advertise AAC. The shipped target (`make airplay`) always builds with the decoder and fails early if the libraries are missing. | The decoder is a build dependency, not an optional extra: see [FFmpeg](#ffmpeg-build-dependency). |
| AAC-ELD advertisement | Accepted but not advertised, matching upstream. | Upstream's RTSP response lists AAC-LC formats only (`rtsp.c` `bufferStreamFormats`); FFmpeg could decode ELD but nothing asks for it. |
| FairPlay-protected content | Not playable; only the handshake is answered, and advertised bit 14 claims only that handshake. | Neither this receiver nor Shairport Sync implements the proprietary FairPlay cipher. |
| MFi | Not attempted. | Requires Apple hardware; upstream's mDNS strings deliberately avoid claiming MFi (`RELEASENOTES.md`, discussion 1691). |
| Pairing administration differences from upstream | Our model is deliberately stronger: pairings survive a restart, each has a stored permission, and the endpoints require an administrator session. | Upstream keeps pairings in an in-memory linked list with **no permissions** (`rtsp.c` `struct pairings`), answers `pair-list` with a hard-coded `Permissions = 1` and performs no session/authority check (`server_list` ignores the request), and a `pair-remove` of an unknown id still returns success (`server_add_remove_request` ignores the callback result). |
| PTP on a runtime without a multicast listener | The receiver advertises no PTP bit, logs the condition once, and falls back to arrival-time pacing. Multi-room AirPlay sync is then not guaranteed. | PTP needs the sender's 224.0.0.1:319/320 multicast stream. A listener that cannot join the group cannot receive it; the app never pretends it does. |
| Grouped/multi-room playback | One sender at a time; a second connection takes the session over. Group membership is advertised but playback is not synchronised. | Upstream is also a single principal session and only advertises `gid`/`gcgl`. |
| AirPlay video / screen mirroring | Not offered; only audio and authentication feature bits are advertised. | Out of scope for an audio receiver. |
| Type-130 remote-control stream (`SETUP`) | Acknowledged only inside an authenticated session. | The descriptor echoes `type=130` and the session `controlPort`; no listener is created. No sender flow in this record exercises it. |
| Exact `srcvers`/feature combination a given iOS release requires | Open device gate. | Senders switch behaviour on `srcvers` and on feature bits (remote control above 360, realtime at or below 355, `shk` above 350, buffered at bit 40, PTP at bit 41, the `/fp-setup` authentication bit 14). Both fields are settings so a device test can pin them; see [README](README.md#interoperability-gate) below. |


## FFmpeg build dependency

AAC-LC needs a decoder, and the one the upstream receiver uses is FFmpeg's. The
app links `libavcodec`/`libavutil` (LGPL-2.1+) and builds with `-tags ffmpeg`;
`apps/airplay/build.sh` refuses to package a receiver without them. Only the
decoder is used: sockets, encryption, timing, buffering and playback stay in
this app, so there is no second receiver, no daemon and no non-SDK transport.

- **Host build.** `pkg-config --exists libavcodec libavutil` must succeed
  (Homebrew/Linux FFmpeg development packages).
- **Device package.** Cross-building needs the target `libavcodec`/`libavutil`
  (statically, as upstream does: `--disable-everything --enable-decoder=aac
  --enable-parser=aac` plus the ALAC decoder upstream also enables). Point
  `PKG_CONFIG_PATH`/`PKG_CONFIG_LIBDIR` at the target sysroot, or pass
  `CGO_CFLAGS`/`CGO_LDFLAGS`. The cross build is a documented release step, not
  something this change could execute: no aarch64 FFmpeg or cross toolchain was
  available here, so the recorded package is a host build.
- **Tests.** `make test-airplay` adds `-tags ffmpeg` when pkg-config finds the
  libraries; the AAC fixture and end-to-end tests then run. Without them those
  tests are compiled out and the receiver honestly refuses AAC. The type-103
  integration test uses authenticated RTSP and the daemon transport seam on
  loopback TCP with real ALAC frames, and adds the AAC subtest only when the
  FFmpeg decoder is linked. Its deferred-flush proof holds an in-range frame in
  pacing, then requires exact pre-range and endpoint PCM and an abort on teardown.

## Upstream comparison

Compared against Shairport Sync 5.6.2 (commit `0a34fbda83724cc06266aadcf7c88acc8fef4e7d`)
as packaged by [capsula-mini-airplay2](https://github.com/ryysel/capsula-mini-airplay2) with
NQPTP 1.2.9.

| Upstream behaviour | Upstream evidence | This receiver |
|---|---|---|
| AirPlay 2 HomeKit pairing, transient and normal | `pair_ap/pair.c` (`pair_client_homekit_normal`, `pair_client_homekit_transient`), `rtsp.c` `handle_pair_setup` | Both messages are implemented; the receiver uses the transient path, like upstream's own receiver (no PIN is configured). |
| Pair-setup response MIME | `rtsp.c` `handle_pair_setup` adds `Content-Type: application/octet-stream` when the body is non-empty | All TLV `/pair-setup` replies (M2/M4, and M6 on the normal path) send `application/octet-stream`; `/pair-verify` and the pairing-administration endpoints keep `application/pairing+tlv8`. A real Apple receiver uses a different MIME for setup, so this is a reference-matching candidate; the iPhone causality is still open. |
| `/pair-pin-start` returns an empty 200 | `rtsp.c` `handle_pair_pin_start` | Returns 403 with a reason. No sender flow uses it after transient pairing. |
| `/pair-add`, `/pair-remove`, `/pair-list`, `/pair-pin-start` | `rtsp.c` handlers + `pair_ap/pair_homekit.c` `server_add_remove`/`server_list`; in-memory `struct pairings` with no permissions; `pair-pin-start` empty 200 | Same wire shapes and TLV states; **stronger** ownership: masked-state persistence via CAS, per-controller admin permission, administrator-only access on the encrypted session, `kTLVError_Unknown` for an unknown removal, and self-removal ending the session. |
| `/fp-setup` fixed replies | `rtsp.c` `handle_fp_setup` (four constants plus a header echo) | Same replies and selection rule, reproduced with the MIT notice in `NOTICE`; feature bit 14 (Authentication4) is in the default mask, as it is in the reference's default mask, and claims this handshake only. |
| AAC-LC buffered formats `44.1K/F24/2` and `48K/F24/2` | `rtsp.c` `bufferStreamFormats`, `player.h` SSRC map | Implemented, advertised with feature bit 40 and the matching `/info` bits on FFmpeg builds, and exercised through the encrypted TCP type-103 path. |
| AAC-ELD | not advertised by upstream (RTSP format list is AAC-LC only) | Not advertised; the FFmpeg decoder would accept it. |
| ALAC buffered 48 kHz/24-bit stereo | `player.h` `ALAC_48000_S24_2`; `rtsp.c` lossless `bufferStreamFormats` | Implemented in Go and advertised; the reference's `ALAC_44100_S16_2` buffered SSRC is marked synthetic and is not claimed. |
| Legacy RAOP: SDP, RSA-OAEP key, AES-128-CBC, UDP/interleaved TCP | `rtsp.c` `handle_announce`, `rtp.c` | Implemented; RSA-OAEP/SHA-1 matches `common.c` `rsa_apply`. |
| RTSP method set (`OPTIONS`, `ANNOUNCE`, `SETUP`, `RECORD`, `SET_PARAMETER`, `GET_PARAMETER`, `FLUSH`, `FLUSHBUFFERED`, `TEARDOWN`, `SETRATEANCHORTI`, `SETRATEANCHORTIME`, `SETMAGICCOOKIE`, `SETPEERS(X)`, `/feedback`, `/command`, `/audioMode`, `/configure`) | `rtsp.c` dispatch table (the AP2 anchor handler literal is `SETRATEANCHORTI`; an iPhone field client sent the full spelling `SETRATEANCHORTIME`) | Both anchor spellings are advertised in `OPTIONS Public`, route to one handler and carry the same plist body; the other methods have the same semantics, and unknown paths/methods get an explicit refusal. |
| PTP timing through NQPTP (ports 319/320, multicast 224.0.0.1) | `AIRPLAY2.md` ("NQPTP must have exclusive access to ports 319 and 320") | Bounded per-source slave fits behind one daemon-mediated multicast listener (`join_group`, group fixed to 224.0.0.1, LAN interfaces only); buffered anchors select by timeline ID. Lock quality is untested (no sender). |
| Metadata: now-playing command plists on the encrypted control channel, DMAP text, `progress:`, cover art, remote control over the event channel | `metadata/metadata_hub_handle_command_plist.c` (`updateMRNowPlayingInfo` / `npi-text` / `mergePolicy`, `kMRMediaRemoteNowPlayingInfo*`, `updateMRPlaybackState`), `rtsp.c` `handle_command` (AP2 only, dispatched from the control connection's `handle_post`), `metadata/hub.c`, `remote/remote.c`, `ap2_event_receiver.c` (write-only for receiver→sender commands) | The AirPlay 2 command record is read at the same nesting, with `replace`/`update` merge semantics, title/artist/album, duration/elapsed time and `ArtworkData`; PlaybackRate is informational and does not move the transport. The legacy DMAP and `progress:` layouts remain. Remote control uses command numbers 0/1/4/5 and the `params` dictionary. |
| Group advertisement `gid`/`gcgl` | `bonjour_strings.c`, `rtsp.c` SETUP | Implemented; no cross-receiver sync, exactly like upstream. |
| No FairPlay or MFi content decryption | `handle_fp_setup` only; `RELEASENOTES.md` MFi note | Same: the handshake completes, DRM content is not decrypted, MFi is not claimed. |
| MP3/AAC/Opus decoders | upstream's FFmpeg build enables AAC and ALAC only | ALAC (Go) and AAC (FFmpeg) are implemented; Opus is refused; MP3 is not an AirPlay audio codec. |

## Transport and lifecycle details

- **Inbound sockets** are all daemon-allocated: one RTSP listener, one UDP AP2 control socket per
  session, one UDP data socket per realtime stream (or one TCP data socket for a buffered
  stream), the event-channel listener, and the PTP multicast receiver. A session may hold at most
  four audio streams; a fifth `SETUP` is refused with `453`.
- **Actions** from the Web UI are live: *End the current session* disconnects the sender,
  *Re-advertise the receiver* withdraws and republishes the mDNS records, and each reports its
  result back to the framework.
- **Restarts** withdraw the old advertisement before publishing the new one; if the new
  configuration cannot start, the previous one is restored, so a rename cannot make the speaker
  vanish and a bad setting cannot leave the receiver dead.
- **Pair-verify authentication.** The control channel is keyed only after M3's controller
  signature verifies. A rejected M3 is `kTLVError_Authentication` inside an ordinary `200 OK`, so
  the connection must not infer success from the RTSP status: it stays plaintext, keeps no
  controller identity, and cannot become or take over the live session.
- **Listener bookkeeping** drops each closed listener from the runtime's registry, so repeated
  settings restarts do not accumulate descriptors.
- **Session ownership.** A TCP accept creates a candidate, not a session. A completed
  pair-setup or pair-verify protects and keys that connection, but it is still not a media
  session: it does not reserve ownership, start the player or board, set the active status flag,
  withdraw the advertisements, or take over a live sender. Only an accepted media/session start
  promotes the connection: an `ANNOUNCE`, or a `SETUP` that stages a real audio stream (AP2 type
  96/103 or a legacy `Transport` SETUP). The event-only and type-130 control-only `SETUP` forms
  configure channels and are acknowledged without media ownership, as in the reference receiver.
  `OPTIONS`, `GET /info`, `/pair-setup`, `/pair-verify`, pairing administration, a malformed or
  refused request, and a silent connection cannot take ownership, start the player or republish
  the advertisements — which is what keeps the losing half of a sender's dual-stack/Happy
  Eyeballs connection (immediately closed with no bytes) from killing the winning session.
  Candidates are capped at four and expire after 30 seconds without a session request; an
  encrypted candidate is bounded by the same policy. Shutdown cancels and joins them. A request
  that will start a session reserves ownership before its success response is written and
  commits only after the full response and any channel upgrade complete; a short write releases
  the reservation and closes the candidate without a committed `200`. The reservation is bounded
  at five seconds so a stalled peer cannot block later senders. A reserved candidate cannot be
  evicted or expire mid-write, and the live sender keeps playing until the commit.
- **Candidate visibility.** A connection that sends at least one request but never starts a
  session leaves at most one log line per five seconds across the receiver: the request count,
  an allowlisted last method and path class (`other` for anything else — the sender's path is
  never echoed), the last response status (including the explicit `503` ownership refusal),
  whether the last response write failed (including a bounded `503` refusal that timed out),
  and the elapsed time. For an encrypted candidate the same line adds the post-upgrade request
  count, the first and last allowlisted post-upgrade stage/status, the fixed HAP read outcome and
  decrypted block count, and a fixed RTSP parse class kept distinct from HAP failures. Headers,
  bodies, query strings, raw errors and payloads are never logged. A connection that sends
  nothing — a dual-stack loser or a port probe — stays silent, and a session that was promoted
  and later taken over is reported only by its own close line, never as a candidate.
- **Response framing.** Every RTSP response states `Content-Length`, including `0` for an empty
  body, and a supplied length is normalised to the body actually written. That matches the
  reference wire (Apple receivers send `Content-Length: 0`; shairport-sync always writes the
  header), so consecutive replies stay unambiguously framed for a sender that pipelines
  requests.
- **SETUP transaction.** A successful `SETUP` validates every requested descriptor before
  opening the AP2 control socket or any audio listener. A malformed or unsupported stream, a
  buffered stream on a plaintext channel, or a `shk` whose length is not 32 bytes is refused with
  no listener or session side effect. Listeners open only after validation and the streams are
  staged; no reader or playback loop runs and no sink opens until the session owns the receiver
  and the success response has been written. A multi-stream `SETUP` that fails while opening a
  listener discards every stream it staged and leaves the live sender, its gid, its advertisement
  and its audio unchanged. A type-103 stream buffers after its reader starts but makes no sound
  until a valid `SETRATEANCHORTI` or `SETRATEANCHORTIME` anchor; the descriptor names the real
  8 MiB queue and omits `streamID`. The `gid` is staged with the candidate and applied on commit; an active sender's
  successful `SETUP` applies it after the response, while a `SETUP` that omits `gid` preserves the
  same owner's current group. On a committed takeover the new player generation starts before
  the old sender's connection, streams, ring and event channel are released.
- **Buffered control.** The AP2 anchor method is `SETRATEANCHORTI` in Shairport Sync's
  dispatch table, while an iPhone field client sent `SETRATEANCHORTIME`; both observed
  spellings route to one handler and carry the same plist body, with `networkTimeSecs`,
  `networkTimeFrac` (a binary fraction), `networkTimeTimelineID`, `rtpTime` and `rate`.
  Rate 1 requires a complete anchor for a buffered stream; rate 0 pauses and flushes.
  `FLUSHBUFFERED` without `flushFromSeq`/`flushFromTS` is immediate through its
  `flushUntilSeq`/`flushUntilTS` endpoint; omitting the until pair means the whole queue. With
  that from pair it is deferred to the named range, which requires both until fields. Immediate
  discards every queued sequence below the endpoint; deferred discards `[from, until)`. Both use
  the reference's 23-bit sequence selection; timestamps travel on the wire but do not override it.
  A present but malformed or out-of-range field, or a partial pair, is refused before playout
  changes. An immediate `FLUSHBUFFERED` retires every affected type-103 playout generation
  before it publishes the pause, so a stale write completion cannot turn PAUSED into PLAYING
  and back. `TEARDOWN`, a
  control disconnect, an invalid takeover and shutdown stop the buffered reader and abort its
  sink before releasing the ring capture, then release the player. A stalled framework capture
  release therefore cannot keep the old sender's queued audio draining.
- **Advertisement ordering.** Session start/end, a group-id update and the *Re-advertise*
  action all publish through one serialized path. A second request waits, then withdraws and
  replaces the first record instead of racing the same instance, and shutdown waits for an
  in-flight publish before withdrawing, so a late publish cannot leave a live record after stop.
  The session-active flag is read from the current lifecycle state when the record is built, not
  captured by the asynchronous caller, so a delayed session-end request cannot replace a newer
  active record with `sf=inactive`.
- **Advertisement records.** `_airplay._tcp` and `_raop._tcp` both carry the 32-byte ed25519
  HomeKit public key as `pk` (64 hex characters), which is what the reference receiver and every
  AirPlay 2 device on the link publish. The legacy 2048-bit RSA key is not advertised: its base64
  encoding is about 360 bytes, and Avahi rejects a DNS TXT item over 255 bytes with
  `InvalidRecordError`, so that record could never be registered. The daemon validates the
  255-byte per-item limit before calling Avahi. A saved name longer than the 63-byte DNS-SD
  instance label is projected on a UTF-8 boundary for the label — and, after the 12-hex device id
  and `@`, for the `_raop` instance — while the setting and `/info` keep the full value.
- **One capability source.** Both records and `/info` take `features` (and `/info`'s
  `featuresEx`) from one mask, and `flags` / `sf` / `statusFlags` from one status value: bit 2
  (audio receiver attached) plus bit 17 while a sender session is live. `/info` reports
  `supportedFormats.audioStream=262144` (ALAC/44.1/S16/2). With buffered bit 40 set it reports
  `bufferStream=0x200000` for ALAC 48/24 stereo and, only on an FFmpeg build, additionally
  `0x400000|0x800000` for the AAC-LC 44.1/48 stereo formats. Clearing either codec bit removes
  its formats; clearing bit 40 or both codec bits reports `bufferStream=0`. Feature overrides are
  sanitized against the linked decoder, so a build cannot advertise AAC it lacks or a buffered
  path with no playable format. The default mask also sets bit 14 (Authentication4), whose claim
  is only the implemented `/fp-setup` handshake, not DRM content or MFi; an override that omits
  that bit is honored. The reference's synthetic buffered ALAC 44.1 value is not advertised.

## Configuration

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | text | `AirPlay Receiver` | Advertised receiver name (≤63 UTF-8 bytes). A longer saved value is projected on a UTF-8 boundary for the mDNS label and for the `_raop` display part; the setting and `/info` keep it in full. Changing it re-advertises and ends a live session. |
| `receiver_model` | text | `AudioAccessory5,1` | AirPlay model string. `AudioAccessory5,1` is what the upstream receiver patches in so senders draw the compact speaker icon; it is a compatibility string, not a hardware claim. |
| `rtsp_port` | integer | `7000` | Requested control port; the daemon allocates it and a refusal falls back to an ephemeral port that is then advertised. |
| `buffer_ms` | integer | `1500` | Playout cushion in milliseconds. |
| `ring_artwork` | boolean | `true` | Cover-art gradient on the ring while a session is live. |
| `source_version` | text | `366.0` | Advertised `srcvers`. |
| `features` | text | empty | Decimal override of the advertised feature mask; empty uses the receiver's default set, including bit 14 (the `/fp-setup` handshake) and buffered bit 40 where a codec is available. An override replaces the whole mask, so omitting bit 14 stops the handshake claim. The receiver clears AAC-LC and buffered bits it cannot back with an available decoder/format, so an override cannot make TXT, `featuresEx` or `/info` promise an unsupported path. |
| `ptp` | boolean | `true` | Use the sender's PTP clock when the runtime grants a multicast listener. Buffered playout selects the fit by the anchor's timeline ID; without a matching disciplined source it uses local arrival timing. |
| `pairing_pin` | text | `3939` | HomeKit setup code answered for transient and normal pair-setup. Changing it makes every sender pair again; empty restores `3939`. |
| `device_identity`, `pairings`, `raop_key` | masked | empty | App-managed generated state: the HomeKit identity (`device_identity`), each paired sender's public key **and admin permission** (`pairings`, compare-and-swap), and the legacy RSA key used for an ANNOUNCE `rsaaeskey` (not advertised in mDNS). Clearing `pairings` revokes every paired sender. |

## Lifecycle and feedback

- **Trigger.** A sender connects to the advertised control port and completes
  pairing and `SETUP`/`RECORD`. A second sender takes the session over.
- **Feedback.** The player appears through the media contract (buffering, then
  playing); the ring shows the cover palette with the audio envelope, or a dim
  neutral breath without cover art. No cover art is ever invented.
- **Ownership.** The ring presentation, the routing capture that selects it and
  the playback stream are owned by the session and released on teardown,
  connection loss, takeover, idle timeout, app stop or receiver settings change.
  Releasing the capture returns the ring to the device's selected baseline; the
  presentation is closed after it.
- **Interruption.** A pause (`rate 0`) flushes queued audio and reports
  `PLAYBACK_PAUSED`; a resume re-anchors playout. Panel gestures and the Web UI
  reach the sender through the event channel; they are never a private
  daemon→sender path.
- **Dismissal.** `TEARDOWN`, a closed control connection, ~45 s without traffic
  (~3 min while audio was flowing) or the "End the current session" action ends
  the session. The receiver stays advertised.
- **Final state.** The media player disappears from `ListPlayers`, the ring
  returns to the device's selected baseline, and the playback stream is aborted
  (not drained) so a stopped session makes no late sound.

### Now-playing metadata

- **Trigger.** A paired AirPlay 2 sender posts `updateMRNowPlayingInfo` command
  plists as `POST /command` on the **main encrypted RTSP control channel**, in
  the shape `type=updateMRNowPlayingInfo`, `params.type=npi-text`,
  `params.params.kMRMediaRemoteNowPlayingInfo…`. Title, artist, album, duration,
  elapsed time and the embedded `kMRMediaRemoteNowPlayingInfoArtworkData` image
  come from that record; its playback rate is carried but treated as
  informational (see Interruption and merging). `updateMRPlaybackState`
  (`mrPlaybackState` 1/2/3) reports playing, paused and stopped. A plist
  `SET_PARAMETER` and the legacy RAOP DMAP/`progress:` path feed the same session
  state, so a sender can use whichever flow it negotiates.
  Inbound commands on the event channel are accepted too; that is a tolerance
  for the receiver's bidirectional channel, not the Apple Music wire path.
- **Feedback.** Every command is answered with `200` on the channel it arrived
  on. The receiver publishes the merged title/artist/album, duration and
  position through the media contract (`ListPlayers`/`WatchPlayers`), which the
  Web UI's Now Playing card displays, and shows the artwork palette on
  `light.ring.main` with brightness following the outgoing PCM envelope. The
  bookkeeping is session state, not a new framework interface.
- **Ownership.** A candidate's commands are stored on its `Session` and never
  reach the live board (the board generation is zero) or another sender's ring;
  promotion replays the text, timeline (including a record that carries only
  duration and elapsed time), artwork and last playback state into the
  generation the new owner starts. The replay and live updates share one
  publication order, so a slow artwork decode cannot repaint an older record
  over a newer one. A command that arrives after a takeover carries the retired
  generation and is dropped by the board's generation check.
- **Interruption and merging.** `mergePolicy=update` changes only the fields the
  record carries, so a title/position update keeps the current artist, album and
  cover. `mergePolicy=replace` clears the previous record first: fields the new
  record omits disappear, and a cover the new record does not carry (or whose
  image cannot be decoded) clears the palette instead of leaving the last
  track's colours under a new title. A nested record is accepted only when
  `params.type` is exactly `npi-text`; a malformed envelope is ignored rather
  than read as a replace that would clear the live record. The older flat
  record form keeps its separate keys-only rule. The record's `PlaybackRate` is
  informational: it never pauses or resumes audio and never discards queued
  sound, because the transport follows `SETRATEANCHORTI`/`SETRATEANCHORTIME`,
  `PAUSE` and `RECORD`. `updateMRPlaybackState` is display feedback, not a
  transport command: a type-103 player stays buffering until decoded PCM
  reaches the sink, and an explicit paused or stopped report outranks
  PCM-derived playing, so an in-flight packet cannot repaint it. A pause,
  immediate flush or reconnected data channel retires the current PCM
  generation; an explicit playing report after that boundary stays buffering
  until fresh PCM arrives, and a late completion from the retired generation
  cannot repaint it either. PCM evidence is per type-103 stream: opening or
  pausing one stream does not erase another stream's current evidence. The
  first cover on a fresh
  session is preceded by the documented neutral breath; no artwork is invented.
- **Cleanup.** `TEARDOWN`, connection loss, takeover, idle timeout and app stop
  release the media player, the ring presentation and its routing capture, and
  the session's playback state. The elapsed position is shown as the sender
  reported it; the receiver does not extrapolate it from the record's timestamp.

## Interface

This app provides `capsulad.contracts.media.v1.MediaPlayerService` (descriptor
`proto/media.protoset`, documentation `docs/media.md`). It consumes
`light.ring.main` (presentation and routing capture) and `audio.output.speaker`
(playback), and it
publishes `_airplay._tcp` and `_raop._tcp` through the daemon's responder. Both records carry the
same ed25519 HomeKit `pk`; the daemon rejects a TXT item over 255 bytes, which is why the legacy
RSA key is not in either record.

## Build, test, package

```sh
make native                 # the shared runtime the app links against
sh apps/airplay/build.sh    # host: stages a development build (PACKAGE=auto writes no archive)
sh apps/airplay/build.sh    # GOOS=linux: builds + packages <id>-<version>.tar.gz with the ABI check
make airplay                # same, through the repository target
cd apps/airplay && CGO_ENABLED=1 go test -tags ffmpeg ./...
```

Every package carries this guide as `README.md` at its root, next to `manifest.toml`, along with
`NOTICE` and the media contract's `docs/media.md`.

### Target ABI and the decoder

A native package is not portable across libc ABIs, and this app links a native decoder, so the
ABI is an explicit build input:

- `TARGET_ABI` is `linux-gnu` (default) or `linux-musl` and means the Linux target only. The
  build stages a manifest whose `abi` is exactly that value (the source manifest keeps the glibc
  default). `capsulad-cli app package` never rewrites `abi`; the staged manifest is the one that
  ships. `make deploy-bundle` refuses an archive whose manifest ABI does not match the platform,
  which trips a mislabelled package early instead of at install.
- **The FFmpeg libraries must be target-built.** A cross build requires
  `AIRPLAY_FFMPEG_PREFIX` (a prefix with `lib/pkgconfig/libavcodec.pc`) or `PKG_CONFIG_LIBDIR`
  pointing at the target sysroot; without one the build fails rather than silently linking the
  host's libavcodec. Relabelling a glibc-linked decoder as musl is never accepted.
- `CC` falls back to `aarch64-linux-gnu-gcc` / `aarch64-linux-musl-gcc` only when that compiler
  exists; a Linux build never falls back to the host compiler.
- The build log names the target ABI and the FFmpeg prefix, so an artifact's manifest can be
  traced to the toolchain that produced it.

```sh
# glibc device package, with the target FFmpeg built for aarch64 glibc
AIRPLAY_FFMPEG_PREFIX=/opt/aarch64-glibc/ffmpeg \
  GOOS=linux GOARCH=arm64 TARGET_ABI=linux-gnu sh apps/airplay/build.sh

# Alpine (musl) device package, with FFmpeg built for aarch64 musl
AIRPLAY_FFMPEG_PREFIX=/opt/aarch64-musl/ffmpeg \
  GOOS=linux GOARCH=arm64 TARGET_ABI=linux-musl sh apps/airplay/build.sh
```

#### Reproducible device packages (`apps/airplay/target/`)

`build.sh` needs three things a workstation may not have: a target FFmpeg prefix, a target C
compiler and a host-executable packager. `apps/airplay/target/` supplies all three inside
pinned containers, so a device package rebuilds on any machine with Docker — no cross
toolchain, no host FFmpeg, no host native runtime:

```sh
# both libcs into an artifact directory
OUT=/tmp/airplay-target sh apps/airplay/target/build-target.sh

# one libc
OUT=/tmp/airplay-target TARGET_ABI=linux-musl sh apps/airplay/target/build-target.sh

# through make: OUT defaults to build/apps/target
make airplay-target
make airplay-target AIRPLAY_TARGET_ABI=linux-gnu AIRPLAY_TARGET_OUT=/tmp/airplay-target
```

`build/` is generated output, not source; `AIRPLAY_TARGET_OUT` (or `OUT`) can point anywhere.

The recipe copies the working tree to a temporary directory, mounts that copy at `/work`, and
writes only under `OUT`:

```
<OUT>/<abi>/org.capsulad.airplay-<version>.tar.gz   the package (manifest abi = <abi>)
<OUT>/<abi>/airplay                                 the same binary, for ELF inspection
<OUT>/<abi>/evidence.txt                            the full build + ELF + smoke transcript
<OUT>/<abi>/SHA256SUMS
<OUT>/lgpl/<abi>/                                   LGPL relink material (see NOTICE §2)
```

Nothing is built in the repository and no source file is modified. `KEEP=1` keeps the
temporary tree, `CACHE` overrides the download cache
(`${XDG_CACHE_HOME:-$HOME/.cache}/capsulad-airplay-target`).

What is pinned, and why (all in `apps/airplay/target/pins.sh`):

| input | pin | why this value |
|---|---|---|
| Alpine base | `alpine:3.21@sha256:bc214871…` | musl 1.2.5 toolchain, pinned by digest because a tag moves |
| Debian base | `debian:buster@sha256:58ce6f12…` | buster == glibc 2.28, the stock device's libc; a bullseye build required `exp/exp2/log/pow@GLIBC_2.29` from libm and would not load there |
| apk transport | `mirrors.edge.kernel.org` | the default Alpinelinux CDN delivers ~120–180 kB/s through the build proxy and stalls `apk` on the multi-megabyte packages; the same aarch64 package set comes from this mirror at ~1.2 MB/s |
| apt transport | `snapshot.debian.org/…/20240601T000000Z` | `deb.debian.org` is inconsistent behind the proxy (index entries that 404) and buster has left the live archive; a dated snapshot is immutable, which is what a pinned build needs |
| Go toolchain | `golang.org/toolchain@v0.0.1-go1.22.6.linux-arm64`, sha256 `6fd75a52…` | the official linux/arm64 build, verified against sum.golang.org; `go.dev/dl` redirects to `dl.google.com`, which the proxy answers with 404 |
| FFmpeg | `ffmpeg-7.1.tar.xz`, sha256 `40973d44…` | release tarball is the source of record (its own `VERSION`/`RELEASE` read 7.1) |

The module graph is downloaded while the image builds (`go.sum` pins every module), so a
package build needs no network. FFmpeg's `-latomic` is repointed at gcc's static archive, so
neither package depends on `libatomic.so.1` being present on the device.

The minimal FFmpeg configure line lives in `target/container/ffmpeg.sh` and is printed into
every artifact's `evidence.txt`:

```
--prefix=/opt/ffmpeg --enable-static --disable-shared --disable-programs --disable-doc
--disable-network --disable-everything --enable-decoder=aac --enable-parser=aac
--disable-zlib --disable-bzlib --disable-lzma --disable-iconv --disable-xlib --disable-sdl2
--disable-avdevice --disable-swscale --disable-swresample --disable-postproc --disable-avfilter
--disable-avformat --disable-autodetect --enable-pic --cc=gcc
```

The resulting `libavcodec.a`/`libavutil.a` are linked statically, which is the LGPL branch
with a distribution precondition: `target/build-target.sh` writes the relink material
(`objects.tar.gz`, the linked archives, the FFmpeg source tarball, `RELINK.md`) beside each
package under `<OUT>/lgpl/<abi>/`. NOTICE §2 carries the obligation and the terms.

**Why the musl package is dynamically linked.** A fully static musl Go+cgo binary cannot
`dlopen` at all: musl's `libc.a` stubs it out ("Dynamic loading not supported"), with or
without `-Wl,-E`, while the dynamic build loads a shared object normally. This app must
`dlopen` the launcher-injected `libcapsulad.so.1`, so `TARGET_ABI=linux-musl` with
`CGO_LDFLAGS=-static` is refused by `build.sh`, and `tools/check-elf-interp.sh …
require-dynamic` enforces the same rule for every native-SDK app. The musl package's only
dynamic dependency is musl's own `libc.musl-aarch64.so.1`.

An optional load smoke uses a real runtime when one is supplied: drop a target
`libcapsulad.so.1` at `<OUT>/runtime/<abi>/libcapsulad.so.1` (or `<OUT>/runtime/libcapsulad.so.1`;
the recipe detects its libc and only uses a matching one, so a musl library is never handed to
a glibc package). Without one, `evidence.txt` records the SDK's missing-library message and
says so explicitly.

The tests are pure Go except for the FFmpeg decoder and the loopback transport
harness: they cover the pairing crypto, the RTSP/DMAP/plist/TLV codecs, ALAC
against fixtures produced by a real encoder (ffmpeg), RTP encryption and
reordering, the RAOP SDP/key exchange, the PTP clock, and end-to-end sessions
over in-memory and real loopback TCP (pair-verify → encrypted control channel →
`SETUP` → realtime or type-103 audio → now-playing command plists with two
different cover images → media state → `TEARDOWN`). The now-playing regression
uses the sender's nested `updateMRNowPlayingInfo`/`npi-text` plists, asserts the
`replace`/`update` merge rules, the artwork palette on the ring, playback-state
feedback and the candidate-to-owner replay; it runs under `-race` with the other
session lifecycle tests. They do not touch the speaker, an Apple sender or the LAN.

## Interoperability gate

No live sender stream has been captured. A field check on the RK3308 unit
observed the receiver in a Mac's mDNS browser (`_airplay._tcp` with a 64-hex
`pk`), confirmed that `_raop._tcp` was missing because Avahi rejected the
oversized RSA TXT item, and received `RTSP/1.0 200 OK` for `OPTIONS` on the
advertised port. The wire behaviour above is otherwise implemented from the
protocol facts and checked against fixtures and the reference receiver's packet
layouts — not against a live sender stream. What remains unproven, and must be
confirmed on a device:

1. that a current iOS release accepts this advertisement set and chooses a
   stream this build decodes (the `srcvers`/`features` settings exist to settle
   that empirically), including whether it selects type-103 buffered audio;
2. that the buffered anchor's timeline ID matches the sender's PTP source and
   that timing is stable without applying another source's clock;
3. acoustic latency and PTP lock quality on the board (the app is board-agnostic: it uses
   framework listeners, presentations and playback, so it runs on the Rockchip rev 4 and on both
   Amlogic targets);
4. behaviour of the sender's event channel for the four remote commands;
5. anything about grouped playback.

See the app's [limits](#not-implemented-and-what-it-means) and
[docs/VERIFICATION.md](../../docs/VERIFICATION.md) for the proof level.

## Licensing and notices

This repository has no project-wide licence (there is no root `LICENSE` or
`COPYING`). The full notices ship as [`NOTICE`](NOTICE), staged next to
the binary by `apps/airplay/build.sh`:

- The four `/fp-setup` reply constants reproduced in
  `internal/session/fairplay.go` come from Shairport Sync's `rtsp.c`
  (`handle_fp_setup`). Shairport Sync's files — `rtsp.c` and `pair_ap/` — are
  MIT/X11, **not** GPL, and `NOTICE` carries the verbatim permission notice and
  copyright lines. NQPTP, which upstream uses for PTP, is GPLv2, and nothing
  GPL is linked or copied into this app: `internal/ptp` takes field offsets and
  constants only.
- AAC decoding links FFmpeg's `libavcodec`/`libavutil` (LGPL-2.1-or-later),
  built with `--disable-everything --enable-decoder=aac --enable-parser=aac`
  plus whatever the target build adds. `NOTICE` states the LGPL relink and
  source obligations, including a static build's duty to ship relinkable object
  files and the corresponding source offer.
