package webui

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	webv1 "capsulad/gen/go/capsula/web/v1"
)

// The browser transport is one binary WebSocket carrying capsula.web.v1
// frames. It is intentionally small and dependency-free: one authenticated
// session per connection, bounded frames, one writer goroutine, a bounded
// outbound queue, and an explicit resync signal instead of unbounded memory.
//
// Lifecycle: the HTTP layer has already validated the session cookie. The
// socket re-validates it on open, on every client frame, and on a monitor
// ticker. Logout/password-change close registered sockets immediately; an
// expired or revoked session closes with policy violation so the SPA returns
// to the login view.

const (
	maxClientFrameBytes = 64 << 10
	maxServerFrameBytes = 1 << 20
	outboundQueueDepth  = 64
	maxConcurrentOps    = 4
	maxSubscriptions    = 8

	closeNormal          = 1000
	closeGoingAway       = 1001
	closeProtocolError   = 1002
	closeUnsupportedData = 1003
	closeTooLarge        = 1009
	closePolicyViolation = 1008
	closeInternal        = 1011
)

var errClosed = errors.New("websocket closed")

// controlSendTimeout bounds how long a response the browser cannot reconstruct
// (result, error, final progress, snapshot) waits for outbound queue space. A
// data event may be dropped with an explicit resync; these frames may not,
// because a dropped CreateApiKey result would lose the only copy of the new
// secret. When the bound expires the socket closes so the failure is explicit.
//
// It is a variable so focused tests can shorten the wait.
var controlSendTimeout = 2 * time.Second

// closeFrameTimeout bounds the best-effort close frame. Safety cancellation
// happens first, so a stalled client can delay this frame at most this long.
const closeFrameTimeout = 250 * time.Millisecond

// closeFrameLockWait lets a healthy writer finish its current frame so the
// browser still receives the specific close code (session expiry drives the
// refresh path). It is a bounded wait, not a reason to delay cancellation.
const closeFrameLockWait = 100 * time.Millisecond

type outFrame struct {
	opcode  byte
	payload []byte
}

type wsConn struct {
	srv   *Server
	conn  net.Conn
	br    *bufio.Reader
	token string

	out    chan outFrame
	resync chan struct{}
	done   chan struct{}
	once   sync.Once

	writeMu sync.Mutex
	rev     atomic.Uint64
	ops     chan struct{}

	subMu sync.Mutex
	subs  map[string]*subscription

	trainingMu sync.Mutex
	training   map[string]context.CancelFunc

	commandMu sync.Mutex
	commands  map[string]context.CancelFunc
}

