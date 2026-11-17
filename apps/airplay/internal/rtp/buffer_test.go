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
