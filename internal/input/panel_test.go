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

func newPanelModeManager(t *testing.T, mode *atomic.Int32, captureID string) (*Manager, *controlCounters) {
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
	return m, c
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

func sendPresses(m *Manager, keys []string, base uint64) {
	for i, key := range keys {
		panelKeyAt(m, key, true, base+uint64(i)*100_000)
	}
}

func sendReleases(m *Manager, keys []string, base uint64) {
	for i, key := range keys {
		panelKeyAt(m, key, false, base+uint64(i)*100_000+50_000)
	}
}

func tapPanelKey(m *Manager, key string, ts uint64) {
	panelKeyAt(m, key, true, ts)
	panelKeyAt(m, key, false, ts+10_000)
}

// Partial capture owns the circle/play-pause and centre keys. It receives
// their presses, releases and short taps, while volume and microphone stay
// framework controls and no media or assistant action runs behind its back.
func TestPartialPanelForwardsCircleAndCentreOnly(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	tapPanelKey(m, "play_pause", 1_000_000)
	evs := drainInput(t, sub)
	if len(evs) != 2 || evs[0].Key == nil || evs[0].Key.Key != "play_pause" || !evs[0].Key.Pressed ||
		evs[1].Key == nil || evs[1].Key.Key != "play_pause" || evs[1].Key.Pressed {
		t.Fatalf("partial play_pause events = %+v", evs)
	}

	tapPanelKey(m, "center", 2_000_000)
	evs = drainInput(t, sub)
	if len(evs) != 2 || evs[0].Key == nil || evs[0].Key.Key != "center" || !evs[0].Key.Pressed ||
		evs[1].Key == nil || evs[1].Key.Key != "center" || evs[1].Key.Pressed {
		t.Fatalf("partial centre events = %+v", evs)
	}

	tapPanelKey(m, "volume_up", 3_000_000)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	tapPanelKey(m, "mic_toggle", 4_000_000)
	if evs := drainInput(t, sub); len(evs) != 0 {
		t.Fatalf("volume/microphone leaked to the partial capture: %+v", evs)
	}
	if got := c.volumes.Load(); got != 1 {
		t.Fatalf("partial capture volume steps = %d, want 1", got)
	}
	if got := c.mics.Load(); got != 1 {
		t.Fatalf("partial capture microphone toggles = %d, want 1", got)
	}
	if c.activations.Load() != 0 || c.mediaToggles.Load() != 0 || c.dismissed.Load() != 0 {
		t.Fatalf("partial capture leaked framework effects: activations=%d media=%d dismissed=%d",
			c.activations.Load(), c.mediaToggles.Load(), c.dismissed.Load())
	}
}

// Full capture receives every key and inferred gesture, and no system effect
// runs: no mute, volume, transport, dismissal or assistant activation.
func TestFullCaptureSuppressesEverySystemEffect(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchFull))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	keys := []string{"mic_toggle", "volume_up", "play_pause", "volume_down", "center"}
	var base uint64 = 1_000_000_000
	for i, key := range keys {
		tapPanelKey(m, key, base+uint64(i)*400_000_000)
	}
	// A closed clockwise circle is recognized in addition to the raw keys.
	circle := []string{"mic_toggle", "volume_up", "play_pause", "volume_down", "mic_toggle"}
	for i, key := range circle {
		ts := base + 3_000_000_000 + uint64(i)*100_000_000
		panelKeyAt(m, key, true, ts)
		panelKeyAt(m, key, false, ts+10_000)
	}
	evs := drainInput(t, sub)
	var keyPresses, gestures int
	var circleSeen bool
	for _, ev := range evs {
		switch {
		case ev.Key != nil:
			if ev.Key.Pressed {
				keyPresses++
			}
		case ev.Gesture != nil:
			gestures++
			if ev.Gesture.ID == "circle_cw" {
				circleSeen = true
			}
		}
	}
	if keyPresses != len(keys)+len(circle) {
		t.Fatalf("full capture key presses = %d, want %d (%+v)", keyPresses, len(keys)+len(circle), evs)
	}
	if !circleSeen || gestures == 0 {
		t.Fatalf("full capture did not receive the inferred circle gesture: %+v", evs)
	}
	if c.volumes.Load() != 0 || c.mics.Load() != 0 || c.activations.Load() != 0 ||
		c.mediaToggles.Load() != 0 || c.mediaPrevious.Load() != 0 || c.mediaNext.Load() != 0 || c.dismissed.Load() != 0 {
		t.Fatalf("full capture leaked system effects: %+v", c)
	}
}

