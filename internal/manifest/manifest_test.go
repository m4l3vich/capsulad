package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeBase() *Manifest {
	return &Manifest{
		ID:      "org.example.sdkdemo",
		Name:    "SDK Demo",
		Version: "0.1.0",
		Entrypoint: Entrypoint{
			Runtime:      RuntimeNative,
			Path:         "demo",
			Architecture: "aarch64",
			ABI:          "linux-musl",
		},
	}
}

func TestNativeSDKManifestValidation(t *testing.T) {
	m := nativeBase()
	m.NativeSDK = &NativeSDK{ABIMajor: 1, MinMinor: 2, RequiredCapabilities: []string{"audio", "events"}}
	if err := m.Validate(); err != nil {
		t.Fatalf("valid native_sdk: %v", err)
	}

	m = nativeBase()
	m.NativeSDK = &NativeSDK{ABIMajor: 0}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "abi_major") {
		t.Fatalf("zero abi_major error = %v", err)
	}

	m = nativeBase()
	m.NativeSDK = &NativeSDK{ABIMajor: 1, MinMinor: -1}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "min_minor") {
		t.Fatalf("negative min_minor error = %v", err)
	}

	m = nativeBase()
	m.NativeSDK = &NativeSDK{ABIMajor: 1, RequiredCapabilities: []string{"Audio"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("malformed capability error = %v", err)
	}

	m = nativeBase()
	m.NativeSDK = &NativeSDK{ABIMajor: 1, RequiredCapabilities: []string{"audio", "audio"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate capability error = %v", err)
	}

	m = &Manifest{
		ID: "org.example.script", Name: "Script", Version: "0.1.0",
		Entrypoint: Entrypoint{Runtime: RuntimeQuickJS, Path: "main.js"},
		NativeSDK:  &NativeSDK{ABIMajor: 1},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "native entrypoint") {
		t.Fatalf("script runtime with native_sdk error = %v", err)
	}
}

func TestNativeSDKLoad(t *testing.T) {
	dir := t.TempDir()
	manifest := `id = "org.example.sdkdemo"
name = "SDK Demo"
version = "0.1.0"

[entrypoint]
runtime = "native"
path = "demo"
architecture = "aarch64"
abi = "linux-musl"

[native_sdk]
abi_major = 1
min_minor = 3
required_capabilities = ["audio", "events"]
`
	file := filepath.Join(dir, "manifest.toml")
	if err := os.WriteFile(file, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(dir, file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.NativeSDK == nil {
		t.Fatal("native_sdk was not parsed")
	}
	if m.NativeSDK.ABIMajor != 1 || m.NativeSDK.MinMinor != 3 {
		t.Fatalf("native_sdk = %+v", m.NativeSDK)
	}
	if len(m.NativeSDK.RequiredCapabilities) != 2 {
		t.Fatalf("capabilities = %v", m.NativeSDK.RequiredCapabilities)
	}
}

func TestAppsWithoutNativeSDKStillValidate(t *testing.T) {
	if err := nativeBase().Validate(); err != nil {
		t.Fatalf("manifest without native_sdk: %v", err)
	}
}
