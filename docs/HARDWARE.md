# Capsula Mini hardware (RK3308 stock image and Amlogic A113X)

Hardware, firmware and interface facts for the supported targets: the Capsula Mini rev 4
(serial family `03104B1D40E15C55`, jailbroken stock Rockchip image) and the VK Capsula Mini
on Amlogic A113X, in both its jailbroken stock Buildroot image and Alpine Linux. Every
measurement below states its proof level: probe result (kernel/device state), backend submission
(what the daemon wrote to a driver), digital output (a captured PCM/monitor stream), or
electrical/acoustic/optical measurement. Facts borrowed from a published third-party survey or
project are labelled as such; they are not measurements taken here.

## Platform

| Fact | Value |
|---|---|
| SoC | Rockchip RK3308, four Cortex-A35 cores at 1.008 GHz, aarch64 |
| Device tree | `RK3308-based Capsula-mini` |
| Kernel | Linux 4.4.167 SMP PREEMPT, Linaro GCC 6.3.1 |
| libc | glibc 2.33, `/lib/ld-linux-aarch64.so.1`; not musl. The native SDK and app binaries must not require newer `GLIBC_*` symbols |
| Root filesystem | Buildroot 2022.11.1 SquashFS 4.0 (xz), read-only; overlay (`S23overlayfs.sh`) with upper/work on the ext2 `userdata` partition |
| Boot image | Android boot image, page size 2048, kernel + ramdisk + DTB in `second` |
| RAM | 239,840 kB, no swap |
| Storage | `/rom` 33.6 M (squashfs, 100 % full), `/data` 78.5 M, `/tmp` tmpfs 117 M |

Partition layout of the stock image (`_capsula-mini-rockchip-backup/`) — start and size in KiB:
`uboot` 8192/8192, `trust` 16384/8192, `misc` 24576/2048, `boot_a` 26624/32976, `boot_b`
59600/32976, `system_a` 92576/88064, `system_b` 180640/88064, `vbmeta_a` 268704/2048, `vbmeta_b`
270752/2048, `security` 272800/8192, `userdata` 280992/166463. `capsulad-cli platform inspect -root
DIR` reads an extracted firmware tree offline.

## Supported configuration and the profile boundary

`internal/backend/profile` holds the device description as inert data: model/SoC fallbacks, serial
source, PulseAudio sink/source and PCM formats, microphone GPIO, LED device paths and geometry,
input directories, watchdog path/timeout/status id, stock Wi-Fi files, deployment budgets, vendor
processes, architecture/ABI acceptance, memory envelope and the ambient brightness curve. It has no
I/O and no interfaces.

Generic packages (daemon, Wi-Fi, audio, CLI) consume those values through their existing `Config`
structs. `internal/backend/linuxdev` is the board-agnostic Linux device layer both board backends
share (device-tree/sysfs helpers, evdev input, IIO/thermal sensors, LED-class devices, the vendor
WS2812B char device and the generic hardware watchdog). `internal/backend/rockchip` keeps device
knowledge that is not a knob: recovered ioctls and the measured segment calibration map.
`internal/backend/amlogic` keeps the two Amlogic panel paths and the stock/Alpine audio selection;
the ring channel map it needs is the same recovered table, so it lives in `linuxdev` with both
sources cited. `internal/backend/syswatchdog` owns the generic `/dev/watchdog` opener. The simulator
is a sibling backend with the neutral `profile.Simulator` profile, which deliberately has no device
paths, stock supplicant files, vendor processes or watchdog opener.

| Selection | Backend | Status |
|---|---|---|
| `-platform simulator` (default) | in-memory simulator | supported |
| `-platform rockchip-stock` (alias `rockchip`) | RK3308 stock backend | supported; unvalidated vendor paths report `PARTIAL`/`UNVERIFIED` |
| `-platform amlogic-stock` | Amlogic A113X stock backend | supported; the clock panel is `UNVERIFIED` because its vendor ABI is unrecovered |
| `-platform amlogic-alpine` | Amlogic A113X Alpine backend | supported; the panel and ring are probed, not assumed |
| `CAPSULA_MINI_AMLOGIC_STOCK`, `CAPSULA_MINI_AMLOGIC_ALPINE` | the two Amlogic backends | now reported by the daemon; the wire enum values are no longer reserved |

## Displays and LED ring

The boot image's DTB describes two LED controllers:

| Device tree node | Compatible | Wiring | Userspace |
|---|---|---|---|
| `/spi@ff120000/ws2812b@00` | `ws2812b`, `device-name = "topleds"` | 36 LED children, SPI max 2.5 MHz, CPOL=1 | `/dev/topleds` |
| `/i2c@ff060000/led-controller@3c` | `issi,is31fl3236` | 30 `clock-ledN` children at i2c 0x3c | `/dev/led_multi_ctrl` |

Role assignment, confirmed against the stock `voice-agent` payload:

| Evidence | Meaning |
|---|---|
| `/dev/topleds` + `LEDS_WS2812B_IOCTL_MULTI_SET` | 36-channel WS2812B chain → 12-pixel RGB ring |
| `/dev/led_multi_ctrl` + `LEDS_MULTI_CTRL_IOCTL_MULTI_SET` | 30-channel IS31FL3236 → 4×7-segment plus two-colon display |
| `usr/share/voice-agent/led/*_diodes.json` | frames with 12 `colors` entries → 12 ring pixels |
| `usr/share/voice-agent/led/*_digits.json` | frames with 30 `brightness` entries → 30 monochrome LEDs |
| `S59disable_led_animation.sh` | boot disables all `/sys/class/leds/*` triggers and brightness |

The vendor ABI is unpublished but recoverable from the stripped binary
(`/source/src/hw/capsula_mini_leds.cpp`, node wrapper `led_multi_ctrl.cpp`):

| Interface | Value | Argument |
|---|---|---|
| `LEDS_MULTI_CTRL_IOCTL_GET_COUNT` (both nodes) | `_IOR('z', 2, int)` = `0x80047a02` | `int *`; 36 for `/dev/topleds`, 30 for `/dev/led_multi_ctrl` |
| `LEDS_WS2812B_IOCTL_MULTI_SET` | `_IOW('z', 1, 16)` = `0x40107a01` | `{u32 start, u32 count, u8 *data}`; 36 channel bytes |
| `LEDS_MULTI_CTRL_IOCTL_MULTI_SET` | `_IOW('z', 1, 8)` = `0x40087a01` | 30 records of 20 bytes `{u32 enable=1, u32, u32 brightness, u32, u32}`, brightness at offset 8 |

`internal/backend/rockchip/led_vendor_linux.go` drives the nodes directly and validates the
driver-reported channel count against the device tree. When the vendor nodes are missing or cannot
be opened the backend falls back to Linux LED class devices (`/sys/class/leds/*`, `multi_intensity`
or `brightness`), and only then reports `BACKEND_UNVERIFIED` while keeping rendered frames in
memory. The sysfs fallback claims each selected LED by writing `none` to its `trigger` attribute,
mirroring `S59disable_led_animation.sh`; otherwise the device-tree default `timer` trigger ignores
brightness writes.

**Ring channel map.** Vendor frame R,G,B per pixel → physical WS2812B channel:

```
1,0,2, 34,33,35, 31,30,32, 28,27,29, 25,24,26, 22,21,23, 19,18,20,
16,15,17, 13,12,14, 10,9,11, 7,6,8, 4,3,5
```

This reverses the ring order and swaps R and G within each pixel, matching WS2812B GRB wire order.
The backend applies it to `RingFrame` pixels. The encoder applies a 2.2 gamma curve; the stock
frames are binary 0/255, so there is no vendor curve to match.

**Segment map.** Lighting all 30 channels produces "88:88" — four seven-segment digits plus two
colon dots — driven entirely through `LEDS_MULTI_CTRL_IOCTL_MULTI_SET`. The driver record order is
neither digit-major nor segment-major; the measured digit/segment → channel table
(`multiCtrlChannelMap`, produced by `cmd/ledmap`) is:

| Digit | a | b | c | d | e | f | g |
|---|---|---|---|---|---|---|---|
| 1 | 1 | 28 | 21 | 17 | 22 | 29 | 24 |
| 2 | 26 | 25 | 19 | 18 | 23 | 27 | 20 |
| 3 | 0 | 2 | 7 | 5 | 6 | 4 | 3 |
| 4 | 10 | 12 | 11 | 14 | 13 | 8 | 9 |

Colon: top `16`, bottom `15`. A clock face renders correctly with this table and
`TestEncodeMultiCtrlFrameClockPattern` pins the chain from glyph bits to channels. The segment
display is driven linearly, matching the accepted stock calibration.

