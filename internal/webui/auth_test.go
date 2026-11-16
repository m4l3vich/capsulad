package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/uichanges"
	"capsulad/internal/websecurity"
)

type testEnv struct {
	store *websecurity.Store
	ts    *httptest.Server
	back  *fakeBackend
	srv   *Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	store := websecurity.Open(t.TempDir(), websecurity.DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitServerCert(websecurity.CertOptions{Hostname: "localhost", IPs: []string{"127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := store.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	back := newFakeBackend()
	conn := startFakeGRPC(t, back)
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	srv, err := New("127.0.0.1:8443", tlsCfg, conn, logs, "test", store, nil, uichanges.New())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{store: store, ts: ts, back: back, srv: srv}
}

func (e *testEnv) client(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := e.ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func doJSON(t *testing.T, c *http.Client, method, target string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

func csrfToken(t *testing.T, c *http.Client, target string) string {
	t.Helper()
	code, body, _ := doJSON(t, c, http.MethodGet, target+"/api/session", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("session endpoint: %d", code)
	}
	token, _ := body["csrf"].(string)
	if token == "" {
		t.Fatal("session endpoint did not return a CSRF token")
	}
	return token
}

func loginSession(t *testing.T, e *testEnv, c *http.Client) string {
	t.Helper()
	token := csrfToken(t, c, e.ts.URL)
	code, body, _ := doJSON(t, c, http.MethodPost, e.ts.URL+"/api/login",
		map[string]string{"password": "correct horse battery", "next": "/settings"},
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("login: %d %v", code, body)
	}
	if next, _ := body["next"].(string); next != "/settings" {
		t.Fatalf("login next = %q", next)
	}
	u, _ := url.Parse(e.ts.URL)
	for _, cookie := range c.Jar.Cookies(u) {
		if cookie.Name == websecurity.SessionCookieName() {
			return cookie.Value
		}
	}
	t.Fatal("login did not set a session cookie")
	return ""
}

func TestSPAAndSessionFlow(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)

	resp, err := c.Get(env.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `id="app"`) {
		t.Fatalf("SPA shell: %d %s", resp.StatusCode, raw)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	// Deep links are served the shell as well.
	resp, err = c.Get(env.ts.URL + "/applications/capsulad.clock")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `id="app"`) {
		t.Fatalf("deep link shell: %d", resp.StatusCode)
	}

	code, body, _ := doJSON(t, c, http.MethodGet, env.ts.URL+"/api/session", nil, nil)
	if code != http.StatusOK || body["authenticated"] != false {
		t.Fatalf("unauthenticated session: %d %v", code, body)
	}
	token, _ := body["csrf"].(string)
	if token == "" {
		t.Fatal("no CSRF token for the SPA")
	}

	code, body, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/login",
		map[string]string{"password": "wrong"}, map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusUnauthorized || body["error"] != "incorrect password" {
		t.Fatalf("wrong password: %d %v", code, body)
	}
	code, _, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/login",
		map[string]string{"password": "correct horse battery"}, map[string]string{"Origin": "https://attacker.example", "X-Capsulad-CSRF": token})
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin login accepted: %d", code)
	}
	code, _, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/login",
		map[string]string{"password": "correct horse battery"}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("login without CSRF accepted: %d", code)
	}

	loginSession(t, env, c)
	code, body, _ = doJSON(t, c, http.MethodGet, env.ts.URL+"/api/session", nil, nil)
	if code != http.StatusOK || body["authenticated"] != true {
		t.Fatalf("authenticated session: %d %v", code, body)
	}
	code, _, _ = doJSON(t, c, http.MethodGet, env.ts.URL+"/api/status", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("session /api/status: %d", code)
	}

	// Password change rotates the session and requires CSRF.
	token = csrfToken(t, c, env.ts.URL)
	code, body, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/password",
		map[string]string{"current": "correct horse battery", "next": "another long password", "confirm": "another long password"},
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("password change: %d %v", code, body)
	}
	if !env.store.VerifyPassword("another long password") {
		t.Fatal("new password not stored")
	}
	code, body, _ = doJSON(t, c, http.MethodGet, env.ts.URL+"/api/session", nil, nil)
	if code != http.StatusOK || body["authenticated"] != true {
		t.Fatalf("session not reissued after password change: %d %v", code, body)
	}

	code, _, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/logout", nil,
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("logout: %d", code)
	}
	code, body, _ = doJSON(t, c, http.MethodGet, env.ts.URL+"/api/session", nil, nil)
	if body["authenticated"] != false {
		t.Fatalf("logout did not revoke the session: %d %v", code, body)
	}
}

func TestAPIKeyBoundary(t *testing.T) {
	env := newTestEnv(t)
	token, _, err := env.store.CreateAPIKey("automation")
	if err != nil {
		t.Fatal(err)
	}
	c := env.client(t)
	headers := map[string]string{"X-API-Key": token}

	code, _, _ := doJSON(t, c, http.MethodGet, env.ts.URL+"/api/status", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("API key status: %d", code)
	}
	code, _, _ = doJSON(t, c, http.MethodGet, env.ts.URL+"/api/cert.pem", nil, headers)
	if code != http.StatusOK {
		t.Fatalf("API key certificate: %d", code)
	}
	// API keys are not browser-admin authority.
	code, _, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/password",
		map[string]string{"current": "correct horse battery", "next": "abcdefgh", "confirm": "abcdefgh"},
		headers)
	if code != http.StatusUnauthorized {
		t.Fatalf("API key reached password change: %d", code)
	}
	code, _, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/logout", nil, headers)
	if code != http.StatusUnauthorized {
		t.Fatalf("API key reached logout: %d", code)
	}
	resp, err := env.ts.Client().Get(env.ts.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API key reached WebSocket: %d", resp.StatusCode)
	}
	if err := env.store.RevokeAPIKey("automation"); err != nil {
		t.Fatal(err)
	}
	code, _, _ = doJSON(t, c, http.MethodGet, env.ts.URL+"/api/status", nil, headers)
	if code != http.StatusUnauthorized {
		t.Fatalf("revoked API key accepted: %d", code)
	}
	if _, _, err := env.store.CreateAPIKey(""); err == nil {
		t.Fatal("empty key name should fail")
	}
}
