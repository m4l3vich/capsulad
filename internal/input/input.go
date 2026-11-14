// Package input interprets the physical touch panel.
//
// On Capsula Mini the panel is five discrete keys: microphone mute (north),
// volume up (east), play/pause (south), volume down (west) and an unmarked
// neutral centre. The play/pause circle is the talk key. There are no raw
// coordinates; swipe and circle gestures are inferred from ordered press
// sequences of those keys, never from invented positions.
//
// The framework owns gesture interpretation. Partial capture forwards the
// circle/play-pause and centre keys plus semantic gestures to the capturing
// app, while volume and microphone stay framework controls. Full capture
// forwards every key and gesture and disables every built-in side effect,
// including mute, volume, media transport and assistant activation. A gesture
// in progress is cancelled cleanly when capture ownership changes.
//
// A sounding framework alert (a clock timer or alarm) takes priority: a circle
// or centre press dismisses it and is consumed, so one press cannot both
// silence the alert and summon the assistant. Volume and microphone keys keep
// their safety behaviour and never dismiss an alert.
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
	// DismissAlert silences a sounding framework alert (a clock timer or
	// alarm) and reports whether one was showing. A true result consumes the
	// physical control that asked for it, so the same press cannot also summon
	// the assistant: its release, hold and repeat are swallowed. Volume and
	// microphone keys never call this; they keep their safety behaviour.
	DismissAlert func() bool
	// DismissAssistant ends the selected assistant's current turn without
	// starting a new one. A true result consumes the press and its release,
	// exactly like alert dismissal. It is consulted after DismissAlert.
	DismissAssistant func() bool
	// PrimaryActive reports whether the framework owns the primary button
	// outside the assistant (a running stopwatch). While true, a press does
	// not summon the assistant; its tap, double tap and hold go to
	// PrimaryGesture instead.
	PrimaryActive func() bool
	// PrimaryGesture handles a framework-owned primary gesture: "tap",
	// "double_tap" or "hold". Returning true means the interaction belonged to
	// the framework and no assistant activation is produced.
	PrimaryGesture func(kind string) bool
	// MediaToggle, MediaPrevious and MediaNext drive the current standard
	// media provider. A short circle tap toggles playback; inferred left and
	// right swipes select the previous and next item. They are never called
	// during a full capture or from a gesture whose constituent key effects
	// were deferred.
	MediaToggle   func()
	MediaPrevious func()
	MediaNext     func()
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
	// volKey owns the active volume ramp. Releasing the other volume key must
	// not stop it, and a duplicate press of the owning key must not step twice.
	volKey string

	actTimer *time.Timer
	actFired bool
	// actConsumed is set when a talk-key press dismissed an alert; the
	// matching release is swallowed so one press cannot both dismiss and
	// summon.
	actConsumed bool
	// actFramework marks a press the framework owns (a stopwatch interaction)
	// rather than an assistant summon. Its release produces a tap, double tap
	// or hold for PrimaryGesture and never activates the assistant.
	actFramework bool
	actStartNS   uint64
	// lastKeyTap is the monotonic time of the previous short talk-key tap, used
	// to recognize a quick double tap. It is separate from the touch panel's
	// lastTap because they are different physical controls.
	lastKeyTap uint64

	// gestureHeld records that the framework summoned on a hold, so a stray
	// hold end cannot release an unrelated turn. gestureConsumed swallows the
	// end of a hold that dismissed an alert instead.
	gestureHeld     bool
	gestureConsumed bool

	// panelMu owns the discrete-panel sequence recognizer. It is separate from
	// mu so sequence resolution can call the existing key helpers without
	// lock inversion.
	// panelExecMu serializes a panel state transition with the effects it
	// produces. A timeout and the matching release must not interleave between
	// "state changed" and "press action applied", or the release could be
	// processed before the press it belongs to.
	panelExecMu  sync.Mutex
	panelMu      sync.Mutex
	panelSeq     *panelSequence
	panelDown    map[string]bool
	panelSwallow map[string]bool
	panelForward map[string]string
	panelHandled map[string]bool
	fullSeq      []string
	fullOverlap  []bool
	fullDown     map[string]bool
	fullStartNS  uint64
	fullLastNS   uint64
}

