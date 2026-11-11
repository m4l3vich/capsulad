package rockchip

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"capsulad/internal/backend"
)

// The stock firmware drives two independent LED controllers. Both were
// recovered from the device tree embedded in the boot partition and from the
// stock voice-agent binary:
//
//   - /spi@ff120000/ws2812b@00 (compatible "ws2812b", device-name "topleds")
//     is a 36-channel WS2812B chain used as the 12-pixel RGB ring. The vendor
//     app animates it with 12-color "*_diodes.json" frames and writes all
//     channels through /dev/topleds.
//   - /i2c@ff060000/led-controller@3c (compatible "issi,is31fl3236") exposes
//     30 monochrome channels labelled clock-led1..30, used as the 4x7-segment
//     plus two-colon display. The vendor app animates it with 30-value
//     "*_digits.json" frames and writes through /dev/led_multi_ctrl.
//
// Both char devices take vendor ioctls (see led_vendor_linux.go). The ring
// channel permutation below is copied verbatim from the stock voice-agent and
// makes logical pixel order line up with the physical WS2812B chain.
const (
	ws2812ChannelCount = 36
	ws2812PixelCount   = 12
	multiCtrlCellCount = 30
	segmentPerDigit    = 7
	segmentDigitCount  = 4
)

var (
	// ws2812ChannelMap maps logical channel index (R,G,B per pixel) to the
	// physical WS2812B channel. Stock voice-agent: buf[map[i]] = in[i].
	ws2812ChannelMap = [ws2812ChannelCount]int{
		1, 0, 2, 34, 33, 35, 31, 30, 32, 28, 27, 29,
		25, 24, 26, 22, 21, 23, 19, 18, 20, 16, 15, 17,
		13, 12, 14, 10, 9, 11, 7, 6, 8, 4, 3, 5,
	}
)

// errLEDOffline is returned when a vendor LED device cannot be driven in the
// current process (non-Linux host or an offline firmware tree inspection).
var errLEDOffline = errors.New("vendor LED device is not accessible in this context")

type ws2812RingNode struct {
	NodePath   string
	DeviceName string
	Channels   int
}

type multiCtrlNode struct {
	NodePath string
	Cells    int
}

type ledDeviceTree struct {
	Ring    *ws2812RingNode
	Segment *multiCtrlNode
}

// probeLEDDeviceTree finds the two LED controllers in the live device tree.
// An offline root prefix normally has no /proc/device-tree, so the result is
// left empty; callers then fall back to the observed vendor defaults.
func probeLEDDeviceTree(prefix string) ledDeviceTree {
	base := deviceTreeBase(prefix)
	if base == "" {
		return ledDeviceTree{}
	}
	var out ledDeviceTree
	if path := findCompatible(base, "ws2812b"); path != "" {
		out.Ring = &ws2812RingNode{
			NodePath:   path,
			DeviceName: readTrim(filepath.Join(path, "device-name")),
			Channels:   countLEDChildren(path),
		}
	}
	if path := findCompatible(base, "issi,is31fl3236"); path != "" {
		out.Segment = &multiCtrlNode{NodePath: path, Cells: countLEDChildren(path)}
	}
	return out
}

func deviceTreeBase(prefix string) string {
	for _, candidate := range []string{"/proc/device-tree", "/sys/firmware/devicetree/base"} {
		if st, err := os.Stat(prefix + candidate); err == nil && st.IsDir() {
			return prefix + candidate
		}
	}
	return ""
}

// findCompatible walks the flattened device tree (exposed as directories and
// property files) and returns the first node whose compatible property
// contains want.
func findCompatible(base, want string) string {
	var found string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if found != "" || depth > 6 {
			return
		}
		if hasCompatible(dir, want) {
			found = dir
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			walk(filepath.Join(dir, name), depth+1)
		}
	}
	walk(base, 0)
	return found
}

func hasCompatible(dir, want string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "compatible"))
	if err != nil {
		return false
	}
	for _, value := range strings.Split(string(data), "\x00") {
		if value == want {
			return true
		}
	}
	return false
}

// countLEDChildren counts the ledN subnodes declared under a controller. The
// label property is present on every stock child; a name fallback keeps the
// probe useful for trees that omit labels.
func countLEDChildren(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(child, "label")); err == nil {
			count++
			continue
		}
		if strings.HasPrefix(e.Name(), "led") {
			count++
		}
	}
	return count
}

