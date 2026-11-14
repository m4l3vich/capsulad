package webui

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"
)

// The parsers here are platform-neutral so their edge cases (guest CPU time,
// absent uptime, an unknown thermal zone) are testable without a Linux host.
// The build-tagged readers only supply the file contents.

// parseCPUStatCPU parses the aggregate "cpu" line from /proc/stat. Linux folds
// guest and guest_nice time into user/nice, so adding those two fields to the
// total would count them twice; only the first eight counters contribute.
func parseCPUStatCPU(line string) (cpuCounters, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, false
	}
	var values [10]uint64
	for i := range values {
		if i+1 >= len(fields) {
			break
		}
		value, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return cpuCounters{}, false
		}
		values[i] = value
	}
	// user, nice, system, idle, iowait, irq, softirq, steal (guest excluded).
	var total uint64
	for _, value := range values[:8] {
		total += value
	}
	// idle and iowait are both non-busy time.
	return cpuCounters{total: total, idle: values[3] + values[4]}, true
}

// cpuUsage computes the busy share between two aggregate samples.
func cpuUsage(prev, cur cpuCounters) (float64, bool) {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0, false
	}
	total := cur.total - prev.total
	idle := cur.idle - prev.idle
	if idle > total {
		return 0, false
	}
	return percent(total-idle, total), true
}

// parseMemInfo parses /proc/meminfo into total and available bytes. It falls
// back to free+buffers+cached on kernels that predate MemAvailable.
func parseMemInfo(r io.Reader) (total, available uint64, ok bool) {
	var free, buffers, cached uint64
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 256), 64<<10)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = value * 1024
		case "MemAvailable":
			available = value * 1024
		case "MemFree":
			free = value * 1024
		case "Buffers":
			buffers = value * 1024
		case "Cached":
			cached = value * 1024
		}
	}
	if err := scanner.Err(); err != nil || total == 0 {
		return 0, 0, false
	}
	if available == 0 {
		available = free + buffers + cached
	}
	if available > total {
		available = total
	}
	return total, available, true
}

// parseUptime parses /proc/uptime. An unreadable or malformed file is reported
// as unavailable rather than as an uptime of zero.
func parseUptime(r io.Reader) (uint64, bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64), 4<<10)
	if !scanner.Scan() {
		return 0, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) == 0 {
		return 0, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1<<53 {
		return 0, false
	}
	return uint64(value), true
}

// thermalZone is one recognized thermal source. Unknown zone types (battery,
// charger, wifi) are never used as a SoC temperature.
type thermalZone struct {
	kind    string // "soc" or "cpu"
	celsius float64
}

// parseThermalZone validates one /sys/class/thermal/thermal_zone*/ pair. A
// zone whose type is not an understood SoC/CPU source is rejected, which lets
// the caller report temperature as unavailable instead of mislabeling a
// battery sensor.
func parseThermalZone(tempRaw, kindRaw string) (thermalZone, bool) {
	kind := ""
	switch lower := strings.ToLower(strings.TrimSpace(kindRaw)); {
	case strings.Contains(lower, "soc"):
		kind = "soc"
	case strings.Contains(lower, "cpu"):
		kind = "cpu"
	default:
		return thermalZone{}, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(tempRaw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return thermalZone{}, false
	}
	celsius := value
	if celsius > 1000 || celsius < -1000 {
		celsius /= 1000
	}
	if celsius < -40 || celsius > 200 {
		return thermalZone{}, false
	}
	return thermalZone{kind: kind, celsius: celsius}, true
}

// selectSoCTemperature prefers a real SoC zone, accepts a CPU zone as the
// honest fallback on boards without one, and returns unavailable otherwise.
func selectSoCTemperature(zones []thermalZone) (float64, bool) {
	var soc, cpu float64
	var haveSOC, haveCPU bool
	for _, zone := range zones {
		switch zone.kind {
		case "soc":
			if !haveSOC || zone.celsius > soc {
				soc, haveSOC = zone.celsius, true
			}
		case "cpu":
			if !haveCPU || zone.celsius > cpu {
				cpu, haveCPU = zone.celsius, true
			}
		}
	}
	if haveSOC {
		return soc, true
	}
	if haveCPU {
		return cpu, true
	}
	return 0, false
}
