package rtp

import (
	"bytes"
	"testing"
	"time"
)

func audioPacket(seq uint16, ts uint32) *Packet {
	return &Packet{Sequence: seq, Timestamp: ts, PayloadType: PayloadALAC}
}

func frameFor(seq uint16) []byte { return []byte{byte(seq >> 8), byte(seq)} }

func pop(t *testing.T, b *Buffer, wantSeq uint16, wantTS uint32) {
	t.Helper()
	frame, ts, ok := b.Pop()
	if !ok {
		t.Fatalf("Pop: no frame, want sequence %d", wantSeq)
	}
	if !bytes.Equal(frame, frameFor(wantSeq)) {
		t.Fatalf("Pop returned %x, want the frame of sequence %d", frame, wantSeq)
	}
	if ts != wantTS {
		t.Fatalf("Pop timestamp %d, want %d", ts, wantTS)
	}
}

func TestBufferReorders(t *testing.T) {
	b := NewBuffer(44100, 0)
	for _, seq := range []uint16{3, 1, 2} {
		b.Push(audioPacket(seq, uint32(seq)*352), frameFor(seq))
	}
	if got := b.Pending(); got != 3 {
		t.Fatalf("Pending = %d, want 3", got)
	}
	for _, seq := range []uint16{1, 2, 3} {
		pop(t, b, seq, uint32(seq)*352)
	}
	if _, _, ok := b.Pop(); ok {
		t.Error("Pop returned a frame from an empty buffer")
	}
	if got := b.Pending(); got != 0 {
		t.Errorf("Pending = %d, want 0", got)
	}
}

func TestBufferDropsDuplicatesAndLatePackets(t *testing.T) {
	b := NewBuffer(44100, 0)
	b.Push(audioPacket(1, 352), frameFor(1))
	b.Push(audioPacket(1, 352), frameFor(1)) // duplicate
	pop(t, b, 1, 352)
	b.Push(audioPacket(0, 0), frameFor(0)) // older than the next frame, i.e. late
	if got := b.Pending(); got != 0 {
		t.Fatalf("Pending = %d, want 0", got)
	}
	if s := b.Stats(); s.Received != 1 || s.Lost != 0 || s.HighestSeq != 1 {
		t.Fatalf("stats = %+v, want one received packet and no loss", s)
	}
}

func TestBufferSequenceWrap(t *testing.T) {
	b := NewBuffer(44100, 0)
	seqs := []uint16{65534, 65535, 0, 1}
	for i, seq := range seqs {
		b.Push(audioPacket(seq, uint32(i)*352), frameFor(seq))
	}
	for i, seq := range seqs {
		pop(t, b, seq, uint32(i)*352)
	}
	s := b.Stats()
	if s.Received != 4 || s.HighestSeq != 1 || s.Cycles != 1 {
		t.Fatalf("stats = %+v, want 4 received, highest 1, one cycle", s)
	}
	if s.Lost != 0 {
		t.Fatalf("Lost = %d, want 0 across a wrap", s.Lost)
	}
	if got := s.ExtendedHighestSeq(); got != 1<<16|1 {
		t.Fatalf("ExtendedHighestSeq = %d, want %d", got, 1<<16|1)
	}

	// Out of order delivery across the wrap is still ordered.
	b = NewBuffer(44100, 0)
	b.Push(audioPacket(2, 704), frameFor(2))
	b.Push(audioPacket(0, 0), frameFor(0))
	b.Push(audioPacket(1, 352), frameFor(1))
	pop(t, b, 0, 0)
	pop(t, b, 1, 352)
	pop(t, b, 2, 704)
}

func TestBufferTargetWindow(t *testing.T) {
	b := NewBuffer(44100, time.Second*352/44100)
	if b.targetTS <= 0 || b.targetTS > 352 {
		t.Fatalf("target window = %d timestamps, want one frame", b.targetTS)
	}
	b.Push(audioPacket(10, 0), frameFor(10))
	if _, _, ok := b.Pop(); ok {
		t.Fatal("Popped before the reorder window was satisfied")
	}
	if got := b.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1", got)
	}
	b.Push(audioPacket(11, 352), frameFor(11))
	pop(t, b, 10, 0)
	if _, _, ok := b.Pop(); ok {
		t.Fatal("Popped the last frame before the window was satisfied")
	}

	// A zero target releases immediately, and the window survives a
	// timestamp wrap.
	now := NewBuffer(44100, 0)
	now.Push(audioPacket(1, 1<<32-256), frameFor(1))
	pop(t, now, 1, 1<<32-256)
	wrap := NewBuffer(44100, time.Second*352/44100)
	wrap.Push(audioPacket(1, 1<<32-256), frameFor(1))
	wrap.Push(audioPacket(2, 96), frameFor(2))
	pop(t, wrap, 1, 1<<32-256)
}

