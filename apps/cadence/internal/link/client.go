// Package link keeps the client's Cadence device link up: it dials through the
// shared SDK, performs the cadence.v2 hello/welcome handshake, reports that
// this device advertises no capabilities, then reads envelopes until the link
// drops and reconnects with jittered backoff.
//
// The client is deliberately a voice-only device: it declares no capability
// descriptor, so the server pushes no config and no command, and its voice
// loop uses the server's default reply output. That is a complete, honest
// device for a speaker that only listens and speaks; anything else would need a
// capability implementation this app does not have.
package link

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/websocket"
	"capsulad/apps/cadence/internal/wire"
)

// Handler receives the link's lifecycle and every decoded envelope. Both run
// on the link's reader goroutine and must return promptly; a slow handler
// stalls the link.
type Handler interface {
	LinkChanged(up bool)
	Message(env *cadencev2.Envelope)
}

// ErrOffline means no link is up; the turn's send fails and the turn ends.
var ErrOffline = errors.New("link: not connected")

const (
	writeWait    = 5 * time.Second
	helloWait    = 10 * time.Second
	pingInterval = 20 * time.Second
	pongTimeout  = 10 * time.Second
	// protocol is the only device-link protocol this client speaks.
	protocol = 2
)

// Client owns the link. One Client serves one device.
type Client struct {
	dialer  Dialer
	log     *log.Logger
	handler Handler
	version string
	board   string
	report  *cadencev2.CapabilityReport
	hash    []byte

	now func() time.Time
	rnd func() float64

	wake chan struct{}

	mu   sync.Mutex
	conn Conn
	// attempt cancels a dial or handshake in flight, so a settings change takes
	// effect immediately instead of after the connect deadline.
	attempt *linkAttempt
	// gen grows on every Wake; a link installed after a Wake is discarded.
	gen atomic.Uint64
}

type linkAttempt struct{ cancel context.CancelFunc }

// New builds the client. It computes the empty capability report and its hash
// once; both are immutable. version and board are advertised in Hello.
func New(d Dialer, logger *log.Logger, h Handler, version, board string) (*Client, error) {
	if logger == nil {
		logger = log.Default()
	}
	report := &cadencev2.CapabilityReport{}
	hash, err := wire.ReportHash(report)
	if err != nil {
		return nil, err
	}
	report.Hash = hash
	return &Client{
		dialer: d, log: logger, handler: h, version: version, board: board,
		report: report, hash: hash,
		now:  time.Now,
		rnd:  rand.Float64,
		wake: make(chan struct{}, 1),
	}, nil
}

// Send writes one envelope on the current link. It is safe for concurrent use
// with the reader; a write blocks the caller, never the read path.
func (c *Client) Send(env *cadencev2.Envelope) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrOffline
	}
	b, err := wire.Encode(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeWait)
	defer cancel()
	return conn.WriteBinary(ctx, b)
}

// Connected reports whether a link is up and past the handshake.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Wake forces the current link down so the next attempt uses the current
// configuration; it never blocks. A caller that changes settings calls it.
func (c *Client) Wake() {
	c.gen.Add(1)
	c.mu.Lock()
	conn, attempt := c.conn, c.attempt
	c.mu.Unlock()
	if attempt != nil {
		attempt.cancel()
	}
	if conn != nil {
		_ = conn.CloseNow()
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run keeps the link up until ctx is done. It returns nil on shutdown.
func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	refused := false
	unconfigured := false
	for {
		up, reason := c.session(ctx, &refused)
		if ctx.Err() != nil {
			return nil
		}
		if reason == "unconfigured" {
			if !unconfigured {
				// One line, not one per attempt: the operator has not set a
				// server yet, and the retry is silent.
				c.log.Printf("link_unconfigured: set server_url and device_token")
				unconfigured = true
			}
		} else {
			unconfigured = false
			attempt = nextAttempt(attempt, up)
			wait := delay(attempt, c.rnd)
			c.log.Printf("link_down reason=%s attempt=%d delay=%s", reason, attempt, wait)
			if !c.sleep(ctx, wait) {
				return nil
			}
			continue
		}
		// Unconfigured: wait for a settings change instead of spinning.
		if !c.sleep(ctx, maxDelay) {
			return nil
		}
	}
}

// sleep waits d, returning false when ctx ends. A Wake shortens the wait.
func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-c.wake:
		return true
	}
}

