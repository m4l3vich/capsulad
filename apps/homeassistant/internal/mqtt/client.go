package mqtt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Conn is a byte stream to a broker: outbound TCP or TLS from
// capsulad.OpenTCP, or any equivalent stream. Reads are serialized (one
// logical reader) and a stalled Read must return once Close is called, so the
// session can always be torn down.
type Conn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// DeadlineConn is an optional Conn capability: bounding one operation. The SDK
// stream adapter implements it, which is what keeps CONNECT/CONNACK, DISCONNECT
// and an individual write from waiting forever on a peer that accepts a
// connection and then stops reading. A Conn without it is used unbounded, so its
// caller owns the timeouts.
//
// The two directions are separate on purpose. A read deadline set for a write
// would abort the reader's blocked Read on a quiet but healthy broker, which is
// exactly the connection the keepalive is meant to tolerate.
type DeadlineConn interface {
	// SetReadDeadline bounds subsequent reads; the zero time clears it.
	SetReadDeadline(deadline time.Time) error
	// SetWriteDeadline bounds subsequent writes; the zero time clears it.
	SetWriteDeadline(deadline time.Time) error
}

// Message is one PUBLISH in either direction.
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// Config configures one MQTT session.
type Config struct {
	// ClientID is the broker client identifier. Empty selects a generated one.
	ClientID string
	// Username and Password authenticate the session; HasUsername/HasPassword
	// distinguish "absent" from "empty", which MQTT encodes differently.
	Username    string
	Password    string
	HasUsername bool
	HasPassword bool
	// KeepAlive is the CONNECT keepalive and the liveness budget. Zero selects
	// DefaultKeepAlive.
	KeepAlive time.Duration
	// Will is the last will, published by the broker when the session ends
	// without a DISCONNECT. It is the crash path for the availability topic.
	Will *Message
	// ConnectTimeout bounds the CONNECT write and the CONNACK read. Zero
	// selects DefaultConnectTimeout. It applies when the Conn implements
	// DeadlineConn.
	ConnectTimeout time.Duration
	// MaxInflight bounds unacknowledged QoS 1 publishes. Zero selects
	// DefaultMaxInflight.
	MaxInflight int
	// QueueDepth bounds inbound messages waiting for the consumer. Zero selects
	// DefaultQueueDepth. When the consumer falls behind the oldest message is
	// dropped and counted, never silently interleaved.
	QueueDepth int
}

// Session defaults.
const (
	DefaultKeepAlive      = 30 * time.Second
	DefaultConnectTimeout = 10 * time.Second
	DefaultMaxInflight    = 32
	DefaultQueueDepth     = 64
	// disconnectTimeout bounds the DISCONNECT write. A broker that stopped
	// reading must not hold up an app that is stopping.
	disconnectTimeout = 2 * time.Second
	// controlWriteTimeout bounds a control write the client originates itself
	// (PUBACK, PINGREQ, PINGRESP), which has no caller context to inherit.
	controlWriteTimeout = 5 * time.Second
)

// Stats reports one session's counters.
type Stats struct {
	// Sent, Received, Dropped count PUBLISH packets in each direction and
	// inbound messages dropped because the consumer fell behind.
	Sent, Received, Dropped uint64
	// Pending is the number of unacknowledged QoS 1 publishes.
	Pending int
}

// Client is one MQTT session over a Conn. It is not safe to reuse a Client
// after Run returns: reconnect with a new Client over a new Conn.
type Client struct {
	conn Conn
	cfg  Config

	// writeSem serializes the write path (capacity 1). It is a channel rather
	// than a mutex because acquiring it must honour a context: a peer that stops
	// draining can hold the write path, and the teardown writes (the offline
	// marker, DISCONNECT) must fail visibly instead of queueing behind it.
	writeSem chan struct{}
	nextID   uint16

	inboxMu     sync.Mutex
	inboxClosed bool

	ackMu        sync.Mutex
	ackWait      map[uint16]chan error
	subWait      map[uint16]chan error
	sessionEnded chan struct{}
	endOnce      sync.Once
	endErr       atomic.Value // errBox

	inbox       chan Message
	dropped     atomic.Uint64
	sent        atomic.Uint64
	received    atomic.Uint64
	lastInbound atomic.Int64 // unix nanos of the last inbound packet
}

// NewClient returns a session over conn. Nothing is written until Connect.
func NewClient(conn Conn, cfg Config) *Client {
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = DefaultKeepAlive
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = DefaultMaxInflight
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.ClientID == "" {
		cfg.ClientID = fmt.Sprintf("capsulad-%d", time.Now().UnixNano())
	}
	c := &Client{
		conn:         conn,
		cfg:          cfg,
		writeSem:     make(chan struct{}, 1),
		ackWait:      map[uint16]chan error{},
		subWait:      map[uint16]chan error{},
		sessionEnded: make(chan struct{}),
		inbox:        make(chan Message, cfg.QueueDepth),
	}
	c.endErr.Store(errBox{err: ErrSessionEnded})
	c.lastInbound.Store(time.Now().UnixNano())
	return c
}

