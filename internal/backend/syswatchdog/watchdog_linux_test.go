//go:build linux

package syswatchdog

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"capsulad/internal/backend"
)

// TestOpenFallsBackToDefaultTimeout opens a non-watchdog file: the open
// succeeds but WDIOC_GETTIMEOUT fails, so the profile default must be reported
// instead of zero.
func TestOpenFallsBackToDefaultTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := w.Timeout(); got != 30*time.Second {
		t.Fatalf("timeout = %v, want the 30s fallback", got)
	}
}

// A device held by another process must be retryable, and every other open
// failure must keep its own classification.
func TestClassifyOpenErrorSeparatesBusyFromMissing(t *testing.T) {
	busy := classifyOpenError("/dev/watchdog", syscall.EBUSY)
	if !errors.Is(busy, backend.ErrWatchdogBusy) {
		t.Fatalf("EBUSY = %v, want ErrWatchdogBusy", busy)
	}
	missing := classifyOpenError("/dev/watchdog", os.ErrNotExist)
	if errors.Is(missing, backend.ErrWatchdogBusy) {
		t.Fatalf("a missing node was reported busy: %v", missing)
	}
	if !errors.Is(missing, os.ErrNotExist) {
		t.Fatalf("missing-node error changed: %v", missing)
	}
}

// Detach transfers ownership: the timer must not be fed or closed afterwards.
func TestDetachDisablesFurtherUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f := w.Detach()
	if f == nil {
		t.Fatal("Detach returned no descriptor")
	}
	f.Close()
	if err := w.Feed(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Feed after Detach = %v, want ErrClosed", err)
	}
	if again := w.Detach(); again != nil {
		t.Fatalf("second Detach returned %v", again)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after Detach = %v", err)
	}
}