// session runs one link from dial to close and reports how long it was up and
// why it ended. refused carries the "token refused" streak so a revoked device
// logs once, not at every attempt.
func (c *Client) session(ctx context.Context, refused *bool) (up time.Duration, reason string) {
	gen := c.gen.Load()
	actx, cancel := context.WithCancel(ctx)
	a := &linkAttempt{cancel: cancel}
	c.mu.Lock()
	c.attempt = a
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.attempt == a {
			c.attempt = nil
		}
		c.mu.Unlock()
		cancel()
	}()

	conn, err := c.dialer.Dial(actx)
	switch {
	case errors.Is(err, ErrNoServer):
		return 0, "unconfigured"
	case errors.Is(err, websocket.ErrUnauthorized):
		if !*refused {
			c.log.Printf("link_unauthorized: the device token was refused; check device_token")
			*refused = true
		}
		return 0, "unauthorized"
	case err != nil:
		return 0, "dial_failed"
	}
	*refused = false
	defer conn.CloseNow()

	if err := c.handshake(actx, conn); err != nil {
		return 0, "handshake_failed"
	}
	// A settings change during the dial/handshake invalidates this link before
	// it can carry a turn on the old server or token.
	if c.gen.Load() != gen {
		conn.CloseNow()
		return 0, "superseded"
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.handler.LinkChanged(true)
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		c.handler.LinkChanged(false)
	}()

	pongs := make(chan struct{}, 1)
	conn.SetPongHandler(func([]byte) {
		select {
		case pongs <- struct{}{}:
		default:
		}
	})
	upCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.keepalive(upCtx, conn, pongs)

	start := c.now()
	for {
		op, payload, err := conn.ReadMessage(upCtx)
		if err != nil {
			return c.now().Sub(start), closureReason(err)
		}
		if op != 0x2 {
			// The device link is binary-only; upstream closes a text frame
			// with 1007 rather than ignoring it.
			return c.now().Sub(start), "invalid"
		}
		env, derr := wire.Decode(payload)
		switch {
		case errors.Is(derr, wire.ErrUnknown):
			continue
		case derr != nil:
			return c.now().Sub(start), "invalid"
		}
		c.handler.Message(env)
	}
}

// closureReason is a fixed token for a log line; the transport's error text is
// never quoted (it can carry a URL or peer detail).
func closureReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "shutdown"
	default:
		return "closed"
	}
}

// handshake sends Hello, waits for Welcome, then sends this device's full
// (empty) capability report.
func (c *Client) handshake(ctx context.Context, conn Conn) error {
	hctx, cancel := context.WithTimeout(ctx, helloWait)
	defer cancel()

	hello := &cadencev2.Envelope{Body: &cadencev2.Envelope_Hello{Hello: &cadencev2.Hello{
		Protocol:         protocol,
		DaemonVersion:    c.version,
		Board:            c.board,
		CapabilitiesHash: c.hash,
	}}}
	if err := sendOn(hctx, conn, hello); err != nil {
		return err
	}
	for {
		op, payload, err := conn.ReadMessage(hctx)
		if err != nil {
			return err
		}
		if op != 0x2 {
			return errors.New("link: non-binary handshake frame")
		}
		env, derr := wire.Decode(payload)
		if errors.Is(derr, wire.ErrUnknown) {
			continue
		}
		if derr != nil {
			return derr
		}
		if w := env.GetWelcome(); w != nil {
			report := ""
			if w.GetSendFullReport() {
				report = " full"
			}
			c.log.Printf("link_up server=%q report=%s", w.GetServerVersion(), report)
			break
		}
	}
	return sendOn(hctx, conn, &cadencev2.Envelope{
		Body: &cadencev2.Envelope_CapabilityReport{CapabilityReport: c.report},
	})
}

func sendOn(ctx context.Context, conn Conn, env *cadencev2.Envelope) error {
	b, err := wire.Encode(env)
	if err != nil {
		return err
	}
	return conn.WriteBinary(ctx, b)
}

// keepalive pings the server and drops a link whose pong does not arrive. The
// server pings too; answering is handled by ReadMessage.
func (c *Client) keepalive(ctx context.Context, conn Conn, pongs <-chan struct{}) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pongTimeout)
		err := conn.WritePing(pctx, nil)
		cancel()
		if err != nil {
			_ = conn.CloseNow()
			return
		}
		timer := time.NewTimer(pongTimeout)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-pongs:
			timer.Stop()
		case <-timer.C:
			c.log.Printf("link_ping_timeout")
			_ = conn.CloseNow()
			return
		}
	}
}
