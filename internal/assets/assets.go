// Package assets serves the framework-provided, read-only shared asset catalog.
// Apps decode and play these bytes through the normal AudioService; there is no
// separate jingle resource service.
package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Info struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	MediaType   string `json:"media_type"`
	SHA256      string `json:"sha256"`
	License     string `json:"license"`
	Attribution string `json:"attribution"`
	Required    bool   `json:"required_base_asset"`
}

type catalogFile struct {
	Assets []Info `json:"assets"`
}

type Catalog struct {
	root   string
	mu     sync.RWMutex
	assets []Info
}

// Open loads <root>/manifest.json when present and hashes the referenced
// files. Missing optional assets are reported, not fatal; required assets that
// are missing make installation incomplete and are surfaced in diagnostics.
func Open(root string) (*Catalog, error) {
	c := &Catalog{root: root}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var doc catalogFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("asset manifest: %w", err)
	}
	// Only declared files that stay inside the asset root, are regular files,
	// and are not symlinks enter the catalog. A malformed manifest must not
	// turn a listing into host-path disclosure or a traversal read.
	clean := doc.Assets[:0]
	for _, a := range doc.Assets {
		if a.Path == "" {
			a.Path = a.ID
		}
		if !strings.HasPrefix(a.Path, "/assets/") {
			if strings.HasPrefix(a.Path, "/") || strings.ContainsAny(a.Path, `\`) {
				continue
			}
			a.Path = "/assets/" + strings.TrimPrefix(a.Path, "/")
		}
		rel := strings.TrimPrefix(a.Path, "/assets/")
		full, ok := containedAssetPath(root, rel)
		if !ok {
			continue
		}
		info, statErr := os.Lstat(full)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				// A declared but absent asset stays visible so required-asset
				// diagnostics can report it; its hash remains empty.
				clean = append(clean, a)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			// Symlinks and special files could alias host data; never publish.
			continue
		}
		if sum, err := hashFile(full); err == nil {
			a.SHA256 = sum
		}
		clean = append(clean, a)
	}
	doc.Assets = clean
	sort.Slice(doc.Assets, func(i, j int) bool { return doc.Assets[i].ID < doc.Assets[j].ID })
	c.assets = doc.Assets
	return c, nil
}

// containedAssetPath resolves rel under root and rejects absolute paths,
// traversal, and any escape after cleaning. Symlink policy is enforced by the
// caller with Lstat, so an entry cannot alias a host file outside the store.
func containedAssetPath(root, rel string) (string, bool) {
	rel = strings.TrimSpace(rel)
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, `\`) {
		return "", false
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	full := filepath.Join(root, filepath.FromSlash(cleaned))
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", false
	}
	within, err := filepath.Rel(rootAbs, fullAbs)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Catalog) List(prefix string) []Info {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Info, 0, len(c.assets))
	for _, a := range c.assets {
		if prefix == "" || strings.HasPrefix(a.ID, prefix) || strings.HasPrefix(a.Path, prefix) {
			out = append(out, a)
		}
	}
	return out
}

func (c *Catalog) Root() string { return c.root }
