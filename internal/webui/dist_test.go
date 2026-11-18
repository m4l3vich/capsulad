package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
)

// The SPA ships a generated subset of Material Symbols Rounded. Guard the
// embedded tree against reintroducing a full family (the bundle previously
// carried rounded, outlined and sharp, 13 MB total) or a stale CSS/hash pair.
func TestEmbeddedDistShipsOneIconSubset(t *testing.T) {
	fonts, err := fs.Glob(distFS, "dist/assets/*.woff2")
	if err != nil {
		t.Fatal(err)
	}
	if len(fonts) != 1 {
		t.Fatalf("embedded dist has %d woff2 fonts, want 1: %v", len(fonts), fonts)
	}
	if !strings.Contains(fonts[0], "material-symbols-rounded-subset") {
		t.Fatalf("unexpected icon font %q", fonts[0])
	}
	info, err := fs.Stat(distFS, fonts[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 64*1024 {
		t.Fatalf("icon subset is %d bytes, want at most 64 KiB", info.Size())
	}

	styles, err := fs.Glob(distFS, "dist/assets/index-*.css")
	if err != nil {
		t.Fatal(err)
	}
	if len(styles) == 0 {
		t.Fatal("embedded dist has no stylesheets")
	}
	referenced := false
	for _, style := range styles {
		css, err := fs.ReadFile(distFS, style)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(css), path.Base(fonts[0])) {
			referenced = true
			break
		}
	}
	if !referenced {
		t.Fatalf("no stylesheet references the emitted font %s", path.Base(fonts[0]))
	}

	err = fs.WalkDir(distFS, "dist", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(distFS, name)
		if err != nil {
			return err
		}
		text := string(data)
		if strings.Contains(text, "material-symbols-outlined") || strings.Contains(text, "material-symbols-sharp") {
			t.Errorf("%s references an unused icon family", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The subset is Apache-2.0. The source tree keeps the licence next to the
// asset, but a released binary only carries the embedded tree, so the served
// dist must contain the licence and a notice naming the font.
func TestEmbeddedDistServesFontLicense(t *testing.T) {
	license, err := fs.ReadFile(distFS, "dist/LICENSE-material-symbols.txt")
	if err != nil {
		t.Fatalf("embedded dist has no font licence: %v", err)
	}
	if !strings.Contains(string(license), "Apache License") || !strings.Contains(string(license), "Version 2.0") {
		t.Fatal("LICENSE-material-symbols.txt is not the Apache-2.0 text")
	}
	notice, err := fs.ReadFile(distFS, "dist/THIRD-PARTY.md")
	if err != nil {
		t.Fatalf("embedded dist has no third-party notice: %v", err)
	}
	for _, want := range []string{"Material Symbols Rounded", "Apache License 2.0", "LICENSE-material-symbols.txt"} {
		if !strings.Contains(string(notice), want) {
			t.Errorf("THIRD-PARTY.md does not mention %q", want)
		}
	}

	handler := (&Server{}).static()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/LICENSE-material-symbols.txt", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /LICENSE-material-symbols.txt = %d, want 200", recorder.Code)
	}
	if recorder.Body.String() != string(license) {
		t.Fatal("served licence differs from the embedded copy")
	}
}
