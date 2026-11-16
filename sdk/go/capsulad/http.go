package capsulad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPOptions configures one native HTTP/1.1 client. Zero values mean the
// native defaults except FollowRedirects, which mirrors the C ABI: 0 disables
// redirects and the native default is 4, so set 4 explicitly to follow them.
type HTTPOptions struct {
	// Roots is "system" (default), "webpki", or "pem". "pem" trusts only CAPEM
	// (no system bundle, no public roots) and requires a non-empty CAPEM.
	Roots string
	// CAPEM adds PEM-encoded certificates to the verification roots.
	CAPEM []byte
	// UserAgent is the default User-Agent; empty selects capsulad-native/1.0.
	UserAgent string
	// MaxBody caps one response body. Zero uses 32 MiB; the hard cap is 64 MiB.
	MaxBody int64
	// FollowRedirects is the redirect budget; 0 disables redirects.
	FollowRedirects int
	// ConnectTimeout bounds DNS, TCP and TLS setup. Zero uses 15 s.
	ConnectTimeout time.Duration
	// ReadTimeout bounds each read (and each write). Zero uses 30 s.
	ReadTimeout time.Duration
	// TotalTimeout bounds the whole exchange, all redirect hops included.
	// Zero uses the native default of 120 s; it does not mean "no deadline".
	// HTTPRequest.Timeout overrides it per request.
	TotalTimeout time.Duration
}

// durationMillis encodes a positive duration in whole milliseconds, rounding
// sub-millisecond values up to 1 so they never become 0 ("use the default").
func durationMillis(d time.Duration) int64 {
	if ms := d.Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

func (o HTTPOptions) encode() ([]byte, error) {
	options := map[string]any{"follow_redirects": o.FollowRedirects}
	if o.Roots != "" {
		options["roots"] = o.Roots
	}
	if len(o.CAPEM) > 0 {
		options["ca_pem"] = string(o.CAPEM)
	}
	if o.UserAgent != "" {
		options["user_agent"] = o.UserAgent
	}
	if o.MaxBody < 0 {
		return nil, errors.New("capsulad: HTTPOptions.MaxBody must not be negative")
	}
	if o.MaxBody > 0 {
		options["max_body"] = o.MaxBody
	}
	if o.ConnectTimeout < 0 || o.ReadTimeout < 0 || o.TotalTimeout < 0 {
		return nil, errors.New("capsulad: HTTPOptions timeouts must not be negative")
	}
	if o.ConnectTimeout > 0 {
		options["connect_timeout_ms"] = durationMillis(o.ConnectTimeout)
	}
	if o.ReadTimeout > 0 {
		options["read_timeout_ms"] = durationMillis(o.ReadTimeout)
	}
	if o.TotalTimeout > 0 {
		options["total_timeout_ms"] = durationMillis(o.TotalTimeout)
	}
	return json.Marshal(options)
}

// HTTPClient is one native HTTP/1.1 client. It performs one request per Do and
// always verifies TLS. Close releases the native handle.
type HTTPClient struct {
	mu            sync.Mutex
	handle        uint64
	nextRequestID atomic.Uint64
}

// OpenHTTP loads the shared engine and opens an HTTP client. No network I/O
// happens here; TLS roots are read from disk.
func OpenHTTP(env Env, opts HTTPOptions) (*HTTPClient, error) {
	if err := ensureLibrary(env.Library); err != nil {
		return nil, err
	}
	options, err := opts.encode()
	if err != nil {
		return nil, err
	}
	handle, err := httpOpenNative(options, 0)
	if err != nil {
		return nil, err
	}
	return &HTTPClient{handle: handle}, nil
}

// Close cancels every in-flight request and then releases the handle. Called
// from Go it waits until no request can still use the native client state; the
// native engine skips that wait only when it is called from a Tokio runtime
// thread (requests then hold Arc snapshots, so the state stays valid). Close is
// idempotent.
func (c *HTTPClient) Close() error {
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
	return httpCloseNative(handle)
}

func (c *HTTPClient) value() (uint64, error) {
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

// HTTPRequest is one request. Header names and values are validated natively;
// host, content-length, transfer-encoding and connection cannot be set.
type HTTPRequest struct {
	Method string
	URL    string
	Header map[string]string
	Body   []byte
	// Timeout bounds the whole request (all redirect hops). Zero falls back to
	// HTTPOptions.TotalTimeout (native default 120 s); a context deadline
	// always wins.
	Timeout time.Duration
}

// HTTPResponse is a completed exchange. Non-2xx statuses are successful calls.
type HTTPResponse struct {
	StatusCode int
	Header     map[string]string
	Body       []byte
}

// Do performs one request. Each call has its own native request id, so
// cancelling ctx aborts only this request and never a concurrent one.
// Cancellation is sticky across the registration race: a cancel that arrives
// before the native request is registered is remembered (bounded to 64 ids) and
// the request fails with CANCELLED without touching the network. When the
// interrupted exchange reports cancellation, Do waits for the native call to
// return and then returns ctx.Err() if the context is still done, otherwise a
// *StatusError. Cancelling an id whose request already finished is a no-op, and
// an exchange that completes just as its watcher fires returns its response.
// Concurrent Do calls on one client are fully independent.
func (c *HTTPClient) Do(ctx context.Context, req HTTPRequest) (*HTTPResponse, error) {
	ctx = orBackground(ctx)
	handle, err := c.value()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	headerMap := req.Header
	if headerMap == nil {
		headerMap = map[string]string{}
	}
	headers, err := json.Marshal(headerMap)
	if err != nil {
		return nil, fmt.Errorf("capsulad: encode request headers: %w", err)
	}
	requestID := c.nextRequestID.Add(1)
	if requestID == 0 {
		requestID = c.nextRequestID.Add(1)
	}
	timeoutMS := timeoutMillis(ctx, req.Timeout)

	call := func() httpResult {
		return httpRequestNative(handle, requestID, req.Method, req.URL, headers, req.Body,
			timeoutMS)
	}

	var result httpResult
	if ctx.Done() == nil {
		result = call()
	} else {
		done := make(chan httpResult, 1)
		go func() { done <- call() }()
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = httpCancelNative(handle, requestID)
			case <-stop:
			}
		}()
		result = <-done
		close(stop)
	}
	if result.status == statusCancelled || (result.status == statusClosed && ctx.Err() != nil) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, &StatusError{
			Op:           "http request",
			Code:         statusCancelled,
			Message:      "request cancelled",
			NativeStatus: true,
		}
	}
	if result.status != statusOK {
		return nil, nativeStatusError("http request", result.status, result.detail)
	}
	response := &HTTPResponse{StatusCode: result.statusCode, Body: result.body}
	if len(result.headersJSON) > 0 {
		if err := json.Unmarshal(result.headersJSON, &response.Header); err != nil {
			return nil, fmt.Errorf("capsulad: decode response headers: %w", err)
		}
	}
	if response.Header == nil {
		response.Header = map[string]string{}
	}
	return response, nil
}
