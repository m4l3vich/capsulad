package mqtt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipe is an in-memory, unbounded byte pipe between the client under test and
// a scripted broker. It is test-only: no network, no listening socket and no
// loopback server, so the test proves protocol behavior and nothing about a
// transport.
type pipe struct {
	mu       sync.Mutex
	cond     *sync.Cond
	toBroker []byte
	toClient []byte
	closed   bool
}

type pipeEnd struct {
	p      *pipe
	broker bool
}

func newPipe() (*pipeEnd, *pipeEnd) {
	// The condition variable must share the pipe's mutex: Read locks p.mu and
	// then waits on the same lock, which a second mutex would not satisfy.
	p := &pipe{}
	p.cond = sync.NewCond(&p.mu)
	return &pipeEnd{p: p}, &pipeEnd{p: p, broker: true}
}

// lonePipeEnd is one end of a pipe with no peer, for tests that only exercise
// argument validation.
func lonePipeEnd() *pipeEnd {
	client, _ := newPipe()
	return client
}

func (e *pipeEnd) Read(b []byte) (int, error) {
	p := e.p
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		buf := e.inbox()
		if len(buf) > 0 {
			n := copy(b, buf)
			e.consume(n)
			return n, nil
		}
		if p.closed {
			return 0, io.EOF
		}
		p.cond.Wait()
	}
}

func (e *pipeEnd) Write(b []byte) (int, error) {
	p := e.p
	p.mu.Lock()
	defer p.mu.Unlock()
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

func (e *pipeEnd) Close() error {
	p := e.p
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	return nil
}

func (e *pipeEnd) inbox() []byte {
	if e.broker {
		return e.p.toBroker
	}
	return e.p.toClient
}

func (e *pipeEnd) consume(n int) {
	if e.broker {
		e.p.toBroker = e.p.toBroker[n:]
		return
	}
	e.p.toClient = e.p.toClient[n:]
}

// rawPacket is a packet read without interpretation, so a test broker can read
// the client-only packets (CONNECT, SUBSCRIBE) that the production decoder
// deliberately rejects.
type rawPacket struct {
	packetType byte
	flags      byte
	body       []byte
}

func readRawPacket(r io.Reader) (rawPacket, error) {
	var out rawPacket
	var header [1]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return out, err
	}
	out.packetType = header[0] >> 4
	out.flags = header[0] & 0x0f
	remaining, _, err := readRemainingLength(r)
	if err != nil {
		return out, err
	}
	out.body = make([]byte, remaining)
	if _, err := io.ReadFull(r, out.body); err != nil {
		return out, err
	}
	return out, nil
}

func (p rawPacket) encode() []byte {
	raw, err := frame(p.packetType, p.flags, p.body)
	if err != nil {
		panic(err)
	}
	return raw
}

// testBroker drives a protocol script against one client.
type testBroker struct {
	t    *testing.T
	conn *pipeEnd
}

func (b *testBroker) read(want byte) rawPacket {
	b.t.Helper()
	pkt, err := readRawPacket(b.conn)
	if err != nil {
		b.t.Errorf("broker read: %v", err)
		return rawPacket{}
	}
	if pkt.packetType != want {
		b.t.Errorf("broker read %s, want %s", packetName(pkt.packetType), packetName(want))
	}
	return pkt
}

func (b *testBroker) send(raw []byte) {
	b.t.Helper()
	if _, err := b.conn.Write(raw); err != nil {
		b.t.Errorf("broker write: %v", err)
	}
}

func (b *testBroker) connack(code byte) {
	b.t.Helper()
	raw, err := frame(packetTypeConnack, 0, []byte{0, code})
	if err != nil {
		b.t.Errorf("frame connack: %v", err)
	}
	b.send(raw)
}

func (b *testBroker) connect() connectOptions {
	b.t.Helper()
	pkt := b.read(packetTypeConnect)
	decoded, err := decodeConnectForTest(pkt.encode())
	if err != nil {
		b.t.Errorf("decode connect: %v", err)
	}
	b.connack(connackAccepted)
	return decoded
}

