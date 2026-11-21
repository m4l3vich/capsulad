package settings_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capsulad/apps/cadence/internal/settings"
)

func values(kv map[string]string) settings.Values {
	v := make(settings.Values, len(kv))
	for k, val := range kv {
		v[k] = val
	}
	return v
}

func str(s string) string { return `"` + s + `"` }

func TestUnconfiguredIsNotAnError(t *testing.T) {
	cfg, err := settings.Parse(values(nil), nil, nil)
	if err != nil {
		t.Fatalf("empty settings: %v", err)
	}
	if cfg.Configured {
		t.Fatal("empty settings reported configured")
	}
	if cfg.HoldTail != time.Second || !cfg.FollowUp {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestValidHTTPS(t *testing.T) {
	cfg, err := settings.Parse(values(map[string]string{
		"server_url":           str("https://cadence.example.com/"),
		"device_token":         str("tok_live_123"),
		"hold_release_tail_ms": "250",
		"follow_up":            "false",
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://cadence.example.com" || cfg.Token != "tok_live_123" || !cfg.Configured {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.HoldTail != 250*time.Millisecond || cfg.FollowUp {
		t.Fatalf("tuning = %+v", cfg)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings = %v", cfg.Warnings)
	}
}

func TestPlainHTTPWarns(t *testing.T) {
	cfg, err := settings.Parse(values(map[string]string{
		"server_url":   str("http://192.168.1.10:8080"),
		"device_token": str("tok"),
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://192.168.1.10:8080" {
		t.Fatalf("base = %q", cfg.BaseURL)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "unencrypted") {
		t.Fatalf("warnings = %v", cfg.Warnings)
	}
}

func TestPathPrefixAllowed(t *testing.T) {
	cfg, err := settings.Parse(values(map[string]string{
		"server_url":   str("https://example.com/cadence/"),
		"device_token": str("tok"),
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://example.com/cadence" {
		t.Fatalf("base = %q", cfg.BaseURL)
	}
}

func TestRejectsBadURLs(t *testing.T) {
	for name, raw := range map[string]string{
		"no scheme": "cadence.example.com",
		"ws scheme": "ws://cadence.example.com",
		"no host":   "https://",
		"userinfo":  "https://user:pass@cadence.example.com",
		"query":     "https://cadence.example.com/?token=abc",
		"fragment":  "https://cadence.example.com/#x",
		"space":     "https://exa mple.com",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := settings.Parse(values(map[string]string{"server_url": str(raw)}), nil, nil)
			if err == nil {
				t.Fatalf("%q accepted", raw)
			}
			if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "pass@") {
				t.Fatalf("error leaked the value tail: %v", err)
			}
		})
	}
}

func TestTokenFileWins(t *testing.T) {
	read := func(path string) ([]byte, error) {
		if path != "/run/secrets/cadence" {
			t.Fatalf("unexpected path %q", path)
		}
		return []byte("  file-token\n"), nil
	}
	cfg, err := settings.Parse(values(map[string]string{
		"server_url":        str("https://cadence.example.com"),
		"device_token":      str("inline-token"),
		"device_token_file": str("/run/secrets/cadence"),
	}), read, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "file-token" {
		t.Fatalf("token = %q", cfg.Token)
	}
}

func TestTokenFileErrors(t *testing.T) {
	missing := func(string) ([]byte, error) { return nil, errors.New("no such file") }
	_, err := settings.Parse(values(map[string]string{
		"server_url":        str("https://cadence.example.com"),
		"device_token_file": str("/nope"),
	}), missing, nil)
	if err == nil {
		t.Fatal("missing token file accepted")
	}
	empty := func(string) ([]byte, error) { return []byte("  \n"), nil }
	_, err = settings.Parse(values(map[string]string{
		"server_url":        str("https://cadence.example.com"),
		"device_token_file": str("/empty"),
	}), empty, nil)
	if err == nil {
		t.Fatal("empty token file accepted")
	}
}

func TestNoTokenMeansUnconfigured(t *testing.T) {
	cfg, err := settings.Parse(values(map[string]string{
		"server_url": str("https://cadence.example.com"),
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Configured {
		t.Fatal("a server without a token reported configured")
	}
}

func TestHoldTailBounds(t *testing.T) {
	for _, raw := range []string{"-1", "5001"} {
		if _, err := settings.Parse(values(map[string]string{"hold_release_tail_ms": raw}), nil, nil); err == nil {
			t.Fatalf("tail %s accepted", raw)
		}
	}
	cfg, err := settings.Parse(values(map[string]string{"hold_release_tail_ms": "0"}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HoldTail != 0 {
		t.Fatalf("tail = %v", cfg.HoldTail)
	}
}

func TestWrongTypesRejected(t *testing.T) {
	for name, kv := range map[string]map[string]string{
		"server url": {"server_url": "42"},
		"token":      {"device_token": "true"},
		"tail":       {"hold_release_tail_ms": `"250"`},
		"follow up":  {"follow_up": `"yes"`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := settings.Parse(values(kv), nil, nil); err == nil {
				t.Fatalf("%v accepted", kv)
			}
		})
	}
}

const testCert = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----
`

func TestCAPEM(t *testing.T) {
	pemJSON, err := json.Marshal(testCert)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := settings.Parse(values(map[string]string{
		"ca_pem": string(pemJSON),
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.CAPEM) != strings.TrimSpace(testCert) {
		t.Fatal("ca_pem dropped")
	}
	// A value without a PEM block is an asset path, not a certificate.
	_, err = settings.Parse(values(map[string]string{"ca_pem": str("not a pem")}),
		nil, func(string) ([]byte, error) { return nil, errors.New("no such asset") })
	if err == nil {
		t.Fatal("a path with no asset accepted")
	}
	badPEM, err := json.Marshal("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----")
	if err != nil {
		t.Fatal(err)
	}
	_, err = settings.Parse(values(map[string]string{"ca_pem": string(badPEM)}), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "CERTIFICATE") {
		t.Fatalf("non-certificate PEM = %v", err)
	}
}

func TestCAPEMAssetPath(t *testing.T) {
	var read string
	cfg, err := settings.Parse(values(map[string]string{
		"ca_pem": str("tls/cadence-ca.pem"),
	}), nil, func(path string) ([]byte, error) {
		read = path
		return []byte(testCert), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read != "tls/cadence-ca.pem" || string(cfg.CAPEM) != testCert {
		t.Fatalf("path = %q, CAPEM = %d bytes", read, len(cfg.CAPEM))
	}
	// The canonical path shown by the Assets page is an accepted alias.
	if _, err := settings.Parse(values(map[string]string{"ca_pem": str("/assets/tls/cadence-ca.pem")}),
		nil, func(path string) ([]byte, error) {
			if path != "tls/cadence-ca.pem" {
				t.Fatalf("alias resolved to %q", path)
			}
			return []byte(testCert), nil
		}); err != nil {
		t.Fatal(err)
	}

	// The store path grammar is enforced before the reader runs.
	bad := func(string) ([]byte, error) {
		t.Fatal("reader called for an invalid path")
		return nil, nil
	}
	for _, value := range []string{"../secret.pem", "/etc/ssl/ca.pem", "tls//ca.pem",
		`tls\ca.pem`, "tls/./ca.pem", ".capsulad-upload-x"} {
		if _, err := settings.Parse(values(map[string]string{"ca_pem": str(value)}), nil, bad); err == nil {
			t.Fatalf("%q accepted", value)
		}
	}

	// A file that exists but is not a certificate is rejected, not trusted.
	_, err = settings.Parse(values(map[string]string{"ca_pem": str("tls/notes.txt")}),
		nil, func(string) ([]byte, error) { return []byte("not a certificate"), nil })
	if err == nil {
		t.Fatal("an asset without a CERTIFICATE block accepted")
	}
}

func TestBoardAndEnrollCode(t *testing.T) {
	for setting, want := range map[string]string{
		"":                 settings.BoardAuto,
		settings.BoardAuto: settings.BoardAuto,
		"sim":              settings.BoardSim,
		"vk-capsula-mini":  settings.BoardVKCapsulaMini,
	} {
		var raw string
		if setting != "" {
			raw = str(setting)
		}
		cfg, err := settings.Parse(values(map[string]string{"board": raw}), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Board != want {
			t.Fatalf("board %q -> %q, want %q", setting, cfg.Board, want)
		}
	}
	if _, err := settings.Parse(values(map[string]string{"board": str("yandex-midi")}), nil, nil); err == nil {
		t.Fatal("an upstream board this client is not was accepted")
	}

	cfg, err := settings.Parse(values(map[string]string{"enroll_code": str("  K7M4QX  ")}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnrollCode != "K7M4QX" || cfg.Configured {
		t.Fatalf("enroll code = %q configured=%v", cfg.EnrollCode, cfg.Configured)
	}
}

func TestBoardID(t *testing.T) {
	for _, tc := range []struct{ setting, model, want string }{
		{"", "VK Capsula Mini (A113X)", settings.BoardVKCapsulaMini},
		{"", "Capsula Mini (simulator)", settings.BoardSim},
		{"", "", settings.BoardVKCapsulaMini},
		{"auto", "Capsula Mini (simulator)", settings.BoardSim},
		{"sim", "VK Capsula Mini (A113X)", settings.BoardSim},
		{"vk-capsula-mini", "Capsula Mini (simulator)", settings.BoardVKCapsulaMini},
	} {
		if got := settings.BoardID(tc.setting, tc.model); got != tc.want {
			t.Fatalf("BoardID(%q, %q) = %q, want %q", tc.setting, tc.model, got, tc.want)
		}
	}
}

func TestReadAssetFileConfinesToRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tls"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tls", "ca.pem"), []byte(testCert), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := settings.ReadAssetFile(root, "tls/ca.pem")
	if err != nil || string(b) != testCert {
		t.Fatalf("read = %d bytes, %v", len(b), err)
	}
	if _, err := settings.ReadAssetFile(root, "../ca.pem"); err == nil {
		t.Fatal("traversal read accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside.pem")
	if err := os.WriteFile(outside, []byte(testCert), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.ReadAssetFile(root, "link.pem"); err == nil {
		t.Fatal("symlink read accepted")
	}
	if _, err := settings.ReadAssetFile(root, "tls/missing.pem"); err == nil {
		t.Fatal("missing read accepted")
	}
}