type sequence struct {
	owner      string // capture ID, or "" for the framework
	appID      string
	mode       routing.TouchMode
	startNS    uint64
	holdFired  bool
	holdToTalk bool
	holdTimer  *time.Timer
	// dismissed marks a contact whose press silenced an alert. Its release is
	// consumed: no tap, hold or release event is produced from that press.
	dismissed bool
	// framework marks a contact owned by a framework interaction (a stopwatch
	// pause/reset). Its gestures go to PrimaryGesture, not the assistant.
	framework bool
	// summon is false for the neutral centre key: it dismisses alerts and
	// drives stopwatch gestures but never activates the assistant.
	summon bool
}

func NewManager(parent context.Context, clock *ids.Clock, logs *logbus.Bus, caps backend.TouchCaps, state State, h Handlers) *Manager {
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		ctx: ctx, cancel: cancel, clock: clock, logs: logs, caps: caps, state: state, h: h,
		subs:         map[string]map[int]chan backend.InputEvent{},
		panelDown:    map[string]bool{},
		panelSwallow: map[string]bool{},
		panelForward: map[string]string{},
		panelHandled: map[string]bool{},
		fullDown:     map[string]bool{},
	}
}

func (m *Manager) Capabilities() backend.TouchCaps { return m.caps }

// Handle consumes one backend input event.
func (m *Manager) Handle(e backend.InputEvent) {
	// The backend may know how old the kernel event was when it managed to read
	// it. Anchor that age to this manager's clock so a delayed read preserves
	// the physical inter-contact gap and the hold deadline (both compared with
	// clock.NowNS) instead of collapsing everything to read time. A zero or
	// impossible age keeps the read-time path.
	if e.MonotonicNS == 0 && e.EventAgeNS != 0 {
		if now := m.clock.NowNS(); e.EventAgeNS < now {
			e.MonotonicNS = now - e.EventAgeNS
		}
	}
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
	// Read ownership under the same lock that serializes panel state
	// transitions. A capture takeover cannot clear bookkeeping between this
	// snapshot and the bookkeeping below; a key sampled before full capture
	// acquired the panel is routed to the full owner under the current state.
	m.panelExecMu.Lock()
	defer m.panelExecMu.Unlock()
	captureID, appID, mode := m.state()
	key := e.Key.Key
	pressed := e.Key.Pressed
	if mode == routing.TouchFull {
		if isPanelKey(key) {
			m.fullPanelKey(key, pressed, eventNS(m.clock, e.MonotonicNS), captureID)
		}
		m.logs.Debugf("input", "", "key %s pressed=%t forwarded to capture %s (full)", key, pressed, captureID)
		m.deliver(captureID, e)
		return
	}
	switch key {
	case "volume_up", "volume_down", "mic_toggle", "play_pause", "center":
		m.handlePanelKey(e, captureID, appID, mode)
	case "primary":
		// "primary" is the generic logical panel control (simulator and
		// hardware that really exposes one). Capsula Mini's unmarked centre
		// key is "center" instead: it never activates the assistant.
		m.handlePrimary(e, captureID, appID, mode, true)
	}
}

// dismissAlert asks the framework whether a sounding alert should be silenced
// by the physical control that was just pressed.
func (m *Manager) dismissAlert() bool {
	return m.h.DismissAlert != nil && m.h.DismissAlert()
}

// dismissAssistant asks the framework whether the selected assistant's current
// turn should end. It is only consulted for a framework-owned press.
func (m *Manager) dismissAssistant() bool {
	return m.h.DismissAssistant != nil && m.h.DismissAssistant()
}

