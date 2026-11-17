// Package amlogic implements the Capsula Mini Amlogic A113X backends.
//
// Two userlands run on the same board and share one device family, so this
// package owns the probe and the frame path while the profile decides which of
// them applies:
//
//   - the jailbroken stock Buildroot image, where the vendor voice-agent owns
//     the ring (/dev/topleds), the clock panel (/dev/is31fl32xx) and the
//     hardware watchdog, and audio is PulseAudio;
//   - Alpine Linux, where the vendor stack is absent: the panel is a kernel
//     line-display (or per-segment LED class) device, the ring is still the
//     WS2812B chain, and audio is ALSA.
//
// Evidence for the board: the published hardware survey (cmp18, 2026-08-23),
// ryysel/marusya-hw (device paths and input) and ryysel/capsula-mini-airplay2
// (the recovered 36-channel WS2812B ABI and channel map used by the stock
// voice-agent). The clock panel's vendor char-device ABI is not recovered, so
// it is reported UNVERIFIED instead of guessed; a mainline line display is
// driven through its own map_seg7 table.
package amlogic

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"capsulad/internal/backend"
	"capsulad/internal/backend/linuxdev"
	"capsulad/internal/backend/profile"
)

// Config is the Amlogic backend configuration. Every value comes from the
// device profile; the composition root only adds the log hooks.
type Config struct {
	// Platform is the wire platform this configuration implements.
	Platform backend.Platform

	// RootPrefix allows inspecting an extracted firmware tree offline.
	RootPrefix string

	// AudioBackend selects the userland audio path: "pulse" (stock) or "alsa"
	// (Alpine).
	AudioBackend string
	// AudioSink/AudioSource are PulseAudio device names; empty targets the
	// PulseAudio defaults.
	AudioSink   string
	AudioSource string
	// PCMDevice, MixerCard, MixerVolume and MixerMute are the ALSA knobs.
	PCMDevice   string
	MixerCard   string
	MixerVolume string
	MixerMute   string
	// MicEnableGPIO gates the microphone array in hardware when the board
	// exposes it; zero means no gate.
	MicEnableGPIO int

	AudioPlaybackFormat backend.AudioFormat
	AudioCaptureFormat  backend.AudioFormat

	// Model/SoC/kernel fallbacks are reported when the corresponding probe
	// cannot read the device.
	ModelFallback  string
	SoC            string
	SoCMatch       string
	KernelFallback string

	// LED geometry, device paths and ring curve come from the profile.
	LEDTopPath           string
	LEDMultiCtrlPath     string
	LEDSysfsClassDir     string
	SPIDevPath           string
	SPISpeedHz           int
	SPIBitsPerWord       int
	SPIPrimeHz           int
	LEDWS2812Channels    int
	LEDWS2812Pixels      int
	LEDMultiCtrlChannels int
	LEDSegmentDigits     int
	LEDSegmentsPerDigit  int
	LEDGamma             float64

	// SegmentLEDPattern/SegmentLetters name per-segment LED class devices.
	// SegmentChannelPrefix/SegmentChannelMap name a fixed channel array instead.
	// ColonTopLED/ColonBottomLED name the colon dots in either layout.
	SegmentLEDPattern    string
	SegmentLetters       string
	SegmentChannelPrefix string
	SegmentChannelMap    []int
	ColonTopLED          string
	ColonBottomLED       string

	// InputEventDir and InputEventPrefix locate evdev nodes; the keymap
	// selector is the gpio-keys device-tree compatible string.
	InputEventDir         string
	InputEventPrefix      string
	InputKeymapCompatible string
	// InputKeyOverrides is merged over the probed device-tree keymap.
	InputKeyOverrides map[int]string

	// Warnf and Infof receive backend diagnostics so the daemon can route them
	// into its log bus. Both are optional: a nil hook discards the message.
	Warnf func(format string, args ...any)
	Infof func(format string, args ...any)
}

