package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/backend/profile"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// fakeWatchdog is a hardware timer the test drives. It counts keepalives and
// records how the supervisor let go of it, which is how these tests state the
// consumer-visible outcome: a fed timer keeps the board up, a silent one resets
// it.
type fakeWatchdog struct {
	mu       sync.Mutex
	timeout  time.Duration
	feeds    int
	closed   bool
	closeErr error
	detach   *os.File
	feedErr  error
}

func (f *fakeWatchdog) Timeout() time.Duration { return f.timeout }

func (f *fakeWatchdog) Feed() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return os.ErrClosed
	}
	if f.feedErr != nil {
		return f.feedErr
	}
	f.feeds++
	return nil
}

func (f *fakeWatchdog) Detach() *os.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.detach
	f.detach = nil
	return out
}

func (f *fakeWatchdog) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.closeErr
}

func (f *fakeWatchdog) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.feeds
}

func (f *fakeWatchdog) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// staleLast ages the progress gauge so the supervisor reads a hung daemon.
func staleLast(l *liveness) {
	l.mu.Lock()
	l.last = time.Now().Add(-time.Hour)
	l.mu.Unlock()
}

// newTestSupervisor builds a supervisor over a temp device node. The node only
// has to exist: the opened handle is the test's own fake timer.
func newTestSupervisor(t *testing.T, wd profile.Watchdog, open func(string) (backend.Watchdog, error)) (*watchdogSupervisor, *liveness, string) {
	t.Helper()
	runDir := t.TempDir()
	dev := filepath.Join(runDir, "watchdog.device")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := logbus.New(ids.NewClock(), 200, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	live := newLiveness()
	wd.Path = dev
	s := newWatchdogSupervisor(runDir, logs, "auto", wd, open, live)
	if !s.on {
		t.Fatalf("supervisor is disabled: %s", s.State().detail)
	}
	return s, live, runDir
}

// A healthy daemon keeps the timer fed: the board stays up.
func TestWatchdogFeedsWhileTheDaemonMakesProgress(t *testing.T) {
	wd := &fakeWatchdog{timeout: 300 * time.Millisecond}
	s, live, _ := newTestSupervisor(t, profile.Watchdog{Nowayout: true, DefaultTimeout: 300 * time.Millisecond},
		func(string) (backend.Watchdog, error) { return wd, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// Stand in for the output loop: a step every 10 ms.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				live.beat()
			}
		}
	}()

	waitFor(t, 5*time.Second, "the first keepalive", func() bool { return wd.count() >= 1 })
	waitFor(t, 5*time.Second, "repeated keepalives", func() bool { return wd.count() >= 3 })
	st := s.State()
	if !st.armed || !st.healthy {
		t.Fatalf("state = %+v, want an armed, healthy timer", st)
	}
}

// The gate and the keepalive cadence are derived from the armed timeout, never
// fixed: both must stay strictly inside the timer the hardware actually runs,
// or the gate could not be what resets the board and the cadence could trip a
// healthy daemon.
func TestWatchdogTimingStaysInsideTheArmedTimeout(t *testing.T) {
	cases := []struct {
		reported, declared time.Duration
		wantCadence        time.Duration
		wantWindow         time.Duration
	}{
		{21 * time.Second, 30 * time.Second, 2 * time.Second, 10 * time.Second},
		{30 * time.Second, 0, 2 * time.Second, 10 * time.Second},
		{10 * time.Second, 30 * time.Second, 2 * time.Second, 5 * time.Second},
		{6 * time.Second, 0, 2 * time.Second, 3 * time.Second},
		{4 * time.Second, 0, 4 * time.Second / 3, 2 * time.Second},
		{3 * time.Second, 0, time.Second, 1500 * time.Millisecond},
		{2 * time.Second, 0, 2 * time.Second / 3, time.Second},
		{time.Hour, 0, 2 * time.Second, 10 * time.Second},
		// The driver answered nothing: the declared interval, then the assumed
		// one, must still produce an inside-the-timer pair.
		{0, 21 * time.Second, 2 * time.Second, 10 * time.Second},
		{0, 0, 2 * time.Second, 10 * time.Second},
	}
	for _, tc := range cases {
		cadence, window := watchdogTiming(tc.reported, tc.declared)
		if cadence != tc.wantCadence || window != tc.wantWindow {
			t.Errorf("watchdogTiming(%s, %s) = %s, %s; want %s, %s",
				tc.reported, tc.declared, cadence, window, tc.wantCadence, tc.wantWindow)
		}
		effective := tc.reported
		if effective <= 0 {
			effective = tc.declared
		}
		if effective <= 0 {
			effective = watchdogAssumedTimeout
		}
		if cadence <= 0 || cadence >= effective {
			t.Errorf("cadence %s is not inside the armed timeout %s", cadence, effective)
		}
		if window <= 0 || window >= effective {
			t.Errorf("window %s is not inside the armed timeout %s", window, effective)
		}
	}
}

