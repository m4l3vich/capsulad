// MCP frontend.
//
// capsulad serves a second frontend beside the browser SPA: a Model Context
// Protocol server on the same HTTPS listener at /mcp. It is a daemon surface,
// not an application: it has no app identity, no manifest and no package.
//
// It is deliberately not a second control plane. Every MCP tool drives the same
// allowlisted dispatch (and the same snapshot builders) the browser WebSocket
// frontend uses, through the operationSink boundary in ops.go. An MCP client
// presents a static API key; the key's scopes decide which tools it may see and
// call, so a key minted for status polling never becomes an administration
// credential merely by reaching the MCP endpoint.
//
// The transport is the MCP Streamable HTTP binding: one JSON-RPC 2.0 endpoint,
// POST for requests/notifications, DELETE to end a session. There is no
// server-initiated stream, so GET is refused with 405 exactly as the binding
// allows. Notifications are acknowledged with an empty 202.
package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	webv1 "capsulad/gen/go/capsulad/web/v1"
	"capsulad/internal/artifact"
	"capsulad/internal/assets"
	"capsulad/internal/uichanges"
	"capsulad/internal/version"
	"capsulad/internal/websecurity"
)

const (
	// mcpProtocolVersion is the MCP revision this server implements.
	mcpProtocolVersion = "2025-06-18"
	// mcpSessionHeader is the Streamable HTTP session header.
	mcpSessionHeader = "Mcp-Session-Id"
	// mcpVersionHeader is the revision a client repeats on every request after
	// initialization. An invalid or unsupported value is a 400.
	mcpVersionHeader = "MCP-Protocol-Version"

	// maxMCPBodyBytes bounds one request body. A chunked upload streams into
	// the existing sink, so this bounds only one chunk (or a small one-call
	// upload); the operation's own limit is enforced separately.
	maxMCPBodyBytes = 16 << 20
	// mcpMaxChunkBytes is the decoded size of one upload_chunk body: small
	// enough that a chunk is allocation-cheap on a 240 MiB board, large enough
	// that a 32 MiB asset is about thirty calls.
	mcpMaxChunkBytes = 1 << 20
	// mcpMaxChunkBase64Bytes is the encoded size of one maximum chunk, checked
	// before decoding so an oversized body cannot allocate a larger slice first.
	mcpMaxChunkBase64Bytes = (mcpMaxChunkBytes/3 + 1) * 4
	// mcpUploadDrainWait bounds how long completion waits for a chunk already
	// inside the pipe before the upload is declared stalled.
	mcpUploadDrainWait = 30 * time.Second
	// mcpMaxWaitSeconds bounds a train_wakeword wait so it stays inside the
	// response write budget.
	mcpMaxWaitSeconds = 300
	// mcpMaxUploadsPerSession bounds concurrent staged uploads so a client
	// cannot pin a pipe goroutine per call.
	mcpMaxUploadsPerSession = 2
	// mcpUploadLifetime bounds one staged upload. A client that begins an
	// upload and stops sending cannot hold the pipe forever.
	mcpUploadLifetime = 15 * time.Minute

	maxMCPSessions = 64
	// mcpWriteBudget bounds one MCP response write. It is generous because a
	// tool may drive a device operation that legitimately runs for a minute.
	mcpWriteBudget = 5 * time.Minute
	// mcpReadBudget raises the listener's 30 s read timeout for one request, so
	// a slow client can deliver a one-call upload body and get a real answer
	// instead of a dropped connection.
	mcpReadBudget = 5 * time.Minute
	// mcpSessionIdle is how long a session survives without a request. A
	// session holds only its training slot and staged uploads, so expiry is
	// cheap.
	mcpSessionIdle = 30 * time.Minute
	// mcpMaxProgress bounds the progress frames one tool call records, so a
	// long wakeword training cannot grow an unbounded response list.
	mcpMaxProgress = 64
)

// mcpSupportedVersions are the Streamable HTTP revisions this server accepts.
// 2024-11-05 is deliberately absent: that revision's transport is HTTP+SSE
// (separate GET and POST endpoints), which this server does not host, so
// answering an initialize with it would mislead the client. A client that asks
// for an unsupported revision is answered with mcpProtocolVersion and decides
// whether to proceed.
var mcpSupportedVersions = []string{mcpProtocolVersion, "2025-03-26"}

// mcpBatchAllowed reports whether JSON-RPC batching is part of a revision.
// 2025-06-18 removed batching: its POST body is exactly one message.
func mcpBatchAllowed(version string) bool {
	return version == "2025-03-26"
}

