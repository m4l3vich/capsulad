package install

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// AlpineReserveBytes is space the installer deliberately leaves free on an
// Alpine data filesystem: logs, certificates, settings and the daemon's own
// runtime files grow after installation.
const AlpineReserveBytes int64 = 64 << 20

// StockDeployReserveBytes is the deployer's own floor on the stock data
// partition. The deployment refuses below it, so the installer reports the same
// number instead of discovering the refusal after the daemon has stopped.
const StockDeployReserveBytes int64 = 8 << 20

// DFInfo is one `df -Pk` result.
type DFInfo struct {
	Filesystem string
	Available  int64 // bytes
	MountedOn  string
}

// ParseDF parses POSIX `df -Pk` output. The final data line is used because a
// long filesystem name may wrap; header and wrapper lines are skipped.
func ParseDF(out string) (DFInfo, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		f := strings.Fields(lines[i])
		if len(f) < 6 {
			continue
		}
		// Filesystem 1024-blocks Used Available Capacity Mounted-on [rest]
		kb, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			continue
		}
		mount := strings.Join(f[5:], " ")
		return DFInfo{Filesystem: f[0], Available: kb * 1024, MountedOn: mount}, nil
	}
	return DFInfo{}, fmt.Errorf("could not parse df -Pk output: %q", strings.TrimSpace(out))
}

// SpaceCheck is the free-space decision.
type SpaceCheck struct {
	AvailableBytes  int64
	BundleBytes     int64
	AppBytes        int64
	ReserveBytes    int64
	RequiredBytes   int64
	StagedDaemon    int64
	InstalledDaemon int64
	OK              bool
}

// CheckSpace compares available space with the bundle, the selected apps and
// the documented reserve. All inputs are bytes. It is the Alpine model.
func CheckSpace(available, bundle, apps, reserve int64) SpaceCheck {
	required := addSat(addSat(bundle, apps), reserve)
	return SpaceCheck{
		AvailableBytes: available,
		BundleBytes:    bundle,
		AppBytes:       apps,
		ReserveBytes:   reserve,
		RequiredBytes:  required,
		OK:             available >= required,
	}
}

// CheckStockSpace mirrors the deployer's stock partition preflight: the 8 MiB
// reserve plus the daemon write that cannot free its own space (a larger staged
// daemon minus the installed one). An installed daemon that is already larger
// adds nothing.
func CheckStockSpace(available, stagedDaemon, installedDaemon int64) SpaceCheck {
	delta := stagedDaemon - installedDaemon
	if delta < 0 {
		delta = 0
	}
	required := addSat(StockDeployReserveBytes, delta)
	return SpaceCheck{
		AvailableBytes:  available,
		ReserveBytes:    StockDeployReserveBytes,
		StagedDaemon:    stagedDaemon,
		InstalledDaemon: installedDaemon,
		RequiredBytes:   required,
		OK:              available >= required,
	}
}

// addSat adds non-negative byte counts without wrapping into a negative
// requirement on absurd inputs.
func addSat(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// BytesLabel formats a byte count for the TUI.
func BytesLabel(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