// Swipes are inferred from discrete key sequences: opposite, centre,
// direction. Their constituent volume, mute and talk presses must not act.
func TestSwipeGesturesFromDiscretePanelKeys(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	cases := []struct {
		name     string
		keys     []string
		wantNext int32
		wantPrev int32
	}{
		{"right", []string{"volume_down", "center", "volume_up"}, 1, 0},
		{"left", []string{"volume_up", "center", "volume_down"}, 1, 1},
		{"up", []string{"play_pause", "center", "mic_toggle"}, 1, 1},
		{"down", []string{"mic_toggle", "center", "play_pause"}, 1, 1},
	}
	var base uint64 = 1_000_000
	for _, tc := range cases {
		sendPresses(m, tc.keys, base)
		sendReleases(m, tc.keys, base)
		base += 1_000_000
		if got := c.mediaNext.Load(); got != tc.wantNext {
			t.Fatalf("%s swipe mediaNext = %d, want %d", tc.name, got, tc.wantNext)
		}
		if got := c.mediaPrevious.Load(); got != tc.wantPrev {
			t.Fatalf("%s swipe mediaPrevious = %d, want %d", tc.name, got, tc.wantPrev)
		}
	}
	if c.volumes.Load() != 0 || c.mics.Load() != 0 || c.activations.Load() != 0 || c.taps.Load() != 0 {
		t.Fatalf("swipe leaked its constituent effects: %+v", c)
	}
}

// A physical swipe on the stock panel is an overlapping contact chain: the
// centre is pressed first and the destination edge follows ~96 ms later while
// the centre is still down; the centre is released just after the edge. This is
// the sequence captured from the unit (centre -> west). It must become one
// swipe with no leaked volume step.
func TestObservedOverlappingSwipeChainDoesNotLeakVolume(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")

	panelKeyAt(m, "center", true, 1_000_000)
	panelKeyAt(m, "volume_down", true, 1_095_700)
	panelKeyAt(m, "center", false, 1_095_750)
	panelKeyAt(m, "volume_down", false, 1_151_000)
	time.Sleep(panelInterContact + 100*time.Millisecond)

	if got := c.mediaPrevious.Load(); got != 1 {
		t.Fatalf("centre->west swipe delivered %d previous, want 1", got)
	}
	if c.mediaNext.Load() != 0 || c.mediaToggles.Load() != 0 {
		t.Fatalf("swipe produced another media action: next=%d toggle=%d", c.mediaNext.Load(), c.mediaToggles.Load())
	}
	if c.volumes.Load() != 0 || c.mics.Load() != 0 || c.activations.Load() != 0 || c.taps.Load() != 0 {
		t.Fatalf("observed swipe leaked effects: %+v", c)
	}
}

// Full capture receives the inferred swipe for an opposite-edge pair and every
// key, and still suppresses media and the key effects.
func TestFullCaptureReceivesOppositePairSwipeWithoutMedia(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchFull))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	panelKeyAt(m, "volume_down", true, 1_000_000)
	panelKeyAt(m, "volume_up", true, 1_090_000)
	panelKeyAt(m, "volume_down", false, 1_150_000)
	panelKeyAt(m, "volume_up", false, 1_160_000)
	events := drainInput(t, sub)

	var gesture bool
	var keys int
	for _, ev := range events {
		if ev.Gesture != nil && ev.Gesture.ID == "swipe_right" {
			gesture = true
		}
		if ev.Key != nil {
			keys++
		}
	}
	if !gesture {
		t.Fatalf("full capture missed the inferred swipe: %+v", events)
	}
	if keys != 4 {
		t.Fatalf("full capture received %d key events, want every press and release", keys)
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 || c.volumes.Load() != 0 {
		t.Fatalf("full capture leaked a system effect: %+v", c)
	}
}

