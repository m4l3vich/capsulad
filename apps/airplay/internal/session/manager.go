package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"capsulad/apps/airplay/internal/aac"
	"capsulad/apps/airplay/internal/airplayinfo"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/raop"
)

// Setting keys the app owns. They hold generated state, not user preferences:
// the HomeKit device identity, the paired-controller records and the RSA key
// that the legacy RAOP path publishes.
const (
	SettingDeviceIdentity = "device_identity"
	SettingPairings       = "pairings"
	SettingRAOPKey        = "raop_key"
)

// Config is the receiver configuration derived from the app's settings.
type Config struct {
	// DeviceID is the 12-hex-digit AirPlay device id (the RAOP name prefix).
	DeviceID string
	// Name is the advertised display name.
	Name string
	// Model is the airplay model string published in mDNS and /info.
	Model string
	// Port is the requested RTSP port; 0 asks the daemon for an ephemeral one.
	Port int
	// FrameTarget is the jitter cushion kept before playout.
	FrameTarget time.Duration
	// RingEnabled drives the cover-art ring presentation while a session is live.
	RingEnabled bool
	// MasterVolumeContext reads the framework mixer's master level (0..1) under
	// the caller's deadline. The receiver reports this one level in /info,
	// updateInfo, GET_PARAMETER and the dvlc notification; it never keeps a
	// second session gain. There is deliberately no unbounded getter form.
	MasterVolumeContext func(context.Context) (float64, bool)
	// SetMasterVolume writes the persisted framework master (0..1) and returns
	// the level the framework applied. The session only calls it for the
	// current HAP-keyed media owner; a candidate or legacy sender never changes
	// the device level. The context bounds the framework call; the daemon also
	// rechecks it immediately before the durable write, so a cancelled result
	// must never be treated as applied.
	SetMasterVolume func(context.Context, float64) (float64, error)
	// PIN is the HomeKit setup code answered for transient and normal
	// pair-setup. Empty means the upstream default ("3939").
	PIN string
	// SourceVersion is the AirPlay srcvers advertised to senders.
	SourceVersion string
	// Features overrides the advertised feature bit set; 0 selects the default.
	Features uint64
	// Logf receives one line per notable event.
	Logf func(string, ...any)
}

// Advertiser publishes mDNS services through the daemon's responder.
type Advertiser interface {
	Publish(Advert) (string, error)
	Unpublish(id string) error
}

// Advert is one advertisement.
type Advert struct {
	Type     string
	Instance string
	Port     int
	TXT      map[string]string
}

// RingFactory opens the ring presentation.
type RingFactory interface {
	OpenRing() (Ring, error)
}

// SinkFactory opens a framework playback stream.
type SinkFactory interface {
	OpenSink(rate, channels int) (Sink, error)
}

// Persist writes one app setting through the compare-and-swap configuration API.
type Persist func(key, value string) error

// PTPFactory opens a PTP clock when the platform supports multicast reception.
// It returns nil (and no error) when PTP is unavailable; the session then runs
// on arrival-time playout.
type PTPFactory func() Clock

// maxStreamsPerSession bounds the audio channels one sender may create. Every
// stream costs daemon listeners and goroutines, and a session can send SETUP
// repeatedly; four is more than any AirPlay sender needs (main plus an
// alternate stream) and keeps the daemon's per-run listener budget intact.
const maxStreamsPerSession = 4

// maxPendingSessions bounds connections that have not yet proved themselves
// with a session-initiating request. A dual-stack sender can leave a losing
// TCP connection open briefly (and a scanner can leave one open for longer),
// so the receiver accepts a few candidates but never an unbounded number.
const maxPendingSessions = 4

// candidateRequestTimeout bounds how long a connection may stay pending
// without sending a request that starts a session. The candidate is closed
// without touching the live session, the player or the advertisements. A real
// sender sends OPTIONS/GET /info and its pairing or SETUP within moments; the
// bound only catches silent connections.
var candidateRequestTimeout = 30 * time.Second

