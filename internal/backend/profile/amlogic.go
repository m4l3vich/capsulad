package profile

import "time"

// The Amlogic A113X board family. Two published builds run on the same hardware
// (VK Capsula Mini, Amlogic A113X, aarch64):
//
//   - the jailbroken stock Buildroot image (glibc 2.28 on the supplied image, PulseAudio
//     16.1, firmware 44.90), with the read-only /rom squashfs and a /data
//     overlay, and the stock voice-agent owning the ring, the panel, the
//     microphones and the hardware watchdog;
//   - Alpine Linux 3.24 (musl) on a mainline kernel, where the stock vendor
//     stack is absent: ALSA directly (with BlueALSA for Bluetooth audio) and
//     kernel LED/line-display devices replace the vendor char devices.
//
// Hardware facts common to both (cmp18, "VK Капсула Мини (на Amlogic A113X)",
// 2026-08-23, and the ryysel/marusya-hw and ryysel/capsula-mini-airplay2
// projects): 4x Cortex-A53, 256 MB DDR3, 256 MB NAND, Fn-Link K255B-SR Wi-Fi/BT,
// NTP8910A class-D amplifier, four PDM microphones, a BH1730FVC ambient-light
// sensor, a 12-pixel WS2812B ring on a 36-channel chain, and a four-digit
// seven-segment clock panel plus two colon dots. The panel is fitted with either
// an ISSI IS31FL3236A or an Awinic AW21036 depending on the production batch.
//
// Everything here is data. The stock vendor char-device ABI for the clock panel
// has not been recovered, so the panel stays a probe result, not a mapping.

