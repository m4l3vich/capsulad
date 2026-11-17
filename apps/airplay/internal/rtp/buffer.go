package rtp

import (
	"sync"
	"time"
)

// maxNACKBatch bounds the number of sequence numbers Missing reports, so that
// a long loss burst cannot produce an unbounded NACK.
const maxNACKBatch = 64

// defaultByteLimit caps the audio a Buffer holds when the caller has not set a
// limit: 4 MiB, which is around ten seconds of 44.1 kHz stereo ALAC at the
// frame sizes AirPlay senders use.
const defaultByteLimit = 4 << 20

// Stats is the receiver side bookkeeping of a buffer, shaped like the fields an
// RTCP receiver report carries.
type Stats struct {
	// Received counts the distinct sequence numbers that were pushed.
	Received uint32
	// Lost counts the sequence numbers that are missing between the first
	// received one and the highest one seen.
	Lost uint32
	// HighestSeq is the highest sequence number seen, and Cycles counts how
	// often it wrapped.
	HighestSeq uint16
	Cycles     uint32
	// Jitter is the RTP interarrival jitter estimate in timestamp units, as
	// defined by RFC 3550 A.8.
	Jitter uint32
	// Dropped counts the packets the byte limit kept out of the buffer, whether
	// they were evicted to make room for newer audio or refused because a
	// single frame did not fit under the cap at all; DroppedBytes is the audio
	// they held. They are counted separately from Lost because they did arrive:
	// the audio is gone either way, but only Lost measures the network.
	Dropped      uint32
	DroppedBytes uint32
}

// ExtendedHighestSeq is the highest sequence number received, extended with
// the number of wraps.
func (s Stats) ExtendedHighestSeq() uint32 {
	return s.Cycles<<16 | uint32(s.HighestSeq)
}

// entry is one buffered packet. A nil frame marks a sequence number that was
// received but could not be decrypted: it is not missing, but there is nothing
// to play out for it.
type entry struct {
	frame []byte
	ts    uint32
}

// Buffer reorders audio packets by their RTP sequence number. It tolerates
// sequence and timestamp wrap around, drops duplicates and late packets, and
// only hands a frame to the caller once the stream has advanced far enough for
// reordering to be pointless, which is what target expresses: a frame is
// released when packets have arrived whose timestamps are at least target
// later than the frame's own, that is target of audio has been buffered behind
// it. A target of zero releases frames immediately.
//
// It is also bounded in bytes, so that a sink which cannot keep up (or a
// stalled playout) cannot grow the buffer without limit: pushing past the limit
// evicts the oldest frames and a frame larger than the whole cap is refused, so
// the cap is a hard bound rather than a target. Both are counted in Stats. The
// cap cannot be removed. The zero value is not usable; call NewBuffer.
type Buffer struct {
	// mu guards the whole buffer: a sender-side reader pushes while the playout
	// goroutine pops, so the two sides are separate goroutines by design.
	mu        sync.Mutex
	clockRate uint32
	targetTS  int32

	entries   map[uint16]entry
	next      uint16
	started   bool
	delivered bool

	highest   uint16
	cycles    uint32
	base      uint16
	received  uint32
	highestTS uint32
	maxTS     bool

	jitter    uint32
	lastTrans int64
	haveTrans bool
	start     time.Time

	// bytes is the audio currently held in entries, limit the at-least-one-byte
	// cap it must fit under, and dropped/droppedBytes count the entries the cap
	// evicted.
	bytes        int
	limit        int
	dropped      uint32
	droppedBytes uint32
}

// NewBuffer returns a reordering buffer for a stream with the given clock rate
// that holds back target of audio and at most 4 MiB of it. Use SetByteLimit to
// change the cap.
func NewBuffer(clockRate uint32, target time.Duration) *Buffer {
	if target < 0 {
		target = 0
	}
	return &Buffer{
		clockRate: clockRate,
		targetTS:  int32(int64(target) * int64(clockRate) / int64(time.Second)),
		entries:   make(map[uint16]entry),
		limit:     defaultByteLimit,
		start:     time.Now(),
	}
}

// SetByteLimit caps how much audio the buffer holds. There is no way to remove
// the cap: a limit of zero or less restores the default cap. Lowering the cap
// below what is already buffered evicts the oldest frames immediately, exactly
// as a push over the cap would.
func (b *Buffer) SetByteLimit(limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit < 1 {
		limit = defaultByteLimit
	}
	b.limit = limit
	b.evict(0)
}

// Bytes reports how many bytes of audio are currently buffered.
func (b *Buffer) Bytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes
}

// Push adds a packet and its decrypted payload. The frame slice is kept by
// reference until it is returned by Pop, so it must not be reused before that.
// Packets that are older than the next frame to be delivered, and duplicates,
// are dropped. The byte limit is a hard bound: a frame that cannot fit under
// the cap at all is refused outright, and one that would push the buffer past
// it evicts the oldest buffered frames first. Both are counted in
// Stats.Dropped.
func (b *Buffer) Push(pkt *Packet, plaintext []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(plaintext) > b.limit {
		// A single frame larger than the whole cap cannot be held without
		// breaking the bound. RTP datagrams are at most 64 KiB, so this only
		// happens when a caller has set a limit below one frame; the frame is
		// counted as dropped and the stream state is left untouched.
		b.dropped++
		b.droppedBytes += uint32(len(plaintext))
		return
	}
	seq := pkt.Sequence
	if b.started {
		if _, dup := b.entries[seq]; dup {
			return
		}
		if int16(seq-b.next) < 0 {
			// Behind the next frame to deliver: either a duplicate of an
			// already delivered frame, or too late to be useful. Before the
			// first frame has been handed out, an earlier sequence number
			// simply moves the window backwards, which is what reordering
			// needs.
			if b.delivered {
				return
			}
			b.next = seq
			b.base = seq
		}
	} else {
		b.started = true
		b.next = seq
		b.highest = seq
		b.base = seq
	}
	b.evict(len(plaintext))
	b.entries[seq] = entry{frame: plaintext, ts: pkt.Timestamp}
	b.bytes += len(plaintext)
	b.received++
	if int16(seq-b.highest) > 0 {
		if seq < b.highest {
			b.cycles++
		}
		b.highest = seq
	}
	if !b.maxTS || int32(pkt.Timestamp-b.highestTS) > 0 {
		b.highestTS = pkt.Timestamp
		b.maxTS = true
	}
	b.trackJitter(pkt)
}