// candidateLogInterval bounds how often a candidate summary is logged across
// the whole receiver, so a burst of connections cannot turn diagnostics into
// log spam. The first summary after each window is emitted.
const candidateLogInterval = 5 * time.Second

// reservationTimeout bounds how long a reserved candidate may hold the sole
// reservation while its success response is being written. The transport's
// writes are not cancellable by a deadline, so a stalled peer could otherwise
// block every later session start; expiry closes the candidate and leaves the
// live sender untouched.
var reservationTimeout = 5 * time.Second

// Manager owns the receiver: one RTSP listener, the mDNS advertisements and at
// most one live sender session.
type Manager struct {
	cfg        Config
	transport  Transport
	advertiser Advertiser
	rings      RingFactory
	sinks      SinkFactory
	board      *media.Board
	persist    Persist
	logf       func(string, ...any)
	ptp        PTPFactory
	clockOnce  sync.Once
	clock      Clock

	store *pair.Store
	raop  *raop.KeyPair

	// volumeOpMu serializes writes of the framework master with the ownership
	// transitions that authorize them (commit, session end, receiver close).
	// It is deliberately separate from mu and from reads: a framework write
	// never runs under the broad receiver lock, an ownership check made before
	// the call stays valid until it returns, and a slow getter can never hold a
	// transition. Nothing here is held while a playback stream or the RTSP loop
	// runs.
	volumeOpMu sync.Mutex
	// masterMu protects the authoritative last-known master level. It is a
	// cache for replies and for the slow-read fallback, not the per-sender
	// display state. masterWriteSeq increments only on an applied write, so a
	// read that was already in flight when a newer write landed cannot clobber
	// that write's result.
	masterMu       sync.Mutex
	masterUnit     float64
	masterKnown    bool
	masterWriteSeq uint64

	// advMu serializes advertisement mutations. Session start/end, a group-id
	// update and the Re-advertise action can all request a republish
	// concurrently; without this lock two calls can withdraw each other's
	// records and then both AddService the same instance, which Avahi reports
	// as a collision. It also makes Close wait for an in-flight publish before
	// it withdraws, so a late publish cannot leave a live record after stop.
	advMu          sync.Mutex
	mu             sync.Mutex
	listener       Listener
	active         *Session
	pending        []*Session // accepted but not yet promoted, oldest first
	reserved       *Session   // candidate that may send a success response but has not committed
	candidateLogAt time.Time  // last receiver-wide candidate summary, under mu
	airplay        string     // mDNS publication id for _airplay._tcp
	raopID         string     // mDNS publication id for _raop._tcp
	groupID        string     // sender-supplied AirPlay group UUID, published as gid
	sessionActive  bool
	closed         bool
	acceptWG       sync.WaitGroup
	sessionWG      sync.WaitGroup
}