// A short timer must be held from the moment the device is armed: opening it
// starts the countdown, so the supervisor feeds before it returns.
func TestWatchdogFeedsImmediatelyOnArming(t *testing.T) {
	wd := &fakeWatchdog{timeout: 10 * time.Second}
	s, _, _ := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: 30 * time.Second},
		func(string) (backend.Watchdog, error) { return wd, nil })
	s.tryOpen()

	if n := wd.count(); n != 1 {
		t.Fatalf("keepalives after arming = %d, want the arming keepalive", n)
	}
	st := s.State()
	if !st.armed || !st.healthy || st.timeout != 10*time.Second {
		t.Fatalf("state = %+v, want an armed 10s timer", st)
	}
}

// A driver that reports no timeout falls back to the profile's declared
// interval, and says so, rather than silently using a guess.
func TestWatchdogReportsAnUnreportedTimeout(t *testing.T) {
	wd := &fakeWatchdog{timeout: 0}
	s, _, _ := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: 21 * time.Second},
		func(string) (backend.Watchdog, error) { return wd, nil })
	s.tryOpen()

	st := s.State()
	if st.timeout != 21*time.Second {
		t.Fatalf("state.timeout = %s, want the declared 21s fallback", st.timeout)
	}
	if !strings.Contains(st.detail, "reported no timeout") {
		t.Fatalf("detail = %q, want it to say the driver reported nothing", st.detail)
	}
}

// A stalled daemon gets at most the keepalive that arming needed and then
// silence; the gate is latched and the armed timer does the rest.
func TestWatchdogStopsFeedingWhenTheDaemonStalls(t *testing.T) {
	wd := &fakeWatchdog{timeout: 300 * time.Millisecond}
	s, live, runDir := newTestSupervisor(t, profile.Watchdog{Nowayout: true, DefaultTimeout: 300 * time.Millisecond},
		func(string) (backend.Watchdog, error) { return wd, nil })
	staleLast(live)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitFor(t, 5*time.Second, "the supervisor to notice the stall", func() bool {
		st := s.State()
		return st.armed && !st.healthy
	})
	// Arming itself needs one keepalive (opening the device starts the timer);
	// everything after it must be silence or the board would never reset.
	if n := wd.count(); n > 1 {
		t.Fatalf("fed a stalled daemon %d times; the board would never reset", n)
	}
	if d := s.State().detail; !strings.Contains(d, "no core progress") {
		t.Fatalf("detail = %q, want the stalled-runtime reason", d)
	}

	// The daemon now shuts down gracefully. Handing the descriptor to a feeder
	// would keep the hung speaker powered on, so the timer must be left to fire.
	s.disarm()
	if feederRegistered(runDir) {
		t.Fatal("a stalled daemon handed its armed timer to a feeder")
	}
	if wd.wasClosed() {
		t.Fatal("the timer was closed; on a nowayout driver it must be left armed to reset the board")
	}
}