func (b *testBroker) publish(topic string, payload []byte, qos byte, retain bool, packetID uint16) {
	b.t.Helper()
	msg := Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain}
	raw, err := encodePublish(msg, packetID)
	if err != nil {
		b.t.Errorf("encode publish: %v", err)
		return
	}
	b.send(raw)
}

// drainUntilEOF consumes packets until the peer closes, so a broker goroutine
// never outlives the test.
func (b *testBroker) drainUntilEOF() {
	b.t.Helper()
	for {
		if _, err := readRawPacket(b.conn); err != nil {
			return
		}
	}
}

func packetIDOf(p rawPacket) uint16 {
	if len(p.body) < 2 {
		return 0
	}
	return uint16(p.body[0])<<8 | uint16(p.body[1])
}

func newTestClient(conn *pipeEnd, cfg Config) *Client {
	if cfg.ClientID == "" {
		cfg.ClientID = "capsulad-test"
	}
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = time.Second
	}
	return NewClient(conn, cfg)
}

func TestClientSessionHappyPath(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	client := newTestClient(clientEnd, Config{
		KeepAlive:   10 * time.Second,
		Username:    "user",
		HasUsername: true,
		Password:    "secret",
		HasPassword: true,
		Will:        &Message{Topic: "ha/status", Payload: []byte("offline"), QoS: 1, Retain: true},
	})

	brokerDone := make(chan struct{})
	// The broker side is a dispatcher, not a fixed script: an inbound PUBLISH
	// and the client's own PUBLISH are independent, and MQTT does not order
	// them. Recording what arrives and asserting afterwards is what keeps this
	// test about the protocol instead of about scheduling luck.
	var (
		mu              sync.Mutex
		subscribed      []string
		ackedIDs        []uint16
		published       []Message
		sawDisconnect   bool
		acceptedConnect bool
	)
	record := func(fn func()) {
		mu.Lock()
		fn()
		mu.Unlock()
	}
	go func() {
		defer close(brokerDone)
		opts := broker.connect()
		record(func() { acceptedConnect = true })
		if !opts.cleanSession {
			t.Error("clean session flag not set")
		}
		if opts.will == nil || opts.will.Topic != "ha/status" || !opts.will.Retain || opts.will.QoS != 1 {
			t.Errorf("will not carried by CONNECT: %+v", opts.will)
		}
		if !opts.hasUsername || opts.username != "user" || !opts.hasPassword || opts.password != "secret" {
			t.Errorf("credentials not carried by CONNECT: %+v", opts)
		}
		commandSent := false
		for {
			pkt, err := readRawPacket(brokerEnd)
			if err != nil {
				return
			}
			switch pkt.packetType {
			case packetTypeSubscribe:
				id := packetIDOf(pkt)
				body := pkt.body[2:]
				for len(body) >= 2 {
					topicLen := int(body[0])<<8 | int(body[1])
					if len(body) < 2+topicLen+1 {
						break
					}
					topic := string(body[2 : 2+topicLen])
					record(func() { subscribed = append(subscribed, topic) })
					body = body[2+topicLen+1:]
				}
				ack, _ := frame(packetTypeSuback, 0, []byte{byte(id >> 8), byte(id), 0})
				broker.send(ack)
				if !commandSent {
					commandSent = true
					// Broker → client command at QoS 1: the client must
					// acknowledge it, in whatever order its own traffic follows.
					payload := []byte("ON")
					command := append([]byte{0x00, 0x05}, []byte("a/set")...)
					command = append(command, 0x00, 0x2a)
					command = append(command, payload...)
					raw, _ := frame(packetTypePublish, 0x02, command)
					broker.send(raw)
				}
			case packetTypePublish:
				decoded, err := decodePacketForTest(pkt)
				if err != nil {
					t.Errorf("decode publish: %v", err)
					continue
				}
				record(func() {
					published = append(published, Message{
						Topic: decoded.topic, Payload: decoded.payload,
						QoS: decoded.qos, Retain: decoded.retain,
					})
				})
				if decoded.qos == 1 {
					ack, _ := encodePacketID(packetTypePuback, decoded.packetID)
					broker.send(ack)
				}
			case packetTypePuback:
				id := packetIDOf(pkt)
				record(func() { ackedIDs = append(ackedIDs, id) })
			case packetTypePingreq:
				resp, _ := frame(packetTypePingresp, 0, nil)
				broker.send(resp)
			case packetTypeDisconnect:
				record(func() { sawDisconnect = true })
				return
			}
		}
	}()

	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()

	if err := client.Subscribe(ctx, "a/set"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := client.Publish(ctx, Message{Topic: "ha/state", Payload: []byte("online"), QoS: 1, Retain: true}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case msg := <-client.Messages():
		if msg.Topic != "a/set" || string(msg.Payload) != "ON" || msg.QoS != 1 {
			t.Fatalf("delivered %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbound command was not delivered")
	}
	if err := client.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if err := <-runErr; !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Run returned %v, want ErrDisconnected", err)
	}
	if !errors.Is(client.Err(), ErrDisconnected) {
		t.Fatalf("Err() = %v, want ErrDisconnected", client.Err())
	}
	<-brokerDone

	mu.Lock()
	defer mu.Unlock()
	if !acceptedConnect {
		t.Fatal("the broker never saw a CONNECT")
	}
	if len(subscribed) != 1 || subscribed[0] != "a/set" {
		t.Fatalf("subscribed topics %v, want [a/set]", subscribed)
	}
	if len(ackedIDs) != 1 || ackedIDs[0] != 0x2a {
		t.Fatalf("acknowledged packet ids %v, want [42]", ackedIDs)
	}
	if len(published) != 1 {
		t.Fatalf("published %v, want exactly the state message", published)
	}
	if msg := published[0]; msg.Topic != "ha/state" || string(msg.Payload) != "online" ||
		msg.QoS != 1 || !msg.Retain {
		t.Fatalf("unexpected publish %+v", published[0])
	}
	if !sawDisconnect {
		t.Fatal("the session did not send DISCONNECT")
	}
	if stats := client.Stats(); stats.Sent != 1 || stats.Received != 1 {
		t.Fatalf("unexpected stats %+v", stats)
	}
}

// decodePacketForTest re-decodes a packet the test broker read raw.
func decodePacketForTest(raw rawPacket) (*packet, error) {
	return readPacket(bytes.NewReader(raw.encode()))
}

func TestClientConnectRejected(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	go func() {
		readRawPacket(brokerEnd)
		raw, _ := frame(packetTypeConnack, 0, []byte{0, connackBadCredentials})
		brokerEnd.Write(raw)
	}()
	client := newTestClient(clientEnd, Config{})
	err := client.Connect()
	if err == nil {
		t.Fatal("Connect succeeded with a rejected CONNACK")
	}
	if !errors.Is(err, ErrProtocol) || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("Connect error = %v, want a credentials protocol error", err)
	}
}

