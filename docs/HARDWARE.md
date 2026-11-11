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

The Rockchip backend uses the firmware's `pacat`/`parec` clients: playback to `master`, capture
from `speex`, at 48 kHz stereo / 16 kHz mono S16LE. Latency, format negotiation and coexistence
with vendor daemons require device testing; the subsystem is reported `PARTIAL`.

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
| `voice-key` | `0x2c0` `BTN_TRIGGER_HAPPY1` in one DTB, `0x246` `KEY_VOICECOMMAND` in the other | `voice` |

The two DTBs embedded in the boot partition disagree on the voice key code and label, so the
backend derives the semantic name from each child's node name and label and merges those entries
over a default map. `BTN_0`, `KEY_ENTER` and `KEY_OK` still map to `primary`. The framework's
system input handler consumes volume and microphone keys, and `primary` drives touch gestures;
`play_pause` and `voice` are emitted as key events and reach an application only while it holds a
full input capture. Raw coordinate availability must be confirmed per unit; the resource advertises `coordinates_available` only when
the evdev device exposes absolute axes. Full/raw capture fails explicitly when the backend cannot
provide it.

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
bring-up; the boot integration is now deployed:

- `/etc/init.d/S60capsulad` (repo: `deploy/rockchip-stock/S60capsulad`) starts the daemon after
  `S02mount-data`, `S50mic-enable` and `S50pulseaudio`, gates on `capsulad -health`, and falls back
  to the preserved vendor chain (`/data/capsdemo/vendor-init/S97voice-agent.sh`) if the daemon
  cannot become healthy.
- **The boot hook is `S99btctl.sh`.** `rcS` expands `/etc/init.d/S??*` once, before
  `S23overlayfs.sh` pivots into the `/data` overlay, so a script that exists only in the overlay
  (`S60capsulad`) is never in that list. The stock `S99btctl.sh` is a vendor no-op; its deployed
  replacement `exec`s `/etc/init.d/S60capsulad start`. The vendor assistant scripts
  (`S97voice-agent.sh`, `S99sysmon.sh`) are whiteouted in the overlay and copied to
  `/data/capsdemo/vendor-init/`.
- Verified across two power cycles on serial `03104B1D40E15C55`: `boot.log` records
  `starting /data/capsdemo/capsulad` and `daemon healthy`, `capsulad -health` exits 0, clock,
  assistant and media report `APP_READY`, and `/dev/watchdog armed, timeout=21s` appears.
- A factory reset wipes the overlay: the device then boots the stock assistant and the deployment
  must be repeated.

In-place upgrades go through the same service:

```sh
# on the device, with the new binary staged outside /data (tmpfs has 117 MB)
capsula -data-dir /data/capsdemo daemon install -binary /tmp/capsulad.new
capsula -data-dir /data/capsdemo daemon rollback     # restores capsulad.prev
```

`daemon install` runs `<binary> -version`, stages beside the target, renames the live binary to
`capsulad.prev`, activates by rename, restarts through `/etc/init.d/S60capsulad`, and requires the
framework API to answer before it reports success; any failure puts the previous binary back. The
`.prev` copy costs 21.9 MB on `/data`, so delete it only when the rollback copy is no longer wanted.

Because `/data` is 78.5 MB, the 24.4 MB negative-training corpus, its sidecar and the older
recordings were moved to the host. `/data` currently has about 5.2 MB free. Restore training input
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
was an adapter that nobody powered after the vendor stack was retired. The daemon now sets
`Adapter1.Powered`, `Discoverable` and `Pairable` through D-Bus, powers the adapter at startup and
reconciles it every 5 s after a BlueZ restart.

The vendor `btctl` was also the component that registered A2DP endpoints. Without it the adapter
advertised only AVRCP (0x110C/0x110E) and no Audio Sink, so a source refused to connect with
`br-connection-profile-unavailable`. The daemon now loads PulseAudio's standard
`module-bluetooth-discover` at startup (the modules ship in `/usr/lib/pulse-16.1/modules`); the
adapter then advertises Audio Sink (0x110B), Headset (0x1108) and Audio Source (0x110A), and the
source's audio appears as `bluez_source.<ADDR>.a2dp_source`. On connect the daemon loads
`module-loopback source=… sink=master latency_msec=60`; on disconnect it unloads it.

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

### Bluetooth volume and A2DP jitter

The framework master volume is applied by the daemon's software mixer, not by PulseAudio. On the
unit `alsa_playback`, `master` and every sink input sit at 100 % / 0.00 dB; that is intentional, not
a drift. A captured `master.monitor` with the local media player confirms the digital gain is exact:
0 % → silence, 1 % → peak 58, 5 % → 294, 10 % → 589, 20 % → 1179. The source's AVRCP absolute volume
tracks the same setting (10 % → 13/127, 50 % → 64/127).

