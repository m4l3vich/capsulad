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
`light.ring.main` presentation, and the daemon's Avahi responder publishes the
mDNS records. No HTTP, TLS or gRPC stack is bundled; the AirPlay protocol, its
pairing crypto and its codecs are implemented in this package on top of the Go
standard library and `golang.org/x/crypto`.

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
| AAC-LC decoding | Implemented (FFmpeg libavcodec) | The same decoder the upstream receiver links. The advertised formats are exactly upstream's: `AAC-LC/44.1K/F24/2` and `AAC-LC/48K/F24/2`. The AudioSpecificConfig is built from the format mask, and the decoder is verified against ffmpeg-encoded fixtures (sample-for-sample, mono bit-exact, stereo within 1 LSB of the encoder's own quantisation). |
| AAC-ELD decoding | Decoder available, not advertised | FFmpeg's AAC decoder handles ELD, so the eight ELD format masks are accepted if a sender sends one. Upstream does **not** advertise any ELD format (its RTSP format list is AAC-LC only), and neither does this receiver, so no parity claim is made beyond the decoder. |
| FairPlay handshake | Implemented, no DRM | `POST /fp-setup` serves the same fixed protocol replies as upstream (Shairport Sync `handle_fp_setup`), so a sender's handshake completes. Those constants decrypt nothing: FairPlay-encrypted content is not playable by either implementation. |
| AirPlay 2 buffered stream (type 103) | Accepted for ALAC/PCM | TCP data channel with the 2-byte big-endian length prefix that includes its own two bytes. The advertised feature set does not claim buffered audio, so a sender only chooses it when its own policy does. |
| Legacy RAOP (ANNOUNCE + SETUP + RTP) | Implemented, without the timing exchange | SDP parsing, `a=rsaaeskey` decrypted with the advertised RSA key (RSA-OAEP/SHA-1, the reference's mode), AES-128-CBC audio, UDP transport with receiver-chosen data **and control** ports (the control socket is drained; retransmission recovery is not implemented because no packet cache is kept), or interleaved TCP RTP (`$` frames on the control connection). The RAOP NTP-style timing exchange is not implemented and the SETUP reply advertises `timing_port=0`, so a legacy sender gets no receiver clock service. |
| Metadata | Implemented | DMAP text (title/artist/album/composer/genre), `progress:` position, cover art (JPEG/PNG) as a ring palette. |
| Remote control | Implemented | The sender's event channel is bidirectional: the receiver posts `sendMediaRemoteCommand` (play 0, pause 1, next 4, previous 5) with the standard `params` dictionary. |
| Group membership advertisement | Implemented to upstream's level | A `gid` sent with `SETUP` is recorded and republished as the mDNS `gid` with `gcgl=0`, which is what upstream does. Neither implementation synchronises playback across a group, so multi-room audio is *not* claimed. |
| Volume | Implemented app-side | The sender's dB value scales this app's PCM. A supervised app cannot write the device master volume, so the mixer's master and limiter remain the ceiling. |
| Timed playout (PTP) | Implemented when the runtime grants it | A PTP slave clock (`internal/ptp`) consumes the sender's Sync/Follow_Up stream through a daemon-allocated multicast listener; without it the receiver paces from the first packet's arrival. |

## Not implemented (and what it means)

| Gap | Consequence | Evidence / reason |
|---|---|---|
| AAC decoding in a build without the FFmpeg tag | Such a build refuses AAC at `SETUP` with an explicit reason and does not advertise AAC. The shipped target (`make airplay`) always builds with the decoder and fails early if the libraries are missing. | The decoder is a build dependency, not an optional extra: see [FFmpeg](#ffmpeg-build-dependency). |
| AAC-ELD advertisement | Accepted but not advertised, matching upstream. | Upstream's RTSP response lists AAC-LC formats only (`rtsp.c` `bufferStreamFormats`); FFmpeg could decode ELD but nothing asks for it. |
| FairPlay-protected content | Not playable; only the handshake is answered. | Neither this receiver nor Shairport Sync implements the proprietary FairPlay cipher. |
| MFi | Not attempted. | Requires Apple hardware; upstream's mDNS strings deliberately avoid claiming MFi (`RELEASENOTES.md`, discussion 1691). |
| Pairing administration differences from upstream | Our model is deliberately stronger: pairings survive a restart, each has a stored permission, and the endpoints require an administrator session. | Upstream keeps pairings in an in-memory linked list with **no permissions** (`rtsp.c` `struct pairings`), answers `pair-list` with a hard-coded `Permissions = 1` and performs no session/authority check (`server_list` ignores the request), and a `pair-remove` of an unknown id still returns success (`server_add_remove_request` ignores the callback result). |
| PTP on a runtime without a multicast listener | The receiver advertises no PTP bit, logs the condition once, and falls back to arrival-time pacing. Multi-room AirPlay sync is then not guaranteed. | PTP needs the sender's 224.0.0.1:319/320 multicast stream. A listener that cannot join the group cannot receive it; the app never pretends it does. |
| Grouped/multi-room playback | One sender at a time; a second connection takes the session over. Group membership is advertised but playback is not synchronised. | Upstream is also a single principal session and only advertises `gid`/`gcgl`. |
| AirPlay video / screen mirroring | Not offered; the audio-only feature bits are advertised. | Out of scope for an audio receiver. |
| Type-130 remote-control stream (`SETUP`) | Refused with an explicit status. | Upstream binds it (`rtsp.c`); no sender flow in this record exercises it, so it is reported as a gap rather than guessed at. |
| Device volume pushed to the sender | Not implemented. The sender's dB scales this app's PCM; the device's own volume keys are not forwarded back to the sender, because a supervised app cannot read or write the mixer's master volume through the framework's app API. | Upstream writes the vendor volume via `stereoctl` and posts a `dvlc` command on the event channel; the equivalent framework surface does not exist for apps. |
| Exact `srcvers`/feature combination a given iOS release requires | Open device gate. | Senders switch behaviour on `srcvers` and on feature bits (remote control above 360, realtime at or below 355, `shk` above 350, buffered at bit 40, PTP at bit 41). Both fields are settings so a device test can pin them; see [README](README.md#interoperability-gate) below. |


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
  tests are compiled out and the receiver honestly refuses AAC.

## Upstream comparison

Compared against Shairport Sync 5.6.2 (commit `0a34fbda83724cc06266aadcf7c88acc8fef4e7d`)
as packaged by [capsula-mini-airplay2](https://github.com/ryysel/capsula-mini-airplay2) with
NQPTP 1.2.9.

| Upstream behaviour | Upstream evidence | This receiver |
|---|---|---|
| AirPlay 2 HomeKit pairing, transient and normal | `pair_ap/pair.c` (`pair_client_homekit_normal`, `pair_client_homekit_transient`), `rtsp.c` `handle_pair_setup` | Both messages are implemented; the receiver uses the transient path, like upstream's own receiver (no PIN is configured). |
| `/pair-pin-start` returns an empty 200 | `rtsp.c` `handle_pair_pin_start` | Returns 403 with a reason. No sender flow uses it after transient pairing. |
| `/pair-add`, `/pair-remove`, `/pair-list`, `/pair-pin-start` | `rtsp.c` handlers + `pair_ap/pair_homekit.c` `server_add_remove`/`server_list`; in-memory `struct pairings` with no permissions; `pair-pin-start` empty 200 | Same wire shapes and TLV states; **stronger** ownership: masked-state persistence via CAS, per-controller admin permission, administrator-only access on the encrypted session, `kTLVError_Unknown` for an unknown removal, and self-removal ending the session. |
| `/fp-setup` fixed replies | `rtsp.c` `handle_fp_setup` (four constants plus a header echo) | Same replies and selection rule, reproduced with the MIT notice in `NOTICE`. |
| AAC-LC buffered formats `44.1K/F24/2` and `48K/F24/2` | `rtsp.c` `bufferStreamFormats` | Implemented, advertised (feature bit 20, RAOP `cn=0,1,2`) and covered by an end-to-end encrypted-RTP test. |
| AAC-ELD | not advertised by upstream (RTSP format list is AAC-LC only) | Not advertised; the FFmpeg decoder would accept it. |
| ALAC realtime/buffered | `alac.c`, FFmpeg ALAC decoder | Implemented in Go and verified bit-exactly against ffmpeg fixtures. |
| Legacy RAOP: SDP, RSA-OAEP key, AES-128-CBC, UDP/interleaved TCP | `rtsp.c` `handle_announce`, `rtp.c` | Implemented; RSA-OAEP/SHA-1 matches `common.c` `rsa_apply`. |
| RTSP method set (`OPTIONS`, `ANNOUNCE`, `SETUP`, `RECORD`, `SET_PARAMETER`, `GET_PARAMETER`, `FLUSH`, `TEARDOWN`, `SETRATEANCHORTIME`, `SETMAGICCOOKIE`, `SETPEERS(X)`, `/feedback`, `/command`, `/audioMode`, `/configure`) | `rtsp.c` dispatch table | Implemented with the same semantics; unknown paths/methods get an explicit refusal. |
| PTP timing through NQPTP (ports 319/320, multicast 224.0.0.1) | `AIRPLAY2.md` ("NQPTP must have exclusive access to ports 319 and 320") | A slave clock with a daemon-mediated multicast listener (`join_group`, group fixed to 224.0.0.1, LAN interfaces only). Lock quality is untested (no sender). |
| Metadata: DMAP text, `progress:`, cover art, remote control over the event channel | `metadata/*`, `remote/remote.c`, `ap2_event_receiver.c` | Implemented to the same packet layouts (command numbers 0/1/4/5 and the `params` dictionary). |
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
- **Listener bookkeeping** drops each closed listener from the runtime's registry, so repeated
  settings restarts do not accumulate descriptors.

## Configuration

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | text | `AirPlay Receiver` | Advertised receiver name (≤63 UTF-8 bytes). Changing it re-advertises and ends a live session. |
| `receiver_model` | text | `AudioAccessory5,1` | AirPlay model string. `AudioAccessory5,1` is what the upstream receiver patches in so senders draw the compact speaker icon; it is a compatibility string, not a hardware claim. |
| `rtsp_port` | integer | `7000` | Requested control port; the daemon allocates it and a refusal falls back to an ephemeral port that is then advertised. |
| `buffer_ms` | integer | `1500` | Playout cushion in milliseconds. |
| `volume_trim` | number | `1.0` | Extra linear attenuation after the sender volume. |
| `ring_artwork` | boolean | `true` | Cover-art gradient on the ring while a session is live. |
| `source_version` | text | `366.0` | Advertised `srcvers`. |
| `features` | text | empty | Decimal override of the advertised feature mask. |
| `ptp` | boolean | `true` | Use the sender's PTP clock when the runtime grants a multicast listener. |
| `pairing_pin` | text | `3939` | HomeKit setup code answered for transient and normal pair-setup. Changing it makes every sender pair again; empty restores `3939`. |
| `device_identity`, `pairings`, `raop_key` | masked | empty | App-managed generated state: the HomeKit identity (`device_identity`), each paired sender's public key **and admin permission** (`pairings`, compare-and-swap), and the legacy RSA key. Clearing `pairings` revokes every paired sender. |

## Lifecycle and feedback

- **Trigger.** A sender connects to the advertised control port and completes
  pairing and `SETUP`/`RECORD`. A second sender takes the session over.
- **Feedback.** The player appears through the media contract (buffering, then
  playing); the ring shows the cover palette with the audio envelope, or a dim
  neutral breath without cover art. No cover art is ever invented.
- **Ownership.** The ring presentation and the playback stream are owned by the
  session and released on teardown, connection loss, takeover, idle timeout,
  app stop or receiver settings change.
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

## Interface

This app provides `capsulad.contracts.media.v1.MediaPlayerService` (descriptor
`proto/media.protoset`, documentation `docs/media.md`). It consumes
`light.ring.main` (presentation) and `audio.output.speaker` (playback), and it
publishes `_airplay._tcp` and `_raop._tcp` through the daemon's responder.

## Build, test, package

```sh
make native                 # the shared runtime the app links against
sh apps/airplay/build.sh    # host: stages a development build (PACKAGE=auto writes no archive)
sh apps/airplay/build.sh    # GOOS=linux: builds + packages <id>-<version>.tar.gz with the ABI check
make airplay                # same, through the repository target
cd apps/airplay && CGO_ENABLED=1 go test -tags ffmpeg ./...
```

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

The tests are pure Go: they cover the pairing crypto, the RTSP/DMAP/plist/TLV
codecs, ALAC against fixtures produced by a real encoder (ffmpeg), RTP
encryption and reordering, the RAOP SDP/key exchange, the PTP clock, and one
end-to-end session over in-memory transports (pair-verify → encrypted control
channel → `SETUP` → encrypted RTP audio → ring frames → media state →
`TEARDOWN`). They do not touch a device, a real sender or the network.

## Interoperability gate

No iPhone, iPad or Mac was available to this change, and no device access was
authorized. The wire behaviour above is implemented from the protocol facts and
checked against fixtures and against the reference receiver's packet layouts —
not against a live sender. What remains unproven, and must be confirmed on a
device:

1. that a current iOS release accepts this advertisement set and chooses a
   stream this build decodes (the `srcvers`/`features` settings exist to settle
   that empirically);
2. acoustic latency and PTP lock quality on the board (the app is board-agnostic: it uses
   framework listeners, presentations and playback, so it runs on the Rockchip rev 4 and on both
   Amlogic targets);
3. behaviour of the sender's event channel for the four remote commands;
4. anything about grouped playback.

See the app's [limits](#not-implemented-and-what-it-means) and
[docs/VERIFICATION.md](../../docs/VERIFICATION.md) for the proof level.

## Licensing and notices

This repository has no project-wide licence (there is no root `LICENSE` or
`COPYING`), and upstream attribution for the reference receiver
capsula-mini-airplay2 is pending the author's agreement, so nothing in this
package grants redistribution rights beyond the notices below. The full notices
ship as [`NOTICE`](NOTICE), staged next to the binary by `apps/airplay/build.sh`:

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
