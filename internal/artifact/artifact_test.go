package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testManifest = `
id = "org.example.test"
name = "Test package"
version = "0.1.0"
[entrypoint]
runtime = "native"
path = "bin/app"
architecture = "aarch64"
abi = "linux-gnu"
`

func packageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "manifest.toml", testManifest, 0o644)
	writeFile(t, dir, "bin/app", "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, dir, "assets/note.txt", "hello", 0o644)
	return dir
}

func writeFile(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

type tarEntry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	linkname string
}

func archive(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: flag, Linkname: e.linkname}
		if flag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDirectoryPreservesExecutableBit(t *testing.T) {
	src := packageDir(t)
	pkg, err := Open(src)
	if err != nil {
		t.Fatalf("open directory: %v", err)
	}
	defer pkg.Close()
	if pkg.Manifest.ID != "org.example.test" {
		t.Fatalf("manifest id = %q", pkg.Manifest.ID)
	}
	for rel, want := range map[string]os.FileMode{
		"bin/app":         0o755,
		"assets/note.txt": 0o444,
	} {
		info, err := os.Stat(filepath.Join(pkg.Dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", rel, got, want)
		}
	}
	dir := pkg.Dir
	if err := pkg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("staging tree survived Close: %v", err)
	}
}

func TestOpenArchiveRoundTrip(t *testing.T) {
	src := packageDir(t)
	out := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := Write(src, out); err != nil {
		t.Fatalf("write: %v", err)
	}
	pkg, err := Open(out)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer pkg.Close()
	body, err := os.ReadFile(filepath.Join(pkg.Dir, "assets", "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("asset body = %q", body)
	}
	info, err := os.Stat(filepath.Join(pkg.Dir, "bin", "app"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("archive executable mode = %04o", info.Mode().Perm())
	}
}

func TestRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../escape", "bin/../../escape", "./../escape"} {
		path := filepath.Join(t.TempDir(), "bad.tar.gz")
		archive(t, path, []tarEntry{
			{name: "manifest.toml", body: testManifest, mode: 0o644},
			{name: name, body: "x", mode: 0o644},
		})
		_, err := Open(path)
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Errorf("entry %q: error = %v, want an escape error", name, err)
		}
	}
}

func TestRejectsAbsolutePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	archive(t, path, []tarEntry{
		{name: "manifest.toml", body: testManifest, mode: 0o644},
		{name: "/etc/evil", body: "x", mode: 0o644},
	})
	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error = %v, want an absolute-path error", err)
	}
}

func TestRejectsSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	archive(t, path, []tarEntry{
		{name: "manifest.toml", body: testManifest, mode: 0o644},
		{name: "bin/app", typeflag: tar.TypeSymlink, linkname: "/bin/sh"},
	})
	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "links are not allowed") {
		t.Fatalf("error = %v, want a link rejection", err)
	}

	dir := packageDir(t)
	if err := os.Symlink("/bin/sh", filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "symlinks are not allowed") {
		t.Fatalf("directory symlink error = %v", err)
	}
}

func TestRejectsDuplicatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	archive(t, path, []tarEntry{
		{name: "manifest.toml", body: testManifest, mode: 0o644},
		{name: "assets/a", body: "one", mode: 0o644},
		{name: "./assets/a", body: "two", mode: 0o644},
	})
	_, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want a duplicate-path error", err)
	}
}

func TestRejectsOversize(t *testing.T) {
	dir := packageDir(t)
	writeFile(t, dir, "assets/big.bin", strings.Repeat("x", 64), 0o644)
	if _, err := OpenLimit(dir, Limits{MaxFileBytes: 16, MaxTotalBytes: 1024, MaxEntries: 32}); err == nil ||
		!strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("per-file error = %v", err)
	}
	if _, err := OpenLimit(dir, Limits{MaxFileBytes: 1024, MaxTotalBytes: 32, MaxEntries: 32}); err == nil ||
		!strings.Contains(err.Error(), "unpacked limit") {
		t.Fatalf("total error = %v", err)
	}
	out := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := Write(dir, out); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLimit(out, Limits{MaxFileBytes: 16, MaxTotalBytes: 1024, MaxEntries: 32}); err == nil ||
		!strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("archive per-file error = %v", err)
	}
}

func TestRejectsMissingRootManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.tar.gz")
	archive(t, path, []tarEntry{
		{name: "wrapped/manifest.toml", body: testManifest, mode: 0o644},
		{name: "wrapped/bin/app", body: "#!/bin/sh\n", mode: 0o755},
	})
	if _, err := Open(path); err == nil {
		t.Fatal("archive without a root manifest was accepted")
	}

	dir := t.TempDir()
	writeFile(t, dir, "bin/app", "#!/bin/sh\n", 0o755)
	if _, err := Open(dir); err == nil {
		t.Fatal("directory without a root manifest was accepted")
	}
}

func TestWriteDeterministic(t *testing.T) {
	src := packageDir(t)
	// Different wall-clock mtimes must not leak into the archive.
	later := t.TempDir()
	for rel, mode := range map[string]os.FileMode{
		"manifest.toml":   0o644,
		"bin/app":         0o755,
		"assets/note.txt": 0o644,
	} {
		body, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, later, rel, string(body), mode)
	}
	stamp := time.Now().Add(-48 * time.Hour)
	if err := filepath.WalkDir(later, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, stamp, stamp)
	}); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "one.tar.gz")
	second := filepath.Join(t.TempDir(), "two.tar.gz")
	if err := Write(src, first); err != nil {
		t.Fatal(err)
	}
	if err := Write(later, second); err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("identical trees produced different archives")
	}
	// Overwriting an existing artifact is atomic and yields the same bytes.
	if err := Write(src, first); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, again) {
		t.Fatal("rewriting produced different archives")
	}
}

func TestWriteRejectsSymlink(t *testing.T) {
	dir := packageDir(t)
	if err := os.Symlink("/bin/sh", filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := Write(dir, filepath.Join(t.TempDir(), "pkg.tar.gz"))
	if err == nil || !strings.Contains(err.Error(), "symlinks are not allowed") {
		t.Fatalf("error = %v, want a symlink rejection", err)
	}
}

// --- streaming staging ------------------------------------------------------
//
// OpenReader is the transport form of OpenIn: the daemon receives a package as
// a .tar.gz stream and must stage it on the applications filesystem without
// writing the archive twice.

func TestOpenReaderStagesArchiveInsideParent(t *testing.T) {
	src := packageDir(t)
	archivePath := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := Write(src, archivePath); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	pkg, err := OpenReader(parent, bytes.NewReader(data), DefaultLimits)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	if !strings.HasPrefix(pkg.Dir, parent+string(os.PathSeparator)) {
		t.Fatalf("staged dir %q is not inside the caller's parent %q", pkg.Dir, parent)
	}
	body, err := os.ReadFile(filepath.Join(pkg.Dir, "assets", "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("asset body = %q", body)
	}
	info, err := os.Stat(filepath.Join(pkg.Dir, "bin", "app"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("streamed executable mode = %04o", info.Mode().Perm())
	}
	staged := pkg.Dir
	if err := pkg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged tree survived Close: %v", err)
	}
}

func TestOpenReaderRejectsInvalidStreams(t *testing.T) {
	if _, err := OpenReader(t.TempDir(), strings.NewReader("not gzip"), DefaultLimits); err == nil {
		t.Fatal("non-gzip stream was accepted")
	}
	if _, err := OpenReader(t.TempDir(), bytes.NewReader(nil), DefaultLimits); err == nil {
		t.Fatal("empty stream was accepted")
	}

	// A truncated archive must not leave a staged tree behind.
	src := packageDir(t)
	archivePath := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := Write(src, archivePath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if _, err := OpenReader(parent, bytes.NewReader(data[:len(data)/2]), DefaultLimits); err == nil {
		t.Fatal("truncated archive was accepted")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed open left %d entries behind", len(entries))
	}
}

func TestOpenReaderRequiresRootManifest(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "bin/app", Mode: 0o755, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("xx")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := OpenReader(t.TempDir(), bytes.NewReader(buf.Bytes()), DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), ManifestFile) {
		t.Fatalf("missing manifest error = %v, want a %s diagnostic", err, ManifestFile)
	}
}

func TestOpenReaderAppliesLimits(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "manifest.toml", testManifest, 0o644)
	writeFile(t, src, "bin/app", strings.Repeat("x", 4096), 0o755)
	archivePath := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := WriteLimit(src, archivePath, Limits{MaxFileBytes: 8192, MaxTotalBytes: 64 << 20, MaxEntries: 4096}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenReader(t.TempDir(), bytes.NewReader(data), Limits{MaxFileBytes: 1024, MaxTotalBytes: 64 << 20, MaxEntries: 4096})
	if err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("streamed limit error = %v, want the per-file limit", err)
	}
}
