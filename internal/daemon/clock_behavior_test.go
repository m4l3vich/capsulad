package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	clockv1 "capsulad/gen/go/capsulad/contracts/clock/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

func TestClockTimerAlertPersistsUntilDismissed(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	started := app.startTimer(1000, "Tea")
	advance(time.Second)
	app.tick()

	state := app.state()
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].Kind != clockv1.AlertKind_ALERT_KIND_TIMER {
		t.Fatalf("timer alert = %+v", state.ActiveAlerts)
	}
	alertID := state.ActiveAlerts[0].AlertId
	waitFor(t, time.Second, "completed timer displays 00:00", func() bool {
		frame := d.dev.(*backend.Simulator).SegmentSnapshot()
		if len(frame.Cells) != 4 {
			return false
		}
		for _, cell := range frame.Cells {
			if cell != clockDigitMasks[0] {
				return false
			}
		}
		return true
	})

	// The old contract expired timer alerts after ten seconds. A timer must
	// ring until the user dismisses it instead.
	advance(10 * time.Minute)
	app.tick()
	state = app.state()
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].AlertId != alertID {
		t.Fatalf("alert did not persist: %+v", state.ActiveAlerts)
	}
	if len(state.Timers) != 1 || state.Timers[0].TimerId != started.TimerId ||
		state.Timers[0].Status != clockv1.TimerStatus_TIMER_STATUS_COMPLETED {
		t.Fatalf("completed timer was lost while its alert was active: %+v", state.Timers)
	}

	if !app.dismissAlert(alertID) {
		t.Fatal("timer alert was not dismissed")
	}
	if state := app.state(); len(state.ActiveAlerts) != 0 || len(state.Timers) != 0 {
		t.Fatalf("state after dismiss = %+v", state)
	}
}

func TestClockAlarmAlertPersistsUntilDismissed(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	setTestAlarms(t, app, "1 9 * * *")
	advance(time.Minute)
	app.tick()

	state := app.state()
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].Kind != clockv1.AlertKind_ALERT_KIND_ALARM {
		t.Fatalf("alarm alert = %+v", state.ActiveAlerts)
	}
	advance(5 * time.Minute)
	app.tick()
	if state := app.state(); len(state.ActiveAlerts) != 1 {
		t.Fatalf("alarm alert expired: %+v", state.ActiveAlerts)
	}
	if !app.dismissAlert("") {
		t.Fatal("alarm alert was not dismissed")
	}
}

func TestClockAlarmRemovalStopsRinging(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	setTestAlarms(t, app, "1 9 * * *")
	advance(time.Minute)
	app.tick()
	if state := app.state(); len(state.ActiveAlerts) != 1 {
		t.Fatalf("alarm did not ring: %+v", state.ActiveAlerts)
	}
	// Removing the schedule (the friendly editor disables an alarm by omitting
	// it) dismisses its alert; unrelated settings keep other alarms intact.
	setTestAlarms(t, app)
	if state := app.state(); len(state.ActiveAlerts) != 0 {
		t.Fatalf("removing the alarm left an alert: %+v", state.ActiveAlerts)
	}

	setTestAlarms(t, app, "2 9 * * *")
	advance(time.Minute)
	app.tick()
	if state := app.state(); len(state.ActiveAlerts) != 1 {
		t.Fatalf("second alarm did not ring: %+v", state.ActiveAlerts)
	}
	setTestAlarms(t, app)
	if state := app.state(); len(state.ActiveAlerts) != 0 {
		t.Fatalf("deleting the alarm left an alert: %+v", state.ActiveAlerts)
	}
}

func TestClockRecurringAlarmDoesNotDuplicateActiveAlert(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	setTestAlarms(t, app, "1 9 * * *")
	advance(time.Minute)
	app.tick()
	state := app.state()
	if len(state.ActiveAlerts) != 1 {
		t.Fatalf("daily alarm alert = %+v", state.ActiveAlerts)
	}
	alertID := state.ActiveAlerts[0].AlertId

	// The next daily occurrence must keep the same ringing alert, not queue a
	// second one behind it.
	advance(24 * time.Hour)
	app.tick()
	state = app.state()
	if len(state.ActiveAlerts) != 1 || state.ActiveAlerts[0].AlertId != alertID {
		t.Fatalf("daily alarm duplicated its alert: %+v", state.ActiveAlerts)
	}
	if !app.dismissAlert("") {
		t.Fatal("daily alarm alert was not dismissed")
	}
}

