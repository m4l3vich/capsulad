// Package settings validates Cadence Client's app settings. The settings
// store delivers JSON-encoded values; this package turns them into the one
// runtime configuration the link and the turn engine use, rejecting a value
// with a reason instead of silently accepting something the client cannot
// honour. It never prints the token or the enrollment code: errors name a
// setting, never its value.
package settings

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Setting keys, as declared in manifest.toml.
const (
	KeyServerURL       = "server_url"
	KeyDeviceToken     = "device_token"
	KeyDeviceTokenFile = "device_token_file"
	KeyEnrollCode      = "enroll_code"
	KeyBoard           = "board"
	KeyCAPEM           = "ca_pem"
	KeyHoldReleaseTail = "hold_release_tail_ms"
	KeyFollowUp        = "follow_up"
)

// Board identifies the Cadence board this client reports in Hello and uses in
// an enrollment fingerprint. Auto selects the upstream id for the detected
// hardware: a Capsula Mini reports vk-capsula-mini, the simulator reports sim.
// An explicit value is needed only when the device was enrolled as another
// upstream board, for example the dashboard's default Simulator on real
// hardware.
const (
	BoardAuto          = "auto"
	BoardVKCapsulaMini = "vk-capsula-mini"
	BoardSim           = "sim"
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
	EnrollCode string // one-time dashboard code, exchanged for Token, never logged
	Board      string // board setting: auto, vk-capsula-mini or sim; BoardID resolves it
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

// ReadAsset reads one store-relative asset. main binds it to CAPSULAD_ASSETS;
// tests pass a fake. The path has already passed NormalizeAssetPath.
type ReadAsset func(path string) ([]byte, error)

// ErrUnconfigured marks the normal not-yet-configured state. It is a value,
// not a validation failure: Parse returns a Config with Configured=false.
var ErrUnconfigured = errors.New("settings: no Cadence server configured")

// Parse validates a settings snapshot. A malformed value returns an error and
// the caller keeps its previous configuration; the returned Config is zero.
//
// ca_pem accepts either inline PEM or a path in the shared asset store. A path
// is read through readAsset, so the trust roots an operator uploaded are used
// exactly as the framework stores them; there is no second file API.
func Parse(v Values, readFile ReadFile, readAsset ReadAsset) (Config, error) {
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
	enrollCode, err := textValue(v, KeyEnrollCode)
	if err != nil {
		return Config{}, err
	}
	board, err := boardValue(v)
	if err != nil {
		return Config{}, err
	}
	caText, err := textValue(v, KeyCAPEM)
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
		EnrollCode: strings.TrimSpace(enrollCode),
		Board:      board,
		HoldTail:   time.Duration(tailMS) * time.Millisecond,
		FollowUp:   followUp,
	}

	if ca := strings.TrimSpace(caText); ca != "" {
		switch {
		case hasCertificate(ca):
			cfg.CAPEM = []byte(ca)
		case strings.Contains(ca, "-----BEGIN"):
			// Pasted PEM that lacks a CERTIFICATE block is a mistake in the
			// value, not a path; say so instead of calling it a missing asset.
			return Config{}, fmt.Errorf("settings: %s has no PEM CERTIFICATE block", KeyCAPEM)
		default:
			// Not inline PEM: an asset path is the other accepted form. It is
			// validated here and read through the store so a missing or unsafe
			// file keeps the previous configuration and logs one clear reason.
			rel, err := NormalizeAssetPath(ca)
			if err != nil {
				return Config{}, fmt.Errorf("settings: %s: %w", KeyCAPEM, err)
			}
			if readAsset == nil {
				return Config{}, fmt.Errorf("settings: %s names asset %q, but no asset store is available", KeyCAPEM, rel)
			}
			pemBytes, err := readAsset(rel)
			if err != nil {
				return Config{}, fmt.Errorf("settings: %s asset %q is unavailable", KeyCAPEM, rel)
			}
			if !hasCertificate(string(pemBytes)) {
				return Config{}, fmt.Errorf("settings: %s asset %q has no PEM CERTIFICATE block", KeyCAPEM, rel)
			}
			cfg.CAPEM = pemBytes
		}
	}

	base, scheme, err := normalizeBase(rawURL)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseURL = base
	if scheme == "http" {
		cfg.Warnings = append(cfg.Warnings,
			"server_url uses plain http://; the device token, enrollment code and microphone audio travel unencrypted on this network")
	}

	token, err := resolveToken(rawToken, strings.TrimSpace(tokenFile), readFile)
	if err != nil {
		return Config{}, err
	}
	cfg.Token = token
	cfg.Configured = cfg.BaseURL != "" && cfg.Token != ""
	return cfg, nil
}