type subscription struct {
	cancel context.CancelFunc
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	auth := authFrom(r.Context())
	if !auth.ViaSession {
		writeUnauthorized(w, r)
		return
	}
	if !websocketUpgrade(r) {
		s.Logs.Warnf("webui", "", "websocket upgrade rejected from %s: proto=%s connection=%q upgrade=%q version=%q",
			remoteHost(r), r.Proto, r.Header.Get("Connection"), r.Header.Get("Upgrade"), r.Header.Get("Sec-WebSocket-Version"))
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	if !sameOriginUpgrade(r) {
		s.Logs.Warnf("webui", "", "websocket upgrade rejected from %s: origin=%q host=%q",
			remoteHost(r), r.Header.Get("Origin"), r.Host)
		http.Error(w, "cross-origin websocket rejected", http.StatusForbidden)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 16 {
		s.Logs.Warnf("webui", "", "websocket upgrade rejected from %s: invalid Sec-WebSocket-Key", remoteHost(r))
		http.Error(w, "invalid websocket key", http.StatusBadRequest)
		return
	}
	if !s.Auth.ValidateSession(auth.Token) {
		writeUnauthorized(w, r)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if _, err := fmt.Fprintf(rw,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
		_ = conn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return
	}

	c := &wsConn{
		srv: s, conn: conn, br: rw.Reader, token: auth.Token,
		out:      make(chan outFrame, outboundQueueDepth),
		resync:   make(chan struct{}, 1),
		done:     make(chan struct{}),
		ops:      make(chan struct{}, maxConcurrentOps),
		subs:     map[string]*subscription{},
		training: map[string]context.CancelFunc{},
		commands: map[string]context.CancelFunc{},
	}
	s.registerConn(auth.Token, c)
	s.Logs.Infof("webui", "", "realtime session opened from %s", remoteHost(r))

	go c.writer()
	go c.monitor()
	go c.readLoop()
}

func websocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		headerContains(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") &&
		strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) == "13"
}

func headerContains(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// sameOriginUpgrade requires a concrete HTTPS origin that matches the Host the
// browser connected to. A missing Origin is not a browser and cannot carry the
// session cookie under SameSite=Strict, but it is still rejected so the
// boundary is explicit and testable.
func sameOriginUpgrade(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (c *wsConn) closeWith(code uint16, reason string) {
	c.once.Do(func() {
		// Release device/training ownership before any writer interaction. A
		// slow client can hold writeMu for the whole write deadline; command
		// and training contexts must not wait for its close frame.
		c.cancelSessionWork()
		// Closing done under subMu makes subscribe's check-and-insert
		// atomic with teardown: a watcher can never be registered after the
		// socket is closed.
		c.subMu.Lock()
		close(c.done)
		for k, sub := range c.subs {
			sub.cancel()
			delete(c.subs, k)
		}
		c.subMu.Unlock()

		payload := make([]byte, 2+len(reason))
		binary.BigEndian.PutUint16(payload, code)
		copy(payload[2:], reason)
		// The close frame is best effort. A writer stuck inside a large frame
		// only delays it for a bounded wait; after that the browser observes a
		// transport close (generic 1006). A healthy writer still delivers
		// session-expiry/policy codes for the client's refresh path.
		locked := false
		deadline := time.Now().Add(closeFrameLockWait)
		for {
			if c.writeMu.TryLock() {
				locked = true
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if locked {
			if c.conn != nil {
				_ = c.writeFrameWithDeadline(opClose, payload, closeFrameTimeout)
			}
			c.writeMu.Unlock()
		}
		if c.conn != nil {
			_ = c.conn.Close()
		}
		if c.srv != nil {
			c.srv.unregisterConn(c.token, c)
		}
	})
}

// cancelSessionWork is the socket-owned lifecycle boundary: no command context
// (including a device recording) or training stream may outlive its session.
func (c *wsConn) cancelSessionWork() {
	c.commandMu.Lock()
	for id, cancel := range c.commands {
		cancel()
		delete(c.commands, id)
	}
	c.commandMu.Unlock()
	c.trainingMu.Lock()
	for id, cancel := range c.training {
		cancel()
		delete(c.training, id)
	}
	c.trainingMu.Unlock()
}

// --- frame I/O -------------------------------------------------------------

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xa
)

func (c *wsConn) writer() {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case f := <-c.out:
			c.writeMu.Lock()
			err := c.writeFrameLocked(f.opcode, f.payload)
			c.writeMu.Unlock()
			if err != nil {
				c.closeWith(closeGoingAway, "write failed")
				return
			}
		case <-c.resync:
			// The outbound queue overflowed. Deliver one topic-less resync so
			// every subscription re-requests its snapshot. If even that slot is
			// unavailable, close so the client reconnects and re-subscribes
			// rather than acting on a stream with a hole.
			if !c.enqueueResync(&webv1.ServerFrame{Body: &webv1.ServerFrame_Event{
				Event: &webv1.Event{ResyncRequired: true},
			}}) {
				c.closeWith(closeGoingAway, "client is not draining events")
				return
			}
		case <-ping.C:
			c.writeMu.Lock()
			err := c.writeFrameLocked(opPing, nil)
			c.writeMu.Unlock()
			if err != nil {
				c.closeWith(closeGoingAway, "ping failed")
				return
			}
		}
	}
}

// enqueueResync reports whether a resync frame fit in the outbound queue. Its
// caller closes the connection when it did not, so convergence is never silent.
func (c *wsConn) enqueueResync(msg *webv1.ServerFrame) bool {
	b, err := proto.Marshal(msg)
	if err != nil || len(b) > maxServerFrameBytes {
		return false
	}
	select {
	case c.out <- outFrame{opcode: opBinary, payload: b}:
		return true
	case <-c.done:
		return false
	default:
		return false
	}
}

// send enqueues a data frame without blocking. A full queue marks the
// connection for resynchronization: the client must re-request its snapshots
// rather than acting on an event stream with a hole.
func (c *wsConn) send(msg *webv1.ServerFrame) {
	b, err := proto.Marshal(msg)
	if err != nil {
		c.sendError(msg.GetRequestId(), webv1.ErrorCode_ERROR_CODE_INTERNAL, "could not encode response")
		return
	}
	if len(b) > maxServerFrameBytes {
		c.sendError(msg.GetRequestId(), webv1.ErrorCode_ERROR_CODE_TOO_LARGE, "response exceeds the browser frame budget")
		return
	}
	select {
	case c.out <- outFrame{opcode: opBinary, payload: b}:
	case <-c.done:
	default:
		select {
		case c.resync <- struct{}{}:
		default:
		}
	}
}

// sendControl enqueues a frame the browser cannot reconstruct from a resync:
// command results (including the one-time API key secret), errors, training
// progress and snapshots. It waits a bounded time for queue space and closes
// the socket when the client is not draining, so a response is never dropped
// silently.
func (c *wsConn) sendControl(msg *webv1.ServerFrame) bool {
	b, err := proto.Marshal(msg)
	if err != nil || len(b) > maxServerFrameBytes {
		return false
	}
	frame := outFrame{opcode: opBinary, payload: b}
	select {
	case c.out <- frame:
		return true
	case <-c.done:
		return false
	default:
	}
	timer := time.NewTimer(controlSendTimeout)
	defer timer.Stop()
	select {
	case c.out <- frame:
		return true
	case <-c.done:
		return false
	case <-timer.C:
		c.closeWith(closeGoingAway, "client is not draining responses")
		return false
	}
}

func (c *wsConn) sendError(reqID uint64, code webv1.ErrorCode, message string) {
	// Error frames are small and always encodable; the bounded control path
	// reports queue exhaustion by closing instead of dropping the error.
	if !c.sendControl(&webv1.ServerFrame{RequestId: reqID, Body: &webv1.ServerFrame_Error{
		Error: &webv1.Error{Code: code, Message: message},
	}}) {
		// A close may already be underway; make the failure explicit if not.
		select {
		case <-c.done:
		default:
			c.closeWith(closeInternal, "could not encode error response")
		}
	}
}

func (c *wsConn) sendProgress(reqID uint64, p *webv1.Progress) {
	c.sendControl(&webv1.ServerFrame{RequestId: reqID, Body: &webv1.ServerFrame_Progress{Progress: p}})
}

func (c *wsConn) sendResult(reqID uint64, result *webv1.Result) {
	if !c.sendControl(&webv1.ServerFrame{RequestId: reqID, Body: &webv1.ServerFrame_Result{Result: result}}) {
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_INTERNAL, "could not encode response")
	}
}

func (c *wsConn) nextRevision() uint64 { return c.rev.Add(1) }

func (c *wsConn) writeFrameLocked(opcode byte, payload []byte) error {
	return c.writeFrameWithDeadline(opcode, payload, 10*time.Second)
}

func (c *wsConn) writeFrameWithDeadline(opcode byte, payload []byte, timeout time.Duration) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xffff:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		header = append(header, buf[:]...)
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

func (c *wsConn) readLoop() {
	defer c.closeWith(closeNormal, "bye")
	_ = c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			if errors.Is(err, errClosed) {
				return
			}
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		switch opcode {
		case opClose:
			return
		case opPing:
			c.writeMu.Lock()
			_ = c.writeFrameLocked(opPong, payload)
			c.writeMu.Unlock()
		case opPong:
		case opText:
			c.closeWith(closeUnsupportedData, "text frames are not accepted")
			return
		case opBinary:
			if !c.srv.Auth.ValidateSession(c.token) {
				c.closeWith(closePolicyViolation, "session expired")
				return
			}
			var frame webv1.ClientFrame
			if err := proto.Unmarshal(payload, &frame); err != nil {
				c.sendError(0, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "malformed protobuf frame")
				continue
			}
			c.handleFrame(&frame)
		default:
			c.closeWith(closeUnsupportedData, "unsupported frame")
			return
		}
	}
}

func (c *wsConn) readFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, errClosed
	}
	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0f
	masked := head[1]&0x80 != 0
	if !fin {
		c.closeWith(closeUnsupportedData, "fragmented frames are not accepted")
		return 0, nil, errClosed
	}
	if !masked {
		c.closeWith(closeProtocolError, "client frames must be masked")
		return 0, nil, errClosed
	}
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, errClosed
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, errClosed
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if opcode >= opClose {
		if length > 125 {
			c.closeWith(closeProtocolError, "oversized control frame")
			return 0, nil, errClosed
		}
	} else if length > maxClientFrameBytes {
		c.closeWith(closeTooLarge, "frame exceeds the browser budget")
		return 0, nil, errClosed
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return 0, nil, errClosed
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, errClosed
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, payload, nil
}

