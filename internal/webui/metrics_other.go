//go:build !linux

package webui

// The appliance is a Linux device. A macOS or other host runs the daemon for
// development, but it must not present process stats as host CPU/RAM/disk/SoC
// telemetry, so this build reports the capability as absent.
func platformMetricsSupported() bool { return false }

func readCPUCounters() (cpuCounters, bool) { return cpuCounters{}, false }

func readSystemMetrics(cpuCounters, bool) (SystemMetrics, cpuCounters, bool) {
	return SystemMetrics{
		Available: false,
		Detail:    "System health readings are not available on this host",
	}, cpuCounters{}, false
}

// diskFreeAt is unavailable on a development host: the daemon does not present
// host storage as appliance capacity.
func diskFreeAt(string) (uint64, bool) { return 0, false }

// diskUsageAt carries the same rule for the applications filesystem.
func diskUsageAt(string) (uint64, uint64, bool) { return 0, 0, false }
