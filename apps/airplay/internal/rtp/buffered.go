package rtp

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// ErrBufferedAuthentication reports a complete buffered-audio record whose
// ChaCha20-Poly1305 authentication failed. The record has been consumed and is
// safe to discard; the caller may read the next record from the stream.
var ErrBufferedAuthentication = errors.New("rtp: buffered payload authentication failed")

// bufferedPacketBufferSize is the reference receiver's packet scratch size.
// Its strict data_len < buffer_packet_size check makes the largest accepted
// body one byte smaller than this capacity.
const bufferedPacketBufferSize = 16 * 1024

// BufferedPacket is one authenticated AirPlay 2 buffered-audio record. Payload
// is the decrypted codec frame, not PCM. It aliases decoder-owned storage and
// remains valid only until the next call to BufferedDecoder.Read.
type BufferedPacket struct {
	Sequence uint32 // low 23 bits of the record's first four bytes
	Timestamp uint32
	SSRC      uint32
	Payload   []byte
}

// BufferedDecoder reads and decrypts the TCP framing used by AirPlay 2
// buffered audio. Each record has a two-byte big-endian length which includes
// the length field itself, followed by a 12-byte header, ciphertext and a
// 16-byte authentication tag, and an eight-byte nonce. The header contains a
// 23-bit sequence number in the low bits of its first word, followed by the
// timestamp and SSRC. Header bytes 4..12 are the additional authenticated
// data; the trailing nonce is front-padded with four zero bytes for the IETF
// ChaCha20-Poly1305 construction.
//
// The AEAD, record scratch and nonce are reused. Payload is decrypted in place
// after the complete record has arrived, without a per-packet allocation or an
// additional payload copy. A BufferedDecoder belongs to one stream and one
// goroutine. Its zero value is not usable; call NewBufferedDecoder.
type BufferedDecoder struct {
	aead     cipher.AEAD
	nonce    [chacha20poly1305.NonceSize]byte
	record   [bufferedPacketBufferSize]byte
	terminal error
}

// NewBufferedDecoder returns a decoder for the 32-byte ChaCha20-Poly1305
// shared key of one AirPlay 2 buffered-audio stream.
func NewBufferedDecoder(key []byte) (*BufferedDecoder, error) {
	if len(key) != chachaKeySize {
		return nil, fmt.Errorf("rtp: unexpected ChaCha20-Poly1305 key length %d", len(key))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return &BufferedDecoder{aead: aead}, nil
}

// Read consumes exactly one buffered-audio record from r. Header fields are
// returned only after authentication succeeds. Every record must carry the
// encrypted minimum body and authenticate; SSRC 0 (SSRC_NONE) is not a
// length-only or cleartext exemption. The decoder keeps no per-stream SSRC or
// timestamp state, so no record can mutate stream state here. A complete
// unauthentic record returns a zero BufferedPacket and
// ErrBufferedAuthentication; because its framing was complete, the next Read
// starts at the following record.
//
// Malformed lengths, truncated records and other I/O errors are terminal:
// Read returns no packet, latches the error, and subsequent calls return it
// again. In particular, there is no attempt to resynchronize or to interpret a
// record as unencrypted RTP. EOF before any byte of a new length prefix is a
// clean end of stream; EOF after a prefix announces a body is io.ErrUnexpectedEOF.
func (d *BufferedDecoder) Read(r io.Reader) (BufferedPacket, error) {
	if d.terminal != nil {
		return BufferedPacket{}, d.terminal
	}

	prefix := d.record[:2]
	if _, err := io.ReadFull(r, prefix); err != nil {
		return d.endBufferedRead(err)
	}
	bodyLen := int(binary.BigEndian.Uint16(prefix)) - len(prefix)
	if bodyLen < MinPacketSize || bodyLen >= len(d.record) {
		return d.endBufferedRead(fmt.Errorf("rtp: invalid buffered record length %d", bodyLen+len(prefix)))
	}

	body := d.record[:bodyLen]
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return d.endBufferedRead(err)
	}

	copy(d.nonce[chacha20poly1305.NonceSize-nonceSize:], body[bodyLen-nonceSize:])
	sealed := body[headerSize : bodyLen-nonceSize]
	payload, err := d.aead.Open(sealed[:0], d.nonce[:], sealed, body[4:headerSize])
	if err != nil {
		return BufferedPacket{}, ErrBufferedAuthentication
	}

	return BufferedPacket{
		Sequence: binary.BigEndian.Uint32(body[0:4]) & 0x7fffff,
		Timestamp: binary.BigEndian.Uint32(body[4:8]),
		SSRC:      binary.BigEndian.Uint32(body[8:12]),
		Payload:   payload,
	}, nil
}

func (d *BufferedDecoder) endBufferedRead(err error) (BufferedPacket, error) {
	d.terminal = err
	return BufferedPacket{}, err
}
