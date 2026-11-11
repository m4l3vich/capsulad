//go:build linux

package rockchip

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"capsulad/internal/backend"
)

const (
	evSyn = 0
	evKey = 1
	evAbs = 3

	synReport = 0

	absX          = 0x00
	absY          = 0x01
	absPressure   = 0x18
	absMTSlot     = 0x2f
	absMTPosX     = 0x35
	absMTPosY     = 0x36
	absMTTracking = 0x39
	absMTPressure = 0x3a

	btnTouch = 0x14a
)

type evdevTouch struct {
	devices []*evdevDevice
	mu      sync.Mutex
	closed  bool
	fds     []int
}

type evdevDevice struct {
	path        string
	fd          int
	keys        keyMap
	x, y        axis
	hasAbs      bool
	hasPressure bool
	touching    bool
	pressure    float32
}

type axis struct {
	min, max int32
}

func (a axis) norm(v int32) float32 {
	if a.max <= a.min {
		return 0
	}
	f := float32(v-a.min) / float32(a.max-a.min)
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return f
}

func (t *evdevTouch) Caps() backend.TouchCaps {
	caps := backend.TouchCaps{
		PartialGestures:  true,
		SemanticGestures: []string{"tap", "double_tap", "hold", "swipe_left", "swipe_right", "swipe_up", "swipe_down", "primary"},
	}
	for _, d := range t.devices {
		if d.hasAbs {
			caps.Coordinates = true
			caps.FullRaw = true
		}
		if d.hasPressure {
			caps.Pressure = true
		}
	}
	return caps
}

func probeEvdev(prefix string, keys keyMap) backend.TouchDevice {
	dir := filepath.Join(prefix, "/dev/input")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "event") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	t := &evdevTouch{}
	for _, name := range names {
		dev, err := openEvdev(filepath.Join(dir, name), keys)
		if err != nil {
			continue
		}
		t.devices = append(t.devices, dev)
	}
	if len(t.devices) == 0 {
		return nil
	}
	return t
}

func openEvdev(path string, keys keyMap) (*evdevDevice, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	dev := &evdevDevice{path: path, fd: fd, keys: keys}
	if bitSet(fd, evAbs, absX) || bitSet(fd, evAbs, absMTPosX) {
		dev.hasAbs = true
		dev.x = absAxis(fd, absMTPosX)
		dev.y = absAxis(fd, absMTPosY)
		if dev.x.max == 0 {
			dev.x = absAxis(fd, absX)
			dev.y = absAxis(fd, absY)
		}
	}
	dev.hasPressure = bitSet(fd, evAbs, absPressure) || bitSet(fd, evAbs, absMTPressure)
	return dev, nil
}

func bitSet(fd int, evType, code int) bool {
	buf := make([]byte, 8)
	req := eviocgbit(evType, len(buf))
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return false
	}
	idx := code / 8
	if idx >= len(buf) {
		return false
	}
	return buf[idx]&(1<<uint(code%8)) != 0
}

func eviocgbit(ev, size int) uintptr {
	const iocRead = 2
	return uintptr(iocRead<<30 | size<<16 | int('E')<<8 | (0x20 + ev))
}

type absInfoRaw struct {
	Value, Min, Max, Fuzz, Flat, Resolution int32
}

func absAxis(fd int, code int) axis {
	var info absInfoRaw
	req := uintptr(2<<30 | int(unsafe.Sizeof(info))<<16 | int('E')<<8 | (0x40 + code))
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&info)))
	if errno != 0 {
		return axis{}
	}
	return axis{min: info.Min, max: info.Max}
}

func (t *evdevTouch) Start(ctx context.Context, emit func(backend.InputEvent)) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return backend.ErrUnavailable
	}
	for _, d := range t.devices {
		_ = unix.SetNonblock(d.fd, false)
		t.fds = append(t.fds, d.fd)
	}
	t.mu.Unlock()
	var wg sync.WaitGroup
	for _, d := range t.devices {
		wg.Add(1)
		go func(d *evdevDevice) {
			defer wg.Done()
			t.readLoop(ctx, d, emit)
		}(d)
	}
	<-ctx.Done()
	t.Close()
	wg.Wait()
	return nil
}

func (t *evdevTouch) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	fds := t.fds
	t.fds = nil
	t.mu.Unlock()
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
	return nil
}

func (t *evdevTouch) readLoop(ctx context.Context, d *evdevDevice, emit func(backend.InputEvent)) {
	size := 24
	if unsafe.Sizeof(uintptr(0)) == 4 {
		size = 16
	}
	buf := make([]byte, size)
	x, y := float32(0), float32(0)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := unix.Read(d.fd, buf)
		if err != nil {
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}
			return
		}
		if n < size {
			continue
		}
		typ := binary.LittleEndian.Uint16(buf[16:18])
		code := binary.LittleEndian.Uint16(buf[18:20])
		value := int32(binary.LittleEndian.Uint32(buf[20:24]))
		switch typ {
		case evKey:
			if int(code) == btnTouch {
				d.touching = value != 0
				continue
			}
			if name, ok := d.keys[int(code)]; ok {
				emit(backend.InputEvent{Key: &backend.KeyEvent{Key: name, Pressed: value != 0}})
			}
		case evAbs:
			switch int(code) {
			case absX, absMTPosX:
				x = d.x.norm(value)
			case absY, absMTPosY:
				y = d.y.norm(value)
			case absPressure, absMTPressure:
				p := float32(value)
				d.pressure = p
			case absMTTracking:
				d.touching = value >= 0
			}
		case evSyn:
			if code != synReport || !d.hasAbs {
				continue
			}
			var pressure *float32
			if d.hasPressure {
				p := d.pressure
				pressure = &p
			}
			report := &backend.RawReport{
				Contacts: []backend.Contact{{
					ID: 0, X: x, Y: y, Pressure: pressure, Touching: d.touching,
				}},
				CoordinatesValid: true,
				SampleFormat:     "evdev.contact.v1",
			}
			emit(backend.InputEvent{Report: report})
		}
	}
}
