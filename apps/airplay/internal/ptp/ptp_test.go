package ptp_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"capsulad/apps/airplay/internal/ptp"
)

// The fixtures below are assembled by hand from the layouts of the reference
// implementation, not from this package's encoder, so a mistake in the encoder
// cannot hide a mistake in the decoder. They are anchored on
//
//   - https://raw.githubusercontent.com/mikebrady/nqptp/master/nqptp-ptp-definitions.h
//     struct ptp_common_message_header (34 octets: type|transportSpecific,
//     version, messageLength, domain, reserved, flags, correctionField,
//     messageTypeSpecific, clockIdentity, sourcePortID, sequenceId, control,
//     logMessagePeriod), and struct ptp_sync / ptp_follow_up / ptp_announce /
//     ptp_delay_req / ptp_delay_resp for the bodies;
//   - https://raw.githubusercontent.com/mikebrady/nqptp/master/nqptp-message-handlers.c
//     handle_follow_up, which reads a timestamp as a 16-bit second half, a
//     32-bit second half and then 32 bits of nanoseconds, all big-endian, and
//     scales the correction field by 2^-16 ns.
const (
	// Sync, 44 octets: tp_common_message_header + ptp_sync (ptp_sync_message).
	// Two-step, so the origin timestamp is zero until the Follow_Up arrives.
	syncHex = `
		10 02 00 2C 00 00 02 08 00 00 00 00 00 00 00 00
		00 00 00 00 00 11 22 FF FE 33 44 55 00 01 12 34
		00 FD
		00 00 00 00 00 00 00 00 00 00`

	// Follow_Up, 76 octets: ptp_follow_up_message plus the 32-octet 802.1AS
	// Follow_Up Information TLV that AirPlay senders append after
	// preciseOriginTimestamp (see the TLV walk in handle_follow_up). The
	// timestamp is 1000000000 s, 123456789 ns and the correction field is
	// 0x0000000003E80000 scaled units, i.e. 1000 ns.
	followUpHex = `
		18 02 00 4C 00 00 02 08 00 00 00 00 03 E8 00 00
		00 00 00 00 00 11 22 FF FE 33 44 55 00 01 12 34
		02 FD
		00 00 3B 9A CA 00 07 5B CD 15
		00 03 00 1C 00 80 C2 00 00 01 00 00 00 00 00 00
		00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00`

	// Announce, 64 octets: ptp_announce_message. currentUtcOffset 37 s,
	// grandmasterPriority1 0xF8 and priority2 0xF8 (aPTPpriority1/2), accuracy
	// 0xFE (aPTPaccuracy), logMessagePeriod 0 (aPTPinitialLogAnnounceInterval).
	announceHex = `
		1B 02 00 40 00 00 00 08 00 00 00 00 00 00 00 00
		00 00 00 00 00 11 22 FF FE 33 44 55 00 01 00 01
		05 00
		00 00 3B 9A CA 00 07 5B CD 15
		00 25 00 F8 F8 FE FF FF F8 00 11 22 FF FE 33 44 55 00 00 A0`

	// Delay_Req, 44 octets: ptp_delay_req_message. The origin timestamp is
	// zero, the control field is Delay_Req and logMessagePeriod is 0x7F.
	delayReqHex = `
		11 02 00 2C 00 00 00 00 00 00 00 00 00 00 00 00
		00 00 00 00 00 11 22 FF FE 33 44 55 00 01 00 07
		01 7F
		00 00 00 00 00 00 00 00 00 00`

	// Delay_Resp, 54 octets: ptp_delay_resp_message, receiveTimestamp plus the
	// requestingPortIdentity of the Delay_Req it answers.
	delayRespHex = `
		19 02 00 36 00 00 00 00 00 00 00 00 00 00 00 00
		00 00 00 00 00 11 22 FF FE 33 44 55 00 01 00 07
		03 7F
		00 00 3B 9A CA 01 07 5B CD 15
		00 22 33 FF FE 44 55 66 00 01`
)

// mustHex decodes a fixture, ignoring the whitespace that keeps the octets
// aligned with the header fields.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return b
}

