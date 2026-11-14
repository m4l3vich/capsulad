package input

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/routing"
)

func TestGestureCancelledWhenOwnershipChanges(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	state := func() (string, string, routing.TouchMode) {
		return "cap-1", "app.game", routing.TouchMode(mode.Load())
	}
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{
		PartialGestures: true, FullRaw: true, Coordinates: true,
	}, state, Handlers{})
	defer m.Close()
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "primary", Pressed: true}})
	first := nextEvent(t, sub)
	if first.Gesture == nil || first.Gesture.ID != "primary" || first.Gesture.Phase != "start" {
		t.Fatalf("unexpected first event: %+v", first)
	}
	// Ownership changes mid-contact: the old owner receives a terminal cancel
	// event and the unmatched release is not delivered.
	mode.Store(int32(routing.TouchNone))
	m.OnRoutingChanged()
	next := nextEvent(t, sub)
	if next.Gesture == nil || next.Gesture.ID != "cancelled" {
		t.Fatalf("expected cancelled gesture, got %+v", next)
	}
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "primary", Pressed: false}})
	select {
	case ev := <-sub.C:
		t.Fatalf("matched release leaked after cancellation: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSystemControlsInPartialAndFullCapture(t *testing.T) {
	clock := ids.NewClock()
	logs, _ := logbus.New(clock, 100, "")
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	state := func() (string, string, routing.TouchMode) {
		return "cap-1", "app.game", routing.TouchMode(mode.Load())
	}
	var volumes atomic.Int32
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{PartialGestures: true, FullRaw: true, Coordinates: true}, state, Handlers{
		VolumeUp: func() bool { volumes.Add(1); return true },
	})
	defer m.Close()
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if volumes.Load() != 1 {
		t.Fatal("partial capture did not route volume to the framework")
	}
	select {
	case ev := <-sub.C:
		t.Fatalf("system volume event leaked to the partial capture: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	mode.Store(int32(routing.TouchFull))
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if volumes.Load() != 1 {
		t.Fatal("full capture still interpreted the system volume control")
	}
	select {
	case ev := <-sub.C:
		if ev.Key == nil || ev.Key.Key != "volume_up" {
			t.Fatalf("unexpected full-capture event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("full capture did not receive the panel key")
	}
}

func nextEvent(t *testing.T, sub *Subscription) backend.InputEvent {
	t.Helper()
	select {
	case ev := <-sub.C:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for input event")
		return backend.InputEvent{}
	}
}

func TestDiscreteKeysActOnPressOnly(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var mic, up atomic.Int32
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{
			MicToggle:  func() { mic.Add(1) },
			VolumeUp:   func() bool { up.Add(1); return true },
			VolumeDown: func() bool { return true },
		})
	defer m.Close()

	// A tap is press + release; only the press may act, otherwise the mute
	// toggles straight back and volume jumps two steps.
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "mic_toggle", Pressed: true}})
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "mic_toggle", Pressed: false}})
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if got := mic.Load(); got != 1 {
		t.Fatalf("mic toggled %d times for one tap, want 1", got)
	}
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if got := up.Load(); got != 1 {
		t.Fatalf("volume stepped %d times for one tap, want 1", got)
	}
}

