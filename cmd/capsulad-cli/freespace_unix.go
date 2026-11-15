//go:build linux || darwin

package main

import "syscall"

// freeSpaceBytes reports the bytes available to unprivileged writers on the
// filesystem holding dir. known is false when the platform cannot answer.
func freeSpaceBytes(dir string) (free uint64, known bool, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, false, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true, nil
}
