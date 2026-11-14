package assets

import (
	"os"
	"path/filepath"
	"testing"
)

// A malformed manifest must not publish host paths or traversal entries. Only
// regular files inside the asset root are listed.
func TestOpenRejectsTraversalAbsoluteAndSymlinkEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "safe.wav"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.wav")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.wav")); err != nil {
		t.Fatal(err)
	}
	manifest := `{"assets":[
		{"id":"safe","path":"safe.wav","media_type":"audio/wav"},
		{"id":"traversal","path":"/assets/../../secret.wav"},
		{"id":"absolute","path":"/etc/passwd"},
		{"id":"link","path":"link.wav"}
	]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	list := c.List("")
	if len(list) != 1 {
		t.Fatalf("catalog = %+v, want only the regular in-root asset", list)
	}
	if list[0].ID != "safe" || list[0].Path != "/assets/safe.wav" {
		t.Fatalf("catalog entry = %+v", list[0])
	}
	if list[0].SHA256 == "" {
		t.Fatal("safe asset was not hashed")
	}
}
