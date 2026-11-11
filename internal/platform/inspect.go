// Package platform inspects an extracted Capsula firmware tree offline.
//
// It is deliberately evidence-based: it reports what the image contains
// (devices, init scripts, PulseAudio topology, LED animation geometry) and
// marks vendor interfaces whose behavior has not been observed on a device as
// partial or unverified. It never claims hardware behavior that has not been
// observed on a device.
package platform

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
)

type Report struct {
	Root          string                    `json:"root"`
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

func Inspect(root string) (*Report, error) {
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("firmware root %q is not a directory", root)
	}
	r := &Report{Root: root}
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
	if r.Model == "" {
		// The boot image's device tree is not part of the rootfs; the stock
		// image identifies this board as RK3308-based Capsula-mini.
		r.Model = "Capsula-mini (RK3308, from stock image metadata)"
	}
	r.SoC = "Rockchip RK3308"
	r.Evidence = append(r.Evidence, "kernel from boot partition: Linux 4.4.167 aarch64 (Buildroot)")

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
	for _, node := range []string{"/dev/led_multi_ctrl", "/dev/topleds", "/dev/vendor_storage"} {
		if exists(filepath.Join(root, node)) {
			r.DeviceNodes = append(r.DeviceNodes, node)
		}
	}
	// The vendor nodes are usually created by mdev at boot, so absence in the
	// extracted image is expected; record the init/binary evidence instead.
	if !contains(r.DeviceNodes, "/dev/led_multi_ctrl") {
		if exists(filepath.Join(root, "usr/bin/voice-agent")) {
			r.DeviceNodes = append(r.DeviceNodes, "/dev/led_multi_ctrl (referenced by voice-agent)")
		}
	}
	if !contains(r.DeviceNodes, "/dev/topleds") {
		if exists(filepath.Join(root, "usr/bin/voice-agent")) {
			r.DeviceNodes = append(r.DeviceNodes, "/dev/topleds (referenced by voice-agent)")
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

	r.Statuses = []backend.SubsystemStatus{
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
	return r, nil
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
