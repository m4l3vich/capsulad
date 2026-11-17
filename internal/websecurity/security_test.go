package websecurity

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelfSignedTLSWithoutClientCertificate(t *testing.T) {
	store := Open(t.TempDir(), DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitServerCert(CertOptions{Hostname: "localhost", IPs: []string{"127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	info, err := store.CertStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Fingerprint) != 64 || !info.NotAfter.After(time.Now()) {
		t.Fatalf("unexpected certificate info: %+v", info)
	}
	cfg, err := store.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, cfg)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Error(w, "no TLS", 500)
			return
		}
		if len(r.TLS.PeerCertificates) != 0 {
			http.Error(w, "unexpected client certificate", 500)
			return
		}
		io.WriteString(w, "ok")
	})}
	go func() { _ = srv.Serve(tlsLn) }()
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	}}, Timeout: 3 * time.Second}
	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("self-signed TLS request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("unexpected response %d %q", resp.StatusCode, body)
	}
}

func TestPasswordAndAPIKeys(t *testing.T) {
	dir := t.TempDir()
	store := Open(dir, DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := store.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if !store.VerifyPassword("correct horse battery") || store.VerifyPassword("wrong") {
		t.Fatal("password verification incorrect")
	}
	token, info, err := store.CreateAPIKey("automation")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "capsulad_") || info.Prefix == "" {
		t.Fatalf("unexpected key: %q %+v", token, info)
	}
	if !store.VerifyAPIKey(token) || store.VerifyAPIKey(token+"x") || store.VerifyAPIKey("") {
		t.Fatal("API key verification incorrect")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "correct horse battery") {
		t.Fatal("plaintext secret persisted in credentials.json")
	}
	keys := store.ListAPIKeys()
	if len(keys) != 1 || keys[0].Revoked {
		t.Fatalf("unexpected key list: %+v", keys)
	}
	if err := store.RevokeAPIKey("automation"); err != nil {
		t.Fatal(err)
	}
	if store.VerifyAPIKey(token) {
		t.Fatal("revoked API key accepted")
	}
	if store.ActiveAPIKeyCount() != 0 {
		t.Fatal("active key count incorrect")
	}
	if _, _, err := store.CreateAPIKey("bad name!"); err == nil {
		t.Fatal("invalid key name accepted")
	}
}

func TestAPIKeyScopesAreExplicitAndLegacyKeysStayRestricted(t *testing.T) {
	dir := t.TempDir()
	store := Open(dir, DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}

	// A key created without scopes is the legacy grant: status only.
	legacy, legacyInfo, err := store.CreateAPIKey("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyInfo.Scopes) != 1 || legacyInfo.Scopes[0] != ScopeStatus {
		t.Fatalf("legacy scopes = %v, want [status]", legacyInfo.Scopes)
	}
	_, scopes, ok := store.APIKeyScopes(legacy)
	if !ok || !ScopesAllow(scopes, ScopeStatus) || ScopesAllow(scopes, ScopeOperator) {
		t.Fatalf("legacy key resolves to %v (ok=%v)", scopes, ok)
	}

	// An operator key must be requested explicitly and implies status.
	operator, operatorInfo, err := store.CreateAPIKeyWithScopes("agent", []string{ScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	if len(operatorInfo.Scopes) != 1 || operatorInfo.Scopes[0] != ScopeOperator {
		t.Fatalf("operator scopes = %v", operatorInfo.Scopes)
	}
	_, scopes, ok = store.APIKeyScopes(operator)
	if !ok || !ScopesAllow(scopes, ScopeOperator) || !ScopesAllow(scopes, ScopeStatus) {
		t.Fatalf("operator scopes = %v (ok=%v)", scopes, ok)
	}
	if !store.VerifyAPIKey(legacy) || !store.VerifyAPIKey(operator) {
		t.Fatal("a valid key stopped verifying")
	}

	// Unknown scopes are refused instead of being persisted.
	if _, _, err := store.CreateAPIKeyWithScopes("bad", []string{"root"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	// Duplicates are canonicalized.
	_, deduped, err := store.CreateAPIKeyWithScopes("dedup", []string{ScopeStatus, ScopeStatus})
	if err != nil {
		t.Fatal(err)
	}
	if len(deduped.Scopes) != 1 || deduped.Scopes[0] != ScopeStatus {
		t.Fatalf("deduped scopes = %v", deduped.Scopes)
	}

	// A reload from disk keeps both grants; the legacy key's record has no
	// scopes field at all, so it must resolve exactly as it did before the
	// field existed.
	reloaded := Open(dir, DefaultOptions())
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	_, scopes, ok = reloaded.APIKeyScopes(legacy)
	if !ok || len(scopes) != 1 || scopes[0] != ScopeStatus {
		t.Fatalf("reloaded legacy scopes = %v (ok=%v)", scopes, ok)
	}
	_, scopes, ok = reloaded.APIKeyScopes(operator)
	if !ok || len(scopes) != 1 || scopes[0] != ScopeOperator {
		t.Fatalf("reloaded operator scopes = %v (ok=%v)", scopes, ok)
	}

	// Revocation is immediate for the scope lookup too.
	if err := reloaded.RevokeAPIKey("agent"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reloaded.APIKeyScopes(operator); ok {
		t.Fatal("revoked key still resolves")
	}
}

func TestSessionsExpireAndPasswordChangeRevokes(t *testing.T) {
	store := Open(t.TempDir(), Options{SessionIdle: 50 * time.Millisecond, SessionAbsolute: 150 * time.Millisecond})
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if !store.ValidateSession(token) {
		t.Fatal("fresh session invalid")
	}
	time.Sleep(80 * time.Millisecond)
	if store.ValidateSession(token) {
		t.Fatal("idle-expired session accepted")
	}
	token2, _, _ := store.NewSession()
	if err := store.SetPassword("another long password"); err != nil {
		t.Fatal(err)
	}
	if store.ValidateSession(token2) {
		t.Fatal("session survived password change")
	}
}

func TestReloadAdoptsOutOfProcessCredentialChanges(t *testing.T) {
	dir := t.TempDir()
	daemon := Open(dir, DefaultOptions())
	if err := daemon.Load(); err != nil {
		t.Fatal(err)
	}
	if err := daemon.SetPassword("first local password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := daemon.CreateAPIKey("first-key"); err != nil {
		t.Fatal(err)
	}
	session, _, err := daemon.NewSession()
	if err != nil {
		t.Fatal(err)
	}

	// A separate process (the CLI) opens the same store and rewrites it.
	cli := Open(dir, DefaultOptions())
	if err := cli.Load(); err != nil {
		t.Fatal(err)
	}
	if err := cli.SetPassword("second local password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.CreateAPIKey("second-key"); err != nil {
		t.Fatal(err)
	}

	changed, err := daemon.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("external password change was not reported")
	}
	if daemon.ValidateSession(session) {
		t.Fatal("session survived an out-of-process password change")
	}
	if !daemon.VerifyPassword("second local password") {
		t.Fatal("reloaded password does not verify")
	}
	keys := daemon.ListAPIKeys()
	if len(keys) != 2 || keys[0].Name != "first-key" || keys[1].Name != "second-key" {
		t.Fatalf("reloaded keys = %+v", keys)
	}

	// An API-key-only edit converges without revoking a live session.
	session2, _, err := daemon.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.CreateAPIKey("third-key"); err != nil {
		t.Fatal(err)
	}
	changed, err = daemon.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("API-key edit reported as a password change")
	}
	if !daemon.ValidateSession(session2) {
		t.Fatal("API-key edit revoked a live session")
	}
}
