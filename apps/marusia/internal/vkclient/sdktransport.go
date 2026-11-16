package vkclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"capsulad/sdk/capsulad"
)

// grpcHandle is the remote-client surface sdkTransport uses. It is satisfied by
// *capsulad.GRPCClient and replaced by a fake in tests that need to control
// connect timing.
type grpcHandle interface {
	Close() error
	Unary(ctx context.Context, service, method string, headers map[string]string,
		request []byte, timeout time.Duration) ([]byte, error)
	NewStream(ctx context.Context, service, method string, headers map[string]string,
		kind capsulad.CallKind, timeout time.Duration) (*capsulad.Stream, error)
}

// sdkTransport is the production adapter: every byte goes through the shared
// libcapsulad HTTP/1.1 and remote gRPC clients, so this binary links neither
// net/http, crypto/tls nor grpc-go.
type sdkTransport struct {
	origin string
	host   string
	caPEM  []byte
	// openGRPC is a test seam; nil uses capsulad.OpenGRPC.
	openGRPC func(ctx context.Context, env capsulad.Env, url string,
		opts capsulad.GRPCOptions) (grpcHandle, error)

	mu      sync.Mutex
	http    *capsulad.HTTPClient
	grpc    grpcHandle
	attempt *grpcAttempt
	closed  bool
}

// grpcAttempt is one in-flight connect. Waiters share its result and a Close
// never waits for it.
type grpcAttempt struct {
	done   chan struct{}
	client grpcHandle
	err    error
}

// NewSDKTransport returns the native transport for a validated base URL.
// caPEM adds test/enterprise trust roots; nil uses the system store. Nothing
// connects until the first request, matching New's no-I/O contract.
func NewSDKTransport(baseURL string, caPEM []byte) (Transport, error) {
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	port := base.Port()
	if port == "" {
		if base.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return &sdkTransport{
		origin: fmt.Sprintf("%s://%s", base.Scheme, net.JoinHostPort(base.Hostname(), port)),
		host:   base.Hostname(),
		caPEM:  caPEM,
	}, nil
}

func (t *sdkTransport) httpClient() (*capsulad.HTTPClient, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, ErrClosed
	}
	if t.http != nil {
		return t.http, nil
	}
	client, err := capsulad.OpenHTTP(capsulad.FromEnv(), capsulad.HTTPOptions{
		Roots:           "system",
		CAPEM:           t.caPEM,
		UserAgent:       "voice-agent/1",
		MaxBody:         10 << 20,
		FollowRedirects: 4,
		ConnectTimeout:  20 * time.Second,
		ReadTimeout:     30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	t.http = client
	return client, nil
}

func (t *sdkTransport) openGRPCClient(ctx context.Context) (grpcHandle, error) {
	options := capsulad.GRPCOptions{
		Roots:          "system",
		CAPEM:          t.caPEM,
		ServerName:     t.host,
		ConnectTimeout: 10 * time.Second,
	}
	if t.openGRPC != nil {
		return t.openGRPC(ctx, capsulad.FromEnv(), t.origin, options)
	}
	return capsulad.OpenGRPC(ctx, capsulad.FromEnv(), t.origin, options)
}

// grpcClient connects once. The connect runs outside t.mu, so Close and other
// transport users are never blocked behind DNS/TCP/TLS. Concurrent first calls
// share one attempt and its result; a connect that finishes after Close is
// closed instead of installed.
func (t *sdkTransport) grpcClient(ctx context.Context) (grpcHandle, error) {
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return nil, ErrClosed
		}
		if t.grpc != nil {
			client := t.grpc
			t.mu.Unlock()
			return client, nil
		}
		if attempt := t.attempt; attempt != nil {
			t.mu.Unlock()
			select {
			case <-attempt.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if attempt.err != nil {
				return nil, attempt.err
			}
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if closed {
				return nil, ErrClosed
			}
			return attempt.client, nil
		}
		attempt := &grpcAttempt{done: make(chan struct{})}
		t.attempt = attempt
		t.mu.Unlock()

		client, err := t.openGRPCClient(ctx)
		if err != nil {
			attempt.err = mapSDKError("connect", err)
		} else {
			attempt.client = client
		}

		t.mu.Lock()
		if t.attempt == attempt {
			t.attempt = nil
		}
		installed := false
		if attempt.err == nil {
			if !t.closed {
				t.grpc = client
				installed = true
			}
		}
		t.mu.Unlock()
		if attempt.err == nil && !installed {
			_ = client.Close()
		}
		close(attempt.done)
		if attempt.err != nil {
			return nil, attempt.err
		}
		if !installed {
			return nil, ErrClosed
		}
		return client, nil
	}
}

func (t *sdkTransport) Do(ctx context.Context, req HTTPRequest, timeout time.Duration) (*HTTPResponse, error) {
	client, err := t.httpClient()
	if err != nil {
		return nil, err
	}
	response, err := client.Do(ctx, capsulad.HTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Header:  req.Header,
		Body:    req.Body,
		Timeout: timeout,
	})
	if err != nil {
		return nil, mapSDKError("http", err)
	}
	return &HTTPResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header,
		Body:       response.Body,
	}, nil
}

