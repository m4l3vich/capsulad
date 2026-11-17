// Package mqtt implements the subset of MQTT 3.1.1 (OASIS, 2014) that a
// Capsulad app needs to bridge local hardware to Home Assistant: CONNECT with
// a last will, PUBLISH (QoS 0 and 1), SUBSCRIBE, PINGREQ/PINGRESP and
// DISCONNECT. It is a protocol implementation over an abstract byte stream, not
// a transport: the stream comes from the shared native SDK
// (capsulad.OpenTCP), so the app never links a networking stack of its own.
//
// Only the client role is implemented, and only the packets this bridge emits
// or consumes are handled. Malformed input is rejected with an explicit error;
// nothing is guessed or truncated silently.
package mqtt

import (
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Packet types (MQTT 3.1.1 section 2.2.1).
const (
	packetTypeConnect     byte = 1
	packetTypeConnack     byte = 2
	packetTypePublish     byte = 3
	packetTypePuback      byte = 4
	packetTypeSubscribe   byte = 8
	packetTypeSuback      byte = 9
	packetTypeUnsubscribe byte = 10
	packetTypeUnsuback    byte = 11
	packetTypePingreq     byte = 12
	packetTypePingresp    byte = 13
	packetTypeDisconnect  byte = 14
)

// Connect return codes (MQTT 3.1.1 section 3.2.2.3).
const (
	connackAccepted           byte = 0
	connackBadProtocolVersion byte = 1
	connackIdentifierRejected byte = 2
	connackServerUnavailable  byte = 3
	connackBadCredentials     byte = 4
	connackNotAuthorized      byte = 5
)

// Suback return codes.
const (
	subackFailure byte = 0x80
)

const (
	// maxRemainingLength is the largest MQTT remaining length the variable
	// length field can express (268 435 455). It also bounds every packet this
	// client will read into memory.
	maxRemainingLength = 268435455
	// maxPacketSize bounds one inbound packet. Broker payloads this bridge
	// consumes are command payloads and retained state; anything larger is a
	// protocol or misconfiguration error, not a value to buffer.
	maxPacketSize = 1 << 20
	// protocolName is the MQTT 3.1.1 protocol name and level.
	protocolName  = "MQTT"
	protocolLevel = 4
)

var (
	// ErrProtocol reports a malformed or unsupported packet.
	ErrProtocol = errors.New("mqtt: protocol error")
	// ErrPacketTooLarge reports a packet above maxPacketSize.
	ErrPacketTooLarge = errors.New("mqtt: packet too large")
	// ErrSessionEnded reports use of a client whose session already finished.
	ErrSessionEnded = errors.New("mqtt: session ended")
	// ErrDisconnected reports a session the client ended itself with
	// Disconnect; it is the clean shutdown path, not a failure.
	ErrDisconnected = errors.New("mqtt: disconnected by client")
)

// encodeRemainingLength writes the variable length integer used by every
// packet after its fixed header byte.
func encodeRemainingLength(n int) ([]byte, error) {
	if n < 0 || n > maxRemainingLength {
		return nil, fmt.Errorf("%w: remaining length %d out of range", ErrProtocol, n)
	}
	out := make([]byte, 0, 4)
	for {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		out = append(out, digit)
		if n == 0 {
			return out, nil
		}
	}
}

// readRemainingLength decodes the variable length integer after a fixed header
// byte, returning the value and the number of bytes consumed.
func readRemainingLength(r io.Reader) (int, int, error) {
	value, multiplier := 0, 1
	for i := range 4 {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, i, err
		}
		value += int(b[0]&0x7f) * multiplier
		if b[0]&0x80 == 0 {
			return value, i + 1, nil
		}
		multiplier *= 128
	}
	return 0, 4, fmt.Errorf("%w: malformed remaining length", ErrProtocol)
}

// appendString appends a UTF-8 length-prefixed field.
func appendString(dst []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("%w: field is not valid UTF-8", ErrProtocol)
	}
	if len(s) > 65535 {
		return nil, fmt.Errorf("%w: field is longer than 65535 bytes", ErrProtocol)
	}
	dst = append(dst, byte(len(s)>>8), byte(len(s)))
	return append(dst, s...), nil
}

// readString reads a UTF-8 length-prefixed field from a fully buffered packet.
func readString(payload []byte) (string, []byte, error) {
	if len(payload) < 2 {
		return "", nil, fmt.Errorf("%w: truncated string field", ErrProtocol)
	}
	n := int(payload[0])<<8 | int(payload[1])
	if len(payload) < 2+n {
		return "", nil, fmt.Errorf("%w: truncated string field", ErrProtocol)
	}
	s := string(payload[2 : 2+n])
	if !utf8.ValidString(s) {
		return "", nil, fmt.Errorf("%w: field is not valid UTF-8", ErrProtocol)
	}
	return s, payload[2+n:], nil
}

