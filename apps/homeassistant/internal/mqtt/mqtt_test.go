package mqtt

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestRemainingLengthRoundTrip(t *testing.T) {
	cases := []struct {
		value int
		bytes []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xff, 0x7f}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{2097151, []byte{0xff, 0xff, 0x7f}},
		{268435455, []byte{0xff, 0xff, 0xff, 0x7f}},
	}
	for _, tc := range cases {
		encoded, err := encodeRemainingLength(tc.value)
		if err != nil {
			t.Fatalf("encodeRemainingLength(%d): %v", tc.value, err)
		}
		if !bytes.Equal(encoded, tc.bytes) {
			t.Fatalf("encodeRemainingLength(%d) = % x, want % x", tc.value, encoded, tc.bytes)
		}
		decoded, n, err := readRemainingLength(bytes.NewReader(tc.bytes))
		if err != nil {
			t.Fatalf("readRemainingLength(% x): %v", tc.bytes, err)
		}
		if decoded != tc.value || n != len(tc.bytes) {
			t.Fatalf("readRemainingLength(% x) = %d (%d bytes), want %d (%d bytes)",
				tc.bytes, decoded, n, tc.value, len(tc.bytes))
		}
	}
	if _, err := encodeRemainingLength(-1); err == nil {
		t.Fatal("encodeRemainingLength(-1) succeeded")
	}
	if _, err := encodeRemainingLength(maxRemainingLength + 1); err == nil {
		t.Fatal("encodeRemainingLength(over max) succeeded")
	}
	if _, _, err := readRemainingLength(bytes.NewReader([]byte{0x80, 0x80, 0x80, 0x80})); err == nil {
		t.Fatal("readRemainingLength accepted a five-byte length")
	}
}

func TestEncodeConnect(t *testing.T) {
	packet, err := encodeConnect(connectOptions{
		clientID:     "capsulad-test",
		cleanSession: true,
		keepAlive:    30,
		username:     "user",
		hasUsername:  true,
		password:     "pass",
		hasPassword:  true,
		will: &Message{
			Topic:   "capsulad/status",
			Payload: []byte("offline"),
			QoS:     1,
			Retain:  true,
		},
	})
	if err != nil {
		t.Fatalf("encodeConnect: %v", err)
	}
	opts, err := decodeConnectForTest(packet)
	if err != nil {
		t.Fatalf("decode connect: %v", err)
	}
	if opts.clientID != "capsulad-test" || !opts.cleanSession || opts.keepAlive != 30 {
		t.Fatalf("unexpected connect options %+v", opts)
	}
	if !opts.hasUsername || opts.username != "user" || !opts.hasPassword || opts.password != "pass" {
		t.Fatalf("credentials lost: %+v", opts)
	}
	if opts.will == nil {
		t.Fatal("will missing")
	}
	if opts.will.Topic != "capsulad/status" || string(opts.will.Payload) != "offline" ||
		opts.will.QoS != 1 || !opts.will.Retain {
		t.Fatalf("unexpected will %+v", opts.will)
	}
}

func TestEncodeConnectWithoutWillOrCredentials(t *testing.T) {
	packet, err := encodeConnect(connectOptions{clientID: "c", cleanSession: true, keepAlive: 10})
	if err != nil {
		t.Fatalf("encodeConnect: %v", err)
	}
	opts, err := decodeConnectForTest(packet)
	if err != nil {
		t.Fatalf("decode connect: %v", err)
	}
	if opts.will != nil || opts.hasUsername || opts.hasPassword {
		t.Fatalf("unexpected optional fields: %+v", opts)
	}
}

func TestPublishRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		msg      Message
		packetID uint16
	}{
		{"qos0", Message{Topic: "a/b", Payload: []byte("hi")}, 0},
		{"retained", Message{Topic: "a/b", Payload: []byte("hi"), Retain: true}, 0},
		{"qos1", Message{Topic: "a/b", Payload: []byte("bye"), QoS: 1}, 7},
		{"qos1-retained-empty", Message{Topic: "a/b", QoS: 1, Retain: true}, 65535},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodePublish(tc.msg, tc.packetID)
			if err != nil {
				t.Fatalf("encodePublish: %v", err)
			}
			got, err := readPacket(bytes.NewReader(encoded))
			if err != nil {
				t.Fatalf("readPacket: %v", err)
			}
			if got.packetType != packetTypePublish {
				t.Fatalf("type %d, want PUBLISH", got.packetType)
			}
			if got.topic != tc.msg.Topic || got.qos != tc.msg.QoS || got.retain != tc.msg.Retain {
				t.Fatalf("header mismatch: %+v", got)
			}
			if !bytes.Equal(got.payload, tc.msg.Payload) {
				t.Fatalf("payload %q, want %q", got.payload, tc.msg.Payload)
			}
			if got.packetID != tc.packetID {
				t.Fatalf("packet id %d, want %d", got.packetID, tc.packetID)
			}
		})
	}
}

func TestEncodePublishRejectsBadInput(t *testing.T) {
	if _, err := encodePublish(Message{}, 0); err == nil {
		t.Fatal("empty topic accepted")
	}
	if _, err := encodePublish(Message{Topic: "a", QoS: 1}, 0); err == nil {
		t.Fatal("QoS 1 without a packet id accepted")
	}
	if _, err := encodePublish(Message{Topic: "a", QoS: 3}, 1); err == nil {
		t.Fatal("QoS 3 accepted")
	}
}

func TestSubscribeEncoding(t *testing.T) {
	packet, err := encodeSubscribe(9, []string{"a/set", "b/set"})
	if err != nil {
		t.Fatalf("encodeSubscribe: %v", err)
	}
	got, err := readPacket(bytes.NewReader(packet))
	if err == nil {
		t.Fatalf("readPacket decoded a SUBSCRIBE (server-side packet): %+v", got)
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("readPacket error = %v, want ErrProtocol", err)
	}
	if packet[0] != packetTypeSubscribe<<4|0x02 {
		t.Fatalf("fixed header = %#x", packet[0])
	}
	if _, err := encodeSubscribe(0, []string{"a"}); err == nil {
		t.Fatal("packet id 0 accepted")
	}
	if _, err := encodeSubscribe(1, nil); err == nil {
		t.Fatal("empty filter list accepted")
	}
	if _, err := encodeSubscribe(1, []string{""}); err == nil {
		t.Fatal("empty filter accepted")
	}
}

func TestReadPacketRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"oversized":        append([]byte{packetTypePublish << 4}, 0xff, 0xff, 0xff, 0x7f),
		"truncated":        {packetTypePublish << 4, 0x10, 0x00},
		"bad connack":      {packetTypeConnack << 4, 0x01, 0x00},
		"qos3 publish":     {packetTypePublish<<4 | 0x06, 0x05, 0x00, 0x01, 'a', 0x00, 0x01},
		"unknown type":     {0x00, 0x00},
		"body on pingresp": {packetTypePingresp << 4, 0x01, 0x00},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := readPacket(bytes.NewReader(raw)); err == nil {
				t.Fatalf("readPacket accepted % x", raw)
			}
		})
	}
	if _, err := readPacket(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream error = %v, want EOF", err)
	}
}

func TestPacketIDRoundTrip(t *testing.T) {
	packet, err := encodePacketID(packetTypePuback, 4242)
	if err != nil {
		t.Fatalf("encodePacketID: %v", err)
	}
	got, err := readPacket(bytes.NewReader(packet))
	if err != nil {
		t.Fatalf("readPacket: %v", err)
	}
	if got.packetType != packetTypePuback || got.packetID != 4242 {
		t.Fatalf("decoded %+v", got)
	}
	if _, err := encodePacketID(packetTypePuback, 0); err == nil {
		t.Fatal("packet id 0 accepted")
	}
}

