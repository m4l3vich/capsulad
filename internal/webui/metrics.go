package webui

import (
	"sync"
	"time"
)

// Host metrics for the authenticated diagnostics topic. They are read-only,
// bounded, and sampled only while a diagnostics subscription is active; the
// browser never polls. Unsupported platforms answer with available=false and
// an explanatory detail rather than invented zeros.
const (
	metricsRefreshFloor = 2 * time.Second
	metricsCPUWarmup    = 250 * time.Millisecond
	metricsSamplePeriod = 5 * time.Second
)

// SystemMetrics is the platform-neutral reading. A domain is meaningful only
// when its *Available flag is true.
type SystemMetrics struct {
	Available            bool
	Detail               string
	CPUAvailable         bool
	CPUUsagePercent      float64
	CPUCount             uint32
	MemoryAvailable      bool
	MemoryTotalBytes     uint64
	MemoryUsedBytes      uint64
	DiskAvailable        bool
	DiskTotalBytes       uint64
	DiskFreeBytes        uint64
	TemperatureAvailable bool
	SoCTemperatureC      float64
	UptimeAvailable      bool
	UptimeSeconds        uint64
}

// cpuCounters is one /proc/stat aggregate sample, kept to compute the busy
// share across the sampling interval instead of reporting a lifetime average.
type cpuCounters struct {
	total uint64
	idle  uint64
	count uint32
}

type metricsCollector struct {
	mu        sync.Mutex
	cached    SystemMetrics
	sampledAt time.Time
	hasSample bool

	prevCPU    cpuCounters
	hasPrevCPU bool
}

// hostMetrics is process-wide: the daemon has one host and every diagnostics
// subscriber shares the same bounded sampling work.
var hostMetrics = &metricsCollector{}

// sample returns a cached reading when the floor has not elapsed, otherwise a
// fresh one. The first call on a supported host takes a short baseline so CPU
// usage is a real delta rather than a fabricated zero.
func (m *metricsCollector) sample() SystemMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.hasSample && now.Sub(m.sampledAt) < metricsRefreshFloor {
		return m.cached
	}
	prev, hasPrev := m.prevCPU, m.hasPrevCPU
	if platformMetricsSupported() && !hasPrev {
		if cur, ok := readCPUCounters(); ok {
			prev, hasPrev = cur, true
			time.Sleep(metricsCPUWarmup)
		}
	}
	reading, cpu, cpuOK := readSystemMetrics(prev, hasPrev)
	m.prevCPU, m.hasPrevCPU = cpu, cpuOK
	m.cached = reading
	m.hasSample = true
	m.sampledAt = time.Now()
	return reading
}

func percent(busy, total uint64) float64 {
	if total == 0 || busy > total {
		return 0
	}
	value := 100 * float64(busy) / float64(total)
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