// primaryActive reports whether a framework interaction (a stopwatch) owns the
// primary button right now.
func (m *Manager) primaryActive() bool {
	return m.h.PrimaryActive != nil && m.h.PrimaryActive()
}

// primaryGesture routes a framework-owned tap, double tap or hold. The result
// only affects diagnostics: the press was already consumed at press time, so
// an unhandled gesture still must not summon the assistant.
func (m *Manager) primaryGesture(kind string) {
	if m.h.PrimaryGesture == nil {
		return
	}
	if m.h.PrimaryGesture(kind) {
		m.logs.Infof("input", "", "framework primary gesture %s handled", kind)
		return
	}
	m.logs.Debugf("input", "", "framework primary gesture %s had no effect", kind)
}

// pressTalkKey reports whether a talk-key press was consumed, either as a
// duplicate of a press that is still down, by dismissing an alert or an
// assistant turn, or because a framework interaction (a stopwatch) owns the
// button. Checking the held state first is what keeps an alert that starts
// during a genuine hold from consuming that hold's release.
func (m *Manager) pressTalkKey(key string, startNS uint64) bool {
	m.mu.Lock()
	armed := m.actTimer != nil
	consumed := m.actConsumed
	fired := m.actFired
	m.mu.Unlock()
	if armed || consumed {
		if consumed {
			m.logs.Debugf("input", "", "key %s press reproduced while its dismissal press is held", key)
		} else if fired {
			m.logs.Infof("input", "", "duplicate talk press ignored: previous press was never released")
		} else {
			m.logs.Debugf("input", "", "duplicate talk press ignored")
		}
		return true
	}
	if m.dismissAlert() {
		m.mu.Lock()
		m.actConsumed = true
		m.mu.Unlock()
		m.logs.Infof("input", "", "key %s press: alert dismissed; activation suppressed", key)
		return true
	}
	// An assistant turn in progress is dismissed by this press. The matching
	// release is swallowed: the turn ended, so there is nothing left to release.
	if m.dismissAssistant() {
		m.mu.Lock()
		m.actConsumed = true
		m.mu.Unlock()
		m.logs.Infof("input", "", "key %s press: assistant turn dismissed; activation suppressed", key)
		return true
	}
	// A running interaction (a stopwatch) owns the button: its tap, double tap
	// and hold are handled later, and this press never summons the assistant.
	if m.primaryActive() {
		m.beginFrameworkKey(key, startNS)
		m.logs.Infof("input", "", "key %s press: framework gesture armed (assistant not summoned)", key)
		return true
	}
	return false
}

// beginFrameworkKey arms the hold timer for a press the framework owns. The
// hold is delivered at the threshold so a configured hold-to-reset acts while
// the button is still down; the release delivers a tap or double tap.
func (m *Manager) beginFrameworkKey(key string, startNS uint64) {
	m.mu.Lock()
	if m.actTimer != nil {
		m.mu.Unlock()
		m.logs.Debugf("input", "", "duplicate framework talk press ignored")
		return
	}
	m.actFired = false
	m.actFramework = true
	m.actStartNS = startNS
	m.actTimer = time.AfterFunc(remainingHold(m.clock, startNS, keyActivationHold), func() {
		m.mu.Lock()
		held := m.actTimer != nil && m.actFramework
		if held {
			m.actFired = true
		}
		m.mu.Unlock()
		if held {
			m.logs.Infof("input", "", "talk hold: framework gesture (key=%s)", key)
			m.primaryGesture("hold")
		}
	})
	m.mu.Unlock()
}

