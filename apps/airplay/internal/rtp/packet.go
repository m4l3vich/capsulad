// Package rtp handles the RTP/RTCP layer of AirPlay 2 and RAOP audio streams:
// it parses the audio datagrams, decrypts their payloads (ChaCha20-Poly1305 for
// AirPlay 2, AES-128-CBC for legacy RAOP), describes the redundancy blocks of
// payload type 97 and provides a small reordering buffer with the RTCP reports
// (receiver report, generic NACK, sender report parsing) a receiver needs.
package rtp

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// Payload types used by AirPlay audio streams.
const (
	PayloadALAC      = 96  // realtime ALAC
	PayloadRedundant = 97  // RFC2198 redundancy, realtime
	PayloadBuffered  = 103 // buffered ALAC
)

const (
	headerSize     = 12
	nonceSize      = 8
	chachaKeySize  = 32
	chachaOverhead = 16
	// MinPacketSize is the smallest datagram that can hold an RTP header, an
	// empty ciphertext, an authentication tag and the nonce.
	MinPacketSize = headerSize + chachaOverhead + nonceSize
)

// Errors reported by this package.
var (
	ErrShortPacket = errors.New("rtp: packet too short")
	ErrVersion     = errors.New("rtp: unsupported RTP version")
	ErrAuth        = errors.New("rtp: payload authentication failed")
	ErrRedundancy  = errors.New("rtp: malformed RFC2198 redundancy header")
)

// Packet is one audio datagram. The slices alias the buffer given to Parse;
// copy them if the buffer is reused.
type Packet struct {
	Marker      bool
	PayloadType uint8
	Sequence    uint16
	Timestamp   uint32
	SSRC        uint32
	Payload     []byte // ciphertext including the authentication tag
	Nonce       []byte // the 8 bytes at the very end of the datagram
	Header      []byte // the first 12 bytes, i.e. the RTP header
}

// Parse validates and splits an AirPlay audio datagram. It requires RTP
// version 2 and at least 12+24 bytes, because the payload is followed by a
// 16-byte authentication tag and an 8-byte nonce.
func Parse(b []byte) (*Packet, error) {
	if len(b) < MinPacketSize {
		return nil, ErrShortPacket
	}
	if b[0]>>6 != 2 {
		return nil, ErrVersion
	}
	if b[0]&0x0f != 0 || b[0]&0x10 != 0 {
		// CSRCs and header extensions shift the payload, which AirPlay audio
		// never uses.
		return nil, fmt.Errorf("rtp: unsupported header (cc=%d, extension=%d)", b[0]&0x0f, b[0]>>4&1)
	}
	return &Packet{
		Marker:      b[1]&0x80 != 0,
		PayloadType: b[1] & 0x7f,
		Sequence:    binary.BigEndian.Uint16(b[2:4]),
		Timestamp:   binary.BigEndian.Uint32(b[4:8]),
		SSRC:        binary.BigEndian.Uint32(b[8:12]),
		Payload:     b[headerSize : len(b)-nonceSize],
		Nonce:       b[len(b)-nonceSize:],
		Header:      b[:headerSize],
	}, nil
}

// aad is the additional authenticated data of the AEAD: the RTP timestamp and
// SSRC, exactly as they appear on the wire.
func (p *Packet) aad() []byte { return p.Header[4:12] }

// RedundantBlock describes one RFC2198 redundancy header block.
type RedundantBlock struct {
	// Timestamp is the absolute RTP timestamp the block belongs to, that is
	// the packet timestamp minus the block's timestamp offset.
	Timestamp uint32
	// Length is the length in bytes of the block's data.
	Length int
}

// RedundantBlocks parses the RFC2198 block descriptors of a payload type 97
// packet. It returns nil for other payload types, for packets without
// redundancy and for malformed descriptors.
func (p *Packet) RedundantBlocks() []RedundantBlock {
	if p.PayloadType != PayloadRedundant {
		return nil
	}
	blocks, _, ok := parseRedundancy(p.Payload, p.Timestamp)
	if !ok {
		return nil
	}
	return blocks
}

