//go:build linux

package webui

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func platformMetricsSupported() bool { return true }

// readSystemMetrics reads the Linux host's real counters. A missing file or an
// unreadable domain clears only that domain's Available flag; it never turns
// into a zero measurement.
func readSystemMetrics(prev cpuCounters, hasPrev bool) (SystemMetrics, cpuCounters, bool) {
	out := SystemMetrics{Available: true}
	cur, cpuOK := readCPUCounters()
	if cpuOK {
		out.CPUCount = cur.count
		if out.CPUCount == 0 {
			out.CPUCount = uint32(runtime.NumCPU())
		}
		if hasPrev {
			if usage, ok := cpuUsage(prev, cur); ok {
				out.CPUAvailable = true
				out.CPUUsagePercent = usage
			}
		}
	}

	var missing []string
	if total, available, ok := readMemInfo(); ok {
		out.MemoryAvailable = true
		out.MemoryTotalBytes = total
		if available > total {
			available = total
		}
		out.MemoryUsedBytes = total - available
	} else {
		missing = append(missing, "memory")
	}

	if total, free, ok := readRootDisk(); ok {
		out.DiskAvailable = true
		out.DiskTotalBytes = total
		out.DiskFreeBytes = free
	} else {
		missing = append(missing, "disk")
	}

	if celsius, ok := readSoCTemperature(); ok {
		out.TemperatureAvailable = true
		out.SoCTemperatureC = celsius
	} else {
		missing = append(missing, "temperature")
	}

	if seconds, ok := readUptime(); ok {
		out.UptimeAvailable = true
		out.UptimeSeconds = seconds
	} else {
		missing = append(missing, "uptime")
	}

	if !out.CPUAvailable {
		missing = append(missing, "cpu")
	}
	if len(missing) == 0 {
		out.Detail = "Live device readings"
	} else {
		out.Detail = "Some readings are unavailable on this device"
	}
	return out, cur, cpuOK
}

func readCPUCounters() (cpuCounters, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuCounters{}, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256), 64<<10)
	var out cpuCounters
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cpu" {
			if found {
				continue
			}
			aggregate, ok := parseCPUStatCPU(line)
			if !ok {
				return cpuCounters{}, false
			}
			out.total, out.idle = aggregate.total, aggregate.idle
			found = true
			continue
		}
		if len(fields[0]) > 3 && fields[0][:3] == "cpu" {
			if _, err := strconv.Atoi(fields[0][3:]); err == nil {
				out.count++
			}
		}
	}
	if err := scanner.Err(); err != nil || !found {
		return cpuCounters{}, false
	}
	return out, true
}

func readMemInfo() (total, available uint64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	return parseMemInfo(f)
}

func readRootDisk() (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, 0, false
	}
	blockSize := uint64(st.Bsize)
	if blockSize == 0 {
		return 0, 0, false
	}
	return st.Blocks * blockSize, st.Bavail * blockSize, true
}

// diskUsageAt reports total and unprivileged free space of the filesystem
// holding path. App storage uses the applications root so its figures match
// the partition that will actually accept an install.
func diskUsageAt(path string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	blockSize := uint64(st.Bsize)
	if blockSize == 0 {
		return 0, 0, false
	}
	return st.Blocks * blockSize, st.Bavail * blockSize, true
}

// diskFreeAt reports the unprivileged free space of the filesystem holding
// path. The asset page uses the store root so its figure matches the partition
// that will actually accept an upload.
func diskFreeAt(path string) (uint64, bool) {
	_, free, ok := diskUsageAt(path)
	return free, ok
}

func readSoCTemperature() (float64, bool) {
	zones, err := filepath.Glob("/sys/class/thermal/thermal_zone*")
	if err != nil {
		return 0, false
	}
	readings := make([]thermalZone, 0, len(zones))
	for _, zone := range zones {
		raw, err := os.ReadFile(filepath.Join(zone, "temp"))
		if err != nil {
			continue
		}
		kindRaw, _ := os.ReadFile(filepath.Join(zone, "type"))
		reading, ok := parseThermalZone(string(raw), string(kindRaw))
		if !ok {
			continue
		}
		readings = append(readings, reading)
	}
	return selectSoCTemperature(readings)
}

func readUptime() (uint64, bool) {
	f, err := os.Open("/proc/uptime")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return parseUptime(f)
}
