// Package websecurity implements the HTTPS Web UI's local security store:
// a self-signed server certificate, an Argon2id admin password, static opaque
// API keys, and server-side browser sessions.
//
// This deliberately replaces the draft's mandatory client-certificate mTLS:
// the HTTPS listener uses a self-signed certificate, browsers log in with the
// admin password, and automation can present a static API key. Keys and
// passwords are stored only as hashes; the plaintext API key is shown once.
package websecurity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	sessionCookie = "capsulad_session"
	maxSessions   = 128
	maxAPIKeys    = 64
)

type Options struct {
	SessionIdle     time.Duration
	SessionAbsolute time.Duration
}

func DefaultOptions() Options {
	return Options{SessionIdle: 30 * time.Minute, SessionAbsolute: 12 * time.Hour}
}

type Store struct {
	root string
	opts Options

	mu       sync.Mutex
	doc      persisted
	sessions map[string]session
}

type persisted struct {
	Password *passwordRecord `json:"password,omitempty"`
	APIKeys  []apiKeyRecord  `json:"api_keys,omitempty"`
}

type passwordRecord struct {
	Salt    string `json:"salt"`
	Hash    string `json:"hash"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
	KeyLen  uint32 `json:"key_len"`
}

type apiKeyRecord struct {
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Hash      string     `json:"hash"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Scopes    []string   `json:"scopes,omitempty"`
}

type APIKeyInfo struct {
	Name      string
	Prefix    string
	CreatedAt time.Time
	Revoked   bool
	// Scopes is the recorded grant. Missing or invalid scopes give no access.
	Scopes []string
}

// API-key scopes separate status polling, assistant controls, and full
// administration across the HTTP and MCP frontends.
const (
	// ScopeStatus is the read-only service identity: GET /api/status and the
	// public certificate.
	ScopeStatus = "status"
	// ScopeAssistant covers ordinary speaker controls and implies ScopeStatus.
	ScopeAssistant = "assistant"
	// ScopeOperator is full daemon administration, exposed by the MCP frontend
	// as the operator tool set. It implies ScopeAssistant and ScopeStatus.
	ScopeOperator = "operator"
)

// KnownScopes lists the scopes an API key may be issued with.
func KnownScopes() []string { return []string{ScopeStatus, ScopeAssistant, ScopeOperator} }

func scopeLevel(scope string) int {
	switch scope {
	case ScopeStatus:
		return 1
	case ScopeAssistant:
		return 2
	case ScopeOperator:
		return 3
	default:
		return 0
	}
}

// grantLevel returns the highest held scope, or zero for a missing or invalid grant.
func grantLevel(held []string) int {
	level := 0
	for _, scope := range held {
		rank := scopeLevel(scope)
		if rank == 0 {
			return 0
		}
		level = max(level, rank)
	}
	return level
}

// ScopesAllow reports whether a valid grant satisfies a known requirement.
func ScopesAllow(held []string, required string) bool {
	rank := scopeLevel(required)
	return rank != 0 && grantLevel(held) >= rank
}

// normalizeScopes validates and canonicalizes a requested grant. Issuance
// without an explicit scope records a status-only grant.
func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return []string{ScopeStatus}, nil
	}
	seen := make(map[string]bool, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scopeLevel(scope) == 0 {
			return nil, fmt.Errorf("unknown API key scope %q (known: %s)", scope, strings.Join(KnownScopes(), ", "))
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		out = append(out, scope)
	}
	sort.Strings(out)
	return out, nil
}

type session struct {
	created time.Time
	last    time.Time
}

func Open(root string, opts Options) *Store {
	if opts.SessionIdle <= 0 {
		opts = DefaultOptions()
	}
	return &Store{root: root, opts: opts, sessions: map[string]session{}}
}

func (s *Store) Root() string { return s.root }

func (s *Store) CertPath() string        { return filepath.Join(s.root, "server.crt") }
func (s *Store) KeyPath() string         { return filepath.Join(s.root, "server.key") }
func (s *Store) credentialsPath() string { return filepath.Join(s.root, "credentials.json") }

// Load reads persisted credentials. A missing file is a valid uninitialized
// state, not an error.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.credentialsPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc persisted
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse credentials: %w", err)
	}
	s.doc = doc
	return nil
}

