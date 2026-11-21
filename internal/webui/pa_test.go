package webui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/websecurity"
)

// TestPAWireFormat pins the hand-written codecs against the PA protobuf
// schema: PlayRequest.data is field 1 (length-delimited), PlayEvent.credit is
// field 1 and PlayEvent.result field 2, and PlayResult fields 1..5 carry the
// documented values. The app's descriptor is published, not compiled in.
func TestPAWireFormat(t *testing.T) {
	frame := paPlayRequestFrame([]byte("abc"))
	want := []byte{0x0a, 0x03, 'a', 'b', 'c'}
	if !bytes.Equal(frame, want) {
		t.Fatalf("PlayRequest frame = %x, want %x", frame, want)
	}

	frames, result, err := decodePAPlayEvent(fakePAEventResult(fakePAResult(7, 16000, 2, "ok")))
	if err != nil {
		t.Fatal(err)
	}
	if frames != 0 {
		t.Fatalf("result event credit = %d, want 0", frames)
	}
	got, err := decodePAPlayResult(result)
	if err != nil {
		t.Fatal(err)
	}
	wantResult := paPlayResult{Played: true, Frames: 7, SampleRateHz: 16000, Channels: 2, Detail: "ok"}
	if got != wantResult {
		t.Fatalf("PlayResult = %+v, want %+v", got, wantResult)
	}

	frames, result, err = decodePAPlayEvent(fakePACredit(3))
	if err != nil {
		t.Fatal(err)
	}
	if frames != 3 || result != nil {
		t.Fatalf("credit event = frames %d result %d bytes, want 3 and nil", frames, len(result))
	}
}

// TestPADeadlineMapsTo504 pins the relay's deadline vocabulary: a local
// context deadline and a gRPC DeadlineExceeded status from start/send/recv
// must both become HTTP 504, not 502 or 400.
func TestPADeadlineMapsTo504(t *testing.T) {
	grpcDeadline := status.Error(codes.DeadlineExceeded, "routed call deadline exceeded")
	cases := []struct {
		name string
		err  error
	}{
		{"context", context.DeadlineExceeded},
		{"grpc", grpcDeadline},
		{"wrapped grpc", fmt.Errorf("recv: %w", grpcDeadline)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			transport := httptest.NewRecorder()
			srv.writePATransportError(transport, httptest.NewRequest(http.MethodPost, "/", nil), tc.err)
			if transport.Code != http.StatusGatewayTimeout {
				t.Fatalf("transport error = %d, want 504", transport.Code)
			}
			upload := httptest.NewRecorder()
			srv.writePAUploadError(upload, tc.err)
			if upload.Code != http.StatusGatewayTimeout {
				t.Fatalf("upload error = %d, want 504", upload.Code)
			}
		})
	}

	transport := httptest.NewRecorder()
	(&Server{}).writePATransportError(transport, httptest.NewRequest(http.MethodPost, "/", nil),
		status.Error(codes.Unavailable, "bus down"))
	if transport.Code != http.StatusBadGateway {
		t.Fatalf("unavailable = %d, want 502", transport.Code)
	}
}

// TestPACreditWaitsForGrant pins the core flow-control behavior: the sender
// must block until the app grants a frame, and a closed call unblocks it.
func TestPACreditWaitsForGrant(t *testing.T) {
	credit := newPACredit()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if credit.take(ctx) {
		t.Fatal("take succeeded without a grant")
	}
	credit.add(1)
	if !credit.take(context.Background()) {
		t.Fatal("take did not consume the granted frame")
	}
	credit.close()
	if credit.take(context.Background()) {
		t.Fatal("take succeeded after close")
	}
}

