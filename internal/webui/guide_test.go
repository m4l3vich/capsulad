package webui

import (
	"encoding/json"
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

func testRegistryWithGuide(t *testing.T) (*registry.Registry, string) {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "org.example.guide")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `id = "org.example.guide"
name = "Guide Test"
version = "1.0.0"

[entrypoint]
runtime = "native"
path = "app"
architecture = "aarch64"
abi = "linux-gnu"
`
	if err := os.WriteFile(filepath.Join(pkg, "manifest.toml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, manifest.GuideFile),
		[]byte("# Guide Test\n\nPress the circle key to start.\n\n## Troubleshooting\n\nNothing to report.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		t.Fatal(err)
	}
	return reg, pkg
}

func serveGuide(t *testing.T, srv *Server, appID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/apps/"+appID+"/guide", nil)
	req.SetPathValue("appID", appID)
	rec := httptest.NewRecorder()
	srv.appGuide(rec, req)
	return rec
}

func TestGuideHandlerServesPackageMarkdown(t *testing.T) {
	reg, pkg := testRegistryWithGuide(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}
	rec := serveGuide(t, srv, "org.example.guide")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	headers := rec.Header()
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache = %q", headers.Get("Cache-Control"))
	}
	if headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("nosniff = %q", headers.Get("X-Content-Type-Options"))
	}
	if ct := headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}
	var body struct {
		AppID    string `json:"app_id"`
		Title    string `json:"title"`
		Source   string `json:"source"`
		Format   string `json:"format"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.AppID != "org.example.guide" || body.Title != "Guide Test" || body.Source != "package" || body.Format != "markdown" {
		t.Fatalf("body = %+v", body)
	}
	if !strings.Contains(body.Markdown, "Press the circle key") {
		t.Fatalf("markdown = %q", body.Markdown)
	}
	if strings.Contains(rec.Body.String(), pkg) {
		t.Fatal("guide response reflected the package path")
	}
}

func TestGuideHandlerDistinguishesMissingAndUnreadable(t *testing.T) {
	reg, pkg := testRegistryWithGuide(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}

	if rec := serveGuide(t, srv, "org.example.absent"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown app status = %d", rec.Code)
	}

	if err := os.Remove(filepath.Join(pkg, manifest.GuideFile)); err != nil {
		t.Fatal(err)
	}
	rec := serveGuide(t, srv, "org.example.guide")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing guide status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not include a user guide") {
		t.Fatalf("missing guide body = %s", rec.Body.String())
	}

	// A directory at the fixed guide name is present but unreadable; it must
	// not become a guide and must not leak the package layout.
	if err := os.Mkdir(filepath.Join(pkg, manifest.GuideFile), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = serveGuide(t, srv, "org.example.guide")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreadable guide status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot be read") {
		t.Fatalf("unreadable guide body = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), pkg) || strings.Contains(rec.Body.String(), manifest.GuideFile) {
		t.Fatalf("unreadable guide leaked package detail: %s", rec.Body.String())
	}
}

func TestGuideHandlerRejectsSymlinkEscape(t *testing.T) {
	reg, pkg := testRegistryWithGuide(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}
	declared := filepath.Join(pkg, manifest.GuideFile)
	if err := os.Remove(declared); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("# secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, declared); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rec := serveGuide(t, srv, "org.example.guide")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("symlink status = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("guide response exposed the symlink target")
	}
}

func TestGuideHandlerServesBuiltinGuide(t *testing.T) {
	root := t.TempDir()
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterBuiltin(&registry.App{Manifest: &manifest.Manifest{
		ID: "capsulad.clock", Name: "Clock", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
	}}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Apps: reg, Logs: logs}
	rec := serveGuide(t, srv, "capsulad.clock")
	if rec.Code != http.StatusOK {
		t.Fatalf("builtin status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"source":"builtin"`) {
		t.Fatalf("builtin source missing: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Troubleshooting") {
		t.Fatal("builtin clock guide missing")
	}
}

func TestGuideRequiresAdminSession(t *testing.T) {
	env := newTestEnv(t)
	reg, _ := testRegistryWithGuide(t)
	env.srv.Apps = reg
	target := env.ts.URL + "/api/apps/org.example.guide/guide"

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
		t.Fatalf("session status = %d", resp.StatusCode)
	}
}