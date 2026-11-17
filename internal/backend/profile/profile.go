// Package profile holds the hardware description of one supported device as
// data. It contains no I/O, no interfaces and no build tags: a backend, the
// daemon or a CLI tool reads the fields it needs and decides how to use them.
//
// The daemon and the deployment CLI select a profile at composition time. All
// device literals (device paths, geometry, budgets, serial location, Wi-Fi
// stock paths and the brightness curve) live here; generic packages accept the
// values through their existing configuration structs.
package profile

import "time"

// Model describes the board identity probe and its fallbacks.
type Model struct {
	// ModelFallback is reported when the device tree has no readable model.
	ModelFallback string
	// SoC is the SoC name reported when the model probe succeeds.
	SoC string
	// SoCMatch is the lower-case token the model must contain for SoC to be
	// used; a model without it is reported in the SoC field instead.
	SoCMatch string
	// KernelFallback is reported when /proc/version cannot be read.
	KernelFallback string
	// ImageModelFallback is reported by the offline firmware inspector when
	// the extracted rootfs has no device tree model.
	ImageModelFallback string
}

// Serial describes where the hardware serial number is read and which keys
// carry it.
type Serial struct {
	// Path is the procfs file, normally /proc/cpuinfo on ARM Linux.
	Path string
	// Fields are the keys accepted within that file.
	Fields []string
}

// Audio describes the PulseAudio topology and PCM formats. A board whose
// userland has no PulseAudio (Alpine) uses PCMDevice/Mixer* instead: they name
// the ALSA PCM and the mixer control the framework pins and mutes.
type Audio struct {
	Sink   string
	Source string
	// MicEnableGPIO is the default sysfs GPIO number for the microphone gate.
	MicEnableGPIO int

	PlaybackFormat   string
	PlaybackRateHz   int
	PlaybackChannels int
	CaptureFormat    string
	CaptureRateHz    int
	CaptureChannels  int

	// PCMDevice is the ALSA PCM name the direct-ALSA backend opens for both
	// playback and capture (empty means the ALSA "default" device).
	PCMDevice string
	// MixerCard is the ALSA card name/number amixer targets; empty leaves the
	// card unset (amixer's default).
	MixerCard string
	// MixerVolume and MixerMute are the ALSA simple-mixer controls the
	// framework pins and mutes. Empty means the platform exposes no known
	// hardware gain, so the software mixer is the only gain stage.
	MixerVolume string
	MixerMute   string
}

// LED describes both vendor LED controllers and the fallback class devices.
type LED struct {
	TopLEDsPath   string
	MultiCtrlPath string
	// SysfsClassDir is the Linux LED class root used by the fallback driver.
	SysfsClassDir string
	// DefaultPath is the device a bring-up tool drives without -ring.
	DefaultPath string

	WS2812Channels    int
	WS2812Pixels      int
	MultiCtrlChannels int
	SegmentDigits     int
	SegmentsPerDigit  int

	// Gamma is applied to WS2812B ring output. The segment display stays
	// linear because its stock calibration was accepted on hardware.
	Gamma float64

	// SegmentChannelPrefix names a fixed monochrome channel array as LED class
	// devices: the prefix plus the 1-based channel number. SegmentChannelMap
	// maps cell*segmentsPerDigit+segment (a..g = 0..6) to the 0-based channel.
	// The Two-colon dots are named by ColonTopLED/ColonBottomLED.
	SegmentChannelPrefix string
	SegmentChannelMap    []int
	// SPIDevPath is a spidev node used to drive a WS2812B chain from userspace
	// when the kernel has no vendor LED char device (Amlogic Alpine). SpeedHz
	// and BitsPerWord are the SPI parameters; zero leaves them unset.
	SPIDevPath     string
	SPISpeedHz     int
	SPIBitsPerWord int
	// SPIPrimeHz is the short transfer rate used before each frame to force the
	// divider to be reprogrammed; zero disables priming. There is deliberately
	// no driver-rebind knob: unbinding a controller can strand it, and the primer
	// plus SetSpeed already drive spi_setup().
	SPIPrimeHz int
	// SegmentLEDPattern names one monochrome segment LED class device when the
	// panel is exposed as LED class devices: %d is the 1-based digit and %s the
	// segment letter taken from SegmentLetters. A panel with no such devices
	// leaves it empty.
	SegmentLEDPattern string
	// SegmentLetters maps segment bit 0..n to a device-name letter. The
	// framework contract is bit i = segment i, so "abcdefg" means a..g = 0..6.
	SegmentLetters string
	// ColonTopLED and ColonBottomLED are LED class device names, not indices:
	// the number in a name is 1-based while SegmentChannelMap is 0-based
	// (clock-led17 is channel 16). They are written outside the segment pattern.
	ColonTopLED    string
	ColonBottomLED string
}

