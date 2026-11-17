// Package websocket is the RFC 6455 client the Cadence device link needs. It
// runs over the shared SDK's verified TLS stream instead of a bundled HTTP
// stack: the upgrade is one HTTP/1.1 request and the connection is binary
// frames from then on.
//
// Only what the link uses is implemented: the client handshake, binary
// messages with fragmentation, and the ping/pong/close control frames. No
// extension is offered, so a server that selects one (permessage-deflate) is
// refused rather than silently misread.
package websocket

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stream is the byte transport a websocket needs. *capsulad.TCPStream
// satisfies it in production (verified TLS, one logical reader) and an
// in-process pipe satisfies it in tests, so the framing code is exercised
// identically in both.
type Stream interface {
	Read(ctx context.Context, p []byte) (int, error)
	Write(ctx context.Context, p []byte) (int, error)
	Close() error
}

// Opcode is one websocket frame type.
type Opcode byte

const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA
)

// Close codes this client sends.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseUnsupportedData = 1003
	CloseInvalidPayload  = 1007
	CloseMessageTooBig   = 1009
)

// controlWriteTimeout bounds a control-frame reply. A peer that has stopped
// reading must never wedge the reader: the pong or close is best effort, and a
// link whose control reply cannot be written is dropped by its owner.
const controlWriteTimeout = 2 * time.Second

// DefaultMaxMessageBytes bounds one reassembled message. The link's envelope
// cap is 16 MiB; a peer that announces more is refused with 1009 instead of
// growing the process.
const DefaultMaxMessageBytes = 16 << 20

var (
	ErrUnauthorized  = errors.New("websocket: the device token was refused")
	ErrHandshake     = errors.New("websocket: handshake failed")
	ErrProtocol      = errors.New("websocket: protocol error")
	ErrMessageTooBig = errors.New("websocket: message too large")
	ErrClosed        = errors.New("websocket: connection closed")
)

// Options configures one dial. Host is the Host header (host:port where the
// port is not the scheme default); Path is the request target.
type Options struct {
	Host            string
	Path            string
	Token           string
	MaxMessageBytes int64
}

// Conn is one established websocket connection. Reads and writes must follow
// the protocol's one-reader/one-writer rule: ReadMessage is called from one
// goroutine, WriteBinary/WritePing from any goroutine (writes are serialized).
type Conn struct {
	s   Stream
	rd  *ctxReader
	br  *bufio.Reader
	max int64

	wmu    sync.Mutex
	closed atomic.Bool

	onPong func(payload []byte)
	onPing func(payload []byte)
}

// ctxReader lets bufio read through the SDK's context-aware Read. The context
// is replaced at the start of each ReadMessage; only one goroutine reads.
type ctxReader struct {
	s   Stream
	ctx context.Context
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if r.ctx == nil {
		return r.s.Read(context.Background(), p)
	}
	return r.s.Read(r.ctx, p)
}

