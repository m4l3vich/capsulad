package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestAssetCreateDirAndOpenFile(t *testing.T) {
	c, root := newCatalog(t)
	if _, err := c.CreateDir("audio/voices"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "audio", "voices")); err != nil || !info.IsDir() {
		t.Fatalf("created directory = %v, %v", info, err)
	}
	if _, err := c.CreateDir("audio/voices"); err != nil {
		t.Fatalf("existing directory: %v", err)
	}

	writeFile(t, root, "audio/voices/hello.wav", []byte("hello"))
	file, info, err := c.Open("audio/voices/hello.wav")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if info.ID != "audio/voices/hello.wav" || info.MediaType != "audio/wav" || info.Size != 5 {
		t.Fatalf("open info = %+v", info)
	}
	body, err := io.ReadAll(file)
	if err != nil || string(body) != "hello" {
		t.Fatalf("read = %q, %v", body, err)
	}

	if _, _, err := c.Open("audio/voices"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("open directory error = %v, want ErrInvalidPath", err)
	}
	if _, err := c.CreateDir("../escape"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("traversal mkdir error = %v, want ErrInvalidPath", err)
	}
	if _, err := c.CreateDir("audio/voices/hello.wav/child"); err == nil {
		t.Fatal("mkdir below a regular file succeeded")
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
	// A replacement stages its new bytes while the old file is still published,
	// so it needs headroom on top of the tree. Replacing 6 bytes with 4 needs
	// exactly the remaining 4-byte budget: accepted.
	if _, err := c.Write("a.bin", bytes.NewReader(make([]byte, 4))); err != nil {
		t.Fatalf("replacement exactly within the budget: %v", err)
	}
	// A larger replacement has no staging headroom: refused, target untouched.
	if _, err := c.Write("a.bin", bytes.NewReader(make([]byte, 7))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("replacement without staging headroom = %v, want ErrStoreFull", err)
	}
	if entry, err := c.Lookup("a.bin"); err != nil || entry.Size != 4 {
		t.Fatalf("refused replacement disturbed the target: %+v %v", entry, err)
	}
	// Fill the store exactly; a max-size replacement at a full store is refused
	// rather than attempting to stage past the budget.
	if _, err := c.Write("c.bin", bytes.NewReader(make([]byte, 6))); err != nil {
		t.Fatalf("write to fill the store: %v", err)
	}
	if _, err := c.Write("a.bin", bytes.NewReader(make([]byte, 16))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("max-size replacement at a full store = %v, want ErrStoreFull", err)
	}
	if _, err := c.Write("d.bin", bytes.NewReader(make([]byte, 1))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("write into a full store = %v, want ErrStoreFull", err)
	}
	// An empty asset consumes no budget and is still accepted, and an empty
	// replacement frees its target without staging anything.
	if _, err := c.Write("empty.bin", strings.NewReader("")); err != nil {
		t.Fatalf("empty write into a full store: %v", err)
	}
	if _, err := c.Write("a.bin", strings.NewReader("")); err != nil {
		t.Fatalf("empty replacement at a full store: %v", err)
	}
	if entry, err := c.Lookup("a.bin"); err != nil || entry.Size != 0 {
		t.Fatalf("empty replacement did not publish: %+v %v", entry, err)
	}
}

// --- concurrent staging budget and delete/publish ordering -----------------

