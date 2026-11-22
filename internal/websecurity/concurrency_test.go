package websecurity

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Temporary CLI keys are created and revoked once per invocation. Their
// tombstones must not consume the active-key budget, or a few dozen installer
// runs would lock the device out of API-key issuance.
func TestTemporaryKeyChurnDoesNotExhaustSlots(t *testing.T) {
	store := Open(t.TempDir(), DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	for i := range maxAPIKeys * 2 {
		name := fmt.Sprintf("cli-wifi-%03d", i)
		if _, _, err := store.CreateAPIKeyWithScopes(name, []string{ScopeOperator}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if err := store.RevokeAPIKey(name); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
	if active := store.ActiveAPIKeyCount(); active != 0 {
		t.Fatalf("active keys = %d, want 0", active)
	}
}

func TestCreatePrunesExpiredTombstones(t *testing.T) {
	dir := t.TempDir()
	store := Open(dir, DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateAPIKey("stale"); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIKey("stale"); err != nil {
		t.Fatal(err)
	}
	if err := store.updateCredentials(func(doc *persisted) (bool, error) {
		at := time.Now().UTC().Add(-8 * 24 * time.Hour)
		for i := range doc.APIKeys {
			if doc.APIKeys[i].Name == "stale" {
				doc.APIKeys[i].RevokedAt = &at
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateAPIKey("replacement"); err != nil {
		t.Fatalf("create after prune: %v", err)
	}
	doc, err := readCredentials(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range doc.APIKeys {
		if rec.Name == "stale" {
			t.Fatal("create path did not prune the expired tombstone")
		}
	}
	if len(doc.APIKeys) != 1 || doc.APIKeys[0].Name != "replacement" {
		t.Fatalf("persisted keys = %+v", doc.APIKeys)
	}
}

// The daemon and a CLI are separate processes, but they share the store. Two
// handles that loaded before either mutation must both land, not overwrite each
// other from their stale snapshots.
func TestConcurrentStoresPreserveIndependentMutations(t *testing.T) {
	dir := t.TempDir()
	first := Open(dir, DefaultOptions())
	if err := first.Load(); err != nil {
		t.Fatal(err)
	}
	second := Open(dir, DefaultOptions())
	if err := second.Load(); err != nil {
		t.Fatal(err)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		failure error
		token   string
	)
	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if failure == nil {
			failure = err
		}
		mu.Unlock()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		record(first.SetPassword("concurrent password"))
	}()
	go func() {
		defer wg.Done()
		var err error
		token, _, err = second.CreateAPIKeyWithScopes("agent", []string{ScopeOperator})
		record(err)
	}()
	wg.Wait()
	if failure != nil {
		t.Fatal(failure)
	}

	reloaded := Open(dir, DefaultOptions())
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if !reloaded.VerifyPassword("concurrent password") {
		t.Fatal("the password mutation was lost")
	}
	if _, _, ok := reloaded.APIKeyScopes(token); !ok {
		t.Fatal("the API-key mutation was lost")
	}
}

// A daemon handle loaded long before a CLI password change must not restore its
// old document when it next mints or revokes a key.
func TestStaleHandleCannotRevertPasswordChange(t *testing.T) {
	dir := t.TempDir()
	stale := Open(dir, DefaultOptions())
	if err := stale.Load(); err != nil {
		t.Fatal(err)
	}
	current := Open(dir, DefaultOptions())
	if err := current.Load(); err != nil {
		t.Fatal(err)
	}
	if err := current.SetPassword("new password from the CLI"); err != nil {
		t.Fatal(err)
	}
	staleToken, _, err := stale.CreateAPIKeyWithScopes("late-key", []string{ScopeStatus})
	if err != nil {
		t.Fatal(err)
	}

	reloaded := Open(dir, DefaultOptions())
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if !reloaded.VerifyPassword("new password from the CLI") {
		t.Fatal("the stale handle reverted the password")
	}
	if _, _, ok := reloaded.APIKeyScopes(staleToken); !ok {
		t.Fatal("the stale handle's key does not authenticate")
	}
	found := false
	for _, key := range reloaded.ListAPIKeys() {
		if key.Name == "late-key" {
			found = true
		}
	}
	if !found {
		t.Fatal("the stale handle's key mutation was lost")
	}
}