func TestBufferMissing(t *testing.T) {
	b := NewBuffer(44100, 0)
	if got := b.Missing(); got != nil {
		t.Fatalf("Missing on an empty buffer = %v", got)
	}
	b.Push(audioPacket(10, 0), frameFor(10))
	b.Push(audioPacket(14, 4*352), frameFor(14))
	missing := b.Missing()
	want := []uint16{11, 12, 13}
	if len(missing) != len(want) {
		t.Fatalf("Missing = %v, want %v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("Missing = %v, want %v", missing, want)
		}
	}
	s := b.Stats()
	if s.Received != 2 || s.Lost != 3 || s.HighestSeq != 14 {
		t.Fatalf("stats = %+v, want 2 received, 3 lost, highest 14", s)
	}

	// The NACK list is capped at 64 entries.
	big := NewBuffer(44100, 0)
	big.Push(audioPacket(0, 0), frameFor(0))
	big.Push(audioPacket(200, 200*352), frameFor(200))
	missing = big.Missing()
	if len(missing) != maxNACKBatch {
		t.Fatalf("Missing returned %d entries, want %d", len(missing), maxNACKBatch)
	}
	if missing[0] != 1 || missing[len(missing)-1] != maxNACKBatch {
		t.Fatalf("Missing = %d..%d, want 1..%d", missing[0], missing[len(missing)-1], maxNACKBatch)
	}
	if pkts := NACKPacket(1, 2, missing); len(pkts) != 4 {
		t.Errorf("got %d NACK packets for %d missing, want 4", len(pkts), len(missing))
	}

	// Sequence numbers that arrive but cannot be decrypted are not missing.
	undecryptable := NewBuffer(44100, 0)
	undecryptable.Push(audioPacket(1, 0), frameFor(1))
	undecryptable.Push(audioPacket(2, 352), nil)
	undecryptable.Push(audioPacket(3, 704), frameFor(3))
	if got := undecryptable.Missing(); got != nil {
		t.Fatalf("Missing = %v, want nil", got)
	}
	if got := undecryptable.Pending(); got != 3 {
		t.Fatalf("Pending = %d, want 3", got)
	}
	pop(t, undecryptable, 1, 0)
	pop(t, undecryptable, 3, 704) // the undecryptable frame is skipped
	if _, _, ok := undecryptable.Pop(); ok {
		t.Error("Pop returned a frame for a packet that could not be decrypted")
	}
}

func TestBufferStatsAndFlush(t *testing.T) {
	b := NewBuffer(44100, 0)
	if s := b.Stats(); s != (Stats{}) {
		t.Fatalf("stats of an empty buffer = %+v", s)
	}
	for seq := uint16(1); seq <= 5; seq++ {
		b.Push(audioPacket(seq, uint32(seq)*352), frameFor(seq))
		// Space the arrivals out so that the jitter estimate stays sane.
		time.Sleep(time.Millisecond)
	}
	s := b.Stats()
	if s.Received != 5 || s.HighestSeq != 5 || s.Lost != 0 || s.Cycles != 0 {
		t.Fatalf("stats = %+v", s)
	}
	// The arrivals are 1 ms apart while the timestamps are 8 ms apart, so the
	// jitter estimate in timestamp units must be non-zero but small.
	if s.Jitter == 0 || s.Jitter > 44100/4 {
		t.Errorf("jitter = %d, want a small non-zero estimate", s.Jitter)
	}

	b.Flush()
	if got := b.Pending(); got != 0 {
		t.Errorf("Pending after Flush = %d", got)
	}
	if s := b.Stats(); s != (Stats{}) {
		t.Errorf("stats after Flush = %+v", s)
	}
	b.Push(audioPacket(7, 0), frameFor(7))
	pop(t, b, 7, 0)
}

// frameBytes returns a frame of n bytes whose contents identify seq, so byte
// accounting can be checked with frames larger than frameFor's two bytes.
func frameBytes(seq uint16, n int) []byte {
	f := make([]byte, 0, n)
	for len(f) < n {
		f = append(f, byte(seq>>8), byte(seq))
	}
	return f[:n]
}

// pushFrames pushes frames of the given size for seq in [from, to].
func pushFrames(b *Buffer, from, to uint16, size int) {
	for seq := from; seq <= to; seq++ {
		b.Push(audioPacket(seq, uint32(seq)*352), frameBytes(seq, size))
	}
}

