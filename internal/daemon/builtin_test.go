package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

// startEmptyDaemon runs the daemon against an application directory with no
// installed packages. The internal clock and ring must still become the
// selected baseline.
func startEmptyDaemon(t *testing.T, base string) *Daemon {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = base
	cfg.RunDir = filepath.Join(base, "run")
	cfg.SocketPath = filepath.Join(cfg.RunDir, "hal.sock")
	cfg.AppsDir = filepath.Join(base, "apps")
	cfg.AssetsDir = filepath.Join(base, "assets")
	cfg.WebUIAddr = ""
	cfg.SandboxMode = "none"
	cfg.NativeLibDir = nativeLibDir(t)
	if err := os.MkdirAll(cfg.AppsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("daemon shutdown timed out")
		}
	})
	return d
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "capsulad-builtin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

func TestBuiltinProvidersWithoutInstalledApps(t *testing.T) {
	base := shortTempDir(t)
	d := startEmptyDaemon(t, base)

	if apps := d.reg.List(); len(apps) != 0 {
		t.Fatalf("expected an empty registry, got %d apps", len(apps))
	}
	if runs := d.sup.List(""); len(runs) != 0 {
		t.Fatalf("built-ins must not be supervised processes, got %d runs", len(runs))
	}

	for _, resourceID := range []string{"display.segment.front", "light.ring.main"} {
		waitFor(t, 5*time.Second, "builtin baseline on "+resourceID, func() bool {
			sel := d.router.Current(resourceID)
			return sel.SelectedAppID == routing.BuiltinAppID && sel.Reason == "builtin_baseline"
		})
	}

	segSel := d.router.Current("display.segment.front")
	seg, ok := d.pres.Get(segSel.SelectedPresentation)
	if !ok {
		t.Fatal("segment builtin presentation disappeared")
	}
	if kind := seg.ContentKind(); kind != presentations.ContentSegmentFrame {
		t.Fatalf("segment baseline content = %v", kind)
	}
	ringSel := d.router.Current("light.ring.main")
	ring, ok := d.pres.Get(ringSel.SelectedPresentation)
	if !ok {
		t.Fatal("ring builtin presentation disappeared")
	}
	if kind := ring.ContentKind(); kind != presentations.ContentRingEffect {
		t.Fatalf("ring baseline content = %v", kind)
	}
}

func TestBuiltinBaselineRestoredAfterCaptureRelease(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))

	waitFor(t, 5*time.Second, "builtin ring selected", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == routing.BuiltinAppID
	})

	external := d.pres.Open("light.ring.main", "test.external", "run-external", presentations.Ring)
	if err := d.pres.Update(external, presentations.RingFrame([]backend.RGB{{}, {}, {}})); err != nil {
		t.Fatal(err)
	}
	cap, err := d.router.Acquire("light.ring.main", "test.external", "run-external", external.ID, routing.TouchNone)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "external capture selected", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == "test.external"
	})

	if !d.router.ForceRelease(cap.ID) {
		t.Fatal("capture was not released")
	}
	waitFor(t, 5*time.Second, "builtin ring restored", func() bool {
		sel := d.router.Current("light.ring.main")
		return sel.SelectedAppID == routing.BuiltinAppID && sel.SelectedCaptureID == ""
	})
	d.pres.Close(external)
}

func TestBuiltinClockMigratesLegacySettings(t *testing.T) {
	base := shortTempDir(t)
	settingsPath := filepath.Join(base, "settings.json")
	legacy := `{
  "revision": 9,
  "values": {
    "default_app_by_resource": {
      "display.segment.front": "org.capsula.clock",
      "light.ring.main": "org.capsula.clock"
    },
    "default_assistant_app_id": "org.capsula.assistant",
    "wakeword_enabled": false,
    "master_volume": 0.6
  }
}`
	if err := os.WriteFile(settingsPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyClock := filepath.Join(base, "app-settings", "org.capsula.clock", "settings.json")
	if err := os.MkdirAll(filepath.Dir(legacyClock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyClock, []byte(`{
  "revision": 4,
  "values": {"blink": false, "timezone": "Asia/Shanghai"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	v := d.user.Get().Values
	if len(v.DefaultAppByResource) != 0 {
		t.Fatalf("legacy defaults were not migrated: %v", v.DefaultAppByResource)
	}
	if v.DefaultAssistantAppID != "" {
		t.Fatalf("legacy assistant default survived: %q", v.DefaultAssistantAppID)
	}

	snap, err := d.appCfg.Snapshot(builtinClockSettingsID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range snap.Values {
		got[v.Key] = v.Value
	}
	if got["blink"] != "false" || got["timezone"] != `"Asia/Shanghai"` {
		t.Fatalf("clock settings were not migrated: %v", got)
	}

	// Migration must not leave a stale default app selected after the built-in
	// provider starts.
	waitFor(t, 5*time.Second, "builtin clock after migration", func() bool {
		return d.router.Current("display.segment.front").SelectedAppID == routing.BuiltinAppID
	})
}

func TestBuiltinRingModeSetting(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitFor(t, 5*time.Second, "builtin ring selected", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == routing.BuiltinAppID
	})
	if _, _, _, err := d.appCfg.Update(builtinRingSettingsID, 0, []settings.Change{{Key: "mode", Value: "spinner"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "spinner mode rendered", func() bool {
		sel := d.router.Current("light.ring.main")
		p, ok := d.pres.Get(sel.SelectedPresentation)
		if !ok {
			return false
		}
		pixels, ok := p.RenderRing(d.dev.Ring().Caps(), d.clock.NowNS())
		if !ok {
			return false
		}
		for _, px := range pixels {
			if px != (backend.RGB{}) {
				return true
			}
		}
		return false
	})
}

func TestClockFrameBlinkAndTimezone(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	at := time.Date(2026, 11, 12, 12, 34, 56, 0, loc)
	cells, colon := clockFrame(at, true)
	if len(cells) != 4 {
		t.Fatalf("cells = %v", cells)
	}
	if !colon {
		t.Fatal("even second with blink enabled must light the colon")
	}
	odd := at.Add(time.Second)
	if _, colon := clockFrame(odd, true); colon {
		t.Fatal("odd second with blink enabled must darken the colon")
	}
	if _, colon := clockFrame(odd, false); !colon {
		t.Fatal("blink disabled must keep the colon lit")
	}
	want := []uint16{clockDigits['1'], clockDigits['2'], clockDigits['3'], clockDigits['4']}
	for i := range want {
		if cells[i] != want[i] {
			t.Fatalf("cell %d = %05b, want %05b", i, cells[i], want[i])
		}
	}
}

func TestRetiredPackageIsNotLaunched(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "apps", "org.capsula.clock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "launched")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "clock"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `id = "org.capsula.clock"
name = "Old clock"
version = "0.1.0"
background = true
start_on_boot = true
restart_on_failure = true
single_instance = true
provides = ["segment_display_provider"]

[entrypoint]
runtime = "native"
path = "clock"
architecture = "aarch64"
abi = "linux-musl"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	if _, ok := d.reg.Get("org.capsula.clock"); !ok {
		t.Fatal("package was not discovered; the test is not exercising the retired path")
	}
	d.startBootApps()
	d.ensureRunning("org.capsula.clock")
	if _, _, _, err := d.Launch("org.capsula.clock", nil); err == nil {
		t.Fatal("retired package was launchable")
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("retired package was executed")
	}
	waitFor(t, 5*time.Second, "builtin clock still selected", func() bool {
		return d.router.Current("display.segment.front").SelectedAppID == routing.BuiltinAppID
	})
}
