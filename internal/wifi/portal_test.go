package wifi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeController struct {
	mu           sync.Mutex
	status       Status
	known        []KnownNetwork
	scans        []ScanResult
	scanCalls    int
	connectCalls []ConnectRequest
	connectCh    chan struct{}
	connectErr   error
}

func (f *fakeController) Status(context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, nil
}

func (f *fakeController) KnownNetworks(context.Context) ([]KnownNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]KnownNetwork(nil), f.known...), nil
}

func (f *fakeController) Scan(context.Context) ([]ScanResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanCalls++
	return append([]ScanResult(nil), f.scans...), nil
}

func (f *fakeController) Connect(_ context.Context, req ConnectRequest) error {
	f.mu.Lock()
	f.connectCalls = append(f.connectCalls, req)
	err := f.connectErr
	ch := f.connectCh
	f.mu.Unlock()
	if ch != nil {
		ch <- struct{}{}
	}
	return err
}

func (f *fakeController) Forget(context.Context, string) error { return nil }

func (f *fakeController) scanCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scanCalls
}

func (f *fakeController) connectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.connectCalls)
}

var tokenRe = regexp.MustCompile(`name="token" value="([^"]+)"`)

func testPortal(t *testing.T, backend *fakeController) (*portal, http.Handler) {
	t.Helper()
	backend.status.ScanSupported = true
	p := &portal{
		backend: backend,
		apAddr:  netip.MustParseAddr("192.168.4.1"),
		prefix:  netip.MustParsePrefix("192.168.4.0/24"),
		port:    80,
		secret:  []byte("0123456789abcdef0123456789abcdef"),
		now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	return p, p.handler()
}

func portalToken(t *testing.T, h http.Handler, remote string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://192.168.4.1/", nil)
	req.RemoteAddr = remote
	req.Host = "192.168.4.1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d: %s", rec.Code, rec.Body.String())
	}
	m := tokenRe.FindStringSubmatch(rec.Body.String())
	if len(m) != 2 {
		t.Fatalf("token not rendered: %s", rec.Body.String())
	}
	return m[1]
}

func portalPost(h http.Handler, path, remote, host, origin string, values url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://192.168.4.1"+path, strings.NewReader(values.Encode()))
	req.RemoteAddr = remote
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPortalPageDoesNotScanAndExposesOnlyProvisioning(t *testing.T) {
	backend := &fakeController{status: Status{
		Available: true, State: StateAPFallback, APActive: true, APSSID: "Capsule-Test",
		Detail: "setup access point", APAddress: "192.168.4.1",
	}}
	_, h := testPortal(t, backend)
	token := portalToken(t, h, "192.168.4.50:1234")
	if token == "" {
		t.Fatal("empty token")
	}
	if backend.scanCount() != 0 {
		t.Fatalf("opening the portal scanned %d times", backend.scanCount())
	}
	req := httptest.NewRequest(http.MethodGet, "http://192.168.4.1/", nil)
	req.RemoteAddr = "192.168.4.50:1234"
	req.Host = "192.168.4.1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"Set up this speaker", "Scan for Wi-Fi networks", "Connect speaker", "capsulad"} {
		if !strings.Contains(body, want) {
			t.Fatalf("portal body missing %q", want)
		}
	}
	// The portal has no admin login, settings or access-key surface.
	admin := httptest.NewRequest(http.MethodPost, "http://192.168.4.1/api/login", strings.NewReader("{}"))
	admin.RemoteAddr = "192.168.4.50:1234"
	admin.Host = "192.168.4.1"
	adminRec := httptest.NewRecorder()
	h.ServeHTTP(adminRec, admin)
	if adminRec.Code == http.StatusOK {
		t.Fatalf("admin login route answered on portal: %d %s", adminRec.Code, adminRec.Body.String())
	}
	if strings.Contains(adminRec.Body.String(), "password") {
		t.Fatal("portal returned an admin credential surface")
	}
}

