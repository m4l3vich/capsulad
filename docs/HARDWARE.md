# Rockchip stock firmware findings

Source: `_capsula-mini-rockchip-backup/` (full backup of a jailbroken Capsula Mini, serial
`03104B1D40E15C55`, backup `03104B1D40E15C55`). Partition table:

| # | name | start (KiB) | size (KiB) |
|---|---|---|---|
| 1 | uboot | 8192 | 8192 |
| 2 | trust | 16384 | 8192 |
| 3 | misc | 24576 | 2048 |
| 4 | boot_a | 26624 | 32976 |
| 5 | boot_b | 59600 | 32976 |
| 6 | system_a | 92576 | 88064 |
| 7 | system_b | 180640 | 88064 |
| 8 | vbmeta_a | 268704 | 2048 |
| 9 | vbmeta_b | 270752 | 2048 |
| 10 | security | 272800 | 8192 |
| 11 | userdata | 280992 | 166463 |

## Platform

- **SoC:** Rockchip RK3308 (aarch64). Device tree model: `RK3308-based Capsula-mini`.
- **Kernel:** Linux 4.4.167, SMP PREEMPT, built 2025-03-03 with Linaro GCC 6.3.1.
- **Userland libc:** glibc 2.33 with `/lib/ld-linux-aarch64.so.1`; native SDK and app binaries must
  not require newer `GLIBC_*` symbols. This image is not musl.
- **Root filesystem:** Buildroot 2022.11.1 SquashFS 4.0 (xz), mounted read-only, with an overlay
  (`S23overlayfs.sh`) whose upper/work directories live on the ext2 `userdata` partition.
- **Boot:** Android boot image with kernel + ramdisk + DTB (`second`), page size 2048.

Reproduce the inspection:

```sh
7z x _capsula-mini-rockchip-backup/06_system_a.img -o/tmp/capsula-root
./build/bin/capsula platform inspect -root /tmp/capsula-root
```

## Hardware interfaces relevant to the framework

### Displays and LED ring

The boot image carries a Rockchip device tree (`ANDROID!` second stage, page size 2048, FDT magic
at `second+2048`). Two LED controllers are described there:

| Device tree node | Compatible | Wiring | Userspace |
|---|---|---|---|
| `/spi@ff120000/ws2812b@00` | `ws2812b`, `device-name = "topleds"` | 36 LED children, SPI max 2.5 MHz, CPOL=1 | `/dev/topleds` |
| `/i2c@ff060000/led-controller@3c` | `issi,is31fl3236` | 30 `clock-ledN` children at i2c 0x3c | `/dev/led_multi_ctrl` |

Role assignment is confirmed by the stock `voice-agent` payload:

| Evidence | Meaning |
|---|---|
| `/dev/topleds` + `LEDS_WS2812B_IOCTL_MULTI_SET` | 36-channel WS2812B chain → 12-pixel RGB ring |
| `/dev/led_multi_ctrl` + `LEDS_MULTI_CTRL_IOCTL_MULTI_SET` | 30-channel IS31FL3236 → 4×7-segment plus two-colon display |
| `usr/share/voice-agent/led/*_diodes.json` | Frames with 12 `colors` entries → 12 RGB ring pixels |
| `usr/share/voice-agent/led/*_digits.json` | Frames with 30 `brightness` entries → 30 monochrome display LEDs |
| `S59disable_led_animation.sh` | Boot disables all `/sys/class/leds/*` triggers and brightness |

The vendor ioctl ABI is not published, but it is recoverable from the stripped `voice-agent`
(`/source/src/hw/capsula_mini_leds.cpp`, with the node wrapper in `led_multi_ctrl.cpp`). The
recovered interface is:

| Interface | Value | Argument |
|---|---|---|
| `LEDS_MULTI_CTRL_IOCTL_GET_COUNT` (both devices) | `_IOR('z', 2, int)` = `0x80047a02` | `int *`; returns 36 for `/dev/topleds`, 30 for `/dev/led_multi_ctrl` |
| `LEDS_WS2812B_IOCTL_MULTI_SET` | `_IOW('z', 1, 16)` = `0x40107a01` | `{u32 start, u32 count, u8 *data}`; ring sends 36 channel bytes |
| `LEDS_MULTI_CTRL_IOCTL_MULTI_SET` | `_IOW('z', 1, 8)` = `0x40087a01` | pointer to 30 records of 20 bytes: `{u32 enable=1, u32, u32 brightness, u32, u32}` (brightness at offset 8) |

The stock app also hard-codes two permutations, both recovered from the binary:

- ring channel map (vendor frame R,G,B per pixel → physical WS2812B channel):
  `1,0,2, 34,33,35, 31,30,32, 28,27,29, 25,24,26, 22,21,23, 19,18,20, 16,15,17, 13,12,14, 10,9,11, 7,6,8, 4,3,5`.
  Besides reversing the ring order it swaps R and G within each pixel, which matches the
  WS2812B GRB wire order. The backend applies this map to `RingFrame` pixels.
- display map (vendor 30-value JSON index → physical record):
  `1,28,21,17,22,29,24,26,25,19,18,23,27,20,16,15,0,2,7,5,6,4,3,10,12,11,14,13,8,9`.
  Its values partition into four groups of seven (`0..6`, `7..13`, `14..20`, `21..27`) plus
  records `28,29` for the colon. That identifies the record order as digit-major, so the backend
  writes `SegmentFrame` cells directly to records `cell*7 + segment` and the colon flags to
  `28,29`; the vendor JSON permutation is not applied twice.