// Centre-first horizontal half swipes are accepted when unambiguous.
func TestCentreHalfSwipes(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	for _, tc := range []struct {
		keys  []string
		next  int32
		prev  int32
		label string
	}{
		{[]string{"center", "volume_up"}, 1, 0, "right"},
		{[]string{"center", "volume_down"}, 1, 1, "left"},
	} {
		sendPresses(m, tc.keys, 1_000_000)
		sendReleases(m, tc.keys, 1_000_000)
		if got := c.mediaNext.Load(); got != tc.next {
			t.Fatalf("%s half swipe next = %d, want %d", tc.label, got, tc.next)
		}
		if got := c.mediaPrevious.Load(); got != tc.prev {
			t.Fatalf("%s half swipe previous = %d, want %d", tc.label, got, tc.prev)
		}
	}
	if c.volumes.Load() != 0 || c.mics.Load() != 0 || c.activations.Load() != 0 {
		t.Fatalf("horizontal half swipe leaked effects: %+v", c)
	}
}

// A centre-first vertical pair is capture-only: outside a capture it must keep
// the deliberate circle and mute controls instead of suppressing them.
func TestVerticalHalfSwipesKeepDeliberateControls(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")

	panelKeyAt(m, "center", true, 1_000_000)
	panelKeyAt(m, "mic_toggle", true, 1_090_000)
	panelKeyAt(m, "center", false, 1_150_000)
	panelKeyAt(m, "mic_toggle", false, 1_160_000)
	time.Sleep(panelInterContact + 100*time.Millisecond)
	if c.mics.Load() != 1 {
		t.Fatalf("centre/mute pair muted %d times, want 1", c.mics.Load())
	}
	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("vertical pair produced a media action")
	}

	panelKeyAt(m, "center", true, 2_000_000)
	panelKeyAt(m, "play_pause", true, 2_090_000)
	panelKeyAt(m, "center", false, 2_150_000)
	panelKeyAt(m, "play_pause", false, 2_160_000)
	time.Sleep(panelInterContact + 100*time.Millisecond)
	if c.mediaToggles.Load() != 1 {
		t.Fatalf("centre/circle pair toggled media %d times, want 1", c.mediaToggles.Load())
	}
	if c.activations.Load() != 0 {
		t.Fatalf("centre/circle pair summoned the assistant")
	}
}

// A circle is a closed perimeter traversal starting at any perimeter key.
// Three quarters of a circle is not a gesture and must not mute, step volume,
// toggle media or summon the assistant.
func TestCircleRequiresClosedPerimeter(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	open := []string{"mic_toggle", "volume_up", "play_pause", "volume_down"}
	var base uint64 = 1_000_000
	// One continuous crossing: each key is pressed while the previous is still
	// down, so this is a gesture attempt that times out, not four taps.
	for _, key := range open {
		panelKeyAt(m, key, true, base)
		base += 100_000
	}
	for _, key := range open {
		panelKeyAt(m, key, false, base)
		base += 40_000
	}
	for _, ev := range drainInput(t, sub) {
		if ev.Gesture != nil {
			t.Fatalf("unfinished circle produced a gesture: %+v", ev)
		}
	}
	if c.volumes.Load() != 0 || c.mics.Load() != 0 || c.activations.Load() != 0 {
		t.Fatalf("unfinished crossing leaked system effects: %+v", c)
	}

	checkCircle := func(keys []string, want string) {
		t.Helper()
		for _, key := range keys {
			panelKeyAt(m, key, true, base)
			panelKeyAt(m, key, false, base+10_000)
			base += 100_000
		}
		evs := drainInput(t, sub)
		found := false
		for _, ev := range evs {
			if ev.Gesture != nil && ev.Gesture.ID == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("closed circle %v did not produce %s: %+v", keys, want, evs)
		}
	}
	checkCircle([]string{"mic_toggle", "volume_up", "play_pause", "volume_down", "mic_toggle"}, "circle_cw")
	checkCircle([]string{"mic_toggle", "volume_down", "play_pause", "volume_up", "mic_toggle"}, "circle_ccw")
	if c.volumes.Load() != 0 || c.mics.Load() != 0 {
		t.Fatalf("circle leaked volume/mute effects: %+v", c)
	}
}