func TestPortalCaptiveProbeRedirectsAndForeignNetworkIsRejected(t *testing.T) {
	backend := &fakeController{status: Status{Available: true, State: StateAPFallback, APActive: true}}
	p, h := testPortal(t, backend)
	_ = p

	probe := httptest.NewRequest(http.MethodGet, "http://connectivitycheck.gstatic.com/generate_204", nil)
	probe.RemoteAddr = "192.168.4.50:1234"
	probe.Host = "connectivitycheck.gstatic.com"
	probeRec := httptest.NewRecorder()
	h.ServeHTTP(probeRec, probe)
	if probeRec.Code != http.StatusFound || probeRec.Header().Get("Location") != "http://192.168.4.1/" {
		t.Fatalf("probe response = %d location=%q", probeRec.Code, probeRec.Header().Get("Location"))
	}
	if backend.scanCount() != 0 {
		t.Fatal("captive probe scanned")
	}

	outside := httptest.NewRequest(http.MethodGet, "http://192.168.4.1/", nil)
	outside.RemoteAddr = "10.0.0.9:1234"
	outside.Host = "192.168.4.1"
	outsideRec := httptest.NewRecorder()
	h.ServeHTTP(outsideRec, outside)
	if outsideRec.Code != http.StatusForbidden {
		t.Fatalf("foreign network page = %d", outsideRec.Code)
	}
}

func TestPortalRejectsHostOriginTokenAndCrossSiteScan(t *testing.T) {
	backend := &fakeController{
		status: Status{Available: true, State: StateAPFallback, APActive: true},
		scans:  []ScanResult{{SSID: "Neighbour", Security: SecurityWPA2, SignalDBM: -40, FrequencyMHz: 2412}},
	}
	_, h := testPortal(t, backend)
	token := portalToken(t, h, "192.168.4.50:1234")

	wrongHost := httptest.NewRequest(http.MethodGet, "http://10.0.0.1/", nil)
	wrongHost.RemoteAddr = "192.168.4.50:1234"
	wrongHost.Host = "10.0.0.1"
	wrongHostRec := httptest.NewRecorder()
	h.ServeHTTP(wrongHostRec, wrongHost)
	if wrongHostRec.Code != http.StatusBadRequest {
		t.Fatalf("wrong Host = %d", wrongHostRec.Code)
	}

	wrongOrigin := portalPost(h, "/scan", "192.168.4.50:1234", "192.168.4.1", "http://evil.example", url.Values{"token": {token}})
	if wrongOrigin.Code != http.StatusForbidden {
		t.Fatalf("wrong Origin = %d", wrongOrigin.Code)
	}

	otherIP := portalPost(h, "/scan", "192.168.4.51:1234", "192.168.4.1", "", url.Values{"token": {token}})
	if otherIP.Code != http.StatusForbidden {
		t.Fatalf("token from another client = %d", otherIP.Code)
	}

	noToken := portalPost(h, "/scan", "192.168.4.50:1234", "192.168.4.1", "", url.Values{})
	if noToken.Code != http.StatusForbidden {
		t.Fatalf("missing token = %d", noToken.Code)
	}
	crossSite := httptest.NewRequest(http.MethodPost, "http://192.168.4.1/scan", strings.NewReader(url.Values{"token": {token}}.Encode()))
	crossSite.RemoteAddr = "192.168.4.50:1234"
	crossSite.Host = "192.168.4.1"
	crossSite.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSiteRec := httptest.NewRecorder()
	h.ServeHTTP(crossSiteRec, crossSite)
	if crossSiteRec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d", crossSiteRec.Code)
	}
	if backend.scanCount() != 0 {
		t.Fatalf("rejected requests scanned %d times", backend.scanCount())
	}

	good := portalPost(h, "/scan", "192.168.4.50:1234", "192.168.4.1", "http://192.168.4.1", url.Values{"token": {token}})
	if good.Code != http.StatusOK || backend.scanCount() != 1 {
		t.Fatalf("valid scan = %d calls=%d body=%s", good.Code, backend.scanCount(), good.Body.String())
	}
	if !strings.Contains(good.Body.String(), "Neighbour") {
		t.Fatal("scan result missing")
	}
}

