//go:build !unix

package manifest

import (
	"os"
	"path/filepath"
)

// Platforms without openat/O_NOFOLLOW still validate the relative path and
// inspect the opened file. The daemon targets Linux and macOS, where the
// component-wise unix implementation is used.
func openContainedRelative(root, rel string) (*os.File, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(resolvedRoot, filepath.FromSlash(rel)))
}
