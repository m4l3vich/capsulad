package input

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/routing"
)

// newPanelModeManager returns a panel manager whose framework owner follows
// mode; captureID is the partial/full owner when the mode has one.
func newPanelModeManager(t *testing.T, mode *atomic.Int32, captureID string) (*Manager, *controlCounters) {
	m, c, _ := newPanelLogManager(t, mode, captureID)
	return m, c
}

// newPanelLogManager also returns the log bus so a test can assert the field
// trace a user's swipe leaves behind.
func newPanelLogManager(t *testing.T, mode *atomic.Int32, captureID string) (*Manager, *controlCounters, *logbus.Bus) {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	c := &controlCounters{}
	var talk atomic.Bool
	state := func() (string, string, routing.TouchMode) {
		m := routing.TouchMode(mode.Load())
		if m == routing.TouchNone {
			return "", "", m
		}
		return captureID, "app.game", m
	}
	m := NewManager(context.Background(), clock, logs,
		backend.TouchCaps{PartialGestures: true, FullRaw: true}, state,
		Handlers{
			VolumeUp:      func() bool { c.volumes.Add(1); return true },
			VolumeDown:    func() bool { c.volumes.Add(1); return true },
			MicToggle:     func() { c.mics.Add(1) },
			Activation:    func(string) { c.activations.Add(1) },
			Release:       func() { c.releases.Add(1) },
			HoldToTalk:    func() bool { return talk.Load() },
			DismissAlert:  func() bool { c.dismissed.Add(1); return false },
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
	return m, c, logs
}

func panelKeyAt(m *Manager, key string, pressed bool, ts uint64) {
	m.Handle(backend.InputEvent{MonotonicNS: ts, Key: &backend.KeyEvent{Key: key, Pressed: pressed}})
}

func drainInput(t *testing.T, sub *Subscription) []backend.InputEvent {
	t.Helper()
	var out []backend.InputEvent
	for {
		select {
		case ev := <-sub.C:
			out = append(out, ev)
		case <-time.After(panelInterContact + 170*time.Millisecond):
			return out
		}
	}
}

func tapPanelKey(m *Manager, key string, ts uint64) {
	panelKeyAt(m, key, true, ts)
	panelKeyAt(m, key, false, ts+10_000)
}

// tapCentreThenEdge is the measured centre evidence: the centre reports the
// start of a centre-anchored contact and the volume edge reports it 90 ms later,
// either while the centre is still down or just after it was released.
func tapCentreThenEdge(m *Manager, edge string, base uint64) {
	panelKeyAt(m, "center", true, base)
	panelKeyAt(m, "center", false, base+40_000)
	panelKeyAt(m, edge, true, base+90_000)
	panelKeyAt(m, edge, false, base+160_000)
}

// A volume step resolves on its own press with no deferral: the handler has
// already run when the release arrives, and nothing changes afterwards.
func TestVolumeStepResolvesOnPressWithoutDeferral(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")

	panelKeyAt(m, "volume_up", true, 1_000_000)
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("volume step after press = %d, want 1 resolved synchronously", got)
	}
	panelKeyAt(m, "volume_up", false, 1_050_000)
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("release changed the step count to %d", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("lone volume key became a swipe: next=%d prev=%d", c.mediaNext.Load(), c.mediaPrevious.Load())
	}
	time.Sleep(panelInterContact + 120*time.Millisecond)
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("volume stepped again with no further input: %d", got)
	}
}

