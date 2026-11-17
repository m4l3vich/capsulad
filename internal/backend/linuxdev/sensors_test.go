package linuxdev

import (
	"path/filepath"
	"testing"
)

func TestProbeSensors(t *testing.T) {
	root := t.TempDir()
	iio := filepath.Join(root, "sys/bus/iio/devices/iio:device0")
	writeFile(t, filepath.Join(iio, "name"), "bh1730\n")
	writeFile(t, filepath.Join(iio, "in_illuminance_input"), "123.5\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone0/type"), "soc-thermal\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone0/temp"), "45000\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone1/type"), "logic-thermal\n")
	writeFile(t, filepath.Join(root, "sys/class/thermal/thermal_zone1/temp"), "40000\n")

	sensors := ProbeSensors(root)
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
	sensors := ProbeSensors(root)
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
	if sensors := ProbeSensors(t.TempDir()); sensors != nil {
		t.Fatalf("unexpected sensors: %+v", sensors.List())
	}
}
