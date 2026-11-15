package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/assets"
)

func TestParseAssistantDelay(t *testing.T) {
	valid := []struct {
		raw      string
		min, max int64
	}{
		{"", 0, 0},
		{"   ", 0, 0},
		{"100", 100, 100},
		{"0", 0, 0},
		{"60000", 60000, 60000},
		{"200-1500", 200, 1500},
		{"  200 - 1500  ", 200, 1500},
		{"1500-1500", 1500, 1500},
	}
	for _, tc := range valid {
		min, max, err := parseAssistantDelay(tc.raw)
		if err != nil || min != tc.min || max != tc.max {
			t.Fatalf("parseAssistantDelay(%q) = %d,%d,%v; want %d,%d", tc.raw, min, max, err, tc.min, tc.max)
		}
	}
	for _, raw := range []string{
		"-1", "+100", "1.5", "1e3", "0x10", "60 000", "100-", "-100-200",
		"200-100", "60001", "1-2-3", "10-60001", "abc",
	} {
		if _, _, err := parseAssistantDelay(raw); err == nil {
			t.Fatalf("parseAssistantDelay(%q) was accepted", raw)
		}
	}
}

// The shared asset store is a directory, not a manifest: a reply asset is
// accepted when it is a regular in-store WAV file and rejected for traversal,
// symlinks, directories, missing files and non-audio media types. Validation
// reads the directory fresh, so a file added after daemon start is accepted.
func TestValidateAssistantAssetAgainstStore(t *testing.T) {
	root := t.TempDir()
	catalog, err := assets.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{AssetsDir: root}, assets: catalog}
	write := func(rel string, data []byte) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("audio/replies/reply.wav", []byte("RIFF"))
	write("audio/replies/notes.txt", []byte("text"))
	if err := os.MkdirAll(filepath.Join(root, "audio", "folder.wav"), 0o755); err != nil {
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

	for _, value := range []string{"", "   ", "audio/replies/reply.wav"} {
		if err := d.validateAssistantAsset(value); err != nil {
			t.Fatalf("validateAssistantAsset(%q) = %v, want accepted", value, err)
		}
	}
	for _, value := range []string{
		"/etc/passwd", "../escape.wav", "audio/../../escape.wav", "audio//reply.wav",
		`audio\replies\reply.wav`, "audio/replies/reply.wav\x00",
		"audio/replies/missing.wav", "audio/replies/notes.txt", "audio/folder.wav",
		"link.wav", ".capsula-upload-123",
	} {
		if err := d.validateAssistantAsset(value); err == nil {
			t.Fatalf("validateAssistantAsset(%q) was accepted", value)
		}
	}

	// A file uploaded after the catalog was opened is immediately valid: there
	// is no cached manifest and no restart requirement.
	write("audio/replies/later.wav", []byte("RIFF"))
	if err := d.validateAssistantAsset("audio/replies/later.wav"); err != nil {
		t.Fatalf("later asset = %v, want accepted", err)
	}
}

func TestAssetServiceListsTheDirectoryStore(t *testing.T) {
	root := t.TempDir()
	catalog, err := assets.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "audio", "reply.wav"), []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{AssetsDir: root}, assets: catalog}

	resp, err := (&assetService{d: d}).ListAssets(context.Background(), &frameworkv1.ListAssetsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetAssets()) != 2 {
		t.Fatalf("ListAssets = %+v, want two entries", resp.GetAssets())
	}
	filtered, err := (&assetService{d: d}).ListAssets(context.Background(), &frameworkv1.ListAssetsRequest{Prefix: "audio/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.GetAssets()) != 1 {
		t.Fatalf("prefixed ListAssets = %+v", filtered.GetAssets())
	}
	wav := filtered.GetAssets()[0]
	sum := sha256.Sum256([]byte("RIFF"))
	if wav.GetAssetId() != "audio/reply.wav" || wav.GetPath() != "/assets/audio/reply.wav" ||
		wav.GetMediaType() != "audio/wav" || wav.GetSizeBytes() != 4 ||
		wav.GetSha256Hex() != hex.EncodeToString(sum[:]) {
		t.Fatalf("asset entry = %+v", wav)
	}
}
