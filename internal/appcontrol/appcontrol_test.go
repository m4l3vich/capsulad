package appcontrol

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

func newTestManager(t *testing.T, reserved ...int) (*Manager, *os.File) {
	t.Helper()
	logs, err := logbus.New(ids.NewClock(), 32, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	m := New(logs, Policy{ReservedPorts: reserved})
	t.Cleanup(m.Close)
	child, abort, err := m.Channel("run_test", "test.app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		abort()
		_ = child.Close()
	})
	// Bound the test so a protocol regression fails instead of hanging.
	_ = unix.SetsockoptTimeval(int(child.Fd()), unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 5})
	return m, child
}

type reply struct {
	body listenResponse
	fds  []int
}

func exchange(t *testing.T, child *os.File, request any) reply {
	t.Helper()
	var payload []byte
	switch value := request.(type) {
	case string:
		payload = []byte(value)
	case []byte:
		payload = value
	default:
		var err error
		payload, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := child.Write(payload); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return receive(t, child)
}

func receive(t *testing.T, child *os.File) reply {
	t.Helper()
	buf := make([]byte, maxResponseBytes)
	oob := make([]byte, unix.CmsgSpace(4))
	for {
		n, oobn, _, _, err := unix.Recvmsg(int(child.Fd()), buf, oob, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatalf("recv response: %v", err)
		}
		out := reply{}
		if err := json.Unmarshal(buf[:n], &out.body); err != nil {
			t.Fatalf("decode response %q: %v", buf[:n], err)
		}
		if oobn > 0 {
			cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
			if err != nil {
				t.Fatalf("parse control message: %v", err)
			}
			for i := range cmsgs {
				fds, err := unix.ParseUnixRights(&cmsgs[i])
				if err != nil {
					t.Fatalf("parse rights: %v", err)
				}
				out.fds = append(out.fds, fds...)
			}
		}
		return out
	}
}

func (r reply) closeFDs(t *testing.T) {
	t.Helper()
	for _, fd := range r.fds {
		_ = unix.Close(fd)
	}
}

func TestTCPListenerFDIsUsable(t *testing.T) {
	_, child := newTestManager(t)
	resp := exchange(t, child, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0, Backlog: 8})
	if !resp.body.OK || resp.body.Protocol != "tcp" || resp.body.Port == 0 {
		t.Fatalf("response = %+v", resp.body)
	}
	if len(resp.fds) != 1 {
		t.Fatalf("received %d fds, want 1", len(resp.fds))
	}
	file := os.NewFile(uintptr(resp.fds[0]), "listener")
	ln, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("wrap listener fd: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		accepted <- conn
	}()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(resp.body.Address, itoa(resp.body.Port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial allocated listener: %v", err)
	}
	_ = conn.Close()
	select {
	case got := <-accepted:
		if got != nil {
			_ = got.Close()
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allocated listener did not accept a connection")
	}
}

func TestUDPListenerFDIsUsable(t *testing.T) {
	_, child := newTestManager(t)
	resp := exchange(t, child, listenRequest{Op: "listen", Protocol: "udp", Bind: "127.0.0.1", Port: 0})
	if !resp.body.OK || resp.body.Protocol != "udp" {
		t.Fatalf("response = %+v", resp.body)
	}
	if len(resp.fds) != 1 {
		t.Fatalf("received %d fds, want 1", len(resp.fds))
	}
	file := os.NewFile(uintptr(resp.fds[0]), "udp")
	conn, err := net.FilePacketConn(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("wrap udp fd: %v", err)
	}
	defer conn.Close()
	sender, err := net.Dial("udp", net.JoinHostPort(resp.body.Address, itoa(resp.body.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if _, err := sender.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, _, err := conn.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("read = %q, %v", buf[:n], err)
	}
}

func TestPolicyRejections(t *testing.T) {
	_, child := newTestManager(t, 8443)
	cases := []struct {
		name    string
		request any
	}{
		{"bad protocol", listenRequest{Op: "listen", Protocol: "sctp", Bind: "127.0.0.1"}},
		{"privileged port", listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 80}},
		{"mdns port", listenRequest{Op: "listen", Protocol: "udp", Bind: "0.0.0.0", Port: 5353}},
		{"web ui port", listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 8443}},
		{"multicast", listenRequest{Op: "listen", Protocol: "udp", Bind: "224.0.0.1", Port: 0}},
		{"broadcast", listenRequest{Op: "listen", Protocol: "udp", Bind: "255.255.255.255", Port: 0}},
		{"foreign unicast", listenRequest{Op: "listen", Protocol: "tcp", Bind: "8.8.8.8", Port: 0}},
		{"backlog too large", listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0, Backlog: 200}},
		{"backlog negative", listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0, Backlog: -1}},
		{"unknown op", listenRequest{Op: "accept", Protocol: "tcp"}},
		{"malformed json", "{not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := exchange(t, child, tc.request)
			if resp.body.OK || resp.body.Error == "" {
				t.Fatalf("response = %+v, want an error", resp.body)
			}
			if len(resp.fds) != 0 {
				resp.closeFDs(t)
				t.Fatalf("rejected request attached %d fds", len(resp.fds))
			}
		})
	}
}

