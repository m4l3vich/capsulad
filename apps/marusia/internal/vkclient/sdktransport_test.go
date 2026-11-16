package vkclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/sdk/capsulad"
)

// fakeGRPC satisfies grpcHandle without a native library.
type fakeGRPC struct {
	closes atomic.Int32
}

func (f *fakeGRPC) Close() error {
	f.closes.Add(1)
	return nil
}

func (f *fakeGRPC) Unary(context.Context, string, string, map[string]string, []byte,
	time.Duration) ([]byte, error) {
	return nil, errors.New("fakeGRPC: unary not used")
}

func (f *fakeGRPC) NewStream(context.Context, string, string, map[string]string,
	capsulad.CallKind, time.Duration) (*capsulad.Stream, error) {
	return nil, errors.New("fakeGRPC: stream not used")
}

func TestSDKTransportConnectsOnce(t *testing.T) {
	var opens atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake := &fakeGRPC{}
	transport := &sdkTransport{
		origin: "https://example.test:443",
		host:   "example.test",
		openGRPC: func(context.Context, capsulad.Env, string,
			capsulad.GRPCOptions) (grpcHandle, error) {
			opens.Add(1)
			started <- struct{}{}
			<-release
			return fake, nil
		},
	}

	const callers = 2
	results := make(chan grpcHandle, callers)
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			client, err := transport.grpcClient(context.Background())
			if err != nil {
				errs <- err
				return
			}
			results <- client
		}()
	}
	<-started
	// Give the second caller time to join the same in-flight attempt.
	time.Sleep(20 * time.Millisecond)
	if got := opens.Load(); got != 1 {
		t.Fatalf("connect attempts = %d, want 1", got)
	}
	close(release)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("grpcClient: %v", err)
	}
	seen := 0
	for client := range results {
		if client != fake {
			t.Fatalf("waiter got %p, want the shared client %p", client, fake)
		}
		seen++
	}
	if seen != callers {
		t.Fatalf("successful waiters = %d, want %d", seen, callers)
	}
	if got := fake.closes.Load(); got != 0 {
		t.Fatalf("successful client closed %d times", got)
	}
}

func TestSDKTransportCloseDuringConnectDoesNotBlock(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake := &fakeGRPC{}
	transport := &sdkTransport{
		origin: "https://example.test:443",
		host:   "example.test",
		openGRPC: func(context.Context, capsulad.Env, string,
			capsulad.GRPCOptions) (grpcHandle, error) {
			started <- struct{}{}
			<-release
			return fake, nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := transport.grpcClient(context.Background())
		done <- err
	}()
	<-started

	closed := make(chan struct{})
	go func() {
		_ = transport.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close blocked behind an in-flight connect")
	}

	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("grpcClient error = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("grpcClient did not return after close")
	}
	if got := fake.closes.Load(); got != 1 {
		t.Fatalf("late client closed %d times, want 1", got)
	}
	transport.mu.Lock()
	installed := transport.grpc
	transport.mu.Unlock()
	if installed != nil {
		t.Fatal("a connect finishing after Close was installed")
	}
}

func TestSDKTransportFailedConnectIsSharedAndRetryable(t *testing.T) {
	connectErr := errors.New("dial failed")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var opens atomic.Int32
	transport := &sdkTransport{
		origin: "https://example.test:443",
		host:   "example.test",
		openGRPC: func(context.Context, capsulad.Env, string,
			capsulad.GRPCOptions) (grpcHandle, error) {
			opens.Add(1)
			started <- struct{}{}
			<-release
			return nil, connectErr
		},
	}

	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := transport.grpcClient(context.Background())
			done <- err
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range 2 {
		select {
		case err := <-done:
			if !errors.Is(err, connectErr) {
				t.Fatalf("waiter error = %v, want the shared connect error", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("waiter did not receive the connect result")
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("connect attempts = %d, want 1", got)
	}

	// A later call starts a fresh attempt rather than replaying the failure.
	transport.mu.Lock()
	transport.openGRPC = func(context.Context, capsulad.Env, string,
		capsulad.GRPCOptions) (grpcHandle, error) {
		return &fakeGRPC{}, nil
	}
	transport.mu.Unlock()
	client, err := transport.grpcClient(context.Background())
	if err != nil || client == nil {
		t.Fatalf("retry = %v, %v", client, err)
	}
}