func TestClientKeepaliveFailure(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		// Stay silent: answer nothing, not even PINGREQ.
		broker.drainUntilEOF()
	}()
	client := newTestClient(clientEnd, Config{KeepAlive: 300 * time.Millisecond})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	err := client.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "silent") {
		t.Fatalf("Run returned %v, want a silent-broker error", err)
	}
	if err := client.Publish(context.Background(), Message{Topic: "a", Payload: []byte("x")}); err == nil {
		t.Fatal("Publish succeeded on a dead session")
	}
}

func TestClientContextCancellation(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		broker.drainUntilEOF()
	}()
	client := newTestClient(clientEnd, Config{})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestClientInflightWindowIsBounded(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	release := make(chan struct{})
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		broker.read(packetTypePublish) // never acknowledged
		<-release
		broker.drainUntilEOF()
	}()
	client := newTestClient(clientEnd, Config{MaxInflight: 1})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go client.Run(ctx)

	first := make(chan error, 1)
	go func() {
		first <- client.Publish(ctx, Message{Topic: "a", Payload: []byte("1"), QoS: 1})
	}()
	deadline := time.Now().Add(time.Second)
	for client.Stats().Pending != 1 {
		if time.Now().After(deadline) {
			t.Fatal("first publish never became pending")
		}
		time.Sleep(5 * time.Millisecond)
	}
	err := client.Publish(ctx, Message{Topic: "a", Payload: []byte("2"), QoS: 1})
	if err == nil || !strings.Contains(err.Error(), "in-flight window") {
		t.Fatalf("second publish error = %v, want a full-window error", err)
	}
	close(release)
	if err := <-first; err == nil {
		t.Fatal("first publish succeeded although the broker never acknowledged it")
	}
}