func (t *sdkTransport) Unary(ctx context.Context, service, method string,
	headers map[string]string, request []byte, timeout time.Duration) ([]byte, error) {
	client, err := t.grpcClient(ctx)
	if err != nil {
		return nil, err
	}
	response, err := client.Unary(ctx, service, method, headers, request, timeout)
	if err != nil {
		return nil, mapSDKError(method, err)
	}
	return response, nil
}

func (t *sdkTransport) NewStream(ctx context.Context, service, method string,
	headers map[string]string, kind CallKind, timeout time.Duration) (Stream, error) {
	client, err := t.grpcClient(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := client.NewStream(ctx, service, method, headers, capsulad.CallKind(kind), timeout)
	if err != nil {
		return nil, mapSDKError(method, err)
	}
	return sdkStream{stream: stream}, nil
}

// Close marks the transport closed and releases already-opened clients. It
// never waits for an in-flight connect: that connect closes its own result when
// it observes closed.
func (t *sdkTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	httpClient, grpcClient := t.http, t.grpc
	t.http, t.grpc = nil, nil
	t.mu.Unlock()
	var first error
	if httpClient != nil {
		if err := httpClient.Close(); err != nil {
			first = err
		}
	}
	if grpcClient != nil {
		if err := grpcClient.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type sdkStream struct {
	stream *capsulad.Stream
}

func (s sdkStream) Send(payload []byte) error {
	return mapSDKError("send", s.stream.Send(payload))
}

func (s sdkStream) Recv() ([]byte, error) {
	payload, err := s.stream.Recv()
	return payload, mapSDKError("recv", err)
}

func (s sdkStream) CloseSend() error {
	return mapSDKError("close send", s.stream.CloseSend())
}

func (s sdkStream) Close() error {
	return s.stream.Close()
}

// Native status values used to classify failures that are not gRPC statuses.
// capsulad.StatusError distinguishes them with NativeStatus because the
// numeric ranges overlap with canonical gRPC codes.
const (
	nativeInvalidArgument = 1
	nativeClosed          = 4
	nativeTransport       = 7
	nativeTimeout         = 10
	nativeCancelled       = 11
	nativeTooLarge        = 15
)

// mapSDKError converts the SDK's status errors into vkclient's own, so callers
// keep branching on canonical codes and never import grpc-go.
func mapSDKError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var statusErr *capsulad.StatusError
	if errors.As(err, &statusErr) {
		code := Code(statusErr.Code)
		if statusErr.NativeStatus {
			switch statusErr.Code {
			case nativeTimeout:
				code = CodeDeadlineExceeded
			case nativeTransport, nativeClosed:
				code = CodeUnavailable
			case nativeCancelled:
				code = CodeCanceled
			case nativeInvalidArgument:
				code = CodeInvalidArgument
			case nativeTooLarge:
				code = CodeResourceExhausted
			default:
				code = CodeInternal
			}
		}
		return &StatusError{Op: op, Code: code, Message: statusErr.Message}
	}
	if errors.Is(err, capsulad.ErrClosed) {
		return &StatusError{Op: op, Code: CodeCanceled, Message: "stream closed"}
	}
	return err
}
