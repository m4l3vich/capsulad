package vkclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"capsulad/apps/marusia/internal/vkproto/servicepb"
)

// TestProductionSDKTransport drives the real native transport against
// in-process TLS servers. It is skipped unless CAPSULAD_TEST_LIBRARY points at
// a staged libcapsulad, so the ordinary test run needs no native build.
//
// One TLS listener serves both protocols: the native HTTP client negotiates
// HTTP/1.1, the native gRPC client negotiates h2 (ALPN). The test CA is passed
// through NewSDKTransport, exactly as an operator would add a private root.
func TestProductionSDKTransport(t *testing.T) {
	library := os.Getenv("CAPSULAD_TEST_LIBRARY")
	if library == "" {
		t.Skip("set CAPSULAD_TEST_LIBRARY to a staged libcapsulad to run the native integration test")
	}
	t.Setenv("CAPSULAD_LIBRARY", library)

	attach := &fakeAttach{response: attachOK()}
	probe := &fakeProbe{}
	speech := &fakeSpeech{aborts: make(chan string, 1)}
	grpcServer := grpc.NewServer()
	servicepb.RegisterClientProbeServer(grpcServer, probe)
	servicepb.RegisterPhraseServer(grpcServer, speech)
	defer grpcServer.Stop()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		attach.ServeHTTP(w, r)
	})
	cert, _ := testCertificate(t)
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	trust := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Certificate[len(cert.Certificate)-1],
	})
	transport, err := NewSDKTransport(server.URL, trust)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	client, err := New(Config{
		BaseURL:   server.URL,
		XTokenID:  "xtoken-abc",
		Serial:    testSerial,
		DeviceID:  testDevice,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Authorize(ctx); err != nil {
		t.Fatalf("authorize through the native transport: %v", err)
	}
	if attach.callCount() != 1 {
		t.Fatalf("attach calls = %d, want 1", attach.callCount())
	}
	if calls, mds := probe.snapshot(); calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	} else {
		assertCallMetadata(t, mds[0], false)
	}

	stream, err := client.OpenSpeech(ctx, SpeechOptions{
		Format:           FormatWave,
		Timezone:         "Europe/Moscow",
		Keyword:          "marusya",
		HWID:             "9F25725B",
		StatusIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("open speech through the native transport: %v", err)
	}
	defer stream.Close()
	if err := stream.SendAudio(bytes.Repeat([]byte{0x01, 0x02}, 2*FrameSamples)); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	if err := stream.EOS(); err != nil {
		t.Fatalf("eos: %v", err)
	}
	var kinds []EventKind
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		kinds = append(kinds, event.Kind)
	}
	want := []EventKind{EventInit, EventStatus, EventTextSoFar, EventCommands, EventFinish, EventEOS}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", kinds, want)
		}
	}
	if _, md := speech.snapshot(); md == nil {
		t.Fatal("the native stream carried no metadata")
	} else {
		assertCallMetadata(t, md, true)
	}
}

// TestProductionSDKTransportCancelsSlowAttach proves that cancelling the turn
// context reaches the native HTTP client: the attach is in flight against a
// server that never answers, the cancel aborts it, and Attach returns promptly
// with context.Canceled. Gated like the other native test.
func TestProductionSDKTransportCancelsSlowAttach(t *testing.T) {
	library := os.Getenv("CAPSULAD_TEST_LIBRARY")
	if library == "" {
		t.Skip("set CAPSULAD_TEST_LIBRARY to a staged libcapsulad to run the native integration test")
	}
	t.Setenv("CAPSULAD_LIBRARY", library)

	started := make(chan struct{})
	var startedOnce sync.Once
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	cert, _ := testCertificate(t)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	// Unblock the handler before Close waits for it.
	defer func() {
		close(release)
		server.Close()
	}()

	trust := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Certificate[len(cert.Certificate)-1],
	})
	transport, err := NewSDKTransport(server.URL, trust)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	client, err := New(Config{
		BaseURL:   server.URL,
		XTokenID:  "xtoken-abc",
		Serial:    testSerial,
		DeviceID:  testDevice,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Attach(ctx)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("attach never reached the slow server")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("attach error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("cancelled attach returned after %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled attach did not return")
	}
}