// NewManager builds the receiver. identityValues maps the app's settings keys
// to their raw JSON values; missing state is generated and persisted through
// persist (which may be nil in tests, where identity then lives for one run).
func NewManager(
	cfg Config,
	transport Transport,
	advertiser Advertiser,
	rings RingFactory,
	sinks SinkFactory,
	board *media.Board,
	persist Persist,
	ptp PTPFactory,
	identityValues map[string]string,
) (*Manager, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.FrameTarget <= 0 {
		cfg.FrameTarget = 1500 * time.Millisecond
	}
	if cfg.SourceVersion == "" {
		cfg.SourceVersion = DefaultSourceVersion
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.PIN == "" {
		cfg.PIN = pair.DefaultPIN
	}
	if cfg.Features == 0 {
		cfg.Features = DefaultFeatures
		if aac.Available() {
			cfg.Features |= FeatureReceiveAudioAACLC
		}
	}
	// An override narrows or expands protocol policy; it cannot manufacture a
	// decoder. Keep the encrypted buffered claim only when at least one of the
	// corresponding /info formats is actually enabled.
	if !aac.Available() {
		cfg.Features &^= FeatureReceiveAudioAACLC
	}
	if bufferedStreamFormats(cfg.Features) == 0 {
		cfg.Features &^= FeatureBufferedAudio
	}
	m := &Manager{
		cfg: cfg, transport: transport, advertiser: advertiser, rings: rings, sinks: sinks,
		board: board, persist: persist, logf: cfg.Logf, ptp: ptp,
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = randomDeviceID()
		m.cfg.DeviceID = cfg.DeviceID
	}
	m.cfg.Name = strings.TrimSpace(cfg.Name)

	store, created, err := loadOrCreateStore(identityValues[SettingDeviceIdentity], identityValues[SettingPairings])
	if err != nil {
		return nil, err
	}
	m.store = store

	if created && persist != nil {
		if err := persist(SettingDeviceIdentity, encodeDevice(store)); err != nil {
			return nil, err
		}
	}
	if persist != nil && identityValues[SettingPairings] == "" && store.ControllerCount() > 0 {
		// A store written by the combined format is migrated into the pairings
		// setting once, so the two documented settings are authoritative.
		if err := persist(SettingPairings, encodeControllers(store)); err != nil {
			return nil, err
		}
	}
	key, keyCreated, err := loadOrCreateRAOPKey(identityValues[SettingRAOPKey])
	if err != nil {
		return nil, err
	}
	m.raop = key
	if keyCreated && persist != nil {
		if err := persist(SettingRAOPKey, base64.StdEncoding.EncodeToString(key.PrivateKey)); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// persistControllers writes the controller records after pair-setup or one of
// the pairing administration requests changed them.
func (m *Manager) persistControllers() error {
	if m.persist == nil {
		return nil
	}
	return m.persist(SettingPairings, encodeControllers(m.store))
}

// persistDevice writes the accessory identity (normally only on first run).
func (m *Manager) persistDevice() error {
	if m.persist == nil {
		return nil
	}
	return m.persist(SettingDeviceIdentity, encodeDevice(m.store))
}

// Store returns the device identity store (used by tests and diagnostics).
func (m *Manager) Store() *pair.Store { return m.store }

// DeviceID is the advertised AirPlay device id.
func (m *Manager) DeviceID() string { return m.cfg.DeviceID }

// Start opens the RTSP listener, publishes the advertisements and begins
// accepting senders. It returns once the receiver is listening.
func (m *Manager) Start() error {
	port, listener, err := m.openRTSPListener()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.listener = listener
	m.mu.Unlock()
	if m.advertiser != nil {
		if err := m.publish(port); err != nil {
			m.logf("mDNS: %v", err)
		}
	}
	m.acceptWG.Add(1)
	go m.acceptLoop()
	m.logf("AirPlay receiver %q listening on RTSP port %d (device id %s)", m.cfg.Name, port, m.cfg.DeviceID)
	return nil
}

// openRTSPListener asks the daemon for the requested port and falls back to an
// ephemeral port when the request is refused (a port already in use, or a
// policy refusal). It never binds a socket itself.
func (m *Manager) openRTSPListener() (int, Listener, error) {
	if m.cfg.Port > 0 {
		listener, err := m.transport.OpenTCPListener("0.0.0.0", m.cfg.Port)
		if err == nil {
			_, port := listener.Addr()
			return port, listener, nil
		}
		m.logf("requested RTSP port %d refused (%v); asking for an ephemeral port", m.cfg.Port, err)
	}
	listener, err := m.transport.OpenTCPListener("0.0.0.0", 0)
	if err != nil {
		return 0, nil, fmt.Errorf("open RTSP listener: %w", err)
	}
	_, port := listener.Addr()
	if port == 0 {
		listener.Close()
		return 0, nil, errors.New("daemon returned an RTSP listener with no port")
	}
	return port, listener, nil
}

func (m *Manager) acceptLoop() {
	defer m.acceptWG.Done()
	for {
		m.mu.Lock()
		listener := m.listener
		closed := m.closed
		m.mu.Unlock()
		if closed || listener == nil {
			return
		}
		stream, err := listener.Accept()
		if err != nil {
			m.mu.Lock()
			closed = m.closed
			m.mu.Unlock()
			if closed {
				return
			}
			m.logf("accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		m.startSession(stream)
	}
}

// startSession accepts a new connection as a candidate. A TCP accept alone
// proves nothing: a dual-stack sender races IPv4 and IPv6 connections and
// closes the loser immediately, and a port probe does the same. Pairing and
// pair-verify key the connection but do not start media; the candidate only
// becomes the live session after an accepted ANNOUNCE or eligible SETUP. Until
// then it cannot disturb the live sender, the player or the advertisements.
func (m *Manager) startSession(stream Stream) {
	s := newSession(m, stream)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = stream.Close()
		return
	}
	var evicted *Session
	if len(m.pending) >= maxPendingSessions {
		evicted = m.pending[0]
		m.pending = append([]*Session(nil), m.pending[1:]...)
	}
	m.pending = append(m.pending, s)
	m.mu.Unlock()
	if evicted != nil {
		evicted.close("too many connections without a session request")
	}
	// The session runs beside the accept loop so another candidate can still
	// arrive and a promoted sender can take the session over.
	m.sessionWG.Add(1)
	go func() {
		defer m.sessionWG.Done()
		s.run()
	}()
}

// reserve protects a candidate that is about to be told its session-starting
// request succeeded. It is removed from the evictable pending set and cannot be
// closed by the candidate timeout, but it does not own the player and the old
// sender keeps playing until commit. A refused SETUP never reserves.
func (m *Manager) reserve(s *Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == s {
		return true
	}
	if m.closed || m.reserved != nil {
		return false
	}
	if !m.removePendingLocked(s) {
		return false
	}
	if !s.reserve() {
		return false
	}
	m.reserved = s
	return true
}

// commit turns a reserved candidate into the live session after its success
// response and channel upgrade completed. The manager lock is held while begin
// starts the player, so no concurrent EndSession/Close can close either session
// between the ownership decision and the player generation. volumeOpMu is held
// across the same transition so an in-flight master write from the old owner
// cannot land after the new owner exists.
func (m *Manager) commit(s *Session) bool {
	// An in-flight master write holds volumeOpMu across its ownership check
	// and framework call. Taking it here means a takeover cannot interleave
	// with that call: the old write finishes before the new owner exists, and
	// every later old-session write sees owns=false.
	m.volumeOpMu.Lock()
	defer m.volumeOpMu.Unlock()
	m.mu.Lock()
	if m.reserved != s {
		m.mu.Unlock()
		return false
	}
	m.reserved = nil
	if m.closed || !s.begin() {
		m.mu.Unlock()
		return false
	}
	previous := m.active
	first := !m.sessionActive
	m.active = s
	m.sessionActive = true
	// A candidate's group id is staged until it owns the receiver, so a
	// refused SETUP or a losing candidate can never overwrite the live gid.
	// A different owner with no gid clears the previous owner's group; the
	// same owner's SETUP with no gid is handled by applyStagedGroup and leaves
	// the value alone.
	gid := ""
	if s.stagedGIDSet {
		gid = s.stagedGID
	}
	changed := m.groupID != gid
	m.groupID = gid
	m.mu.Unlock()

	if previous != nil {
		m.logf("new sender from %s takes over the live session", s.peer.Address)
		previous.close("superseded by a new sender")
	}
	s.commitStreams()
	if first || changed {
		go m.republishStatus()
	}
	return true
}

// releaseReservation drops a reservation whose success response or channel
// upgrade failed. The candidate is about to close; the live sender is intact.
func (m *Manager) releaseReservation(s *Session) {
	m.mu.Lock()
	if m.reserved == s {
		m.reserved = nil
		s.release()
	}
	m.mu.Unlock()
}

// owns reports whether s is still the live session.
func (m *Manager) owns(s *Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active == s
}

// removePendingLocked removes s from the candidate list. The caller holds m.mu.
func (m *Manager) removePendingLocked(s *Session) bool {
	for i, candidate := range m.pending {
		if candidate != s {
			continue
		}
		m.pending = append(m.pending[:i], m.pending[i+1:]...)
		return true
	}
	return false
}

// SessionEnded is called by a session when it finishes, whatever the reason.
func (m *Manager) SessionEnded(s *Session, reason string) {
	// Ending a session is an ownership transition. An in-flight master write
	// from this sender completes before the transition, and every write that
	// arrives afterwards fails the owns check.
	m.volumeOpMu.Lock()
	m.mu.Lock()
	if m.reserved == s {
		m.reserved = nil
		s.release()
	}
	if m.active != s {
		// A candidate that never started a session must not withdraw
		// advertisements, end the player or emit a session-end line. It leaves
		// one bounded summary line instead, only when it actually sent a
		// request, only for a connection that never promoted (a superseded
		// session already logged its own close), never with the raw end error
		// and at most once per receiver-wide interval.
		m.removePendingLocked(s)
		m.mu.Unlock()
		m.volumeOpMu.Unlock()
		if !s.promoted.Load() {
			if summary := s.candidateSummary(); summary.requests > 0 && m.allowCandidateLog() {
				suffix := ""
				if summary.writeFailed {
					suffix = ", last write failed"
				}
				line := fmt.Sprintf("candidate from %s ended before starting a session after %s: %d request(s), last %s -> %s%s",
					s.peer.Address, time.Since(s.started).Round(time.Millisecond), summary.requests,
					candidateStageToken(summary.stage), candidateStatusToken(summary.status), suffix)
				if summary.encrypted {
					line += fmt.Sprintf(", post-upgrade %d, first %s -> %s, last %s -> %s, hap=%s/%d, rtsp=%s",
						summary.postUpgrade, candidateStageToken(summary.firstStage),
						candidateStatusToken(summary.firstStatus), candidateStageToken(summary.lastStage),
						candidateStatusToken(summary.lastStatus), summary.hapOutcome,
						summary.hapBlocks, candidateParseToken(summary.rtspParse))
				}
				m.logf("%s", line)
			}
		}
		return
	}
	m.active = nil
	wasActive := m.sessionActive
	m.sessionActive = false
	m.groupID = ""
	m.mu.Unlock()
	m.volumeOpMu.Unlock()
	if wasActive {
		go m.republishStatus()
	}
	if m.board != nil {
		if gen := s.boardGeneration(); gen != 0 {
			m.board.SessionEnded(gen)
		}
	}
	m.logf("AirPlay session ended: %s", reason)
}

// allowCandidateLog reports whether a candidate summary may be written now. It
// is receiver-wide: the first summary after each interval is emitted and the
// rest are dropped, so connection churn cannot flood the log. The caller must
// not hold m.mu.
func (m *Manager) allowCandidateLog() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.candidateLogAt.IsZero() && time.Since(m.candidateLogAt) < candidateLogInterval {
		return false
	}
	m.candidateLogAt = time.Now()
	return true
}

// Close stops the receiver: no new senders, the live session is torn down and
// the advertisements are withdrawn.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	listener := m.listener
	active := m.active
	reserved := m.reserved
	m.reserved = nil
	pending := append([]*Session(nil), m.pending...)
	m.pending = nil
	m.mu.Unlock()

	// Signal cancellation to every session before waiting for an in-flight
	// master write. The setter's context is derived from the session, so this
	// wakes a daemon-queued call instead of letting Close block behind it.
	if active != nil {
		active.close("receiver shutting down")
	}
	if reserved != nil && reserved != active {
		reserved.close("receiver shutting down")
	}
	for _, candidate := range pending {
		candidate.close("receiver shutting down")
	}
	// Now drain the narrow write scope. No new write can pass canWriteMaster
	// because closed is set; this wait is bounded by the setter's context.
	m.volumeOpMu.Lock()
	m.volumeOpMu.Unlock()
	if listener != nil {
		if err := listener.Close(); err != nil {
			m.logf("closing the RTSP listener: %v", err)
		}
	}
	if m.advertiser != nil {
		// Wait for a publish that started before closed was set, then remove
		// the ids it installed. A publish that starts after closed is set
		// observes it under advMu and makes no responder call.
		m.advMu.Lock()
		m.mu.Lock()
		airplayAdv, raopAdv := m.airplay, m.raopID
		m.airplay, m.raopID = "", ""
		m.mu.Unlock()
		if airplayAdv != "" {
			if err := m.advertiser.Unpublish(airplayAdv); err != nil {
				m.logf("withdrawing _airplay._tcp: %v", err)
			}
		}
		if raopAdv != "" {
			if err := m.advertiser.Unpublish(raopAdv); err != nil {
				m.logf("withdrawing _raop._tcp: %v", err)
			}
		}
		m.advMu.Unlock()
	}
	m.acceptWG.Wait()
	m.sessionWG.Wait()
	return nil
}

// MediaSnapshot returns the canonical JSON player status for ListPlayers. The
// player exists only while a sender is connected; an idle receiver offers no
// player at all.
func (m *Manager) MediaSnapshot() map[string]any {
	if !m.board.Live() {
		return map[string]any{"players": []any{}}
	}
	return map[string]any{"players": []any{m.board.Snapshot()}}
}

// systemPairingID is the `psi` identity derived from the device id. It is
// published in both mDNS and /info so a sender sees one identity.
func (m *Manager) systemPairingID() string {
	psi, err := airplayinfo.DefaultSystemPairingID(m.cfg.DeviceID)
	if err != nil {
		return ""
	}
	return psi
}

// advertisedFeatures is the feature mask every advertisement uses: the
// configured set plus the PTP bits only when a clock is actually available and
// consumed by the playout path.
func (m *Manager) advertisedFeatures() uint64 {
	features := m.cfg.Features
	if bufferedStreamFormats(features) == 0 {
		features &^= FeatureBufferedAudio
	}
	if m.ptpClock() != nil {
		features |= 1<<41 | 1<<47
	}
	return features
}

// ptpClock opens the PTP clock once and returns it for the session's playout.
// A nil clock means the runtime has no multicast listener; the session then
// paces from packet arrival.
func (m *Manager) ptpClock() Clock {
	m.clockOnce.Do(func() {
		if m.ptp != nil {
			m.clock = m.ptp()
		}
	})
	return m.clock
}

// setGroupID records the live sender's AirPlay group UUID. Upstream records and
// advertises it (gid/gcgl) without synchronising playback across the group, and
// so does this receiver; the advertisement is refreshed on the next publish.
// The owner check keeps a request that started on the previous session from
// applying its group id after a takeover; a candidate stages it instead.
func (m *Manager) setGroupID(s *Session, gid string) {
	m.mu.Lock()
	if m.active != s {
		m.mu.Unlock()
		return
	}
	changed := m.groupID != gid
	m.groupID = gid
	m.mu.Unlock()
	if changed {
		go m.republishStatus()
	}
}

// Active returns the live session, if any.
func (m *Manager) Active() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// EndSession disconnects the live sender, if any. It is the "End the current
// session" action; the receiver stays advertised.
func (m *Manager) EndSession(reason string) bool {
	s := m.Active()
	if s == nil {
		return false
	}
	s.close(reason)
	return true
}

// Republish withdraws and re-publishes the advertisements, which is the
// "Re-advertise" action after a sender cached a stale record. A live session
// is not interrupted.
func (m *Manager) Republish() bool {
	m.mu.Lock()
	listener := m.listener
	closed := m.closed
	m.mu.Unlock()
	if closed || listener == nil || m.advertiser == nil {
		return false
	}
	_, port := listener.Addr()
	if err := m.publish(port); err != nil {
		m.logf("re-advertising: %v", err)
		return false
	}
	return true
}

// Command forwards a media-contract action to the live sender. It reports
// whether the command was accepted; the actual playback change arrives later
// through the sender's own status.
func (m *Manager) Command(action string) (bool, string) {
	s := m.Active()
	if s == nil {
		return false, "no sender is connected"
	}
	return s.sendRemoteCommand(action)
}

// --- identity state --------------------------------------------------------

// loadOrCreateStore loads the accessory identity from device_identity and the
// controller records from pairings. A device_identity written by the earlier
// combined format is still accepted, including its controllers, which are then
// migrated by the caller.
func loadOrCreateStore(deviceRaw, controllersRaw string) (*pair.Store, bool, error) {
	store, created := (*pair.Store)(nil), false
	if deviceRaw != "" {
		if blob, err := base64.StdEncoding.DecodeString(deviceRaw); err == nil {
			// The device_identity setting holds the flat device document written
			// by MarshalDevice. An earlier combined document (device plus
			// controllers) is still accepted so a store from that format keeps
			// its identity instead of silently re-pairing every sender.
			loaded := &pair.Store{}
			if err := loaded.LoadDevice(blob); err == nil {
				store = loaded
			} else if combined, err := pair.LoadStore(blob); err == nil {
				store = combined
			}
		}
	}
	if store == nil {
		fresh, err := pair.NewStore(randomPairingID())
		if err != nil {
			return nil, false, err
		}
		store, created = fresh, true
	}
	if controllersRaw != "" {
		if blob, err := base64.StdEncoding.DecodeString(controllersRaw); err == nil {
			if err := store.LoadControllers(blob); err == nil {
				return store, created, nil
			}
		}
	}
	return store, created, nil
}

func encodeDevice(store *pair.Store) string {
	blob, err := store.MarshalDevice()
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(blob)
}

func encodeControllers(store *pair.Store) string {
	blob, err := store.MarshalControllers()
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(blob)
}

func loadOrCreateRAOPKey(raw string) (*raop.KeyPair, bool, error) {
	if raw != "" {
		der, err := base64.StdEncoding.DecodeString(raw)
		if err == nil {
			if key, err := raop.LoadKeyPair(der); err == nil {
				return key, false, nil
			}
		}
	}
	key, err := raop.GenerateKeyPair()
	if err != nil {
		return nil, false, err
	}
	return key, true, nil
}

func randomPairingID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "airplay-receiver"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func randomDeviceID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "020000000001"
	}
	buf[0] = (buf[0] | 0x02) & 0xfe // locally administered, unicast
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 12)
	for i, b := range buf {
		out[i*2] = hexDigits[b>>4]
		out[i*2+1] = hexDigits[b&0x0f]
	}
	return string(out)
}

// decodeStringSetting decodes a JSON string setting value.
func decodeStringSetting(raw string) string {
	if raw == "" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ""
	}
	return s
}

// candidateStageToken renders an allowlisted stage for the bounded candidate
// summary; an empty stage means the request was never parsed.
func candidateStageToken(stage string) string {
	if stage == "" {
		return "none"
	}
	return stage
}

// candidateStatusToken renders a status without inventing one for a request
// that never produced a response.
func candidateStatusToken(status int) string {
	if status == 0 {
		return "none"
	}
	return fmt.Sprintf("%d", status)
}

// candidateParseToken renders the fixed RTSP parse class or "none".
func candidateParseToken(class string) string {
	if class == "" {
		return "none"
	}
	return class
}
