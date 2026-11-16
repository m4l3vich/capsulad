package capsulad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// UDPAddr is one datagram peer: an IP literal plus a port. It is also the JSON
// shape used by the native peer_json parameters, which are not DNS-resolved.
type UDPAddr struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// String renders host:port for diagnostics. IPv6 literals are bracketed; it
// deliberately avoids the net package so UDP users do not link a second
// transport stack into the app.
func (a UDPAddr) String() string {
	if strings.Contains(a.Address, ":") {
		return "[" + a.Address + "]:" + strconv.Itoa(a.Port)
	}
	return a.Address + ":" + strconv.Itoa(a.Port)
}

// UDPOptions configures the local side of a UDP socket. The zero value binds
// 0.0.0.0:0 (an ephemeral port on every interface). Outbound sockets always
// bind an ephemeral local port; a fixed inbound port belongs to OpenListener,
// which the daemon authorizes.
type UDPOptions struct {
	// Bind is the local address ("0.0.0.0", "::", or an interface address).
	// It must not embed a port ("127.0.0.1:5353" is refused): the bound port
	// is always ephemeral.
	Bind string
	// Port must be 0: udp_open only ever binds an ephemeral local port. A
	// fixed inbound UDP port needs OpenListener with ListenerOptions{Protocol:
	// "udp", Port: n} so the daemon applies its listener policy.
	Port int
}

func (o UDPOptions) encode() ([]byte, error) {
	if o.Port < 0 || o.Port > 65535 {
		return nil, errors.New("capsulad: UDPOptions.Port must be 0..65535")
	}
	if o.Port > 0 {
		return nil, fmt.Errorf("capsulad: UDPOptions.Port must be 0 (udp_open binds only an ephemeral local port), not %d; a fixed inbound UDP port needs OpenListener with ListenerOptions{Protocol: \"udp\", Port: %d} so the daemon applies its listener policy", o.Port, o.Port)
	}
	options := map[string]any{}
	if o.Bind != "" {
		options["bind"] = o.Bind
	}
	return json.Marshal(options)
}

// UDPConn is one UDP socket, connected or unconnected. Send and Receive are
// safe for concurrent use; Close unblocks a pending receive. A context
// deadline leaves the socket usable (the native per-call timeout reports
// TIMEOUT); only an explicit cancellation closes it.
type UDPConn struct {
	mu     sync.Mutex
	handle uint64
}

// OpenUDP loads the shared engine and opens a UDP socket. A non-empty host and
// non-zero port connect the socket to that peer (host may be a DNS name);
// empty/0 leaves it unconnected so every Send must name a peer, and those
// peers must be IP literals. A context deadline bounds resolution, bind and
// connect (no deadline uses the native 30 s default); cancellation without a
// deadline is not observed once the native call has started.
func OpenUDP(ctx context.Context, env Env, host string, port uint16, opts UDPOptions) (*UDPConn, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if (host == "") != (port == 0) {
		return nil, errors.New("capsulad: OpenUDP needs both host and port, or neither")
	}
	// Validate the options before loading the engine so a local misuse (a
	// fixed bind port) is reported without depending on the shared library.
	options, err := opts.encode()
	if err != nil {
		return nil, err
	}
	if err := ensureLibrary(env.Library); err != nil {
		return nil, err
	}
	handle, err := udpOpenNative(host, port, options, timeoutMillis(ctx, 0))
	if err != nil {
		return nil, err
	}
	conn := &UDPConn{handle: handle}
	runtime.SetFinalizer(conn, func(dead *UDPConn) { _ = dead.Close() })
	return conn, nil
}

// Close releases the socket and unblocks a pending Receive. It is idempotent.
func (c *UDPConn) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	handle := c.handle
	c.handle = 0
	c.mu.Unlock()
	if handle == 0 {
		return nil
	}
	runtime.SetFinalizer(c, nil)
	return udpCloseNative(handle)
}

// Handle returns the native handle, or 0 after Close.
func (c *UDPConn) Handle() uint64 { return c.value() }

func (c *UDPConn) value() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handle
}

// Send sends one datagram. A nil peer uses the connected peer and fails with a
// NOT_CONNECTED status on an unconnected socket; peer.Address must be an IP
// literal, because the native side does not resolve names. The returned count
// is the bytes sent (0..len(data)). A deadline returns a TIMEOUT status and
// leaves the socket usable.
func (c *UDPConn) Send(ctx context.Context, peer *UDPAddr, data []byte) (int, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	handle := c.value()
	if handle == 0 {
		return 0, ErrClosed
	}
	var peerJSON []byte
	if peer != nil {
		var err error
		peerJSON, err = json.Marshal(peer)
		if err != nil {
			return 0, fmt.Errorf("capsulad: encode udp peer: %w", err)
		}
	}
	var sent int
	err := c.runIO(ctx, "send udp", func() (int32, string) {
		var status int32
		var detail string
		sent, status, detail = udpSendNative(c.value(), peerJSON, data, ioTimeoutMS(ctx))
		return status, detail
	})
	if err != nil {
		return sent, err
	}
	return sent, nil
}

// Receive waits for one datagram. maxLen 0 uses the native 65507-byte default;
// larger values are rejected natively with TOO_LARGE. The returned peer is nil
// only if the library omitted it. A deadline returns a TIMEOUT status and
// leaves the socket usable; explicit cancellation closes it.
func (c *UDPConn) Receive(ctx context.Context, maxLen int) ([]byte, *UDPAddr, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if maxLen < 0 {
		return nil, nil, errors.New("capsulad: Receive maxLen must not be negative")
	}
	handle := c.value()
	if handle == 0 {
		return nil, nil, ErrClosed
	}
	var data, peerJSON []byte
	err := c.runIO(ctx, "receive udp", func() (int32, string) {
		var status int32
		var detail string
		data, peerJSON, status, detail = udpRecvNative(c.value(), maxLen, ioTimeoutMS(ctx))
		return status, detail
	})
	if err != nil {
		return nil, nil, err
	}
	peer, err := decodeUDPAddr(peerJSON)
	if err != nil {
		return nil, nil, err
	}
	return data, peer, nil
}

func (c *UDPConn) runIO(ctx context.Context, op string, call func() (int32, string)) error {
	if c.value() == 0 {
		return ErrClosed
	}
	status, detail, aborted := watchAbort(ctx, func() { _ = c.Close() }, call)
	err := udpStatusError(ctx, op, status, detail)
	if err != nil && aborted && (status == statusCancelled || status == statusClosed) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return err
}

func udpStatusError(ctx context.Context, op string, status int32, detail string) error {
	switch status {
	case statusOK:
		return nil
	case statusCancelled, statusClosed:
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrClosed
	}
	return nativeStatusError(op, status, detail)
}

func decodeUDPAddr(payload []byte) (*UDPAddr, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var peer UDPAddr
	if err := json.Unmarshal(payload, &peer); err != nil {
		return nil, fmt.Errorf("capsulad: decode udp peer: %w", err)
	}
	return &peer, nil
}