// check compares two comparable values.
func check(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

func TestParseSync(t *testing.T) {
	b := mustHex(t, syncHex)
	if len(b) != 44 {
		t.Fatalf("fixture is %d octets, want 44", len(b))
	}
	m, err := ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	check(t, "Type", m.Header.Type, ptp.MsgSync)
	check(t, "Version", m.Header.Version, uint8(2))
	check(t, "Domain", m.Header.Domain, uint8(0))
	check(t, "Flags", m.Header.Flags, uint16(0x0208))
	check(t, "CorrectionNanos", m.Header.CorrectionNanos, int64(0))
	check(t, "SourceClockID", m.Header.SourceClockID, [8]byte{0x00, 0x11, 0x22, 0xFF, 0xFE, 0x33, 0x44, 0x55})
	check(t, "SourcePort", m.Header.SourcePort, uint16(1))
	check(t, "SequenceID", m.Header.SequenceID, uint16(0x1234))
	check(t, "Control", m.Header.Control, uint8(0))
	check(t, "LogInterval", m.Header.LogInterval, int8(-3))
	check(t, "OriginTimestamp", m.OriginTimestamp, ptp.Timestamp{})
	check(t, "HasOrigin", m.HasOrigin, true)
	check(t, "HasReceive", m.HasReceive, false)
	check(t, "HasGrandmaster", m.HasGrandmaster, false)
	if !bytes.Equal(m.Raw, b) {
		t.Errorf("Raw = %X, want the packet", m.Raw)
	}
}

func TestParseFollowUp(t *testing.T) {
	b := mustHex(t, followUpHex)
	if len(b) != 76 {
		t.Fatalf("fixture is %d octets, want 76", len(b))
	}
	m, err := ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	check(t, "Type", m.Header.Type, ptp.MsgFollowUp)
	check(t, "Version", m.Header.Version, uint8(2))
	check(t, "Flags", m.Header.Flags, uint16(0x0208))
	// 0x0000000003E80000 scaled nanoseconds = 1000 ns.
	check(t, "CorrectionNanos", m.Header.CorrectionNanos, int64(1000))
	check(t, "SourcePort", m.Header.SourcePort, uint16(1))
	check(t, "SequenceID", m.Header.SequenceID, uint16(0x1234))
	check(t, "Control", m.Header.Control, uint8(2))
	check(t, "LogInterval", m.Header.LogInterval, int8(-3))
	check(t, "OriginTimestamp", m.OriginTimestamp, ptp.Timestamp{Seconds: 1_000_000_000, Nanos: 123_456_789})
	check(t, "HasOrigin", m.HasOrigin, true)
}

func TestParseAnnounce(t *testing.T) {
	b := mustHex(t, announceHex)
	if len(b) != 64 {
		t.Fatalf("fixture is %d octets, want 64", len(b))
	}
	m, err := ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	check(t, "Type", m.Header.Type, ptp.MsgAnnounce)
	check(t, "Version", m.Header.Version, uint8(2))
	check(t, "Domain", m.Header.Domain, uint8(0))
	check(t, "Flags", m.Header.Flags, uint16(0x0008))
	check(t, "SourceClockID", m.Header.SourceClockID, [8]byte{0x00, 0x11, 0x22, 0xFF, 0xFE, 0x33, 0x44, 0x55})
	check(t, "SourcePort", m.Header.SourcePort, uint16(1))
	check(t, "SequenceID", m.Header.SequenceID, uint16(1))
	check(t, "Control", m.Header.Control, uint8(5))
	check(t, "LogInterval", m.Header.LogInterval, int8(0))
	check(t, "OriginTimestamp", m.OriginTimestamp, ptp.Timestamp{Seconds: 1_000_000_000, Nanos: 123_456_789})
	// currentUtcOffset sits right after the origin timestamp; the grandmaster
	// identity sits at body offset 19, past priority1/clockQuality/priority2.
	check(t, "UTCOffset", m.UTCOffset, int16(37))
	check(t, "GrandmasterID", m.GrandmasterID, [8]byte{0x00, 0x11, 0x22, 0xFF, 0xFE, 0x33, 0x44, 0x55})
	check(t, "HasGrandmaster", m.HasGrandmaster, true)
}

func TestParseDelayReq(t *testing.T) {
	b := mustHex(t, delayReqHex)
	if len(b) != 44 {
		t.Fatalf("fixture is %d octets, want 44", len(b))
	}
	m, err := ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	check(t, "Type", m.Header.Type, ptp.MsgDelayReq)
	check(t, "Version", m.Header.Version, uint8(2))
	check(t, "Flags", m.Header.Flags, uint16(0))
	check(t, "SequenceID", m.Header.SequenceID, uint16(7))
	check(t, "Control", m.Header.Control, uint8(1))
	check(t, "LogInterval", m.Header.LogInterval, int8(0x7F))
	check(t, "OriginTimestamp", m.OriginTimestamp, ptp.Timestamp{})
	check(t, "HasOrigin", m.HasOrigin, true)
}

func TestParseDelayResp(t *testing.T) {
	b := mustHex(t, delayRespHex)
	if len(b) != 54 {
		t.Fatalf("fixture is %d octets, want 54", len(b))
	}
	m, err := ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	check(t, "Type", m.Header.Type, ptp.MsgDelayResp)
	check(t, "SequenceID", m.Header.SequenceID, uint16(7))
	check(t, "Control", m.Header.Control, uint8(3))
	check(t, "ReceiveTimestamp", m.ReceiveTimestamp, ptp.Timestamp{Seconds: 1_000_000_001, Nanos: 123_456_789})
	check(t, "HasReceive", m.HasReceive, true)
	check(t, "RequestingClockID", m.RequestingClockID, [8]byte{0x00, 0x22, 0x33, 0xFF, 0xFE, 0x44, 0x55, 0x66})
	check(t, "RequestingPort", m.RequestingPort, uint16(1))
}

// TestMarshalRoundTrip encodes every fixture again and requires the result to be
// byte for byte the packet that was parsed. The Follow_Up and Announce fixtures
// carry octets this package does not model (the Follow_Up Information TLV, the
// announce priorities and quality) so this also pins down that they survive.
func TestMarshalRoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, hex string }{
		{"Sync", syncHex},
		{"FollowUp", followUpHex},
		{"Announce", announceHex},
		{"DelayReq", delayReqHex},
		{"DelayResp", delayRespHex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := mustHex(t, tc.hex)
			m, err := ptp.Parse(b)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			out, err := m.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary: %v", err)
			}
			if !bytes.Equal(out, b) {
				t.Fatalf("round trip differs\n got %X\nwant %X", out, b)
			}
		})
	}
}