// parseRedundancy walks the RFC2198 header blocks at the start of a payload.
// It returns the redundant blocks, the length of the whole cleartext header
// area (including the final descriptor of the primary block) and whether the
// descriptors were well formed.
func parseRedundancy(payload []byte, timestamp uint32) ([]RedundantBlock, int, bool) {
	var blocks []RedundantBlock
	off := 0
	for {
		if off >= len(payload) {
			return nil, 0, false
		}
		first := payload[off]
		if first&0x80 == 0 {
			// Last (primary) block: only the payload type byte follows.
			return blocks, off + 1, true
		}
		if off+4 > len(payload) {
			return nil, 0, false
		}
		offset := uint32(binary.BigEndian.Uint16(payload[off+1:off+3])&0x3ffc) >> 2
		length := int(binary.BigEndian.Uint16(payload[off+2:off+4]) & 0x03ff)
		blocks = append(blocks, RedundantBlock{Timestamp: timestamp - offset, Length: length})
		off += 4
	}
}

// DecryptRealtime verifies and decrypts a realtime packet with the 32-byte
// shared key that the sender supplied in SETUP. It expects the AirPlay
// ChaCha20-Poly1305 layout: the ciphertext (followed by its 16-byte tag) sits
// after the RTP header and any RFC2198 block descriptors, the additional data
// is the RTP timestamp and SSRC, and the nonce is the last 8 bytes of the
// datagram. The 8-byte nonce is front-padded with four zero bytes to the
// 12-byte nonce of the IETF construction, which is what senders use.
//
// For payload type 97 the redundancy descriptors are cleartext and are skipped
// before the AEAD is applied; the returned frame is the current audio frame
// only, with the redundant copies of earlier frames removed.
func (p *Packet) DecryptRealtime(key []byte) ([]byte, error) {
	if len(key) != chachaKeySize {
		return nil, fmt.Errorf("rtp: unexpected ChaCha20-Poly1305 key length %d", len(key))
	}
	if len(p.Nonce) != nonceSize {
		return nil, fmt.Errorf("rtp: unexpected nonce length %d", len(p.Nonce))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	copy(nonce[chacha20poly1305.NonceSize-nonceSize:], p.Nonce)

	body := p.Payload
	redundant := 0
	if p.PayloadType == PayloadRedundant {
		blocks, headerLen, ok := parseRedundancy(p.Payload, p.Timestamp)
		if !ok {
			return nil, ErrRedundancy
		}
		for _, b := range blocks {
			redundant += b.Length
		}
		body = p.Payload[headerLen:]
	}
	if len(body) < aead.Overhead() {
		return nil, ErrShortPacket
	}
	plain, err := aead.Open(nil, nonce, body, p.aad())
	if err != nil {
		return nil, ErrAuth
	}
	if redundant > len(plain) {
		return nil, ErrRedundancy
	}
	return plain[redundant:], nil
}

// DecryptLegacyAES decrypts the legacy RAOP layout with the 16-byte AES key
// and 16-byte IV from ANNOUNCE: AES-128-CBC over the 16-byte multiple prefix
// of Payload||Nonce, with the remaining trailing bytes (fewer than 16) copied
// unchanged, exactly as the RAOP sender produced them.
func (p *Packet) DecryptLegacyAES(key, iv []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("rtp: unexpected AES key length %d", len(key))
	}
	if len(iv) != 16 {
		return nil, fmt.Errorf("rtp: unexpected AES IV length %d", len(iv))
	}

	// The ciphertext, the tag and the nonce form one contiguous run in the
	// datagram; RAOP encrypts only its 16-byte multiple prefix.
	data := make([]byte, 0, len(p.Payload)+len(p.Nonce))
	data = append(data, p.Payload...)
	data = append(data, p.Nonce...)
	crypted := len(data) &^ 0x0f
	if crypted == 0 {
		return data, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	copy(out[crypted:], data[crypted:])
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out[:crypted], data[:crypted])
	return out, nil
}