func TestTalkKeyRequiresHold(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var origin atomic.Value
	var releases atomic.Int32
	var talkMode atomic.Bool
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{
			Activation: func(o string) { origin.Store(o) },
			Release:    func() { releases.Add(1) },
			HoldToTalk: func() bool { return talkMode.Load() },
		})
	defer m.Close()

	// A tap shorter than the hold threshold must do nothing at all.
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
	time.Sleep(keyActivationHold / 3)
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: false}})
	time.Sleep(keyActivationHold + 100*time.Millisecond)
	if got, _ := origin.Load().(string); got != "" {
		t.Fatalf("short tap activated the assistant (origin %q)", got)
	}
	if releases.Load() != 0 {
		t.Fatal("short tap ended a conversation")
	}

	// Hold to summon (the default, like stock): releasing leaves the
	// conversation running.
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
	time.Sleep(keyActivationHold + 150*time.Millisecond)
	if got, _ := origin.Load().(string); got != "play_pause_key" {
		t.Fatalf("hold activation origin = %q", got)
	}
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: false}})
	if releases.Load() != 0 {
		t.Fatalf("summon mode ended the conversation on release (%d)", releases.Load())
	}

	// Hold to talk: release ends the turn.
	talkMode.Store(true)
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
	time.Sleep(keyActivationHold + 150*time.Millisecond)
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: false}})
	if releases.Load() != 1 {
		t.Fatalf("talk mode release delivered %d times, want 1", releases.Load())
	}
}

func TestHeldVolumeKeyRepeats(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var steps atomic.Int32
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{VolumeUp: func() bool { steps.Add(1); return true }})
	defer m.Close()
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	time.Sleep(2*volumeRepeatStart + 500*time.Millisecond)
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	got := steps.Load()
	if got < 3 {
		t.Fatalf("held key produced %d steps, want at least 3 (initial + repeats)", got)
	}
	time.Sleep(volumeRepeatStart + 60*time.Millisecond)
	if after := steps.Load(); after != got {
		t.Fatalf("repeat continued after release: %d → %d", got, after)
	}
}

// A ramp must stop as soon as the handler reports the bound was reached.
func TestVolumeRampStopsAtBound(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{VolumeUp: func() bool {
			// First call (the press) succeeds, then the volume is at its bound.
			return calls.Add(1) == 1
		}})
	defer m.Close()
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	time.Sleep(3*volumeRepeatStart + 80*time.Millisecond)
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	// One call for the press, one to learn the value is clamped; no more.
	if got := calls.Load(); got != 2 {
		t.Fatalf("ramp called %d times after reporting a bound, want 2", got)
	}
}

// controlCounters records how the framework interpreted physical controls.
type controlCounters struct {
	dismissed        atomic.Int32
	dismissedTurn    atomic.Int32
	activations      atomic.Int32
	releases         atomic.Int32
	volumes          atomic.Int32
	mics             atomic.Int32
	taps             atomic.Int32
	holds            atomic.Int32
	doubleTaps       atomic.Int32
	primaryActive    atomic.Bool
	dismissAssistant atomic.Bool
	mediaToggles     atomic.Int32
	mediaPrevious    atomic.Int32
	mediaNext        atomic.Int32
}

// newControlManager builds a framework-owned panel whose DismissAlert result
// follows active, so alert dismissal can be toggled mid-test.
func newControlManager(t *testing.T, active *atomic.Bool, talk *atomic.Bool) (*Manager, *controlCounters) {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	c := &controlCounters{}
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{
			VolumeUp:   func() bool { c.volumes.Add(1); return true },
			VolumeDown: func() bool { c.volumes.Add(1); return true },
			MicToggle:  func() { c.mics.Add(1) },
			Activation: func(string) { c.activations.Add(1) },
			Release:    func() { c.releases.Add(1) },
			HoldToTalk: func() bool { return talk.Load() },
			DismissAlert: func() bool {
				c.dismissed.Add(1)
				return active.Load()
			},
			DismissAssistant: func() bool {
				c.dismissedTurn.Add(1)
				return c.dismissAssistant.Load()
			},
			PrimaryActive: func() bool { return c.primaryActive.Load() },
			PrimaryGesture: func(kind string) bool {
				switch kind {
				case "tap":
					c.taps.Add(1)
				case "hold":
					c.holds.Add(1)
				case "double_tap":
					c.doubleTaps.Add(1)
				default:
					return false
				}
				return true
			},
			MediaToggle:   func() { c.mediaToggles.Add(1) },
			MediaPrevious: func() { c.mediaPrevious.Add(1) },
			MediaNext:     func() { c.mediaNext.Add(1) },
		})
	t.Cleanup(m.Close)
	return m, c
}

