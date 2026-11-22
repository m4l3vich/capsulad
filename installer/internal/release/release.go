// Package release describes, discovers, downloads and stages Capsula Mini
// release artifacts: one deployment bundle and a set of app archives per
// firmware platform.
//
// A release is a JSON manifest plus files addressed relative to it. The same
// manifest shape is produced by cmd/stage-release, served by GitHub Releases
// and read from an offline directory, so there is exactly one artifact
// vocabulary for the installer.
package release

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Schema is the only manifest schema this build understands. It exists so a
// future incompatible layout can be refused rather than misread.
const Schema = 1

// ManifestFile is the manifest's fixed name at the root of a release tree and
// at the release download base URL.
const ManifestFile = "release.json"

// Platforms lists every firmware platform a release may carry, in the order
// the installer presents them.
var Platforms = []string{"rockchip-stock", "amlogic-stock", "amlogic-alpine"}

// Labels are the human names used when a manifest does not carry one.
var Labels = map[string]string{
	"rockchip-stock": "Jailbroken stock (Rockchip RK3308)",
	"amlogic-stock":  "Jailbroken stock (Amlogic A113X)",
	"amlogic-alpine": "Alpine Linux (Amlogic A113X)",
}

// File is a regular artifact addressed relative to the release root.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// App is one packaged first-party application for one platform.
type App struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// Platform is everything a release carries for one firmware target.
type Platform struct {
	Label  string `json:"label"`
	Bundle File   `json:"bundle"`
	Apps   []App  `json:"apps"`
}

// Manifest is the release document. Firmwares is keyed by platform name.
type Manifest struct {
	Schema    int                 `json:"schema"`
	Version   string              `json:"version"`
	Firmwares map[string]Platform `json:"firmwares"`
}

// Load parses and validates a manifest. It rejects unknown schemas, unsafe
// relative paths and malformed hashes before any file is fetched.
func Load(data []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("release manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("release manifest: trailing JSON after the document")
		}
		return nil, fmt.Errorf("release manifest: trailing data: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadFile reads a manifest from disk.
func LoadFile(name string) (*Manifest, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	m, err := Load(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return m, nil
}

// Validate checks the whole document. Every field the installer relies on is
// checked here, so later code can assume a well-formed manifest.
func (m *Manifest) Validate() error {
	if m.Schema != Schema {
		return fmt.Errorf("release manifest: unsupported schema %d (want %d)", m.Schema, Schema)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("release manifest: missing version")
	}
	if len(m.Firmwares) == 0 {
		return fmt.Errorf("release manifest: no firmwares")
	}
	for name, p := range m.Firmwares {
		if !KnownPlatform(name) {
			return fmt.Errorf("release manifest: unknown firmware %q", name)
		}
		if err := checkFile(p.Bundle); err != nil {
			return fmt.Errorf("release manifest: %s bundle: %w", name, err)
		}
		if p.Bundle.Size > BundleMaxBytes {
			return fmt.Errorf("release manifest: %s bundle is %d bytes, over the %d byte cap", name, p.Bundle.Size, BundleMaxBytes)
		}
		seen := make(map[string]bool, len(p.Apps))
		for _, a := range p.Apps {
			if err := checkApp(a); err != nil {
				return fmt.Errorf("release manifest: %s app %q: %w", name, a.ID, err)
			}
			if a.Size > AppMaxBytes {
				return fmt.Errorf("release manifest: %s app %q is %d bytes, over the %d byte cap", name, a.ID, a.Size, AppMaxBytes)
			}
			if seen[a.ID] {
				return fmt.Errorf("release manifest: %s app %q appears twice", name, a.ID)
			}
			seen[a.ID] = true
		}
	}
	return nil
}

// For returns the artifacts for one platform.
func (m *Manifest) For(platform string) (Platform, error) {
	p, ok := m.Firmwares[platform]
	if !ok {
		return Platform{}, fmt.Errorf("release %s has no %s firmware", m.Version, platform)
	}
	return p, nil
}

// App returns the artifact for one app ID on one platform.
func (m *Manifest) App(platform, id string) (App, error) {
	p, err := m.For(platform)
	if err != nil {
		return App{}, err
	}
	for _, a := range p.Apps {
		if a.ID == id {
			return a, nil
		}
	}
	return App{}, fmt.Errorf("release %s has no %s app %s", m.Version, platform, id)
}

// KnownPlatform reports whether name is one of the supported targets.
func KnownPlatform(name string) bool {
	for _, p := range Platforms {
		if p == name {
			return true
		}
	}
	return false
}

func checkApp(a App) error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("missing id")
	}
	if strings.TrimSpace(a.Version) == "" {
		return fmt.Errorf("missing version")
	}
	return checkFile(File{Path: a.Path, Size: a.Size, SHA256: a.SHA256})
}

func checkFile(f File) error {
	if err := CheckRelativePath(f.Path); err != nil {
		return err
	}
	if f.Size <= 0 {
		return fmt.Errorf("invalid size %d", f.Size)
	}
	if len(f.SHA256) != 64 {
		return fmt.Errorf("sha256 must be 64 hex characters")
	}
	for _, c := range f.SHA256 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return fmt.Errorf("sha256 must be lowercase hex")
		}
	}
	return nil
}

// CheckRelativePath rejects absolute paths, parent escapes and empty paths.
// A manifest comes from a downloaded file, so it is treated as untrusted.
func CheckRelativePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("path %q must be relative with forward slashes", p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q escapes the release root", p)
	}
	if clean != p {
		return fmt.Errorf("path %q is not clean", p)
	}
	return nil
}

// SafeJoin joins a validated relative path to a root directory.
func SafeJoin(root, rel string) (string, error) {
	if err := CheckRelativePath(rel); err != nil {
		return "", err
	}
	return path.Join(root, rel), nil
}
