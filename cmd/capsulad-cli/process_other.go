//go:build !linux && !darwin

package main

// pidAlive has no portable implementation outside Linux and macOS. It reports
// false so the caller falls back to the framework API probe alone.
func pidAlive(int) bool {
	return false
}
