package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/backend/profile"
	"capsulad/internal/logbus"
)

// watchdogProbeInterval is how often the supervisor looks at the device and at
// the daemon's liveness. It is also the lock-out retry cadence while the vendor
// stack (or a feeder) still owns the device.
const watchdogProbeInterval = 500 * time.Millisecond

// watchdogFeedCeiling bounds the keepalive interval so a driver that reports an
// implausibly long armed timeout is still fed often enough.
const watchdogFeedCeiling = 2 * time.Second

// watchdogLivenessCeiling bounds the liveness window for a long timer: a slow
// render step or a long GC pause must not reset a working speaker just because
// the driver armed an hour-long timeout.
const watchdogLivenessCeiling = 10 * time.Second

// watchdogAssumedTimeout is the last-resort timer interval when the driver
// reports none and the profile declares none. With the caps below it yields a
// 2 s keepalive cadence and a 10 s gate, which is safe for any real watchdog
// timeout above a couple of seconds.
const watchdogAssumedTimeout = 30 * time.Second

// watchdogTiming derives the keepalive cadence and the liveness window from the
// interval the driver actually reports (WDIOC_GETTIMEOUT), with the profile's
// declared interval used only when the driver answers nothing. Both derived
// values stay strictly inside the armed timeout, which is the invariant that
// makes the gate meaningful: a window that reached or passed the timeout could
// not be what resets the board, and a cadence at the timeout would trip the
// hardware on a healthy daemon. The loop then schedules itself at the cadence,
// so the effective period is the cadence and not the probe grid; the one
// assumption left is that the loop can run at least as often as the cadence,
// which holds for any real watchdog (the measured boards report 10-30 s) and is
// why a sub-millisecond interval is not a case this code pretends to hold.
// Do not turn either value into a constant.
func watchdogTiming(reported, declared time.Duration) (cadence, window time.Duration) {
	t := reported
	if t <= 0 {
		t = declared
	}
	if t <= 0 {
		t = watchdogAssumedTimeout
	}
	cadence = t / 3
	if cadence > watchdogFeedCeiling {
		cadence = watchdogFeedCeiling
	}
	window = t / 2
	if window > watchdogLivenessCeiling {
		window = watchdogLivenessCeiling
	}
	return cadence, window
}

// watchdogPath parses the -watchdog value. "auto" means the profile's
// device path when it has one, "off" disables the supervisor entirely,
// anything else is a device path.
func watchdogPath(setting, defaultPath string) (path string, enabled bool) {
	switch strings.TrimSpace(setting) {
	case "", "auto":
		if defaultPath == "" {
			return "", false
		}
		return defaultPath, true
	case "off", "none", "disabled":
		return "", false
	default:
		return setting, true
	}
}

// liveness is the daemon's progress gauge. A loop that is not dead but has
// stopped completing steps is exactly the failure a hardware watchdog exists
// for, so the supervisor gates feeding on progress, not on the goroutine
// existing.
type liveness struct {
	mu   sync.Mutex
	last time.Time
}

func newLiveness() *liveness { return &liveness{last: time.Now()} }

// beat records that one step of the core runtime completed.
func (l *liveness) beat() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
}

// stale reports whether no step has completed within window.
func (l *liveness) stale(window time.Duration) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.last) > window
}

// watchdogState is the externally reported condition of the hardware timer.
type watchdogState struct {
	path    string
	enabled bool
	// armed reports that this daemon currently feeds an armed hardware timer.
	// It is false before the first successful open, after the timer is handed
	// off, and after a release that failed.
	armed bool
	// healthy is false once the supervisor stopped feeding because the daemon
	// broke its liveness contract, and after a release that could not stop the
	// timer. In that case the detail says the board resets.
	healthy bool
	timeout time.Duration
	detail  string
}

