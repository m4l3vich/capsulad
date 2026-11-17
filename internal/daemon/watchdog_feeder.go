package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	feederPIDFile  = "watchdog.feeder"
	feederHandback = "watchdog.handback"

	// feederMarker is the argv[0] the feeder script runs under. A process
	// holding /dev/watchdog whose command line carries it is ours: it must be
	// asked to hand the timer back, never killed, because killing the last
	// feeder of an armed nowayout timer resets the board.
	feederMarker = "capsulad-watchdog-feeder"

	// legacyFeederMarker is the hidden subcommand of the Go feeder shipped
	// before the shell feeder. It still holds an armed timer the same way, so
	// it must be handed back rather than killed all the same.
	legacyFeederMarker = "__capsulad_watchdog_feeder"

	// feederTick is how often the feeder writes a keepalive.
	feederTick = 2 * time.Second

	// feederWindow bounds how long a handed-off timer is kept alive. The feeder
	// is a stop-gap for a planned restart or a binary deployment, not a second
	// daemon: a board whose framework never comes back must reset instead of
	// running unmanaged forever. It is deliberately longer than a deployment
	// can plausibly take on the 78.5 MiB data partition.
	feederWindow = 5 * time.Minute
)

// feederScript keeps a handed-off hardware timer alive until a running daemon
// asks for it back, or until the bounded window runs out. $1 is the pid file,
// $2 the handback marker, $3 the tick budget; the armed descriptor arrives as
// fd 3. The pid file records "<pid> shell" so a deploy tool can tell this
// feeder apart from the legacy Go one.
var feederScript = fmt.Sprintf(`printf '%%s shell\n' "$$" > "$1" || exit 2
i=0
while :; do
	if [ -e "$2" ]; then rm -f "$1"; exit 0; fi
	if [ "$i" -ge "$3" ]; then rm -f "$1"; exit 3; fi
	if ( printf '\000' >&3 ) 2>/dev/null; then :; else rm -f "$1"; exit 1; fi
	i=$((i+1))
	sleep %d
done
`, int(feederTick/time.Second))

// feederTicks converts a feeder window into sleep iterations. One tick is
// always run, so a zero or negative window still writes one keepalive first.
func feederTicks(window time.Duration) int {
	n := int(window / feederTick)
	if n < 1 {
		n = 1
	}
	return n
}

// handOffWatchdog starts a detached feeder holding the armed descriptor for at
// most window. It returns once the child has started; the child exits when the
// handback marker appears, when the descriptor stops accepting keepalives, or
// when the window runs out. Exiting without a new owner means the board resets.
//
// The RK3308 stock kernel ignores both the magic close and WDIOC_SETOPTIONS:
// once the timer is armed it fires unless somebody keeps writing keepalives, so
// a graceful `capsulad stop` must leave another process feeding it.
//
// That feeder is a BusyBox-compatible shell, deliberately not a capsulad
// subcommand. An executable image stays mapped for the lifetime of any process
// running it, so a Go feeder would pin the old daemon inode in /data after the
// daemon exits: on a 78.5 MB partition that leaves no room to stage a 40 MB
// replacement while the watchdog is still being fed. A shell holds only the
// inherited descriptor, so the binary can be replaced in place and the timer
// still never misses a keepalive.
func handOffWatchdog(f *os.File, runDir string, window time.Duration) error {
	if runDir == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	// A stale handback marker would make the feeder exit immediately, and a
	// stale pid file would make the next start wait for a feeder that is gone.
	_ = os.Remove(filepath.Join(runDir, feederHandback))
	cleanStaleFeeder(runDir)
	cmd := exec.Command("/bin/sh", "-c", feederScript,
		feederMarker,
		filepath.Join(runDir, feederPIDFile),
		filepath.Join(runDir, feederHandback),
		strconv.Itoa(feederTicks(window)))
	// ExtraFiles starts at fd 3, which is what the script writes to.
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// A service script that holds the daemon's stdout open (S99btctl.sh) must
	// see EOF when the daemon exits, so the feeder drops stdio entirely.
	if devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		defer devNull.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	}
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
// so this process can arm it again. Only the pid file's presence matters: the
// feeder runs under whatever shell the device provides, so its contents are
// informational. It runs on the supervisor's goroutine and gives up as soon as
// the context is cancelled.
func reclaimWatchdog(ctx context.Context, runDir string) {
	if runDir == "" {
		return
	}
	cleanStaleFeeder(runDir)
	if !feederRegistered(runDir) {
		return
	}
	requestFeederHandback(runDir)
	// The feeder only looks at the marker between keepalives.
	deadline := time.Now().Add(2*feederTick + time.Second)
	for time.Now().Before(deadline) {
		if !feederRegistered(runDir) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// feederRegistered reports whether a pid file for a feeder exists.
func feederRegistered(runDir string) bool {
	if runDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(runDir, feederPIDFile))
	return err == nil
}

// cleanStaleFeeder removes a pid file whose feeder is gone or whose pid has been
// reused by an unrelated process. run/ lives on the persistent data partition,
// so the file survives a reboot or a hard kill; left in place it makes the next
// start wait for a handback that can never arrive and makes a deployment report
// a feeder that does not exist.
func cleanStaleFeeder(runDir string) {
	pid, ok := feederPID(runDir)
	if !ok {
		return
	}
	if !processAlive(pid) {
		_ = os.Remove(filepath.Join(runDir, feederPIDFile))
		return
	}
	if owned, known := feederOwnsPID(pid); known && !owned {
		_ = os.Remove(filepath.Join(runDir, feederPIDFile))
	}
}

// feederPID reads the pid a feeder published. The body is "<pid> shell" for the
// current feeder; a pid left by a legacy Go feeder parses the same way.
func feederPID(runDir string) (int, bool) {
	if runDir == "" {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(runDir, feederPIDFile))
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid names a live process. EPERM counts as alive:
// the process exists, it just belongs to another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// feederOwnsPID reports whether pid is still running a capsulad feeder, so a
// reused pid cannot be mistaken for one. known is false where the platform
// exposes no way to inspect another process (no /proc), in which case the
// caller keeps the pid file.
func feederOwnsPID(pid int) (owned, known bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false, false
	}
	cmdline := string(b)
	return strings.Contains(cmdline, feederMarker) || strings.Contains(cmdline, legacyFeederMarker), true
}