func paOperatorKey(t *testing.T, env *testEnv) string {
	t.Helper()
	token, _, err := env.store.CreateAPIKeyWithScopes("pa-operator", []string{websecurity.ScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func paStatusKey(t *testing.T, env *testEnv) string {
	t.Helper()
	token, _, err := env.store.CreateAPIKey("pa-status")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func paPost(t *testing.T, c *http.Client, env *testEnv, body io.Reader, headers map[string]string) (int, map[string]any) {
	t.Helper()
	return rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/apps/org.capsulad.pa/play", body, "audio/wav", headers)
}

func paOnlyCall(t *testing.T, env *testEnv) *fakePACall {
	t.Helper()
	env.back.mu.Lock()
	defer env.back.mu.Unlock()
	if len(env.back.paCalls) != 1 {
		t.Fatalf("PA calls = %d, want 1", len(env.back.paCalls))
	}
	return env.back.paCalls[0]
}

// TestPASpeakerHTTPRequiresOperatorCredential pins the route's two-credential
// contract: no credential is 401, a status key is 403, and an operator key
// reaches the app. It also proves the route addresses the PA app's own service
// rather than a generic audio or app-bus surface.
func TestPASpeakerHTTPRequiresOperatorCredential(t *testing.T) {
	env := newTestEnv(t)
	c := env.ts.Client()
	wav := []byte("RIFF....WAVEfmt ")

	code, body := paPost(t, c, env, bytes.NewReader(wav), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated play = %d %v, want 401", code, body)
	}

	statusKey := paStatusKey(t, env)
	code, body = paPost(t, c, env, bytes.NewReader(wav), map[string]string{"Authorization": "Bearer " + statusKey})
	if code != http.StatusForbidden {
		t.Fatalf("status-key play = %d %v, want 403", code, body)
	}

	operatorKey := paOperatorKey(t, env)
	code, body = paPost(t, c, env, bytes.NewReader(wav), map[string]string{
		"Authorization": "Bearer " + operatorKey,
		"Origin":        "https://attacker.example",
	})
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin operator play = %d %v, want 403", code, body)
	}

	code, body = paPost(t, c, env, bytes.NewReader(wav), map[string]string{"X-API-Key": operatorKey})
	if code != http.StatusOK {
		t.Fatalf("operator-key play = %d %v, want 200", code, body)
	}
	if body["played"] != true {
		t.Fatalf("play result = %v, want played true", body)
	}
	call := paOnlyCall(t, env)
	if call.TargetAppID != paAppID || call.Service != paSpeakerService || call.RPC != paPlayRPC {
		t.Fatalf("routed call = %+v, want the PA Speaker Play RPC", call)
	}
	if call.TimeoutMs != uint32(paPlayDeadline/time.Millisecond) {
		t.Fatalf("routed timeout = %d, want %d", call.TimeoutMs, paPlayDeadline/time.Millisecond)
	}
}

// TestPASpeakerHTTPStreamsWAVAndRelaysResult proves the body is split into
// PlayRequest frames, concatenates back to the exact file, and the app's
// PlayResult fields (including the camelCase sampleRateHz) reach the client.
func TestPASpeakerHTTPStreamsWAVAndRelaysResult(t *testing.T) {
	env := newTestEnv(t)
	body := bytes.Repeat([]byte{0x7a}, paRequestChunk+100)
	env.back.mu.Lock()
	env.back.paResult = fakePAResult(48000, 16000, 1, "played 48000 frames at 16000 Hz/1 ch")
	env.back.mu.Unlock()

	code, reply := paPost(t, env.ts.Client(), env, bytes.NewReader(body),
		map[string]string{"Authorization": "Bearer " + paOperatorKey(t, env)})
	if code != http.StatusOK {
		t.Fatalf("play = %d %v, want 200", code, reply)
	}
	if reply["frames"] != float64(48000) || reply["sampleRateHz"] != float64(16000) ||
		reply["channels"] != float64(1) || reply["detail"] != "played 48000 frames at 16000 Hz/1 ch" {
		t.Fatalf("play result = %v", reply)
	}
	call := paOnlyCall(t, env)
	if call.Frames < 2 {
		t.Fatalf("request frames = %d, want at least 2 for a multi-chunk body", call.Frames)
	}
	if !bytes.Equal(call.Data, body) {
		t.Fatalf("the app received %d bytes, want the exact %d-byte body", len(call.Data), len(body))
	}
}

// TestPASpeakerHTTPMapsAppRejections pins the documented status mapping. The
// app's detail is shown verbatim because it names the rejected WAV field.
func TestPASpeakerHTTPMapsAppRejections(t *testing.T) {
	env := newTestEnv(t)
	key := paOperatorKey(t, env)
	c := env.ts.Client()
	cases := []struct {
		code   codes.Code
		status int
	}{
		{codes.InvalidArgument, http.StatusBadRequest},
		{codes.ResourceExhausted, http.StatusRequestEntityTooLarge},
		{codes.DeadlineExceeded, http.StatusGatewayTimeout},
		{codes.FailedPrecondition, http.StatusServiceUnavailable},
		{codes.Unavailable, http.StatusServiceUnavailable},
		{codes.Canceled, http.StatusConflict},
		{codes.Unimplemented, http.StatusNotImplemented},
		{codes.Internal, http.StatusInternalServerError},
		{codes.PermissionDenied, http.StatusForbidden},
	}
	for _, tc := range cases {
		env.back.mu.Lock()
		env.back.paCode = tc.code
		env.back.paDetail = "rejected: " + tc.code.String()
		env.back.mu.Unlock()
		code, body := paPost(t, c, env, bytes.NewReader([]byte("RIFF")),
			map[string]string{"Authorization": "Bearer " + key})
		if code != tc.status {
			t.Fatalf("%s mapped to %d %v, want %d", tc.code, code, body, tc.status)
		}
		if body["error"] != "rejected: "+tc.code.String() {
			t.Fatalf("%s detail = %v", tc.code, body["error"])
		}
	}
}

// TestPASpeakerHTTPOnlyPOST pins the route's method: a GET is not a playback
// trigger and must be refused by the mux before authentication.
func TestPASpeakerHTTPOnlyPOST(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/apps/org.capsulad.pa/play", nil)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET play = %d, want 405", rec.Code)
	}
}

// TestPASpeakerHTTPRejectsOversizedContentLength refuses before opening a
// routed call, so a client that declares a body over the cap cannot start a
// playback that would then be truncated.
func TestPASpeakerHTTPRejectsOversizedContentLength(t *testing.T) {
	env := newTestEnv(t)
	key := paOperatorKey(t, env)
	req := httptest.NewRequest(http.MethodPost, "/api/apps/org.capsulad.pa/play", nil)
	req.ContentLength = paMaxWAVBytes + 1
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized play = %d, want 413", rec.Code)
	}
	env.back.mu.Lock()
	defer env.back.mu.Unlock()
	if len(env.back.paCalls) != 0 {
		t.Fatalf("oversized body opened %d routed calls, want none", len(env.back.paCalls))
	}
}