func clampBrightness(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func scaleChannel(v uint8, brightness float64) uint8 {
	return uint8(math.Round(float64(v) * brightness))
}

// ws2812Gamma is the perceptual curve applied to ring output. The stock agent
// only ever wrote 0 or 255 (integer JSON frames), so there is no vendor
// evidence for a curve; 2.2 is the sRGB default and the common choice for
// WS2812B indicators. The segment display is left linear because its calibration
// and brightness behaviour were accepted on hardware as they are.
const ws2812Gamma = 2.2

var ws2812GammaTable = buildGammaTable(ws2812Gamma)

func buildGammaTable(gamma float64) (table [256]uint8) {
	for i := range table {
		table[i] = uint8(math.Round(255 * math.Pow(float64(i)/255, gamma)))
	}
	return table
}

// encodeWS2812Frame expands RGB pixels into the controller's channel buffer
// and applies the physical channel permutation. Missing pixels are black.
func encodeWS2812Frame(pixels []backend.RGB, brightness float64, channels int) []byte {
	if channels <= 0 {
		channels = ws2812ChannelCount
	}
	brightness = clampBrightness(brightness)
	logical := make([]byte, channels)
	pixels = pixels[:min(len(pixels), channels/3)]
	for pixel := range pixels {
		px := pixels[pixel]
		// Brightness acts in linear light, then the perceptual curve is
		// applied once per channel so dim and bright levels look even.
		logical[pixel*3] = ws2812GammaTable[scaleChannel(px.R, brightness)]
		logical[pixel*3+1] = ws2812GammaTable[scaleChannel(px.G, brightness)]
		logical[pixel*3+2] = ws2812GammaTable[scaleChannel(px.B, brightness)]
	}
	out := make([]byte, channels)
	if channels == len(ws2812ChannelMap) {
		for i, target := range ws2812ChannelMap {
			out[target] = logical[i]
		}
		return out
	}
	copy(out, logical)
	return out
}

// multiCtrlRecord is the 20-byte record consumed by
// LEDS_MULTI_CTRL_IOCTL_MULTI_SET. The stock app initialises every record as
// {enable: 1, 0, brightness: 0, 0, 0} and then updates brightness in place.
type multiCtrlRecord struct {
	Enable     uint32
	_          uint32
	Brightness uint32
	_          [2]uint32
}

// multiCtrlChannelMap is the measured record order of the IS31FL3236 display on
// the Capsula Mini. Index [cell][segment] with segment bits a..g = 0..6 and
// cell 0 leftmost; the driver's record order is neither digit-major nor
// segment-major, so it cannot be derived and must stay measured. Calibrated
// channel by channel with cmd/ledmap on firmware 03104B1D40E15C55.
var multiCtrlChannelMap = [4][7]int{
	{1, 28, 21, 17, 22, 29, 24},  // digit 1: a b c d e f g
	{26, 25, 19, 18, 23, 27, 20}, // digit 2
	{0, 2, 7, 5, 6, 4, 3},        // digit 3
	{10, 12, 11, 14, 13, 8, 9},   // digit 4
}

const (
	multiCtrlColonTop    = 16
	multiCtrlColonBottom = 15
)

// encodeMultiCtrlFrame maps a cell/segment frame onto the 30 monochrome
// control records using the calibrated channel table. Missing cells are dark.
// A frame on a display with an unexpected record count falls back to the linear
// order so an unverified variant is not driven with the wrong map.
func encodeMultiCtrlFrame(cells []uint16, colon bool, brightness float64, count int) []multiCtrlRecord {
	if count <= 0 {
		count = multiCtrlCellCount
	}
	brightness = clampBrightness(brightness)
	logical := make([]byte, count)
	level := uint8(math.Round(brightness * 255))
	calibrated := count == multiCtrlCellCount
	for cell := 0; cell < segmentDigitCount && cell < len(cells); cell++ {
		mask := cells[cell]
		for seg := 0; seg < segmentPerDigit; seg++ {
			if mask&(1<<uint(seg)) == 0 {
				continue
			}
			idx := cell*segmentPerDigit + seg
			if calibrated {
				idx = multiCtrlChannelMap[cell][seg]
			}
			if idx < count {
				logical[idx] = level
			}
		}
	}
	if colon {
		top, bottom := multiCtrlColonTop, multiCtrlColonBottom
		if !calibrated {
			top, bottom = 28, 29
		}
		if top < count {
			logical[top] = level
		}
		if bottom < count {
			logical[bottom] = level
		}
	}
	records := make([]multiCtrlRecord, count)
	for i := range records {
		records[i].Enable = 1
		records[i].Brightness = uint32(logical[i])
	}
	return records
}
