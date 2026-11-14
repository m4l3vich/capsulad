package backend

import (
	"context"
	"sync"
	"time"
)

// Simulator is a complete in-memory backend used by capsulad's development
// and test mode. It has no physical side effects and exposes injection hooks
// for tests and the local development admin socket.
type Simulator struct {
	mu      sync.Mutex
	info    DeviceInfo
	segment *simSegment
	ring    *simRing
	audio   *simAudio
	touch   *simTouch
	sensors *simSensors
	caps    []SubsystemStatus
	closed  bool
}

func NewSimulator(opts SimulatorOptions) *Simulator {
	model := opts.Model
	if model == "" {
		model = "Capsula Mini (simulator)"
	}
	s := &Simulator{
		info: DeviceInfo{
			DeviceID: opts.DeviceID,
			Model:    model,
			SoC:      "virtual",
			Kernel:   "simulator",
			Firmware: "capsula-simulator",
			Platform: PlatformSimulator,
		},
		segment: newSimSegment(),
		ring:    newSimRing(),
		audio:   newSimAudio(),
		touch:   newSimTouch(),
		sensors: newSimSensors(),
	}
	if opts.DeviceID == "" {
		s.info.DeviceID = "simulator-0001"
	}
	s.caps = []SubsystemStatus{
		{Subsystem: "segment", BackendID: "sim.segment", Status: Available, Detail: "4-digit 7-segment virtual display"},
		{Subsystem: "ring", BackendID: "sim.ring", Status: Available, Detail: "12-pixel RGB virtual ring"},
		{Subsystem: "touch", BackendID: "sim.evdev", Status: Available, Detail: "virtual contact/gesture surface"},
		{Subsystem: "audio", BackendID: "sim.audio", Status: Available, Detail: "virtual 16 kHz capture / 48 kHz stereo playback"},
		{Subsystem: "sensors", BackendID: "sim.sensors", Status: Available, Detail: "virtual ambient light sensor"},
		{Subsystem: "wakeword", BackendID: "sim.wakeword", Status: Available, Detail: "manual activation trigger"},
	}
	return s
}

type SimulatorOptions struct {
	DeviceID string
	Model    string
}

func (s *Simulator) Platform() Platform { return PlatformSimulator }
func (s *Simulator) Info() DeviceInfo   { return s.info }
func (s *Simulator) Capabilities() []SubsystemStatus {
	return append([]SubsystemStatus(nil), s.caps...)
}
func (s *Simulator) Segment() SegmentDevice { return s.segment }
func (s *Simulator) Ring() RingDevice       { return s.ring }
func (s *Simulator) Audio() AudioDevice     { return s.audio }
func (s *Simulator) Touch() TouchDevice     { return s.touch }
func (s *Simulator) Sensors() Sensors       { return s.sensors }

func (s *Simulator) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.touch.Close()
	s.audio.Close()
	s.sensors.Close()
	return nil
}

// SegmentSnapshot returns the most recent virtual hardware frame.
func (s *Simulator) SegmentSnapshot() SegmentFrame { return s.segment.Snapshot() }

// RingSnapshot returns the most recent virtual ring frame.
func (s *Simulator) RingSnapshot() RingFrame { return s.ring.Snapshot() }

// InjectPCM feeds the virtual microphone for wakeword/mic tests.
func (s *Simulator) InjectPCM(pcm []byte) { s.audio.InjectPCM(pcm) }

// --- segment ---------------------------------------------------------------

type simSegment struct {
	mu    sync.Mutex
	frame SegmentFrame
	caps  SegmentCaps
}

func newSimSegment() *simSegment {
	return &simSegment{caps: SegmentCaps{DigitCount: 4, SupportsColon: true, SupportsDecimal: true, SegmentsPerDigit: 7}}
}

func (s *simSegment) Caps() SegmentCaps  { return s.caps }
func (s *simSegment) ResourceID() string { return "display.segment.front" }
func (s *simSegment) WriteSegment(f SegmentFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frame = SegmentFrame{Cells: append([]uint16(nil), f.Cells...), Colon: f.Colon, Brightness: f.Brightness}
	return nil
}

// Snapshot returns the most recent hardware frame for diagnostics/tests.
func (s *simSegment) Snapshot() SegmentFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SegmentFrame{Cells: append([]uint16(nil), s.frame.Cells...), Colon: s.frame.Colon, Brightness: s.frame.Brightness}
}

// --- ring ------------------------------------------------------------------

type simRing struct {
	mu    sync.Mutex
	frame RingFrame
	caps  RingCaps
}

func newSimRing() *simRing {
	return &simRing{caps: RingCaps{PixelCount: 12, SupportsRGB: true}}
}

func (r *simRing) Caps() RingCaps     { return r.caps }
func (r *simRing) ResourceID() string { return "light.ring.main" }
func (r *simRing) WriteRing(f RingFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frame = RingFrame{Pixels: append([]RGB(nil), f.Pixels...), Brightness: f.Brightness}
	return nil
}

