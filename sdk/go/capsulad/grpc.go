package capsulad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// GRPCOptions configures one remote gRPC channel. TLS always verifies.
type GRPCOptions struct {
	// Roots is "system" (default), "webpki", or "pem" (CAPEM only; no system
	// bundle and no public roots, and CAPEM must supply at least one CA).
	Roots string
	// CAPEM adds PEM-encoded certificates to the verification roots.
	CAPEM []byte
	// ServerName overrides the TLS verification name; empty uses the URL host.
	ServerName string
	// ConnectTimeout bounds DNS, TCP, TLS and the HTTP/2 handshake. Zero uses
	// 10 s.
	ConnectTimeout time.Duration
}

func (o GRPCOptions) encode() ([]byte, error) {
	options := map[string]any{}
	if o.Roots != "" {
		options["roots"] = o.Roots
	}
	if len(o.CAPEM) > 0 {
		options["ca_pem"] = string(o.CAPEM)
	}
	if o.ServerName != "" {
		options["server_name"] = o.ServerName
	}
	if o.ConnectTimeout < 0 {
		return nil, errors.New("capsulad: GRPCOptions.ConnectTimeout must not be negative")
	}
	if o.ConnectTimeout > 0 {
		options["connect_timeout_ms"] = durationMillis(o.ConnectTimeout)
	}
	return json.Marshal(options)
}

// CallKind is the declared gRPC call shape.
type CallKind int32

const (
	CallUnary CallKind = iota
	CallServerStream
	CallClientStream
	CallBidi
)

// GRPCClient is a remote gRPC channel. It is a normal native client handle:
// calls and streams use the existing machinery, but it has no app bus and no
// run id, so routed calls are rejected.
type GRPCClient struct {
	mu     sync.Mutex
	handle uint64
}

// OpenGRPC loads the shared engine and connects to an origin
// (https://host[:port] or http://host[:port]). The connection is established
// before the call returns; ctx only bounds the wait before it starts.
func OpenGRPC(ctx context.Context, env Env, url string, opts GRPCOptions) (*GRPCClient, error) {
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
	timeoutMS := timeoutMillis(ctx, opts.ConnectTimeout)
	handle, err := grpcOpenNative(url, options, timeoutMS)
	if err != nil {
		return nil, err
	}
	return &GRPCClient{handle: handle}, nil
}

// Close releases the channel and any calls it owns. It is idempotent.
func (c *GRPCClient) Close() error {
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
	return closeNative(handle)
}

func (c *GRPCClient) value() (uint64, error) {
	if c == nil {
		return 0, ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handle == 0 {
		return 0, ErrClosed
	}
	return c.handle, nil
}

func encodeMetadata(headers map[string]string) ([]byte, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	return json.Marshal(headers)
}

// NewStream starts a raw-protobuf call with caller metadata. The returned
// Stream uses the normal Send/Recv/HalfClose/Close surface; Close cancels.
func (c *GRPCClient) NewStream(ctx context.Context, service, method string,
	headers map[string]string, kind CallKind, timeout time.Duration) (*Stream, error) {
	ctx = orBackground(ctx)
	handle, err := c.value()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata, err := encodeMetadata(headers)
	if err != nil {
		return nil, fmt.Errorf("capsulad: encode metadata: %w", err)
	}
	call, err := callStartMetaNative(handle, service, method, metadata, int32(kind),
		timeoutMillis(ctx, timeout))
	if err != nil {
		return nil, err
	}
	return &Stream{call: newCallHandle(call, ctx, service+"/"+method)}, nil
}

// Unary performs one unary call and returns its single response message.
func (c *GRPCClient) Unary(ctx context.Context, service, method string,
	headers map[string]string, request []byte, timeout time.Duration) ([]byte, error) {
	ctx = orBackground(ctx)
	stream, err := c.NewStream(ctx, service, method, headers, CallUnary, timeout)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	if err := stream.Send(request); err != nil {
		return nil, err
	}
	if err := stream.HalfClose(); err != nil {
		return nil, err
	}
	var first []byte
	for {
		message, err := stream.Recv()
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
