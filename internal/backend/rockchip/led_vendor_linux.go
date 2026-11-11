//go:build linux && (amd64 || arm64)

package rockchip

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"capsulad/internal/backend"
)

// Vendor ioctls recovered from the stock voice-agent (firmware backup
// 03104B1D40E15C55). The encoding is standard Linux _IOC:
//
//	LEDS_MULTI_CTRL_IOCTL_GET_COUNT  _IOR('z', 2, int) = 0x80047a02
//	LEDS_MULTI_CTRL_IOCTL_MULTI_SET  _IOW('z', 1,  8)  = 0x40087a01
//	LEDS_WS2812B_IOCTL_MULTI_SET     _IOW('z', 1, 16)  = 0x40107a01
//
// The count ioctl is shared by /dev/topleds and /dev/led_multi_ctrl.
const (
	ledsIoctlGetCount = 0x80047a02
	ledsMultiCtrlSet  = 0x40087a01
	ledsWS2812BSet    = 0x40107a01
)

type ws2812SetRequest struct {
	Start uint32
	Count uint32
	Data  uintptr
}

// The vendor struct is exactly 16 bytes on 64-bit Linux.
var _ = [1]struct{}{}[unsafe.Sizeof(ws2812SetRequest{})-16]

type ws2812bRing struct {
	mu       sync.Mutex
	fd       *os.File
	channels int
}

// openWS2812BRing opens /dev/topleds and validates the channel count reported
// by the driver. cfg.RootPrefix disables real devices (offline inspection).
func openWS2812BRing(cfg Config, node *ws2812RingNode) (backend.RingDevice, error) {
	if cfg.RootPrefix != "" {
		return nil, errLEDOffline
	}
	f, err := os.OpenFile("/dev/topleds", os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	count, err := unix.IoctlGetInt(int(f.Fd()), ledsIoctlGetCount)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read WS2812B channel count: %w", err)
	}
	if count != ws2812ChannelCount {
		f.Close()
		return nil, fmt.Errorf("driver reports %d WS2812B channels, want the %d-channel Capsula Mini ring", count, ws2812ChannelCount)
	}
	if node != nil && node.DeviceName != "" && node.DeviceName != "topleds" {
		f.Close()
		return nil, fmt.Errorf("unsupported WS2812B device-name %q", node.DeviceName)
	}
	if node != nil && node.Channels > 0 && node.Channels != count {
		f.Close()
		return nil, fmt.Errorf("device tree declares %d WS2812B channels, driver reports %d", node.Channels, count)
	}
	return &ws2812bRing{fd: f, channels: count}, nil
}

func (r *ws2812bRing) Caps() backend.RingCaps {
	return backend.RingCaps{PixelCount: r.channels / 3, SupportsRGB: true}
}
func (r *ws2812bRing) ResourceID() string { return "light.ring.main" }

func (r *ws2812bRing) WriteRing(frame backend.RingFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fd == nil {
		return os.ErrClosed
	}
	buffer := encodeWS2812Frame(frame.Pixels, frame.Brightness, r.channels)
	request := ws2812SetRequest{Count: uint32(len(buffer)), Data: uintptr(unsafe.Pointer(&buffer[0]))}
	if err := ioctlPointer(r.fd, ledsWS2812BSet, unsafe.Pointer(&request)); err != nil {
		return fmt.Errorf("set WS2812B frame: %w", err)
	}
	runtime.KeepAlive(buffer)
	return nil
}

func (r *ws2812bRing) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fd == nil {
		return nil
	}
	err := r.fd.Close()
	r.fd = nil
	return err
}

type multiCtrlSegment struct {
	mu    sync.Mutex
	fd    *os.File
	cells int
}

// openMultiCtrlSegment opens /dev/led_multi_ctrl (the IS31FL3236 driver) and
// validates the cell count reported by the driver.
func openMultiCtrlSegment(cfg Config, node *multiCtrlNode) (backend.SegmentDevice, error) {
	if cfg.RootPrefix != "" {
		return nil, errLEDOffline
	}
	f, err := os.OpenFile("/dev/led_multi_ctrl", os.O_RDONLY, 0)
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
	return &multiCtrlSegment{fd: f, cells: count}, nil
}

func (s *multiCtrlSegment) Caps() backend.SegmentCaps {
	digits := s.cells / segmentPerDigit
	if digits > segmentDigitCount {
		digits = segmentDigitCount
	}
	if digits < 1 {
		digits = 1
	}
	return backend.SegmentCaps{
		DigitCount:       digits,
		SupportsColon:    s.cells >= multiCtrlCellCount,
		SupportsDecimal:  false,
		SegmentsPerDigit: segmentPerDigit,
	}
}
func (s *multiCtrlSegment) ResourceID() string { return "display.segment.front" }

func (s *multiCtrlSegment) WriteSegment(frame backend.SegmentFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd == nil {
		return os.ErrClosed
	}
	records := encodeMultiCtrlFrame(frame.Cells, frame.Colon, frame.Brightness, s.cells)
	if err := ioctlPointer(s.fd, ledsMultiCtrlSet, unsafe.Pointer(&records[0])); err != nil {
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

func ioctlPointer(f *os.File, command uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), command, uintptr(arg))
		switch errno {
		case 0:
			return nil
		case unix.EINTR:
			continue
		default:
			return errno
		}
	}
}
