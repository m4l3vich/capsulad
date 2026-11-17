package session

// AirPlay 2 control datagrams, decoded exactly as Shairport Sync does.
//
// References (all shairport-sync 5.6.3 rtp.c): the AP2 control receiver
// (`rtp_ap2_control_receiver`, the 28-byte minimum, the sentinel bit and the
// first-packet rule, frames at +4/+16, the master time at +8, the clock id at
// +20), the resend request builder (`send_resend_request`, the 8-byte
// 80 D5 00 01 first count packet), and the resend response handling
// (`case 0xd6: decipher_player_put_packet(packet + 6, …)` with the comment
// "six bytes in is the sequence number at the start of the encrypted audio
// packet").
//
// A 0xD6 datagram has a four-byte control header followed by the **entire**
// embedded RTP packet (12-byte header, ciphertext, tag, nonce), so a resent
// payload-type-97 packet keeps its own header instead of being relabelled.
// Shairport hands `packet + 6` to its decipher because that routine starts at
// the RTP sequence number, i.e. two bytes into the RTP header; `packet + 4` is
// the complete packet.

import (
	"encoding/binary"
)

const (
	// ap2RealTimeStreamLatencyFudge is the frame offset upstream subtracts from
	// the anchor frame ("seems to bring everything into sync", rtp.c).
	ap2RealTimeStreamLatencyFudge = 11025
	// ap2RTPHeaderRestore is the RTP header prefix missing from a 0xD6 payload:
	// version 2 and payload type 96 (realtime audio).
	ap2RTPVersionFlags = 0x80
	ap2RTPRealtimePT   = 0x60
)

type controlKind int

const (
	controlUnknown controlKind = iota
	controlTimeAnnounce
	controlResendRequest
	controlResendResponse
	controlAnchor
)

// ap2Control is one decoded control datagram. Only the fields that belong to
// Kind are meaningful.
type ap2Control struct {
	Kind controlKind

	// Resend request (we send these; parsing is used by tests).
	RequestSequence uint16
	First           uint16
	Count           uint16

	// Resend response: a complete RTP packet, header restored.
	Payload []byte

	// Anchor (0xD7).
	AnchorFrame    uint32
	AnchorFrameRef uint32
	LatencyFrames  uint32
	RemoteTimeNS   uint64
	ClockID        uint64

	// Time announce (0xD4).
	RTPTimestamp uint32
	NTPSeconds   uint32
	NTPFraction  uint32
}

// parseAP2Control decodes one control datagram. It rejects anything shorter
// than the reference's 28-byte minimum.
func parseAP2Control(packet []byte) (ap2Control, bool) {
	if len(packet) < 28 {
		return ap2Control{}, false
	}
	switch packet[1] {
	case 215: // 0xD7 anchor
		return ap2Control{
			Kind:           controlAnchor,
			AnchorFrame:    binary.BigEndian.Uint32(packet[4:8]),
			RemoteTimeNS:   binary.BigEndian.Uint64(packet[8:16]),
			AnchorFrameRef: binary.BigEndian.Uint32(packet[16:20]),
			ClockID:        binary.BigEndian.Uint64(packet[20:28]),
			LatencyFrames:  binary.BigEndian.Uint32(packet[16:20]) - binary.BigEndian.Uint32(packet[4:8]),
		}, true
	case 0xd6: // resend response
		if len(packet) < 4+12 {
			return ap2Control{}, false
		}
		// Keep the embedded packet verbatim: only its own header says which
		// payload type (96, or 97 for redundancy) the sender resent.
		payload := make([]byte, len(packet)-4)
		copy(payload, packet[4:])
		return ap2Control{Kind: controlResendResponse, Payload: payload}, true
	case 0xd4: // time announce
		// The RTP timestamp paired with the NTP instant is the one at +16; the
		// value at +4 is the same timestamp less the latency (shairport reads
		// both and subtracts them for the latency).
		return ap2Control{
			Kind:         controlTimeAnnounce,
			RTPTimestamp: binary.BigEndian.Uint32(packet[16:20]),
			NTPSeconds:   binary.BigEndian.Uint32(packet[8:12]),
			NTPFraction:  binary.BigEndian.Uint32(packet[12:16]),
		}, true
	case 0xd5: // resend request (we build these; parse for tests)
		return ap2Control{
			Kind:            controlResendRequest,
			RequestSequence: binary.BigEndian.Uint16(packet[2:4]),
			First:           binary.BigEndian.Uint16(packet[4:6]),
			Count:           binary.BigEndian.Uint16(packet[6:8]),
		}, true
	}
	return ap2Control{}, false
}

// buildResendRequest renders the 8-byte AirPlay 2 resend request:
// 80 D5 <our sequence 1> <first missing> <count>, all big-endian.
func buildResendRequest(sequence, first, count uint16) []byte {
	request := make([]byte, 8)
	request[0] = 0x80
	request[1] = 0xd5
	binary.BigEndian.PutUint16(request[2:4], sequence)
	binary.BigEndian.PutUint16(request[4:6], first)
	binary.BigEndian.PutUint16(request[6:8], count)
	return request
}

// anchorPlayoutFrame is the frame the sender's master-clock instant refers to,
// with upstream's fudge factor applied to the anchored frame.
func (c ap2Control) anchorPlayoutFrame() uint32 {
	return c.AnchorFrame - ap2RealTimeStreamLatencyFudge
}