// releaseTalkKey swallows the release that follows a dismissing press, routes
// a framework-owned release as a tap or double tap, and otherwise ends the
// assistant activation as before.
func (m *Manager) releaseTalkKey(key string, ts uint64) {
	m.mu.Lock()
	consumed := m.actConsumed
	framework := m.actFramework
	if !consumed && !framework {
		m.mu.Unlock()
		m.logs.Debugf("input", "", "key %s release (mode=framework)", key)
		m.endKeyActivation()
		return
	}
	fired := m.actFired
	timer := m.actTimer
	start := m.actStartNS
	now := ts
	var doubleTap bool
	if framework && timer != nil && !fired {
		held := time.Duration(int64(now) - int64(start))
		doubleTap = m.lastKeyTap != 0 && time.Duration(int64(now-m.lastKeyTap)) < 350*time.Millisecond && held < 400*time.Millisecond
		if held < 400*time.Millisecond {
			m.lastKeyTap = now
		}
	}
	m.actConsumed = false
	m.actFramework = false
	m.actTimer = nil
	m.actFired = false
	m.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if consumed {
		m.logs.Infof("input", "", "key %s release: consumed with dismissal", key)
		return
	}
	if fired {
		m.logs.Infof("input", "", "key %s release: framework hold complete", key)
		return
	}
	if doubleTap {
		m.primaryGesture("double_tap")
		return
	}
	m.primaryGesture("tap")
}

