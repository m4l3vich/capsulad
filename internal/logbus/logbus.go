// Package logbus holds the daemon's bounded structured log ring and fan-out.
//
// Apps never write framework files directly; supervisor stdout/stderr and the
// structured Log RPC both land here. Records are bounded in memory and
// optionally mirrored to a file owned by the daemon. The mirror is bounded on
// disk too: the active file stays under 500 KiB on /data and rotated copies
// move to a 2 MiB archive under /tmp, because the RK3308 keeps only 78.5 MiB
// on /data and an unbounded log file has already blocked a binary deployment.
package logbus

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"capsulad/internal/ids"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	default:
		return "INFO"
	}
}

// Entry is a single log record. TimeNS is monotonic since daemon start.
type Entry struct {
	TimeNS    uint64
	Component string
	AppID     string
	Severity  string
	Message   string
	wall      time.Time
}

type subscriber struct {
	appID string
	ch    chan Entry
}

const (
	defaultMaxEntries = 4000

	// DefaultMaxBytes bounds the active daemon log on the data filesystem.
	// Once the next record would exceed it, the file is rotated away so /data
	// never holds more than one bounded log file.
	DefaultMaxBytes int64 = 500 << 10

	// DefaultArchiveMaxBytes bounds every rotated log together. Rotated files
	// live outside /data (normally /tmp) so a full archive cannot block a
	// deployment; four 500 KiB files fit exactly.
	DefaultArchiveMaxBytes int64 = 2 << 20

	// defaultArchiveDirName is created under os.TempDir() when no archive
	// directory is configured.
	defaultArchiveDirName = "capsulad-logs"
)

// Options configure a Bus. Zero values select the defaults above.
type Options struct {
	Clock *ids.Clock
	// MaxEntries bounds the in-memory ring; zero means 4000.
	MaxEntries int
	// Path is the active log file. Empty disables file mirroring and archives.
	Path string
	// ArchiveDir receives rotated files. Empty means
	// filepath.Join(os.TempDir(), "capsulad-logs").
	ArchiveDir string
	// MaxBytes bounds the active file; zero means DefaultMaxBytes.
	MaxBytes int64
	// ArchiveMaxBytes bounds all archives together; zero means
	// DefaultArchiveMaxBytes.
	ArchiveMaxBytes int64
}

type Bus struct {
	mu     sync.Mutex
	ring   []Entry
	max    int
	subs   map[int]*subscriber
	nextID int
	clock  *ids.Clock
	out    *logFile
}

// New creates a bus with the default rotation layout. It is shorthand for
// Open(Options{Clock: clock, MaxEntries: max, Path: path}).
func New(clock *ids.Clock, max int, path string) (*Bus, error) {
	return Open(Options{Clock: clock, MaxEntries: max, Path: path})
}

