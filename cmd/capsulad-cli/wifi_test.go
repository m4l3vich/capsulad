package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"capsulad/internal/websecurity"
)

// wifiTestServer presents its own certificate and writes it into a temporary
// security store, so the CLI verification path (pin exactly the stored
// certificate) runs exactly as it does on the device.
func wifiTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	dataDir := t.TempDir()
	securityDir := filepath.Join(dataDir, "security")
	if err := os.MkdirAll(securityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(securityDir, "server.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return ts, dataDir
}

func activeWifiKeys(t *testing.T, dataDir string) []websecurity.APIKeyInfo {
	t.Helper()
	store := websecurity.Open(filepath.Join(dataDir, "security"), websecurity.DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	var active []websecurity.APIKeyInfo
	for _, key := range store.ListAPIKeys() {
		if !key.Revoked && strings.HasPrefix(key.Name, "cli-wifi-") {
			active = append(active, key)
		}
	}
	return active
}

// TestWifiStatusRetriesUntilTheDaemonReloads pins the credential-watcher
// handshake: the first request is refused because the daemon has not yet
// reloaded the store, the retry succeeds, and the key is revoked afterwards.
func TestWifiStatusRetriesUntilTheDaemonReloads(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var authHeader string
	ts, dataDir := wifiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		authHeader = r.Header.Get("Authorization")
		first := attempts == 1
		mu.Unlock()
		if first {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"authentication required"}`)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/wifi" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"wifi":{"available":true,"state":"connected","ssid":"Home",
			"ip_address":"192.168.1.20","known_networks":[],"scan_supported":true}}`)
	})

	out, err := captureStdout(t, func() error {
		return runWifi([]string{"status", "-web-url", ts.URL}, dataDir, "", true)
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	mu.Lock()
	if attempts != 2 {
		t.Fatalf("attempts = %d; want a retried 401", attempts)
	}
	if !strings.HasPrefix(authHeader, "Bearer capsulad_") {
		t.Fatalf("Authorization = %q", authHeader)
	}
	mu.Unlock()

	var response struct {
		Wifi wifiView `json:"wifi"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatalf("status JSON: %v: %s", err, out)
	}
	if response.Wifi.State != "connected" || response.Wifi.SSID != "Home" || response.Wifi.IPAddress != "192.168.1.20" {
		t.Fatalf("parsed view = %#v", response.Wifi)
	}
	if active := activeWifiKeys(t, dataDir); len(active) != 0 {
		t.Fatalf("temporary key was not revoked: %#v", active)
	}
}

func TestWifiConnectSendsTheBodyAndNeverPrintsThePassphrase(t *testing.T) {
	var mu sync.Mutex
	var body map[string]string
	ts, dataDir := wifiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/wifi/connect" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":"Wi-Fi network saved and selected: Home","wifi":{"state":"connected","ssid":"Home","known_networks":[]}}`)
	})

	out, err := captureStdout(t, func() error {
		return runWifi([]string{"connect", "Home", "-password", "hunter2-pass", "-security", "wpa2", "-web-url", ts.URL}, dataDir, "", false)
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	mu.Lock()
	got := body
	mu.Unlock()
	if got["ssid"] != "Home" || got["passphrase"] != "hunter2-pass" || got["security"] != "wpa2" {
		t.Fatalf("body = %#v", got)
	}
	if strings.Contains(out, "hunter2-pass") {
		t.Fatalf("passphrase printed: %s", out)
	}
	if !strings.Contains(out, "Wi-Fi network saved and selected: Home") {
		t.Fatalf("message = %q", out)
	}
	if active := activeWifiKeys(t, dataDir); len(active) != 0 {
		t.Fatalf("temporary key was not revoked: %#v", active)
	}
}

func TestWifiConnectRejectsConflictingPasswordFlags(t *testing.T) {
	err := runWifi([]string{"connect", "Home", "-password", "a", "-password-stdin"}, t.TempDir(), "", false)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("conflicting password flags: %v", err)
	}
}

func TestWifiConnectDoesNotEchoPositionalPassphrase(t *testing.T) {
	err := runWifi([]string{"connect", "Home", "hunter2-positional"}, t.TempDir(), "", false)
	if err == nil {
		t.Fatal("a positional passphrase was accepted")
	}
	if strings.Contains(err.Error(), "hunter2-positional") {
		t.Fatalf("error echoed the passphrase: %v", err)
	}
	if !strings.Contains(err.Error(), "password-stdin") {
		t.Fatalf("error = %v; want -password-stdin guidance", err)
	}
}