// beginKeyActivation arms the hold-to-talk timer for a key press.
func (m *Manager) beginKeyActivation(origin string, startNS uint64) {
	m.mu.Lock()
	if m.actTimer != nil {
		fired := m.actFired
		m.mu.Unlock()
		if fired {
			m.logs.Infof("input", "", "duplicate talk press origin=%s ignored: previous press was never released", origin)
		} else {
			m.logs.Debugf("input", "", "duplicate talk press origin=%s ignored", origin)
		}
		return
	}
	m.actFired = false
	m.actStartNS = startNS
	m.actTimer = time.AfterFunc(remainingHold(m.clock, startNS, keyActivationHold), func() {
		m.mu.Lock()
		held := m.actTimer != nil
		m.actFired = held
		m.mu.Unlock()
		if held && m.h.Activation != nil {
			m.logs.Infof("input", "", "talk hold: activating assistant (origin=%s)", origin)
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
	if timer == nil {
		// No press is armed: a stray or duplicate release must not release a
		// turn it did not start.
		m.logs.Debugf("input", "", "talk release without an armed press")
		return
	}
	timer.Stop()
	holdToTalk := m.h.HoldToTalk != nil && m.h.HoldToTalk()
	if !fired {
		// A circle tap is not a hold: it toggles the current media provider
		// instead of summoning the assistant.
		m.logs.Infof("input", "", "talk release: short tap toggles media")
		if m.h.MediaToggle != nil {
			m.h.MediaToggle()
		}
		return
	}
	if holdToTalk {
		if m.h.Release != nil {
			m.logs.Infof("input", "", "talk release: turn released (hold-to-talk)")
			m.h.Release()
		}
		return
	}
	m.logs.Infof("input", "", "talk release: conversation stays open (hold-to-summon)")
}

// keyOrigin names the activation source for diagnostics.
func keyOrigin(string) string { return "play_pause_key" }

// pressVolume applies one step and starts a ramp for key. A duplicate press of
// the same key (backend repeat or a lost release) is ignored so the value does
// not jump twice and the ramp does not restart.
func (m *Manager) pressVolume(key string, step func() bool) {
	if step == nil {
		return
	}
	m.mu.Lock()
	active := m.volKey == key
	m.mu.Unlock()
	if active {
		m.logs.Debugf("input", "", "duplicate %s press ignored: ramp already running", key)
		return
	}
	step()
	m.startVolumeRepeat(key, step)
}

// releaseVolume stops the ramp only when its own key is released. Releasing
// one volume key must not silence a ramp the other key started.
func (m *Manager) releaseVolume(key string) {
	m.mu.Lock()
	if m.volKey != key || m.volStop == nil {
		m.mu.Unlock()
		return
	}
	stop := m.volStop
	m.volStop = nil
	m.volKey = ""
	m.mu.Unlock()
	close(stop)
}

// startVolumeRepeat ramps the volume while a volume key is held. The interval
// decays from the stock repeat speed toward volumeRepeatFloor, and the ramp
// stops early when the handler reports the value did not move (a bound) or a
// full input capture takes the keys over.
func (m *Manager) startVolumeRepeat(key string, step func() bool) {
	m.mu.Lock()
	if m.volStop != nil {
		close(m.volStop)
	}
	stop := make(chan struct{})
	m.volStop = stop
	m.volKey = key
	m.mu.Unlock()
	go func() {
		defer m.endVolumeRepeat(key, stop)
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

// endVolumeRepeat clears ownership when the ramp stops on its own (a bound,
// the deadline, or a full capture taking the keys over), so a later release is
// a no-op and a fresh press is not mistaken for a duplicate.
func (m *Manager) endVolumeRepeat(key string, stop chan struct{}) {
	m.mu.Lock()
	if m.volStop == stop {
		m.volStop = nil
		if m.volKey == key {
			m.volKey = ""
		}
	}
	m.mu.Unlock()
}

func (m *Manager) handlePrimary(e backend.InputEvent, captureID, appID string, mode routing.TouchMode, summon bool) {
	owner := captureID
	if mode != routing.TouchPartial {
		owner = ""
	}
	if e.Key.Pressed {
		m.mu.Lock()
		if m.seq != nil {
			m.mu.Unlock()
			m.logs.Debugf("input", "", "key primary press ignored: gesture already in progress (mode=%s)", mode)
			return
		}
		m.mu.Unlock()

		// When the framework owns the panel, a sounding alert takes the press:
		// dismissing it must not also summon the assistant, and the matching
		// release (plus any hold) is swallowed below.
		if owner == "" && m.dismissAlert() {
			m.mu.Lock()
			if m.seq == nil {
				m.seq = &sequence{owner: owner, appID: appID, mode: mode, startNS: e.MonotonicNS, dismissed: true}
			}
			m.mu.Unlock()
			m.logs.Infof("input", "", "key primary press: alert dismissed; activation suppressed (mode=%s)", mode)
			return
		}
		// An assistant turn in progress is dismissed by the press itself.
		if owner == "" && m.dismissAssistant() {
			m.mu.Lock()
			if m.seq == nil {
				m.seq = &sequence{owner: owner, appID: appID, mode: mode, startNS: e.MonotonicNS, dismissed: true}
			}
			m.mu.Unlock()
			m.logs.Infof("input", "", "key primary press: assistant turn dismissed; activation suppressed (mode=%s)", mode)
			return
		}
		// A running framework interaction (a stopwatch) owns the button. Its
		// hold is delivered at the threshold and its release as a tap, without
		// ever summoning the assistant.
		if owner == "" && m.primaryActive() {
			m.mu.Lock()
			if m.seq != nil {
				m.mu.Unlock()
				m.logs.Debugf("input", "", "key primary press ignored: gesture already in progress (mode=%s)", mode)
				return
			}
			seq := &sequence{owner: "", appID: appID, mode: mode, startNS: e.MonotonicNS, framework: true}
			seq.holdTimer = time.AfterFunc(holdThreshold, func() { m.holdElapsed(seq) })
			m.seq = seq
			m.mu.Unlock()
			m.logs.Infof("input", "", "key primary press: framework gesture armed (mode=%s)", mode)
			return
		}

		m.mu.Lock()
		if m.seq != nil {
			m.mu.Unlock()
			return
		}
		seq := &sequence{
			owner: owner, appID: appID, mode: mode, startNS: e.MonotonicNS,
			holdToTalk: m.h.HoldToTalk != nil && m.h.HoldToTalk(), summon: summon,
		}
		now := e.MonotonicNS
		doubleTap := m.lastTap != 0 && time.Duration(int64(now-m.lastTap)) < 350*time.Millisecond
		seq.holdTimer = time.AfterFunc(holdThreshold, func() { m.holdElapsed(seq) })
		m.seq = seq
		m.mu.Unlock()
		m.logs.Infof("input", "", "key primary press: gesture start double_tap=%t (mode=%s)", doubleTap, mode)
		m.deliver(owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "primary", Phase: "start"}})
		if doubleTap {
			m.deliver(owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "double_tap", Phase: "start"}})
		}
		if summon && owner == "" && !seq.holdToTalk {
			if m.h.Activation != nil {
				m.logs.Infof("input", "", "key primary press: activating assistant (touch)")
				m.h.Activation("touch")
			}
		}
		return
	}
	m.mu.Lock()
	seq := m.seq
	if seq == nil {
		m.mu.Unlock()
		m.logs.Debugf("input", "", "key primary release ignored: no gesture in progress")
		return
	}
	m.seq = nil
	if seq.holdTimer != nil {
		seq.holdTimer.Stop()
	}
	if seq.dismissed {
		m.mu.Unlock()
		m.logs.Infof("input", "", "key primary release: consumed with dismissal")
		return
	}
	held := time.Duration(int64(e.MonotonicNS) - int64(seq.startNS))
	doubleTap := m.lastTap != 0 && time.Duration(int64(e.MonotonicNS-m.lastTap)) < 350*time.Millisecond && held < 400*time.Millisecond
	if held < 400*time.Millisecond {
		m.lastTap = e.MonotonicNS
	}
	framework := seq.framework
	holdFired := seq.holdFired
	m.mu.Unlock()

	if framework {
		m.logs.Infof("input", "", "key primary release: held=%s hold=%t double_tap=%t (framework)",
			held.Round(time.Millisecond), holdFired, doubleTap)
		if holdFired {
			return // the hold gesture was delivered at the threshold
		}
		if doubleTap {
			m.primaryGesture("double_tap")
			return
		}
		m.primaryGesture("tap")
		return
	}
	m.logs.Infof("input", "", "key primary release: held=%s hold=%t tap=%t double_tap=%t (mode=%s)",
		held.Round(time.Millisecond), seq.holdFired, !seq.holdFired && !doubleTap, doubleTap, seq.mode)
	m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "primary", Phase: "end"}})
	if seq.holdFired {
		m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "hold", Phase: "end"}})
	} else if !doubleTap {
		m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "tap", Phase: "end"}})
	}
	if doubleTap {
		m.deliver(seq.owner, backend.InputEvent{MonotonicNS: e.MonotonicNS, Gesture: &backend.GestureEvent{ID: "double_tap", Phase: "end"}})
	}
	if seq.summon && seq.owner == "" {
		if seq.holdFired && seq.holdToTalk {
			if m.h.Release != nil {
				m.logs.Infof("input", "", "key primary hold release: turn released (hold-to-talk)")
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
	framework := seq.framework
	m.mu.Unlock()
	if framework {
		m.logs.Infof("input", "", "key primary hold: framework gesture")
		m.primaryGesture("hold")
		return
	}
	m.logs.Infof("input", "", "key primary hold: gesture start (owner=%s)", owner)
	m.deliver(owner, backend.InputEvent{MonotonicNS: m.clock.NowNS(), Gesture: &backend.GestureEvent{ID: "hold", Phase: "start"}})
	if seq.summon && owner == "" && seq.holdToTalk && m.h.Activation != nil {
		m.logs.Infof("input", "", "key primary hold: activating assistant (touch)")
		m.h.Activation("touch")
	}
}

func (m *Manager) handleGesture(e backend.InputEvent) {
	captureID, _, mode := m.state()
	if mode == routing.TouchFull {
		// Full/raw capture does not accept semantic gestures the backend
		// inferred; that would silently degrade to partial capture.
		m.logs.Debugf("input", "", "gesture %s %s ignored during full capture", e.Gesture.ID, e.Gesture.Phase)
		return
	}
	owner := captureID
	if mode != routing.TouchPartial {
		owner = ""
	}
	if owner == "" {
		switch e.Gesture.ID {
		case "tap", "primary":
			if e.Gesture.Phase != "end" {
				return
			}
			if m.dismissAlert() {
				m.logs.Infof("input", "", "gesture %s end: alert dismissed; activation suppressed", e.Gesture.ID)
				return
			}
			if m.dismissAssistant() {
				m.logs.Infof("input", "", "gesture %s end: assistant turn dismissed; activation suppressed", e.Gesture.ID)
				return
			}
			if m.primaryActive() {
				m.primaryGesture("tap")
				return
			}
			if m.h.Activation != nil {
				m.logs.Infof("input", "", "gesture %s end: activating assistant (touch)", e.Gesture.ID)
				m.h.Activation("touch")
			}
		case "double_tap":
			if e.Gesture.Phase != "end" {
				return
			}
			m.primaryGesture("double_tap")
		case "swipe_left":
			if e.Gesture.Phase == "end" {
				m.logs.Infof("input", "", "gesture swipe_left end: previous media")
				if m.h.MediaPrevious != nil {
					m.h.MediaPrevious()
				}
			}
		case "swipe_right":
			if e.Gesture.Phase == "end" {
				m.logs.Infof("input", "", "gesture swipe_right end: next media")
				if m.h.MediaNext != nil {
					m.h.MediaNext()
				}
			}
		case "hold":
			switch e.Gesture.Phase {
			case "start":
				m.mu.Lock()
				held := m.gestureHeld
				consumed := m.gestureConsumed
				m.mu.Unlock()
				if held || consumed {
					// A repeat of the press that is already down (or already
					// dismissed the alert) must not act twice or clobber the
					// held state its release depends on.
					m.logs.Debugf("input", "", "gesture hold start repeated while held; ignored")
					return
				}
				if m.dismissAlert() {
					// The hold silenced an alert. Its end must not release a
					// conversation it never started.
					m.mu.Lock()
					m.gestureConsumed = true
					m.mu.Unlock()
					m.logs.Infof("input", "", "gesture hold start: alert dismissed; activation suppressed")
					return
				}
				if m.dismissAssistant() {
					m.mu.Lock()
					m.gestureConsumed = true
					m.mu.Unlock()
					m.logs.Infof("input", "", "gesture hold start: assistant turn dismissed; activation suppressed")
					return
				}
				if m.primaryActive() {
					// A framework interaction owns the button: deliver its hold
					// and consume the matching end without summoning.
					m.mu.Lock()
					m.gestureConsumed = true
					m.mu.Unlock()
					m.primaryGesture("hold")
					return
				}
				m.mu.Lock()
				m.gestureHeld = true
				m.mu.Unlock()
				if m.h.Activation != nil {
					m.logs.Infof("input", "", "gesture hold start: activating assistant (touch)")
					m.h.Activation("touch")
				}
			case "end":
				m.mu.Lock()
				consumed := m.gestureConsumed
				held := m.gestureHeld
				m.gestureConsumed = false
				m.gestureHeld = false
				m.mu.Unlock()
				if consumed {
					m.logs.Infof("input", "", "gesture hold end: consumed with alert dismissal")
					return
				}
				if !held {
					// A stray or duplicated hold end must not release a turn
					// this gesture did not start.
					m.logs.Debugf("input", "", "gesture hold end without a hold start; ignored")
					return
				}
				if m.h.HoldToTalk != nil && m.h.HoldToTalk() {
					if m.h.Release != nil {
						m.logs.Infof("input", "", "gesture hold end: turn released (hold-to-talk)")
						m.h.Release()
					}
					return
				}
				m.logs.Infof("input", "", "gesture hold end: conversation stays open (hold-to-summon)")
			case "cancelled":
				m.mu.Lock()
				m.gestureConsumed = false
				m.gestureHeld = false
				m.mu.Unlock()
				m.logs.Infof("input", "", "gesture hold cancelled")
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
// Any capture mode takes the circle/centre keys over, so a pending talk or
// neutrally-armed centre press is dropped. Full capture also takes volume and
// microphone, so their ramps and deferred panel sequences are dropped instead
// of firing later against the new owner.
func (m *Manager) OnRoutingChanged() {
	captureID, _, mode := m.state()
	m.cancelPanelForRouting(captureID, mode)
	m.mu.Lock()
	seq := m.seq
	var cancelTo string
	seqRelease := false
	if seq != nil && (seq.owner != captureID || (mode != routing.TouchPartial && seq.owner != "")) {
		if seq.holdTimer != nil {
			seq.holdTimer.Stop()
		}
		cancelTo = seq.owner
		// A framework-owned hold that already summoned must end its turn: the
		// matching release now belongs to the new owner and will never arrive.
		seqRelease = seq.summon && seq.owner == "" && seq.holdFired && seq.holdToTalk && !seq.dismissed
		m.seq = nil
	}
	var actTimer *time.Timer
	var volStop chan struct{}
	actFired, actConsumed := false, false
	gestureRelease := false
	if mode != routing.TouchNone {
		// The framework no longer owns semantic gestures: partial capture
		// delivers them to the app and full capture disables them. A framework
		// hold that already summoned has lost its release event, so end it.
		gestureRelease = m.gestureHeld && !m.gestureConsumed
		m.gestureHeld = false
		m.gestureConsumed = false
	}
	if mode != routing.TouchNone {
		// Partial capture owns circle/play-pause and centre, so an armed talk
		// or framework gesture press must not keep running behind the app.
		actTimer = m.actTimer
		actFired = m.actFired
		actConsumed = m.actConsumed
		m.actTimer = nil
		m.actFired = false
		m.actConsumed = false
		m.actFramework = false
	}
	if mode == routing.TouchFull {
		volStop = m.volStop
		m.volStop = nil
		m.volKey = ""
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
	if actTimer != nil {
		actTimer.Stop()
	}
	if volStop != nil {
		close(volStop)
	}
	if seqRelease && m.h.Release != nil {
		m.logs.Infof("input", "", "routing change ended a held touch turn (hold-to-talk)")
		m.h.Release()
	}
	if gestureRelease && m.h.HoldToTalk != nil && m.h.HoldToTalk() && m.h.Release != nil {
		m.logs.Infof("input", "", "routing change ended a held gesture turn (hold-to-talk)")
		m.h.Release()
	}
	if actTimer != nil {
		if !actFired {
			m.logs.Infof("input", "", "capture took the talk key: pending activation cancelled")
		} else if !actConsumed && m.h.HoldToTalk != nil && m.h.HoldToTalk() && m.h.Release != nil {
			m.logs.Infof("input", "", "capture took the talk key: turn released (hold-to-talk)")
			m.h.Release()
		}
	}
}

func (m *Manager) Close() {
	m.cancel()
	m.cancelPanelSequence("manager closed")
	m.mu.Lock()
	actTimer := m.actTimer
	m.actTimer = nil
	m.actFired = false
	m.actConsumed = false
	m.actFramework = false
	volStop := m.volStop
	m.volStop = nil
	m.volKey = ""
	seq := m.seq
	m.seq = nil
	for _, subs := range m.subs {
		for id, ch := range subs {
			close(ch)
			delete(subs, id)
		}
	}
	m.mu.Unlock()
	if actTimer != nil {
		actTimer.Stop()
	}
	if seq != nil && seq.holdTimer != nil {
		seq.holdTimer.Stop()
	}
	if volStop != nil {
		close(volStop)
	}
}
