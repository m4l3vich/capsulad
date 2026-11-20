package hapcrypt

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// endpoint is one end of an in-memory duplex byte pipe.
type endpoint struct {
	rd, wr *bytes.Buffer
	writes int
}

func (e *endpoint) Read(p []byte) (int, error)  { return e.rd.Read(p) }
func (e *endpoint) Write(p []byte) (int, error) { e.writes++; return e.wr.Write(p) }

func duplex() (*endpoint, *endpoint) {
	ab := &bytes.Buffer{}
	ba := &bytes.Buffer{}
	return &endpoint{rd: ba, wr: ab}, &endpoint{rd: ab, wr: ba}
}

func TestFramingRoundTripSizes(t *testing.T) {
	k1 := bytes.Repeat([]byte{0x11}, 32)
	k2 := bytes.Repeat([]byte{0x22}, 32)

	for _, size := range []int{1, 1023, 1024, 1025, 2048} {
		epA, epB := duplex()
		sA, err := NewSession(epA, k1, k2)
		if err != nil {
			t.Fatal(err)
		}
		sB, err := NewSession(epB, k2, k1)
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte{0xab}, size)
		n, err := sA.Write(payload)
		if err != nil || n != size {
			t.Fatalf("size %d: Write = %d,%v", size, n, err)
		}
		got := make([]byte, size)
		if _, err := io.ReadFull(sB, got); err != nil {
			t.Fatalf("size %d: ReadFull: %v", size, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: payload mismatch", size)
		}
	}
}

func TestZeroLengthWrite(t *testing.T) {
	epA, epB := duplex()
	k := bytes.Repeat([]byte{0x33}, 32)
	sA, _ := NewSession(epA, k, k)
	sB, _ := NewSession(epB, k, k)
	if n, err := sA.Write(nil); n != 0 || err != nil {
		t.Fatalf("Write(nil) = %d,%v", n, err)
	}
	if epA.wr.Len() != 0 {
		t.Fatal("Write(nil) emitted bytes")
	}
	if _, err := sB.ReadOne(); err != io.EOF {
		t.Fatalf("ReadOne on empty = %v, want io.EOF", err)
	}
}

func TestInterleavedDirections(t *testing.T) {
	k1 := bytes.Repeat([]byte{0x01}, 32)
	k2 := bytes.Repeat([]byte{0x02}, 32)
	epA, epB := duplex()
	sA, _ := NewSession(epA, k1, k2)
	sB, _ := NewSession(epB, k2, k1)

	a1 := bytes.Repeat([]byte{0xa1}, 500)
	a2 := bytes.Repeat([]byte{0xa2}, 700)
	b1 := bytes.Repeat([]byte{0xb1}, 300)
	b2 := bytes.Repeat([]byte{0xb2}, 1500)

	// Interleave writes from both directions before reading.
	if _, err := sA.Write(a1); err != nil {
		t.Fatal(err)
	}
	if _, err := sB.Write(b1); err != nil {
		t.Fatal(err)
	}
	if _, err := sA.Write(a2); err != nil {
		t.Fatal(err)
	}
	if _, err := sB.Write(b2); err != nil {
		t.Fatal(err)
	}

	if got := readN(t, sB, len(a1)+len(a2)); !bytes.Equal(got, append(append([]byte{}, a1...), a2...)) {
		t.Fatal("A->B stream mismatch")
	}
	if got := readN(t, sA, len(b1)+len(b2)); !bytes.Equal(got, append(append([]byte{}, b1...), b2...)) {
		t.Fatal("B->A stream mismatch")
	}
}

func readN(t *testing.T, s *Session, n int) []byte {
	t.Helper()
	out := make([]byte, n)
	// Exercise partial reads buffering by reading in small chunks.
	off := 0
	for off < n {
		step := 7
		if off+step > n {
			step = n - off
		}
		m, err := s.Read(out[off : off+step])
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		off += m
	}
	return out
}

