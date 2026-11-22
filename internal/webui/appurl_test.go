package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/artifact"
)

// The URL install route is a convenience for an administrator who hosts a
// package elsewhere. It must fetch only http(s), never trust the origin's
// length, and reach the same daemon validation as a browser upload; these
// tests pin those properties and the browser-facing error vocabulary.

func TestAppURLInstallStreamsTheDownloadedArchive(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	archive := []byte("\x1f\x8b\x08\x00download-bytes")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixtures/org.example.new.tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	defer origin.Close()

	code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": origin.URL + "/fixtures/org.example.new.tar.gz"},
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("url install = %d %v, want 200", code, body)
	}
	if got := body["app_id"]; got != "org.example.new" {
		t.Fatalf("app_id = %v", got)
	}
	env.back.mu.Lock()
	received := append([]byte(nil), env.back.installedArchive...)
	env.back.mu.Unlock()
	if !bytes.Equal(received, archive) {
		t.Fatalf("daemon received %q, want %q", received, archive)
	}
}

func TestAppURLInstallRejectsUnsafeURLs(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	cases := map[string]string{
		"file scheme":  "file:///etc/passwd",
		"data scheme":  "data:application/gzip;base64,H4sI",
		"ftp scheme":   "ftp://example.test/pkg.tar.gz",
		"relative":     "/pkg.tar.gz",
		"no host":      "https:///pkg.tar.gz",
		"credentials":  "https://user:secret@example.test/pkg.tar.gz",
		"fragment":     "https://example.test/pkg.tar.gz#frag",
		"blank":        "",
		"not a url at": "http://",
	}
	for name, raw := range cases {
		code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
			map[string]string{"url": raw}, map[string]string{"X-Capsulad-CSRF": token})
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d %v, want 400", name, code, body)
		}
	}
	env.back.mu.Lock()
	received := len(env.back.installedArchive)
	env.back.mu.Unlock()
	if received != 0 {
		t.Fatalf("daemon received %d bytes from rejected URLs", received)
	}
}

func TestAppURLInstallRejectsNonSuccessOrigin(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer origin.Close()

	code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": origin.URL + "/pkg.tar.gz"}, map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusBadGateway {
		t.Fatalf("non-2xx origin = %d %v, want 502", code, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "404") {
		t.Fatalf("error = %q, want the origin status", message)
	}
}

func TestAppURLInstallRejectsDeclaredOversize(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(artifact.MaxArchiveBytes+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": origin.URL + "/huge.tar.gz"}, map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversize = %d %v, want 413", code, body)
	}
	env.back.mu.Lock()
	received := len(env.back.installedArchive)
	env.back.mu.Unlock()
	if received != 0 {
		t.Fatalf("daemon received %d bytes from an oversized download", received)
	}
}

func TestAppURLInstallStopsAChunkedOversizeBody(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	// No Content-Length: the route has to discover the oversize while reading.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := bytes.Repeat([]byte{'a'}, 1<<20)
		for sent := int64(0); sent <= artifact.MaxArchiveBytes; sent += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer origin.Close()

	code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": origin.URL + "/stream.tar.gz"}, map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversize = %d %v, want 413", code, body)
	}
}

func TestAppURLInstallBoundsRedirects(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL+"/again", http.StatusFound)
	}))
	defer origin.Close()

	code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": origin.URL + "/pkg.tar.gz"}, map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusBadGateway {
		t.Fatalf("redirect loop = %d %v, want 502", code, body)
	}
}

func TestAppURLInstallKeepsDaemonRefusal(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("archive"))
	}))
	defer origin.Close()

	env.back.mu.Lock()
	env.back.installErr = status.Error(codes.FailedPrecondition,
		"app org.example.player is running (run run-2); stop it before replacing the package")
	env.back.mu.Unlock()

	code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": origin.URL + "/pkg.tar.gz"}, map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusConflict {
		t.Fatalf("refused install = %d %v, want 409", code, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "is running") {
		t.Fatalf("error = %q, want the daemon refusal", message)
	}
}

func TestAppURLInstallRequiresSessionAndCSRF(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)

	code, _, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": "https://example.test/pkg.tar.gz"}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated url install = %d, want 401", code)
	}

	loginSession(t, env, c)
	code, _, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/apps/url",
		map[string]string{"url": "https://example.test/pkg.tar.gz"}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("url install without CSRF = %d, want 403", code)
	}
}
