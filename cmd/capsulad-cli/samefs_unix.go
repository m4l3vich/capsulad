//go:build linux || darwin

package main

import "syscall"

// sameFilesystemOnDisk reports whether file and dir live on one filesystem,
// where the caller can replace a target with a rename.
func sameFilesystemOnDisk(file, dir string) bool {
	var fileStat, dirStat syscall.Stat_t
	if err := syscall.Stat(file, &fileStat); err != nil {
		return false
	}
	if err := syscall.Stat(dir, &dirStat); err != nil {
		return false
	}
	return fileStat.Dev == dirStat.Dev
}
