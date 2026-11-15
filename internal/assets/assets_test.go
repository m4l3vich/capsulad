package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCatalog(t *testing.T) (*Catalog, string) {
	t.Helper()
	root := t.TempDir()
	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return c, root
}

func writeFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAssetListEnumeratesNestedFilesWithTypesHashesAndSizes(t *testing.T) {
	c, root := newCatalog(t)
	writeFile(t, root, "audio/replies/hello.wav", []byte("wave"))
	writeFile(t, root, "images/logo.PNG", []byte("png"))
	writeFile(t, root, "notes.txt", []byte("text"))
	writeFile(t, root, "unknown.xyz", []byte("data"))

	list := c.List("")
	if len(list) != 4 {
		t.Fatalf("list = %+v, want four files", list)
	}
	byID := map[string]Info{}
	for _, entry := range list {
		byID[entry.ID] = entry
	}
	wav, ok := byID["audio/replies/hello.wav"]
	if !ok {
		t.Fatalf("nested wav missing from %+v", list)
	}
	if wav.Path != "/assets/audio/replies/hello.wav" || wav.MediaType != "audio/wav" || wav.Size != 4 {
		t.Fatalf("wav entry = %+v", wav)
	}
	sum := sha256.Sum256([]byte("wave"))
	if wav.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("wav hash = %q", wav.SHA256)
	}
	if byID["images/logo.PNG"].MediaType != "image/png" {
		t.Fatalf("uppercase extension type = %q", byID["images/logo.PNG"].MediaType)
	}
	if byID["notes.txt"].MediaType != "text/plain" {
		t.Fatalf("txt type = %q", byID["notes.txt"].MediaType)
	}
	if byID["unknown.xyz"].MediaType != DefaultMediaType {
		t.Fatalf("unknown type = %q", byID["unknown.xyz"].MediaType)
	}
	if list[0].ID > list[len(list)-1].ID {
		t.Fatalf("list is not sorted by id: %+v", list)
	}
}

func TestAssetListPrefixFiltersIDsAndCanonicalPaths(t *testing.T) {
	c, root := newCatalog(t)
	writeFile(t, root, "audio/a.wav", []byte("a"))
	writeFile(t, root, "audio/deep/b.wav", []byte("b"))
	writeFile(t, root, "images/c.png", []byte("c"))

	if got := len(c.List("audio/")); got != 2 {
		t.Fatalf("id prefix listed %d entries, want 2", got)
	}
	if got := len(c.List("/assets/audio/")); got != 2 {
		t.Fatalf("canonical prefix listed %d entries, want 2", got)
	}
	if got := len(c.List("/assets")); got != 3 {
		t.Fatalf("root prefix listed %d entries, want 3", got)
	}
}

func TestAssetListRejectsSymlinksAndStagingFiles(t *testing.T) {
	c, root := newCatalog(t)
	writeFile(t, root, "safe.wav", []byte("safe"))
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.wav")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.wav")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, stagingPrefix+"123", []byte("partial"))

	list := c.List("")
	if len(list) != 1 || list[0].ID != "safe.wav" {
		t.Fatalf("list = %+v, want only the regular in-root file", list)
	}
}

func TestAssetLookupValidatesContainmentAndType(t *testing.T) {
	c, root := newCatalog(t)
	writeFile(t, root, "audio/reply.wav", []byte("wave"))
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.wav")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.wav")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "audio", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	info, err := c.Lookup("audio/reply.wav")
	if err != nil || info.ID != "audio/reply.wav" || info.MediaType != "audio/wav" || info.SHA256 != "" {
		t.Fatalf("Lookup(reply) = %+v, %v", info, err)
	}
	invalid := []string{
		"", " ", "/etc/passwd", "../secret.wav", "audio/../reply.wav",
		"audio//reply.wav", "./audio/reply.wav", `audio\reply.wav`,
		"link.wav", "audio/sub", stagingPrefix + "x", "audio/reply.wav\x00",
	}
	for _, rel := range invalid {
		if _, err := c.Lookup(rel); err == nil {
			t.Fatalf("Lookup(%q) was accepted", rel)
		}
	}
	if _, err := c.Lookup("missing.wav"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lookup(missing) error = %v, want fs.ErrNotExist", err)
	}
}