// Messages delivers inbound PUBLISH messages. It is closed when the session
// ends.
func (c *Client) Messages() <-chan Message { return c.inbox }

// Stats returns the session counters.
func (c *Client) Stats() Stats {
	c.ackMu.Lock()
	pending := len(c.ackWait)
	c.ackMu.Unlock()
	return Stats{
		Sent:     c.sent.Load(),
		Received: c.received.Load(),
		Dropped:  c.dropped.Load(),
		Pending:  pending,
	}
}

// errBox wraps the terminal error so atomic.Value always sees one concrete
// type: a bare error would panic when a later path stores a different error
// implementation.
type errBox struct{ err error }

// Err reports why the session ended. It is ErrSessionEnded while the session
// is alive. Callers should read it after Run returns.
func (c *Client) Err() error {
	box, _ := c.endErr.Load().(errBox)
	return box.err
}

// Connect sends CONNECT and waits for a CONNACK that accepted the session. When
// the stream supports deadlines, the whole exchange is bounded by
// Config.ConnectTimeout: a peer that accepts the connection and never answers
// fails here instead of wedging the session before its keepalive watchdog
// exists.
func (c *Client) Connect() error {
	connectDeadline := time.Now().Add(c.cfg.ConnectTimeout)
	if bounded, ok := c.conn.(DeadlineConn); ok {
		// Clear both directions on every return path, registered before either
		// setter so a failed set cannot leave a stale deadline behind.
		defer func() {
			_ = bounded.SetReadDeadline(time.Time{})
			_ = bounded.SetWriteDeadline(time.Time{})
		}()
		if err := bounded.SetReadDeadline(connectDeadline); err != nil {
			return fmt.Errorf("mqtt: bound CONNECT: %w", err)
		}
		if err := bounded.SetWriteDeadline(connectDeadline); err != nil {
			return fmt.Errorf("mqtt: bound CONNECT: %w", err)
		}
	}
	keepAlive := uint16(c.cfg.KeepAlive / time.Second)
	packet, err := encodeConnect(connectOptions{
		clientID:     c.cfg.ClientID,
		cleanSession: true,
		keepAlive:    keepAlive,
		username:     c.cfg.Username,
		hasUsername:  c.cfg.HasUsername,
		password:     c.cfg.Password,
		hasPassword:  c.cfg.HasPassword,
		will:         c.cfg.Will,
	})
	if err != nil {
		return err
	}
	connectCtx, cancelConnect := context.WithDeadline(context.Background(), connectDeadline)
	defer cancelConnect()
	if err := c.writePacket(connectCtx, packet, connectDeadline); err != nil {
		return fmt.Errorf("mqtt: send CONNECT: %w", err)
	}
	reply, err := readPacket(c.conn)
	if err != nil {
		return fmt.Errorf("mqtt: read CONNACK: %w", err)
	}
	if reply.packetType != packetTypeConnack {
		return fmt.Errorf("%w: expected CONNACK, got %s", ErrProtocol, packetName(reply.packetType))
	}
	if err := connackError(reply.returnCode); err != nil {
		return err
	}
	return nil
}

// Run reads packets until the session ends: ctx is cancelled, the broker
// closes the stream, the broker goes silent past the keepalive budget, or a
// protocol error arrives. It closes the client's channels exactly once and
// unblocks every pending publish or subscribe.
func (c *Client) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop() }()

	// Ping when the connection has been quiet for half the keepalive, so a
	// healthy broker is never disconnected by its own liveness timer.
	pingInterval := c.cfg.KeepAlive / 2
	if pingInterval <= 0 {
		pingInterval = time.Second
	}
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.finish(ctx.Err())
			_ = c.conn.Close()
			<-readErr
			return c.Err()
		case err := <-readErr:
			if err == nil {
				err = ErrSessionEnded
			}
			c.finish(err)
			_ = c.conn.Close()
			return c.Err()
		case now := <-ticker.C:
			silent := time.Duration(now.UnixNano() - c.lastInbound.Load())
			if silent > c.cfg.KeepAlive+c.cfg.KeepAlive/2 {
				err := fmt.Errorf("mqtt: broker silent for %s (keepalive %s)", silent.Round(time.Millisecond), c.cfg.KeepAlive)
				c.finish(err)
				_ = c.conn.Close()
				<-readErr
				return c.Err()
			}
			if silent >= pingInterval {
				if err := c.Ping(); err != nil {
					c.finish(err)
					_ = c.conn.Close()
					<-readErr
					return c.Err()
				}
			}
		}
	}
}

