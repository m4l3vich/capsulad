// Package backend defines the hardware abstraction implemented per target
// platform (simulator, Rockchip stock, Amlogic stock/Alpine).
//
// Backends own physical devices. They never implement routing or presentation
// semantics: the framework renders built-in presets into frames and writes
// those frames through this interface. Capabilities are reported, not
// assumed; a backend that cannot verify a vendor ABI reports
// BACKEND_UNVERIFIED rather than guessing.
package backend

import (
	"context"
	"errors"
	"os"
	"time"
)

type Platform string

const (
	PlatformSimulator     Platform = "simulator"
	PlatformRockchipStock Platform = "CAPSULA_MINI_ROCKCHIP_STOCK"
	// The Amlogic A113X board family shares one wire platform per userland:
	// the jailbroken stock Buildroot image and Alpine Linux.
	PlatformAmlogicStock  Platform = "CAPSULA_MINI_AMLOGIC_STOCK"
	PlatformAmlogicAlpine Platform = "CAPSULA_MINI_AMLOGIC_ALPINE"
)

type Status string

const (
	Available   Status = "AVAILABLE"
	Partial     Status = "PARTIAL"
	Unavailable Status = "UNAVAILABLE"
	Unverified  Status = "UNVERIFIED"
)

type SubsystemStatus struct {
	Subsystem string
	BackendID string
	Status    Status
	Detail    string
}

type DeviceInfo struct {
	DeviceID      string
	Model         string
	BoardRevision string
	SoC           string
	Kernel        string
	Firmware      string
	Platform      Platform
}

// RGB is one ring pixel.
type RGB struct{ R, G, B uint8 }

// SegmentCaps describes the physical glyph geometry. Support for individual
// glyphs is a preset-catalog concern, not this capability record.
type SegmentCaps struct {
	DigitCount       int
	SupportsColon    bool
	SupportsDecimal  bool
	SegmentsPerDigit int
}

// RingCaps describes the LED ring geometry.
type RingCaps struct {
	PixelCount  int
	SupportsRGB bool
}

// SegmentFrame is a fully mapped hardware frame: one mask per digit cell,
// a colon flag and the global brightness the framework has already resolved.
type SegmentFrame struct {
	Cells      []uint16 // bit i = segment i within that digit
	Colon      bool
	Brightness float64 // [0,1], applied after content selection
}

type RingFrame struct {
	Pixels     []RGB
	Brightness float64
}

type SegmentDevice interface {
	WriteSegment(frame SegmentFrame) error
	Caps() SegmentCaps
	ResourceID() string
}

type RingDevice interface {
	WriteRing(frame RingFrame) error
	Caps() RingCaps
	ResourceID() string
}

type SampleFormat string

const (
	S16LE SampleFormat = "PCM_S16LE"
	F32LE SampleFormat = "PCM_F32LE"
)

type AudioFormat struct {
	Format   SampleFormat
	RateHz   int
	Channels int
}

func (f AudioFormat) BytesPerFrame() int {
	bps := 2
	if f.Format == F32LE {
		bps = 4
	}
	return bps * f.Channels
}

// Sink receives already-mixed PCM. Implementations must not block forever;
// the framework treats a slow hardware sink as a discontinuity, not a reason
// to block capture.
type Sink interface {
	WritePCM(p []byte) error
	Close() error
}

// Source produces physical capture PCM in the backend's native format.
type Source interface {
	ReadPCM(p []byte) (int, error)
	Close() error
}

type AudioDevice interface {
	PlaybackFormat() AudioFormat
	CaptureFormat() AudioFormat
	OpenOutput() (Sink, error)
	OpenInput() (Source, error)
	// SetMasterVolume pins the hardware/sink gain; the framework mixer owns the
	// only software master gain. It must be bounded or non-blocking and is
	// called at startup and on every volume change.
	SetMasterVolume(v float64) error
	// SetHardwareMute is best-effort. It returns false when the backend cannot
	// mute in hardware and the framework must only suppress streams.
	SetHardwareMute(muted bool) (bool, error)
	Close() error
}