func TestAssetWriteCreatesSubdirectoriesAndAtomicallyOverwrites(t *testing.T) {
	c, root := newCatalog(t)
	entry, err := c.Write("audio/replies/hello.wav", strings.NewReader("first"))
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "audio/replies/hello.wav" || entry.Size != 5 || entry.MediaType != "audio/wav" {
		t.Fatalf("written entry = %+v", entry)
	}
	sum := sha256.Sum256([]byte("first"))
	if entry.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("written hash = %q", entry.SHA256)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "audio", "replies", "hello.wav"))
	if err != nil || string(onDisk) != "first" {
		t.Fatalf("on disk = %q, %v", onDisk, err)
	}

	replaced, err := c.Write("audio/replies/hello.wav", strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced.SHA256 == entry.SHA256 || replaced.Size != 6 {
		t.Fatalf("overwrite entry = %+v", replaced)
	}
	onDisk, err = os.ReadFile(filepath.Join(root, "audio", "replies", "hello.wav"))
	if err != nil || string(onDisk) != "second" {
		t.Fatalf("overwritten file = %q, %v", onDisk, err)
	}
	list := c.List("")
	if len(list) != 1 || list[0].SHA256 != replaced.SHA256 {
		t.Fatalf("catalog after overwrite = %+v", list)
	}
	staged, _ := filepath.Glob(filepath.Join(root, "audio", "replies", stagingPrefix+"*"))
	if len(staged) != 0 {
		t.Fatalf("staging files survived: %v", staged)
	}
}