func TestMarshalAppliesChanges(t *testing.T) {
	b := mustHex(t, syncHex)
	m, err := ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m.Header.SequenceID = 0x0007
	m.Header.Domain = 3
	m.Header.CorrectionNanos = -1500
	m.OriginTimestamp = ptp.Timestamp{Seconds: 42, Nanos: 7}
	m.HasOrigin = true

	out, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	back, err := ptp.Parse(out)
	if err != nil {
		t.Fatalf("Parse of marshalled message: %v", err)
	}
	check(t, "SequenceID", back.Header.SequenceID, uint16(7))
	check(t, "Domain", back.Header.Domain, uint8(3))
	check(t, "CorrectionNanos", back.Header.CorrectionNanos, int64(-1500))
	check(t, "OriginTimestamp", back.OriginTimestamp, ptp.Timestamp{Seconds: 42, Nanos: 7})
	// Untouched header fields keep their wire values.
	check(t, "Flags", back.Header.Flags, uint16(0x0208))
	check(t, "LogInterval", back.Header.LogInterval, int8(-3))
	check(t, "SourcePort", back.Header.SourcePort, uint16(1))
}

func TestParseRejectsMalformed(t *testing.T) {
	good := mustHex(t, syncHex)

	withBytes := func(b []byte, at int, v ...byte) []byte {
		out := append([]byte(nil), b...)
		copy(out[at:], v)
		return out
	}

	for _, tc := range []struct {
		name string
		buf  []byte
		want error
	}{
		{"empty", nil, ptp.ErrShortPacket},
		{"ten octets", good[:10], ptp.ErrShortPacket},
		{"short header", good[:33], ptp.ErrShortPacket},
		{"truncated body", good[:40], ptp.ErrLength},
		{"header only", good[:34], ptp.ErrLength},
		{"messageLength zero", withBytes(good, 2, 0x00, 0x00), ptp.ErrLength},
		{"messageLength too small for the type", withBytes(good, 2, 0x00, 0x28), ptp.ErrLength},
		{"messageLength past the datagram", withBytes(good, 2, 0xFF, 0xFF), ptp.ErrLength},
		{"version one", withBytes(good, 1, 0x01), ptp.ErrVersion},
		{"version zero", withBytes(good, 1, 0x00), ptp.ErrVersion},
		{"signaling type", withBytes(good, 0, 0x1C), ptp.ErrType},
		{"pdelay type", withBytes(good, 0, 0x12), ptp.ErrType},
		{"nanoseconds out of range", withBytes(mustHex(t, followUpHex), 40, 0x3B, 0x9A, 0xCA, 0x00), ptp.ErrTimestamp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ptp.Parse(tc.buf); !errors.Is(err, tc.want) {
				t.Fatalf("Parse error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestParseAcceptsMinorVersionAndTrailingBytes covers the two cases that must
// not be treated as malformed: a non-zero minorVersion nibble, and octets past
// messageLength (which are not part of the message).
func TestParseAcceptsMinorVersionAndTrailingBytes(t *testing.T) {
	good := mustHex(t, syncHex)

	minor := append([]byte(nil), good...)
	minor[1] = 0x12 // versionPTP 2, minorVersion 1
	if _, err := ptp.Parse(minor); err != nil {
		t.Fatalf("minorVersion 1: %v", err)
	}

	padded := append(append([]byte(nil), good...), 0xDE, 0xAD)
	m, err := ptp.Parse(padded)
	if err != nil {
		t.Fatalf("trailing octets: %v", err)
	}
	if len(m.Raw) != len(good) {
		t.Errorf("Raw is %d octets, want the declared %d", len(m.Raw), len(good))
	}
}

// TestParseNeverPanics feeds truncations of every fixture, which is where a
// decoder that indexes before it checks would panic.
func TestParseNeverPanics(t *testing.T) {
	for _, fixture := range []string{syncHex, followUpHex, announceHex, delayReqHex, delayRespHex} {
		b := mustHex(t, fixture)
		for n := 0; n <= len(b); n++ {
			if _, err := ptp.Parse(b[:n]); err == nil && n != len(b) {
				t.Fatalf("%d-octet prefix was accepted", n)
			}
		}
	}
}

func TestMarshalErrors(t *testing.T) {
	m := ptp.NewDelayReq([8]byte{1}, 1, 1, 0, ptp.Timestamp{})
	if _, err := m.MarshalBinary(); err != nil {
		t.Fatalf("NewDelayReq: %v", err)
	}

	for _, tc := range []struct {
		name string
		m    *ptp.Message
		want error
	}{
		{"unknown type", &ptp.Message{Header: ptp.Header{Type: 0x5, Version: 2}}, ptp.ErrType},
		{"wrong version", &ptp.Message{Header: ptp.Header{Type: ptp.MsgSync}}, ptp.ErrVersion},
		{"seconds past 48 bits", &ptp.Message{
			Header:          ptp.Header{Type: ptp.MsgSync, Version: 2},
			OriginTimestamp: ptp.Timestamp{Seconds: 1 << 48},
		}, ptp.ErrTimestamp},
		{"nanoseconds past one second", &ptp.Message{
			Header:          ptp.Header{Type: ptp.MsgSync, Version: 2},
			OriginTimestamp: ptp.Timestamp{Nanos: 1_000_000_000},
		}, ptp.ErrTimestamp},
		{"correction overflow", &ptp.Message{
			Header: ptp.Header{Type: ptp.MsgSync, Version: 2, CorrectionNanos: 1 << 62},
		}, ptp.ErrCorrection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.m.MarshalBinary(); !errors.Is(err, tc.want) {
				t.Fatalf("MarshalBinary error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewDelayReq(t *testing.T) {
	clockID := [8]byte{0xAA, 0xBB, 0xCC, 0xFF, 0xFE, 0xDD, 0xEE, 0xFF}
	m := ptp.NewDelayReq(clockID, 0x0002, 0x0102, 0, ptp.Timestamp{})

	b, err := m.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(b) != 44 {
		t.Fatalf("Delay_Req is %d octets, want 44", len(b))
	}
	check(t, "byte 0 (transportSpecific 1 | Delay_Req)", b[0], byte(0x11))
	check(t, "byte 1 (versionPTP 2)", b[1], byte(0x02))
	check(t, "messageLength", uint16(b[2])<<8|uint16(b[3]), uint16(44))
	check(t, "domainNumber", b[4], byte(0))
	check(t, "controlField", b[32], byte(1))
	check(t, "logMessageInterval", b[33], byte(0x7F))

	back, err := ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	check(t, "Type", back.Header.Type, ptp.MsgDelayReq)
	check(t, "SourceClockID", back.Header.SourceClockID, clockID)
	check(t, "SourcePort", back.Header.SourcePort, uint16(2))
	check(t, "SequenceID", back.Header.SequenceID, uint16(0x0102))
	check(t, "OriginTimestamp", back.OriginTimestamp, ptp.Timestamp{})

	// A non-zero origin is carried through: some senders stamp Delay_Req.
	origin := ptp.Timestamp{Seconds: 1_000_000_000, Nanos: 999}
	b, err = ptp.NewDelayReq(clockID, 1, 3, 0, origin).MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	back, err = ptp.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	check(t, "OriginTimestamp", back.OriginTimestamp, origin)
}