// watchdogSupervisor takes the hardware liveness timer over from the stock
// voice-agent. The vendor process owns /dev/watchdog; when it dies the SoC
// resets unless somebody else keeps feeding the timer. The supervisor retries
// the open (a single-owner device stays busy until the vendor releases it),
// then feeds at a third of the armed interval (capped at 2 s) for as long as the
// daemon keeps making progress, and gates on a stall window derived from that
// same interval.
type watchdogSupervisor struct {
	logs     *logbus.Bus
	path     string
	runDir   string
	on       bool
	nowayout bool
	open     func(path string) (backend.Watchdog, error)
	live     *liveness
	// declared is the profile's fallback interval, used only when the driver
	// reports no timeout of its own.
	declared time.Duration

	mu    sync.Mutex
	wd    backend.Watchdog
	state watchdogState
	// cadence and window are derived per arm from the reported timeout by
	// watchdogTiming, so both stay inside the timer the hardware actually runs.
	cadence time.Duration
	window  time.Duration
	// stopped latches the supervisor off after the daemon broke its liveness
	// contract or a keepalive failed: the descriptor stays open (this process
	// stays the device's owner) and no keepalive is ever written again.
	stopped bool
	// retired is set when the timer is released, so the loop never opens the
	// device again (re-opening after a handoff would race the feeder we just
	// spawned for ownership).
	retired bool
	// releaseOnce makes disarm both idempotent and a barrier: callers after the
	// first block until the release finished, which is what stops shutdown from
	// exiting ahead of the feeder it needs.
	releaseOnce sync.Once
	// done is closed when the supervisor goroutine has finished. See Wait.
	done chan struct{}
}

func newWatchdogSupervisor(runDir string, logs *logbus.Bus, setting string, wd profile.Watchdog, open func(string) (backend.Watchdog, error), live *liveness) *watchdogSupervisor {
	path, enabled := watchdogPath(setting, wd.Path)
	if open == nil {
		enabled = false
	}
	s := &watchdogSupervisor{
		logs: logs, path: path, on: enabled, runDir: runDir, open: open,
		nowayout: wd.Nowayout, live: live, declared: wd.DefaultTimeout,
		done: make(chan struct{}),
	}
	// Until the device is armed there is no reported timeout; the gate uses the
	// ceiling so an unarmed supervisor can never trip it.
	s.cadence, s.window = watchdogFeedCeiling, watchdogLivenessCeiling
	s.state = watchdogState{path: path, enabled: enabled, healthy: enabled}
	if !enabled {
		s.state.detail = "disabled by configuration"
		return s
	}
	// A missing device node is not permanent: devtmpfs, a module load or a
	// transient permission problem can resolve later, so the retry loop stays
	// armed and the state only says why the timer is not fed yet.
	if _, err := os.Stat(path); err != nil {
		s.state.detail = "no watchdog device yet: " + err.Error()
		s.state.healthy = false
	}
	return s
}

// Start begins the retry/feed loop. It never blocks the caller: the feeder
// handback wait runs on the loop goroutine too. The loop stops on ctx
// cancellation, releasing the timer on the way out, and closes done so Wait can
// prove the process is not exiting ahead of an in-flight release.
func (s *watchdogSupervisor) Start(ctx context.Context) {
	if !s.on {
		// Nothing will run, so nothing may wait for it either.
		close(s.done)
		return
	}
	go func() {
		defer close(s.done)
		reclaimWatchdog(ctx, s.runDir)
		s.run(ctx)
	}()
}

// Wait blocks until the supervisor goroutine has finished, including any open or
// release it had in flight. Shutdown calls it after disarm so the process cannot
// exit between arming a timer and giving that timer an owner. It is deliberately
// unbounded: a wedged handoff turns a graceful stop into the boot service's
// SIGTERM-then-SIGKILL escalation, which is the accepted price of never letting
// exit(0) leave an armed timer with nobody feeding it.
func (s *watchdogSupervisor) Wait() { <-s.done }