// ConfigFor builds the Amlogic backend configuration from a device profile.
func ConfigFor(p profile.Profile) Config {
	cfg := Config{
		RootPrefix:            "",
		AudioSink:             p.Audio.Sink,
		AudioSource:           p.Audio.Source,
		PCMDevice:             p.Audio.PCMDevice,
		MixerCard:             p.Audio.MixerCard,
		MixerVolume:           p.Audio.MixerVolume,
		MixerMute:             p.Audio.MixerMute,
		MicEnableGPIO:         p.Audio.MicEnableGPIO,
		AudioPlaybackFormat:   audioFormat(p.Audio.PlaybackFormat, p.Audio.PlaybackRateHz, p.Audio.PlaybackChannels),
		AudioCaptureFormat:    audioFormat(p.Audio.CaptureFormat, p.Audio.CaptureRateHz, p.Audio.CaptureChannels),
		ModelFallback:         p.Model.ModelFallback,
		SoC:                   p.Model.SoC,
		SoCMatch:              p.Model.SoCMatch,
		KernelFallback:        p.Model.KernelFallback,
		LEDTopPath:            p.LED.TopLEDsPath,
		LEDMultiCtrlPath:      p.LED.MultiCtrlPath,
		LEDSysfsClassDir:      p.LED.SysfsClassDir,
		SPIDevPath:            p.LED.SPIDevPath,
		SPISpeedHz:            p.LED.SPISpeedHz,
		SPIBitsPerWord:        p.LED.SPIBitsPerWord,
		SPIPrimeHz:            p.LED.SPIPrimeHz,
		LEDWS2812Channels:     p.LED.WS2812Channels,
		LEDWS2812Pixels:       p.LED.WS2812Pixels,
		LEDMultiCtrlChannels:  p.LED.MultiCtrlChannels,
		LEDSegmentDigits:      p.LED.SegmentDigits,
		LEDSegmentsPerDigit:   p.LED.SegmentsPerDigit,
		LEDGamma:              p.LED.Gamma,
		SegmentLEDPattern:     p.LED.SegmentLEDPattern,
		SegmentLetters:        p.LED.SegmentLetters,
		SegmentChannelPrefix:  p.LED.SegmentChannelPrefix,
		SegmentChannelMap:     append([]int(nil), p.LED.SegmentChannelMap...),
		ColonTopLED:           p.LED.ColonTopLED,
		ColonBottomLED:        p.LED.ColonBottomLED,
		InputEventDir:         p.Input.EventDir,
		InputEventPrefix:      p.Input.EventPrefix,
		InputKeymapCompatible: p.Input.KeymapCompatible,
		InputKeyOverrides:     mergeKeys(p.Input.KeyOverrides),
	}
	switch p.Platform {
	case "amlogic-alpine":
		cfg.Platform = backend.PlatformAmlogicAlpine
		cfg.AudioBackend = "alsa"
	default:
		cfg.Platform = backend.PlatformAmlogicStock
		cfg.AudioBackend = "pulse"
	}
	return cfg
}

func audioFormat(format string, rate, channels int) backend.AudioFormat {
	return backend.AudioFormat{Format: backend.SampleFormat(format), RateHz: rate, Channels: channels}
}

// Backend is one probed Amlogic board.
type Backend struct {
	cfg     Config
	info    backend.DeviceInfo
	caps    []backend.SubsystemStatus
	seg     backend.SegmentDevice
	ring    backend.RingDevice
	audio   backend.AudioDevice
	touch   backend.TouchDevice
	sensors backend.Sensors
}

