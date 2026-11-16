package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/manifest"
)

func TestCheckNativeSDKRequirementInstallGate(t *testing.T) {
	plain := &manifest.Manifest{ID: "org.example.plain"}
	if err := checkNativeSDKRequirement(plain, t.TempDir(), ""); err != nil {
		t.Fatalf("app without native_sdk must install unchanged: %v", err)
	}

	required := &manifest.Manifest{
		ID:        "org.example.sdk",
		NativeSDK: &manifest.NativeSDK{ABIMajor: 1},
	}
	if err := checkNativeSDKRequirement(required, t.TempDir(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "install rejected") {
		t.Fatalf("missing runtime error = %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "capsulad-native.json"),
		[]byte(`{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "libcapsulad.so.1"), []byte("shared object"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := checkNativeSDKRequirement(required, t.TempDir(), dir); err != nil {
		t.Fatalf("compatible runtime rejected: %v", err)
	}

	required.NativeSDK.ABIMajor = 2
	if err := checkNativeSDKRequirement(required, t.TempDir(), dir); err == nil || !strings.Contains(err.Error(), "major 2") {
		t.Fatalf("incompatible requirement error = %v", err)
	}
}

func TestResolveNativeLibDirAcceptsRelativeExplicit(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveNativeLibDir(t.TempDir(), rel)
	if err != nil {
		t.Fatalf("relative -native-lib-dir %q: %v", rel, err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("relative -native-lib-dir resolved %q, want canonical %q", got, want)
	}

	if _, err := resolveNativeLibDir(t.TempDir(), filepath.Join("missing", "native-lib")); err == nil {
		t.Fatal("missing -native-lib-dir must be rejected")
	}
}