func mcpSupportedVersion(version string) bool {
	for _, supported := range mcpSupportedVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// JSON-RPC 2.0 codes used by the transport, plus two server-defined codes in
// the implementation-defined range for authentication and authorization.
const (
	mcpCodeParse          = -32700
	mcpCodeInvalidRequest = -32600
	mcpCodeMethodNotFound = -32601
	mcpCodeInvalidParams  = -32602
	mcpCodeInternal       = -32603
	// mcpCodeUnauthorized is returned when the presented API key is unknown or
	// revoked.
	mcpCodeUnauthorized = -32001
	// mcpCodeForbidden is returned when the key is valid but lacks the scope a
	// tool requires.
	mcpCodeForbidden = -32002
	// mcpCodeUnavailable is returned when a session-scoped resource (an upload
	// slot, a live session) cannot be provided right now.
	mcpCodeUnavailable = -32003
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Result and Error are only used to recognize an unsolicited JSON-RPC
	// response: this server never sends requests, so accepting one is a no-op
	// acknowledgement rather than a malformed-request error.
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *mcpError) Error() string { return e.Message }

// mcpHTTPError carries the transport status a session-management failure must
// use. The MCP Streamable HTTP binding asks for 400 when the session header is
// missing and 404 when the id is unknown.
type mcpHTTPError struct {
	status  int
	code    int
	message string
}

func (e *mcpHTTPError) Error() string { return e.message }

func newMCPHTTPError(status, code int, message string) error {
	return &mcpHTTPError{status: status, code: code, message: message}
}

type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type mcpToolResult struct {
	Content           []mcpContent `json:"content"`
	IsError           bool         `json:"isError,omitempty"`
	StructuredContent any          `json:"structuredContent,omitempty"`
}

// mcpAuth is one authenticated MCP request's credential.
type mcpAuth struct {
	info   websecurity.APIKeyInfo
	scopes []string
}

// keyID is the credential identity a session is bound to. It is never sent to
// a client; the prefix is public key metadata, not the secret.
func (a mcpAuth) keyID() string { return a.info.Name + "\x00" + a.info.Prefix }

// --- session registry ------------------------------------------------------

type mcpTraining struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type mcpSession struct {
	id string
	// keyID identifies the credential record the session belongs to: the key's
	// name plus its token prefix. The name alone is unique in normal use, but a
	// hand-edited credentials.json could repeat it, and the prefix is derived
	// from the token so two records can share it only by being the same
	// credential.
	keyID string
	// version is the revision negotiated at initialize. It governs batching
	// and is the fallback when a request omits MCP-Protocol-Version.
	version string
	created time.Time

	mu       sync.Mutex
	last     time.Time
	closed   bool
	training map[string]*mcpTraining
	uploads  map[string]*mcpUpload
	// inflight maps a JSON-RPC request id to its cancel function so an MCP
	// notifications/cancelled sent on another HTTP request can stop the work.
	inflight map[string]context.CancelFunc
}

// beginTraining reserves the session's single wakeword-training slot.
func (sess *mcpSession) beginTraining(id string, cancel context.CancelFunc) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return false
	}
	if sess.training == nil {
		sess.training = map[string]*mcpTraining{}
	}
	if len(sess.training) >= 1 {
		return false
	}
	sess.training[id] = &mcpTraining{cancel: cancel, done: make(chan struct{})}
	return true
}

// finishTraining clears the slot and releases any waiter. It is idempotent: a
// cancelled run is deleted by cancelTraining and the goroutine's deferred
// finish still runs.
func (sess *mcpSession) finishTraining(id string) {
	sess.mu.Lock()
	entry := sess.training[id]
	delete(sess.training, id)
	sess.mu.Unlock()
	if entry != nil {
		close(entry.done)
	}
}

func (sess *mcpSession) cancelTraining(id string) bool {
	sess.mu.Lock()
	entry := sess.training[id]
	delete(sess.training, id)
	sess.mu.Unlock()
	if entry == nil {
		return false
	}
	close(entry.done)
	entry.cancel()
	return true
}

func (sess *mcpSession) trainingEntry(id string) *mcpTraining {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.training[id]
}

// stop releases everything the session owns: its training run, its staged
// uploads and its in-flight tool calls. It is called when the session ends,
// when its key is revoked, and on listener shutdown.
func (sess *mcpSession) stop() {
	sess.mu.Lock()
	sess.closed = true
	entries := sess.training
	sess.training = map[string]*mcpTraining{}
	uploads := sess.uploads
	sess.uploads = map[string]*mcpUpload{}
	inflight := sess.inflight
	sess.inflight = map[string]context.CancelFunc{}
	sess.mu.Unlock()
	for _, entry := range entries {
		close(entry.done)
		entry.cancel()
	}
	for _, upload := range uploads {
		upload.abort(errMCPUploadSessionEnded)
	}
	for _, cancel := range inflight {
		cancel()
	}
}

// trackRequest registers an in-flight tool call so a cancellation notification
// can stop it. An empty id (a notification) is never tracked.
func (sess *mcpSession) trackRequest(id string, cancel context.CancelFunc) {
	if id == "" {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		cancel()
		return
	}
	if sess.inflight == nil {
		sess.inflight = map[string]context.CancelFunc{}
	}
	sess.inflight[id] = cancel
}

func (sess *mcpSession) untrackRequest(id string) {
	if id == "" {
		return
	}
	sess.mu.Lock()
	delete(sess.inflight, id)
	sess.mu.Unlock()
}

// cancelRequest stops one in-flight tool call by its JSON-RPC id.
func (sess *mcpSession) cancelRequest(id string) bool {
	sess.mu.Lock()
	cancel := sess.inflight[id]
	delete(sess.inflight, id)
	sess.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// setVersion records the revision negotiated at initialize. It governs later
// requests, including whether batching is part of the revision.
func (sess *mcpSession) setVersion(version string) {
	sess.mu.Lock()
	sess.version = version
	sess.mu.Unlock()
}

func (sess *mcpSession) negotiatedVersion() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.version == "" {
		return mcpProtocolVersion
	}
	return sess.version
}

// --- staged uploads --------------------------------------------------------

var (
	errMCPUploadAborted      = errors.New("upload aborted")
	errMCPUploadExpired      = errors.New("upload expired")
	errMCPUploadSessionEnded = errors.New("MCP session ended")
	errMCPUploadTooLarge     = errors.New("upload exceeds the operation limit")
	errMCPUploadStalled      = errors.New("upload stalled while draining")
)

