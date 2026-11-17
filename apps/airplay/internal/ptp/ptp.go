// Package ptp implements the IEEE 1588-2008 (PTPv2) message layer and the slave
// clock that AirPlay 2 audio uses to place the sender's audio timestamps on the
// receiver's own clock.
//
// In an AirPlay 2 session the sender is the PTP grandmaster: it multicasts Sync,
// Follow_Up and Announce to the PTP event port (319) and general port (320) of
// 224.0.0.1, and the receiver, as the slave, answers with Delay_Req on the event
// port and correlates the Delay_Resp that comes back.
//
// The wire layout below was cross-checked read-only against nqptp, the de-facto
// AirPlay PTP daemon, because AirPlay senders use the 802.1AS-flavoured "Apple"
// profile rather than the plain IEEE 1588 default profile:
//
//   - https://raw.githubusercontent.com/mikebrady/nqptp/master/nqptp-ptp-definitions.h
//     struct ptp_common_message_header (34-byte header layout), ptp_sync,
//     ptp_follow_up, ptp_announce, ptp_delay_req, ptp_delay_resp, enum
//     messageType (Table 19) and enum controlFieldValue (Table 23).
//   - https://raw.githubusercontent.com/mikebrady/nqptp/master/nqptp-message-handlers.c
//     handle_follow_up: the 48-bit-seconds + 32-bit-nanoseconds split of a
//     timestamp, the correctionField / 65536 scaling, and the 802.1AS 11.4.4.2.1
//     rule that the preciseOriginTimestamp is corrected by the sum of the
//     Follow_Up and Sync correction fields.
//   - https://raw.githubusercontent.com/mikebrady/nqptp/master/nqptp.c
//     the receive loop: the message type is the low nibble of byte 0 and the
//     transportSpecific nibble is the high nibble (its debug printer matches
//     0x10|type); send_awakening_announcement_sequence shows the byte values
//     that daemon puts in its own messages.
//
// nqptp is GPLv2 and was consulted for field offsets and constants only; no code
// was copied. This package is stdlib-only and uses no cgo.
//
// Parse and MarshalBinary cover the five message types of that exchange (Sync,
// Delay_Req, Follow_Up, Delay_Resp, Announce) and every other type is rejected,
// so an unusual message is logged rather than misread. Clock turns matched
// Sync/Follow_Up pairs into a drifting estimate of (master - local): the delay
// exchange itself is left to the caller, which sends the Delay_Req that
// NewDelayReq builds and parses the Delay_Resp that answers it.
package ptp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// PTP transport of the AirPlay 2 timing exchange.
const (
	// PortEvent is the PTP event port, which carries Sync and Delay_Req.
	PortEvent = 319
	// PortGeneral is the PTP general port, which carries Announce, Follow_Up
	// and Delay_Resp.
	PortGeneral = 320
	// MulticastV4 is the IPv4 multicast group PTP messages are sent to.
	MulticastV4 = "224.0.0.1"
)

// Wire constants.
const (
	// headerLen is the size of the common message header (IEEE 1588-2008 13.3;
	// ptp_common_message_header in the reference).
	headerLen = 34
	// versionPTP is the only PTP version this package implements: the low
	// nibble of byte 1 (0x02 in the reference).
	versionPTP = 2
	// transportSpecificApple is the high nibble of byte 0 of the Apple profile:
	// the reference matches received messages on 0x10|messageType and sends its
	// own with the same high nibble.
	transportSpecificApple = 1
	// maxSeconds48 is the largest value the 48-bit seconds field can hold.
	maxSeconds48 = 1 << 48
	// nanosPerSecond is the number of nanoseconds in a second.
	nanosPerSecond = 1_000_000_000
	// maxUnixSeconds bounds a Timestamp whose nanoseconds fit in an int64.
	maxUnixSeconds = math.MaxInt64 / nanosPerSecond
	// correctionScale is 2^16: correctionField counts units of 2^-16 ns.
	correctionScale = 1 << 16
	// controlDelayReq is the controlField of a Delay_Req
	// (Control_Field_Value_Delay_Req, IEEE 1588-2008 Table 23).
	controlDelayReq = 1
	// logIntervalDelayReq is the logMessageInterval a Delay_Req carries
	// (0x7F, IEEE 1588-2008 13.4).
	logIntervalDelayReq = 0x7F
)