The framework drives these nodes directly (`internal/backend/rockchip/led_vendor_linux.go`) and
validates the driver-reported channel count against the device tree. When the vendor nodes are
missing or cannot be opened it falls back to standard Linux LED class devices (`/sys/class/leds/*`,
`multi_intensity` or `brightness`), and only then reports `BACKEND_UNVERIFIED` while keeping
rendered frames in memory. The sysfs fallback claims each selected LED by writing `none` to its
`trigger` attribute, mirroring `S59disable_led_animation.sh`; otherwise the device-tree default
`timer` trigger would ignore brightness writes.

The 30-LED display is still treated as a 4-digit × 7-segment layout plus two colon LEDs; the
segment-bit order and glyph geometry must be verified on hardware before claiming face parity.
Frame writes and the ioctl calls themselves are also pending hardware validation.

### Audio

`/etc/pulse/device.pa` configures a system PulseAudio instance:

- playback: ALSA sink `playback` plus virtual sinks `alarm`, `ring`, `call`, `purr`, `master`
  (`master` is default) and a `snapcast` FIFO;
- capture: `speex` (default, processed), `call_src`, `mics` (4 ch), `mics_loopback` (5 ch),
  `loopback`;
- `S49stereo-softvol.sh` primes the stereo playback path; `S51volume_up` sets DAC line-out gain.

The Rockchip backend uses the firmware's `pacat`/`parec` clients: playback to `master` at 48 kHz
stereo and capture from `speex` at 16 kHz mono S16LE. Latency, format negotiation and coexistence
with vendor daemons require device testing; the subsystem is reported `PARTIAL`.

Applications capture `audio.input.microphone` through a bounded framework DSP chain that runs only
for the production S16LE mono 16 kHz `speex` format: 80 Hz high-pass, adaptive spectral suppression
capped at 12 dB, unity gain (no AGC) and a −1 dBFS peak limiter, with a 256-sample frame / 128 hop
(about 15.9 ms) STFT. The noise estimate follows aggregate frame energy against a fast-down/slow-up
floor and a very-slow-up average, so colored steady room noise is learned; spectral flatness no
longer gates learning. State and output length are fixed, chunk boundaries do not change output,
and the chain resets on mute, source reopen or a capture gap. Wakeword inference, enrollment and
training consume this same processed tap (`framework.dsp.v1`); the untouched physical `speex`
samples are available only at the explicit `audio.input.microphone.raw` endpoint, and frames whose
tap declaration does not match are rejected. A general cold-start guard keeps a voice or word at
0 dB relative to the learned reference out of the estimate; the first-vowel gate is ≥0.85 of
reference (worst 0.896), and room re-engagement plus a second speech segment are preserved. On the
one authorized 10 s background recording — mostly music played through a Mac's own or external
speakers, not the Capsula path — cleanup measured 2.9 dB overall (80–300 Hz: 2.7 dB). That
recording is unannotated and has no local playback reference to cancel the music, so it is consumed
background evidence, not fan/traffic, music-immunity, new-location or zero-FAR proof. Endpoint
negotiation with a real `speex` source, subjective suppression benefit and transparency remain
hardware/listening gates.

### Microphone gate and mute

`S50mic-enable` exports GPIO 21 (GPIO0_C5) as an output and drives it high at boot. The backend
reads `MIC_EN_GPIO` from that script (falling back to the compiled default) and writes `0`/`1` to
`/sys/class/gpio/gpioN/value` on mute/unmute, also asking PulseAudio to mute the `speex` source.
Hardware mute is reported only when the GPIO path exists.

### Touch and keys

Input is standard evdev under `/dev/input`. There is no vendor input protocol needed for basic
operation: the backend reads `struct input_event` and maps keys using the live `gpio-keys` node.
The stock device tree declares five keys:

| Device-tree node | `linux,code` | Framework key |
|---|---|---|
| `micmute-key` | `0xf8` `KEY_MICMUTE` | `mic_toggle` |
| `volumeup-key` | `0x73` `KEY_VOLUMEUP` | `volume_up` |
| `playpause-key` | `0xa4` `KEY_PLAYPAUSE` | `play_pause` |
| `volumedown-key` | `0x72` `KEY_VOLUMEDOWN` | `volume_down` |
| `voice-key` | `0x2c0` `BTN_TRIGGER_HAPPY1` in one DTB, `0x246` `KEY_VOICECOMMAND` in the other | `center` |

The two DTBs embedded in the boot partition disagree on the voice-key code and label, so the
backend derives the semantic name from each child's node name and label and merges those entries
over a default map. `BTN_0`, `KEY_ENTER` and `KEY_OK` still map to `primary`. The `voice-key` name
is a vendor label, not an assistant function: both codes are the same **unmarked neutral centre**
in the physical five-key layout. The roles are microphone mute (north), volume up (east),
circle/play-pause talk key (south), volume down (west) and neutral centre. The centre dismisses
alerts/assistant turns and drives stopwatch gestures but never summons the assistant; the talk key
is `play_pause`.

There is no coordinate panel. `touch.panel.main` advertises `coordinates_available=false` and
`full_raw_available=false`, but the backend advertises `full_panel_available=true`: all five keys
and the gestures inferred from ordered presses are deliverable. Partial capture receives
circle/centre key events and swipes/circles; full capture receives every key and gesture with all
system effects suppressed. The user's map is authoritative — east is volume up and west is volume
down — and direction is not inverted from a single passive trace. A full swipe is
opposite → centre → direction, or the minimum two-contact opposite-edge pair without the centre
(W→E right, E→W left), where the first contact must still be down when the second is pressed; a
sequential opposite tap pair or circle-then-microphone remains deliberate key actions, and the
vertical two-contact pair is full-capture only. A centre-first half swipe is accepted when
unambiguous, and a closed perimeter traversal from any corner is `circle_cw`/`circle_ccw`. Gesture
windows and the 700 ms hold use the kernel event timestamp converted to the daemon clock; only an
invalid event older than 5 s falls back to receive time. A crossing that reports zero or
one contact cannot be distinguished from a key press or no input and is not inferred as a gesture.
A failed multi-contact sequence suppresses its prefix effects, while a standalone volume tap or
held ramp still works; raw coordinates are never fabricated.

