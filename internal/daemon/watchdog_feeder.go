package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	feederPIDFile  = "watchdog.feeder"
	feederHandback = "watchdog.handback"
)

// feederScript keeps a handed-off hardware timer alive until a running daemon
// asks for it back. $1 is the pid file, $2 the handback marker; the armed
// descriptor arrives as fd 3. The pid file records "<pid> shell" so a deploy
// tool can tell this feeder apart from the legacy Go one.
const feederScript = `printf '%s shell\n' "$$" > "$1" || exit 2
while :; do
	if [ -e "$2" ]; then rm -f "$1"; exit 0; fi
	if ( printf '\000' >&3 ) 2>/dev/null; then :; else rm -f "$1"; exit 1; fi
	sleep 2
done
`

// handOffWatchdog starts a detached feeder holding the armed descriptor.
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
func handOffWatchdog(f *os.File, runDir string) error {
	if runDir == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	// A stale handback marker would make the feeder exit immediately.
	_ = os.Remove(filepath.Join(runDir, feederHandback))
	cmd := exec.Command("/bin/sh", "-c", feederScript,
		"capsulad-watchdog-feeder",
		filepath.Join(runDir, feederPIDFile),
		filepath.Join(runDir, feederHandback))
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
// informational.
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
