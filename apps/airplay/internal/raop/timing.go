package raop

import "encoding/binary"

// RAOP timing packet types. rtp.c (rtp_control_receiver, rtp_timing_sender,
// rtp_timing_receiver) dispatches on the second byte of every packet.
const (
	// KindSync is a sync packet sent by the sender to the receiver's control
	// port (rtp.c: packet[1] == 0xd4).
	KindSync uint8 = 0xd4
	// KindTimingRequest is a timing request sent by the receiver to the
	// sender's timing port (rtp.c: struct timing_request, type 0xd2).
	KindTimingRequest uint8 = 0xd2
	// KindTimingReply is the sender's reply (rtp.c: packet[1] == 0xd3).
	KindTimingReply uint8 = 0xd3
)

const (
	// syncPacketLen is the fixed length of an AF1 sync packet. rtp.c reads
	// fields through byte 19; senders send exactly 20 bytes.
	syncPacketLen = 20
	// timingPacketLen is the fixed length of both the timing request and
	// reply packet (rtp.c reads the transmit timestamp from bytes 24..31).
	timingPacketLen = 32
	// timingSequence is the sequence number the reference puts in timing
	// requests: req.seqno = htons(7) in rtp.c rtp_timing_sender().
	timingSequence = 7
	// syncFlags is the flags field at bytes 2..3 of a sync packet. rtp.c
	// notes that iTunes sends 7 and AirPlay sends 4; a receiver that builds a
	// sync packet (for a test harness or an AirPlay 1 sender fallback) uses
	// the iTunes convention.
	syncFlags = 7
)

// TimingPacket is one RAOP sync/control/timing packet.
//
// Field meanings depend on Kind, because the three layouts carry different
// subsets of the clock information:
//
//	KindSync (0xd4), 20 bytes:
//	  [0]        0x80 (RTP version 2)
//	  [1]        0xd4
//	  [2:4]      flags, big-endian
//	  [4:8]      remote RTP timestamp less latency -> RTPRemote
//	  [8:16]     remote NTP timestamp -> NTPRemote
//	  [16:20]    remote RTP timestamp -> RTPTimestamp
//
//	KindTimingRequest (0xd2), 32 bytes (rtp.c struct timing_request):
//	  [0]        0x80
//	  [1]        0xd2
//	  [2:4]      sequence number, big-endian (7)
//	  [4:8]      filler, zero
//	  [8:16]     origin timestamp -> NTPNow
//	  [16:24]    receive timestamp, zero
//	  [24:32]    transmit timestamp, zero
//
//	KindTimingReply (0xd3), 32 bytes:
//	  [0]        0x80
//	  [1]        0xd3
//	  [2:4]      sequence number, big-endian
//	  [4:8]      filler
//	  [8:16]     origin (echo)
//	  [16:24]    remote receive timestamp -> NTPRemote
//	  [24:32]    remote transmit timestamp -> NTPNow
//
// RTPNow is the local (receiver) RTP timestamp at request send time and has
// no wire field in either 32-byte timing layout; Clock.Update takes it
// separately.
type TimingPacket struct {
	Kind         uint8
	RTPNow       uint32
	RTPRemote    uint32
	NTPNow       uint64
	NTPRemote    uint64
	RTPTimestamp uint32
}

// BuildSyncPacket encodes a 20-byte AF1 sync packet. The layout matches
// rtp.c rtp_control_receiver(), which reads the flags from packet[2], the
// latency-adjusted RTP timestamp from packet[4], the NTP timestamp from
// packet[8..15] and the RTP timestamp from packet[16..19].
//
// t.Kind may be zero or KindSync; any other Kind returns nil. t.RTPRemote
// lands at offset 4 and t.NTPRemote / t.RTPTimestamp land at offsets 8 and
// 16 respectively; the other fields are not part of a sync packet.
func BuildSyncPacket(t TimingPacket) []byte {
	if t.Kind != 0 && t.Kind != KindSync {
		return nil
	}
	b := make([]byte, syncPacketLen)
	b[0] = 0x80 // RTP version 2, no padding, extension or CSRC count
	b[1] = KindSync
	binary.BigEndian.PutUint16(b[2:4], syncFlags)
	binary.BigEndian.PutUint32(b[4:8], t.RTPRemote)
	binary.BigEndian.PutUint64(b[8:16], t.NTPRemote)
	binary.BigEndian.PutUint32(b[16:20], t.RTPTimestamp)
	return b
}

// BuildControlPacket encodes the 32-byte AF1 timing request the receiver
// sends to the sender's timing port. The layout is rtp.c's struct
// timing_request: leader 0x80, type 0xd2, sequence number 7, a zero filler,
// then three NTP timestamps.
//
// The reference zeroes all three timestamps (rtp.c sets
// req.origin = req.receive = req.transmit = 0 and measures the round trip
// with its own departure time). Filling the origin field with t.NTPNow is
// layout-compatible and lets the sender echo it; t.Kind may be zero or
// KindTimingRequest, otherwise nil is returned.
func BuildControlPacket(t TimingPacket) []byte {
	if t.Kind != 0 && t.Kind != KindTimingRequest {
		return nil
	}
	b := make([]byte, timingPacketLen)
	b[0] = 0x80
	b[1] = KindTimingRequest
	binary.BigEndian.PutUint16(b[2:4], timingSequence)
	binary.BigEndian.PutUint32(b[4:8], 0)
	binary.BigEndian.PutUint64(b[8:16], t.NTPNow)
	binary.BigEndian.PutUint64(b[16:24], 0)
	binary.BigEndian.PutUint64(b[24:32], 0)
	return b
}

// ParseTimingReply decodes a sender's 32-byte timing reply. The field offsets
// are the ones rtp.c rtp_timing_receiver() reads for a packet whose second
// byte is 0xd3: the remote receive timestamp at bytes 16..23 and the remote
// transmit timestamp at bytes 24..31. It returns false, without panicking,
// for a short packet, a packet with a non-2 RTP version, or a non-0xd3 type.
func ParseTimingReply(b []byte) (TimingPacket, bool) {
	if len(b) < timingPacketLen {
		return TimingPacket{}, false
	}
	if b[0]>>6 != 2 {
		return TimingPacket{}, false
	}
	if b[1] != KindTimingReply {
		return TimingPacket{}, false
	}
	return TimingPacket{
		Kind:      KindTimingReply,
		NTPRemote: binary.BigEndian.Uint64(b[16:24]),
		NTPNow:    binary.BigEndian.Uint64(b[24:32]),
	}, true
}
