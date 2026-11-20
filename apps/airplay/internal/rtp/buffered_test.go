package rtp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"testing"
	"testing/iotest"

	"golang.org/x/crypto/chacha20poly1305"
)

// independentBufferedWireVector was generated independently of this package
// with Python cryptography's ChaCha20Poly1305. Its plaintext is 24 bytes, the
// ciphertext and tag are 40 bytes, and the two-byte length includes itself.
// Keeping the complete record fixed makes offset, nonce and AAD regressions
// visible even if the dynamic sender helper below shares a mistaken assumption
// with the decoder.
const independentBufferedWireVector = "003ef812345601020304a0b0c0d0047b797c8c6e74d303a79837bf4464a178a942c546427da56023973314580230ee0e6fadded85ea0f0f1f2f3f4f5f6f7"

func decodeBufferedTestHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bufferedTestKey() []byte {
	key := make([]byte, chachaKeySize)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func independentBufferedVector(t *testing.T) []byte {
	t.Helper()
	return decodeBufferedTestHex(t, independentBufferedWireVector)
}

func independentBufferedPayload(t *testing.T) []byte {
	t.Helper()
	return decodeBufferedTestHex(t, "00112233445566778899aabbccddeeff1020304050607080")
}

func newBufferedTestDecoder(t *testing.T, key []byte) *BufferedDecoder {
	t.Helper()
	d, err := NewBufferedDecoder(key)
	if err != nil {
		t.Fatalf("NewBufferedDecoder: %v", err)
	}
	return d
}

func assertBufferedTestPacket(t *testing.T, got BufferedPacket, sequence, timestamp, ssrc uint32, payload []byte) {
	t.Helper()
	if got.Sequence != sequence || got.Timestamp != timestamp || got.SSRC != ssrc {
		t.Fatalf("header = sequence %#x timestamp %#x SSRC %#x, want %#x %#x %#x",
			got.Sequence, got.Timestamp, got.SSRC, sequence, timestamp, ssrc)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("payload = %x, want %x", got.Payload, payload)
	}
}

func assertZeroBufferedTestPacket(t *testing.T, got BufferedPacket) {
	t.Helper()
	if got.Sequence != 0 || got.Timestamp != 0 || got.SSRC != 0 || got.Payload != nil {
		t.Fatalf("error returned nonzero packet: %+v", got)
	}
}

// bufferedTestRecord builds dynamic records for framing and corruption tests.
// The fixed vector above, rather than this helper, is the wire-layout oracle.
func bufferedTestRecord(t *testing.T, key []byte, sequenceWord, timestamp, ssrc uint32, nonce, payload []byte) []byte {
	t.Helper()
	if len(nonce) != nonceSize {
		t.Fatalf("test nonce is %d bytes", len(nonce))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	fullNonce := make([]byte, chacha20poly1305.NonceSize)
	copy(fullNonce[chacha20poly1305.NonceSize-nonceSize:], nonce)

	body := make([]byte, headerSize, headerSize+len(payload)+aead.Overhead())
	binary.BigEndian.PutUint32(body[0:4], sequenceWord)
	binary.BigEndian.PutUint32(body[4:8], timestamp)
	binary.BigEndian.PutUint32(body[8:12], ssrc)
	aad := append([]byte(nil), body[4:headerSize]...)
	body = aead.Seal(body, fullNonce, payload, aad)

	recordLen := 2 + len(body) + nonceSize
	if recordLen > 0xffff {
		t.Fatalf("test record is too large: %d", recordLen)
	}
	record := make([]byte, 2, recordLen)
	binary.BigEndian.PutUint16(record, uint16(recordLen))
	record = append(record, body...)
	record = append(record, nonce...)
	return record
}

func TestBufferedDecoderIndependentWireVector(t *testing.T) {
	wire := independentBufferedVector(t)
	r := bytes.NewReader(wire)
	d := newBufferedTestDecoder(t, bufferedTestKey())

	got, err := d.Read(r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	assertBufferedTestPacket(t, got, 0x123456, 0x01020304, 0xa0b0c0d0, independentBufferedPayload(t))
	if _, err := d.Read(r); err != io.EOF {
		t.Fatalf("Read at clean end = %v, want io.EOF", err)
	}
}

func TestBufferedDecoderRejectsInvalidKeyLengths(t *testing.T) {
	for _, size := range []int{0, 1, 16, 31, 33} {
		if _, err := NewBufferedDecoder(make([]byte, size)); err == nil {
			t.Errorf("NewBufferedDecoder accepted a %d-byte key", size)
		}
	}
}

func TestBufferedDecoderFragmentedAndCoalescedFrames(t *testing.T) {
	key := bufferedTestKey()
	nonce := []byte{8, 7, 6, 5, 4, 3, 2, 1}
	firstPayload := []byte("first buffered codec frame")
	secondPayload := []byte("second buffered codec frame, with a different size")
	first := bufferedTestRecord(t, key, 0xff000001, 1000, 0x10203040, nonce, firstPayload)
	second := bufferedTestRecord(t, key, 0x807fffff, 2000, 0x50607080, nonce, secondPayload)
	wire := append(append([]byte(nil), first...), second...)

	for _, test := range []struct {
		name    string
		reader  func() io.Reader
	}{
		{name: "fragmented TCP reads", reader: func() io.Reader {
			return iotest.OneByteReader(bytes.NewReader(wire))
		}},
		{name: "coalesced frames", reader: func() io.Reader {
			return bytes.NewReader(wire)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := newBufferedTestDecoder(t, key)
			r := test.reader()
			first, err := d.Read(r)
			if err != nil {
				t.Fatalf("first Read: %v", err)
			}
			assertBufferedTestPacket(t, first, 1, 1000, 0x10203040, firstPayload)
			firstAddress := &first.Payload[0]

			second, err := d.Read(r)
			if err != nil {
				t.Fatalf("second Read: %v", err)
			}
			assertBufferedTestPacket(t, second, 0x7fffff, 2000, 0x50607080, secondPayload)
			if &second.Payload[0] != firstAddress {
				t.Fatal("decoder allocated or copied a new payload scratch between records")
			}
			if _, err := d.Read(r); err != io.EOF {
				t.Fatalf("Read at clean end = %v, want io.EOF", err)
			}
		})
	}
}

func TestBufferedDecoderAuthenticationFailureIsDiscardable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "ciphertext", mutate: func(b []byte) { b[2+headerSize] ^= 0x80 }},
		{name: "tag", mutate: func(b []byte) { b[len(b)-nonceSize-1] ^= 0x01 }},
		{name: "nonce", mutate: func(b []byte) { b[len(b)-1] ^= 0x01 }},
		{name: "timestamp AAD", mutate: func(b []byte) { b[2+4] ^= 0x01 }},
		{name: "SSRC AAD", mutate: func(b []byte) { b[2+8] ^= 0x01 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := independentBufferedVector(t)
			test.mutate(tampered)
			wire := append(tampered, independentBufferedVector(t)...)
			d := newBufferedTestDecoder(t, bufferedTestKey())
			r := bytes.NewReader(wire)

			got, err := d.Read(r)
			if !errors.Is(err, ErrBufferedAuthentication) {
				t.Fatalf("Read error = %v, want ErrBufferedAuthentication", err)
			}
			assertZeroBufferedTestPacket(t, got)

			// A bad complete record does not poison the next length prefix.
			got, err = d.Read(r)
			if err != nil {
				t.Fatalf("Read following authentication failure: %v", err)
			}
			assertBufferedTestPacket(t, got, 0x123456, 0x01020304, 0xa0b0c0d0, independentBufferedPayload(t))
		})
	}
}

