package capsulad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// ShutdownHow selects which direction of a TCP stream to half-close.
type ShutdownHow int32

const (
	// ShutdownWrite sends FIN (or TLS close_notify); later writes fail.
	ShutdownWrite ShutdownHow = 0
	// ShutdownRead makes further reads return io.EOF.
	ShutdownRead ShutdownHow = 1
	// ShutdownBoth performs both half-closes.
	ShutdownBoth ShutdownHow = 2
)

// TCPOptions configures one outbound TCP stream. The zero value opens plain
// TCP with the native defaults (system TLS roots when TLS is enabled, TCP
// no-delay on).
type TCPOptions struct {
	// TLS enables a verified TLS 1.2+ handshake. There is no insecure mode.
	TLS bool
	// Roots is "system" (default), "webpki", or "pem". "pem" trusts only CAPEM
	// (no system bundle, no public roots) and requires a non-empty CAPEM.
	Roots string
	// CAPEM adds PEM-encoded certificates to the verification roots.
	CAPEM []byte
	// ServerName overrides SNI and certificate verification; empty uses Host.
	ServerName string
	// NoDelay controls TCP_NODELAY. Nil keeps the native default (enabled);
	// a pointer distinguishes "leave the default" from an explicit false.
	NoDelay *bool
}

func (o TCPOptions) encode() ([]byte, error) {
	options := map[string]any{}
	if o.TLS {
		options["tls"] = true
	}
	switch o.Roots {
	case "":
	case "system", "webpki", "pem":
		options["roots"] = o.Roots
	default:
		return nil, fmt.Errorf("capsulad: TCPOptions.Roots %q must be \"system\", \"webpki\" or \"pem\"", o.Roots)
	}
	if len(o.CAPEM) > 0 {
		options["ca_pem"] = string(o.CAPEM)
	}
	if o.ServerName != "" {
		options["server_name"] = o.ServerName
	}
	if o.NoDelay != nil {
		options["nodelay"] = *o.NoDelay
	}
	return json.Marshal(options)
}

