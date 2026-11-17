package link_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/apps/cadence/internal/link"
	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/websocket"
	"capsulad/apps/cadence/internal/wire"
)

// TestTransportOverLoopbackFakeUpstream drives the real websocket client and
// link handshake against an independent loopback server that speaks the
// upstream cadence.v2 device link: HTTP upgrade, then one protobuf Envelope per
// masked binary frame. Nothing here touches a remote service; it is the
// transport proof for the SDK seam (the SDK supplies the TLS socket, this test
// supplies a loopback TCP one) and for the handshake bytes.
func TestTransportOverLoopbackFakeUpstream(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	var (
		mu        sync.Mutex
		hello     *cadencev2.Hello
		report    *cadencev2.CapabilityReport
		sawAuth   string
		serverErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := lis.Accept()
		if err != nil {
			mu.Lock()
			serverErr = err
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		headers, err := readUpgrade(br)
		if err != nil {
			mu.Lock()
			serverErr = err
			return
		}
		mu.Lock()
		sawAuth = headers["authorization"]
		mu.Unlock()
		accept := acceptFor(headers["sec-websocket-key"])
		if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
			"Connection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n"); err != nil {
			mu.Lock()
			serverErr = err
			return
		}
		// Read the Hello, answer with Welcome, then read the report and close
		// the session the way the server would.
		seen := 0
		for seen < 2 {
			env, err := readClientEnvelope(br)
			if err != nil {
				mu.Lock()
				serverErr = err
				return
			}
			seen++
			switch {
			case env.GetHello() != nil:
				mu.Lock()
				hello = env.GetHello()
				mu.Unlock()
				if err := writeServerEnvelope(conn, &cadencev2.Envelope{Body: &cadencev2.Envelope_Welcome{
					Welcome: &cadencev2.Welcome{ServerVersion: "loopback-fake", SendFullReport: true},
				}}); err != nil {
					mu.Lock()
					serverErr = err
					return
				}
			case env.GetCapabilityReport() != nil:
				mu.Lock()
				report = env.GetCapabilityReport()
				mu.Unlock()
				if err := writeServerEnvelope(conn, &cadencev2.Envelope{Body: &cadencev2.Envelope_SessionClose{
					SessionClose: &cadencev2.SessionClose{SessionId: "s-loop", Code: "ok"},
				}}); err != nil {
					mu.Lock()
					serverErr = err
					return
				}
			}
		}
		// Hold the connection until the client cancels.
		_, _ = io.Copy(io.Discard, br)
	}()

	d := &loopbackDialer{addr: lis.Addr().String()}
	h := &fakeHandler{}
	client, err := link.New(d, log.New(io.Discard, "", 0), h, "loopback/1", "capsula-mini")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	waitFor(t, "session close dispatched", func() bool {
		for _, k := range h.kinds() {
			if k == "session_close" {
				return true
			}
		}
		return false
	})
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake upstream did not finish after the link closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if serverErr != nil {
		t.Fatalf("fake upstream: %v", serverErr)
	}
	if sawAuth != "Bearer loopback-token" {
		t.Fatalf("authorization = %q", sawAuth)
	}
	if hello.GetProtocol() != 2 || hello.GetBoard() != "capsula-mini" {
		t.Fatalf("hello = %v", hello)
	}
	if len(report.GetDescriptors()) != 0 || len(report.GetHash()) != 32 {
		t.Fatalf("report = %v", report)
	}
	if string(hello.GetCapabilitiesHash()) != string(report.GetHash()) {
		t.Fatal("Hello and the report disagree on the capability hash")
	}
}

// loopbackDialer is the production transport minus the SDK: a TCP connection
// plus the app's real websocket upgrade.
type loopbackDialer struct{ addr string }

func (d *loopbackDialer) Dial(ctx context.Context) (link.Conn, error) {
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		return nil, err
	}
	c, err := websocket.Dial(ctx, netStream{conn}, websocket.Options{
		Host: "127.0.0.1", Path: "/v2/device", Token: "loopback-token",
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	// The SDK's TCPStream honours ctx on Read; a plain net.Conn does not, so
	// close it when the link's context ends to unblock the reader exactly as
	// Close does in production.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return c, nil
}

type netStream struct{ c net.Conn }

func (s netStream) Read(_ context.Context, p []byte) (int, error)  { return s.c.Read(p) }
func (s netStream) Write(_ context.Context, p []byte) (int, error) { return s.c.Write(p) }
func (s netStream) Close() error                                   { return s.c.Close() }

func readUpgrade(br *bufio.Reader) (map[string]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "GET /v2/device HTTP/1.1") {
		return nil, errors.New("bad request line: " + line)
	}
	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSuffix(line, "\r\n")
		if line == "" {
			return headers, nil
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, errors.New("bad header")
		}
		headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
}

func acceptFor(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// readClientEnvelope reads one masked client binary frame and decodes it.
func readClientEnvelope(br *bufio.Reader) (*cadencev2.Envelope, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0]&0x0f != 0x2 || hdr[1]&0x80 == 0 {
		return nil, errors.New("not a masked binary frame")
	}
	length := int64(hdr[1] & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	}
	var mask [4]byte
	if _, err := io.ReadFull(br, mask[:]); err != nil {
		return nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return wire.Decode(payload)
}

// writeServerEnvelope writes one unmasked server binary frame.
func writeServerEnvelope(w io.Writer, env *cadencev2.Envelope) error {
	b, err := wire.Encode(env)
	if err != nil {
		return err
	}
	hdr := []byte{0x82}
	switch {
	case len(b) <= 125:
		hdr = append(hdr, byte(len(b)))
	case len(b) <= 0xffff:
		hdr = append(hdr, 126, byte(len(b)>>8), byte(len(b)))
	default:
		return errors.New("frame too large for the fake")
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
