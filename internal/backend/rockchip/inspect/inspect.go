// Package inspect inspects an extracted Capsula firmware tree offline.
//
// It is deliberately evidence-based: it reports what the image contains
// (devices, init scripts, PulseAudio topology, LED animation geometry) and
// marks vendor interfaces whose behavior has not been observed on a device as
// partial or unverified. It never claims hardware behavior that has not been
// observed on a device, and it identifies the platform before applying any
// board-specific interpretation.
package inspect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"capsulad/internal/backend"
	"capsulad/internal/backend/profile"
)

type Report struct {
	Root          string                    `json:"root"`
	Platform      string                    `json:"platform"`
	Model         string                    `json:"model"`
	SoC           string                    `json:"soc"`
	Firmware      string                    `json:"firmware"`
	VendorDaemons []string                  `json:"vendor_daemons"`
	InitScripts   []string                  `json:"init_scripts"`
	PulseSinks    []string                  `json:"pulse_sinks"`
	PulseSources  []string                  `json:"pulse_sources"`
	DeviceNodes   []string                  `json:"device_nodes"`
	MicEnableGPIO int                       `json:"mic_enable_gpio"`
	RingPixels    int                       `json:"ring_pixels"`
	DisplayLEDs   int                       `json:"display_leds"`
	Statuses      []backend.SubsystemStatus `json:"subsystems"`
	Evidence      []string                  `json:"evidence"`
}

var (
	sinkRe   = regexp.MustCompile(`sink_name=([a-zA-Z0-9_.-]+)`)
	sourceRe = regexp.MustCompile(`source_name=([a-zA-Z0-9_.-]+)`)
	gpioRe   = regexp.MustCompile(`MIC_EN_GPIO=([0-9]+)`)
)

// Platform identifiers reported by the inspector. An image without a marker is
// reported as unknown rather than assumed to be the Rockchip board.
const (
	PlatformRockchipStock = "rockchip-stock"
	PlatformAmlogicStock  = "amlogic-stock"
	PlatformAmlogicAlpine = "amlogic-alpine"
	PlatformUnknown       = "unknown"
)

// detectPlatform classifies a rootfs by the init and userland files each
// published image carries. The device tree lives in the boot partition, not in
// the rootfs, so the init scripts and the package manager are the evidence
// available offline.
func detectPlatform(root string) string {
	switch {
	case exists(filepath.Join(root, "etc/alpine-release")) || exists(filepath.Join(root, "sbin/openrc-run")):
		return PlatformAmlogicAlpine
	case exists(filepath.Join(root, "etc/init.d/S02datamount")) ||
		exists(filepath.Join(root, "etc/init.d/S04overlaymount")) ||
		exists(filepath.Join(root, "etc/init.d/S98micro_ui.sh")) ||
		exists(filepath.Join(root, "dev/is31fl32xx")):
		return PlatformAmlogicStock
	case exists(filepath.Join(root, "etc/init.d/S23overlayfs.sh")) ||
		exists(filepath.Join(root, "etc/init.d/S99btctl.sh")) ||
		exists(filepath.Join(root, "usr/share/voice-agent/led")):
		return PlatformRockchipStock
	}
	return PlatformUnknown
}