// A key after the inter-contact window starts a fresh sequence: a centre press
// followed too late by east is not a swipe.
func TestSwipeInterContactBoundary(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")

	// Inside the window: centre -> east is an unambiguous right half swipe.
	panelKeyAt(m, "center", true, 0)
	time.Sleep(80 * time.Millisecond)
	panelKeyAt(m, "volume_up", true, 0)
	panelKeyAt(m, "center", false, 0)
	panelKeyAt(m, "volume_up", false, 0)
	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("in-window half swipe mediaNext = %d, want 1", got)
	}

	// Outside the window the centre press has already failed on its own; the
	// east press starts a new single-key sequence and no swipe is inferred.
	panelKeyAt(m, "center", true, 0)
	time.Sleep(panelInterContact + 120*time.Millisecond)
	panelKeyAt(m, "center", false, 0)
	panelKeyAt(m, "volume_up", true, 0)
	panelKeyAt(m, "volume_up", false, 0)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("late centre+volume leaked a swipe: mediaNext = %d", got)
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

// The centre key is neutral: a tap does nothing, a hold does not summon, but
// alert dismissal still consumes its press and release.
func TestNeutralCentreNeverSummons(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	tapPanelKey(m, "center", 1_000_000)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	panelKeyAt(m, "center", true, 2_000_000)
	time.Sleep(holdThreshold + 120*time.Millisecond)
	panelKeyAt(m, "center", false, 2_100_000)
	if c.activations.Load() != 0 || c.mediaToggles.Load() != 0 {
		t.Fatalf("neutral centre acted: %+v", c)
	}
}

// A partial capture receives an inferred swipe as one semantic gesture, not as
// the constituent volume/centre/mute key events and not as a framework media
// command.
func TestPartialSwipeDeliveredWithoutSystemEffects(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// West -> centre -> east is swipe_right.
	sendPresses(m, []string{"volume_down", "center", "volume_up"}, 1_000_000)
	sendReleases(m, []string{"volume_down", "center", "volume_up"}, 1_000_000)
	evs := drainInput(t, sub)
	gesture := false
	for _, ev := range evs {
		if ev.Key != nil {
			t.Fatalf("swipe leaked a constituent key to the capture: %+v", ev)
		}
		if ev.Gesture != nil && ev.Gesture.ID == "swipe_right" {
			gesture = true
		}
	}
	if !gesture {
		t.Fatalf("partial capture did not receive swipe_right: %+v", evs)
	}
	if c.volumes.Load() != 0 || c.mediaNext.Load() != 0 || c.activations.Load() != 0 {
		t.Fatalf("partial swipe leaked system effects: %+v", c)
	}
}

// A deferred press must be applied before its matching release even when the
// promise timer and release arrive together. The release handler blocks on the
// in-flight volume step in the broken ordering and would leave an 8 s ramp
// running without a stop.
func TestPanelTimeoutPressPrecedesRelease(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	var mode atomic.Int32 // TouchNone
	var steps atomic.Int32
	entered := make(chan struct{})
	unblock := make(chan struct{})
	m := NewManager(context.Background(), clock, logs, backend.TouchCaps{},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchMode(mode.Load()) },
		Handlers{VolumeUp: func() bool {
			if steps.Add(1) == 1 {
				close(entered)
				<-unblock
			}
			return true
		}})
	defer m.Close()

	panelKeyAt(m, "volume_up", true, 0)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred press never reached the volume handler")
	}
	released := make(chan struct{})
	go func() {
		panelKeyAt(m, "volume_up", false, 0)
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("release was processed before the deferred press")
	case <-time.After(100 * time.Millisecond):
	}
	close(unblock)
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("release did not complete after the press was allowed to finish")
	}
	time.Sleep(volumeRepeatStart + 100*time.Millisecond)
	if got := steps.Load(); got != 1 {
		t.Fatalf("volume stepped %d times; the release did not stop the deferred press's ramp", got)
	}
}