The volume-change tick used to be synthesized at 0.95 of full scale, i.e. ~5× louder than content
at the same setting and −6 dBFS at 50 % — painful on the Mini's amplifier. It is now generated at
0.22 peak; `TestVolumeCueIsPercussivePCM` pins the quieter range. The daemon never writes a
PulseAudio sink volume, so a `pactl` level changing independently would come from another process.

A2DP initially jittered. The loopback target was 60 ms: PulseAudio's source buffer swung between
0 and 400 ms and the adaptive resampler ran between 44.03 and 44.4 kHz, producing audible wobble.
With `latency_msec=250` the source settles at 57–111 ms against its 68 ms target and the loopback
rate sits at 44.10–44.13 kHz. The kernel also logged `hci_uart: Unexpected start frame` (two in a
30 s capture) while the Bluetooth UART IRQ was handled on CPU0 together with the daemon and
wakeword inference; `S60capsulad` now moves that IRQ to CPU3 (`/proc/irq/12/smp_affinity`). After
that change no new frame errors appeared in a 30 s window, though the source pausing makes long
silence-based captures inconclusive; sustained listening remains the acceptance test. HSP/HFP
telephony is still not implemented.

Paired-device listing and unpair are Web-UI/CLI only. `ListPairedDevices` and `UnpairDevice` reject
supervised app callers by run ID and by sandbox UID; the rest of the Bluetooth surface is
app-facing. Unpair itself was exercised against the simulator manager, not against the user's
paired MacBook.

HSP/HFP (telephony) is not implemented. Only A2DP Sink plus AVRCP CT are exposed.

Once superseded, capture is unaffected (a 2 s take recorded after takeover has room-noise RMS 1470
against 2196 before it). `voice-agent` was also the process that kept `/dev/watchdog` alive, so
killing it by hand without giving the timer a new owner resets the board after the 21 s timeout
(`dw_wdt: keepalive missed, machine will reset`); always let capsulad perform the handover.

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

`capsulad -platform rockchip-stock` starts on this image and reports `mode=mountns`; the sandboxed
`org.capsula.clock` runs as a distinct UID in a private mount namespace and takes
`display.segment.front` over the app bus.

### Operating the unit without a service manager

There is no init integration yet, so the daemon is started by hand. Two facts make that less trivial
than it looks:

- **`/data` holds only 78.5 MB.** A stripped `capsulad` is 21.7 MB, the two sample apps 18.4 MB, and
  recordings/logs grow. A deploy that keeps the old binary next to the new one needs ~43 MB free;
  free space first (`df -h /data`), deploy to `*.new` and `mv` into place, and keep the log small.
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

The clock app takes the timezone from its own `timezone` setting (IANA name, empty = device
timezone). The sandbox no longer forces `TZ=UTC`; `/etc/localtime` and `/usr/share/zoneinfo` are
bind-mounted so a scripted or native app can resolve zones.

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
3. evdev capabilities: coordinates, pressure and gesture keys cannot be checked on this unit — it
   has no touch panel, so only the five `gpio-keys` exist.
4. ~~Namespace/seccomp support~~ measured: mount namespace only, no user/net/PID namespaces and no
   seccomp filter. Remaining: app runtime dependencies inside the mount namespace (the static Go
   sample apps need no libraries; Rust binaries with glibc may).
5. ~~GPIO 21 mute behavior~~ verified with the vendor agent stopped: `gpio21` goes 1→0 with the
   setting, `pactl get-source-mute speex` follows, and an acoustic loop shows the gate cuts the
   microphone (live capture RMS 1977, peak 9420 → RMS 38, peak 222 with PulseAudio unmuted, so the
   attenuation is the GPIO gate and not the mixer). The vendor assistant is now retired at boot, so
   this is the normal configuration.
6. ~~Vendor daemon coexistence/ownership rules and boot ordering~~ done: `S99btctl.sh` execs
   `S60capsulad start`, which health-gates and falls back to the preserved vendor chain; verified
   across power cycles with clock, assistant and media `APP_READY`. Bluetooth connect, AVRCP
   control and absolute volume are verified with a paired source without `voice-agent`/`btctl`.
   Remaining: pair and unpair a throwaway device through the Web UI (unpair was deliberately not
   run against the user's MacBook), exercise a source that publishes AVRCP track metadata, and
   decide the boundary for the vendor helpers that still run (`okcall`, `stereoctl`, `telemetryd`,
   `snapclient`).
