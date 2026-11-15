package daemon

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHandOffWatchdogShellFeeder drives the real handoff: the feeder must be a
// plain shell that feeds the inherited descriptor (fd 3), advertise itself as
// "<pid> shell" so a deploy tool can recognise it, and exit on handback. EOF on
// the pipe is the exit proof: only a live child keeps the write end open.
func TestHandOffWatchdogShellFeeder(t *testing.T) {
	runDir := t.TempDir()
	pidFile := filepath.Join(runDir, feederPIDFile)
	handback := filepath.Join(runDir, feederHandback)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := handOffWatchdog(w, runDir); err != nil {
		_ = w.Close()
		t.Fatalf("handOffWatchdog: %v", err)
	}
	// The parent must not keep the write end: EOF then proves the child is gone.
	_ = w.Close()
	// If an assertion fails early, still let the feeder wind down.
	t.Cleanup(func() { _ = os.WriteFile(handback, []byte("handback"), 0o644) })

	var content string
	waitFor(t, 5*time.Second, "feeder pid file", func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil || len(b) == 0 {
			return false
		}
		content = string(b)
		return true
	})
	fields := strings.Fields(content)
	if len(fields) != 2 || fields[1] != "shell" {
		t.Fatalf("pid file = %q, want \"<pid> shell\"", content)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		t.Fatalf("pid file pid = %q: %v", fields[0], err)
	}
	if runtime.GOOS == "linux" {
		// The whole point: the feeder never executes or maps the daemon image,
		// so an upgrade can replace it while the timer is still fed.
		if exe, err := os.Readlink("/proc/" + fields[0] + "/exe"); err == nil {
			if self, err := os.Executable(); err == nil && exe == self {
				t.Fatalf("feeder %d runs %s instead of a shell", pid, exe)
			}
		}
	}

	// The first keepalive is written before the first sleep, so this must not
	// wait a full feed interval.
	buf := make([]byte, 1)
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("keepalive byte: %v", err)
	}
	if buf[0] != 0 {
		t.Fatalf("keepalive byte = %#x, want one NUL", buf[0])
	}

	if err := os.WriteFile(handback, []byte("handback"), 0o644); err != nil {
		t.Fatalf("write handback: %v", err)
	}
	waitFor(t, 5*time.Second, "feeder pid file removal after handback", func() bool {
		_, err := os.Stat(pidFile)
		return os.IsNotExist(err)
	})
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	for {
		if _, err := r.Read(buf); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("waiting for feeder exit: %v", err)
		}
	}
}
