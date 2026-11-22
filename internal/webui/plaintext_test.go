package webui

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type plaintextTestServer struct {
	env  *testEnv
	addr string
}

// startPlaintextTestServer runs the real serve path on an ephemeral port, so
// the test exercises the probe listener and the TLS wiring together.
func startPlaintextTestServer(t *testing.T) *plaintextTestServer {
	t.Helper()
	env := newTestEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.srv.serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("webui server did not stop")
		}
	})
	return &plaintextTestServer{env: env, addr: ln.Addr().String()}
}

func TestPlainHTTPGetsHTTPSGuidance(t *testing.T) {
	s := startPlaintextTestServer(t)
	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	_, err = io.WriteString(conn, "GET /some/path HTTP/1.1\r\n"+
		"Host: Speakers-Living-Room.local:8443\r\n"+
		"User-Agent: test\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp := string(raw)
	for _, want := range []string{
		"HTTP/1.1 200 OK\r\n",
		"Content-Type: text/html; charset=utf-8\r\n",
		"Connection: close\r\n",
		"lang=\"en-GB\"",
		"self-signed certificate",
		"https://speakers-living-room.local:8443/",
		"<strong>Access</strong>",
		"download the CA or server certificate",
		"trusted root certificate",
		"capsulad-cli cert status",
		"capsulad-cli cert init",
		"capsulad-cli auth init",
		"capsulad-cli cert renew",
		"capsulad-cli status",
		"creates the server certificate and the administrator",
	} {
		if !strings.Contains(resp, want) {
			t.Errorf("response missing %q\n%s", want, resp)
		}
	}
	if strings.Contains(resp, "Client sent an HTTP request to an HTTPS server") {
		t.Errorf("response kept the bare TLS error page:\n%s", resp)
	}
	if strings.Contains(resp, "/some/path") {
		t.Errorf("response reflected the request path:\n%s", resp)
	}
}

func TestPlainHTTPEscapesHost(t *testing.T) {
	s := startPlaintextTestServer(t)
	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: <script>alert(1)</script>\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp := string(raw)
	if strings.Contains(resp, "<script>") {
		t.Errorf("response reflects an unsafe host: %s", resp)
	}
	if !strings.Contains(resp, "https://capsula.local:8443/") {
		t.Errorf("expected fallback host in response: %s", resp)
	}
}

func TestPlainHTTPUsesRequestPort(t *testing.T) {
	s := startPlaintextTestServer(t)
	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: 127.0.0.1:9543\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp := string(raw)
	if !strings.Contains(resp, "https://127.0.0.1:9543/") {
		t.Errorf("response missing the request port\n%s", resp)
	}
	if strings.Contains(resp, "127.0.0.1:8443") {
		t.Errorf("response ignored the request port\n%s", resp)
	}
}

func trustedTLSClient(t *testing.T, s *plaintextTestServer) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(s.env.store.CertPath())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("could not parse test certificate")
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   5 * time.Second,
	}
}

func TestTLSCertificateStillReachesHandler(t *testing.T) {
	s := startPlaintextTestServer(t)
	client := trustedTLSClient(t, s)
	resp, err := client.Get("https://" + s.addr + "/healthz")
	if err != nil {
		t.Fatalf("TLS request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}

func TestIdlePlainConnectionDoesNotBlockTLS(t *testing.T) {
	s := startPlaintextTestServer(t)
	client := trustedTLSClient(t, s)

	// Hold a silent plaintext connection open. It must be classified off the
	// accept path, so the TLS handshake below is not delayed by its probe.
	idle, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	resp, err := client.Get("https://" + s.addr + "/healthz")
	if err != nil {
		t.Fatalf("TLS request with an idle plaintext peer: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("TLS request took %v while an idle plaintext peer was open", elapsed)
	}
}

func TestOversizedPlainRequestHeadIsBoundedAndClosed(t *testing.T) {
	s := startPlaintextTestServer(t)
	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	head := "GET / HTTP/1.1\r\nHost: ignored.local\r\nX-Filler: " +
		strings.Repeat("a", 9<<10) + "\r\n\r\n"
	if _, err := io.WriteString(conn, head); err != nil {
		// A close mid-write is allowed; the response may still be readable.
		t.Logf("write: %v", err)
	}
	start := time.Now()
	raw, readErr := io.ReadAll(conn)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("oversized head took %v to close", elapsed)
	}
	if len(raw) == 0 {
		t.Fatalf("no response before close: %v", readErr)
	}
	resp := string(raw)
	if !strings.Contains(resp, "HTTP/1.1 200 OK") ||
		!strings.Contains(resp, "https://capsula.local:8443/") {
		t.Fatalf("unexpected bounded response:\n%s", resp)
	}
	if len(resp) > 64<<10 {
		t.Fatalf("response unexpectedly large: %d bytes", len(resp))
	}
}