// mcpUploadState is the staged-upload state machine. open accepts chunks;
// completing has stopped accepting them and is draining the chunks already in
// flight; done is terminal (completed, aborted, expired or failed).
type mcpUploadState uint8

const (
	mcpUploadOpen mcpUploadState = iota
	mcpUploadCompleting
	mcpUploadDone
)

// mcpUpload is one staged upload. Chunks arrive in separate HTTP requests and
// are written into a pipe that the existing sink consumes: the asset store's
// streaming writer for an asset, or the package InstallApplication stream for an
// app. That preserves the store's 32 MiB per-file limit and the artifact's
// archive limit without ever holding the file in the daemon heap, and it keeps
// the MCP path semantically identical to the browser's streaming route.
//
// Chunks must be sent sequentially; the state machine serializes the two
// operations that cannot overlap, namely a chunk write and the closing write
// that publishes the file.
type mcpUpload struct {
	id string

	pw     *io.PipeWriter
	cancel context.CancelFunc

	limit int64
	done  chan struct{}

	mu       sync.Mutex
	received int64
	state    mcpUploadState
	// cause records why the upload ended early, so a later chunk or completion
	// reports the specific reason instead of a generic sink failure.
	cause error
	// writes counts the chunks currently inside pw.Write, and drained is closed
	// whenever it falls back to zero. Completion waits for that instead of
	// closing the pipe under a chunk, which would publish a truncated file.
	writes  int
	drained chan struct{}
	timer   *time.Timer

	// result and err are written by the sink goroutine before done closes.
	result *mcpToolResult
	err    *mcpError
}

// abort ends the upload and makes its sink observe cause. It is idempotent and
// is the escape hatch for a stuck sink: it closes the pipe immediately, which
// also releases a chunk blocked inside pw.Write.
func (u *mcpUpload) abort(cause error) {
	u.mu.Lock()
	if u.state == mcpUploadDone {
		u.mu.Unlock()
		return
	}
	u.state = mcpUploadDone
	if u.cause == nil {
		u.cause = cause
	}
	if u.timer != nil {
		u.timer.Stop()
	}
	u.mu.Unlock()
	u.cancel()
	_ = u.pw.CloseWithError(cause)
}

// write appends one decoded chunk. It blocks when the sink is slower than the
// client, which is the intended backpressure; the chunk bound keeps the write
// itself allocation-cheap. A sink refusal is returned as the sink's own tool
// result, so a chunk call fails the way the completing call would.
func (u *mcpUpload) write(data []byte) (*mcpToolResult, *mcpError) {
	u.mu.Lock()
	switch u.state {
	case mcpUploadDone:
		cause := u.cause
		u.mu.Unlock()
		result, _ := mcpToolFailure(errConflict, mcpUploadEndedMessage(cause))
		return result, nil
	case mcpUploadCompleting:
		u.mu.Unlock()
		result, _ := mcpToolFailure(errConflict,
			"the upload is being completed; no further chunks are accepted")
		return result, nil
	}
	if u.received+int64(len(data)) > u.limit {
		u.mu.Unlock()
		u.abort(errMCPUploadTooLarge)
		return nil, &mcpError{Code: mcpCodeInvalidParams,
			Message: fmt.Sprintf("the upload exceeds the %d-byte limit", u.limit)}
	}
	u.received += int64(len(data))
	if u.writes == 0 {
		u.drained = make(chan struct{})
	}
	u.writes++
	u.mu.Unlock()

	_, writeErr := u.pw.Write(data)

	u.mu.Lock()
	u.writes--
	if u.writes == 0 && u.drained != nil {
		close(u.drained)
		u.drained = nil
	}
	u.mu.Unlock()

	if writeErr != nil {
		// The sink already refused or stopped; report its typed outcome, or the
		// reason the upload ended if the sink never produced one.
		<-u.done
		if u.result != nil {
			return u.result, nil
		}
		if u.err != nil {
			return nil, u.err
		}
		u.mu.Lock()
		cause := u.cause
		u.mu.Unlock()
		result, _ := mcpToolFailure(errConflict, mcpUploadEndedMessage(cause))
		return result, nil
	}
	return nil, nil
}

// beginComplete moves the upload into its completing state and returns a
// channel that closes once every in-flight chunk has left pw.Write. An upload
// that already ended is reported as a failed tool call, consistently with every
// other operation failure; only a duplicate completion is a protocol error.
func (u *mcpUpload) beginComplete() (<-chan struct{}, *mcpToolResult, *mcpError) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch u.state {
	case mcpUploadDone:
		result, _ := mcpToolFailure(errConflict, mcpUploadEndedMessage(u.cause))
		return nil, result, nil
	case mcpUploadCompleting:
		return nil, nil, &mcpError{Code: mcpCodeInvalidParams, Message: "the upload is already being completed"}
	}
	u.state = mcpUploadCompleting
	if u.writes == 0 {
		return nil, nil, nil
	}
	return u.drained, nil, nil
}

// stateOf reports the current state.
func (u *mcpUpload) stateOf() mcpUploadState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func (u *mcpUpload) receivedBytes() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.received
}

func (u *mcpUpload) failure() *mcpError {
	if u.err != nil {
		return u.err
	}
	return &mcpError{Code: mcpCodeInternal, Message: "the upload failed"}
}