// StreamInfo is the diagnostics JSON of one TCP/TLS stream.
type StreamInfo struct {
	Kind          string `json:"kind"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	TLS           bool   `json:"tls"`
	ServerName    string `json:"server_name"`
	Peer          string `json:"peer"`
	Closed        bool   `json:"closed"`
	ReadShutdown  bool   `json:"read_shutdown"`
	WriteShutdown bool   `json:"write_shutdown"`
}

// TCPStream is one outbound TCP or TLS connection. Reads are serialized (one
// logical reader) and writes are serialized per call (one logical writer), so
// concurrent writes never interleave bytes; a write may accept fewer bytes
// than len(p). The read and write halves are independent: a parked Read never
// delays Write or Shutdown(ShutdownWrite). Close may be called from another
// goroutine and unblocks pending I/O with CANCELLED.
type TCPStream struct {
	mu     sync.Mutex
	handle uint64
}

// OpenTCP loads the shared engine and connects to host:port. Only a context
// deadline bounds DNS, TCP and the TLS handshake; cancellation without a
// deadline does not abort the native dial (30 s default). Reads and writes
// without a deadline wait indefinitely.
func OpenTCP(ctx context.Context, env Env, host string, port uint16, opts TCPOptions) (*TCPStream, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureLibrary(env.Library); err != nil {
		return nil, err
	}
	if host == "" {
		return nil, errors.New("capsulad: OpenTCP host is required")
	}
	if port == 0 {
		return nil, errors.New("capsulad: OpenTCP port must be 1..65535")
	}
	options, err := opts.encode()
	if err != nil {
		return nil, err
	}
	handle, err := tcpOpenNative(host, port, options, timeoutMillis(ctx, 0))
	if err != nil {
		return nil, err
	}
	stream := &TCPStream{handle: handle}
	runtime.SetFinalizer(stream, func(dead *TCPStream) { _ = dead.Close() })
	return stream, nil
}

// Close releases the native handle and unblocks a pending read or write. It is
// idempotent.
func (s *TCPStream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	handle := s.handle
	s.handle = 0
	s.mu.Unlock()
	if handle == 0 {
		return nil
	}
	runtime.SetFinalizer(s, nil)
	return streamCloseNative(handle)
}

// Handle returns the native handle, or 0 after Close.
func (s *TCPStream) Handle() uint64 { return s.value() }

func (s *TCPStream) value() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handle
}

// Read reads one chunk into p. It returns io.EOF when the peer closed cleanly.
// A zero-length p returns (0, nil) without a native call. Cancelling ctx closes
// the stream (there is no separate per-read cancel), and Read then returns
// ctx.Err().
func (s *TCPStream) Read(ctx context.Context, p []byte) (int, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	maxLen := len(p)
	if maxLen > maxStreamRead {
		maxLen = maxStreamRead
	}
	var n int
	err := s.runIO(ctx, "read stream", func() (int32, string) {
		data, status, detail := streamReadNative(s.value(), maxLen, ioTimeoutMS(ctx))
		n = copy(p, data)
		return status, detail
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Write writes one chunk and returns the bytes accepted, which may be less
// than len(p). A zero-length write returns (0, nil). Cancelling ctx closes the
// stream.
func (s *TCPStream) Write(ctx context.Context, p []byte) (int, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var written int
	err := s.runIO(ctx, "write stream", func() (int32, string) {
		var status int32
		var detail string
		written, status, detail = streamWriteNative(s.value(), p, ioTimeoutMS(ctx))
		return status, detail
	})
	if err != nil {
		return written, err
	}
	return written, nil
}

// runIO runs one native I/O call with a cancellation watcher. Native reads and
// writes block, so context cancellation closes the stream to unblock them.
func (s *TCPStream) runIO(ctx context.Context, op string, call func() (int32, string)) error {
	handle := s.value()
	if handle == 0 {
		return ErrClosed
	}
	if ctx.Done() == nil {
		status, detail := call()
		return s.mapStatus(ctx, op, status, detail)
	}
	var cancelled atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			_ = s.Close()
		case <-stop:
		}
	}()
	status, detail := call()
	close(stop)
	<-done
	err := s.mapStatus(ctx, op, status, detail)
	if err != nil && cancelled.Load() && (status == statusCancelled || status == statusClosed) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	return err
}

func (s *TCPStream) mapStatus(ctx context.Context, op string, status int32, detail string) error {
	switch status {
	case statusOK:
		return nil
	case statusEndOfStream:
		return io.EOF
	case statusCancelled, statusClosed:
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrClosed
	}
	return nativeStatusError(op, status, detail)
}

// Shutdown half-closes the stream. A peer or local shutdown is terminal for
// that direction but leaves the other one usable.
func (s *TCPStream) Shutdown(how ShutdownHow) error {
	if how < ShutdownWrite || how > ShutdownBoth {
		return fmt.Errorf("capsulad: invalid shutdown mode %d", int32(how))
	}
	handle := s.value()
	if handle == 0 {
		return ErrClosed
	}
	return streamShutdownNative(handle, int32(how))
}

// Info returns native stream diagnostics.
func (s *TCPStream) Info() (StreamInfo, error) {
	handle := s.value()
	if handle == 0 {
		return StreamInfo{}, ErrClosed
	}
	payload, status, detail := streamInfoNative(handle)
	if err := nativeStatusError("stream info", status, detail); err != nil {
		return StreamInfo{}, err
	}
	var info StreamInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return StreamInfo{}, fmt.Errorf("capsulad: decode stream info: %w", err)
	}
	return info, nil
}

// maxStreamRead is the native per-read cap (4 MiB).
const maxStreamRead = 4 << 20

// ioTimeoutMS is the native timeout for one blocking read/write: the context
// deadline when present, otherwise "wait forever". A non-positive remaining
// budget becomes 1 ms so the call cannot poll or inherit a native default.
func ioTimeoutMS(ctx context.Context) uint32 {
	if ctx == nil {
		return waitForever
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return waitForever
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 1
	}
	millis := remaining.Milliseconds()
	if millis == 0 {
		millis = 1
	}
	if millis > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(millis)
}

// watchAbort runs one blocking native call for a handle whose deadline is
// enforced by the native per-call timeout (listeners and UDP sockets have no
// per-operation cancel). A context deadline therefore never aborts: the call
// returns TIMEOUT and the handle stays usable. Only an explicit cancellation
// runs abort, which releases the handle and unblocks the parked call; aborted
// reports that it happened while the call was in flight.
func watchAbort(ctx context.Context, abort func(), call func() (int32, string)) (status int32, detail string, aborted bool) {
	if ctx.Done() == nil {
		status, detail = call()
		return status, detail, false
	}
	var cancelSeen atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			// Only an explicit cancellation aborts. A deadline is enforced by
			// the native timeout, which leaves the handle usable.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return
			}
			cancelSeen.Store(true)
			abort()
		case <-stop:
		}
	}()
	status, detail = call()
	close(stop)
	<-done
	return status, detail, cancelSeen.Load()
}
