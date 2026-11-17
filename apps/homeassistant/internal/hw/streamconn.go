package hw

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"capsulad/apps/homeassistant/internal/appcfg"
	"capsulad/apps/homeassistant/internal/mqtt"
	"capsulad/sdk/capsulad"
)

// streamConn adapts a capsulad TCP/TLS stream to the MQTT client's byte-stream
// seam. The SDK owns DNS, TCP and the verified TLS handshake; the app links no
// transport of its own, and there is no insecure mode to configure.
//
// Reads and writes are bound by the session's own I/O context, never by the
// runtime context that starts the session: the retained offline marker and the
// DISCONNECT are written while the runtime context is already cancelled, and
// they must still reach the broker. The I/O context is cancelled by the session
// teardown, after those writes.
//
// A per-operation deadline (mqtt.DeadlineConn) turns into a bounded context for
// that call, so CONNECT/CONNACK and DISCONNECT cannot wait forever.
type streamConn struct {
	stream *capsulad.TCPStream
	ctx    context.Context

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
}

// SetReadDeadline implements mqtt.DeadlineConn for reads. A zero time clears it.
func (c *streamConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.mu.Unlock()
	return nil
}

// SetWriteDeadline implements mqtt.DeadlineConn for writes. A zero time clears
// it. The two directions are independent: a write deadline must never abort the
// reader's blocked Read on an otherwise healthy connection.
func (c *streamConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *streamConn) readContext() (context.Context, func()) {
	return c.operationContext(false)
}

func (c *streamConn) writeContext() (context.Context, func()) {
	return c.operationContext(true)
}

func (c *streamConn) operationContext(write bool) (context.Context, func()) {
	c.mu.Lock()
	deadline := c.readDeadline
	if write {
		deadline = c.writeDeadline
	}
	c.mu.Unlock()
	if deadline.IsZero() {
		return c.ctx, func() {}
	}
	ctx, cancel := context.WithDeadline(c.ctx, deadline)
	return ctx, cancel
}

func (c *streamConn) Read(p []byte) (int, error) {
	ctx, cancel := c.readContext()
	defer cancel()
	n, err := c.stream.Read(ctx, p)
	return n, c.mapError(ctx, err)
}

func (c *streamConn) Write(p []byte) (int, error) {
	ctx, cancel := c.writeContext()
	defer cancel()
	n, err := c.stream.Write(ctx, p)
	return n, c.mapError(ctx, err)
}

func (c *streamConn) Close() error {
	return c.stream.Close()
}

// mapError distinguishes the three ways an operation can end early: the
// session's I/O context was cancelled (teardown), the operation deadline
// expired (a wedged peer) and a transport failure. Reporting a deadline as a
// plain cancellation would hide a broker that never answers.
func (c *streamConn) mapError(ctx context.Context, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	if ctxErr := c.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return fmt.Errorf("mqtt stream: %w", context.DeadlineExceeded)
	}
	return err
}

// dialBroker opens the MQTT stream through the shared SDK. The configured roots
// mode and CA bundle are passed through unchanged: verification is never
// disabled or narrowed beyond what the administrator configured.
//
// dialCtx bounds DNS, TCP and TLS (a stop during dialling must not wait for the
// native 30 s default); ioCtx bounds the session's reads and writes and outlives
// dialCtx, because the session's final writes happen during teardown.
func dialBroker(dialCtx, ioCtx context.Context, env capsulad.Env, cfg appcfg.Config) (mqtt.Conn, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("no broker host is configured")
	}
	options := capsulad.TCPOptions{
		TLS:        cfg.TLS,
		Roots:      cfg.TLSRoots,
		ServerName: cfg.TLSServerName,
	}
	if cfg.CAPEM != "" {
		options.CAPEM = []byte(cfg.CAPEM)
	}
	stream, err := capsulad.OpenTCP(dialCtx, env, cfg.Host, uint16(cfg.Port), options)
	if err != nil {
		return nil, fmt.Errorf("dial broker %s (port %d): %w", cfg.Host, cfg.Port, err)
	}
	return &streamConn{stream: stream, ctx: ioCtx}, nil
}
