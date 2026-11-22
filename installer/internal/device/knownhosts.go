package device

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"capsulad/installer/internal/atomicfile"
	"golang.org/x/crypto/ssh"
)

// HostKeyStatus is the result of checking a presented key against the local
// trust store.
type HostKeyStatus int

const (
	HostKeyKnown HostKeyStatus = iota
	HostKeyNew
	HostKeyMismatch
)

// String names the status for logs and test failures.
func (s HostKeyStatus) String() string {
	switch s {
	case HostKeyKnown:
		return "known"
	case HostKeyNew:
		return "new"
	case HostKeyMismatch:
		return "mismatch"
	}
	return "unknown"
}

// HostKeyEntry is one pinned host key.
type HostKeyEntry struct {
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"key_type"`
	AddedUTC    string `json:"added_utc"`
}

type knownHostsFile struct {
	Version int                     `json:"version"`
	Hosts   map[string]HostKeyEntry `json:"hosts"`
}

// KnownHosts is a small JSON trust store under the user config directory. It
// is deliberately not OpenSSH's known_hosts: the installer owns one file and
// can explain it in one sentence.
type KnownHosts struct {
	mu      sync.Mutex
	path    string
	entries map[string]HostKeyEntry
}

// LoadKnownHosts reads path, creating an empty store when it is absent.
func LoadKnownHosts(path string) (*KnownHosts, error) {
	k := &KnownHosts{path: path, entries: map[string]HostKeyEntry{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return k, nil
	}
	if err != nil {
		return nil, err
	}
	var f knownHostsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("known-hosts file %s: %w", path, err)
	}
	for host, e := range f.Hosts {
		k.entries[host] = e
	}
	return k, nil
}

// Path is the backing file.
func (k *KnownHosts) Path() string { return k.path }

// CanonicalHost normalises the address used as the store key.
func CanonicalHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.Count(addr, ":") > 1 && !strings.HasPrefix(addr, "[") {
			return "[" + addr + "]:22"
		}
		return addr + ":22"
	}
	return net.JoinHostPort(host, port)
}

// FingerprintSHA256 is the conventional SHA-256 host-key fingerprint.
func FingerprintSHA256(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Verify reports whether the presented key is known. A mismatch returns
// HostKeyMismatch and an error naming both fingerprints: the caller must
// refuse the connection, never overwrite the pin.
func (k *KnownHosts) Verify(addr string, key ssh.PublicKey) (HostKeyStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	host := CanonicalHost(addr)
	fp := FingerprintSHA256(key)
	e, ok := k.entries[host]
	if !ok {
		return HostKeyNew, nil
	}
	if e.Fingerprint != fp {
		return HostKeyMismatch, fmt.Errorf("%w for %s: pinned %s, presented %s (refusing; remove %s only if you reflashed the device)",
			ErrHostKey, host, e.Fingerprint, fp, k.path)
	}
	return HostKeyKnown, nil
}

// Accept persists a key after the user approves it.
func (k *KnownHosts) Accept(addr string, key ssh.PublicKey) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	host := CanonicalHost(addr)
	k.entries[host] = HostKeyEntry{
		Fingerprint: FingerprintSHA256(key),
		KeyType:     key.Type(),
		AddedUTC:    time.Now().UTC().Format(time.RFC3339),
	}
	return k.saveLocked()
}

// Entries lists the store's contents for display.
func (k *KnownHosts) Entries() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]string, 0, len(k.entries))
	for host, e := range k.entries {
		out = append(out, host+" "+e.Fingerprint)
	}
	sort.Strings(out)
	return out
}

func (k *KnownHosts) saveLocked() error {
	if k.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(k.path), 0o700); err != nil {
		return err
	}
	f := knownHostsFile{Version: 1, Hosts: k.entries}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(k.path), ".known-hosts-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(name)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return atomicfile.Replace(name, k.path)
}