func pressKey(m *Manager, key string, pressed bool) {
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: key, Pressed: pressed}})
}

func sendGesture(m *Manager, id, phase string) {
	m.Handle(backend.InputEvent{Gesture: &backend.GestureEvent{ID: id, Phase: phase}})
}

// A talk key pressed while an alert sounds dismisses it and is consumed: the
// same physical press must not summon the assistant, and its release must not
// end a turn that was never started.
func TestTalkKeyDismissesAlertWithoutActivating(t *testing.T) {
	var active, talk atomic.Bool
	active.Store(true)
	talk.Store(true)
	m, c := newControlManager(t, &active, &talk)

	pressKey(m, "play_pause", true)
	pressKey(m, "play_pause", true) // backend duplicate press during the same hold
	time.Sleep(keyActivationHold + 100*time.Millisecond)
	pressKey(m, "play_pause", false)
	time.Sleep(50 * time.Millisecond)

	if got := c.dismissed.Load(); got != 1 {
		t.Fatalf("dismissed %d times for one press, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("alert dismissing press activated the assistant %d times", got)
	}
	if got := c.releases.Load(); got != 0 {
		t.Fatalf("alert dismissing release ended a turn %d times", got)
	}

	// Once the alert is gone the same key must arm normally again.
	active.Store(false)
	pressKey(m, "play_pause", true)
	time.Sleep(keyActivationHold + 150*time.Millisecond)
	pressKey(m, "play_pause", false)
	if got := c.activations.Load(); got != 1 {
		t.Fatalf("talk key activated %d times after the alert, want 1", got)
	}
	if got := c.releases.Load(); got != 1 {
		t.Fatalf("hold-to-talk release delivered %d times, want 1", got)
	}
}

// The primary panel key dismisses an alert on press and consumes the hold and
// release that belong to that press.
func TestPrimaryPressDismissesAlertAndSwallowsHold(t *testing.T) {
	var active, talk atomic.Bool
	active.Store(true)
	talk.Store(true)
	m, c := newControlManager(t, &active, &talk)

	pressKey(m, "primary", true)
	time.Sleep(holdThreshold + 100*time.Millisecond) // long enough to be a hold
	pressKey(m, "primary", false)

	if got := c.dismissed.Load(); got != 1 {
		t.Fatalf("dismissed %d times, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("alert dismissing primary press activated the assistant %d times", got)
	}
	if got := c.releases.Load(); got != 0 {
		t.Fatalf("alert dismissing primary hold released a turn %d times", got)
	}

	// The next press has no alert to consume and behaves normally.
	active.Store(false)
	pressKey(m, "primary", true)
	time.Sleep(holdThreshold + 100*time.Millisecond)
	pressKey(m, "primary", false)
	if got := c.activations.Load(); got != 1 {
		t.Fatalf("primary activated %d times after the alert, want 1", got)
	}
	if got := c.releases.Load(); got != 1 {
		t.Fatalf("primary hold released %d times, want 1", got)
	}
}

// Volume and microphone keys are safety controls: they keep working during an
// alert and never dismiss it.
func TestVolumeAndMicStayActiveDuringAlert(t *testing.T) {
	var active, talk atomic.Bool
	active.Store(true)
	m, c := newControlManager(t, &active, &talk)

	pressKey(m, "volume_up", true)
	pressKey(m, "volume_up", false)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	pressKey(m, "mic_toggle", true)
	pressKey(m, "mic_toggle", false)
	time.Sleep(panelInterContact + 80*time.Millisecond)

	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("volume stepped %d times, want 1", got)
	}
	if got := c.mics.Load(); got != 1 {
		t.Fatalf("microphone toggled %d times, want 1", got)
	}
	if got := c.dismissed.Load(); got != 0 {
		t.Fatalf("volume/microphone keys dismissed the alert %d times", got)
	}
}

// Backend semantic tap and hold gestures follow the same rule: a sounding alert
// consumes them instead of summoning the assistant.
func TestGestureTapAndHoldDismissAlert(t *testing.T) {
	var active, talk atomic.Bool
	active.Store(true)
	talk.Store(true)
	m, c := newControlManager(t, &active, &talk)

	sendGesture(m, "tap", "end")
	sendGesture(m, "hold", "start")
	sendGesture(m, "hold", "end")

	if got := c.dismissed.Load(); got != 2 {
		t.Fatalf("dismissed %d times, want 2 (tap and hold)", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("alert dismissing gestures activated the assistant %d times", got)
	}
	if got := c.releases.Load(); got != 0 {
		t.Fatalf("consumed hold released a turn %d times", got)
	}
}

// A hold end with no matching start must not release a turn it never began,
// and a repeated hold start must not summon the assistant twice.
func TestGestureHoldNeedsMatchingStart(t *testing.T) {
	var active, talk atomic.Bool
	talk.Store(true)
	m, c := newControlManager(t, &active, &talk)

	sendGesture(m, "hold", "end")
	if got := c.releases.Load(); got != 0 {
		t.Fatalf("stray hold end released a turn %d times", got)
	}

	sendGesture(m, "hold", "start")
	sendGesture(m, "hold", "start")
	if got := c.activations.Load(); got != 1 {
		t.Fatalf("repeated hold start activated %d times, want 1", got)
	}
	sendGesture(m, "hold", "end")
	if got := c.releases.Load(); got != 1 {
		t.Fatalf("hold release delivered %d times, want 1", got)
	}
	sendGesture(m, "hold", "end")
	if got := c.releases.Load(); got != 1 {
		t.Fatalf("duplicate hold end released again (%d)", got)
	}
}

// Full capture owns the panel outright: controls are forwarded and no alert is
// dismissed behind the capturing application's back.
func TestFullCaptureForwardsControlsWithoutDismissal(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var dismissed atomic.Int32
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{FullRaw: true},
		func() (string, string, routing.TouchMode) { return "cap-1", "app.game", routing.TouchFull },
		Handlers{DismissAlert: func() bool { dismissed.Add(1); return true }})
	defer m.Close()
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	pressKey(m, "primary", true)
	if ev := nextEvent(t, sub); ev.Key == nil || ev.Key.Key != "primary" || !ev.Key.Pressed {
		t.Fatalf("full capture did not receive the primary press: %+v", ev)
	}
	pressKey(m, "play_pause", true)
	if ev := nextEvent(t, sub); ev.Key == nil || ev.Key.Key != "play_pause" {
		t.Fatalf("full capture did not receive the talk key: %+v", ev)
	}
	if got := dismissed.Load(); got != 0 {
		t.Fatalf("full capture dismissed an alert %d times", got)
	}
}

// When a crossing reports both opposite volume edges without the centre, the
// ordered pair is the left/right swipe, not a volume chord. The swipe must
// produce previous/next and no volume ramp, and neither contact's release may
// leak a step. A crossing that reports only one contact remains a volume press
// and is not inferred as a swipe.
func TestOppositeVolumeContactsFormASwipe(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var up, down, previous, next atomic.Int32
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{
			VolumeUp:      func() bool { up.Add(1); return true },
			VolumeDown:    func() bool { down.Add(1); return true },
			MediaPrevious: func() { previous.Add(1) },
			MediaNext:     func() { next.Add(1) },
		})
	defer m.Close()

	// East then west is a leftward crossing. The east contact is still down
	// when the west edge is pressed, as an overlapping crossing can report.
	panelKeyAt(m, "volume_up", true, 1_000_000)
	panelKeyAt(m, "volume_down", true, 1_090_000)
	panelKeyAt(m, "volume_up", false, 1_150_000)
	panelKeyAt(m, "volume_down", false, 1_160_000)
	if got := previous.Load(); got != 1 {
		t.Fatalf("leftward crossing delivered %d previous, want 1", got)
	}
	if up.Load() != 0 || down.Load() != 0 {
		t.Fatalf("swipe leaked volume steps up=%d down=%d", up.Load(), down.Load())
	}

	// West then east is a rightward crossing.
	panelKeyAt(m, "volume_down", true, 2_000_000)
	panelKeyAt(m, "volume_up", true, 2_090_000)
	panelKeyAt(m, "volume_down", false, 2_150_000)
	panelKeyAt(m, "volume_up", false, 2_160_000)
	if got := next.Load(); got != 1 {
		t.Fatalf("rightward crossing delivered %d next, want 1", got)
	}
	if up.Load() != 0 || down.Load() != 0 {
		t.Fatalf("swipe leaked volume steps up=%d down=%d", up.Load(), down.Load())
	}
}

