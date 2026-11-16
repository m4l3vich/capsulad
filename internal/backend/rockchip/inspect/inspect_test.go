package inspect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInspectGolden pins the JSON shape, field names and evidence strings the
// offline firmware inspector has always reported. A change here changes the
// CLI's -json contract.
func TestInspectGolden(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/os-release", "PRETTY_NAME=\"Capsula Stock 1.0\"\n")
	write("etc/init.d/S50mic-enable", "MIC_EN_GPIO=21\n")
	write("etc/init.d/S99btctl.sh", "#!/bin/sh\n")
	write("usr/bin/voice-agent", "binary\n")
	write("usr/bin/sysmon", "binary\n")
	write("etc/pulse/device.pa", "sink_name=master\nsource_name=speex\n")
	write("dev/led_multi_ctrl", "")
	write("usr/share/voice-agent/led/test.json", `[{"colors":[[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3],[1,2,3]],"brightness":[1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1]}]`)

	report, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	report.Root = "<ROOT>"
	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	want, err := os.ReadFile(filepath.Join("testdata", "report.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v; actual output:\n%s", err, got)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("inspector JSON drifted:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestInspectRejectsNonDirectory keeps the CLI error contract.
func TestInspectRejectsNonDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(path); err == nil {
		t.Fatal("Inspect accepted a file as a firmware root")
	}
}
