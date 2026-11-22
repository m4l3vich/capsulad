package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"capsulad/internal/websecurity"
)

const defaultWifiWebURL = "https://127.0.0.1:8443"

// The Wi-Fi commands drive the daemon's existing HTTPS administration API.
// They mint a short-lived operator key from the local security store, trust the
// server certificate the store already names, and revoke the key on every path.
// No second control plane and no insecure TLS mode.

type wifiView struct {
	Available      bool               `json:"available"`
	State          string             `json:"state"`
	Interface      string             `json:"interface"`
	SSID           string             `json:"ssid"`
	IPAddress      string             `json:"ip_address"`
	Security       string             `json:"security"`
	Detail         string             `json:"detail"`
	APActive       bool               `json:"ap_active"`
	APSSID         string             `json:"ap_ssid"`
	APAddress      string             `json:"ap_address"`
	APPortalURL    string             `json:"ap_portal_url"`
	APPasswordHint string             `json:"ap_password_hint"`
	ScanSupported  bool               `json:"scan_supported"`
	LastError      string             `json:"last_error"`
	KnownNetworks  []wifiKnownNetwork `json:"known_networks"`
}

type wifiKnownNetwork struct {
	SSID        string `json:"ssid"`
	Security    string `json:"security"`
	HasPassword bool   `json:"has_password"`
	Connected   bool   `json:"connected"`
}

type wifiScanResult struct {
	SSID         string `json:"ssid"`
	Security     string `json:"security"`
	SignalDBM    int    `json:"signal_dbm"`
	FrequencyMHz uint32 `json:"frequency_mhz"`
	Connected    bool   `json:"connected"`
}

type wifiStatusResponse struct {
	Wifi wifiView `json:"wifi"`
}

type wifiScanResponse struct {
	Message string           `json:"message"`
	Results []wifiScanResult `json:"results"`
	Wifi    wifiView         `json:"wifi"`
}

type wifiActionResponse struct {
	Message string   `json:"message"`
	Wifi    wifiView `json:"wifi"`
}

// wifiAPI is the authenticated HTTP client bound to one temporary operator key.
type wifiAPI struct {
	baseURL string
	token   string
	client  *http.Client
	// store and keyName keep the temporary credential revocable even when the
	// HTTP call fails. revokeFn is a test seam; production uses store.
	store    *websecurity.Store
	keyName  string
	revokeFn func(string) error
}

// runWifi binds the command to the terminal's lifetime. A cancelled request
// returns through the deferred revocation instead of orphaning its temporary
// operator key.
func runWifi(args []string, dataDir, securityDir string, jsonOut bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWifiContext(ctx, args, dataDir, securityDir, jsonOut)
}

