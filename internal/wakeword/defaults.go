package wakeword

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Bundled word artifacts are compiled into the daemon, one version-3 JSON file
// per word under defaults/. The file name is the model id, exactly like an
// installed artifact: it is the identity the settings binding and the
// activation report.
//
//go:embed defaults/*.json
var bundledDefaults embed.FS

// BundledArtifact is one wake word shipped inside the daemon binary.
type BundledArtifact struct {
	ID  string
	Raw []byte
}

// BundledArtifacts returns the word artifacts compiled into this build, ordered
// by id. Every artifact is parsed and validated here, so a broken bundled file
// is a build/startup error instead of a silently skipped word.
func BundledArtifacts() ([]BundledArtifact, error) {
	entries, err := fs.ReadDir(bundledDefaults, "defaults")
	if err != nil {
		return nil, fmt.Errorf("read bundled wakewords: %w", err)
	}
	out := make([]BundledArtifact, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if err := ValidateModelID(id); err != nil {
			return nil, fmt.Errorf("bundled wakeword %s: %w", entry.Name(), err)
		}
		raw, err := bundledDefaults.ReadFile(path.Join("defaults", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read bundled wakeword %s: %w", entry.Name(), err)
		}
		clf, err := DecodeClassifier(raw)
		if err != nil {
			return nil, fmt.Errorf("bundled wakeword %s: %w", entry.Name(), err)
		}
		if clf.ID != id {
			return nil, fmt.Errorf("bundled wakeword %s: artifact id %q does not match the file name", entry.Name(), clf.ID)
		}
		out = append(out, BundledArtifact{ID: id, Raw: raw})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SeedDefaults installs the bundled artifacts into a store directory that does
// not exist yet. The directory is the marker of an initialized store: once it
// exists, its contents are authoritative, including an empty directory left
// behind after deleting the bundled word. A deletion therefore survives every
// restart and only removing the store directory (a deliberate reset) seeds the
// bundled words again.
//
// The artifacts are written to a temporary sibling directory and published with
// one rename, so a failure or crash cannot leave a half-seeded store that would
// look initialized. Bytes are copied verbatim; the calibrated weights and
// provenance are never rewritten.
func (s *ModelStore) SeedDefaults(artifacts []BundledArtifact) (bool, error) {
	if len(artifacts) == 0 {
		return false, nil
	}
	if _, err := os.Stat(s.dir); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	parent := filepath.Dir(s.dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(s.dir)+"-seed-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	for _, artifact := range artifacts {
		if err := ValidateModelID(artifact.ID); err != nil {
			return false, err
		}
		clf, err := DecodeClassifier(artifact.Raw)
		if err != nil {
			return false, fmt.Errorf("bundled wakeword %s: %w", artifact.ID, err)
		}
		if clf.ID != artifact.ID {
			return false, fmt.Errorf("bundled wakeword %s: artifact id %q does not match the file name", artifact.ID, clf.ID)
		}
		if err := os.WriteFile(filepath.Join(tmp, artifact.ID+".json"), artifact.Raw, 0o644); err != nil {
			return false, err
		}
	}
	// MkdirTemp creates 0700; the store directory is created 0755 by Write, so
	// keep one mode whether the store was seeded or written by a training run.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, s.dir); err != nil {
		// Another writer may have created the store while the defaults were
		// staged. An existing directory is authoritative; drop the staged copy.
		if _, statErr := os.Stat(s.dir); statErr == nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
