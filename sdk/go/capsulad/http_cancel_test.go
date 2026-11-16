package capsulad

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHTTPCancelIsPerRequest needs the native engine and exercises the Go
// wrapper's request-id cancellation: cancelling one Do must not abort a
// concurrent Do, and the client must remain usable afterwards.
func TestHTTPCancelIsPerRequest(t *testing.T) {
	library := os.Getenv("CAPSULAD_TEST_LIBRARY")
	if library == "" {
		t.Skip("set CAPSULAD_TEST_LIBRARY to the staged libcapsulad artifact")
	}

	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	var releaseAOnce, releaseBOnce sync.Once
	closeA := func() { releaseAOnce.Do(func() { close(releaseA) }) }
	closeB := func() { releaseBOnce.Do(func() { close(releaseB) }) }
	defer closeA()
	defer closeB()
	startedA := make(chan struct{}, 1)
	startedB := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		startedA <- struct{}{}
		select {
		case <-releaseA:
			_, _ = w.Write([]byte("A"))
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		startedB <- struct{}{}
		select {
		case <-releaseB:
			_, _ = w.Write([]byte("B"))
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/fast", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fast"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := OpenHTTP(Env{Library: library}, HTTPOptions{
		Roots:           "webpki",
		FollowRedirects: 4,
	})
	if err != nil {
		t.Fatalf("open http: %v", err)
	}
	defer client.Close()

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	doneA := make(chan error, 1)
	doneB := make(chan *HTTPResponse, 1)
	errB := make(chan error, 1)
	go func() {
		_, err := client.Do(ctxA, HTTPRequest{Method: "GET", URL: server.URL + "/a"})
		doneA <- err
	}()
	go func() {
		response, err := client.Do(context.Background(),
			HTTPRequest{Method: "GET", URL: server.URL + "/b"})
		if err != nil {
			errB <- err
			return
		}
		doneB <- response
	}()
	select {
	case <-startedA:
	case <-time.After(5 * time.Second):
		t.Fatal("request A never reached the server")
	}
	select {
	case <-startedB:
	case <-time.After(5 * time.Second):
		t.Fatal("request B never reached the server")
	}

	cancelA()
	select {
	case err := <-doneA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled request error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request did not return")
	}

	// B was in flight while A was cancelled; it must not have been aborted.
	select {
	case err := <-errB:
		t.Fatalf("concurrent request failed: %v", err)
	case response := <-doneB:
		t.Fatalf("concurrent request completed before its server released it: %+v", response)
	case <-time.After(150 * time.Millisecond):
	}
	closeB()
	select {
	case err := <-errB:
		t.Fatalf("concurrent request failed: %v", err)
	case response := <-doneB:
		if response.StatusCode != 200 || string(response.Body) != "B" {
			t.Fatalf("concurrent response = %+v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent request did not complete")
	}

	// A request started immediately after the cancelled one must work.
	response, err := client.Do(context.Background(),
		HTTPRequest{Method: "GET", URL: server.URL + "/fast"})
	if err != nil {
		t.Fatalf("request after cancellation: %v", err)
	}
	if response.StatusCode != 200 || string(response.Body) != "fast" {
		t.Fatalf("request after cancellation = %+v", response)
	}
}

// TestHTTPCancelBeforeRegistration needs the native engine and covers the
// cancel-before-register race deterministically: the native cancel for the id
// the next Do will pick lands before that request is registered. The request
// must fail promptly without reaching the server, and the client must stay
// usable.
func TestHTTPCancelBeforeRegistration(t *testing.T) {
	library := os.Getenv("CAPSULAD_TEST_LIBRARY")
	if library == "" {
		t.Skip("set CAPSULAD_TEST_LIBRARY to the staged libcapsulad artifact")
	}

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client, err := OpenHTTP(Env{Library: library}, HTTPOptions{Roots: "webpki"})
	if err != nil {
		t.Fatalf("open http: %v", err)
	}
	defer client.Close()

	handle, err := client.value()
	if err != nil {
		t.Fatalf("http handle: %v", err)
	}
	// Do allocates ids monotonically, so this is exactly the id of the next
	// request: arming the cancel first reproduces the race where a context
	// watcher wins against request registration.
	nextID := client.nextRequestID.Load() + 1
	if err := httpCancelNative(handle, nextID); err != nil {
		t.Fatalf("pre-arm cancel: %v", err)
	}

	started := time.Now()
	_, err = client.Do(context.Background(),
		HTTPRequest{Method: "GET", URL: server.URL + "/never"})
	if err == nil {
		t.Fatal("a request cancelled before registration must not succeed")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != statusCancelled {
		t.Fatalf("error = %v, want a cancelled StatusError", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("pre-registration cancel took %v", elapsed)
	}
	if hits.Load() != 0 {
		t.Fatal("the cancelled request reached the server")
	}

	// The sticky record was consumed; the next request proceeds normally.
	response, err := client.Do(context.Background(),
		HTTPRequest{Method: "GET", URL: server.URL + "/fast"})
	if err != nil {
		t.Fatalf("request after pre-registration cancel: %v", err)
	}
	if response.StatusCode != 200 || string(response.Body) != "ok" {
		t.Fatalf("request after pre-registration cancel = %+v", response)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
}