func (r *simRing) Snapshot() RingFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RingFrame{Pixels: append([]RGB(nil), r.frame.Pixels...), Brightness: r.frame.Brightness}
}

// --- audio -----------------------------------------------------------------

type simAudio struct {
	mu      sync.Mutex
	volume  float64
	muted   bool
	sink    *simSink
	source  *simSource
	closed  bool
	capture []byte // injected PCM for wakeword/mic tests
}

func newSimAudio() *simAudio {
	return &simAudio{volume: 0.6}
}

func (a *simAudio) PlaybackFormat() AudioFormat {
	return AudioFormat{Format: S16LE, RateHz: 48000, Channels: 2}
}
func (a *simAudio) CaptureFormat() AudioFormat {
	return AudioFormat{Format: S16LE, RateHz: 16000, Channels: 1}
}
func (a *simAudio) OpenOutput() (Sink, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrUnavailable
	}
	a.sink = &simSink{audio: a}
	return a.sink, nil
}
func (a *simAudio) OpenInput() (Source, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrUnavailable
	}
	a.source = &simSource{audio: a, frames: make(chan []byte, 64)}
	return a.source, nil
}
func (a *simAudio) SetMasterVolume(v float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.volume = clamp01(v)
	return nil
}
func (a *simAudio) SetHardwareMute(muted bool) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.muted = muted
	return false, nil // simulator has no hardware mute
}
func (a *simAudio) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}

// InjectPCM queues capture PCM for connected sources (tests/dev only).
func (a *simAudio) InjectPCM(pcm []byte) {
	a.mu.Lock()
	src := a.source
	a.mu.Unlock()
	if src != nil {
		cp := append([]byte(nil), pcm...)
		select {
		case src.frames <- cp:
		default:
		}
	}
}

// LastOutput returns the tail of playback written to the virtual sink.
func (a *simAudio) LastOutput() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sink == nil {
		return nil
	}
	return a.sink.tail()
}

type simSink struct {
	audio *simAudio
	mu    sync.Mutex
	buf   []byte
	peak  int
}

func (s *simSink) WritePCM(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > 1<<20 {
		s.buf = s.buf[len(s.buf)-(1<<20):]
	}
	for _, b := range p {
		if int(b) > s.peak {
			s.peak = int(b)
		}
	}
	return nil
}
func (s *simSink) Close() error { return nil }
func (s *simSink) tail() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf...)
}

type simSource struct {
	audio  *simAudio
	frames chan []byte
	mu     sync.Mutex
	closed bool
	carry  []byte
}

func (s *simSource) ReadPCM(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrUnavailable
	}
	carry := s.carry
	s.mu.Unlock()
	if len(carry) == 0 {
		select {
		case b := <-s.frames:
			carry = b
		case <-time.After(20 * time.Millisecond):
			// Silence keeps the capture clock advancing.
			carry = make([]byte, len(p))
		}
	}
	n := copy(p, carry)
	s.mu.Lock()
	if n < len(carry) {
		s.carry = carry[n:]
	} else {
		s.carry = nil
	}
	s.mu.Unlock()
	return n, nil
}

func (s *simSource) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// --- touch -----------------------------------------------------------------

type simTouch struct {
	mu     sync.Mutex
	emit   func(InputEvent)
	closed bool
	caps   TouchCaps
}

func newSimTouch() *simTouch {
	return &simTouch{caps: TouchCaps{
		PartialGestures:  true,
		FullPanel:        true,
		FullRaw:          true,
		Coordinates:      true,
		Pressure:         false,
		SemanticGestures: []string{"tap", "double_tap", "hold", "swipe_left", "swipe_right", "swipe_up", "swipe_down", "circle_cw", "circle_ccw", "primary"},
	}}
}

func (t *simTouch) Caps() TouchCaps { return t.caps }
func (t *simTouch) Start(ctx context.Context, emit func(InputEvent)) error {
	t.mu.Lock()
	t.emit = emit
	t.mu.Unlock()
	<-ctx.Done()
	return nil
}
func (t *simTouch) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

// Emit injects a synthetic input event (tests/dev admin only).
func (t *simTouch) Emit(e InputEvent) {
	t.mu.Lock()
	emit := t.emit
	t.mu.Unlock()
	if emit != nil {
		emit(e)
	}
}

// --- sensors ---------------------------------------------------------------

type simSensors struct {
	mu     sync.Mutex
	closed bool
}

func newSimSensors() *simSensors { return &simSensors{} }

func (s *simSensors) List() []SensorInfo {
	return []SensorInfo{{ID: "sensor.ambient_light", Unit: "lux"}}
}

func (s *simSensors) Read(id string) (float64, bool) {
	if id != "sensor.ambient_light" {
		return 0, false
	}
	return 42, true
}
func (s *simSensors) Close() error { return nil }

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
