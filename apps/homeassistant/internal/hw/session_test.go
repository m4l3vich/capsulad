// Package hw integration tests. They drive the real session and bridge stack
// over an in-memory MQTT stream: no broker, no network, no device. What they
// prove is the session contract (discovery on connect, command pump, offline on
// stop, republish after a drop), not a transport result.
package hw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/apps/homeassistant/internal/appcfg"
	"capsulad/apps/homeassistant/internal/ha"
	"capsulad/apps/homeassistant/internal/mqtt"
	"capsulad/sdk/capsulad"
)

// --- in-memory stream -------------------------------------------------------

type memPipe struct {
	mu       sync.Mutex
	cond     *sync.Cond
	toBroker []byte
	toClient []byte
	closed   bool
}

type memEnd struct {
	p      *memPipe
	broker bool
	// ctx is the session's I/O context on the client end. Like the SDK stream
	// adapter, an operation on a done context fails immediately, which is what
	// makes the teardown ordering testable.
	ctx context.Context

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	timer         *time.Timer
}

func newMemPipe() (*memEnd, *memEnd) {
	p := &memPipe{}
	p.cond = sync.NewCond(&p.mu)
	return &memEnd{p: p}, &memEnd{p: p, broker: true}
}

func (e *memEnd) Read(b []byte) (int, error) {
	p := e.p
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if err := e.blocked(false); err != nil {
			return 0, err
		}
		buf := e.buf()
		if len(buf) > 0 {
			n := copy(b, buf)
			e.drop(n)
			return n, nil
		}
		if p.closed {
			return 0, io.EOF
		}
		p.cond.Wait()
	}
}

func (e *memEnd) Write(b []byte) (int, error) {
	p := e.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := e.blocked(true); err != nil {
		return 0, err
	}
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	if e.broker {
		p.toClient = append(p.toClient, b...)
	} else {
		p.toBroker = append(p.toBroker, b...)
	}
	p.cond.Broadcast()
	return len(b), nil
}

func (e *memEnd) Close() error {
	p := e.p
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	return nil
}

// SetReadDeadline implements mqtt.DeadlineConn. The timer wakes a blocked reader
// so an expired deadline is observed instead of waiting forever.
func (e *memEnd) SetReadDeadline(deadline time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readDeadline = deadline
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	if !deadline.IsZero() {
		e.timer = time.AfterFunc(time.Until(deadline), func() {
			e.p.mu.Lock()
			e.p.cond.Broadcast()
			e.p.mu.Unlock()
		})
	}
	return nil
}

// SetWriteDeadline implements mqtt.DeadlineConn for writes.
func (e *memEnd) SetWriteDeadline(deadline time.Time) error {
	e.mu.Lock()
	e.writeDeadline = deadline
	e.mu.Unlock()
	return nil
}

// blocked reports why an operation cannot proceed: a done I/O context or an
// expired deadline. Reads consult the read deadline and writes the write
// deadline, so a publish cannot fail a blocked read.
func (e *memEnd) blocked(write bool) error {
	if e.ctx != nil {
		if err := e.ctx.Err(); err != nil {
			return err
		}
	}
	e.mu.Lock()
	deadline := e.readDeadline
	if write {
		deadline = e.writeDeadline
	}
	e.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}
	return nil
}

func (e *memEnd) buf() []byte {
	if e.broker {
		return e.p.toBroker
	}
	return e.p.toClient
}

func (e *memEnd) drop(n int) {
	if e.broker {
		e.p.toBroker = e.p.toBroker[n:]
		return
	}
	e.p.toClient = e.p.toClient[n:]
}

// --- minimal broker-side packet codec --------------------------------------

type wirePacket struct {
	kind  byte
	flags byte
	body  []byte
}

