package wifi

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/pbkdf2"
)

const (
	SecurityOpen     = "open"
	SecurityWPA      = "wpa"
	SecurityWPA2     = "wpa2"
	SecurityWPA2WPA3 = "wpa2_wpa3"
	SecurityWPA3     = "wpa3"
	SecurityWEP      = "wep"
)

// storedNetwork is the durable form. A passphrase is never stored: only the
// derived 64-hex PSK that wpa_supplicant needs is persisted, in a 0600 file.
type storedNetwork struct {
	SSID            string `json:"ssid"`
	Security        string `json:"security"`
	PSK             string `json:"psk,omitempty"`
	Priority        int    `json:"priority"`
	LastConnectedAt int64  `json:"last_connected_unix,omitempty"`
}

type storeDoc struct {
	Version  int             `json:"version"`
	Networks []storedNetwork `json:"networks"`
}

type networkStore struct {
	dir  string
	path string
	mu   sync.Mutex
}

// openNetworkStore creates the private Wi-Fi state directory. The file holds
// derived PSKs, so both the directory and file are owner-only.
func openNetworkStore(root string) (*networkStore, error) {
	dir := filepath.Join(root, "wifi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wifi: create state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wifi: secure state directory: %w", err)
	}
	s := &networkStore{dir: dir, path: filepath.Join(dir, "networks.json")}
	if info, err := os.Lstat(s.path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("wifi: %s is a symlink", s.path)
		}
		if err := os.Chmod(s.path, 0o600); err != nil {
			return nil, fmt.Errorf("wifi: secure network store: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("wifi: inspect network store: %w", err)
	}
	return s, nil
}

func (s *networkStore) list() ([]storedNetwork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *networkStore) replace(list []storedNetwork) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(list)
}

func (s *networkStore) stockMarkerPath() string {
	return filepath.Join(s.dir, ".stock-imported")
}

func (s *networkStore) stockImported() bool {
	_, err := os.Stat(s.stockMarkerPath())
	return err == nil
}

func (s *networkStore) markStockImported() error {
	return writePrivateFile(s.stockMarkerPath(), "1\n")
}

// quarantine moves an unreadable credential file aside instead of leaving the
// speaker unable to start its setup AP. The bytes are preserved for recovery.
func (s *networkStore) quarantine() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Lstat(s.path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	dst := s.path + ".corrupt-" + fmt.Sprint(time.Now().UnixNano())
	return os.Rename(s.path, dst)
}

func (s *networkStore) loadLocked() ([]storedNetwork, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wifi: read network store: %w", err)
	}
	var doc storeDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("wifi: parse network store: %w", err)
	}
	if doc.Version != 0 && doc.Version != 1 {
		return nil, fmt.Errorf("wifi: unsupported network store version %d", doc.Version)
	}
	out := make([]storedNetwork, 0, len(doc.Networks))
	for _, n := range doc.Networks {
		security, err := NormalizeSecurity(n.Security)
		if err != nil || security == SecurityWEP {
			continue
		}
		if !validSSID(n.SSID) {
			continue
		}
		if security != SecurityOpen && !validPSK(n.PSK) {
			continue
		}
		n.Security = security
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].SSID < out[j].SSID
	})
	return out, nil
}