func TestBufferDefaultByteLimit(t *testing.T) {
	b := NewBuffer(44100, 0)
	if b.limit != defaultByteLimit {
		t.Fatalf("limit = %d, want the %d-byte default", b.limit, defaultByteLimit)
	}
	// Well under the cap: nothing is evicted, and the accounting tracks the
	// frames in and out.
	for seq := uint16(1); seq <= 10; seq++ {
		b.Push(audioPacket(seq, uint32(seq)*352), frameFor(seq))
	}
	if got := b.Bytes(); got != 20 {
		t.Fatalf("Bytes = %d, want 20", got)
	}
	for seq := uint16(1); seq <= 10; seq++ {
		pop(t, b, seq, uint32(seq)*352)
	}
	if got := b.Bytes(); got != 0 {
		t.Fatalf("Bytes after playing everything = %d, want 0", got)
	}
	if s := b.Stats(); s.Dropped != 0 || s.DroppedBytes != 0 {
		t.Errorf("stats = %+v, want nothing dropped", s)
	}
}

func TestBufferByteLimitEvictsOldest(t *testing.T) {
	const size = 1000
	b := NewBuffer(44100, 0)
	b.SetByteLimit(2500)
	pushFrames(b, 1, 5, size)

	// Five 1000-byte frames against a 2500-byte cap: the newest two fit.
	if got := b.Bytes(); got != 2*size {
		t.Fatalf("Bytes = %d, want %d", got, 2*size)
	}
	if got := b.Pending(); got != 2 {
		t.Fatalf("Pending = %d, want 2", got)
	}
	s := b.Stats()
	if s.Received != 5 {
		t.Errorf("Received = %d, want 5: evicted packets were received", s.Received)
	}
	if s.Dropped != 3 || s.DroppedBytes != 3*size {
		t.Errorf("stats = %+v, want 3 packets and %d bytes dropped", s, 3*size)
	}
	if s.Lost != 0 {
		t.Errorf("Lost = %d, want 0: these packets were not lost on the network", s.Lost)
	}
	// Eviction advanced the stream past the dropped frames, so the loss
	// recovery loop does not keep asking for frames the buffer just discarded.
	if got := b.Missing(); got != nil {
		t.Errorf("Missing = %v, want nil", got)
	}
	// The survivors are the newest frames, in order.
	for seq := uint16(4); seq <= 5; seq++ {
		got, ts, ok := b.Pop()
		if !ok {
			t.Fatalf("Pop: no frame, want sequence %d", seq)
		}
		if !bytes.Equal(got, frameBytes(seq, size)) || ts != uint32(seq)*352 {
			t.Fatalf("Pop returned %d bytes at timestamp %d, want sequence %d", len(got), ts, seq)
		}
	}
	if _, _, ok := b.Pop(); ok {
		t.Error("Pop returned a frame from an emptied buffer")
	}
	if got := b.Bytes(); got != 0 {
		t.Errorf("Bytes after playing everything = %d, want 0", got)
	}
}

func TestBufferByteLimitDropsFrameLargerThanTheWholeCap(t *testing.T) {
	b := NewBuffer(44100, 0)
	b.SetByteLimit(100)
	b.Push(audioPacket(1, 0), frameBytes(1, 40))
	b.Push(audioPacket(2, 352), frameBytes(2, 400))

	// A frame larger than the entire cap is refused, so the bound always
	// holds; the earlier 40-byte frame that fits stays buffered.
	if got := b.Bytes(); got != 40 {
		t.Fatalf("Bytes = %d, want 40 (the frame that fits)", got)
	}
	if got := b.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1", got)
	}
	if got := b.Stats().DroppedBytes; got < 400 {
		t.Errorf("DroppedBytes = %d, want at least the 400-byte refused frame", got)
	}
	frame, _, ok := b.Pop()
	if !ok || len(frame) != 40 {
		t.Fatalf("Pop = (%d bytes, %v), want the 40-byte frame", len(frame), ok)
	}
}