// Two bare volume taps 1.2 s apart are two steps and no gesture; opposite
// edges farther apart than the full-panel window are two deliberate steps too.
func TestBareVolumeTapsStaySteps(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_up", true, base)
	panelKeyAt(m, "volume_up", false, base+70*ms)
	panelKeyAt(m, "volume_up", true, base+1_200*ms)
	panelKeyAt(m, "volume_up", false, base+1_270*ms)
	if got := c.volumes.Load(); got != 2 {
		t.Fatalf("two taps 1.2 s apart stepped %d times, want 2", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("two taps became a skip: next=%d prev=%d", c.mediaNext.Load(), c.mediaPrevious.Load())
	}

	// Opposite edges farther apart than the full-panel window are two
	// deliberate steps, never a skip.
	panelKeyAt(m, "volume_down", true, base+2_000*ms)
	panelKeyAt(m, "volume_down", false, base+2_070*ms)
	panelKeyAt(m, "volume_up", true, base+2_450*ms)
	panelKeyAt(m, "volume_up", false, base+2_520*ms)
	if got := c.volumes.Load(); got != 4 {
		t.Fatalf("opposite bare edges stepped %d times, want 4", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("opposite bare edges became a skip: next=%d prev=%d", c.mediaNext.Load(), c.mediaPrevious.Load())
	}
}

// The centre gate: a centre contact followed by an edge inside the
// inter-contact bound is one swipe, and the edge's own volume step is
// suppressed. Both the overlapping report and the one where the centre is
// released before the edge is pressed count.
func TestCentreGatedSwipeSuppressesTheEdgeStep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edge  string
		next  int32
		prev  int32
		press func(m *Manager, edge string, base uint64)
	}{
		{"right/overlap", "volume_up", 1, 0, func(m *Manager, edge string, base uint64) {
			panelKeyAt(m, "center", true, base)
			panelKeyAt(m, edge, true, base+90_000)
			panelKeyAt(m, "center", false, base+150_000)
			panelKeyAt(m, edge, false, base+160_000)
		}},
		{"right/released", "volume_up", 1, 0, tapCentreThenEdge},
		{"left/overlap", "volume_down", 0, 1, func(m *Manager, edge string, base uint64) {
			panelKeyAt(m, "center", true, base)
			panelKeyAt(m, edge, true, base+90_000)
			panelKeyAt(m, "center", false, base+150_000)
			panelKeyAt(m, edge, false, base+160_000)
		}},
		{"left/released", "volume_down", 0, 1, tapCentreThenEdge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mode atomic.Int32
			m, c := newPanelModeManager(t, &mode, "")
			tc.press(m, tc.edge, 1_000_000_000)
			if got := c.mediaNext.Load(); got != tc.next {
				t.Fatalf("mediaNext = %d, want %d", got, tc.next)
			}
			if got := c.mediaPrevious.Load(); got != tc.prev {
				t.Fatalf("mediaPrevious = %d, want %d", got, tc.prev)
			}
			if got := c.volumes.Load(); got != 0 {
				t.Fatalf("gated swipe leaked %d volume steps", got)
			}
			if c.mediaToggles.Load() != 0 || c.mics.Load() != 0 || c.activations.Load() != 0 {
				t.Fatalf("gated swipe leaked another control: %+v", c)
			}
		})
	}
}

// A centre contact followed by an edge outside the released window is a step
// and no gesture: the centre already resolved as its own neutral key.
func TestCentreGateBoundary(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "center", true, base)
	panelKeyAt(m, "center", false, base+40*ms)
	panelKeyAt(m, "volume_up", true, base+900*ms)
	panelKeyAt(m, "volume_up", false, base+970*ms)
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("late centre/edge pair stepped %d times, want 1", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("late centre/edge pair became a swipe: next=%d prev=%d", c.mediaNext.Load(), c.mediaPrevious.Load())
	}
}

// A held centre keeps gating a volume edge beyond the released-gate press
// window, provided the edge begins while the centre is still down and within
// the overall gesture budget. The centre key's release remains forwarded.
func TestHeldCentreGatesDelayedVolumeEdges(t *testing.T) {
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)
	for _, tc := range []struct {
		name     string
		edge     string
		gesture  string
		next     int32
		previous int32
	}{
		{"volume up", "volume_up", "swipe_right", 1, 0},
		{"volume down", "volume_down", "swipe_left", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mode atomic.Int32
			mode.Store(int32(routing.TouchPartial))
			m, c := newPanelModeManager(t, &mode, "cap-1")
			sub, err := m.Subscribe("cap-1")
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()

			panelKeyAt(m, "center", true, base)
			panelKeyAt(m, tc.edge, true, base+600*ms)
			panelKeyAt(m, "center", false, base+650*ms)
			panelKeyAt(m, tc.edge, false, base+670*ms)

			var gestures, centrePresses, centreReleases int
			for _, ev := range drainInput(t, sub) {
				if ev.Gesture != nil && ev.Gesture.Phase == "end" {
					if ev.Gesture.ID != tc.gesture {
						t.Fatalf("gesture = %q, want %q", ev.Gesture.ID, tc.gesture)
					}
					gestures++
				}
				if ev.Key != nil {
					if ev.Key.Key != "center" {
						t.Fatalf("gated edge leaked to capture: %+v", ev)
					}
					if ev.Key.Pressed {
						centrePresses++
					} else {
						centreReleases++
					}
				}
			}
			if gestures != 1 {
				t.Fatalf("gesture count = %d, want exactly 1", gestures)
			}
			if centrePresses != 1 || centreReleases != 1 {
				t.Fatalf("capture received centre press/release = %d/%d, want 1/1", centrePresses, centreReleases)
			}
			if c.volumes.Load() != 0 {
				t.Fatalf("held-centre swipe leaked %d volume steps", c.volumes.Load())
			}
		})
	}
}

