package rtp

import "encoding/binary"

// RTCP packet types used by AirPlay audio.
const (
	rtcpSenderReport   = 200
	rtcpReceiverReport = 201
	rtcpRTPFeedback    = 205 // RFC 4585, FMT 1 is a generic NACK
)

// receiverReportBlocks is the number of report blocks ReceiverReport emits. The
// receiver report header's report count field is the number of blocks that
// follow the sender SSRC (RFC 3550 6.4.2), so the two must agree.
const receiverReportBlocks = 1

// ReceiverReport builds a minimal RTCP receiver report (32 bytes, version 2,
// payload type 201, one report block) describing s. ssrc is the SSRC of the
// sender of the report, source the SSRC that is being reported on. The last
// sender report fields (LSR/DLSR) are left zero: the buffer does not track
// sender reports.
//
// This is a RAOP helper: the AirPlay 2 control plane uses the sender's own
// 0xD4-0xD7 datagrams on the UDP control socket (see
// internal/session/controlpackets.go), not RTCP, so nothing in the AirPlay 2
// path calls this.
func ReceiverReport(ssrc, source uint32, s Stats) []byte {
	b := make([]byte, 32)
	b[0] = 0x80 | receiverReportBlocks // version 2, no padding, one report block
	b[1] = rtcpReceiverReport
	binary.BigEndian.PutUint16(b[2:4], 7) // length in 32-bit words, minus one
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint32(b[8:12], source)

	if total := uint64(s.Received) + uint64(s.Lost); total > 0 {
		fraction := uint64(s.Lost) * 256 / total
		if fraction > 255 {
			fraction = 255
		}
		b[12] = byte(fraction)
	}
	cumulative := s.Lost
	if cumulative > 0x7fffff {
		cumulative = 0x7fffff
	}
	b[13] = byte(cumulative >> 16)
	b[14] = byte(cumulative >> 8)
	b[15] = byte(cumulative)
	binary.BigEndian.PutUint32(b[16:20], s.ExtendedHighestSeq())
	binary.BigEndian.PutUint32(b[20:24], s.Jitter)
	return b
}

// NACKPacket builds the RFC 4585 generic NACK (RTCP payload type 205, FMT 1)
// packets asking for the given missing sequence numbers to be resent. Each
// returned packet carries a single feedback control information word and
// therefore covers at most 17 sequence numbers, the packet it names plus the
// 16 bits of its bitmask.
//
// This is a RAOP helper: AirPlay 2 asks for a resend with the sender's own
// 0xD5 request on the UDP control socket (see
// internal/session/controlpackets.go), consistently with the 0xD6 responses it
// receives, so nothing in the AirPlay 2 path calls this.
func NACKPacket(ssrc, source uint32, missing []uint16) [][]byte {
	if len(missing) == 0 {
		return nil
	}
	var out [][]byte
	for i := 0; i < len(missing); {
		pid := missing[i]
		var mask uint16
		n := 1
		for n < 17 && i+n < len(missing) {
			if missing[i+n] != pid+uint16(n) {
				break
			}
			mask |= 1 << (n - 1)
			n++
		}
		pkt := make([]byte, 16)
		pkt[0] = 0x81 // version 2, FMT 1: generic NACK
		pkt[1] = rtcpRTPFeedback
		binary.BigEndian.PutUint16(pkt[2:4], 3)
		binary.BigEndian.PutUint32(pkt[4:8], ssrc)
		binary.BigEndian.PutUint32(pkt[8:12], source)
		binary.BigEndian.PutUint16(pkt[12:14], pid)
		binary.BigEndian.PutUint16(pkt[14:16], mask)
		out = append(out, pkt)
		i += n
	}
	return out
}

// ParseSenderReport extracts the timing of an RTCP sender report: the NTP
// seconds and fraction of the sender's wall clock and the RTP timestamp that
// corresponds to them. ok is false for anything that is not a complete sender
// report, which lets a caller feed it any RTCP packet it receives.
func ParseSenderReport(b []byte) (ntpSeconds uint32, ntpFraction uint32, rtpTimestamp uint32, ok bool) {
	if len(b) < 28 || b[0]>>6 != 2 || b[1] != rtcpSenderReport {
		return 0, 0, 0, false
	}
	return binary.BigEndian.Uint32(b[8:12]), binary.BigEndian.Uint32(b[12:16]), binary.BigEndian.Uint32(b[16:20]), true
}
