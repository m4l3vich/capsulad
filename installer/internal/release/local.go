package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// base is the GitHub Releases download root. Artifacts live on Releases, never
// on GitHub Packages: arbitrary files cannot be downloaded from Packages.
const base = "https://github.com/m4l3vich/capsulad/releases"

// DefaultBaseURL returns the download base for the latest release, or for a
// named tag when tag is non-empty.
func DefaultBaseURL(tag string) string {
	if tag == "" {
		return base + "/latest/download/"
	}
	return base + "/download/" + pathEscape(tag) + "/"
}

func pathEscape(s string) string {
	// Tags are ordinary names; escape only what would change the URL meaning.
	r := strings.NewReplacer("/", "%2F", " ", "%20", "#", "%23", "?", "%3F")
	return r.Replace(s)
}

// NormalizeBaseURL ensures a trailing slash so manifest-relative paths join
// predictably.
func NormalizeBaseURL(u string) string {
	if u == "" {
		return DefaultBaseURL("")
	}
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

// LoadDir returns the manifest for an offline release tree. A release.json at
// the root wins; when it is absent the tree is discovered and hashed in place.
func LoadDir(root string) (*Manifest, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	manifestPath := filepath.Join(root, ManifestFile)
	if _, err := os.Stat(manifestPath); err == nil {
		return LoadFile(manifestPath)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return DiscoverLocal(root)
}

// DiscoverLocal builds a manifest from the files present in root. It expects
// <platform>/deploy-bundle.tar.gz (or a single other tarball) and
// <platform>/apps/<id>-<version>.tar.gz. Every file is hashed, which is what
// makes an offline tree equivalent to a downloaded manifest.
func DiscoverLocal(root string) (*Manifest, error) {
	m := &Manifest{Schema: Schema, Version: "local", Firmwares: map[string]Platform{}}
	for _, platform := range Platforms {
		dir := filepath.Join(root, platform)
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", dir)
		}
		bundlePath, err := findBundle(dir, platform)
		if err != nil {
			return nil, err
		}
		bundle, err := hashFile(bundlePath, platform+"/"+filepath.Base(bundlePath))
		if err != nil {
			return nil, err
		}
		apps, err := discoverApps(dir, platform)
		if err != nil {
			return nil, err
		}
		m.Firmwares[platform] = Platform{
			Label:  Labels[platform],
			Bundle: bundle,
			Apps:   apps,
		}
	}
	if len(m.Firmwares) == 0 {
		return nil, fmt.Errorf("%s holds no %s release tree", root, strings.Join(Platforms, ", "))
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func findBundle(dir, platform string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isArchive(name) || strings.HasPrefix(name, "._") {
			continue
		}
		candidates = append(candidates, filepath.Join(dir, name))
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%s has no deployment bundle archive", dir)
	}
	for _, c := range candidates {
		if filepath.Base(c) == "deploy-bundle.tar.gz" {
			return c, nil
		}
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("%s has several possible bundles; name one deploy-bundle.tar.gz (%s)", dir, platform)
	}
	return candidates[0], nil
}

var archiveName = regexp.MustCompile(`^(.+)-([0-9][^/]*)\.(?:tar\.gz|tgz)$`)

func discoverApps(dir, platform string) ([]App, error) {
	appsDir := filepath.Join(dir, "apps")
	entries, err := os.ReadDir(appsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var apps []App
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "._") || !isArchive(name) {
			continue
		}
		match := archiveName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("%s: app archive %q is not <id>-<version>.tar.gz", appsDir, name)
		}
		rel := platform + "/apps/" + name
		f, err := hashFile(filepath.Join(appsDir, name), rel)
		if err != nil {
			return nil, err
		}
		apps = append(apps, App{ID: match[1], Version: match[2], Path: f.Path, Size: f.Size, SHA256: f.SHA256})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return apps, nil
}

func isArchive(name string) bool {
	return strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}

func hashFile(name, rel string) (File, error) {
	f, err := os.Open(name)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return File{}, err
	}
	return File{Path: rel, Size: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
