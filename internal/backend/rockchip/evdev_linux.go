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
	"time"
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

	// eviocsetClockID is EVIOCSCLOCKID: _IOW('E', 0xa0, int). It selects the
	// clock an evdev device stamps its events with.
	eviocsetClockID = 0x400445a0

	// maxEventAge bounds how old a kernel timestamp may be before it is treated
	// as unusable. Beyond it the event cannot contribute to a gesture or a hold
	// anyway, and read time is the safer fallback.
	maxEventAge = 5 * time.Second
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
	// monotonic is true when the device accepted EVIOCSCLOCKID(CLOCK_MONOTONIC)
	// and therefore stamps kernel events on the monotonic clock. Otherwise the
	// default CLOCK_REALTIME timestamps are used.
	monotonic bool
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
		SemanticGestures: []string{"tap", "double_tap", "hold", "swipe_left", "swipe_right", "swipe_up", "swipe_down", "circle_cw", "circle_ccw", "primary"},
	}
	for _, d := range t.devices {
		// The Mini has no touch panel, only gpio-keys, so full capture can
		// still receive every key and inferred gesture while raw coordinates
		// remain unavailable.
		if len(d.keys) > 0 {
			caps.FullPanel = true
		}
		if d.hasAbs {
			caps.Coordinates = true
			caps.FullRaw = true
		}
		if d.hasPressure {
			caps.Pressure = true
		}
	}
	if len(t.devices) > 0 {
		caps.FullPanel = true
	}
	return caps
}

func probeEvdev(cfg Config, keys keyMap) backend.TouchDevice {
	dir := filepath.Join(cfg.RootPrefix, cfg.InputEventDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), cfg.InputEventPrefix) {
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
	dev := &evdevDevice{path: path, fd: fd, keys: keys, monotonic: setEventClock(fd)}
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

// setEventClock asks the device for CLOCK_MONOTONIC event timestamps. The
// default input_event timeval is CLOCK_REALTIME, which can jump and does not
// share an origin with the framework clock; monotonic timestamps let the
// framework recover the real age of an event it read late. The ioctl is
// per-file-descriptor and sets only the timestamp source.
func setEventClock(fd int) bool {
	clockID := int32(unix.CLOCK_MONOTONIC)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), eviocsetClockID, uintptr(unsafe.Pointer(&clockID)))
	return errno == 0
}

// eventAgeNS converts one kernel timestamp into an age on the same clock. Zero
// means the timestamp cannot be trusted (driver zeroed it, clock mismatch, or
// implausible skew) and the reader must keep its own read time.
func (d *evdevDevice) eventAgeNS(sec, usec int64) uint64 {
	clockID := int32(unix.CLOCK_REALTIME)
	if d.monotonic {
		clockID = int32(unix.CLOCK_MONOTONIC)
	}
	var now unix.Timespec
	if err := unix.ClockGettime(clockID, &now); err != nil {
		return 0
	}
	age := now.Nano() - (sec*1e9 + usec*1e3)
	if age <= 0 || age > int64(maxEventAge) {
		return 0
	}
	return uint64(age)
}

// evdevFrame is one decoded input_event, including the kernel timestamp's age.
type evdevFrame struct {
	typ   uint16
	code  uint16
	value int32
	ageNS uint64
}

func (d *evdevDevice) decode(buf []byte, size int) evdevFrame {
	var sec, usec int64
	if size >= 24 {
		sec = int64(binary.LittleEndian.Uint64(buf[0:8]))
		usec = int64(binary.LittleEndian.Uint64(buf[8:16]))
	} else {
		sec = int64(int32(binary.LittleEndian.Uint32(buf[0:4])))
		usec = int64(int32(binary.LittleEndian.Uint32(buf[4:8])))
	}
	// input_event is timeval + type(u16) + code(u16) + value(s32); the tail is
	// identical for 32- and 64-bit time_t, only its offset differs.
	off := size - 8
	return evdevFrame{
		typ:   binary.LittleEndian.Uint16(buf[off : off+2]),
		code:  binary.LittleEndian.Uint16(buf[off+2 : off+4]),
		value: int32(binary.LittleEndian.Uint32(buf[off+4 : off+8])),
		ageNS: d.eventAgeNS(sec, usec),
	}
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
		frame := d.decode(buf, size)
		switch frame.typ {
		case evKey:
			if int(frame.code) == btnTouch {
				d.touching = frame.value != 0
				continue
			}
			if name, ok := d.keys[int(frame.code)]; ok {
				emit(backend.InputEvent{
					EventAgeNS: frame.ageNS,
					Key:        &backend.KeyEvent{Key: name, Pressed: frame.value != 0},
				})
			}
		case evAbs:
			switch int(frame.code) {
			case absX, absMTPosX:
				x = d.x.norm(frame.value)
			case absY, absMTPosY:
				y = d.y.norm(frame.value)
			case absPressure, absMTPressure:
				p := float32(frame.value)
				d.pressure = p
			case absMTTracking:
				d.touching = frame.value >= 0
			}
		case evSyn:
			if frame.code != synReport || !d.hasAbs {
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
			emit(backend.InputEvent{EventAgeNS: frame.ageNS, Report: report})
		}
	}
}
