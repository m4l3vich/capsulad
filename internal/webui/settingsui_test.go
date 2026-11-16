package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/registry"
)

func testRegistryWithUI(t *testing.T) (*registry.Registry, string) {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "org.example.ui")
	if err := os.MkdirAll(filepath.Join(pkg, "ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `id = "org.example.ui"
name = "UI Test"
version = "1.0.0"

[entrypoint]
runtime = "native"
path = "app"
architecture = "aarch64"
abi = "linux-gnu"

[[settings]]
key = "mode"
label = "Mode"
type = "select"
[[settings.choices]]
value = "a"
label = "A"

[settings_ui]
html = "ui/settings.html"
`
	if err := os.WriteFile(filepath.Join(pkg, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "ui", "settings.html"),
		[]byte("<!doctype html><html><head><title>t</title></head><body><p id=\"app-marker\">hi</p></body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		t.Fatal(err)
	}
	return reg, pkg
}

func TestSettingsUIHandlerServesSandboxedDocument(t *testing.T) {
	reg, _ := testRegistryWithUI(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}
	req := httptest.NewRequest(http.MethodGet, "/api/apps/org.example.ui/settings-ui", nil)
	req.SetPathValue("appID", "org.example.ui")
	rec := httptest.NewRecorder()
	srv.appSettingsUI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	headers := rec.Header()
	if !strings.Contains(headers.Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("CSP = %q", headers.Get("Content-Security-Policy"))
	}
	if !strings.Contains(headers.Get("Content-Security-Policy"), "connect-src 'none'") {
		t.Fatalf("CSP lacks network denial: %q", headers.Get("Content-Security-Policy"))
	}
	if headers.Get("Set-Cookie") != "" {
		t.Fatal("asset response must not set or expose session state")
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache = %q", headers.Get("Cache-Control"))
	}
	body := rec.Body.String()
	for _, want := range []string{"capsulad-ui-kit", "capsulad-ui-bridge", "id=\"app-marker\"", "default-src 'none'"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q", want)
		}
	}
}

func TestSettingsUIHandlerRejectsUnknownAndEscapingAssets(t *testing.T) {
	reg, pkg := testRegistryWithUI(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("appID", "org.example.missing")
	rec := httptest.NewRecorder()
	srv.appSettingsUI(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown app status = %d", rec.Code)
	}

	// Replace the declared file with a symlink outside the package after the
	// registry loaded it; serving must re-check containment, not reuse a path.
	outside := filepath.Join(t.TempDir(), "secret.html")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	declared := filepath.Join(pkg, "ui", "settings.html")
	if err := os.Remove(declared); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, declared); err == nil {
		req = httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetPathValue("appID", "org.example.ui")
		rec = httptest.NewRecorder()
		srv.appSettingsUI(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("symlink escape status = %d", rec.Code)
		}
	}
}

func TestSettingsUIHandlerServesBuiltin(t *testing.T) {
	root := t.TempDir()
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterBuiltin(&registry.App{Manifest: builtinSettingsUIManifest()}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Apps: reg, Logs: logs}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("appID", "capsulad.clock")
	rec := httptest.NewRecorder()
	srv.appSettingsUI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("builtin status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Alarms") {
		t.Fatal("builtin clock document missing")
	}
}
func builtinSettingsUIManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ID: "capsulad.clock", Name: "Clock", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
		Settings:   []manifest.Setting{{Key: "alarms", Label: "Alarms", Type: "text"}},
		SettingsUI: &manifest.SettingsUI{HTML: "clock.html"},
	}
}

func TestSettingsUIRequiresAdminSession(t *testing.T) {
	env := newTestEnv(t)
	reg, _ := testRegistryWithUI(t)
	env.srv.Apps = reg
	target := env.ts.URL + "/api/apps/org.example.ui/settings-ui"

	anon := env.ts.Client()
	resp, err := anon.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", resp.StatusCode)
	}

	c := env.client(t)
	loginSession(t, env, c)
	resp, err = c.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("CSP = %q", resp.Header.Get("Content-Security-Policy"))
	}
}