// TestPASpeakerHTTPBrowserSessionRequiresCSRF proves the browser half of the
// auth contract: a session without the CSRF header is refused and the same
// request with it is accepted.
func TestPASpeakerHTTPBrowserSessionRequiresCSRF(t *testing.T) {
	env := newTestEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	code, body := paPost(t, c, env, bytes.NewReader([]byte("RIFF")), nil)
	if code != http.StatusForbidden {
		t.Fatalf("session without CSRF = %d %v, want 403", code, body)
	}
	code, body = paPost(t, c, env, bytes.NewReader([]byte("RIFF")),
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("session with CSRF = %d %v, want 200", code, body)
	}
}

// TestPASpeakerHTTPRejectsMalformedAppResult proves a broken or unsuccessful
// PlayResult is not turned into a 200: the route reports a bad gateway instead
// of claiming playback.
func TestPASpeakerHTTPRejectsMalformedAppResult(t *testing.T) {
	env := newTestEnv(t)
	key := paOperatorKey(t, env)
	c := env.ts.Client()
	cases := []struct {
		name   string
		result []byte
	}{
		{"truncated field", []byte{0x0a, 0x80}},
		{"played false", []byte{0x08, 0x00}},
	}
	for _, tc := range cases {
		env.back.mu.Lock()
		env.back.paResult = tc.result
		env.back.mu.Unlock()
		code, body := paPost(t, c, env, bytes.NewReader([]byte("RIFF")),
			map[string]string{"Authorization": "Bearer " + key})
		if code != http.StatusBadGateway {
			t.Fatalf("%s: status = %d %v, want 502", tc.name, code, body)
		}
	}
}