func readWire(r io.Reader) (wirePacket, error) {
	var out wirePacket
	var header [1]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return out, err
	}
	out.kind = header[0] >> 4
	out.flags = header[0] & 0x0f
	length, multiplier := 0, 1
	for range 4 {
		var digit [1]byte
		if _, err := io.ReadFull(r, digit[:]); err != nil {
			return out, err
		}
		length += int(digit[0]&0x7f) * multiplier
		if digit[0]&0x80 == 0 {
			break
		}
		multiplier *= 128
	}
	out.body = make([]byte, length)
	if _, err := io.ReadFull(r, out.body); err != nil {
		return out, err
	}
	return out, nil
}

func wireOut(kind, flags byte, body []byte) []byte {
	out := []byte{kind<<4 | flags&0x0f}
	length := len(body)
	for {
		digit := byte(length % 128)
		length /= 128
		if length > 0 {
			digit |= 0x80
		}
		out = append(out, digit)
		if length == 0 {
			break
		}
	}
	return append(out, body...)
}

func (p wirePacket) packetID() uint16 {
	if len(p.body) < 2 {
		return 0
	}
	return uint16(p.body[0])<<8 | uint16(p.body[1])
}

// publishParts splits a PUBLISH body into topic, packet id (QoS>0) and payload.
func (p wirePacket) publishParts() (topic string, packetID uint16, payload []byte) {
	qos := (p.flags >> 1) & 0x03
	if len(p.body) < 2 {
		return "", 0, nil
	}
	topicLen := int(p.body[0])<<8 | int(p.body[1])
	if topicLen > len(p.body)-2 {
		topicLen = len(p.body) - 2
	}
	topic = string(p.body[2 : 2+topicLen])
	rest := p.body[2+topicLen:]
	if qos > 0 && len(rest) >= 2 {
		packetID = uint16(rest[0])<<8 | uint16(rest[1])
		rest = rest[2:]
	}
	return topic, packetID, rest
}

// observer records what the broker side saw.
type observer struct {
	mu         sync.Mutex
	published  map[string][]string // topic → payloads in order
	retained   map[string]bool
	subscribed []string
	disconnect bool
}

func newObserver() *observer {
	return &observer{published: map[string][]string{}, retained: map[string]bool{}}
}

func (o *observer) record(topic, payload string, retained bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.published[topic] = append(o.published[topic], payload)
	if retained {
		o.retained[topic] = true
	}
}

func (o *observer) payloads(topic string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.published[topic]...)
}

func (o *observer) payloadCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	total := 0
	for _, payloads := range o.published {
		total += len(payloads)
	}
	return total
}

func (o *observer) subscriptions() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.subscribed...)
}

func (o *observer) isRetained(topic string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.retained[topic]
}

func (o *observer) sawDisconnect() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.disconnect
}

// runBroker reads packets and answers them. inject, when non-nil, is called
// once after the first SUBACK so the test can send a command.
func runBroker(conn *memEnd, obs *observer, inject func(*memEnd)) {
	for {
		pkt, err := readWire(conn)
		if err != nil {
			return
		}
		switch pkt.kind {
		case 1: // CONNECT
			conn.Write(wireOut(2, 0, []byte{0, 0})) // CONNACK accepted
		case 8: // SUBSCRIBE
			id := pkt.packetID()
			body := pkt.body[2:]
			codes := []byte{byte(id >> 8), byte(id)}
			for len(body) >= 2 {
				topicLen := int(body[0])<<8 | int(body[1])
				if len(body) < 2+topicLen+1 {
					break
				}
				obs.mu.Lock()
				obs.subscribed = append(obs.subscribed, string(body[2:2+topicLen]))
				obs.mu.Unlock()
				codes = append(codes, 0)
				body = body[2+topicLen+1:]
			}
			conn.Write(wireOut(9, 0, codes))
			if inject != nil {
				inject(conn)
				inject = nil
			}
		case 3: // PUBLISH
			topic, packetID, payload := pkt.publishParts()
			obs.record(topic, string(payload), pkt.flags&0x01 != 0)
			if (pkt.flags>>1)&0x03 == 1 {
				conn.Write(wireOut(4, 0, []byte{byte(packetID >> 8), byte(packetID)}))
			}
		case 12: // PINGREQ
			conn.Write(wireOut(13, 0, nil))
		case 14: // DISCONNECT
			obs.mu.Lock()
			obs.disconnect = true
			obs.mu.Unlock()
			return
		}
	}
}

