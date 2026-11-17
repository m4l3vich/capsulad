package capsulad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
)

// ListenerOptions configures a daemon-mediated listener. The daemon binds the
// socket and passes the descriptor over the inherited CAPSULAD_CONTROL_FD
// channel; the app never creates a listening socket itself. This is the only
// way to own a fixed inbound port: OpenUDP always binds an ephemeral port.
type ListenerOptions struct {
	// Protocol is "tcp" or "udp"; empty uses "tcp".
	Protocol string
	// Bind is the local unicast address; empty uses "127.0.0.1".
	Bind string
	// Port is 0 for an ephemeral port, or 1024..65535. Port 5353 and the Web UI
	// port are refused by the daemon.
	Port int
	// Backlog is the TCP accept queue (1..128); 0 uses the native default 64.
	Backlog int
	// JoinGroup joins an IPv4 multicast group on a receive-only UDP socket.
	// The daemon accepts it only for Protocol "udp" with Bind "0.0.0.0" and
	// Port 319 or 320 (the PTP event and general ports), which is what the
	// AirPlay PTP clock needs. Empty is an ordinary listener.
	JoinGroup string
}

func (o ListenerOptions) encode() ([]byte, error) {
	options := map[string]any{}
	switch o.Protocol {
	case "":
	case "tcp", "udp":
		options["protocol"] = o.Protocol
	default:
		return nil, fmt.Errorf("capsulad: ListenerOptions.Protocol %q must be \"tcp\" or \"udp\"", o.Protocol)
	}
	if o.Bind != "" {
		options["bind"] = o.Bind
	}
	if o.Port < 0 || o.Port > 65535 {
		return nil, errors.New("capsulad: ListenerOptions.Port must be 0..65535")
	}
	if o.Port > 0 {
		options["port"] = o.Port
	}
	if o.Backlog < 0 || o.Backlog > 128 {
		return nil, errors.New("capsulad: ListenerOptions.Backlog must be 0 or 1..128")
	}
	if o.Backlog > 0 {
		options["backlog"] = o.Backlog
	}
	if o.JoinGroup != "" {
		if o.Protocol != "udp" {
			return nil, errors.New("capsulad: ListenerOptions.JoinGroup requires Protocol \"udp\"")
		}
		options["join_group"] = o.JoinGroup
	}
	return json.Marshal(options)
}

// ListenerInfo is the bound address reported by the daemon plus the local
// closed flag. Port is the actual bound port, so an ephemeral request is
// resolved here.
type ListenerInfo struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Closed   bool   `json:"closed"`
}

// Listener is one daemon-allocated TCP or UDP listener. Accept is valid only
// for TCP; Receive/Send only for UDP. Close unblocks pending calls. A context
// deadline leaves the listener usable (the native per-call timeout reports
// TIMEOUT); only an explicit cancellation closes it.
type Listener struct {
	mu     sync.Mutex
	handle uint64
}

// OpenListener loads the shared engine and asks the daemon for a bound
// listener. Without a usable control channel it fails with
// CAPSULAD_STATUS_UNSUPPORTED (visible as a *StatusError with NativeStatus).
func OpenListener(ctx context.Context, env Env, opts ListenerOptions) (*Listener, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLibrary(env.Library); err != nil {
		return nil, err
	}
	options, err := opts.encode()
	if err != nil {
		return nil, err
	}
	handle, _, status, detail := listenerOpenNative(options, timeoutMillis(ctx, 0))
	if err := nativeStatusError("open listener", status, detail); err != nil {
		if handle != 0 {
			_ = listenerCloseNative(handle)
		}
		return nil, err
	}
	listener := &Listener{handle: handle}
	runtime.SetFinalizer(listener, func(dead *Listener) { _ = dead.Close() })
	return listener, nil
}

// Close releases the daemon-allocated listener and unblocks pending calls. It
// is idempotent.
func (l *Listener) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	handle := l.handle
	l.handle = 0
	l.mu.Unlock()
	if handle == 0 {
		return nil
	}
	runtime.SetFinalizer(l, nil)
	return listenerCloseNative(handle)
}