// Errors returned by Parse, MarshalBinary and the clock accessors. They are
// wrapped with detail, so callers test them with errors.Is.
var (
	// ErrShortPacket means the datagram is smaller than the 34-byte common
	// header.
	ErrShortPacket = errors.New("ptp: packet shorter than the common header")
	// ErrVersion means the versionPTP nibble is not 2.
	ErrVersion = errors.New("ptp: unsupported PTP version")
	// ErrLength means messageLength disagrees with the datagram: it either
	// exceeds the datagram or is too small to hold the header and the body the
	// message type requires.
	ErrLength = errors.New("ptp: inconsistent messageLength")
	// ErrType means the message type is not one this package decodes.
	ErrType = errors.New("ptp: unsupported message type")
	// ErrTimestamp means a timestamp field is out of range: nanoseconds of one
	// second or more, or seconds that do not fit the 48-bit field.
	ErrTimestamp = errors.New("ptp: timestamp out of range")
	// ErrCorrection means the correction field does not fit 2^-16 ns units.
	ErrCorrection = errors.New("ptp: correction field out of range")
)

// MessageType is the low nibble of byte 0 of a PTP message
// (enum messageType, IEEE 1588-2008 Table 19).
type MessageType uint8

// The message types of the AirPlay 2 timing exchange.
const (
	MsgSync      MessageType = 0x0
	MsgDelayReq  MessageType = 0x1
	MsgFollowUp  MessageType = 0x8
	MsgDelayResp MessageType = 0x9
	MsgAnnounce  MessageType = 0xB
)

// bodyLen returns the body length, after the common header, that a message type
// needs before it can be decoded, and reports whether the type is one this
// package handles. The sizes follow ptp_sync / ptp_delay_req / ptp_follow_up
// (10 bytes), ptp_delay_resp (20 bytes) and ptp_announce (30 bytes) in the
// reference definitions.
func bodyLen(t MessageType) (int, bool) {
	switch t {
	case MsgSync, MsgDelayReq, MsgFollowUp:
		return 10, true
	case MsgDelayResp:
		return 20, true
	case MsgAnnounce:
		return 30, true
	default:
		return 0, false
	}
}

// Timestamp is a PTP timestamp: 48-bit seconds plus 32-bit nanoseconds, both
// big-endian on the wire (the reference reads it as a 16-bit second half, a
// 32-bit second half and then the nanoseconds: handle_follow_up in
// nqptp-message-handlers.c).
type Timestamp struct {
	Seconds uint64
	Nanos   uint32
}

// nanos returns the timestamp as nanoseconds, and reports whether it can be
// represented: the reference sends PTP timescale seconds, which for the
// foreseeable future fit an int64 of nanoseconds.
func (t Timestamp) nanos() (int64, bool) {
	if t.Seconds > maxUnixSeconds || uint64(t.Nanos) >= nanosPerSecond {
		return 0, false
	}
	return int64(t.Seconds)*nanosPerSecond + int64(t.Nanos), true
}

// Header is the common message header, minus the octets no AirPlay profile
// field is carried in.
type Header struct {
	// Type is the message type (low nibble of byte 0).
	Type MessageType
	// Version is versionPTP (low nibble of byte 1); always 2 for messages this
	// package produces.
	Version uint8
	// Domain is the domainNumber (byte 4); AirPlay senders use domain 0.
	Domain uint8
	// Flags is the flagField (bytes 6-7). The Apple profile sets the
	// twoStepFlag and ptpTimescale bits, but nothing here depends on it.
	Flags uint16
	// CorrectionNanos is the correctionField (bytes 8-15) in nanoseconds. The
	// wire value counts 2^-16 ns, so the sub-nanosecond fraction is truncated
	// towards zero (as the reference does with its /65536).
	CorrectionNanos int64
	// SourceClockID is the clockIdentity of the sender (bytes 20-27).
	SourceClockID [8]byte
	// SourcePort is the portNumber of the sender (bytes 28-29).
	SourcePort uint16
	// SequenceID is the sequenceId (bytes 30-31); it pairs a Sync with its
	// Follow_Up and a Delay_Req with its Delay_Resp.
	SequenceID uint16
	// Control is the controlField (byte 32): 0 Sync, 1 Delay_Req, 2 Follow_Up,
	// 3 Delay_Resp, 5 other.
	Control uint8
	// LogInterval is the logMessageInterval (byte 33), the log2 of the interval
	// in seconds: AirPlay senders sync at -3 (125 ms) and announce at 0 (1 s).
	LogInterval int8
}

