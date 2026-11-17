//go:build !linux

package syswatchdog

import (
	"errors"
	"os"
	"time"

	"capsulad/internal/backend"
)

// ErrNoWatchdog reports that this build has no hardware watchdog support.
var ErrNoWatchdog = errors.New("hardware watchdog is only implemented for Linux")

// Watchdog is an inert placeholder so callers compile off-target.
type Watchdog struct{}

// Open is unavailable outside Linux.
func Open(string, time.Duration) (*Watchdog, error) { return nil, ErrNoWatchdog }

func (w *Watchdog) Timeout() time.Duration { return 0 }
func (w *Watchdog) Feed() error            { return ErrNoWatchdog }
func (w *Watchdog) Detach() *os.File       { return nil }
func (w *Watchdog) Close() error           { return nil }

var _ backend.Watchdog = (*Watchdog)(nil)
