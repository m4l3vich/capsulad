// Package rtp handles the RTP/RTCP layer of AirPlay 2 and RAOP audio streams:
// it parses the audio datagrams, decrypts their payloads (ChaCha20-Poly1305 for
// AirPlay 2, AES-128-CBC for legacy RAOP), describes the redundancy blocks of
// payload type 97 and provides a byte-bounded reordering buffer with the RTCP
// reports (receiver report, generic NACK, sender report parsing) a receiver
// needs.
//
// A stream decrypts packet by packet through a RealtimeDecoder or a
// LegacyDecoder so that the plaintext of every packet is appended into one
// caller-owned buffer instead of being allocated per packet.
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
	// MinUnencryptedPacketSize is the smallest datagram an unencrypted stream
	// can carry: the RTP header alone. Such a datagram has neither an
	// authentication tag nor a nonce, so every byte after the header is
	// payload.
	MinUnencryptedPacketSize = headerSize
)

// Errors reported by this package.
var (
	ErrShortPacket = errors.New("rtp: packet too short")
	ErrVersion     = errors.New("rtp: unsupported RTP version")
	ErrAuth        = errors.New("rtp: payload authentication failed")
	ErrRedundancy  = errors.New("rtp: malformed RFC2198 redundancy header")
)

// Packet is one audio datagram. The slices alias the buffer given to Parse or
// ParseUnencrypted; copy them if the buffer is reused.
type Packet struct {
	Marker      bool
	PayloadType uint8
	Sequence    uint16
	Timestamp   uint32
	SSRC        uint32
	Payload     []byte // ciphertext including the authentication tag
	Nonce       []byte // the 8 bytes at the very end of the datagram, nil when unencrypted
	Header      []byte // the first 12 bytes, i.e. the RTP header
}

// Parse validates and splits an encrypted realtime audio datagram. It requires
// RTP version 2 and at least MinPacketSize bytes, because the payload is
// followed by a 16-byte authentication tag and an 8-byte nonce.
//
// Unencrypted streams (legacy RAOP with et=0, or PCM) have no tag and no
// nonce, so their datagrams may be as short as the header; use
// ParseUnencrypted for those.
func Parse(b []byte) (*Packet, error) {
	if len(b) < MinPacketSize {
		return nil, ErrShortPacket
	}
	return parseHeader(b, b[headerSize:len(b)-nonceSize], b[len(b)-nonceSize:])
}

// ParseUnencrypted validates and splits a datagram of an unencrypted stream: an
// RTP header followed by an arbitrary payload, with neither an authentication
// tag nor a nonce. The payload is every byte after the header, so a datagram of
// exactly MinUnencryptedPacketSize bytes carries an empty frame, and Packet.Nonce
// is nil.
func ParseUnencrypted(b []byte) (*Packet, error) {
	if len(b) < MinUnencryptedPacketSize {
		return nil, ErrShortPacket
	}
	return parseHeader(b, b[headerSize:], nil)
}