### Vendor daemons (superseded at startup)

`voice-agent` (13 MB), `stereoctl`, `okcall`, `telemetryd`, `sysmon`, `btctl`, `snapclient`,
`snapserver`, `avahi-daemon`, `dnsmasq`, `crond`, `pulseaudio`. Local vendor IPC observed:
`/tmp/stereoctl.sock`, `/tmp/okcall.sock`, `/tmp/telemetry.sock`, `/tmp/btctl.socket`, plus
`/tmp/voice-assets` and `/tmp/tgclient` (LZMA-decompressed at boot).

The stock chain is `sysmon` (supervisor) → `voice-agent -mode=master` → `voice-agent -mode=app
-watchdog`. Measured conflicts with the framework: the app process holds `/dev/input/event0` (the
play/pause key the framework also reads) and `/dev/watchdog`; `btctl` and `tgclient` inherit the
watchdog descriptor. Two owners of the key and three of the timer are not a workable arrangement.

`capsulad -supersede=auto` (the default on `*-stock` platforms) therefore stops `sysmon` and
`voice-agent`, escalates to SIGKILL when SIGTERM is ignored, then stops every remaining
`/dev/watchdog` holder, and finally arms the timer itself. Observed on the unit:

```
INFO daemon    superseding voice-agent pid=418 (SIGTERM)
INFO daemon    superseding sysmon pid=427 (SIGTERM)
INFO daemon    superseding voice-agent pid=437 (SIGTERM)
INFO watchdog  /dev/watchdog armed, timeout=21s
```

`-supersede=off` leaves the vendor stack alone; an explicit comma list replaces the default set. A
reaper stops any superseded process that respawns. The takeover was **runtime only** during
bring-up; boot integration is now one removable hook:

- **The only capsulad boot file is `/etc/init.d/S99btctl.sh`** (repo:
  `deploy/rockchip-stock/S99btctl.sh`). `rcS` expands `/etc/init.d/S??*` once, before
  `S23overlayfs.sh` pivots into the `/data` overlay, so the file must keep the pre-existing S99
  name; there is no separate `S60capsulad`. The hook performs every capsulad-specific action:
  boot-log cap, data/run directory preparation, Bluetooth UART IRQ affinity, retirement of
  `S97voice-agent.sh`/`S99sysmon.sh` while the daemon owns the hardware (pristine copies are kept
  in `/data/capsdemo/vendor-init/stock`), daemon start after `S02mount-data`, `S50mic-enable` and
  `S50pulseaudio`, the `capsulad -health` gate, and restoration plus startup of the preserved
  vendor chain (`/data/capsdemo/vendor-init/S97voice-agent.sh`) when the daemon cannot become
  healthy.
- **Disable and restore.** `/etc/init.d/S99btctl.sh disable` stops the daemon, restores the vendor
  init scripts, starts the vendor assistant, writes `/data/capsdemo/boot/disabled` and moves the
  hook to `/data/capsdemo/boot/S99btctl.sh.disabled`. Run that copy with `enable` to remove the
  marker and reinstate the hook. Removing the file alone is only safe after `disable` has restored
  the vendor scripts; a firmware-provided pristine S99btctl.sh can be copied back if desired.
- Historical evidence: the previous two-file deployment (`S60capsulad` plus an `exec` wrapper) was
  verified across two power cycles on serial `03104B1D40E15C55`: `boot.log` records
  `starting /data/capsdemo/capsulad` and `daemon healthy`, `capsulad -health` exits 0, the
  supervised clock/assistant/media packages reported `APP_READY`, and
  `/dev/watchdog armed, timeout=21s` appears. Current builds start the clock and ring as
  compiled-in system apps and no longer ship those sample packages. The 2026-11-12 redeploy
  (`capsulad` sha256 `86d131ae…fe604`) made the old `S60capsulad` restart healthy, `capsulad
  -health` exit 0, watchdog 21 s; the shared native SDK (`libcapsula.so.1`, sha256
  `b8632b17…72b50d`, ABI 1.0, 35 capabilities) and the optional `org.capsula.dumbassistant`
  (sha256 `993f58ae…45686`) were installed at that time. The single-hook rewrite and the
  public-contract clock/ring build were subsequently deployed through that hook on 2026-11-14
  (daemon `a847c805…`, then the clock/ring binary-wire correction as `fce5c506…`; details and
  limits in [STATUS.md](STATUS.md)); normal service restarts and the health gate are verified,
  while no power-cycle, vendor-fallback or physical-effect verification exists for them yet.
- A factory reset wipes the overlay: the device then boots the stock assistant and the deployment
  must be repeated.

In-place upgrades go through the same service:

```sh
# on the device, with the new binary staged outside /data (tmpfs has 117 MB)
capsula -data-dir /data/capsdemo daemon install -binary /tmp/capsulad.new
capsula -data-dir /data/capsdemo daemon rollback     # restores the previous binary only
```

`daemon install` runs `<binary> -version`, stages beside the target, renames the live binary to
`capsulad.prev`, activates by rename, restarts through `/etc/init.d/S99btctl.sh`, and requires the
framework API to answer before it reports success; any failure puts the previous binary back. The `.prev` copy is 22,610,180 bytes on `/data` after the 2026-11-12T12:26Z install, so delete it only when the
rollback copy is no longer wanted. `daemon rollback` restores the binary only: a full return to the
old app-based clock/ring/assistant also needs the old bundles and manifests from the host backup
(`_capsula-mini-deploy-backup/20261112T181842/`), because the old daemon's strict manifest loader
does not understand `[native_sdk]`.

