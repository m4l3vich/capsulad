// Package syswatchdog implements the generic Linux hardware watchdog
// (/dev/watchdog) shared by the board backends.
//
// The board backend supplies the device path and default timeout from its
// profile; this package owns only the ioctl ABI, the busy-device classification
// and the descriptor handoff. Liveness policy — when to feed, when to hand the
// descriptor to a detached feeder, and how to report it — stays in
// internal/daemon.
package syswatchdog

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"capsulad/internal/backend"
)

// wdioGetTimeout is WDIOC_GETTIMEOUT (_IOR('W', 7, int)). Feeding is a plain
// write (the keepalive ioctl is not needed), so no other code is defined here.
const wdioGetTimeout = 0x80045707

// Watchdog is one armed hardware timer.
type Watchdog struct {
	f    *os.File
	to   time.Duration
	mu   sync.Mutex
	dead bool
}

// Open arms path. Opening a watchdog device starts the timer, so the caller
// must Feed it or Close it. defaultTimeout is reported when the driver refuses
// WDIOC_GETTIMEOUT. A device another process still holds returns an error that
// wraps backend.ErrWatchdogBusy, which the supervisor retries.
func Open(path string, defaultTimeout time.Duration) (*Watchdog, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, classifyOpenError(path, err)
	}
	w := &Watchdog{f: f, to: defaultTimeout}
	if secs, err := unix.IoctlGetInt(int(f.Fd()), wdioGetTimeout); err == nil && secs > 0 {
		w.to = time.Duration(secs) * time.Second
	}
	return w, nil
}

// classifyOpenError marks a device held by another process as retryable. Only
// one process may own a watchdog, so EBUSY means "someone else has it", not
// "the watchdog is missing"; every other failure is returned unchanged.
func classifyOpenError(path string, err error) error {
	if errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("%s: %w", path, backend.ErrWatchdogBusy)
	}
	return err
}

// Timeout reports the armed interval.
func (w *Watchdog) Timeout() time.Duration { return w.to }

// Feed resets the timer.
func (w *Watchdog) Feed() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return os.ErrClosed
	}
	if _, err := w.f.Write([]byte{0}); err != nil {
		return fmt.Errorf("watchdog keepalive: %w", err)
	}
	return nil
}

// Detach hands the descriptor to the caller and disables further use.
func (w *Watchdog) Detach() *os.File {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return nil
	}
	w.dead = true
	return w.f
}

// Close writes the magic 'V' before closing. A nowayout driver ignores it and
// keeps the timer armed, so callers that must not reset the board hand the
// descriptor off instead.
func (w *Watchdog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return nil
	}
	w.dead = true
	_, writeErr := w.f.Write([]byte{'V'})
	closeErr := w.f.Close()
	if writeErr != nil {
		return fmt.Errorf("watchdog magic close: %w", writeErr)
	}
	return closeErr
}

var _ backend.Watchdog = (*Watchdog)(nil)