// Entering full capture must clear framework key bookkeeping even when a held
// key was already flushed and no sequence is pending. Its release is forwarded
// to the full capture and never reaches panelRelease, so stale panelDown would
// silently swallow the first press after full capture ends.
func TestFullCaptureClearsFlushedHeldKeyBookkeeping(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "cap-1")

	pressKey(m, "mic_toggle", true)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	if got := c.mics.Load(); got != 1 {
		t.Fatalf("first microphone press toggles = %d, want 1", got)
	}

	// Release only after full capture owns the keys, so panelRelease never sees
	// it and would leave panelDown set without the full-transition cleanup.
	mode.Store(int32(routing.TouchFull))
	m.OnRoutingChanged()
	pressKey(m, "mic_toggle", false)
	mode.Store(int32(routing.TouchNone))
	m.OnRoutingChanged()

	pressKey(m, "mic_toggle", true)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	pressKey(m, "mic_toggle", false)
	if got := c.mics.Load(); got != 2 {
		t.Fatalf("first press after full capture was swallowed: toggles = %d, want 2", got)
	}

	// The same applies to a held volume action that full capture stops.
	pressKey(m, "volume_up", true)
	time.Sleep(panelInterContact + 80*time.Millisecond)
	before := c.volumes.Load()
	mode.Store(int32(routing.TouchFull))
	m.OnRoutingChanged()
	pressKey(m, "volume_up", false)
	mode.Store(int32(routing.TouchNone))
	m.OnRoutingChanged()
	pressKey(m, "volume_up", true)
	time.Sleep(panelInterContact + 80*time.Millisecond)
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
	// old snapshot point but before any panel bookkeeping.
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
	if m.panelSeq != nil && m.panelSeq.timer != nil {
		m.panelSeq.timer.Stop()
	}
	m.panelSeq = nil
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

	// The press now resolves under the current full capture and is delivered
	// to its owner, never to the framework microphone control.
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
	time.Sleep(panelInterContact + 80*time.Millisecond)
	pressKey(m, "mic_toggle", false)
	if got := c.mics.Load(); got != 1 {
		t.Fatalf("first press after full capture = %d toggles, want 1", got)
	}
}

// Two sequential volume taps inside the inter-contact window are deliberate
// controls, not a crossing: each steps volume and no media command runs.
func TestSequentialVolumeTapsStayDeliberate(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	panelKeyAt(m, "volume_up", true, 0)
	panelKeyAt(m, "volume_up", false, 0)
	panelKeyAt(m, "volume_down", true, 0)
	panelKeyAt(m, "volume_down", false, 0)
	time.Sleep(panelInterContact + 100*time.Millisecond)

	if c.volumes.Load() != 2 {
		t.Fatalf("sequential volume taps stepped %d times, want 2", c.volumes.Load())
	}
	if c.mediaPrevious.Load() != 0 || c.mediaNext.Load() != 0 {
		t.Fatalf("sequential taps became a skip: prev=%d next=%d", c.mediaPrevious.Load(), c.mediaNext.Load())
	}
}

// A circle tap quickly followed by a mute tap keeps both functions. With a
// release between them it is not a vertical swipe.
func TestSequentialCircleThenMuteStaysDeliberate(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	tapPanelKey(m, "play_pause", 0)
	panelKeyAt(m, "mic_toggle", true, 0)
	panelKeyAt(m, "mic_toggle", false, 0)
	time.Sleep(panelInterContact + 100*time.Millisecond)

	if c.mediaToggles.Load() != 1 || c.mics.Load() != 1 {
		t.Fatalf("sequential circle/mute acted %d/%d times, want 1/1", c.mediaToggles.Load(), c.mics.Load())
	}
	if c.activations.Load() != 0 {
		t.Fatalf("sequential taps summoned the assistant")
	}
}