Because `/data` is 78.5 MB, the 24.4 MB negative-training corpus, its sidecar and the older
recordings were moved to the host. After the 2026-11-12 deployment `/data` reported 24.7 MB free
(69 % used) with the `.prev` retained. Restore training input
before a new enrollment:

```sh
adb push _capsula-mini-data-backup/wakeword/negatives.bin \
  /data/capsdemo/wakeword/negatives.bin
adb push _capsula-mini-data-backup/wakeword/negatives.bin.provenance.json.gz \
  /data/capsdemo/wakeword/negatives.bin.provenance.json
adb push _capsula-mini-data-backup/wakeword/samples/. /data/capsdemo/wakeword/samples/
```

The daemon also accepts `-wakeword-negatives PATH` so the corpus can live on `/tmp` or another
mounted path during training instead of `/data`. New takes and training are refused below 2 MiB
free, and the daemon log rotates at 512 KiB plus one backup.

### Bluetooth (deployed)

The framework no longer uses `hciconfig` for anything; it talks to BlueZ 5.65 over the system bus.
The original `hciconfig hci0 piscan` failure (`Can't set scan mode on hci0: Network is down (100)`)
was an adapter that nobody powered after the vendor stack was retired. The daemon sets
`Adapter1.Powered`, `Discoverable` and `Pairable` through D-Bus, and the BlueZ manager repairs its
A2DP registration and power state after a BlueZ restart; it does not poll adapter properties.

The vendor `btctl` was the component that registered A2DP endpoints. Without it the adapter
advertised only AVRCP (0x110C/0x110E) and no Audio Sink, so a source refused to connect with
`br-connection-profile-unavailable`. The current daemon registers its own SBC sink endpoint through
the public `org.bluez.Media1.RegisterEndpoint` API and verifies that the registration is still armed
for the current adapter generation **before** the adapter is made connectable, discoverable or
pairable, and before an API request can enable those. A failed registration, a BlueZ restart or an
adapter power cycle invalidates readiness; it is not reported ready and is repaired before the next
connect.
Discoverability and pairability are refused while the profile is missing, and
`BluetoothStatus.Detail` says the audio profile is not advertised. If native registration is not
possible, the daemon falls back to PulseAudio's standard `module-bluetooth-discover`, which makes
the adapter advertise Audio Sink (0x110B), Headset (0x1108) and Audio Source (0x110A) and exposes
`bluez_source.<ADDR>.a2dp_source`. PA's Bluetooth endpoints are removed only after the native sink
is armed and never while a source is actively routed through them; that source keeps its PA route
until disconnect, and the native owner takes over for the next connection.

BlueZ may autopower the adapter before `capsulad` starts. In that pre-daemon window a paired source
can open an ACL while the framework's sink is not yet advertised. The incoming-connect handler
repairs/verifies the registration and requests A2DP/AVRCP over the already-connected device via
`Device1.ConnectProfile` (no second ACL). Whether a peer that already abandoned A2DP during that
window honors the request without reconnecting is device-dependent and remains a real-peer gate.

The live verification below is retained as dated **PulseAudio-route** evidence from 2026-11-11, when
PA discovery owned the sink. It is not a claim that the native endpoint has passed the same
end-to-end listening/peer test.

Live verification with the paired MacBook (2026-11-11):

- after `Device1.ConnectProfile(0x110A)` and the AVRCP profile, `Device1.Connected=true`,
  `MediaControl1.Connected=true`, and `MediaPlayer1`/`MediaTransport1` objects exist;
- AVRCP Play/Pause/Next/Previous calls returned success and the player state moved to `playing`
  after Play and Next; the Web UI showed the `bluetooth` player as `PLAYBACK_PLAYING`;
- setting the framework volume to 50 % wrote `Volume=64` (of 127) on the idle A2DP transport;
  restoring 80 % wrote `102`;
- the MacBook's `MediaPlayer1` exposes `Device` and `Position` but **no `Status` or `Track`** at
  rest, so title/artist stayed empty. Control still works; metadata is read when a source
  publishes it. macOS only fills those attributes for some players, so this is a source limitation,
  not a parse fallback;
- `Pairable` and `Discoverable` toggled from the Web UI showed up in BlueZ and survived a reboot
  (`bluetooth_pairable` is a typed setting); the paired MacBook is listed in the UI.

### Bluetooth volume and A2DP jitter (PulseAudio fallback path)

These measurements describe the PulseAudio fallback owner and are retained as dated evidence. The
native endpoint uses AVRCP transport volume plus the framework's stream trim instead of a PA
loopback; its audible level has not been certified.

The Mac's AVRCP volume and the Capsula master are kept equal in both directions through one
`applyMasterVolume` path: a master change writes the transport, and a transport change from the
source updates the framework master. Updates are event-driven (BlueZ `PropertiesChanged`); the
earlier implementation's 2 s poll, echo window and coalescing were removed. The Mac is **not**
trusted as the only attenuation. Observed on the unit with the earlier build: with the Capsula
master at 5 % and the Mac output at its minimum, the transport volume read ~6/127 yet the captured
stream was near full scale, and the speaker was ~90–100 % loud. The AVRCP slider had moved without
attenuating the stream.

When PulseAudio is the fallback owner, the loopback carries a local safety gain of master^1.5
(cube-root compensated, because PulseAudio sink-input volume is cubic), while the source is held
at a fixed 90 % trim. A linear master was still too hot at 5–7 % on this amplifier. The local gain
and source trim are applied on source/route changes and volume events, rather than by a periodic
re-assert. The audible Bluetooth level is bounded by master^1.5 and is at most about 0.73 × it;
the Mac's own attenuation, when it does apply, makes it quieter still. This is the only
arrangement that is safe with a source that may ignore AVRCP.

