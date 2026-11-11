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
	if got := mic.Load(); got != 1 {
		t.Fatalf("mic toggled %d times for one tap, want 1", got)
	}
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
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
	time.Sleep(2*volumeRepeatStart + 80*time.Millisecond)
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