func runWifiContext(ctx context.Context, args []string, dataDir, securityDir string, jsonOut bool) (err error) {
	if len(args) == 0 {
		return fmt.Errorf("usage: capsulad-cli wifi <status|scan|connect|forget>")
	}
	sub := args[0]
	args = args[1:]
	fs := flag.NewFlagSet("wifi "+sub, flag.ContinueOnError)
	webURL := fs.String("web-url", defaultWifiWebURL, "Web UI base URL")
	var password string
	passwordStdin := false
	var security string
	switch sub {
	case "status", "scan", "forget":
	case "connect":
		fs.StringVar(&password, "password", "", "network passphrase (prefer -password-stdin)")
		fs.BoolVar(&passwordStdin, "password-stdin", false, "read the passphrase from the first stdin line")
		fs.StringVar(&security, "security", "", "security mode (default wpa2; use open for an open network)")
	default:
		return fmt.Errorf("unknown wifi command %q", sub)
	}
	positionals, err := parseSubcommandFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		if sub == "connect" {
			return fmt.Errorf("too many arguments; use -password-stdin to pass the passphrase")
		}
		return fmt.Errorf("too many arguments")
	}
	ssid := ""
	if len(positionals) == 1 {
		ssid = strings.TrimSpace(positionals[0])
	}
	if (sub == "connect" || sub == "forget") && ssid == "" {
		return fmt.Errorf("usage: capsulad-cli wifi %s SSID [-web-url URL]", sub)
	}
	if sub == "connect" && password != "" && passwordStdin {
		return fmt.Errorf("use either -password or -password-stdin, not both")
	}
	if passwordStdin {
		line, err := readSecretFromStdin("Wi-Fi passphrase: ")
		if err != nil {
			return err
		}
		if line == "" {
			return fmt.Errorf("passphrase on stdin is empty")
		}
		password = line
	}
	if sub == "status" || sub == "scan" {
		if ssid != "" {
			return fmt.Errorf("unexpected argument %q", ssid)
		}
	}

	api, err := newWifiAPI(dataDir, securityDir, *webURL)
	if err != nil {
		return err
	}
	defer func() {
		if revokeErr := api.revoke(); revokeErr != nil {
			if err == nil {
				err = fmt.Errorf("revoke temporary operator key: %w", revokeErr)
			} else {
				err = fmt.Errorf("%w (also could not revoke the temporary operator key: %v)", err, revokeErr)
			}
		}
	}()

	var cancel context.CancelFunc
	var raw []byte
	switch sub {
	case "status":
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		raw, err = api.do(ctx, http.MethodGet, "/api/wifi", nil)
	case "scan":
		ctx, cancel = context.WithTimeout(ctx, 45*time.Second)
		raw, err = api.do(ctx, http.MethodPost, "/api/wifi/scan", nil)
	case "connect":
		ctx, cancel = context.WithTimeout(ctx, 70*time.Second)
		raw, err = api.do(ctx, http.MethodPost, "/api/wifi/connect", map[string]string{
			"ssid": ssid, "passphrase": password, "security": security,
		})
	case "forget":
		ctx, cancel = context.WithTimeout(ctx, 35*time.Second)
		raw, err = api.do(ctx, http.MethodPost, "/api/wifi/forget", map[string]string{"ssid": ssid})
	}
	cancel()
	if err != nil {
		return err
	}
	if jsonOut {
		return printIndentedJSON(raw)
	}
	switch sub {
	case "status":
		var resp wifiStatusResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("unexpected daemon response: %w", err)
		}
		printWifiView(resp.Wifi)
	case "scan":
		var resp wifiScanResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("unexpected daemon response: %w", err)
		}
		fmt.Println(resp.Message)
		for _, result := range resp.Results {
			fmt.Printf("  %-32s %-8s %d dBm", result.SSID, result.Security, result.SignalDBM)
			if result.Connected {
				fmt.Print("  connected")
			}
			fmt.Println()
		}
	case "connect", "forget":
		var resp wifiActionResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("unexpected daemon response: %w", err)
		}
		fmt.Println(resp.Message)
	}
	return nil
}

func printWifiView(view wifiView) {
	fmt.Printf("state:        %s\n", view.State)
	if view.Interface != "" {
		fmt.Printf("interface:    %s\n", view.Interface)
	}
	if view.SSID != "" {
		fmt.Printf("SSID:         %s\n", view.SSID)
	}
	if view.IPAddress != "" {
		fmt.Printf("IP address:   %s\n", view.IPAddress)
	}
	if view.Security != "" {
		fmt.Printf("security:     %s\n", view.Security)
	}
	if view.Detail != "" {
		fmt.Printf("detail:       %s\n", view.Detail)
	}
	if view.LastError != "" {
		fmt.Printf("last error:   %s\n", view.LastError)
	}
	if view.APActive {
		fmt.Println("access point: active")
		if view.APSSID != "" {
			fmt.Printf("  SSID:       %s\n", view.APSSID)
		}
		if view.APAddress != "" {
			fmt.Printf("  address:    %s\n", view.APAddress)
		}
		if view.APPortalURL != "" {
			fmt.Printf("  portal:     %s\n", view.APPortalURL)
		}
		if view.APPasswordHint != "" {
			fmt.Printf("  password:   %s\n", view.APPasswordHint)
		}
	}
	fmt.Printf("scan supported: %v\n", view.ScanSupported)
	if len(view.KnownNetworks) == 0 {
		fmt.Println("known networks: (none)")
		return
	}
	fmt.Println("known networks:")
	for _, network := range view.KnownNetworks {
		state := "password not saved"
		if network.HasPassword {
			state = "password saved"
		}
		if network.Connected {
			state += ", connected"
		}
		fmt.Printf("  %-32s %-8s %s\n", network.SSID, network.Security, state)
	}
}