func TestClientQueueDropsOldestAndCounts(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		broker.drainUntilEOF()
	}()
	client := newTestClient(clientEnd, Config{QueueDepth: 2})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go client.Run(ctx)

	for i := range 3 {
		broker.publish("t", []byte{byte('0' + i)}, 0, false, 0)
	}
	deadline := time.Now().Add(time.Second)
	for client.Stats().Dropped == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no message was dropped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	first := <-client.Messages()
	second := <-client.Messages()
	if string(first.Payload) != "1" || string(second.Payload) != "2" {
		t.Fatalf("delivered %q then %q, want the two newest payloads", first.Payload, second.Payload)
	}
	if stats := client.Stats(); stats.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", stats.Dropped)
	}
}

func TestClientProtocolErrorEndsSession(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		broker.send([]byte{0x00, 0x00}) // packet type 0 does not exist
	}()
	client := newTestClient(clientEnd, Config{})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	err := client.Run(context.Background())
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Run returned %v, want ErrProtocol", err)
	}
	if err := client.Publish(context.Background(), Message{Topic: "a"}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Publish after the protocol error returned %v, want the session error", err)
	}
}

func TestClientPingRoundTrips(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	sawPing := make(chan struct{}, 1)
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		pkt, err := readRawPacket(brokerEnd)
		if err != nil || pkt.packetType != packetTypePingreq {
			return
		}
		resp, _ := frame(packetTypePingresp, 0, nil)
		broker.send(resp)
		sawPing <- struct{}{}
		broker.drainUntilEOF()
	}()
	client := newTestClient(clientEnd, Config{})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	select {
	case <-sawPing:
	case <-time.After(time.Second):
		t.Fatal("broker did not receive PINGREQ")
	}
}

func TestClientBrokerClosedConnection(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		brokerEnd.Close()
	}()
	client := newTestClient(clientEnd, Config{})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded after the broker closed the stream")
	}
	if err := client.Disconnect(); err == nil {
		t.Fatal("Disconnect after a failed session returned nil")
	}
}

// silentConn accepts writes and never answers a read. It honours SetDeadline,
// which is how a real stream adapter bounds CONNECT/CONNACK.
type silentConn struct {
	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	closed        bool
}

func (c *silentConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *silentConn) Read([]byte) (int, error) {
	c.mu.Lock()
	deadline := c.readDeadline
	c.mu.Unlock()
	if deadline.IsZero() {
		select {} // no deadline and no answer: wait forever
	}
	if wait := time.Until(deadline); wait > 0 {
		time.Sleep(wait)
	}
	return 0, os.ErrDeadlineExceeded
}

func (c *silentConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *silentConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *silentConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

func TestClientBoundsTheConnectExchange(t *testing.T) {
	client := NewClient(&silentConn{}, Config{ClientID: "c", ConnectTimeout: 100 * time.Millisecond})
	started := time.Now()
	err := client.Connect()
	if err == nil {
		t.Fatal("Connect succeeded against a broker that never answers")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Connect took %s, want it bounded by the connect timeout", elapsed)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Connect error = %v, want a deadline error", err)
	}
}