func TestHeldCentreCannotExceedGestureBudget(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "center", true, base)
	panelKeyAt(m, "volume_up", true, base+1600*ms)
	panelKeyAt(m, "volume_up", false, base+1670*ms)
	panelKeyAt(m, "center", false, base+1700*ms)
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("edge after overlong centre hold stepped %d times, want 1", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("edge after overlong centre hold became a gesture: next=%d prev=%d",
			c.mediaNext.Load(), c.mediaPrevious.Load())
	}
}

func TestHeldCentreAcceptsBothVolumeEdges(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "center", true, base)
	panelKeyAt(m, "volume_up", true, base+600*ms)
	panelKeyAt(m, "volume_up", false, base+650*ms)
	panelKeyAt(m, "volume_down", true, base+800*ms)
	panelKeyAt(m, "volume_down", false, base+850*ms)
	panelKeyAt(m, "center", false, base+900*ms)
	if c.mediaNext.Load() != 1 || c.mediaPrevious.Load() != 1 {
		t.Fatalf("held-centre edge gestures next/previous = %d/%d, want 1/1",
			c.mediaNext.Load(), c.mediaPrevious.Load())
	}
	if c.volumes.Load() != 0 {
		t.Fatalf("held-centre edge taps leaked %d volume steps", c.volumes.Load())
	}
}

// The documented three-contact chain stays one gesture. It can only suppress
// the contact that completes it, so the opposite edge keeps the single action
// it resolved on its own press.
func TestCentreChainStaysOneGesture(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)
	for i, key := range []string{"volume_down", "center", "volume_up"} {
		panelKeyAt(m, key, true, base+uint64(i)*100*ms)
	}
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("chain leading edge stepped volume %d times, want 1", got)
	}
	for i, key := range []string{"volume_down", "center", "volume_up"} {
		panelKeyAt(m, key, false, base+150*ms+uint64(i)*100*ms)
	}
	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("W/C/E chain delivered %d next, want 1", got)
	}
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("chain direction edge also stepped volume (%d steps)", got)
	}
	if c.mediaPrevious.Load() != 0 || c.mics.Load() != 0 || c.activations.Load() != 0 {
		t.Fatalf("chain leaked another control: %+v", c)
	}
}