// CredentialsPath names the persisted credential document. The daemon watches
// it so an out-of-process local administrator (`capsulad-cli auth ...`) converges
// live sessions without polling.
func (s *Store) CredentialsPath() string { return s.credentialsPath() }

// Reload re-reads credentials changed by another process and reports whether
// the admin password changed. A password change invalidates every in-memory
// session; API-key edits leave sessions intact.
func (s *Store) Reload() (passwordChanged bool, err error) {
	data, err := os.ReadFile(s.credentialsPath())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var doc persisted
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse credentials: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := ""
	if s.doc.Password != nil {
		before = s.doc.Password.Hash
	}
	after := ""
	if doc.Password != nil {
		after = doc.Password.Hash
	}
	s.doc = doc
	if before != after {
		s.sessions = map[string]session{}
		return true, nil
	}
	return false, nil
}

func (s *Store) persistLocked() error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.credentialsPath(), data, 0o600)
}

// --- admin password --------------------------------------------------------

func (s *Store) PasswordSet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc.Password != nil
}

// SetPassword hashes and stores the admin password, invalidating browser
// sessions.
func (s *Store) SetPassword(password string) error {
	if len(password) < 8 {
		return fmt.Errorf("admin password must be at least 8 characters")
	}
	if len(password) > 1024 {
		return fmt.Errorf("admin password is too long")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	const t, memory, threads, keyLen = 3, 32 * 1024, 2, 32
	// One KDF at a time: each call allocates 32 MiB, and two concurrent logins
	// on this 240 MiB board can push the daemon into the OOM killer.
	kdfMu.Lock()
	hash := argon2.IDKey([]byte(password), salt, t, memory, threads, keyLen)
	kdfMu.Unlock()
	rec := &passwordRecord{
		Salt: base64.StdEncoding.EncodeToString(salt),
		Hash: base64.StdEncoding.EncodeToString(hash),
		Time: t, Memory: memory, Threads: threads, KeyLen: keyLen,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.doc.Password
	s.doc.Password = rec
	if err := s.persistLocked(); err != nil {
		s.doc.Password = prev
		return err
	}
	s.sessions = map[string]session{}
	return nil
}

// VerifyPassword checks a candidate in constant time.
func (s *Store) VerifyPassword(password string) bool {
	s.mu.Lock()
	rec := s.doc.Password
	s.mu.Unlock()
	if rec == nil {
		return false
	}
	return verifyRecord(rec, password)
}

func verifyRecord(rec *passwordRecord, password string) bool {
	salt, err := base64.StdEncoding.DecodeString(rec.Salt)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(rec.Hash)
	if err != nil {
		return false
	}
	kdfMu.Lock()
	got := argon2.IDKey([]byte(password), salt, rec.Time, rec.Memory, rec.Threads, rec.KeyLen)
	kdfMu.Unlock()
	return subtle.ConstantTimeCompare(want, got) == 1
}

// kdfMu serializes password hashing, whose working memory is device-scale
// significant.
var kdfMu sync.Mutex

// --- API keys --------------------------------------------------------------

var keyNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// CreateAPIKey issues a status-only key. The plaintext is returned exactly
// once; the stored credential contains only its hash and identifying prefix.
func (s *Store) CreateAPIKey(name string) (string, APIKeyInfo, error) {
	return s.CreateAPIKeyWithScopes(name, nil)
}

// CreateAPIKeyWithScopes issues a key restricted to the given scopes. An empty
// scope list defaults to ScopeStatus at issuance.
func (s *Store) CreateAPIKeyWithScopes(name string, scopes []string) (string, APIKeyInfo, error) {
	if !keyNameRe.MatchString(name) {
		return "", APIKeyInfo{}, fmt.Errorf("key name must match %s", keyNameRe)
	}
	normalized, err := normalizeScopes(scopes)
	if err != nil {
		return "", APIKeyInfo{}, err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", APIKeyInfo{}, err
	}
	token := "capsulad_" + base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(token))
	rec := apiKeyRecord{
		Name: name, Prefix: token[:16], Hash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC(),
		Scopes: normalized,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.doc.APIKeys) >= maxAPIKeys {
		return "", APIKeyInfo{}, fmt.Errorf("maximum of %d API keys reached", maxAPIKeys)
	}
	for _, existing := range s.doc.APIKeys {
		if existing.Name == name {
			return "", APIKeyInfo{}, fmt.Errorf("API key %q already exists", name)
		}
	}
	s.doc.APIKeys = append(s.doc.APIKeys, rec)
	if err := s.persistLocked(); err != nil {
		s.doc.APIKeys = s.doc.APIKeys[:len(s.doc.APIKeys)-1]
		return "", APIKeyInfo{}, err
	}
	return token, infoFromRecord(rec), nil
}

func infoFromRecord(rec apiKeyRecord) APIKeyInfo {
	return APIKeyInfo{
		Name: rec.Name, Prefix: rec.Prefix, CreatedAt: rec.CreatedAt,
		Revoked: rec.RevokedAt != nil, Scopes: append([]string(nil), rec.Scopes...),
	}
}

func (s *Store) VerifyAPIKey(token string) bool {
	_, _, ok := s.APIKeyScopes(token)
	return ok
}

// APIKeyScopes resolves a presented key to its info and grant. Revoked or
// unknown keys and records without a valid explicit grant report ok=false.
func (s *Store) APIKeyScopes(token string) (APIKeyInfo, []string, bool) {
	if token == "" {
		return APIKeyInfo{}, nil, false
	}
	sum := sha256.Sum256([]byte(token))
	got := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.doc.APIKeys {
		if rec.RevokedAt != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(got)) == 1 && grantLevel(rec.Scopes) != 0 {
			info := infoFromRecord(rec)
			return info, info.Scopes, true
		}
	}
	return APIKeyInfo{}, nil, false
}

