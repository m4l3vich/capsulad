package session

import (
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
	// VolumeTrim is an extra linear trim applied after the sender volume.
	VolumeTrim float64
	// MasterVolume reads the framework mixer's master level (0..1) and reports
	// whether it is available. The app only reads it: the master is a ceiling,
	// and the level is forwarded to the sender on the event channel.
	MasterVolume func() (float64, bool)
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

	mu            sync.Mutex
	listener      Listener
	active        *Session
	airplay       string // mDNS publication id for _airplay._tcp
	raopID        string // mDNS publication id for _raop._tcp
	groupID       string // sender-supplied AirPlay group UUID, published as gid
	sessionActive bool
	closed        bool
	acceptWG      sync.WaitGroup
	sessionWG     sync.WaitGroup
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
	if cfg.VolumeTrim <= 0 {
		cfg.VolumeTrim = 1
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
		if err := m.publish(port, false); err != nil {
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

// startSession takes ownership of a new RTSP connection, replacing any live
// session: a second sender wins, exactly like the upstream receiver where the
// newest connection becomes the principal one.
func (m *Manager) startSession(stream Stream) {
	s := newSession(m, stream)
	m.mu.Lock()
	previous := m.active
	m.active = s
	if !m.sessionActive {
		m.sessionActive = true
		go m.republishStatus(true)
	}
	m.mu.Unlock()
	if previous != nil {
		m.logf("new sender from %s takes over the live session", stream.Peer().Address)
		previous.close("superseded by a new sender")
	}
	// The session runs beside the accept loop so a second sender can still be
	// accepted and take the session over.
	m.sessionWG.Add(1)
	go func() {
		defer m.sessionWG.Done()
		s.run()
	}()
}

// SessionEnded is called by a session when it finishes, whatever the reason.
func (m *Manager) SessionEnded(s *Session, reason string) {
	m.mu.Lock()
	if m.active != s {
		m.mu.Unlock()
		return
	}
	m.active = nil
	wasActive := m.sessionActive
	m.sessionActive = false
	m.groupID = ""
	m.mu.Unlock()
	if wasActive {
		go m.republishStatus(false)
	}
	m.board.SessionEnded(s.boardGen)
	m.logf("AirPlay session ended: %s", reason)
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
	airplayAdv, raopAdv := m.airplay, m.raopID
	m.airplay, m.raopID = "", ""
	m.mu.Unlock()

	if active != nil {
		active.close("receiver shutting down")
	}
	if listener != nil {
		if err := listener.Close(); err != nil {
			m.logf("closing the RTSP listener: %v", err)
		}
	}
	if m.advertiser != nil {
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

// setGroupID records the sender's AirPlay group UUID. Upstream records and
// advertises it (gid/gcgl) without synchronising playback across the group, and
// so does this receiver; the advertisement is refreshed on the next publish.
func (m *Manager) setGroupID(gid string) {
	m.mu.Lock()
	changed := m.groupID != gid
	m.groupID = gid
	m.mu.Unlock()
	if changed {
		go m.republishStatus(true)
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
	active := m.sessionActive
	closed := m.closed
	m.mu.Unlock()
	if closed || listener == nil || m.advertiser == nil {
		return false
	}
	_, port := listener.Addr()
	if err := m.publish(port, active); err != nil {
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
