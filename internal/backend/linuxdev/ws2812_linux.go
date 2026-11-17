//go:build linux && (amd64 || arm64)

package linuxdev

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"capsulad/internal/backend"
)

// Vendor ioctls recovered from the stock voice-agent. The encoding is standard
// Linux _IOC:
//
//	LEDS_MULTI_CTRL_IOCTL_GET_COUNT  _IOR('z', 2, int) = 0x80047a02
//	LEDS_WS2812B_IOCTL_MULTI_SET     _IOW('z', 1, 16)  = 0x40107a01
//
// The count ioctl is shared by every vendor LED node. The same ABI is used by
// the RK3308 rev 4 image and the Amlogic A113X Mini image.
const (
	LedIoctlGetCount  = 0x80047a02
	LedIoctlWS2812Set = 0x40107a01
)

type ws2812SetRequest struct {
	Start uint32
	Count uint32
	Data  uintptr
}

// The vendor struct is exactly 16 bytes on 64-bit Linux.
var _ = [1]struct{}{}[unsafe.Sizeof(ws2812SetRequest{})-16]

// WS2812Device is the vendor /dev/topleds char device: a 36-channel WS2812B
// chain presented as one channel buffer.
type WS2812Device struct {
	mu       sync.Mutex
	fd       *os.File
	channels int
}

// OpenWS2812Device opens the vendor ring node and reports its driver count.
func OpenWS2812Device(path string) (*WS2812Device, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	count, err := unix.IoctlGetInt(int(f.Fd()), LedIoctlGetCount)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("read WS2812B channel count: %w", err)
	}
	return &WS2812Device{fd: f, channels: count}, nil
}

// Channels reports the channel count the driver advertises.
func (d *WS2812Device) Channels() int { return d.channels }

// Write submits one already-encoded channel buffer.
func (d *WS2812Device) Write(buffer []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fd == nil {
		return os.ErrClosed
	}
	if len(buffer) == 0 {
		return nil
	}
	request := ws2812SetRequest{Count: uint32(len(buffer)), Data: uintptr(unsafe.Pointer(&buffer[0]))}
	if err := IoctlPointer(d.fd, LedIoctlWS2812Set, unsafe.Pointer(&request)); err != nil {
		return fmt.Errorf("set WS2812B frame: %w", err)
	}
	runtime.KeepAlive(buffer)
	return nil
}

// Close releases the device.
func (d *WS2812Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fd == nil {
		return nil
	}
	err := d.fd.Close()
	d.fd = nil
	return err
}

// WS2812Ring is the vendor ring driven through the recovered ABI.
type WS2812Ring struct {
	mu       sync.Mutex
	dev      *WS2812Device
	channels int
	gamma    [256]uint8
	channel  []int
}

// OpenWS2812Ring opens path and validates that the driver reports exactly
// expectedChannels. channelMap and gamma are the board's frame succession.
func OpenWS2812Ring(path string, expectedChannels int, channelMap []int, gamma float64) (*WS2812Ring, error) {
	dev, err := OpenWS2812Device(path)
	if err != nil {
		return nil, err
	}
	if expectedChannels > 0 && dev.Channels() != expectedChannels {
		dev.Close()
		return nil, fmt.Errorf("driver reports %d WS2812B channels, want the %d-channel ring", dev.Channels(), expectedChannels)
	}
	return &WS2812Ring{dev: dev, channels: dev.Channels(), gamma: BuildGammaTable(gamma), channel: channelMap}, nil
}

func (r *WS2812Ring) Caps() backend.RingCaps {
	return backend.RingCaps{PixelCount: r.channels / 3, SupportsRGB: true}
}

func (r *WS2812Ring) ResourceID() string { return "light.ring.main" }

func (r *WS2812Ring) WriteRing(frame backend.RingFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	buffer := EncodeWS2812Frame(frame.Pixels, frame.Brightness, r.channels, r.channel, r.gamma)
	return r.dev.Write(buffer)
}

func (r *WS2812Ring) Close() error { return r.dev.Close() }

// WriteWS2812Frame opens path, reads the driver's channel count and submits one
// encoded frame. It is the bring-up path: the caller supplies the map and gamma
// explicitly instead of going through a backend.
func WriteWS2812Frame(path string, pixels []backend.RGB, brightness float64, channelMap []int, gamma float64) error {
	dev, err := OpenWS2812Device(path)
	if err != nil {
		return err
	}
	defer dev.Close()
	buffer := EncodeWS2812Frame(pixels, brightness, dev.Channels(), channelMap, BuildGammaTable(gamma))
	return dev.Write(buffer)
}

// IoctlPointer issues one ioctl with a pointer argument, retrying EINTR.
func IoctlPointer(f *os.File, command uintptr, arg unsafe.Pointer) error {
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

var _ backend.RingDevice = (*WS2812Ring)(nil)
