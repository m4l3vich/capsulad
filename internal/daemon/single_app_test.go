package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestSingleAppLaunchesOnlyThatApp pins the launch policy: a specified app is
// started even when its manifest does not ask for boot, while start_on_boot
// apps and selected providers stay stopped and cannot be launched on demand.
func TestSingleAppLaunchesOnlyThatApp(t *testing.T) {
	d, _, _ := newTestDaemonPrepared(t, func(d *Daemon) {
		d.cfg.SingleApp = "test.probe"
	})
	waitReady(t, d, "test.probe", 10*time.Second)
	for _, appID := range []string{"test.clock", "test.assistant"} {
		if runs := d.sup.List(appID); len(runs) != 0 {
			t.Fatalf("%s started in single-app mode: %+v", appID, runs)
		}
	}
	if _, _, _, err := d.Launch("test.clock", nil); err == nil {
		t.Fatal("launching an app other than the single app succeeded")
	}
}

// TestSingleAppUnknownFailsBeforeServing refuses an unknown ID instead of
// serving a daemon that quietly launches nothing.
func TestSingleAppUnknownFailsBeforeServing(t *testing.T) {
	base := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = base
	cfg.RunDir = filepath.Join(base, "run")
	cfg.SocketPath = filepath.Join(cfg.RunDir, "hal.sock")
	cfg.AppsDir = filepath.Join(base, "apps")
	cfg.AssetsDir = filepath.Join(base, "assets")
	cfg.WebUIAddr = ""
	cfg.SandboxMode = "none"
	d, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.cancel()
	d.cfg.SingleApp = "test.missing"
	if err := d.Run(); err == nil {
		t.Fatal("expected an error for an unknown single app")
	}
}
