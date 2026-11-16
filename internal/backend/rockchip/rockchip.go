// Package rockchip implements the Capsula Mini Rockchip (RK3308) stock
// firmware backend.
//
// Evidence for this backend comes from the jailbroken stock firmware image:
// Buildroot rootfs, kernel 4.4.167, PulseAudio system mode with `master` sink
// and `speex`/`mics` sources, microphone-enable GPIO 21 (GPIO0_C5), and evdev
// input under /dev/input.
//
// The device tree in the boot image describes two LED controllers, both driven
// through vendor char devices whose ABI was recovered from the stock
// voice-agent:
//
//   - /dev/topleds: a 36-channel WS2812B SPI chain presented as 12 RGB ring
//     pixels, set with LEDS_WS2812B_IOCTL_MULTI_SET.
//   - /dev/led_multi_ctrl: an IS31FL3236 (i2c 0x3c) with 30 monochrome
//     channels forming a 4x7-segment plus two-colon display, set with
//     LEDS_MULTI_CTRL_IOCTL_MULTI_SET.
//
// Standard Linux LED class devices and evdev are still used as fallbacks when
// the vendor nodes are absent. The vendor frame paths are implemented from the
// recovered ABI but have not yet been validated on physical hardware, so they
// report PARTIAL rather than claiming face parity.
package rockchip

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"capsulad/internal/backend"
	"capsulad/internal/backend/profile"
)

type Config struct {
	// RootPrefix allows inspecting an extracted firmware tree offline.
	RootPrefix string
	// AudioSink and AudioSource are PulseAudio device names discovered from
	// /etc/pulse/device.pa: master is the mixed output, speex the processed
	// wakeword/microphone source.
	AudioSink   string
	AudioSource string
	// AudioPlaybackFormat and AudioCaptureFormat are the PCM formats the
	// firmware clients negotiate. The profile supplies them.
	AudioPlaybackFormat backend.AudioFormat
	AudioCaptureFormat  backend.AudioFormat
	// MicEnableGPIO is the sysfs GPIO number used by S50mic-enable.
	MicEnableGPIO int
	// Model/SoC/kernel fallbacks are reported when the corresponding probe
	// cannot read the device.
	ModelFallback  string
	SoC            string
	SoCMatch       string
	KernelFallback string
	// LED geometry and device paths come from the profile.
	LEDTopPath           string
	LEDMultiCtrlPath     string
	LEDSysfsClassDir     string
	LEDWS2812Channels    int
	LEDWS2812Pixels      int
	LEDMultiCtrlChannels int
	LEDSegmentDigits     int
	LEDSegmentsPerDigit  int
	LEDGamma             float64
	// InputEventDir and InputEventPrefix locate evdev nodes; the keymap
	// selector is the gpio-keys device-tree compatible string.
	InputEventDir         string
	InputEventPrefix      string
	InputKeymapCompatible string
	// Warnf and Infof receive backend diagnostics so the daemon can route them
	// into its log bus (rotated log file and Web UI). Both are optional: a nil
	// hook discards the message, so a Config built for offline inspection or a
	// test never writes to the process's standard logger, which bypasses the
	// daemon's log entirely.
	Warnf func(format string, args ...any)
	Infof func(format string, args ...any)
}

