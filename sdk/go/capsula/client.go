// Package capsula is the Go SDK for supervised Capsula applications.
//
// An app connects to the single framework Unix socket, opens one long-lived
// AppBusService.Connect stream, identifies itself with the launcher-assigned
// run ID, and signals READY. The SDK serves incoming custom calls, launch
// invocations, settings changes, actions and assistant activations over that
// stream and provides helpers for presentations, routing and audio.
package capsula

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

type Env struct {
	Socket string
	RunID  string
	AppID  string
	SDK    string
}

// FromEnv reads the launcher-assigned identity.
func FromEnv() Env {
	return Env{
		Socket: envOr("CAPSULA_SOCKET", "/run/capsulad/hal.sock"),
		RunID:  os.Getenv("CAPSULA_RUN_ID"),
		AppID:  os.Getenv("CAPSULA_APP_ID"),
		SDK:    os.Getenv("CAPSULA_SDK"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Handlers receive framework events on the app's bus connection.
type Handlers struct {
	Invocation   func(context.Context, *frameworkv1.AppLaunchInvocation)
	Settings     func(*frameworkv1.AppSettingsChanged)
	Action       func(context.Context, *frameworkv1.AppActionInvoked)
	Activation   func(context.Context, *frameworkv1.AssistantActivation)
	TouchRelease func(*frameworkv1.AssistantInputReleased)
	Ready        func()
	Disconnected func(error)
}

// CallHandler serves one custom routed call. req yields serialized request
// messages; send serialized response messages through ServerCall.
type CallHandler func(ctx context.Context, call *ServerCall)

type Client struct {
	socket string
	runID  string
	appID  string

	conn *grpc.ClientConn
	bus  frameworkv1.AppBusServiceClient

	mu       sync.Mutex
	handlers map[string]map[string]CallHandler
	h        Handlers
	calls    map[string]*serverCall
	send     chan *frameworkv1.AppOutbound
	done     chan struct{}
	once     sync.Once
	err      error
	started  bool
	ready    bool
}

func Connect(ctx context.Context, socket, runID string) (*Client, error) {
	if socket == "" || runID == "" {
		return nil, fmt.Errorf("socket and run_id are required")
	}
	conn, err := grpc.NewClient("passthrough:///capsulad",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}),
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return inv(metadata.AppendToOutgoingContext(ctx, "capsula-run-id", runID), method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(metadata.AppendToOutgoingContext(ctx, "capsula-run-id", runID), desc, cc, method, opts...)
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20), grpc.MaxCallSendMsgSize(8<<20)),
	)
	if err != nil {
		return nil, err
	}
	c := &Client{
		socket: socket, runID: runID, conn: conn,
		bus:      frameworkv1.NewAppBusServiceClient(conn),
		handlers: map[string]map[string]CallHandler{},
		calls:    map[string]*serverCall{},
		send:     make(chan *frameworkv1.AppOutbound, 128),
		done:     make(chan struct{}),
	}
	c.appID = os.Getenv("CAPSULA_APP_ID")
	return c, nil
}

// Close closes the framework connection.
func (c *Client) Close() error {
	c.finish(nil)
	return c.conn.Close()
}

func (c *Client) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
		if c.h.Disconnected != nil {
			c.h.Disconnected(err)
		}
	})
}

func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
func (c *Client) RunID() string { return c.runID }
func (c *Client) AppID() string { return c.appID }

// SetHandlers replaces lifecycle handlers before Start.
func (c *Client) SetHandlers(h Handlers) {
	c.mu.Lock()
	c.h = h
	c.mu.Unlock()
}

// Handle registers a custom routed method for this app.
func (c *Client) Handle(service, method string, handler CallHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handlers[service] == nil {
		c.handlers[service] = map[string]CallHandler{}
	}
	c.handlers[service][method] = handler
}

// Start opens the persistent bus stream and blocks until it disconnects.
func (c *Client) Start(ctx context.Context) error {
	stream, err := c.bus.Connect(ctx)
	if err != nil {
		c.finish(err)
		return err
	}
	if err := stream.Send(&frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_Hello{
		Hello: &frameworkv1.AppHello{RunId: c.runID},
	}}); err != nil {
		c.finish(err)
		return err
	}
	if err := stream.Send(&frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_Ready{Ready: &frameworkv1.AppReady{}}}); err != nil {
		c.finish(err)
		return err
	}
	go c.writer(stream)
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	if h := c.readyHandler(); h != nil {
		go h()
	}
	err = c.readLoop(stream)
	c.finish(err)
	return err
}

func (c *Client) readyHandler() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.h.Ready
}

func (c *Client) writer(stream frameworkv1.AppBusService_ConnectClient) {
	for {
		select {
		case <-c.done:
			return
		case <-stream.Context().Done():
			return
		case msg := <-c.send:
			if err := stream.Send(msg); err != nil {
				c.finish(err)
				return
			}
		}
	}
}

func (c *Client) enqueue(msg *frameworkv1.AppOutbound) bool {
	select {
	case <-c.done:
		return false
	case c.send <- msg:
		return true
	default:
		return false
	}
}