// evict drops the oldest buffered frames until need more bytes fit under the
// limit. It must be called with the lock held, and only for a frame that can
// fit: Push refuses anything larger than the whole cap, and both entry points
// into the buffer (NewBuffer and SetByteLimit) keep the cap at one byte or
// more, so no path can exceed the bound.
func (b *Buffer) evict(need int) {
	for b.bytes+need > b.limit {
		seq, e, ok := b.front()
		if !ok {
			return
		}
		delete(b.entries, seq)
		b.bytes -= len(e.frame)
		b.dropped++
		b.droppedBytes += uint32(len(e.frame))
		if seq == b.next {
			// The frame that would have been delivered next is gone, so the
			// stream has to move past it: otherwise Missing would report it
			// forever and keep asking for a resend that cannot help.
			b.next = seq + 1
			b.delivered = true
		}
	}
}

// trackJitter updates the RFC 3550 A.8 interarrival jitter estimate.
func (b *Buffer) trackJitter(pkt *Packet) {
	arrival := int64(time.Since(b.start)) * int64(b.clockRate) / int64(time.Second)
	// The RTP timestamp is interpreted as signed so that the estimate stays
	// continuous across a timestamp wrap.
	transit := arrival - int64(int32(pkt.Timestamp))
	if b.haveTrans {
		d := transit - b.lastTrans
		if d < 0 {
			d = -d
		}
		b.jitter = uint32(int64(b.jitter) + (d-int64(b.jitter))/16)
	}
	b.lastTrans = transit
	b.haveTrans = true
}

// front returns the buffered packet with the lowest sequence number, which is
// the next one in stream order.
func (b *Buffer) front() (uint16, entry, bool) {
	if len(b.entries) == 0 {
		return 0, entry{}, false
	}
	best := uint16(0)
	bestDelta := int16(0)
	first := true
	for seq := range b.entries {
		d := int16(seq - b.next)
		if first || d < bestDelta {
			best, bestDelta, first = seq, d, false
		}
	}
	return best, b.entries[best], true
}

// ready reports whether enough audio has arrived behind e for reordering to be
// pointless.
func (b *Buffer) ready(e entry) bool {
	if b.targetTS <= 0 {
		return true
	}
	return int32(b.highestTS-e.ts) >= b.targetTS
}

// Pop returns the next frame in stream order. It reports ok=false while the
// target reorder window has not been satisfied or nothing is buffered.
func (b *Buffer) Pop() (frame []byte, timestamp uint32, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		seq, e, found := b.front()
		if !found {
			return nil, 0, false
		}
		if e.frame == nil {
			// Received but undecryptable: nothing to hand out, but it must
			// not look missing either.
			delete(b.entries, seq)
			b.next = seq + 1
			b.delivered = true
			continue
		}
		if !b.ready(e) {
			return nil, 0, false
		}
		delete(b.entries, seq)
		b.bytes -= len(e.frame)
		b.next = seq + 1
		b.delivered = true
		return e.frame, e.ts, true
	}
}

// Pending reports how many packets are buffered.
func (b *Buffer) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// Missing lists the sequence numbers between the next frame to deliver and the
// highest one received that are not buffered, capped at 64 entries, which is
// what a receiver asks to be resent.
func (b *Buffer) Missing() []uint16 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started || len(b.entries) == 0 {
		return nil
	}
	var missing []uint16
	gap := int16(b.highest - b.next)
	if gap <= 0 {
		return nil
	}
	for i := int16(0); i < gap && len(missing) < maxNACKBatch; i++ {
		seq := b.next + uint16(i)
		if _, ok := b.entries[seq]; !ok {
			missing = append(missing, seq)
		}
	}
	return missing
}

// Stats reports the buffer's counters.
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	// The drop counters are meaningful before the first frame is buffered: a
	// buffer whose cap refuses everything still has to report what it threw
	// away.
	s := Stats{Dropped: b.dropped, DroppedBytes: b.droppedBytes}
	if !b.started {
		return s
	}
	s.Received = b.received
	s.HighestSeq = b.highest
	s.Cycles = b.cycles
	s.Jitter = b.jitter
	expected := b.cycles<<16 + uint32(b.highest) - uint32(b.base) + 1
	if expected > b.received {
		s.Lost = expected - b.received
	}
	return s
}

// Flush discards every buffered frame and resets the buffer, including its
// counters, so that the next stream starts fresh. The byte limit is
// configuration and survives.
func (b *Buffer) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.entries)
	b.started = false
	b.delivered = false
	b.next = 0
	b.highest = 0
	b.cycles = 0
	b.base = 0
	b.received = 0
	b.highestTS = 0
	b.maxTS = false
	b.jitter = 0
	b.lastTrans = 0
	b.haveTrans = false
	b.start = time.Now()
	b.bytes = 0
	b.dropped = 0
	b.droppedBytes = 0
}