// ConfigFor builds the Rockchip backend configuration from a device profile.
func ConfigFor(p profile.Profile) Config {
	return Config{
		AudioSink:             p.Audio.Sink,
		AudioSource:           p.Audio.Source,
		AudioPlaybackFormat:   backend.AudioFormat{Format: backend.SampleFormat(p.Audio.PlaybackFormat), RateHz: p.Audio.PlaybackRateHz, Channels: p.Audio.PlaybackChannels},
		AudioCaptureFormat:    backend.AudioFormat{Format: backend.SampleFormat(p.Audio.CaptureFormat), RateHz: p.Audio.CaptureRateHz, Channels: p.Audio.CaptureChannels},
		MicEnableGPIO:         p.Audio.MicEnableGPIO,
		ModelFallback:         p.Model.ModelFallback,
		SoC:                   p.Model.SoC,
		SoCMatch:              p.Model.SoCMatch,
		KernelFallback:        p.Model.KernelFallback,
		LEDTopPath:            p.LED.TopLEDsPath,
		LEDMultiCtrlPath:      p.LED.MultiCtrlPath,
		LEDSysfsClassDir:      p.LED.SysfsClassDir,
		LEDWS2812Channels:     p.LED.WS2812Channels,
		LEDWS2812Pixels:       p.LED.WS2812Pixels,
		LEDMultiCtrlChannels:  p.LED.MultiCtrlChannels,
		LEDSegmentDigits:      p.LED.SegmentDigits,
		LEDSegmentsPerDigit:   p.LED.SegmentsPerDigit,
		LEDGamma:              p.LED.Gamma,
		InputEventDir:         p.Input.EventDir,
		InputEventPrefix:      p.Input.EventPrefix,
		InputKeymapCompatible: p.Input.KeymapCompatible,
	}
}

func DefaultConfig() Config { return ConfigFor(profile.RK3308Rev4Stock) }

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

