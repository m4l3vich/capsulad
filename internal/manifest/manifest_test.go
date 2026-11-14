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

func TestScriptEntrypointExtensionIsEnforced(t *testing.T) {
	base := func(runtime Runtime, path string) *Manifest {
		return &Manifest{
			ID: "org.example.script", Name: "Script", Version: "0.1.0",
			Entrypoint: Entrypoint{Runtime: runtime, Path: path},
		}
	}
	for _, tc := range []struct {
		name    string
		runtime Runtime
		path    string
		ok      bool
	}{
		{"quickjs js", RuntimeQuickJS, "main.js", true},
		{"quickjs mjs is not a classic script", RuntimeQuickJS, "main.mjs", false},
		{"quickjs wrong", RuntimeQuickJS, "main.py", false},
		{"micropython py", RuntimeMicroPython, "main.py", true},
		{"micropython wrong", RuntimeMicroPython, "main.js", false},
		{"missing path", RuntimeQuickJS, "", false},
	} {
		err := base(tc.runtime, tc.path).Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
}

func settingsUIBase() *Manifest {
	return &Manifest{
		ID: "org.example.settingsui", Name: "Settings UI", Version: "1.0.0",
		Entrypoint: Entrypoint{Runtime: RuntimeNative, Path: "app", Architecture: "aarch64", ABI: "linux-gnu"},
		Settings:   []Setting{{Key: "delay", Label: "Delay", Type: "text"}},
		SettingsUI: &SettingsUI{HTML: "ui/settings.html"},
	}
}

func TestSettingsUIValidation(t *testing.T) {
	m := settingsUIBase()
	if err := m.Validate(); err != nil {
		t.Fatalf("valid settings_ui: %v", err)
	}

	for _, path := range []string{"../settings.html", "/etc/passwd", "ui/settings.htm", "ui/../../x.html"} {
		bad := settingsUIBase()
		bad.SettingsUI = &SettingsUI{HTML: path}
		if err := bad.Validate(); err == nil {
			t.Fatalf("path %q accepted", path)
		}
	}

	noSettings := settingsUIBase()
	noSettings.Settings = nil
	if err := noSettings.Validate(); err == nil || !strings.Contains(err.Error(), "declared setting") {
		t.Fatalf("settings_ui without settings error = %v", err)
	}

	// Built-in manifests validate without a package directory; their document
	// is embedded by the daemon.
	builtin := settingsUIBase()
	builtin.Entrypoint = Entrypoint{Runtime: RuntimeBuiltin}
	builtin.SettingsUI = &SettingsUI{HTML: "clock.html"}
	if err := builtin.Validate(); err != nil {
		t.Fatalf("builtin settings_ui: %v", err)
	}
}

func TestSettingsUIAssetIsContained(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ui", "settings.html"), []byte("<!doctype html><p>ok</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettingsUIAsset(root, "ui/settings.html"); err != nil {
		t.Fatalf("read: %v", err)
	}

	// A symlink inside the package must not alias a host file.
	outside := filepath.Join(t.TempDir(), "secret.html")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "ui", "link.html")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := ReadSettingsUIAsset(root, "ui/link.html"); err == nil {
			t.Fatal("symlink escaping the package was read")
		}
	}

	// Oversized documents are rejected before the browser sees them.
	big := filepath.Join(root, "ui", "big.html")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", MaxSettingsUIBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSettingsUIAsset(root, "ui/big.html"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized document error = %v", err)
	}
}

func TestSettingsUILoadRequiresDocument(t *testing.T) {
	root := t.TempDir()
	doc := `id = "org.example.settingsui"
name = "Settings UI"
version = "1.0.0"

[entrypoint]
runtime = "native"
path = "app"
architecture = "aarch64"
abi = "linux-gnu"

[[settings]]
key = "delay"
label = "Delay"
type = "text"

[settings_ui]
html = "ui/settings.html"
`
	if err := os.WriteFile(filepath.Join(root, "manifest.toml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, filepath.Join(root, "manifest.toml")); err == nil {
		t.Fatal("manifest loaded without its declared document")
	}
	if err := os.MkdirAll(filepath.Join(root, "ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ui", "settings.html"), []byte("<p>ok</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, filepath.Join(root, "manifest.toml")); err != nil {
		t.Fatalf("load with document: %v", err)
	}
}