Bring-up instruments:

- `capsulad-cli platform ledtest` writes raw driver channels outside the daemon: `-channels 0-6,28`,
  `-sweep`, `-bg` for a dim grid, `-json FILE -map identity|vendor|inverse` to replay a vendor
  animation, and `-ring -color R,G,B -level` for the WS2812B ring.
- `cmd/ledmap` walks all 30 channels over adb, asks which digit and segment lit, writes
  `ledmap.json` and prints the table; it resumes if interrupted.

## Audio

`/etc/pulse/device.pa` configures a system PulseAudio instance:

- playback: ALSA sink `playback`, virtual sinks `alarm`, `ring`, `call`, `purr`, `master`
  (`master` is default) and a `snapcast` FIFO;
- capture: `speex` (default, processed), `call_src`, `mics` (4 ch), `mics_loopback` (5 ch),
  `loopback`;
- `S49stereo-softvol.sh` primes the stereo playback path; `S51volume_up` sets DAC line-out gain.

The Rockchip backend uses the firmware's `pacat`/`parec` clients: playback to `master` at 48 kHz
stereo, capture from `speex` at 16 kHz mono S16LE. The backend pins the `master` sink gain at unity
(`pactl set-sink-volume … 100%` followed by a read-back of every channel) at startup and on every
master change, so the mixer's master is the only gain the framework sets; the pin covers the sink
volume, not `pacat`'s per-stream sink-input volume, which nothing here sets or reads. The subsystem
reports `PARTIAL`. Latency, format negotiation and coexistence with vendor daemons are open gates.

Applications capture `audio.input.microphone` through a bounded framework DSP chain that runs only
for the production S16LE mono 16 kHz `speex` format: 80 Hz high-pass, adaptive spectral suppression
capped at 12 dB, unity gain (no AGC), a −1 dBFS peak limiter, and a 256-sample frame / 128 hop
(≈15.9 ms) STFT. State and output length are fixed, chunk boundaries do not change output, and the
chain resets on mute, source reopen or a capture gap. Wakeword inference, enrollment and training
consume this same processed tap (`framework.dsp.v1`); untouched physical `speex` samples exist only
at `audio.input.microphone.raw`, and frames whose tap declaration does not match are rejected.
Estimator behaviour and its measured limits are in [WAKEWORD.md](WAKEWORD.md).