// A duplicate press of the same volume key must not apply a second step.
func TestDuplicateVolumePressDoesNotStepTwice(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var steps atomic.Int32
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{VolumeUp: func() bool { steps.Add(1); return true }})
	defer m.Close()

	pressKey(m, "volume_up", true)
	pressKey(m, "volume_up", true)
	pressKey(m, "volume_up", false)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if got := steps.Load(); got != 1 {
		t.Fatalf("duplicate press stepped %d times, want 1", got)
	}
	time.Sleep(volumeRepeatStart + 60*time.Millisecond)
	if got := steps.Load(); got != 1 {
		t.Fatalf("ramp ran after an immediate release (%d steps)", got)
	}
}

// A capture takeover in the middle of a framework-owned turn must end it: the
// release now belongs to the new owner and will never reach the framework.
func TestCaptureTakeoverEndsHeldTurn(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	var mode atomic.Int32
	mode.Store(int32(routing.TouchNone))
	state := func() (string, string, routing.TouchMode) {
		if mode.Load() == int32(routing.TouchNone) {
			return "", "", routing.TouchNone
		}
		return "cap-1", "app.game", routing.TouchMode(mode.Load())
	}
	var activations, releases atomic.Int32
	m := NewManager(context.Background(), clock, logs,
		backend.TouchCaps{PartialGestures: true, FullRaw: true}, state,
		Handlers{
			Activation: func(string) { activations.Add(1) },
			Release:    func() { releases.Add(1) },
			HoldToTalk: func() bool { return true },
		})
	defer m.Close()

	// Gesture hold in progress when a partial capture takes the panel.
	sendGesture(m, "hold", "start")
	if activations.Load() != 1 {
		t.Fatalf("hold start activated %d times, want 1", activations.Load())
	}
	mode.Store(int32(routing.TouchPartial))
	m.OnRoutingChanged()
	if releases.Load() != 1 {
		t.Fatalf("takeover released the held gesture turn %d times, want 1", releases.Load())
	}
	sendGesture(m, "hold", "end") // now delivered to the app, not the framework
	if releases.Load() != 1 {
		t.Fatalf("gesture hold end released a second time (%d)", releases.Load())
	}

	// Talk key hold in progress when a full capture takes the keys.
	mode.Store(int32(routing.TouchNone))
	pressKey(m, "play_pause", true)
	time.Sleep(keyActivationHold + 100*time.Millisecond)
	if activations.Load() != 2 {
		t.Fatalf("talk hold activated %d times, want 2", activations.Load())
	}
	mode.Store(int32(routing.TouchFull))
	m.OnRoutingChanged()
	if releases.Load() != 2 {
		t.Fatalf("takeover released the held talk turn %d times, want 2", releases.Load())
	}
	pressKey(m, "play_pause", false) // forwarded to the capture; must not release again
	if releases.Load() != 2 {
		t.Fatalf("forwarded release ended the turn again (%d)", releases.Load())
	}
}