func waitForPending(t *testing.T, c *Catalog, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		got := c.pending
		c.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending reservation = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

type failingReader struct{ remaining int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("body read failed")
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

// TestAssetWriteReservesBudgetAcrossConcurrentBodies pins the invariant the
// client-paced copy needs: a body that is still streaming holds its worst-case
// budget, so a second concurrent upload cannot stage bytes the store cannot
// eventually hold.
func TestAssetWriteReservesBudgetAcrossConcurrentBodies(t *testing.T) {
	c, root := newCatalog(t)
	c.limit = 100
	c.storeLimit = 120

	firstReader, firstWriter := io.Pipe()
	firstDone := make(chan error, 1)
	go func() {
		_, err := c.Write("first.bin", firstReader)
		firstDone <- err
	}()
	waitForPending(t, c, 100)

	// The remaining budget is 20 bytes, so a second full-size body is refused
	// up front instead of staging 100 more bytes beside the first body.
	if _, err := c.Write("second.bin", bytes.NewReader(make([]byte, 101))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("concurrent oversized body error = %v, want ErrStoreFull", err)
	}
	// The bytes the budget can still admit are accepted and published.
	if _, err := c.Write("small.bin", bytes.NewReader(make([]byte, 20))); err != nil {
		t.Fatalf("write inside the remaining budget: %v", err)
	}
	if _, err := c.Write("third.bin", bytes.NewReader(make([]byte, 1))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("write beyond the reserved budget error = %v, want ErrStoreFull", err)
	}

	firstWriter.Write(make([]byte, 100))
	firstWriter.Close()
	if err := <-firstDone; err != nil {
		t.Fatalf("first body: %v", err)
	}
	if _, err := c.Write("fourth.bin", bytes.NewReader(make([]byte, 1))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("write into a full store error = %v, want ErrStoreFull", err)
	}
	staged, _ := filepath.Glob(filepath.Join(root, stagingPrefix+"*"))
	if len(staged) != 0 {
		t.Fatalf("staging files survived the budgeted writes: %v", staged)
	}
	var total int64
	for _, entry := range c.List("") {
		total += entry.Size
	}
	if total != 120 {
		t.Fatalf("published bytes = %d, want exactly the 120-byte budget", total)
	}
}

// TestAssetWriteReleasesReservationOnFailure: a body that fails mid-stream must
// give its whole reservation back so the space is usable again.
func TestAssetWriteReleasesReservationOnFailure(t *testing.T) {
	c, _ := newCatalog(t)
	c.limit = 100
	c.storeLimit = 100

	if _, err := c.Write("broken.bin", &failingReader{remaining: 50}); err == nil {
		t.Fatal("a failing body was published")
	}
	waitForPending(t, c, 0)
	if _, err := c.Write("full.bin", bytes.NewReader(make([]byte, 100))); err != nil {
		t.Fatalf("write after a released reservation: %v", err)
	}
}

// TestAssetWriteReplacementNeedsStagingHeadroom: the atomic rename frees the
// old file only at publication, so replacing a file needs budget for the new
// body on top of the still-published target.
func TestAssetWriteReplacementNeedsStagingHeadroom(t *testing.T) {
	c, root := newCatalog(t)
	c.limit = 100
	c.storeLimit = 100
	if _, err := c.Write("same.bin", bytes.NewReader(make([]byte, 60))); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Write("same.bin", bytes.NewReader(make([]byte, 41))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("replacement beyond the staging headroom = %v, want ErrStoreFull", err)
	}
	if entry, err := c.Lookup("same.bin"); err != nil || entry.Size != 60 {
		t.Fatalf("refused replacement disturbed the target: %+v %v", entry, err)
	}
	// Exactly the remaining 40 bytes fit and publish atomically.
	if _, err := c.Write("same.bin", bytes.NewReader(make([]byte, 40))); err != nil {
		t.Fatalf("replacement within the headroom: %v", err)
	}
	if entry, err := c.Lookup("same.bin"); err != nil || entry.Size != 40 {
		t.Fatalf("replacement result: %+v %v", entry, err)
	}
	if staged, _ := filepath.Glob(filepath.Join(root, stagingPrefix+"*")); len(staged) != 0 {
		t.Fatalf("staging files survived the replacement: %v", staged)
	}
}

// physicalBytes counts every regular file in the store, including the in-flight
// staging files that List deliberately hides.
func physicalBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// TestAssetWriteConcurrentReplacementStaysWithinPhysicalBudget is the physical
// form of the invariant: while a replacement streams, the published file plus
// its staging file never exceed the store budget, and a second concurrent
// replacement has no room to stage anything.
func TestAssetWriteConcurrentReplacementStaysWithinPhysicalBudget(t *testing.T) {
	c, root := newCatalog(t)
	c.limit = 100
	c.storeLimit = 100
	if _, err := c.Write("big.bin", bytes.NewReader(make([]byte, 60))); err != nil {
		t.Fatal(err)
	}

	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := c.Write("big.bin", reader)
		done <- err
	}()
	waitForPending(t, c, 40) // the 60 published bytes stay counted

	if _, err := writer.Write(make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	if got := physicalBytes(t, root); got > 100 {
		t.Fatalf("published+staged = %d bytes, want <= the 100-byte budget", got)
	}
	if _, err := c.Write("big.bin", bytes.NewReader(make([]byte, 1))); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("second concurrent replacement = %v, want ErrStoreFull", err)
	}
	if got := physicalBytes(t, root); got > 100 {
		t.Fatalf("refused replacement added bytes: %d", got)
	}

	if _, err := writer.Write(make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if err := <-done; err != nil {
		t.Fatalf("replacement: %v", err)
	}
	if got := physicalBytes(t, root); got != 40 {
		t.Fatalf("published bytes after replacement = %d, want 40", got)
	}
}

// TestAssetRemoveWinsOverInFlightReplacement: a delete that lands while the
// replacement body streams must not be undone by the publish.
func TestAssetRemoveWinsOverInFlightReplacement(t *testing.T) {
	c, root := newCatalog(t)
	c.limit = 64
	// Headroom for the replacement's staging file on top of the target.
	c.storeLimit = 120
	if _, err := c.Write("gone.bin", bytes.NewReader(make([]byte, 40))); err != nil {
		t.Fatal(err)
	}

	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := c.Write("gone.bin", reader)
		done <- err
	}()
	waitForPending(t, c, 64)

	if err := c.Remove("gone.bin"); err != nil {
		t.Fatalf("remove during a replacement: %v", err)
	}
	writer.Write(make([]byte, 64))
	writer.Close()
	if err := <-done; !errors.Is(err, ErrDeleted) {
		t.Fatalf("publish after the delete = %v, want ErrDeleted", err)
	}
	if _, err := os.Stat(filepath.Join(root, "gone.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a deleted asset was resurrected: %v", err)
	}
	for _, entry := range c.List("") {
		if entry.ID == "gone.bin" {
			t.Fatal("the deleted id is still listed")
		}
	}
	waitForPending(t, c, 0)
	// The deletion generation is scoped to the write that raced it: a fresh
	// upload of the same id is a deliberate recreation and publishes.
	if _, err := c.Write("gone.bin", bytes.NewReader(make([]byte, 40))); err != nil {
		t.Fatalf("recreate after the delete won: %v", err)
	}
}
