package release

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AppsFile is the app-channel manifest name. It is separate from release.json
// so app packages can be published without rebuilding or republishing the
// daemon, CLI, runtime or interpreter hosts.
const AppsFile = "apps.json"

// AppPlatform is one firmware's app list. It deliberately has no bundle.
type AppPlatform struct {
	Label string `json:"label,omitempty"`
	Apps  []App  `json:"apps"`
}

// AppsManifest is the app-channel document.
type AppsManifest struct {
	Schema    int                    `json:"schema"`
	Version   string                 `json:"version"`
	Core      string                 `json:"core,omitempty"`
	Firmwares map[string]AppPlatform `json:"firmwares"`
}

// LoadApps parses and validates an app manifest.
func LoadApps(data []byte) (*AppsManifest, error) {
	var m AppsManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("app manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("app manifest: trailing JSON after the document")
		}
		return nil, fmt.Errorf("app manifest: trailing data: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadAppsFile reads an app manifest from disk.
func LoadAppsFile(name string) (*AppsManifest, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	m, err := LoadApps(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return m, nil
}

// Validate checks the document before any artifact is fetched.
func (m *AppsManifest) Validate() error {
	if m.Schema != Schema {
		return fmt.Errorf("app manifest: unsupported schema %d (want %d)", m.Schema, Schema)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("app manifest: missing version")
	}
	if len(m.Firmwares) == 0 {
		return fmt.Errorf("app manifest: no firmwares")
	}
	for name, p := range m.Firmwares {
		if !KnownPlatform(name) {
			return fmt.Errorf("app manifest: unknown firmware %q", name)
		}
		seen := make(map[string]bool, len(p.Apps))
		for _, a := range p.Apps {
			if err := checkApp(a); err != nil {
				return fmt.Errorf("app manifest: %s app %q: %w", name, a.ID, err)
			}
			if a.Size > AppMaxBytes {
				return fmt.Errorf("app manifest: %s app %q is %d bytes, over the %d byte cap", name, a.ID, a.Size, AppMaxBytes)
			}
			if seen[a.ID] {
				return fmt.Errorf("app manifest: %s app %q appears twice", name, a.ID)
			}
			seen[a.ID] = true
		}
	}
	return nil
}

// ForApps returns one platform's app entry.
func (m *AppsManifest) ForApps(platform string) (AppPlatform, error) {
	p, ok := m.Firmwares[platform]
	if !ok {
		return AppPlatform{}, fmt.Errorf("app channel %s has no %s firmware", m.Version, platform)
	}
	return p, nil
}

// App returns one app entry for a platform.
func (m *AppsManifest) App(platform, id string) (App, error) {
	p, err := m.ForApps(platform)
	if err != nil {
		return App{}, err
	}
	for _, a := range p.Apps {
		if a.ID == id {
			return a, nil
		}
	}
	return App{}, fmt.Errorf("app channel %s has no %s app %s", m.Version, platform, id)
}

// StageAppsOptions describes one platform's app-channel entry.
type StageAppsOptions struct {
	Out      string
	Version  string
	Core     string
	Platform string
	Libc     string
	Apps     []string
	// Flat writes assets directly under Out with the libc as a filename
	// prefix, so the two gnu stock platforms can share the same archives on a
	// GitHub release. The nested layout is kept for offline trees.
	Flat bool
}

// StageApps copies app archives into OUT, hashes them and merges the platform
// into OUT/apps.json without dropping other platforms.
func StageApps(opts StageAppsOptions) (*AppsManifest, error) {
	if opts.Out == "" {
		return nil, fmt.Errorf("stage-apps: -out is required")
	}
	if opts.Platform == "" || !KnownPlatform(opts.Platform) {
		return nil, fmt.Errorf("stage-apps: -platform must be one of %s", strings.Join(Platforms, ", "))
	}
	if opts.Libc != "gnu" && opts.Libc != "musl" {
		return nil, fmt.Errorf("stage-apps: -libc must be gnu or musl")
	}
	if strings.TrimSpace(opts.Version) == "" {
		return nil, fmt.Errorf("stage-apps: -version is required")
	}

	root := opts.Out
	appsDir := filepath.Join(root, opts.Platform, "apps")
	assetPrefix := opts.Platform + "/apps/"
	if opts.Flat {
		appsDir = root
		assetPrefix = opts.Libc + "__"
	}
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return nil, err
	}

	entry := AppPlatform{Label: Labels[opts.Platform], Apps: []App{}}
	seen := map[string]bool{}
	for _, src := range opts.Apps {
		name := filepath.Base(src)
		match := archiveName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("stage-apps: app archive %q is not <id>-<version>.tar.gz", src)
		}
		id, version := match[1], match[2]
		if seen[id] {
			return nil, fmt.Errorf("stage-apps: app %s supplied twice", id)
		}
		seen[id] = true
		destName := name
		destPath := assetPrefix + name
		if opts.Flat {
			destName = opts.Libc + "__" + name
		}
		dest := filepath.Join(appsDir, destName)
		if err := copyInto(src, dest); err != nil {
			return nil, fmt.Errorf("stage-apps: app %s: %w", id, err)
		}
		f, err := hashFile(dest, destPath)
		if err != nil {
			return nil, err
		}
		entry.Apps = append(entry.Apps, App{ID: id, Version: version, Path: f.Path, Size: f.Size, SHA256: f.SHA256})
	}
	sort.Slice(entry.Apps, func(i, j int) bool { return entry.Apps[i].ID < entry.Apps[j].ID })

	manifestPath := filepath.Join(root, AppsFile)
	var m *AppsManifest
	if _, err := os.Stat(manifestPath); err == nil {
		m, err = LoadAppsFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("stage-apps: existing manifest is invalid: %w", err)
		}
	} else if os.IsNotExist(err) {
		m = &AppsManifest{Schema: Schema, Firmwares: map[string]AppPlatform{}}
	} else {
		return nil, err
	}
	if m.Firmwares == nil {
		m.Firmwares = map[string]AppPlatform{}
	}
	m.Schema = Schema
	m.Version = opts.Version
	if strings.TrimSpace(opts.Core) != "" {
		m.Core = opts.Core
	}
	m.Firmwares[opts.Platform] = entry
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := writeAppsManifest(manifestPath, m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeAppsManifest(name string, m *AppsManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(name, data, 0o644)
}
