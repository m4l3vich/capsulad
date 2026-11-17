package tuner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"capsulad/apps/radio/tuner/internal/stream"
	"capsulad/sdk/capsulad"
)

const (
	// ServiceName is served by this app.
	ServiceName = "org.capsulad.radio.v1.RadioTunerService"
	// PlayerService is served by org.capsulad.radio.
	PlayerService = "org.capsulad.radio.v1.RadioPlayerService"
	// DefaultPlayerApp is the app id used when the tuner's setting is empty.
	DefaultPlayerApp = "org.capsulad.radio"

	chunkMS         = 200
	pushTimeout     = 10 * time.Second
	tuneOpenTimeout = 12 * time.Second
	// authTimeout stays well under the radio's 2 s Stop budget: the check is
	// fail-closed, so a congested daemon must not make a legitimate Stop
	// unacknowledged.
	authTimeout     = 1500 * time.Millisecond
	maxReconnects   = 3
	reconnectWindow = 30 * time.Second
	finalPushBudget = 3 * time.Second
)

// Manager serves Tune/Stop and owns the one live session.
type Manager struct {
	ctx    context.Context
	client *capsulad.Client
	cfg    stream.Config

	pushTimeout     time.Duration
	tuneOpenTimeout time.Duration
	authTimeout     time.Duration

	// listAppInterfaces is a test seam; nil uses the daemon-backed lookup.
	listAppInterfaces func(context.Context) ([]appInterface, error)

	mu  sync.Mutex
	cur *session

	settingsMu sync.Mutex
	playerApp  string
}

// NewManager returns a tuner bound to client. The manager's context is the
// app lifetime; cancelling it stops any live session.
func NewManager(ctx context.Context, client *capsulad.Client, env capsulad.Env) *Manager {
	return &Manager{
		ctx:             ctx,
		client:          client,
		cfg:             stream.Config{Env: env},
		pushTimeout:     pushTimeout,
		tuneOpenTimeout: tuneOpenTimeout,
		authTimeout:     authTimeout,
		playerApp:       DefaultPlayerApp,
	}
}

// PlayerApp returns the currently configured allowed player app id.
func (m *Manager) PlayerApp() string {
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()
	return m.playerApp
}

// ApplySettings applies the app-private settings snapshot. Only player_app is
// interpreted; an invalid value keeps the previous one and is logged.
func (m *Manager) ApplySettings(event *capsulad.SettingsChanged) {
	if event == nil {
		return
	}
	for _, value := range event.Values {
		if value.Key != "player_app" {
			continue
		}
		var appID string
		if err := json.Unmarshal([]byte(value.ValueJSON), &appID); err != nil {
			log.Printf("radio-tuner: player_app setting is not a JSON string: %v", err)
			return
		}
		appID = strings.TrimSpace(appID)
		if appID == "" {
			appID = DefaultPlayerApp
		}
		if !validAppID(appID) {
			log.Printf("radio-tuner: player_app %q is not a valid app id; keeping %s", appID, m.PlayerApp())
			return
		}
		m.settingsMu.Lock()
		previous := m.playerApp
		m.playerApp = appID
		m.settingsMu.Unlock()
		if previous != appID {
			log.Printf("radio-tuner: player app set to %s", appID)
		}
	}
}

// Close stops the live session. The manager context should already be
// cancelled by the app's signal handler.
func (m *Manager) Close() {
	m.mu.Lock()
	s := m.cur
	m.cur = nil
	m.mu.Unlock()
	if s != nil {
		s.stop()
		s.wait(2 * time.Second)
	}
}

// ServeTune is the app bus handler for RadioTunerService.Tune.
func (m *Manager) ServeTune(ctx context.Context, call *capsulad.ServerCall) {
	request, err := receiveOne(call)
	if err != nil {
		_ = call.End(3, "invalid Tune request")
		return
	}
	req, err := DecodeTuneRequest(request)
	if err != nil {
		_ = call.End(3, "malformed TuneRequest")
		return
	}
	if !m.authorizeCall(call) {
		return
	}
	result := m.Tune(ctx, req)
	if err := call.Send(result.Marshal()); err != nil {
		// The caller never learned the session id, so no push can be
		// accepted; drop what we started instead of leaking a session.
		m.Stop(StopRequest{SessionID: req.SessionID, Detail: "TuneResult send failed"})
		_ = call.End(13, "send TuneResult: "+err.Error())
		return
	}
	_ = call.End(0, "")
}

// ServeStop is the app bus handler for RadioTunerService.Stop.
func (m *Manager) ServeStop(_ context.Context, call *capsulad.ServerCall) {
	request, err := receiveOne(call)
	if err != nil {
		_ = call.End(3, "invalid Stop request")
		return
	}
	req, err := DecodeStopRequest(request)
	if err != nil {
		_ = call.End(3, "malformed StopRequest")
		return
	}
	if !m.authorizeCall(call) {
		return
	}
	result := m.Stop(req)
	if err := call.Send(result.Marshal()); err != nil {
		_ = call.End(13, "send StopResult: "+err.Error())
		return
	}
	_ = call.End(0, "")
}

