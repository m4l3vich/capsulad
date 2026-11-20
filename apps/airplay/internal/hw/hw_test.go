package hw

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"capsulad/apps/airplay/internal/session"
)

// datagramOnlyListener has exactly the method set of the production
// udpListener: Receive/Send/Addr/Group/Close, with no Accept. The address
// wrapper must not require the full session.Listener interface, because a UDP
// socket that fails that assertion reports port 0 and every SETUP is answered
// with server_port=0.
type datagramOnlyListener struct{ port int }

func (l *datagramOnlyListener) Receive() ([]byte, session.UDPAddr, error) {
	return nil, session.UDPAddr{}, nil
}

func (l *datagramOnlyListener) Send(session.UDPAddr, []byte) error { return nil }
func (l *datagramOnlyListener) Addr() (string, int)                { return "0.0.0.0", l.port }
func (l *datagramOnlyListener) Group() string                      { return "" }
func (l *datagramOnlyListener) Close() error                       { return nil }

func TestTrackedUDPListenerReportsTheBoundPort(t *testing.T) {
	rt := &Runtime{listeners: map[uint64]closer{}}
	var listener session.UDPListener = rt.track(&datagramOnlyListener{port: 46406})
	if _, port := listener.Addr(); port != 46406 {
		t.Fatalf("Addr port = %d, want 46406", port)
	}
}

func TestTrackedListenerIgnoresUnknownInner(t *testing.T) {
	rt := &Runtime{listeners: map[uint64]closer{}}
	var listener session.UDPListener = rt.track(nopCloser{})
	if address, port := listener.Addr(); address != "" || port != 0 {
		t.Fatalf("Addr = %q/%d, want an empty address", address, port)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// chunkSDKStream stands in for capsulad.TCPStream: it accepts a bounded
// number of bytes per call and returns the short count with a nil error.
type chunkSDKStream struct {
	max   int
	calls int
	buf   bytes.Buffer
}

func (s *chunkSDKStream) Write(ctx context.Context, p []byte) (int, error) {
	s.calls++
	n := len(p)
	if n > s.max {
		n = s.max
	}
	s.buf.Write(p[:n])
	return n, nil
}

type stuckSDKStream struct{ calls int }

func (s *stuckSDKStream) Write(context.Context, []byte) (int, error) {
	s.calls++
	return 0, nil
}

type failingSDKStream struct {
	n   int
	err error
}

func (s *failingSDKStream) Write(context.Context, []byte) (int, error) { return s.n, s.err }

func TestWriteFullCompletesSDKShortWrites(t *testing.T) {
	stream := &chunkSDKStream{max: 7}
	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = byte(i)
	}
	n, err := writeFull(stream, payload)
	if err != nil || n != len(payload) {
		t.Fatalf("writeFull = %d,%v, want %d,nil", n, err, len(payload))
	}
	if stream.calls < 2 {
		t.Fatalf("underlying Write called %d time(s), want retries", stream.calls)
	}
	if !bytes.Equal(stream.buf.Bytes(), payload) {
		t.Fatal("short writes did not reassemble the payload")
	}
}

func TestWriteFullNoProgressIsShortWrite(t *testing.T) {
	stream := &stuckSDKStream{}
	n, err := writeFull(stream, []byte("payload"))
	if n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeFull = %d,%v, want 0,io.ErrShortWrite", n, err)
	}
	if stream.calls != 1 {
		t.Fatalf("underlying Write called %d times, want 1 (no spin)", stream.calls)
	}
}

func TestWriteFullPropagatesErrors(t *testing.T) {
	sentinel := errors.New("stream closed")
	stream := &failingSDKStream{n: 3, err: sentinel}
	n, err := writeFull(stream, []byte("payload"))
	if n != 3 || !errors.Is(err, sentinel) {
		t.Fatalf("writeFull = %d,%v, want 3,%v", n, err, sentinel)
	}
}

func TestWriteFullRejectsInvalidCount(t *testing.T) {
	n, err := writeFull(&failingSDKStream{n: 8, err: nil}, []byte("payload"))
	if n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeFull = %d,%v, want 0,io.ErrShortWrite", n, err)
	}
}

func TestWriteFullEmptyWrite(t *testing.T) {
	stream := &chunkSDKStream{max: 1}
	if n, err := writeFull(stream, nil); n != 0 || err != nil || stream.calls != 0 {
		t.Fatalf("writeFull(nil) = %d,%v calls=%d, want a no-op", n, err, stream.calls)
	}
}
