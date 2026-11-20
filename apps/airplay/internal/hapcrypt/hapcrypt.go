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
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// maxOutbound is the maximum plaintext size of a single outbound block.
const maxOutbound = 1024

var errBadTag = errors.New("hapcrypt: authentication tag mismatch")

// ReadOutcome is a bounded classification of the terminal outcome of reading
// an encrypted channel. It carries no error text, peer data or payload bytes,
// so a close summary can report the token from ReadOutcomeToken.
type ReadOutcome uint8

const (
	// ReadOK is the zero value: the last block read (if any) succeeded.
	ReadOK ReadOutcome = iota
	// ReadClosed is a clean close at a block boundary: no byte of the next
	// 2-byte length prefix was received.
	ReadClosed
	// ReadTruncatedLength is a close or I/O failure after one byte of the
	// 2-byte length prefix.
	ReadTruncatedLength
	// ReadTruncatedBlock is a close or I/O failure inside the ciphertext and
	// authentication tag a length prefix promised.
	ReadTruncatedBlock
	// ReadAuthFailed is a ChaCha20-Poly1305 tag mismatch.
	ReadAuthFailed
	// ReadTransportError is an underlying stream failure before a complete
	// block was received (for example a reset), not a clean close.
	ReadTransportError
)

// ReadOutcomeToken returns the fixed, log-safe token for an outcome.
func ReadOutcomeToken(outcome ReadOutcome) string {
	switch outcome {
	case ReadClosed:
		return "eof"
	case ReadTruncatedLength:
		return "partial-length"
	case ReadTruncatedBlock:
		return "partial-body"
	case ReadAuthFailed:
		return "bad-tag"
	case ReadTransportError:
		return "io-error"
	default:
		return "none"
	}
}

// readFailure couples the error readers already received with its bounded
// outcome. The text and Unwrap chain are unchanged; the OS error is not
// retained, so a caller that logs err.Error() cannot leak unvetted text.
type readFailure struct {
	outcome ReadOutcome
	err     error
}

func (e *readFailure) Error() string { return e.err.Error() }
func (e *readFailure) Unwrap() error { return e.err }

// ReadOutcomeOf reports the bounded class of an error returned by ReadOne or
// Read. A nil error is ReadOK; a clean io.EOF is ReadClosed; errors not
// produced by this package are ReadTransportError. Wrapped errors keep their
// class.
func ReadOutcomeOf(err error) ReadOutcome {
	if err == nil {
		return ReadOK
	}
	var failure *readFailure
	if errors.As(err, &failure) {
		return failure.outcome
	}
	if errors.Is(err, io.EOF) {
		return ReadClosed
	}
	return ReadTransportError
}

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

	// Telemetry for a bounded close summary. It is read from the connection
	// goroutine and may be sampled by whoever ends the session.
	readBlocks  atomic.Uint64
	readOutcome atomic.Uint32
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
//
// The error vocabulary is unchanged: a clean close at a block boundary is
// io.EOF, and every short read is io.ErrUnexpectedEOF so the session still ends
// with "the control connection ended". ReadOutcomeOf recovers the bounded class
// without exposing the underlying error text.
func (s *Session) ReadOne() ([]byte, error) {
	var lb [2]byte
	n, err := io.ReadFull(s.rw, lb[:])
	if err != nil {
		if n == 0 && err == io.EOF {
			s.readOutcome.Store(uint32(ReadClosed))
			return nil, io.EOF
		}
		outcome := ReadTruncatedLength
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			outcome = ReadTransportError
		}
		s.readOutcome.Store(uint32(outcome))
		return nil, &readFailure{
			outcome: outcome,
			err:     fmt.Errorf("hapcrypt: reading block length: %w", io.ErrUnexpectedEOF),
		}
	}
	length := int(binary.LittleEndian.Uint16(lb[:]))
	body := make([]byte, length+chacha20poly1305.Overhead)
	if _, err := io.ReadFull(s.rw, body); err != nil {
		outcome := ReadTruncatedBlock
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			outcome = ReadTransportError
		}
		s.readOutcome.Store(uint32(outcome))
		return nil, &readFailure{
			outcome: outcome,
			err:     fmt.Errorf("hapcrypt: reading block body: %w", io.ErrUnexpectedEOF),
		}
	}
	plain, err := s.readAEAD.Open(nil, nonce(s.readCounter), body, lb[:])
	if err != nil {
		s.readOutcome.Store(uint32(ReadAuthFailed))
		return nil, &readFailure{outcome: ReadAuthFailed, err: errBadTag}
	}
	s.readCounter++
	s.readBlocks.Add(1)
	s.readOutcome.Store(uint32(ReadOK))
	return plain, nil
}

// ReadStats reports how many inbound blocks this channel decrypted and the
// terminal outcome token of its last read. It is safe to call from another
// goroutine and returns only bounded values for a sanitized close summary.
func (s *Session) ReadStats() (blocks uint64, outcome string) {
	return s.readBlocks.Load(), ReadOutcomeToken(ReadOutcome(s.readOutcome.Load()))
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

// Write encodes p into outbound HAP blocks and writes the ciphertext
// completely, retrying a short count. It returns len(p) only when every block
// reached the underlying writer; any error means the channel must close.
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
	// The underlying stream may accept fewer ciphertext bytes than offered:
	// the public SDK's TCPStream.Write contract explicitly allows a short
	// count with a nil error. Complete the write here as well; a partial
	// encrypted frame desynchronizes the channel, so it is never reported as
	// accepted plaintext. A writer that reports no progress is a short write,
	// not something to spin on; closing the stream unblocks a stalled call.
	written := 0
	for written < len(buf) {
		n, err := s.rw.Write(buf[written:])
		if n < 0 || n > len(buf)-written {
			return 0, io.ErrShortWrite
		}
		written += n
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.ErrShortWrite
		}
	}
	return len(p), nil
}
