package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"capsulad/internal/websecurity"
	"capsulad/internal/wifi"
)

// fakeWifi is a focused controller: it records commands and returns fixed
// snapshots, so a route test proves the handler's contract rather than the
// Linux backend.
type fakeWifi struct {
	mu       sync.Mutex
	status   wifi.Status
	known    []wifi.KnownNetwork
	scan     []wifi.ScanResult
	scanErr  error
	connects []wifi.ConnectRequest
	forgets  []string
	scans    int
}

func (f *fakeWifi) Status(context.Context) (wifi.Status, error) { return f.status, nil }

func (f *fakeWifi) KnownNetworks(context.Context) ([]wifi.KnownNetwork, error) {
	return f.known, nil
}

func (f *fakeWifi) Scan(context.Context) ([]wifi.ScanResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans++
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	return f.scan, nil
}

func (f *fakeWifi) Connect(_ context.Context, req wifi.ConnectRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, req)
	return nil
}

func (f *fakeWifi) Forget(_ context.Context, ssid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgets = append(f.forgets, ssid)
	return nil
}

func wifiTestEnv(t *testing.T) (*testEnv, *fakeWifi, string) {
	t.Helper()
	env := newTestEnv(t)
	fake := &fakeWifi{
		status: wifi.Status{
			Available: true, State: wifi.StateConnected, Interface: "wlan0",
			SSID: "Home", IPAddress: "192.168.1.20", Security: "wpa2",
			APActive: true, APSSID: "Capsula-1234", APAddress: "192.168.4.1",
			APPortalURL: "http://192.168.4.1/", ScanSupported: true,
		},
		known: []wifi.KnownNetwork{{SSID: "Home", Security: "wpa2", HasPassword: true, Connected: true}},
		scan: []wifi.ScanResult{
			{SSID: "Neighbour", Security: "wpa2", SignalDBM: -52, FrequencyMHz: 2437},
			{SSID: "Home", Security: "wpa2", SignalDBM: -31, FrequencyMHz: 5180, Connected: true},
		},
	}
	env.srv.Wifi = fake
	token, _, err := env.store.CreateAPIKeyWithScopes("installer", []string{websecurity.ScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	return env, fake, token
}

func TestWifiHTTPStatusNeverScans(t *testing.T) {
	env, fake, token := wifiTestEnv(t)
	c := env.client(t)
	code, body, _ := doJSON(t, c, http.MethodGet, env.ts.URL+"/api/wifi", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK {
		t.Fatalf("status route: %d %v", code, body)
	}
	view, _ := body["wifi"].(map[string]any)
	if view["state"] != "connected" || view["ssid"] != "Home" || view["ip_address"] != "192.168.1.20" {
		t.Fatalf("view = %#v", view)
	}
	if view["ap_active"] != true || view["ap_ssid"] != "Capsula-1234" || view["ap_password_hint"] != wifi.APPassword {
		t.Fatalf("access point view = %#v", view)
	}
	networks, _ := view["known_networks"].([]any)
	if len(networks) != 1 {
		t.Fatalf("known networks = %#v", view["known_networks"])
	}
	if !strings.Contains(string(mustJSON(t, body)), "\"has_password\":true") {
		t.Fatalf("known network did not expose has_password: %#v", body)
	}
	if fake.scans != 0 {
		t.Fatalf("status scanned the radio %d time(s)", fake.scans)
	}
}

func TestWifiHTTPRequiresOperatorScope(t *testing.T) {
	env, _, _ := wifiTestEnv(t)
	c := env.client(t)
	statusKey, _, err := env.store.CreateAPIKeyWithScopes("probe", []string{websecurity.ScopeStatus})
	if err != nil {
		t.Fatal(err)
	}
	code, body, _ := doJSON(t, c, http.MethodGet, env.ts.URL+"/api/wifi", nil,
		map[string]string{"Authorization": "Bearer " + statusKey})
	if code != http.StatusForbidden {
		t.Fatalf("status-scoped key reached the Wi-Fi route: %d %v", code, body)
	}
	code, _, _ = doJSON(t, c, http.MethodPost, env.ts.URL+"/api/wifi/scan", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous scan route: %d", code)
	}
}

func TestWifiHTTPScanReturnsResultsAndView(t *testing.T) {
	env, fake, token := wifiTestEnv(t)
	code, body, _ := doJSON(t, env.client(t), http.MethodPost, env.ts.URL+"/api/wifi/scan", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK {
		t.Fatalf("scan route: %d %v", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %#v", body["results"])
	}
	first, _ := results[0].(map[string]any)
	if first["ssid"] != "Neighbour" || first["signal_dbm"] != float64(-52) || first["frequency_mhz"] != float64(2437) {
		t.Fatalf("first result = %#v", first)
	}
	message, _ := body["message"].(string)
	if message != "Scan complete: 2 network(s) found" {
		t.Fatalf("message = %q", message)
	}
	if fake.scans != 1 {
		t.Fatalf("scans = %d", fake.scans)
	}
}

func TestWifiHTTPConnectForwardsAndNeverEchoes(t *testing.T) {
	env, fake, token := wifiTestEnv(t)
	code, body, _ := doJSON(t, env.client(t), http.MethodPost, env.ts.URL+"/api/wifi/connect",
		map[string]string{"ssid": "Home", "passphrase": "s3cret-passphrase", "security": "wpa2"},
		map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK {
		t.Fatalf("connect route: %d %v", code, body)
	}
	if len(fake.connects) != 1 {
		t.Fatalf("connect calls = %d", len(fake.connects))
	}
	got := fake.connects[0]
	if got.SSID != "Home" || got.Passphrase != "s3cret-passphrase" || got.Security != "wpa2" {
		t.Fatalf("forwarded request = %#v", got)
	}
	raw := string(mustJSON(t, body))
	if strings.Contains(raw, "s3cret-passphrase") {
		t.Fatalf("response echoed the passphrase: %s", raw)
	}

	code, body, _ = doJSON(t, env.client(t), http.MethodPost, env.ts.URL+"/api/wifi/connect",
		map[string]string{"ssid": "  "}, map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusBadRequest {
		t.Fatalf("empty SSID: %d %v", code, body)
	}
	if len(fake.connects) != 1 {
		t.Fatalf("empty SSID reached the controller: %#v", fake.connects)
	}
}

func TestWifiHTTPForgetAndMissingController(t *testing.T) {
	env, fake, token := wifiTestEnv(t)
	code, body, _ := doJSON(t, env.client(t), http.MethodPost, env.ts.URL+"/api/wifi/forget",
		map[string]string{"ssid": "Home"}, map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK || fake.forgets[0] != "Home" {
		t.Fatalf("forget route: %d %v forgets=%v", code, body, fake.forgets)
	}

	env.srv.Wifi = nil
	code, body, _ = doJSON(t, env.client(t), http.MethodGet, env.ts.URL+"/api/wifi", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusServiceUnavailable || body["error"] == nil {
		t.Fatalf("missing controller: %d %v", code, body)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWifiHTTPMapsControllerErrors(t *testing.T) {
	env, fake, token := wifiTestEnv(t)
	fake.scanErr = errors.Join(wifi.ErrUnavailable, errors.New("control socket is missing"))
	code, body, _ := doJSON(t, env.client(t), http.MethodPost, env.ts.URL+"/api/wifi/scan", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusServiceUnavailable || body["error"] == nil {
		t.Fatalf("unavailable scan: %d %v", code, body)
	}
}
