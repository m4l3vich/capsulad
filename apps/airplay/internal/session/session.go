package session

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"capsulad/apps/airplay/internal/artwork"
	"capsulad/apps/airplay/internal/dmap"
	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/raop"
	"capsulad/apps/airplay/internal/rtsp"
)

// Audio payload types used by AirPlay.
const (
	streamTypeRealtime      = 96
	streamTypeRedundant     = 97
	streamTypeBuffered      = 103
	streamTypeRemoteControl = 130
	defaultIdleTimeout      = 45 * time.Second
	maxRequestBody          = 8 << 20
)

// errSessionEnded reports that the sender ended the session on purpose.
var errSessionEnded = errors.New("session ended by the sender")

// channelKeys is one direction-keyed pair for the encrypted channels.
type channelKeys struct {
	write []byte
	read  []byte
}

// hapReadStats is the optional telemetry EncryptedControlInterop exposes on a
// hapcrypt Session. The tokens are fixed and contain no error text: none,
// eof, partial-length, partial-body, bad-tag or io-error. A build or test
// double without the snapshot simply omits the HAP fields.
type hapReadStats interface {
	ReadStats() (blocks uint64, outcome string)
}

// progress is the sender's start/current/end playback position report.
type progress struct {
	start, current, end uint32
	has                 bool
}

// Session is one sender's RTSP session: the control connection, the pairing
// state, the audio streams, the event channel and the ring presentation.
type Session struct {
	m    *Manager
	conn Stream
	br   *bufio.Reader
	out  io.Writer
	peer Peer

	writeMu      sync.Mutex
	eventWriteMu sync.Mutex

	mu                 sync.Mutex
	setup              *pair.SetupServer
	verify             *pair.VerifyServer
	shared             []byte
	ctrlKeys           *channelKeys
	evKeys             *channelKeys
	encrypted          bool
	secure             *hapcrypt.Session
	announce           *raop.Stream
	announceKey        []byte
	controllerID       string
	closeAfterResponse bool

	userAgent  string
	deviceName string

	streams []*audioStream

	ap2Control    *controlState
	eventListener Listener
	eventConn     Stream
	eventOut      io.Writer
	eventIn       *bufio.Reader
	eventReady    bool   // updateInfo has been written on the current channel
	eventGen      uint64 // incremented whenever a ready event channel is published
	eventMu       sync.Mutex
	eventApplyMu  sync.Mutex // serializes replacement publish with request state application

	volumeKick chan struct{} // wakes volumeWatch after a new event channel
	// masterCtx bounds and cancels a framework master write when this session
	// ends. It is created once per session and never replaced.
	masterCtx    context.Context
	masterCancel context.CancelFunc
	// displayMu protects this session's delivered/display volume state. It is
	// separate from the manager's authoritative cache: updateInfo and a
	// successful sender SET seed it, while /info and GET_PARAMETER never do, so
	// a probe cannot consume a pending receiver-side change notification.
	displayMu    sync.Mutex
	displayUnit  float64
	displayKnown bool
	displaySeq   uint64
	// senderEpoch advances when an owner SET is accepted (before its framework
	// write) and when a new updateInfo seeds a channel. A queued mirror pass
	// rechecks it under the event writer order so a value read before a newer
	// sender write is not posted afterwards.
	senderEpoch uint64
	// senderPending counts owner SETs accepted but not completed. A mirror
	// pass must not reserve while one is pending; completion also bumps the
	// epoch so a pass started during a parked setter cannot post the old level.
	senderPending int
	// mirror* is one last notification state, not a history: pending/reserved
	// while its TCP write is in flight, then sent/armed for a bounded delayed
	// echo. mirrorWriter/mirrorGen make correlation live-writer scoped, so a
	// replacement or unpublish invalidates it even before updateInfo.
	mirrorUnit     float64
	mirrorSentAt   time.Time
	mirrorArmed    bool
	mirrorPending  bool
	mirrorReserved bool
	mirrorTokenID  uint64
	mirrorGen      uint64
	mirrorWriter   io.Writer
	rate           float64

	metadata      dmap.Metadata
	palette       []artwork.RGB
	envelopeState *artwork.Envelope
	envelopeLevel float64
	ring          Ring
	ringDone      chan struct{}
	progress      progress

	boardGen   uint64
	done       chan struct{}
	closeOnce  sync.Once
	lastActive int64
	audioSeen  atomic.Bool

	// Candidate telemetry: a connection that never starts a session leaves one
	// bounded log line when it ends. Only a request count, an allowlisted
	// method/path class and the last status are kept; never headers, bodies or
	// query strings. Zero-request connections (dual-stack losers, port probes)
	// stay silent.
	started              time.Time
	candidateRequests    int
	candidateStage       string
	candidateStatus      int
	candidateWriteFailed bool

	// Encrypted-candidate telemetry. Pairing keys a connection but does not
	// promote it; if that candidate ends before ANNOUNCE/SETUP, these fields
	// show whether it sent post-upgrade requests and where its read loop
	// stopped. Only allowlisted stages and fixed outcome tokens are kept.
	postUpgradeRequests    int
	postUpgradeFirstStage  string
	postUpgradeFirstStatus int
	postUpgradeLastStage   string
	postUpgradeLastStatus  int
	rtspParseClass         string

	// promoted is set once the manager makes this connection the live session.
	// A candidate that closes without a session-initiating request must leave
	// the player and the advertisements untouched.
	promoted    atomic.Bool
	promoteOnce sync.Once
	promotedCh  chan struct{}

	// lifecycleMu makes begin and close mutually exclusive. A close that wins
	// prevents the player from being started afterwards; a begin that wins is
	// always followed by that close's cleanup, which ends the player.
	lifecycleMu sync.Mutex
	ended       bool
	reserved    bool

	reservationOnce sync.Once
	// Timeouts are snapshotted when the connection is accepted, so a test or
	// settings change cannot race a watchdog goroutine's read.
	candidateTimeout   time.Duration
	reservationTimeout time.Duration

	// staged holds streams created by a successful SETUP that has not yet
	// committed ownership. Their listeners are open so the reply can name the
	// ports, but no reader or playback loop runs until commitStreams.
	staged           []*audioStream
	stagedGID        string
	stagedGIDSet     bool
	setupStartsMedia bool // this SETUP staged at least one media stream
}