// Open creates a log bus from opts. A non-empty Path mirrors records there with
// owner-only permissions; the file is rotated into ArchiveDir on a write once
// it would exceed MaxBytes.
func Open(opts Options) (*Bus, error) {
	max := opts.MaxEntries
	if max <= 0 {
		max = defaultMaxEntries
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	archiveMax := opts.ArchiveMaxBytes
	if archiveMax <= 0 {
		archiveMax = DefaultArchiveMaxBytes
	}
	b := &Bus{max: max, subs: map[int]*subscriber{}, clock: opts.Clock}
	if opts.Path == "" {
		return b, nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, fmt.Errorf("log directory: %w", err)
	}
	archiveDir := opts.ArchiveDir
	if archiveDir == "" {
		archiveDir = filepath.Join(os.TempDir(), defaultArchiveDirName)
	}
	f, err := openLogFile(opts.Path, archiveDir, maxBytes, archiveMax)
	if err != nil {
		return nil, fmt.Errorf("log file: %w", err)
	}
	b.out = f
	return b, nil
}

// logFile is an append-only active file with a bounded archive of rotated
// copies. When the next write would exceed max bytes the active file moves to
// archiveDir as <base>.1 (newest) and older copies shift up; only enough
// copies to stay under archiveMax bytes are kept. A rotation failure keeps the
// active file usable and appends there: losing diagnostics is worse than an
// oversized log. The failure is reported once through the Bus ring.
type logFile struct {
	path       string
	archiveDir string
	max        int64
	archiveMax int64
	f          *os.File
	size       int64
	canRotate  bool
	rotateErr  error
}

func openLogFile(path, archiveDir string, max, archiveMax int64) (*logFile, error) {
	l := &logFile{
		path:       path,
		archiveDir: archiveDir,
		max:        max,
		archiveMax: archiveMax,
		canRotate:  true,
	}
	if err := os.MkdirAll(archiveDir, 0o750); err != nil {
		// Archive trouble must not stop logging: keep appending to the active
		// file and report the condition on the first record.
		l.canRotate = false
		l.noteRotateError(fmt.Errorf("create archive directory %s: %w", archiveDir, err))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l.f = f
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	if l.canRotate {
		if err := l.migrateLegacy(); err != nil {
			l.noteRotateError(err)
		}
		if l.canRotate && l.max > 0 && l.size > l.max {
			_ = l.rotate()
		}
	}
	return l, nil
}

func (l *logFile) noteRotateError(err error) {
	if l.rotateErr == nil {
		l.rotateErr = err
	}
}

func (l *logFile) takeRotateError() error {
	err := l.rotateErr
	l.rotateErr = nil
	return err
}

func (l *logFile) Write(p []byte) (int, error) {
	if l.f == nil {
		return 0, errors.New("logbus: log file unavailable")
	}
	if l.canRotate && l.max > 0 && l.size > 0 && l.size+int64(len(p)) > l.max {
		_ = l.rotate()
	}
	if l.f == nil {
		return 0, errors.New("logbus: log file unavailable")
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) rotate() error {
	if l.f != nil {
		_ = l.f.Close()
	}
	if err := l.archiveActive(); err != nil {
		// Keep the active file (and its records) usable. Rotation stays off so
		// a failing archive is not retried for every record; a restart retries
		// and rotates an oversized active file.
		l.canRotate = false
		l.noteRotateError(err)
		f, reopenErr := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if reopenErr != nil {
			l.f = nil
			return errors.Join(err, reopenErr)
		}
		l.f = f
		if st, statErr := f.Stat(); statErr == nil {
			l.size = st.Size()
		}
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o600)
	if err != nil {
		// The old file is gone, so appending creates a fresh one. If it is not
		// gone (truncation failed), keep its real size for the next rotation.
		if f, err = os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
			l.f = nil
			return err
		}
		if st, statErr := f.Stat(); statErr == nil {
			l.size = st.Size()
			l.f = f
			return nil
		}
	}
	l.f = f
	l.size = 0
	return nil
}

// archiveFile is one rotated copy; index 1 is the newest.
type archiveFile struct {
	index int
	size  int64
}

// listArchives returns the recognized rotated copies, oldest first (index
// descending).
func (l *logFile) listArchives() ([]archiveFile, error) {
	entries, err := os.ReadDir(l.archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read archive directory: %w", err)
	}
	prefix := filepath.Base(l.path) + "."
	var out []archiveFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err != nil || index <= 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, archiveFile{index: index, size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index > out[j].index })
	return out, nil
}

// prune removes the oldest archives until newSize plus the survivors fits the
// byte budget. It returns the survivors, oldest first.
func (l *logFile) prune(archives []archiveFile, newSize int64) ([]archiveFile, error) {
	total := newSize
	for _, a := range archives {
		total += a.size
	}
	i := 0
	for i < len(archives) && total > l.archiveMax {
		name := filepath.Join(l.archiveDir, filepath.Base(l.path)+"."+strconv.Itoa(archives[i].index))
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("prune archive %s: %w", name, err)
		}
		total -= archives[i].size
		i++
	}
	return archives[i:], nil
}

func (l *logFile) archiveActive() error {
	size := l.size
	if size <= 0 {
		return nil
	}
	if err := os.MkdirAll(l.archiveDir, 0o750); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}
	keep := size
	if l.archiveMax > 0 && keep > l.archiveMax {
		// One file can never fit the archive budget; preserve its newest
		// records instead of dropping the file entirely.
		keep = l.archiveMax
	}
	archives, err := l.listArchives()
	if err != nil {
		return err
	}
	archives, err = l.prune(archives, keep)
	if err != nil {
		return err
	}
	prefix := filepath.Base(l.path) + "."
	// Shift older copies up before the newest takes .1. Descending order keeps
	// renames collision-free.
	for _, a := range archives {
		from := filepath.Join(l.archiveDir, prefix+strconv.Itoa(a.index))
		to := filepath.Join(l.archiveDir, prefix+strconv.Itoa(a.index+1))
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("shift archive %s: %w", from, err)
		}
	}
	if err := l.moveActive(filepath.Join(l.archiveDir, prefix+"1"), keep); err != nil {
		return err
	}
	return nil
}

func (l *logFile) moveActive(target string, keep int64) error {
	if keep >= l.size {
		return moveFile(l.path, target, l.size, l.size)
	}
	// The active file stays in place; rotate truncates it afterwards.
	return copyTail(l.path, target, l.size, keep)
}

// moveFile renames source to target, falling back to a copy across
// filesystems. A failed copy leaves no partial archive behind.
func moveFile(source, target string, size, keep int64) error {
	if keep >= size {
		if err := os.Rename(source, target); err == nil {
			return nil
		}
	}
	if err := copyTail(source, target, size, keep); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("remove %s: %w", source, err)
	}
	return nil
}

// copyTail copies the newest keep bytes of source into target.
func copyTail(source, target string, size, keep int64) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := src.Seek(size-keep, io.SeekStart); err != nil {
		return err
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	if err := dst.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		_ = os.Remove(target)
		return copyErr
	}
	return nil
}