// A keepalive that fails is breakage, not a planned stop: the timer must be
// left to reset the board instead of being handed to a feeder.
func TestWatchdogStopsFeedingWhenTheKeepaliveFails(t *testing.T) {
	wd := &fakeWatchdog{timeout: 100 * time.Millisecond, feedErr: errors.New("i2c bus gone")}
	s, _, runDir := newTestSupervisor(t, profile.Watchdog{Nowayout: true, DefaultTimeout: 100 * time.Millisecond},
		func(string) (backend.Watchdog, error) { return wd, nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitFor(t, 5*time.Second, "the supervisor to give up on the device", func() bool {
		st := s.State()
		return st.armed && !st.healthy
	})
	if d := s.State().detail; !strings.Contains(d, "keepalive failed") {
		t.Fatalf("detail = %q, want the keepalive failure", d)
	}
	s.disarm()
	if feederRegistered(runDir) {
		t.Fatal("a failed keepalive handed the timer to a feeder")
	}
}

// A graceful stop under nowayout keeps the board up: the descriptor goes to a
// detached feeder that keeps feeding until the next daemon asks for it back.
func TestWatchdogHandsOffOnGracefulStopUnderNowayout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	wd := &fakeWatchdog{timeout: time.Minute, detach: w}
	s, _, runDir := newTestSupervisor(t, profile.Watchdog{Nowayout: true, DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil })
	s.tryOpen()
	if !s.State().armed {
		t.Fatalf("supervisor did not arm: %s", s.State().detail)
	}

	s.disarm()

	pidFile := filepath.Join(runDir, feederPIDFile)
	waitFor(t, 5*time.Second, "the feeder pid file", func() bool {
		b, err := os.ReadFile(pidFile)
		return err == nil && strings.Contains(string(b), " shell")
	})
	if wd.wasClosed() {
		t.Fatal("a nowayout timer must be handed off, not closed")
	}
	// The feeder holds the armed descriptor, so the board is still fed after
	// the daemon let go (the fake's own Feed calls never touch this pipe).
	buf := make([]byte, 1)
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("keepalive from the handed-off timer: %v", err)
	}

	// The next daemon asks for it back; the feeder exits and drops the pid file.
	requestFeederHandback(runDir)
	waitFor(t, 5*time.Second, "the feeder to release the timer", func() bool {
		_, err := os.Stat(pidFile)
		return os.IsNotExist(err)
	})
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := r.Read(buf); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("waiting for the feeder to exit: %v", err)
		}
	}
}

// The release policy is conservative: a driver whose disarm behaviour is not
// measured is handed off, never trusted to a close. On an unmeasured Amlogic
// stock timer a planned stop must not reset the board.
func TestWatchdogHandsOffAnUnmeasuredTimer(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	wd := &fakeWatchdog{timeout: time.Minute, detach: w}
	s, _, runDir := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil })
	s.tryOpen()
	s.disarm()

	if wd.wasClosed() {
		t.Fatal("an unmeasured timer must be handed off, not closed")
	}
	waitFor(t, 5*time.Second, "the feeder to take the descriptor", func() bool {
		return feederRegistered(runDir)
	})
	if st := s.State(); !st.healthy || !strings.Contains(st.detail, "feeder") {
		t.Fatalf("state = %+v, want a healthy handoff reported", st)
	}
	requestFeederHandback(runDir)
	waitFor(t, 5*time.Second, "the feeder to release the timer", func() bool {
		return !feederRegistered(runDir)
	})
}

// With no descriptor to transfer, a close is all that is left: a driver that
// honours the magic close really is disarmed, and the state says so.
func TestWatchdogClosesWhenNoDescriptorCanBeTransferred(t *testing.T) {
	wd := &fakeWatchdog{timeout: time.Minute}
	s, _, runDir := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil })
	s.tryOpen()
	s.disarm()

	if !wd.wasClosed() {
		t.Fatal("a timer with no transferable descriptor must be closed")
	}
	if feederRegistered(runDir) {
		t.Fatal("a timer with no transferable descriptor must not be handed to a feeder")
	}
	st := s.State()
	if !strings.Contains(st.detail, "disarmed") || !st.healthy {
		t.Fatalf("state = %+v, want a clean disarm reported", st)
	}
}

