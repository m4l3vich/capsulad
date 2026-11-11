// Package logbus holds the daemon's bounded structured log ring and fan-out.
//
// Apps never write framework files directly; supervisor stdout/stderr and the
// structured Log RPC both land here. Records are bounded in memory and
// optionally mirrored to a file owned by the daemon. The mirror is bounded on
// disk too: the RK3308 keeps only 78.5 MiB on /data and an unbounded log file
// has already blocked a binary deployment.
package logbus

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// defaultMaxLogBytes bounds one mirrored log file. One rotated copy is
	// kept, so the daemon never owns more than twice this on /data.
	defaultMaxLogBytes = 512 << 10
)

type Bus struct {
	mu     sync.Mutex
	ring   []Entry
	max    int
	subs   map[int]*subscriber
	nextID int
	clock  *ids.Clock
	out    io.WriteCloser
}

// New creates a log bus retaining at most max entries. If path is non-empty,
// records are appended there with owner-only permissions, rotating once the
// file reaches defaultMaxLogBytes.
func New(clock *ids.Clock, max int, path string) (*Bus, error) {
	return newBus(clock, max, path, defaultMaxLogBytes)
}

func newBus(clock *ids.Clock, max int, path string, maxBytes int64) (*Bus, error) {
	if max <= 0 {
		max = defaultMaxEntries
	}
	b := &Bus{max: max, subs: map[int]*subscriber{}, clock: clock}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("log directory: %w", err)
		}
		f, err := openLogFile(path, maxBytes)
		if err != nil {
			return nil, fmt.Errorf("log file: %w", err)
		}
		b.out = f
	}
	return b, nil
}

// logFile is an append-only file that renames itself to <path>.1 when the
// current file would exceed max bytes, then starts a fresh file. A rotation
// failure keeps the existing file open and keeps writing: losing diagnostics
// is worse than an oversized file.
type logFile struct {
	path string
	max  int64
	f    *os.File
	size int64
}

func openLogFile(path string, max int64) (*logFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &logFile{path: path, max: max, f: f}
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	return l, nil
}

func (l *logFile) Write(p []byte) (int, error) {
	if l.max > 0 && l.size > 0 && l.size+int64(len(p)) > l.max {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) rotate() {
	_ = l.f.Close()
	_ = os.Remove(l.path + ".1")
	_ = os.Rename(l.path, l.path+".1")
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o600)
	if err != nil {
		// Reopen without truncation where possible so records are not lost.
		if f, err = os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
			return
		}
	}
	l.f = f
	l.size = 0
}

func (l *logFile) Close() error { return l.f.Close() }

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
	b.ring = append(b.ring, e)
	if len(b.ring) > b.max {
		copy(b.ring, b.ring[len(b.ring)-b.max:])
		b.ring = b.ring[:b.max]
	}
	if b.out != nil {
		fmt.Fprintf(b.out, "%s %-5s %-14s %-28s %s\n",
			now.UTC().Format(time.RFC3339Nano), e.Severity, e.Component, e.AppID, e.Message)
	}
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
	b.mu.Unlock()
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