// Dial performs the client handshake over s and returns the connection. A
// non-101 response is ErrUnauthorized (401) or ErrHandshake (anything else);
// s is closed on every failure.
func Dial(ctx context.Context, s Stream, opts Options) (*Conn, error) {
	if opts.Host == "" || opts.Path == "" {
		_ = s.Close()
		return nil, fmt.Errorf("%w: host and path are required", ErrHandshake)
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
	}
	keyBytes := make([]byte, 16)
	if _, err := cryptoRandRead(keyBytes); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("%w: key: %v", ErrHandshake, err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	var b strings.Builder
	b.WriteString("GET " + opts.Path + " HTTP/1.1\r\n")
	b.WriteString("Host: " + opts.Host + "\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	if opts.Token != "" {
		b.WriteString("Authorization: Bearer " + opts.Token + "\r\n")
	}
	b.WriteString("\r\n")

	rd := &ctxReader{s: s, ctx: ctx}
	if err := writeAll(ctx, s, []byte(b.String())); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}

	br := bufio.NewReaderSize(rd, 16<<10)
	status, headers, err := readResponseHead(br)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if status != 101 {
		_ = s.Close()
		if status == 401 {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("%w: status %d", ErrHandshake, status)
	}
	if !headerHasToken(headers["upgrade"], "websocket") || !headerHasToken(headers["connection"], "upgrade") {
		_ = s.Close()
		return nil, fmt.Errorf("%w: missing upgrade headers", ErrHandshake)
	}
	if ext := headers["sec-websocket-extensions"]; ext != "" {
		_ = s.Close()
		return nil, fmt.Errorf("%w: server selected extension %q; none was offered", ErrHandshake, ext)
	}
	want := acceptKey(key)
	if got := headers["sec-websocket-accept"]; got != want {
		_ = s.Close()
		return nil, fmt.Errorf("%w: bad Sec-WebSocket-Accept", ErrHandshake)
	}
	return &Conn{s: s, rd: rd, br: br, max: opts.MaxMessageBytes}, nil
}

// SetPongHandler installs a callback for a pong, used by the link's keepalive.
// It must not block.
func (c *Conn) SetPongHandler(f func(payload []byte)) { c.onPong = f }

// SetPingHandler installs a callback after an automatic pong reply. It must
// not block.
func (c *Conn) SetPingHandler(f func(payload []byte)) { c.onPing = f }

// ReadMessage reads the next data message, answering pings and honoring a
// close internally. It returns OpText or OpBinary and the fully reassembled
// payload. On a close it answers with the peer's code (once) and returns
// ErrClosed; a protocol violation returns ErrProtocol after a 1002 close.
func (c *Conn) ReadMessage(ctx context.Context) (Opcode, []byte, error) {
	c.rd.ctx = ctx
	var (
		messageOp  Opcode
		buf        []byte
		fragmented bool
	)
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case OpPing:
			_ = c.writeControl(ctx, OpPong, payload)
			if c.onPing != nil {
				c.onPing(payload)
			}
			continue
		case OpPong:
			if c.onPong != nil {
				c.onPong(payload)
			}
			continue
		case OpClose:
			code, reason := parseClose(payload)
			// Answer best effort and return at once: a peer that sent a close
			// and stopped reading must not park this reader.
			c.closed.Store(true)
			go func() {
				wctx, cancel := context.WithTimeout(context.Background(), controlWriteTimeout)
				defer cancel()
				_ = c.writeClose(wctx, code, reason)
			}()
			return 0, nil, &CloseError{Code: code, Reason: reason}
		case OpText, OpBinary:
			if fragmented {
				return 0, nil, c.fail(ctx, CloseProtocolError, ErrProtocol, "new data frame inside a fragmented message")
			}
			messageOp = op
			buf = append(buf, payload...)
		case OpContinuation:
			if messageOp == 0 {
				return 0, nil, c.fail(ctx, CloseProtocolError, ErrProtocol, "continuation without a start frame")
			}
			buf = append(buf, payload...)
		default:
			return 0, nil, c.fail(ctx, CloseProtocolError, ErrProtocol, "unknown opcode")
		}
		if int64(len(buf)) > c.max {
			return 0, nil, c.fail(ctx, CloseMessageTooBig, ErrMessageTooBig, "message too large")
		}
		fragmented = !fin
		if fin {
			return messageOp, buf, nil
		}
	}
}

// fail reports a protocol violation. The close is best effort and must not
// block the reader on a peer that stopped reading.
func (c *Conn) fail(ctx context.Context, code uint16, base error, detail string) error {
	c.closed.Store(true)
	go func() {
		wctx, cancel := context.WithTimeout(context.Background(), controlWriteTimeout)
		defer cancel()
		_ = c.writeClose(wctx, code, "")
	}()
	return fmt.Errorf("%w: %s", base, detail)
}

