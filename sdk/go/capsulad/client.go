package capsulad

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime/cgo"
	"sync"
	"time"
)

// Handlers receive framework events on the app's bus connection.
type Handlers struct {
	Invocation   func(context.Context, *LaunchInvocation)
	Settings     func(*SettingsChanged)
	Action       func(context.Context, *ActionInvoked)
	Activation   func(context.Context, *Activation)
	TouchRelease func(*InputReleased)
	Ready        func()
	Disconnected func(error)
}

// CallHandler serves one custom routed call. Recv yields serialized request
// messages; Send emits responses; End reports the final status.
type CallHandler func(ctx context.Context, call *ServerCall)

// Client is one app bus connection backed by the shared native engine.
type Client struct {
	env Env

	mu       sync.Mutex
	handle   uint64
	bus      uint64
	busUser  uintptr
	handlers Handlers
	calls    map[string]map[string]CallHandler
	started  bool
	err      error

	done       chan struct{}
	finishOnce sync.Once
}

// Connect loads the shared native engine and opens a client. The bus itself
// is opened by Start.
func Connect(ctx context.Context, env Env) (*Client, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.RunID == "" {
		return nil, errors.New("capsulad: run_id is required")
	}
	timeout := timeoutMillis(ctx, 10*time.Second)
	handle, err := connectNative(env.Library, env.Socket, env.RunID, env.AppID, timeout)
	if err != nil {
		return nil, err
	}
	return &Client{
		env:    env,
		handle: handle,
		calls:  map[string]map[string]CallHandler{},
		done:   make(chan struct{}),
	}, nil
}

// Close closes the app bus and client. It is idempotent.
func (c *Client) Close() error {
	c.shutdown(ErrClosed)
	return nil
}

// Done closes when the client's bus has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is the terminal bus error, or nil while connected.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// RunID is the launcher-assigned run identity.
func (c *Client) RunID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.env.RunID
}

// AppID is the application identity.
func (c *Client) AppID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.env.AppID
}

// SetHandlers replaces the lifecycle handlers. It may be called before or
// after Start.
func (c *Client) SetHandlers(h Handlers) {
	c.mu.Lock()
	c.handlers = h
	c.mu.Unlock()
}

// Handle registers a custom routed method for this app.
func (c *Client) Handle(service, method string, handler CallHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls[service] == nil {
		c.calls[service] = map[string]CallHandler{}
	}
	c.calls[service][method] = handler
}

// RegisterDescriptors registers a serialized google.protobuf.FileDescriptorSet
// (including imports) so JSON calls to custom app services can be resolved.
func (c *Client) RegisterDescriptors(fileDescriptorSet []byte) error {
	handle, err := c.clientHandle()
	if err != nil {
		return err
	}
	if len(fileDescriptorSet) == 0 {
		return errors.New("capsulad: empty file descriptor set")
	}
	return registerDescriptorsNative(handle, fileDescriptorSet)
}

// Start opens the persistent bus stream and blocks until it ends or ctx is
// cancelled. READY is reported once via Handlers.Ready.
func (c *Client) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	client, err := c.clientHandle()
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return errors.New("capsulad: client already started")
	}
	c.started = true
	c.mu.Unlock()

	user := cgo.NewHandle(c)
	bus, err := busOpenNative(client, uintptr(user))
	if err != nil {
		user.Delete()
		c.shutdown(err)
		return err
	}
	c.mu.Lock()
	c.bus = bus
	c.busUser = uintptr(user)
	closed := c.handle == 0
	c.mu.Unlock()
	if closed {
		// Another goroutine closed the client while the bus was opening.
		deleteBusUser(uintptr(user))
		c.bus = 0
		c.busUser = 0
		return ErrClosed
	}

	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	if ctx.Done() != nil {
		go func() {
			defer close(watchDone)
			select {
			case <-ctx.Done():
				c.shutdown(ctx.Err())
			case <-watchStop:
			}
		}()
	} else {
		close(watchDone)
	}
	defer func() {
		if ctx.Done() != nil {
			select {
			case <-watchStop:
			default:
				close(watchStop)
			}
		}
		<-watchDone
	}()

	if err := busReadyNative(bus, timeoutMillis(ctx, 10*time.Second)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		c.shutdown(err)
		return err
	}

	c.mu.Lock()
	ready := c.handlers.Ready
	c.mu.Unlock()
	if ready != nil {
		safeGo(ready)
	}

	select {
	case <-c.done:
		return c.Err()
	case <-ctx.Done():
		c.shutdown(ctx.Err())
		return ctx.Err()
	}
}