func (s *Store) ListAPIKeys() []APIKeyInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]APIKeyInfo, 0, len(s.doc.APIKeys))
	for _, rec := range s.doc.APIKeys {
		out = append(out, infoFromRecord(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) RevokeAPIKey(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.doc.APIKeys {
		if s.doc.APIKeys[i].Name != name {
			continue
		}
		if s.doc.APIKeys[i].RevokedAt != nil {
			return nil
		}
		now := time.Now().UTC()
		s.doc.APIKeys[i].RevokedAt = &now
		return s.persistLocked()
	}
	return fmt.Errorf("API key %q not found", name)
}

func (s *Store) ActiveAPIKeyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, rec := range s.doc.APIKeys {
		if rec.RevokedAt == nil && grantLevel(rec.Scopes) != 0 {
			n++
		}
	}
	return n
}

// --- browser sessions ------------------------------------------------------

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) NewSession() (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := time.Now()
	expiry := now.Add(s.opts.SessionAbsolute)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= maxSessions {
		// Evict the least recently used session rather than growing unbounded.
		var oldestKey string
		var oldest time.Time
		for key, sess := range s.sessions {
			if oldestKey == "" || sess.last.Before(oldest) {
				oldestKey, oldest = key, sess.last
			}
		}
		delete(s.sessions, oldestKey)
	}
	s.sessions[hashToken(token)] = session{created: now, last: now}
	return token, expiry, nil
}

func (s *Store) ValidateSession(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	key := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[key]
	if !ok {
		return false
	}
	if now.Sub(sess.last) > s.opts.SessionIdle || now.Sub(sess.created) > s.opts.SessionAbsolute {
		delete(s.sessions, key)
		return false
	}
	sess.last = now
	s.sessions[key] = sess
	return true
}

func (s *Store) RevokeSession(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, hashToken(token))
	s.mu.Unlock()
}

func (s *Store) RevokeAllSessions() {
	s.mu.Lock()
	s.sessions = map[string]session{}
	s.mu.Unlock()
}

// --- self-signed server certificate ---------------------------------------

type CertOptions struct {
	Hostname string
	IPs      []string
	Days     int
}

type CertInfo struct {
	Subject     string
	DNSNames    []string
	IPAddresses []string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string // sha256 hex of the DER certificate
}

func (s *Store) InitServerCert(opts CertOptions) (CertInfo, error) {
	if _, err := os.Stat(s.CertPath()); err == nil {
		return CertInfo{}, fmt.Errorf("server certificate already exists; use renew")
	}
	return s.newServerCert(opts)
}