// parseHeader splits a datagram whose header the caller has validated as
// present, with payload and nonce being the extents it computed.
func parseHeader(b []byte, payload, nonce []byte) (*Packet, error) {
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
		Payload:     payload,
		Nonce:       nonce,
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
//
// The blocks describe the redundant copies only, and they are informational:
// the frame that DecryptRealtime or RealtimeDecoder.DecodeInto returns is
// already the current frame with those copies removed, so the caller must not
// use these lengths to slice the returned frame again.
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

// RealtimeDecoder decrypts the realtime packets of one stream with the 32-byte
// shared key the sender supplied in SETUP. The AirPlay ChaCha20-Poly1305
// layout is: the ciphertext (followed by its 16-byte tag) sits after the RTP
// header and any RFC2198 block descriptors, the additional data is the RTP
// timestamp and SSRC, and the nonce is the last 8 bytes of the datagram. The
// 8-byte nonce is front-padded with four zero bytes to the 12-byte nonce of the
// IETF construction, which is what senders use.
//
// The AEAD and the nonce scratch are built once, so a stream that decrypts
// packet by packet does not allocate: see DecodeInto. The zero value is not
// usable; call NewRealtimeDecoder. A decoder belongs to one stream and one
// goroutine: the nonce scratch is reused, so concurrent calls would race.
type RealtimeDecoder struct {
	aead cipher.AEAD
	// nonce is the 12-byte IETF nonce. Its first four bytes stay zero: only
	// the last eight are overwritten with the datagram's trailing nonce.
	nonce [chacha20poly1305.NonceSize]byte
}

// NewRealtimeDecoder returns a decoder for the 32-byte ChaCha20-Poly1305 shared
// key of one realtime stream.
func NewRealtimeDecoder(key []byte) (*RealtimeDecoder, error) {
	if len(key) != chachaKeySize {
		return nil, fmt.Errorf("rtp: unexpected ChaCha20-Poly1305 key length %d", len(key))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return &RealtimeDecoder{aead: aead}, nil
}

// Decode verifies and decrypts p, returning the current audio frame in a fresh
// slice. Prefer DecodeInto when the caller has a buffer to reuse.
func (d *RealtimeDecoder) Decode(p *Packet) ([]byte, error) {
	return d.DecodeInto(nil, p)
}

// DecodeInto verifies and decrypts p, appending the current audio frame to dst
// and returning the extended slice. dst must not overlap the packet's payload:
// the AEAD writes the plaintext into dst while it reads the ciphertext, so the
// two must be disjoint. The appended frame aliases dst and stays valid until
// the caller reuses it; on failure dst is unchanged and the error is ErrAuth or
// ErrRedundancy.
//
// For payload type 97 the RFC2198 descriptors are cleartext and are consumed
// before the AEAD is applied. The returned slice is the CURRENT frame only: the
// redundant copies of earlier frames that the cleartext prefix accounts for are
// already removed. Callers must not slice the returned frame again.
func (d *RealtimeDecoder) DecodeInto(dst []byte, p *Packet) ([]byte, error) {
	if len(p.Nonce) != nonceSize {
		return nil, fmt.Errorf("rtp: unexpected nonce length %d", len(p.Nonce))
	}
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
	if len(body) < d.aead.Overhead() {
		return nil, ErrShortPacket
	}
	copy(d.nonce[chacha20poly1305.NonceSize-nonceSize:], p.Nonce)
	start := len(dst)
	plain, err := d.aead.Open(dst, d.nonce[:], body, p.aad())
	if err != nil {
		return nil, ErrAuth
	}
	// The decrypted bytes are plain[start:]; drop the prefix that the cleartext
	// descriptors said was redundant.
	if redundant > len(plain)-start {
		return nil, ErrRedundancy
	}
	return plain[start+redundant:], nil
}

// DecryptRealtime verifies and decrypts a realtime packet, returning the
// current audio frame only, in a fresh slice. It is a thin wrapper around
// NewRealtimeDecoder and RealtimeDecoder.Decode; a stream that decrypts per
// packet should hold the decoder instead, because building the AEAD per packet
// allocates.
//
// The returned slice is exactly the frame to play: for payload type 97 the
// redundancy descriptors and the redundant copies of earlier frames have
// already been stripped, so the caller must not slice it again.
func (p *Packet) DecryptRealtime(key []byte) ([]byte, error) {
	return p.DecryptRealtimeInto(nil, key)
}

// DecryptRealtimeInto verifies and decrypts p, appending the current audio
// frame to dst, with a decoder built for this call. dst must not overlap the
// packet's payload. The returned slice is exactly the frame to play, as in
// DecryptRealtime; holding a RealtimeDecoder and calling DecodeInto avoids
// rebuilding the AEAD per packet.
func (p *Packet) DecryptRealtimeInto(dst []byte, key []byte) ([]byte, error) {
	d, err := NewRealtimeDecoder(key)
	if err != nil {
		return nil, err
	}
	return d.DecodeInto(dst, p)
}

// LegacyDecoder decrypts the legacy RAOP packets of one stream with the 16-byte
// AES key and 16-byte IV from ANNOUNCE: AES-128-CBC over the 16-byte multiple
// prefix of Payload||Nonce, with the remaining trailing bytes (fewer than 16)
// copied unchanged, exactly as the RAOP sender produced them. The key schedule
// and the IV are built once; see DecodeInto. The decoder holds no per-packet
// state, so unlike RealtimeDecoder it is safe for concurrent use.
type LegacyDecoder struct {
	block cipher.Block
	iv    [aes.BlockSize]byte
}

// NewLegacyDecoder returns a decoder for the AES-128-CBC key and IV of one
// legacy RAOP stream.
func NewLegacyDecoder(key, iv []byte) (*LegacyDecoder, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("rtp: unexpected AES key length %d", len(key))
	}
	if len(iv) != 16 {
		return nil, fmt.Errorf("rtp: unexpected AES IV length %d", len(iv))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	d := &LegacyDecoder{block: block}
	copy(d.iv[:], iv)
	return d, nil
}

// Decode returns the decrypted packet in a fresh slice. Prefer DecodeInto when
// the caller has a buffer to reuse.
func (d *LegacyDecoder) Decode(p *Packet) ([]byte, error) {
	return d.DecodeInto(nil, p)
}

// DecodeInto decrypts p, appending the packet's payload and trailing nonce to
// dst and returning the extended slice. Every RAOP packet carries its own copy
// of the ciphertext and starts the CBC chain from the ANNOUNCE IV, so the mode
// is rebuilt per packet; the (overwhelmingly larger) plaintext buffer and the
// AES key schedule are not.
func (d *LegacyDecoder) DecodeInto(dst []byte, p *Packet) ([]byte, error) {
	// The ciphertext, the tag and the nonce form one contiguous run in the
	// datagram; RAOP encrypts only its 16-byte multiple prefix.
	start := len(dst)
	dst = append(dst, p.Payload...)
	dst = append(dst, p.Nonce...)
	crypted := (len(dst) - start) &^ 0x0f
	if crypted == 0 {
		return dst, nil
	}
	frame := dst[start : start+crypted]
	// CryptBlocks decrypts in place: its inexact-overlap check allows dst and
	// src to be the same slice (the standard library loops over the blocks
	// backwards for exactly this case).
	cipher.NewCBCDecrypter(d.block, d.iv[:]).CryptBlocks(frame, frame)
	return dst, nil
}

// DecryptLegacyAES decrypts the legacy RAOP layout with the 16-byte AES key and
// 16-byte IV from ANNOUNCE, in a fresh slice. It is a thin wrapper around
// NewLegacyDecoder and LegacyDecoder.Decode.
func (p *Packet) DecryptLegacyAES(key, iv []byte) ([]byte, error) {
	return p.DecryptLegacyAESInto(nil, key, iv)
}

// DecryptLegacyAESInto decrypts p, appending the result to dst, with a decoder
// built for this call. Holding a LegacyDecoder and calling DecodeInto avoids
// rebuilding the AES key schedule per packet.
func (p *Packet) DecryptLegacyAESInto(dst []byte, key, iv []byte) ([]byte, error) {
	d, err := NewLegacyDecoder(key, iv)
	if err != nil {
		return nil, err
	}
	return d.DecodeInto(dst, p)
}