func newSession(m *Manager, conn Stream) *Session {
	masterCtx, masterCancel := context.WithCancel(context.Background())
	s := &Session{
		m:                  m,
		conn:               conn,
		br:                 bufio.NewReaderSize(conn, 16*1024),
		out:                conn,
		peer:               conn.Peer(),
		rate:               1,
		started:            time.Now(),
		done:               make(chan struct{}),
		volumeKick:         make(chan struct{}, 1),
		masterCtx:          masterCtx,
		masterCancel:       masterCancel,
		promotedCh:         make(chan struct{}),
		candidateTimeout:   candidateRequestTimeout,
		reservationTimeout: reservationTimeout,
	}
	s.touch()
	return s
}

func (s *Session) touch() { atomic.StoreInt64(&s.lastActive, time.Now().UnixNano()) }

func (s *Session) idleFor() time.Duration {
	return time.Since(time.Unix(0, atomic.LoadInt64(&s.lastActive)))
}

// run serves a candidate until it starts a session or the connection breaks.
// It does not own the player or the advertisements: begin runs on the first
// session-initiating request, after the manager has promoted the connection.
func (s *Session) run() {
	defer func() {
		if r := recover(); r != nil {
			s.m.logf("session: recovered from %v", r)
			s.cleanup(fmt.Sprintf("internal error: %v", r))
			return
		}
	}()
	go s.candidateWatchdog()
	err := s.serveRTSP()
	reason := "the sender closed the connection"
	switch {
	case err == nil:
	case errors.Is(err, errSessionEnded):
		reason = "teardown"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		reason = "the control connection ended"
	default:
		reason = err.Error()
	}
	s.cleanup(reason)
}

// candidateWatchdog closes a connection that never sends a request that starts
// a session. The live session, the player and the advertisements are untouched;
// a dual-stack sender's losing connection simply ends here.
func (s *Session) candidateWatchdog() {
	timer := time.NewTimer(s.candidateTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-s.promotedCh:
	case <-timer.C:
		s.expireCandidate()
	}
}

