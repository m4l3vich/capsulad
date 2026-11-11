//go:build unix

package main

import (
	"os/signal"
	"syscall"
)

// ignoreHangup keeps the daemon alive when the shell that spawned it exits.
// Without this, launching from an adb shell or an interactive terminal kills
// the supervisor (and trips the hardware watchdog) on SIGHUP.
func ignoreHangup() {
	signal.Ignore(syscall.SIGHUP)
}
