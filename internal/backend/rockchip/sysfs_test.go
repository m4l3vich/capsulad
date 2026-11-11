package rockchip

import (
	"path/filepath"
	"testing"
)

func TestProbeSysfsLEDsClassifiesRoles(t *testing.T) {
	root := t.TempDir()
	leds := filepath.Join(root, "sys/class/leds")
	writeFile(t, filepath.Join(leds, "rgb0", "multi_intensity"), "255 0 0\n")
	writeFile(t, filepath.Join(leds, "top-led1", "brightness"), "0\n")
	writeFile(t, filepath.Join(leds, "top-led2", "brightness"), "0\n")
	writeFile(t, filepath.Join(leds, "clock-led10", "brightness"), "0\n")
	writeFile(t, filepath.Join(leds, "clock-led1", "brightness"), "0\n")

	ring, mono := probeSysfsLEDs(root)
	if len(ring) != 1 || filepath.Base(ring[0]) != "rgb0" {
		t.Fatalf("ring = %v", ring)
	}
	if len(mono) != 2 {
		t.Fatalf("mono = %v", mono)
	}
	if filepath.Base(mono[0]) != "clock-led1" || filepath.Base(mono[1]) != "clock-led10" {
		t.Fatalf("mono order = %v", mono)
	}
}
