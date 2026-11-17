package daemon

import (
	"context"
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

// These tests pin the supervisor's release ordering with channels, not sleeps:
// open, feed and release all happen on the supervisor goroutine, and the only
// way out of it is disarm (idempotent and blocking, see releaseOnce) plus Wait.

// gatedWatchdog blocks inside Detach so a release can be observed while it runs.
type gatedWatchdog struct {
	*fakeWatchdog
	detachEntered chan struct{}
	detachGate    chan struct{}
	once          sync.Once
}

func (g *gatedWatchdog) Detach() *os.File {
	g.once.Do(func() { close(g.detachEntered) })
	<-g.detachGate
	return g.fakeWatchdog.Detach()
}

// gatedFeedWatchdog blocks inside Feed, so a release can land while a keepalive
// is in flight.
type gatedFeedWatchdog struct {
	*fakeWatchdog
	feedEntered chan struct{}
	feedGate    chan struct{}
	returned    chan struct{}
	once        sync.Once
}

func (g *gatedFeedWatchdog) Feed() error {
	g.once.Do(func() { close(g.feedEntered) })
	<-g.feedGate
	err := g.fakeWatchdog.Feed()
	close(g.returned)
	return err
}

// newGatedSupervisor builds an enabled supervisor over a temp device node whose
// opener returns wd.
func newGatedSupervisor(t *testing.T, wd backend.Watchdog) (*watchdogSupervisor, string) {
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
	s := newWatchdogSupervisor(runDir, logs, "auto", profile.Watchdog{Path: dev, DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil }, newLiveness())
	if !s.on {
		t.Fatalf("supervisor is disabled: %s", s.State().detail)
	}
	return s, runDir
}

// A graceful stop must be a barrier: while one release is running, every other
// caller blocks until it finished, so a process exiting from shutdown cannot
// outrun the feeder the board needs.
func TestWatchdogDisarmIsABarrier(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	gated := &gatedWatchdog{
		fakeWatchdog:  &fakeWatchdog{timeout: time.Minute, detach: w},
		detachEntered: make(chan struct{}),
		detachGate:    make(chan struct{}),
	}
	s, runDir := newGatedSupervisor(t, gated)
	s.tryOpen()

	first := make(chan struct{})
	go func() {
		s.disarm()
		close(first)
	}()
	<-gated.detachEntered

	second := make(chan struct{})
	go func() {
		s.disarm()
		close(second)
	}()
	select {
	case <-second:
		t.Fatal("a second disarm returned while the first release was still running")
	default:
	}

	// Letting the handoff finish must release both callers and leave a feeder
	// holding the descriptor.
	close(gated.detachGate)
	<-first
	<-second
	waitFor(t, 5*time.Second, "the feeder pid file", func() bool { return feederRegistered(runDir) })
}

// An open that returns after the supervisor was released must hand the fresh
// descriptor over instead of storing an armed, unfed timer, and Wait must cover
// that work so the process cannot exit in the middle of it.
func TestWatchdogOpenResultAfterRetirementIsHandedOff(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	wd := &fakeWatchdog{timeout: time.Minute, detach: w}
	runDir := t.TempDir()
	dev := filepath.Join(runDir, "watchdog.device")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := logbus.New(ids.NewClock(), 200, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	s := newWatchdogSupervisor(runDir, logs, "auto", profile.Watchdog{Path: dev, DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) {
			once.Do(func() { close(entered) })
			<-gate
			return wd, nil
		}, newLiveness())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	<-entered // the loop is inside open, before the descriptor is stored
	cancel()
	s.disarm() // retire while that open is still in flight

	waited := make(chan struct{})
	go func() {
		s.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned while an open was still in flight")
	default:
	}

	close(gate) // the open now produces an armed timer for a retired supervisor
	waitFor(t, 5*time.Second, "the racing open to be handed off", func() bool { return feederRegistered(runDir) })
	<-waited
	if s.State().armed {
		t.Fatal("a released supervisor armed a timer")
	}
	if wd.wasClosed() {
		t.Fatal("the racing descriptor was closed instead of being handed off")
	}
}

// A keepalive that fails because a release took the descriptor away is the
// release, not breakage: it must not latch the supervisor or log an error.
func TestWatchdogKeepaliveFailureDuringReleaseIsNotBreakage(t *testing.T) {
	gated := &gatedFeedWatchdog{
		fakeWatchdog: &fakeWatchdog{timeout: time.Minute},
		feedEntered:  make(chan struct{}),
		feedGate:     make(chan struct{}),
		returned:     make(chan struct{}),
	}
	s, _ := newGatedSupervisor(t, gated)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	<-gated.feedEntered // the arming keepalive is blocked inside Feed
	s.disarm()          // releases the descriptor underneath it
	close(gated.feedGate)
	<-gated.returned // Feed came back with os.ErrClosed

	st := s.State()
	if !st.healthy {
		t.Fatalf("a release during a keepalive was reported as breakage: %+v", st)
	}
	if !strings.Contains(st.detail, "disarmed") {
		t.Fatalf("detail = %q, want the release outcome", st.detail)
	}
}

// While an armed timer's derived cadence is shorter than the probe interval the
// loop must tick at the cadence, otherwise the keepalive would be rounded up to
// the probe grid and a short timer could miss its deadline.
func TestWatchdogTickFollowsTheDerivedCadence(t *testing.T) {
	short := &fakeWatchdog{timeout: 300 * time.Millisecond}
	s, _, _ := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return short, nil })
	if got := s.tickInterval(); got != watchdogProbeInterval {
		t.Fatalf("tickInterval before arming = %s, want the probe interval", got)
	}
	s.tryOpen()
	cadence, _ := watchdogTiming(short.timeout, time.Minute)
	if cadence >= watchdogProbeInterval {
		t.Fatalf("cadence %s is not below the probe interval; the test would not exercise the grid", cadence)
	}
	if got := s.tickInterval(); got != cadence {
		t.Fatalf("tickInterval while armed = %s, want the derived cadence %s", got, cadence)
	}

	// Behavioural half: at a 100 ms cadence a healthy daemon collects five
	// keepalives in well under a second, while the old 500 ms grid needs more
	// than two, so this bound rejects the quantized scheduling.
	wd := &fakeWatchdog{timeout: 300 * time.Millisecond}
	s2, live, _ := newTestSupervisor(t, profile.Watchdog{DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil })
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s2.Start(ctx)
	waitFor(t, 2*time.Second, "the short timer to be fed at its own cadence", func() bool {
		return wd.count() >= 5
	})
}