func (s *watchdogSupervisor) run(ctx context.Context) {
	var lastFeed time.Time
	// A per-iteration timer, not a fixed ticker: while armed the tick has to
	// follow the cadence derived from the armed timeout, or a short timer's
	// keepalive would be rounded up to the probe grid and could miss its
	// deadline.
	timer := time.NewTimer(watchdogProbeInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			s.disarm()
			return
		case <-timer.C:
		}
		s.mu.Lock()
		wd, stopped, retired := s.wd, s.stopped, s.retired
		cadence, window := s.cadence, s.window
		s.mu.Unlock()
		if !stopped && !retired {
			switch {
			case wd == nil:
				s.tryOpen()
			case s.live.stale(window):
				s.stopFeeding(fmt.Sprintf("no core progress for %s; the hardware timer is left to reset the board", window))
			case time.Since(lastFeed) >= cadence:
				if err := wd.Feed(); err != nil {
					s.stopFeeding("keepalive failed: " + err.Error())
				} else {
					lastFeed = time.Now()
				}
			}
		}
		timer.Reset(s.tickInterval())
	}
}

// tickInterval is how long the loop waits before looking at the device again.
// While an armed timer is being fed the tick follows the derived cadence, so the
// effective keepalive period is the cadence itself; otherwise the probe
// interval is the retry cadence.
func (s *watchdogSupervisor) tickInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wd == nil || s.stopped || s.retired {
		return watchdogProbeInterval
	}
	if s.cadence < watchdogProbeInterval {
		return s.cadence
	}
	return watchdogProbeInterval
}

func (s *watchdogSupervisor) tryOpen() {
	s.mu.Lock()
	stopped, retired := s.stopped, s.retired
	s.mu.Unlock()
	if stopped || retired {
		return
	}
	wd, err := s.open(s.path)
	if err != nil {
		s.mu.Lock()
		stopped, retired = s.stopped, s.retired
		s.mu.Unlock()
		switch {
		case stopped || retired:
			// The supervisor was released while this open was in flight; the
			// attempt is not a state transition any more.
			return
		case errors.Is(err, backend.ErrWatchdogBusy):
			// A feeder left by a previous daemon keeps feeding the timer; ask it
			// to release so this supervisor can own the device. This also covers
			// the case where the old daemon stopped after this one started.
			requestFeederHandback(s.runDir)
			s.setDetail("not armed; the device is held by another process, retrying (" + err.Error() + ")")
		case errors.Is(err, os.ErrNotExist):
			s.setDetail("no watchdog device yet; retrying (" + err.Error() + ")")
		default:
			s.setDetail("not armed; retrying (" + err.Error() + ")")
		}
		return
	}
	// The handback marker only exists to make a previous feeder exit; keeping
	// it would make the next handoff look stale.
	if s.runDir != "" {
		_ = os.Remove(filepath.Join(s.runDir, feederHandback))
	}
	// Derive the cadence and the gate from the interval the driver actually
	// reports, so a unit that arms a shorter timer than the profile assumed is
	// fed faster and gated sooner instead of being reset while healthy.
	reported := wd.Timeout()
	cadence, window := watchdogTiming(reported, s.declared)
	effective := reported
	if effective <= 0 {
		effective = s.declared
	}
	if effective <= 0 {
		effective = watchdogAssumedTimeout
	}
	detail := "armed; the framework feeds the hardware timer while the daemon makes progress"
	if reported <= 0 {
		detail = fmt.Sprintf("armed; the driver reported no timeout, using %s (keepalive %s, stall window %s)", effective, cadence, window)
	}
	s.mu.Lock()
	if s.stopped || s.retired {
		// The supervisor was released while this open was in flight. The
		// descriptor is now the only owner of an armed timer, so it must be
		// handed off (or closed) exactly as a planned stop would: leaving it
		// stored but unfed would reset the board.
		s.mu.Unlock()
		s.releaseDescriptor(wd)
		return
	}
	s.wd = wd
	s.cadence, s.window = cadence, window
	s.state.armed = true
	s.state.healthy = true
	s.state.timeout = effective
	s.state.detail = detail
	s.mu.Unlock()
	s.logs.Infof("watchdog", "", "%s armed, timeout=%s (keepalive %s, stall window %s)", s.path, effective, cadence, window)
	// Opening the device armed the timer, so hold it right away instead of
	// waiting for the first loop tick: a short timer must never expire between
	// the open and the first scheduled keepalive.
	if err := wd.Feed(); err != nil {
		s.stopFeeding("keepalive failed immediately after arming: " + err.Error())
	}
}

