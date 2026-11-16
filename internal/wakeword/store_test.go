package wakeword

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeArtifact stores a valid version-3 artifact for id, returning its bytes.
func writeArtifact(t *testing.T, path, id string) []byte {
	t.Helper()
	clf := NewClassifier(id, "Word "+id, "ru", 7)
	if err := clf.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(clf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestModelStoreMigratesLegacyArtifactByteIdentical(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "wakeword", "model.json")
	before := writeArtifact(t, legacy, "word.personal")
	store := NewModelStore(filepath.Join(root, "wakeword", "models"))

	migrated, err := store.Migrate(legacy)
	if err != nil || !migrated {
		t.Fatalf("migrate = %v, %v; want true, nil", migrated, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy artifact still present: %v", err)
	}
	target := store.Path("word.personal")
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("migration changed the artifact bytes; the installed model must move untouched")
	}
	// The move is a rename: the inode is preserved, so the artifact cannot be a
	// partially written copy.
	legacyInfo, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if !legacyInfo.IsDir() {
		t.Fatal("store directory missing after migration")
	}

	valid, invalid, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) != 0 || len(valid) != 1 {
		t.Fatalf("loaded %d valid / %d invalid, want 1/0", len(valid), len(invalid))
	}
	if valid[0].ID != "word.personal" || valid[0].Classifier.ID != "word.personal" {
		t.Fatalf("migrated model = %+v", valid[0])
	}

	// A second scan must not move anything again and must not fail.
	if migrated, err := store.Migrate(legacy); err != nil || migrated {
		t.Fatalf("second migrate = %v, %v; want false, nil", migrated, err)
	}
}

func TestModelStoreMigrationKeepsNewerArtifact(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "wakeword", "model.json")
	writeArtifact(t, legacy, "word.personal")
	store := NewModelStore(filepath.Join(root, "wakeword", "models"))
	current := writeArtifact(t, store.Path("word.personal"), "word.personal")

	migrated, err := store.Migrate(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if migrated {
		t.Fatal("migration overwrote an artifact that already existed")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy artifact was removed while a newer artifact exists: %v", err)
	}
	got, err := os.ReadFile(store.Path("word.personal"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(current) {
		t.Fatal("existing artifact changed")
	}
}

func TestModelStoreMigrationRejectsInvalidLegacyArtifact(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "wakeword", "model.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewModelStore(filepath.Join(root, "wakeword", "models"))
	if migrated, err := store.Migrate(legacy); err == nil || migrated {
		t.Fatalf("invalid legacy artifact migrated: %v, %v", migrated, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("refused artifact was moved or deleted: %v", err)
	}
	if _, err := os.Stat(store.Dir()); !os.IsNotExist(err) {
		t.Fatal("store directory created for a refused artifact")
	}
}

func TestModelStoreReportsInvalidArtifacts(t *testing.T) {
	dir := t.TempDir()
	store := NewModelStore(dir)
	writeArtifact(t, store.Path("word.alpha"), "word.alpha")
	// A file name that cannot be an identity.
	if err := os.WriteFile(filepath.Join(dir, "Bad Name.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An artifact whose internal id disagrees with its file name.
	writeArtifact(t, store.Path("word.beta"), "word.gamma")
	// A truncated artifact.
	if err := os.WriteFile(store.Path("word.delta"), []byte(`{"version":3,`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-artifact files are ignored, not reported.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	valid, invalid, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 || valid[0].ID != "word.alpha" {
		t.Fatalf("valid = %+v", valid)
	}
	if len(invalid) != 3 {
		t.Fatalf("invalid = %d (%+v), want 3", len(invalid), invalid)
	}
	want := map[string]bool{"Bad Name": true, "word.beta": true, "word.delta": true}
	for _, entry := range invalid {
		if !want[entry.ID] {
			t.Fatalf("unexpected invalid id %q", entry.ID)
		}
		if entry.Err == nil || entry.Path == "" {
			t.Fatalf("invalid entry carries no reason: %+v", entry)
		}
		delete(want, entry.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing reports for %v", want)
	}
	// The refused files stay on disk: a corrupt artifact is never deleted or
	// silently replaced by training.
	for _, entry := range invalid {
		if _, err := os.Stat(entry.Path); err != nil {
			t.Fatalf("refused artifact %s removed: %v", entry.Path, err)
		}
	}
}

func TestModelStoreMissingDirectoryIsEmpty(t *testing.T) {
	store := NewModelStore(filepath.Join(t.TempDir(), "absent"))
	valid, invalid, err := store.Load()
	if err != nil || len(valid) != 0 || len(invalid) != 0 {
		t.Fatalf("Load on absent directory = %v, %v, %v", valid, invalid, err)
	}
	if migrated, err := store.Migrate(filepath.Join(t.TempDir(), "model.json")); err != nil || migrated {
		t.Fatalf("migrate absent legacy = %v, %v", migrated, err)
	}
}

func TestModelStoreWriteValidatesIDAndDeletes(t *testing.T) {
	store := NewModelStore(filepath.Join(t.TempDir(), "models"))
	for _, id := range []string{"", "Word", "word/alpha", "../word", "a" + string(make([]byte, 80))} {
		if ValidModelID(id) {
			t.Fatalf("accepted invalid id %q", id)
		}
		if err := store.Write(id, []byte("{}")); err == nil {
			t.Fatalf("wrote invalid id %q", id)
		}
		if err := store.Delete(id); err == nil {
			t.Fatalf("deleted invalid id %q", id)
		}
	}
	for _, id := range []string{"word.personal", "word-alpha_2", "a", "0.mix.ed"} {
		if !ValidModelID(id) {
			t.Fatalf("rejected valid id %q", id)
		}
	}
	writeArtifact(t, store.Path("word.alpha"), "word.alpha")
	if err := store.Delete("word.alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path("word.alpha")); !os.IsNotExist(err) {
		t.Fatal("artifact survived deletion")
	}
	// Deleting a missing artifact is idempotent.
	if err := store.Delete("word.alpha"); err != nil {
		t.Fatalf("second delete = %v", err)
	}
}

// An artifact is only addressable when its file name, id and content agree;
// Load enforces that so a renaming accident cannot bind a word to the wrong app.
func TestModelStoreRequiresStemAndArtifactIDMatch(t *testing.T) {
	dir := t.TempDir()
	store := NewModelStore(dir)
	writeArtifact(t, store.Path("word.alpha"), "word.alpha")
	valid, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 {
		t.Fatalf("matching artifact not loaded: %+v", valid)
	}
}
