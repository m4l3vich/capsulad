package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/manifest"
)

// newSerialDaemon builds a daemon with the given /proc/cpuinfo fixture and
// returns it without starting Run: the launch environment is assembled at
// construction, so nothing else needs to be running.
func newSerialDaemon(t *testing.T, cpuinfo string) *Daemon {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	if err := os.WriteFile(path, []byte(cpuinfo), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := hardwareSerialPath
	hardwareSerialPath = path
	t.Cleanup(func() { hardwareSerialPath = previous })

	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.RunDir = filepath.Join(dir, "run")
	cfg.SocketPath = filepath.Join(cfg.RunDir, "hal.sock")
	cfg.AppsDir = filepath.Join(dir, "apps")
	cfg.AssetsDir = filepath.Join(dir, "assets")
	cfg.WebUIAddr = ""
	cfg.SandboxMode = "none"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("daemon: %v", err)
	}
	t.Cleanup(func() { _ = d.logs.Close() })
	return d
}

func nativeLaunchEnv(t *testing.T, d *Daemon) []string {
	t.Helper()
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, "app"), []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ID:         "org.example.native",
		Dir:        appDir,
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeNative, Path: "app"},
	}
	cmd, err := d.launcher.Command(m, appDir, "run1", m.ID)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	return cmd.Env
}

func TestLaunchEnvCarriesHardwareSerial(t *testing.T) {
	d := newSerialDaemon(t, "processor\t: 0\nSerial\t\t: 11bfcbc5d63d55fc\n")
	found := false
	for _, entry := range nativeLaunchEnv(t, d) {
		if entry == "CAPSULA_DEVICE_SERIAL=11bfcbc5d63d55fc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the daemon serial did not reach the app environment: %v", nativeLaunchEnv(t, d))
	}
}

func TestLaunchEnvOmitsUnknownHardwareSerial(t *testing.T) {
	d := newSerialDaemon(t, "processor\t: 0\nHardware\t: Rockchip\n")
	for _, entry := range nativeLaunchEnv(t, d) {
		if strings.HasPrefix(entry, "CAPSULA_DEVICE_SERIAL=") {
			t.Fatalf("an unknown serial must not be exported: %q", entry)
		}
	}
}