// Stop drops the session whose id matches; an empty id drops the current
// session. Report accepted only when a session was actually cancelled.
func (m *Manager) Stop(req StopRequest) StopResult {
	m.mu.Lock()
	s := m.cur
	if s == nil {
		m.mu.Unlock()
		return StopResult{Detail: "no active session"}
	}
	if req.SessionID != "" && req.SessionID != s.id {
		m.mu.Unlock()
		return StopResult{Detail: "session is not active"}
	}
	m.cur = nil
	m.mu.Unlock()
	s.stop()
	log.Printf("radio-tuner: session %s stopped (%s)", s.id, strings.TrimSpace(req.Detail))
	return StopResult{Accepted: true, Detail: "stopped"}
}

// Tune opens the stream and returns once the first MP3 frame decoded. A newer
// Tune supersedes an in-flight one by cancelling it; the superseded Tune then
// reports a refusal instead of starting a second session.
func (m *Manager) Tune(_ context.Context, req TuneRequest) TuneResult {
	result := TuneResult{SessionID: req.SessionID}
	if detail := m.validate(req); detail != "" {
		result.Detail = detail
		return result
	}
	if m.ctx.Err() != nil {
		result.Detail = "tuner is shutting down"
		return result
	}

	s := newSession(m, req)
	m.mu.Lock()
	previous := m.cur
	m.cur = s
	m.mu.Unlock()
	if previous != nil {
		previous.stop()
	}

	openCtx, cancel := context.WithTimeout(s.ctx, m.tuneOpenTimeout)
	defer cancel()

	st, dec, err := s.open(openCtx)
	if err != nil {
		s.stop()
		m.clearCurrent(s)
		switch {
		case s.ctx.Err() != nil:
			result.Detail = "Tune was superseded or stopped"
		case errors.Is(err, context.DeadlineExceeded):
			result.Detail = "timed out opening the stream"
		default:
			result.Detail = err.Error()
		}
		log.Printf("radio-tuner: tune %s station=%q failed: %s", req.SessionID, req.StationName, result.Detail)
		return result
	}
	// Install and start under the manager lock so a newer Tune cannot slip
	// between the current check and the goroutine launch.
	m.mu.Lock()
	if m.cur != s {
		m.mu.Unlock()
		_ = st.Close()
		s.stop()
		result.Detail = "superseded by a newer Tune"
		log.Printf("radio-tuner: tune %s station=%q superseded before playback", req.SessionID, req.StationName)
		return result
	}
	st.SetContext(s.ctx)
	s.start(st, dec)
	m.mu.Unlock()

	result.Accepted = true
	result.ContentType = "audio/mpeg"
	log.Printf("radio-tuner: session %s accepted station=%q url=%s codec=%s", s.id, req.StationName, redact(req.URL), st.Codec())
	return result
}

// isCurrent reports whether s is still the live session.
func (m *Manager) isCurrent(s *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur == s
}

func (m *Manager) validate(req TuneRequest) string {
	if strings.TrimSpace(req.SessionID) == "" {
		return "session_id is required"
	}
	if len(req.SessionID) > 256 {
		return "session_id is too long"
	}
	if hasControl(req.SessionID) {
		return "session_id contains control characters"
	}
	if len(req.StationName) > 256 {
		return "station_name is too long"
	}
	if hasControl(req.StationName) {
		return "station_name contains control characters"
	}
	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		return "url is required"
	}
	if len(raw) > 8192 {
		return "url is too long"
	}
	if hasControl(raw) {
		return "url contains control characters"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "url must be an absolute http:// or https:// URL"
	}
	if parsed.User != nil && parsed.User.Username() == "" {
		return "url has an empty user name"
	}
	player := strings.TrimSpace(req.PlayerAppID)
	if player == "" {
		player = m.PlayerApp()
	}
	if !validAppID(player) {
		return "player_app_id is not a valid app id"
	}
	if allowed := m.PlayerApp(); player != allowed {
		return fmt.Sprintf("player app %q is not configured (expected %q)", player, allowed)
	}
	return ""
}

// hasControl reports C0/C1 controls and DEL, which would otherwise land in a
// log line or an error detail verbatim.
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

func receiveOne(call *capsulad.ServerCall) ([]byte, error) {
	message, err := call.Recv()
	if err != nil {
		return nil, err
	}
	// A caller may half-close immediately; no second message is part of the
	// contract, so the first one is the whole request.
	return message, nil
}

// validAppID matches the manifest's app id grammar.
func validAppID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// redact removes userinfo and the query string, which can carry stream tokens.
func redact(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "invalid URL"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