func (c *wsConn) monitor() {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-tick.C:
			if !c.srv.Auth.ValidateSession(c.token) {
				c.closeWith(closePolicyViolation, "session expired")
				return
			}
		}
	}
}

// --- frame dispatch --------------------------------------------------------

func (c *wsConn) handleFrame(frame *webv1.ClientFrame) {
	reqID := frame.GetRequestId()
	switch body := frame.GetBody().(type) {
	case *webv1.ClientFrame_Hello:
		c.sendControl(&webv1.ServerFrame{RequestId: reqID, Body: &webv1.ServerFrame_Hello{
			Hello: &webv1.HelloResult{
				DaemonVersion: c.srv.daemonVersion(), Platform: c.srv.Platform, User: "admin",
				MaxClientFrameBytes: maxClientFrameBytes,
			},
		}})
	case *webv1.ClientFrame_Ping:
		c.send(&webv1.ServerFrame{RequestId: reqID, Body: &webv1.ServerFrame_Pong{Pong: &webv1.Pong{}}})
	case *webv1.ClientFrame_SnapshotRequest:
		if reqID == 0 {
			c.sendError(0, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "snapshot requests need a request id")
			return
		}
		topic, appID := body.SnapshotRequest.GetTopic(), body.SnapshotRequest.GetAppId()
		c.runOp(reqID, func() { c.sendSnapshot(reqID, topic, appID) })
	case *webv1.ClientFrame_Subscribe:
		topic, appID := body.Subscribe.GetTopic(), body.Subscribe.GetAppId()
		c.runOp(reqID, func() { c.subscribe(reqID, topic, appID) })
	case *webv1.ClientFrame_Unsubscribe:
		c.unsubscribe(body.Unsubscribe.GetTopic(), body.Unsubscribe.GetAppId())
	case *webv1.ClientFrame_Cancel:
		c.cancelOperation(body.Cancel.GetOperationId())
	case *webv1.ClientFrame_Command:
		if reqID == 0 {
			c.sendError(0, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "commands need a request id")
			return
		}
		c.runCommand(reqID, body.Command)
	default:
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "empty or unknown frame")
	}
}