In the fallback path, PulseAudio's `module-bluez5-device` is reloaded with
`avrcp_absolute_volume=0` on every source connect, because PulseAudio recreates it with the
default (1) on a profile reconnect. The default mirrored the transport onto the local source and
applied a cubic second attenuation; at master 30 / Mac 50 the source mirrored 47 % (10.4 % cubic)
and the audio collapsed to ~1.5 %, which sounded like a phone call. No periodic self-heal remains;
the per-connect reload and source/route events cover recreation.

Audio quality was compared directly with the stock stack: the vendor `voice-agent`/`btctl` chain
sounded clearly better on the same source. The vendor registers its own A2DP endpoint and feeds
decoded PCM to the device; the old capsulad path received the PulseAudio bluez5 SBC source,
resampled it through `module-loopback` into the 48 kHz master sink and then ALSA.

capsulad prefers its own A2DP sink endpoint. It exports an `org.bluez.MediaEndpoint1` SBC endpoint
and registers it with `Media1.RegisterEndpoint`, verifying that it is armed for the current adapter
generation before the adapter is made connectable or pairable; PA's bluez5 endpoints are removed
only after that and never while a source is actively using them. On this BlueZ 5.65 the endpoint
API is `SelectConfiguration(ay) -> ay` and
`SetConfiguration(o transport, a{sv} properties)`; the transport object has no `Endpoint`
property, so the transport path returned by `SetConfiguration` is used (UUID/Codec fallback).
The transport fd is acquired with `MediaTransport1.Acquire` only while the transport is
`pending` or `active`; an `idle` transport is silent (the source is paused) and reading its fd
blocks while BlueZ streams on a real active transport, which was observed as a silent stream and
then `EOF`. RTP is depacketized and decoded in-process by `internal/sbc`, a pure-Go
8-subband SBC decoder ported from the Apache-2.0 Android reference (attribution in
`internal/sbc/NOTICE`). It keeps its synthesis filter history across A2DP packets and writes S16LE
directly to the mixer, so there is no `sbcdec` process, temp file, chunk boundary or warm-up. The
port matches the system `sbcdec` and the Android reference sample-for-sample on the 172-frame
fixture (correlation 1.0, RMS 6936). The selected configuration is 44.1 kHz joint stereo,
16 blocks, 8 subbands, loudness allocation, bitpool 2..53. PA still owns app playback and the
microphones; the Mac's AVRCP volume sync is unchanged. The endpoint negotiation reached
CONFIGURED/OPEN and a transport was created in hardware testing. Listening and the freeze
comparison with the previous chunked decoder remain the end-to-end gate.

The three numeric stages cannot all equal the master: one of them must be a neutral/trim stage, or
the signal is attenuated twice. The synced pair is Mac ↔ Capsula; the PA source (90 %) and loopback
(cube-root of the master) are the gain stages.

The volume cue peaks at 0.22 of full scale, about five times quieter than the original 0.95.

A2DP jitter was tuned on the PulseAudio fallback path: the original 60 ms loopback target let the
source buffer swing by hundreds of milliseconds and the resampler move between 44.03 and 44.4 kHz.
With 250 ms the source settles at 57–111 ms against its 68 ms target at ~44.1 kHz. The native
endpoint bypasses that loopback and its resampler. The Bluetooth UART IRQ is moved to CPU3
by the boot hook, because `hci_uart` logged `Unexpected start frame` errors while the IRQ shared
CPU0 with the daemon and wakeword inference.

Paired-device listing and unpair are Web-UI/CLI only. `ListPairedDevices` and `UnpairDevice` reject
supervised app callers by run ID and by sandbox UID; the rest of the Bluetooth surface is
app-facing. Unpair itself was exercised against the simulator manager, not against the user's
paired MacBook.

HSP/HFP (telephony) is not implemented. Only A2DP Sink plus AVRCP CT are exposed.

Once superseded, capture is unaffected (a 2 s take recorded after takeover has room-noise RMS 1470
against 2196 before it). `voice-agent` was also the process that kept `/dev/watchdog` alive, so
killing it by hand without giving the timer a new owner resets the board after the 21 s timeout
(`dw_wdt: keepalive missed, machine will reset`); always let capsulad perform the handover.

### mDNS and Avahi

The stock image runs `avahi-daemon`, which owns multicast DNS and the 5353 socket. `capsulad` does
not start a second responder or bind 5353: it applies the host name through the public
`org.freedesktop.Avahi.Server` calls on the system bus. The value is the persisted single-label
`Hostname` setting (default `capsula`), normalized to lower case and validated as one DNS label;
the `.local` suffix is added by Avahi. Saves use the normal settings CAS revision. Apply reads
`GetState`/`GetHostName` and makes **no** `SetHostName` write when the responder is RUNNING and
already owns the requested label, confirming the advertisement through `GetHostNameFqdn`. After a
write it waits a bounded interval for the responder to report the requested label; a redundant
reply or a concurrent writer winning the race is accepted only from that authoritative read.
Write-time collisions, invalid names and a missing responder remain visible errors with the
observed FQDN, and a successful write whose FQDN cannot be read is reported as
applied-but-unconfirmed rather than a fabricated advertisement. The daemon applies the latest
requested label/generation at startup and when Avahi transitions to `AVAHI_SERVER_RUNNING`; the
monitor subscribes to `StateChanged` before taking its initial snapshot and a coalesced worker
drains changes during an apply, replaying the latest RUNNING edge instead of dropping it. `Close`
cancels without publishing a warning. `GET /settings` is read-only: it renders the stored status
and never applies a name or saves configuration. The simulator and unsupported platforms report
mDNS as unavailable, cannot rename the host, and retain the saved label.

