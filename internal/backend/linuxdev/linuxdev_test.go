package linuxdev

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

func TestRooted(t *testing.T) {
	if got := Rooted("", "sys/class/thermal"); got != "/sys/class/thermal" {
		t.Fatalf("empty prefix = %q", got)
	}
	if got := Rooted("/tmp/root", "/sys/class/thermal"); got != "/tmp/root/sys/class/thermal" {
		t.Fatalf("prefixed = %q", got)
	}
}

func TestClaimLED(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "trigger"), "timer\n")
	ClaimLED(dir)
	data, err := os.ReadFile(filepath.Join(dir, "trigger"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "none" {
		t.Fatalf("trigger = %q, want none", data)
	}
	ClaimLED(t.TempDir()) // no trigger file: must be a no-op
}

func TestNaturalLess(t *testing.T) {
	if !NaturalLess("top-led2", "top-led10") {
		t.Fatal("top-led2 must sort before top-led10")
	}
	if NaturalLess("top-led10", "top-led2") {
		t.Fatal("top-led10 must sort after top-led2")
	}
	if !NaturalLess("clock-led9", "top-led1") {
		t.Fatal("different prefixes keep lexicographic order")
	}
}

func TestProbeSysfsLEDsClassifiesRoles(t *testing.T) {
	root := t.TempDir()
	leds := filepath.Join(root, "sys/class/leds")
	writeFile(t, filepath.Join(leds, "rgb0", "multi_intensity"), "255 0 0\n")
	writeFile(t, filepath.Join(leds, "top-led1", "brightness"), "0\n")
	writeFile(t, filepath.Join(leds, "top-led2", "brightness"), "0\n")
	writeFile(t, filepath.Join(leds, "clock-led10", "brightness"), "0\n")
	writeFile(t, filepath.Join(leds, "clock-led1", "brightness"), "0\n")

	ring, mono := ProbeSysfsLEDs(root, "/sys/class/leds", 12, 30, []string{"top-led"})
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