func publishCommand(conn *memEnd, topic, payload string) {
	body := []byte{byte(len(topic) >> 8), byte(len(topic))}
	body = append(body, topic...)
	body = append(body, payload...)
	conn.Write(wireOut(3, 0x00, body))
}

// --- fake hardware ----------------------------------------------------------

type fakeHardware struct {
	mu     sync.Mutex
	calls  []string
	onRing func()
}

func (f *fakeHardware) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	hook := f.onRing
	f.mu.Unlock()
	if hook != nil && call == "ring-colour" {
		hook()
	}
}

func (f *fakeHardware) RingSetColor(ha.RGB, float64) error { f.record("ring-colour"); return nil }
func (f *fakeHardware) RingSetEffect(string) error         { f.record("ring-effect"); return nil }
func (f *fakeHardware) RingRelease()                       { f.record("ring-release") }
func (f *fakeHardware) DisplaySetFace(string) error        { f.record("display-face"); return nil }
func (f *fakeHardware) DisplayRelease()                    { f.record("display-release") }

func (f *fakeHardware) ReleaseRingAndDisplay() {
	f.record("ring-release")
	f.record("display-release")
}
func (f *fakeHardware) MediaCommand(string) error { f.record("media"); return nil }

func (f *fakeHardware) callsList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// --- test fixture -----------------------------------------------------------

// sessionPublisher adapts a session to the bridge's Publisher seam. The bridge
// is constructed before the session exists, so the proxy is wired afterwards.
type sessionPublisher struct {
	mu      sync.RWMutex
	session *Session
}

func (p *sessionPublisher) Publish(ctx context.Context, topic string, payload []byte, retain bool, qos byte) error {
	p.mu.RLock()
	session := p.session
	p.mu.RUnlock()
	if session == nil {
		return ha.ErrNoSession
	}
	return session.Publish(ctx, topic, payload, retain, qos)
}

func (p *sessionPublisher) set(session *Session) {
	p.mu.Lock()
	p.session = session
	p.mu.Unlock()
}

func valuesWith(pairs map[string]string) appcfg.Values {
	values := appcfg.Default()
	for key, raw := range pairs {
		values[key] = raw
	}
	return values
}

func testSession(t *testing.T, inject func(*memEnd), obs *observer) (*Session, *ha.Bridge, *fakeHardware) {
	t.Helper()
	cfg, warnings, err := appcfg.Build(valuesWith(map[string]string{
		appcfg.KeyBrokerHost: `"broker.test"`,
	}))
	if err != nil {
		t.Fatalf("Build config: %v (warnings %v)", err, warnings)
	}
	topics, err := cfg.TopicLayout("capsula")
	if err != nil {
		t.Fatalf("TopicLayout: %v", err)
	}
	caps := ha.Capabilities{SegmentDisplay: true, Ring: true, RingRGB: true}
	ringPresets := []ha.Preset{{ID: "system.ring.pulse", Kind: ha.KindRingEffect}}
	facePresets := []ha.Preset{{ID: "system.face.neutral", Kind: ha.KindSegmentFace}}
	catalog := ha.BuildCatalog(topics, ha.Device{NodeID: "capsula", Name: "Capsula Mini"}, caps, nil,
		ringPresets, facePresets, cfg.BuildOptions())

	hardware := &fakeHardware{}
	proxy := &sessionPublisher{}
	bridge := ha.NewBridge(topics, catalog, proxy, hardware, ha.Actions{}, t.Logf)
	bridge.SetPresets(ringPresets, facePresets)

	session := NewSession(capsulad.Env{AppID: "org.capsulad.homeassistant", RunID: "run"},
		cfg, bridge, nil, t.Logf)
	session.dial = func(_ context.Context, ioCtx context.Context, _ capsulad.Env, _ appcfg.Config) (mqtt.Conn, error) {
		clientEnd, brokerEnd := newMemPipe()
		clientEnd.ctx = ioCtx
		go runBroker(brokerEnd, obs, inject)
		return clientEnd, nil
	}
	proxy.set(session)
	return session, bridge, hardware
}