// Message is a decoded PTP message. Only the fields the slave needs are
// decoded; everything else stays in Raw and is reproduced by MarshalBinary.
type Message struct {
	Header Header

	// OriginTimestamp is the originTimestamp of a Sync or Delay_Req, or the
	// preciseOriginTimestamp of a Follow_Up. A two-step Sync leaves it zero and
	// the precise time arrives in the Follow_Up.
	OriginTimestamp Timestamp
	// HasOrigin reports that OriginTimestamp was present in the message.
	HasOrigin bool

	// ReceiveTimestamp is the receiveTimestamp of a Delay_Resp.
	ReceiveTimestamp Timestamp
	// HasReceive reports that ReceiveTimestamp was present in the message.
	HasReceive bool
	// RequestingClockID and RequestingPort identify the Delay_Req a Delay_Resp
	// answers (its requestingPortIdentity).
	RequestingClockID [8]byte
	RequestingPort    uint16

	// GrandmasterID is the grandmasterIdentity of an Announce.
	GrandmasterID [8]byte
	// HasGrandmaster reports that GrandmasterID was present in the message.
	HasGrandmaster bool
	// UTCOffset is the currentUtcOffset of an Announce, in seconds.
	UTCOffset int16

	// Raw is the message as it appeared on the wire, in the form Parse read it:
	// it aliases the slice given to Parse (trimmed to messageLength) and is nil
	// for messages built by NewDelayReq or as a composite literal. It is also
	// the template MarshalBinary patches the decoded fields into, so the
	// reserved octets and any trailing TLVs survive a parse/encode round trip.
	Raw []byte

	// transportSpecific is the high nibble of byte 0 (1 in the Apple profile).
	transportSpecific uint8
}

// Parse decodes a PTP datagram. It accepts the message types of the AirPlay 2
// timing exchange (Sync, Delay_Req, Follow_Up, Delay_Resp and Announce) and
// rejects anything malformed: a datagram shorter than the common header, a
// version other than 2, a messageLength that does not cover the type's body, a
// timestamp out of range, or a type this package does not decode. It never
// panics.
func Parse(b []byte) (*Message, error) {
	if len(b) < headerLen {
		return nil, fmt.Errorf("%w: %d bytes, header is %d", ErrShortPacket, len(b), headerLen)
	}
	version := b[1] & 0x0F
	if version != versionPTP {
		return nil, fmt.Errorf("%w: %d", ErrVersion, version)
	}
	msgType := MessageType(b[0] & 0x0F)
	need, known := bodyLen(msgType)
	if !known {
		return nil, fmt.Errorf("%w: 0x%X", ErrType, b[0]&0x0F)
	}
	length := int(binary.BigEndian.Uint16(b[2:4]))
	if length < headerLen+need || length > len(b) {
		return nil, fmt.Errorf("%w: %d bytes declared, datagram is %d bytes and %s needs at least %d",
			ErrLength, length, len(b), typeName(msgType), headerLen+need)
	}

	m := &Message{
		Raw:               b[:length],
		transportSpecific: b[0] >> 4,
		Header: Header{
			Type:            msgType,
			Version:         version,
			Domain:          b[4],
			Flags:           binary.BigEndian.Uint16(b[6:8]),
			CorrectionNanos: int64(binary.BigEndian.Uint64(b[8:16])) / correctionScale,
			SourcePort:      binary.BigEndian.Uint16(b[28:30]),
			SequenceID:      binary.BigEndian.Uint16(b[30:32]),
			Control:         b[32],
			LogInterval:     int8(b[33]),
		},
	}
	copy(m.Header.SourceClockID[:], b[20:28])

	body := b[headerLen:length]
	switch msgType {
	case MsgSync, MsgDelayReq, MsgFollowUp:
		ts, err := parseTimestamp(body[:10])
		if err != nil {
			return nil, err
		}
		m.OriginTimestamp, m.HasOrigin = ts, true
	case MsgDelayResp:
		ts, err := parseTimestamp(body[:10])
		if err != nil {
			return nil, err
		}
		m.ReceiveTimestamp, m.HasReceive = ts, true
		copy(m.RequestingClockID[:], body[10:18])
		m.RequestingPort = binary.BigEndian.Uint16(body[18:20])
	case MsgAnnounce:
		ts, err := parseTimestamp(body[:10])
		if err != nil {
			return nil, err
		}
		m.OriginTimestamp, m.HasOrigin = ts, true
		// ptp_announce: currentUtcOffset, then reserved1, priority1, the
		// grandmaster clockQuality and priority2, and only then the
		// grandmasterIdentity at body offset 19.
		m.UTCOffset = int16(binary.BigEndian.Uint16(body[10:12]))
		copy(m.GrandmasterID[:], body[19:27])
		m.HasGrandmaster = true
	}
	return m, nil
}

// typeName names a message type for error messages.
func typeName(t MessageType) string {
	switch t {
	case MsgSync:
		return "Sync"
	case MsgDelayReq:
		return "Delay_Req"
	case MsgFollowUp:
		return "Follow_Up"
	case MsgDelayResp:
		return "Delay_Resp"
	case MsgAnnounce:
		return "Announce"
	default:
		return fmt.Sprintf("type 0x%X", uint8(t))
	}
}

