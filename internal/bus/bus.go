// Package bus implements the reverse-RPC application bus.
//
// Every supervised app holds one long-lived AppBusService.Connect stream. The
// daemon relays AppBusService.Call streams to the target app over that
// connection; apps never listen on their own IPC socket. Buffers are bounded,
// deadlines and cancellation are forwarded, and a disconnect fails in-flight
// calls with UNAVAILABLE instead of replaying them into a new process.
package bus

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

const (
	controlQueue    = 128
	responseQueue   = 32
	maxDeadline     = 5 * time.Minute
	defaultDeadline = 30 * time.Second
)

type ResolveFunc func(runID string) (appID string, pid int, ok bool)

// InternalHandler answers a routed app-bus call on behalf of a compiled-in
// system app. request and each response are the same protobuf JSON frames the
// SDK sends over AppBusService.Call, so an external app cannot tell a system
// app from a supervised one. An empty responses slice means an empty successful
// reply; returning a gRPC status preserves its code and message.
type InternalHandler func(ctx context.Context, service, rpc string, request []byte) ([][]byte, error)

type internalTarget struct {
	runID   string
	appID   string
	handler InternalHandler
}

type Hooks struct {
	OnReady            func(runID, appID string)
	OnDisconnect       func(runID, appID string)
	OnInvocationResult func(runID string, result *frameworkv1.AppInvocationResult)
	OnActionResult     func(runID string, result *frameworkv1.AppActionResult)
}

type Bus struct {
	logs    *logbus.Bus
	resolve ResolveFunc
	hooks   Hooks

	mu       sync.Mutex
	conns    map[string]*conn
	byApp    map[string]*conn
	internal map[string]*internalTarget
	calls    map[string]*call
	closed   bool
}

func New(logs *logbus.Bus, resolve ResolveFunc, hooks Hooks) *Bus {
	return &Bus{
		logs: logs, resolve: resolve, hooks: hooks,
		conns: map[string]*conn{}, byApp: map[string]*conn{},
		internal: map[string]*internalTarget{}, calls: map[string]*call{},
	}
}

type conn struct {
	bus    *Bus
	runID  string
	appID  string
	stream frameworkv1.AppBusService_ConnectServer
	out    chan *frameworkv1.AppInbound
	done   chan struct{}
	once   sync.Once
	ready  atomic.Bool
}

func (c *conn) close() {
	c.once.Do(func() { close(c.done) })
}

func (c *conn) enqueue(msg *frameworkv1.AppInbound) error {
	select {
	case <-c.done:
		return fmt.Errorf("app %s is disconnected", c.appID)
	case c.out <- msg:
		return nil
	default:
		return fmt.Errorf("app %s inbound queue is full", c.appID)
	}
}

// Connect services an app's persistent bus connection.
func (b *Bus) Connect(stream frameworkv1.AppBusService_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.GetRunId() == "" {
		return status.Error(codes.InvalidArgument, "first AppBus message must be AppHello{run_id}")
	}
	appID, pid, ok := b.resolve(hello.GetRunId())
	if !ok {
		b.logs.Warnf("bus", "", "rejected bus connection for unknown run %q", hello.GetRunId())
		return status.Error(codes.PermissionDenied, "unknown run_id")
	}
	creds := grpcpeer.FromContext(stream.Context())
	if creds.OK && creds.PID != pid {
		b.logs.Errorf("bus", appID, "peer credential mismatch: hello claims pid %d, socket peer is %d", pid, creds.PID)
		return status.Error(codes.PermissionDenied, "run_id does not match connecting process")
	}

	c := &conn{
		bus: b, runID: hello.GetRunId(), appID: appID, stream: stream,
		out: make(chan *frameworkv1.AppInbound, controlQueue), done: make(chan struct{}),
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		c.close()
		return status.Error(codes.Unavailable, "daemon is shutting down")
	}
	old := b.conns[c.runID]
	b.conns[c.runID] = c
	b.mu.Unlock()
	if old != nil {
		old.close()
	}
	b.logs.Infof("bus", appID, "connected run=%s pid=%d", c.runID, pid)

	go b.writer(c)
	defer func() {
		c.close()
		b.unregister(c)
	}()

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch m := msg.Message.(type) {
		case *frameworkv1.AppOutbound_Ready:
			b.markReady(c)
		case *frameworkv1.AppOutbound_CallReply:
			b.handleReply(c, m.CallReply)
		case *frameworkv1.AppOutbound_InvocationResult:
			if b.hooks.OnInvocationResult != nil && m.InvocationResult != nil {
				b.hooks.OnInvocationResult(c.runID, m.InvocationResult)
			}
		case *frameworkv1.AppOutbound_ActionResult:
			if b.hooks.OnActionResult != nil && m.ActionResult != nil {
				b.hooks.OnActionResult(c.runID, m.ActionResult)
			}
		case *frameworkv1.AppOutbound_Hello:
			b.logs.Warnf("bus", appID, "duplicate AppHello on established connection ignored")
		default:
			b.logs.Warnf("bus", appID, "unexpected AppBus message %T", m)
		}
	}
}