func TestTamperedBlockFails(t *testing.T) {
	k1 := bytes.Repeat([]byte{0x44}, 32)
	k2 := bytes.Repeat([]byte{0x55}, 32)
	epA, epB := duplex()
	sA, _ := NewSession(epA, k1, k2)
	sB, _ := NewSession(epB, k2, k1)

	if _, err := sA.Write(bytes.Repeat([]byte{0x7f}, 64)); err != nil {
		t.Fatal(err)
	}
	// Flip one ciphertext byte (offset 2 is the first ciphertext byte).
	epB.rd.Bytes()[2] ^= 0x01
	if _, err := sB.ReadOne(); err == nil {
		t.Fatal("expected tag verification failure")
	}
}

func TestSingleWriteCall(t *testing.T) {
	k := bytes.Repeat([]byte{0x66}, 32)
	epA, _ := duplex()
	sA, _ := NewSession(epA, k, k)
	if _, err := sA.Write(bytes.Repeat([]byte{0x01}, 2048)); err != nil {
		t.Fatal(err)
	}
	if epA.writes != 1 {
		t.Fatalf("underlying Write called %d times, want 1", epA.writes)
	}
}

func TestKeys(t *testing.T) {
	// Independent vectors: HKDF-SHA512 over the shared secret with the HAP
	// salt/info strings, computed with a Python hashlib/hmac script (not with
	// this package). The control and event directions are asymmetric by
	// design; the first expectation pins that.
	secret := make([]byte, 64)
	for i := range secret {
		secret[i] = byte(i)
	}
	const (
		controlOutbound = "c2f56f1912e2fc1f6604fd5bccab619272f687345aa4bce1c3c857421bd3b821"
		controlInbound  = "5a6cb19bcbe7d4df2dd8279f39562f7fae2dbf73eb5a4f98849c245c82b2fe96"
		eventOutbound   = "4ab337f2edc98d6a5dcdebc107b1aa174f3b24908a3780294fc94dc6983ee353"
		eventInbound    = "11954fc3044c580a9a64ed0b699039dfd0e7dd476eb78366ae160f2962708e18"
	)
	control := ControlChannelKeys(secret)
	if got := hex.EncodeToString(control.Outbound); got != controlOutbound {
		t.Fatalf("control outbound = %s, want %s (Control-Read-Encryption-Key)", got, controlOutbound)
	}
	if got := hex.EncodeToString(control.Inbound); got != controlInbound {
		t.Fatalf("control inbound = %s, want %s (Control-Write-Encryption-Key)", got, controlInbound)
	}
	events := EventChannelKeys(secret)
	if got := hex.EncodeToString(events.Outbound); got != eventOutbound {
		t.Fatalf("event outbound = %s, want %s (Events-Write-Encryption-Key)", got, eventOutbound)
	}
	if got := hex.EncodeToString(events.Inbound); got != eventInbound {
		t.Fatalf("event inbound = %s, want %s (Events-Read-Encryption-Key)", got, eventInbound)
	}
	other := make([]byte, 64)
	for i := range other {
		other[i] = 0xff
	}
	if bytes.Equal(ControlChannelKeys(other).Outbound, control.Outbound) {
		t.Fatal("keys must change with the shared secret")
	}
	// Keys are copied, not aliased, and a session accepts the pair.
	if _, err := NewSession(duplexEcho{}, control.Outbound, control.Inbound); err != nil {
		t.Fatal(err)
	}
}

type duplexEcho struct{}

func (duplexEcho) Read(p []byte) (int, error)  { return 0, io.EOF }
func (duplexEcho) Write(p []byte) (int, error) { return len(p), nil }

// The session-side close summary asserts this optional interface.
var _ interface{ ReadStats() (uint64, string) } = (*Session)(nil)

