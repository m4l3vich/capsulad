package registry

import (
	"os"
	"path/filepath"
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
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
