package websocket_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"capsulad/apps/cadence/internal/websocket"
)

// pipeStream adapts one end of net.Pipe to websocket.Stream. An in-process
// pipe is the test-only loopback the project allows; production uses the
// shared SDK's verified TLS stream.
type pipeStream struct{ c net.Conn }

func (p pipeStream) Read(_ context.Context, b []byte) (int, error)  { return p.c.Read(b) }
func (p pipeStream) Write(_ context.Context, b []byte) (int, error) { return p.c.Write(b) }
func (p pipeStream) Close() error                                   { return p.c.Close() }

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type serverPeer struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func newPipe(t *testing.T) (websocket.Stream, *serverPeer) {
	t.Helper()
	a, b := net.Pipe()
	return pipeStream{a}, &serverPeer{t: t, c: b, br: bufio.NewReader(b)}
}

// upgrade reads the client request, answers 101 with the correct accept key,
// and returns the request headers.
func (p *serverPeer) upgrade() map[string]string {
	p.t.Helper()
	req, err := httpReadRequest(p.br)
	if err != nil {
		p.t.Errorf("read request: %v", err)
		return nil
	}
	key := req["sec-websocket-key"]
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptFor(key) + "\r\n\r\n"
	if _, err := io.WriteString(p.c, resp); err != nil {
		p.t.Errorf("write response: %v", err)
	}
	return req
}

func (p *serverPeer) refuse(status int) {
	p.t.Helper()
	// A pipe write blocks until the other end reads, so the request must be
	// consumed before the refusal: this is the server's real order too.
	_, _ = httpReadRequest(p.br)
	_, _ = io.WriteString(p.c, "HTTP/1.1 "+itoa(status)+" Nope\r\nContent-Length: 0\r\n\r\n")
}

// send writes one unmasked server frame.
func (p *serverPeer) send(op byte, fin bool, payload []byte) {
	p.t.Helper()
	var hdr [10]byte
	hdr[0] = op
	if fin {
		hdr[0] |= 0x80
	}
	n := 2
	switch l := len(payload); {
	case l <= 125:
		hdr[1] = byte(l)
	case l <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(l))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(l))
		n = 10
	}
	if _, err := p.c.Write(hdr[:n]); err != nil {
		p.t.Errorf("send header: %v", err)
		return
	}
	if _, err := p.c.Write(payload); err != nil {
		p.t.Errorf("send payload: %v", err)
	}
}

