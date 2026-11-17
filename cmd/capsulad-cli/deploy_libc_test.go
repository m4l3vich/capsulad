package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bundle whose runtime is not an ELF, or whose symlink escapes the bundle,
// must be refused before the daemon is stopped.
func TestCheckNativeLibcRejectsBadRuntimes(t *testing.T) {
	defer selectDeployProfile("rockchip-stock")
	if _, err := selectDeployProfile("amlogic-alpine"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	lib := filepath.Join(root, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(lib, "libcapsulad.so.1.0.0")
	if err := os.WriteFile(target, []byte("not an elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &deployment{libSrc: lib}
	err := d.checkBundleELF()
	if err == nil || !strings.Contains(err.Error(), "not an ELF") {
		t.Fatalf("bad runtime accepted: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "libcapsulad.so.1.0.0")
	if err := os.WriteFile(outside, []byte("not an elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	if err := d.checkBundleELF(); err == nil || !strings.Contains(err.Error(), "outside the bundle") {
		t.Fatalf("escaping symlink accepted: %v", err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(lib, "missing"), target); err != nil {
		t.Fatal(err)
	}
	if err := d.checkBundleELF(); err == nil {
		t.Fatal("dangling symlink accepted")
	}
}