// Two different controls separated by a release must not merge into a failed
// gesture that drops both effects.
func TestSequentialCentreThenVolumeStaysDeliberate(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	tapPanelKey(m, "center", 0)
	panelKeyAt(m, "volume_up", true, 0)
	panelKeyAt(m, "volume_up", false, 0)
	time.Sleep(panelInterContact + 100*time.Millisecond)

	if c.mediaNext.Load() != 0 || c.mediaPrevious.Load() != 0 {
		t.Fatalf("sequential centre/volume became a swipe")
	}
	if c.volumes.Load() != 1 {
		t.Fatalf("sequential centre/volume stepped %d times, want 1", c.volumes.Load())
	}
}

// An overlapping vertical pair has no framework gesture action. Outside a
// capture it must still deliver the deliberate circle and mute controls.
func TestOverlappingVerticalPairKeepsCircleAndMute(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	panelKeyAt(m, "play_pause", true, 0)
	panelKeyAt(m, "mic_toggle", true, 0)
	panelKeyAt(m, "mic_toggle", false, 0)
	panelKeyAt(m, "play_pause", false, 0)
	time.Sleep(panelInterContact + 100*time.Millisecond)

	if c.mics.Load() != 1 || c.mediaToggles.Load() != 1 {
		t.Fatalf("overlapping vertical pair lost controls: mic=%d toggle=%d", c.mics.Load(), c.mediaToggles.Load())
	}
	if c.activations.Load() != 0 {
		t.Fatalf("overlapping vertical pair summoned the assistant")
	}
}

// A partial capture gets the horizontal pair as one gesture and no constituent
// volume keys or framework steps.
func TestPartialOppositePairDeliversGestureOnly(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	panelKeyAt(m, "volume_down", true, 0)
	panelKeyAt(m, "volume_up", true, 0)
	panelKeyAt(m, "volume_down", false, 0)
	panelKeyAt(m, "volume_up", false, 0)
	evs := drainInput(t, sub)
	gesture := false
	for _, ev := range evs {
		if ev.Key != nil {
			t.Fatalf("partial pair leaked a volume key: %+v", ev)
		}
		if ev.Gesture != nil && ev.Gesture.ID == "swipe_right" {
			gesture = true
		}
	}
	if !gesture {
		t.Fatalf("partial capture did not receive swipe_right: %+v", evs)
	}
	if c.volumes.Load() != 0 || c.mediaNext.Load() != 0 {
		t.Fatalf("partial pair leaked a system effect: %+v", c)
	}
}

// A partial capture keeps the microphone control: an overlapping vertical pair
// is not consumed as a capture-only gesture, and the app still receives the
// circle press and release it owns.
func TestPartialVerticalPairKeepsMuteAndCircle(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	panelKeyAt(m, "play_pause", true, 0)
	panelKeyAt(m, "mic_toggle", true, 0)
	panelKeyAt(m, "mic_toggle", false, 0)
	panelKeyAt(m, "play_pause", false, 0)
	evs := drainInput(t, sub)
	var presses, releases int
	for _, ev := range evs {
		if ev.Gesture != nil {
			t.Fatalf("vertical pair produced a capture gesture: %+v", ev)
		}
		if ev.Key != nil && ev.Key.Key == "play_pause" {
			if ev.Key.Pressed {
				presses++
			} else {
				releases++
			}
		}
	}
	if presses != 1 || releases != 1 {
		t.Fatalf("capture received circle press/release %d/%d, want 1/1", presses, releases)
	}
	if c.mics.Load() != 1 {
		t.Fatalf("microphone toggled %d times, want 1", c.mics.Load())
	}
}