// recv reads one client frame and fails unless it is masked.
func (p *serverPeer) recv() (op byte, payload []byte) {
	p.t.Helper()
	var hdr [2]byte
	if _, err := io.ReadFull(p.br, hdr[:]); err != nil {
		p.t.Errorf("read frame header: %v", err)
		return 0, nil
	}
	op = hdr[0] & 0x0f
	if hdr[1]&0x80 == 0 {
		p.t.Error("client frame is not masked")
		return 0, nil
	}
	length := int64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(p.br, ext[:]); err != nil {
			p.t.Errorf("read ext length: %v", err)
			return 0, nil
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(p.br, ext[:]); err != nil {
			p.t.Errorf("read ext length: %v", err)
			return 0, nil
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	var mask [4]byte
	if _, err := io.ReadFull(p.br, mask[:]); err != nil {
		p.t.Errorf("read mask: %v", err)
		return 0, nil
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(p.br, payload); err != nil {
		p.t.Errorf("read payload: %v", err)
		return 0, nil
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return op, payload
}

func acceptFor(key string) string {
	sum := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func httpReadRequest(br *bufio.Reader) (map[string]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "GET ") || !strings.Contains(line, "HTTP/1.1") {
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
			return nil, errors.New("bad header: " + line)
		}
		headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
}

func itoa(n int) string {
	if n == 401 {
		return "401"
	}
	return "500"
}

// connect performs the handshake: the server side runs concurrently because a
// pipe write blocks until the other end reads.
func connect(t *testing.T, cs websocket.Stream, srv *serverPeer) (*websocket.Conn, map[string]string) {
	t.Helper()
	reqCh := make(chan map[string]string, 1)
	go func() { reqCh <- srv.upgrade() }()
	conn, err := websocket.Dial(context.Background(), cs, websocket.Options{
		Host: "cadence.example.com", Path: "/v2/device", Token: "tok",
	})
	req := <-reqCh
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, req
}

func TestHandshakeAndBinaryMessage(t *testing.T) {
	cs, srv := newPipe(t)
	conn, req := connect(t, cs, srv)
	defer conn.CloseNow()

	if got := req["authorization"]; got != "Bearer tok" {
		t.Fatalf("authorization = %q", got)
	}
	if req["upgrade"] != "websocket" || req["sec-websocket-version"] != "13" {
		t.Fatalf("headers = %v", req)
	}
	if !strings.HasPrefix(req["sec-websocket-key"], "") || len(req["sec-websocket-key"]) == 0 {
		t.Fatalf("no key: %v", req)
	}

	go srv.send(0x2, true, []byte("envelope"))
	op, payload, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if op != websocket.OpBinary || string(payload) != "envelope" {
		t.Fatalf("op=%v payload=%q", op, payload)
	}
}

func TestHandshakeUnauthorized(t *testing.T) {
	cs, srv := newPipe(t)
	errCh := make(chan error, 1)
	go func() {
		_, err := websocket.Dial(context.Background(), cs, websocket.Options{
			Host: "cadence.example.com", Path: "/v2/device", Token: "bad",
		})
		errCh <- err
	}()
	srv.refuse(401)
	if err := <-errCh; !errors.Is(err, websocket.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestBadAcceptKeyRejected(t *testing.T) {
	a, b := net.Pipe()
	cs := pipeStream{a}
	defer b.Close()
	go func() {
		br := bufio.NewReader(b)
		_, _ = httpReadRequest(br)
		_, _ = io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
			"Connection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n")
	}()
	if _, err := websocket.Dial(context.Background(), cs, websocket.Options{Host: "h", Path: "/p"}); !errors.Is(err, websocket.ErrHandshake) {
		t.Fatalf("got %v", err)
	}
}

func TestExtensionRefused(t *testing.T) {
	a, b := net.Pipe()
	cs := pipeStream{a}
	defer b.Close()
	go func() {
		br := bufio.NewReader(b)
		req, _ := httpReadRequest(br)
		_, _ = io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
			"Connection: Upgrade\r\nSec-WebSocket-Extensions: permessage-deflate\r\n"+
			"Sec-WebSocket-Accept: "+acceptFor(req["sec-websocket-key"])+"\r\n\r\n")
	}()
	if _, err := websocket.Dial(context.Background(), cs, websocket.Options{Host: "h", Path: "/p"}); !errors.Is(err, websocket.ErrHandshake) {
		t.Fatalf("got %v", err)
	}
}

func TestPingIsAnswered(t *testing.T) {
	cs, srv := newPipe(t)
	conn, _ := connect(t, cs, srv)
	defer conn.CloseNow()

	go srv.send(0x9, true, []byte("keepalive"))

	read := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadMessage(context.Background())
		read <- err
	}()
	op, payload := srv.recv()
	if op != 0xA || string(payload) != "keepalive" {
		t.Fatalf("pong op=%x payload=%q", op, payload)
	}
	srv.send(0x2, true, []byte("after-ping"))
	if err := <-read; err != nil {
		t.Fatal(err)
	}
}

func TestFragmentedMessageReassembled(t *testing.T) {
	cs, srv := newPipe(t)
	conn, _ := connect(t, cs, srv)
	defer conn.CloseNow()

	go func() {
		srv.send(0x2, false, []byte("one-"))
		srv.send(0x0, false, []byte("two-"))
		srv.send(0x0, true, []byte("three"))
	}()
	op, payload, err := conn.ReadMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if op != websocket.OpBinary || string(payload) != "one-two-three" {
		t.Fatalf("op=%v payload=%q", op, payload)
	}
}

func TestCloseFrameIsAnError(t *testing.T) {
	cs, srv := newPipe(t)
	conn, _ := connect(t, cs, srv)
	defer conn.CloseNow()

	go srv.send(0x8, true, []byte{0x03, 0xE8, 'b', 'y', 'e'})
	_, _, err := conn.ReadMessage(context.Background())
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseNormal || ce.Reason != "bye" {
		t.Fatalf("got %v", err)
	}
}

func TestServerMaskedFrameRejected(t *testing.T) {
	cs, srv := newPipe(t)
	conn, _ := connect(t, cs, srv)
	defer conn.CloseNow()

	go func() { _, _ = srv.c.Write([]byte{0x82, 0x81, 0, 0, 0, 0, 'x'}) }()
	_, _, err := conn.ReadMessage(context.Background())
	if !errors.Is(err, websocket.ErrProtocol) {
		t.Fatalf("got %v", err)
	}
}

func TestOversizedMessageRejected(t *testing.T) {
	cs, srv := newPipe(t)
	conn, _ := connect(t, cs, srv)
	defer conn.CloseNow()

	go func() {
		// A 32 MiB announcement.
		_, _ = srv.c.Write([]byte{0x82, 127, 0, 0, 0, 0, 0x02, 0, 0, 0})
	}()
	_, _, err := conn.ReadMessage(context.Background())
	if !errors.Is(err, websocket.ErrMessageTooBig) {
		t.Fatalf("got %v", err)
	}
}

// A peer that withholds CRLF must not grow memory: the head reader stops at its
// hard cap and the handshake fails.
func TestOversizedResponseHeadRejected(t *testing.T) {
	a, b := net.Pipe()
	cs := pipeStream{a}
	defer b.Close()
	go func() {
		br := bufio.NewReader(b)
		_, _ = httpReadRequest(br)
		huge := make([]byte, 9000)
		for i := range huge {
			huge[i] = 'a'
		}
		_, _ = b.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nX-Fill: "))
		_, _ = b.Write(huge) // no CRLF, ever
	}()
	start := time.Now()
	if _, err := websocket.Dial(context.Background(), cs, websocket.Options{Host: "h", Path: "/p"}); !errors.Is(err, websocket.ErrHandshake) {
		t.Fatalf("got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("header cap took %s", elapsed)
	}
}

// A CRLF-terminated head that is many short lines over the cumulative budget
// must also be refused: the per-line cap alone would allow it.
func TestManyLineResponseHeadRejected(t *testing.T) {
	a, b := net.Pipe()
	cs := pipeStream{a}
	defer b.Close()
	go func() {
		br := bufio.NewReader(b)
		_, _ = httpReadRequest(br)
		_, _ = io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\n")
		// ~900 one-kilobyte headers: every line is short, the head is not.
		line := "X-Pad: " + strings.Repeat("a", 1000) + "\r\n"
		for range 900 {
			if _, err := io.WriteString(b, line); err != nil {
				return
			}
		}
	}()
	if _, err := websocket.Dial(context.Background(), cs, websocket.Options{Host: "h", Path: "/p"}); !errors.Is(err, websocket.ErrHandshake) {
		t.Fatalf("got %v", err)
	}
}

func TestClientWriteIsMasked(t *testing.T) {
	cs, srv := newPipe(t)
	conn, _ := connect(t, cs, srv)
	defer conn.CloseNow()

	go func() { _ = conn.WriteBinary(context.Background(), []byte("hi")) }()
	op, payload := srv.recv()
	if op != 0x2 || string(payload) != "hi" {
		t.Fatalf("op=%x payload=%q", op, payload)
	}
}