The detection budget is per **frontend**, not per word. One complete frontend emits an embedding in
37.9 ms mean against an 80 ms hop, which is why every installed word shares it: N words add only
their own heads over one 16×96 embedding ring, and per-hop scoring of N heads allocates nothing.
Each word keeps one in-memory classifier head of about 53 KB (weights plus its widened embedding
window) and one JSON artifact of about 130 KB under `<data-dir>/wakeword/models/<id>.json` (the
installed reference artifact measures 132,984 bytes). Eight words are therefore about 1 MB of
`/data` plus roughly 0.4 MB of process memory, over one shared 1.15 MB frontend. Installing or
deleting a word rebuilds the ensemble and re-subscribes one runner, which costs roughly two seconds
of warm-up for all words; a per-word enable toggle or rebinding takes effect on the next activation
and costs nothing. The host measurements over N=1…5
and the device limits are in [WAKEWORD.md](WAKEWORD.md#multiple-wakewords-and-app-binding).

Synthetic pink, brown/rumble and amplitude-modulated room regressions measure 4.23, 8.33 and 3.42 dB
of cleanup. Real-world evidence is one 10 s background recording — mostly music played through a Mac's
own or external speakers, not through the Capsula Mini — where cleanup measures 2.9 dB overall and 2.7 dB in
80–300 Hz. The recording is unannotated and has no local playback reference to cancel the music, so
it is consumed background evidence, not fan/traffic, music-immunity, new-location or zero-FAR proof.
Endpoint negotiation with a real `speex` source, subjective suppression benefit and transparency are
hardware/listening gates.

## Microphone gate and mute

`S50mic-enable` exports GPIO 21 (GPIO0_C5) as an output and drives it high at boot. The backend
reads `MIC_EN_GPIO` from that script (falling back to the compiled default) and writes `0`/`1` to
`/sys/class/gpio/gpioN/value` on mute/unmute, also asking PulseAudio to mute the `speex` source.
Hardware mute is reported only when the GPIO path exists.

Verified with the vendor agent stopped: `gpio21` goes 1→0 with the setting, `pactl get-source-mute
speex` follows, and an acoustic loop shows the gate cuts the microphone (live capture RMS 1977, peak
9420 → RMS 38, peak 222 with PulseAudio unmuted), so the attenuation is the GPIO gate and not the
mixer. Proof level: electrical gate state plus digital capture.

## Touch and keys

Input is standard evdev under `/dev/input`. No vendor input protocol is needed: the backend reads
`struct input_event` and maps keys using the live `gpio-keys` node. Five keys are declared:

| Device-tree node | `linux,code` | Framework key |
|---|---|---|
| `micmute-key` | `0xf8` `KEY_MICMUTE` | `mic_toggle` |
| `volumeup-key` | `0x73` `KEY_VOLUMEUP` | `volume_up` |
| `playpause-key` | `0xa4` `KEY_PLAYPAUSE` | `play_pause` |
| `volumedown-key` | `0x72` `KEY_VOLUMEDOWN` | `volume_down` |
| `voice-key` | `0x2c0` `BTN_TRIGGER_HAPPY1` in one DTB, `0x246` `KEY_VOICECOMMAND` in the other | `center` |

The two DTBs in the boot partition disagree on the voice-key code and label, so the backend derives
the semantic name from each child's node name and label and merges those entries over a default map.
`BTN_0`, `KEY_ENTER` and `KEY_OK` map to `primary`. `voice-key` is a vendor label, not an assistant
function: both codes are the same **unmarked neutral centre** of the five-key layout. Roles:
microphone mute (north), volume up (east), circle/play-pause talk key (south), volume down (west),
neutral centre. The centre dismisses alerts and assistant turns and drives stopwatch gestures but
never summons the assistant; the talk key is `play_pause`.

There is no coordinate panel. `touch.panel.main` advertises `coordinates_available=false` and
`full_raw_available=false`, while the backend advertises `full_panel_available=true`: all five keys
and gestures inferred from ordered presses are deliverable. Partial capture receives
`play_pause`/`center` keys and swipes; full capture additionally recognizes circles while
forwarding every key, with all system effects suppressed. The user's map is authoritative — east
is volume up, west is volume down — and direction is not inverted from a single passive trace.

Raw captures from `/dev/input/event0` show the node is `gpio-keys` only, advertising `EV_SYN|EV_KEY`
(`EV=3`), and that a swipe over the whole panel emits exactly one press + SYN + release + SYN.
Full-panel up reports `0x72` (`volume_down`), right `0xf8` (`mic_toggle`), down `0xf8` and left
`0x73` (`volume_up`). A swipe anchored at the physical centre can report `0x2c0`
(`BTN_TRIGGER_HAPPY1`) or `0x246` (`KEY_VOICECOMMAND`). The centre key is therefore the only extra
gesture evidence a single contact adds: a bare volume edge acts immediately as a volume step, with
zero deferral. Because the pad reports one key per gesture, a whole-panel crossing appears as two
opposite end-stop edges; `volume_down` then `volume_up` (or the reverse) with no centre between them
and a press-to-press gap of at most 400 ms is the full-panel crossing — `swipe_right` (next) or
`swipe_left` (previous). The leading edge has already stepped when the second resolves the gesture,
so that one step is reverted through the canonical master-volume path and the completing edge is
suppressed: the crossing costs a revert inside the 400 ms window, never a deferral. A slow opposite
pair (> 400 ms) stays two deliberate steps, and a single end-stop key remains a tap, not a skip gesture. The
retained device log contains a real `center p+0ms r+115ms | gap 14ms volume_down p+130ms down`
centre→edge pair (130 ms press-to-press, inside the 250 ms window); its previous heuristic also
scored `volume_down … gap 123ms volume_up` as `swipe_right`, which is now the full-panel crossing
shape rather than the rejected unanchored pair. The centre gate is a centre press followed by a
volume edge — no press-gap allowance while the centre is still held, or within 700 ms after it is
released (the usable form on this pad, which reports one key at a time); the documented
opposite→centre→direction chain also has 250 ms inter-contact and
1500 ms total bounds. Every contact acts on its own edge, so a chain's leading edge has already
produced its action when the direction contact completes the gesture, and a full-panel crossing's
leading step is reverted the same way. Circles are recognised in full
capture only. The measured `swipe_half.hex` capture reports one gesture as `0xf8` and the next as
`0x2c0`, while `swipe_full.hex` holds three gestures with no centre contact; a single edge-only
report is still indistinguishable from a volume tap, which is why the crossing needs the ordered
opposite pair inside the window.

These captures prove the event shape, not the deployed resolver's behaviour: the centre→edge pair is
observed in a retained device log, while the new resolver's on-device recognition still needs
confirmation with physical presses. Host-side gesture, zero-deferral and full-capture-only circle
contracts are in [VERIFICATION.md](VERIFICATION.md); the full gesture and precedence contract is in
[SPEAKER_BEHAVIOR.md](SPEAKER_BEHAVIOR.md).

## Vendor stack and supersede

Stock daemons: `voice-agent` (13 MB), `stereoctl`, `okcall`, `telemetryd`, `sysmon`, `btctl`,
`snapclient`, `snapserver`, `avahi-daemon`, `dnsmasq`, `crond`, `pulseaudio`. Vendor IPC:
`/tmp/stereoctl.sock`, `/tmp/okcall.sock`, `/tmp/telemetry.sock`, `/tmp/btctl.socket`, plus
`/tmp/voice-assets` and `/tmp/tgclient` (LZMA-decompressed at boot).

The stock chain is `sysmon` (supervisor) → `voice-agent -mode=master` → `voice-agent -mode=app
-watchdog`. Conflicts with the framework: the app process holds `/dev/input/event0` (the play/pause
key the framework also reads) and `/dev/watchdog`; `btctl` and `tgclient` inherit the watchdog
descriptor. Two owners of the key and three of the timer are not workable.

`capsulad -supersede=auto` (the default on `*-stock` platforms) stops `sysmon` and `voice-agent`,
escalates to SIGKILL when SIGTERM is ignored, asks the framework's own watchdog feeder to release
the device and stops every other `/dev/watchdog` holder, then arms the timer itself:

```
INFO daemon    superseding voice-agent pid=418 (SIGTERM)
INFO daemon    superseding sysmon pid=427 (SIGTERM)
INFO daemon    superseding voice-agent pid=437 (SIGTERM)
INFO watchdog  /dev/watchdog armed, timeout=21s
```

`-supersede=off` leaves the vendor stack alone; an explicit comma list replaces the default set. A
reaper stops any superseded process that respawns. Capture is unaffected by supersede: a 2 s take
records room-noise RMS 1470 with the vendor stack replaced, against 2196 on the vendor stack.

The LED nodes are the remaining shared surface: two vendor processes hold `/dev/topleds` and
`/dev/led_multi_ctrl` open concurrently with `capsulad`. Opening both nodes and issuing `GET_COUNT`
alongside them works — the driver does not enforce exclusive ownership — but frame writes race the
stock agent until it is stopped as well. When the vendor assistant does not start, the nodes have
one writer.

`capsulad -platform rockchip-stock` starts on this image and reports `mode=mountns`; the built-in
clock and ring system apps drive `display.segment.front` and the LED ring without separately
installed clock or ring packages.

## Boot service

**`/etc/init.d/S99btctl.sh` is the only capsulad-specific boot file** (repo:
`deploy/rockchip-stock/S99btctl.sh`). `rcS` expands `/etc/init.d/S??*` once, before
`S23overlayfs.sh` pivots into the `/data` overlay, so the hook keeps the stock S99 path name. The
hook caps the boot log at 64 KiB, prepares `/data` and `/data/capsdemo/run`,
moves the Bluetooth UART IRQ, retires `S97voice-agent.sh`/`S99sysmon.sh` (pristine copies in
`/data/capsdemo/vendor-init/stock`), starts the daemon after `S02mount-data`, `S50mic-enable` and
`S50pulseaudio`, gates on `capsulad -health` (0 = framework API answers, 1 = no daemon, 2 = binary
predates `-health`, trust the process), and otherwise restores and starts the preserved vendor chain
(`/data/capsdemo/vendor-init/S97voice-agent.sh`).

`/etc/init.d/S99btctl.sh disable` stops the daemon, restores the vendor init scripts, starts the vendor
assistant, writes `/data/capsdemo/boot/disabled` and moves the hook to
`/data/capsdemo/boot/S99btctl.sh.disabled`; running that copy with `enable` clears the marker and
reinstates the hook. Removing the file alone is safe only after `disable` restored the vendor scripts,
and a firmware-provided pristine `S99btctl.sh` can be copied back.

Deployment is one bundle applied by `capsulad-cli deploy <dir-or-tar.gz>` holding `capsulad`,
`capsulad-cli`, `lib/`, `apps/*.tar.gz` and `boot/S99btctl.sh`; unknown top-level entries are reported
and ignored, and the restart goes through the hook so the watchdog keeps one owner. The app archives
are compiled and packaged on the build host (`make dumbassistant`, `make devapps`, then
`make deploy-bundle`, which only copies them); the device installs archives and never packages. A
factory reset wipes the overlay, so the device boots the stock assistant and the deployment must be
repeated.

## Bluetooth

BlueZ 5.65 over the system bus; `hciconfig` is not used. The daemon sets `Adapter1.Powered`,
`Discoverable` and `Pairable` through D-Bus, repairs its A2DP registration and power state after a
BlueZ restart, and never polls adapter properties.

**A2DP sink.** Without a registered endpoint the adapter advertises only AVRCP (0x110C/0x110E) and a
source refuses to connect with `br-connection-profile-unavailable`. The daemon exports an
`org.bluez.MediaEndpoint1` SBC endpoint, registers it with `Media1.RegisterEndpoint`, and verifies
it is armed for the current adapter generation **before** the adapter becomes connectable,
discoverable or pairable and before an API request can enable those. A failed registration, a BlueZ
restart or an adapter power cycle invalidates readiness: it is not reported ready, it is repaired
before the next connect, `BluetoothStatus.Detail` says the audio profile is not advertised, and
discoverability and pairability stay refused.

If native registration is impossible the daemon falls back to PulseAudio's
`module-bluetooth-discover`, which advertises Audio Sink (0x110B), Headset (0x1108) and Audio Source
(0x110A) and exposes `bluez_source.<ADDR>.a2dp_source`. PA's Bluetooth endpoints are removed only
after the native sink is armed, and never while PulseAudio is actively carrying a connected
source's audio (its bluez5 source is `RUNNING`, or the loopback this daemon loaded for it is
present). A loaded module set with no running source is retired as soon as the native sink is
armed, including while a source is merely connected or paused, so the next connection cannot land on
the Headset (0x1108) endpoint PA keeps advertising; the native owner then takes that connection.

BlueZ may autopower the adapter before `capsulad` starts. In that window a paired source can open an
ACL while the framework's sink is not advertised; the incoming-connect handler repairs and verifies
the registration and requests A2DP/AVRCP over the same link via `Device1.ConnectProfile` (no second
ACL). Whether a peer that already abandoned A2DP honors that request without reconnecting is
device-dependent (real-peer gate).

After every connection the daemon asserts the session, not the profile request: within about five
seconds of the profile request finishing it requires a `MediaTransport1` carrying the A2DP sink
UUID (0x110B) and the SBC codec for that device (`internal/bluetooth.AudioLink`). While the link is
not A2DP, `BluetoothStatus.Detail` and the media detail both carry the bounded sentence "audio link
is not A2DP", and a Warn names the profiles that are observably up plus the audio services the
peer advertises. If no transport appears, the daemon **requests** the peer's A2DP source profile
(`Device1.ConnectProfile` with the Audio Source UUID `0000110a`, bounded at 15 s) **once per
connection**, and never disconnects the source to renegotiate: dropping a user's phone off the
adapter stops their audio mid-sentence and cannot be undone by the daemon, and a reconnect can land
on an even worse profile. The bound is per connection, not per source: a fresh connection is a fresh
negotiation and may ask again immediately — the field case where a third reconnect was refused
because the previous connection's request was remembered left the host with no audio output at all —
while a second verdict inside one connection is refused, so a source that keeps landing on a
non-A2DP profile is not asked in a loop. A newer connection, a disconnect and daemon shutdown cancel a pending
verdict, and a source that stays without a transport keeps its link and its `audio link is not
A2DP` verdict instead of being cycled. A successful `ConnectProfile` is never logged as a profile
that is up: the log states the observed transport instead.

On this BlueZ the endpoint API is `SelectConfiguration(ay) -> ay` and `SetConfiguration(o transport,
a{sv} properties)`. The transport object has no `Endpoint` property, so the path returned by
`SetConfiguration` is used (UUID/Codec fallback). The transport fd is acquired with
`MediaTransport1.Acquire` only while the transport is `pending` or `active`; an `idle` transport is
silent (the source is paused), and reading its fd blocks while BlueZ streams on a real active
transport, observed as a silent stream then `EOF`.

