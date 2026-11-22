package link_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/apps/cadence/internal/link"
	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/settings"
	"capsulad/apps/cadence/internal/wire"
	"capsulad/sdk/websocket"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeConn struct {
	in     chan frame
	done   chan struct{}
	once   sync.Once
	closed atomic.Bool

	mu      sync.Mutex
	written []*cadencev2.Envelope
	onPong  func([]byte)
	// autoWelcome answers the Hello with a Welcome, as the server does.
	autoWelcome bool
}

type frame struct {
	op      websocket.Opcode
	payload []byte
}

func newFakeConn(autoWelcome bool) *fakeConn {
	return &fakeConn{in: make(chan frame, 16), done: make(chan struct{}), autoWelcome: autoWelcome}
}

func (c *fakeConn) ReadMessage(ctx context.Context) (websocket.Opcode, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-c.done:
		return 0, nil, io.EOF
	case f := <-c.in:
		return f.op, f.payload, nil
	}
}

func (c *fakeConn) WriteBinary(ctx context.Context, payload []byte) error {
	if c.closed.Load() {
		return io.ErrClosedPipe
	}
	env, err := wire.Decode(payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.written = append(c.written, env)
	c.mu.Unlock()
	if env.GetHello() != nil && c.autoWelcome {
		c.push(&cadencev2.Envelope{Body: &cadencev2.Envelope_Welcome{
			Welcome: &cadencev2.Welcome{ServerVersion: "test", SendFullReport: true},
		}})
	}
	return nil
}

func (c *fakeConn) WritePing(ctx context.Context, payload []byte) error {
	c.mu.Lock()
	handler := c.onPong
	c.mu.Unlock()
	if handler != nil {
		handler(payload)
	}
	return nil
}

func (c *fakeConn) SetPongHandler(f func([]byte)) {
	c.mu.Lock()
	c.onPong = f
	c.mu.Unlock()
}

func (c *fakeConn) CloseNow() error {
	c.closed.Store(true)
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *fakeConn) push(env *cadencev2.Envelope) {
	b, err := wire.Encode(env)
	if err != nil {
		panic(err)
	}
	select {
	case c.in <- frame{op: 0x2, payload: b}:
	default:
	}
}

func (c *fakeConn) sent() []*cadencev2.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*cadencev2.Envelope, len(c.written))
	copy(out, c.written)
	return out
}

type fakeDialer struct {
	mu    sync.Mutex
	conns []*fakeConn
	err   error
	dials int
	auto  bool
}

func (d *fakeDialer) Dial(context.Context) (link.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	if d.err != nil {
		return nil, d.err
	}
	c := newFakeConn(d.auto)
	d.conns = append(d.conns, c)
	return c, nil
}

func (d *fakeDialer) last() *fakeConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.conns) == 0 {
		return nil
	}
	return d.conns[len(d.conns)-1]
}

func (d *fakeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

type fakeHandler struct {
	mu       sync.Mutex
	messages []string
	ups      []bool
}

func (h *fakeHandler) LinkChanged(up bool, generation uint64) {
	h.mu.Lock()
	h.ups = append(h.ups, up)
	h.mu.Unlock()
}

func (h *fakeHandler) Message(_ uint64, env *cadencev2.Envelope) {
	h.mu.Lock()
	h.messages = append(h.messages, envelopeKind(env))
	h.mu.Unlock()
}

func (h *fakeHandler) kinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.messages))
	copy(out, h.messages)
	return out
}

func (h *fakeHandler) upEvents() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]bool, len(h.ups))
	copy(out, h.ups)
	return out
}