// A close that leaves the timer armed must not be reported as a disarm: the
// board will reset, and the state has to say so.
func TestWatchdogReportsACloseThatLeavesTheTimerArmed(t *testing.T) {
	wd := &fakeWatchdog{timeout: time.Minute}
	s, _, _ := newTestSupervisor(t, profile.Watchdog{Nowayout: true, DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil })
	s.tryOpen()
	s.disarm()

	st := s.State()
	if st.healthy {
		t.Fatalf("state = %+v, want an unhealthy release", st)
	}
	for _, want := range []string{"still armed", "will reset"} {
		if !strings.Contains(st.detail, want) {
			t.Fatalf("detail = %q, want it to mention %q", st.detail, want)
		}
	}
}

// A failed close is the other way a release can leave the timer running, and it
// must not be masked either.
func TestWatchdogReportsACloseFailure(t *testing.T) {
	wd := &fakeWatchdog{timeout: time.Minute, closeErr: errors.New("io error")}
	s, _, _ := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil })
	s.tryOpen()
	s.disarm()

	st := s.State()
	if st.healthy {
		t.Fatalf("state = %+v, want an unhealthy release", st)
	}
	for _, want := range []string{"close failed", "may still be armed"} {
		if !strings.Contains(st.detail, want) {
			t.Fatalf("detail = %q, want it to mention %q", st.detail, want)
		}
	}
}

// After a release the loop must not open the device again: a re-open can race
// the feeder that was just given the timer.
func TestWatchdogDoesNotReopenAfterDisarm(t *testing.T) {
	wd := &fakeWatchdog{timeout: time.Minute}
	opens := 0
	s, _, _ := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) {
			opens++
			return wd, nil
		})
	s.tryOpen()
	s.disarm()
	s.tryOpen()

	if opens != 1 {
		t.Fatalf("opened the device %d times, want one open before the release", opens)
	}
	if s.State().armed {
		t.Fatal("an open after the release re-armed the supervisor")
	}
}

