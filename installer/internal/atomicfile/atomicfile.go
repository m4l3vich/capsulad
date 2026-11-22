// Package atomicfile replaces a file with a completed temporary sibling.
//
// The temporary file is always created in the destination directory, so the
// POSIX rename is atomic. On Windows, rename over an existing file fails; in
// that case the destination is removed and the rename retried. The installer
// runs on all three hosts, so both paths exist.
package atomicfile

import "os"

// Replace moves tmp to dest. On success tmp no longer exists.
func Replace(tmp, dest string) error {
	if err := os.Rename(tmp, dest); err == nil {
		return nil
	}
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmp, dest)
}