// The vertical pair has no gesture form: it stays two deliberate controls, and
// a centre between the vertical edges is only the documented chain.
func TestVerticalPairStaysDeliberate(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "play_pause", true, base)
	panelKeyAt(m, "mic_toggle", true, base+90*ms)
	panelKeyAt(m, "mic_toggle", false, base+130*ms)
	panelKeyAt(m, "play_pause", false, base+140*ms)
	if got := c.mics.Load(); got != 1 {
		t.Fatalf("vertical pair muted %d times, want 1", got)
	}
	if got := c.mediaToggles.Load(); got != 1 {
		t.Fatalf("vertical pair toggled media %d times, want 1", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 || c.activations.Load() != 0 {
		t.Fatalf("vertical pair produced a crossing or summon: %+v", c)
	}
}

// The centre is neutral on its own: a tap does nothing, a hold does not summon,
// and it never turns a neighbouring key into a gesture by itself.
func TestNeutralCentreNeverSummons(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	tapPanelKey(m, "center", 1_000_000)
	panelKeyAt(m, "center", true, 2_000_000)
	time.Sleep(holdThreshold + 120*time.Millisecond)
	panelKeyAt(m, "center", false, 2_100_000)
	if c.activations.Load() != 0 || c.mediaToggles.Load() != 0 {
		t.Fatalf("neutral centre acted: %+v", c)
	}
}

// A circle/play-pause short tap toggles current media once; a hold still
// summons the assistant and does not also toggle playback.
func TestCircleTapTogglesMediaAndHoldStillSummons(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")

	panelKeyAt(m, "play_pause", true, 0)
	time.Sleep(60 * time.Millisecond)
	panelKeyAt(m, "play_pause", false, 0)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if got := c.mediaToggles.Load(); got != 1 {
		t.Fatalf("short circle tap toggles = %d, want 1", got)
	}
	if got := c.activations.Load(); got != 0 {
		t.Fatalf("short circle tap summoned the assistant %d times", got)
	}

	panelKeyAt(m, "play_pause", true, 0)
	time.Sleep(keyActivationHold + 120*time.Millisecond)
	panelKeyAt(m, "play_pause", false, 0)
	time.Sleep(50 * time.Millisecond)
	if got := c.activations.Load(); got != 1 {
		t.Fatalf("circle hold activations = %d, want 1", got)
	}
	if got := c.mediaToggles.Load(); got != 1 {
		t.Fatalf("circle hold also toggled media (%d)", got)
	}
}

// A perimeter traversal without the centre is five individual keys: volume
// steps, mute toggles and transports act, and no gesture is produced.
func TestPerimeterTraversalWithoutCentreIsKeys(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)
	for i, key := range []string{"mic_toggle", "volume_up", "play_pause", "volume_down", "mic_toggle"} {
		panelKeyAt(m, key, true, base+uint64(i)*60*ms)
		panelKeyAt(m, key, false, base+uint64(i)*60*ms+30*ms)
	}
	if got := c.volumes.Load(); got != 2 {
		t.Fatalf("ring traversal stepped volume %d times, want 2", got)
	}
	if got := c.mics.Load(); got != 2 {
		t.Fatalf("ring traversal muted %d times, want 2", got)
	}
	if got := c.mediaToggles.Load(); got != 1 {
		t.Fatalf("ring traversal toggled media %d times, want 1", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("ring traversal produced a skip: next=%d prev=%d", c.mediaNext.Load(), c.mediaPrevious.Load())
	}
}

// A late or duplicate release never double-fires: the release of a contact the
// gesture consumed is swallowed once, and a repeated release resolves nothing.
func TestLateAndDuplicateReleaseNeverDoubleFire(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "center", true, base)
	panelKeyAt(m, "volume_up", true, base+90*ms)
	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("centre/right swipe delivered %d next, want 1", got)
	}
	// Duplicate press of the consumed edge and a doubled release both resolve
	// nothing further.
	panelKeyAt(m, "volume_up", true, base+120*ms)
	panelKeyAt(m, "volume_up", false, base+200*ms)
	panelKeyAt(m, "volume_up", false, base+240*ms)
	panelKeyAt(m, "center", false, base+300*ms)
	panelKeyAt(m, "center", false, base+340*ms)
	if got := c.volumes.Load(); got != 0 {
		t.Fatalf("consumed edge acted on a repeat: %d steps", got)
	}
	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("duplicate edges delivered %d next, want 1", got)
	}

	// A lone release with no press state resolves nothing at all.
	panelKeyAt(m, "volume_down", false, base+400*ms)
	if got := c.volumes.Load(); got != 0 {
		t.Fatalf("stray release stepped volume %d times", got)
	}
}

// Partial capture owns the circle and centre keys and receives a centre-gated
// swipe as one gesture. Volume and microphone stay framework controls, and the
// gated edge contributes no framework step.
func TestPartialSwipeDeliveredWithoutSystemEffects(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	panelKeyAt(m, "center", true, 1_000_000)
	panelKeyAt(m, "center", false, 1_040_000)
	panelKeyAt(m, "volume_up", true, 1_090_000)
	panelKeyAt(m, "volume_up", false, 1_160_000)
	evs := drainInput(t, sub)

	var gesture bool
	var centreKeys int
	for _, ev := range evs {
		if ev.Gesture != nil && ev.Gesture.ID == "swipe_right" && ev.Gesture.Phase == "end" {
			gesture = true
		}
		if ev.Key != nil {
			if ev.Key.Key != "center" {
				t.Fatalf("swipe leaked a non-centre key to the capture: %+v", ev)
			}
			centreKeys++
		}
	}
	if !gesture {
		t.Fatalf("partial capture did not receive swipe_right: %+v", evs)
	}
	if centreKeys != 2 {
		t.Fatalf("partial capture received %d centre key events, want press+release", centreKeys)
	}
	if c.volumes.Load() != 0 || c.mediaNext.Load() != 0 || c.activations.Load() != 0 {
		t.Fatalf("partial swipe leaked framework effects: %+v", c)
	}
}

