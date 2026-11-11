// Package wakeword owns framework-side wakeword detection state.
//
// The assistant is a ready, always-running supervised app. A detection creates
// a short-lived activation and the daemon relays it over that app's bus
// connection. Late capture may lose audio; nothing is queued forever, and a
// detection with no ready assistant is logged and dropped.
package wakeword

import (
	"context"
	"sync"
	"time"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

type Model struct {
	ID          string
	DisplayName string
	Language    string
	EngineID    string
	Available   bool
}

type Activation struct {
	ID        string
	Origin    string
	ModelID   string
	TimeNS    uint64
	ExpiresNS uint64
}

type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	clock  *ids.Clock
	logs   *logbus.Bus
	models []Model

	mu      sync.Mutex
	enabled bool
	muted   bool
	onFire  func(Activation)
	// gate reports whether detection may run right now. The daemon suspends it
	// during training takes and while the assistant holds audio streams, so
	// recording the word cannot start an assistant response inside the take.
	gate      func() bool
	suspended bool
	// resumeAtNS holds detection off for a short tail after the gate reopens:
	// the assistant's last output is still in flight and would re-trigger.
	resumeAtNS  uint64
	gateWasBusy bool
	resumeDelay time.Duration
}

func NewManager(parent context.Context, clock *ids.Clock, logs *logbus.Bus, models []Model) *Manager {
	ctx, cancel := context.WithCancel(parent)
	if len(models) == 0 {
		models = []Model{{ID: "wakeword.unavailable", DisplayName: "Unavailable", EngineID: "none"}}
	}
	return &Manager{ctx: ctx, cancel: cancel, clock: clock, logs: logs, models: models, resumeDelay: defaultResumeDelay}
}

func (m *Manager) Models() []Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Model(nil), m.models...)
}

func (m *Manager) SetEnabled(v bool) {
	m.mu.Lock()
	m.enabled = v
	m.mu.Unlock()
}

func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

func (m *Manager) SetMuted(v bool) {
	m.mu.Lock()
	m.muted = v
	m.mu.Unlock()
	if v {
		m.logs.Infof("wakeword", "", "detection suspended: microphone muted")
	}
}

// SetGate installs the activity predicate consulted before every window.
func (m *Manager) SetGate(fn func() bool) {
	m.mu.Lock()
	m.gate = fn
	m.mu.Unlock()
}

// State reports the detection inputs for diagnostics.
func (m *Manager) State() (enabled, muted, gate, suspended bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gate != nil {
		m.mu.Unlock()
		gate = !m.gate()
		m.mu.Lock()
	}
	return m.enabled, m.muted, gate, m.suspended
}

// SetResumeDelay tunes the tail that follows the assistant releasing audio.
func (m *Manager) SetResumeDelay(d time.Duration) {
	m.mu.Lock()
	m.resumeDelay = d
	m.mu.Unlock()
}

func (m *Manager) SetOnFire(fn func(Activation)) {
	m.mu.Lock()
	m.onFire = fn
	m.mu.Unlock()
}

// SetModels replaces the advertised models, e.g. when a trained classifier is
// installed at startup. It does not change enable/mute state.
func (m *Manager) SetModels(models []Model) {
	m.mu.Lock()
	m.models = append([]Model(nil), models...)
	m.mu.Unlock()
}

// Available reports whether any engine can currently detect.
func (m *Manager) Available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, model := range m.models {
		if model.Available {
			return true
		}
	}
	return false
}

// ManualOrigin reports whether an activation came from a physical input rather
// than a detector. Those must work even when no wakeword engine is available:
// the stock device has none, and its talk button still summons the assistant.
func ManualOrigin(origin string) bool {
	switch origin {
	case "touch", "play_pause_key", "voice_key", "key", "manual", "simulator":
		return true
	}
	return false
}

// Trigger creates an activation for the selected model. It is the simulator's
// manual detection path and the hook a real DSP engine calls. Manual origins are
// not gated on detector availability; engine detections still are.
func (m *Manager) Trigger(origin, modelID string) (Activation, bool) {
	m.mu.Lock()
	manual := ManualOrigin(origin)
	// A physical button is explicit intent: it works even with hands-free
	// detection switched off. Muting the microphone still blocks activation.
	if m.muted || (!manual && !m.enabled) {
		m.mu.Unlock()
		return Activation{}, false
	}
	if manual && modelID == "" {
		modelID = "manual.button"
	} else if modelID == "" && len(m.models) > 0 {
		modelID = m.models[0].ID
	}
	if !manual {
		known := false
		for _, model := range m.models {
			if model.ID == modelID && model.Available {
				known = true
				break
			}
		}
		if !known {
			m.mu.Unlock()
			m.logs.Warnf("wakeword", "", "detection for unavailable model %q ignored", modelID)
			return Activation{}, false
		}
	}
	now := m.clock.NowNS()
	act := Activation{
		ID: ids.New("act"), Origin: origin, ModelID: modelID,
		TimeNS: now, ExpiresNS: now + uint64(5*time.Second),
	}
	fn := m.onFire
	m.mu.Unlock()
	m.logs.Infof("wakeword", "", "activation=%s origin=%s model=%s", act.ID, origin, modelID)
	if fn != nil {
		fn(act)
	}
	return act, true
}

func (m *Manager) Close() { m.cancel() }
