//go:build linux && (amd64 || arm64)

package rockchip

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"capsulad/internal/backend"
	"capsulad/internal/backend/linuxdev"
)

// Vendor ioctls recovered from the stock voice-agent (firmware backup
// 03104B1D40E15C55). The encoding is standard Linux _IOC:
//
//	LEDS_MULTI_CTRL_IOCTL_GET_COUNT  _IOR('z', 2, int) = 0x80047a02
//	LEDS_MULTI_CTRL_IOCTL_MULTI_SET  _IOW('z', 1,  8)  = 0x40087a01
//	LEDS_WS2812B_IOCTL_MULTI_SET     _IOW('z', 1, 16)  = 0x40107a01
//
// The count ioctl is shared by /dev/topleds and /dev/led_multi_ctrl. The ring
// node is driven through internal/backend/linuxdev, which owns the shared
// 36-channel ABI and the WS2812B frame encoder.
const (
	ledsIoctlGetCount = linuxdev.LedIoctlGetCount
	ledsMultiCtrlSet  = 0x40087a01
)

// openWS2812BRing opens the vendor ring device and validates the channel count
// reported by the driver. cfg.RootPrefix disables real devices (offline
// inspection).
func openWS2812BRing(cfg Config, node *ws2812RingNode) (backend.RingDevice, error) {
	if cfg.RootPrefix != "" {
		return nil, errLEDOffline
	}
	if node != nil && node.DeviceName != "" && node.DeviceName != filepath.Base(cfg.LEDTopPath) {
		return nil, fmt.Errorf("unsupported WS2812B device-name %q", node.DeviceName)
	}
	ring, err := linuxdev.OpenWS2812Ring(cfg.LEDTopPath, cfg.LEDWS2812Channels, linuxdev.WS2812ChannelMap[:], cfg.LEDGamma)
	if err != nil {
		return nil, err
	}
	// The device tree and the driver must agree on the chain length; a
	// disagreement is a different board, not a frame order to guess at.
	if node != nil && node.Channels > 0 && node.Channels != ring.Caps().PixelCount*3 {
		ring.Close()
		return nil, fmt.Errorf("device tree declares %d WS2812B channels, driver reports %d", node.Channels, ring.Caps().PixelCount*3)
	}
	return ring, nil
}

type multiCtrlSegment struct {
	mu               sync.Mutex
	fd               *os.File
	cells            int
	digits           int
	segmentsPerDigit int
	colonChannels    int
}

// openMultiCtrlSegment opens the vendor IS31FL3236 node and validates the cell
// count reported by the driver.
func openMultiCtrlSegment(cfg Config, node *multiCtrlNode) (backend.SegmentDevice, error) {
	if cfg.RootPrefix != "" {
		return nil, errLEDOffline
	}
	f, err := os.OpenFile(cfg.LEDMultiCtrlPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	count, err := unix.IoctlGetInt(int(f.Fd()), ledsIoctlGetCount)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read multi-control LED count: %w", err)
	}
	if count <= 0 {
		f.Close()
		return nil, fmt.Errorf("driver reports %d multi-control LEDs", count)
	}
	if node != nil && node.Cells > 0 && node.Cells != count {
		f.Close()
		return nil, fmt.Errorf("device tree declares %d display cells, driver reports %d", node.Cells, count)
	}
	return &multiCtrlSegment{fd: f, cells: count, digits: cfg.LEDSegmentDigits, segmentsPerDigit: cfg.LEDSegmentsPerDigit, colonChannels: cfg.LEDMultiCtrlChannels}, nil
}

func (s *multiCtrlSegment) Caps() backend.SegmentCaps {
	digits := s.cells / s.segmentsPerDigit
	if digits > s.digits {
		digits = s.digits
	}
	if digits < 1 {
		digits = 1
	}
	return backend.SegmentCaps{
		DigitCount:       digits,
		SupportsColon:    s.cells >= s.colonChannels,
		SupportsDecimal:  false,
		SegmentsPerDigit: s.segmentsPerDigit,
	}
}
func (s *multiCtrlSegment) ResourceID() string { return "display.segment.front" }

func (s *multiCtrlSegment) WriteSegment(frame backend.SegmentFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd == nil {
		return os.ErrClosed
	}
	records := encodeMultiCtrlFrame(frame.Cells, frame.TopColonOn(), frame.BottomColonOn(), frame.Brightness, s.cells, s.digits, s.segmentsPerDigit)
	if err := linuxdev.IoctlPointer(s.fd, ledsMultiCtrlSet, unsafe.Pointer(&records[0])); err != nil {
		return fmt.Errorf("set segment frame: %w", err)
	}
	runtime.KeepAlive(records)
	return nil
}

func (s *multiCtrlSegment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd == nil {
		return nil
	}
	err := s.fd.Close()
	s.fd = nil
	return err
}