// migrateLegacy moves the single <path>.1 backup left by the previous rotation
// scheme into the archive directory as the oldest copy, so upgrading neither
// duplicates nor strands it. When the archive lives beside the active file the
// name already matches the new scheme and is left alone.
func (l *logFile) migrateLegacy() error {
	legacy := l.path + ".1"
	info, err := os.Stat(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy log %s: %w", legacy, err)
	}
	if info.IsDir() || filepath.Clean(l.archiveDir) == filepath.Clean(filepath.Dir(l.path)) {
		return nil
	}
	keep := info.Size()
	if l.archiveMax > 0 && keep > l.archiveMax {
		keep = l.archiveMax
	}
	archives, err := l.listArchives()
	if err != nil {
		return err
	}
	archives, err = l.prune(archives, keep)
	if err != nil {
		return err
	}
	next := 1
	if len(archives) > 0 {
		next = archives[0].index + 1
	}
	target := filepath.Join(l.archiveDir, filepath.Base(l.path)+"."+strconv.Itoa(next))
	if err := moveFile(legacy, target, info.Size(), keep); err != nil {
		return fmt.Errorf("migrate legacy log: %w", err)
	}
	return nil
}

func (l *logFile) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, sub := range b.subs {
		close(sub.ch)
		delete(b.subs, id)
	}
	if b.out != nil {
		return b.out.Close()
	}
	return nil
}

func (b *Bus) Log(component, appID string, level Level, format string, args ...any) {
	now := time.Now()
	e := Entry{
		TimeNS:    b.clock.NowNS(),
		Component: component,
		AppID:     appID,
		Severity:  level.String(),
		Message:   fmt.Sprintf(format, args...),
		wall:      now,
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(e)
	if b.out != nil {
		writeEntry(b.out, now, e)
	}
	b.publishLocked(e)
	if b.out == nil {
		return
	}
	if err := b.out.takeRotateError(); err != nil {
		// Rotation failures are surfaced once in the ring so the Log RPC and
		// the file both show why archives stopped; appending continues.
		w := Entry{
			TimeNS:    b.clock.NowNS(),
			Component: "logbus",
			Severity:  Warn.String(),
			Message:   fmt.Sprintf("log rotation failed: %v; appending to %s without rotating", err, b.out.path),
			wall:      now,
		}
		b.appendLocked(w)
		writeEntry(b.out, now, w)
		b.publishLocked(w)
	}
}

func (b *Bus) appendLocked(e Entry) {
	b.ring = append(b.ring, e)
	if len(b.ring) > b.max {
		copy(b.ring, b.ring[len(b.ring)-b.max:])
		b.ring = b.ring[:b.max]
	}
}

func (b *Bus) publishLocked(e Entry) {
	for _, sub := range b.subs {
		if sub.appID != "" && e.AppID != sub.appID && e.AppID != "" {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			// Slow log consumers are dropped rather than blocking the daemon.
		}
	}
}

func writeEntry(w io.Writer, now time.Time, e Entry) {
	fmt.Fprintf(w, "%s %-5s %-14s %-28s %s\n",
		now.UTC().Format(time.RFC3339Nano), e.Severity, e.Component, e.AppID, e.Message)
}

func (b *Bus) Debugf(component, appID, format string, args ...any) {
	b.Log(component, appID, Debug, format, args...)
}
func (b *Bus) Infof(component, appID, format string, args ...any) {
	b.Log(component, appID, Info, format, args...)
}
func (b *Bus) Warnf(component, appID, format string, args ...any) {
	b.Log(component, appID, Warn, format, args...)
}
func (b *Bus) Errorf(component, appID, format string, args ...any) {
	b.Log(component, appID, Error, format, args...)
}

// Snapshot returns the retained entries with the given app filter applied.
// An empty appID means all entries; framework entries have an empty app ID and
// are included when filtering by app so diagnostics show surrounding context.
func (b *Bus) Snapshot(appID string) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, 0, len(b.ring))
	for _, e := range b.ring {
		if appID == "" || e.AppID == appID || e.AppID == "" {
			out = append(out, e)
		}
	}
	return out
}

// Subscription receives entries as they are logged.
type Subscription struct {
	C    <-chan Entry
	id   int
	bus  *Bus
	once sync.Once
}

func (s *Subscription) Close() {
	s.once.Do(func() {
		s.bus.mu.Lock()
		if sub, ok := s.bus.subs[s.id]; ok {
			close(sub.ch)
			delete(s.bus.subs, s.id)
		}
		s.bus.mu.Unlock()
	})
}

func (b *Bus) Subscribe(appID string, buffer int) *Subscription {
	if buffer <= 0 {
		buffer = 64
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	ch := make(chan Entry, buffer)
	b.subs[b.nextID] = &subscriber{appID: appID, ch: ch}
	return &Subscription{C: ch, id: b.nextID, bus: b}
}

// FormatLine renders one record for CLI output.
func (e Entry) FormatLine() string {
	ts := time.Duration(e.TimeNS).Truncate(time.Millisecond)
	comp := e.Component
	if e.AppID != "" {
		comp = comp + "/" + e.AppID
	}
	return fmt.Sprintf("%10s  %-5s  %-40s %s", ts, e.Severity, comp, e.Message)
}
