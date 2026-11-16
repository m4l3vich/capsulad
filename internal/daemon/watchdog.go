package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/backend/profile"
	"capsulad/internal/logbus"
)

// watchdogOptions parses the -watchdog value. "auto" means the profile's
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

// watchdogState is the externally reported condition of the hardware timer.
type watchdogState struct {
	path    string
	enabled bool
	armed   bool
	timeout time.Duration
	detail  string
}

// watchdogSupervisor takes the hardware liveness timer over from the stock
// voice-agent. The vendor process owns /dev/watchdog; when it dies the SoC
// resets unless somebody else keeps feeding the timer. The supervisor retries
// the open (an single-owner device stays busy until the vendor releases it),
// then feeds at a third of the armed timeout.
type watchdogSupervisor struct {
	logs   *logbus.Bus
	path   string
	runDir string
	on     bool
	open   func(path string) (backend.Watchdog, error)

	mu    sync.Mutex
	wd    backend.Watchdog
	state watchdogState
}

func newWatchdogSupervisor(runDir string, logs *logbus.Bus, setting string, wd profile.Watchdog, open func(string) (backend.Watchdog, error)) *watchdogSupervisor {
	path, enabled := watchdogPath(setting, wd.Path)
	if open == nil {
		enabled = false
	}
	s := &watchdogSupervisor{logs: logs, path: path, on: enabled, runDir: runDir, open: open}
	s.state = watchdogState{path: path, enabled: enabled}
	if !enabled {
		s.state.detail = "disabled by configuration"
		return s
	}
	if _, err := os.Stat(path); err != nil {
		s.state.detail = "no watchdog device: " + err.Error()
		s.on = false
	}
	return s
}

// Start begins the retry/feed loop. It never blocks and stops on ctx
// cancellation, disarming the timer on the way out.
func (s *watchdogSupervisor) Start(ctx context.Context) {
	if !s.on {
		return
	}
	reclaimWatchdog(s.runDir)
	go func() {
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		var lastFeed time.Time
		for {
			select {
			case <-ctx.Done():
				s.disarm()
				return
			case <-tick.C:
				s.mu.Lock()
				wd := s.wd
				s.mu.Unlock()
				if wd == nil {
					s.tryOpen()
					continue
				}
				every := wd.Timeout() / 3
				if every <= 0 || every > 2*time.Second {
					every = 2 * time.Second
				}
				if time.Since(lastFeed) < every {
					continue
				}
				if err := wd.Feed(); err != nil {
					s.logs.Warnf("watchdog", "", "keepalive failed: %v", err)
					s.disarm()
					continue
				}
				lastFeed = time.Now()
			}
		}
	}()
}

func (s *watchdogSupervisor) tryOpen() {
	wd, err := s.open(s.path)
	if err != nil {
		// A feeder left by a previous daemon keeps feeding forever; ask it to
		// release so this supervisor can own the timer. This also covers the
		// case where the old daemon stopped after this one started.
		requestFeederHandback(s.runDir)
		detail := "held by another process; retrying (" + err.Error() + ")"
		s.mu.Lock()
		changed := s.state.detail != detail
		s.state.detail = detail
		s.mu.Unlock()
		// Log the transition once per distinct message, not every retry.
		if changed {
			s.logs.Infof("watchdog", "", "%s", detail)
		}
		return
	}
	// The handback marker only exists to make a previous feeder exit; keeping
	// it would make the next handoff look stale.
	if s.runDir != "" {
		_ = os.Remove(filepath.Join(s.runDir, feederHandback))
	}
	s.mu.Lock()
	s.wd = wd
	s.state.armed = true
	s.state.timeout = wd.Timeout()
	s.state.detail = "armed; framework feeds the hardware timer"
	s.mu.Unlock()
	s.logs.Infof("watchdog", "", "%s armed, timeout=%s", s.path, wd.Timeout())
}

// disarm releases the timer on a graceful stop. This kernel offers no way to
// stop an armed timer, so the descriptor is handed to a detached feeder; only
// when that fails does the daemon fall back to closing it and letting the SoC
// reset.
func (s *watchdogSupervisor) disarm() {
	s.mu.Lock()
	wd := s.wd
	s.wd = nil
	if wd != nil {
		s.state.armed = false
	}
	s.mu.Unlock()
	if wd == nil {
		return
	}
	if f := wd.Detach(); f != nil {
		if err := handOffWatchdog(f, s.runDir); err == nil {
			s.mu.Lock()
			s.state.detail = "handed to watchdog feeder process; board stays up, timer keeps running"
			s.mu.Unlock()
			_ = f.Close()
			s.logs.Infof("watchdog", "", "%s handed to feeder (armed timer cannot be disarmed on this kernel)", s.path)
			return
		}
		s.logs.Warnf("watchdog", "", "handoff failed; the SoC will reset")
		_ = f.Close()
		return
	}
	_ = wd.Close()
	s.logs.Warnf("watchdog", "", "%s closed with the timer still armed; the SoC will reset", s.path)
}

func (s *watchdogSupervisor) State() watchdogState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