func Inspect(root string) (*Report, error) {
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("firmware root %q is not a directory", root)
	}
	r := &Report{Root: root, Platform: detectPlatform(root)}
	if data, err := os.ReadFile(filepath.Join(root, "etc/os-release")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				r.Firmware = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "proc/device-tree/model")); err == nil {
		r.Model = strings.TrimRight(string(data), "\x00\n ")
	}
	// The platform decides the SoC name, the model fallback and the static
	// evidence; the probe results below only fill the rest of the report.
	switch r.Platform {
	case PlatformAmlogicStock:
		r.SoC = profile.AmlogicA113XStock.Model.SoC
		if r.Model == "" {
			r.Model = profile.AmlogicA113XStock.Model.ImageModelFallback
		}
		r.Evidence = append(r.Evidence,
			"Amlogic A113X init sequence (S02datamount/S04overlaymount): read-only /rom rootfs with a /data overlay",
			"clock panel is an ISSI IS31FL3236A or Awinic AW21036 on the vendor char device; the ABI is unrecovered")
	case PlatformAmlogicAlpine:
		r.SoC = profile.AmlogicA113XAlpine.Model.SoC
		if r.Model == "" {
			r.Model = profile.AmlogicA113XAlpine.Model.ImageModelFallback
		}
		r.Evidence = append(r.Evidence,
			"Alpine Linux rootfs: no vendor assistant stack; the kernel exposes the panel and ring",
			"audio is ALSA (with BlueALSA for Bluetooth sources), not PulseAudio")
	case PlatformRockchipStock:
		r.SoC = profile.RK3308Rev4Stock.Model.SoC
		if r.Model == "" {
			r.Model = profile.RK3308Rev4Stock.Model.ImageModelFallback
		}
		r.Evidence = append(r.Evidence, "kernel from boot partition: Linux 4.4.167 aarch64 (Buildroot)")
	default:
		r.SoC = "unknown"
		if r.Model == "" {
			r.Model = "unknown (no device-tree model and no recognised init scripts)"
		}
		r.Evidence = append(r.Evidence, "no supported platform marker found; no board-specific mapping was applied")
	}

	initDir := filepath.Join(root, "etc/init.d")
	if entries, err := os.ReadDir(initDir); err == nil {
		for _, e := range entries {
			r.InitScripts = append(r.InitScripts, e.Name())
		}
		sort.Strings(r.InitScripts)
	}
	for _, d := range []string{"voice-agent", "stereoctl", "okcall", "telemetryd", "sysmon", "btctl", "snapclient", "snapserver"} {
		if exists(filepath.Join(root, "usr/bin", d)) || exists(filepath.Join(root, "usr/bin", d+".lzma")) {
			r.VendorDaemons = append(r.VendorDaemons, d)
		}
	}
	if data, err := os.ReadFile(filepath.Join(initDir, "S50mic-enable")); err == nil {
		if m := gpioRe.FindStringSubmatch(string(data)); len(m) == 2 {
			r.MicEnableGPIO, _ = strconv.Atoi(m[1])
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "etc/pulse/device.pa")); err == nil {
		r.PulseSinks = unique(sinkRe.FindAllStringSubmatch(string(data), -1))
		r.PulseSources = unique(sourceRe.FindAllStringSubmatch(string(data), -1))
		r.Evidence = append(r.Evidence, "PulseAudio system mode with virtual sinks for alarm/ring/call/purr and processed microphone sources")
	}
	for _, node := range []string{"/dev/led_multi_ctrl", "/dev/topleds", "/dev/is31fl32xx", "/dev/vendor_storage"} {
		if exists(filepath.Join(root, node)) {
			r.DeviceNodes = append(r.DeviceNodes, node)
		}
	}
	// The vendor nodes are usually created by mdev at boot, so absence in the
	// extracted image is expected; record the init/binary evidence instead.
	vendorNodes := []string{"/dev/led_multi_ctrl", "/dev/topleds"}
	if r.Platform == PlatformAmlogicStock {
		vendorNodes = append(vendorNodes, "/dev/is31fl32xx")
	}
	for _, node := range vendorNodes {
		if !contains(r.DeviceNodes, node) && exists(filepath.Join(root, "usr/bin/voice-agent")) {
			r.DeviceNodes = append(r.DeviceNodes, node+" (referenced by voice-agent)")
		}
	}
	ledDir := filepath.Join(root, "usr/share/voice-agent/led")
	if entries, err := os.ReadDir(ledDir); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(ledDir, e.Name()))
			if err != nil {
				continue
			}
			var frames []map[string]any
			if json.Unmarshal(data, &frames) != nil || len(frames) == 0 {
				continue
			}
			if colors, ok := frames[0]["colors"].([]any); ok && len(colors) > r.RingPixels {
				r.RingPixels = len(colors)
			}
			if brightness, ok := frames[0]["brightness"].([]any); ok && len(brightness) > r.DisplayLEDs {
				r.DisplayLEDs = len(brightness)
			}
			if r.RingPixels > 0 && r.DisplayLEDs > 0 {
				break
			}
		}
	}
	if r.RingPixels > 0 {
		r.Evidence = append(r.Evidence, fmt.Sprintf("vendor LED animations: %d RGB ring pixels", r.RingPixels))
	}
	if r.DisplayLEDs > 0 {
		r.Evidence = append(r.Evidence, fmt.Sprintf("vendor LED animations: %d monochrome display LEDs", r.DisplayLEDs))
	}

	switch r.Platform {
	case PlatformAmlogicStock:
		r.Statuses = amlogicStockStatuses(r)
	case PlatformAmlogicAlpine:
		r.Statuses = amlogicAlpineStatuses(r)
	case PlatformRockchipStock:
		r.Statuses = rockchipStatuses(r)
	default:
		r.Statuses = []backend.SubsystemStatus{
			{Subsystem: "platform", Status: backend.Unverified, Detail: "rootfs does not match the Rockchip stock, Amlogic stock or Amlogic Alpine image markers"},
		}
	}
	return r, nil
}