This is a runtime setting in Avahi; durability comes from the settings store being re-applied on
daemon start. A read-only snapshot of the unit observed Avahi `RUNNING`, `GetHostName=capsula` and
`GetHostNameFqdn=capsula.local`, which is why the old unconditional `SetHostName` produced a
redundant-operation warning. The portable fake-responder tests, a barrier-driven private-bus Linux
adapter fixture (`-tags avahifixture`) and a real Web UI/auth/TLS/settings browser run against a
successful responder fixture cover the ordering. In that run startup, two GETs and a same-name save
made zero `SetHostName` calls with a clean “Advertised as capsula.local”; a changed label made one
call; a genuine collision stayed visible with no advertised claim and no writes from the GET;
restoring the label recovered. That is fixture-backed browser evidence, not a device or transport
proof. Real `.local` resolution and real collision/rename behavior after an Avahi restart remain
device gates. The fixture is not the simulator's unsupported status.

### Sensors

The `bh1730fvc` on `i2c@ff060000` is exposed through IIO. The backend scans
`/sys/bus/iio/devices/iio:device*/name`, prefers the processed `in_illuminance_input` (unit `lux`)
and falls back to `in_illuminance_raw` or the older i2c-driver paths (unit `lux-raw`). It is
surfaced as `sensor.ambient_light`.

`tsadc@ff1f0000` and the `soc-thermal` / `logic-thermal` zones are surfaced as
`sensor.soc_temperature` and `sensor.logic_temperature` from
`/sys/class/thermal/thermal_zone*/temp` (millidegrees converted to Celsius). Other zones are
reported as `sensor.temperature.<type>`.

## Sandbox constraints on this firmware

- Root filesystem is SquashFS read-only; persistent state lives on ext2 `userdata` via overlay.
- Apps are launched with a read-only `/app` and `/assets`, no writable `/tmp` or home, and only
  the daemon socket as a filesystem IPC endpoint.
- Kernel 4.4.167 delivers only a mount namespace: user, PID, network and IPC namespaces and the
  seccomp filter are absent (see the measurements below). `capsulad` probes user namespace and
  filter support at startup, falls back to a root mount namespace with a distinct app UID, and
  reports the actual mode. It never claims isolation it did not obtain.
- Vendor daemons may hold PulseAudio, evdev or LED nodes. Capability status and diagnostics are the
  source of truth; no claim of parity across Mini variants is made.

## Measured on the unit (2026-11-10)

Serial `03104B1D40E15C55`, kernel 4.4.167, reached over `adb` at `192.168.1.50:5555`.

| Probe | Result |
|---|---|
| `unshare(CLONE_NEWNS)` | ok |
| `unshare(CLONE_NEWUSER)` | `EINVAL` — `CONFIG_USER_NS` is off |
| `unshare(CLONE_NEWNET)` | `EINVAL` |
| `prctl(PR_SET_SECCOMP, FILTER)` | `EINVAL` — no `CONFIG_SECCOMP_FILTER` |
| `/proc/self/ns` | `mnt` only |
| `LEDS_MULTI_CTRL_IOCTL_GET_COUNT` on `/dev/topleds` | 36 |
| `LEDS_MULTI_CTRL_IOCTL_GET_COUNT` on `/dev/led_multi_ctrl` | 30 |
| `/proc/bus/input/devices` | one node: `gpio-keys` (event0); no touch panel |
| `pactl list short sinks/sources` | `alsa_playback` + `alarm`/`ring`/`call`/`purr`/`master`/`snapcast`, `speex` + `call_src`/`mics`/`mics_loopback`/`loopback` at 16 kHz mono |
| `/sys/class/gpio` | `gpio21` exported at boot by `S50mic-enable` |
| storage | `/rom` 33.6 M (100 % full, squashfs), `/data` 78.5 M, `/tmp` tmpfs 117 M |
| timezone | `/etc/timezone` says `Asia/Shanghai` (UTC+8) while `/etc/localtime` → `Europe/Moscow` (UTC+3); `date` reports MSK |
| `/sys/class/watchdog` | absent (no `CONFIG_WATCHDOG_SYSFS`), so `nowayout` cannot be read |
| `WDIOC_GETTIMEOUT` on `/dev/watchdog` | 21 s |
| `WDIOC_SETOPTIONS(WDIOS_DISABLECARD)` | `ENOTTY` — unsupported |
| magic close (`'V'`) | ignored: the board resets ~21 s after feeding stops |

### Hardware watchdog ownership

`/dev/watchdog` is opened by the vendor stack: `voice-agent -mode=app -watchdog` holds it and
`btctl`/`tgclient` inherit the descriptor. `self-check.sh` (S98) is a firmware-update validator, not
a service watchdog: it only reboots when `/data/update_successful` exists, and it reboots on
`pgrep -x voice-agent` failure within that window. On a settled unit neither flag exists and the
script exits immediately.

Because `SETOPTIONS` and the magic close are both unavailable, an armed timer on this kernel can
only be **kept alive**, never stopped. The framework therefore treats the timer as a system liveness
contract:

- `capsulad` starts the retry loop before superseding the vendor stack, so the moment the vendor
  releases the descriptor the framework owns it; it reports `BACKEND_PARTIAL` until then.
- Once armed, it feeds at `timeout/3` (capped at 2 s) and reports `BACKEND_AVAILABLE`.
- A graceful stop hands the descriptor to a detached `capsulad __capsula_watchdog_feeder` process,
  so planned restarts and upgrades do not reset the board; the next `capsulad` reclaims it.
- A crash or OOM kill of `capsulad` leaves nobody feeding, so the board resets. That is the
  intended fail-safe: the supervisor owns the timer, and its death returns the device to a known
  state.

After the takeover the timer has exactly one owner (`capsulad`) and the key device has one reader.
The LED nodes are the remaining shared surface: **two vendor processes hold `/dev/topleds` and
`/dev/led_multi_ctrl` open** concurrently with `capsulad`. Opening both nodes and issuing `GET_COUNT`
alongside them works, so the driver does not enforce exclusive ownership, but frame writes race the
stock agent until it is stopped as well.

