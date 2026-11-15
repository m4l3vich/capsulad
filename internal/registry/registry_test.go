package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/artifact"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
)

const testManifest = `
id = "test.pkg"
name = "Test package"
version = "%s"
[entrypoint]
runtime = "native"
path = "bin/app"
architecture = "aarch64"
abi = "linux-musl"
`

func testPackage(t *testing.T, version string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(fmt.Sprintf(testManifest, version)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "app"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	logs, err := logbus.New(ids.NewClock(), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(t.TempDir(), logs)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// A failed install must not leave a partial package behind: on the 78.5 MiB
// data partition the remnants would consume the space a retry needs.
func TestInstallFailureRemovesPartialPackage(t *testing.T) {
	root := t.TempDir()
	src := testPackage(t, "0.1.0", nil)
	// Artifact admission rejects symlinks; the install must not stage anything.
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
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging tree left behind: %v", entries)
	}
}

func TestInstallReplacesExistingPackage(t *testing.T) {
	reg := newTestRegistry(t)
	first := testPackage(t, "0.1.0", map[string]string{"old.txt": "v1"})
	app, err := reg.Install(first)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if app.Manifest.Version != "0.1.0" {
		t.Fatalf("installed version = %q", app.Manifest.Version)
	}

	second := testPackage(t, "0.2.0", map[string]string{"new.txt": "v2"})
	if _, err := reg.Install(second); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := reg.Rescan(); err != nil {
		t.Fatal(err)
	}
	app, ok := reg.Get("test.pkg")
	if !ok {
		t.Fatal("app missing after update")
	}
	if app.Manifest.Version != "0.2.0" {
		t.Fatalf("updated version = %q", app.Manifest.Version)
	}
	// An update is a replacement, not a merge.
	if _, err := os.Stat(filepath.Join(app.Dir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("file from the previous version survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.Dir, "new.txt")); err != nil {
		t.Fatalf("new file missing: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(app.Dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stage-") || strings.HasPrefix(e.Name(), ".previous-") {
			t.Fatalf("install left staging directory %q", e.Name())
		}
	}
}

func TestInstallArchive(t *testing.T) {
	reg := newTestRegistry(t)
	src := testPackage(t, "1.0.0", map[string]string{"data.txt": "archived"})
	archive := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := artifact.Write(src, archive); err != nil {
		t.Fatalf("package: %v", err)
	}
	app, err := reg.Install(archive)
	if err != nil {
		t.Fatalf("install archive: %v", err)
	}
	if app.Manifest.Version != "1.0.0" {
		t.Fatalf("version = %q", app.Manifest.Version)
	}
	body, err := os.ReadFile(filepath.Join(app.Dir, "data.txt"))
	if err != nil || string(body) != "archived" {
		t.Fatalf("installed archive content = %q, %v", body, err)
	}
	info, err := os.Stat(filepath.Join(app.Dir, "bin", "app"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode = %04o", info.Mode().Perm())
	}
}

func TestInstallRejectsTraversal(t *testing.T) {
	reg := newTestRegistry(t)
	archive := filepath.Join(t.TempDir(), "bad.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		body string
	}{
		{"manifest.toml", fmt.Sprintf(testManifest, "0.1.0")},
		{"../escape", "x"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Install(archive); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("install error = %v, want a traversal rejection", err)
	}
	if _, ok := reg.Get("test.pkg"); ok {
		t.Fatal("traversal package was installed")
	}
}

func TestInstallRefusesBuiltin(t *testing.T) {
	reg := newTestRegistry(t)
	builtin := &manifest.Manifest{
		ID: "test.pkg", Name: "Built in", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
	}
	if err := reg.RegisterBuiltin(&App{Manifest: builtin}); err != nil {
		t.Fatalf("register builtin: %v", err)
	}
	src := testPackage(t, "0.1.0", nil)
	if _, err := reg.Install(src); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("install error = %v, want a builtin refusal", err)
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