func (c *wsConn) runOp(reqID uint64, fn func()) {
	select {
	case c.ops <- struct{}{}:
		go func() {
			defer func() { <-c.ops }()
			fn()
		}()
	case <-c.done:
	default:
		// The request id is the only way the browser can settle the exact
		// snapshot/subscription promise; an uncorrelated error would leave it
		// waiting for the transport timeout.
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "too many concurrent operations")
	}
}

// requestOperationID is the cancel handle for one command frame. The browser
// aborts a request by sending CancelOperation with exactly this id, so a modal
// dismissal can end a device recording instead of only rejecting a promise.
func requestOperationID(reqID uint64) string {
	return "request:" + strconv.FormatUint(reqID, 10)
}

// runCommand admits one command and registers its cancellable context before
// the asynchronous dispatch, so an immediate abort cannot race registration.
func (c *wsConn) runCommand(reqID uint64, cmd *webv1.Command) {
	select {
	case c.ops <- struct{}{}:
	case <-c.done:
		return
	default:
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "too many concurrent operations")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	key := requestOperationID(reqID)
	c.commandMu.Lock()
	c.commands[key] = cancel
	c.commandMu.Unlock()
	go func() {
		defer func() { <-c.ops }()
		defer func() {
			c.commandMu.Lock()
			delete(c.commands, key)
			c.commandMu.Unlock()
			cancel()
		}()
		if ctx.Err() != nil {
			// The session or this request was cancelled before dispatch;
			// do not start a device mutation for a dead owner.
			return
		}
		c.srv.dispatch(c, reqID, cmd, ctx)
	}()
}

// sendSnapshot reserves the envelope revision before reading the view, so an
// event produced while the view is being built is always labeled newer than
// this snapshot even if it is delivered first. The returned boundary is the
// set of log records already present in the snapshot; a logs watcher uses it
// to skip its upstream backlog instead of replaying entries the browser has.
func (c *wsConn) sendSnapshot(reqID uint64, topic webv1.Topic, appID string) logSnapshotBoundary {
	rev := c.nextRevision()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snap, err := c.srv.snapshot(ctx, topic, appID)
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return logSnapshotBoundary{}
	}
	snap.Revision = rev
	frame := &webv1.ServerFrame{RequestId: reqID, Body: &webv1.ServerFrame_Snapshot{Snapshot: snap}}
	if !c.sendControl(frame) {
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_INTERNAL, "could not encode snapshot")
		return logSnapshotBoundary{}
	}
	return snapshotLogBoundary(snap)
}

// logIdentity is the daemon's monotonic record identity as exposed to the
// browser. Two records can share a coarse timestamp, so equality at the
// boundary is decided by the whole record rather than by time alone.
type logIdentity struct {
	timeNS    uint64
	component string
	appID     string
	severity  string
	message   string
}

