package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// devTestSocket prepares a data directory whose development admin socket path
// is short enough for the platform's sockaddr_un limit.
func devTestSocket(t *testing.T) (dataDir, sock string) {
	t.Helper()
	dataDir, err := os.MkdirTemp("", "capcdev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	if err := os.MkdirAll(filepath.Join(dataDir, "run"), 0o750); err != nil {
		t.Fatal(err)
	}
	return dataDir, filepath.Join(dataDir, "run", "dev.sock")
}

func serveDev(t *testing.T, sock string, h http.Handler) {
	t.Helper()
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(lis)
		close(done)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
}

// TestDevExchangeChunkedBody covers the response shape the daemon's writeJSON
// produces for a populated /state: no Content-Length, so once the document
// exceeds the server's 2 KiB header-buffer the body arrives with chunked
// framing. The consumer must print the document, never the frame sizes.
func TestDevExchangeChunkedBody(t *testing.T) {
	_, sock := devTestSocket(t)
	want := `{"platform":"simulator","settings":{"wakeword":"hey_capsula"},` +
		`"routing":{"media":"builtin.media"},"ring":[` + strings.Repeat(`{"pixel":0},`, 200) + `{}]}`
	srv := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, want[:len(want)/2])
		flusher.Flush() // forces Transfer-Encoding: chunked
		_, _ = io.WriteString(w, want[len(want)/2:]+"\n")
	})
	serveDev(t, sock, srv)

	got, err := devExchange(context.Background(), sock, "GET", "/state", nil)
	if err != nil {
		t.Fatalf("devExchange: %v", err)
	}
	if got != want {
		t.Fatalf("chunked body decoded to %q, want %q", got, want)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("chunked body is not the JSON document: %v", err)
	}
}

// TestDevExchangeRawChunked wire-checks the same edge with hand-written framing
// split across several chunks, independent of the test server's buffering.
func TestDevExchangeRawChunked(t *testing.T) {
	_, sock := devTestSocket(t)
	body := `{"state":"` + strings.Repeat("y", 4096) + `"}`
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		for { // consume the request head before answering
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		var out strings.Builder
		out.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
		for _, part := range []string{body[:100], body[100:2000], body[2000:]} {
			fmt.Fprintf(&out, "%x\r\n%s\r\n", len(part), part)
		}
		out.WriteString("0\r\n\r\n")
		_, _ = io.WriteString(conn, out.String())
	}()

	got, err := devExchange(context.Background(), sock, "GET", "/state", nil)
	if err != nil {
		t.Fatalf("devExchange: %v", err)
	}
	if got != body {
		t.Fatalf("chunked body decoded to %d bytes, want %d", len(got), len(body))
	}
}

// TestDevExchangeRequestSemantics pins method, path, Content-Length framing and
// the JSON body the command has always sent to the admin socket.
func TestDevExchangeRequestSemantics(t *testing.T) {
	_, sock := devTestSocket(t)
	var seen struct {
		method, path, contentType string
		contentLength             int64
		transferEncoding          []string
		payload                   []byte
	}
	serveDev(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.method, seen.path = r.Method, r.URL.Path
		seen.contentType = r.Header.Get("Content-Type")
		seen.contentLength = r.ContentLength
		seen.transferEncoding = r.TransferEncoding
		seen.payload, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	payload := []byte(`{"key":"play_pause","pressed":true}`)
	got, err := devExchange(context.Background(), sock, "POST", "/input/key", payload)
	if err != nil {
		t.Fatalf("devExchange: %v", err)
	}
	if got != `{"ok":true}` {
		t.Fatalf("body = %q, want %q", got, `{"ok":true}`)
	}
	if seen.method != "POST" || seen.path != "/input/key" {
		t.Fatalf("request = %s %s, want POST /input/key", seen.method, seen.path)
	}
	if seen.contentLength != int64(len(payload)) || len(seen.transferEncoding) != 0 {
		t.Fatalf("body framing = ContentLength %d, TransferEncoding %v; want %d and none",
			seen.contentLength, seen.transferEncoding, len(payload))
	}
	if string(seen.payload) != string(payload) {
		t.Fatalf("payload = %q, want %q", seen.payload, payload)
	}
}

// TestDevExchangeErrorStatus keeps the historical consumer behavior: a non-2xx
// admin response is printed as the command output, not turned into an error.
func TestDevExchangeErrorStatus(t *testing.T) {
	_, sock := devTestSocket(t)
	serveDev(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "admin injection disabled", http.StatusServiceUnavailable)
	}))

	got, err := devExchange(context.Background(), sock, "POST", "/wakeword", []byte(`{"model":""}`))
	if err != nil {
		t.Fatalf("error status returned an error: %v", err)
	}
	if got != "admin injection disabled" {
		t.Fatalf("body = %q, want the error document", got)
	}
}

