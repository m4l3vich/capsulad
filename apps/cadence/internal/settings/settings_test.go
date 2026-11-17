package settings_test

import (
	"encoding/json"
	"errors"
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
	cfg, err := settings.Parse(values(nil), nil)
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
	}), nil)
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
	}), nil)
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
	}), nil)
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
			_, err := settings.Parse(values(map[string]string{"server_url": str(raw)}), nil)
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
	}), read)
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
	}), missing)
	if err == nil {
		t.Fatal("missing token file accepted")
	}
	empty := func(string) ([]byte, error) { return []byte("  \n"), nil }
	_, err = settings.Parse(values(map[string]string{
		"server_url":        str("https://cadence.example.com"),
		"device_token_file": str("/empty"),
	}), empty)
	if err == nil {
		t.Fatal("empty token file accepted")
	}
}

func TestNoTokenMeansUnconfigured(t *testing.T) {
	cfg, err := settings.Parse(values(map[string]string{
		"server_url": str("https://cadence.example.com"),
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Configured {
		t.Fatal("a server without a token reported configured")
	}
}

func TestHoldTailBounds(t *testing.T) {
	for _, raw := range []string{"-1", "5001"} {
		if _, err := settings.Parse(values(map[string]string{"hold_release_tail_ms": raw}), nil); err == nil {
			t.Fatalf("tail %s accepted", raw)
		}
	}
	cfg, err := settings.Parse(values(map[string]string{"hold_release_tail_ms": "0"}), nil)
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
			if _, err := settings.Parse(values(kv), nil); err == nil {
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
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CAPEM) == 0 {
		t.Fatal("ca_pem dropped")
	}
	if _, err := settings.Parse(values(map[string]string{"ca_pem": str("not a pem")}), nil); err == nil {
		t.Fatal("non-PEM accepted")
	}
}