// AmlogicA113XStock is the jailbroken stock Buildroot image.
var AmlogicA113XStock = Profile{
	ID:       "amlogic-a113x-capsula-mini-stock",
	Platform: "amlogic-stock",

	Model: Model{
		BoardRevision: "Capsula-mini-6",
		// The stock DTB's root model is just "Amlogic", so SoCMatch is the board
		// vendor token, not the SoC part number.
		ModelFallback:      "VK Capsula Mini (model probe unavailable)",
		SoC:                "Amlogic A113X",
		SoCMatch:           "amlogic",
		KernelFallback:     "unknown",
		ImageModelFallback: "VK Capsula Mini (A113X, from stock image metadata)",
	},
	Serial: Serial{
		Path:   "/proc/cpuinfo",
		Fields: []string{"Serial", "Serial Number"},
	},
	Audio: Audio{
		// From the stock image's /etc/pulse/device.pa: `master` is the mixed
		// output and `speex` the processed microphone source (also alarm, call,
		// ring, output, alsa_playback, rtp, snapcast sinks; mics, mics_loopback,
		// call_src, input, loopback, uroc sources).
		Sink:             "master",
		Source:           "speex",
		MicEnableGPIO:    0,
		PlaybackFormat:   s16le,
		PlaybackRateHz:   48000,
		PlaybackChannels: 2,
		CaptureFormat:    s16le,
		CaptureRateHz:    16000,
		CaptureChannels:  1,
	},
	LED: LED{
		TopLEDsPath:   "/dev/topleds",
		MultiCtrlPath: "/dev/is31fl32xx",
		SysfsClassDir: "/sys/class/leds",
		// DefaultPath is the display a bring-up tool drives without -ring.
		DefaultPath:       "/dev/is31fl32xx",
		WS2812Channels:    36,
		WS2812Pixels:      12,
		MultiCtrlChannels: 30,
		SegmentDigits:     4,
		SegmentsPerDigit:  7,
		Gamma:             2.2,
		// The stock clock panel is exposed as the LED class array clock-led1..30
		// (read/written there; the voice-agent writes the same 30 values through
		// /dev/is31fl32xx with one ioctl). The channel order is the map published
		// by the MicroPython hardware layer for this board and is identical to
		// the RK3308 IS31FL3236 table (third-party evidence, not measured here).
		SegmentChannelPrefix: "clock-led",
		SegmentChannelMap: []int{
			1, 28, 21, 17, 22, 29, 24,
			26, 25, 19, 18, 23, 27, 20,
			0, 2, 7, 5, 6, 4, 3,
			10, 12, 11, 14, 13, 8, 9,
		},
		ColonTopLED:    "clock-led17",
		ColonBottomLED: "clock-led16",
	},
	Input: Input{
		EventDir:    "/dev/input",
		EventPrefix: "event",
		// The panel is a Hynitron CSK05T touch panel read through evdev, not a
		// device-tree gpio-keys node, so no compatible string is probed.
		KeymapCompatible: "",
		// The panel reports
		// KEY_VOLUMEUP/VOLUMEDOWN/MICMUTE/VOICECOMMAND; on this board
		// KEY_VOICECOMMAND is the assistant/action key, not the RK3308 centre.
		KeyOverrides: map[int]string{0x246: "play_pause"},
	},
	Watchdog: Watchdog{
		// The stock voice-agent holds the timer and stopping it can reset the
		// board. Disarm behaviour (magic close / WDIOC_SETOPTIONS) is not
		// measured on this kernel, so Nowayout stays false until it is.
		Path: "/dev/watchdog",
		// 30 s: fallback only. The vendor DT property default_timeout = 10 belongs
		// to a vendor driver whose units are unknown (the mainline gxbb property of
		// that name is milliseconds), so it is not treated as a fact; the supervisor
		// derives its cadence and window from the value the driver reports.
		DefaultTimeout: 30 * time.Second,
		StatusID:       "amlogic.syswatchdog",
	},
	// No published stock supplicant paths for this image; the framework reports
	// Wi-Fi from the live manager instead of adopting a guessed file.
	Wifi: Wifi{},
	Deploy: Deploy{
		// rcS expands /etc/init.d/S??* before the /data overlay is mounted
		// (S02datamount, S04overlaymount). A hook that exists only in the overlay
		// is therefore not started at boot; see docs/HARDWARE.md.
		// rcS expands S??* before the overlay, so a new overlay-only name never
		// runs. S99sysmon.sh is the vendor supervisor this framework replaces and
		// is already in the expanded list; S99btctl.sh stays untouched.
		InitScript:          "/etc/init.d/S99sysmon.sh",
		BootHookName:        "S99sysmon.sh",
		StagingReserveBytes: 26 << 20,
		DataReserveBytes:    8 << 20,
		ExtractionRatio:     2,
		BundleMaxFileBytes:  64 << 20,
		BundleMaxTotalBytes: 128 << 20,
		BundleMaxEntries:    8192,
		// The /data partition size has not been measured on this board; the
		// operator-facing prose omits the figure when it is zero.
		DataPartitionMiB: 0,
		DaemonBinaryMiB:  40,
	},
	Architecture: Architecture{
		Target:      "aarch64",
		Accepted:    []string{"aarch64", "arm64"},
		ABIContains: "linux",
		MaxGLIBC:    "2.28",
	},
	Memory: Memory{
		// 256 MB DDR3 (ESMT M15T2G16128A-DEB) on the published unit.
		PhysicalMiB: 256,
		HeapLimitMB: 80,
		GCPercent:   50,
	},
	// The stock ambient-brightness curve for this board has not been measured.
	// The framework policy curve is used so automatic brightness has a defined
	// response; it makes no claim of matching the vendor agent.
	Brightness: Simulator.Brightness,
	// sysmon is the vendor supervisor that respawns voice-agent; both must go,
	// supervisor first, or the reaper fights a respawn loop.
	VendorProcesses: []string{"sysmon", "voice-agent"},
}

