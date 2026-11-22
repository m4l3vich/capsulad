package logbus

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"capsulad/internal/ids"
)

const testLogName = "capsulad.log"

func openTestBus(t *testing.T, opts Options) *Bus {
	t.Helper()
	opts.Clock = ids.NewClock()
	opts.MaxEntries = 64
	b, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return b
}

func closeBus(t *testing.T, b *Bus) {
	t.Helper()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type archiveFileInfo struct {
	name  string
	index int
	size  int64
	data  string
}

func readArchives(t *testing.T, dir string) []archiveFileInfo {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	prefix := testLogName + "."
	var out []archiveFileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(e.Name(), prefix))
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read archive %s: %v", e.Name(), err)
		}
		out = append(out, archiveFileInfo{name: e.Name(), index: index, size: int64(len(data)), data: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

func archiveTotal(files []archiveFileInfo) int64 {
	var total int64
	for _, f := range files {
		total += f.size
	}
	return total
}

// markers extracts every "MARK <n>" value in file order.
func markers(data string) []int {
	var out []int
	for _, line := range strings.Split(data, "\n") {
		i := strings.Index(line, "MARK ")
		if i < 0 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(line[i:], "MARK %d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func maxMarker(t *testing.T, data string) int {
	t.Helper()
	ms := markers(data)
	if len(ms) == 0 {
		t.Fatalf("no MARK records in:\n%s", data)
	}
	best := ms[0]
	for _, n := range ms[1:] {
		if n > best {
			best = n
		}
	}
	return best
}

func minMarker(t *testing.T, data string) int {
	t.Helper()
	ms := markers(data)
	if len(ms) == 0 {
		t.Fatalf("no MARK records in:\n%s", data)
	}
	best := ms[0]
	for _, n := range ms[1:] {
		if n < best {
			best = n
		}
	}
	return best
}

func TestPartialArchivesCountTowardTotal(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archivesDir := t.TempDir()
	// Eight small archives fit the byte budget even though four full-size
	// rotations would not; pruning must follow bytes, not a fixed slot count.
	for i := 1; i <= 8; i++ {
		name := filepath.Join(archivesDir, fmt.Sprintf("%s.%d", testLogName, i))
		if err := os.WriteFile(name, []byte(strings.Repeat("p", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := openTestBus(t, Options{
		Path:            active,
		ArchiveDir:      archivesDir,
		MaxBytes:        1000,
		ArchiveMaxBytes: 4000,
	})
	padding := strings.Repeat("q", 200)
	for i := range 4 {
		b.Infof("test", "", "MARK %04d %s", i, padding)
	}
	closeBus(t, b)

	files := readArchives(t, archivesDir)
	if len(files) <= 4 {
		t.Fatalf("partial archives were pruned by count: %v", archiveNames(files))
	}
	if total := archiveTotal(files); total > 4000 {
		t.Fatalf("archive total = %d, cap is 4000", total)
	}
	for i, f := range files {
		if f.name != testLogName+"."+strconv.Itoa(i+1) {
			t.Fatalf("archives not contiguous at %d: %v", i, archiveNames(files))
		}
	}
}

func TestRotationAtActiveCap(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archivesDir := t.TempDir()
	b := openTestBus(t, Options{Path: active, ArchiveDir: archivesDir})
	padding := strings.Repeat("x", 900)
	for i := range 2600 {
		b.Infof("test", "", "MARK %04d %s", i, padding)
	}
	closeBus(t, b)

	info, err := os.Stat(active)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > DefaultMaxBytes {
		t.Fatalf("active log is %d bytes, cap is %d", info.Size(), DefaultMaxBytes)
	}
	files := readArchives(t, archivesDir)
	if len(files) != 4 {
		t.Fatalf("archive count = %d, want 4 (names %v)", len(files), archiveNames(files))
	}
	if total := archiveTotal(files); total > DefaultArchiveMaxBytes {
		t.Fatalf("archive total = %d, cap is %d", total, DefaultArchiveMaxBytes)
	}
	for i, f := range files {
		if f.name != testLogName+"."+strconv.Itoa(i+1) {
			t.Fatalf("archive %d named %s, want %s.%d", i, f.name, testLogName, i+1)
		}
	}
	for i := 0; i+1 < len(files); i++ {
		newer, older := maxMarker(t, files[i].data), maxMarker(t, files[i+1].data)
		if newer <= older {
			t.Fatalf("%s newest marker %d <= %s marker %d", files[i].name, newer, files[i+1].name, older)
		}
	}
	activeData, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	if got := maxMarker(t, string(activeData)); got != 2599 {
		t.Fatalf("active newest marker = %d, want 2599", got)
	}
	if maxMarker(t, files[0].data) >= minMarker(t, string(activeData)) {
		t.Fatalf("newest archive overlaps the active file")
	}
	if strings.Contains(string(activeData), "MARK 0000") {
		t.Fatalf("oldest record still in the active file")
	}
}

func TestArchiveNamingOrderAndPrune(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archivesDir := t.TempDir()
	b := openTestBus(t, Options{
		Path:            active,
		ArchiveDir:      archivesDir,
		MaxBytes:        1000,
		ArchiveMaxBytes: 4000,
	})
	padding := strings.Repeat("y", 200)
	for i := range 60 {
		b.Infof("test", "", "MARK %04d %s", i, padding)
	}
	closeBus(t, b)

	files := readArchives(t, archivesDir)
	if len(files) != 4 {
		t.Fatalf("archive count = %d, want 4: %v", len(files), archiveNames(files))
	}
	for i, f := range files {
		if f.name != testLogName+"."+strconv.Itoa(i+1) {
			t.Fatalf("archive %d named %s", i, f.name)
		}
	}
	if total := archiveTotal(files); total > 4000 {
		t.Fatalf("archive total = %d, cap is 4000", total)
	}
	for i := 0; i+1 < len(files); i++ {
		if maxMarker(t, files[i].data) <= maxMarker(t, files[i+1].data) {
			t.Fatalf("%s is not newer than %s", files[i].name, files[i+1].name)
		}
	}
	all := readAllLogs(t, active, archivesDir)
	if strings.Contains(all, "MARK 0000") {
		t.Fatalf("oldest record was not pruned:\n%s", all)
	}
	if !strings.Contains(all, "MARK 0059") {
		t.Fatalf("newest record missing:\n%s", all)
	}
}

func TestStartupRotatesOversizedActiveFile(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archivesDir := t.TempDir()
	old := strings.Repeat("old record\n", int(DefaultMaxBytes/4)+500)
	if err := os.WriteFile(active, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	b := openTestBus(t, Options{Path: active, ArchiveDir: archivesDir})
	b.Infof("test", "", "MARK 9999 fresh record")
	closeBus(t, b)

	info, err := os.Stat(active)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= int64(len(old)) {
		t.Fatalf("oversized active file was not rotated (size %d)", info.Size())
	}
	files := readArchives(t, archivesDir)
	if len(files) == 0 {
		t.Fatal("no archive after startup rotation")
	}
	if !strings.Contains(files[0].data, "old record") {
		t.Fatalf("startup archive lost the old records: %q", files[0].data[:64])
	}
	if total := archiveTotal(files); total > DefaultArchiveMaxBytes {
		t.Fatalf("archive total = %d, cap is %d", total, DefaultArchiveMaxBytes)
	}
	activeData, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(activeData), "MARK 9999") {
		t.Fatalf("write after startup rotation missing: %q", activeData)
	}
}

func TestArchiveDirCreated(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archiveDir := filepath.Join(base, "nested", "capsulad-logs")
	b := openTestBus(t, Options{Path: active, ArchiveDir: archiveDir})
	closeBus(t, b)

	info, err := os.Stat(archiveDir)
	if err != nil {
		t.Fatalf("archive dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", archiveDir)
	}
	if perm := info.Mode().Perm(); perm != 0o750 {
		t.Fatalf("archive dir mode = %o, want 750", perm)
	}
}

func TestWriteContinuityAcrossRotation(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archivesDir := t.TempDir()
	b := openTestBus(t, Options{
		Path:            active,
		ArchiveDir:      archivesDir,
		MaxBytes:        1024,
		ArchiveMaxBytes: 1 << 20,
	})
	padding := strings.Repeat("z", 100)
	for i := range 60 {
		b.Infof("test", "", "MARK %04d %s", i, padding)
	}
	closeBus(t, b)

	if len(readArchives(t, archivesDir)) < 2 {
		t.Fatal("expected several rotations")
	}
	all := readAllLogs(t, active, archivesDir)
	for i := range 60 {
		want := fmt.Sprintf("MARK %04d", i)
		if !strings.Contains(all, want) {
			t.Fatalf("%s dropped across rotation", want)
		}
	}
}

func TestRotationFailureKeepsWriting(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archivesDir := filepath.Join(base, "archives")
	b := openTestBus(t, Options{
		Path:            active,
		ArchiveDir:      archivesDir,
		MaxBytes:        256,
		ArchiveMaxBytes: 1024,
	})
	// Replace the archive directory with a regular file so archiving fails at
	// write time, after a successful Open.
	if err := os.Remove(archivesDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivesDir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("f", 100)
	for i := range 20 {
		b.Infof("test", "", "MARK %04d %s", i, padding)
	}
	closeBus(t, b)

	info, err := os.Stat(active)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 256 {
		t.Fatalf("active log stopped growing after rotation failure (size %d)", info.Size())
	}
	data, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		want := fmt.Sprintf("MARK %04d", i)
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s dropped after rotation failure", want)
		}
	}
	var warned bool
	for _, e := range b.Snapshot("") {
		if e.Component == "logbus" && strings.Contains(e.Message, "rotation failed") {
			warned = true
			break
		}
	}
	if !warned {
		t.Fatal("rotation failure was not reported through the log ring")
	}
}

func TestLogFileAcceptsOversizedRecord(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	b := openTestBus(t, Options{
		Path:            active,
		ArchiveDir:      t.TempDir(),
		MaxBytes:        16,
		ArchiveMaxBytes: 1024,
	})
	b.Infof("test", "", "one record that is longer than the whole rotation cap")
	closeBus(t, b)

	data, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "longer than the whole rotation cap") {
		t.Fatalf("oversized record dropped: %q", data)
	}
}

func TestLegacyBackupMigrated(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, testLogName)
	archivesDir := t.TempDir()
	if err := os.WriteFile(active, []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active+".1", []byte("legacy backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := openTestBus(t, Options{Path: active, ArchiveDir: archivesDir})
	closeBus(t, b)

	if _, err := os.Stat(active + ".1"); !os.IsNotExist(err) {
		t.Fatalf("legacy backup still beside the active file: %v", err)
	}
	files := readArchives(t, archivesDir)
	if len(files) != 1 {
		t.Fatalf("archive count = %d, want 1: %v", len(files), archiveNames(files))
	}
	if !strings.Contains(files[0].data, "legacy backup") {
		t.Fatalf("legacy backup not migrated: %q", files[0].data)
	}
}

func readAllLogs(t *testing.T, active, archiveDir string) string {
	t.Helper()
	var b strings.Builder
	data, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	b.Write(data)
	for _, f := range readArchives(t, archiveDir) {
		b.WriteString(f.data)
	}
	return b.String()
}

func archiveNames(files []archiveFileInfo) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.name)
	}
	return names
}
