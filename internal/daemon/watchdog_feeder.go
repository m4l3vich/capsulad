package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// WatchdogFeederArg is the hidden capsulad subcommand that keeps a handed-off
// hardware timer alive. The device descriptor is inherited as fd 3.
const WatchdogFeederArg = "__capsula_watchdog_feeder"

const (
	feederPIDFile  = "watchdog.feeder"
	feederHandback = "watchdog.handback"
)

// RunWatchdogFeeder keeps the inherited watchdog descriptor fed until a running
// daemon asks for it back (or the machine goes down). It exists because the
// RK3308 stock kernel ignores both the magic close and WDIOC_SETOPTIONS: once
// the timer is armed it fires unless somebody keeps writing keepalives, so a
// graceful `capsulad stop` must leave another process feeding it.
func RunWatchdogFeeder() int {
	dir := os.Getenv("CAPSULA_WATCHDOG_DIR")
	if dir == "" {
		return 2
	}
	f := os.NewFile(3, "watchdog")
	if f == nil {
		return 2
	}
	defer f.Close()
	pidFile := filepath.Join(dir, feederPIDFile)
	handback := filepath.Join(dir, feederHandback)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return 2
	}
	last := time.Time{}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		if _, err := os.Stat(handback); err == nil {
			_ = os.Remove(pidFile)
			return 0
		}
		if time.Since(last) >= 2*time.Second {
			if _, err := f.Write([]byte{0}); err != nil {
				_ = os.Remove(pidFile)
				return 1
			}
			last = time.Now()
		}
	}
	return 0
}

// handOffWatchdog starts a detached feeder holding the armed descriptor. It
// returns false when the timer cannot be handed over, in which case the caller
// must keep feeding or the board will reset.
func handOffWatchdog(f *os.File, runDir string) error {
	if runDir == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	// A stale handback marker would make the feeder exit immediately.
	_ = os.Remove(filepath.Join(runDir, feederHandback))
	// Prefer /proc/self/exe: it resolves the running inode even when the
	// binary was replaced or deleted by an upgrade, which would otherwise make
	// the handoff fail and reset the board on a graceful stop.
	exe := "/proc/self/exe"
	if _, err := os.Stat(exe); err != nil {
		fallback, ferr := os.Executable()
		if ferr != nil {
			return ferr
		}
		exe = fallback
	}
	cmd := exec.Command(exe, WatchdogFeederArg)
	cmd.Env = append(os.Environ(), "CAPSULA_WATCHDOG_DIR="+runDir)
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// requestFeederHandback drops the marker a feeder watches for. It is a no-op
// when no feeder is registered.
func requestFeederHandback(runDir string) {
	if runDir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(runDir, feederPIDFile)); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(runDir, feederHandback), []byte("handback"), 0o644)
}

// reclaimWatchdog asks a feeder left by a previous daemon to release the device
// so this process can arm it again.
func reclaimWatchdog(runDir string) {
	if runDir == "" {
		return
	}
	requestFeederHandback(runDir)
	pidFile := filepath.Join(runDir, feederPIDFile)
	if _, err := os.Stat(pidFile); err != nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
