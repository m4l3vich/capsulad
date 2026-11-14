package webui

import (
	"strings"
	"testing"
)

func TestParseCPUStatExcludesGuestTime(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice.
	// guest/guest_nice are already included in user/nice on Linux and must not
	// be counted again.
	cur, ok := parseCPUStatCPU("cpu 100 10 50 1000 20 5 5 5 900 40")
	if !ok {
		t.Fatal("valid aggregate cpu line was rejected")
	}
	if want := uint64(100 + 10 + 50 + 1000 + 20 + 5 + 5 + 5); cur.total != want {
		t.Fatalf("total = %d, want %d (guest time double counted?)", cur.total, want)
	}
	if want := uint64(1000 + 20); cur.idle != want {
		t.Fatalf("idle = %d, want %d", cur.idle, want)
	}
	if _, ok := parseCPUStatCPU("cpu0 1 2 3 4"); ok {
		t.Fatal("a per-core line was accepted as the aggregate")
	}
	if _, ok := parseCPUStatCPU("cpu 1 2 nope 4"); ok {
		t.Fatal("a malformed counter was accepted")
	}
}

func TestCPUUsageDelta(t *testing.T) {
	prev := cpuCounters{total: 1000, idle: 800}
	cur := cpuCounters{total: 1200, idle: 900}
	usage, ok := cpuUsage(prev, cur)
	if !ok {
		t.Fatal("valid delta was rejected")
	}
	if usage != 50 {
		t.Fatalf("usage = %v, want 50", usage)
	}
	if _, ok := cpuUsage(cur, prev); ok {
		t.Fatal("a reversed sample produced a reading")
	}
	if _, ok := cpuUsage(cpuCounters{total: 10, idle: 10}, cpuCounters{total: 10, idle: 10}); ok {
		t.Fatal("a zero delta produced a reading")
	}
}

func TestParseMemInfo(t *testing.T) {
	raw := "MemTotal:       1000 kB\nMemFree:         100 kB\nBuffers:          50 kB\nCached:          250 kB\n"
	total, available, ok := parseMemInfo(strings.NewReader(raw))
	if !ok {
		t.Fatal("valid meminfo was rejected")
	}
	if total != 1000*1024 {
		t.Fatalf("total = %d", total)
	}
	if available != (100+50+250)*1024 {
		t.Fatalf("available fallback = %d, want %d", available, (100+50+250)*1024)
	}

	total, available, ok = parseMemInfo(strings.NewReader("MemTotal: 1000 kB\nMemAvailable: 400 kB\n"))
	if !ok || total != 1000*1024 || available != 400*1024 {
		t.Fatalf("MemAvailable = %d/%d/%v", total, available, ok)
	}
	if _, _, ok := parseMemInfo(strings.NewReader("No memory counters here")); ok {
		t.Fatal("malformed meminfo claimed a reading")
	}
}

func TestParseUptime(t *testing.T) {
	seconds, ok := parseUptime(strings.NewReader("12345.67 9876.54\n"))
	if !ok || seconds != 12345 {
		t.Fatalf("uptime = %d/%v", seconds, ok)
	}
	for _, raw := range []string{"", "\n", "nan 1", "-5 1"} {
		if seconds, ok := parseUptime(strings.NewReader(raw)); ok {
			t.Fatalf("malformed uptime %q reported %d seconds", raw, seconds)
		}
	}
}

func TestParseThermalZoneRecognizesOnlySoCAndCPU(t *testing.T) {
	zone, ok := parseThermalZone("45000\n", "soc-thermal\n")
	if !ok || zone.kind != "soc" || zone.celsius != 45 {
		t.Fatalf("soc zone = %+v/%v", zone, ok)
	}
	zone, ok = parseThermalZone("40000", "cpu-thermal")
	if !ok || zone.kind != "cpu" || zone.celsius != 40 {
		t.Fatalf("cpu zone = %+v/%v", zone, ok)
	}
	if zone, ok := parseThermalZone("32000", "battery"); ok {
		t.Fatalf("battery sensor accepted as %+v", zone)
	}
	if _, ok := parseThermalZone("250000", "soc-thermal"); ok {
		t.Fatal("out-of-range temperature accepted")
	}
	if _, ok := parseThermalZone("garbage", "soc-thermal"); ok {
		t.Fatal("malformed temperature accepted")
	}
}

func TestSelectSoCTemperaturePrefersRecognizedSource(t *testing.T) {
	celsius, ok := selectSoCTemperature([]thermalZone{
		{kind: "cpu", celsius: 55},
		{kind: "soc", celsius: 48},
	})
	if !ok || celsius != 48 {
		t.Fatalf("preferred reading = %v/%v", celsius, ok)
	}
	celsius, ok = selectSoCTemperature([]thermalZone{{kind: "cpu", celsius: 55}})
	if !ok || celsius != 55 {
		t.Fatalf("cpu fallback = %v/%v", celsius, ok)
	}
	if _, ok := selectSoCTemperature(nil); ok {
		t.Fatal("no recognized zone reported a temperature")
	}
}
