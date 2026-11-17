package webui

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/artifact"
)

// The browser package routes are the only way an administrator installs or
// removes an application without a shell. These tests pin the authenticated
// upload contract (the archive reaches the daemon byte for byte), the response
// the page needs to phrase the outcome, and the error mapping: a daemon refusal
// stays a client-visible 4xx with the daemon's own text, while an unexpected
// failure never reflects host detail.

func TestAppsRoutesRequireBrowserSession(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)

	code, _ := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps", strings.NewReader("x"), "application/gzip", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated install = %d, want 401", code)
	}
	code, _ = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/apps/org.example.player", nil, "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated remove = %d, want 401", code)
	}

	// An API key is an automation read credential, never package authority.
	apiKey, _, err := env.store.CreateAPIKey("apps-test")
	if err != nil {
		t.Fatal(err)
	}
	code, _ = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps", strings.NewReader("x"), "application/gzip",
		map[string]string{"X-API-Key": apiKey})
	if code != http.StatusUnauthorized {
		t.Fatalf("API-key install = %d, want 401", code)
	}

	// A session without the CSRF token proves no same-origin intent.
	loginSession(t, env, c)
	code, _ = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps", strings.NewReader("x"), "application/gzip", nil)
	if code != http.StatusForbidden {
		t.Fatalf("install without CSRF = %d, want 403", code)
	}
}

func TestAppInstallStreamsArchiveAndReportsOutcome(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	archive := []byte("\x1f\x8b\x08\x00package-bytes")
	code, body := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps",
		bytes.NewReader(archive), "application/gzip", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("install = %d %v, want 200", code, body)
	}
	if got := body["app_id"]; got != "org.example.new" {
		t.Fatalf("app_id = %v, want org.example.new", got)
	}
	if got := body["version"]; got != "1.2.0" {
		t.Fatalf("version = %v, want 1.2.0", got)
	}
	if got := body["previous_version"]; got != "1.1.0" {
		t.Fatalf("previous_version = %v, want 1.1.0", got)
	}
	if got, _ := body["replaced"].(bool); !got {
		t.Fatalf("replaced = %v, want true", body["replaced"])
	}

	env.back.mu.Lock()
	received := append([]byte(nil), env.back.installedArchive...)
	env.back.mu.Unlock()
	if !bytes.Equal(received, archive) {
		t.Fatalf("daemon received %q, want the uploaded body %q", received, archive)
	}
}

func TestAppInstallKeepsDaemonRefusalMessage(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	// The daemon refuses before reading the archive; the page must show its
	// explanation, because "stop the app first" is the fix.
	env.back.mu.Lock()
	env.back.installErr = status.Error(codes.FailedPrecondition,
		"app org.example.player is running (run run-2); stop it before replacing the package")
	env.back.mu.Unlock()

	code, body := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps",
		strings.NewReader("archive"), "application/gzip", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusConflict {
		t.Fatalf("running-app install = %d %v, want 409", code, body)
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "is running") || !strings.Contains(message, "run-2") {
		t.Fatalf("error = %q, want the daemon's running-app explanation", message)
	}
}

func TestAppInstallRejectsArchiveOverTheTransportBound(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	csrf := csrfCookie(t, c, env.ts.URL)

	// The route refuses a declared upload larger than the archive bound before
	// reading anything, so an oversized body is never streamed to the device.
	req := httptest.NewRequest(http.MethodPost, "/api/apps", strings.NewReader("archive"))
	req.ContentLength = artifact.MaxArchiveBytes + 1
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Capsulad-CSRF", csrf)
	// The session and CSRF cookies are both required: the middleware compares
	// the header with the request's CSRF cookie.
	u, err := url.Parse(env.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range c.Jar.Cookies(u) {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized install = %d %s, want 413", rec.Code, rec.Body.String())
	}
	env.back.mu.Lock()
	received := len(env.back.installedArchive)
	env.back.mu.Unlock()
	if received != 0 {
		t.Fatalf("daemon received %d archive bytes, want none", received)
	}
}

func TestAppRemoveAddressesOneApp(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	code, body := rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/apps/org.example.player", nil, "",
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("remove = %d %v, want 200", code, body)
	}
	if got := body["app_id"]; got != "org.example.player" {
		t.Fatalf("app_id = %v, want org.example.player", got)
	}
	env.back.mu.Lock()
	calls := append([]string(nil), env.back.uninstallCalls...)
	env.back.mu.Unlock()
	if len(calls) != 1 || calls[0] != "org.example.player" {
		t.Fatalf("uninstall calls = %v, want exactly org.example.player", calls)
	}

	env.back.mu.Lock()
	env.back.uninstallErr = status.Error(codes.FailedPrecondition, "app org.example.player is running (run run-2); stop it before removing")
	env.back.mu.Unlock()
	code, body = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/apps/org.example.player", nil, "",
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusConflict {
		t.Fatalf("running-app remove = %d %v, want 409", code, body)
	}
}

func TestAppInstallHidesUnexpectedFailureDetail(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	env.back.mu.Lock()
	env.back.installErr = errors.New("stage package: mkdir /data/apps/.upload-1: no space left on device")
	env.back.mu.Unlock()

	code, body := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps",
		strings.NewReader("archive"), "application/gzip", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusInternalServerError {
		t.Fatalf("internal failure install = %d %v, want 500", code, body)
	}
	message, _ := body["error"].(string)
	if strings.Contains(message, "/data") || strings.Contains(message, "mkdir") {
		t.Fatalf("error = %q, want no host path", message)
	}
}

// csrfCookie returns the CSRF cookie value the session middleware issued, so a
// test can build a request outside the browser's cookie jar.
func csrfCookie(t *testing.T, c *http.Client, target string) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range c.Jar.Cookies(u) {
		if cookie.Name == "capsulad_csrf" {
			return cookie.Value
		}
	}
	t.Fatal("no CSRF cookie in the jar")
	return ""
}

func TestAppInstallMapsAdmissionRefusalToRetryableStatus(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	// The device admits one package staging tree at a time; a second upload is
	// refused while the first is unpacking. That is retryable, not an unknown
	// server error, so the page can say so.
	env.back.mu.Lock()
	env.back.installErr = status.Error(codes.ResourceExhausted, "another package is being installed; try again when it finishes")
	env.back.mu.Unlock()

	code, body := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps",
		strings.NewReader("archive"), "application/gzip", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusTooManyRequests {
		t.Fatalf("busy install = %d %v, want 429", code, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "another package is being installed") {
		t.Fatalf("error = %q, want the admission message", message)
	}
}