// mcpUploadEndedMessage explains why an upload can no longer accept data.
func mcpUploadEndedMessage(cause error) string {
	switch {
	case errors.Is(cause, errMCPUploadExpired):
		return "the staged upload expired; begin a new one"
	case errors.Is(cause, errMCPUploadAborted):
		return "the staged upload was aborted"
	case errors.Is(cause, errMCPUploadSessionEnded):
		return "the MCP session ended before the upload finished"
	case errors.Is(cause, errMCPUploadStalled):
		return "the staged upload did not drain; begin a new one"
	case errors.Is(cause, errMCPUploadTooLarge):
		return "the staged upload exceeded the operation limit"
	default:
		return "the upload is no longer open"
	}
}

// mcpUploadCauseResult maps an upload's own end cause onto a failed tool call,
// so a completion of an aborted or expired upload is legible.
func mcpUploadCauseResult(err error) (*mcpToolResult, bool) {
	switch {
	case errors.Is(err, errMCPUploadExpired),
		errors.Is(err, errMCPUploadAborted),
		errors.Is(err, errMCPUploadSessionEnded),
		errors.Is(err, errMCPUploadStalled),
		errors.Is(err, errMCPUploadTooLarge):
		result, _ := mcpToolFailure(errConflict, mcpUploadEndedMessage(err))
		return result, true
	default:
		return nil, false
	}
}

func (sess *mcpSession) addUpload(u *mcpUpload) *mcpError {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return &mcpError{Code: mcpCodeUnavailable, Message: "the MCP session has ended"}
	}
	if sess.uploads == nil {
		sess.uploads = map[string]*mcpUpload{}
	}
	// A finished upload can never accept or publish anything more, so it is
	// reaped here: otherwise an abandoned or expired upload would occupy a slot
	// of the session's limited budget forever.
	for id, existing := range sess.uploads {
		if existing.stateOf() == mcpUploadDone {
			delete(sess.uploads, id)
		}
	}
	if len(sess.uploads) >= mcpMaxUploadsPerSession {
		return &mcpError{Code: mcpCodeUnavailable, Message: fmt.Sprintf(
			"at most %d uploads may be staged at once; complete or abort one", mcpMaxUploadsPerSession)}
	}
	sess.uploads[u.id] = u
	return nil
}

func (sess *mcpSession) upload(id string) *mcpUpload {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.uploads[id]
}

// dropUpload removes and returns one staged upload, so exactly one caller can
// complete or abort it.
func (sess *mcpSession) dropUpload(id string) *mcpUpload {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	u := sess.uploads[id]
	delete(sess.uploads, id)
	return u
}

// startMCPUpload begins one staged upload over a pipe. The sink starts reading
// immediately, so the first chunk is consumed while later chunks are still in
// flight.
func (s *Server) startMCPUpload(sess *mcpSession, kind, path, filename string) (*mcpUpload, *mcpError) {
	if mcpUploadLimit(kind) <= 0 {
		return nil, &mcpError{Code: mcpCodeInvalidParams, Message: `kind must be "asset" or "app"`}
	}
	if kind == "asset" && s.Assets == nil {
		return nil, &mcpError{Code: mcpCodeUnavailable, Message: "asset store is unavailable"}
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	u := &mcpUpload{
		id: "upload_" + hex.EncodeToString(raw[:]),
		pw: pw, cancel: cancel, limit: mcpUploadLimit(kind), done: make(chan struct{}),
	}
	u.timer = time.AfterFunc(mcpUploadLifetime, func() { u.abort(errMCPUploadExpired) })
	if rpcErr := sess.addUpload(u); rpcErr != nil {
		u.abort(errMCPUploadAborted)
		return nil, rpcErr
	}
	go func() {
		defer close(u.done)
		u.result, u.err = s.mcpUploadSink(ctx, kind, path, filename, pr)
		u.mu.Lock()
		u.state = mcpUploadDone
		if u.timer != nil {
			u.timer.Stop()
		}
		u.mu.Unlock()
	}()
	return u, nil
}

// mcpUploadLimit is the decoded size a staged upload may carry. It is the limit
// the browser route and the daemon enforce, not a smaller MCP-only one.
func mcpUploadLimit(kind string) int64 {
	switch kind {
	case "asset":
		return assets.MaxFileBytes
	case "app":
		return artifact.MaxArchiveBytes
	}
	return 0
}

func (s *Server) mcpOpenSession(keyID string) *mcpSession {
	now := time.Now()
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	sess := &mcpSession{
		id: hex.EncodeToString(raw[:]), keyID: keyID, created: now, last: now,
		version:  mcpProtocolVersion,
		training: map[string]*mcpTraining{},
		uploads:  map[string]*mcpUpload{},
		inflight: map[string]context.CancelFunc{},
	}
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	if s.mcpSessions == nil {
		s.mcpSessions = map[string]*mcpSession{}
	}
	s.mcpPruneLocked(now)
	if len(s.mcpSessions) >= maxMCPSessions {
		// Evict the least recently used session rather than growing unbounded.
		var oldestID string
		var oldest time.Time
		for id, existing := range s.mcpSessions {
			if oldestID == "" || existing.last.Before(oldest) {
				oldestID, oldest = id, existing.last
			}
		}
		if oldestID != "" {
			s.mcpDropLocked(oldestID)
		}
	}
	s.mcpSessions[sess.id] = sess
	return sess
}

// mcpSession resolves a session id for one authenticated key. A session is
// bound to the key that created it: a different valid key can neither borrow
// the session nor read its training slot.
func (s *Server) mcpSession(id, keyID string) (*mcpSession, bool) {
	if id == "" {
		return nil, false
	}
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	s.mcpPruneLocked(time.Now())
	sess, ok := s.mcpSessions[id]
	if !ok || sess.keyID != keyID {
		return nil, false
	}
	sess.last = time.Now()
	return sess, true
}

func (s *Server) mcpCloseSession(id, keyID string) bool {
	s.mcpMu.Lock()
	sess := s.mcpSessions[id]
	if sess != nil && sess.keyID == keyID {
		delete(s.mcpSessions, id)
	} else {
		sess = nil
	}
	s.mcpMu.Unlock()
	if sess == nil {
		return false
	}
	sess.stop()
	return true
}

// mcpPruneLocked drops idle sessions. Callers hold s.mcpMu.
func (s *Server) mcpPruneLocked(now time.Time) {
	for id, sess := range s.mcpSessions {
		if now.Sub(sess.last) > mcpSessionIdle {
			s.mcpDropLocked(id)
		}
	}
}

// mcpDropLocked removes one session and cancels its work. Callers hold s.mcpMu.
func (s *Server) mcpDropLocked(id string) {
	sess := s.mcpSessions[id]
	delete(s.mcpSessions, id)
	if sess != nil {
		go sess.stop()
	}
}

// reconcileMCPKeysOnAccess ends MCP sessions whose key was revoked or removed.
// Credential edits publish the access topic (the daemon watches
// credentials.json), so a revocation cancels that session's device work
// immediately instead of waiting for its next request or for idle expiry.
func (s *Server) reconcileMCPKeysOnAccess(ctx context.Context) {
	if s.Changes == nil || s.Auth == nil {
		return
	}
	signals, cancel := s.Changes.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case topic, ok := <-signals:
			if !ok {
				return
			}
			if topic == uichanges.Access {
				s.reconcileMCPKeys()
			}
		}
	}
}