// connectOptions configures the CONNECT packet.
type connectOptions struct {
	clientID     string
	cleanSession bool
	keepAlive    uint16
	username     string
	hasUsername  bool
	password     string
	hasPassword  bool
	will         *Message
}

// encodeConnect builds a CONNECT packet.
func encodeConnect(o connectOptions) ([]byte, error) {
	var flags byte
	if o.cleanSession {
		flags |= 0x02
	}
	if o.will != nil {
		flags |= 0x04
		flags |= (o.will.QoS & 0x03) << 3
		if o.will.Retain {
			flags |= 0x20
		}
	}
	if o.hasUsername {
		flags |= 0x80
	}
	if o.hasPassword {
		flags |= 0x40
	}

	body, err := appendString(nil, protocolName)
	if err != nil {
		return nil, err
	}
	body = append(body, protocolLevel, flags, byte(o.keepAlive>>8), byte(o.keepAlive))
	if body, err = appendString(body, o.clientID); err != nil {
		return nil, err
	}
	if o.will != nil {
		if o.will.QoS > 2 {
			return nil, fmt.Errorf("%w: will QoS %d", ErrProtocol, o.will.QoS)
		}
		if body, err = appendString(body, o.will.Topic); err != nil {
			return nil, err
		}
		if body, err = appendBytes(body, o.will.Payload); err != nil {
			return nil, err
		}
	}
	if o.hasUsername {
		if body, err = appendString(body, o.username); err != nil {
			return nil, err
		}
	}
	if o.hasPassword {
		if body, err = appendBytes(body, []byte(o.password)); err != nil {
			return nil, err
		}
	}
	return frame(packetTypeConnect, 0, body)
}

// appendBytes appends a binary length-prefixed field.
func appendBytes(dst, data []byte) ([]byte, error) {
	if len(data) > 65535 {
		return nil, fmt.Errorf("%w: binary field is longer than 65535 bytes", ErrProtocol)
	}
	dst = append(dst, byte(len(data)>>8), byte(len(data)))
	return append(dst, data...), nil
}

// frame builds a packet: fixed header byte, remaining length, body.
func frame(packetType, flags byte, body []byte) ([]byte, error) {
	length, err := encodeRemainingLength(len(body))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(length)+len(body))
	out = append(out, packetType<<4|flags&0x0f)
	out = append(out, length...)
	return append(out, body...), nil
}

// encodePublish builds a PUBLISH packet. A QoS 1 packet carries its packet id.
func encodePublish(msg Message, packetID uint16) ([]byte, error) {
	if msg.Topic == "" {
		return nil, fmt.Errorf("%w: publish topic is empty", ErrProtocol)
	}
	if msg.QoS > 2 {
		return nil, fmt.Errorf("%w: publish QoS %d", ErrProtocol, msg.QoS)
	}
	var flags byte
	flags |= (msg.QoS & 0x03) << 1
	if msg.Retain {
		flags |= 0x01
	}
	body, err := appendString(nil, msg.Topic)
	if err != nil {
		return nil, err
	}
	if msg.QoS > 0 {
		if packetID == 0 {
			return nil, fmt.Errorf("%w: QoS %d publish needs a packet id", ErrProtocol, msg.QoS)
		}
		body = append(body, byte(packetID>>8), byte(packetID))
	}
	body = append(body, msg.Payload...)
	return frame(packetTypePublish, flags, body)
}

// encodePacketID builds a two-byte-body packet that carries only a packet id
// (PUBACK, UNSUBACK acknowledgement and friends).
func encodePacketID(packetType byte, packetID uint16) ([]byte, error) {
	if packetID == 0 {
		return nil, fmt.Errorf("%w: packet type %d needs a non-zero packet id", ErrProtocol, packetType)
	}
	return frame(packetType, 0, []byte{byte(packetID >> 8), byte(packetID)})
}

// encodeSubscribe builds a SUBSCRIBE packet for filters at QoS 0.
func encodeSubscribe(packetID uint16, filters []string) ([]byte, error) {
	if len(filters) == 0 {
		return nil, fmt.Errorf("%w: subscribe needs at least one topic filter", ErrProtocol)
	}
	if packetID == 0 {
		return nil, fmt.Errorf("%w: SUBSCRIBE needs a non-zero packet id", ErrProtocol)
	}
	body := []byte{byte(packetID >> 8), byte(packetID)}
	var err error
	for _, filter := range filters {
		if filter == "" {
			return nil, fmt.Errorf("%w: subscribe topic filter is empty", ErrProtocol)
		}
		if body, err = appendString(body, filter); err != nil {
			return nil, err
		}
		body = append(body, 0) // requested QoS 0
	}
	return frame(packetTypeSubscribe, 0x02, body)
}

