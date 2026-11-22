//go:build unix

package websecurity

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireCredentialsLock takes the exclusive advisory lock that serializes
// credential mutations between the daemon, the CLI and any other local writer.
// The returned function releases it. The lock file is never read or removed:
// unlinking it while another process waits would split the lock domain.
func acquireCredentialsLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open credentials lock: %w", err)
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock credentials: %w", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