// expireCandidate closes a connection that never reserved or started a
// session. The decision is taken under lifecycleMu, the same lock reservation
// and begin use, so a timeout cannot close a connection that just became the
// live owner.
func (s *Session) expireCandidate() {
	s.lifecycleMu.Lock()
	if s.ended || s.reserved || s.promoted.Load() {
		s.lifecycleMu.Unlock()
		return
	}
	s.ended = true
	s.lifecycleMu.Unlock()
	s.close("no session request")
}

// reserve marks this candidate as the one whose success response is in flight.
// It no longer expires or gets evicted, but the live sender is untouched until
// commit.
func (s *Session) reserve() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.ended || s.promoted.Load() {
		return false
	}
	s.reserved = true
	s.reservationOnce.Do(func() { go s.reservationWatchdog() })
	return true
}

// reservationWatchdog releases a reservation whose success response never
// completes. The expiry decision and begin share lifecycleMu, so a timeout can
// never close a connection that already committed.
func (s *Session) reservationWatchdog() {
	timer := time.NewTimer(s.reservationTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return
	case <-s.promotedCh:
		return
	case <-timer.C:
	}
	s.lifecycleMu.Lock()
	expired := s.reserved && !s.promoted.Load() && !s.ended
	if expired {
		s.reserved = false
		s.ended = true
	}
	s.lifecycleMu.Unlock()
	if expired {
		// Clear the manager's sole reservation immediately. cleanup may be
		// blocked behind a control write, and later senders must not receive
		// 503s for a reservation this watchdog has already expired.
		s.m.releaseReservation(s)
		s.close("session reservation expired")
	}
}

// release drops a reservation that did not commit.
func (s *Session) release() {
	s.lifecycleMu.Lock()
	s.reserved = false
	s.lifecycleMu.Unlock()
}

// boardGeneration returns the published media generation, or zero while this
// connection is still a candidate. Event-channel goroutines use it so a
// candidate's metadata can never mutate the previous owner's player.
func (s *Session) boardGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boardGen
}

// sourceName builds the board source name from the latest sender hints. The
// caller must not hold s.mu.
func (s *Session) sourceName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return media.NamesFromSource(s.userAgent, s.deviceName, s.peer.Address)
}

// setUserAgentOnce records the RTSP User-Agent the first time it is seen and
// refreshes the live board's source name without holding the session lock
// across the board call.
func (s *Session) setUserAgentOnce(userAgent string) {
	s.mu.Lock()
	if s.userAgent != "" {
		s.mu.Unlock()
		return
	}
	s.userAgent = userAgent
	source := media.NamesFromSource(s.userAgent, s.deviceName, s.peer.Address)
	gen := s.boardGen
	s.mu.Unlock()
	if gen != 0 && s.m.board != nil {
		s.m.board.SetSourceName(gen, source)
	}
}

// setDeviceName records the event channel's device name and refreshes the live
// board's source name, unless this connection is still a candidate.
func (s *Session) setDeviceName(name string) {
	s.mu.Lock()
	s.deviceName = name
	source := media.NamesFromSource(s.userAgent, s.deviceName, s.peer.Address)
	gen := s.boardGen
	s.mu.Unlock()
	if gen != 0 && s.m.board != nil {
		s.m.board.SetSourceName(gen, source)
	}
}

// replayPendingState publishes metadata, progress and the latest source name
// to a newly started board generation. Event-channel updates that arrived
// while this connection was a candidate are stored on the Session and would
// otherwise never reach the board because their generation was still zero.
func (s *Session) replayPendingState(gen uint64) {
	if s.m.board == nil || gen == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep the snapshot and its board publication under the same lock so an
	// event update cannot land between them and then be overwritten by this
	// older replay. Board mutations carry the new generation and are safe to
	// call here; they do not acquire s.mu.
	source := media.NamesFromSource(s.userAgent, s.deviceName, s.peer.Address)
	metadata := s.metadata
	progress := s.progress
	s.m.board.SetSourceName(gen, source)
	if metadata.Title != "" || metadata.Artist != "" || metadata.Album != "" {
		position := uint64(0)
		duration := uint64(0)
		if progress.has {
			position = uint64(progress.current)
			if progress.end > progress.start {
				duration = uint64(progress.end - progress.start)
			}
		}
		s.m.board.SetMetadata(gen, metadata.Title, metadata.Artist, metadata.Album, duration, position)
	}
	if progress.has {
		s.m.board.SetProgress(gen, uint64(progress.current))
	}
}