func TestReadOutcomeTokens(t *testing.T) {
	cases := map[ReadOutcome]string{
		ReadOK:              "none",
		ReadClosed:          "eof",
		ReadTruncatedLength: "partial-length",
		ReadTruncatedBlock:  "partial-body",
		ReadAuthFailed:      "bad-tag",
		ReadTransportError:  "io-error",
		ReadOutcome(200):    "none",
	}
	for outcome, want := range cases {
		if got := ReadOutcomeToken(outcome); got != want {
			t.Errorf("ReadOutcomeToken(%d) = %q, want %q", outcome, got, want)
		}
	}
}

func newKeySession(t *testing.T, rw io.ReadWriter) *Session {
	t.Helper()
	key := bytes.Repeat([]byte{0x5a}, 32)
	session, err := NewSession(rw, key, key)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// failingRW serves data once, then reports a transport error. It stands in for
// a reset or a daemon stream that fails without a clean close.
type failingRW struct {
	data []byte
	err  error
}

func (r *failingRW) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *failingRW) Write(p []byte) (int, error) { return len(p), nil }

func TestReadOutcomeCleanClose(t *testing.T) {
	session := newKeySession(t, bytes.NewBuffer(nil))
	if _, err := session.ReadOne(); err != io.EOF {
		t.Fatalf("ReadOne on an empty stream = %v, want io.EOF", err)
	}
	if got := ReadOutcomeOf(io.EOF); got != ReadClosed {
		t.Fatalf("ReadOutcomeOf(io.EOF) = %v, want ReadClosed", got)
	}
	blocks, outcome := session.ReadStats()
	if blocks != 0 || outcome != "eof" {
		t.Fatalf("ReadStats = %d,%q, want 0,eof", blocks, outcome)
	}
}

func TestReadOutcomePartialLength(t *testing.T) {
	session := newKeySession(t, bytes.NewBuffer([]byte{0x05}))
	_, err := session.ReadOne()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadOne = %v, want io.ErrUnexpectedEOF", err)
	}
	if got := ReadOutcomeOf(err); got != ReadTruncatedLength {
		t.Fatalf("ReadOutcomeOf = %v, want ReadTruncatedLength", got)
	}
	if !strings.Contains(err.Error(), "block length") {
		t.Fatalf("error text changed: %q", err)
	}
	if blocks, outcome := session.ReadStats(); blocks != 0 || outcome != "partial-length" {
		t.Fatalf("ReadStats = %d,%q, want 0,partial-length", blocks, outcome)
	}
}

func TestReadOutcomePartialBody(t *testing.T) {
	// Length 8 promises 8 ciphertext bytes and a 16-byte tag; deliver 3.
	session := newKeySession(t, bytes.NewBuffer([]byte{0x08, 0x00, 0xaa, 0xbb, 0xcc}))
	_, err := session.ReadOne()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadOne = %v, want io.ErrUnexpectedEOF", err)
	}
	if got := ReadOutcomeOf(err); got != ReadTruncatedBlock {
		t.Fatalf("ReadOutcomeOf = %v, want ReadTruncatedBlock", got)
	}
	if !strings.Contains(err.Error(), "block body") {
		t.Fatalf("error text changed: %q", err)
	}
	if blocks, outcome := session.ReadStats(); blocks != 0 || outcome != "partial-body" {
		t.Fatalf("ReadStats = %d,%q, want 0,partial-body", blocks, outcome)
	}
}

func TestReadOutcomeBadTag(t *testing.T) {
	k1 := bytes.Repeat([]byte{0x44}, 32)
	k2 := bytes.Repeat([]byte{0x55}, 32)
	epA, epB := duplex()
	sA, _ := NewSession(epA, k1, k2)
	sB, _ := NewSession(epB, k2, k1)
	if _, err := sA.Write(bytes.Repeat([]byte{0x7f}, 64)); err != nil {
		t.Fatal(err)
	}
	epB.rd.Bytes()[2] ^= 0x01
	_, err := sB.ReadOne()
	if err == nil || err.Error() != errBadTag.Error() {
		t.Fatalf("ReadOne = %v, want the tag-mismatch message", err)
	}
	if got := ReadOutcomeOf(err); got != ReadAuthFailed {
		t.Fatalf("ReadOutcomeOf = %v, want ReadAuthFailed", got)
	}
	if blocks, outcome := sB.ReadStats(); blocks != 0 || outcome != "bad-tag" {
		t.Fatalf("ReadStats = %d,%q, want 0,bad-tag", blocks, outcome)
	}
}