func TestWifiWebURLRejectsAmbiguity(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8443",
		"https://user:secret@127.0.0.1:8443",
		"https://127.0.0.1:8443/api",
		"https://127.0.0.1:8443/?x=1",
		"https://127.0.0.1:8443/#fragment",
	} {
		if _, err := newWifiAPI(t.TempDir(), "", raw); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
}

func TestWifiRevokeRetriesOnceAndReportsFailure(t *testing.T) {
	calls := 0
	recovering := &wifiAPI{keyName: "cli-wifi-test", revokeFn: func(string) error {
		calls++
		if calls == 1 {
			return errors.New("lock busy")
		}
		return nil
	}}
	if err := recovering.revoke(); err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if calls != 2 {
		t.Fatalf("revoke attempts = %d, want 2", calls)
	}

	calls = 0
	failing := &wifiAPI{keyName: "cli-wifi-test", revokeFn: func(string) error {
		calls++
		return errors.New("still locked")
	}}
	if err := failing.revoke(); err == nil || !strings.Contains(err.Error(), "still locked") {
		t.Fatalf("failed revoke = %v", err)
	}
	if calls != 2 {
		t.Fatalf("failed revoke attempts = %d, want 2", calls)
	}
}

// A cancelled request (the signal path) must still revoke the key the command
// minted before the cancellation.
func TestWifiCancellationRevokesKey(t *testing.T) {
	started := make(chan struct{})
	ts, dataDir := wifiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWifiContext(ctx, []string{"scan", "-web-url", ts.URL}, dataDir, "", false)
	}()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request reported success")
	}
	if active := activeWifiKeys(t, dataDir); len(active) != 0 {
		t.Fatalf("cancelled invocation left a temporary key: %#v", active)
	}
}

func TestWifiPinsTheStoredCertificate(t *testing.T) {
	ts, dataDir := wifiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"wifi":{"state":"connected"}}`)
	})
	// A different self-signed certificate for the same host must not be
	// accepted: only the certificate in the security store is a trust anchor.
	otherStore := websecurity.Open(t.TempDir(), websecurity.DefaultOptions())
	if _, err := otherStore.InitServerCert(websecurity.CertOptions{Hostname: "localhost"}); err != nil {
		t.Fatal(err)
	}
	otherPEM, err := os.ReadFile(otherStore.CertPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "security", "server.crt"), otherPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	err = runWifi([]string{"status", "-web-url", ts.URL}, dataDir, "", false)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("lookalike certificate accepted: %v", err)
	}
	if active := activeWifiKeys(t, dataDir); len(active) != 0 {
		t.Fatalf("failed verification left a temporary key: %#v", active)
	}
}

func TestWifiReportsUninitialisedWebUI(t *testing.T) {
	err := runWifi([]string{"status", "-web-url", "https://127.0.0.1:8443"}, t.TempDir(), "", false)
	if err == nil || !strings.Contains(err.Error(), "auth init") {
		t.Fatalf("uninitialised store: %v", err)
	}
}

func TestReadStdinLine(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		fail  bool
	}{
		{input: "correct horse\n", want: "correct horse"},
		{input: "correct horse\r\n", want: "correct horse"},
		{input: "\n", want: ""},
		{input: "", fail: true},
	} {
		got, err := readStdinLine(strings.NewReader(tc.input))
		if tc.fail {
			if err == nil {
				t.Errorf("input %q: expected an error, got %q", tc.input, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("input %q: got %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
}

// withStdin replaces the process stdin for the duration of fn. The CLI tests
// are not parallel, so this cannot leak into another test.
func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
}

func TestAuthPasswordStdin(t *testing.T) {
	dataDir := t.TempDir()
	if err := runAuth([]string{"init", "-hostname", "localhost", "-password", "a", "-password-stdin"}, dataDir, ""); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("mutually exclusive flags: %v", err)
	}
	withStdin(t, "correct horse\r\n")
	if err := runAuth([]string{"init", "-hostname", "localhost", "-password-stdin"}, dataDir, ""); err != nil {
		t.Fatalf("auth init -password-stdin: %v", err)
	}
	store := websecurity.Open(filepath.Join(dataDir, "security"), websecurity.DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.VerifyPassword("correct horse") {
		t.Fatal("stdin password was not stored")
	}
	withStdin(t, "battery staple\n")
	if err := runAuth([]string{"set-password", "-password-stdin"}, dataDir, ""); err != nil {
		t.Fatalf("auth set-password -password-stdin: %v", err)
	}
	store = websecurity.Open(filepath.Join(dataDir, "security"), websecurity.DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if !store.VerifyPassword("battery staple") {
		t.Fatal("stdin password was not applied")
	}
	withStdin(t, "\n")
	if err := runAuth([]string{"set-password", "-password-stdin"}, dataDir, ""); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty stdin: %v", err)
	}
}
