//go:build !linux

package webui

import "testing"

// A development host must not present process stats as appliance telemetry.
func TestHostMetricsReportUnsupportedPlatform(t *testing.T) {
	reading := (&metricsCollector{}).sample()
	if reading.Available {
		t.Fatal("non-Linux host claimed host metrics support")
	}
	if reading.Detail == "" {
		t.Fatal("unsupported host metrics have no explanation")
	}
	if reading.CPUAvailable || reading.MemoryAvailable || reading.DiskAvailable || reading.TemperatureAvailable || reading.UptimeAvailable {
		t.Fatalf("unsupported host exposed a measured domain: %+v", reading)
	}
	if reading.CPUUsagePercent != 0 || reading.MemoryTotalBytes != 0 || reading.DiskTotalBytes != 0 || reading.UptimeSeconds != 0 {
		t.Fatalf("unsupported host fabricated a value: %+v", reading)
	}
}
