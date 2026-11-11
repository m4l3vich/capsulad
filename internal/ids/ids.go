// Package ids generates runtime-only identifiers.
//
// Presentation, capture, run, conversation, stream, invocation and activation
// IDs are runtime values. They are never persisted as stable app or resource
// identity; see ARCHITECTURE.md section 3.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var counter atomic.Uint64

// New returns a prefixed, unpredictable identifier. The counter suffix keeps
// IDs unique even if the random source is duplicated across a fork.
func New(prefix string) string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is not recoverable in a meaningful way here; fall
		// back to time+counter rather than emitting identical IDs.
		return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), counter.Add(1))
	}
	return prefix + "_" + hex.EncodeToString(b[:]) + fmt.Sprintf("%x", counter.Add(1)&0xff)
}

// Clock provides monotonic nanoseconds since an arbitrary origin (boot for the
// daemon). Wall-clock values are avoided across the wire for ordering.
type Clock struct {
	origin time.Time
}

func NewClock() *Clock { return &Clock{origin: time.Now()} }

func (c *Clock) NowNS() uint64 { return uint64(time.Since(c.origin).Nanoseconds()) }