func TestBufferedDecoderRejectsWrongKey(t *testing.T) {
	key := bufferedTestKey()
	key[0] ^= 0xff
	d := newBufferedTestDecoder(t, key)
	got, err := d.Read(bytes.NewReader(independentBufferedVector(t)))
	if !errors.Is(err, ErrBufferedAuthentication) {
		t.Fatalf("Read error = %v, want ErrBufferedAuthentication", err)
	}
	assertZeroBufferedTestPacket(t, got)
}

func TestBufferedDecoderSSRCNoneRequiresAuthentication(t *testing.T) {
	key := bufferedTestKey()
	nonce := []byte{8, 8, 8, 8, 8, 8, 8, 8}
	payload := []byte("SSRC_NONE is still an encrypted buffered record")
	wire := bufferedTestRecord(t, key, 0xff000002, 3000, 0, nonce, payload)
	d := newBufferedTestDecoder(t, key)
	got, err := d.Read(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("Read authenticated SSRC_NONE record: %v", err)
	}
	assertBufferedTestPacket(t, got, 2, 3000, 0, payload)

	wire[len(wire)-nonceSize-1] ^= 0x01
	d = newBufferedTestDecoder(t, key)
	got, err = d.Read(bytes.NewReader(wire))
	if !errors.Is(err, ErrBufferedAuthentication) {
		t.Fatalf("Read unauthentic SSRC_NONE record error = %v, want ErrBufferedAuthentication", err)
	}
	assertZeroBufferedTestPacket(t, got)
}

