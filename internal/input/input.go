// Package input interprets the physical touch panel.
//
// The framework owns gesture interpretation. Partial capture forwards semantic
// gestures to the capturing app while the framework still handles volume and
// microphone-toggle controls; full/raw capture forwards backend touch samples
// and disables every built-in interpretation for the duration. A gesture in
// progress is cancelled cleanly when capture ownership changes.
package input

import (
	"context"
	"sync"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/routing"
)

const holdThreshold = 700 * time.Millisecond

// volumeRepeatInterval mirrors the stock `volume_change_speed_ms` default (200)
// so holding a volume key ramps at the same speed.
// keyActivationHold is how long the talk button must be held before the
// assistant is summoned; stock activates on a hold, not on a tap.
const keyActivationHold = 700 * time.Millisecond

// Volume ramps start at the stock repeat speed and accelerate while the key
// stays down, so a long hold traverses the range quickly.
const (
	volumeRepeatStart = 200 * time.Millisecond
	volumeRepeatFloor = 60 * time.Millisecond
	volumeRepeatDecay = 0.8
	// volumeRepeatMax bounds a ramp in case a key release is lost.
	volumeRepeatMax = 8 * time.Second
)

type Handlers struct {
	// Volume handlers report whether the value actually moved, so a ramp stops
	// when the volume hits a bound instead of ticking at the limit.
	VolumeUp   func() bool
	VolumeDown func() bool
	MicToggle  func()
	Activation func(origin string)
	Release    func()
	HoldToTalk func() bool
}

// State reports the currently effective touch capture.
type State func() (captureID, appID string, mode routing.TouchMode)

type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	clock  *ids.Clock
	logs   *logbus.Bus
	caps   backend.TouchCaps
	state  State
	h      Handlers

	mu      sync.Mutex
	subs    map[string]map[int]chan backend.InputEvent
	nextSub int
	seq     *sequence
	lastTap uint64
	volStop chan struct{}

	actTimer *time.Timer
	actFired bool
}

type sequence struct {
	owner     string // capture ID, or "" for the framework
	appID     string
	mode      routing.TouchMode
	startNS   uint64
	holdFired bool
	holdTimer *time.Timer
}

func NewManager(parent context.Context, clock *ids.Clock, logs *logbus.Bus, caps backend.TouchCaps, state State, h Handlers) *Manager {
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		ctx: ctx, cancel: cancel, clock: clock, logs: logs, caps: caps, state: state, h: h,
		subs: map[string]map[int]chan backend.InputEvent{},
	}
}

func (m *Manager) Capabilities() backend.TouchCaps { return m.caps }

// Handle consumes one backend input event.
func (m *Manager) Handle(e backend.InputEvent) {
	switch {
	case e.Report != nil:
		m.handleReport(e)
	case e.Gesture != nil:
		m.handleGesture(e)
	case e.Key != nil:
		m.handleKey(e)
	}
}

func (m *Manager) handleReport(e backend.InputEvent) {
	captureID, _, mode := m.state()
	if mode != routing.TouchFull {
		// Raw samples are only delivered during full capture.
		return
	}
	m.deliver(captureID, backend.InputEvent{MonotonicNS: e.MonotonicNS, Report: e.Report})
}

func (m *Manager) handleKey(e backend.InputEvent) {
	captureID, appID, mode := m.state()
	key := e.Key.Key
	pressed := e.Key.Pressed
	if mode == routing.TouchFull {
		m.deliver(captureID, e)
		return
	}
	switch key {
	case "volume_up", "volume_down":
		step := m.h.VolumeUp
		if key == "volume_down" {
			step = m.h.VolumeDown
		}
		if pressed {
			if step != nil {
				step()
			}
			m.startVolumeRepeat(step)
			return
		}
		m.startVolumeRepeat(nil)
	case "mic_toggle":
		if pressed && m.h.MicToggle != nil {
			m.h.MicToggle()
		}
	case "voice", "play_pause":
		// Stock uses the play/pause (circle) button as the talk button and
		// activates on a hold: short taps do nothing, holding past the
		// threshold summons, releasing ends the turn. The device tree also
		// declares a voice key, so both keys behave the same.
		if pressed {
			m.beginKeyActivation(keyOrigin(key))
			return
		}
		m.endKeyActivation()
	case "primary":
		m.handlePrimary(e, captureID, appID, mode)
	}
}

