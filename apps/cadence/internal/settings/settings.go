// Package settings validates the Cadence client's app settings. The settings
// store delivers JSON-encoded values; this package turns them into the one
// runtime configuration the link and the turn engine use, rejecting a value
// with a reason instead of silently accepting something the client cannot
// honour. It never prints the token: errors name a setting, never its value.
package settings

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Setting keys, as declared in manifest.toml.
const (
	KeyServerURL       = "server_url"
	KeyDeviceToken     = "device_token"
	KeyDeviceTokenFile = "device_token_file"
	KeyCAPEM           = "ca_pem"
	KeyHoldReleaseTail = "hold_release_tail_ms"
	KeyFollowUp        = "follow_up"
)

// Defaults and bounds. The hold-release tail is the silence the client streams
// after a hold-to-talk release so the server's recognizer closes the utterance;
// it is bounded because it is dead air the user hears as latency.
const (
	DefaultHoldReleaseTailMS = 1000
	MinHoldReleaseTailMS     = 0
	MaxHoldReleaseTailMS     = 5000
	DefaultFollowUp          = true
)

// Config is the validated runtime configuration. Configured is false when the
// app is ready but has no server or credential yet: it then reports activations
// as unconfigured instead of failing to start (a selected assistant must stay
// available).
type Config struct {
	BaseURL    string // absolute http(s) base, no trailing slash
	Token      string // bearer device token, never logged
	CAPEM      []byte // extra trust roots for a self-hosted CA, nil for the system store
	HoldTail   time.Duration
	FollowUp   bool
	Configured bool
	// Warnings are messages to log once after a successful update (for example
	// plaintext http://). They never contain a secret.
	Warnings []string
}

// Values maps a setting key to the JSON-encoded value the store delivered.
// A missing key means "use the declared default".
type Values map[string]string

// ReadFile resolves the token file. main passes os.ReadFile; tests pass a fake.
type ReadFile func(path string) ([]byte, error)

// ErrUnconfigured marks the normal not-yet-configured state. It is a value,
// not a validation failure: Parse returns a Config with Configured=false.
var ErrUnconfigured = errors.New("settings: no Cadence server configured")

// Parse validates a settings snapshot. A malformed value returns an error and
// the caller keeps its previous configuration; the returned Config is zero.
func Parse(v Values, readFile ReadFile) (Config, error) {
	rawURL, err := textValue(v, KeyServerURL)
	if err != nil {
		return Config{}, err
	}
	rawToken, err := textValue(v, KeyDeviceToken)
	if err != nil {
		return Config{}, err
	}
	tokenFile, err := textValue(v, KeyDeviceTokenFile)
	if err != nil {
		return Config{}, err
	}
	caPEM, err := textValue(v, KeyCAPEM)
	if err != nil {
		return Config{}, err
	}
	tailMS, err := intValue(v, KeyHoldReleaseTail, DefaultHoldReleaseTailMS)
	if err != nil {
		return Config{}, err
	}
	if tailMS < MinHoldReleaseTailMS || tailMS > MaxHoldReleaseTailMS {
		return Config{}, fmt.Errorf("settings: %s must be %d..%d ms", KeyHoldReleaseTail, MinHoldReleaseTailMS, MaxHoldReleaseTailMS)
	}
	followUp, err := boolValue(v, KeyFollowUp, DefaultFollowUp)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		HoldTail: time.Duration(tailMS) * time.Millisecond,
		FollowUp: followUp,
	}

	if pemText := strings.TrimSpace(caPEM); pemText != "" {
		if !hasCertificate(pemText) {
			return Config{}, fmt.Errorf("settings: %s has no PEM CERTIFICATE block", KeyCAPEM)
		}
		cfg.CAPEM = []byte(pemText)
	}

	base, scheme, err := normalizeBase(rawURL)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseURL = base
	if scheme == "http" {
		cfg.Warnings = append(cfg.Warnings,
			"server_url uses plain http://; the device token and microphone audio travel unencrypted on this network")
	}

	token, err := resolveToken(rawToken, strings.TrimSpace(tokenFile), readFile)
	if err != nil {
		return Config{}, err
	}
	cfg.Token = token
	cfg.Configured = cfg.BaseURL != "" && cfg.Token != ""
	return cfg, nil
}

// normalizeBase validates an absolute http(s) URL and returns it without a
// trailing slash. A path prefix is allowed (a reverse proxy may mount the
// server under one); a query, fragment, userinfo or non-http scheme is not.
func normalizeBase(raw string) (base, scheme string, err error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", "", nil
	}
	safe := safeURL(text)
	u, perr := url.Parse(text)
	if perr != nil {
		return "", "", fmt.Errorf("settings: %s %s is not a URL", KeyServerURL, safe)
	}
	switch u.Scheme {
	case "https", "http":
	case "":
		return "", "", fmt.Errorf("settings: %s %s must start with https:// or http://", KeyServerURL, safe)
	default:
		return "", "", fmt.Errorf("settings: %s %s must use https:// or http://, not %q", KeyServerURL, safe, u.Scheme)
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("settings: %s %s has no host", KeyServerURL, safe)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("settings: %s %s must not carry userinfo", KeyServerURL, safe)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("settings: %s %s must not carry a query or fragment", KeyServerURL, safe)
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), u.Scheme, nil
}

// safeURL shortens a value for an error message: userinfo, query and fragment
// are removed so a pasted credential cannot be echoed.
func safeURL(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			raw = raw[:i+3] + rest[at+1:]
		}
	}
	if len(raw) > 120 {
		raw = raw[:120] + "…"
	}
	return raw
}

// resolveToken prefers a token file over the inline masked setting. A file is
// read fresh on every settings update so an operator can rotate it without
// redeploying the package.
func resolveToken(inline, file string, readFile ReadFile) (string, error) {
	if file == "" {
		return strings.TrimSpace(inline), nil
	}
	if readFile == nil {
		return "", errors.New("settings: token file reader is unavailable")
	}
	b, err := readFile(file)
	if err != nil {
		// The path is the operator's own config; the reader's error may quote
		// it, which is not a secret, but it is not useful either.
		return "", fmt.Errorf("settings: cannot read %s", KeyDeviceTokenFile)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("settings: %s is empty", KeyDeviceTokenFile)
	}
	return token, nil
}

func hasCertificate(pemText string) bool {
	rest := []byte(pemText)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" {
			return true
		}
	}
}

func textValue(v Values, key string) (string, error) {
	raw, ok := v[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return "", fmt.Errorf("settings: %s must be a string", key)
	}
	return s, nil
}

func intValue(v Values, key string, def int64) (int64, error) {
	raw, ok := v[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	var n int64
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		return 0, fmt.Errorf("settings: %s must be a whole number", key)
	}
	return n, nil
}

func boolValue(v Values, key string, def bool) (bool, error) {
	raw, ok := v[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return def, nil
	}
	var b bool
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return false, fmt.Errorf("settings: %s must be true or false", key)
	}
	return b, nil
}
