package linuxdev

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"capsulad/internal/backend"
)

// sensorEntry is one readable sysfs sample. scale converts the raw kernel
// value to the reported unit (thermal zones report millidegrees Celsius).
type sensorEntry struct {
	path  string
	unit  string
	scale float64
}

type sysfsSensors struct {
	entries map[string]sensorEntry
}

// ProbeSensors discovers the ambient-light sensor and the SoC thermal zones.
// IIO device names are preferred over hard-coded bus paths so a kernel re-probe
// does not break the backend. Both supported boards carry a bh1730 behind the
// tinted front panel; the thermal zones are board-independent.
func ProbeSensors(prefix string) backend.Sensors {
	entries := map[string]sensorEntry{}
	probeAmbientLight(prefix, entries)
	probeThermalZones(prefix, entries)
	if len(entries) == 0 {
		return nil
	}
	return &sysfsSensors{entries: entries}
}

func probeAmbientLight(prefix string, entries map[string]sensorEntry) {
	for _, dev := range GlobSorted(Rooted(prefix, "sys/bus/iio/devices/iio:device*")) {
		name := strings.ToLower(ReadTrim(filepath.Join(dev, "name")))
		if !strings.Contains(name, "bh1730") && !strings.Contains(name, "bh1750") {
			continue
		}
		if path := ExistingFile(filepath.Join(dev, "in_illuminance_input")); path != "" {
			entries["sensor.ambient_light"] = sensorEntry{path: path, unit: "lux", scale: 1}
			return
		}
		if path := ExistingFile(filepath.Join(dev, "in_illuminance_raw")); path != "" {
			entries["sensor.ambient_light"] = sensorEntry{path: path, unit: "lux-raw", scale: 1}
			return
		}
	}
	// Older vendor kernels expose the IIO device directly below the i2c
	// driver directory rather than under /sys/bus/iio/devices.
	for _, pattern := range []string{
		"sys/bus/i2c/drivers/bh1750/*/iio:device*/in_illuminance_raw",
		"sys/bus/i2c/drivers/bh1730/*/iio:device*/in_illuminance_both_input",
		"sys/bus/i2c/drivers/bh1730/*/iio:device*/in_illuminance_raw",
	} {
		matches := GlobSorted(Rooted(prefix, pattern))
		if len(matches) > 0 {
			entries["sensor.ambient_light"] = sensorEntry{path: matches[0], unit: "lux-raw", scale: 1}
			return
		}
	}
}

func probeThermalZones(prefix string, entries map[string]sensorEntry) {
	for _, zone := range GlobSorted(Rooted(prefix, "sys/class/thermal/thermal_zone*")) {
		path := ExistingFile(filepath.Join(zone, "temp"))
		if path == "" {
			continue
		}
		id := thermalSensorID(ReadTrim(filepath.Join(zone, "type")))
		if _, exists := entries[id]; exists {
			continue
		}
		entries[id] = sensorEntry{path: path, unit: "celsius", scale: 0.001}
	}
}

func thermalSensorID(zoneType string) string {
	typ := strings.ToLower(strings.TrimSpace(zoneType))
	switch {
	case strings.Contains(typ, "soc"), strings.Contains(typ, "cpu"), strings.Contains(typ, "tsadc"):
		return "sensor.soc_temperature"
	case strings.Contains(typ, "logic"):
		return "sensor.logic_temperature"
	case typ == "":
		return "sensor.temperature"
	}
	var out []rune
	for _, r := range typ {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, r)
			continue
		}
		if len(out) > 0 && out[len(out)-1] != '_' {
			out = append(out, '_')
		}
	}
	id := strings.Trim(string(out), "_")
	if id == "" {
		id = "thermal"
	}
	return "sensor.temperature." + id
}

func (s *sysfsSensors) List() []backend.SensorInfo {
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]backend.SensorInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, backend.SensorInfo{ID: id, Unit: s.entries[id].unit})
	}
	return out
}

func (s *sysfsSensors) Read(id string) (float64, bool) {
	entry, ok := s.entries[id]
	if !ok {
		return 0, false
	}
	data, err := os.ReadFile(entry.path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, false
	}
	return v * entry.scale, true
}

func (s *sysfsSensors) Close() error { return nil }