// upsert stores a validated network and returns the updated list. The
// passphrase is converted to a PSK before this call.
func (s *networkStore) upsert(n storedNetwork) ([]storedNetwork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	// wpa_supplicant parses priority as a signed int. Unix seconds stay inside
	// the 32-bit range on every supported build and are sufficient to prefer
	// the most recently saved network.
	n.Priority = int(time.Now().Unix())
	n.LastConnectedAt = time.Now().Unix()
	replaced := false
	for i := range list {
		if list[i].SSID != n.SSID {
			continue
		}
		// Preserve the previous priority ordering for a reconnect that did not
		// change the credential; a newly saved credential becomes most recent.
		if list[i].PSK != n.PSK || list[i].Security != n.Security {
			list[i].Priority = n.Priority
		} else {
			n.Priority = list[i].Priority
		}
		list[i] = n
		replaced = true
		break
	}
	if !replaced {
		list = append(list, n)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Priority != list[j].Priority {
			return list[i].Priority > list[j].Priority
		}
		return list[i].SSID < list[j].SSID
	})
	if err := s.saveLocked(list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *networkStore) remove(ssid string) ([]storedNetwork, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadLocked()
	if err != nil {
		return nil, false, err
	}
	out := list[:0]
	found := false
	for _, n := range list {
		if n.SSID == ssid {
			found = true
			continue
		}
		out = append(out, n)
	}
	if !found {
		return list, false, nil
	}
	if err := s.saveLocked(out); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (s *networkStore) saveLocked(list []storedNetwork) error {
	// Callers hold s.mu; the temp-file install keeps a crash from leaving a
	// partial credential file.
	doc := storeDoc{Version: 1, Networks: list}
	data, err := json.MarshalIndent(&doc, "", "  ")
	if err != nil {
		return fmt.Errorf("wifi: encode network store: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(s.dir, ".networks-*")
	if err != nil {
		return fmt.Errorf("wifi: stage network store: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("wifi: secure staged network store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("wifi: write network store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("wifi: sync network store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wifi: close staged network store: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("wifi: install network store: %w", err)
	}
	return nil
}

// NormalizeSecurity maps UI and scan vocabulary to one canonical value.
func NormalizeSecurity(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", SecurityWPA2:
		return SecurityWPA2, nil
	case SecurityOpen, "none":
		return SecurityOpen, nil
	case SecurityWPA, "wpa-psk":
		return SecurityWPA, nil
	case SecurityWPA2WPA3, "wpa2-psk", "wpa2_psk", "wpa2/wpa3", "wpa2-wpa3", "sae+wpa2":
		if strings.Contains(strings.ToLower(raw), "sae") || strings.Contains(strings.ToLower(raw), "wpa3") {
			return SecurityWPA2WPA3, nil
		}
		return SecurityWPA2, nil
	case SecurityWPA3, "sae":
		return SecurityWPA3, nil
	case SecurityWEP:
		return SecurityWEP, nil
	default:
		return "", fmt.Errorf("%w: security %q is not supported", ErrInvalidNetwork, raw)
	}
}

func validSSID(ssid string) bool {
	if len(ssid) == 0 || len(ssid) > 32 || !utf8.ValidString(ssid) {
		return false
	}
	for _, r := range ssid {
		if r == 0 || r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}

func validPSK(psk string) bool {
	if len(psk) != 64 {
		return false
	}
	_, err := hex.DecodeString(psk)
	return err == nil
}

// derivePSK implements the IEEE 802.11i PBKDF2-HMAC-SHA1 passphrase-to-PSK
// function. A 64-hex input is treated as an already-derived PSK so an advanced
// caller can provision one without a passphrase being stored.
func derivePSK(ssid, passphrase string) (string, error) {
	if len(passphrase) == 64 {
		if _, err := hex.DecodeString(passphrase); err == nil {
			return strings.ToLower(passphrase), nil
		}
	}
	if len(passphrase) < 8 || len(passphrase) > 63 {
		return "", fmt.Errorf("%w: passphrase must be 8-63 characters", ErrInvalidNetwork)
	}
	for _, r := range passphrase {
		if r == 0 || r == '\n' || r == '\r' {
			return "", fmt.Errorf("%w: passphrase contains a control character", ErrInvalidNetwork)
		}
	}
	key := pbkdf2.Key([]byte(passphrase), []byte(ssid), 4096, 32, sha1.New)
	return hex.EncodeToString(key), nil
}

// quoteSSID writes the double-quoted form wpa_supplicant expects. UTF-8 bytes
// pass through; backslash, quote and line controls are escaped.
func quoteSSID(ssid string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(ssid) {
		switch c := ssid[i]; c {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