// Handle returns the native handle, or 0 after Close.
func (l *Listener) Handle() uint64 { return l.value() }

func (l *Listener) value() uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.handle
}

// Accept waits for one TCP connection and returns a normal TCPStream plus the
// connected peer. A deadline returns a TIMEOUT status and leaves the listener
// usable; explicit cancellation closes it.
func (l *Listener) Accept(ctx context.Context) (*TCPStream, *UDPAddr, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if l.value() == 0 {
		return nil, nil, ErrClosed
	}
	var handle uint64
	var peerJSON []byte
	err := l.runIO(ctx, "accept", func() (int32, string) {
		var status int32
		var detail string
		handle, peerJSON, status, detail = listenerAcceptNative(l.value(), ioTimeoutMS(ctx))
		return status, detail
	})
	if err != nil {
		return nil, nil, err
	}
	if handle == 0 {
		return nil, nil, &StatusError{Op: "accept", Code: statusInternal,
			Message: "listener returned a zero stream handle", NativeStatus: true}
	}
	peer, err := decodeUDPAddr(peerJSON)
	if err != nil {
		_ = streamCloseNative(handle)
		return nil, nil, err
	}
	stream := &TCPStream{handle: handle}
	runtime.SetFinalizer(stream, func(dead *TCPStream) { _ = dead.Close() })
	return stream, peer, nil
}

// Receive waits for one UDP datagram and its peer. maxLen 0 uses the native
// 65507-byte default; larger values are rejected natively with TOO_LARGE. A
// deadline returns a TIMEOUT status and leaves the listener usable.
func (l *Listener) Receive(ctx context.Context, maxLen int) ([]byte, *UDPAddr, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if maxLen < 0 {
		return nil, nil, errors.New("capsulad: Receive maxLen must not be negative")
	}
	if l.value() == 0 {
		return nil, nil, ErrClosed
	}
	var data, peerJSON []byte
	err := l.runIO(ctx, "receive", func() (int32, string) {
		var status int32
		var detail string
		data, peerJSON, status, detail = listenerRecvNative(l.value(), maxLen, ioTimeoutMS(ctx))
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

// Send sends one datagram to peer. Listeners have no connected peer, so peer is
// required and its Address must be an IP literal (the native side does not
// resolve names). A deadline returns a TIMEOUT status and leaves the listener
// usable.
func (l *Listener) Send(ctx context.Context, peer UDPAddr, data []byte) (int, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if l.value() == 0 {
		return 0, ErrClosed
	}
	peerJSON, err := json.Marshal(peer)
	if err != nil {
		return 0, fmt.Errorf("capsulad: encode listener peer: %w", err)
	}
	var sent int
	err = l.runIO(ctx, "send", func() (int32, string) {
		var status int32
		var detail string
		sent, status, detail = listenerSendNative(l.value(), peerJSON, data, ioTimeoutMS(ctx))
		return status, detail
	})
	if err != nil {
		return sent, err
	}
	return sent, nil
}

// Info returns the bound address and closed flag. Port is the actual port, so
// a listener opened with Port 0 exposes its ephemeral assignment here.
func (l *Listener) Info() (ListenerInfo, error) {
	handle := l.value()
	if handle == 0 {
		return ListenerInfo{}, ErrClosed
	}
	payload, status, detail := listenerInfoNative(handle)
	if err := nativeStatusError("listener info", status, detail); err != nil {
		return ListenerInfo{}, err
	}
	var info ListenerInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return ListenerInfo{}, fmt.Errorf("capsulad: decode listener info: %w", err)
	}
	return info, nil
}

func (l *Listener) runIO(ctx context.Context, op string, call func() (int32, string)) error {
	if l.value() == 0 {
		return ErrClosed
	}
	status, detail, aborted := watchAbort(ctx, func() { _ = l.Close() }, call)
	err := listenerStatusError(ctx, op, status, detail)
	if err != nil && aborted && (status == statusCancelled || status == statusClosed) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return err
}

func listenerStatusError(ctx context.Context, op string, status int32, detail string) error {
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
