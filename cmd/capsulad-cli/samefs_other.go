//go:build !linux && !darwin

package main

// sameFilesystemOnDisk has no portable implementation outside Linux and macOS.
// It reports false so the caller always takes the copy path, which is correct
// on every filesystem; the atomic rename path is only an optimization.
func sameFilesystemOnDisk(string, string) bool {
	return false
}