`capsulad -platform rockchip-stock` starts on this image and reports `mode=mountns`; the daemon's
built-in clock and ring system apps drive `display.segment.front` and the LED ring without separately
installed clock or ring apps.

### Operating the unit without a service manager

The deployed unit runs the single self-contained `/etc/init.d/S99btctl.sh` hook (installed since
2026-11-13; the historical two-file `S60capsulad` + `exec` wrapper integration is retired). Normal
starts, restarts and upgrades go through that boot service; the manual sequence below remains the
maintenance fallback when the overlay or boot hook is missing. Two facts make that less trivial
than it looks:

- **`/data` holds only 78.5 MB.** The current stripped `capsulad` is about 27 MB (27,132,164 bytes
  as of the 2026-11-14 web-rewrite deployment);
  operator-installed app
  packages, recordings and logs grow from there. A deploy that keeps the old binary next to the new
  one needs the new binary's size free (about 27 MB, or the same again for a retained
  `.prev`); free space first (`df -h /data`), deploy to `*.new` and `mv` into place,
  and keep the log small. Upgrading the daemon quarantines old installed sample bundles but keeps
  their files; a manual `ls` may still show them even though they are not active. Removing those
  directories is an explicit operator action to reclaim space.
- **SIGHUP killed the daemon silently.** A shell that exits takes its child with it; the framework
  now ignores SIGHUP (`cmd/capsulad/signal_unix.go`) and should still be started with
  `nohup ... &`. Without the new owner the armed timer resets the board 21 s after the old process
  dies, which is what produced several unexplained reboots.

The safe restart sequence, verified on the unit:

```sh
adb push capsulad /data/capsdemo/capsulad.new
adb shell 'killall -TERM capsulad; sleep 7; mv /data/capsdemo/capsulad.new /data/capsdemo/capsulad'
adb shell 'cd /data/capsdemo && nohup ./capsulad -platform rockchip-stock -data-dir /data/capsdemo   -web-addr 0.0.0.0:8443 -log-file /data/capsdemo/capsulad.log >/data/capsdemo/nohup.out 2>&1 &'
```

### Display channel order

Lighting all 30 channels confirms the geometry: four seven-segment digits plus the two colon dots,
"88:88", driven entirely through `LEDS_MULTI_CTRL_IOCTL_MULTI_SET`. The driver's record order is
**not** digit-major and **not** segment-major — channels 0..3 land in different digit positions and
channels 0..6 are not one digit — so the previous `cell*7 + segment` encoding is wrong and the
vendor's JSON permutation cannot be assumed correct either.

Two bring-up instruments exist for this:

- `capsula platform ledtest` on the device lights arbitrary channels (`-channels 0-6,28`, `-sweep`,
  `-json FILE -map identity|vendor|inverse`) and can leave a frame lit (`-hold 0`) for an external
  stepper.
- `cmd/ledmap` on the host walks all 30 channels over adb, asks which digit and segment lit, writes
  `ledmap.json`, and prints the digit/segment → channel table. It resumes if interrupted.

The map is now measured — `ledmap.json` came from `cmd/ledmap` and lives in `multiCtrlChannelMap`
(collected 2026-11-10): digit/segment → channel is digit 1 `a=1 b=28 c=21 d=17 e=22 f=29 g=24`,
digit 2 `26 25 19 18 23 27 20`, digit 3 `0 2 7 5 6 4 3`, digit 4 `10 12 11 14 13 8 9`, colon top 16,
bottom 15. A clock face renders correctly with it, and `TestEncodeMultiCtrlFrameClockPattern` pins the
whole chain from glyph bits to channels.

The ring was verified with `capsula platform ledtest -ring`: pixel 0 is at the top, indices 0→11
travel smoothly clockwise, and a red pixel renders red, which confirms the recovered WS2812B
permutation (including the vendor's GRB swap) on hardware. Ring output now applies a 2.2 gamma
curve in the encoder (the stock frames are binary 0/255, so there is no vendor curve to match); the
segment display stays linear because its calibration was accepted as-is.

### Ambient-light brightness (stock algorithm)

The stock brightness is *not* the `powf(x, 4)` curve in `ClockBrightnessCalc` (0x157258) — that path
feeds something else. The value the clock actually displays comes from the light handler at
0x27ca68, which reads `in_illuminance_both_input` (the same IIO file the framework uses) and maps:

```
lux < 5        -> 0
lux > 32768    -> 100
otherwise      -> trunc(logf(0.75 + 0.05*lux) * 19.9375)   // percent
clamped to [5, 95]; re-applied only when |new - applied| > 5
```

Constants: `0x3d4ccccd` (0.05), 0.75, `0x419f806b` (19.9375), thresholds 5 and `0x453b8000`
(32768). This is confirmed by the driver's LED-class brightness: the stock unit wrote **14/255**
at 7.4 lux, and the framework's implementation writes **13/255** at 7.3 lux (5 % floor, one LSB of
truncation). The framework animates toward each new value at 0.06 per 100 ms tick, which matches the
smooth stock transition.

**Framework divergence:** the vendor curve returns 0 below 5 lux, which blanks an enabled automatic
display in darkness. The framework keeps the stock curve for the sensor range but floors the auto
path at 5 % (13/255 after quantization), ignores malformed samples (`NaN`, `±Inf`, negative) instead
of treating them as darkness, and keeps the last good target when a read fails. The floor applies
only in auto mode; a manual brightness of 0 is still an explicit off. The 5 % level is intended to
be visible, but the physical result in a dark room is not measured.