// BoardID resolves the board id reported in Hello and used in an enrollment
// fingerprint. An explicit setting wins; auto picks the upstream id for the
// framework's model: the simulator reports sim, every Capsula Mini profile
// reports vk-capsula-mini. A model probe failure therefore never invents an
// unknown board.
func BoardID(setting, model string) string {
	switch setting {
	case BoardSim:
		return BoardSim
	case BoardVKCapsulaMini:
		return BoardVKCapsulaMini
	}
	if strings.Contains(strings.ToLower(model), "simulator") {
		return BoardSim
	}
	return BoardVKCapsulaMini
}

func boardValue(v Values) (string, error) {
	board, err := textValue(v, KeyBoard)
	if err != nil {
		return "", err
	}
	board = strings.TrimSpace(board)
	switch board {
	case "":
		return BoardAuto, nil
	case BoardAuto, BoardVKCapsulaMini, BoardSim:
		return board, nil
	default:
		return "", fmt.Errorf("settings: %s must be %q, %q or %q", KeyBoard, BoardAuto, BoardVKCapsulaMini, BoardSim)
	}
}

// NormalizeAssetPath validates a store-relative asset id with the daemon's own
// grammar: slash separators, no leading slash, no empty, dot or dot-dot
// component, and no reserved upload staging name. It returns the cleaned id.
func NormalizeAssetPath(value string) (string, error) {
	rel := strings.TrimSpace(value)
	if rel == "" {
		return "", errors.New("asset path is empty")
	}
	if strings.IndexByte(rel, 0) >= 0 {
		return "", errors.New("asset path contains a NUL byte")
	}
	if strings.ContainsRune(rel, '\\') {
		return "", errors.New("use / as the path separator")
	}
	if strings.HasPrefix(rel, "/assets/") {
		// The Assets page shows this canonical in-sandbox path; accept it as
		// the alias the framework's ListAssets also accepts.
		rel = strings.TrimPrefix(rel, "/assets/")
	} else if strings.HasPrefix(rel, "/") {
		return "", errors.New("asset path must be relative to the asset store")
	}
	if len(rel) > 4096 {
		return "", errors.New("asset path is too long")
	}
	for _, part := range strings.Split(rel, "/") {
		switch {
		case part == "" || part == "." || part == "..":
			return "", fmt.Errorf("%q is not a valid path component", part)
		case strings.HasPrefix(part, ".capsulad-upload-"):
			return "", fmt.Errorf("%q is reserved for in-flight uploads", part)
		case len(part) > 255:
			return "", errors.New("asset path component is too long")
		}
	}
	return path.Clean(rel), nil
}

// ReadAssetFile reads one store-relative asset below root and confines it to
// that directory: no traversal, no symlink component, and only a regular file.
// It is the read side of the same grammar the framework's asset catalog uses.
func ReadAssetFile(root, value string) ([]byte, error) {
	rel, err := NormalizeAssetPath(value)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	inside, err := filepath.Rel(root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
		return nil, errors.New("asset path is outside the asset store")
	}
	current := root
	for _, part := range strings.Split(rel, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("asset path must not use a symlink")
		}
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("asset is not a regular file")
	}
	return os.ReadFile(full)
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
