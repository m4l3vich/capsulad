//go:build !linux || !(amd64 || arm64)

package rockchip

import (
	"errors"
	"os"
	"time"

	"capsulad/internal/backend"
)

// ErrNoWatchdog reports that this build has no hardware watchdog support.
var ErrNoWatchdog = errors.New("hardware watchdog is only implemented for linux/amd64 and linux/arm64")

// OpenWatchdog is unavailable outside Linux.
func OpenWatchdog(string) (*SysWatchdog, error) { return nil, ErrNoWatchdog }

// SysWatchdog is an inert placeholder so the package compiles off-target.
type SysWatchdog struct{}

func (w *SysWatchdog) Timeout() time.Duration { return 0 }
func (w *SysWatchdog) Feed() error            { return ErrNoWatchdog }
func (w *SysWatchdog) Detach() *os.File       { return nil }
func (w *SysWatchdog) Close() error           { return nil }

var _ backend.Watchdog = (*SysWatchdog)(nil)