func TestMaxListenersPerRun(t *testing.T) {
	_, child := newTestManager(t)
	for i := range MaxListenersPerRun {
		resp := exchange(t, child, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0})
		if !resp.body.OK {
			t.Fatalf("allocation %d: %+v", i, resp.body)
		}
		if len(resp.fds) != 1 {
			t.Fatalf("allocation %d returned %d fds", i, len(resp.fds))
		}
		_ = unix.Close(resp.fds[0])
	}
	resp := exchange(t, child, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0})
	if resp.body.OK || !strings.Contains(resp.body.Error, "maximum") {
		t.Fatalf("17th allocation = %+v, want the per-run cap", resp.body)
	}
	if len(resp.fds) != 0 {
		resp.closeFDs(t)
	}
}

func TestOversizedRequestIsRejected(t *testing.T) {
	_, child := newTestManager(t)
	resp := exchange(t, child, []byte(`{"op":"listen","protocol":"tcp","bind":"`+strings.Repeat("x", maxRequestBytes)+`"}`))
	if resp.body.OK || resp.body.Error == "" {
		t.Fatalf("response = %+v, want rejection", resp.body)
	}
}

func TestCloseRunReleasesSocketAndAllowsRelaunch(t *testing.T) {
	m, child := newTestManager(t)
	m.CloseRun("run_test")
	// The app end is now dead.
	buf := make([]byte, 8)
	if _, err := child.Read(buf); err == nil {
		t.Fatal("read after CloseRun succeeded")
	}
	// A restarted run with the same ID gets a fresh channel.
	next, abort, err := m.Channel("run_test", "test.app")
	if err != nil {
		t.Fatalf("relaunch channel: %v", err)
	}
	defer func() {
		abort()
		_ = next.Close()
	}()
	resp := exchange(t, next, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0})
	if !resp.body.OK {
		t.Fatalf("relaunched listener = %+v", resp.body)
	}
	resp.closeFDs(t)
}

func TestSendErrorClosesDupWithoutLeak(t *testing.T) {
	m, child := newTestManager(t)
	_ = child.Close()
	m.CloseRun("run_test")
	// The failed exchange happens in the serve goroutine; this test mainly
	// asserts that a closed peer does not wedge the manager: a new run can
	// still allocate.
	next, abort, err := m.Channel("run_other", "test.app")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		abort()
		_ = next.Close()
	}()
	resp := exchange(t, next, listenRequest{Op: "listen", Protocol: "udp", Bind: "127.0.0.1", Port: 0})
	if !resp.body.OK {
		t.Fatalf("response = %+v", resp.body)
	}
	resp.closeFDs(t)
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestErrorStatusClassification(t *testing.T) {
	_, child := newTestManager(t)
	resp := exchange(t, child, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 80})
	if resp.body.Status != StatusInvalidArgument {
		t.Fatalf("policy refusal status = %d, want %d (%+v)", resp.body.Status, StatusInvalidArgument, resp.body)
	}

	// Occupy an ephemeral port so the daemon's bind fails with the transport
	// class rather than a policy class.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	resp = exchange(t, child, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: port})
	if resp.body.Status != StatusTransport {
		t.Fatalf("bind failure status = %d, want %d (%+v)", resp.body.Status, StatusTransport, resp.body)
	}
	if len(resp.fds) != 0 {
		resp.closeFDs(t)
	}
}