func parseTimestamp(b []byte) (Timestamp, error) {
	secs := uint64(binary.BigEndian.Uint16(b[0:2]))<<32 | uint64(binary.BigEndian.Uint32(b[2:6]))
	nanos := binary.BigEndian.Uint32(b[6:10])
	if uint64(nanos) >= nanosPerSecond {
		return Timestamp{}, fmt.Errorf("%w: %d ns in one second", ErrTimestamp, nanos)
	}
	return Timestamp{Seconds: secs, Nanos: nanos}, nil
}

// MarshalBinary encodes m for the wire. A message that came from Parse is
// reproduced byte for byte: the bytes Parse kept in Raw are the template, and
// only the fields decoded into Header and the body are written over them.
func (m *Message) MarshalBinary() ([]byte, error) {
	need, known := bodyLen(m.Header.Type)
	if !known {
		return nil, fmt.Errorf("%w: 0x%X", ErrType, uint8(m.Header.Type))
	}
	if m.Header.Version != versionPTP {
		return nil, fmt.Errorf("%w: %d", ErrVersion, m.Header.Version)
	}
	scaled := m.Header.CorrectionNanos * correctionScale
	if scaled/correctionScale != m.Header.CorrectionNanos {
		return nil, fmt.Errorf("%w: %d ns", ErrCorrection, m.Header.CorrectionNanos)
	}

	n := headerLen + need
	if len(m.Raw) > n {
		n = len(m.Raw)
	}
	if n > math.MaxUint16 {
		return nil, fmt.Errorf("%w: message of %d bytes exceeds the messageLength field", ErrLength, n)
	}
	out := make([]byte, n)
	copy(out, m.Raw)

	out[0] = m.transportSpecific<<4 | uint8(m.Header.Type)&0x0F
	out[1] = out[1]&0xF0 | versionPTP
	binary.BigEndian.PutUint16(out[2:4], uint16(n))
	out[4] = m.Header.Domain
	binary.BigEndian.PutUint16(out[6:8], m.Header.Flags)
	binary.BigEndian.PutUint64(out[8:16], uint64(scaled))
	copy(out[20:28], m.Header.SourceClockID[:])
	binary.BigEndian.PutUint16(out[28:30], m.Header.SourcePort)
	binary.BigEndian.PutUint16(out[30:32], m.Header.SequenceID)
	out[32] = m.Header.Control
	out[33] = byte(m.Header.LogInterval)

	body := out[headerLen:]
	switch m.Header.Type {
	case MsgSync, MsgDelayReq, MsgFollowUp:
		if err := putTimestamp(body[0:10], m.OriginTimestamp); err != nil {
			return nil, err
		}
	case MsgDelayResp:
		if err := putTimestamp(body[0:10], m.ReceiveTimestamp); err != nil {
			return nil, err
		}
		copy(body[10:18], m.RequestingClockID[:])
		binary.BigEndian.PutUint16(body[18:20], m.RequestingPort)
	case MsgAnnounce:
		if err := putTimestamp(body[0:10], m.OriginTimestamp); err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint16(body[10:12], uint16(m.UTCOffset))
		copy(body[19:27], m.GrandmasterID[:])
	}
	return out, nil
}

func putTimestamp(dst []byte, ts Timestamp) error {
	if ts.Seconds >= maxSeconds48 || uint64(ts.Nanos) >= nanosPerSecond {
		return fmt.Errorf("%w: %d s %d ns", ErrTimestamp, ts.Seconds, ts.Nanos)
	}
	binary.BigEndian.PutUint16(dst[0:2], uint16(ts.Seconds>>32))
	binary.BigEndian.PutUint32(dst[2:6], uint32(ts.Seconds))
	binary.BigEndian.PutUint32(dst[6:10], ts.Nanos)
	return nil
}

// NewDelayReq builds the Delay_Req a slave sends on the event port to time the
// path to the grandmaster. With the Apple profile the transportSpecific nibble
// is 1, the control field is Delay_Req, and the logMessageInterval is 0x7F
// because the field is not meaningful in a Delay_Req (IEEE 1588-2008 13.4).
// origin now is typically zero: the E2E delay mechanism derives t1 from the
// receiveTimestamp of the Delay_Resp, not from the requester's send time.
func NewDelayReq(sourceClockID [8]byte, sourcePort, sequence uint16, domain uint8, origin Timestamp) *Message {
	return &Message{
		Header: Header{
			Type:            MsgDelayReq,
			Version:         versionPTP,
			Domain:          domain,
			CorrectionNanos: 0,
			SourceClockID:   sourceClockID,
			SourcePort:      sourcePort,
			SequenceID:      sequence,
			Control:         controlDelayReq,
			LogInterval:     logIntervalDelayReq,
		},
		OriginTimestamp:   origin,
		HasOrigin:         true,
		transportSpecific: transportSpecificApple,
	}
}