func TestClockCountdownExtinguishesCounterclockwiseFromTop(t *testing.T) {
	const duration = uint64(60 * time.Second)
	for _, step := range []struct {
		elapsed time.Duration
		lit     int
	}{
		{0, 12},
		{5 * time.Second, 11},
		{30 * time.Second, 6},
		{55 * time.Second, 1},
		{60 * time.Second, 0},
	} {
		remaining := duration - uint64(step.elapsed)
		if got := countdownLitPixels(12, duration, remaining); got != step.lit {
			t.Fatalf("elapsed %v: lit = %d, want %d", step.elapsed, got, step.lit)
		}
		pixels := countdownRingPixels(12, step.lit)
		if len(pixels) != 12 {
			t.Fatalf("elapsed %v: frame has %d pixels", step.elapsed, len(pixels))
		}
		if step.lit == 12 {
			for i, px := range pixels {
				if px != ringCountdownColor {
					t.Fatalf("full ring pixel %d = %+v", i, px)
				}
			}
			continue
		}
		if pixels[0] != (backend.RGB{}) {
			t.Fatalf("elapsed %v: top pixel stayed lit", step.elapsed)
		}
		for i := 1; i < 12; i++ {
			wantLit := i <= step.lit
			if got := pixels[i] != (backend.RGB{}); got != wantLit {
				t.Fatalf("elapsed %v: pixel %d lit=%v, want %v", step.elapsed, i, got, wantLit)
			}
		}
	}

	// Geometry is not assumed to be twelve pixels; the same direction rule
	// keeps a contiguous arc beginning at index 1.
	pixels := countdownRingPixels(8, countdownLitPixels(8, duration, duration/2))
	for i, px := range pixels {
		wantLit := i >= 1 && i <= 4
		if got := px == ringCountdownColor; got != wantLit {
			t.Fatalf("eight-pixel pixel %d lit=%v, want %v", i, got, wantLit)
		}
	}
}

func TestClockTimerCountdownUsesNearestTimerAndNormalCapture(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)
	ring := d.dev.Ring()
	if ring == nil {
		t.Skip("no ring backend")
	}
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	caps := ring.Caps()

	long := app.startTimer(120_000, "Long")
	short := app.startTimer(60_000, "Short")

	waitFor(t, 5*time.Second, "clock countdown ring capture", func() bool {
		sel := d.router.Current("light.ring.main")
		return sel.SelectedAppID == clockAppID && sel.SelectedCaptureID != ""
	})
	assertOrange := func(want int, when string) {
		t.Helper()
		waitFor(t, 2*time.Second, when+": orange frame reaches backend", func() bool {
			pixels := d.dev.(*backend.Simulator).RingSnapshot().Pixels
			if len(pixels) != caps.PixelCount {
				return false
			}
			got := 0
			for i, px := range pixels {
				if px == ringCountdownColor {
					if i == 0 && want < caps.PixelCount {
						return false
					}
					got++
				} else if px != (backend.RGB{}) {
					return false
				}
			}
			return got == want
		})
	}

	assertOrange(caps.PixelCount, "start")

	// Advance half of the nearest (60 s) timer. Using the 120 s timer instead
	// would show nine pixels, so this also proves nearest-timer selection.
	advance(30 * time.Second)
	app.tick()
	assertOrange(6, "half of nearest timer")

	// A normal external capture preempts the countdown, and releasing it puts
	// the clock's countdown back.
	ext := d.pres.Open("light.ring.main", "test.external", "run-external", presentations.Ring)
	if err := d.pres.Update(ext, presentations.RingFrame([]backend.RGB{{R: 1}, {G: 2}, {B: 3}})); err != nil {
		t.Fatal(err)
	}
	cap, err := d.router.Acquire("light.ring.main", "test.external", "run-external", ext.ID, routing.TouchNone)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "external ring preemption", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == "test.external"
	})
	if !d.router.ForceRelease(cap.ID) {
		t.Fatal("external capture was not released")
	}
	waitFor(t, 5*time.Second, "clock countdown resumed", func() bool {
		sel := d.router.Current("light.ring.main")
		return sel.SelectedAppID == clockAppID && sel.SelectedCaptureID != ""
	})
	assertOrange(6, "resumed countdown")
	d.pres.Close(ext)

	// The nearest timer is now the 120 s timer, 90 s before its deadline.
	if _, err := app.cancelTimer(short.TimerId); err != nil {
		t.Fatal(err)
	}
	assertOrange(9, "remaining timer")

	if _, err := app.cancelTimer(long.TimerId); err != nil {
		t.Fatal(err)
	}
	if got := app.state().Timers; len(got) != 0 {
		t.Fatalf("timers after cancel = %+v", got)
	}
	waitFor(t, 5*time.Second, "ring baseline resumed", func() bool {
		sel := d.router.Current("light.ring.main")
		return sel.SelectedAppID == ringAppID && sel.SelectedCaptureID == ""
	})
}