func TestClientClearsTheConnectDeadline(t *testing.T) {
	// After CONNACK the session must run without an operation deadline:
	// otherwise a quiet broker would be treated as a dead one.
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		broker.drainUntilEOF()
	}()
	client := NewClient(clientEnd, Config{ClientID: "c", KeepAlive: time.Second, ConnectTimeout: 50 * time.Millisecond})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// A publish long after the connect deadline still succeeds, and a read of an
	// idle connection is not failed by a stale deadline.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	time.Sleep(100 * time.Millisecond)
	if err := client.Publish(ctx, Message{Topic: "a", Payload: []byte("x")}); err != nil {
		t.Fatalf("Publish after the connect deadline: %v", err)
	}
}

func TestClientConnectRejectsNonConnack(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		raw, _ := frame(packetTypeSuback, 0, []byte{0, 1, 0})
		broker.send(raw)
	}()
	client := newTestClient(clientEnd, Config{})
	if err := client.Connect(); err == nil {
		t.Fatal("Connect accepted a SUBACK as CONNACK")
	}
}

func TestClientSubscribeRefused(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		sub := broker.read(packetTypeSubscribe)
		id := packetIDOf(sub)
		raw, _ := frame(packetTypeSuback, 0, []byte{byte(id >> 8), byte(id), subackFailure})
		broker.send(raw)
		broker.drainUntilEOF()
	}()
	client := newTestClient(clientEnd, Config{})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go client.Run(ctx)
	if err := client.Subscribe(ctx, "a/set"); err == nil {
		t.Fatal("Subscribe succeeded although the broker returned 0x80")
	}
}

func TestClientSubscribeRejectsEmptyFilters(t *testing.T) {
	client := newTestClient(lonePipeEnd(), Config{})
	if err := client.Subscribe(context.Background()); err == nil {
		t.Fatal("Subscribe with no filters succeeded")
	}
	if err := client.Publish(context.Background(), Message{Topic: ""}); err == nil {
		t.Fatal("Publish with an empty topic succeeded")
	}
}

func TestEncodeConnectKeepsEmptyPassword(t *testing.T) {
	raw, err := encodeConnect(connectOptions{clientID: "c", hasPassword: true})
	if err != nil {
		t.Fatalf("encodeConnect: %v", err)
	}
	opts, err := decodeConnectForTest(raw)
	if err != nil {
		t.Fatalf("decodeConnectForTest: %v", err)
	}
	if !opts.hasPassword || opts.password != "" {
		t.Fatalf("empty password not preserved: %+v", opts)
	}
}

func TestStatsPendingTracksWaiters(t *testing.T) {
	client := newTestClient(lonePipeEnd(), Config{MaxInflight: 4})
	if got := client.Stats().Pending; got != 0 {
		t.Fatalf("Pending = %d, want 0", got)
	}
	id, _, err := client.reserve(client.subWait)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if id == 0 {
		t.Fatal("reserve returned packet id 0")
	}
	client.release(client.subWait, id)
	if _, _, err := client.reserve(client.ackWait); err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if got := client.Stats().Pending; got != 1 {
		t.Fatalf("Pending = %d, want 1", got)
	}
}