// A delayed read must not compress real inter-contact time. All four events are
// handed over together ~150 ms late, but the physical 95 ms crossing still
// forms one swipe and leaks no volume.
func TestDelayedReadKeepsPhysicalSwipeGap(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	time.Sleep(200 * time.Millisecond)

	m.Handle(backend.InputEvent{EventAgeNS: uint64(150 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(55 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_down", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(20 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(10 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_down", Pressed: false}})

	if got := c.mediaPrevious.Load(); got != 1 {
		t.Fatalf("late-dispatched crossing delivered %d previous, want 1", got)
	}
	if c.volumes.Load() != 0 {
		t.Fatalf("late-dispatched crossing leaked %d volume steps", c.volumes.Load())
	}
}

// A real 300 ms gap must not merge into a swipe merely because dispatch was
// delayed: the converted timestamps keep both contacts deliberate.
func TestDelayedReadKeepsTrueGapFromMerging(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	time.Sleep(450 * time.Millisecond)

	// Physical order: up at t0, released 50 ms later, down pressed at t0+300.
	m.Handle(backend.InputEvent{EventAgeNS: uint64(380 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(330 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(80 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_down", Pressed: true}})
	m.Handle(backend.InputEvent{EventAgeNS: uint64(70 * time.Millisecond), Key: &backend.KeyEvent{Key: "volume_down", Pressed: false}})
	time.Sleep(panelInterContact + 100*time.Millisecond)

	if c.mediaPrevious.Load() != 0 || c.mediaNext.Load() != 0 {
		t.Fatalf("a true 300 ms gap became a swipe: prev=%d next=%d", c.mediaPrevious.Load(), c.mediaNext.Load())
	}
	if c.volumes.Load() != 2 {
		t.Fatalf("true 300 ms gap stepped volume %d times, want 2", c.volumes.Load())
	}
}

// A press read 250 ms late must keep its physical hold deadline: only the
// remaining 200 ms are armed after the deferred panel press, not a fresh hold.
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

// Full capture receives the exact vertical pair gesture and every key, with no
// framework side effect.
func TestFullVerticalOverlapDeliversSwipeAndKeys(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchFull))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{"play_pause", "mic_toggle"}, "swipe_up"},
		{[]string{"mic_toggle", "play_pause"}, "swipe_down"},
	} {
		panelKeyAt(m, tc.keys[0], true, 0)
		panelKeyAt(m, tc.keys[1], true, 0)
		panelKeyAt(m, tc.keys[0], false, 0)
		panelKeyAt(m, tc.keys[1], false, 0)
		evs := drainInput(t, sub)
		var ids []string
		var keys int
		for _, ev := range evs {
			if ev.Gesture != nil && ev.Gesture.Phase == "end" {
				ids = append(ids, ev.Gesture.ID)
			}
			if ev.Key != nil {
				keys++
			}
		}
		if len(ids) != 1 || ids[0] != tc.want {
			t.Fatalf("%v gestures = %v, want exactly %s", tc.keys, ids, tc.want)
		}
		if keys != 4 {
			t.Fatalf("%v delivered %d key events, want every press and release", tc.keys, keys)
		}
	}
	if c.mediaToggles.Load() != 0 || c.mics.Load() != 0 || c.volumes.Load() != 0 {
		t.Fatalf("full capture leaked a system effect: %+v", c)
	}
}

// Three-contact vertical crossings keep their exact gesture IDs in a partial
// capture, with no constituent key or framework effect.
func TestPartialVerticalCrossingGestureIDs(t *testing.T) {
	var mode atomic.Int32
	mode.Store(int32(routing.TouchPartial))
	m, c := newPanelModeManager(t, &mode, "cap-1")
	sub, err := m.Subscribe("cap-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	var base uint64 = 1_000_000
	for _, tc := range []struct {
		keys []string
		want string
	}{
		{[]string{"play_pause", "center", "mic_toggle"}, "swipe_up"},
		{[]string{"mic_toggle", "center", "play_pause"}, "swipe_down"},
	} {
		sendPresses(m, tc.keys, base)
		sendReleases(m, tc.keys, base)
		base += 2_000_000
		evs := drainInput(t, sub)
		var ids []string
		for _, ev := range evs {
			if ev.Key != nil {
				t.Fatalf("%v leaked a key to the capture: %+v", tc.keys, ev)
			}
			if ev.Gesture != nil && ev.Gesture.Phase == "end" {
				ids = append(ids, ev.Gesture.ID)
			}
		}
		if len(ids) != 1 || ids[0] != tc.want {
			t.Fatalf("%v gestures = %v, want exactly %s", tc.keys, ids, tc.want)
		}
	}
	if c.volumes.Load() != 0 || c.mics.Load() != 0 || c.mediaToggles.Load() != 0 {
		t.Fatalf("vertical crossing leaked a system effect: %+v", c)
	}
}