func TestBufferedDecoderSequenceIsNotAAD(t *testing.T) {
	wire := independentBufferedVector(t)
	sequenceWord := binary.BigEndian.Uint32(wire[2:6]) ^ 1
	binary.BigEndian.PutUint32(wire[2:6], sequenceWord)
	d := newBufferedTestDecoder(t, bufferedTestKey())
	got, err := d.Read(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("Read authenticated record with changed sequence: %v", err)
	}
	assertBufferedTestPacket(t, got, 0x123457, 0x01020304, 0xa0b0c0d0, independentBufferedPayload(t))
}

func TestBufferedDecoderDoesNotAcceptPlaintext(t *testing.T) {
	wire := independentBufferedVector(t)
	// Preserve the structurally valid header but replace ciphertext, tag and
	// nonce with bytes that cannot authenticate as an encrypted record.
	for i := 2 + headerSize; i < len(wire); i++ {
		wire[i] = 0
	}
	d := newBufferedTestDecoder(t, bufferedTestKey())
	got, err := d.Read(bytes.NewReader(wire))
	if !errors.Is(err, ErrBufferedAuthentication) {
		t.Fatalf("Read error = %v, want ErrBufferedAuthentication", err)
	}
	assertZeroBufferedTestPacket(t, got)
}

func TestBufferedDecoderRejectsInvalidRecordLengths(t *testing.T) {
	for _, size := range []int{0, 1, 2, MinPacketSize + 1, bufferedPacketBufferSize + 2, 0xffff} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			prefix := []byte{byte(size >> 8), byte(size)}
			valid := independentBufferedVector(t)
			r := bytes.NewReader(append(prefix, valid...))
			d := newBufferedTestDecoder(t, bufferedTestKey())
			got, err := d.Read(r)
			if err == nil || errors.Is(err, ErrBufferedAuthentication) {
				t.Fatalf("Read length %d error = %v, want terminal framing error", size, err)
			}
			assertZeroBufferedTestPacket(t, got)
			remaining := r.Len()

			_, again := d.Read(r)
			if again != err {
				t.Fatalf("terminal error changed from %v to %v", err, again)
			}
			if r.Len() != remaining {
				t.Fatal("terminal decoder attempted to resynchronize or consume more input")
			}
		})
	}
}

func TestBufferedDecoderAuthenticatedBodyBounds(t *testing.T) {
	key := bufferedTestKey()
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	maximum := make([]byte, bufferedPacketBufferSize-1-MinPacketSize)
	for i := range maximum {
		maximum[i] = byte(i * 29)
	}
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "minimum empty authenticated payload", payload: nil},
		{name: "reference maximum body", payload: maximum},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := bufferedTestRecord(t, key, 0x80000007, 77, 88, nonce, test.payload)
			d := newBufferedTestDecoder(t, key)
			got, err := d.Read(bytes.NewReader(wire))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			assertBufferedTestPacket(t, got, 7, 77, 88, test.payload)
		})
	}
}

func TestBufferedDecoderTruncationIsTerminal(t *testing.T) {
	wire := independentBufferedVector(t)
	for _, test := range []struct {
		name string
		wire []byte
		want error
	}{
		{name: "clean EOF between records", wire: nil, want: io.EOF},
		{name: "partial length", wire: wire[:1], want: io.ErrUnexpectedEOF},
		{name: "missing body", wire: wire[:2], want: io.ErrUnexpectedEOF},
		{name: "partial header", wire: wire[:2+headerSize-1], want: io.ErrUnexpectedEOF},
		{name: "partial tag", wire: wire[:len(wire)-nonceSize-1], want: io.ErrUnexpectedEOF},
		{name: "partial nonce", wire: wire[:len(wire)-1], want: io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := newBufferedTestDecoder(t, bufferedTestKey())
			got, err := d.Read(bytes.NewReader(test.wire))
			if !errors.Is(err, test.want) {
				t.Fatalf("Read error = %v, want %v", err, test.want)
			}
			assertZeroBufferedTestPacket(t, got)
			if errors.Is(err, ErrBufferedAuthentication) {
				t.Fatal("truncation was misreported as discardable authentication failure")
			}
			if _, again := d.Read(bytes.NewReader(independentBufferedVector(t))); again != err {
				t.Fatalf("terminal error changed from %v to %v", err, again)
			}
		})
	}
}

func TestBufferedDecoderIOErrorIsTerminal(t *testing.T) {
	wire := independentBufferedVector(t)
	want := errors.New("buffered TCP read failed")
	r := io.MultiReader(bytes.NewReader(wire[:10]), iotest.ErrReader(want))
	d := newBufferedTestDecoder(t, bufferedTestKey())
	got, err := d.Read(r)
	if err != want {
		t.Fatalf("Read error = %v, want %v", err, want)
	}
	assertZeroBufferedTestPacket(t, got)
	if _, again := d.Read(bytes.NewReader(independentBufferedVector(t))); again != want {
		t.Fatalf("terminal error = %v, want %v", again, want)
	}
}