// New probes the configured board. Every device may be nil; the capability
// list is the authoritative availability report.
func New(cfg Config) (*Backend, error) {
	cfg = withDefaults(cfg)
	b := &Backend{cfg: cfg}
	b.info = b.probeInfo()
	b.ring, b.caps = openRing(cfg, b.caps)
	b.seg, b.caps = openPanel(cfg, b.caps)

	audio, audioStatus := newAudio(cfg)
	b.audio = audio
	b.caps = append(b.caps, audioStatus)

	b.caps = append(b.caps, probeBluetooth(cfg))

	keys := linuxdev.ProbeKeyMap(cfg.RootPrefix, cfg.InputKeymapCompatible)
	for code, name := range cfg.InputKeyOverrides {
		keys[code] = name
	}
	if t := linuxdev.ProbeEvdev(cfg.RootPrefix, cfg.InputEventDir, cfg.InputEventPrefix, keys); t != nil {
		b.touch = t
		caps := t.Caps()
		status := backend.Partial
		if caps.Coordinates {
			status = backend.Available
		}
		b.caps = append(b.caps, backend.SubsystemStatus{
			Subsystem: "touch", BackendID: "amlogic.evdev", Status: status,
			Detail: fmt.Sprintf("evdev keys from the gpio-keys node; coordinates=%v pressure=%v", caps.Coordinates, caps.Pressure),
		})
	} else {
		b.caps = append(b.caps, backend.SubsystemStatus{Subsystem: "touch", Status: backend.Unavailable, Detail: "no evdev input node detected"})
	}

	b.sensors = linuxdev.ProbeSensors(cfg.RootPrefix)
	if b.sensors != nil {
		ids := make([]string, 0, 3)
		for _, info := range b.sensors.List() {
			ids = append(ids, info.ID)
		}
		b.caps = append(b.caps, backend.SubsystemStatus{
			Subsystem: "sensors", BackendID: "amlogic.iio", Status: backend.Available,
			Detail: "sysfs sensors: " + strings.Join(ids, ", "),
		})
	}

	b.caps = append(b.caps, backend.SubsystemStatus{
		Subsystem: "wakeword", BackendID: "amlogic.vendor", Status: backend.Unverified,
		Detail: "the board has a four-microphone PDM array, but no documented on-device wakeword engine ABI; framework wakeword stays unavailable until a detector is integrated and tested.",
	})
	return b, nil
}

func withDefaults(cfg Config) Config {
	if cfg.Platform == "" {
		cfg.Platform = backend.PlatformAmlogicStock
	}
	if cfg.AudioBackend == "" {
		if cfg.Platform == backend.PlatformAmlogicAlpine {
			cfg.AudioBackend = "alsa"
		} else {
			cfg.AudioBackend = "pulse"
		}
	}
	if cfg.AudioPlaybackFormat.RateHz <= 0 || cfg.AudioPlaybackFormat.Channels <= 0 {
		cfg.AudioPlaybackFormat = backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
	}
	if cfg.AudioCaptureFormat.RateHz <= 0 || cfg.AudioCaptureFormat.Channels <= 0 {
		cfg.AudioCaptureFormat = backend.AudioFormat{Format: backend.S16LE, RateHz: 16000, Channels: 1}
	}
	if cfg.LEDTopPath == "" {
		cfg.LEDTopPath = "/dev/topleds"
	}
	if cfg.LEDSysfsClassDir == "" {
		cfg.LEDSysfsClassDir = "/sys/class/leds"
	}
	if cfg.LEDWS2812Channels <= 0 {
		cfg.LEDWS2812Channels = 36
	}
	if cfg.LEDWS2812Pixels <= 0 {
		cfg.LEDWS2812Pixels = 12
	}
	if cfg.LEDMultiCtrlChannels <= 0 {
		cfg.LEDMultiCtrlChannels = 30
	}
	if cfg.LEDSegmentDigits <= 0 {
		cfg.LEDSegmentDigits = 4
	}
	if cfg.LEDSegmentsPerDigit <= 0 {
		cfg.LEDSegmentsPerDigit = 7
	}
	if cfg.LEDGamma <= 0 {
		cfg.LEDGamma = 2.2
	}
	if cfg.InputEventDir == "" {
		cfg.InputEventDir = "/dev/input"
	}
	if cfg.InputEventPrefix == "" {
		cfg.InputEventPrefix = "event"
	}
	if cfg.ModelFallback == "" {
		cfg.ModelFallback = "VK Capsula Mini Amlogic 113X (model probe unavailable)"
	}
	if cfg.SoC == "" {
		cfg.SoC = "Amlogic A113X"
	}
	if cfg.SoCMatch == "" {
		cfg.SoCMatch = "a113"
	}
	if cfg.KernelFallback == "" {
		cfg.KernelFallback = "unknown"
	}
	return cfg
}

// DefaultConfig builds the stock-firmware configuration.
func DefaultConfig() Config { return ConfigFor(profile.AmlogicA113XStock) }