// begin starts the resources a promoted session owns. It returns false when
// the session already ended, in which case the player must not be started. It
// runs once, from the session goroutine, after the manager made this connection
// active.
func (s *Session) begin() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.ended {
		return false
	}
	s.touch()
	var gen uint64
	if s.m.board != nil {
		gen = s.m.board.BeginSession(s.sourceName())
	}
	s.mu.Lock()
	s.boardGen = gen
	s.mu.Unlock()
	if s.m.board != nil {
		s.replayPendingState(gen)
	}
	s.promoted.Store(true)
	s.promoteOnce.Do(func() { close(s.promotedCh) })
	go s.idleWatchdog()
	go s.volumeWatch()
	return true
}

// reserveOwnership secures this candidate before its success response is sent.
// commitOwnership makes it the live session after the response and any channel
// upgrade completed. A failure to reserve must prevent the success response;
// a failure to commit closes the connection without touching the live sender.
func (s *Session) reserveOwnership() error {
	if s.promoted.Load() {
		return nil
	}
	if !s.m.reserve(s) {
		return errors.New("the connection could not reserve session ownership")
	}
	return nil
}

func (s *Session) commitOwnership() error {
	if s.promoted.Load() {
		return nil
	}
	if !s.m.commit(s) {
		return errors.New("the connection could not commit session ownership")
	}
	return nil
}

func (s *Session) releaseOwnership() { s.m.releaseReservation(s) }

// refusalWriteTimeout bounds a refusal written when ownership cannot be
// reserved. The transport write has no deadline of its own, so a stalled peer
// must not hold the session goroutine open.
var refusalWriteTimeout = time.Second

// writeRefusal writes an explicit refusal when ownership cannot be reserved.
// The serving goroutine owns the outcome: the write runs beside it, reports its
// error on a buffered channel, and a timeout is recorded here before cleanup
// runs. A stalled peer therefore cannot produce a summary that claims a 503 was
// delivered when the write never completed.
func (s *Session) writeRefusal(request *rtsp.Request, status int, reason string) {
	response := rtsp.NewResponse(request.CSeq(), status, reason)
	outcome := make(chan error, 1)
	go func() {
		outcome <- s.writeResponse(request, response)
	}()
	select {
	case err := <-outcome:
		if err != nil {
			s.noteCandidateWriteFailed()
		}
	case <-time.After(refusalWriteTimeout):
		s.noteCandidateWriteFailed()
		if s.conn != nil {
			_ = s.conn.Close()
		}
	}
}

// noteGroupID stages a successful SETUP's group id. It is applied after the
// success response: commit uses it for a new owner, and an already-active owner
// applies it only while it still owns the receiver, so a refused or superseded
// SETUP cannot overwrite the live gid. A SETUP that omits gid does not clear a
// group id already belonging to the same sender.
func (s *Session) noteGroupID(gid string, set bool) {
	if !set && !s.promoted.Load() {
		// A candidate can send an event-only SETUP carrying gid and a later
		// media SETUP without one. Keep the last acknowledged candidate gid
		// so promotion still advertises the group.
		return
	}
	s.stagedGID, s.stagedGIDSet = gid, set
}

// applyStagedGroup applies the last successful SETUP's group id to this
// session, provided the manager still considers it the owner. An omitted gid
// leaves the current same-owner value alone.
func (s *Session) applyStagedGroup() {
	if s.stagedGIDSet {
		s.m.setGroupID(s, s.stagedGID)
	}
}

func (s *Session) idleWatchdog() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			limit := defaultIdleTimeout
			if s.audioSeen.Load() {
				// A sender that is streaming keeps the session alive; a source
				// that goes quiet without a teardown is still bounded.
				limit = 4 * defaultIdleTimeout
			}
			if s.idleFor() > limit {
				s.m.logf("session: no activity for %s; ending it", s.idleFor())
				s.close("idle timeout")
				return
			}
		}
	}
}