// stopFeeding latches the supervisor off after the daemon broke the liveness
// contract or the keepalive failed. It deliberately does not hand the
// descriptor to a feeder: masking a hung daemon with a feeder would keep a
// broken speaker powered on. The timer runs out and the SoC resets.
func (s *watchdogSupervisor) stopFeeding(detail string) {
	s.mu.Lock()
	if s.stopped || s.retired {
		// A concurrent release closed the descriptor: the keepalive failure is
		// that release, not breakage, and must not be reported as one.
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.state.healthy = false
	s.state.detail = detail
	s.mu.Unlock()
	s.logs.Errorf("watchdog", "", "%s: %s", s.path, detail)
}

// disarm releases the timer and is the barrier before the process may exit.
// sync.Once is what makes that true: the first caller performs the release,
// every other caller blocks until that release has finished, so shutdown cannot
// return while a feeder it needs is still being started. The policy is
// conservative on purpose: when the platform exposes the descriptor it is
// always handed to a detached feeder, because a close is only proven to stop the
// timer on a driver measured to honour the magic close, and `Nowayout` records
// that as a fact only where it was measured (the RK3308 stock kernel). A stop
// after the supervisor already stopped feeding is not rescued: the reset is the
// point.
func (s *watchdogSupervisor) disarm() { s.releaseOnce.Do(s.release) }

// release performs the release exactly once, through disarm.
func (s *watchdogSupervisor) release() {
	s.mu.Lock()
	if s.stopped || s.retired {
		// Keep the descriptor referenced (and the timer armed) instead of
		// handing a broken daemon's timer to a feeder.
		s.mu.Unlock()
		return
	}
	s.retired = true
	wd := s.wd
	s.wd = nil
	if wd == nil {
		s.mu.Unlock()
		return
	}
	s.state.armed = false
	s.mu.Unlock()
	s.releaseDescriptor(wd)
}

// releaseDescriptor hands a descriptor to a feeder, or closes it when the
// platform exposes nothing to transfer, and reports the outcome as it is.
func (s *watchdogSupervisor) releaseDescriptor(wd backend.Watchdog) {
	if f := wd.Detach(); f != nil {
		err := handOffWatchdog(f, s.runDir, feederWindow)
		// The child holds its own descriptor; the parent's copy must not
		// outlive it, and it must be closed even when the handoff failed.
		_ = f.Close()
		if err != nil {
			s.failDetail(fmt.Sprintf("%s: handoff failed (%v); the timer is still armed and the board will reset", s.path, err))
			return
		}
		s.setDetail("handed to the watchdog feeder; the board stays up for the deployment window")
		s.logs.Infof("watchdog", "", "%s handed to the feeder for %s", s.path, feederWindow)
		return
	}

	// No descriptor to transfer: a close is the only release left, and its
	// outcome is reported as it is rather than assumed.
	err := wd.Close()
	switch {
	case err != nil:
		s.failDetail(fmt.Sprintf("%s: close failed (%v); the timer may still be armed and the board may reset", s.path, err))
	case s.nowayout:
		s.failDetail(fmt.Sprintf("%s: closed, but this driver ignores the magic close; the timer is still armed and the board will reset", s.path))
	default:
		s.setDetail("closed; the hardware timer is disarmed")
	}
}

// setDetail records a state detail and logs the transition once per distinct
// message instead of on every retry.
func (s *watchdogSupervisor) setDetail(detail string) {
	s.mu.Lock()
	changed := s.state.detail != detail
	s.state.detail = detail
	s.mu.Unlock()
	if changed {
		s.logs.Infof("watchdog", "", "%s", detail)
	}
}

// failDetail records a release that did not stop the timer. The timer is no
// longer fed by this daemon, so the state must never claim it was disarmed:
// healthy goes false and the detail says what happens to the board.
func (s *watchdogSupervisor) failDetail(detail string) {
	s.mu.Lock()
	changed := s.state.detail != detail
	s.state.detail = detail
	s.state.healthy = false
	s.mu.Unlock()
	if changed {
		s.logs.Warnf("watchdog", "", "%s", detail)
	}
}

func (s *watchdogSupervisor) State() watchdogState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
