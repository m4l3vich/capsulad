package wifi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testLinuxManagerConfig(t *testing.T, runner *fakeRunner) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		DataDir: dir, RunDir: filepath.Join(dir, "run"), Interface: "wlan0",
		APAddress: "192.168.4.1", APPrefix: 24, PortalPort: 8080, APPassword: "capsulad",
		APName: func(context.Context) (string, error) { return "Capsule-Test", nil },
		Runner: runner,
	}
}

func fakeLinuxTools() *fakeRunner {
	return &fakeRunner{path: map[string]string{
		"wpa_supplicant": "/usr/sbin/wpa_supplicant",
		"wpa_cli":        "/usr/sbin/wpa_cli",
		"ip":             "/usr/sbin/ip",
	}}
}

// TestConfigDefaultsAreNeutral pins the separation: the generic Wi-Fi package
// injects no stock-firmware files, restore path or adoption. Only the standard
// Linux supplicant control directories are built in.
func TestConfigDefaultsAreNeutral(t *testing.T) {
	runner := fakeLinuxTools()
	cfg, err := (Config{DataDir: t.TempDir(), Runner: runner}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.StockConfigPaths) != 0 {
		t.Fatalf("stock config paths = %v, want none", cfg.StockConfigPaths)
	}
	if len(cfg.StockCtrlDirs) != 0 {
		t.Fatalf("stock control dirs = %v, want none injected", cfg.StockCtrlDirs)
	}
	if cfg.RestoreConfigPath != "" {
		t.Fatalf("restore config = %q, want none", cfg.RestoreConfigPath)
	}
	if cfg.AdoptStock {
		t.Fatal("adoption must be opt-in")
	}

	m, err := newLinuxManager(cfg, "wlan0")
	if err != nil {
		t.Fatal(err)
	}
	dirs := m.ctrlDirs()
	want := map[string]bool{m.ctrlDir(): true, "/var/run/wpa_supplicant": true, "/run/wpa_supplicant": true}
	if len(dirs) != len(want) {
		t.Fatalf("control dirs = %v", dirs)
	}
	for _, dir := range dirs {
		if !want[dir] {
			t.Fatalf("unexpected control dir %q in %v", dir, dirs)
		}
	}
	if calls := runner.snapshotRuns(); len(calls) != 0 {
		t.Fatalf("construction ran commands: %v", calls)
	}
}

// TestConfigExplicitStockInjection confirms caller-supplied paths survive
// normalization and add to, rather than replace, the generic control dirs.
func TestConfigExplicitStockInjection(t *testing.T) {
	runner := fakeLinuxTools()
	cfg := testLinuxManagerConfig(t, runner)
	cfg.StockConfigPaths = []string{"/custom/wpa.conf"}
	cfg.StockCtrlDirs = []string{"/custom/wpa", "/var/run/wpa_supplicant"}
	cfg.RestoreConfigPath = "/custom/restore.conf"
	cfg.AdoptStock = true
	cfg, err := cfg.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.StockConfigPaths) != 1 || cfg.StockConfigPaths[0] != "/custom/wpa.conf" {
		t.Fatalf("stock config paths = %v", cfg.StockConfigPaths)
	}
	if cfg.RestoreConfigPath != "/custom/restore.conf" || !cfg.AdoptStock {
		t.Fatalf("restore/adoption = %q/%v", cfg.RestoreConfigPath, cfg.AdoptStock)
	}
	m, err := newLinuxManager(cfg, "wlan0")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, dir := range m.ctrlDirs() {
		if seen[dir] {
			t.Fatalf("duplicate control dir %q", dir)
		}
		seen[dir] = true
	}
	for _, dir := range []string{"/custom/wpa", "/var/run/wpa_supplicant", "/run/wpa_supplicant"} {
		if !seen[dir] {
			t.Fatalf("control dirs %v missing %q", m.ctrlDirs(), dir)
		}
	}
}

// TestStockAdoptionDisabledSkipsImport proves adoption short-circuits before
// any file read or supplicant command, including termination.
func TestStockAdoptionDisabledSkipsImport(t *testing.T) {
	dir := t.TempDir()
	stockPath := filepath.Join(dir, "wpa_supplicant.conf")
	if err := os.WriteFile(stockPath, []byte("network={\n\tssid=\"home\"\n\tpsk=\"password123\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := fakeLinuxTools()
	cfg := testLinuxManagerConfig(t, runner)
	cfg.StockConfigPaths = []string{stockPath}
	cfg.AdoptStock = false
	cfg, err := cfg.normalized()
	if err != nil {
		t.Fatal(err)
	}
	m, err := newLinuxManager(cfg, "wlan0")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.importStockNetworks(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.stopStockSupplicants(context.Background())
	if calls := runner.snapshotRuns(); len(calls) != 0 {
		t.Fatalf("disabled adoption ran commands: %v", calls)
	}
	list, err := m.store.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("disabled adoption imported networks: %v", list)
	}
	if !m.store.stockImported() {
		t.Fatal("disabled adoption must be recorded so it is not retried forever")
	}
}