func (c *Client) readLoop(stream frameworkv1.AppBusService_ConnectClient) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		switch m := msg.Message.(type) {
		case *frameworkv1.AppInbound_Call:
			c.startCall(m.Call)
		case *frameworkv1.AppInbound_CallData:
			c.mu.Lock()
			call := c.calls[m.CallData.CallId]
			c.mu.Unlock()
			if call != nil {
				call.push(m.CallData.RequestMessage)
			}
		case *frameworkv1.AppInbound_CallHalfClose:
			c.mu.Lock()
			call := c.calls[m.CallHalfClose.CallId]
			c.mu.Unlock()
			if call != nil {
				call.halfClose()
			}
		case *frameworkv1.AppInbound_CallCancel:
			c.mu.Lock()
			call := c.calls[m.CallCancel.CallId]
			c.mu.Unlock()
			if call != nil {
				call.cancel()
			}
		case *frameworkv1.AppInbound_Invocation:
			c.callInvocation(m.Invocation)
		case *frameworkv1.AppInbound_SettingsChanged:
			c.mu.Lock()
			h := c.h.Settings
			c.mu.Unlock()
			if h != nil {
				h(m.SettingsChanged)
			}
		case *frameworkv1.AppInbound_Action:
			c.mu.Lock()
			h := c.h.Action
			c.mu.Unlock()
			if h != nil {
				go h(context.Background(), m.Action)
			}
		case *frameworkv1.AppInbound_Activation:
			c.mu.Lock()
			h := c.h.Activation
			c.mu.Unlock()
			if h != nil {
				go h(context.Background(), m.Activation)
			}
		case *frameworkv1.AppInbound_TouchReleased:
			c.mu.Lock()
			h := c.h.TouchRelease
			c.mu.Unlock()
			if h != nil {
				h(m.TouchReleased)
			}
		}
	}
}

func (c *Client) callInvocation(inv *frameworkv1.AppLaunchInvocation) {
	c.mu.Lock()
	h := c.h.Invocation
	c.mu.Unlock()
	if h != nil {
		go h(context.Background(), inv)
	}
}

// NotifyInvocationResult reports the asynchronous outcome of a launch.
func (c *Client) NotifyInvocationResult(invocationID string, accepted bool, detail string) error {
	if !c.enqueue(&frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_InvocationResult{
		InvocationResult: &frameworkv1.AppInvocationResult{InvocationId: invocationID, Accepted: accepted, Detail: detail},
	}}) {
		return fmt.Errorf("bus send queue is full")
	}
	return nil
}

// NotifyActionResult reports an action button outcome.
func (c *Client) NotifyActionResult(invocationID string, succeeded bool, detail string) error {
	if !c.enqueue(&frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_ActionResult{
		ActionResult: &frameworkv1.AppActionResult{ActionInvocationId: invocationID, Succeeded: succeeded, Detail: detail},
	}}) {
		return fmt.Errorf("bus send queue is full")
	}
	return nil
}

// --- server side of routed calls -------------------------------------------

// ServerCall is the app-side handler view of one routed call.
type ServerCall = serverCall

type serverCall struct {
	id       string
	ctx      context.Context
	cancelFn context.CancelFunc
	client   *Client
	mu       sync.Mutex
	chunks   chan []byte
	half     bool
	ended    bool
}

func (c *Client) startCall(in *frameworkv1.IncomingAppCall) {
	ctx, cancel := context.WithCancel(context.Background())
	sc := &serverCall{id: in.CallId, ctx: ctx, cancelFn: cancel, client: c, chunks: make(chan []byte, 16)}
	c.mu.Lock()
	c.calls[in.CallId] = sc
	handler := c.handlers[in.GrpcService][in.RpcName]
	c.mu.Unlock()
	if handler == nil {
		_ = sc.sendEnd(12, fmt.Sprintf("method %s.%s is not implemented by this app", in.GrpcService, in.RpcName))
		c.removeCall(in.CallId)
		return
	}
	go func() {
		handler(ctx, sc)
		sc.finish()
		c.removeCall(in.CallId)
	}()
}

func (c *Client) removeCall(id string) {
	c.mu.Lock()
	delete(c.calls, id)
	c.mu.Unlock()
}

// Recv returns the next request message; io.EOF indicates half-close.
func (s *serverCall) Recv() ([]byte, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case b, ok := <-s.chunks:
		if !ok {
			return nil, io.EOF
		}
		return b, nil
	}
}

// Send emits one serialized response message.
func (s *serverCall) Send(response []byte) error {
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if ended {
		return fmt.Errorf("call already ended")
	}
	if !s.client.enqueue(&frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_CallReply{
		CallReply: &frameworkv1.AppCallReply{CallId: s.id, Result: &frameworkv1.AppCallReply_ResponseMessage{ResponseMessage: response}},
	}}) {
		return fmt.Errorf("bus send queue is full")
	}
	return nil
}

// End terminates the call with a canonical gRPC status code.
func (s *serverCall) End(code int32, detail string) error {
	return s.sendEnd(code, detail)
}

func (s *serverCall) sendEnd(code int32, detail string) error {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil
	}
	s.ended = true
	s.mu.Unlock()
	ok := s.client.enqueue(&frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_CallReply{
		CallReply: &frameworkv1.AppCallReply{CallId: s.id, Result: &frameworkv1.AppCallReply_End{
			End: &frameworkv1.AppCallEnd{GrpcStatusCode: code, Detail: detail},
		}},
	}})
	if !ok {
		return fmt.Errorf("bus send queue is full")
	}
	return nil
}

func (s *serverCall) finish() {
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if !ended {
		_ = s.sendEnd(0, "")
	}
	s.cancelFn()
}

func (s *serverCall) push(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.half {
		return
	}
	select {
	case s.chunks <- data:
	default:
		go func() {
			_ = s.sendEnd(8, "app request buffer is full")
			s.cancelFn()
		}()
	}
}

func (s *serverCall) halfClose() {
	s.mu.Lock()
	if s.half {
		s.mu.Unlock()
		return
	}
	s.half = true
	close(s.chunks)
	s.mu.Unlock()
}

func (s *serverCall) cancel() {
	s.mu.Lock()
	if !s.half {
		s.half = true
		close(s.chunks)
	}
	s.mu.Unlock()
	s.cancelFn()
}
