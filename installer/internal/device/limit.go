package device

import (
	"bytes"
	"errors"
)

// ErrOutputLimit reports that a bounded device read exceeded its cap.
var ErrOutputLimit = errors.New("device output exceeded the read limit")

// Read limits used by command construction. They are deliberately generous for
// the expected text and tiny for structured replies.
const (
	MaxTextRead     int64 = 1 << 20
	MaxSessionRead  int64 = 64 << 10
	MaxBackupStream int64 = 1 << 30
)

// limitedBuffer is a bytes.Buffer that refuses to grow past limit.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int64
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && int64(b.buf.Len()+len(p)) > b.limit {
		b.overflow = true
		return 0, ErrOutputLimit
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// limitWriter bounds a binary stream and remembers the overflow.
type limitWriter struct {
	w         interface{ Write([]byte) (int, error) }
	remaining int64
	overflow  bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.remaining {
		l.overflow = true
		return 0, ErrOutputLimit
	}
	n, err := l.w.Write(p)
	l.remaining -= int64(n)
	return n, err
}