// TestPASpeakerHTTPRealStalledUploadRejectsPromptly uses a real HTTP/1.1
// connection that sends a chunk, then pauses without finishing the request.
// The app rejects the bad bytes; the route must flush that rejection instead
// of waiting for the upload's read deadline. Both a declared Content-Length
// (where net/http can see the unread size) and a chunked body (where it cannot)
// are exercised.
func TestPASpeakerHTTPRealStalledUploadRejectsPromptly(t *testing.T) {
	env := newTestEnv(t)
	key := paOperatorKey(t, env)
	env.back.mu.Lock()
	env.back.paEndAfterFirst = true
	env.back.paCode = codes.InvalidArgument
	env.back.paDetail = "not a RIFF/WAVE file"
	env.back.mu.Unlock()

	cases := []struct {
		name    string
		chunked bool
	}{
		{"content-length", false},
		{"chunked", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := env.ts.Listener.Addr().String()
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr,
				&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

			body := bytes.Repeat([]byte{0x7a}, 64<<10)
			header := fmt.Sprintf("POST /api/apps/org.capsulad.pa/play HTTP/1.1\r\nHost: %s\r\n"+
				"Authorization: Bearer %s\r\nContent-Type: audio/wav\r\n", addr, key)
			if tc.chunked {
				header += "Transfer-Encoding: chunked\r\n\r\n"
			} else {
				header += fmt.Sprintf("Content-Length: %d\r\n\r\n", 10<<20)
			}
			if _, err := io.WriteString(conn, header); err != nil {
				t.Fatal(err)
			}
			if tc.chunked {
				if _, err := fmt.Fprintf(conn, "%x\r\n", len(body)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := conn.Write(body); err != nil {
				t.Fatal(err)
			}
			if tc.chunked {
				// Deliberately no terminating zero chunk: the client stalls.
				if _, err := io.WriteString(conn, "\r\n"); err != nil {
					t.Fatal(err)
				}
			}

			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatalf("early rejection did not arrive: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("early rejection status = %d, want 400", resp.StatusCode)
			}
			if !resp.Close {
				t.Fatalf("early rejection did not close the connection")
			}
		})
	}
}

type paAsyncResult struct {
	code int
	body map[string]any
	err  error
}

func paPostAsync(c *http.Client, target string, body []byte, headers map[string]string) <-chan paAsyncResult {
	out := make(chan paAsyncResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			out <- paAsyncResult{err: err}
			return
		}
		req.Header.Set("Content-Type", "audio/wav")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			out <- paAsyncResult{err: err}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		out <- paAsyncResult{code: resp.StatusCode, body: body}
	}()
	return out
}

func paCallCount(env *testEnv) int {
	env.back.mu.Lock()
	defer env.back.mu.Unlock()
	return len(env.back.paCalls)
}

func paWaitFor(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}

// TestPASpeakerHTTPSerializesOverlappingRequests proves the relay's admission
// gate: while one HTTP request owns the app call, a second request must not
// open a second routed call (and therefore cannot fill the script host's
// inbound queue with a body it cannot consume). It waits, then plays.
func TestPASpeakerHTTPSerializesOverlappingRequests(t *testing.T) {
	env := newTestEnv(t)
	key := paOperatorKey(t, env)
	hold := make(chan struct{})
	env.back.mu.Lock()
	env.back.paHold = hold
	env.back.mu.Unlock()
	var holdOnce sync.Once
	release := func() { holdOnce.Do(func() { close(hold) }) }
	t.Cleanup(release)

	c := env.ts.Client()
	target := env.ts.URL + "/api/apps/org.capsulad.pa/play"
	headers := map[string]string{"Authorization": "Bearer " + key}
	first := paPostAsync(c, target, bytes.Repeat([]byte{0x11}, 32<<10), headers)
	if !paWaitFor(2*time.Second, func() bool { return paCallCount(env) == 1 }) {
		t.Fatal("first request did not open a PA call")
	}

	second := paPostAsync(c, target, bytes.Repeat([]byte{0x22}, 100<<10), headers)
	time.Sleep(150 * time.Millisecond)
	if calls := paCallCount(env); calls != 1 {
		t.Fatalf("second request opened %d PA calls while the first was held; want 1", calls)
	}

	release()
	for name, result := range map[string]<-chan paAsyncResult{"first": first, "second": second} {
		select {
		case got := <-result:
			if got.err != nil {
				t.Fatalf("%s request: %v", name, got.err)
			}
			if got.code != http.StatusOK {
				t.Fatalf("%s request = %d %v, want 200", name, got.code, got.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s request did not finish", name)
		}
	}
	if calls := paCallCount(env); calls != 2 {
		t.Fatalf("PA calls = %d after both requests, want 2", calls)
	}
}

// TestPASpeakerHTTPClientCancelCancelsRoutedCall proves the HTTP request owns
// the routed call: cancelling the request is the app's documented stop path,
// so the app sees its call context end and releases playback.
func TestPASpeakerHTTPClientCancelCancelsRoutedCall(t *testing.T) {
	env := newTestEnv(t)
	key := paOperatorKey(t, env)
	hold := make(chan struct{})
	contextDone := make(chan struct{})
	env.back.mu.Lock()
	env.back.paHold = hold
	env.back.paContextDone = contextDone
	env.back.mu.Unlock()
	t.Cleanup(func() { close(hold) })

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/apps/org.capsulad.pa/play",
		bytes.NewReader([]byte("RIFF"))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		env.srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		env.back.mu.Lock()
		calls := len(env.back.paCalls)
		env.back.mu.Unlock()
		if calls == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the PA route did not open a routed call")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-contextDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the HTTP request did not cancel the routed call")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the HTTP handler did not return after the client cancelled")
	}
}