func New(cfg Config) (*Backend, error) {
	def := DefaultConfig()
	if cfg.AudioSink == "" {
		cfg.AudioSink = def.AudioSink
	}
	if cfg.AudioSource == "" {
		cfg.AudioSource = def.AudioSource
	}
	if cfg.AudioPlaybackFormat.RateHz <= 0 || cfg.AudioPlaybackFormat.Channels <= 0 {
		cfg.AudioPlaybackFormat = def.AudioPlaybackFormat
	}
	if cfg.AudioCaptureFormat.RateHz <= 0 || cfg.AudioCaptureFormat.Channels <= 0 {
		cfg.AudioCaptureFormat = def.AudioCaptureFormat
	}
	if cfg.MicEnableGPIO <= 0 {
		cfg.MicEnableGPIO = def.MicEnableGPIO
	}
	if cfg.ModelFallback == "" {
		cfg.ModelFallback = def.ModelFallback
	}
	if cfg.SoC == "" {
		cfg.SoC = def.SoC
	}
	if cfg.SoCMatch == "" {
		cfg.SoCMatch = def.SoCMatch
	}
	if cfg.KernelFallback == "" {
		cfg.KernelFallback = def.KernelFallback
	}
	if cfg.LEDTopPath == "" {
		cfg.LEDTopPath = def.LEDTopPath
	}
	if cfg.LEDMultiCtrlPath == "" {
		cfg.LEDMultiCtrlPath = def.LEDMultiCtrlPath
	}
	if cfg.LEDSysfsClassDir == "" {
		cfg.LEDSysfsClassDir = def.LEDSysfsClassDir
	}
	if cfg.LEDWS2812Channels <= 0 {
		cfg.LEDWS2812Channels = def.LEDWS2812Channels
	}
	if cfg.LEDWS2812Pixels <= 0 {
		cfg.LEDWS2812Pixels = def.LEDWS2812Pixels
	}
	if cfg.LEDMultiCtrlChannels <= 0 {
		cfg.LEDMultiCtrlChannels = def.LEDMultiCtrlChannels
	}
	if cfg.LEDSegmentDigits <= 0 {
		cfg.LEDSegmentDigits = def.LEDSegmentDigits
	}
	if cfg.LEDSegmentsPerDigit <= 0 {
		cfg.LEDSegmentsPerDigit = def.LEDSegmentsPerDigit
	}
	if cfg.LEDGamma <= 0 {
		cfg.LEDGamma = def.LEDGamma
	}
	if cfg.InputEventDir == "" {
		cfg.InputEventDir = def.InputEventDir
	}
	if cfg.InputEventPrefix == "" {
		cfg.InputEventPrefix = def.InputEventPrefix
	}
	if cfg.InputKeymapCompatible == "" {
		cfg.InputKeymapCompatible = def.InputKeymapCompatible
	}
	// S50mic-enable is the authoritative source for the gate GPIO on this
	// firmware; an explicit non-default Config value still wins.
	if gpio, ok := probeMicEnableGPIO(cfg.RootPrefix); ok && cfg.MicEnableGPIO == def.MicEnableGPIO {
		cfg.MicEnableGPIO = gpio
	}
	b := &Backend{cfg: cfg}
	b.info = b.probeInfo()
	dt := probeLEDDeviceTree(cfg.RootPrefix)
	// The live device tree is authoritative. When it is unavailable (offline
	// inspection or an unusual boot), the observed vendor geometry is used so
	// an existing char device is still driven with the recovered ABI.
	if dt.Ring == nil && nodeExists(cfg.RootPrefix, cfg.LEDTopPath) {
		dt.Ring = &ws2812RingNode{DeviceName: filepath.Base(cfg.LEDTopPath), Channels: cfg.LEDWS2812Channels}
	}
	if dt.Segment == nil && nodeExists(cfg.RootPrefix, cfg.LEDMultiCtrlPath) {
		dt.Segment = &multiCtrlNode{Cells: cfg.LEDMultiCtrlChannels}
	}
	ringLEDs, monoLEDs := probeSysfsLEDs(cfg.RootPrefix, cfg.LEDSysfsClassDir, cfg.LEDWS2812Pixels, cfg.LEDMultiCtrlChannels)

	var ringErr error
	if dt.Ring != nil {
		if ring, err := openWS2812BRing(cfg, dt.Ring); err == nil {
			b.ring = ring
			channels := dt.Ring.Channels
			if channels <= 0 {
				channels = cfg.LEDWS2812Channels
			}
			b.caps = append(b.caps, backend.SubsystemStatus{
				Subsystem: "ring", BackendID: "rockchip.vendor.topleds.ws2812b", Status: backend.Partial,
				Detail: fmt.Sprintf("%d-channel WS2812B chain (%s) driven as %d RGB pixels; ioctl ABI recovered from the stock voice-agent, hardware validation pending", channels, cfg.LEDTopPath, channels/3),
			})
		} else {
			ringErr = err
		}
	}
	if b.ring == nil && len(ringLEDs) > 0 {
		if cfg.RootPrefix == "" {
			for _, led := range ringLEDs {
				claimSysfsLED(led)
			}
		}
		b.ring = &sysfsRing{leds: ringLEDs}
		b.caps = append(b.caps, backend.SubsystemStatus{
			Subsystem: "ring", BackendID: "rockchip.sysfs.led", Status: backend.Partial,
			Detail: fmt.Sprintf("%d RGB LED class devices", len(ringLEDs)),
		})
	}
	if b.ring == nil {
		if dt.Ring != nil {
			pixels := dt.Ring.Channels / 3
			if pixels <= 0 {
				pixels = cfg.LEDWS2812Pixels
			}
			b.ring = &unverifiedRing{pixels: pixels}
			b.caps = append(b.caps, backend.SubsystemStatus{
				Subsystem: "ring", BackendID: "rockchip.vendor.topleds.ws2812b", Status: backend.Unverified,
				Detail: fmt.Sprintf("WS2812B ring device present but not driven (%v); frames are kept in memory for diagnostics", ringErr),
			})
		} else {
			b.caps = append(b.caps, backend.SubsystemStatus{Subsystem: "ring", Status: backend.Unavailable, Detail: "no LED ring detected"})
		}
	}

	var segmentErr error
	if dt.Segment != nil {
		if seg, err := openMultiCtrlSegment(cfg, dt.Segment); err == nil {
			b.seg = seg
			cells := dt.Segment.Cells
			if cells <= 0 {
				cells = cfg.LEDMultiCtrlChannels
			}
			b.caps = append(b.caps, backend.SubsystemStatus{
				Subsystem: "segment", BackendID: "rockchip.vendor.led_multi_ctrl", Status: backend.Partial,
				Detail: fmt.Sprintf("%d-channel monochrome display (%s, IS31FL3236) as %dx%d segments plus colon; ioctl ABI recovered from the stock voice-agent, hardware validation pending", cells, cfg.LEDMultiCtrlPath, cfg.LEDSegmentDigits, cfg.LEDSegmentsPerDigit),
			})
		} else {
			segmentErr = err
		}
	}
	if b.seg == nil && len(monoLEDs) >= cfg.LEDSegmentsPerDigit {
		if cfg.RootPrefix == "" {
			for _, led := range monoLEDs {
				claimSysfsLED(led)
			}
		}
		b.seg = &sysfsSegment{leds: monoLEDs, digits: cfg.LEDSegmentDigits, segmentsPerDigit: cfg.LEDSegmentsPerDigit, colonChannels: cfg.LEDMultiCtrlChannels}
		b.caps = append(b.caps, backend.SubsystemStatus{
			Subsystem: "segment", BackendID: "rockchip.sysfs.led", Status: backend.Partial,
			Detail: fmt.Sprintf("%d monochrome LED class devices mapped as %dx%d segments plus colon", len(monoLEDs), cfg.LEDSegmentDigits, cfg.LEDSegmentsPerDigit),
		})
	}
	if b.seg == nil {
		if dt.Segment != nil {
			b.seg = &unverifiedSegment{cells: cfg.LEDSegmentDigits, segmentsPerDigit: cfg.LEDSegmentsPerDigit}
			b.caps = append(b.caps, backend.SubsystemStatus{
				Subsystem: "segment", BackendID: "rockchip.vendor.led_multi_ctrl", Status: backend.Unverified,
				Detail: fmt.Sprintf("IS31FL3236 display present but not driven (%v); frames are kept in memory for diagnostics", segmentErr),
			})
		} else {
			b.caps = append(b.caps, backend.SubsystemStatus{Subsystem: "segment", Status: backend.Unavailable, Detail: "no segment display detected"})
		}
	}

	if hasCommand(cfg.RootPrefix, "pacat") && hasCommand(cfg.RootPrefix, "parec") {
		b.audio = newPulseAudio(cfg)
		b.caps = append(b.caps, backend.SubsystemStatus{
			Subsystem: "audio", BackendID: "rockchip.pulseaudio", Status: backend.Partial,
			Detail: fmt.Sprintf("PulseAudio client pipes (sink=%s, source=%s); latency and format behavior must be validated on hardware", cfg.AudioSink, cfg.AudioSource),
		})
	} else {
		b.caps = append(b.caps, backend.SubsystemStatus{Subsystem: "audio", Status: backend.Unavailable, Detail: "pacat/parec not found"})
	}

	if cfg.RootPrefix == "" {
		if nodeExists(cfg.RootPrefix, "/sys/class/bluetooth/hci0") {
			b.caps = append(b.caps, backend.SubsystemStatus{
				Subsystem: "bluetooth", BackendID: "bluez", Status: backend.Partial,
				Detail: "adapter node present; BlueZ owns profiles and media",
			})
		} else {
			b.caps = append(b.caps, backend.SubsystemStatus{
				Subsystem: "bluetooth", Status: backend.Unavailable,
				Detail: "no Bluetooth adapter node under /sys/class/bluetooth",
			})
		}
	}

	if t := probeEvdev(cfg, probeKeyMap(cfg.RootPrefix, cfg.InputKeymapCompatible)); t != nil {
		b.touch = t
		caps := t.Caps()
		status := backend.Partial
		if caps.Coordinates {
			status = backend.Available
		}
		b.caps = append(b.caps, backend.SubsystemStatus{
			Subsystem: "touch", BackendID: "rockchip.evdev", Status: status,
			Detail: fmt.Sprintf("evdev panel and gpio-keys; coordinates=%v pressure=%v", caps.Coordinates, caps.Pressure),
		})
	} else {
		b.caps = append(b.caps, backend.SubsystemStatus{Subsystem: "touch", Status: backend.Unavailable, Detail: "no evdev panel detected"})
	}

	b.sensors = probeSensors(cfg.RootPrefix)
	if b.sensors != nil {
		ids := make([]string, 0, 3)
		for _, info := range b.sensors.List() {
			ids = append(ids, info.ID)
		}
		b.caps = append(b.caps, backend.SubsystemStatus{
			Subsystem: "sensors", BackendID: "rockchip.iio", Status: backend.Available,
			Detail: "sysfs sensors: " + strings.Join(ids, ", "),
		})
	}

	b.caps = append(b.caps, backend.SubsystemStatus{
		Subsystem: "wakeword", BackendID: "rockchip.vendor", Status: backend.Unverified,
		Detail: "stock voice-agent ships a wakeword model, but no documented engine ABI; framework wakeword stays unavailable until a detector is integrated and tested.",
	})
	return b, nil
}

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
	model := readTrim(b.cfg.RootPrefix + "/proc/device-tree/model")
	if model == "" {
		model = readTrim(b.cfg.RootPrefix + "/sys/firmware/devicetree/base/model")
	}
	if model == "" {
		model = b.cfg.ModelFallback
	}
	kernel := readTrim(b.cfg.RootPrefix + "/proc/version")
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
	if !strings.Contains(strings.ToLower(model), b.cfg.SoCMatch) && model != "" {
		soc = model
	}
	bootID := readTrim(b.cfg.RootPrefix + "/proc/sys/kernel/random/boot_id")
	serial := readTrim(b.cfg.RootPrefix + "/proc/device-tree/serial-number")
	if serial == "" {
		serial = bootID
	}
	return backend.DeviceInfo{
		DeviceID: serial, Model: model, SoC: soc, Kernel: kernel,
		Firmware: firmware, Platform: backend.PlatformRockchipStock,
	}
}