// close ends the session from another goroutine (takeover, shutdown, timeout).
func (s *Session) close(reason string) {
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.ended = true
		s.lifecycleMu.Unlock()
		close(s.done)
		s.cancelMasterVolume()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		// A live sender's teardown is logged here; a candidate is logged once,
		// with its bounded request summary, from SessionEnded.
		if s.promoted.Load() {
			s.m.logf("closing session from %s: %s", s.peer.Address, reason)
		}
	})
}

// cleanup releases every resource the session holds. It runs once, on the
// session goroutine, after the control connection ended.
func (s *Session) cleanup(reason string) {
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.ended = true
		s.lifecycleMu.Unlock()
		close(s.done)
		s.cancelMasterVolume()
		// The read loop can return on a parse error or protocol refusal without
		// close ever running; the control socket must not outlive the session.
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
	s.stopRing()
	s.discardStagedStreams()
	for _, stream := range s.activeStreams() {
		stream.stop()
	}
	s.eventMu.Lock()
	if s.eventConn != nil {
		_ = s.eventConn.Close()
		s.eventConn = nil
	}
	s.eventOut = nil
	s.eventIn = nil
	if s.eventListener != nil {
		_ = s.eventListener.Close()
		s.eventListener = nil
	}
	s.eventMu.Unlock()
	s.closeAP2Control()
	s.m.SessionEnded(s, reason)
}

func (s *Session) activeStreams() []*audioStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*audioStream(nil), s.streams...)
}

// --- RTSP loop ------------------------------------------------------------

func (s *Session) serveRTSP() error {
	for {
		select {
		case <-s.done:
			return nil
		default:
		}
		interleaved, err := s.readInterleaved()
		if err != nil {
			s.noteReadFailure(err)
			return err
		}
		if interleaved {
			continue
		}
		request, err := rtsp.ReadRequest(s.br)
		if err != nil {
			s.noteReadFailure(err)
			return err
		}
		s.touch()
		s.setUserAgentOnce(request.Get("User-Agent"))
		response, upgrade, err := s.dispatch(request)
		if err != nil {
			// Count the parsed request even when no response was produced, so a
			// connection that dies in dispatch is still visible as a candidate.
			s.noteCandidateRequest(request.Method, request.Path(), 0)
			return err
		}
		if response == nil {
			response = rtsp.NewResponse(request.CSeq(), 200, "")
		}
		starting := s.shouldPromote(request, response)
		if starting {
			// Secure the candidate before it can be evicted or time out while
			// the success response is being written. The live sender keeps
			// playing until commit.
			if err := s.reserveOwnership(); err != nil {
				// The request itself validated, but another candidate holds the
				// single reservation or the receiver is closing. Tell the
				// sender explicitly instead of dropping a would-be success.
				s.noteCandidateRequest(request.Method, request.Path(), 503)
				s.writeRefusal(request, 503, "session ownership is unavailable")
				return err
			}
		}
		// Record before writing: a failed write must not lose the request from
		// the candidate summary.
		s.noteCandidateRequest(request.Method, request.Path(), response.Status)
		if err := s.writeResponse(request, response); err != nil {
			s.noteCandidateWriteFailed()
			if starting {
				s.releaseOwnership()
			}
			return err
		}
		if upgrade != nil {
			if err := s.upgradeChannel(upgrade); err != nil {
				if starting {
					s.releaseOwnership()
				}
				return err
			}
		}
		if starting {
			if err := s.commitOwnership(); err != nil {
				return err
			}
		} else if request.Method == "SETUP" && response.Status == 200 {
			// An already-active sender's SETUP does not take ownership, but its
			// staged streams and group id become live only after the success
			// response. If a takeover happened while the write was in flight,
			// the superseded session must not start that audio.
			if s.m.owns(s) {
				s.applyStagedGroup()
				s.commitStreams()
			} else {
				s.discardStagedStreams()
			}
		}
		if request.Method == "TEARDOWN" || s.closeAfterResponse {
			return errSessionEnded
		}
	}
}