func TestClockAlertAudioRepeatsAndStopsOnDismiss(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	var calls atomic.Int32
	started := make(chan struct{}, 1)
	app.mu.Lock()
	app.playAudio = func(ctx context.Context, _ backend.AudioFormat, _ []byte) error {
		n := calls.Add(1)
		if n == 1 {
			started <- struct{}{}
		}
		<-ctx.Done()
		return ctx.Err()
	}
	app.mu.Unlock()

	app.startTimer(1000, "Tea")
	advance(time.Second)
	app.tick()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("alert audio never started")
	}

	// Repeated ticks must not start another loop or overlap clips.
	for range 5 {
		app.tick()
	}
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("audio calls while ringing = %d, want 1", got)
	}

	if !app.dismissAlert("") {
		t.Fatal("alert was not dismissed")
	}
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("audio restarted after dismiss: %d calls", got)
	}
	app.audioMu.Lock()
	alertID, cancel := app.audioAlertID, app.audioCancel
	app.audioMu.Unlock()
	if alertID != "" || cancel != nil {
		t.Fatalf("audio state after dismiss: alert=%q cancel=%v", alertID, cancel != nil)
	}
}

func TestClockOverlappingAlertsHandOffAudio(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)

	var calls atomic.Int32
	app.mu.Lock()
	app.playAudio = func(ctx context.Context, _ backend.AudioFormat, _ []byte) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}
	app.mu.Unlock()

	first := app.startTimer(1000, "One")
	app.startTimer(2000, "Two")
	advance(time.Second)
	app.tick()
	waitFor(t, 5*time.Second, "first alert audio", func() bool { return calls.Load() == 1 })

	advance(time.Second)
	app.tick()
	state := app.state()
	if len(state.ActiveAlerts) != 2 {
		t.Fatalf("overlapping alerts = %+v", state.ActiveAlerts)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("second alert overlapped the first audio: %d calls", got)
	}

	if !app.dismissAlert(state.ActiveAlerts[0].AlertId) {
		t.Fatal("first alert was not dismissed")
	}
	waitFor(t, 5*time.Second, "second alert audio", func() bool { return calls.Load() == 2 })
	remaining := app.state().ActiveAlerts
	if len(remaining) != 1 || remaining[0].SourceId == first.TimerId {
		t.Fatalf("remaining alert = %+v", remaining)
	}

	if !app.dismissAlert("") {
		t.Fatal("remaining alert was not dismissed")
	}
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("audio restarted after dismissing all alerts: %d calls", got)
	}
}

func TestClockAlertAudioLoopRepeatsUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		alertAudioLoop(ctx, backend.AudioFormat{}, nil, 5*time.Millisecond,
			func(context.Context, backend.AudioFormat, []byte) error {
				calls.Add(1)
				return nil
			})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() < 3 {
		t.Fatal("alert audio loop did not repeat")
	}
	cancel()
	select {
	case <-looped:
	case <-time.After(5 * time.Second):
		t.Fatal("alert audio loop did not stop on cancellation")
	}
	after := calls.Load()
	time.Sleep(30 * time.Millisecond)
	if got := calls.Load(); got != after {
		t.Fatalf("audio loop kept playing after cancellation: %d -> %d", after, got)
	}
}

// A running stopwatch owns the ring with an orange pulse, a single tap
// pauses/resumes it, and the configured gesture (hold by default, double tap
// on request) resets and dismisses it.
func TestClockStopwatchRingPulseAndPrimaryGestures(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	app, advance := testClockTimes(t, d)
	ring := d.dev.Ring()
	if ring == nil {
		t.Skip("no ring backend")
	}
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	caps := ring.Caps()

	waitStopwatch := func(want clockv1.StopwatchStatus, when string) {
		t.Helper()
		got := app.state().Stopwatch.Status
		if got != want {
			t.Fatalf("%s: stopwatch state = %v, want %v", when, got, want)
		}
	}
	waitRing := func(appID, when string) {
		t.Helper()
		waitFor(t, 2*time.Second, when, func() bool {
			return d.router.Current("light.ring.main").SelectedAppID == appID
		})
	}

	app.stopwatchCommand("StopwatchStart")
	waitRing(clockAppID, "running stopwatch captures the ring")
	waitFor(t, 2*time.Second, "orange pulse reaches the backend", func() bool {
		pixels := d.dev.(*backend.Simulator).RingSnapshot().Pixels
		if len(pixels) != caps.PixelCount {
			return false
		}
		for _, px := range pixels {
			// The pulse preset dims the whole ring together; every pixel keeps
			// the warm orange channel ordering.
			if px.R == 0 || px.R < px.G || px.G < px.B {
				return false
			}
		}
		return true
	})

	// A single tap pauses: the ring returns to the selected baseline and the
	// elapsed time is preserved.
	advance(2500 * time.Millisecond)
	if !app.primaryGesture("tap") {
		t.Fatal("tap was not handled by an engaged stopwatch")
	}
	waitStopwatch(clockv1.StopwatchStatus_STOPWATCH_STATUS_PAUSED, "tap pause")
	if got := app.state().Stopwatch.ElapsedMs; got != 2500 {
		t.Fatalf("paused elapsed = %d, want 2500", got)
	}
	waitRing(ringAppID, "paused stopwatch releases the ring")

	// Another tap resumes and the pulse returns.
	if !app.primaryGesture("tap") {
		t.Fatal("resume tap was not handled")
	}
	waitStopwatch(clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING, "tap resume")
	waitRing(clockAppID, "resumed stopwatch recaptures the ring")

	// The default reset gesture is a hold, and it does not need the assistant.
	if !app.primaryGesture("hold") {
		t.Fatal("configured hold did not reset the stopwatch")
	}
	waitStopwatch(clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE, "hold reset")
	if app.stopwatchEngaged() {
		t.Fatal("reset stopwatch still owns the primary key")
	}
	waitRing(ringAppID, "reset stopwatch releases the ring")

	// Selecting double tap moves the reset gesture; a hold then has no effect
	// but is still consumed by the framework, and two taps reset.
	app.applySettings(0, []settings.AppSettingView{{
		Key: "stopwatch_reset_gesture", Value: `"double_tap"`, HasValue: true,
	}})
	app.stopwatchCommand("StopwatchStart")
	advance(time.Second)
	if app.primaryGesture("hold") {
		t.Fatal("hold reset while the configured gesture is double tap")
	}
	waitStopwatch(clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING, "hold ignored")
	if !app.primaryGesture("double_tap") {
		t.Fatal("configured double tap did not reset the stopwatch")
	}
	waitStopwatch(clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE, "double tap reset")
}
