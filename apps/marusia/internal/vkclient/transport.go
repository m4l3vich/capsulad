package vkclient

import (
	"context"
	"time"
)

// The transport seam below is the only network surface vkclient uses in
// production. It is implemented by the native capsulad SDK (no bundled
// net/http, crypto/tls or grpc-go) and replaced in tests by an in-process
// adapter, so the protocol code is exercised identically in both.
//
// Service and method names are the recovered wire names:
// vk.voice.grpc.front.v1.service.Phrase/Speech and
// vk.voice.grpc.front.v1.service.ClientProbe/AuthReady.

const (
	phraseService = "vk.voice.grpc.front.v1.service.Phrase"
	probeService  = "vk.voice.grpc.front.v1.service.ClientProbe"
)

// CallKind is the declared gRPC call shape of a stream.
type CallKind int32

const (
	CallUnary CallKind = iota
	CallServerStream
	CallClientStream
	CallBidi
)

// HTTPRequest is one attach/media request.
type HTTPRequest struct {
	Method string
	URL    string
	Header map[string]string
	Body   []byte
	// NoRedirects disables redirect following for this request. The phrase
	// result query carries credential-class identifiers; a cross-origin
	// Location must not receive them, even though the transport strips
	// headers on a cross-origin hop.
	NoRedirects bool
}

// HTTPResponse is a completed exchange; non-2xx is not an error.
type HTTPResponse struct {
	StatusCode int
	Header     map[string]string
	Body       []byte
}

// Stream is one raw-protobuf gRPC stream. Send/Recv carry marshalled messages;
// CloseSend half-closes the request side and Close cancels the call.
type Stream interface {
	Send(payload []byte) error
	Recv() ([]byte, error)
	CloseSend() error
	Close() error
}

// Transport performs every network operation vkclient needs.
type Transport interface {
	Do(ctx context.Context, req HTTPRequest, timeout time.Duration) (*HTTPResponse, error)
	Unary(ctx context.Context, service, method string, headers map[string]string,
		request []byte, timeout time.Duration) ([]byte, error)
	NewStream(ctx context.Context, service, method string, headers map[string]string,
		kind CallKind, timeout time.Duration) (Stream, error)
	Close() error
}