// beginKeyActivation arms the hold-to-talk timer for a key press.
func (m *Manager) beginKeyActivation(origin string) {
	m.mu.Lock()
	if m.actTimer != nil {
		m.mu.Unlock()
		return
	}
	m.actFired = false
	m.actTimer = time.AfterFunc(keyActivationHold, func() {
		m.mu.Lock()
		held := m.actTimer != nil
		m.actFired = held
		m.mu.Unlock()
		if held && m.h.Activation != nil {
			m.h.Activation(origin)
		}
	})
	m.mu.Unlock()
}

// endKeyActivation cancels a pending summon, or ends the turn when the user
// chose hold-to-talk. The default (hold to summon, matching stock) lets the
// conversation outlive the key press.
func (m *Manager) endKeyActivation() {
	m.mu.Lock()
	timer := m.actTimer
	fired := m.actFired
	m.actTimer = nil
	m.actFired = false
	m.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	holdToTalk := m.h.HoldToTalk != nil && m.h.HoldToTalk()
	if fired && holdToTalk && m.h.Release != nil {
		m.h.Release()
	}
}

// keyOrigin names the activation source for diagnostics.
func keyOrigin(key string) string {
	if key == "play_pause" {
		return "play_pause_key"
	}
	return "voice_key"
}

// startVolumeRepeat ramps the volume while a volume key is held. The interval
// decays from the stock repeat speed toward volumeRepeatFloor, and the ramp
// stops early when the handler reports the value did not move (a bound) or a
// full input capture takes the keys over. Passing nil stops an active ramp.
func (m *Manager) startVolumeRepeat(step func() bool) {
	m.mu.Lock()
	if m.volStop != nil {
		close(m.volStop)
		m.volStop = nil
	}
	if step == nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	m.volStop = stop
	m.mu.Unlock()
	go func() {
		interval := volumeRepeatStart
		deadline := time.Now().Add(volumeRepeatMax)
		for {
			timer := time.NewTimer(interval)
			select {
			case <-m.ctx.Done():
				timer.Stop()
				return
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			if time.Now().After(deadline) {
				return
			}
			if _, _, mode := m.state(); mode == routing.TouchFull {
				return
			}
			if !step() {
				return
			}
			interval = time.Duration(float64(interval) * volumeRepeatDecay)
			if interval < volumeRepeatFloor {
				interval = volumeRepeatFloor
			}
		}
	}()
}

func (m *Manager) handlePrimary(e backend.InputEvent, captureID, appID string, mode routing.TouchMode) {
	m.mu.Lock()
	owner := captureID
	if mode != routing.TouchPartial {
		owner = ""
	}
	if e.Key.Pressed {
		if m.seq != nil {
			m.mu.Unlock()
			return
		}
		seq := &sequence{owner: owner, appID: appID, mode: mode, startNS: e.MonotonicNS}
		m.seq = seq
		now := e.MonotonicNS
		doubleTap := m.lastTap != 0 && time.Duration(int64(now-m.lastTap)) < 350*time.Millisecond
		seq.holdTimer = time.AfterFunc(holdThreshold, func() { m.holdElapsed(seq) })
		m.mu.Unlock()
		m.deliver(owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "primary", Phase: "start"}})
		if doubleTap {
			m.deliver(owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "double_tap", Phase: "start"}})
		}
		if owner == "" {
			if m.h.Activation != nil {
				m.h.Activation("touch")
			}
		}
		return
	}
	seq := m.seq
	if seq == nil {
		m.mu.Unlock()
		return
	}
	m.seq = nil
	if seq.holdTimer != nil {
		seq.holdTimer.Stop()
	}
	held := time.Duration(int64(e.MonotonicNS) - int64(seq.startNS))
	doubleTap := m.lastTap != 0 && time.Duration(int64(e.MonotonicNS-m.lastTap)) < 350*time.Millisecond && held < 400*time.Millisecond
	if held < 400*time.Millisecond {
		m.lastTap = e.MonotonicNS
	}
	m.mu.Unlock()

	m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "primary", Phase: "end"}})
	if seq.holdFired {
		m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "hold", Phase: "end"}})
	} else if !doubleTap {
		m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "tap", Phase: "end"}})
	}
	if doubleTap {
		m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "double_tap", Phase: "end"}})
	}
	if seq.owner == "" {
		if seq.holdFired && m.h.HoldToTalk != nil && m.h.HoldToTalk() {
			if m.h.Release != nil {
				m.h.Release()
			}
		}
	}
}