// mcpKeyID is the credential identity used for session binding. It is shared
// with the reconcile path so a revoked record can never keep a session alive.
func mcpKeyID(key websecurity.APIKeyInfo) string { return key.Name + "\x00" + key.Prefix }

// reconcileMCPKeys drops every session whose key is no longer active. The
// session is removed from the registry before its work is stopped, so no new
// request can reach it while the teardown runs.
func (s *Server) reconcileMCPKeys() {
	if s.Auth == nil {
		return
	}
	active := map[string]bool{}
	for _, key := range s.Auth.ListAPIKeys() {
		if !key.Revoked {
			active[mcpKeyID(key)] = true
		}
	}
	s.mcpMu.Lock()
	var dropped []*mcpSession
	for id, sess := range s.mcpSessions {
		if !active[sess.keyID] {
			delete(s.mcpSessions, id)
			dropped = append(dropped, sess)
		}
	}
	s.mcpMu.Unlock()
	for _, sess := range dropped {
		sess.stop()
	}
	if len(dropped) > 0 && s.Logs != nil {
		s.Logs.Infof("mcp", "", "ended %d MCP session(s) after an API-key change", len(dropped))
	}
}

// closeAllMCPSessions ends every session and cancels its device work. It runs
// on listener shutdown so no training run outlives the server.
func (s *Server) closeAllMCPSessions() {
	s.mcpMu.Lock()
	sessions := s.mcpSessions
	s.mcpSessions = map[string]*mcpSession{}
	s.mcpMu.Unlock()
	for _, sess := range sessions {
		sess.stop()
	}
}

// --- HTTP transport --------------------------------------------------------

