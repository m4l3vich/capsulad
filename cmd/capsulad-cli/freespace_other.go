//go:build !linux && !darwin

package main

// freeSpaceBytes has no portable implementation outside Linux and macOS. The
// caller skips the preflight there; extraction is still bounded by
// bundleLimits and a failure removes the staging tree.
func freeSpaceBytes(string) (free uint64, known bool, err error) {
	return 0, false, nil
}