func TestDevExchangeDialFailure(t *testing.T) {
	_, sock := devTestSocket(t) // no listener: dial is refused
	_, err := devExchange(context.Background(), sock, "GET", "/state", nil)
	if err == nil {
		t.Fatal("missing admin socket dialed successfully")
	}
	if !strings.Contains(err.Error(), "dev admin socket") {
		t.Fatalf("error = %q, want it to name the admin socket", err)
	}
}

func TestDevExchangeDeadline(t *testing.T) {
	_, sock := devTestSocket(t)
	block := make(chan struct{})
	serveDev(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := devExchange(ctx, sock, "GET", "/state", nil)
	if err == nil {
		t.Fatal("hung admin socket returned no error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("deadline not honored: %v", elapsed)
	}
}

// TestDevExchangeCancellation pins the post-dial cancellation contract: a
// caller that cancels a context with no deadline of its own must interrupt a
// response the server never sends, rather than waiting for the 3 s bound. The
// test server signals once the request head is consumed, so the cancel lands
// while devExchange is blocked reading response headers.
func TestDevExchangeCancellation(t *testing.T) {
	_, sock := devTestSocket(t)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	requestSeen := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		close(requestSeen)
		// Hold the connection open without answering until the client aborts.
		_, _ = io.Copy(io.Discard, br)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		body, err := devExchange(ctx, sock, "GET", "/state", nil)
		results <- result{body: body, err: err}
	}()

	select {
	case <-requestSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the test server")
	}
	start := time.Now()
	cancel()

	var got result
	select {
	case got = <-results:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled exchange did not return")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation not honored promptly: %v", elapsed)
	}
	if got.err == nil {
		t.Fatalf("cancelled exchange returned body %q with no error", got.body)
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", got.err)
	}
	if !strings.Contains(got.err.Error(), "dev admin socket") {
		t.Fatalf("error = %q, want it to name the admin socket", got.err)
	}
	<-serverDone
}

func TestDevExchangeBodyLimit(t *testing.T) {
	_, sock := devTestSocket(t)
	serveDev(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat("x", devMaxResponse+1))
	}))

	if _, err := devExchange(context.Background(), sock, "GET", "/state", nil); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body error = %v, want a bounded-read failure", err)
	}
}

// TestRunDevStatePrintsChunkedJSON is the consumer-visible regression: a
// populated /state above 2 KiB must print exactly the JSON document.
func TestRunDevStatePrintsChunkedJSON(t *testing.T) {
	dataDir, sock := devTestSocket(t)
	settings := map[string]any{}
	for i := range 40 {
		settings[fmt.Sprintf("app.setting.%02d", i)] = strings.Repeat("v", 40)
	}
	state := map[string]any{
		"platform": "simulator",
		"settings": settings,
		"routing":  map[string]string{"media": "builtin.media"},
	}
	document, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	document = append(document, '\n')
	if len(document) <= 2048 {
		t.Fatalf("fixture is %d bytes; it must exceed the server's 2 KiB buffer", len(document))
	}
	serveDev(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document) // no Content-Length: net/http uses chunked
	}))

	out, err := captureStdout(t, func() error { return runDev([]string{"state"}, dataDir) })
	if err != nil {
		t.Fatalf("runDev state: %v", err)
	}
	if want := strings.TrimSpace(string(document)) + "\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &map[string]any{}); err != nil {
		t.Fatalf("stdout is not the JSON document: %v", err)
	}
}

// TestRunDevKeySendsJSON keeps the injected-event path (POST with a JSON body)
// printing the daemon's answer through the character-level CLI output.
func TestRunDevKeySendsJSON(t *testing.T) {
	dataDir, sock := devTestSocket(t)
	var gotKey string
	var gotPressed bool
	serveDev(t, sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key     string `json:"key"`
			Pressed bool   `json:"pressed"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotKey, gotPressed = req.Key, req.Pressed
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\n  \"ok\": true\n}\n")
	}))

	out, err := captureStdout(t, func() error { return runDev([]string{"key", "play_pause", "true"}, dataDir) })
	if err != nil {
		t.Fatalf("runDev key: %v", err)
	}
	if gotKey != "play_pause" || !gotPressed {
		t.Fatalf("injected key = (%q, %v), want (play_pause, true)", gotKey, gotPressed)
	}
	if want := "{\n  \"ok\": true\n}\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestRunDevUnknownSubcommand(t *testing.T) {
	dataDir, _ := devTestSocket(t)
	if err := runDev([]string{"bogus"}, dataDir); err == nil || !strings.Contains(err.Error(), "unknown dev command") {
		t.Fatalf("error = %v, want unknown dev command", err)
	}
}
