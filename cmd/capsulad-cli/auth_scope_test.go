package main

import (
	"path/filepath"
	"testing"

	"capsulad/internal/websecurity"
)

// TestAuthKeyCreateScope pins the provisioning contract the MCP frontend
// relies on: a key created without --scope stays read-only, and only an
// explicit request mints an operator key.
func TestAuthKeyCreateScope(t *testing.T) {
	dataDir := t.TempDir()
	if err := runAuth([]string{"init", "-hostname", "localhost", "-password", "correct horse battery", "-no-api-key"}, dataDir, ""); err != nil {
		t.Fatalf("auth init: %v", err)
	}
	if err := runAuth([]string{"key", "create", "agent", "--scope", "operator"}, dataDir, ""); err != nil {
		t.Fatalf("key create --scope operator: %v", err)
	}
	if err := runAuth([]string{"key", "create", "probe"}, dataDir, ""); err != nil {
		t.Fatalf("key create: %v", err)
	}
	if err := runAuth([]string{"key", "create", "rooted", "--scope", "root"}, dataDir, ""); err == nil {
		t.Fatal("unknown scope was accepted")
	}

	store := websecurity.Open(filepath.Join(dataDir, "security"), websecurity.DefaultOptions())
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	scopes := map[string][]string{}
	for _, key := range store.ListAPIKeys() {
		scopes[key.Name] = key.Scopes
	}
	if got := scopes["agent"]; len(got) != 1 || got[0] != websecurity.ScopeOperator {
		t.Fatalf("agent scopes = %v", got)
	}
	if got := scopes["probe"]; len(got) != 1 || got[0] != websecurity.ScopeStatus {
		t.Fatalf("probe scopes = %v", got)
	}
	if _, exists := scopes["rooted"]; exists {
		t.Fatal("a rejected scope minted a key")
	}
}