func TestPacketNameCoverage(t *testing.T) {
	for _, tc := range []struct {
		value byte
		want  string
	}{
		{packetTypeConnect, "CONNECT"},
		{packetTypeConnack, "CONNACK"},
		{packetTypePublish, "PUBLISH"},
		{packetTypePuback, "PUBACK"},
		{packetTypeSubscribe, "SUBSCRIBE"},
		{packetTypeSuback, "SUBACK"},
		{packetTypeUnsubscribe, "UNSUBSCRIBE"},
		{packetTypeUnsuback, "UNSUBACK"},
		{packetTypePingreq, "PINGREQ"},
		{packetTypePingresp, "PINGRESP"},
		{packetTypeDisconnect, "DISCONNECT"},
	} {
		if got := packetName(tc.value); got != tc.want {
			t.Fatalf("packetName(%d) = %q, want %q", tc.value, got, tc.want)
		}
	}
	// Packet type 0 is reserved and 6 (PUBREL) is not handled by this client.
	for _, unknown := range []byte{0, 6} {
		if got := packetName(unknown); !strings.HasPrefix(got, "packet(") {
			t.Fatalf("packetName(%d) = %q", unknown, got)
		}
	}
}

// deadlineRecordingConn wraps a Conn and records the deadlines the client
// installs, so a test can assert which direction an operation bounded.
type deadlineRecordingConn struct {
	Conn
	mu     sync.Mutex
	reads  []time.Time
	writes []time.Time
}

func (c *deadlineRecordingConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.reads = append(c.reads, deadline)
	c.mu.Unlock()
	return nil
}

func (c *deadlineRecordingConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writes = append(c.writes, deadline)
	c.mu.Unlock()
	return nil
}

func (c *deadlineRecordingConn) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reads), len(c.writes)
}

func (c *deadlineRecordingConn) lastWrite() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.writes) == 0 {
		return time.Time{}
	}
	return c.writes[len(c.writes)-1]
}