func TestPortalConnectFlushesAcceptedPageBeforeBackgroundTeardown(t *testing.T) {
	backend := &fakeController{
		status:    Status{Available: true, State: StateAPFallback, APActive: true},
		connectCh: make(chan struct{}, 1),
	}
	p, h := testPortal(t, backend)
	token := portalToken(t, h, "192.168.4.50:1234")
	flushed := false
	p.beforeConnect = func() { flushed = true }
	rec := portalPost(h, "/connect", "192.168.4.50:1234", "192.168.4.1", "", url.Values{
		"token": {token}, "ssid": {"Home"}, "security": {"wpa2"}, "passphrase": {"correct horse battery"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d: %s", rec.Code, rec.Body.String())
	}
	if !flushed || !rec.Flushed {
		t.Fatal("accepted page was not flushed before connection transition")
	}
	if strings.Contains(rec.Body.String(), "correct horse battery") {
		t.Fatal("portal response echoed the passphrase")
	}
	select {
	case <-backend.connectCh:
	case <-time.After(2 * time.Second):
		t.Fatal("background connect was not started")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.connectCalls) != 1 {
		t.Fatalf("connect calls = %d", len(backend.connectCalls))
	}
	got := backend.connectCalls[0]
	if got.SSID != "Home" || got.Passphrase != "correct horse battery" || got.Security != SecurityWPA2 {
		t.Fatalf("connect request = %+v", got)
	}
}

func TestPortalRejectsInvalidConnectWithoutCallingBackend(t *testing.T) {
	backend := &fakeController{status: Status{Available: true, State: StateAPFallback, APActive: true}}
	_, h := testPortal(t, backend)
	token := portalToken(t, h, "192.168.4.50:1234")
	cases := []url.Values{
		{"token": {token}, "ssid": {""}, "security": {"wpa2"}, "passphrase": {"correct horse"}},
		{"token": {token}, "ssid": {"Home"}, "security": {"wpa2"}, "passphrase": {"short"}},
		{"token": {token}, "ssid": {"Home"}, "security": {"wep"}, "passphrase": {"correct horse"}},
	}
	for _, values := range cases {
		rec := portalPost(h, "/connect", "192.168.4.50:1234", "192.168.4.1", "", values)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "notice") {
			t.Fatalf("invalid connect = %d body=%s", rec.Code, rec.Body.String())
		}
	}
	if backend.connectCount() != 0 {
		t.Fatalf("invalid connect called backend %d times", backend.connectCount())
	}
}

func TestPortalTokenExpiryBoundary(t *testing.T) {
	backend := &fakeController{status: Status{Available: true, State: StateAPFallback, APActive: true}}
	p, h := testPortal(t, backend)
	token := portalToken(t, h, "192.168.4.50:1234")
	p.now = func() time.Time { return time.Unix(1_700_000_000+int64(portalTokenLifetime/time.Second), 0) }
	atBoundary := portalPost(h, "/scan", "192.168.4.50:1234", "192.168.4.1", "", url.Values{"token": {token}})
	if atBoundary.Code != http.StatusOK {
		t.Fatalf("token at its expiry boundary = %d", atBoundary.Code)
	}
	p.now = func() time.Time { return time.Unix(1_700_000_001+int64(portalTokenLifetime/time.Second), 0) }
	expired := portalPost(h, "/scan", "192.168.4.50:1234", "192.168.4.1", "", url.Values{"token": {token}})
	if expired.Code != http.StatusForbidden {
		t.Fatalf("token one second past expiry = %d", expired.Code)
	}
}

func TestPortalTokenExpiresAndGETScanIsNotAllowed(t *testing.T) {
	backend := &fakeController{status: Status{Available: true, State: StateAPFallback, APActive: true}}
	p, h := testPortal(t, backend)
	token := portalToken(t, h, "192.168.4.50:1234")
	p.now = func() time.Time { return time.Unix(1_700_000_000+int64(portalTokenLifetime/time.Second)+120, 0) }
	rec := portalPost(h, "/scan", "192.168.4.50:1234", "192.168.4.1", "", url.Values{"token": {token}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expired token = %d", rec.Code)
	}

	get := httptest.NewRequest(http.MethodGet, "http://192.168.4.1/scan", nil)
	get.RemoteAddr = "192.168.4.50:1234"
	get.Host = "192.168.4.1"
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusMethodNotAllowed && getRec.Code != http.StatusNotFound && getRec.Code != http.StatusFound {
		t.Fatalf("GET /scan = %d", getRec.Code)
	}
	if backend.scanCount() != 0 {
		t.Fatal("GET /scan triggered a scan")
	}
}