func TestSessionPublishesDiscoveryAppliesCommandsAndStopsCleanly(t *testing.T) {
	obs := newObserver()
	commandSent := make(chan struct{})
	var bridge *ha.Bridge
	session, createdBridge, hw := testSession(t, func(conn *memEnd) {
		// The bridge is assigned before the session dials, which is what
		// runs this closure.
		publishCommand(conn, bridge.Topics().SetTopic(ha.SlugRing),
			`{"state":"ON","brightness":32,"color":{"r":1,"g":2,"b":3}}`)
		close(commandSent)
	}, obs)
	bridge = createdBridge

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.runOnce(ctx)
		done <- err
	}()

	select {
	case <-commandSent:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("the bridge never subscribed")
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(hw.callsList()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the inbound light command never reached the hardware")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Every entity has a retained discovery payload.
	for _, entity := range bridge.Catalog().Entities {
		topic := bridge.Topics().DiscoveryTopic(entity.Component, entity.Slug)
		payloads := obs.payloads(topic)
		if len(payloads) == 0 {
			t.Errorf("no discovery payload for %s.%s", entity.Component, entity.Slug)
			continue
		}
		if !obs.isRetained(topic) {
			t.Errorf("%s discovery was not retained", topic)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payloads[0]), &decoded); err != nil {
			t.Errorf("%s payload is not JSON: %v", topic, err)
		}
	}
	status := obs.payloads(bridge.Topics().StatusTopic())
	if len(status) == 0 || status[0] != ha.PayloadOnline {
		t.Fatalf("availability payloads %v, want the first to be online", status)
	}
	if got, want := len(obs.subscriptions()), len(bridge.Catalog().CommandTopics()); got < want {
		t.Fatalf("subscribed to %d topics, want at least %d", got, want)
	}

	// The changed ring state is republished after the command.
	ringStates := obs.payloads(bridge.Topics().StateTopic(ha.SlugRing))
	if len(ringStates) < 2 {
		t.Fatalf("ring state payloads %v, want the command to republish", ringStates)
	}
	if !contains(ringStates[len(ringStates)-1], `"ON"`) {
		t.Fatalf("last ring state %q does not report ON", ringStates[len(ringStates)-1])
	}

	// Stopping the runtime cancels the context that started the session. The
	// retained offline marker and the DISCONNECT must still reach the broker:
	// they are written on the session's own I/O context, which only the teardown
	// cancels. The in-memory stream here fails an operation on a done context
	// exactly like the SDK stream does, so this is the regression test for a
	// stop that degenerated into the crash path.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the session did not stop")
	}
	status = obs.payloads(bridge.Topics().StatusTopic())
	if last := status[len(status)-1]; last != ha.PayloadOffline {
		t.Fatalf("final availability payload %q, want offline", last)
	}
	if !obs.isRetained(bridge.Topics().StatusTopic()) {
		t.Fatal("the availability topic was not retained")
	}
	if !obs.sawDisconnect() {
		t.Fatal("the session did not send DISCONNECT")
	}
	if session.Connected() {
		t.Fatal("the session still reports a live connection")
	}
	if endpoint := session.Endpoint(); endpoint != "" {
		t.Fatalf("Endpoint() = %q after teardown", endpoint)
	}
	if bridge.Available() {
		t.Fatal("the bridge still reports an available session")
	}
	if snapshot := bridge.Snapshot(); snapshot.Counters.PublishErrs != 0 {
		t.Fatalf("the teardown recorded publish failures: %+v", snapshot.Counters)
	}
}

