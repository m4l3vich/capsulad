//go:build unix

package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openContainedRelative opens rel under root without following a symlink in
// any component. root itself is daemon-owned and resolved once; every path
// component below it is opened with O_NOFOLLOW relative to the previous
// directory descriptor, which also prevents ".." and absolute-path escapes.
func openContainedRelative(root, rel string) (*os.File, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	dirfd, err := unix.Open(resolvedRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			unix.Close(dirfd)
			return nil, fmt.Errorf("invalid path component %q", part)
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			// A FIFO/device at the declared path must not block before the
			// regular-file check rejects it.
			flags |= unix.O_NONBLOCK
		}
		next, openErr := unix.Openat(dirfd, part, flags, 0)
		unix.Close(dirfd)
		if openErr != nil {
			return nil, openErr
		}
		dirfd = next
	}
	name := filepath.Join(resolvedRoot, filepath.FromSlash(rel))
	return os.NewFile(uintptr(dirfd), name), nil
}