func (b *Backend) Platform() backend.Platform { return b.info.Platform }
func (b *Backend) Info() backend.DeviceInfo   { return b.info }
func (b *Backend) Capabilities() []backend.SubsystemStatus {
	return append([]backend.SubsystemStatus(nil), b.caps...)
}
func (b *Backend) Segment() backend.SegmentDevice { return b.seg }
func (b *Backend) Ring() backend.RingDevice       { return b.ring }
func (b *Backend) Audio() backend.AudioDevice     { return b.audio }
func (b *Backend) Touch() backend.TouchDevice     { return b.touch }
func (b *Backend) Sensors() backend.Sensors       { return b.sensors }

func (b *Backend) Close() error {
	if c, ok := b.ring.(interface{ Close() error }); ok {
		c.Close()
	}
	if c, ok := b.seg.(interface{ Close() error }); ok {
		c.Close()
	}
	if b.audio != nil {
		b.audio.Close()
	}
	if b.touch != nil {
		b.touch.Close()
	}
	if b.sensors != nil {
		b.sensors.Close()
	}
	return nil
}

func (b *Backend) probeInfo() backend.DeviceInfo {
	model := linuxdev.ReadTrim(b.cfg.RootPrefix + "/proc/device-tree/model")
	if model == "" {
		model = linuxdev.ReadTrim(b.cfg.RootPrefix + "/sys/firmware/devicetree/base/model")
	}
	modelKnown := model != ""
	if !modelKnown {
		model = b.cfg.ModelFallback
	}
	kernel := linuxdev.ReadTrim(b.cfg.RootPrefix + "/proc/version")
	if kernel == "" {
		kernel = b.cfg.KernelFallback
	}
	firmware := ""
	if f, err := os.Open(b.cfg.RootPrefix + "/etc/os-release"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "PRETTY_NAME=") {
				firmware = strings.Trim(strings.TrimPrefix(sc.Text(), "PRETTY_NAME="), `"`)
			}
		}
		f.Close()
	}
	soc := b.cfg.SoC
	if modelKnown && !strings.Contains(strings.ToLower(model), b.cfg.SoCMatch) {
		// An unreadable probe reports the fallback, not a board identity; only a
		// real device-tree model can replace the configured SoC name.
		soc = model
	}
	bootID := linuxdev.ReadTrim(b.cfg.RootPrefix + "/proc/sys/kernel/random/boot_id")
	serial := linuxdev.ReadTrim(b.cfg.RootPrefix + "/proc/device-tree/serial-number")
	if serial == "" {
		serial = bootID
	}
	return backend.DeviceInfo{
		DeviceID: serial, Model: model, SoC: soc, Kernel: kernel,
		Firmware: firmware, Platform: b.cfg.Platform,
	}
}

// probeBluetooth reports the adapter node. BlueZ (stock) or BlueALSA (Alpine)
// owns the profiles either way; the framework never claims the radio itself.
func probeBluetooth(cfg Config) backend.SubsystemStatus {
	if cfg.RootPrefix != "" {
		return backend.SubsystemStatus{Subsystem: "bluetooth", Status: backend.Unavailable, Detail: "offline firmware root"}
	}
	if linuxdev.NodeExists(cfg.RootPrefix, "/sys/class/bluetooth/hci0") {
		return backend.SubsystemStatus{
			Subsystem: "bluetooth", BackendID: "bluez", Status: backend.Partial,
			Detail: "adapter node present; BlueZ/BlueALSA owns profiles and media",
		}
	}
	return backend.SubsystemStatus{
		Subsystem: "bluetooth", Status: backend.Unavailable,
		Detail: "no Bluetooth adapter node under /sys/class/bluetooth",
	}
}

