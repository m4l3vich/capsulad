package rockchip

import (
	"errors"
	"math"
	"path/filepath"

	"capsulad/internal/backend/linuxdev"
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
// channel permutation and the frame encoder live in internal/backend/linuxdev
// because the Amlogic A113X Mini uses the same recovered 36-channel wiring.
// Channel counts, pixel geometry and gamma come from the device profile.

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
	base := linuxdev.DeviceTreeBase(prefix)
	if base == "" {
		return ledDeviceTree{}
	}
	var out ledDeviceTree
	if path := linuxdev.FindCompatible(base, "ws2812b"); path != "" {
		out.Ring = &ws2812RingNode{
			NodePath:   path,
			DeviceName: linuxdev.ReadTrim(filepath.Join(path, "device-name")),
			Channels:   linuxdev.CountLEDChildren(path),
		}
	}
	if path := linuxdev.FindCompatible(base, "issi,is31fl3236"); path != "" {
		out.Segment = &multiCtrlNode{NodePath: path, Cells: linuxdev.CountLEDChildren(path)}
	}
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
func encodeMultiCtrlFrame(cells []uint16, colonTop, colonBottom bool, brightness float64, count, digits, segmentsPerDigit int) []multiCtrlRecord {
	brightness = linuxdev.ClampBrightness(brightness)
	logical := make([]byte, count)
	level := uint8(math.Round(brightness * 255))
	calibrated := count == digits*segmentsPerDigit+2
	for cell := range min(digits, len(cells)) {
		mask := cells[cell]
		for seg := range segmentsPerDigit {
			if mask&(1<<uint(seg)) == 0 {
				continue
			}
			idx := cell*segmentsPerDigit + seg
			if calibrated && cell < len(multiCtrlChannelMap) && seg < len(multiCtrlChannelMap[cell]) {
				idx = multiCtrlChannelMap[cell][seg]
			}
			if idx < count {
				logical[idx] = level
			}
		}
	}
	if colonTop || colonBottom {
		top, bottom := multiCtrlColonTop, multiCtrlColonBottom
		if !calibrated {
			top, bottom = 28, 29
		}
		if colonTop && top < count {
			logical[top] = level
		}
		if colonBottom && bottom < count {
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
