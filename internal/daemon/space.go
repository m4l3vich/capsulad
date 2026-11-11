package daemon

import (
	"fmt"
	"syscall"
)

// minFreeBytes is the free-space floor below which the daemon refuses to write
// new durable state. /data is 78.5 MiB on the RK3308 and the log mirror, the
// settings file and the daemon itself share it; filling it breaks logging and
// the next upgrade rather than failing one write.
const minFreeBytes = 2 << 20

// freeBytes reports the capacity available to unprivileged writers below path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Bavail <= 0 || st.Bsize <= 0 {
		return 0, nil
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// requireFreeSpace rejects a new durable write when the data partition cannot
// absorb it. A failed probe never blocks the write: refusing work because a
// statfs call is unsupported would be worse than the space risk.
func (d *Daemon) requireFreeSpace(purpose string) error {
	free, err := freeBytes(d.cfg.DataDir)
	if err != nil {
		return nil
	}
	if free < minFreeBytes {
		return fmt.Errorf("device storage is nearly full (%d KiB free); free space before %s", free>>10, purpose)
	}
	return nil
}
