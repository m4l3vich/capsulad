package capsulad

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTCPOptionsEncode(t *testing.T) {
	noDelay := false
	options, err := TCPOptions{
		TLS:        true,
		Roots:      "pem",
		CAPEM:      []byte("-----BEGIN CERTIFICATE-----"),
		ServerName: "voice.test",
		NoDelay:    &noDelay,
	}.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(options, &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
	if decoded["tls"] != true || decoded["roots"] != "pem" ||
		decoded["server_name"] != "voice.test" || decoded["nodelay"] != false {
		t.Fatalf("options = %s", options)
	}
	if decoded["ca_pem"] != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("ca_pem = %#v", decoded["ca_pem"])
	}

	// A nil NoDelay leaves the key out so the native default (enabled) applies.
	zero, err := TCPOptions{}.encode()
	if err != nil {
		t.Fatalf("encode zero: %v", err)
	}
	var zeroDecoded map[string]any
	if err := json.Unmarshal(zero, &zeroDecoded); err != nil {
		t.Fatalf("json zero: %v", err)
	}
	if _, ok := zeroDecoded["nodelay"]; ok {
		t.Fatalf("nil NoDelay must be omitted: %s", zero)
	}
	if _, err := (TCPOptions{Roots: "bogus"}).encode(); err == nil {
		t.Fatal("an unknown roots mode must be rejected")
	}
}

func TestUDPAndListenerOptionsEncode(t *testing.T) {
	udp, err := UDPOptions{Bind: "127.0.0.1", Port: 0}.encode()
	if err != nil {
		t.Fatalf("udp encode: %v", err)
	}
	if string(udp) != `{"bind":"127.0.0.1"}` {
		t.Fatalf("udp options = %s", udp)
	}
	if _, err := (UDPOptions{Port: 5354}).encode(); err == nil {
		t.Fatal("a fixed local UDP port must be rejected")
	}
	if _, err := (UDPOptions{Port: -1}).encode(); err == nil {
		t.Fatal("a negative UDP port must be rejected")
	}

	listener, err := ListenerOptions{Protocol: "udp", Bind: "0.0.0.0", Port: 0, Backlog: 0}.encode()
	if err != nil {
		t.Fatalf("listener encode: %v", err)
	}
	if string(listener) != `{"bind":"0.0.0.0","protocol":"udp"}` {
		t.Fatalf("listener options = %s", listener)
	}
	if _, err := (ListenerOptions{Protocol: "sctp"}).encode(); err == nil {
		t.Fatal("an unknown listener protocol must be rejected")
	}
	if _, err := (ListenerOptions{Backlog: 129}).encode(); err == nil {
		t.Fatal("a backlog above 128 must be rejected")
	}
}

// TestUDPOptionsRejectFixedPort pins the local bind rule: udp_open binds an
// ephemeral local port only, so a fixed inbound port has to go through
// OpenListener and gets an actionable error here.
func TestUDPOptionsRejectFixedPort(t *testing.T) {
	for _, port := range []int{1, 5354, 65535} {
		_, err := (UDPOptions{Bind: "127.0.0.1", Port: port}).encode()
		if err == nil {
			t.Fatalf("Port %d must be rejected", port)
		}
		if !strings.Contains(err.Error(), "OpenListener") {
			t.Fatalf("Port %d error = %v, want an OpenListener hint", port, err)
		}
	}
	encoded, err := (UDPOptions{Bind: "127.0.0.1"}).encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(encoded) != `{"bind":"127.0.0.1"}` {
		t.Fatalf("options = %s, want no port key", encoded)
	}
}

// TestWatchAbortIgnoresDeadline: a context deadline must never release a
// listener or UDP handle, because the native per-call timeout enforces it and
// leaves the handle usable.
func TestWatchAbortIgnoresDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var aborts atomic.Int32
	status, detail, aborted := watchAbort(ctx, func() { aborts.Add(1) },
		func() (int32, string) {
			<-ctx.Done()
			// Give the watcher time to observe ctx.Done before the call
			// returns; a regressed watcher records its abort here.
			time.Sleep(20 * time.Millisecond)
			return statusTimeout, "deadline elapsed"
		})
	if aborted || aborts.Load() != 0 {
		t.Fatalf("a deadline aborted the handle (reported=%v aborts=%d)", aborted, aborts.Load())
	}
	if status != statusTimeout || detail != "deadline elapsed" {
		t.Fatalf("status = %d %q, want the native TIMEOUT", status, detail)
	}
}