The built-in clock system app takes the timezone from the daemon's device timezone, unless the
`capsula.clock` app-configuration `timezone` key overrides it; the sandbox no longer forces `TZ=UTC`;
`/etc/localtime` and `/usr/share/zoneinfo` are bind-mounted so a scripted or native app can resolve
zones.

## What must be verified on hardware before production

1. LED ring/display `MULTI_SET` writes and permutations: both maps are validated on the unit —
   the segment table came from `cmd/ledmap`, and the ring shows pixel 0 at 12 o'clock, travels
   smoothly clockwise with no jumps, and renders `R,G,B=(255,0,0)` as red. What is left is gamma
   (no photometer); concurrent vendor writes no longer apply because the vendor assistant init
   scripts are retired.
2. ~~Mixer path~~ verified with a scripted app: playback from the app bus reaches `master`
   (1 kHz tone, RMS 4454 on `master.monitor` at 70 % volume and 1908 at 30 %, i.e. exactly the
   source's 6364 × the software volume), a conversation ducks it to 0.25 × (1113 / 477) with a
   ~150 ms fade, and microphone capture arrives over the bus. The visible-indication watchdog
   terminates the stream 5 s after the last qualifying indication (polls every 1 s, so 5–6 s after
   the indication disappears). Remaining: latency/underrun measurement under sustained load, and
   pre-roll timing.
3. evdev capabilities: there is no touch/coordinate panel, so only the five `gpio-keys` exist.
   `coordinates_available`/`full_raw_available` stay false, but `full_panel_available` is true and
   the key/gesture contract is testable on the unit; raw coordinate semantics need a different
   panel. Verify a single-key tap resolves once after the 250 ms recognition window, a held circle
   still summons at 700 ms, an overlapping edge-to-edge crossing is recognized without stepping
   volume, sequential opposite taps and circle-then-microphone remain deliberate key actions, the
   vertical pair is full-capture only, a 0/1-contact glide is not treated as a gesture, full
   swipes/circles emit the documented gesture, and an unfinished multi-key sequence produces no
   mute/volume/summon effect. Confirm the 250 ms/700 ms windows follow the kernel event timestamp
   even when the read loop is delayed. The user's map is authoritative; a passive trace cannot
   establish contact order and must not be used to invert direction.
4. ~~Namespace/seccomp support~~ measured: mount namespace only, no user/net/PID namespaces and no
   seccomp filter. ~~App runtime dependencies inside the mount namespace~~ verified 2026-11-12: a
   native app mapped `libcapsula.so.1` (ABI 1.0) from `/capsula/lib` and reported `APP_READY`;
   static Go app packages need no libraries, and other Rust binaries still need a matching libc and
   loader.
5. ~~GPIO 21 mute behavior~~ verified with the vendor agent stopped: `gpio21` goes 1→0 with the
   setting, `pactl get-source-mute speex` follows, and an acoustic loop shows the gate cuts the
   microphone (live capture RMS 1977, peak 9420 → RMS 38, peak 222 with PulseAudio unmuted, so the
   attenuation is the GPIO gate and not the mixer). The vendor assistant is now retired at boot, so
   this is the normal configuration.
6. ~~Vendor daemon coexistence/ownership rules and boot ordering~~ done: one self-contained
   `/etc/init.d/S99btctl.sh` hook contains preparation, health-gated start, vendor retirement and
   fallback. The historical two-file deployment was verified across power cycles with clock,
   assistant and media `APP_READY`; current builds do not include those sample packages, and the
   clock and ring are now compiled-in system apps. The 2026-11-12 build was health-verified with
   the old service (watchdog 21 s) but has no power-cycle test; the single-hook rewrite has not
   been deployed. Bluetooth connect, AVRCP control and absolute volume are verified with a paired
   source without `voice-agent`/`btctl`. Remaining: deploy and power-cycle the single hook, pair
   and unpair a throwaway device through the Web UI (unpair was deliberately not run against the
   user's MacBook), exercise a source that publishes AVRCP track metadata, and decide the boundary
   for the vendor helpers that still run (`okcall`, `stereoctl`, `telemetryd`, `snapclient`).
7. Native A2DP ordering/readiness with a real source: confirm the sink is advertised before the
   adapter is connectable after a cold boot and after a BlueZ restart/power cycle, that a failed
   registration keeps pairable/discoverable off with the documented `Detail`, and that a source
   connected during the pre-daemon/autopower window either recovers A2DP on the same link or
   requires a reconnect. PA-module removal timing around an active source is a real-device gate.
8. mDNS host name: save a label in the Web UI, confirm `.local` resolution with `avahi-resolve`,
   restart Avahi through the device init system and confirm the same label is re-adopted, and
   check a colliding/invalid name keeps the prior name and reports the collision. With the label
   already active, a Settings GET and a daemon restart must produce zero `SetHostName` calls
   (`dbus-monitor --system "interface=org.freedesktop.Avahi.Server,member=SetHostName"`) and no
   redundant-operation warning. The simulator, fake responder,
   barrier-driven private-bus fixture and fixture-backed browser run cannot prove device/network
   resolution.
9. Application capture DSP: confirm the live `speex` source is S16LE mono 16 kHz so the chain is
   active, that `audio.input.microphone` and the wakeword/inference feed report
   `framework.dsp.v1` while only the explicit `.raw` endpoint stays unprocessed, that
   muting/reopening produces a clean discontinuity, and that output length is invariant across
   capture chunk sizes. The colored-noise estimator and the installed raw-trained model on the
   processed tap have local finite-clip evidence only: 2.9 dB cleanup on the consumed mostly-Mac-
   music recording; settled warm classifier replay 6/6 raw and 6/6 processed (legacy-model
   usability, threshold unverified). Real fan/traffic/music behaviour, quiet speech, subjective
   suppression benefit and acoustic transparency remain listening/acoustic gates.