// A supervisor that never starts must not make shutdown wait on it.
func TestWatchdogWaitReturnsForADisabledSupervisor(t *testing.T) {
	logs, err := logbus.New(ids.NewClock(), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	s := newWatchdogSupervisor(t.TempDir(), logs, "off", profile.Watchdog{}, nil, newLiveness())
	if s.on {
		t.Fatal("supervisor is enabled with -watchdog off")
	}
	s.Start(context.Background())
	// Returns immediately, or this test hangs: nothing was started to wait for.
	s.Wait()
}

// The failure branch of a release needs a live supervisor to mean anything, so
// it runs with a real device path, an enabled supervisor, and only the handoff
// unpublishable (no run directory).
func TestWatchdogReportsAHandoffFailureOnALiveSupervisor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	wd := &fakeWatchdog{timeout: time.Minute, detach: w}
	dev := filepath.Join(t.TempDir(), "watchdog.device")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := logbus.New(ids.NewClock(), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	s := newWatchdogSupervisor("", logs, "auto", profile.Watchdog{Path: dev, DefaultTimeout: time.Minute},
		func(string) (backend.Watchdog, error) { return wd, nil }, newLiveness())
	if !s.on {
		t.Fatalf("supervisor is disabled: %s", s.State().detail)
	}
	s.tryOpen()
	if !s.State().armed {
		t.Fatalf("supervisor did not arm: %s", s.State().detail)
	}
	s.disarm()

	st := s.State()
	if st.armed || st.healthy {
		t.Fatalf("state = %+v, want a released timer reported honestly", st)
	}
	for _, want := range []string{"handoff failed", "will reset"} {
		if !strings.Contains(st.detail, want) {
			t.Fatalf("detail = %q, want it to mention %q", st.detail, want)
		}
	}
}