func TestSessionRepublishesDiscoveryAfterADrop(t *testing.T) {
	obs := newObserver()
	session, bridge, _ := testSession(t, nil, obs)

	cycle := func() int {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		session.runOnce(ctx) //nolint:errcheck // each cycle ends by the deadline
		return obs.payloadCount()
	}
	first := cycle()
	if first == 0 {
		t.Fatal("the first session published nothing")
	}
	second := cycle()
	if second <= first {
		t.Fatalf("payload count %d after a reconnect, want more than %d", second, first)
	}
	for _, entity := range bridge.Catalog().Entities {
		topic := bridge.Topics().DiscoveryTopic(entity.Component, entity.Slug)
		if len(obs.payloads(topic)) < 2 {
			t.Fatalf("%s was not republished after the drop", topic)
		}
	}
}

func TestSessionRejectsPublishWithoutConnection(t *testing.T) {
	obs := newObserver()
	session, _, _ := testSession(t, nil, obs)
	if err := session.Publish(context.Background(), "a", []byte("b"), false, 0); !errors.Is(err, ha.ErrNoSession) {
		t.Fatalf("Publish without a session = %v, want ha.ErrNoSession", err)
	}
	if session.Connected() {
		t.Fatal("Connected is true without a session")
	}
	if session.LastError() != "" {
		t.Fatalf("unexpected last error %q", session.LastError())
	}
}