func (m *Manager) holdElapsed(seq *sequence) {
	m.mu.Lock()
	if m.seq != seq {
		m.mu.Unlock()
		return
	}
	seq.holdFired = true
	owner := seq.owner
	m.mu.Unlock()
	m.deliver(owner, backend.InputEvent{MonotonicNS: m.clock.NowNS(), Gesture: &backend.GestureEvent{ID: "hold", Phase: "start"}})
}

func (m *Manager) handleGesture(e backend.InputEvent) {
	captureID, _, mode := m.state()
	if mode == routing.TouchFull {
		// Full/raw capture does not accept semantic gestures the backend
		// inferred; that would silently degrade to partial capture.
		return
	}
	owner := captureID
	if mode != routing.TouchPartial {
		owner = ""
	}
	if owner == "" {
		switch e.Gesture.ID {
		case "tap", "primary":
			if e.Gesture.Phase == "end" && m.h.Activation != nil {
				m.h.Activation("touch")
			}
		case "hold":
			if e.Gesture.Phase == "start" && m.h.Activation != nil {
				m.h.Activation("touch")
			}
			if e.Gesture.Phase == "end" && m.h.HoldToTalk != nil && m.h.HoldToTalk() && m.h.Release != nil {
				m.h.Release()
			}
		}
		return
	}
	m.deliver(owner, e)
}

func (m *Manager) deliver(captureID string, e backend.InputEvent) {
	if captureID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.subs[captureID] {
		select {
		case ch <- e:
		default:
			m.logs.Warnf("input", "", "input consumer for capture %s is slow; event dropped", captureID)
		}
	}
}

// Subscription is one application's input stream.
type Subscription struct {
	C    <-chan backend.InputEvent
	id   int
	capt string
	mgr  *Manager
	once sync.Once
}

func (s *Subscription) Close() {
	s.once.Do(func() {
		s.mgr.mu.Lock()
		if m := s.mgr.subs[s.capt]; m != nil {
			if ch, ok := m[s.id]; ok {
				close(ch)
				delete(m, s.id)
			}
		}
		s.mgr.mu.Unlock()
	})
}

// Subscribe validates that captureID is the currently selected touch capture.
func (m *Manager) Subscribe(captureID string) (*Subscription, error) {
	selected, _, mode := m.state()
	if captureID == "" || captureID != selected {
		return nil, errNotSelected
	}
	if mode != routing.TouchPartial && mode != routing.TouchFull {
		return nil, errNotSelected
	}
	m.mu.Lock()
	m.nextSub++
	id := m.nextSub
	ch := make(chan backend.InputEvent, 32)
	if m.subs[captureID] == nil {
		m.subs[captureID] = map[int]chan backend.InputEvent{}
	}
	m.subs[captureID][id] = ch
	m.mu.Unlock()
	return &Subscription{C: ch, id: id, capt: captureID, mgr: m}, nil
}

var errNotSelected = errString("capture_id is not the caller's currently selected touch capture")

type errString string

func (e errString) Error() string { return string(e) }

// OnRoutingChanged cancels any gesture in progress if the effective owner
// would change, and closes subscriptions for captures that lost selection.
func (m *Manager) OnRoutingChanged() {
	captureID, _, mode := m.state()
	m.mu.Lock()
	seq := m.seq
	var cancelTo string
	if seq != nil && (seq.owner != captureID || (mode != routing.TouchPartial && seq.owner != "")) {
		if seq.holdTimer != nil {
			seq.holdTimer.Stop()
		}
		cancelTo = seq.owner
		m.seq = nil
	}
	for capID, subs := range m.subs {
		if capID != captureID {
			for id, ch := range subs {
				close(ch)
				delete(subs, id)
			}
			delete(m.subs, capID)
		}
	}
	m.mu.Unlock()
	if cancelTo != "" {
		m.deliver(cancelTo, backend.InputEvent{MonotonicNS: m.clock.NowNS(),
			Gesture: &backend.GestureEvent{ID: "cancelled", Phase: "cancelled"}})
	}
}

func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	for _, subs := range m.subs {
		for id, ch := range subs {
			close(ch)
			delete(subs, id)
		}
	}
	m.mu.Unlock()
}