func logIdentityOf(entry *webv1.LogEntry) logIdentity {
	return logIdentity{
		timeNS: entry.GetMonotonicTimeNs(), component: entry.GetComponent(), appID: entry.GetAppId(),
		severity: entry.GetSeverity(), message: entry.GetMessage(),
	}
}

// logSnapshotBoundary marks log records the initial snapshot already carries.
// Anything older than the newest snapshot record is part of the replayed
// backlog; records newer than it are live.
type logSnapshotBoundary struct {
	through uint64
	seen    map[logIdentity]struct{}
}

func (b logSnapshotBoundary) includes(entry *webv1.LogEntry) bool {
	if b.through == 0 {
		return false
	}
	switch ts := entry.GetMonotonicTimeNs(); {
	case ts < b.through:
		return true
	case ts > b.through:
		return false
	default:
		_, ok := b.seen[logIdentityOf(entry)]
		return ok
	}
}

// snapshotLogBoundary returns the newest log timestamp carried by a snapshot
// and the identities at that timestamp.
func snapshotLogBoundary(snap *webv1.Snapshot) logSnapshotBoundary {
	var entries []*webv1.LogEntry
	switch {
	case snap.GetDiagnostics() != nil:
		entries = snap.GetDiagnostics().GetLogs()
	case snap.GetApplication() != nil:
		entries = snap.GetApplication().GetLogs()
	}
	var boundary logSnapshotBoundary
	for _, entry := range entries {
		if ts := entry.GetMonotonicTimeNs(); ts > boundary.through {
			boundary.through = ts
		}
	}
	if boundary.through == 0 {
		return boundary
	}
	boundary.seen = make(map[logIdentity]struct{}, len(entries))
	for _, entry := range entries {
		if entry.GetMonotonicTimeNs() == boundary.through {
			boundary.seen[logIdentityOf(entry)] = struct{}{}
		}
	}
	return boundary
}

func (c *wsConn) sendEvent(ev *webv1.Event) {
	ev.Revision = c.nextRevision()
	c.send(&webv1.ServerFrame{Body: &webv1.ServerFrame_Event{Event: ev}})
}

func subscriptionKey(topic webv1.Topic, appID string) string {
	return fmt.Sprintf("%d:%s", topic, appID)
}

func (c *wsConn) subscribe(reqID uint64, topic webv1.Topic, appID string) {
	if topic == webv1.Topic_TOPIC_UNSPECIFIED {
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "a subscription needs a topic")
		return
	}
	if topic == webv1.Topic_TOPIC_APPLICATION && appID == "" {
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "application subscriptions need an app id")
		return
	}
	key := subscriptionKey(topic, appID)
	ctx, cancel := context.WithCancel(context.Background())
	c.subMu.Lock()
	select {
	case <-c.done:
		c.subMu.Unlock()
		cancel()
		return
	default:
	}
	if _, exists := c.subs[key]; exists {
		c.subMu.Unlock()
		cancel()
		c.sendSnapshot(reqID, topic, appID)
		return
	}
	if len(c.subs) >= maxSubscriptions {
		c.subMu.Unlock()
		cancel()
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "subscription limit reached")
		return
	}
	c.subs[key] = &subscription{cancel: cancel}
	c.subMu.Unlock()

	// The invalidation subscription is installed synchronously above so a
	// mutation racing the snapshot is queued. The snapshot is enqueued before
	// the watcher goroutine starts: events it produces are then necessarily
	// behind the snapshot in the single outbound queue. The watcher starts even
	// if the snapshot failed, so later changes can still resynchronize.
	signals, stopSignals := c.srv.topicSignals(topic)
	logBoundary := c.sendSnapshot(reqID, topic, appID)
	go func() {
		if stopSignals != nil {
			defer stopSignals()
		}
		c.srv.watchTopicWith(ctx, c, topic, appID, signals, logBoundary)
	}()
}

func (c *wsConn) unsubscribe(topic webv1.Topic, appID string) {
	key := subscriptionKey(topic, appID)
	c.subMu.Lock()
	sub := c.subs[key]
	delete(c.subs, key)
	c.subMu.Unlock()
	if sub != nil {
		sub.cancel()
	}
}

func (c *wsConn) cancelOperation(id string) {
	if id == "" {
		return
	}
	c.commandMu.Lock()
	command := c.commands[id]
	delete(c.commands, id)
	c.commandMu.Unlock()
	if command != nil {
		command()
		return
	}
	c.trainingMu.Lock()
	sub := c.training[id]
	delete(c.training, id)
	c.trainingMu.Unlock()
	if sub != nil {
		sub()
	}
}