func (b *Bus) writer(c *conn) {
	for {
		select {
		case <-c.done:
			return
		case <-c.stream.Context().Done():
			c.close()
			return
		case msg := <-c.out:
			if err := c.stream.Send(msg); err != nil {
				b.logs.Warnf("bus", c.appID, "send failed: %v", err)
				c.close()
				return
			}
		}
	}
}

func (b *Bus) markReady(c *conn) {
	c.ready.Store(true)
	b.mu.Lock()
	b.byApp[c.appID] = c
	b.mu.Unlock()
	b.logs.Infof("bus", c.appID, "app-ready run=%s", c.runID)
	if b.hooks.OnReady != nil {
		h := b.hooks.OnReady
		go h(c.runID, c.appID)
	}
}

func (b *Bus) unregister(c *conn) {
	b.mu.Lock()
	if b.conns[c.runID] == c {
		delete(b.conns, c.runID)
	}
	if b.byApp[c.appID] == c {
		delete(b.byApp, c.appID)
	}
	var doomed []*call
	for id, cl := range b.calls {
		if cl.targetRunID == c.runID {
			doomed = append(doomed, cl)
			delete(b.calls, id)
		}
	}
	b.mu.Unlock()

	for _, cl := range doomed {
		cl.finish(int32(codes.Unavailable), "target app disconnected")
	}
	if b.hooks.OnDisconnect != nil {
		go b.hooks.OnDisconnect(c.runID, c.appID)
	}
}

// RegisterInternal attaches a compiled-in system app to the app-bus routing
// table. It has no Connect stream: routed calls are answered in-process, while
// ReadyApp/ReadyRun report the app exactly like a connected supervised app.
func (b *Bus) RegisterInternal(appID, runID string, h InternalHandler) error {
	if appID == "" || runID == "" || h == nil {
		return fmt.Errorf("internal app id, run id and handler are required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("daemon is shutting down")
	}
	if old, ok := b.internal[appID]; ok {
		return fmt.Errorf("internal app %s is already registered for run %s", appID, old.runID)
	}
	if c, ok := b.byApp[appID]; ok && c.ready.Load() {
		return fmt.Errorf("app %s already has a connected ready run %s", appID, c.runID)
	}
	b.internal[appID] = &internalTarget{runID: runID, appID: appID, handler: h}
	return nil
}

// UnregisterInternal detaches a system app's routed-call endpoint.
func (b *Bus) UnregisterInternal(appID string) {
	b.mu.Lock()
	delete(b.internal, appID)
	b.mu.Unlock()
}

func (b *Bus) internalTarget(appID string) (*internalTarget, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.internal[appID]
	return t, ok
}

// --- routed calls ----------------------------------------------------------

type call struct {
	id          string
	targetRunID string
	callerRunID string
	data        chan []byte
	done        chan struct{}
	once        sync.Once
	code        int32
	detail      string
}

func (c *call) push(data []byte) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.data <- data:
		return true
	default:
		return false
	}
}

func (c *call) finish(code int32, detail string) {
	c.once.Do(func() {
		c.code = code
		c.detail = detail
		close(c.done)
	})
}

func (c *call) result() (int32, string) { return c.code, c.detail }