func TestPublishBoundsOnlyTheWrite(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	recorder := &deadlineRecordingConn{Conn: clientEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		pub := broker.read(packetTypePublish)
		ack, _ := encodePacketID(packetTypePuback, packetIDOf(pub))
		broker.send(ack)
		broker.drainUntilEOF()
	}()

	client := NewClient(recorder, Config{ClientID: "c", KeepAlive: time.Second, ConnectTimeout: time.Second})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Acknowledgements are consumed by the session reader, so it must run.
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go client.Run(runCtx) //nolint:errcheck // the assertion is on the deadlines
	// CONNECT bounds both directions and clears both afterwards.
	reads, writes := recorder.counts()
	if reads < 2 || writes < 2 {
		t.Fatalf("CONNECT set %d read and %d write deadlines, want a bound and a clear in each direction", reads, writes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	readsBefore, _ := recorder.counts()
	if err := client.Publish(ctx, Message{Topic: "a", Payload: []byte("x"), QoS: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	readsAfter, writesAfter := recorder.counts()
	if readsAfter != readsBefore {
		t.Fatal("a publish installed a read deadline, which would abort a blocked reader")
	}
	if writesAfter != writes+2 {
		t.Fatalf("publish installed %d write deadlines, want the bound and its clear", writesAfter-writes)
	}
	if last := recorder.lastWrite(); !last.IsZero() {
		t.Fatalf("the write deadline was not cleared after the publish: %s", last)
	}
}

func TestSubscribeBoundsOnlyTheWrite(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	recorder := &deadlineRecordingConn{Conn: clientEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		sub := broker.read(packetTypeSubscribe)
		id := packetIDOf(sub)
		ack, _ := frame(packetTypeSuback, 0, []byte{byte(id >> 8), byte(id), 0})
		broker.send(ack)
		broker.drainUntilEOF()
	}()

	client := NewClient(recorder, Config{ClientID: "c", KeepAlive: time.Second})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go client.Run(runCtx) //nolint:errcheck // the assertion is on the deadlines
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	readsBefore, writesBefore := recorder.counts()
	if err := client.Subscribe(ctx, "a/set"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	readsAfter, writesAfter := recorder.counts()
	if readsAfter != readsBefore {
		t.Fatal("a subscribe installed a read deadline")
	}
	if writesAfter <= writesBefore {
		t.Fatal("a subscribe with a context deadline installed no write deadline")
	}
	if last := recorder.lastWrite(); !last.IsZero() {
		t.Fatalf("the write deadline was not cleared after the subscribe: %s", last)
	}
}

// wedgingConn blocks every write until Close, which is what a peer that stops
// draining its receive window looks like from this side.
type wedgingConn struct {
	release chan struct{}
	once    sync.Once
}

func (c *wedgingConn) Read([]byte) (int, error) {
	<-c.release
	return 0, io.EOF
}

func (c *wedgingConn) Write([]byte) (int, error) {
	<-c.release
	return 0, io.ErrClosedPipe
}

func (c *wedgingConn) Close() error {
	c.once.Do(func() { close(c.release) })
	return nil
}

func TestWedgedWriteDoesNotBlockOtherWriters(t *testing.T) {
	// One writer is wedged mid-write and therefore holds the write path. A
	// second writer must fail within its own budget instead of queueing behind
	// it — that is what keeps the teardown writes (offline marker, DISCONNECT)
	// from hanging on a frozen broker.
	conn := &wedgingConn{release: make(chan struct{})}
	client := NewClient(conn, Config{ClientID: "c", KeepAlive: time.Second})

	wedged := make(chan struct{})
	go func() {
		defer close(wedged)
		_ = client.Publish(context.Background(), Message{Topic: "a", Payload: []byte("x")})
	}()
	// Wait until the wedged writer holds the write path.
	deadline := time.Now().Add(time.Second)
	for len(client.writeSem) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first writer never entered the write path")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.Publish(ctx, Message{Topic: "b", Payload: []byte("y")})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a publish succeeded while the write path was wedged")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second publish error = %v, want its context deadline", err)
	}
	if elapsed > time.Second {
		t.Fatalf("the second publish waited %s, want it bounded by its context", elapsed)
	}

	_ = conn.Close()
	select {
	case <-wedged:
	case <-time.After(2 * time.Second):
		t.Fatal("the wedged writer did not finish after the connection closed")
	}
}

func TestDisconnectDoesNotQueueBehindAWedgedWriter(t *testing.T) {
	// The write path is held by a writer that cannot finish because the peer
	// stopped draining. Disconnect must give up on the bounded budget and close
	// the connection instead of waiting for the path forever.
	conn := &wedgingConn{release: make(chan struct{})}
	client := NewClient(conn, Config{ClientID: "c", KeepAlive: time.Second})

	wedged := make(chan struct{})
	go func() {
		defer close(wedged)
		_ = client.Publish(context.Background(), Message{Topic: "a", Payload: []byte("x")})
	}()
	deadline := time.Now().Add(time.Second)
	for len(client.writeSem) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the wedged writer never entered the write path")
		}
		time.Sleep(time.Millisecond)
	}

	started := time.Now()
	err := client.Disconnect()
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Disconnect reported success while the write path was wedged")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Disconnect waited %s, want it bounded by the disconnect budget", elapsed)
	}
	_ = conn.Close()
	select {
	case <-wedged:
	case <-time.After(2 * time.Second):
		t.Fatal("the wedged writer did not finish after the connection closed")
	}
}

func TestDisconnectBoundsItsWrite(t *testing.T) {
	clientEnd, brokerEnd := newPipe()
	broker := &testBroker{t: t, conn: brokerEnd}
	recorder := &deadlineRecordingConn{Conn: clientEnd}
	go func() {
		broker.read(packetTypeConnect)
		broker.connack(connackAccepted)
		broker.read(packetTypeDisconnect)
	}()

	client := NewClient(recorder, Config{ClientID: "c", KeepAlive: time.Second})
	if err := client.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, writesBefore := recorder.counts()
	if err := client.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	_, writesAfter := recorder.counts()
	if writesAfter <= writesBefore {
		t.Fatal("Disconnect installed no write deadline")
	}
	if last := recorder.lastWrite(); !last.IsZero() {
		t.Fatalf("the write deadline was not cleared after Disconnect: %s", last)
	}
}