// A running stopwatch owns the talk key: a short tap pauses/resumes it and
// never summons the assistant, and the release of that press is not a turn
// release.
func TestTalkKeyTapDrivesFrameworkGesture(t *testing.T) {
	var active, talk atomic.Bool
	m, c := newControlManager(t, &active, &talk)
	c.primaryActive.Store(true)

	pressKey(m, "play_pause", true)
	time.Sleep(60 * time.Millisecond)
	pressKey(m, "play_pause", false)
	time.Sleep(panelInterContact + 120*time.Millisecond)

	if got := c.taps.Load(); got != 1 {
		t.Fatalf("framework taps = %d, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("stopwatch tap summoned the assistant %d times", got)
	}
	if got := c.releases.Load(); got != 0 {
		t.Fatalf("stopwatch tap released a turn %d times", got)
	}
}

// Holding the talk key while a framework interaction owns it delivers the hold
// at the threshold and does not summon the assistant, even in hold-to-talk
// mode. The matching release produces no tap.
func TestTalkKeyHoldDrivesFrameworkGestureWithoutSummoning(t *testing.T) {
	var active, talk atomic.Bool
	talk.Store(true)
	m, c := newControlManager(t, &active, &talk)
	c.primaryActive.Store(true)

	pressKey(m, "play_pause", true)
	time.Sleep(keyActivationHold + 120*time.Millisecond)
	pressKey(m, "play_pause", false)
	time.Sleep(50 * time.Millisecond)

	if got := c.holds.Load(); got != 1 {
		t.Fatalf("framework holds = %d, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("framework hold summoned the assistant %d times", got)
	}
	if got := c.releases.Load(); got != 0 {
		t.Fatalf("framework hold released a turn %d times", got)
	}
}

// Two quick taps while the framework owns the key are recognized as the
// configurable double-tap reset gesture.
func TestTalkKeyDoubleTapDrivesFrameworkGesture(t *testing.T) {
	var active, talk atomic.Bool
	m, c := newControlManager(t, &active, &talk)
	c.primaryActive.Store(true)

	pressKey(m, "play_pause", true)
	time.Sleep(40 * time.Millisecond)
	pressKey(m, "play_pause", false)
	time.Sleep(60 * time.Millisecond)
	pressKey(m, "play_pause", true)
	time.Sleep(40 * time.Millisecond)
	pressKey(m, "play_pause", false)
	time.Sleep(2*panelInterContact + 150*time.Millisecond)

	if got := c.doubleTaps.Load(); got != 1 {
		t.Fatalf("framework double taps = %d, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("double tap summoned the assistant %d times", got)
	}
}

// An active assistant turn is dismissed by the talk-key press itself; the
// press and its release are consumed so no new turn starts and no release is
// delivered to the turn that already ended.
func TestTalkKeyDismissesAssistantTurn(t *testing.T) {
	var active, talk atomic.Bool
	talk.Store(true)
	m, c := newControlManager(t, &active, &talk)
	c.dismissAssistant.Store(true)

	pressKey(m, "play_pause", true)
	time.Sleep(keyActivationHold + 120*time.Millisecond)
	pressKey(m, "play_pause", false)
	time.Sleep(50 * time.Millisecond)

	if got := c.dismissedTurn.Load(); got != 1 {
		t.Fatalf("assistant dismissals = %d, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("dismissing press activated the assistant %d times", got)
	}
	if got := c.releases.Load(); got != 0 {
		t.Fatalf("dismissing release ended a turn %d times", got)
	}
}

// The panel primary key follows the same arbitration: a framework tap is
// delivered and does not activate the assistant.
func TestPrimaryTapDrivesFrameworkGesture(t *testing.T) {
	var active, talk atomic.Bool
	m, c := newControlManager(t, &active, &talk)
	c.primaryActive.Store(true)

	pressKey(m, "primary", true)
	time.Sleep(50 * time.Millisecond)
	pressKey(m, "primary", false)
	time.Sleep(50 * time.Millisecond)

	if got := c.taps.Load(); got != 1 {
		t.Fatalf("framework panel taps = %d, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("panel tap summoned the assistant %d times", got)
	}
}