// TestBufferByteLimitNonPositiveRestoresTheDefault pins the meaning of a limit
// that is not a limit: it puts the default cap back, rather than removing the
// cap or leaving a caller's tighter one in force.
func TestBufferByteLimitNonPositiveRestoresTheDefault(t *testing.T) {
	b := NewBuffer(44100, 0)
	b.SetByteLimit(150)
	b.SetByteLimit(0)
	if b.limit != defaultByteLimit {
		t.Fatalf("limit = %d after SetByteLimit(0), want the %d-byte default", b.limit, defaultByteLimit)
	}
	// 4096 bytes fit only because the default, not the 150-byte cap, is back.
	b.Push(audioPacket(1, 0), frameBytes(1, 4096))
	if got := b.Bytes(); got != 4096 {
		t.Fatalf("Bytes = %d, want 4096 under the default cap", got)
	}
}

// TestBufferSetByteLimitEvictsOnLowering covers the immediate eviction of a
// lowered cap and admitting more once it is raised again.
func TestBufferSetByteLimitEvictsOnLowering(t *testing.T) {
	b := NewBuffer(44100, 0)
	pushFrames(b, 1, 4, 100)
	if got := b.Bytes(); got != 400 {
		t.Fatalf("Bytes = %d, want 400", got)
	}
	b.SetByteLimit(150)
	if got := b.Bytes(); got != 100 {
		t.Fatalf("Bytes after lowering the limit = %d, want 100", got)
	}
	if s := b.Stats(); s.Dropped != 3 || s.DroppedBytes != 300 {
		t.Fatalf("stats = %+v, want 3 packets and 300 bytes dropped", s)
	}

	// Raising the cap admits more again: the frame that arrives is what
	// survives when only one still fits.
	b.SetByteLimit(500)
	pushFrames(b, 5, 8, 100)
	if got := b.Bytes(); got != 500 {
		t.Fatalf("Bytes with a 500-byte cap = %d, want 500", got)
	}
	if got := b.Pending(); got != 5 {
		t.Fatalf("Pending = %d, want 5", got)
	}
}

// TestBufferByteLimitCannotBeDisabled pins that there is no way to leave a
// receiver buffering without bound: the cap starts at the default, a limit of
// zero or less restores that default rather than removing the cap, and a
// leftover tiny cap still evicts instead of growing.
func TestBufferByteLimitCannotBeDisabled(t *testing.T) {
	b := NewBuffer(44100, 0)
	for _, limit := range []int{0, -1, -4096} {
		b.SetByteLimit(limit)
		if b.limit != defaultByteLimit {
			t.Fatalf("SetByteLimit(%d) left the cap at %d, want the %d-byte default",
				limit, b.limit, defaultByteLimit)
		}
	}
	// The restored cap is in force: under it the buffer evicts rather than
	// growing, and a frame larger than the cap is refused outright.
	b.SetByteLimit(150)
	pushFrames(b, 1, 8, 100)
	if got := b.Bytes(); got != 100 {
		t.Fatalf("Bytes = %d, want 100 under the 150-byte cap", got)
	}
	if s := b.Stats(); s.Dropped != 7 || s.DroppedBytes != 700 {
		t.Errorf("stats = %+v, want 7 packets and 700 bytes dropped", s)
	}

	// A refused frame is not buffered, so it must not look received and must
	// not invent loss. A buffer that refuses everything still reports what it
	// threw away.
	refusing := NewBuffer(44100, 0)
	refusing.SetByteLimit(50)
	refusing.Push(audioPacket(7, 0), frameBytes(7, 100))
	if got := refusing.Bytes(); got != 0 {
		t.Errorf("Bytes = %d, want 0: the frame does not fit", got)
	}
	if got := refusing.Pending(); got != 0 {
		t.Errorf("Pending = %d, want 0", got)
	}
	if s := refusing.Stats(); s.Received != 0 || s.Lost != 0 || s.Dropped != 1 || s.DroppedBytes != 100 {
		t.Errorf("stats = %+v, want one refused packet and no received or lost ones", s)
	}
}

func TestBufferByteLimitSurvivesFlush(t *testing.T) {
	b := NewBuffer(44100, 0)
	b.SetByteLimit(250)
	pushFrames(b, 1, 4, 100)
	b.Flush()
	if got := b.Bytes(); got != 0 {
		t.Fatalf("Bytes after Flush = %d, want 0", got)
	}
	if s := b.Stats(); s != (Stats{}) {
		t.Fatalf("stats after Flush = %+v, want zeroed counters", s)
	}
	// The limit is configuration, not state: it still applies to the next
	// stream.
	pushFrames(b, 1, 4, 100)
	if got := b.Bytes(); got != 200 {
		t.Fatalf("Bytes = %d, want 200", got)
	}
	if s := b.Stats(); s.Dropped != 2 || s.DroppedBytes != 200 {
		t.Errorf("stats = %+v, want 2 packets and 200 bytes dropped", s)
	}
}
