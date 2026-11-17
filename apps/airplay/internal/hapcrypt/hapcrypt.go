// Package hapcrypt implements the AirPlay 2 / HAP encrypted-channel transport
// framing and the HKDF-derived channel keys.
//
// The framing and the nonce convention are taken from shairport-sync
// pair_ap/pair_homekit.c (encrypt_chacha/decrypt_chacha) and the
// salt/info table at pair_ap/pair_homekit.c:95-125.
package hapcrypt

import (
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// maxOutbound is the maximum plaintext size of a single outbound block.
const maxOutbound = 1024

var errBadTag = errors.New("hapcrypt: authentication tag mismatch")

func derive(sharedSecret []byte, salt, info string) []byte {
	r := hkdf.New(sha512.New, sharedSecret, []byte(salt), []byte(info))
	out := make([]byte, 32)
	if _, err := io.ReadFull(r, out); err != nil {
		panic("hapcrypt: hkdf: " + err.Error())
	}
	return out
}

// BlockKeys is one channel's key pair, named for the accessory's direction.
//
// Outbound seals what the accessory writes; Inbound opens what it reads. The
// control and event channels are deliberately asymmetric (upstream's comment:
// the event channel is "opposite, probably because it is a reverse
// connection"):
//
//	accessory control: outbound Control-Read-Encryption-Key, inbound Control-Write-Encryption-Key
//	accessory events:  outbound Events-Write-Encryption-Key,  inbound Events-Read-Encryption-Key
//
// (Shairport Sync `pair_homekit.c:2943-2975` cases 3/4; independently pyatv
// `protocols/airplay/server_auth.py:298-308`.) The controller mirrors each
// channel; swapping a direction makes every encrypted reply fail its tag.
type BlockKeys struct {
	Outbound []byte
	Inbound  []byte
}

// ControlChannelKeys derives the accessory's control-channel keys. The shared
// secret is the SRP session key after a transient pair-setup, or the X25519
// secret after pair-verify.
func ControlChannelKeys(sharedSecret []byte) BlockKeys {
	return BlockKeys{
		Outbound: derive(sharedSecret, "Control-Salt", "Control-Read-Encryption-Key"),
		Inbound:  derive(sharedSecret, "Control-Salt", "Control-Write-Encryption-Key"),
	}
}

// EventChannelKeys derives the accessory's event-channel keys. Note the
// opposite direction mapping from the control channel.
func EventChannelKeys(sharedSecret []byte) BlockKeys {
	return BlockKeys{
		Outbound: derive(sharedSecret, "Events-Salt", "Events-Write-Encryption-Key"),
		Inbound:  derive(sharedSecret, "Events-Salt", "Events-Read-Encryption-Key"),
	}
}

// Session wraps an io.ReadWriter with HAP block framing.
type Session struct {
	rw           io.ReadWriter
	writeAEAD    cipher.AEAD
	readAEAD     cipher.AEAD
	writeCounter uint64
	readCounter  uint64
	readBuf      []byte
}

// NewSession creates an encrypted channel. writeKey seals outbound blocks,
// readKey opens inbound blocks. Both keys must be 32 bytes; they are copied.
func NewSession(rw io.ReadWriter, writeKey, readKey []byte) (*Session, error) {
	if len(writeKey) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("hapcrypt: write key must be %d bytes, got %d", chacha20poly1305.KeySize, len(writeKey))
	}
	if len(readKey) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("hapcrypt: read key must be %d bytes, got %d", chacha20poly1305.KeySize, len(readKey))
	}
	wk := append([]byte(nil), writeKey...)
	rk := append([]byte(nil), readKey...)
	wa, err := chacha20poly1305.New(wk)
	if err != nil {
		return nil, err
	}
	ra, err := chacha20poly1305.New(rk)
	if err != nil {
		return nil, err
	}
	return &Session{rw: rw, writeAEAD: wa, readAEAD: ra}, nil
}

func nonce(counter uint64) []byte {
	var n [chacha20poly1305.NonceSize]byte
	binary.LittleEndian.PutUint64(n[4:], counter)
	return n[:]
}

// ReadOne reads and decrypts exactly one inbound block.
func (s *Session) ReadOne() ([]byte, error) {
	var lb [2]byte
	n, err := io.ReadFull(s.rw, lb[:])
	if err != nil {
		if err == io.EOF && n == 0 {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("hapcrypt: reading block length: %w", io.ErrUnexpectedEOF)
	}
	length := int(binary.LittleEndian.Uint16(lb[:]))
	body := make([]byte, length+chacha20poly1305.Overhead)
	if _, err := io.ReadFull(s.rw, body); err != nil {
		return nil, fmt.Errorf("hapcrypt: reading block body: %w", io.ErrUnexpectedEOF)
	}
	plain, err := s.readAEAD.Open(nil, nonce(s.readCounter), body, lb[:])
	if err != nil {
		return nil, errBadTag
	}
	s.readCounter++
	return plain, nil
}

// Read decodes one inbound block (or serves what is buffered), returning as
// soon as data is available. It must not try to fill the caller's whole buffer:
// callers like bufio.Read pass a large buffer and would otherwise block waiting
// for blocks the peer has not sent yet.
func (s *Session) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.readBuf) > 0 {
		c := copy(p, s.readBuf)
		s.readBuf = s.readBuf[c:]
		return c, nil
	}
	blk, err := s.ReadOne()
	if err != nil {
		return 0, err
	}
	c := copy(p, blk)
	if c < len(blk) {
		s.readBuf = blk[c:]
	}
	return c, nil
}

// Write encodes p into outbound HAP blocks and writes them in one call.
func (s *Session) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var buf []byte
	for off := 0; off < len(p); {
		end := off + maxOutbound
		if end > len(p) {
			end = len(p)
		}
		chunk := p[off:end]
		var lb [2]byte
		binary.LittleEndian.PutUint16(lb[:], uint16(len(chunk)))
		ct := s.writeAEAD.Seal(nil, nonce(s.writeCounter), chunk, lb[:])
		buf = append(buf, lb[0], lb[1])
		buf = append(buf, ct...)
		s.writeCounter++
		off = end
	}
	if _, err := s.rw.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}
