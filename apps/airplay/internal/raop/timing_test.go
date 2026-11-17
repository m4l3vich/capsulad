package raop

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBuildSyncPacket(t *testing.T) {
	in := TimingPacket{
		Kind:         KindSync,
		RTPRemote:    0x11223344,
		NTPRemote:    0xA1B2C3D4E5F60718,
		RTPTimestamp: 0x99AABBCC,
	}
	b := BuildSyncPacket(in)
	if len(b) != syncPacketLen {
		t.Fatalf("len = %d, want %d", len(b), syncPacketLen)
	}

	// Byte-level assertions, not a re-decode of our own fields.
	if b[0] != 0x80 {
		t.Errorf("b[0] = %#x, want 0x80", b[0])
	}
	if b[1] != 0xd4 {
		t.Errorf("b[1] = %#x, want 0xd4", b[1])
	}
	if got := binary.BigEndian.Uint16(b[2:4]); got != syncFlags {
		t.Errorf("flags = %d, want %d", got, syncFlags)
	}
	if got := binary.BigEndian.Uint32(b[4:8]); got != 0x11223344 {
		t.Errorf("bytes 4..7 = %#x, want 0x11223344", got)
	}
	if got := binary.BigEndian.Uint64(b[8:16]); got != 0xA1B2C3D4E5F60718 {
		t.Errorf("bytes 8..15 = %#x, want 0xA1B2C3D4E5F60718", got)
	}
	if got := binary.BigEndian.Uint32(b[16:20]); got != 0x99AABBCC {
		t.Errorf("bytes 16..19 = %#x, want 0x99AABBCC", got)
	}

	// Round trip through the documented offsets.
	got := TimingPacket{
		Kind:         b[1],
		RTPRemote:    binary.BigEndian.Uint32(b[4:8]),
		NTPRemote:    binary.BigEndian.Uint64(b[8:16]),
		RTPTimestamp: binary.BigEndian.Uint32(b[16:20]),
	}
	if got.RTPRemote != in.RTPRemote || got.NTPRemote != in.NTPRemote ||
		got.RTPTimestamp != in.RTPTimestamp || got.Kind != KindSync {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}

	if b := BuildSyncPacket(TimingPacket{Kind: KindTimingReply}); b != nil {
		t.Errorf("BuildSyncPacket with wrong Kind = %x, want nil", b)
	}
}

func TestBuildControlPacket(t *testing.T) {
	const origin uint64 = 0xE903A2B180000000
	b := BuildControlPacket(TimingPacket{Kind: KindTimingRequest, NTPNow: origin})
	if len(b) != timingPacketLen {
		t.Fatalf("len = %d, want %d", len(b), timingPacketLen)
	}
	if b[0] != 0x80 || b[1] != 0xd2 {
		t.Errorf("header = %#x %#x, want 0x80 0xd2", b[0], b[1])
	}
	if got := binary.BigEndian.Uint16(b[2:4]); got != timingSequence {
		t.Errorf("sequence = %d, want %d", got, timingSequence)
	}
	if got := binary.BigEndian.Uint32(b[4:8]); got != 0 {
		t.Errorf("filler = %#x, want 0", got)
	}
	if got := binary.BigEndian.Uint64(b[8:16]); got != origin {
		t.Errorf("origin = %#x, want %#x", got, origin)
	}
	if !bytes.Equal(b[16:32], make([]byte, 16)) {
		t.Errorf("receive/transmit = %x, want zero", b[16:32])
	}

	if b := BuildControlPacket(TimingPacket{Kind: KindSync}); b != nil {
		t.Errorf("BuildControlPacket with wrong Kind = %x, want nil", b)
	}
}

func TestParseTimingReply(t *testing.T) {
	// Hand-built 0xd3 reply, following rtp.c rtp_timing_receiver(): the
	// receive timestamp is at bytes 16..23 and the transmit timestamp at
	// bytes 24..31. This builder is independent of BuildControlPacket.
	const (
		origin   uint64 = 0x1111111122222222
		receive  uint64 = 0xAABBCCDDEEFF0011
		transmit uint64 = 0x123456789ABCDEF0
	)
	b := make([]byte, timingPacketLen)
	b[0] = 0x80
	b[1] = 0xd3
	binary.BigEndian.PutUint16(b[2:4], 7)
	binary.BigEndian.PutUint64(b[8:16], origin)
	binary.BigEndian.PutUint64(b[16:24], receive)
	binary.BigEndian.PutUint64(b[24:32], transmit)

	p, ok := ParseTimingReply(b)
	if !ok {
		t.Fatal("ParseTimingReply returned false for a valid reply")
	}
	if p.Kind != KindTimingReply {
		t.Errorf("Kind = %#x, want %#x", p.Kind, KindTimingReply)
	}
	if p.NTPRemote != receive {
		t.Errorf("NTPRemote = %#x, want receive timestamp %#x", p.NTPRemote, receive)
	}
	if p.NTPNow != transmit {
		t.Errorf("NTPNow = %#x, want transmit timestamp %#x", p.NTPNow, transmit)
	}
}

func TestParseTimingReplyRejectsMalformed(t *testing.T) {
	valid := make([]byte, timingPacketLen)
	valid[0] = 0x80
	valid[1] = 0xd3

	for n := range timingPacketLen {
		if _, ok := ParseTimingReply(valid[:n]); ok {
			t.Errorf("ParseTimingReply(%d bytes) = true, want false", n)
		}
	}

	wrongType := append([]byte(nil), valid...)
	wrongType[1] = 0xd4
	if _, ok := ParseTimingReply(wrongType); ok {
		t.Error("sync packet accepted as a timing reply")
	}

	wrongVersion := append([]byte(nil), valid...)
	wrongVersion[0] = 0x40
	if _, ok := ParseTimingReply(wrongVersion); ok {
		t.Error("RTP version 1 packet accepted")
	}

	if _, ok := ParseTimingReply(nil); ok {
		t.Error("nil packet accepted")
	}
	// Longer packets (e.g. a buffer with trailing data) are parsed from the
	// first 32 bytes.
	if _, ok := ParseTimingReply(append(valid, 0xff, 0xff)); !ok {
		t.Error("valid reply with trailing bytes rejected")
	}
}

// TestTimingPacketNoPanic exercises the parser over every prefix and a few
// random-looking buffers.
func TestTimingPacketNoPanic(t *testing.T) {
	base := make([]byte, timingPacketLen)
	base[0], base[1] = 0x80, 0xd3
	for i := range len(base) + 1 {
		_, _ = ParseTimingReply(base[:i])
	}
	for _, b := range [][]byte{{0x00}, {0xff}, bytes.Repeat([]byte{0xd3}, 40)} {
		_, _ = ParseTimingReply(b)
	}
}