// AmlogicA113XAlpine is Alpine Linux 3.24 (musl) on the same A113X board.
var AmlogicA113XAlpine = Profile{
	ID:       "amlogic-a113x-capsula-mini-alpine",
	Platform: "amlogic-alpine",

	Model: Model{
		BoardRevision: "Capsula-mini-6",
		ModelFallback: "VK Capsula Mini Amlogic 113X (model probe unavailable)",
		// The supplied DTB reports compatible "amlogic,a113d amlogic,meson-axg"
		// while the model string calls the product A113X; both are kept visible.
		SoC:                "Amlogic A113X (A113D, meson-axg)",
		SoCMatch:           "a113",
		KernelFallback:     "unknown",
		ImageModelFallback: "VK Capsula Mini (A113X, from image metadata)",
	},
	Serial: Serial{
		Path:   "/proc/cpuinfo",
		Fields: []string{"Serial", "Serial Number"},
	},
	Audio: Audio{
		// Alpine has no PulseAudio on this board: ALSA directly, with BlueALSA
		// for Bluetooth sources. The image's /etc/asound.conf makes `default`
		// asym: playback plug:mastervol (softvol, "Master Playback Volume",
		// card 0) and capture plug:micboost (hw:0,1, "Mic Boost Capture
		// Volume"); there is no dmix. The framework pins the playback control so
		// its mixer is the only gain. Mute is a softvol, so it stays software.
		PCMDevice:        "default",
		MixerCard:        "0",
		MixerVolume:      "Master Playback Volume",
		PlaybackFormat:   s16le,
		PlaybackRateHz:   48000,
		PlaybackChannels: 2,
		CaptureFormat:    s16le,
		CaptureRateHz:    16000,
		CaptureChannels:  1,
	},
	LED: LED{
		// The Alpine DTB exposes the ring as spi@13000/topleds@0
		// (rohm,dh2228fv), not a vendor char device; the image's ringled drives
		// /dev/spidev0.0 at 3.34 MHz.
		TopLEDsPath: "",
		SPIDevPath:  "/dev/spidev0.0",
		SPISpeedHz:  3340000,
		// ringled sends 32-bit words by default; --pio 8-bit "breaks the frame
		// every 16 bytes".
		SPIBitsPerWord: 32,
		// ringled's --prime default is not printed; 1,000,000 is a rate constant
		// in the binary and the primer only has to differ from the frame rate.
		SPIPrimeHz:    1000000,
		SysfsClassDir: "/sys/class/leds",
		// No vendor display char device on Alpine: the panel is LED class.
		DefaultPath: "",
		// The panel is an IS31FL3236A/AW21036 clock on the mainline line-display
		// driver (or, on some builds, per-segment LED class devices named
		// clock:dN:a..g). Both are probed; the vendor ABI is not involved.
		WS2812Channels:    36,
		WS2812Pixels:      12,
		MultiCtrlChannels: 30,
		SegmentDigits:     4,
		SegmentsPerDigit:  7,
		Gamma:             2.2,
		SegmentLEDPattern: "clock:d%d:%s",
		SegmentLetters:    "abcdefg",
		ColonTopLED:       "clock:colon:top",
		ColonBottomLED:    "clock:colon:bottom",
	},
	Input: Input{
		EventDir:         "/dev/input",
		EventPrefix:      "event",
		KeymapCompatible: "",
		KeyOverrides:     map[int]string{0x246: "play_pause"},
	},
	// The supplied DTB has watchdog@f0d0 (amlogic,meson-gxbb-wdt) and the kernel
	// config builds meson_gxbb_watchdog=m with WATCHDOG_SYSFS=y and
	// WATCHDOG_NOWAYOUT unset, so a timer exists when the module loads. Disarm
	// behaviour is a kernel default, not a measurement, so Nowayout stays false;
	// if the module is absent the daemon reports "no watchdog device yet" and
	// retries instead of claiming protection.
	Watchdog: Watchdog{
		Path:           "/dev/watchdog",
		DefaultTimeout: 30 * time.Second, // mainline meson_gxbb_wdt.c DEFAULT_TIMEOUT
		StatusID:       "amlogic.syswatchdog",
	},
	Wifi: Wifi{},
	Deploy: Deploy{
		// OpenRC service. The deploy CLI installs the hook and starts it with
		// `rc-service capsulad` through the same -init-script contract.
		InitScript:          "/etc/init.d/capsulad",
		BootHookName:        "capsulad",
		StagingReserveBytes: 26 << 20,
		DataReserveBytes:    8 << 20,
		ExtractionRatio:     2,
		BundleMaxFileBytes:  64 << 20,
		BundleMaxTotalBytes: 128 << 20,
		BundleMaxEntries:    8192,
		DataPartitionMiB:    0,
		DaemonBinaryMiB:     40,
	},
	Architecture: Architecture{
		Target:   "aarch64",
		Accepted: []string{"aarch64", "arm64"},
		// Alpine links musl; a glibc-linked package must be rejected here.
		ABIContains: "musl",
	},
	Memory: Memory{
		PhysicalMiB: 256,
		HeapLimitMB: 80,
		GCPercent:   50,
	},
	Brightness: Simulator.Brightness,
}
