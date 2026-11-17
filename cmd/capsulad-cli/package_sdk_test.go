package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staticELF writes a minimal aarch64 ELF64 with no PT_INTERP (a static binary).
func staticELF(t *testing.T, path string) {
	t.Helper()
	const ehsize, phentsize = 64, 56
	buf := make([]byte, ehsize)
	copy(buf[0:4], "\x7fELF")
	buf[4] = 2 // ELFCLASS64
	buf[5] = 1 // little-endian
	buf[6] = 1
	binary.LittleEndian.PutUint16(buf[16:], 2)    // ET_EXEC
	binary.LittleEndian.PutUint16(buf[18:], 0xB7) // EM_AARCH64
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint64(buf[32:], ehsize)
	binary.LittleEndian.PutUint16(buf[52:], ehsize)
	binary.LittleEndian.PutUint16(buf[54:], phentsize)
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writePackage(t *testing.T, withSDK bool, abi string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := `id = "org.capsulad.probe"
name = "Probe"
version = "0.1.0"
[entrypoint]
runtime = "native"
path = "probe"
architecture = "aarch64"
abi = "` + abi + `"
`
	if withSDK {
		manifest += "\n[native_sdk]\nabi_major = 1\nmin_minor = 0\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	staticELF(t, filepath.Join(dir, "probe"))
	return dir
}

// A static musl entrypoint cannot dlopen the shared runtime, so packaging a
// package that declares [native_sdk] must be refused with a clear reason.
func TestPackageRejectsStaticMuslNativeSDK(t *testing.T) {
	dir := writePackage(t, true, "linux-musl")
	out := filepath.Join(t.TempDir(), "probe.tar.gz")
	err := runAppPackage([]string{dir, "-out", out, "-target-abi", "linux-musl", "-target-arch", "aarch64"})
	if err == nil || !strings.Contains(err.Error(), "statically linked") {
		t.Fatalf("static musl native-SDK package accepted: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("rejected package was still written")
	}
}

// The same static binary is fine when the app does not need the shared runtime,
// and a gnu target keeps its own ceiling rules.
func TestPackageAcceptsStaticWithoutNativeSDK(t *testing.T) {
	dir := writePackage(t, false, "linux-musl")
	out := filepath.Join(t.TempDir(), "probe.tar.gz")
	if err := runAppPackage([]string{dir, "-out", out, "-target-abi", "linux-musl", "-target-arch", "aarch64"}); err != nil {
		t.Fatalf("static non-SDK package rejected: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("package not written: %v", err)
	}
}