// Full capture receives every key and the centre-gated gesture, with every
// system effect suppressed.
func TestFullCaptureForwardsKeysAndCentreGesture(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchFull))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	panelKeyAt(m, "center", true, 1_000_000)
	panelKeyAt(m, "volume_down", true, 1_090_000)
	panelKeyAt(m, "center", false, 1_150_000)
	panelKeyAt(m, "volume_down", false, 1_160_000)
	evs := drainInput(t, sub)

	var gestures []string
	var keys int
	for _, ev := range evs {
		if ev.Gesture != nil && ev.Gesture.Phase == "end" {
			gestures = append(gestures, ev.Gesture.ID)
		}
		if ev.Key != nil {
			keys++
		}
	}
	if len(gestures) != 1 || gestures[0] != "swipe_left" {
		t.Fatalf("full capture gestures = %v, want exactly swipe_left", gestures)
	}
	if keys != 4 {
		t.Fatalf("full capture received %d key events, want every press and release", keys)
	}
	if c.volumes.Load() != 0 || c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("full capture leaked a system effect: %+v", c)
	}

	// A bare vertical pair is two forwarded keys and no gesture.
	panelKeyAt(m, "play_pause", true, 2_000_000)
	panelKeyAt(m, "mic_toggle", true, 2_090_000)
	panelKeyAt(m, "mic_toggle", false, 2_130_000)
	panelKeyAt(m, "play_pause", false, 2_140_000)
	evs = drainInput(t, sub)
	for _, ev := range evs {
		if ev.Gesture != nil {
			t.Fatalf("bare vertical pair produced a capture gesture: %+v", ev)
		}
	}
	if c.mics.Load() != 0 || c.mediaToggles.Load() != 0 {
		t.Fatalf("full capture ran a framework control: %+v", c)
	}
}

// Partial capture still receives the centre keys of a centre-gated vertical
// chain and the gesture, while the microphone edge stays framework-owned.
func TestPartialChainKeepsOwnedKeys(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// S, C, N is swipe_up; the circle leading edge is app-owned and already
	// delivered, so only the mute edge is suppressed.
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)
	for i, key := range []string{"play_pause", "center", "mic_toggle"} {
		panelKeyAt(m, key, true, base+uint64(i)*100*ms)
	}
	for i, key := range []string{"play_pause", "center", "mic_toggle"} {
		panelKeyAt(m, key, false, base+150*ms+uint64(i)*100*ms)
	}
	evs := drainInput(t, sub)
	var gesture bool
	for _, ev := range evs {
		if ev.Gesture != nil && ev.Gesture.ID == "swipe_up" {
			gesture = true
		}
		if ev.Key != nil && ev.Key.Key == "mic_toggle" {
			t.Fatalf("chain delivered the framework-owned mute edge: %+v", ev)
		}
	}
	if !gesture {
		t.Fatalf("partial capture did not receive swipe_up: %+v", evs)
	}
	if c.mics.Load() != 0 || c.activations.Load() != 0 {
		t.Fatalf("chain leaked a framework effect: %+v", c)
	}
}

