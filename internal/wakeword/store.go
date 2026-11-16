package wakeword

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Model artifacts live one per word under <data-dir>/wakeword/models/<id>.json.
// The file name stem is the artifact identity: it is the value the settings
// binding, the detection log and the Web UI all use, so a rename can never
// silently rebind a word to a different app.

// ModelIDMaxLen bounds a model id. The stem is a file name, so the pattern is
// deliberately narrow: one lowercase alphanumeric first character, then
// lowercase letters, digits, dots, underscores and dashes.
const ModelIDMaxLen = 64

var modelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidModelID reports whether id may name a stored artifact.
func ValidModelID(id string) bool { return modelIDPattern.MatchString(id) }

// ValidateModelID returns an error describing why id cannot name an artifact.
// It never normalizes the id: a caller that wants the pattern enforced has to
// submit a conforming value, so two ids can never collide after cleaning.
func ValidateModelID(id string) error {
	if id == "" {
		return ErrBadModel{"missing model id"}
	}
	if len(id) > ModelIDMaxLen {
		return ErrBadModel{fmt.Sprintf("model id %q is longer than %d characters", id, ModelIDMaxLen)}
	}
	if !ValidModelID(id) {
		return ErrBadModel{fmt.Sprintf("model id %q must match %s", id, modelIDPattern.String())}
	}
	return nil
}

// StoredModel is one valid artifact found in the store.
type StoredModel struct {
	ID         string
	Path       string
	Classifier *Classifier
}

// InvalidModel is one file the store refused to load. It is reported so the
// operator can see that an artifact exists but is unusable; it is never
// silently replaced, deleted or interpreted as a functioning model.
type InvalidModel struct {
	ID   string
	Path string
	Err  error
}

// ModelStore is the on-disk set of trained wake words.
type ModelStore struct {
	dir string
}

// NewModelStore returns the store rooted at dir, normally
// <data-dir>/wakeword/models.
func NewModelStore(dir string) *ModelStore { return &ModelStore{dir: dir} }

// Dir is the artifact directory.
func (s *ModelStore) Dir() string { return s.dir }

// Path is the artifact path for id. The caller validates id; Path only joins.
func (s *ModelStore) Path(id string) string { return filepath.Join(s.dir, id+".json") }

// Migrate moves a legacy single-artifact file into the store. The artifact's
// own id becomes the file name and its bytes are moved, not rewritten: no
// re-train, no settings write and no content change. A target that already
// exists wins; the legacy file is then left untouched rather than overwriting a
// newer artifact. A missing legacy file is not an error. An unreadable or
// invalid legacy artifact is reported so the caller can keep it visible as
// unusable instead of pretending the device has no model.
func (s *ModelStore) Migrate(legacyPath string) (bool, error) {
	if strings.TrimSpace(legacyPath) == "" {
		return false, nil
	}
	clf, err := LoadClassifier(legacyPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := ValidateModelID(clf.ID); err != nil {
		return false, fmt.Errorf("legacy artifact %s: %w", legacyPath, err)
	}
	target := s.Path(clf.ID)
	if _, err := os.Stat(target); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return false, err
	}
	renameErr := os.Rename(legacyPath, target)
	if renameErr == nil {
		return true, nil
	}
	// The legacy override may live on another filesystem (for example a tmpfs
	// corpus directory), where a rename cannot work. Copy the exact bytes into a
	// temporary file beside the target and rename that, so the store only ever
	// contains complete artifacts, then drop the legacy file.
	raw, err := os.ReadFile(legacyPath)
	if err != nil {
		return false, fmt.Errorf("move legacy artifact %s: %w (rename: %v)", legacyPath, err, renameErr)
	}
	if err := s.Write(clf.ID, raw); err != nil {
		return false, fmt.Errorf("move legacy artifact %s: %w (rename: %v)", legacyPath, err, renameErr)
	}
	if err := os.Remove(legacyPath); err != nil {
		return true, fmt.Errorf("artifact copied to %s but the legacy file remains: %w", target, err)
	}
	return true, nil
}

// Load returns every valid artifact sorted by id plus every refused file. A
// missing directory is an empty store, not an error.
func (s *ModelStore) Load() ([]StoredModel, []InvalidModel, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var valid []StoredModel
	var invalid []InvalidModel
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		id := strings.TrimSuffix(e.Name(), ".json")
		if err := ValidateModelID(id); err != nil {
			invalid = append(invalid, InvalidModel{ID: id, Path: path, Err: err})
			continue
		}
		clf, err := LoadClassifier(path)
		if err != nil {
			invalid = append(invalid, InvalidModel{ID: id, Path: path, Err: err})
			continue
		}
		// The file name is the identity the binding and the activation use. An
		// artifact whose internal id disagrees cannot be addressed reliably.
		if clf.ID != id {
			invalid = append(invalid, InvalidModel{ID: id, Path: path, Err: ErrBadModel{
				fmt.Sprintf("artifact id %q does not match the file name %q", clf.ID, id)}})
			continue
		}
		valid = append(valid, StoredModel{ID: id, Path: path, Classifier: clf})
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].ID < valid[j].ID })
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].ID < invalid[j].ID })
	return valid, invalid, nil
}

// Write atomically replaces one artifact. The temporary file is written in the
// target directory so the rename cannot cross filesystems, and a failure leaves
// the previous artifact readable.
func (s *ModelStore) Write(id string, raw []byte) error {
	if err := ValidateModelID(id); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	target := s.Path(id)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Delete removes one artifact. A missing file is not an error: the caller's
// intent (that id is not installed) already holds.
func (s *ModelStore) Delete(id string) error {
	if err := ValidateModelID(id); err != nil {
		return err
	}
	if err := os.Remove(s.Path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