// TestWatchAbortCancelsExplicitly: only an explicit cancellation releases the
// handle, which is what unblocks a native call waiting indefinitely.
func TestWatchAbortCancelsExplicitly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	unblocked := make(chan struct{})
	var aborts atomic.Int32
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	status, _, aborted := watchAbort(ctx, func() {
		aborts.Add(1)
		close(unblocked) // releasing the handle unblocks the parked native call
	}, func() (int32, string) {
		<-unblocked
		return statusCancelled, "cancelled"
	})
	if !aborted || aborts.Load() != 1 {
		t.Fatalf("explicit cancellation must abort once (reported=%v aborts=%d)",
			aborted, aborts.Load())
	}
	if status != statusCancelled {
		t.Fatalf("status = %d, want CANCELLED", status)
	}
	if err := ctx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", err)
	}
}

// TestListenerDeadlineKeepsHandleUsable drives the real I/O path with a fake
// native call: the daemon-allocated handle must survive a context deadline.
// A regression releases the handle through Close; this test never loads the
// engine, so it would fail (loudly) instead of passing silently.
func TestListenerDeadlineKeepsHandleUsable(t *testing.T) {
	const handle = uint64(0x5eed)
	listener := &Listener{handle: handle}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := listener.runIO(ctx, "accept", func() (int32, string) {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		return statusTimeout, "deadline elapsed"
	})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != statusTimeout {
		t.Fatalf("deadline error = %v, want a TIMEOUT *StatusError", err)
	}
	if listener.Handle() != handle {
		t.Fatalf("a deadline released the listener handle (now %#x)", listener.Handle())
	}
}

// TestUDPConnDeadlineKeepsHandleUsable is the UDP mirror of the listener test.
func TestUDPConnDeadlineKeepsHandleUsable(t *testing.T) {
	const handle = uint64(0x5eed)
	conn := &UDPConn{handle: handle}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := conn.runIO(ctx, "receive udp", func() (int32, string) {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		return statusTimeout, "deadline elapsed"
	})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != statusTimeout {
		t.Fatalf("deadline error = %v, want a TIMEOUT *StatusError", err)
	}
	if conn.Handle() != handle {
		t.Fatalf("a deadline released the UDP handle (now %#x)", conn.Handle())
	}
}