func TestAssetWriteRejectsInvalidPathsAndSymlinkTargets(t *testing.T) {
	c, root := newCatalog(t)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.wav")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.wav")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir.wav"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		"", "/absolute.wav", "../escape.wav", "a/../../b.wav", `a\b.wav`,
		"link.wav", "linkdir/inner.wav", "dir.wav", stagingPrefix + "abc",
	} {
		if _, err := c.Write(rel, strings.NewReader("x")); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("Write(%q) error = %v, want ErrInvalidPath", rel, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "secret" {
		t.Fatalf("outside file was modified: %q, %v", data, err)
	}
}

func TestAssetWriteEnforcesSizeLimit(t *testing.T) {
	c, root := newCatalog(t)
	c.limit = 8
	if _, err := c.Write("small.bin", bytes.NewReader(make([]byte, 8))); err != nil {
		t.Fatalf("write at limit: %v", err)
	}
	if _, err := c.Write("big.bin", bytes.NewReader(make([]byte, 9))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized write error = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(filepath.Join(root, "big.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("oversized file was published: %v", err)
	}
}

func TestAssetRemoveDeletesRegularFilesOnly(t *testing.T) {
	c, root := newCatalog(t)
	writeFile(t, root, "audio/gone.wav", []byte("x"))
	if err := c.Remove("audio/gone.wav"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "audio", "gone.wav")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file survived remove: %v", err)
	}
	if err := c.Remove("audio/gone.wav"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("second remove error = %v, want fs.ErrNotExist", err)
	}

	writeFile(t, root, "audio/kept.wav", []byte("x"))
	if err := c.Remove("audio"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("directory remove error = %v, want ErrInvalidPath", err)
	}
	if _, err := os.Stat(filepath.Join(root, "audio", "kept.wav")); err != nil {
		t.Fatalf("directory contents were removed: %v", err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.wav")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.wav")); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("link.wav"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("symlink remove error = %v, want ErrInvalidPath", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("symlink target was removed: %v", err)
	}
	if err := c.Remove("../outside.wav"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("traversal remove error = %v, want ErrInvalidPath", err)
	}
}

func TestAssetMediaTypeTable(t *testing.T) {
	cases := map[string]string{
		"a.wav": "audio/wav", "a.mp3": "audio/mpeg", "a.ogg": "audio/ogg", "a.flac": "audio/flac",
		"a.png": "image/png", "a.jpg": "image/jpeg", "a.jpeg": "image/jpeg", "a.gif": "image/gif",
		"a.svg": "image/svg+xml", "a.webp": "image/webp", "a.json": "application/json",
		"a.txt": "text/plain", "a.html": "text/html", "a.css": "text/css", "a.js": "text/javascript",
		"a.bin": "application/octet-stream", "noext": DefaultMediaType, "a.WAV": "audio/wav",
	}
	for name, want := range cases {
		if got := MediaType(name); got != want {
			t.Fatalf("MediaType(%q) = %q, want %q", name, got, want)
		}
	}
}

// The store root is bind-mounted into sandboxes whose processes run as a
// different UID, so Open must relax a pre-created restrictive root.
func TestAssetOpenMakesStoreRootReadable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "assets")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("store root mode = %o, want 0755", perm)
	}
	if _, err := c.Write("audio/replies/hi.wav", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Join(root, "audio", "replies"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o755 {
		t.Fatalf("asset subdirectory mode = %o, want 0755", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(root, "audio", "replies", "hi.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o644 {
		t.Fatalf("asset file mode = %o, want 0644", perm)
	}
}

// Repeated List calls must not re-read unchanged files: the framework socket
// is reachable by every app, so each call would otherwise hash the whole store.
func TestAssetListCachesUnchangedHashes(t *testing.T) {
	c, root := newCatalog(t)
	writeFile(t, root, "audio/a.wav", []byte("first"))
	reads := 0
	c.hashFile = func(full string) (string, error) {
		reads++
		return hashFile(full)
	}

	first := c.List("")
	if reads != 1 {
		t.Fatalf("first List hashed %d files, want 1", reads)
	}
	second := c.List("")
	if reads != 1 {
		t.Fatalf("second List re-hashed unchanged files (%d reads)", reads)
	}
	if len(second) != 1 || second[0].SHA256 != first[0].SHA256 {
		t.Fatalf("cached entry = %+v, want %+v", second, first)
	}

	// A changed size invalidates the cached hash.
	writeFile(t, root, "audio/a.wav", []byte("second version"))
	c.List("")
	if reads != 2 {
		t.Fatalf("modified file hashed %d times total, want 2", reads)
	}

	// Remove invalidates the entry even if the same bytes and mtime reappear.
	info, err := os.Stat(filepath.Join(root, "audio", "a.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("audio/a.wav"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "audio/a.wav", []byte("second version"))
	if err := os.Chtimes(filepath.Join(root, "audio", "a.wav"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	before := reads
	c.List("")
	if reads != before+1 {
		t.Fatalf("removed-and-restored file was not re-hashed (%d -> %d reads)", before, reads)
	}

	// Entries for files that left the store are pruned.
	if err := c.Remove("audio/a.wav"); err != nil {
		t.Fatal(err)
	}
	c.List("")
	c.cacheMu.Lock()
	_, cached := c.hashes["audio/a.wav"]
	c.cacheMu.Unlock()
	if cached {
		t.Fatal("hash cache retained a removed asset")
	}
}

func TestAssetWriteEnforcesStoreBudget(t *testing.T) {
	c, root := newCatalog(t)
	c.limit = 16
	c.storeLimit = 10

	if _, err := c.Write("a.bin", bytes.NewReader(make([]byte, 6))); err != nil {
		t.Fatalf("write within budget: %v", err)
	}
	if _, err := c.Write("b.bin", bytes.NewReader(make([]byte, 5))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("over-budget write error = %v, want ErrStoreFull", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("over-budget file was published: %v", err)
	}
	staged, _ := filepath.Glob(filepath.Join(root, stagingPrefix+"*"))
	if len(staged) != 0 {
		t.Fatalf("staging files survived a rejected write: %v", staged)
	}
	// Replacing the target counts the store without that target's old size.
	if _, err := c.Write("a.bin", bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatalf("replace at the aggregate limit: %v", err)
	}
	if _, err := c.Write("c.bin", bytes.NewReader(make([]byte, 1))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("write into a full store = %v, want ErrStoreFull", err)
	}
	// An empty asset consumes no budget and is still accepted.
	if _, err := c.Write("empty.bin", strings.NewReader("")); err != nil {
		t.Fatalf("empty write into a full store: %v", err)
	}
}