// Call relays one caller stream to a target app's Connect stream.
func (b *Bus) Call(stream frameworkv1.AppBusService_CallServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || start.GetTargetAppId() == "" || start.GetGrpcService() == "" || start.GetRpcName() == "" {
		return status.Error(codes.InvalidArgument, "first RoutedCall message must be RoutedCallStart with target, service and rpc")
	}
	if target, ok := b.internalTarget(start.GetTargetAppId()); ok {
		return b.callInternal(stream, start, target)
	}
	target, ok := b.readyConn(start.GetTargetAppId())
	if !ok {
		_ = stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
			End: &frameworkv1.AppCallEnd{GrpcStatusCode: int32(codes.Unavailable), Detail: "target app is not ready"},
		}})
		return nil
	}
	callerRunID := ""
	if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
		if vals := md.Get("capsula-run-id"); len(vals) > 0 {
			if _, _, ok := b.resolve(vals[0]); ok {
				callerRunID = vals[0]
			}
		}
	}
	if callerRunID == "" {
		if creds := grpcpeer.FromContext(stream.Context()); creds.OK {
			callerRunID = fmt.Sprintf("pid:%d", creds.PID)
		}
	}
	cl := &call{
		id: ids.New("call"), targetRunID: target.runID, callerRunID: callerRunID,
		data: make(chan []byte, responseQueue), done: make(chan struct{}),
	}
	b.mu.Lock()
	b.calls[cl.id] = cl
	b.mu.Unlock()

	deadline := time.Duration(start.GetTimeoutMs()) * time.Millisecond
	if deadline <= 0 {
		deadline = defaultDeadline
	}
	if deadline > maxDeadline {
		deadline = maxDeadline
	}
	timer := time.AfterFunc(deadline, func() {
		cl.finish(int32(codes.DeadlineExceeded), "routed call deadline exceeded")
		_ = target.enqueue(&frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_CallCancel{
			CallCancel: &frameworkv1.IncomingCallCancel{CallId: cl.id},
		}})
	})
	defer timer.Stop()

	fail := func(code codes.Code, detail string) {
		cl.finish(int32(code), detail)
		_ = target.enqueue(&frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_CallCancel{
			CallCancel: &frameworkv1.IncomingCallCancel{CallId: cl.id},
		}})
	}
	if err := target.enqueue(&frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_Call{
		Call: &frameworkv1.IncomingAppCall{
			CallId: cl.id, CallerRunId: callerRunID,
			GrpcService: start.GetGrpcService(), RpcName: start.GetRpcName(),
		},
	}}); err != nil {
		b.removeCall(cl.id)
		_ = stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
			End: &frameworkv1.AppCallEnd{GrpcStatusCode: int32(codes.Unavailable), Detail: err.Error()},
		}})
		return nil
	}

	go b.pumpRequests(stream, target, cl, fail)

	for {
		select {
		case data := <-cl.data:
			if err := stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_ResponseMessage{ResponseMessage: data}}); err != nil {
				fail(codes.Canceled, "caller stream closed")
				return nil
			}
		case <-cl.done:
			// Drain responses that were queued before the app's end frame so
			// streaming replies are never reordered or dropped.
			for {
				select {
				case data := <-cl.data:
					if err := stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_ResponseMessage{ResponseMessage: data}}); err != nil {
						b.removeCall(cl.id)
						return nil
					}
					continue
				default:
				}
				break
			}
			b.removeCall(cl.id)
			code, detail := cl.result()
			_ = stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
				End: &frameworkv1.AppCallEnd{GrpcStatusCode: code, Detail: detail},
			}})
			return nil
		case <-stream.Context().Done():
			b.removeCall(cl.id)
			_ = target.enqueue(&frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_CallCancel{
				CallCancel: &frameworkv1.IncomingCallCancel{CallId: cl.id},
			}})
			return nil
		}
	}
}

// callInternal services a routed call against an in-process system app. The
// wire shape is identical to a supervised target: one request frame for unary
// or server-streaming calls, then an optional half-close. Bidirectional request
// streaming is rejected because no system app exposes it; it is reported as
// UNIMPLEMENTED rather than silently dropping frames.
func (b *Bus) callInternal(stream frameworkv1.AppBusService_CallServer, start *frameworkv1.RoutedCallStart, target *internalTarget) error {
	ctx := stream.Context()
	if ms := start.GetTimeoutMs(); ms > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
		defer cancel()
	}
	var request []byte
	called := false
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch m := msg.Message.(type) {
		case *frameworkv1.RoutedCallRequest_RequestMessage:
			if called {
				_ = stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
					End: &frameworkv1.AppCallEnd{GrpcStatusCode: int32(codes.Unimplemented), Detail: "system apps do not accept bidirectional request streams"},
				}})
				return nil
			}
			request = m.RequestMessage
			called = true
		case *frameworkv1.RoutedCallRequest_HalfClose:
			goto invoke
		case *frameworkv1.RoutedCallRequest_Start:
			return status.Error(codes.InvalidArgument, "RoutedCallStart must be the only start message")
		}
	}
invoke:
	responses, err := target.handler(ctx, start.GetGrpcService(), start.GetRpcName(), request)
	if err != nil {
		code := codes.Internal
		detail := err.Error()
		if st, ok := status.FromError(err); ok {
			code = st.Code()
			detail = st.Message()
		}
		_ = stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
			End: &frameworkv1.AppCallEnd{GrpcStatusCode: int32(code), Detail: detail},
		}})
		return nil
	}
	for _, resp := range responses {
		if err := stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_ResponseMessage{
			ResponseMessage: resp,
		}}); err != nil {
			return err
		}
	}
	return stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
		End: &frameworkv1.AppCallEnd{GrpcStatusCode: int32(codes.OK)},
	}})
}