func (s *Store) RenewServerCert(opts CertOptions) (CertInfo, error) {
	return s.newServerCert(opts)
}

func (s *Store) newServerCert(opts CertOptions) (CertInfo, error) {
	if opts.Days <= 0 {
		opts.Days = 397
	}
	if opts.Hostname == "" {
		opts.Hostname, _ = os.Hostname()
	}
	if opts.Hostname == "" {
		opts.Hostname = "capsula.local"
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CertInfo{}, err
	}
	now := time.Now().UTC()
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	seenIP := map[string]bool{"127.0.0.1": true, "::1": true}
	for _, raw := range opts.IPs {
		if ip := net.ParseIP(raw); ip != nil && !seenIP[ip.String()] {
			seenIP[ip.String()] = true
			ips = append(ips, ip)
		}
	}
	dns := []string{"localhost"}
	if !containsString(dns, opts.Hostname) {
		dns = append(dns, opts.Hostname)
	}
	tpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: opts.Hostname, Organization: []string{"Capsulad Web UI"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(0, 0, opts.Days),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dns,
		IPAddresses:           ips,
		IsCA:                  false,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return CertInfo{}, err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return CertInfo{}, err
	}
	if err := writeAtomic(s.KeyPath(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: mustMarshalKey(key)}), 0o600); err != nil {
		return CertInfo{}, err
	}
	if err := writeAtomic(s.CertPath(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return CertInfo{}, err
	}
	return certInfoFrom(tpl, der), nil
}

func certInfoFrom(cert *x509.Certificate, der []byte) CertInfo {
	sum := sha256.Sum256(der)
	info := CertInfo{
		Subject: cert.Subject.CommonName, DNSNames: append([]string(nil), cert.DNSNames...),
		NotBefore: cert.NotBefore, NotAfter: cert.NotAfter,
		Fingerprint: hex.EncodeToString(sum[:]),
	}
	for _, ip := range cert.IPAddresses {
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}
	return info
}

// CertStatus returns the current certificate metadata.
func (s *Store) CertStatus() (CertInfo, error) {
	data, err := os.ReadFile(s.CertPath())
	if err != nil {
		return CertInfo{}, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return CertInfo{}, fmt.Errorf("server certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CertInfo{}, err
	}
	return certInfoFrom(cert, cert.Raw), nil
}

// TLSConfig builds the self-signed HTTPS configuration. No client certificate
// is requested or required; authentication happens at the HTTP layer.
func (s *Store) TLSConfig() (*tls.Config, error) {
	if _, err := os.Stat(s.CertPath()); err != nil {
		return nil, fmt.Errorf("server certificate missing; run `capsulad-cli auth init`")
	}
	pair, err := tls.LoadX509KeyPair(s.CertPath(), s.KeyPath())
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

// Ready reports whether the Web UI can start: a certificate and password exist.
func (s *Store) Ready() bool {
	if _, err := os.Stat(s.CertPath()); err != nil {
		return false
	}
	if _, err := os.Stat(s.KeyPath()); err != nil {
		return false
	}
	return s.PasswordSet()
}

// Status is a non-secret summary for diagnostics and the SystemService.
type Status struct {
	HTTPSEnabled      bool
	BootstrapComplete bool
	AdminPasswordSet  bool
	ActiveAPIKeys     int
	ListenAddresses   []string
	ServerFingerprint string
	ServerExpiry      time.Time
	ServerDNSNames    []string
	ServerIPAddresses []string
}

func (s *Store) Status(listen []string) Status {
	st := Status{
		HTTPSEnabled: true, BootstrapComplete: s.Ready(), AdminPasswordSet: s.PasswordSet(),
		ActiveAPIKeys: s.ActiveAPIKeyCount(), ListenAddresses: append([]string(nil), listen...),
	}
	if info, err := s.CertStatus(); err == nil {
		st.ServerFingerprint = info.Fingerprint
		st.ServerExpiry = info.NotAfter
		st.ServerDNSNames = info.DNSNames
		st.ServerIPAddresses = info.IPAddresses
	}
	return st
}

// SessionCookieName is shared with the Web UI.
func SessionCookieName() string { return sessionCookie }

// --- helpers ---------------------------------------------------------------

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(err)
	}
	return n
}

func mustMarshalKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err)
	}
	return der
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