func TestSessionReconnectRequestEndsTheCycle(t *testing.T) {
	obs := newObserver()
	session, bridge, _ := testSession(t, nil, obs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.runOnce(ctx)
		done <- err
	}()
	// Wait until the session is connected before asking for a new one.
	deadline := time.Now().Add(3 * time.Second)
	for !session.Connected() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the session never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if endpoint := session.Endpoint(); endpoint != "broker.test:1883" {
		t.Fatalf("Endpoint() = %q, want the dialled endpoint", endpoint)
	}
	session.Reconnect("test")
	select {
	case err := <-done:
		if !errors.Is(err, ErrReconnectRequested) {
			t.Fatalf("runOnce = %v, want ErrReconnectRequested", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a reconnect request did not end the session")
	}
	status := obs.payloads(bridge.Topics().StatusTopic())
	if len(status) == 0 || status[len(status)-1] != ha.PayloadOffline {
		t.Fatalf("availability payloads %v, want offline before the reconnect", status)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A broker that accepts the CONNECT and the SUBSCRIBE and then never answers the
// SUBSCRIBE must fail the session and be retried, not leave a connection that
// looks online while Home Assistant's commands go nowhere.
func TestSessionBoundsTheSubscribeExchange(t *testing.T) {
	obs := newObserver()
	session, bridge, _ := testSession(t, nil, obs)
	session.subscribeTimeout = 200 * time.Millisecond
	session.dial = func(_ context.Context, ioCtx context.Context, _ capsulad.Env, _ appcfg.Config) (mqtt.Conn, error) {
		clientEnd, brokerEnd := newMemPipe()
		clientEnd.ctx = ioCtx
		go func() {
			// CONNECT and the discovery publishes are answered; the SUBSCRIBE is
			// deliberately ignored, which is the condition under test.
			if _, err := readWire(brokerEnd); err != nil {
				return
			}
			brokerEnd.Write(wireOut(2, 0, []byte{0, 0}))
			for {
				pkt, err := readWire(brokerEnd)
				if err != nil {
					return
				}
				switch pkt.kind {
				case 3: // PUBLISH: record, and acknowledge so discovery is not what times out
					topic, id, payload := pkt.publishParts()
					obs.record(topic, string(payload), pkt.flags&0x01 != 0)
					if (pkt.flags>>1)&0x03 == 1 {
						brokerEnd.Write(wireOut(4, 0, []byte{byte(id >> 8), byte(id)}))
					}
				case 14: // DISCONNECT ends the script
					obs.mu.Lock()
					obs.disconnect = true
					obs.mu.Unlock()
					return
				}
			}
		}()
		return clientEnd, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.runOnce(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a broker that never answers SUBSCRIBE was reported as established")
		}
		if !strings.Contains(err.Error(), "subscribe") {
			t.Fatalf("runOnce = %v, want a subscribe error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session stalled waiting for a SUBACK")
	}
	// The failed session still announces itself offline and disconnects.
	status := obs.payloads(bridge.Topics().StatusTopic())
	if len(status) == 0 || status[len(status)-1] != ha.PayloadOffline {
		t.Fatalf("availability payloads %v, want the last to be offline", status)
	}
	if !obs.sawDisconnect() {
		t.Fatal("the failed session did not send DISCONNECT")
	}
	if session.Connected() {
		t.Fatal("the session still reports a live connection")
	}
}

func TestAdvanceBackoff(t *testing.T) {
	cases := []struct {
		name     string
		previous time.Duration
		usable   bool
		ranFor   time.Duration
		want     time.Duration
	}{
		// A session that never became usable must decay from the first retry.
		{"seeded", 0, false, time.Second, minBackoff},
		{"never usable", minBackoff, false, 5 * time.Second, 2 * time.Second},
		{"never usable, growing", 4 * time.Second, false, 5 * time.Second, 8 * time.Second},
		{"never usable, capped", maxBackoff, false, 5 * time.Second, maxBackoff},
		// A session that became usable and dropped immediately is flapping.
		{"usable but flapping", 4 * time.Second, true, 2 * time.Second, 8 * time.Second},
		{"just under the threshold", minBackoff, true, backoffResetAfter - time.Millisecond, 2 * time.Second},
		{"flapping, capped", maxBackoff, true, time.Second, maxBackoff},
		// A session that held up starts again promptly.
		{"usable and stable", 32 * time.Second, true, backoffResetAfter, minBackoff},
		{"usable far past the threshold", maxBackoff, true, time.Hour, minBackoff},
	}
	for _, tc := range cases {
		if got := advanceBackoff(tc.previous, tc.usable, tc.ranFor); got != tc.want {
			t.Fatalf("%s: advanceBackoff(%s, %v, %s) = %s, want %s",
				tc.name, tc.previous, tc.usable, tc.ranFor, got, tc.want)
		}
	}
}

func TestBackoffDecaysForABrokerThatNeverAcceptsCommands(t *testing.T) {
	// The same scenario as TestSessionBoundsTheSubscribeExchange, run through
	// Run: a broker that answers CONNACK and ignores SUBSCRIBE must not be
	// retried once a second forever.
	obs := newObserver()
	session, _, _ := testSession(t, nil, obs)
	session.subscribeTimeout = 50 * time.Millisecond
	dials := make(chan struct{}, 8)
	session.dial = func(_ context.Context, ioCtx context.Context, _ capsulad.Env, _ appcfg.Config) (mqtt.Conn, error) {
		select {
		case dials <- struct{}{}:
		default:
		}
		clientEnd, brokerEnd := newMemPipe()
		clientEnd.ctx = ioCtx
		go func() {
			if _, err := readWire(brokerEnd); err != nil {
				return
			}
			brokerEnd.Write(wireOut(2, 0, []byte{0, 0}))
			for {
				pkt, err := readWire(brokerEnd)
				if err != nil {
					return
				}
				if pkt.kind == 3 {
					_, id, _ := pkt.publishParts()
					if (pkt.flags>>1)&0x03 == 1 {
						brokerEnd.Write(wireOut(4, 0, []byte{byte(id >> 8), byte(id)}))
					}
				}
			}
		}()
		return clientEnd, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	go session.Run(ctx)
	// Attempt 1 starts at once, the second follows minBackoff (1 s) and the
	// third follows 2 s. Sampling 2.4 s after the first attempt therefore sees
	// exactly two: without the decay the third would already have fired at
	// ~2 s, which is the difference between a decaying retry and a
	// once-a-second flap. The channel is only inspected, never drained.
	deadline := time.Now().Add(2 * time.Second)
	for len(dials) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the session never dialled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if wait := time.Until(started.Add(2400 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if attempts := len(dials); attempts != 2 {
		t.Fatalf("%d dial attempts in ~2.4s, want 2 (one immediate, one after the first backoff)", attempts)
	}
}