// readFrame reads one frame's header, payload mask and payload.
func (c *Conn) readFrame() (fin bool, op Opcode, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = hdr[0]&0x80 != 0
	rsv := hdr[0] & 0x70
	op = Opcode(hdr[0] & 0x0f)
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7f)
	switch {
	case rsv != 0:
		return false, 0, nil, fmt.Errorf("%w: reserved bits set", ErrProtocol)
	case masked:
		// A server must not mask; accepting it would hide a broken peer.
		return false, 0, nil, fmt.Errorf("%w: masked server frame", ErrProtocol)
	}
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
		if length < 126 {
			return false, 0, nil, fmt.Errorf("%w: non-minimal length", ErrProtocol)
		}
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		v := binary.BigEndian.Uint64(ext[:])
		if v < 0x10000 {
			return false, 0, nil, fmt.Errorf("%w: non-minimal length", ErrProtocol)
		}
		if v > uint64(c.max) {
			return false, 0, nil, ErrMessageTooBig
		}
		length = int64(v)
	}
	if length < 0 || length > c.max {
		return false, 0, nil, ErrMessageTooBig
	}
	if op == OpPing || op == OpPong || op == OpClose {
		if !fin || length > 125 {
			return false, 0, nil, fmt.Errorf("%w: invalid control frame", ErrProtocol)
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// WriteBinary sends one unfragmented binary message, masked as a client must.
func (c *Conn) WriteBinary(ctx context.Context, payload []byte) error {
	return c.writeFrame(ctx, OpBinary, true, payload)
}

// WritePing sends a ping with a small payload.
func (c *Conn) WritePing(ctx context.Context, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	return c.writeControl(ctx, OpPing, payload)
}

// Close sends a close frame (best effort) and closes the stream.
func (c *Conn) Close(ctx context.Context) error {
	_ = c.writeClose(ctx, CloseNormal, "")
	c.closed.Store(true)
	return c.s.Close()
}

// CloseNow closes the stream without a close frame; the link uses it when the
// peer is already gone.
func (c *Conn) CloseNow() error {
	c.closed.Store(true)
	return c.s.Close()
}

func (c *Conn) writeClose(ctx context.Context, code uint16, reason string) error {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	copy(payload[2:], reason)
	return c.writeControl(ctx, OpClose, payload)
}

func (c *Conn) writeControl(ctx context.Context, op Opcode, payload []byte) error {
	if c.closed.Load() && op != OpClose {
		return ErrClosed
	}
	wctx, cancel := context.WithTimeout(ctx, controlWriteTimeout)
	defer cancel()
	return c.writeFrame(wctx, op, true, payload)
}

// writeFrame writes one masked client frame.
func (c *Conn) writeFrame(ctx context.Context, op Opcode, fin bool, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	var hdr [14]byte
	hdr[0] = byte(op)
	if fin {
		hdr[0] |= 0x80
	}
	hdr[1] = 0x80 // masked
	n := 2
	length := len(payload)
	switch {
	case length <= 125:
		hdr[1] |= byte(length)
	case length <= 0xffff:
		hdr[1] |= 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(length))
		n = 4
	default:
		hdr[1] |= 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(length))
		n = 10
	}
	var mask [4]byte
	if _, err := cryptoRandRead(mask[:]); err != nil {
		return fmt.Errorf("websocket: mask: %w", err)
	}
	copy(hdr[n:], mask[:])
	n += 4

	// Mask into a fresh buffer: the caller may reuse payload immediately, and
	// the SDK copies the write before returning.
	masked := make([]byte, length)
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	if err := writeAll(ctx, c.s, hdr[:n]); err != nil {
		return err
	}
	return writeAll(ctx, c.s, masked)
}

func writeAll(ctx context.Context, s Stream, p []byte) error {
	for len(p) > 0 {
		n, err := s.Write(ctx, p)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// maxHeadBytes bounds the whole HTTP response head, including the status line.
const maxHeadBytes = 8 << 10

// readResponseHead reads the HTTP response head with a hard byte cap, so a
// hostile peer cannot grow memory by withholding CRLF.
func readResponseHead(br *bufio.Reader) (int, map[string]string, error) {
	line, err := readBoundedLine(br, maxHeadBytes)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/1.") {
		return 0, nil, fmt.Errorf("%w: bad status line", ErrHandshake)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, nil, fmt.Errorf("%w: bad status line", ErrHandshake)
	}
	headers := map[string]string{}
	total := len(line)
	for {
		line, err := readBoundedLine(br, maxHeadBytes-total)
		if err != nil {
			return 0, nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
		total += len(line) + 2
		if total > maxHeadBytes {
			return 0, nil, fmt.Errorf("%w: response head too large", ErrHandshake)
		}
		if line == "" {
			return status, headers, nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return 0, nil, fmt.Errorf("%w: bad header", ErrHandshake)
		}
		key := strings.ToLower(strings.TrimSpace(name))
		val := strings.TrimSpace(value)
		if prev, seen := headers[key]; seen {
			headers[key] = prev + "," + val
		} else {
			headers[key] = val
		}
	}
}

// readBoundedLine reads one CRLF-terminated line without its terminator,
// failing once the line exceeds limit bytes. It reads byte by byte so the bound
// applies while the line is being read, not after it was already buffered.
func readBoundedLine(br *bufio.Reader, limit int) (string, error) {
	var buf []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		buf = append(buf, b)
		if len(buf) > limit {
			return "", errors.New("line too long")
		}
		if n := len(buf); n >= 2 && buf[n-2] == '\r' && buf[n-1] == '\n' {
			return string(buf[:n-2]), nil
		}
	}
}

// headerHasToken reports whether a comma-separated header carries token.
func headerHasToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// acceptKey is the RFC 6455 handshake response value.
func acceptKey(key string) string {
	h := sha1.New() //nolint:gosec // RFC 6455 defines SHA-1 here; it is not a security primitive
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func parseClose(payload []byte) (uint16, string) {
	if len(payload) < 2 {
		return CloseNormal, ""
	}
	return binary.BigEndian.Uint16(payload[:2]), string(payload[2:])
}

// CloseError is a peer's close frame.
type CloseError struct {
	Code   uint16
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("websocket: peer closed with %d", e.Code)
	}
	return fmt.Sprintf("websocket: peer closed with %d: %s", e.Code, e.Reason)
}