func TestReadOutcomeTransportErrorIsSanitized(t *testing.T) {
	sentinel := errors.New("raw transport text must not escape")
	session := newKeySession(t, &failingRW{data: []byte{0x05}, err: sentinel})
	_, err := session.ReadOne()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadOne = %v, want io.ErrUnexpectedEOF", err)
	}
	if got := ReadOutcomeOf(err); got != ReadTransportError {
		t.Fatalf("ReadOutcomeOf = %v, want ReadTransportError", got)
	}
	if strings.Contains(err.Error(), "raw transport") {
		t.Fatalf("error leaked the underlying text: %q", err)
	}
	if blocks, outcome := session.ReadStats(); blocks != 0 || outcome != "io-error" {
		t.Fatalf("ReadStats = %d,%q, want 0,io-error", blocks, outcome)
	}

	// A transport failure inside a declared block is also io-error.
	session = newKeySession(t, &failingRW{data: []byte{0x01, 0x00, 0xaa}, err: sentinel})
	if _, err := session.ReadOne(); ReadOutcomeOf(err) != ReadTransportError {
		t.Fatalf("body transport ReadOutcomeOf = %v, want ReadTransportError", ReadOutcomeOf(err))
	}
	if blocks, outcome := session.ReadStats(); blocks != 0 || outcome != "io-error" {
		t.Fatalf("body ReadStats = %d,%q, want 0,io-error", blocks, outcome)
	}
}

func TestReadStatsCountsBlocks(t *testing.T) {
	k := bytes.Repeat([]byte{0x77}, 32)
	epA, epB := duplex()
	sA, _ := NewSession(epA, k, k)
	sB, _ := NewSession(epB, k, k)
	for _, part := range [][]byte{{0x01}, {0x02, 0x03}} {
		if _, err := sA.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if block, err := sB.ReadOne(); err != nil || !bytes.Equal(block, []byte{0x01}) {
		t.Fatalf("first ReadOne = %x,%v", block, err)
	}
	if blocks, outcome := sB.ReadStats(); blocks != 1 || outcome != "none" {
		t.Fatalf("after the first block ReadStats = %d,%q, want 1,none", blocks, outcome)
	}
	if block, err := sB.ReadOne(); err != nil || !bytes.Equal(block, []byte{0x02, 0x03}) {
		t.Fatalf("second ReadOne = %x,%v", block, err)
	}
	if blocks, outcome := sB.ReadStats(); blocks != 2 || outcome != "none" {
		t.Fatalf("after the second block ReadStats = %d,%q, want 2,none", blocks, outcome)
	}
	if _, err := sB.ReadOne(); err != io.EOF {
		t.Fatalf("ReadOne after close = %v, want io.EOF", err)
	}
	if blocks, outcome := sB.ReadStats(); blocks != 2 || outcome != "eof" {
		t.Fatalf("after close ReadStats = %d,%q, want 2,eof", blocks, outcome)
	}
}

func TestReadOutcomeOfWrapped(t *testing.T) {
	session := newKeySession(t, bytes.NewBuffer([]byte{0x05}))
	_, err := session.ReadOne()
	wrapped := fmt.Errorf("outer: %w", err)
	if got := ReadOutcomeOf(wrapped); got != ReadTruncatedLength {
		t.Fatalf("ReadOutcomeOf(wrapped) = %v, want ReadTruncatedLength", got)
	}
	if got := ReadOutcomeOf(nil); got != ReadOK {
		t.Fatalf("ReadOutcomeOf(nil) = %v, want ReadOK", got)
	}
}

// chunkWriter accepts at most max bytes per call and returns a nil error for
// the partial count, matching the public SDK stream contract.
type chunkWriter struct {
	max   int
	calls int
	buf   bytes.Buffer
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.calls++
	n := len(p)
	if w.max > 0 && n > w.max {
		n = w.max
	}
	w.buf.Write(p[:n])
	return n, nil
}

func (w *chunkWriter) Read(p []byte) (int, error) { return 0, io.EOF }

// zeroWriter reports no progress with a nil error: a broken writer must not
// become an infinite write loop.
type zeroWriter struct{ calls int }

func (w *zeroWriter) Write(p []byte) (int, error) { w.calls++; return 0, nil }
func (w *zeroWriter) Read(p []byte) (int, error)  { return 0, io.EOF }

type errWriter struct {
	n     int
	err   error
	calls int
}

func (w *errWriter) Write(p []byte) (int, error) { w.calls++; return w.n, w.err }
func (w *errWriter) Read(p []byte) (int, error)  { return 0, io.EOF }

// bufferRW reads from a buffer and discards writes.
type bufferRW struct{ *bytes.Buffer }

func (b bufferRW) Write(p []byte) (int, error) { return len(p), nil }

func TestWriteCompletesShortWrites(t *testing.T) {
	key := bytes.Repeat([]byte{0x6a}, 32)
	writer := &chunkWriter{max: 7}
	sender, err := NewSession(writer, key, key)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5c}, 5000)
	n, err := sender.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write = %d,%v, want %d,nil", n, err, len(payload))
	}
	if writer.calls < 2 {
		t.Fatalf("underlying Write called %d time(s); a short writer should be retried", writer.calls)
	}
	receiver, err := NewSession(bufferRW{Buffer: &writer.buf}, key, key)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(receiver, got); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload changed across short writes")
	}
}