// shouldPromote reports whether a request that just succeeded starts a session.
// OPTIONS, GET /info and any failed request never do: a reachability probe or
// the losing half of a dual-stack connect must not evict a working sender.
// Pairing and pair-verify protect the connection and establish its channel
// keys, but they do not start playback: the reference marks a receiver active
// only when it accepts ANNOUNCE or SETUP. A completed HAP exchange therefore
// stays a candidate until a later accepted ANNOUNCE or SETUP promotes it.
func (s *Session) shouldPromote(request *rtsp.Request, response *rtsp.Response) bool {
	if s.promoted.Load() || response == nil || response.Status != 200 {
		return false
	}
	switch request.Method {
	case "ANNOUNCE":
		return true
	case "SETUP":
		// Only a SETUP that staged a real audio stream (AP2 type 96/103 or
		// legacy Transport) starts media. Event-only and type-130 control
		// SETUPs configure the session but do not take the player.
		s.mu.Lock()
		startsMedia := s.setupStartsMedia
		s.mu.Unlock()
		return startsMedia
	}
	return false
}

// candidateStage projects a request onto a fixed vocabulary so a candidate's
// log line carries a protocol stage rather than a sender-supplied URL.
func candidateStage(method, path string) string {
	switch method {
	case "OPTIONS":
		return "OPTIONS"
	case "GET":
		if path == "/info" {
			return "GET /info"
		}
	case "POST":
		switch path {
		case "/pair-setup":
			return "POST /pair-setup"
		case "/pair-verify":
			return "POST /pair-verify"
		case "/fp-setup":
			return "POST /fp-setup"
		case "/command":
			return "POST /command"
		case "/feedback":
			return "POST /feedback"
		case "/audioMode":
			return "POST /audioMode"
		case "/configure":
			return "POST /configure"
		case "/setProperty":
			return "POST /setProperty"
		case "/pair-pin-start":
			return "POST /pair-pin-start"
		case "/pair-add":
			return "POST /pair-add"
		case "/pair-remove":
			return "POST /pair-remove"
		case "/pair-list":
			return "POST /pair-list"
		}
	case "ANNOUNCE":
		return "ANNOUNCE"
	case "SETUP":
		return "SETUP"
	case "RECORD":
		return "RECORD"
	case "TEARDOWN":
		return "TEARDOWN"
	case "FLUSH":
		return "FLUSH"
	case "FLUSHBUFFERED":
		return "FLUSHBUFFERED"
	case "SET_PARAMETER":
		return "SET_PARAMETER"
	case "GET_PARAMETER":
		return "GET_PARAMETER"
	case "SETRATE":
		return "SETRATE"
	case "SETRATEANCHORTI":
		return "SETRATEANCHORTI"
	case "SETRATEANCHORTIME":
		return "SETRATEANCHORTIME"
	case "SETPEERS":
		return "SETPEERS"
	case "SETPEERSX":
		return "SETPEERSX"
	case "PAUSE":
		return "PAUSE"
	case "PUT":
		return "PUT"
	case "SETMAGICCOOKIE":
		return "SETMAGICCOOKIE"
	}
	return "other"
}

// noteCandidateRequest records a parsed request of a connection. It feeds one
// bounded end-of-connection log line for a connection that never starts a
// session; only the counts, an allowlisted method/path class (candidateStage)
// and statuses are kept, never headers, bodies, query strings or raw errors.
func (s *Session) noteCandidateRequest(method, path string, status int) {
	s.mu.Lock()
	s.candidateRequests++
	stage := candidateStage(method, path)
	s.candidateStage = stage
	s.candidateStatus = status
	if s.encrypted {
		s.postUpgradeRequests++
		if s.postUpgradeRequests == 1 {
			s.postUpgradeFirstStage = stage
			s.postUpgradeFirstStatus = status
		}
		s.postUpgradeLastStage = stage
		s.postUpgradeLastStatus = status
	}
	s.mu.Unlock()
}