// TestControlRepliesEchoRequestID proves the reply carries the request id on
// every path: success, policy refusal, transport failure and unknown op. An app
// with more than one request outstanding relies on it to pair a reply with its
// request.
func TestControlRepliesEchoRequestID(t *testing.T) {
	_, child := newTestManager(t)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port

	cases := []struct {
		name       string
		id         uint64
		request    listenRequest
		wantOK     bool
		wantStatus int32
	}{
		{"tcp listener", 41, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0}, true, 0},
		{"udp listener", 42, listenRequest{Op: "listen", Protocol: "udp", Bind: "127.0.0.1", Port: 0}, true, 0},
		{"policy refusal", 43, listenRequest{Op: "listen", Protocol: "udp", Bind: "127.0.0.1", Port: 5353}, false, StatusInvalidArgument},
		{"transport failure", 44, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: occupiedPort}, false, StatusTransport},
		{"unknown op", 45, listenRequest{Op: "accept", Protocol: "tcp"}, false, StatusInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.request
			req.ID = tc.id
			resp := exchange(t, child, req)
			if resp.body.ID != tc.id {
				t.Fatalf("reply id = %d, want %d (%+v)", resp.body.ID, tc.id, resp.body)
			}
			if resp.body.OK != tc.wantOK {
				t.Fatalf("reply ok = %v, want %v (%+v)", resp.body.OK, tc.wantOK, resp.body)
			}
			if !tc.wantOK && resp.body.Error == "" {
				t.Fatalf("refusal carries no error text: %+v", resp.body)
			}
			if tc.wantStatus != 0 && resp.body.Status != tc.wantStatus {
				t.Fatalf("reply status = %d, want %d", resp.body.Status, tc.wantStatus)
			}
			wantFDs := 0
			if tc.wantOK {
				wantFDs = 1
			}
			if len(resp.fds) != wantFDs {
				resp.closeFDs(t)
				t.Fatalf("fds = %d, want %d", len(resp.fds), wantFDs)
			}
			resp.closeFDs(t)
		})
	}
}