// Input describes evdev discovery and the device-tree keymap selector.
type Input struct {
	EventDir    string
	EventPrefix string
	// KeymapCompatible is the compatible string of the gpio-keys node.
	KeymapCompatible string
	// KeyOverrides are board-specific semantic names merged over the probed
	// device-tree map. A panel that reports a code two boards name differently
	// (KEY_VOICECOMMAND is the neutral centre on the RK3308 panel and the
	// assistant key on the Amlogic panel) is decided here, not in code.
	KeyOverrides map[int]string
}

// Watchdog describes the hardware liveness timer.
type Watchdog struct {
	Path string
	// DefaultTimeout is used when the driver refuses to report the armed
	// interval.
	DefaultTimeout time.Duration
	// StatusID is the subsystem_backends backend id reported for the timer.
	StatusID string
	// Nowayout reports that opening the device arms the timer for good: the
	// driver ignores the magic close and refuses WDIOC_SETOPTIONS, so an armed
	// timer can only be kept alive or handed to another process, never
	// stopped. The supervisor must not pretend a Close disarmed it.
	Nowayout bool
}

// Wifi describes the stock supplicant files the framework adopts and restores.
type Wifi struct {
	StockConfigPaths  []string
	StockCtrlDirs     []string
	RestoreConfigPath string
	// AdoptStock imports the stock network list before terminating the stock
	// supplicant. It never copies plaintext passphrases, only derived PSKs.
	AdoptStock bool
}

// Deploy describes the boot service, bundle limits and free-space floors used
// by capsulad-cli deploy.
type Deploy struct {
	InitScript   string
	BootHookName string

	StagingReserveBytes uint64
	DataReserveBytes    uint64
	ExtractionRatio     int

	BundleMaxFileBytes  uint64
	BundleMaxTotalBytes uint64
	BundleMaxEntries    int

	// DataPartitionMiB and DaemonBinaryMiB appear in operator-facing prose;
	// the numeric budgets above are the enforced values.
	DataPartitionMiB float64
	DaemonBinaryMiB  float64
}

// Architecture describes which package binaries the target can execute.
type Architecture struct {
	// Target is the label used in errors, e.g. "aarch64".
	Target string
	// Accepted lists the GOARCH spellings that target executes.
	Accepted []string
	// ABIContains is the substring every accepted package ABI must contain.
	ABIContains string
	// MaxGLIBC is the highest GLIBC_* symbol version a glibc target accepts
	// (empty for musl/host). Deployment and packaging check it against the ELF.
	MaxGLIBC string
}

// Memory describes the device memory envelope. HeapLimitMB is the CLI default;
// GCPercent is applied by the daemon.
type Memory struct {
	PhysicalMiB int
	HeapLimitMB int
	GCPercent   int
}

// Brightness is the ambient-light curve recovered from the stock agent.
type Brightness struct {
	Scale    float64
	Lambda   float64
	Bias     float64
	Floor    float64
	Ceiling  float64
	Deadband float64
	MinLux   float64
	MaxLux   float64
	// AutoFloor is the lowest level an auto-managed display may reach.
	AutoFloor float64

	PollInterval  time.Duration
	Tick          time.Duration
	AnimationStep float64
}

// Profile is one device configuration. It is inert data.
type Profile struct {
	ID       string
	Platform string

	Model        Model
	Serial       Serial
	Audio        Audio
	LED          LED
	Input        Input
	Watchdog     Watchdog
	Wifi         Wifi
	Deploy       Deploy
	Architecture Architecture
	Memory       Memory
	Brightness   Brightness

	// VendorProcesses is the stock assistant chain replaced by -supersede auto.
	VendorProcesses []string
}

const s16le = "PCM_S16LE"