// noteReadFailure records the first non-HAP parse failure seen by the RTSP
// reader as a fixed class. It never stores the error text. HAP-layer outcomes
// are reported separately by hapcrypt's optional ReadStats snapshot.
func (s *Session) noteReadFailure(err error) {
	if err == nil || !s.encrypted {
		return
	}
	class := rtspParseClass(err)
	if class == "" {
		return
	}
	// RTSP wraps a body read failure in its own text and may replace the
	// original hapcrypt error with a bare io.ErrUnexpectedEOF. The HAP
	// snapshot still records the root outcome, so do not attribute a
	// non-clean encrypted read to the RTSP parser.
	if _, outcome, ok := s.hapReadSnapshot(); ok && outcome != "none" && outcome != "eof" {
		return
	}
	s.mu.Lock()
	if s.rtspParseClass == "" {
		s.rtspParseClass = class
	}
	s.mu.Unlock()
}

// rtspParseClass maps an RTSP parser error to a fixed token. A clean EOF and a
// hapcrypt framing/authentication error belong to the HAP snapshot, not the
// RTSP parser, so they return "". Wrapping must not hide that layer: RTSP
// wraps a body read error with its own prefix, so the chain is inspected for
// the hapcrypt marker before classifying. A bare io.ErrUnexpectedEOF without a
// HAP error means the parser saw a partial request at a clean encrypted-EOF
// boundary, which is reported as truncated rather than dropped.
func rtspParseClass(err error) string {
	if err == nil || errors.Is(err, io.EOF) {
		return ""
	}
	if errorFromHAP(err) {
		return ""
	}
	text := err.Error()
	if strings.HasPrefix(text, "rtsp:") {
		// Match the parser's fixed prefixes, not the sender text quoted in
		// an error value. In particular, an invalid Content-Length can quote
		// a value containing header phrases; it is still a content-length
		// failure.
		switch {
		case strings.HasPrefix(text, "rtsp: invalid Content-Length"),
			strings.HasPrefix(text, "rtsp: negative Content-Length"),
			strings.HasPrefix(text, "rtsp: Content-Length"):
			return "content-length"
		case strings.HasPrefix(text, "rtsp: malformed request line"):
			return "request-line"
		case strings.HasPrefix(text, "rtsp: unexpected end of headers"),
			strings.HasPrefix(text, "rtsp: folded header line"),
			strings.HasPrefix(text, "rtsp: more than "),
			strings.HasPrefix(text, "rtsp: malformed header line"),
			strings.HasPrefix(text, "rtsp: empty header name"),
			strings.HasPrefix(text, "rtsp: line exceeds "):
			return "headers"
		case strings.HasPrefix(text, "rtsp: truncated body "),
			strings.HasPrefix(text, "rtsp: reading body"):
			return "body"
		case errors.Is(err, io.ErrUnexpectedEOF):
			return "truncated"
		default:
			return "other"
		}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "truncated"
	}
	return ""
}

