package capsula

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// callHandle owns one native call handle plus its context watcher. Every path
// releases the handle exactly once: explicit Close, context cancellation (the
// library unblocks the reader and the owner releases), or the finalizer.
type callHandle struct {
	handle uint64
	op     string
	ctx    context.Context

	mu        sync.Mutex
	released  bool
	watchStop chan struct{}
	watchOnce sync.Once
}

func newCallHandle(handle uint64, ctx context.Context, op string) *callHandle {
	c := &callHandle{handle: handle, op: op, ctx: ctx}
	if ctx != nil && ctx.Done() != nil {
		stop := make(chan struct{})
		c.watchStop = stop
		go func() {
			select {
			case <-ctx.Done():
				_ = callCancelNative(handle)
			case <-stop:
			}
		}()
	}
	runtime.SetFinalizer(c, func(dead *callHandle) { dead.release() })
	return c
}

func (c *callHandle) stopWatcher() {
	if c.watchStop != nil {
		c.watchOnce.Do(func() { close(c.watchStop) })
	}
}

func (c *callHandle) value() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handle
}

func (c *callHandle) contextErr() error {
	if c.ctx != nil {
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// terminalError is the error for a failed or gRPC-ended call.
func (c *callHandle) terminalError() error {
	if err := c.contextErr(); err != nil {
		return err
	}
	handle := c.value()
	if handle == 0 {
		return ErrClosed
	}
	if err := callTerminalError(c.op, handle); err != nil {
		return err
	}
	return io.EOF
}

func (c *callHandle) sendJSON(v any) error {
	handle := c.value()
	if handle == 0 {
		return ErrClosed
	}
	payload, err := marshalProtoJSON(v)
	if err != nil {
		return err
	}
	return callSendJSONNative(handle, payload)
}

func (c *callHandle) sendProto(payload []byte) error {
	handle := c.value()
	if handle == 0 {
		return ErrClosed
	}
	return callSendProtoNative(handle, payload)
}

func (c *callHandle) halfClose() error {
	handle := c.value()
	if handle == 0 {
		return ErrClosed
	}
	return callHalfCloseNative(handle)
}

func (c *callHandle) recvJSON() ([]byte, error) {
	payload, status, detail := callRecvJSONNative(c.value(), waitForever)
	switch status {
	case statusOK:
		return payload, nil
	case statusEndOfStream:
		return nil, io.EOF
	case statusGRPC:
		return nil, c.terminalError()
	case statusCancelled, statusClosed:
		if err := c.contextErr(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
	return nil, nativeError(c.op+" recv", status, detail)
}

func (c *callHandle) recvProto() ([]byte, error) {
	payload, status, detail := callRecvProtoNative(c.value(), waitForever)
	switch status {
	case statusOK:
		return payload, nil
	case statusEndOfStream:
		return nil, io.EOF
	case statusGRPC:
		return nil, c.terminalError()
	case statusCancelled, statusClosed:
		if err := c.contextErr(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
	return nil, nativeError(c.op+" recv", status, detail)
}

func (c *callHandle) recvJSONTimeout(timeoutMS uint32) ([]byte, error) {
	payload, status, detail := callRecvJSONNative(c.value(), timeoutMS)
	switch status {
	case statusOK:
		return payload, nil
	case statusEndOfStream:
		return nil, io.EOF
	case statusGRPC:
		return nil, c.terminalError()
	case statusTimeout:
		return nil, &StatusError{Op: c.op + " recv", Code: statusTimeout, Message: "deadline elapsed"}
	case statusCancelled, statusClosed:
		if err := c.contextErr(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
	return nil, nativeError(c.op+" recv", status, detail)
}

// release drops the caller's reference exactly once. Releasing an unfinished
// call cancels it.
func (c *callHandle) release() error {
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return nil
	}
	c.released = true
	handle := c.handle
	c.handle = 0
	c.mu.Unlock()

	runtime.SetFinalizer(c, nil)
	c.stopWatcher()
	if handle == 0 {
		return nil
	}
	return callReleaseNative(handle)
}

// Client-facing stream handles -------------------------------------------------

// Stream is one outbound framework or routed stream.
type Stream struct{ call *callHandle }

// RoutedCall is the app-routed stream returned by Client.Stream.
type RoutedCall = Stream

// Send sends one serialized protobuf request message.
func (s *Stream) Send(payload []byte) error {
	if s == nil || s.call == nil {
		return ErrClosed
	}
	return s.call.sendProto(payload)
}

// SendJSON sends one request message encoded as plain protobuf JSON.
func (s *Stream) SendJSON(v any) error {
	if s == nil || s.call == nil {
		return ErrClosed
	}
	return s.call.sendJSON(v)
}

// HalfClose closes the request direction.
func (s *Stream) HalfClose() error {
	if s == nil || s.call == nil {
		return ErrClosed
	}
	return s.call.halfClose()
}

// Recv returns one serialized response; io.EOF marks a successful end.
func (s *Stream) Recv() ([]byte, error) {
	if s == nil || s.call == nil {
		return nil, ErrClosed
	}
	return s.call.recvProto()
}

// RecvJSON decodes one response into v; io.EOF marks a successful end.
func (s *Stream) RecvJSON(v any) error {
	if s == nil || s.call == nil {
		return ErrClosed
	}
	payload, err := s.call.recvJSON()
	if err != nil {
		return err
	}
	return unmarshalProtoJSON(payload, v)
}

// CloseSend is an alias for HalfClose.
func (s *Stream) CloseSend() error { return s.HalfClose() }

// Close cancels the stream and releases its native handle. It is idempotent.
func (s *Stream) Close() error {
	if s == nil || s.call == nil {
		return nil
	}
	return s.call.release()
}

// ServerCall is the app-side handler view of one routed call.
type ServerCall struct {
	handle  uint64
	id      string
	service string
	method  string
	ctx     context.Context
	cancel  context.CancelFunc

	mu    sync.Mutex
	ended bool
}

func newServerCall(handle uint64, info incomingCallInfo) *ServerCall {
	ctx, cancel := context.WithCancel(context.Background())
	return &ServerCall{
		handle:  handle,
		id:      info.CallID,
		service: info.GRPCService,
		method:  info.RPCName,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Context returns the handler context; it is cancelled after the handler
// returns.
func (s *ServerCall) Context() context.Context { return s.ctx }

// ID is the framework call identifier.
func (s *ServerCall) ID() string { return s.id }

// Service is the fully-qualified protobuf service name.
func (s *ServerCall) Service() string { return s.service }

// Method is the bare RPC name.
func (s *ServerCall) Method() string { return s.method }

// Recv returns the next serialized request message; io.EOF indicates the
// caller half-closed.
func (s *ServerCall) Recv() ([]byte, error) {
	payload, status, detail := callRecvProtoNative(s.handle, waitForever)
	switch status {
	case statusOK:
		return payload, nil
	case statusEndOfStream:
		return nil, io.EOF
	case statusGRPC:
		return nil, callTerminalError("call "+s.service+"/"+s.method, s.handle)
	case statusCancelled:
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	case statusClosed:
		return nil, ErrClosed
	}
	return nil, nativeError("call recv", status, detail)
}

// RecvJSON decodes the next request message.
func (s *ServerCall) RecvJSON(v any) error {
	payload, err := s.recvJSON()
	if err != nil {
		return err
	}
	return unmarshalProtoJSON(payload, v)
}

func (s *ServerCall) recvJSON() ([]byte, error) {
	payload, status, detail := callRecvJSONNative(s.handle, waitForever)
	switch status {
	case statusOK:
		return payload, nil
	case statusEndOfStream:
		return nil, io.EOF
	case statusGRPC:
		return nil, callTerminalError("call "+s.service+"/"+s.method, s.handle)
	case statusCancelled:
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	case statusClosed:
		return nil, ErrClosed
	}
	return nil, nativeError("call recv", status, detail)
}

// Send emits one serialized response message.
func (s *ServerCall) Send(payload []byte) error {
	if s.isEnded() {
		return fmt.Errorf("capsula: call already ended")
	}
	return callSendProtoNative(s.handle, payload)
}

// SendJSON emits one response message encoded as plain protobuf JSON.
func (s *ServerCall) SendJSON(v any) error {
	if s.isEnded() {
		return fmt.Errorf("capsula: call already ended")
	}
	payload, err := marshalProtoJSON(v)
	if err != nil {
		return err
	}
	return callSendJSONNative(s.handle, payload)
}

// End terminates the call with a canonical gRPC status code.
func (s *ServerCall) End(code int32, detail string) error {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil
	}
	s.ended = true
	s.mu.Unlock()
	return callEndNative(s.handle, code, detail)
}

func (s *ServerCall) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// finish ends an unended handler with OK, matching the previous SDK.
func (s *ServerCall) finish() {
	if !s.isEnded() {
		_ = s.End(0, "")
	}
	s.cancel()
}
