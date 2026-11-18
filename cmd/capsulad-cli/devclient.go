package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// devRequestTimeout bounds one development admin exchange (dial, request,
	// response headers and body), matching the previous http.Client timeout.
	devRequestTimeout = 3 * time.Second
	// devMaxResponse caps the development admin body read. The socket serves
	// small JSON documents; a larger body is reported instead of truncated.
	devMaxResponse = 1 << 20
)

// devExchange performs a single HTTP/1.1 request over the development admin
// Unix socket and returns the trimmed response body.
//
// It deliberately uses only the request writer and response reader from
// net/http rather than http.Client: the client Transport (and its HTTP/2
// branch) is the bulk of the CLI's net/http footprint, while these two
// functions already handle Content-Length, chunked and close-delimited bodies.
// The connection is closed after one exchange, so no idle pool is needed.
func devExchange(ctx context.Context, sock, method, path string, payload []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, devRequestTimeout)
	defer cancel()

	dialer := net.Dialer{Timeout: devRequestTimeout}
	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return "", fmt.Errorf("dev admin socket %s: %w", sock, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Honor cancellation after the dial as well. context.AfterFunc starts a
	// goroutine only when ctx is done. stop() runs before the deferred Close:
	// it unregisters a callback that has not started, while a callback that
	// has already started is not joined by stop but finishes immediately (a
	// SetDeadline on the then-closed conn is safe). Nothing long-lived is left
	// behind. The deadline interrupts a blocked request write, response header
	// read or body read without needing an http.Transport.
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stop()

	socketErr := func(err error) error {
		if errors.Is(ctx.Err(), context.Canceled) {
			err = context.Canceled
		}
		return fmt.Errorf("dev admin socket %s: %w", sock, err)
	}

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://capsulad"+path, body)
	if err != nil {
		return "", err
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		return "", socketErr(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return "", socketErr(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, devMaxResponse+1))
	if err != nil {
		return "", socketErr(err)
	}
	if len(raw) > devMaxResponse {
		return "", fmt.Errorf("dev admin response to %s exceeds %d bytes", path, devMaxResponse)
	}
	return strings.TrimSpace(string(raw)), nil
}
