package wakeword

import (
	"os"
	"path/filepath"
	"testing"
)

// The word compiled into the daemon is a real version-3 artifact whose file
// name is its identity; it must load exactly like an installed file.
func TestBundledArtifactsAreValid(t *testing.T) {
	artifacts, err := BundledArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) == 0 {
		t.Fatal("no wakeword is bundled into the daemon")
	}
	personal := false
	for _, artifact := range artifacts {
		clf, err := DecodeClassifier(artifact.Raw)
		if err != nil {
			t.Fatalf("bundled %s: %v", artifact.ID, err)
		}
		if clf.Version != ClassifierVersion {
			t.Fatalf("bundled %s has version %d, want %d", artifact.ID, clf.Version, ClassifierVersion)
		}
		if clf.ID != artifact.ID {
			t.Fatalf("bundled %s carries id %q", artifact.ID, clf.ID)
		}
		if clf.Display == "" {
			t.Fatalf("bundled %s has no display name", artifact.ID)
		}
		if artifact.ID == "word.personal" {
			personal = true
		}
	}
	if !personal {
		t.Fatal("the personal word is not bundled")
	}
}

// Seeding publishes a complete store once. Deleting an artifact leaves the
// directory behind, and that directory is authoritative: a restart must not
// resurrect the word the operator removed.
func TestSeedDefaultsPublishesTheStoreOnce(t *testing.T) {
	root := t.TempDir()
	store := NewModelStore(filepath.Join(root, "wakeword", "models"))
	artifacts, err := BundledArtifacts()
	if err != nil {
		t.Fatal(err)
	}

	seeded, err := store.SeedDefaults(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if !seeded {
		t.Fatal("a missing store was not seeded")
	}
	for _, artifact := range artifacts {
		got, err := os.ReadFile(store.Path(artifact.ID))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(artifact.Raw) {
			t.Fatalf("seeded %s was rewritten", artifact.ID)
		}
	}
	valid, invalid, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != len(artifacts) || len(invalid) != 0 {
		t.Fatalf("seeded store loaded %d valid, %d invalid; want %d, 0", len(valid), len(invalid), len(artifacts))
	}

	// A deletion keeps the directory. Seeding an initialized store is a no-op.
	if err := store.Delete(artifacts[0].ID); err != nil {
		t.Fatal(err)
	}
	seeded, err = store.SeedDefaults(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if seeded {
		t.Fatal("an initialized store was seeded again")
	}
	if _, err := os.Stat(store.Path(artifacts[0].ID)); !os.IsNotExist(err) {
		t.Fatalf("a deleted bundled word came back: %v", err)
	}
}

// An existing store is never touched, even when it is empty: the directory is
// the marker of an explicit decision about what the device runs.
func TestSeedDefaultsLeavesExistingStoreAlone(t *testing.T) {
	root := t.TempDir()
	store := NewModelStore(filepath.Join(root, "wakeword", "models"))
	artifacts, err := BundledArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	seeded, err := store.SeedDefaults(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if seeded {
		t.Fatal("an existing empty store was seeded")
	}
	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("seeding wrote into an existing store: %v", entries)
	}
}