// readLoop decodes packets, answers the broker's own tracking traffic and
// queues inbound publications.
func (c *Client) readLoop() error {
	for {
		pkt, err := readPacket(c.conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("mqtt: broker closed the connection")
			}
			return err
		}
		c.lastInbound.Store(time.Now().UnixNano())
		switch pkt.packetType {
		case packetTypePublish:
			c.received.Add(1)
			if pkt.qos == 1 {
				if ack, err := encodePacketID(packetTypePuback, pkt.packetID); err == nil {
					if err := c.writeControl(ack); err != nil {
						return fmt.Errorf("mqtt: send PUBACK: %w", err)
					}
				}
			} else if pkt.qos == 2 {
				// This bridge never subscribes above QoS 1, so a QoS 2 delivery
				// means the broker is misbehaving; refusing is better than a
				// half-implemented flow.
				return fmt.Errorf("%w: inbound QoS 2 publish is not supported", ErrProtocol)
			}
			c.deliver(Message{Topic: pkt.topic, Payload: pkt.payload, QoS: pkt.qos, Retain: pkt.retain})
		case packetTypePuback:
			c.resolve(c.ackWait, pkt.packetID, nil)
		case packetTypeSuback:
			var err error
			for _, code := range pkt.subackCodes {
				if code == subackFailure {
					err = fmt.Errorf("mqtt: broker refused a subscription")
				}
			}
			c.resolve(c.subWait, pkt.packetID, err)
		case packetTypePingresp:
			// liveness is tracked by lastInbound
		case packetTypePingreq:
			reply, err := frame(packetTypePingresp, 0, nil)
			if err != nil {
				return err
			}
			if err := c.writeControl(reply); err != nil {
				return fmt.Errorf("mqtt: send PINGRESP: %w", err)
			}
		default:
			return fmt.Errorf("%w: unexpected packet %s", ErrProtocol, packetName(pkt.packetType))
		}
	}
}

// deliver queues one inbound message, dropping and counting the oldest when
// the consumer is behind. Publishing commands are low-rate; a full queue means
// the consumer is wedged, and a bounded lossy queue beats stalling the read
// loop (which would kill the keepalive).
//
// The queue is closed by finish, which runs on another goroutine, so the send
// and the close are serialized: a send racing a teardown must not panic on a
// closed channel.
func (c *Client) deliver(msg Message) {
	c.inboxMu.Lock()
	defer c.inboxMu.Unlock()
	if c.inboxClosed {
		return
	}
	for {
		select {
		case c.inbox <- msg:
			return
		default:
		}
		select {
		case <-c.inbox:
			c.dropped.Add(1)
		default:
		}
	}
}

// resolve completes a waiting publish or subscribe.
func (c *Client) resolve(waiters map[uint16]chan error, packetID uint16, err error) {
	c.ackMu.Lock()
	ch, ok := waiters[packetID]
	if ok {
		delete(waiters, packetID)
	}
	c.ackMu.Unlock()
	if ok {
		ch <- err
	}
}

// finish records the terminal error, closes the inbox and fails every waiter.
func (c *Client) finish(err error) {
	c.endOnce.Do(func() {
		c.endErr.Store(errBox{err: err})
		close(c.sessionEnded)
		c.inboxMu.Lock()
		if !c.inboxClosed {
			c.inboxClosed = true
			close(c.inbox)
		}
		c.inboxMu.Unlock()
		c.ackMu.Lock()
		for id, ch := range c.ackWait {
			delete(c.ackWait, id)
			ch <- err
		}
		for id, ch := range c.subWait {
			delete(c.subWait, id)
			ch <- err
		}
		c.ackMu.Unlock()
	})
}

// writeControl sends one packet the client originates itself (a CONNECT, a
// PUBACK, a keepalive) over a bounded background context.
func (c *Client) writeControl(packet []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), controlWriteTimeout)
	defer cancel()
	return c.writePacket(ctx, packet, time.Now().Add(controlWriteTimeout))
}

// writePacket writes one packet. Both the acquisition of the single write path
// and the potential per-packet deadline are bounded: a wedged writer holds the
// path, and every other writer fails with a visible error when its own budget
// expires instead of queueing behind it forever. The deadline is installed and
// cleared while the write path is held, so it applies to exactly this packet and
// never leaks onto a concurrent write or the reader.
func (c *Client) writePacket(ctx context.Context, packet []byte, deadline time.Time) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-c.sessionEnded:
		return c.Err()
	default:
	}
	select {
	case c.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.sessionEnded:
		return c.Err()
	}
	defer func() { <-c.writeSem }()

	if bounded, ok := c.conn.(DeadlineConn); ok && !deadline.IsZero() {
		if err := bounded.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer func() { _ = bounded.SetWriteDeadline(time.Time{}) }()
	}
	for len(packet) > 0 {
		n, err := c.conn.Write(packet)
		if err != nil {
			return err
		}
		if n <= 0 {
			return fmt.Errorf("mqtt: short write of 0 bytes")
		}
		packet = packet[n:]
	}
	return nil
}