func (b *Bus) pumpRequests(stream frameworkv1.AppBusService_CallServer, target *conn, cl *call, fail func(codes.Code, string)) {
	halfClosed := false
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			if !halfClosed {
				_ = target.enqueue(&frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_CallHalfClose{
					CallHalfClose: &frameworkv1.IncomingCallHalfClose{CallId: cl.id},
				}})
			}
			return
		}
		if err != nil {
			fail(codes.Canceled, "caller request stream failed")
			return
		}
		switch m := msg.Message.(type) {
		case *frameworkv1.RoutedCallRequest_RequestMessage:
			if err := target.enqueue(&frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_CallData{
				CallData: &frameworkv1.IncomingCallData{CallId: cl.id, RequestMessage: m.RequestMessage},
			}}); err != nil {
				fail(codes.ResourceExhausted, err.Error())
				return
			}
		case *frameworkv1.RoutedCallRequest_HalfClose:
			halfClosed = true
			if err := target.enqueue(&frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_CallHalfClose{
				CallHalfClose: &frameworkv1.IncomingCallHalfClose{CallId: cl.id},
			}}); err != nil {
				fail(codes.ResourceExhausted, err.Error())
				return
			}
		case *frameworkv1.RoutedCallRequest_Start:
			fail(codes.InvalidArgument, "RoutedCallStart must be the only start message")
			return
		}
	}
}

func (b *Bus) handleReply(c *conn, reply *frameworkv1.AppCallReply) {
	if reply == nil {
		return
	}
	b.mu.Lock()
	cl := b.calls[reply.GetCallId()]
	b.mu.Unlock()
	if cl == nil || cl.targetRunID != c.runID {
		b.logs.Warnf("bus", c.appID, "reply for unknown call %q ignored", reply.GetCallId())
		return
	}
	switch r := reply.Result.(type) {
	case *frameworkv1.AppCallReply_ResponseMessage:
		if !cl.push(r.ResponseMessage) {
			cl.finish(int32(codes.ResourceExhausted), "caller is not consuming responses fast enough")
		}
	case *frameworkv1.AppCallReply_End:
		cl.finish(r.End.GetGrpcStatusCode(), r.End.GetDetail())
	case nil:
		cl.finish(int32(codes.Internal), "empty AppCallReply")
	}
}

func (b *Bus) removeCall(id string) {
	b.mu.Lock()
	delete(b.calls, id)
	b.mu.Unlock()
}

func (b *Bus) readyConn(appID string) (*conn, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byApp[appID]
	if !ok || !c.ready.Load() {
		return nil, false
	}
	return c, true
}

// --- outbound control events ----------------------------------------------

// Send delivers one inbound event to a run's connection.
func (b *Bus) Send(runID string, msg *frameworkv1.AppInbound) error {
	b.mu.Lock()
	c, ok := b.conns[runID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("run %s is not connected", runID)
	}
	return c.enqueue(msg)
}

// SendApp delivers one inbound event to a ready app's current run.
func (b *Bus) SendApp(appID string, msg *frameworkv1.AppInbound) error {
	c, ok := b.readyConn(appID)
	if !ok {
		return fmt.Errorf("app %s is not ready", appID)
	}
	return c.enqueue(msg)
}

// ReadyRun reports whether a run completed AppReady or is a registered
// in-process system app.
func (b *Bus) ReadyRun(runID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.conns[runID]; ok && c.ready.Load() {
		return true
	}
	for _, t := range b.internal {
		if t.runID == runID {
			return true
		}
	}
	return false
}

// ReadyApp returns the current ready run for an app.
func (b *Bus) ReadyApp(appID string) (string, bool) {
	if c, ok := b.readyConn(appID); ok {
		return c.runID, true
	}
	if t, ok := b.internalTarget(appID); ok {
		return t.runID, true
	}
	return "", false
}

// Connected reports whether a run has an established bus connection. A live
// in-process system app counts as connected for discovery.
func (b *Bus) Connected(runID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.conns[runID]; ok {
		return true
	}
	for _, t := range b.internal {
		if t.runID == runID {
			return true
		}
	}
	return false
}

// Close terminates every connection.
func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	all := make([]*conn, 0, len(b.conns))
	for _, c := range b.conns {
		all = append(all, c)
	}
	calls := make([]*call, 0, len(b.calls))
	for _, c := range b.calls {
		calls = append(calls, c)
	}
	b.calls = map[string]*call{}
	b.internal = map[string]*internalTarget{}
	b.mu.Unlock()
	for _, c := range all {
		c.close()
	}
	for _, c := range calls {
		c.finish(int32(codes.Unavailable), "daemon shutting down")
	}
}
