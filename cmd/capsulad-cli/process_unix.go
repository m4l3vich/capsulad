//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether pid names a live process. A permission error still
// means the process exists, which is all the caller needs to know.
func pidAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}
