package rockchip

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbeMicEnableGPIO(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc/init.d/S50mic-enable"),
		"#!/bin/sh\nMIC_EN_GPIO=42 # GPIO1_D2\n")
	gpio, ok := probeMicEnableGPIO(root)
	if !ok || gpio != 42 {
		t.Fatalf("gpio = %d,%v", gpio, ok)
	}

	b, err := New(Config{RootPrefix: root})
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