// serveMCP is the Streamable HTTP MCP endpoint.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	// The browser middleware mints a CSRF cookie for every response; an MCP
	// client is not a browser and must never be handed one.
	w.Header().Del("Set-Cookie")
	if s.Auth == nil {
		http.Error(w, "MCP is unavailable: no security store", http.StatusServiceUnavailable)
		return
	}
	if !mcpOriginAllowed(r) {
		mcpWriteError(w, http.StatusForbidden, mcpCodeForbidden, "cross-origin MCP request rejected")
		return
	}
	key := apiKeyFromRequest(r)
	info, scopes, ok := s.Auth.APIKeyScopes(key)
	if !ok {
		if s.Logs != nil {
			s.Logs.Warnf("mcp", "", "rejected MCP request from %s: invalid or revoked API key", remoteHost(r))
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="capsulad-mcp"`)
		mcpWriteError(w, http.StatusUnauthorized, mcpCodeUnauthorized,
			"a valid API key is required; present it as an Authorization: Bearer header")
		return
	}
	auth := mcpAuth{info: info, scopes: scopes}

	switch r.Method {
	case http.MethodGet:
		// No server-initiated notification stream is offered; the binding
		// permits refusing GET with 405 in that case.
		w.Header().Set("Allow", "POST, DELETE")
		mcpWriteError(w, http.StatusMethodNotAllowed, mcpCodeMethodNotFound,
			"the capsulad MCP endpoint is request/response only; use POST")
		return
	case http.MethodDelete:
		if !s.mcpCloseSession(strings.TrimSpace(r.Header.Get(mcpSessionHeader)), auth.keyID()) {
			mcpWriteError(w, http.StatusNotFound, mcpCodeInvalidRequest, "unknown MCP session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if mediaTypeOf(r) != "application/json" {
		mcpWriteError(w, http.StatusUnsupportedMediaType, mcpCodeInvalidRequest,
			"the MCP endpoint accepts application/json bodies only")
		return
	}
	if !mcpAcceptable(r) {
		mcpWriteError(w, http.StatusNotAcceptable, mcpCodeInvalidRequest,
			"the MCP endpoint answers application/json or text/event-stream")
		return
	}
	// A client repeats the negotiated revision here; an invalid or unsupported
	// value is a 400 rather than a silent reinterpretation of the request.
	if version := strings.TrimSpace(r.Header.Get(mcpVersionHeader)); version != "" && !mcpSupportedVersion(version) {
		mcpWriteError(w, http.StatusBadRequest, mcpCodeInvalidRequest, fmt.Sprintf(
			"unsupported MCP-Protocol-Version %q; this server implements %s",
			version, strings.Join(mcpSupportedVersions, ", ")))
		return
	}
	// A long operation (a Wi-Fi connect, a package install, a training run a
	// caller waits on) may legitimately outlive the listener's default write
	// timeout, and a one-call upload may outlive its read timeout. Extend both
	// budgets for this request, exactly as the browser's package upload route
	// extends its read budget.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(mcpWriteBudget))
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(mcpReadBudget))
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPBodyBytes+1))
	if err != nil {
		mcpWriteError(w, http.StatusBadRequest, mcpCodeParse, "could not read the request body")
		return
	}
	if len(body) > maxMCPBodyBytes {
		mcpWriteError(w, http.StatusRequestEntityTooLarge, mcpCodeInvalidRequest,
			fmt.Sprintf("request body exceeds the %d-byte MCP limit", maxMCPBodyBytes))
		return
	}
	if err := s.serveMCPMessages(w, r, auth, body); err != nil {
		var httpErr *mcpHTTPError
		if errors.As(err, &httpErr) {
			mcpWriteError(w, httpErr.status, httpErr.code, httpErr.message)
			return
		}
		mcpWriteError(w, http.StatusBadRequest, mcpCodeParse, err.Error())
	}
}

// serveMCPMessages handles one JSON-RPC message or a batch.
func (s *Server) serveMCPMessages(w http.ResponseWriter, r *http.Request, auth mcpAuth, body []byte) error {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		writeMCPResponses(w, r, nil, nil)
		return nil
	}
	var responses []mcpResponse
	var created *mcpSession
	if strings.HasPrefix(trimmed, "[") {
		if !mcpBatchAllowed(s.mcpEffectiveVersion(r, auth)) {
			return newMCPHTTPError(http.StatusBadRequest, mcpCodeInvalidRequest,
				"JSON-RPC batching was removed in MCP 2025-06-18; send one message per POST")
		}
		var batch []mcpRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			return fmt.Errorf("malformed JSON-RPC batch: %w", err)
		}
		if len(batch) == 0 {
			return errors.New("empty JSON-RPC batch")
		}
		for i := range batch {
			resp, sess, err := s.mcpHandle(r.Context(), r, auth, &batch[i])
			if err != nil {
				return err
			}
			if sess != nil {
				created = sess
			}
			if resp != nil {
				responses = append(responses, *resp)
			}
		}
	} else {
		var req mcpRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return fmt.Errorf("malformed JSON-RPC request: %w", err)
		}
		resp, sess, err := s.mcpHandle(r.Context(), r, auth, &req)
		if err != nil {
			return err
		}
		created = sess
		if resp != nil {
			responses = append(responses, *resp)
		}
	}
	writeMCPResponses(w, r, responses, created)
	return nil
}

// writeMCPResponses writes the JSON-RPC outcome. Notifications alone are
// acknowledged with an empty 202.
func writeMCPResponses(w http.ResponseWriter, r *http.Request, responses []mcpResponse, created *mcpSession) {
	if created != nil {
		w.Header().Set(mcpSessionHeader, created.id)
	}
	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	payload := any(responses[0])
	if len(responses) > 1 {
		payload = responses
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		mcpWriteError(w, http.StatusInternalServerError, mcpCodeInternal, "could not encode the response")
		return
	}
	if mcpPrefersSSE(r) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", encoded)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// mcpHandle dispatches one JSON-RPC message. A nil response means the message
// was a notification or an accepted JSON-RPC response. A non-nil session is the
// one the transport must announce in the Mcp-Session-Id header.
func (s *Server) mcpHandle(ctx context.Context, r *http.Request, auth mcpAuth, req *mcpRequest) (*mcpResponse, *mcpSession, error) {
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return mcpErrorResponse(req.ID, mcpCodeInvalidRequest, `"jsonrpc" must be "2.0"`), nil, nil
	}
	notification := len(req.ID) == 0
	switch {
	case req.Method == "initialize":
		return s.mcpInitialize(req, auth.keyID(), strings.TrimSpace(r.Header.Get(mcpSessionHeader)))
	case req.Method == "ping":
		// A ping is allowed before initialization completes.
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, nil, nil
	case req.Method == "":
		if len(req.Result) > 0 || len(req.Error) > 0 {
			// An unsolicited JSON-RPC response. This server sends no requests,
			// so accepting it is a no-op acknowledgement, not an error.
			return nil, nil, nil
		}
		return mcpErrorResponse(req.ID, mcpCodeInvalidRequest, "missing method"), nil, nil
	case strings.HasPrefix(req.Method, "notifications/"):
		if req.Method == "notifications/cancelled" {
			s.mcpCancelNotification(r, auth, req.Params)
		}
		// Every notification is acknowledged by the empty 202; a client that
		// wrongly attaches an id still gets a well-formed empty result.
		if notification {
			return nil, nil, nil
		}
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, nil, nil
	}

	// Every other method is session-bound so a training run's cancellation
	// handle, a staged upload and an in-flight call stay with the client that
	// started them.
	id := strings.TrimSpace(r.Header.Get(mcpSessionHeader))
	if id == "" {
		return nil, nil, newMCPHTTPError(http.StatusBadRequest, mcpCodeInvalidRequest,
			"Mcp-Session-Id is required; call initialize first")
	}
	sess, ok := s.mcpSession(id, auth.keyID())
	if !ok {
		return nil, nil, newMCPHTTPError(http.StatusNotFound, mcpCodeInvalidRequest,
			"unknown, expired or foreign MCP session")
	}

	switch req.Method {
	case "tools/list":
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: s.mcpToolsList(auth)}, nil, nil
	case "tools/call":
		result, rpcErr := s.mcpToolsCall(ctx, auth, sess, req.Params, string(req.ID))
		if rpcErr != nil {
			return mcpErrorResponse(req.ID, rpcErr.Code, rpcErr.Message), nil, nil
		}
		return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, nil, nil
	default:
		return mcpErrorResponse(req.ID, mcpCodeMethodNotFound, fmt.Sprintf("unknown method %q", req.Method)), nil, nil
	}
}

// mcpCancelNotification applies notifications/cancelled to the in-flight tool
// call it names. The cancel handle lives on the session, so a notification sent
// on another HTTP request stops the same work.
func (s *Server) mcpCancelNotification(r *http.Request, auth mcpAuth, params json.RawMessage) {
	id := strings.TrimSpace(r.Header.Get(mcpSessionHeader))
	if id == "" {
		return
	}
	sess, ok := s.mcpSession(id, auth.keyID())
	if !ok {
		return
	}
	var note struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if len(params) == 0 || json.Unmarshal(params, &note) != nil || len(note.RequestID) == 0 {
		return
	}
	if sess.cancelRequest(string(note.RequestID)) && s.Logs != nil {
		s.Logs.Infof("mcp", "", "request %s cancelled by the client", string(note.RequestID))
	}
}

// mcpEffectiveVersion is the revision that governs one request body: the
// session's negotiated revision when there is a session, then the request
// header, then the transport-spec fallback for a body that carries neither
// (2025-03-26). It matters for batching, which 2025-06-18 removed.
func (s *Server) mcpEffectiveVersion(r *http.Request, auth mcpAuth) string {
	if id := strings.TrimSpace(r.Header.Get(mcpSessionHeader)); id != "" {
		if sess, ok := s.mcpSession(id, auth.keyID()); ok {
			return sess.negotiatedVersion()
		}
	}
	if version := strings.TrimSpace(r.Header.Get(mcpVersionHeader)); version != "" {
		return version
	}
	return "2025-03-26"
}

func (s *Server) mcpInitialize(req *mcpRequest, keyID, existingID string) (*mcpResponse, *mcpSession, error) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(req.Params) > 0 {
		// A malformed params object is tolerated: the revision is the only
		// field the server reads and it defaults to the current revision.
		_ = json.Unmarshal(req.Params, &params)
	}
	chosen := mcpProtocolVersion
	for _, supported := range mcpSupportedVersions {
		if params.ProtocolVersion == supported {
			chosen = supported
			break
		}
	}
	// A repeated initialize keeps the session it names instead of minting a
	// new one; a client that lost its session sends initialize without a
	// header and gets a fresh one.
	var sess *mcpSession
	if existingID != "" {
		if current, ok := s.mcpSession(existingID, keyID); ok {
			sess = current
		}
	}
	if sess == nil {
		sess = s.mcpOpenSession(keyID)
	}
	sess.setVersion(chosen)
	result := map[string]any{
		"protocolVersion": chosen,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name": "capsulad", "title": "capsulad appliance daemon", "version": version.Version,
		},
		"instructions": "capsulad exposes the same operations as its Web UI: read daemon state with " +
			"get_status/get_state, and administer the appliance with the operator tools. Tools appear " +
			"according to the scopes of the API key presented. All mutations go through the daemon's " +
			"existing allowlisted operations; there are no private endpoints.",
	}
	if len(req.ID) == 0 {
		return nil, sess, nil
	}
	return &mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, sess, nil
}

func mcpErrorResponse(id json.RawMessage, code int, message string) *mcpResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message}}
}

func mcpWriteError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(mcpErrorResponse(nil, code, message))
}

// mcpOriginAllowed rejects a concrete cross-origin request. A non-browser
// client sends no Origin at all, which is allowed: the API key is the
// credential, and the check exists to keep a random web page from reaching a
// loopback MCP endpoint through the user's own browser.
func mcpOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	// "null" is what a sandboxed or certificate-bypassed document reports; it is
	// a browser context, not an API client, so it is refused.
	if origin == "null" {
		return false
	}
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return strings.EqualFold(host, r.Host)
}

func mcpAcceptable(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, ';'); i >= 0 {
			part = strings.TrimSpace(part[:i])
		}
		switch part {
		case "*/*", "application/*", "application/json", "text/event-stream":
			return true
		}
	}
	return false
}

// mcpPrefersSSE reports whether the client accepts only an event stream. A
// client that offers application/json as well receives plain JSON.
func mcpPrefersSSE(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	sse, jsonOK := false, false
	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, ';'); i >= 0 {
			part = strings.TrimSpace(part[:i])
		}
		switch part {
		case "*/*", "application/*":
			jsonOK = true
		case "application/json":
			jsonOK = true
		case "text/event-stream":
			sse = true
		}
	}
	return sse && !jsonOK
}

// --- operation recorder ----------------------------------------------------

// mcpRecorder implements operationSink for one MCP tool call. It collects the
// terminal result or error and the progress frames of a streaming operation.
// The training slot is delegated to the session, which outlives the request.
type mcpRecorder struct {
	sess *mcpSession

	mu       sync.Mutex
	done     chan struct{}
	settled  bool
	result   *webv1.Result
	errCode  webv1.ErrorCode
	errMsg   string
	progress []*webv1.Progress

	// trainingID and trainingDone identify the run this call started, so a
	// waiting caller observes the same completion the session's slot does.
	trainingID     string
	trainingDoneCh chan struct{}
}

func newMCPRecorder(sess *mcpSession) *mcpRecorder {
	return &mcpRecorder{sess: sess, done: make(chan struct{})}
}

func (rec *mcpRecorder) sendError(_ uint64, code webv1.ErrorCode, message string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.settled {
		return
	}
	rec.settled, rec.errCode, rec.errMsg = true, code, message
	close(rec.done)
}

func (rec *mcpRecorder) sendResult(_ uint64, res *webv1.Result) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.settled {
		return
	}
	rec.settled, rec.result = true, res
	close(rec.done)
}

// sendProgress records a non-terminal frame. A caller that returned before the
// operation finished (the non-blocking wakeword training start) keeps the
// recorder alive through the session's training slot, so progress still lands
// somewhere; the list is bounded and the oldest frames are dropped.
func (rec *mcpRecorder) sendProgress(_ uint64, p *webv1.Progress) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.progress) >= mcpMaxProgress {
		rec.progress = rec.progress[1:]
	}
	rec.progress = append(rec.progress, p)
}

// sendEvent drops live-subscription events: the MCP frontend is
// request/response, and a caller reads the same state through get_state.
func (rec *mcpRecorder) sendEvent(*webv1.Event) {}

func (rec *mcpRecorder) beginTraining(id string, cancel context.CancelFunc) bool {
	if !rec.sess.beginTraining(id, cancel) {
		return false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.trainingID = id
	if entry := rec.sess.trainingEntry(id); entry != nil {
		rec.trainingDoneCh = entry.done
	}
	return true
}

func (rec *mcpRecorder) finishTraining(id string) { rec.sess.finishTraining(id) }

func (rec *mcpRecorder) cancelTraining(id string) bool { return rec.sess.cancelTraining(id) }

// trainingDone reports the completion channel of the run this call started.
// It is nil when the call did not start a run.
func (rec *mcpRecorder) trainingDone(id string) <-chan struct{} {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.trainingID != id {
		return nil
	}
	return rec.trainingDoneCh
}

// wait blocks until the operation settled or ctx ended. It reports whether the
// operation settled.
func (rec *mcpRecorder) wait(ctx context.Context) bool {
	select {
	case <-rec.done:
		return true
	default:
	}
	select {
	case <-rec.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (rec *mcpRecorder) snapshot() (res *webv1.Result, errCode webv1.ErrorCode, errMsg string, ok bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.result, rec.errCode, rec.errMsg, rec.settled
}

// lastProgress returns the newest recorded progress frame.
func (rec *mcpRecorder) lastProgress() *webv1.Progress {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.progress) == 0 {
		return nil
	}
	return rec.progress[len(rec.progress)-1]
}

// --- shared result plumbing ------------------------------------------------

// mcpDispatch runs one allowlisted command through the shared operation layer
// and converts its outcome into an MCP tool result.
func (s *Server) mcpDispatch(ctx context.Context, sess *mcpSession, cmd *webv1.Command) (*mcpToolResult, *mcpError) {
	rec := newMCPRecorder(sess)
	s.dispatch(rec, 1, cmd, ctx)
	if !rec.wait(ctx) {
		return mcpToolFailure(webv1.ErrorCode_ERROR_CODE_CANCELLED, "the request was cancelled before the daemon answered")
	}
	res, code, message, settled := rec.snapshot()
	if !settled {
		return mcpToolFailure(webv1.ErrorCode_ERROR_CODE_INTERNAL, "the daemon returned no outcome")
	}
	if res == nil {
		return mcpToolFailure(code, message)
	}
	return mcpProtoResult(res)
}

// mcpProtoResult renders a protobuf result as MCP content.
func mcpProtoResult(msg proto.Message) (*mcpToolResult, *mcpError) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the result"}
	}
	return mcpJSONResult(json.RawMessage(raw))
}

// mcpJSONResult renders a JSON value as MCP content carrying both the text and
// the structured form.
func mcpJSONResult(raw json.RawMessage) (*mcpToolResult, *mcpError) {
	var structured any
	if err := json.Unmarshal(raw, &structured); err != nil {
		return nil, &mcpError{Code: mcpCodeInternal, Message: "could not encode the result"}
	}
	return &mcpToolResult{
		Content:           []mcpContent{{Type: "text", Text: string(raw)}},
		StructuredContent: structured,
	}, nil
}

// mcpToolFailure renders a daemon refusal as a failed tool call: the operation
// ran and failed, which is a result with isError, not a protocol error.
func mcpToolFailure(code webv1.ErrorCode, message string) (*mcpToolResult, *mcpError) {
	if message == "" {
		message = "the operation failed"
	}
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code.String(), "message": message},
	})
	return &mcpToolResult{
		Content:           []mcpContent{{Type: "text", Text: string(payload)}},
		IsError:           true,
		StructuredContent: map[string]any{"error": map[string]any{"code": code.String(), "message": message}},
	}, nil
}

// mcpInvalidParams reports a malformed tool argument object.
func mcpInvalidParams(err error) *mcpError {
	return &mcpError{Code: mcpCodeInvalidParams, Message: "invalid tool arguments: " + err.Error()}
}