func rockchipStatuses(r *Report) []backend.SubsystemStatus {
	return []backend.SubsystemStatus{
		{Subsystem: "ring", BackendID: "rockchip.vendor.topleds.ws2812b", Status: backend.Partial,
			Detail: "12 RGB pixels via the 36-channel WS2812B chain on /dev/topleds; ioctl ABI recovered from voice-agent, hardware validation pending"},
		{Subsystem: "segment", BackendID: "rockchip.vendor.led_multi_ctrl", Status: backend.Partial,
			Detail: "30 monochrome LEDs (4x7 segments plus colon) via the IS31FL3236 on /dev/led_multi_ctrl; ioctl ABI recovered from voice-agent, hardware validation pending"},
		{Subsystem: "audio", BackendID: "rockchip.pulseaudio", Status: backend.Partial,
			Detail: "pacat/parec available; master sink and speex source from device.pa"},
		{Subsystem: "touch", BackendID: "rockchip.evdev", Status: backend.Partial,
			Detail: "evdev input under /dev/input; raw coordinate availability must be probed"},
		{Subsystem: "wakeword", BackendID: "rockchip.vendor", Status: backend.Unverified,
			Detail: "stock voice-agent model exists; no documented engine ABI"},
		{Subsystem: "mic_gate", BackendID: "rockchip.gpio", Status: backend.Partial,
			Detail: fmt.Sprintf("microphone enable GPIO %d per S50mic-enable", r.MicEnableGPIO)},
	}
}

func amlogicStockStatuses(r *Report) []backend.SubsystemStatus {
	ringPixels := r.RingPixels
	if ringPixels == 0 {
		ringPixels = 12
	}
	return []backend.SubsystemStatus{
		{Subsystem: "ring", BackendID: "amlogic.vendor.topleds.ws2812b", Status: backend.Partial,
			Detail: fmt.Sprintf("%d RGB pixels via the 36-channel WS2812B chain on /dev/topleds; ioctl ABI and channel map recovered from the stock voice-agent, one hardware check pending", ringPixels)},
		{Subsystem: "segment", BackendID: "amlogic.vendor.is31fl32xx", Status: backend.Unverified,
			Detail: "clock panel (IS31FL3236A or AW21036) on the vendor char device; the ioctl ABI is unrecovered, so frames are not driven"},
		{Subsystem: "audio", BackendID: "amlogic.pulseaudio", Status: backend.Partial,
			Detail: "PulseAudio system mode; no published device.pa names, so the default sink/source are used"},
		{Subsystem: "touch", BackendID: "amlogic.evdev", Status: backend.Partial,
			Detail: "evdev keys from the gpio-keys node; no coordinate panel"},
		{Subsystem: "sensors", BackendID: "amlogic.iio", Status: backend.Partial,
			Detail: "BH1730FVC ambient light through IIO; the stock brightness curve is unmeasured"},
		{Subsystem: "watchdog", BackendID: "amlogic.syswatchdog", Status: backend.Unverified,
			Detail: "the stock voice-agent holds /dev/watchdog and stopping it can reset the board; disarm semantics are unevidenced"},
		{Subsystem: "wakeword", BackendID: "amlogic.vendor", Status: backend.Unverified,
			Detail: "four-microphone PDM array; no documented engine ABI"},
	}
}

func amlogicAlpineStatuses(r *Report) []backend.SubsystemStatus {
	return []backend.SubsystemStatus{
		{Subsystem: "ring", BackendID: "amlogic.vendor.topleds.ws2812b", Status: backend.Unverified,
			Detail: "Alpine builds drive the WS2812B chain through userspace helpers; /dev/topleds and the LED class devices must be probed on the unit"},
		{Subsystem: "segment", BackendID: "amlogic.sysfs.linedisp", Status: backend.Unverified,
			Detail: "mainline line display (message/map_seg7) or clock:dN:a..g LED class devices; the fitted panel can be an IS31FL3236A or an AW21036"},
		{Subsystem: "audio", BackendID: "amlogic.alsa", Status: backend.Partial,
			Detail: "ALSA through aplay/arecord; BlueALSA carries Bluetooth sources"},
		{Subsystem: "touch", BackendID: "amlogic.evdev", Status: backend.Partial,
			Detail: "evdev keys from the gpio-keys node"},
		{Subsystem: "sensors", BackendID: "amlogic.iio", Status: backend.Partial,
			Detail: "BH1730FVC ambient light through IIO"},
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func unique(matches [][]string) []string {
	set := map[string]bool{}
	var out []string
	for _, m := range matches {
		if len(m) == 2 && !set[m[1]] {
			set[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}
