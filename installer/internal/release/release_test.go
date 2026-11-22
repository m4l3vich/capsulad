package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeArtifact creates deterministic bytes and returns the file plus its
// manifest entry. The path is relative to a release root.
func writeArtifact(t *testing.T, dir, rel string, data []byte) File {
	t.Helper()
	name := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return File{Path: rel, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func marshalManifest(m *Manifest) ([]byte, error) {
	return json.Marshal(m)
}

func TestManifestParseAndValidate(t *testing.T) {
	sum := hex.EncodeToString(make([]byte, 32))
	data := []byte(`{
		"schema": 1,
		"version": "1.2.3",
		"firmwares": {
			"rockchip-stock": {
				"label": "stock",
				"bundle": {"path":"rockchip-stock/deploy-bundle.tar.gz","size":10,"sha256":"` + sum + `"},
				"apps": [{"id":"org.capsulad.simon","version":"0.1.0","path":"rockchip-stock/apps/org.capsulad.simon-0.1.0.tar.gz","size":4,"sha256":"` + sum + `"}]
			}
		}
	}`)
	m, err := Load(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != "1.2.3" {
		t.Fatalf("version = %q", m.Version)
	}
	if _, err := m.App("rockchip-stock", "org.capsulad.simon"); err != nil {
		t.Fatalf("App: %v", err)
	}
	if _, err := m.App("amlogic-alpine", "org.capsulad.simon"); err == nil {
		t.Fatal("expected a missing platform error")
	}
}

func TestManifestRejectsUnsafePathsAndSchema(t *testing.T) {
	sum := hex.EncodeToString(make([]byte, 32))
	for name, doc := range map[string]string{
		"traversal": `{"schema":1,"version":"1","firmwares":{"rockchip-stock":{"bundle":{"path":"../x.tar.gz","size":1,"sha256":"` + sum + `"},"apps":[]}}}`,
		"absolute":  `{"schema":1,"version":"1","firmwares":{"rockchip-stock":{"bundle":{"path":"/x.tar.gz","size":1,"sha256":"` + sum + `"},"apps":[]}}}`,
		"schema":    `{"schema":2,"version":"1","firmwares":{}}`,
		"hash":      `{"schema":1,"version":"1","firmwares":{"rockchip-stock":{"bundle":{"path":"x.tar.gz","size":1,"sha256":"nope"},"apps":[]}}}`,
	} {
		if _, err := Load([]byte(doc)); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestDiscoverLocalAndProvider(t *testing.T) {
	root := t.TempDir()
	bundleData := []byte("bundle-bytes")
	writeArtifact(t, root, "rockchip-stock/deploy-bundle.tar.gz", bundleData)
	appData := []byte("app-bytes")
	writeArtifact(t, root, "rockchip-stock/apps/org.capsulad.simon-0.1.0.tar.gz", appData)
	writeArtifact(t, root, "amlogic-alpine/deploy-bundle.tar.gz", []byte("alpine"))
	writeArtifact(t, root, "amlogic-alpine/apps/org.capsulad.pa-2.0.0.tar.gz", []byte("pa"))

	m, err := LoadDir(root)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if m.Version != "local" {
		t.Fatalf("version = %q", m.Version)
	}
	p, err := m.For("rockchip-stock")
	if err != nil {
		t.Fatal(err)
	}
	if p.Bundle.Size != int64(len(bundleData)) {
		t.Fatalf("bundle size = %d", p.Bundle.Size)
	}
	if len(p.Apps) != 1 || p.Apps[0].ID != "org.capsulad.simon" || p.Apps[0].Version != "0.1.0" {
		t.Fatalf("apps = %+v", p.Apps)
	}
	prov := NewLocalProvider(root, m)
	art, err := prov.App(context.Background(), "rockchip-stock", "org.capsulad.simon")
	if err != nil {
		t.Fatalf("App: %v", err)
	}
	if art.Path == "" {
		t.Fatal("empty artifact path")
	}
	// Corrupting the tree after discovery must be refused, not used.
	if err := os.WriteFile(filepath.Join(root, "rockchip-stock/deploy-bundle.tar.gz"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Bundle(context.Background(), "rockchip-stock"); err == nil {
		t.Fatal("expected a hash mismatch for a modified artifact")
	}
}

func TestFetchOverTLSAndCache(t *testing.T) {
	root := t.TempDir()
	bundle := writeArtifact(t, root, "rockchip-stock/deploy-bundle.tar.gz", []byte("verified bundle"))
	manifest := &Manifest{
		Schema:  Schema,
		Version: "9.9.9",
		Firmwares: map[string]Platform{
			"rockchip-stock": {
				Label:  "stock",
				Bundle: bundle,
				Apps:   []App{},
			},
		},
	}
	data, err := marshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/latest/download/release.json" {
			w.Write(data)
			return
		}
		if r.URL.Path == "/latest/download/rockchip-stock/deploy-bundle.tar.gz" {
			w.Write([]byte("verified bundle"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	f := NewFetcher(srv.URL+"/latest/download/", t.TempDir())
	f.Client = srv.Client()
	got, err := f.FetchManifest(context.Background())
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	path, err := f.Artifact(context.Background(), got.Version, got.Firmwares["rockchip-stock"].Bundle, BundleMaxBytes)
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "verified bundle" {
		t.Fatalf("downloaded %q", body)
	}
	before := requests
	if _, err := f.Artifact(context.Background(), got.Version, got.Firmwares["rockchip-stock"].Bundle, BundleMaxBytes); err != nil {
		t.Fatalf("cached Artifact: %v", err)
	}
	if requests != before {
		t.Fatalf("cache miss: %d new requests", requests-before)
	}
}

func TestFetchRejectsOversizeAndHashMismatch(t *testing.T) {
	payload := []byte("0123456789")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	f := NewFetcher(srv.URL+"/", t.TempDir())
	f.Client = srv.Client()
	sum := sha256.Sum256([]byte("different"))
	file := File{Path: "x/bundle.tar.gz", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}
	if _, err := f.Artifact(context.Background(), "1", file, BundleMaxBytes); err == nil {
		t.Fatal("expected a hash mismatch")
	}
	// Declared size over the cap is refused before the body is read.
	big := File{Path: "x/bundle.tar.gz", Size: BundleMaxBytes + 1, SHA256: file.SHA256}
	if _, err := f.Artifact(context.Background(), "1", big, BundleMaxBytes); err == nil {
		t.Fatal("expected a size-cap refusal")
	}
	// A server that streams more than the manifest said is cut off.
	short := File{Path: "x/bundle.tar.gz", Size: 4, SHA256: file.SHA256}
	if _, err := f.Artifact(context.Background(), "1", short, 4); err == nil {
		t.Fatal("expected an over-limit stream to fail")
	}
}

func TestStageReleaseMergesPlatforms(t *testing.T) {
	out := t.TempDir()
	bundleRel := writeArtifact(t, out, "src/deploy-bundle.tar.gz", []byte("bundle"))
	appARel := writeArtifact(t, out, "src/apps/org.capsulad.simon-0.1.0.tar.gz", []byte("simon"))
	appBRel := writeArtifact(t, out, "src/apps/org.capsulad.radio-0.2.0.tar.gz", []byte("radio"))
	bundle := filepath.Join(out, filepath.FromSlash(bundleRel.Path))
	appA := filepath.Join(out, filepath.FromSlash(appARel.Path))
	appB := filepath.Join(out, filepath.FromSlash(appBRel.Path))

	if _, err := Stage(StageOptions{Out: out, Version: "1.0.0", Platform: "rockchip-stock", Bundle: bundle, Apps: []string{appA, appB}}); err != nil {
		t.Fatalf("Stage rockchip: %v", err)
	}
	alpineRel := writeArtifact(t, out, "src2/deploy-bundle.tar.gz", []byte("alpine bundle"))
	alpineBundle := filepath.Join(out, filepath.FromSlash(alpineRel.Path))
	if _, err := Stage(StageOptions{Out: out, Version: "1.0.0", Platform: "amlogic-alpine", Bundle: alpineBundle, Apps: []string{appA}}); err != nil {
		t.Fatalf("Stage alpine: %v", err)
	}
	m, err := LoadFile(filepath.Join(out, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Firmwares) != 2 {
		t.Fatalf("firmwares = %v", m.Firmwares)
	}
	if len(m.Firmwares["rockchip-stock"].Apps) != 2 {
		t.Fatalf("rockchip apps were dropped: %+v", m.Firmwares["rockchip-stock"].Apps)
	}
	if _, err := os.Stat(filepath.Join(out, "amlogic-alpine/apps/org.capsulad.simon-0.1.0.tar.gz")); err != nil {
		t.Fatalf("staged app missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "rockchip-stock/deploy-bundle.tar.gz")); err != nil {
		t.Fatalf("staged bundle missing: %v", err)
	}

	// A bundle-only platform is valid and writes an empty app array.
	stockRel := writeArtifact(t, out, "src3/deploy-bundle.tar.gz", []byte("stock bundle"))
	stockBundle := filepath.Join(out, filepath.FromSlash(stockRel.Path))
	if _, err := Stage(StageOptions{Out: out, Version: "1.0.0", Platform: "amlogic-stock", Bundle: stockBundle}); err != nil {
		t.Fatalf("Stage bundle-only: %v", err)
	}
	m, err = LoadFile(filepath.Join(out, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if apps := m.Firmwares["amlogic-stock"].Apps; apps == nil || len(apps) != 0 {
		t.Fatalf("bundle-only apps = %#v", apps)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	if got := DefaultBaseURL(""); got != "https://github.com/m4l3vich/capsulad/releases/latest/download/" {
		t.Fatalf("latest = %q", got)
	}
	if got := DefaultBaseURL("v1.2"); got != "https://github.com/m4l3vich/capsulad/releases/download/v1.2/" {
		t.Fatalf("tag = %q", got)
	}
}

func TestManifestRejectsTrailingAndOversizedArtifacts(t *testing.T) {
	sum := hex.EncodeToString(make([]byte, 32))
	base := `{"schema":1,"version":"1","firmwares":{"rockchip-stock":{"bundle":{"path":"b.tar.gz","size":1,"sha256":"` + sum + `"},"apps":[]}}}`
	if _, err := Load([]byte(base)); err != nil {
		t.Fatalf("base manifest: %v", err)
	}
	if _, err := Load([]byte(base + " garbage")); err == nil {
		t.Fatal("expected trailing JSON to be refused")
	}
	bigBundle := `{"schema":1,"version":"1","firmwares":{"rockchip-stock":{"bundle":{"path":"b.tar.gz","size":` +
		fmt.Sprint(BundleMaxBytes+1) + `,"sha256":"` + sum + `"},"apps":[]}}}`
	if _, err := Load([]byte(bigBundle)); err == nil {
		t.Fatal("expected an oversized bundle to be refused")
	}
	bigApp := `{"schema":1,"version":"1","firmwares":{"rockchip-stock":{"bundle":{"path":"b.tar.gz","size":1,"sha256":"` + sum +
		`"},"apps":[{"id":"x","version":"1","path":"x.tar.gz","size":` + fmt.Sprint(AppMaxBytes+1) + `,"sha256":"` + sum + `"}]}}}`
	if _, err := Load([]byte(bigApp)); err == nil {
		t.Fatal("expected an oversized app to be refused")
	}
}