// closeBusHandle closes the bus, retrying asynchronously when a callback
// thread is still running (BUSY). Native callbacks prohibit closing their own
// bus; the retry runs after the callback returns.
func closeBusHandle(bus uint64, user uintptr) {
	err := busCloseNative(bus)
	if err == nil {
		deleteBusUser(user)
		return
	}
	if code, ok := nativeStatusCode(err); ok && code == statusBusy {
		go retryBusClose(bus, user)
		return
	}
	deleteBusUser(user)
}

func retryBusClose(bus uint64, user uintptr) {
	for range 2000 {
		time.Sleep(time.Millisecond)
		err := busCloseNative(bus)
		if err == nil {
			deleteBusUser(user)
			return
		}
		if code, ok := nativeStatusCode(err); !ok || code != statusBusy {
			deleteBusUser(user)
			return
		}
	}
	deleteBusUser(user)
}

func deleteBusUser(user uintptr) {
	if user != 0 {
		cgo.Handle(user).Delete()
	}
}

func closeClientHandle(handle uint64) {
	err := closeNative(handle)
	if err == nil {
		return
	}
	if code, ok := nativeStatusCode(err); ok && code == statusBusy {
		go func() {
			for range 2000 {
				time.Sleep(time.Millisecond)
				err := closeNative(handle)
				if err == nil {
					return
				}
				if code, ok := nativeStatusCode(err); !ok || code != statusBusy {
					return
				}
			}
		}()
	}
}

func nativeStatusCode(err error) (int32, bool) {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code, true
	}
	return 0, false
}

func (c *Client) clientHandle() (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handle == 0 {
		return 0, ErrClosed
	}
	return c.handle, nil
}

func (c *Client) busHandle() (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bus == 0 || c.handle == 0 {
		return 0, ErrClosed
	}
	return c.bus, nil
}

func (c *Client) shutdown(cause error) {
	c.finishOnce.Do(func() {
		c.mu.Lock()
		if cause != nil {
			c.err = cause
		}
		bus := c.bus
		user := c.busUser
		client := c.handle
		c.bus = 0
		c.busUser = 0
		c.mu.Unlock()

		if bus != 0 {
			closeBusHandle(bus, user)
		}
		if client != 0 {
			closeClientHandle(client)
		}

		c.mu.Lock()
		c.handle = 0
		err := c.err
		handler := c.handlers.Disconnected
		c.mu.Unlock()
		close(c.done)
		if handler != nil {
			safeCall(func() { handler(err) })
		}
	})
}

// NotifyInvocationResult reports the asynchronous outcome of a launch.
func (c *Client) NotifyInvocationResult(invocationID string, accepted bool, detail string) error {
	bus, err := c.busHandle()
	if err != nil {
		return err
	}
	return busNotifyInvocationNative(bus, invocationID, accepted, detail)
}

// NotifyActionResult reports an action button outcome.
func (c *Client) NotifyActionResult(actionInvocationID string, succeeded bool, detail string) error {
	bus, err := c.busHandle()
	if err != nil {
		return err
	}
	return busNotifyActionNative(bus, actionInvocationID, succeeded, detail)
}