func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\x00\n ")
}

func nodeExists(prefix, path string) bool {
	_, err := os.Stat(prefix + path)
	return err == nil
}

func hasCommand(prefix, name string) bool {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if st, err := os.Stat(filepath.Join(prefix+dir, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	if prefix == "" {
		_, err := exec.LookPath(name)
		return err == nil
	}
	return false
}

// probeMicEnableGPIO reads MIC_EN_GPIO from the stock S50mic-enable script so
// a board revision with a different gate pin is followed instead of the
// compiled-in default.
func probeMicEnableGPIO(prefix string) (int, bool) {
	data, err := os.ReadFile(prefix + "/etc/init.d/S50mic-enable")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		value, ok := strings.CutPrefix(line, "MIC_EN_GPIO=")
		if !ok {
			continue
		}
		if i := strings.IndexByte(value, '#'); i >= 0 {
			value = value[:i]
		}
		gpio, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && gpio >= 0 {
			return gpio, true
		}
	}
	return 0, false
}

// claimSysfsLED disables a kernel trigger before the sysfs fallback writes
// brightness. The stock S59disable_led_animation.sh does the same at boot, but
// capsulad may start earlier or the script may be skipped.
func claimSysfsLED(dir string) {
	trigger := filepath.Join(dir, "trigger")
	if _, err := os.Stat(trigger); err == nil {
		_ = os.WriteFile(trigger, []byte("none"), 0o644)
	}
}

// --- sysfs LED class -------------------------------------------------------

type sysfsRing struct {
	mu   sync.Mutex
	leds []string
}

func (r *sysfsRing) Caps() backend.RingCaps {
	return backend.RingCaps{PixelCount: len(r.leds), SupportsRGB: true}
}
func (r *sysfsRing) ResourceID() string { return "light.ring.main" }
func (r *sysfsRing) WriteRing(f backend.RingFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, path := range r.leds {
		var px backend.RGB
		if i < len(f.Pixels) {
			px = f.Pixels[i]
		}
		lvl := f.Brightness
		if lvl < 0 || lvl > 1 {
			lvl = 1
		}
		val := fmt.Sprintf("%d %d %d", int(float64(px.R)*lvl), int(float64(px.G)*lvl), int(float64(px.B)*lvl))
		if err := os.WriteFile(filepath.Join(path, "multi_intensity"), []byte(val), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type sysfsSegment struct {
	mu               sync.Mutex
	leds             []string
	digits           int
	segmentsPerDigit int
	colonChannels    int
}

func (s *sysfsSegment) Caps() backend.SegmentCaps {
	return backend.SegmentCaps{DigitCount: s.digits, SupportsColon: len(s.leds) >= s.colonChannels, SupportsDecimal: false, SegmentsPerDigit: s.segmentsPerDigit}
}
func (s *sysfsSegment) ResourceID() string { return "display.segment.front" }
func (s *sysfsSegment) WriteSegment(f backend.SegmentFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for cell := range s.digits {
		var mask uint16
		if cell < len(f.Cells) {
			mask = f.Cells[cell]
		}
		for seg := range s.segmentsPerDigit {
			idx := cell*s.segmentsPerDigit + seg
			if idx >= len(s.leds) {
				break
			}
			on := mask&(1<<uint(seg)) != 0
			v := "0"
			if on {
				v = strconv.Itoa(int(f.Brightness * 255))
			}
			if err := os.WriteFile(filepath.Join(s.leds[idx], "brightness"), []byte(v), 0o644); err != nil {
				return err
			}
		}
	}
	if len(s.leds) >= s.colonChannels {
		v := "0"
		if f.Colon {
			v = strconv.Itoa(int(f.Brightness * 255))
		}
		for _, idx := range []int{s.digits * s.segmentsPerDigit, s.digits*s.segmentsPerDigit + 1} {
			_ = os.WriteFile(filepath.Join(s.leds[idx], "brightness"), []byte(v), 0o644)
		}
	}
	return nil
}

// naturalLess orders LED class device names by their numeric suffix so that
// top-led2 precedes top-led10. The kernel names stock channels led1..led36.
func naturalLess(a, b string) bool {
	pa, na := splitNumericSuffix(a)
	pb, nb := splitNumericSuffix(b)
	if pa == pb && na != nb {
		return na < nb
	}
	return a < b
}

func splitNumericSuffix(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, 0
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}

// probeSysfsLEDs enumerates /sys/class/leds. Devices exposing multi_intensity
// are treated as RGB pixels; the rest as monochrome display LEDs. Ordering is
// natural by channel number, which matches the firmware's ledN declaration.
func probeSysfsLEDs(prefix, classDir string, ringCap, monoCap int) (ring, mono []string) {
	entries, err := os.ReadDir(filepath.Join(prefix, classDir))
	if err != nil {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	for _, name := range names {
		dir := filepath.Join(prefix, classDir, name)
		if _, err := os.Stat(filepath.Join(dir, "multi_intensity")); err == nil {
			ring = append(ring, dir)
			continue
		}
		// The stock ws2812b driver registers its 36 channels as top-ledN.
		// Those are ring channels, not monochrome display cells; writing them
		// as a segment frame would misdrive the ring.
		if strings.HasPrefix(name, "top-led") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "brightness")); err == nil {
			mono = append(mono, dir)
		}
	}
	if ringCap > 0 && len(ring) > ringCap {
		ring = ring[:ringCap]
	}
	if monoCap > 0 && len(mono) > monoCap {
		mono = mono[:monoCap]
	}
	return ring, mono
}

// --- unverified vendor nodes ----------------------------------------------

type unverifiedRing struct {
	mu     sync.Mutex
	pixels int
	last   []backend.RGB
}

func (r *unverifiedRing) Caps() backend.RingCaps {
	return backend.RingCaps{PixelCount: r.pixels, SupportsRGB: true}
}
func (r *unverifiedRing) ResourceID() string { return "light.ring.main" }
func (r *unverifiedRing) WriteRing(f backend.RingFrame) error {
	r.mu.Lock()
	r.last = append([]backend.RGB(nil), f.Pixels...)
	r.mu.Unlock()
	return nil
}

type unverifiedSegment struct {
	mu               sync.Mutex
	cells            int
	segmentsPerDigit int
	last             backend.SegmentFrame
}

func (s *unverifiedSegment) Caps() backend.SegmentCaps {
	return backend.SegmentCaps{DigitCount: s.cells, SupportsColon: true, SupportsDecimal: false, SegmentsPerDigit: s.segmentsPerDigit}
}
func (s *unverifiedSegment) ResourceID() string { return "display.segment.front" }
func (s *unverifiedSegment) WriteSegment(f backend.SegmentFrame) error {
	s.mu.Lock()
	s.last = f
	s.mu.Unlock()
	return nil
}