// newWifiAPI pins the device certificate, refusing every other chain, and mints
// the temporary operator key. No insecure mode exists.
func newWifiAPI(dataDir, securityDir, webURL string) (*wifiAPI, error) {
	u, err := url.Parse(webURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid -web-url %q", webURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("-web-url must use https")
	}
	if u.User != nil {
		return nil, fmt.Errorf("-web-url must not include credentials")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("-web-url must not include a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("-web-url must not include a query or fragment")
	}
	store := securityStore(resolveDataDir(dataDir), securityDir)
	if err := store.Load(); err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(store.CertPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("the Web UI is not initialised; run `capsulad-cli auth init` on the device")
		}
		return nil, fmt.Errorf("read server certificate: %w", err)
	}
	// Only the device certificate is trusted. Adding it to the system roots
	// would let any system-trusted certificate for the same host impersonate
	// the daemon.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("server certificate %s is not a usable PEM certificate; run `capsulad-cli auth init` on the device", store.CertPath())
	}

	// Withdraw keys left behind by an invocation that could not revoke (for
	// example a killed process) without touching a concurrent fresh key.
	revokeStaleWifiKeys(store)

	// A random name keeps concurrent installer sessions from colliding.
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	name := wifiKeyPrefix + hex.EncodeToString(suffix[:])
	token, _, err := store.CreateAPIKeyWithScopes(name, []string{"operator"})
	if err != nil {
		return nil, fmt.Errorf("mint temporary operator key: %w", err)
	}
	return &wifiAPI{
		baseURL: "https://" + u.Host,
		token:   token,
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		}}},
		store:   store,
		keyName: name,
	}, nil
}

const (
	wifiKeyPrefix     = "cli-wifi-"
	wifiKeyStaleAfter = 10 * time.Minute
)

// revokeStaleWifiKeys withdraws abandoned temporary keys older than the grace
// window. A concurrent invocation's fresh key is left alone. Failures are not
// fatal: the current invocation's own revocation is what must be guaranteed.
func revokeStaleWifiKeys(store *websecurity.Store) {
	now := time.Now().UTC()
	for _, key := range store.ListAPIKeys() {
		if key.Revoked || !strings.HasPrefix(key.Name, wifiKeyPrefix) {
			continue
		}
		if now.Sub(key.CreatedAt) < wifiKeyStaleAfter {
			continue
		}
		_ = store.RevokeAPIKey(key.Name)
	}
}

// revoke withdraws the temporary credential even when the command failed. It
// retries once, then returns the failure so the command exits non-zero instead
// of claiming success with a live key behind it.
func (a *wifiAPI) revoke() error {
	if a.keyName == "" {
		return nil
	}
	revoke := a.revokeFn
	if revoke == nil {
		if a.store == nil {
			return nil
		}
		revoke = a.store.RevokeAPIKey
	}
	firstErr := revoke(a.keyName)
	if firstErr == nil {
		return nil
	}
	time.Sleep(50 * time.Millisecond)
	if retryErr := revoke(a.keyName); retryErr != nil {
		return fmt.Errorf("first attempt: %v; retry: %w", firstErr, retryErr)
	}
	return nil
}

func (a *wifiAPI) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var payload []byte
	var err error
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return nil, a.transportError(err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && time.Now().Before(deadline) {
			// The daemon reloads the credential store asynchronously.
			select {
			case <-time.After(50 * time.Millisecond):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			var failure struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
				return nil, errors.New(failure.Error)
			}
			return nil, fmt.Errorf("daemon returned %s", resp.Status)
		}
		return data, nil
	}
}

func (a *wifiAPI) transportError(err error) error {
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return fmt.Errorf("the server certificate is not valid for %q; use https://127.0.0.1:8443 or a name the certificate covers", hostname.Host)
	}
	var unknown x509.UnknownAuthorityError
	var verify *tls.CertificateVerificationError
	if errors.As(err, &unknown) || errors.As(err, &verify) {
		return fmt.Errorf("the server certificate could not be verified; if the Web UI is not initialised run `capsulad-cli auth init` on the device")
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return fmt.Errorf("the Web UI did not answer with TLS; if it is not initialised run `capsulad-cli auth init` on the device")
	}
	return fmt.Errorf("cannot reach the daemon at %s: %w", a.baseURL, err)
}

func printIndentedJSON(raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("unexpected daemon response: %w", err)
	}
	buf.WriteByte('\n')
	_, err := os.Stdout.Write(buf.Bytes())
	return err
}