// Call performs one unary call and returns its first serialized response.
// targetAppID empty targets the framework's own services.
func (c *Client) Call(ctx context.Context, targetAppID, service, rpc string,
	request []byte, timeout time.Duration) ([]byte, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call, err := c.startCall(ctx, targetAppID, service, rpc, callKindUnary, timeout)
	if err != nil {
		return nil, err
	}
	defer call.release()

	if err := call.sendProto(request); err != nil {
		return nil, err
	}
	if err := call.halfClose(); err != nil {
		return nil, err
	}
	var first []byte
	for {
		message, err := call.recvProto()
		if err == io.EOF {
			return first, nil
		}
		if err != nil {
			return nil, err
		}
		if first == nil {
			first = message
		}
	}
}

// CallJSON performs one unary call with plain Go values. The request is
// marshalled as canonical protobuf JSON and converted by the native engine;
// the response JSON is decoded into response (which may be nil).
func (c *Client) CallJSON(ctx context.Context, targetAppID, service, rpc string,
	request, response any, timeout time.Duration) error {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	call, err := c.startCall(ctx, targetAppID, service, rpc, callKindUnary, timeout)
	if err != nil {
		return err
	}
	defer call.release()

	var payload []byte
	if request != nil {
		payload, err = marshalProtoJSON(request)
		if err != nil {
			return err
		}
	} else {
		payload = []byte("{}")
	}
	if err := call.sendJSONNative(payload); err != nil {
		return err
	}
	if err := call.halfClose(); err != nil {
		return err
	}
	var first []byte
	for {
		message, err := call.recvJSON()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if first == nil {
			first = message
		}
	}
	if response != nil && first != nil {
		return unmarshalProtoJSON(first, response)
	}
	return nil
}

// Stream opens a routed or framework stream. targetAppID empty targets the
// framework's own service.
func (c *Client) Stream(ctx context.Context, targetAppID, service, rpc string,
	timeout time.Duration) (*RoutedCall, error) {
	call, err := c.startCall(ctx, targetAppID, service, rpc, callKindBidi, timeout)
	if err != nil {
		return nil, err
	}
	return &Stream{call: call}, nil
}

// OpenStream opens a server-streaming framework call and sends request once.
func (c *Client) OpenStream(ctx context.Context, service, rpc string, request any) (*Stream, error) {
	call, err := c.startCall(ctx, "", service, rpc, callKindServerStream, 0)
	if err != nil {
		return nil, err
	}
	stream := &Stream{call: call}
	if request != nil {
		if err := call.sendJSON(request); err != nil {
			call.release()
			return nil, err
		}
	} else {
		if err := call.sendJSONNative([]byte("{}")); err != nil {
			call.release()
			return nil, err
		}
	}
	if err := call.halfClose(); err != nil {
		call.release()
		return nil, err
	}
	return stream, nil
}

// OpenBidi opens a bidirectional framework call.
func (c *Client) OpenBidi(ctx context.Context, service, rpc string) (*Stream, error) {
	call, err := c.startCall(ctx, "", service, rpc, callKindBidi, 0)
	if err != nil {
		return nil, err
	}
	return &Stream{call: call}, nil
}

func (c *Client) startCall(ctx context.Context, targetAppID, service, rpc string,
	kind int32, timeout time.Duration) (*callHandle, error) {
	ctx = orBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, err := c.clientHandle()
	if err != nil {
		return nil, err
	}
	timeoutMS := timeoutMillis(ctx, timeout)
	var handle uint64
	if targetAppID == "" {
		handle, err = callStartNative(client, service, rpc, kind, timeoutMS)
	} else {
		handle, err = callStartRoutedNative(client, targetAppID, service, rpc, timeoutMS)
	}
	if err != nil {
		return nil, err
	}
	return newCallHandle(handle, ctx, service+"/"+rpc), nil
}

// sendJSONNative is used where the caller already produced canonical JSON.
func (c *callHandle) sendJSONNative(payload []byte) error {
	handle := c.value()
	if handle == 0 {
		return ErrClosed
	}
	return callSendJSONNative(handle, payload)
}

