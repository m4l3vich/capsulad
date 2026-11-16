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
	// Available reports that the artifact loads and its engine runs on this
	// device. It is independent of enablement and binding: an unusable artifact
	// stays listed so the operator can retrain it.
	Available bool
	// Enabled is the effective enablement of this word: the global detection
	// switch, this word's own setting and its availability, already combined by
	// the daemon. A detector activation for a model that is not enabled is
	// refused, so a saved binding cannot keep a suspended word live.
	Enabled bool
	// AppID is the app this word is bound to. Empty means the currently selected
	// assistant, which is also the legacy single-word behaviour.
	AppID string
	// Threshold is the calibrated decision threshold stored in the artifact.
	Threshold float64
	// Detail explains why a listed model cannot be used (unreadable artifact,
	// unavailable capture preprocessing). Empty for a ready model.
	Detail string
	// Gate is the quality certificate recorded when this model was trained:
	// tune/final-test recall and false activations, the threshold and the
	// auxiliary off-policy result. It is nil for placeholder entries and for
	// artifacts that predate the record. The report is immutable once set.
	Gate *GateReport
}

type Activation struct {
	ID      string
	Origin  string
	ModelID string
	// AppID is the app the matching word is bound to. Empty means the selected
	// assistant: manual and API activations always use that path.
	AppID     string
	TimeNS    uint64
	UnixMS    int64
	ExpiresNS uint64
}

// Detection is one real engine activation retained for the authenticated Web
// UI. It is written at detection time and never reconstructed from logs.
type Detection struct {
	// ID is a runtime-only identity for the entry, so the browser can dismiss
	// a single false detection. It is never persisted.
	ID          string
	ModelID     string
	DisplayName string
	TimeNS      uint64
	UnixMS      int64
}

// recentDetectionLimit bounds the in-memory history. Detections are rare
// (the runner enforces a 2.9 s cooldown) and the browser only shows the tail.
const recentDetectionLimit = 20

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
	// recent holds real detector activations only, oldest first.
	recent []Detection
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
	case "touch", "play_pause_key", "key", "manual", "simulator":
		return true
	}
	return false
}

// RecentDetections returns the newest real detections first, capped at limit.
func (m *Manager) RecentDetections(limit int) []Detection {
	if limit <= 0 || limit > recentDetectionLimit {
		limit = recentDetectionLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.recent)
	if n > limit {
		n = limit
	}
	out := make([]Detection, 0, n)
	for i := len(m.recent) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.recent[i])
	}
	return out
}

// DismissDetection removes one stored detection by its runtime ID and reports
// whether it existed. Ordering of the remaining entries is preserved; the
// browser calls this when the user marks a detection as a false positive.
func (m *Manager) DismissDetection(id string) bool {
	if id == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, d := range m.recent {
		if d.ID == id {
			m.recent = append(m.recent[:i], m.recent[i+1:]...)
			return true
		}
	}
	return false
}

// detectionTarget resolves the live policy for one detector model: the global
// switch may be off, the microphone may be muted, or the artifact may be
// unavailable or not enabled. It returns the effective model so the caller can
// use its binding and display name. The runner calls it before arbitration so a
// word that cannot fire never shadows one that can.
func (m *Manager) detectionTarget(modelID string) (Model, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled || m.muted {
		return Model{}, false
	}
	model, ok := m.modelLocked(modelID)
	if !ok || !model.Available || !model.Enabled {
		return model, false
	}
	return model, true
}

// modelLocked finds one advertised model. The caller holds m.mu; it never
// decides enablement, because manual origins bypass parts of that policy.
func (m *Manager) modelLocked(modelID string) (Model, bool) {
	for _, model := range m.models {
		if model.ID == modelID {
			return model, true
		}
	}
	return Model{}, false
}

// Trigger creates an activation for the selected model. It is the simulator's
// manual detection path and the hook a real DSP engine calls. Manual origins are
// not gated on detector availability or per-word enablement; engine detections
// are gated on both.
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
	displayName, appID := "", ""
	if !manual {
		model, found := m.modelLocked(modelID)
		switch {
		case !found || !model.Available:
			m.mu.Unlock()
			m.logs.Warnf("wakeword", "", "detection for unavailable model %q ignored", modelID)
			return Activation{}, false
		// Per-word enablement is enforced here, not only in the runner: an
		// explicit API trigger must not reactivate a word the operator disabled.
		case !model.Enabled:
			m.mu.Unlock()
			m.logs.Warnf("wakeword", "", "detection for disabled model %q ignored", modelID)
			return Activation{}, false
		}
		displayName, appID = model.DisplayName, model.AppID
	}
	now := m.clock.NowNS()
	act := Activation{
		ID: ids.New("act"), Origin: origin, ModelID: modelID, AppID: appID,
		TimeNS: now, UnixMS: time.Now().UnixMilli(), ExpiresNS: now + uint64(5*time.Second),
	}
	// Only the real detector's origin is a detection. Manual buttons, the
	// simulator and the administrator API remain activations but are not
	// presented as keyword detections.
	if origin == "wakeword" {
		m.recent = append(m.recent, Detection{
			ID: ids.New("det"), ModelID: modelID, DisplayName: displayName, TimeNS: now, UnixMS: act.UnixMS,
		})
		if len(m.recent) > recentDetectionLimit {
			copy(m.recent, m.recent[len(m.recent)-recentDetectionLimit:])
			m.recent = m.recent[:recentDetectionLimit]
		}
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