// Delayed reads must not compress or stretch real contact time: the converted
// timestamps keep a 90 ms centre/anchor inside the bound and a 700 ms one
// outside it.
func TestDelayedReadKeepsPhysicalGap(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	time.Sleep(900 * time.Millisecond)

	// Physical order: centre at t0, released at t0+40, east pressed at t0+90.
	m.Handle(backend.InputEvent{EventAgeNS: uint64(810 * time.Millisecond), Key: &backend.KeyEvent{Key: "center", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(770 * time.Millisecond), Key: &backend.KeyEvent{Key: "center", Pressed: false}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(720 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(650 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("late-read centre swipe delivered %d next, want 1", got)
	}
	if c.volumes.Load() != 0 {
		t.Fatalf("late-read centre swipe leaked %d volume steps", c.volumes.Load())
	}

	time.Sleep(1000 * time.Millisecond)
	// Physical order: centre at t0, released at t0+40, east pressed at t0+900.
	m.Handle(backend.InputEvent{EventAgeNS: uint64(960 * time.Millisecond), Key: &backend.KeyEvent{Key: "center", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(920 * time.Millisecond), Key: &backend.KeyEvent{Key: "center", Pressed: false}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(60 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(10 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	if c.mediaNext.Load() != 1 {
		t.Fatalf("a true 900 ms gap became a swipe: next=%d", c.mediaNext.Load())
	}
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("a true 900 ms gap stepped volume %d times, want 1", got)
	}
}

// A press read 250 ms late must keep its physical hold deadline: the remaining
// 450 ms are armed, not a fresh 700 ms.
func TestDelayedPressKeepsPhysicalHoldDeadline(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	time.Sleep(300 * time.Millisecond)

	m.Handle(backend.InputEvent{EventAgeNS: uint64(250 * time.Millisecond), Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
	time.Sleep(600 * time.Millisecond)
	if got := c.activations.Load(); got != 1 {
		t.Fatalf("delayed press activated %d times by 600 ms, want 1 (physical 700 ms deadline)", got)
	}
	if c.mediaToggles.Load() != 0 {
		t.Fatalf("hold also toggled media")
	}
}

// Entering full capture must clear framework key bookkeeping even when no
// sequence is pending. Its release is forwarded to the full capture and never
// reaches panelRelease, so stale panelDown would silently swallow the first
// press after full capture ends.
func TestFullCaptureClearsFlushedHeldKeyBookkeeping(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "cap-1")

	pressKey(m, "mic_toggle", true)
	if got := c.mics.Load(); got != 1 {
		t.Fatalf("first microphone press toggles = %d, want 1", got)
	}

	mode.Store(int32(routing.TouchFull))
	m.OnRoutingChanged()
	pressKey(m, "mic_toggle", false)
	mode.Store(int32(routing.TouchNone))
	m.OnRoutingChanged()

	pressKey(m, "mic_toggle", true)
	pressKey(m, "mic_toggle", false)
	if got := c.mics.Load(); got != 2 {
		t.Fatalf("first press after full capture was swallowed: toggles = %d, want 2", got)
	}

	// The same applies to a held volume action that full capture stops.
	pressKey(m, "volume_up", true)
	before := c.volumes.Load()
	mode.Store(int32(routing.TouchFull))
	m.OnRoutingChanged()
	pressKey(m, "volume_up", false)
	mode.Store(int32(routing.TouchNone))
	m.OnRoutingChanged()
	pressKey(m, "volume_up", true)
	pressKey(m, "volume_up", false)
	if got := c.volumes.Load(); got != before+1 {
		t.Fatalf("volume after full capture = %d, want %d", got, before+1)
	}
}

// A backend key whose routing snapshot was taken just before full capture
// acquired the panel must be routed against the current state, not registered
// as framework panel bookkeeping after full capture cleared it. Otherwise its
// release is delivered to the full capture and the first framework press after
// full capture ends is silently swallowed.
func TestPanelPressSampledBeforeFullCaptureIsRoutedToFullOwner(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "cap-1")

	// Hold the serialization lock so the backend press is preempted after the
	// snapshot point but before any panel bookkeeping.
	m.panelExecMu.Lock()
	started := make(chan struct{})
	go func() {
		close(started)
		m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "mic_toggle", Pressed: true}})
	}()
	<-started
	time.Sleep(50 * time.Millisecond)

	mode.Store(int32(routing.TouchFull))
	m.panelMu.Lock()
	m.panelSeq.reset()
	m.panelDown = map[string]bool{}
	m.panelSwallow = map[string]bool{}
	m.panelForward = map[string]string{}
	m.panelHandled = map[string]bool{}
	m.panelMu.Unlock()
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		m.panelExecMu.Unlock()
		t.Fatal(err)
	}
	defer sub.Close()
	m.panelExecMu.Unlock()

	ev := nextEvent(t, sub)
	if ev.Key == nil || ev.Key.Key != "mic_toggle" || !ev.Key.Pressed {
		t.Fatalf("preempted press was not routed to the full owner: %+v", ev)
	}
	if got := c.mics.Load(); got != 0 {
		t.Fatalf("preempted press toggled the framework microphone %d times", got)
	}
	m.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "mic_toggle", Pressed: false}})

	mode.Store(int32(routing.TouchNone))
	m.OnRoutingChanged()
	pressKey(m, "mic_toggle", true)
	pressKey(m, "mic_toggle", false)
	if got := c.mics.Load(); got != 1 {
		t.Fatalf("first press after full capture = %d toggles, want 1", got)
	}
}

// panelKeyEvent is one scripted press or release for the log assertions.
type panelKeyEvent struct {
	key     string
	pressed bool
	at      uint64
}

// Every decision leaves one bounded Info line carrying both contacts, their
// press offsets, the measured gaps and one of the five tier names.
func TestCentreDecisionLoggedAtInfo(t *testing.T) {
	const ms = uint64(time.Millisecond)
	for _, tc := range []struct {
		name   string
		events []panelKeyEvent
		want   []string
	}{
		{
			name: "centre gate accepted",
			events: []panelKeyEvent{
				{"center", true, 1_000_000_000},
				{"center", false, 1_000_000_000 + 40*ms},
				{"volume_up", true, 1_000_000_000 + 90*ms},
				{"volume_up", false, 1_000_000_000 + 160*ms},
			},
			want: []string{"panel gesture swipe_right resolved", "tier=centre", "press_gap=90ms", "window=700ms"},
		},
		{
			name: "held centre gate accepted after press window",
			events: []panelKeyEvent{
				{"center", true, 1_000_000_000},
				{"volume_up", true, 1_000_000_000 + 600*ms},
				{"center", false, 1_000_000_000 + 650*ms},
				{"volume_up", false, 1_000_000_000 + 670*ms},
			},
			want: []string{"panel gesture swipe_right resolved tier=centre", "press_gap=600ms", "window=700ms", "center p+0ms down", "volume_up p+600ms down overlap"},
		},
		{
			name: "held centre gate rejected beyond total budget",
			events: []panelKeyEvent{
				{"center", true, 1_000_000_000},
				{"volume_up", true, 1_000_000_000 + 1600*ms},
				{"volume_up", false, 1_000_000_000 + 1670*ms},
				{"center", false, 1_000_000_000 + 1700*ms},
			},
			want: []string{"panel centre -> volume_up", "decision=keys", "tier=centre", "reason=gesture total", "press_gap=1600ms", "window=700ms"},
		},
		{
			name: "centre gate accepted after a slow roll",
			events: []panelKeyEvent{
				{"center", true, 1_000_000_000},
				{"center", false, 1_000_000_000 + 40*ms},
				{"volume_up", true, 1_000_000_000 + 400*ms},
				{"volume_up", false, 1_000_000_000 + 470*ms},
			},
			want: []string{"panel gesture swipe_right resolved tier=centre", "press_gap=400ms", "window=700ms"},
		},
		{
			name: "centre gate rejected beyond the released window",
			events: []panelKeyEvent{
				{"center", true, 1_000_000_000},
				{"center", false, 1_000_000_000 + 40*ms},
				{"volume_up", true, 1_000_000_000 + 800*ms},
				{"volume_up", false, 1_000_000_000 + 870*ms},
			},
			want: []string{"panel centre -> volume_up", "decision=keys", "tier=centre", "reason=contact gap", "press_gap=800ms", "window=700ms"},
		},
		{
			name: "overlapping pair resolved as keys",
			events: []panelKeyEvent{
				{"play_pause", true, 1_000_000_000},
				{"mic_toggle", true, 1_000_000_000 + 90*ms},
				{"mic_toggle", false, 1_000_000_000 + 130*ms},
				{"play_pause", false, 1_000_000_000 + 140*ms},
				{"center", true, 1_000_000_000 + 500*ms},
			},
			want: []string{"panel gesture rejected", "decision=keys", "tier=overlap", "reason=contact gap", "overlap"},
		},
		{
			name: "full-panel crossing accepted at 380 ms",
			events: []panelKeyEvent{
				{"volume_down", true, 1_000_000_000},
				{"volume_down", false, 1_000_000_000 + 60*ms},
				{"volume_up", true, 1_000_000_000 + 380*ms},
				{"volume_up", false, 1_000_000_000 + 440*ms},
			},
			want: []string{"panel gesture swipe_right resolved", "tier=full", "press_gap=380ms", "window=400ms"},
		},
		{
			name: "full-panel crossing rejected beyond 400 ms",
			events: []panelKeyEvent{
				{"volume_down", true, 1_000_000_000},
				{"volume_down", false, 1_000_000_000 + 60*ms},
				{"volume_up", true, 1_000_000_000 + 500*ms},
				{"volume_up", false, 1_000_000_000 + 560*ms},
			},
			want: []string{"panel full-panel crossing rejected", "decision=keys", "reason=contact gap", "press_gap=500ms", "window=400ms", "both edges stay volume steps"},
		},
		{
			name: "chain accepted",
			events: []panelKeyEvent{
				{"volume_down", true, 1_000_000_000},
				{"center", true, 1_000_000_000 + 100*ms},
				{"volume_up", true, 1_000_000_000 + 200*ms},
			},
			want: []string{"panel gesture swipe_right resolved", "tier=chain"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mode atomic.Int32
			m, c, logs := newPanelLogManager(t, &mode, "")
			for _, ev := range tc.events {
				panelKeyAt(m, ev.key, ev.pressed, ev.at)
			}
			var joined []string
			for _, entry := range logs.Snapshot("") {
				if entry.Severity == "INFO" && strings.Contains(entry.Message, "panel") {
					joined = append(joined, entry.Message)
				}
			}
			if len(joined) == 0 {
				t.Fatal("no panel decision logged at Info")
			}
			all := strings.Join(joined, "\n")
			for _, want := range tc.want {
				if !strings.Contains(all, want) {
					t.Fatalf("log missing %q:\n%s", want, all)
				}
			}
			for _, entry := range joined {
				if strings.Count(entry, "|") > 4 {
					t.Fatalf("log trace exceeded five contacts: %q", entry)
				}
				if strings.ContainsAny(entry, "\x00\t") {
					t.Fatalf("log trace carried control characters: %q", entry)
				}
			}
			if tc.name == "centre gate rejected" && c.volumes.Load() != 1 {
				t.Fatalf("rejected centre gate resolved %d volume steps, want 1", c.volumes.Load())
			}
		})
	}
}

// A capture takeover in the middle of a contact shadow drops the shadow without
// replaying any key action.
func TestCaptureTakeoverDropsContactShadow(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	panelKeyAt(m, "center", true, 1_000_000)
	if m.panelSeq.n != 1 {
		t.Fatalf("centre contact shadow = %d contacts, want 1", m.panelSeq.n)
	}
	mode.Store(int32(routing.TouchFull))
	m.OnRoutingChanged()
	if m.panelSeq.n != 0 {
		t.Fatalf("contact shadow survived a capture takeover: %d contacts", m.panelSeq.n)
	}
	panelKeyAt(m, "volume_up", true, 1_100_000)
	panelKeyAt(m, "volume_up", false, 1_170_000)
	if got := c.volumes.Load(); got != 0 {
		t.Fatalf("takeover replayed a volume action: %d steps", got)
	}
}

// A closed perimeter traversal is the one advertised gesture that yields no
// key suppression: full capture already forwards every key, so the shadow adds
// exactly one circle event on top of the four perimeter keys.
func TestFullCaptureInfersCircleTraversal(t *testing.T) {
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)
	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{"clockwise", []string{"mic_toggle", "volume_up", "play_pause", "volume_down", "mic_toggle"}, "circle_cw"},
		{"counter-clockwise", []string{"mic_toggle", "volume_down", "play_pause", "volume_up", "mic_toggle"}, "circle_ccw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mode atomic.Int32
			mode.Store(int32(routing.TouchFull))
			m, c := newPanelModeManager(t, &mode, "cap-1")
			sub, err := m.Subscribe("cap-1")
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()

			// One continuous contact: each key is pressed 60 ms after the
			// previous press and released 90 ms after it, so every contact
			// begins while the previous one is still down.
			at := func(i uint64) uint64 { return base + i*60*ms }
			events := []panelKeyEvent{
				{tc.keys[0], true, at(0)},
				{tc.keys[1], true, at(1)},
				{tc.keys[0], false, at(0) + 90*ms},
				{tc.keys[2], true, at(2)},
				{tc.keys[1], false, at(1) + 90*ms},
				{tc.keys[3], true, at(3)},
				{tc.keys[2], false, at(2) + 90*ms},
				{tc.keys[4], true, at(4)},
				{tc.keys[3], false, at(3) + 90*ms},
				{tc.keys[4], false, at(4) + 90*ms},
			}
			for _, ev := range events {
				panelKeyAt(m, ev.key, ev.pressed, ev.at)
			}
			evs := drainInput(t, sub)

			var gestures []string
			var presses, releases int
			for _, ev := range evs {
				if ev.Gesture != nil && ev.Gesture.Phase == "end" {
					gestures = append(gestures, ev.Gesture.ID)
				}
				if ev.Key != nil {
					if ev.Key.Pressed {
						presses++
					} else {
						releases++
					}
				}
			}
			if len(gestures) != 1 || gestures[0] != tc.want {
				t.Fatalf("full capture gestures = %v, want exactly %s", gestures, tc.want)
			}
			if presses != len(tc.keys) || releases != len(tc.keys) {
				t.Fatalf("full capture received %d/%d presses/releases, want every key forwarded",
					presses, releases)
			}
			if c.volumes.Load() != 0 || c.mics.Load() != 0 || c.mediaToggles.Load() != 0 {
				t.Fatalf("full capture ran a framework control: %+v", c)
			}
		})
	}
}

// Partial capture must never infer a circle: suppressing four key actions that
// the framework or the app already own would be a silent loss. The traversal
// stays its own keys and no gesture is emitted.
func TestPartialCaptureNeverInfersCircles(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)
	keys := []string{"mic_toggle", "volume_up", "play_pause", "volume_down", "mic_toggle"}
	for i, key := range keys {
		panelKeyAt(m, key, true, base+uint64(i)*60*ms)
		panelKeyAt(m, key, false, base+uint64(i)*60*ms+90*ms)
	}
	evs := drainInput(t, sub)

	var circleKeys int
	for _, ev := range evs {
		if ev.Gesture != nil {
			t.Fatalf("partial capture received a gesture: %+v", ev)
		}
		if ev.Key != nil && ev.Key.Key == "play_pause" {
			circleKeys++
		}
	}
	if circleKeys != 2 {
		t.Fatalf("partial capture received %d circle-key events, want press+release", circleKeys)
	}
	if got := c.mics.Load(); got != 2 {
		t.Fatalf("traversal muted %d times, want 2 framework toggles", got)
	}
	if got := c.volumes.Load(); got != 2 {
		t.Fatalf("traversal stepped volume %d times, want 2 framework steps", got)
	}
}