// timeoutMillis converts a call timeout plus the context deadline into the
// native millisecond budget. Zero means no deadline.
func timeoutMillis(ctx context.Context, timeout time.Duration) uint32 {
	budget := timeout
	hasDeadline := false
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			hasDeadline = true
			remaining := time.Until(deadline)
			if budget <= 0 || remaining < budget {
				budget = remaining
			}
		}
	}
	if budget <= 0 {
		if timeout > 0 || hasDeadline {
			return 1 // already expired
		}
		return 0
	}
	millis := budget.Milliseconds()
	if millis == 0 {
		millis = 1
	}
	if millis > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(millis)
}

func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func safeGo(fn func()) {
	go func() {
		defer func() { _ = recover() }()
		fn()
	}()
}

func safeCall(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// --- inbound dispatch -------------------------------------------------------

// incomingCallInfo is the on_call metadata JSON from the native engine. The
// ABI contract fixes these keys as snake_case (they are not protobuf JSON);
// on_event payloads remain canonical protobuf JSON.
type incomingCallInfo struct {
	CallID         string `json:"call_id"`
	CallerRunID    string `json:"caller_run_id"`
	GRPCService    string `json:"grpc_service"`
	RPCName        string `json:"rpc_name"`
	DeadlineMonoNS uint64 `json:"deadline_monotonic_ns"`
}

func (c *Client) onBusEvent(kind int32, payload []byte) {
	switch kind {
	case 1: // invocation
		var event LaunchInvocation
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			return
		}
		c.mu.Lock()
		handler := c.handlers.Invocation
		c.mu.Unlock()
		if handler != nil {
			safeGo(func() { handler(context.Background(), &event) })
		}
	case 2: // settings changed
		var event SettingsChanged
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			return
		}
		c.mu.Lock()
		handler := c.handlers.Settings
		c.mu.Unlock()
		if handler != nil {
			safeCall(func() { handler(&event) })
		}
	case 3: // action
		var event ActionInvoked
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			return
		}
		c.mu.Lock()
		handler := c.handlers.Action
		c.mu.Unlock()
		if handler != nil {
			safeGo(func() { handler(context.Background(), &event) })
		}
	case 4: // activation
		var event Activation
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			return
		}
		c.mu.Lock()
		handler := c.handlers.Activation
		c.mu.Unlock()
		if handler != nil {
			safeGo(func() { handler(context.Background(), &event) })
		}
	case 5: // touch released
		var event InputReleased
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			return
		}
		c.mu.Lock()
		handler := c.handlers.TouchRelease
		c.mu.Unlock()
		if handler != nil {
			safeCall(func() { handler(&event) })
		}
	}
}

func (c *Client) onIncomingCall(handle uint64, infoPayload []byte) {
	var info incomingCallInfo
	if err := unmarshalProtoJSON(infoPayload, &info); err != nil {
		_ = callEndNative(handle, 13, "invalid incoming call metadata")
		return
	}
	call := newServerCall(handle, info)
	defer call.cancel()

	c.mu.Lock()
	handler := c.calls[info.GRPCService][info.RPCName]
	c.mu.Unlock()

	if handler == nil {
		_ = call.End(12, fmt.Sprintf("method %s.%s is not implemented by this app",
			info.GRPCService, info.RPCName))
		return
	}

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				_ = call.End(13, fmt.Sprintf("handler panic: %v", recovered))
			}
		}()
		handler(call.ctx, call)
	}()
	call.finish()
}

func (c *Client) onBusDisconnected(status int32, detail string) {
	cause := error(ErrClosed)
	if status != statusOK {
		if detail == "" {
			detail = "app bus disconnected"
		}
		cause = &StatusError{Op: "app bus", Code: status, Message: detail}
	}
	c.shutdown(cause)
}
