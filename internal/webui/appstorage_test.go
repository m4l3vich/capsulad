package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/registry"
)

// The install dialog reads the applications filesystem, not "/": a data
// partition with a package reserve would otherwise look far larger than it is.
// The uninstall confirmation reads one package's own size so it can say what
// removing it releases.

func testRegistryWithPackage(t *testing.T) (*registry.Registry, string, int64) {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "org.example.sized")
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `id = "org.example.sized"
name = "Sized Test"
version = "1.0.0"

[entrypoint]
runtime = "native"
path = "bin/app"
architecture = "aarch64"
abi = "linux-gnu"
`
	if err := os.WriteFile(filepath.Join(pkg, "manifest.toml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "bin", "app"), []byte(strings.Repeat("a", 1000)), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink must not be followed: its target may be anywhere on the host.
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte(strings.Repeat("b", 2<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pkg, "outside")); err != nil {
		t.Fatal(err)
	}
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		t.Fatal(err)
	}
	return reg, pkg, int64(len(doc) + 1000)
}

func serveAppStorage(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/apps/storage", nil)
	rec := httptest.NewRecorder()
	srv.appStorage(rec, req)
	return rec
}

func TestAppStorageReportsApplicationsFilesystem(t *testing.T) {
	reg, _, _ := testRegistryWithPackage(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}

	rec := serveAppStorage(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	available, _ := body["available"].(bool)
	if runtime.GOOS == "linux" {
		if !available {
			t.Fatal("linux host did not report the applications filesystem")
		}
		total, _ := body["total_bytes"].(float64)
		free, _ := body["free_bytes"].(float64)
		used, _ := body["used_bytes"].(float64)
		if total <= 0 || free < 0 || used != total-free {
			t.Fatalf("storage = total %v free %v used %v", total, free, used)
		}
		return
	}
	if available {
		t.Fatal("non-Linux host presented host storage as appliance capacity")
	}
	for _, key := range []string{"total_bytes", "free_bytes", "used_bytes"} {
		if value, _ := body[key].(float64); value != 0 {
			t.Fatalf("%s = %v, want 0 when unavailable", key, value)
		}
	}
}

func TestAppPackageStorageSizesOnePackageWithoutFollowingSymlinks(t *testing.T) {
	reg, _, want := testRegistryWithPackage(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/apps/org.example.sized/storage", nil)
	req.SetPathValue("appID", "org.example.sized")
	srv.appPackageStorage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	if got, _ := body["size_bytes"].(float64); int64(got) != want {
		t.Fatalf("size_bytes = %v, want %d", body["size_bytes"], want)
	}
	if _, ok := body["free_bytes"]; !ok {
		t.Fatalf("free_bytes missing: %v", body)
	}
	if _, ok := body["total_bytes"]; !ok {
		t.Fatalf("total_bytes missing: %v", body)
	}
	if _, ok := body["available"].(bool); !ok {
		t.Fatalf("available missing: %v", body)
	}
}

func TestAppPackageStorageRejectsUnknownApp(t *testing.T) {
	reg, _, _ := testRegistryWithPackage(t)
	logs, _ := logbus.New(ids.NewClock(), 20, "")
	srv := &Server{Apps: reg, Logs: logs}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/apps/org.example.missing/storage", nil)
	req.SetPathValue("appID", "org.example.missing")
	srv.appPackageStorage(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAppStorageWithoutRegistryIsHonest keeps the dialog's "Unavailable" path
// reachable: a daemon that was built without a registry does not invent a
// capacity.
func TestAppStorageWithoutRegistryIsHonest(t *testing.T) {
	srv := &Server{}
	rec := serveAppStorage(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeJSONBody(t, rec)
	if available, _ := body["available"].(bool); available {
		t.Fatal("registry-less server claimed storage availability")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/apps/org.example.sized/storage", nil)
	req.SetPathValue("appID", "org.example.sized")
	rec = httptest.NewRecorder()
	srv.appPackageStorage(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestPackageDirSizeRejectsMissingTree keeps a stale package directory from
// reporting a fabricated zero.
func TestPackageDirSizeRejectsMissingTree(t *testing.T) {
	if _, err := packageDirSize(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing package directory reported a size")
	}
}