Two facts about BlueZ 5.65 that the code depends on: **an acquired `MediaTransport1` is not removed
from the object tree** (BlueZ unregisters the interface only when the transport is destroyed, while
`Acquire` merely takes an owner and sets `State=active`), so the object tree stays readable during
playback and the transport path recorded at `Acquire` is used only to avoid a lookup that could lag
that reply. And `org.bluez.MediaControl1` **is deprecated but present**: it is exported while an
AVRCP target/controller session for the device is connected, and each method is an AVRCP
passthrough key press (Play/Pause/Next/Previous, `VolumeUp`/`VolumeDown`) with no volume value, so
it is only a fallback for a source that exposes no `MediaPlayer1`.

Skip and voice are sent as an AV/C panel press **and an explicit release**, not through BlueZ's
`Next()`/`Previous()` convenience methods: those send the press and rely on the peer answering
`ACCEPTED` before BlueZ emits the release, so one lost reply latches the key and every later press
of the same operation is ignored, while Play/Pause keeps working because alternating operations
force a release of the previous key. The operations used are `0x4b` (Forward), `0x4c` (Backward) and
`0x41` (Voice Recognition, which opens the source's own assistant). Voice needs a `MediaPlayer1`
object; `MediaControl1` has no such method, so a source exposing only that interface answers
"does not support this operation" and the daemon reports it instead of pretending to have sent it.

RTP is depacketized and decoded in-process by `internal/sbc`, a pure-Go 8-subband SBC decoder ported
from the Apache-2.0 Android reference (`internal/sbc/NOTICE`). It keeps synthesis filter history
across A2DP packets and writes S16LE straight to the mixer — no `sbcdec` process, temp file, chunk
boundary or warm-up — and matches the system `sbcdec` and the Android reference sample-for-sample on
the 172-frame fixture (correlation 1.0, RMS 6936). Configuration: 44.1 kHz joint stereo, 16 blocks,
8 subbands, loudness allocation, bitpool 2..53, negotiated from the remote SEP's four-octet SBC
capabilities. The local SEP advertises the spec's four-octet media codec capabilities
(`F3 7F 02 35`: 44.1/48/32/16 kHz with joint stereo and stereo, 16/12/8 blocks, 8/4 subbands,
loudness and SNR, bitpool 2..53). The maximum is 53 because that is the A2DP-recommended
high-quality ceiling for 44.1 kHz joint stereo and every SBC source must support it; a higher
advertiseable value (the Mac offers 250) buys no audible quality for SBC and only spends airtime.
PA keeps app playback and the microphones. Negotiation reached CONFIGURED/OPEN and created a
transport on hardware; audible parity with the vendor chain is a listening gate.

**Volume.** The framework master is the listening level and the AVRCP absolute-volume register mirrors
it in both directions. A volume the source reports (BlueZ `PropertiesChanged`, or the coalesced
transport read on resync) becomes the framework master through the canonical master path, which
persists it and applies the local gain, so moving the phone's slider moves the speaker. A change made
on the speaker (dial, keys, Web UI, an app) is written back to the source's register so the source's
own level display can follow it; the write is a display mirror, applied asynchronously and never on
the key or render path, coalesced over about 100 ms so the leading step of a full-panel swipe and its
revert produce one write, and the local curve (`sqrt(master)` on top of the mixer's own master factor)
remains the only gain stage, with no compensation factor for the source's share. Field evidence from
the deployed build: every speaker-side change logs `transport volume mirrored to the source: N%` and
BlueZ accepts the write with no refusal — whether the peer applies or displays it is the peer's
decision, and this MacBook does not move its own level display. That is not chased further on our
side. Both alternatives fail on the unit: writing the register *and* letting it attenuate the stream
made 10 % on both ends sound near-silent, and compensating the local trim for the host's share made 5 %
far too loud, because this peer reports the written value back without actually attenuating the A2DP
stream, so its "confirmation" is not evidence. The peer's report of the level the daemon just wrote is
therefore suppressed as an echo: a report within one AVRCP step (the peer's own rounding) of a level
written in the last 900 ms is ignored instead of adopted, while anything further away is the user's
slider and is adopted at once, even while a write is still open. The window is deliberately narrow —
an earlier 4-step/5-second filter swallowed a whole slider drag and made the speaker lag the phone.
The echo memory holds the last four writes, is cleared on connect and disconnect so a new session
cannot inherit it, and adds no polling. A source without a writable register is not an error: the
write is refused, logged once per session at Debug, and the speaker keeps attenuating locally while a
source-side slider still drives the master.
A source is **not** trusted as the only attenuation:
with the capsulad master at 5 % and a source output at minimum, the transport volume can read
~6/127 while the captured stream is near full scale and the speaker ~90–100 % loud — the slider
moved without attenuating the stream. Proof level: D-Bus property read plus digital capture;
loudness is acoustic.

With PulseAudio as fallback owner the loopback carries a local safety gain of master^1.5 (cube-root
compensated, because PA sink-input volume is cubic) while the source is held at a fixed 90 % trim; a
linear master is too hot at 5–7 % on this amplifier, and gain and trim are applied on source/route
changes and volume events rather than by periodic re-assert. Audible Bluetooth level is bounded by
master^1.5 and at most about 0.73 × it. Three numeric stages cannot all equal the master — one must
be neutral — so the master is the one synced value and the source register is its mirror, with the
PA source (90 %) and the loopback (cube-root) as the gain stages. The framework volume cue peaks at
0.22 of full scale.

PulseAudio's `module-bluez5-device` is reloaded with `avrcp_absolute_volume=0` on every source
connect in the fallback path, because PA recreates it with the default (1) on a profile reconnect;
that default mirrors the transport onto the local source with a cubic second attenuation — at master
30 / source 50 the source mirrored 47 % (10.4 % cubic) and the audio collapsed to ~1.5 %. The
per-connect reload plus source/route events cover recreation without a periodic self-heal.

**Jitter.** With a 60 ms loopback target the source buffer swung by hundreds of milliseconds and the
resampler moved between 44.03 and 44.4 kHz; at 250 ms the source settles at 57–111 ms against its 68
ms target at ~44.1 kHz. The native endpoint bypasses that loopback and its resampler. The Bluetooth
UART IRQ is moved to CPU3 by the boot hook, because `hci_uart` logged `Unexpected start frame`
errors while the IRQ shared CPU0 with the daemon and wakeword inference.

HSP/HFP telephony is not implemented: only A2DP Sink plus AVRCP CT, which is why a session that
lands on a Headset profile is silent rather than narrowband, and why the daemon requests its own
A2DP source profile and reports the link instead of accepting any connected profile. Paired-device
listing and unpair are Web-UI/CLI only — `ListPairedDevices` and `UnpairDevice` reject supervised app
callers by run ID and sandbox UID, while the rest of the Bluetooth surface is app-facing. The
browser never receives a Bluetooth address: each paired device is identified by an opaque ID derived
from the address, and unpair resolves that ID server-side before calling the framework. Unpair is
exercised against the simulator manager only.

Verified with a paired MacBook: after `Device1.ConnectProfile(0x110A)` and the AVRCP profile,
`Device1.Connected=true`, `MediaControl1.Connected=true`, and `MediaPlayer1`/`MediaTransport1`
objects exist; AVRCP Play/Pause/Next/Previous calls succeed and the player state moves to `playing`;
setting the framework volume to 50 % writes `Volume=64` of 127 on the idle A2DP transport and
restoring 80 % writes `102`; toggling `Pairable`/`Discoverable` from the Web UI reaches BlueZ and
survives a reboot
(`bluetooth_discoverable` is a typed setting and discoverable implies pairable). That MacBook's
`MediaPlayer1` exposes `Device` and `Position` but no `Status` or `Track` at rest, so title/artist
stay empty: metadata is read when a source publishes it, and macOS fills those attributes only for
some players.

The Web UI "Now playing" panel is fed by the framework, not by a shipped app: while a source is
connected, the panel lists one framework-owned player (`bluetooth`) built from the public
`BluetoothMediaService`, with the playback direction the daemon resolved and the source's metadata,
and its transport buttons call that service's `Command`. No media app is bundled and no app id is
reserved for one. The simulator proves the provider view and command routing; the MacBook session
above predates it, so the rendered Overview was not observed against a physical phone.

## mDNS and Avahi

The stock `avahi-daemon` owns multicast DNS and the 5353 socket; `capsulad` never starts a second
responder or binds 5353. It applies the persisted single-label `Hostname` setting (default
`capsula`, lower-cased, validated as one DNS label, `.local` added by Avahi) through the public
`org.freedesktop.Avahi.Server` calls on the system bus, using the normal settings CAS revision.

Apply reads `GetState`/`GetHostName` and makes **no** `SetHostName` write when the responder is
RUNNING and already owns the requested label, confirming the advertisement through
`GetHostNameFqdn`. After a write it waits a bounded interval for the responder to report the
requested label; a redundant reply or a concurrent writer winning the race is accepted only from
that authoritative read. Write-time collisions, invalid names and a missing responder stay visible
errors with the observed FQDN; a write whose FQDN cannot be read is reported as
applied-but-unconfirmed, never a fabricated advertisement.

The daemon applies the latest requested label and generation at startup and when Avahi transitions
to `AVAHI_SERVER_RUNNING`; the monitor subscribes to `StateChanged` before its initial snapshot, and
a coalesced worker drains changes during an apply, replaying the latest RUNNING edge instead of
dropping it. `Close` cancels without a warning. `GET /settings` is read-only: it renders the stored
status and never applies a name or saves configuration. The simulator and unsupported platforms
report mDNS as unavailable and retain the saved label. Durability comes from the settings store
being re-applied on daemon start.

A read-only snapshot of the unit reported Avahi `RUNNING`, `GetHostName=capsula`,
`GetHostNameFqdn=capsula.local`. Fake-responder tests and a browser run against a responder fixture
cover the ordering (zero `SetHostName` calls for startup, a GET and a same-name save; one for a real
change; a collision stays visible with no advertised claim; recovery after restoring the label). That
is fixture-backed browser evidence, not device proof: real `.local` resolution and collision/rename
after an Avahi restart are device gates.

## Sensors

The `bh1730fvc` on `i2c@ff060000` is exposed through IIO. The backend scans
`/sys/bus/iio/devices/iio:device*/name`, prefers the processed `in_illuminance_input` (unit `lux`)
and falls back to `in_illuminance_raw` or the i2c-driver paths (unit `lux-raw`). Surfaced as
`sensor.ambient_light`.

`tsadc@ff1f0000` and the `soc-thermal` / `logic-thermal` zones are surfaced as
`sensor.soc_temperature` and `sensor.logic_temperature` from `/sys/class/thermal/thermal_zone*/temp`
(millidegrees converted to Celsius). Other zones appear as `sensor.temperature.<type>`.

## Sandbox constraints

- Root filesystem is SquashFS read-only; persistent state lives on ext2 `userdata` via the overlay.
- Apps get a read-only `/app` and `/assets`, no writable `/tmp` or home, and the daemon socket as
  their only filesystem IPC endpoint.
- Kernel 4.4.167 delivers only a mount namespace: user, PID, network and IPC namespaces and the
  seccomp filter are absent (see probe results). `capsulad` probes user-namespace and filter support
  at startup, falls back to a root mount namespace with a distinct app UID, and reports the actual
  mode. It never claims isolation it did not obtain.
- Vendor daemons may hold PulseAudio, evdev or LED nodes. Capability status and diagnostics are the
  source of truth; no parity across Mini variants is claimed.
- A native app maps `libcapsulad.so.1` (ABI 1.0) from `/capsulad/lib` and reaches `APP_READY`; static
  Go app packages need no libraries, and other Rust binaries need a matching libc and loader.

## Device probe results

Kernel 4.4.167 on the Capsula Mini, reached over `adb` at `192.168.1.50:5555`.

| Probe | Result | Proof level |
|---|---|---|
| `unshare(CLONE_NEWNS)` | ok | probe |
| `unshare(CLONE_NEWUSER)` | `EINVAL` — `CONFIG_USER_NS` off | probe |
| `unshare(CLONE_NEWNET)` | `EINVAL` | probe |
| `prctl(PR_SET_SECCOMP, FILTER)` | `EINVAL` — no `CONFIG_SECCOMP_FILTER` | probe |
| `/proc/self/ns` | `mnt` only | probe |
| `LEDS_MULTI_CTRL_IOCTL_GET_COUNT` on `/dev/topleds` | 36 | probe |
| `LEDS_MULTI_CTRL_IOCTL_GET_COUNT` on `/dev/led_multi_ctrl` | 30 | probe |
| `/proc/bus/input/devices` | one node, `gpio-keys` (event0); no touch panel | probe |
| `pactl list short sinks/sources` | `alsa_playback` + `alarm`/`ring`/`call`/`purr`/`master`/`snapcast`; `speex` + `call_src`/`mics`/`mics_loopback`/`loopback` at 16 kHz mono | probe |
| all 30 display channels lit | "88:88" — 4 digits + 2 colon dots | optical |
| ring pixel 0 | top of the ring; indices 0→11 travel smoothly clockwise; `R,G,B=(255,0,0)` renders red | optical |
| `/sys/class/gpio` | `gpio21` exported at boot by `S50mic-enable` | probe |
| `WDIOC_GETTIMEOUT` on `/dev/watchdog` | 21 s | probe |
| `WDIOC_SETOPTIONS(WDIOS_DISABLECARD)` | `ENOTTY` — unsupported | probe |
| magic close (`'V'`) | ignored: the board resets ~21 s after feeding stops | probe |
| `/sys/class/watchdog` | absent (no `CONFIG_WATCHDOG_SYSFS`); `nowayout` unreadable | probe |
| mixer path | app-bus playback reaches `master` (1 kHz tone, RMS 4454 on `master.monitor` at 70 % volume, 1908 at 30 % — the source's 6364 × software volume); a conversation ducks it to 0.25 × (1113 / 477) with a ~150 ms fade | digital output |
| microphone gate | GPIO 0→1 with mute state; live capture RMS 1977 / peak 9420 → RMS 38 / peak 222 | electrical + digital output |
| timezone | `/etc/timezone` says `Asia/Shanghai` (UTC+8) while `/etc/localtime` → `Europe/Moscow` (UTC+3); `date` reports MSK | probe |

The visible-indication watchdog terminates a stream 5 s after the last qualifying indication
(polling every 1 s, so 5–6 s after the indication disappears).

## Hardware watchdog ownership

`/dev/watchdog` is opened by the vendor stack: `voice-agent -mode=app -watchdog` holds it and
`btctl`/`tgclient` inherit the descriptor. `self-check.sh` (S98) is a firmware-update validator, not
a service watchdog: it reboots only when `/data/update_successful` exists, and reboots on `pgrep -x
voice-agent` failure within that window. On a settled unit neither flag exists and the script exits
immediately.

Because `SETOPTIONS` and the magic close are both unavailable, an armed timer on this kernel can
only be **kept alive**, never stopped; the RK3308 stock profile records that as
`Watchdog.Nowayout: true` and the supervisor's `Close` is not treated as a stop button. (Other
profiles keep the field false until their own disarm semantics are measured, so a graceful stop
there closes the handle and uses no feeder.) The framework therefore treats the timer as a system
liveness contract:

- `capsulad` starts the retry loop before superseding the vendor stack, so the framework owns the
  descriptor the moment the vendor releases it; it reports `BACKEND_PARTIAL` until then. A device
  held by another process is `backend.ErrWatchdogBusy`: the supervisor retries it and asks a
  framework feeder to hand the timer back instead of killing the holder.
- Once armed it feeds **only while the output loop keeps stepping**. The keepalive cadence and the
  stall window are derived per arm from the interval the driver reports (`WDIOC_GETTIMEOUT`):
  cadence `min(timeout/3, 2 s)`, window `min(timeout/2, 10 s)`, both strictly inside the armed
  timer; a driver that reports nothing falls back to the profile's declared interval and says so in
  the state. Opening the device arms the timer, so a keepalive is written before the open returns,
  and the loop then ticks at that same derived cadence — faster than the probe interval for a short
  timer — so the effective keepalive period is the cadence itself. A stalled loop, or a
  failed keepalive, latches the supervisor off, reports `BACKEND_PARTIAL` with the reason, and
  leaves the timer to reset the board. A live goroutine that stops completing steps is not health,
  and a broken daemon's timer is never handed to a feeder.
- A graceful stop hands the descriptor to a detached BusyBox `sh` feeder that holds only the
  inherited descriptor (fd 3) and writes a keepalive every 2 s **for at most 5 minutes**, so planned
  restarts and upgrades do not reset the board while a speaker whose framework never comes back
  still resets instead of running unmanaged. The next `capsulad` reclaims it through
  `run/watchdog.handback`; `S99btctl.sh start` and the deploy tool do the same, and `disable`
  releases it before the vendor chain takes over. The feeder is a shell on purpose: a process
  running a copy of the daemon would keep the old binary's inode alive on `/data` after the daemon
  exits, which leaves no room to stage a 40 MiB replacement on the 78.5 MiB partition. Its pid file
  (`run/watchdog.feeder`) records `<pid> shell`; a feeder left by an older release is a daemon copy
  (`__capsulad_watchdog_feeder`). `run/` is persistent, so a pid file whose process is gone — or
  whose pid now belongs to another process — is cleared by the daemon, the boot script and the
  deploy tool rather than waited on.
- A crash or OOM kill of `capsulad` leaves nobody feeding, so the board resets within one armed
  timeout. That is the intended fail-safe: the supervisor owns the timer and its death returns the
  device to a known state.
- Proof limits: the driver facts (21 s, ignored magic close, `ENOTTY` disable, no
  `/sys/class/watchdog`) are probe results on this unit, and the health-gated reset itself has not
  been observed on hardware. Host tests pin the supervisor's decisions, the feeder lifetime, the
  handback path and the stale-pid cleanup, not a real reset.

Killing a vendor watchdog holder by hand without giving the timer a new owner resets the board after
the 21 s timeout (`dw_wdt: keepalive missed, machine will reset`); handover is always the daemon's
job. Never `SIGKILL` a framework feeder for the same reason: ask for the handback marker instead.

## Ambient-light brightness

The stock brightness is not the `powf(x, 4)` curve in `ClockBrightnessCalc` (0x157258) — that path
feeds something else. The displayed value comes from the light handler at 0x27ca68, which reads
`in_illuminance_both_input` (the same IIO file the framework uses) and maps:

```
lux < 5        -> 0
lux > 32768    -> 100
otherwise      -> trunc(logf(0.75 + 0.05*lux) * 19.9375)   // percent
clamped to [5, 95]; re-applied only when |new - applied| > 5
```

Constants: `0x3d4ccccd` (0.05), 0.75, `0x419f806b` (19.9375), thresholds 5 and `0x453b8000` (32768).
Confirmed by the driver's LED-class brightness: the stock unit wrote **14/255** at 7.4 lux and the
framework writes **13/255** at 7.3 lux (5 % floor, one LSB of truncation). The framework animates
toward each new value at 0.06 per 100 ms tick, matching the smooth stock transition.

Framework divergence: the vendor curve returns 0 below 5 lux, which blanks an enabled automatic
display in darkness. The framework keeps the stock curve across the sensor range but floors the auto
path at 5 % (13/255 after quantization), ignores malformed samples (`NaN`, `±Inf`, negative) instead
of treating them as darkness, and keeps the last good target when a read fails. The floor applies
only in auto mode; a manual brightness of 0 is an explicit off. The 5 % level is intended to be
visible; its physical result in a dark room is not measured (optical gate).

The built-in clock system app takes the timezone from the daemon's device timezone unless the
`capsulad.clock` app-configuration `timezone` key overrides it; the sandbox does not force `TZ=UTC`,
and `/etc/localtime` and `/usr/share/zoneinfo` are bind-mounted so a scripted or native app can
resolve zones.

## Amlogic A113X (stock Buildroot and Alpine Linux)

A second Capsula Mini board family runs the same product on an Amlogic A113X. It is a different
SoC, a different amplifier, a different Wi-Fi module and a different display controller; nothing
from the RK3308 sections above transfers except the framework contracts.

**Evidence level.** Board facts now come from the supplied offline images (device trees and kernel
config), plus a published hardware survey and two third-party projects. The offline DT/config facts
are first-hand but not a boot test; nothing here is claimed as live proof.

Supplied images inspected read-only (provenance; no user data or keys are reproduced):

| Artifact | SHA-256 |
|---|---|
| stock `device.dtb` (from the after-jailbreak dump) | `928710fd7b975194259a9b4ab3879ee698f5a2db1353ec206210ea35dd86caa2` |
| Alpine `fw_alpine/vk-mini-a113x.dtb` | `35284a73514d35fdf199a8694e417c7af22457feaebf3f3d55d1893e5ee5f7f5` |
| Alpine `fw_alpine/Image` (6.18.44) | `5b906997a6594dd1452e8ca3edbdacc27c2ba1615d81a36866ff1908b0c7ba0a` |
| Alpine kernel `config_alpine` | `7f4b544a97ab7961971ed188c0b9032a22aefa7ee174c414f793577b0e4dc1cc` |

From the device trees: the stock root model is `Amlogic` (compatible `amlogic,axg`) and the Alpine
root is `vk,capsula-mini` / `amlogic,a113d` / `amlogic,meson-axg`, model “VK Capsula Mini (Amlogic
A113X)”; the Alpine memory node reports 256 MiB. Stock: `/ws2812b@00` (`compatible = "ws2812b"`,
`device-name = "topleds"`), two clock controllers `issi,is31fl3236` (0x3c) and `awinic,aw21036`
(0x34, `device-name = "is31fl32xx"`), input `csk05t` (0x15), `rohm,bh1730fvc` (0x29), watchdog
`amlogic,meson-wdt`. Alpine: SPI `topleds@0` (`rohm,dh2228fv`, no vendor char device), the same two
clock controllers with 30 per-segment labels `clock:dN:a..g` plus `clock:colon:top|bottom`, input
`hynitron,csk05t` (0x15), `rohm,bh1730fvc`, watchdog `amlogic,meson-gxbb-wdt`, and audio
`amlogic,axg-sound-card` with `neofidelity,ntp8910a`/`ntp8918` or `acmesemi,acm8615` codecs.
Alpine `config_alpine`: `CONFIG_LED_IS31FL32XX`/`CONFIG_LEDS_AW21036`/`CONFIG_BH1730`/
`CONFIG_LINEDISP` built in, `CONFIG_WATCHDOG_SYSFS=y`, `CONFIG_WATCHDOG_NOWAYOUT` **not set**,
`CONFIG_WATCHDOG_CORE=m`.

Third-party sources for the facts the images do not settle:

- cmp18, "VK Капсула Мини (на Amlogic A113X), часть 1" (debianlive.ru, 2026-08-23): board photos,
  UART console, the part list, and an Alpine 3.24.1 image on a mainline 6.18 kernel;
- `ryysel/capsula-mini-airplay2`: the recovered `/dev/topleds` ABI and the 36-channel map it says
  the stock `voice-agent` uses on this board;
- `ryysel/marusya-hw`: Alpine device paths (`/dev/topleds`, `/usr/local/bin/ringled`,
  `/usr/local/bin/clockdisp`, `/usr/local/bin/capsule-ipc`, the BH1730 IIO attribute);
- `byrbyrka/capsula-mini-tweaker`: the line-display sysfs interface (`message`, `map_seg7`,
  `brightness`, `clock:colon:{top,bottom}`) and the ALSA mixer usage.

No Amlogic unit was probed for this change, so every subsystem below reports what the code can
prove locally. A `PARTIAL` here means "the interface exists and is driven, one hardware check
pending", not "measured".

| Fact | Value | Source |
|---|---|---|
| SoC | Amlogic A113X, 4x Cortex-A53, ARMv8, aarch64 | survey |
| RAM | 256 MB DDR3 (ESMT M15T2G16128A-DEB) | survey |
| Storage | 256 MB NAND (Kioxia TC58NVG1S3HTA00), TSOP48 ONFI | survey |
| Wi-Fi/BT | Fn-Link K255B-SR SDIO module (Amlogic W155S1), Wi-Fi AC + BT 5.0 | survey |
| Amplifier | NeoFidelity NTP8910A, I2C/I2S class-D, 5 W | survey |
| Microphones | four PDM microphones wired to the SoC | survey |
| Light sensor | ROHM BH1730FVC, IIO `in_illuminance_*` | survey + marusya-hw |
| Ring | 12-pixel WS2812B on a 36-channel chain, `/dev/topleds` on stock | airplay2 |
| Clock panel | 4 digits x 7 segments + 2 colon dots; ISSI IS31FL3236A **or** Awinic AW21036 depending on the batch | survey |
| Stock userland | Buildroot, **glibc 2.28** (`/lib/libc-2.28.so` in the supplied system image), PulseAudio, `/rom` + `/data` overlay (`S02datamount.sh`, `S04overlaymount.sh`); init also runs `S59disable_led_animation.sh`, `S50pulseaudio`, `S97voice-agent.sh`, `S99sysmon.sh`, `S99btctl.sh` | supplied image |
| Stock audio | `/etc/pulse/device.pa`: sink `master` (default; also `alarm`, `call`, `ring`, `output`, `alsa_playback`, `rtp`, `snapcast`), source `speex` (default; also `mics`, `mics_loopback`, `call_src`, `input`, `loopback`, `uroc`); `/usr/lib/libopus.so.0.8.0` | supplied image |
| Alpine userland | Alpine 3.24.1, **musl 1.2.6-r2**, kernel 6.18.63-vk-mini-a113x, OpenRC, ALSA + BlueALSA, `iwd` and `wpa_supplicant` installed; helpers `/usr/local/bin/{ringled,clockdisp,lightmon,touch-keys,bt-pair}`; `/usr/lib/libopus.so.0` → 0.11.1 (opus 1.6.1) | supplied image |

### Displays

- **Alpine ring.** The Alpine DTB exposes the chain as `spi@13000/topleds@0`
  (`rohm,dh2228fv`), and the image's `ringled` drives **`/dev/spidev0.0` at 3,340,000 Hz**
  (`--speed HZ` default; it also rebinds `meson-spicc` and can use PIO/8-bit words). There is no
  `/dev/topleds` on Alpine. The backend therefore has a spidev WS2812B encoder (four SPI bits per
  symbol, GRB, reset gap) selected by `profile.LED.SPIDevPath/SPISpeedHz/SPIBitsPerWord`; it does
  not rebind the SPI controller, so a bus held by the kernel is reported rather than forced. Pixel
  order and GRB mapping are unverified on hardware.
- **Stock ring.** Stock's DTB confirms `/ws2812b@00` (`device-name = "topleds"`); it exposes
  `/dev/topleds` with the same vendor ABI recovered for the RK3308
  (`LEDS_WS2812B_IOCTL_MULTI_SET`, 36 channels) and the same channel permutation; the backend
  drives it through `internal/backend/linuxdev`, which validates the driver-reported channel count
  before the first frame. When the char device is absent (or unopenable) the backend falls back to
  LED-class `multi_intensity` pixels, and only then to an in-memory `UNVERIFIED` frame path.
- **Clock panel.** The stock image exposes the IS31FL3236A as the LED class array `clock-led1..30`,
  and the MicroPython hardware layer on this board documents both paths: frames are written with one
  private ioctl to `/dev/is31fl32xx`, while the 30 values are read from the LED class array
  (`micro_hal/hal_display.py`: `COUNT = 30`, `DEVICE = /dev/is31fl32xx`, `SYSFS_DIR` = the LED class
  root, `STOCK_MAX = 153`). The backend drives the array through `brightness` and applies the
  **channel table published by that same source** (`STOCK_MAP`/`LAYOUT`: cell-major a..g, colon
  top = channel index 16, bottom = 15). That published table is byte-identical to the RK3308
  IS31FL3236 table, which was calibrated on hardware with `cmd/ledmap`; the Amlogic copy is
  third-party evidence until the same walk is run on a unit.
  The private ioctl is still unrecovered, so the daemon does not use `/dev/is31fl32xx`; when the LED
  class array is absent it publishes **no segment resource** and reports the char device
  `UNVERIFIED` — an app can never route a frame into a no-op. Alpine instead exposes either the
  kernel line display (`linedisp.*/message` + `map_seg7`, each segment mask reverse-looked-up in
  the device's own 128-byte glyph table; a panel-level `brightness` attribute is probed in the node
  dir and then the parent, and when neither exists brightness is applied as on/off with 0 blanking
  the panel) or per-segment LED class devices
  (`clock:dN:a`..`g` plus `clock:colon:top|bottom`), both probed. The panel can be an IS31FL3236A or
  an AW21036. On the supplied stock DTB both controllers are `status = okay`; only the AW21036 node
  carries `device-name = "is31fl32xx"`, so that char device name is the AW21036 variant, while both
  expose the same 30 `clock-ledN` LED class channels the stock backend drives. On Alpine both
  controllers are `status = fail-needs-probe` and each carries 30 labeled children
  (`clock:d1:a` … `clock:d4:g`, `clock:colon:top|bottom`), matching the per-segment pattern in the
  profile.
  Stock content brightness is capped at 153/255 by the vendor UI; the framework writes the frame's
  resolved level instead, so a stock-parity ceiling is a calibration choice, not a hidden limit.

### Audio

- **Stock** keeps PulseAudio. The image's `/etc/pulse/device.pa` names `master` as the mixed output and `speex` as the processed capture source, so the profile uses those instead of the PulseAudio defaults.

- **Alpine** has no PulseAudio: the backend runs `aplay`/`arecord` on the ALSA PCM named by the
  profile (`default`). The image's `asound.conf` makes `default` asym: playback `plug:mastervol`
  (softvol, "Master Playback Volume", card 0) and capture `plug:micboost` (hw:0,1, "Mic Boost
  Capture Volume"). The profile pins `Master Playback Volume` at unity (`MixerCard`/`MixerVolume`)
  so the framework mixer is the only gain; capture conversion from the native four channels to the
  requested mono is the image's own `plug` mapping, not a channel choice made by the daemon.
  No simple-mixer control name is published, so the profile names none: the framework mixer is the
  only gain stage and no hardware gain is pinned until a control is measured.
- Microphone mute is the capture-source mute (stock) or the configured mixer mute (Alpine). No
  microphone gate GPIO is documented for this board, so the profile leaves `MicEnableGPIO` at zero
  and `SetHardwareMute` reports hardware muting only when a gate actually exists.

### Input and sensors

Input is a capacitive panel (marusya-hw names the controller "Hynitron CSK05T") read through
evdev, not a gpio-keys node: `KEY_VOLUMEUP` 115, `KEY_VOLUMEDOWN` 114, `KEY_MICMUTE` 248 and
`KEY_VOICECOMMAND` 582 (primary source: `marusya-hw/internal/backend/capsula/evdev.go`). On this
board 582 is the assistant/action key, whereas the RK3308 panel uses the same code for its neutral
centre, so the distinction is board data (`profile.Input.KeyOverrides`), not a code branch. The
backend reports coordinates only if the node advertises absolute axes; nothing claims a coordinate
surface for it. The BH1730FVC and the SoC thermal zones are surfaced exactly as on the RK3308
(`sensor.ambient_light`, `sensor.soc_temperature`, …).

### Brightness

The stock Amlogic ambient-brightness curve has **not been measured**. The Amlogic profiles use the
framework's own policy curve (the same numbers the simulator uses) so automatic brightness has a
defined response; the profile makes no claim of matching the vendor agent, and the auto floor,
deadband and animation constants are framework policy, not device facts.

### Watchdog

The stock board has a hardware watchdog, and `voice-agent` holds it: stopping that process can reset
the speaker. The profile therefore names `/dev/watchdog`, and `Watchdog.Nowayout` stays **false**
because disarm semantics (magic close, `WDIOC_SETOPTIONS`) are unmeasured on this kernel — it records
only measured facts, and RK3308 is the only board where `true` is known. Safety does not depend on
that field: the supervisor never trusts a `Close` for an armed timer, it `Detach`s the descriptor to
a feeder on a graceful stop whenever the platform exposes one, so a planned stop never depends on
guessing the driver: the feeder keeps the board up for its bounded window (5 min) and, if no daemon
returns within it, the armed timer expires and the board resets by design. Alpine names `/dev/watchdog` (30 s default, `amlogic.syswatchdog`) from its DTB node
(`amlogic,meson-gxbb-wdt`), `CONFIG_MESON_GXBB_WATCHDOG=m`, `WATCHDOG_SYSFS=y`, `WATCHDOG_NOWAYOUT`
unset and the module in its rootfs; the OpenRC service loads it and the supervisor retries and
reports a missing node, so `-watchdog auto` arms when the module loads.

### Deployment and boot

- **Stock.** `deploy/amlogic-stock/S99sysmon.sh`, installed as `/etc/init.d/S99sysmon.sh`. The
  stock list is BusyBox `rcS`, which expands `/etc/init.d/S??*` **before** `/data` is mounted and
  before the overlay pivots, so a new overlay-only name never runs. Replacing an already-expanded
  name does, because the name is in the list and the file resolved at run time comes from the
  overlay. `S99sysmon.sh` is the vendor supervisor (`sysmon` → `voice-agent`) that this framework
  replaces, so it is the name the hook wraps; `S99btctl.sh` keeps `btctl` untouched. `disable`
  writes the stock `S99sysmon.sh` back verbatim and starts `sysmon` again; `enable` restores the
  saved wrapper (`boot/S99sysmon.sh.wrapper`); the embedded rollback copy matches the extracted
  vendor script byte-for-byte (354 bytes, verified offline). Both are idempotent, and the daemon's
  `-supersede auto` still stops `voice-agent`/`sysmon` at runtime and reaps a respawned copy.
  Data directory: `/data/local/capsulad`.
- **Alpine.** `deploy/amlogic-alpine/capsulad` is an OpenRC service installed as
  `/etc/init.d/capsulad`; enable it with `rc-update add capsulad default`. Data directory:
  `/var/lib/capsulad`.
- `make deploy-bundle DEPLOY_PLATFORM=amlogic-stock` (or `amlogic-alpine`) copies the matching hook
  into `boot/`; pass the same value to `capsulad-cli deploy -platform`. The bundle layout, limits and
  rollback path are identical on every target; only the boot service, hook name and free-space
  floors come from the profile.

### Bring-up tooling

`capsulad-cli platform ledtest` and `cmd/ledmap` are RK3308 tools: they open the vendor
`/dev/led_multi_ctrl`/`/dev/topleds` nodes with `profile.RK3308Rev4Stock` hard-coded, so they do
not drive an Amlogic panel and cannot perform the `clock-led1..30` walk. Until an Amlogic/sysfs-LED
walk target exists, that check is a manual one and the panel map stays third-party evidence.

### Build and ABI

Both Amlogic userlands are aarch64. The RK3308 stock image is glibc 2.33; the supplied Amlogic stock image is glibc 2.28, so package ABIs must contain
`linux` (the supplied stock image ships **glibc 2.28**, so native packages must not require newer
symbols; the RK3308 image ships glibc 2.33); Alpine is musl 1.2.6, so its profile accepts only ABIs containing `musl`. A glibc-linked native
app package is rejected on Alpine by the existing architecture gate, not by a second rule.
`make daemon GOOS=linux GOARCH=arm64` cross-builds both binaries. The native SDK needs its own Rust
target: `aarch64-unknown-linux-gnu` for stock, `aarch64-unknown-linux-musl` for Alpine. `make
deploy-bundle DEPLOY_PLATFORM=amlogic-alpine` defaults `NATIVE_TARGET` to the musl triple and refuses
to assemble a bundle whose `lib/libcapsulad.so` does not look musl-linked (the reverse for
`amlogic-stock`), so a glibc runtime cannot be installed on Alpine by accident. **Proof level:** the musl shared runtime was built on the host (arm64 Alpine container) with
`-C target-feature=-crt-static`; its ELF `DT_NEEDED` names `libc.musl-aarch64.so.1`, and an Alpine
`dlopen`/`dlsym("capsulad_abi_major")` smoke resolved the symbol. No device loaded it.

For **Amlogic stock** the glibc runtime must be linked against the image's glibc 2.28. A
bullseye-era build needs `GLIBC_2.29`/`2.30` and the device loader rejects it
(`libm.so.6: version GLIBC_2.29 not found`), even though app packages built for 2.28 load. Build it
with `tools/build-native-glibc228.sh` (Debian buster, archived repos, verifies the symbol ceiling and
writes `build/lib` plus metadata). The deploy gate independently refuses a runtime above
`Architecture.MaxGLIBC` (2.28 here, 2.33 on RK3308), so a 2.33 build cannot be installed on this
board by accident. Verified build: Rust 1.100 in buster produced 6,313,560 B,
`DT_NEEDED` = `libgcc_s.so.1`, `libpthread.so.0`, `libm.so.6`, `libdl.so.2`, `libc.so.6`, and the
highest symbol version is **GLIBC_2.28** (sha256 `620297dd…c895354f4a3db9`); the same binary loads
on a buster (2.28) image and the app reaches its first framework call (`connect failed (7)` for a
nonexistent socket). The independently built app package also maxes at 2.28, so runtime and package
are coherent on the stock rootfs. A superseded 2.33-era build (`7b6c7a6c…`, max `GLIBC_2.30`) is
rejected by the loader at `libm.so.6: version GLIBC_2.29 not found` and by the deploy gate with the
same reason.

## Open hardware gates

1. **Ring gamma.** Channel maps are verified on the unit; the 2.2 ring gamma is not
   photometer-verified.
2. **Audio under load.** Latency and underrun behaviour under sustained load, and pre-roll timing,
   are unmeasured; routing, ducking and indication termination are verified digitally.
3. **Touch contract.** Confirm on the unit, with physical presses and the deployed resolver, that a
   bare volume tap steps exactly once with no added latency, a volume edge pressed while the centre
   is still held (or within 700 ms of its release) resolves `swipe_right`/`swipe_left` while the
   centre keeps its own action, the
   opposite→centre→direction chain resolves the gesture after its leading edge has already stepped,
   a full-panel crossing (`volume_down`→`volume_up` or the reverse, no centre, ≤ 400 ms
   press-to-press) resolves `swipe_right`/`swipe_left`, suppresses the completing step and reverts
   the leading one, a slow opposite pair stays two steps, a held circle summons at 700 ms, a single
   end-stop key stays a volume step, and an unfinished
   sequence produces no further effect — with windows following the kernel timestamp even when the
   read loop is delayed. The centre→edge form and the volume timing are now device-confirmed (a
   79 ms centre→`volume_up` resolved `swipe_right` and sent `BLUETOOTH_MEDIA_NEXT`; a 114 ms
   centre→`volume_down` sent `BLUETOOTH_MEDIA_PREVIOUS`). Still open on the unit: the full-capture
   circle traversal, the 700 ms hold summon, and whether any deliberate action other than a
   centre tap or a whole-panel crossing can carry a gesture — the pad reports one key at a time and
   a lone end-stop edge carries no gesture evidence. Raw coordinate semantics need a different panel.
4. **Power cycle and pairing.** The boot hook, health gate and vendor fallback are exercised by
   service restarts, not by a power cycle. Pair and unpair a throwaway device through the Web UI
   (unpair is deliberately not run against the user's MacBook), exercise a source that publishes
   AVRCP track metadata, and decide the boundary for the vendor helpers that keep running (`okcall`,
   `stereoctl`, `telemetryd`, `snapclient`).
5. **Native A2DP ordering.** Confirm the sink is advertised before the adapter is connectable after a
   cold boot and after a BlueZ restart or power cycle, that a failed registration keeps
   pairable/discoverable off with the documented `Detail`, and that a source connected in the
   pre-daemon autopower window either recovers A2DP on the same link or needs a reconnect. Confirm a
   strict peer accepts the conformant four-octet SBC capability IE, and record the
   "audio link is not A2DP" Warn together with the one A2DP profile request per source and its
   outcome, confirming the source stays connected throughout. PA-module removal timing around an
   active source, and the retirement of an idle module set while a paused source is still connected,
   are real-device gates.
6. **mDNS host name.** Save a label, confirm `.local` resolution with `avahi-resolve`, restart Avahi
   through the device init system and confirm the label is re-adopted, and check that a colliding or
   invalid name keeps the prior name and reports the collision. With the label active, a Settings GET
   and a daemon restart must produce zero `SetHostName` calls (`dbus-monitor --system
   "interface=org.freedesktop.Avahi.Server,member=SetHostName"`) and no warning.
8. **Amlogic ring and panel.** On a unit, confirm `/dev/topleds` reports 36 channels and that the
   recovered permutation renders the ring clockwise from the top on this board too; confirm the
   `clock-led1..30` array accepts `brightness` writes (the documented read path) and that the
   published table lights the expected segments, including the colon; confirm which panel is fitted
   (IS31FL3236A or AW21036) and, on Alpine, whether the line display, the per-segment LED class
   devices or both exist. Only if no LED class array exists on a stock unit does recovering the
   private `/dev/is31fl32xx` ioctl become the gate.
9. **Amlogic audio and watchdog.** On Alpine, confirm `Master Playback Volume` (the `mastervol`
   softvol) exists only after the first playback stream has opened `default`: the first `amixer
   sset` may fail, the pin retries on the next `SetMasterVolume`, and until it lands the framework
   volume is multiplied by the image's softvol — verify the pin lands and the level matches before
   trusting volume. Measure capture latency on the PDM array. On stock, measure whether
   `/dev/watchdog` can be disarmed (magic close, `WDIOC_SETOPTIONS`) and record it in
   `Watchdog.Nowayout`. The flag records a measured fact only and does not gate the handoff: a
   transferable descriptor is always handed to a feeder, and `Close` is used only when the
   platform exposes none, where the flag chooses the wording of that release path.
10. **Amlogic stock autostart.** Confirm whether `/etc/init.d/S??*` really is expanded before the
   overlay mounts on firmware 44.90 (the AirPlay 2 package replaces `S50avahi-daemon` for exactly
   this reason), and choose the supported autostart mechanism: base-image hook, or a stock script
   that starts it. Service restarts alone do not satisfy this gate.
11. **Amlogic brightness.** Measure the stock ambient-brightness response before claiming automatic
   brightness parity; today the Amlogic profiles use the framework policy curve deliberately.


