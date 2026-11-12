package registry

import (
	"os"
	"path/filepath"
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
)

const testManifest = `
id = "test.pkg"
name = "Test package"
version = "0.1.0"
[entrypoint]
runtime = "native"
path = "bin/app"
architecture = "aarch64"
abi = "linux-musl"
`

// A failed install must not leave a partial package behind: on the 78.5 MiB
// data partition the remnants would consume the space a retry needs.
func TestInstallFailureRemovesPartialPackage(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "manifest.toml"), []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "app"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// copyTree rejects symlinks; the install must then remove its destination.
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	logs, err := logbus.New(ids.NewClock(), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(root, logs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Install(src); err == nil {
		t.Fatal("install of a package with a symlink succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "test.pkg")); !os.IsNotExist(err) {
		t.Fatalf("partial package left behind: %v", err)
	}
}

// A compiled-in system app is a real registry entry: it lists, resolves and
// survives a package rescan, but it is not a directory and cannot be removed.
func TestBuiltinRegistrationSurvivesRescanAndCannotBeUninstalled(t *testing.T) {
	logs, err := logbus.New(ids.NewClock(), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(t.TempDir(), logs)
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ID: "capsula.clock", Name: "Clock", Version: "1.0.0",
		Provides:   []string{"segment_display_provider"},
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
	}
	if err := reg.RegisterBuiltin(&App{Manifest: m}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !reg.IsBuiltin("capsula.clock") {
		t.Fatal("builtin not reported")
	}
	if app, ok := reg.Get("capsula.clock"); !ok || app.Manifest != m {
		t.Fatalf("get = %+v, %v", app, ok)
	}
	if err := reg.Rescan(); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("capsula.clock"); !ok {
		t.Fatal("rescan dropped the builtin")
	}
	listed := false
	for _, app := range reg.List() {
		listed = listed || app.ID() == "capsula.clock"
	}
	if !listed {
		t.Fatal("builtin is not listed")
	}
	if err := reg.Uninstall("capsula.clock"); err == nil {
		t.Fatal("builtin was uninstalled")
	}
	if err := reg.RegisterBuiltin(&App{Manifest: m}); err == nil {
		t.Fatal("duplicate builtin registration was accepted")
	}
}