func TestWriteNoProgressIsShortWrite(t *testing.T) {
	key := bytes.Repeat([]byte{0x6b}, 32)
	writer := &zeroWriter{}
	session, err := NewSession(writer, key, key)
	if err != nil {
		t.Fatal(err)
	}
	n, err := session.Write([]byte("payload"))
	if n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write = %d,%v, want 0,io.ErrShortWrite", n, err)
	}
	if writer.calls != 1 {
		t.Fatalf("underlying Write called %d times, want 1 (no spin)", writer.calls)
	}
}

func TestWriteInvalidCountIsShortWrite(t *testing.T) {
	key := bytes.Repeat([]byte{0x6c}, 32)
	session, err := NewSession(&errWriter{n: 100, err: nil}, key, key)
	if err != nil {
		t.Fatal(err)
	}
	n, err := session.Write([]byte("payload"))
	if n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write = %d,%v, want 0,io.ErrShortWrite", n, err)
	}
}

func TestWritePropagatesErrors(t *testing.T) {
	key := bytes.Repeat([]byte{0x6d}, 32)
	sentinel := errors.New("stream closed")
	writer := &errWriter{n: 3, err: sentinel}
	session, err := NewSession(writer, key, key)
	if err != nil {
		t.Fatal(err)
	}
	n, err := session.Write([]byte("payload"))
	if n != 0 || !errors.Is(err, sentinel) {
		t.Fatalf("Write = %d,%v, want 0,%v", n, err, sentinel)
	}
	// A late error must not be reported as accepted plaintext either. The
	// sealed frame is the payload plus a 2-byte length and a 16-byte tag, so
	// this writer accepts the full ciphertext and then reports the error.
	session, err = NewSession(&errWriter{n: len("payload") + 2 + 16, err: sentinel}, key, key)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := session.Write([]byte("payload")); n != 0 || !errors.Is(err, sentinel) {
		t.Fatalf("full-count error Write = %d,%v, want 0,%v", n, err, sentinel)
	}
}