// RK3308Rev4Stock is the Capsula Mini rev 4 with the jailbroken stock Rockchip
// image (firmware 03104B1D40E15C55). Every value here is the literal the
// framework hard-coded before the profile existed.
var RK3308Rev4Stock = Profile{
	ID:       "rk3308-capsula-mini-rev4-stock",
	Platform: "rockchip-stock",

	Model: Model{
		ModelFallback:      "Capsula Mini Rockchip (model probe unavailable)",
		SoC:                "Rockchip RK3308",
		SoCMatch:           "rk3308",
		KernelFallback:     "unknown",
		ImageModelFallback: "Capsula-mini (RK3308, from stock image metadata)",
	},
	Serial: Serial{
		Path:   "/proc/cpuinfo",
		Fields: []string{"Serial", "Serial Number"},
	},
	Audio: Audio{
		Sink:             "master",
		Source:           "speex",
		MicEnableGPIO:    21,
		PlaybackFormat:   s16le,
		PlaybackRateHz:   48000,
		PlaybackChannels: 2,
		CaptureFormat:    s16le,
		CaptureRateHz:    16000,
		CaptureChannels:  1,
	},
	LED: LED{
		TopLEDsPath:       "/dev/topleds",
		MultiCtrlPath:     "/dev/led_multi_ctrl",
		SysfsClassDir:     "/sys/class/leds",
		DefaultPath:       "/dev/led_multi_ctrl",
		WS2812Channels:    36,
		WS2812Pixels:      12,
		MultiCtrlChannels: 30,
		SegmentDigits:     4,
		SegmentsPerDigit:  7,
		Gamma:             2.2,
	},
	Input: Input{
		EventDir:         "/dev/input",
		EventPrefix:      "event",
		KeymapCompatible: "gpio-keys",
	},
	Watchdog: Watchdog{
		Path:           "/dev/watchdog",
		DefaultTimeout: 30 * time.Second,
		StatusID:       "rockchip.syswatchdog",
		// Probed on the stock kernel: WDIOC_SETOPTIONS(WDIOS_DISABLECARD)
		// returns ENOTTY, the magic close ('V') is ignored and feeding must
		// continue or the board resets ~21 s later.
		Nowayout: true,
	},
	Wifi: Wifi{
		StockConfigPaths: []string{
			"/etc/wpa_supplicant.conf",
			"/etc/wpa_supplicant/wpa_supplicant.conf",
		},
		StockCtrlDirs:     []string{"/var/run/wpa_supplicant", "/run/wpa_supplicant"},
		RestoreConfigPath: "/etc/wpa_supplicant.conf",
		AdoptStock:        true,
	},
	Deploy: Deploy{
		InitScript:          "/etc/init.d/S99btctl.sh",
		BootHookName:        "S99btctl.sh",
		StagingReserveBytes: 26 << 20,
		DataReserveBytes:    8 << 20,
		ExtractionRatio:     2,
		BundleMaxFileBytes:  64 << 20,
		BundleMaxTotalBytes: 128 << 20,
		BundleMaxEntries:    8192,
		DataPartitionMiB:    78.5,
		DaemonBinaryMiB:     40,
	},
	Architecture: Architecture{
		Target:      "aarch64",
		Accepted:    []string{"aarch64", "arm64"},
		ABIContains: "linux",
		MaxGLIBC:    "2.33",
	},
	Memory: Memory{
		PhysicalMiB: 240,
		HeapLimitMB: 80,
		GCPercent:   50,
	},
	Brightness: Brightness{
		Scale:         19.9375, // 0x419f806b
		Lambda:        0.05,    // 0x3d4ccccd
		Bias:          0.75,
		Floor:         5.0,
		Ceiling:       95.0,
		Deadband:      0.05,
		MinLux:        5.0,
		MaxLux:        32768.0,
		AutoFloor:     0.05,
		PollInterval:  time.Second,
		Tick:          100 * time.Millisecond,
		AnimationStep: 0.06,
	},
	VendorProcesses: []string{"sysmon", "voice-agent"},
}

// Simulator is the neutral development profile. It keeps the same display
// geometry, PCM formats, memory envelope and brightness curve so the simulator
// renders and mixes like the device, but it has no device paths, no stock
// supplicant files and no watchdog opener.
var Simulator = Profile{
	ID:       "simulator",
	Platform: "simulator",

	Model: Model{
		ModelFallback:  "Capsula Mini (simulator)",
		SoC:            "simulator",
		SoCMatch:       "",
		KernelFallback: "unknown",
	},
	Audio: Audio{
		PlaybackFormat:   s16le,
		PlaybackRateHz:   48000,
		PlaybackChannels: 2,
		CaptureFormat:    s16le,
		CaptureRateHz:    16000,
		CaptureChannels:  1,
	},
	LED: LED{
		WS2812Pixels:      12,
		MultiCtrlChannels: 30,
		SegmentDigits:     4,
		SegmentsPerDigit:  7,
		Gamma:             2.2,
	},
	Architecture: Architecture{
		Target:      "host",
		ABIContains: "linux",
	},
	Memory: Memory{
		PhysicalMiB: 240,
		HeapLimitMB: 80,
		GCPercent:   50,
	},
	Brightness: RK3308Rev4Stock.Brightness,
}
