package hapcrypt

import (
	"bytes"
	"encoding/hex"
	"io"
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