// openRing selects the ring driver: the vendor WS2812B char device first, then
// LED class pixels, then an in-memory frame for diagnostics.
func openRing(cfg Config, caps []backend.SubsystemStatus) (backend.RingDevice, []backend.SubsystemStatus) {
	if cfg.LEDTopPath != "" && cfg.RootPrefix == "" && linuxdev.NodeExists(cfg.RootPrefix, cfg.LEDTopPath) {
		if ring, err := linuxdev.OpenWS2812Ring(cfg.LEDTopPath, cfg.LEDWS2812Channels, linuxdev.WS2812ChannelMap[:], cfg.LEDGamma); err == nil {
			return ring, append(caps, backend.SubsystemStatus{
				Subsystem: "ring", BackendID: "amlogic.vendor.topleds.ws2812b", Status: backend.Partial,
				Detail: fmt.Sprintf("%d-channel WS2812B chain (%s) driven as %d RGB pixels with the channel map recovered from the stock voice-agent; ioctl ABI and frame order need one hardware check", cfg.LEDWS2812Channels, cfg.LEDTopPath, cfg.LEDWS2812Channels/3),
			})
		} else if cfg.Warnf != nil {
			cfg.Warnf("vendor ring %s: %v", cfg.LEDTopPath, err)
		}
	}
	spiReported := false
	if cfg.SPIDevPath != "" && cfg.RootPrefix == "" {
		if ring, err := linuxdev.OpenSPIRing(cfg.SPIDevPath, cfg.LEDWS2812Pixels, cfg.SPISpeedHz, cfg.SPIBitsPerWord, cfg.SPIPrimeHz, cfg.LEDGamma); err == nil {
			return ring, append(caps, backend.SubsystemStatus{
				Subsystem: "ring", BackendID: "amlogic.spidev.ws2812b", Status: backend.Partial,
				Detail: fmt.Sprintf("%d-pixel WS2812B chain on %s requested at %d Hz (configured readback %d Hz; the achieved divider is not observable from userspace), four SPI bits per symbol with the image's own symbol/bit order and per-word byte reversal for %d-bit words; a primer transfer runs before each frame so the divider is reprogrammed (no driver rebind), and pixel order and sustained-frame timing still need one unit check", cfg.LEDWS2812Pixels, cfg.SPIDevPath, cfg.SPISpeedHz, ring.ConfiguredSpeedHz(), cfg.SPIBitsPerWord),
			})
		} else {
			spiReported = true
			caps = append(caps, backend.SubsystemStatus{
				Subsystem: "ring", BackendID: "amlogic.spidev.ws2812b", Status: backend.Unverified,
				Detail: fmt.Sprintf("spidev ring %s present but not driven: %v; no ring resource is published for this path", cfg.SPIDevPath, err),
			})
			if cfg.Warnf != nil {
				cfg.Warnf("spidev ring %s: %v", cfg.SPIDevPath, err)
			}
		}
	}
	ringLEDs, _ := linuxdev.ProbeSysfsLEDs(cfg.RootPrefix, cfg.LEDSysfsClassDir, cfg.LEDWS2812Pixels, 0, []string{"clock"})
	if len(ringLEDs) > 0 {
		if cfg.RootPrefix == "" {
			for _, led := range ringLEDs {
				linuxdev.ClaimLED(led)
			}
		}
		return linuxdev.NewSysfsRing(ringLEDs), append(caps, backend.SubsystemStatus{
			Subsystem: "ring", BackendID: "amlogic.sysfs.led", Status: backend.Partial,
			Detail: fmt.Sprintf("%d RGB LED class devices", len(ringLEDs)),
		})
	}
	if cfg.LEDTopPath != "" && linuxdev.NodeExists(cfg.RootPrefix, cfg.LEDTopPath) {
		// The node exists but this process cannot drive it (offline tree or a
		// failed open). Publish no ring resource: an app must not route frames
		// into a no-op sink.
		return nil, append(caps, backend.SubsystemStatus{
			Subsystem: "ring", BackendID: "amlogic.vendor.topleds.ws2812b", Status: backend.Unverified,
			Detail: "WS2812B ring device present but not driven (offline tree or the node could not be opened); no ring resource is published",
		})
	}
	if spiReported {
		return nil, caps
	}
	return nil, append(caps, backend.SubsystemStatus{Subsystem: "ring", Status: backend.Unavailable, Detail: "no LED ring detected"})
}

var _ backend.Backend = (*Backend)(nil)

// pathFor roots an absolute device path against the configuration prefix.
func pathFor(prefix, path string) string {
	if prefix == "" {
		return path
	}
	return filepath.Join(prefix, path)
}

// mergeKeys copies a profile key override map so the backend never aliases the
// inert profile data.
func mergeKeys(in map[int]string) map[int]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int]string, len(in))
	for code, name := range in {
		out[code] = name
	}
	return out
}