// A device that appears late must still be taken over: the constructor only
// records why the timer is not fed yet, and the retry loop arms it once the
// node exists.
func TestWatchdogRetriesALateDevice(t *testing.T) {
	wd := &fakeWatchdog{timeout: time.Minute}
	runDir := t.TempDir()
	dev := filepath.Join(runDir, "late-watchdog.device")
	logs, err := logbus.New(ids.NewClock(), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	attempts := 0
	s := newWatchdogSupervisor(runDir, logs, "auto", profile.Watchdog{Path: dev, DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) {
			attempts++
			// The first two attempts run before the node exists, like devtmpfs
			// populating after the daemon starts.
			if attempts <= 2 {
				return nil, &os.PathError{Op: "open", Path: dev, Err: os.ErrNotExist}
			}
			return wd, nil
		}, newLiveness())
	if !s.on {
		t.Fatal("a missing device node disabled the supervisor instead of retrying")
	}
	if d := s.State().detail; !strings.Contains(d, "no watchdog device yet") {
		t.Fatalf("detail = %q, want the missing-node reason", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	waitFor(t, 5*time.Second, "the late device to be taken over", func() bool {
		return s.State().armed
	})
	if !s.State().healthy {
		t.Fatalf("state = %+v, want the late device armed and fed", s.State())
	}
}

// A device held by another process is a retry, not a dead watchdog: the
// supervisor asks the holder to release and reports that it is not armed yet.
func TestWatchdogBusyDeviceIsRetriedAndAskedToRelease(t *testing.T) {
	s, _, runDir := newTestSupervisor(t, profile.Watchdog{Nowayout: true, DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) {
			return nil, fmt.Errorf("open /dev/watchdog: %w", backend.ErrWatchdogBusy)
		})
	if err := os.WriteFile(filepath.Join(runDir, feederPIDFile), []byte("4242 shell\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.tryOpen()

	st := s.State()
	if st.armed {
		t.Fatalf("state = %+v, want an unarmed supervisor while the device is held", st)
	}
	if !strings.Contains(st.detail, "not armed") || !strings.Contains(st.detail, "held by another process") {
		t.Fatalf("detail = %q, want the busy-device reason", st.detail)
	}
	if _, err := os.Stat(filepath.Join(runDir, feederHandback)); err != nil {
		t.Fatalf("the holder was not asked to release the device: %v", err)
	}
}

// A pid file whose feeder is gone must not survive a reboot: run/ is on the
// persistent partition, and a stale file would make the next start wait for a
// handback that can never arrive.
func TestWatchdogCleansAStaleFeederPIDFile(t *testing.T) {
	runDir := t.TempDir()
	pidFile := filepath.Join(runDir, feederPIDFile)
	// Above the largest Linux pid_max (4 194 304), so the pid is free on every
	// platform this runs on.
	const deadPID = 1 << 29
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d shell\n", deadPID)), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	reclaimWatchdog(context.Background(), runDir)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("reclaiming a stale pid file waited %s", elapsed)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("stale feeder pid file survived: %v", err)
	}
}

// A pid file whose pid was reused by an unrelated live process is stale too:
// the supervisor must clear it instead of waiting for a handback, and must not
// mistake the bystander for the device's owner.
func TestWatchdogCleansAReusedFeederPID(t *testing.T) {
	if fi, err := os.Stat("/proc"); err != nil || !fi.IsDir() {
		t.Skip("no procfs on this host")
	}
	runDir := t.TempDir()
	pidFile := filepath.Join(runDir, feederPIDFile)
	// This process is alive but does not run the feeder script.
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d shell\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanStaleFeeder(runDir)
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pid file of a reused pid survived: %v", err)
	}
}

func TestFeederPIDParsing(t *testing.T) {
	runDir := t.TempDir()
	pidFile := filepath.Join(runDir, feederPIDFile)
	cases := []struct {
		body string
		pid  int
		ok   bool
	}{
		{"123 shell\n", 123, true},
		{"77\n", 77, true},
		{"", 0, false},
		{"garbage shell\n", 0, false},
		{"-3 shell\n", 0, false},
	}
	for _, tc := range cases {
		if err := os.WriteFile(pidFile, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		pid, ok := feederPID(runDir)
		if ok != tc.ok || pid != tc.pid {
			t.Errorf("feederPID(%q) = %d, %v; want %d, %v", tc.body, pid, ok, tc.pid, tc.ok)
		}
	}
}

// The feeder window is bounded, and that bound is expressed in feeder ticks:
// a feeder can keep an armed timer alive for a deployment, never forever.
func TestFeederTicksBoundTheWindow(t *testing.T) {
	if got := feederTicks(feederWindow); got != int(feederWindow/feederTick) || got <= 0 {
		t.Fatalf("feederTicks(%s) = %d", feederWindow, got)
	}
	if got := feederTicks(0); got != 1 {
		t.Fatalf("feederTicks(0) = %d, want one tick so a zero window still feeds once", got)
	}
	if feederWindow < 2*time.Minute {
		t.Fatalf("feederWindow = %s, too short for a binary deployment", feederWindow)
	}
}

// The daemon's own output loop is the liveness source. This is the wiring test:
// it fails if the supervisor is gated on a gauge nothing ever beats, which
// would reset every board.
func TestOutputLoopReportsWatchdogLiveness(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	staleLast(d.live)
	waitFor(t, 5*time.Second, "the output loop to report progress", func() bool {
		return !d.live.stale(watchdogLivenessCeiling)
	})
}