// TouchCaps is the discovered gesture/raw surface for one panel.
type TouchCaps struct {
	PartialGestures bool
	// FullPanel means full capture can receive every panel key and inferred
	// gesture even if raw coordinates or contact samples are unavailable.
	FullPanel        bool
	FullRaw          bool
	Coordinates      bool
	Pressure         bool
	SemanticGestures []string
}

type Contact struct {
	ID       uint32
	X, Y     float32 // [0,1] when Coordinates is true
	Pressure *float32
	Touching bool
}

// RawReport is one backend sample. Backends without coordinates may only emit
// opaque BackendSample plus a format tag; they must leave CoordinatesValid
// false so a full capture can report the limitation instead of inventing
// positions.
type RawReport struct {
	Contacts         []Contact
	BackendSample    []byte
	SampleFormat     string
	CoordinatesValid bool
}

type KeyEvent struct {
	Key     string
	Pressed bool
}

// GestureEvent is a gesture a backend recognizes itself (stock bridge or
// driver). Framework-recognized gestures arrive as contacts.
type GestureEvent struct {
	ID    string
	Phase string // start, update, end, cancelled
}

type InputEvent struct {
	MonotonicNS uint64
	// EventAgeNS is how long before the backend emitted this event the kernel
	// recorded it, measured on a clock the backend actually supports. The
	// framework anchors it to its own monotonic clock so a late read does not
	// collapse real inter-contact gaps or restart a hold deadline. Zero means
	// the backend has no usable kernel timestamp and read time is authoritative.
	EventAgeNS uint64
	Key        *KeyEvent
	Report     *RawReport
	Gesture    *GestureEvent
}

type TouchDevice interface {
	Caps() TouchCaps
	Start(ctx context.Context, emit func(InputEvent)) error
	Close() error
}

type SensorInfo struct {
	ID   string
	Unit string
}

type Sensors interface {
	List() []SensorInfo
	Read(id string) (float64, bool)
	Close() error
}

// Backend is one probed platform. Any device may be nil when unavailable; the
// capability list is the authoritative availability report.
type Backend interface {
	Platform() Platform
	Info() DeviceInfo
	Capabilities() []SubsystemStatus
	Segment() SegmentDevice
	Ring() RingDevice
	Audio() AudioDevice
	Touch() TouchDevice
	Sensors() Sensors
	Close() error
}

var ErrUnavailable = errors.New("hardware backend unavailable")

// ErrWatchdogBusy reports that another process still holds the watchdog device.
// Only one process may open /dev/watchdog at a time, so a supervisor that
// starts while the vendor agent (or a feeder left by a previous daemon) still
// owns it must treat this as "retry", not as a dead device.
var ErrWatchdogBusy = errors.New("hardware watchdog held by another process")

// Watchdog is a hardware liveness timer (e.g. the DesignWare /dev/watchdog on
// the RK3308). The stock firmware's voice-agent feeds it, so a replacement
// supervisor must take the feed over or the SoC resets when the vendor process
// stops. Timeout reports the armed interval; Feed resets it; Detach transfers
// the descriptor so another process can keep feeding it.
//
// Close is the graceful release: it disarms the timer where the driver allows
// it. On a nowayout driver (profile.Watchdog.Nowayout: the RK3308 stock kernel
// ignores the magic close and refuses the disable ioctl) an armed timer cannot
// be stopped at all. The supervisor is conservative about this: it hands the
// descriptor to a feeder whenever Detach returns one and only closes when the
// platform exposes none, so no planned stop has to rely on Close actually
// disarming an unmeasured driver.
type Watchdog interface {
	Timeout() time.Duration
	Feed() error
	// Detach transfers the armed descriptor to the caller, which may hand it
	// to another process. The watchdog must not be fed or closed afterwards;
	// it returns nil when the platform cannot expose the descriptor.
	Detach() *os.File
	Close() error
}
