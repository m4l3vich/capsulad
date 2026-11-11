package webui

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	mediav1 "capsulad/gen/go/capsula/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/websecurity"
)

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func newAuthTest(t *testing.T) (*websecurity.Store, *httptest.Server) {
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
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	srv, err := New("127.0.0.1:0", tlsCfg, nil, logs, "test", store)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)
	return store, ts
}

func newClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func get(t *testing.T, c *http.Client, url string, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func post(t *testing.T, c *http.Client, target string, form url.Values, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func csrfOf(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("no CSRF token in page")
	}
	return m[1]
}

func TestPasswordLoginSessionAndCSRF(t *testing.T) {
	store, ts := newAuthTest(t)
	c := newClient(t, ts)

	code, _, hdr := get(t, c, ts.URL+"/", nil)
	if code != http.StatusSeeOther || !strings.Contains(hdr.Get("Location"), "/login") {
		t.Fatalf("unauthenticated page: %d %s", code, hdr.Get("Location"))
	}
	code, body, _ := get(t, c, ts.URL+"/login", nil)
	if code != 200 {
		t.Fatalf("login page: %d", code)
	}
	token := csrfOf(t, body)

	form := url.Values{"csrf": {token}, "password": {"wrong"}}
	code, body, _ = post(t, c, ts.URL+"/login", form, nil)
	if code != http.StatusUnauthorized || !strings.Contains(body, "incorrect password") {
		t.Fatalf("wrong password: %d", code)
	}

	form.Set("password", "correct horse battery")
	code, _, hdr = post(t, c, ts.URL+"/login", form, map[string]string{"Origin": "null"})
	if code != http.StatusSeeOther {
		t.Fatalf("login: %d", code)
	}
	if !strings.Contains(hdr.Get("Location"), "/") {
		t.Fatalf("login redirect: %s", hdr.Get("Location"))
	}
	code, body, _ = get(t, c, ts.URL+"/access", nil)
	if code != 200 || !strings.Contains(body, "self-signed") || !strings.Contains(body, "Administrator password") {
		t.Fatalf("security page after login: %d", code)
	}
	fingerprint, _ := store.CertStatus()
	if !strings.Contains(body, fingerprint.Fingerprint) {
		t.Fatal("certificate fingerprint missing from security page")
	}

	// A concrete mismatched origin is rejected even with a valid CSRF token.
	code, _, _ = post(t, c, ts.URL+"/access/password", url.Values{
		"csrf": {csrfOf(t, body)}, "current": {"correct horse battery"},
		"next": {"another long password"}, "confirm": {"another long password"},
	}, map[string]string{"Origin": "https://attacker.example"})
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin POST accepted: %d", code)
	}

	// Session POST without CSRF is rejected.
	code, _, _ = post(t, c, ts.URL+"/access/password", url.Values{
		"current": {"correct horse battery"}, "next": {"another long password"}, "confirm": {"another long password"},
	}, nil)
	if code != http.StatusForbidden {
		t.Fatalf("password change without CSRF: %d", code)
	}
	// With CSRF it succeeds and rotates the session.
	code, _, hdr = post(t, c, ts.URL+"/access/password", url.Values{
		"csrf": {csrfOf(t, body)}, "current": {"correct horse battery"},
		"next": {"another long password"}, "confirm": {"another long password"},
	}, nil)
	if code != http.StatusSeeOther {
		t.Fatalf("password change: %d", code)
	}
	if !store.VerifyPassword("another long password") {
		t.Fatal("new password not stored")
	}
	code, _, _ = get(t, c, ts.URL+"/access", nil)
	if code != 200 {
		t.Fatalf("session was not reissued after password change: %d", code)
	}

	// Logout ends the session.
	code, body, _ = get(t, c, ts.URL+"/access", nil)
	code, _, hdr = post(t, c, ts.URL+"/logout", url.Values{"csrf": {csrfOf(t, body)}}, nil)
	if code != http.StatusSeeOther || !strings.Contains(hdr.Get("Location"), "/login") {
		t.Fatalf("logout: %d %s", code, hdr.Get("Location"))
	}
	code, _, _ = get(t, c, ts.URL+"/access", nil)
	if code != http.StatusSeeOther {
		t.Fatalf("logout did not revoke session: %d", code)
	}
}

func TestStaticAPIKeyAuthAndRevocation(t *testing.T) {
	store, ts := newAuthTest(t)
	token, _, err := store.CreateAPIKey("automation")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, ts)
	headers := map[string]string{"X-API-Key": token}

	code, body, _ := get(t, c, ts.URL+"/access", headers)
	if code != 200 || !strings.Contains(body, "API keys") {
		t.Fatalf("API key GET: %d", code)
	}
	// Key-authenticated POST does not need a CSRF token.
	code, _, _ = post(t, c, ts.URL+"/access/password", url.Values{
		"current": {"correct horse battery"},
		"next":    {"api key rotated password"},
		"confirm": {"api key rotated password"},
	}, headers)
	if code != http.StatusSeeOther {
		t.Fatalf("API key password change: %d", code)
	}
	if !store.VerifyPassword("api key rotated password") {
		t.Fatal("API key password change not applied")
	}
	// Bearer form is accepted too.
	code, _, _ = get(t, c, ts.URL+"/access", map[string]string{"Authorization": "Bearer " + token})
	if code != 200 {
		t.Fatalf("Bearer auth GET: %d", code)
	}
	// Invalid key is denied, not silently treated as anonymous.
	code, _, _ = get(t, c, ts.URL+"/access", map[string]string{"X-API-Key": "capsula_invalid"})
	if code != http.StatusSeeOther {
		t.Fatalf("invalid key was not denied: %d", code)
	}
	if err := store.RevokeAPIKey("automation"); err != nil {
		t.Fatal(err)
	}
	code, _, hdr := get(t, c, ts.URL+"/access", headers)
	if code != http.StatusSeeOther || !strings.Contains(hdr.Get("Location"), "/login") {
		t.Fatalf("revoked key accepted: %d", code)
	}
}

func TestUnauthenticatedJSONIs401(t *testing.T) {
	_, ts := newAuthTest(t)
	c := newClient(t, ts)
	code, _, _ := get(t, c, ts.URL+"/api/status", map[string]string{"Accept": "application/json"})
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status: %d", code)
	}
}

// The overview template is the only place media metadata and the Bluetooth
// state are formatted; a nil-safe render here guards the page contract.
func TestOverviewRendersMediaBluetoothAndApps(t *testing.T) {
	s, err := New("127.0.0.1:0", nil, nil, nil, "test", nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	page := pageData{Title: "Overview", Nav: "overview", CSRF: "tok", Data: overviewData{
		Bluetooth: &frameworkv1.BluetoothStatus{Present: true, Powered: true, Discoverable: true, Adapter: "hci0", Detail: "adapter reachable"},
		Providers: []mediaProvider{{AppID: "org.example.player", Name: "Player", Players: []*mediav1.PlayerStatus{{
			PlayerId: "p1", PlaybackState: mediav1.PlaybackState_PLAYBACK_PLAYING,
			Title: protoString("Scarlet Fire"), Artist: protoString("Otis McDonald"),
		}}}},
		Apps: []*frameworkv1.InstalledApplication{{AppId: "org.example.player", DisplayName: "Player app", Version: "1.0.0"}},
	}}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "overview", page); err != nil {
		t.Fatalf("render overview: %v", err)
	}
	body := buf.String()
	for _, want := range []string{"Scarlet Fire", "Otis McDonald", "discoverable", "Player app", "Stop advertising"} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview missing %q", want)
		}
	}
}

func protoString(v string) *string { return &v }