// TestControlRepliesAreOrderedPerRequestID sends several pipelined requests with
// distinct ids before reading any reply: each reply must carry its own request's
// id, in request order, so a late reply can never be mistaken for an earlier
// request's answer. A request without an id then replies with 0 instead of
// reusing the previous id, which would be a stale-id regression.
func TestControlRepliesAreOrderedPerRequestID(t *testing.T) {
	_, child := newTestManager(t)

	for _, id := range []uint64{7, 8, 9} {
		one, err := json.Marshal(listenRequest{ID: id, Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := child.Write(one); err != nil {
			t.Fatalf("write request %d: %v", id, err)
		}
	}
	for _, want := range []uint64{7, 8, 9} {
		resp := receive(t, child)
		if resp.body.ID != want {
			t.Fatalf("reply id = %d, want %d (%+v)", resp.body.ID, want, resp.body)
		}
		if !resp.body.OK {
			t.Fatalf("pipelined request %d = %+v, want a listener", want, resp.body)
		}
		resp.closeFDs(t)
	}

	// A refusal keeps the same correlation: no id is carried over from the
	// previous success.
	refused, err := json.Marshal(listenRequest{ID: 0, Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Write(refused); err != nil {
		t.Fatal(err)
	}
	resp := receive(t, child)
	if resp.body.ID != 0 || resp.body.OK || resp.body.Error == "" {
		t.Fatalf("reply to an id-less request = %+v, want id 0 and a refusal", resp.body)
	}
	if len(resp.fds) != 0 {
		resp.closeFDs(t)
		t.Fatalf("refusal attached %d fds", len(resp.fds))
	}
	if resp.body.Status != StatusInvalidArgument {
		t.Fatalf("refusal status = %d, want %d", resp.body.Status, StatusInvalidArgument)
	}
}

// TestControlUndecodableRequestStillReplies covers the one reply that cannot
// name an id: the request itself was not JSON. The reply must still be a
// refusal with the invalid-argument class, never a crash or a silent drop.
func TestControlUndecodableRequestStillReplies(t *testing.T) {
	_, child := newTestManager(t)
	resp := exchange(t, child, "{not json")
	if resp.body.OK || resp.body.Error == "" || resp.body.ID != 0 {
		t.Fatalf("response = %+v, want an id-0 refusal", resp.body)
	}
	if resp.body.Status != StatusInvalidArgument {
		t.Fatalf("status = %d, want %d", resp.body.Status, StatusInvalidArgument)
	}
	if len(resp.fds) != 0 {
		resp.closeFDs(t)
	}
}

// TestListenerAllocationReleaseAccounting covers the lifetime protocol: a
// listener is counted while live, and the explicit close request releases both
// the count and the daemon's retained descriptor.
func TestListenerAllocationReleaseAccounting(t *testing.T) {
	_, child := newTestManager(t)
	first := exchange(t, child, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0})
	if !first.body.OK || first.body.Allocation == 0 {
		t.Fatalf("allocation reply = %+v", first.body)
	}
	first.closeFDs(t)
	if resp := exchange(t, child, listenRequest{Op: "close", Allocation: first.body.Allocation}); !resp.body.OK {
		t.Fatalf("close = %+v", resp.body)
	}
	// The released slot must be reusable: fill the cap exactly.
	for i := range MaxListenersPerRun {
		resp := exchange(t, child, listenRequest{Op: "listen", Protocol: "tcp", Bind: "127.0.0.1", Port: 0})
		if !resp.body.OK || resp.body.Allocation == 0 {
			t.Fatalf("allocation %d after a release = %+v", i, resp.body)
		}
		resp.closeFDs(t)
	}
	// And a second close of the same allocation is idempotent.
	if resp := exchange(t, child, listenRequest{Op: "close", Allocation: first.body.Allocation}); !resp.body.OK {
		t.Fatalf("repeat close = %+v", resp.body)
	}
	if resp := exchange(t, child, listenRequest{Op: "close", Allocation: 999999}); !resp.body.OK {
		t.Fatalf("unknown close = %+v", resp.body)
	}
}

// TestRetainRejectsBeyondLiveCap covers the commit-time guard and rollback: the
// bind pre-check can race with concurrent commits, so retainAllocation must
// refuse and close the descriptor rather than exceed the cap.
func TestRetainRejectsBeyondLiveCap(t *testing.T) {
	m, child := newTestManager(t)
	_ = child
	channel := &runChannel{manager: m, runID: "run_cap", appID: "test.app", allocations: map[uint64]*listenerAlloc{}}
	for i := range MaxListenersPerRun {
		file, err := os.CreateTemp(t.TempDir(), "listener")
		if err != nil {
			t.Fatal(err)
		}
		alloc := &listenerAlloc{id: channel.nextAllocationID(), file: file, protocol: "tcp"}
		if !channel.retainAllocation(alloc) {
			t.Fatalf("allocation %d refused before the cap", i)
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "listener")
	if err != nil {
		t.Fatal(err)
	}
	alloc := &listenerAlloc{id: channel.nextAllocationID(), file: file, protocol: "tcp"}
	if channel.retainAllocation(alloc) {
		t.Fatal("retainAllocation exceeded the live cap")
	}
	if channel.listeners != MaxListenersPerRun {
		t.Fatalf("live count = %d, want %d", channel.listeners, MaxListenersPerRun)
	}
	// Releasing one slot makes room again.
	channel.releaseAllocation(1)
	file2, err := os.CreateTemp(t.TempDir(), "listener")
	if err != nil {
		t.Fatal(err)
	}
	if !channel.retainAllocation(&listenerAlloc{id: channel.nextAllocationID(), file: file2, protocol: "tcp"}) {
		t.Fatal("a released slot was not reusable")
	}
	channel.close()
}