func TestConnackError(t *testing.T) {
	if err := connackError(connackAccepted); err != nil {
		t.Fatalf("accepted CONNACK mapped to %v", err)
	}
	for _, code := range []byte{
		connackBadProtocolVersion, connackIdentifierRejected, connackServerUnavailable,
		connackBadCredentials, connackNotAuthorized, 9,
	} {
		err := connackError(code)
		if err == nil {
			t.Fatalf("CONNACK code %d mapped to nil", code)
		}
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("CONNACK code %d error %v, want ErrProtocol", code, err)
		}
	}
}

func TestReadStringRejectsTruncation(t *testing.T) {
	if _, _, err := readString([]byte{0x00}); err == nil {
		t.Fatal("truncated length accepted")
	}
	if _, _, err := readString([]byte{0x00, 0x05, 'a'}); err == nil {
		t.Fatal("truncated value accepted")
	}
	if _, _, err := readString([]byte{0x00, 0x02, 0xff, 0xfe}); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

// decodeConnectForTest parses a CONNECT packet for assertions. The production
// decoder deliberately does not implement the client-only packet.
func decodeConnectForTest(raw []byte) (connectOptions, error) {
	var opts connectOptions
	body := raw
	if len(body) < 2 || body[0]>>4 != packetTypeConnect {
		return opts, errors.New("not a CONNECT packet")
	}
	body = body[1:]
	remaining, n, err := readRemainingLength(bytes.NewReader(body))
	if err != nil {
		return opts, err
	}
	body = body[n:]
	if remaining != len(body) {
		return opts, errors.New("declared length does not match the body")
	}
	var name string
	if name, body, err = readString(body); err != nil {
		return opts, err
	}
	if name != protocolName {
		return opts, errors.New("bad protocol name " + name)
	}
	if len(body) < 3 {
		return opts, errors.New("truncated variable header")
	}
	level, flags := body[0], body[1]
	if level != protocolLevel {
		return opts, errors.New("bad protocol level")
	}
	opts.keepAlive = uint16(body[2])<<8 | uint16(body[3])
	opts.cleanSession = flags&0x02 != 0
	if opts.clientID, body, err = readString(body[4:]); err != nil {
		return opts, err
	}
	if flags&0x04 != 0 {
		will := &Message{QoS: (flags >> 3) & 0x03, Retain: flags&0x20 != 0}
		if will.Topic, body, err = readString(body); err != nil {
			return opts, err
		}
		if will.Payload, body, err = readBinary(body); err != nil {
			return opts, err
		}
		opts.will = will
	}
	if flags&0x80 != 0 {
		if opts.username, body, err = readString(body); err != nil {
			return opts, err
		}
		opts.hasUsername = true
	}
	if flags&0x40 != 0 {
		var password []byte
		if password, body, err = readBinary(body); err != nil {
			return opts, err
		}
		opts.password = string(password)
		opts.hasPassword = true
	}
	if len(body) != 0 {
		return opts, errors.New("trailing bytes in CONNECT")
	}
	return opts, nil
}

func readBinary(payload []byte) ([]byte, []byte, error) {
	if len(payload) < 2 {
		return nil, nil, errors.New("truncated binary field")
	}
	n := int(payload[0])<<8 | int(payload[1])
	if len(payload) < 2+n {
		return nil, nil, errors.New("truncated binary value")
	}
	out := make([]byte, n)
	copy(out, payload[2:2+n])
	return out, payload[2+n:], nil
}

func TestDecodeConnectForTestMatchesEncoder(t *testing.T) {
	// Guard the test helper itself: a helper that accepted anything would make
	// TestEncodeConnect meaningless.
	raw, err := encodeConnect(connectOptions{clientID: "x", keepAlive: 1})
	if err != nil {
		t.Fatalf("encodeConnect: %v", err)
	}
	opts, err := decodeConnectForTest(raw)
	if err != nil {
		t.Fatalf("decodeConnectForTest: %v", err)
	}
	if !reflect.DeepEqual(opts, connectOptions{clientID: "x", keepAlive: 1}) {
		t.Fatalf("decoded %+v", opts)
	}
	if _, err := decodeConnectForTest(raw[:len(raw)-1]); err == nil {
		t.Fatal("truncated CONNECT accepted")
	}
}