// packet is one decoded packet (not a CONNECT, which only the client sends).
type packet struct {
	packetType byte
	flags      byte
	// topic and payload describe a PUBLISH packet.
	topic    string
	payload  []byte
	qos      byte
	retain   bool
	packetID uint16
	// returnCode is the CONNACK return code or a SUBACK code.
	returnCode byte
	// subackCodes holds every return code of a SUBACK.
	subackCodes []byte
	// sessionPresent is the CONNACK session-present flag.
	sessionPresent bool
}

// readPacket reads exactly one packet from a MQTT stream.
func readPacket(r io.Reader) (*packet, error) {
	var header [1]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	packetType := header[0] >> 4
	flags := header[0] & 0x0f
	remaining, _, err := readRemainingLength(r)
	if err != nil {
		return nil, err
	}
	if remaining > maxPacketSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrPacketTooLarge, remaining)
	}
	body := make([]byte, remaining)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: truncated %s packet", ErrProtocol, packetName(packetType))
		}
		return nil, err
	}

	out := &packet{packetType: packetType, flags: flags}
	switch packetType {
	case packetTypeConnack:
		if len(body) != 2 {
			return nil, fmt.Errorf("%w: CONNACK is %d bytes", ErrProtocol, len(body))
		}
		out.sessionPresent = body[0]&0x01 != 0
		out.returnCode = body[1]
	case packetTypePublish:
		out.qos = (flags >> 1) & 0x03
		out.retain = flags&0x01 != 0
		if out.qos > 2 {
			return nil, fmt.Errorf("%w: publish QoS %d", ErrProtocol, out.qos)
		}
		topic, rest, err := readString(body)
		if err != nil {
			return nil, err
		}
		if out.qos > 0 {
			if len(rest) < 2 {
				return nil, fmt.Errorf("%w: publish has no packet id", ErrProtocol)
			}
			out.packetID = uint16(rest[0])<<8 | uint16(rest[1])
			rest = rest[2:]
			if out.packetID == 0 {
				return nil, fmt.Errorf("%w: publish packet id 0", ErrProtocol)
			}
		}
		out.topic = topic
		out.payload = rest
	case packetTypePuback, packetTypeUnsuback:
		if len(body) != 2 {
			return nil, fmt.Errorf("%w: %s is %d bytes", ErrProtocol, packetName(packetType), len(body))
		}
		out.packetID = uint16(body[0])<<8 | uint16(body[1])
	case packetTypeSuback:
		if len(body) < 3 {
			return nil, fmt.Errorf("%w: SUBACK is %d bytes", ErrProtocol, len(body))
		}
		out.packetID = uint16(body[0])<<8 | uint16(body[1])
		out.subackCodes = body[2:]
	case packetTypePingresp, packetTypePingreq:
		if len(body) != 0 {
			return nil, fmt.Errorf("%w: %s carries a body", ErrProtocol, packetName(packetType))
		}
	default:
		return nil, fmt.Errorf("%w: unexpected packet type %d", ErrProtocol, packetType)
	}
	return out, nil
}

func packetName(packetType byte) string {
	switch packetType {
	case packetTypeConnect:
		return "CONNECT"
	case packetTypeConnack:
		return "CONNACK"
	case packetTypePublish:
		return "PUBLISH"
	case packetTypePuback:
		return "PUBACK"
	case packetTypeSubscribe:
		return "SUBSCRIBE"
	case packetTypeSuback:
		return "SUBACK"
	case packetTypeUnsubscribe:
		return "UNSUBSCRIBE"
	case packetTypeUnsuback:
		return "UNSUBACK"
	case packetTypePingreq:
		return "PINGREQ"
	case packetTypePingresp:
		return "PINGRESP"
	case packetTypeDisconnect:
		return "DISCONNECT"
	default:
		return fmt.Sprintf("packet(%d)", packetType)
	}
}

// connackError renders a CONNACK return code as an error, or nil when the
// broker accepted the connection.
func connackError(code byte) error {
	switch code {
	case connackAccepted:
		return nil
	case connackBadProtocolVersion:
		return fmt.Errorf("%w: broker rejected protocol level %d", ErrProtocol, protocolLevel)
	case connackIdentifierRejected:
		return fmt.Errorf("%w: broker rejected the client id", ErrProtocol)
	case connackServerUnavailable:
		return fmt.Errorf("%w: broker is unavailable", ErrProtocol)
	case connackBadCredentials:
		return fmt.Errorf("%w: broker rejected the credentials", ErrProtocol)
	case connackNotAuthorized:
		return fmt.Errorf("%w: broker refused authorization", ErrProtocol)
	default:
		return fmt.Errorf("%w: unknown CONNACK code %d", ErrProtocol, code)
	}
}