func envelopeKind(env *cadencev2.Envelope) string {
	switch env.GetBody().(type) {
	case *cadencev2.Envelope_SessionClose:
		return "session_close"
	case *cadencev2.Envelope_StreamStart:
		return "stream_start"
	default:
		return "other"
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func start(t *testing.T, d link.Dialer, h link.Handler) (*link.Client, *syncBuffer, context.CancelFunc) {
	t.Helper()
	buf := &syncBuffer{}
	logger := log.New(buf, "", 0)
	client, err := link.New(d, logger, h, "test/1", "capsula-mini")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = client.Run(ctx) }()
	return client, buf, cancel
}

func TestHandshakeAdvertisesAudioOutput(t *testing.T) {
	d := &fakeDialer{auto: true}
	h := &fakeHandler{}
	_, _, cancel := start(t, d, h)
	defer cancel()

	waitFor(t, "handshake", func() bool { return d.last() != nil && len(d.last().sent()) >= 3 })
	sent := d.last().sent()
	hello := sent[0].GetHello()
	if hello.GetProtocol() != 2 || hello.GetBoard() != "capsula-mini" || hello.GetDaemonVersion() != "test/1" {
		t.Fatalf("hello = %v", hello)
	}
	report := sent[1].GetCapabilityReport()
	if report == nil || len(report.GetDescriptors()) != 1 {
		t.Fatalf("report = %v", report)
	}
	desc := report.GetDescriptors()[0]
	if desc.GetId() != "audio.output" || desc.GetVersion() != 1 || len(desc.GetCommands()) != 0 {
		t.Fatalf("descriptor = %v", desc)
	}
	want, err := wire.ReportHash(report)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex(report.GetHash()); got != hex(want) {
		t.Fatalf("report hash = %s, want %s", got, hex(want))
	}
	if string(hello.GetCapabilitiesHash()) != string(report.GetHash()) {
		t.Fatal("Hello hash and report hash differ")
	}
	state := sent[2].GetStateDelta()
	if state.GetCapability() != "audio.output" || state.GetJson() != `{"channels":2,"maxBitrate":128000}` {
		t.Fatalf("state = %v", state)
	}
	waitFor(t, "link up event", func() bool {
		ups := h.upEvents()
		return len(ups) == 1 && ups[0]
	})
}

func TestMessagesReachTheHandler(t *testing.T) {
	d := &fakeDialer{auto: true}
	h := &fakeHandler{}
	client, _, cancel := start(t, d, h)
	defer cancel()
	waitFor(t, "link up", func() bool { return client.Connected() })

	d.last().push(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionClose{SessionClose: &cadencev2.SessionClose{
		SessionId: "s1", Code: "ok",
	}}})
	waitFor(t, "session close", func() bool {
		for _, k := range h.kinds() {
			if k == "session_close" {
				return true
			}
		}
		return false
	})

	// An unknown (newer) arm is skipped, not an error: field 99, wire type
	// varint, is not a body this client declares.
	d.last().in <- frame{op: 0x2, payload: []byte{0x98, 0x06, 0x01}}
	time.Sleep(20 * time.Millisecond)
	if !client.Connected() {
		t.Fatal("an unknown arm closed the link")
	}
}

