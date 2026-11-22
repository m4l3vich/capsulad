package release

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"capsulad/installer/internal/atomicfile"
)

// StageOptions describes one release tree to build or extend.
type StageOptions struct {
	Out      string
	Version  string
	Platform string
	Bundle   string
	Apps     []string
	// Flat writes release assets directly under Out with the platform as a
	// filename prefix. GitHub Releases cannot serve directory components:
	// /releases/download/<tag>/<platform>/<file> is a 404, while
	// /releases/download/<tag>/<platform>__<file> is an asset.
	Flat bool
}

// Stage copies a bundle and app archives into OUT, hashes them and merges the
// platform entry into OUT/release.json without dropping other platforms.
func Stage(opts StageOptions) (*Manifest, error) {
	if opts.Out == "" {
		return nil, fmt.Errorf("stage-release: -out is required")
	}
	if opts.Platform == "" || !KnownPlatform(opts.Platform) {
		return nil, fmt.Errorf("stage-release: -platform must be one of %s", strings.Join(Platforms, ", "))
	}
	if strings.TrimSpace(opts.Version) == "" {
		return nil, fmt.Errorf("stage-release: -version is required")
	}
	if opts.Bundle == "" {
		return nil, fmt.Errorf("stage-release: -bundle is required")
	}

	root := opts.Out
	platformDir := filepath.Join(root, opts.Platform)
	appsDir := filepath.Join(platformDir, "apps")
	bundleName := "deploy-bundle.tar.gz"
	bundlePath := opts.Platform + "/" + bundleName
	if opts.Flat {
		platformDir = root
		appsDir = root
		bundleName = opts.Platform + "__" + bundleName
		bundlePath = bundleName
	}
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return nil, err
	}

	bundleDest := filepath.Join(platformDir, bundleName)
	if err := copyInto(opts.Bundle, bundleDest); err != nil {
		return nil, fmt.Errorf("stage-release: bundle: %w", err)
	}
	bundle, err := hashFile(bundleDest, bundlePath)
	if err != nil {
		return nil, err
	}

	entry := Platform{Label: Labels[opts.Platform], Bundle: bundle, Apps: []App{}}
	seen := map[string]bool{}
	for _, src := range opts.Apps {
		name := filepath.Base(src)
		match := archiveName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("stage-release: app archive %q is not <id>-<version>.tar.gz", src)
		}
		id, version := match[1], match[2]
		if seen[id] {
			return nil, fmt.Errorf("stage-release: app %s supplied twice", id)
		}
		seen[id] = true
		destName := name
		destPath := opts.Platform + "/apps/" + name
		if opts.Flat {
			destName = opts.Platform + "__" + name
			destPath = destName
		}
		dest := filepath.Join(appsDir, destName)
		if err := copyInto(src, dest); err != nil {
			return nil, fmt.Errorf("stage-release: app %s: %w", id, err)
		}
		f, err := hashFile(dest, destPath)
		if err != nil {
			return nil, err
		}
		entry.Apps = append(entry.Apps, App{ID: id, Version: version, Path: f.Path, Size: f.Size, SHA256: f.SHA256})
	}
	sort.Slice(entry.Apps, func(i, j int) bool { return entry.Apps[i].ID < entry.Apps[j].ID })

	manifestPath := filepath.Join(root, ManifestFile)
	var m *Manifest
	if _, err := os.Stat(manifestPath); err == nil {
		m, err = LoadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("stage-release: existing manifest is invalid: %w", err)
		}
	} else if os.IsNotExist(err) {
		m = &Manifest{Schema: Schema, Firmwares: map[string]Platform{}}
	} else {
		return nil, err
	}
	MergePlatform(m, opts.Version, opts.Platform, entry)
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := writeManifest(manifestPath, m); err != nil {
		return nil, err
	}
	return m, nil
}

// MergePlatform replaces one platform entry and stamps the release version.
// Other platforms are preserved; that is what lets a per-platform CI job stage
// into the same tree without clobbering its siblings.
func MergePlatform(m *Manifest, version, platform string, p Platform) {
	if m.Schema == 0 {
		m.Schema = Schema
	}
	m.Version = version
	if m.Firmwares == nil {
		m.Firmwares = map[string]Platform{}
	}
	if p.Label == "" {
		p.Label = Labels[platform]
	}
	m.Firmwares[platform] = p
}

func writeManifest(name string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(name, data, 0o644)
}

func copyInto(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".stage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return atomicfile.Replace(tmpName, dest)
}

func writeAtomic(name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
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
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return atomicfile.Replace(tmpName, name)
}