func TestMDNSCanonicalJSON(t *testing.T) {
	request, err := json.Marshal(MDNSService{
		ServiceType:  "_snapcast._tcp",
		InstanceName: "Living Room",
		Port:         1704,
		TXT:          map[string]string{"version": "1"},
	})
	if err != nil {
		t.Fatalf("marshal service: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(request, &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
	for key, want := range map[string]any{
		"serviceType":  "_snapcast._tcp",
		"instanceName": "Living Room",
		"port":         float64(1704),
	} {
		if decoded[key] != want {
			t.Errorf("%s = %#v, want %#v", key, decoded[key], want)
		}
	}

	payload := []byte(`{"kind":"EVENT_KIND_ADDED","name":"Kitchen","serviceType":"_airplay._tcp",` +
		`"domain":"local","host":"kitchen.local","addresses":["192.0.2.10"],"port":7000,` +
		`"txt":{"model":"x"}}`)
	var event MDNSEvent
	if err := unmarshalProtoJSON(payload, &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if event.Kind != MDNSEventKindAdded || event.Name != "Kitchen" ||
		event.Port != 7000 || event.Addresses[0] != "192.0.2.10" || event.TXT["model"] != "x" {
		t.Fatalf("event = %+v", event)
	}
}

func TestNetworkStatusMapping(t *testing.T) {
	if err := nativeStatusError("test", statusEndOfStream, ""); err != io.EOF {
		t.Fatalf("END_OF_STREAM = %v, want io.EOF", err)
	}
	if err := nativeStatusError("test", statusCancelled, ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("CANCELLED = %v, want ErrClosed", err)
	}
	for _, status := range []int32{statusTimeout, statusUnsupported, statusTooLarge} {
		err := nativeStatusError("test", status, "")
		var statusErr *StatusError
		if !errors.As(err, &statusErr) {
			t.Fatalf("status %d = %v, want *StatusError", status, err)
		}
		if statusErr.Code != status || !statusErr.NativeStatus {
			t.Fatalf("status %d mapped to %+v", status, statusErr)
		}
	}
	if nativeStatusError("test", statusOK, "") != nil {
		t.Fatal("OK must map to nil")
	}
}

func requireTestLibrary(t *testing.T) string {
	t.Helper()
	path := os.Getenv("CAPSULAD_TEST_LIBRARY")
	if path == "" {
		t.Skip("set CAPSULAD_TEST_LIBRARY to the staged libcapsulad artifact")
	}
	if err := ensureLibrary(path); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return path
}

func loopbackContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestTCPLoopbackEcho(t *testing.T) {
	path := requireTestLibrary(t)
	server, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer server.Close()
	go func() {
		conn, err := server.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	ctx := loopbackContext(t)
	port := server.Addr().(*net.TCPAddr).Port
	stream, err := OpenTCP(ctx, Env{Library: path}, "127.0.0.1", uint16(port), TCPOptions{})
	if err != nil {
		t.Fatalf("open tcp: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Write(ctx, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(&contextReader{ctx: ctx, stream: stream}, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}
	info, err := stream.Info()
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Kind != "stream" || info.Port != port || info.Closed {
		t.Fatalf("info = %+v", info)
	}
	if err := stream.Shutdown(ShutdownWrite); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// The echo server returns the FIN as a clean close.
	if _, err := stream.Read(ctx, buf); err != io.EOF {
		t.Fatalf("read after shutdown = %v, want io.EOF", err)
	}
}

// contextReader adapts TCPStream to io.Reader for io.ReadFull without hiding
// the context.
type contextReader struct {
	ctx    context.Context
	stream *TCPStream
}

func (r *contextReader) Read(p []byte) (int, error) { return r.stream.Read(r.ctx, p) }

func TestUDPLoopbackEcho(t *testing.T) {
	path := requireTestLibrary(t)
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer server.Close()
	go func() {
		buf := make([]byte, 64)
		n, peer, err := server.ReadFromUDP(buf)
		if err != nil {
			return
		}
		_, _ = server.WriteToUDP(buf[:n], peer)
	}()

	ctx := loopbackContext(t)
	conn, err := OpenUDP(ctx, Env{Library: path}, "", 0, UDPOptions{Bind: "127.0.0.1"})
	if err != nil {
		t.Fatalf("open udp: %v", err)
	}
	defer conn.Close()

	peer := &UDPAddr{Address: "127.0.0.1", Port: server.LocalAddr().(*net.UDPAddr).Port}
	if _, err := conn.Send(ctx, peer, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	data, from, err := conn.Receive(ctx, 0)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if string(data) != "ping" || from == nil || from.Port != peer.Port {
		t.Fatalf("receive = %q from %+v", data, from)
	}
}

// TestUDPDeadlineKeepsSocketUsableAndCancelCloses needs the native engine: a
// deadline must report TIMEOUT and leave the socket usable, while an explicit
// cancellation closes it. This is the behavior the Go watcher must not
// override.
func TestUDPDeadlineKeepsSocketUsableAndCancelCloses(t *testing.T) {
	path := requireTestLibrary(t)
	ctx := loopbackContext(t)
	conn, err := OpenUDP(ctx, Env{Library: path}, "", 0, UDPOptions{Bind: "127.0.0.1"})
	if err != nil {
		t.Fatalf("open udp: %v", err)
	}
	defer conn.Close()

	deadline, cancelDeadline := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelDeadline()
	_, _, err = conn.Receive(deadline, 0)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != statusTimeout {
		t.Fatalf("receive with a deadline = %v, want a TIMEOUT *StatusError", err)
	}
	if conn.Handle() == 0 {
		t.Fatal("a deadline closed the UDP socket")
	}

	// The socket stays usable: a plain echo round-trip still works.
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer server.Close()
	go func() {
		buf := make([]byte, 64)
		n, peer, err := server.ReadFromUDP(buf)
		if err != nil {
			return
		}
		_, _ = server.WriteToUDP(buf[:n], peer)
	}()
	echoPeer := &UDPAddr{Address: "127.0.0.1", Port: server.LocalAddr().(*net.UDPAddr).Port}
	if _, err := conn.Send(ctx, echoPeer, []byte("after-deadline")); err != nil {
		t.Fatalf("send after a deadline: %v", err)
	}
	data, from, err := conn.Receive(ctx, 0)
	if err != nil {
		t.Fatalf("receive after a deadline: %v", err)
	}
	if string(data) != "after-deadline" || from == nil {
		t.Fatalf("receive after a deadline = %q from %+v", data, from)
	}

	// Explicit cancellation closes the socket and unblocks the receive.
	cancelCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := conn.Receive(cancelCtx, 0)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled receive = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled receive did not return")
	}
	if conn.Handle() != 0 {
		t.Fatal("explicit cancellation must close the UDP socket")
	}
	if _, _, err := conn.Receive(ctx, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("receive after cancellation = %v, want ErrClosed", err)
	}
}

func TestTLSLoopbackEcho(t *testing.T) {
	path := requireTestLibrary(t)
	dir := filepath.Join("..", "..", "native", "testdata")
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"),
		filepath.Join(dir, "server-key.pem"))
	if err != nil {
		t.Skipf("TLS fixtures unavailable: %v", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Skipf("TLS fixtures unavailable: %v", err)
	}
	server, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer server.Close()
	go func() {
		conn, err := server.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		_, _ = conn.Write(buf)
	}()

	ctx := loopbackContext(t)
	port := server.Addr().(*net.TCPAddr).Port
	stream, err := OpenTCP(ctx, Env{Library: path}, "127.0.0.1", uint16(port), TCPOptions{
		TLS:        true,
		Roots:      "pem",
		CAPEM:      caPEM,
		ServerName: "localhost",
	})
	if err != nil {
		t.Fatalf("open tls: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Write(ctx, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(&contextReader{ctx: ctx, stream: stream}, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("tls echo = %q", buf)
	}
	info, err := stream.Info()
	if err != nil || !info.TLS || info.ServerName != "localhost" {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestListenerPolicyUnsupported(t *testing.T) {
	path := requireTestLibrary(t)
	if os.Getenv("CAPSULAD_CONTROL_FD") != "" {
		t.Skip("a control channel is present; cannot assert the no-control-channel policy error")
	}
	ctx := loopbackContext(t)
	listener, err := OpenListener(ctx, Env{Library: path}, ListenerOptions{Protocol: "tcp"})
	if err == nil {
		_ = listener.Close()
		t.Skip("listener_open succeeded; this environment has a usable control channel")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != statusUnsupported {
		t.Fatalf("listener without a control channel = %v, want status %d", err, statusUnsupported)
	}
}

func TestMDNSPublishBrowse(t *testing.T) {
	requireTestLibrary(t)
	if os.Getenv("CAPSULAD_TEST_MDNS") != "1" {
		t.Skip("set CAPSULAD_TEST_MDNS=1 with a running daemon and Avahi to exercise mDNS")
	}
	env := FromEnv()
	if env.RunID == "" {
		t.Skip("CAPSULAD_RUN_ID is required for the mDNS integration test")
	}
	ctx := loopbackContext(t)
	client, err := Connect(ctx, env)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	publication, err := client.PublishMDNS(ctx, MDNSService{
		ServiceType:  "_capsulad-test._tcp",
		InstanceName: "bindings-test",
		Port:         1704,
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	browse, err := client.BrowseMDNS(ctx, "_capsulad-test._tcp", "")
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	defer browse.Close()
	event, err := browse.Next(ctx)
	if err != nil && err != io.EOF {
		t.Fatalf("browse next: %v", err)
	}
	if event != nil && event.ServiceType != "_capsulad-test._tcp" {
		t.Fatalf("event = %+v", event)
	}
	if err := client.UnpublishMDNS(ctx, publication.PublicationID, 5*time.Second); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
}
