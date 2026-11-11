package rockchip

import (
	"os"
	"path/filepath"
	"testing"
)

// mkdirAll is a tiny test helper to keep the fixtures readable.
func mkdirAll(path string) error { return os.MkdirAll(path, 0o755) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbeSensors(t *testing.T) {
	root := t.TempDir()
	iio := filepath.Join(root, "sys/bus/iio/devices/iio:device0")
	writeFile(t, filepath.Join(iio, "name"), "bh1730\n")
	writeFile(t, filepath.Join(iio, "in_illuminance_input"), "123.5\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone0/type"), "soc-thermal\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone0/temp"), "45000\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone1/type"), "logic-thermal\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone1/temp"), "40000\n")

	sensors := probeSensors(root)
	if sensors == nil {
		t.Fatal("no sensors discovered")
	}
	list := sensors.List()
	units := map[string]string{}
	for _, info := range list {
		units[info.ID] = info.Unit
	}
	if units["sensor.ambient_light"] != "lux" {
		t.Fatalf("ambient unit = %q", units["sensor.ambient_light"])
	}
	if units["sensor.soc_temperature"] != "celsius" || units["sensor.logic_temperature"] != "celsius" {
		t.Fatalf("thermal units = %+v", units)
	}
	if v, ok := sensors.Read("sensor.ambient_light"); !ok || v != 123.5 {
		t.Fatalf("ambient = %v,%v", v, ok)
	}
	if v, ok := sensors.Read("sensor.soc_temperature"); !ok || v != 45 {
		t.Fatalf("soc temperature = %v,%v", v, ok)
	}
	if v, ok := sensors.Read("sensor.logic_temperature"); !ok || v != 40 {
		t.Fatalf("logic temperature = %v,%v", v, ok)
	}
	if list[0].ID != "sensor.ambient_light" || list[len(list)-1].ID != "sensor.soc_temperature" {
		t.Fatalf("sensor list not sorted: %+v", list)
	}
}

func TestProbeSensorsRawIlluminance(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sys/bus/iio/devices/iio:device0/name"), "bh1750")
	writeFile(t, filepath.Join(root, "sys/bus/iio/devices/iio:device0/in_illuminance_raw"), "77")
	sensors := probeSensors(root)
	if sensors == nil {
		t.Fatal("no sensors discovered")
	}
	if v, ok := sensors.Read("sensor.ambient_light"); !ok || v != 77 {
		t.Fatalf("raw ambient = %v,%v", v, ok)
	}
	if list := sensors.List(); len(list) != 1 || list[0].Unit != "lux-raw" {
		t.Fatalf("list = %+v", list)
	}
}

func TestProbeSensorsEmpty(t *testing.T) {
	if sensors := probeSensors(t.TempDir()); sensors != nil {
		t.Fatalf("unexpected sensors: %+v", sensors.List())
	}
}

func TestProbeMicEnableGPIO(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc/init.d/S50mic-enable"),
		"#!/bin/sh\nMIC_EN_GPIO=42 # GPIO1_D2\n")
	gpio, ok := probeMicEnableGPIO(root)
	if !ok || gpio != 42 {
		t.Fatalf("gpio = %d,%v", gpio, ok)
	}

	cfg := Config{RootPrefix: root}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if b.cfg.MicEnableGPIO != 42 {
		t.Fatalf("backend GPIO = %d, want 42", b.cfg.MicEnableGPIO)
	}
}

func TestProbeMicEnableGPIOExplicitConfigWins(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc/init.d/S50mic-enable"), "MIC_EN_GPIO=42\n")
	b, err := New(Config{RootPrefix: root, MicEnableGPIO: 7})
	if err != nil {
		t.Fatal(err)
	}
	if b.cfg.MicEnableGPIO != 7 {
		t.Fatalf("backend GPIO = %d, want explicit 7", b.cfg.MicEnableGPIO)
	}
}

func TestClaimSysfsLED(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "trigger"), "timer\n")
	claimSysfsLED(dir)
	data, err := os.ReadFile(filepath.Join(dir, "trigger"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "none" {
		t.Fatalf("trigger = %q, want none", data)
	}
	claimSysfsLED(t.TempDir()) // no trigger file: must be a no-op
}

func TestRooted(t *testing.T) {
	if got := rooted("", "sys/class/thermal"); got != "/sys/class/thermal" {
		t.Fatalf("empty prefix = %q", got)
	}
	if got := rooted("/tmp/root", "/sys/class/thermal"); got != "/tmp/root/sys/class/thermal" {
		t.Fatalf("prefixed = %q", got)
	}
}
