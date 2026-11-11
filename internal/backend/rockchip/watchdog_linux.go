//go:build linux && (amd64 || arm64)

package rockchip

import (
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"capsulad/internal/backend"
)

// Generic Linux watchdog ioctls (include/uapi/linux/watchdog.h).
const (
	wdioKeepalive  = 0x80045705 // _IOR('W', 5, int)
	wdioGetTimeout = 0x80045707 // _IOR('W', 7, int)
	// watchdogDefaultTimeout is used when the driver refuses to report its
	// armed timeout; the DesignWare watchdog on this firmware arms at 30 s.
	watchdogDefaultTimeout = 30 * time.Second
)

// SysWatchdog is the RK3308 DesignWare /dev/watchdog. The vendor voice-agent
// opens it, feeds it and relies on the SoC reset when it stops; the framework
// takes that role over so superseding the vendor agent does not reboot the
// device. Only one process can hold the device, so a caller must retry while a
// vendor daemon still owns it.
type SysWatchdog struct {
	f    *os.File
	to   time.Duration
	mu   sync.Mutex
	dead bool
}

// OpenWatchdog opens and arms path (default /dev/watchdog). Opening a watchdog
// device starts the timer, so the caller must Feed it or Close it.
func OpenWatchdog(path string) (*SysWatchdog, error) {
	if path == "" {
		path = "/dev/watchdog"
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	w := &SysWatchdog{f: f, to: watchdogDefaultTimeout}
	if secs, err := unix.IoctlGetInt(int(f.Fd()), wdioGetTimeout); err == nil && secs > 0 {
		w.to = time.Duration(secs) * time.Second
	}
	return w, nil
}

// Timeout reports the armed interval.
func (w *SysWatchdog) Timeout() time.Duration { return w.to }

// Feed resets the timer.
func (w *SysWatchdog) Feed() error {
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
func (w *SysWatchdog) Detach() *os.File {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return nil
	}
	w.dead = true
	return w.f
}

// Close writes the magic 'V' before closing. The RK3308 stock kernel ignores
// it (the timer stays armed), so callers that must not reset the board hand the
// descriptor off instead.
func (w *SysWatchdog) Close() error {
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

var _ backend.Watchdog = (*SysWatchdog)(nil)