// errorFromHAP reports whether any error in the unwrap chain came from the
// encrypted transport. The marker is a fixed package prefix, not payload or
// peer-controlled text.
func errorFromHAP(err error) bool {
	for err != nil {
		if strings.HasPrefix(err.Error(), "hapcrypt:") {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// candidateTelemetry is the bounded, payload-free shape the manager logs for a
// connection that ends before it starts a session.
type candidateTelemetry struct {
	requests    int
	stage       string
	status      int
	writeFailed bool
	encrypted   bool
	postUpgrade int
	firstStage  string
	firstStatus int
	lastStage   string
	lastStatus  int
	hapBlocks   uint64
	hapOutcome  string
	rtspParse   string
}

// candidateSummary returns the telemetry for a connection that never became
// the live session. The caller must not hold s.mu. HAP stats are optional: a
// Session built without the encrypted stream reports "unavailable" and zero
// blocks rather than inventing an outcome.
func (s *Session) candidateSummary() candidateTelemetry {
	s.mu.Lock()
	summary := candidateTelemetry{
		requests:    s.candidateRequests,
		stage:       s.candidateStage,
		status:      s.candidateStatus,
		writeFailed: s.candidateWriteFailed,
		encrypted:   s.encrypted,
		postUpgrade: s.postUpgradeRequests,
		firstStage:  s.postUpgradeFirstStage,
		firstStatus: s.postUpgradeFirstStatus,
		lastStage:   s.postUpgradeLastStage,
		lastStatus:  s.postUpgradeLastStatus,
		rtspParse:   s.rtspParseClass,
	}
	s.mu.Unlock()
	blocks, outcome, ok := s.hapReadSnapshot()
	if !ok {
		summary.hapOutcome = "unavailable"
		return summary
	}
	summary.hapBlocks, summary.hapOutcome = blocks, outcome
	return summary
}

// hapReadSnapshot returns the encrypted transport's bounded read telemetry.
// ok is false when the stream is not encrypted or the optional snapshot is
// absent; no outcome is invented in that case.
func (s *Session) hapReadSnapshot() (blocks uint64, outcome string, ok bool) {
	s.mu.Lock()
	secure := s.secure
	s.mu.Unlock()
	if secure == nil {
		return 0, "", false
	}
	stats, ok := any(secure).(hapReadStats)
	if !ok {
		return 0, "", false
	}
	blocks, outcome = stats.ReadStats()
	return blocks, outcome, true
}

// noteCandidateWriteFailed records that a response never reached the peer. The
// serving goroutine calls it, including when a refusal write timed out, so the
// end-of-connection summary does not claim delivery that did not happen.
func (s *Session) noteCandidateWriteFailed() {
	if s.promoted.Load() {
		return
	}
	s.mu.Lock()
	s.candidateWriteFailed = true
	s.mu.Unlock()
}

// readInterleaved consumes one interleaved RTP/RTCP frame when the next byte is
// the RTSP interleaved marker ($). Legacy senders that ask for
// `Transport: RTP/AVP/TCP;interleaved=0-1` carry their audio this way.
func (s *Session) readInterleaved() (bool, error) {
	prefix, err := s.br.Peek(1)
	if err != nil {
		return false, err
	}
	if prefix[0] != '$' {
		return false, nil
	}
	var header [4]byte
	if _, err := io.ReadFull(s.br, header[:]); err != nil {
		return true, err
	}
	channel := int(header[1])
	length := int(binary.BigEndian.Uint16(header[2:4]))
	if length <= 0 {
		return true, fmt.Errorf("interleaved frame with length %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(s.br, payload); err != nil {
		return true, err
	}
	s.touch()
	for _, stream := range s.activeStreams() {
		if stream.spec.interleavedData == channel {
			stream.deliver(payload)
			return true, nil
		}
		if stream.spec.interleavedControl == channel {
			stream.deliverControl(payload)
			return true, nil
		}
	}
	return true, nil
}

func (s *Session) writeResponse(request *rtsp.Request, response *rtsp.Response) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	response.Set("Server", "AirTunes/"+s.m.cfg.SourceVersion)
	if request != nil {
		response.Set("CSeq", request.CSeq())
	}
	return response.Write(s.out)
}

// channelUpgrade derives the control and event keys from a shared secret: the
// SRP session key for a transient pair-setup, or the X25519 secret after
// pair-verify. Keys are stored from the accessory's perspective (read opens
// inbound blocks, write seals outbound ones).
func (s *Session) channelUpgrade(shared []byte) (*channelKeys, error) {
	if len(shared) == 0 {
		return nil, errors.New("no shared secret for the encrypted channels")
	}
	control := hapcrypt.ControlChannelKeys(shared)
	events := hapcrypt.EventChannelKeys(shared)
	s.ctrlKeys = &channelKeys{write: control.Outbound, read: control.Inbound}
	s.evKeys = &channelKeys{write: events.Outbound, read: events.Inbound}
	return s.ctrlKeys, nil
}

// upgradeChannel switches the control connection to the encrypted HAP framing.
// The sender must not pipeline anything past the pair-verify response; buffered
// plaintext would otherwise be misread as ciphertext, so it is an error.
func (s *Session) upgradeChannel(keys *channelKeys) error {
	if keys == nil {
		return nil
	}
	if s.br.Buffered() > 0 {
		return errors.New("client pipelined plaintext past the pairing handshake")
	}
	session, err := hapcrypt.NewSession(s.conn, keys.write, keys.read)
	if err != nil {
		return err
	}
	s.br = bufio.NewReaderSize(session, 16*1024)
	s.out = session
	s.mu.Lock()
	s.secure = session
	s.encrypted = true
	s.mu.Unlock()
	s.m.logf("session from %s: control channel encrypted", s.peer.Address)
	return nil
}