func TestSendCarriesEnvelopes(t *testing.T) {
	d := &fakeDialer{auto: true}
	h := &fakeHandler{}
	client, _, cancel := start(t, d, h)
	defer cancel()
	waitFor(t, "link up", func() bool { return client.Connected() })

	err := client.Send(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionStart{SessionStart: &cadencev2.SessionStart{
		SessionId: "s1", Trigger: cadencev2.Trigger_TRIGGER_WAKE,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sent session start", func() bool {
		for _, env := range d.last().sent() {
			if env.GetSessionStart() != nil {
				return true
			}
		}
		return false
	})
}

func TestSendOffline(t *testing.T) {
	d := &fakeDialer{err: errors.New("dial failed")}
	h := &fakeHandler{}
	client, _, cancel := start(t, d, h)
	defer cancel()
	if err := client.Send(&cadencev2.Envelope{}); !errors.Is(err, link.ErrOffline) {
		t.Fatalf("got %v", err)
	}
}

func TestUnauthorizedLoggedOnce(t *testing.T) {
	d := &fakeDialer{err: websocket.ErrUnauthorized}
	h := &fakeHandler{}
	_, logBuf, cancel := start(t, d, h)

	waitFor(t, "one unauthorized log", func() bool {
		return bytes.Count([]byte(logBuf.String()), []byte("link_unauthorized")) == 1
	})
	time.Sleep(50 * time.Millisecond)
	if n := bytes.Count([]byte(logBuf.String()), []byte("link_unauthorized")); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	cancel()
}

// blockingDialer's first dial parks until its context ends, modelling a slow
// connect; a Wake must cancel it and start a new attempt.
type blockingDialer struct {
	started chan struct{}
	once    sync.Once
	mu      sync.Mutex
	dials   int
	newConn chan struct{}
}

func (d *blockingDialer) Dial(ctx context.Context) (link.Conn, error) {
	d.mu.Lock()
	d.dials++
	n := d.dials
	d.mu.Unlock()
	if n == 1 {
		d.once.Do(func() { close(d.started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case d.newConn <- struct{}{}:
	default:
	}
	return nil, errors.New("stop after the second attempt")
}

func TestWakeDuringDialCancelsTheAttempt(t *testing.T) {
	d := &blockingDialer{started: make(chan struct{}), newConn: make(chan struct{}, 1)}
	buf := &syncBuffer{}
	client, err := link.New(d, log.New(buf, "", 0), &fakeHandler{}, "test/1", "capsula-mini")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	waitFor(t, "first dial", func() bool {
		select {
		case <-d.started:
			return true
		default:
			return false
		}
	})
	client.Wake()
	waitFor(t, "second dial", func() bool {
		select {
		case <-d.newConn:
			return true
		default:
			return false
		}
	})
}

func TestWakeRedials(t *testing.T) {
	d := &fakeDialer{auto: true}
	h := &fakeHandler{}
	client, _, cancel := start(t, d, h)
	defer cancel()
	waitFor(t, "link up", func() bool { return client.Connected() })
	first := d.dialCount()

	client.Wake()
	waitFor(t, "second dial", func() bool { return d.dialCount() > first })
	waitFor(t, "link back", func() bool { return client.Connected() })
	// The link-down then link-up pair was reported to the handler.
	ups := h.upEvents()
	if len(ups) < 3 || !ups[len(ups)-1] {
		t.Fatalf("link events = %v", ups)
	}
}

func hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = digits[v>>4]
		out[2*i+1] = digits[v&0xf]
	}
	return string(out)
}

// The board advertised in Hello follows the persisted board setting, so a
// device enrolled as the dashboard's Simulator reports the same id after a
// reconnect.
func TestSetBoardChangesHello(t *testing.T) {
	d := &fakeDialer{auto: true}
	h := &fakeHandler{}
	buf := &syncBuffer{}
	logger := log.New(buf, "", 0)
	client, err := link.New(d, logger, h, "test/1", "vk-capsula-mini")
	if err != nil {
		t.Fatal(err)
	}
	client.SetBoard("sim")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	waitFor(t, "hello", func() bool {
		c := d.last()
		return c != nil && len(c.sent()) > 0
	})
	hello := d.last().sent()[0].GetHello()
	if hello == nil || hello.GetBoard() != "sim" {
		t.Fatalf("hello = %v", hello)
	}
	if client.Board() != "sim" {
		t.Fatalf("Board() = %q", client.Board())
	}
}

// An enrollment code is not a bearer token: the dialer refuses before any
// network I/O, and the link stays unconfigured until the exchange succeeds.
func TestDialerRequiresAToken(t *testing.T) {
	d := &link.SDKDialer{Config: func() settings.Config {
		return settings.Config{BaseURL: "https://cadence.example.com", EnrollCode: "K7M4QX"}
	}}
	if _, err := d.Dial(context.Background()); !errors.Is(err, link.ErrNoServer) {
		t.Fatalf("Dial = %v, want ErrNoServer", err)
	}
}

func TestConfigSetIsAcknowledgedOnTheSourceConnection(t *testing.T) {
	d := &fakeDialer{auto: true}
	h := &fakeHandler{}
	client, _, cancel := start(t, d, h)
	defer cancel()
	waitFor(t, "link up", func() bool { return client.Connected() })

	conn := d.last()
	conn.push(&cadencev2.Envelope{Body: &cadencev2.Envelope_ConfigSet{ConfigSet: &cadencev2.ConfigSet{
		Capability: "audio.output", Generation: 3, Json: "{}",
	}}})
	waitFor(t, "config applied", func() bool {
		for _, env := range conn.sent() {
			a := env.GetConfigApplied()
			if a != nil && a.GetCapability() == "audio.output" && a.GetGeneration() == 3 && a.GetErrorCode() == "" {
				return true
			}
		}
		return false
	})
	conn.push(&cadencev2.Envelope{Body: &cadencev2.Envelope_ConfigSet{ConfigSet: &cadencev2.ConfigSet{
		Capability: "audio.output", Generation: 4, Json: `{"delayMs":5}`,
	}}})
	waitFor(t, "unsupported config", func() bool {
		for _, env := range conn.sent() {
			a := env.GetConfigApplied()
			if a != nil && a.GetGeneration() == 4 && a.GetErrorCode() == "unsupported" {
				return true
			}
		}
		return false
	})
	// The controller never sees ConfigSet.
	if h.kinds() != nil && len(h.kinds()) != 0 {
		t.Fatalf("controller got %v", h.kinds())
	}
}