// reserve allocates a packet id and registers its waiter under one lock, so
// two concurrent publishes can never share an id. A full in-flight window is
// an explicit error, never a silent drop.
func (c *Client) reserve(waiters map[uint16]chan error) (uint16, chan error, error) {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	if len(waiters) >= c.cfg.MaxInflight {
		return 0, nil, fmt.Errorf("mqtt: in-flight window of %d is full", c.cfg.MaxInflight)
	}
	for range 65535 {
		c.nextID++
		if c.nextID == 0 {
			c.nextID++
		}
		if _, used := c.ackWait[c.nextID]; used {
			continue
		}
		if _, used := c.subWait[c.nextID]; used {
			continue
		}
		ch := make(chan error, 1)
		waiters[c.nextID] = ch
		return c.nextID, ch, nil
	}
	return 0, nil, fmt.Errorf("%w: no free packet id", ErrProtocol)
}

// deadlineFrom renders a context deadline as an absolute time, or the zero time
// when the context has none.
func deadlineFrom(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Time{}
}

// release abandons a reservation without an acknowledgement.
func (c *Client) release(waiters map[uint16]chan error, id uint16) {
	if id == 0 {
		return
	}
	c.ackMu.Lock()
	delete(waiters, id)
	c.ackMu.Unlock()
}

// Publish sends one message and, for QoS 1, waits for its PUBACK.
func (c *Client) Publish(ctx context.Context, msg Message) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.sessionEnded:
		return c.Err()
	default:
	}

	var (
		id      uint16
		ackChan chan error
	)
	if msg.QoS > 0 {
		var err error
		if id, ackChan, err = c.reserve(c.ackWait); err != nil {
			return err
		}
	}
	packet, err := encodePublish(msg, id)
	if err != nil {
		c.release(c.ackWait, id)
		return err
	}
	if err := c.writePacket(ctx, packet, deadlineFrom(ctx)); err != nil {
		c.release(c.ackWait, id)
		return err
	}
	c.sent.Add(1)
	if ackChan == nil {
		return nil
	}
	select {
	case err := <-ackChan:
		return err
	case <-ctx.Done():
		c.release(c.ackWait, id)
		return ctx.Err()
	case <-c.sessionEnded:
		return c.Err()
	}
}

// Subscribe registers topic filters at QoS 0 and waits for the SUBACK.
func (c *Client) Subscribe(ctx context.Context, filters ...string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id, waiter, err := c.reserve(c.subWait)
	if err != nil {
		return err
	}
	packet, err := encodeSubscribe(id, filters)
	if err != nil {
		c.release(c.subWait, id)
		return err
	}
	if err := c.writePacket(ctx, packet, deadlineFrom(ctx)); err != nil {
		c.release(c.subWait, id)
		return err
	}
	select {
	case err := <-waiter:
		return err
	case <-ctx.Done():
		c.release(c.subWait, id)
		return ctx.Err()
	case <-c.sessionEnded:
		return c.Err()
	}
}

// Ping sends a PINGREQ. Callers normally do not need it: Run keeps the
// connection alive on its own.
func (c *Client) Ping() error {
	packet, err := frame(packetTypePingreq, 0, nil)
	if err != nil {
		return err
	}
	if err := c.writeControl(packet); err != nil {
		return fmt.Errorf("mqtt: send PINGREQ: %w", err)
	}
	return nil
}

// Disconnect sends DISCONNECT and closes the stream. A clean DISCONNECT makes
// the broker discard the last will, so a shutdown that must show the device
// offline publishes that state (retained, QoS 1) before calling Disconnect.
func (c *Client) Disconnect() error {
	select {
	case <-c.sessionEnded:
		return c.Err()
	default:
	}
	packet, err := frame(packetTypeDisconnect, 0, nil)
	if err != nil {
		return err
	}
	// The acquisition of the write path is bounded by the same budget as the
	// write itself: a wedged writer must not queue a stopping app behind it.
	disconnectCtx, cancelDisconnect := context.WithTimeout(context.Background(), disconnectTimeout)
	defer cancelDisconnect()
	writeErr := c.writePacket(disconnectCtx, packet, time.Now().Add(disconnectTimeout))
	closeErr := c.conn.Close()
	c.finish(ErrDisconnected)
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil && !errors.Is(closeErr, io.EOF) {
		return closeErr
	}
	return nil
}
