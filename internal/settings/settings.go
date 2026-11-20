// Package settings implements the framework's two persistent configuration
// stores: device-wide user settings and per-application typed settings.
//
// Both use compare-and-swap revisions and atomic owner-only files. Apps never
// write these files; the daemon does so on their behalf and delivers values
// over the app bus. Masked values are never returned by snapshots or logs.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"capsulad/internal/discovery"
	"capsulad/internal/logbus"
)

type TouchHoldMode int

const (
	HoldSummon TouchHoldMode = 1
	HoldToTalk TouchHoldMode = 2
)

type MutedIndicatorMode int

const (
	MutedImmediate   MutedIndicatorMode = 1
	MutedDelay       MutedIndicatorMode = 2
	MutedUntilUnmute MutedIndicatorMode = 3
)

// WakewordBinding is one installed wake word's configuration: whether that word
// may activate and which app it activates. ModelID is the artifact id (the file
// name stem under <data-dir>/wakeword/models). AppID empty means the selected
// assistant, which is also the legacy single-word behaviour.
type WakewordBinding struct {
	ModelID string `json:"model_id"`
	AppID   string `json:"app_id"`
	Enabled bool   `json:"enabled"`
}

// MaxWakewordBindings bounds the per-word configuration. Eight heads at
// 0.2–0.4 ms each stay well inside the 80 ms hop; a longer list is an operator
// mistake, not a supported detector.
const MaxWakewordBindings = 8

// UserSettings is the device-wide configuration rendered by the Web UI.
// Wakewords is the whole per-word list; nil is the legacy state in which every
// installed word is live under WakewordEnabled and bound to the selected
// assistant, and an empty non-nil list is the explicit "no word is live" state.
// The key is always written: `null` is the legacy state, `[]` is the explicit
// empty list, so a document cannot lose the distinction by being re-encoded.
// The retired wakeword_model_id key is not read: that selector never gated
// anything, so restoring it from an old document would resurrect dead state.
type UserSettings struct {
	DefaultAppByResource  map[string]string  `json:"default_app_by_resource"`
	DefaultAssistantAppID string             `json:"default_assistant_app_id"`
	Wakewords             []WakewordBinding  `json:"wakewords"`
	WakewordEnabled       bool               `json:"wakeword_enabled"`
	DisplayBrightness     map[string]float64 `json:"display_brightness"`
	MasterVolume          float64            `json:"master_volume"`
	MicrophoneMuted       bool               `json:"microphone_muted"`
	TouchHoldMode         TouchHoldMode      `json:"touch_hold_mode"`
	MutedIndicator        MutedIndicatorMode `json:"muted_indicator_mode"`
	MutedIndicatorDelayMS uint32             `json:"muted_indicator_delay_ms"`
	// Timezone is the device wall clock (IANA name). Empty follows the
	// firmware's /etc/localtime. Applications inherit it as TZ.
	Timezone string `json:"timezone"`
	// BrightnessMode maps a display/ring resource to "const" or "auto". Auto
	// follows the ambient-light sensor; a missing entry means "const".
	BrightnessMode map[string]string `json:"brightness_mode"`
	// BluetoothIncomingConnections allows inbound links. Off denies every
	// device, including paired ones, while the radio stays powered for BLE.
	BluetoothIncomingConnections bool `json:"bluetooth_incoming_connections"`
	// BluetoothDiscoverable advertises the adapter and is the pairing window.
	// It implies incoming connections; disabling them disables it.
	BluetoothDiscoverable bool `json:"bluetooth_discoverable"`
	// StartupSoundEnabled replays the installation startup cue on every boot.
	// Off by default: the cue plays exactly once on installation regardless,
	// and afterwards only when the user opts in.
	StartupSoundEnabled bool `json:"startup_sound_enabled"`
	// Hostname is the single DNS label advertised as <hostname>.local over
	// mDNS. It is applied by the discovery manager at startup and on change.
	Hostname string `json:"hostname"`
}

const (
	BrightnessConst = "const"
	BrightnessAuto  = "auto"
	// DefaultHostname is the mDNS label a fresh install advertises and the
	// fallback when a settings document carries no valid host name.
	DefaultHostname = "capsula"
)

func DefaultUserSettings() UserSettings {
	return UserSettings{
		DefaultAppByResource: map[string]string{},
		WakewordEnabled:      true,
		DisplayBrightness:    map[string]float64{"display.segment.front": 0.8, "light.ring.main": 0.8},
		MasterVolume:         0.6,
		TouchHoldMode:        HoldSummon,
		// Default: the muted indication stays until the microphone is unmuted.
		MutedIndicator:        MutedUntilUnmute,
		MutedIndicatorDelayMS: 30000,
		BrightnessMode:        map[string]string{},
		Hostname:              DefaultHostname,
		// Paired devices may reconnect out of the box; advertising for new
		// devices is an explicit privacy choice.
		BluetoothIncomingConnections: true,
		BluetoothDiscoverable:        false,
	}
}

type UserSnapshot struct {
	Revision uint64
	Values   UserSettings
}

type UserStore struct {
	path     string
	logs     *logbus.Bus
	validate func(UserSettings) error

	mu     sync.Mutex
	rev    uint64
	values UserSettings
	subs   map[int]chan UserSnapshot
	nextID int
}

// OpenUser loads or initializes the device settings store.
func OpenUser(path string, logs *logbus.Bus, validate func(UserSettings) error) (*UserStore, error) {
	s := &UserStore{path: path, logs: logs, validate: validate, values: DefaultUserSettings(), rev: 1, subs: map[int]chan UserSnapshot{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var doc struct {
			Revision uint64          `json:"revision"`
			Values   json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse settings %s: %w", path, err)
		}
		// Unmarshal over the documented defaults: a field missing from the
		// document keeps its default, a present field replaces it.
		values := DefaultUserSettings()
		if len(doc.Values) > 0 && string(doc.Values) != "null" {
			if err := json.Unmarshal(doc.Values, &values); err != nil {
				return nil, fmt.Errorf("parse settings %s values: %w", path, err)
			}
		}
		if doc.Revision > 0 {
			s.rev = doc.Revision
		}
		s.values = normalize(values)
		if s.values.Hostname == "" {
			// An empty host name is invalid: a document without one advertises
			// the default instead.
			s.values.Hostname = DefaultHostname
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func normalize(v UserSettings) UserSettings {
	d := DefaultUserSettings()
	if v.DefaultAppByResource == nil {
		v.DefaultAppByResource = d.DefaultAppByResource
	}
	if v.DisplayBrightness == nil {
		v.DisplayBrightness = d.DisplayBrightness
	}
	if v.TouchHoldMode == 0 {
		v.TouchHoldMode = d.TouchHoldMode
	}
	if v.MutedIndicator == 0 {
		v.MutedIndicator = d.MutedIndicator
	}
	if v.MutedIndicatorDelayMS == 0 {
		v.MutedIndicatorDelayMS = d.MutedIndicatorDelayMS
	}
	// DNS labels are case-insensitive; keep one canonical form so a saved
	// value and the live advertisement compare equal.
	v.Hostname = discovery.NormalizeHostname(v.Hostname)
	// The two Bluetooth controls are dependent: advertising for new devices
	// requires accepting inbound links, and an adapter that accepts no inbound
	// link cannot coherently advertise or pair.
	if v.BluetoothDiscoverable {
		v.BluetoothIncomingConnections = true
	}
	if !v.BluetoothIncomingConnections {
		v.BluetoothDiscoverable = false
	}
	return v
}

func (s *UserStore) Get() UserSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return UserSnapshot{Revision: s.rev, Values: cloneUser(s.values)}
}

func cloneUser(v UserSettings) UserSettings {
	out := v
	out.Wakewords = cloneBindings(v.Wakewords)
	out.DefaultAppByResource = make(map[string]string, len(v.DefaultAppByResource))
	for k, val := range v.DefaultAppByResource {
		out.DefaultAppByResource[k] = val
	}
	out.DisplayBrightness = make(map[string]float64, len(v.DisplayBrightness))
	for k, val := range v.DisplayBrightness {
		out.DisplayBrightness[k] = val
	}
	out.BrightnessMode = cloneMap(v.BrightnessMode)
	return out
}

// Update applies a compare-and-swap patch. Fields lists top-level paths to
// replace; an empty list replaces the whole object. Map fields are replaced as
// a whole when included.
func (s *UserStore) Update(expected uint64, patch UserSettings, fields []string) (UserSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected != 0 && expected != s.rev {
		return UserSnapshot{}, fmt.Errorf("settings revision %d is stale (current %d)", expected, s.rev)
	}
	next := cloneUser(s.values)
	if len(fields) == 0 {
		next = normalize(patch)
	} else {
		for _, f := range fields {
			switch f {
			case "default_app_by_resource":
				next.DefaultAppByResource = cloneMap(patch.DefaultAppByResource)
			case "default_assistant_app_id":
				next.DefaultAssistantAppID = patch.DefaultAssistantAppID
			case "wakewords":
				// Whole-list replacement, like default_app_by_resource: the
				// browser sends every entry, so adding or removing a word is one
				// patch instead of a per-entry merge with ambiguous ordering.
				next.Wakewords = cloneBindings(patch.Wakewords)
			case "wakeword_enabled":
				next.WakewordEnabled = patch.WakewordEnabled
			case "display_brightness":
				next.DisplayBrightness = cloneMap(patch.DisplayBrightness)
			case "master_volume":
				next.MasterVolume = patch.MasterVolume
			case "microphone_muted":
				next.MicrophoneMuted = patch.MicrophoneMuted
			case "touch_hold_mode":
				next.TouchHoldMode = patch.TouchHoldMode
			case "muted_microphone_indicator":
				next.MutedIndicator = patch.MutedIndicator
				next.MutedIndicatorDelayMS = patch.MutedIndicatorDelayMS
			case "muted_indicator_mode":
				next.MutedIndicator = patch.MutedIndicator
			case "muted_indicator_delay_ms":
				next.MutedIndicatorDelayMS = patch.MutedIndicatorDelayMS
			case "timezone":
				next.Timezone = patch.Timezone
			case "brightness_mode":
				next.BrightnessMode = cloneMap(patch.BrightnessMode)
			case "bluetooth_incoming_connections":
				next.BluetoothIncomingConnections = patch.BluetoothIncomingConnections
				if !next.BluetoothIncomingConnections {
					// A patch that turns inbound links off must also clear the
					// stale discoverable flag copied from the current snapshot;
					// normalize otherwise sees discoverable=true and re-enables
					// incoming.
					next.BluetoothDiscoverable = false
				}
			case "bluetooth_discoverable":
				next.BluetoothDiscoverable = patch.BluetoothDiscoverable
				if next.BluetoothDiscoverable {
					next.BluetoothIncomingConnections = true
				}
			case "startup_sound_enabled":
				next.StartupSoundEnabled = patch.StartupSoundEnabled
			case "hostname":
				next.Hostname = patch.Hostname
			default:
				return UserSnapshot{}, fmt.Errorf("unknown update field %q", f)
			}
		}
	}
	return s.commitLocked(next)
}

// SetMasterVolume replaces only the device master volume. check runs after the
// store lock is acquired and before any mutation, so an authorization or
// context check cannot go stale while waiting for another settings writer; its
// error leaves the stored snapshot, revision and subscribers untouched. check
// must not call back into this store. Unlike Update there is no expected
// revision: a serialized scalar control commits the value current under the
// lock.
func (s *UserStore) SetMasterVolume(v float64, check func() error) (UserSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if check != nil {
		if err := check(); err != nil {
			return UserSnapshot{}, err
		}
	}
	next := cloneUser(s.values)
	next.MasterVolume = v
	return s.commitLocked(next)
}

// commitLocked validates, persists and publishes next. The caller holds s.mu.
func (s *UserStore) commitLocked(next UserSettings) (UserSnapshot, error) {
	next = normalize(next)
	if err := validateUser(next); err != nil {
		return UserSnapshot{}, err
	}
	if s.validate != nil {
		if err := s.validate(next); err != nil {
			return UserSnapshot{}, err
		}
	}
	if err := s.persistLocked(next, s.rev+1); err != nil {
		return UserSnapshot{}, err
	}
	s.values = next
	s.rev++
	snap := UserSnapshot{Revision: s.rev, Values: cloneUser(next)}
	for _, ch := range s.subs {
		select {
		case ch <- snap:
		default:
		}
	}
	return snap, nil
}

// cloneBindings deep-copies the per-word list. nil stays nil: it is the legacy
// state, and an empty non-nil list is the explicit "no word is live" state.
// The copy is allocated before filling so a zero-length slice stays non-nil;
// append would return nil and silently turn "no word is live" back into legacy.
func cloneBindings(in []WakewordBinding) []WakewordBinding {
	if in == nil {
		return nil
	}
	out := make([]WakewordBinding, len(in))
	copy(out, in)
	return out
}

func cloneMap[T any](in map[string]T) map[string]T {
	out := make(map[string]T, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func validateUser(v UserSettings) error {
	if v.MasterVolume < 0 || v.MasterVolume > 1 {
		return fmt.Errorf("master_volume must be in [0,1]")
	}
	for id, b := range v.DisplayBrightness {
		if b < 0 || b > 1 {
			return fmt.Errorf("display_brightness[%s] must be in [0,1]", id)
		}
	}
	switch v.TouchHoldMode {
	case HoldSummon, HoldToTalk:
	default:
		return fmt.Errorf("invalid touch_hold_mode")
	}
	switch v.MutedIndicator {
	case MutedImmediate, MutedDelay, MutedUntilUnmute:
	default:
		return fmt.Errorf("invalid muted indicator mode")
	}
	if v.MutedIndicator == MutedDelay && v.MutedIndicatorDelayMS == 0 {
		return fmt.Errorf("muted indicator delay requires a non-zero delay")
	}
	for id, mode := range v.BrightnessMode {
		switch mode {
		case BrightnessConst, BrightnessAuto:
		default:
			return fmt.Errorf("brightness_mode[%s] must be %q or %q", id, BrightnessConst, BrightnessAuto)
		}
	}
	if v.Timezone != "" {
		if _, err := time.LoadLocation(v.Timezone); err != nil {
			return fmt.Errorf("timezone %q is not an IANA zone name", v.Timezone)
		}
	}
	if err := discovery.ValidateHostname(v.Hostname); err != nil {
		return fmt.Errorf("hostname %q: %w", v.Hostname, err)
	}
	if err := validateWakewords(v.Wakewords); err != nil {
		return err
	}
	return nil
}

// validateWakewords checks the shape of the per-word list. Model availability is
// not checked here: an artifact can be installed, deleted or repaired between
// writes, and Settings is a durable record, not the engine's registry.
func validateWakewords(list []WakewordBinding) error {
	if list == nil {
		return nil
	}
	if len(list) > MaxWakewordBindings {
		return fmt.Errorf("wakewords: at most %d words are supported, got %d", MaxWakewordBindings, len(list))
	}
	seen := make(map[string]int, len(list))
	for i, entry := range list {
		if entry.ModelID == "" {
			return fmt.Errorf("wakewords[%d]: model_id is required", i)
		}
		if _, ok := seen[entry.ModelID]; ok {
			return fmt.Errorf("wakewords[%d]: model %q is listed twice", i, entry.ModelID)
		}
		seen[entry.ModelID] = i
	}
	return nil
}

func (s *UserStore) persistLocked(v UserSettings, rev uint64) error {
	if s.path == "" {
		return nil
	}
	doc := struct {
		Revision uint64       `json:"revision"`
		Values   UserSettings `json:"values"`
	}{Revision: rev, Values: v}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path, data)
}

// Watch returns the current snapshot and a channel of subsequent snapshots.
func (s *UserStore) Watch() (UserSnapshot, <-chan UserSnapshot, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	ch := make(chan UserSnapshot, 8)
	s.subs[id] = ch
	ch <- UserSnapshot{Revision: s.rev, Values: cloneUser(s.values)}
	return UserSnapshot{Revision: s.rev, Values: cloneUser(s.values)}, ch, func() {
		s.mu.Lock()
		if cur, ok := s.subs[id]; ok {
			close(cur)
			delete(s.subs, id)
		}
		s.mu.Unlock()
	}
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// --- per-application settings ---------------------------------------------

// SettingDefinition is a manifest-derived schema entry.
type AppSettingView struct {
	Key      string
	Value    string // empty for masked values
	HasValue bool
}

type AppSnapshot struct {
	AppID    string
	Revision uint64
	Values   []AppSettingView
}

type appState struct {
	rev    uint64
	values map[string]json.RawMessage
}

type ManifestLookup func(appID string) (*ManifestSchema, bool)

// ManifestSchema is the subset of a manifest the settings store needs. The
// daemon supplies it from the registry, keeping this package independent of
// manifest parsing.
type ManifestSchema struct {
	AppID    string
	Settings []SchemaSetting
}

type SchemaSetting struct {
	Key         string
	Type        string
	DefaultJSON string
	Required    bool
}

// StaleRevisionError reports a failed compare-and-swap. Callers map it to
// ABORTED; it is distinct from invalid input and storage failures.
type StaleRevisionError struct {
	Expected uint64
	Current  uint64
}

func (e *StaleRevisionError) Error() string {
	return fmt.Sprintf("app settings revision %d is stale (current %d)", e.Expected, e.Current)
}

// InvalidValueError wraps a manifest/semantic validation rejection. Callers
// map it to INVALID_ARGUMENT; storage and IO failures stay plain errors.
type InvalidValueError struct{ Err error }

func (e *InvalidValueError) Error() string { return e.Err.Error() }
func (e *InvalidValueError) Unwrap() error { return e.Err }

// NotFoundError reports a request for an app the registry does not know.
// Callers map it to NOT_FOUND; it is not a storage failure.
type NotFoundError struct{ AppID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("unknown app %s", e.AppID) }

type AppStore struct {
	root string
	logs *logbus.Bus
	look ManifestLookup
	// validate, when set, receives the fully merged candidate value map before
	// it is written. It lets the owning daemon/app reject semantically invalid
	// settings (cross-field ranges, path policy) synchronously, with no write
	// and no revision bump on failure. It must not call back into the store.
	validate func(appID string, values map[string]json.RawMessage) error

	mu    sync.Mutex
	state map[string]*appState
	subs  map[string]map[int]chan AppSnapshot
	next  int
}

func OpenApps(root string, logs *logbus.Bus, look ManifestLookup) (*AppStore, error) {
	a := &AppStore{root: root, logs: logs, look: look, state: map[string]*appState{}, subs: map[string]map[int]chan AppSnapshot{}}
	return a, nil
}

// SetValidator installs the semantic validator used by Update. The daemon
// installs one dispatcher that routes to per-app validators.
func (a *AppStore) SetValidator(v func(appID string, values map[string]json.RawMessage) error) {
	a.mu.Lock()
	a.validate = v
	a.mu.Unlock()
}

func (a *AppStore) stateLocked(appID string) (*appState, error) {
	if st, ok := a.state[appID]; ok {
		return st, nil
	}
	st := &appState{rev: 1, values: map[string]json.RawMessage{}}
	if a.root != "" {
		path := filepath.Join(a.root, appID, "settings.json")
		if data, err := os.ReadFile(path); err == nil {
			var doc struct {
				Revision uint64                     `json:"revision"`
				Values   map[string]json.RawMessage `json:"values"`
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				return nil, fmt.Errorf("parse app settings %s: %w", path, err)
			}
			if doc.Revision > 0 {
				st.rev = doc.Revision
			}
			if doc.Values != nil {
				st.values = doc.Values
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	a.state[appID] = st
	return st, nil
}

// Snapshot returns the redacted view rendered by the Web UI.
func (a *AppStore) Snapshot(appID string) (AppSnapshot, error) {
	schema, ok := a.look(appID)
	if !ok {
		return AppSnapshot{}, &NotFoundError{AppID: appID}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.stateLocked(appID)
	if err != nil {
		return AppSnapshot{}, err
	}
	return snapshotLocked(appID, *schema, st), nil
}

// snapshotLocked builds the redacted view. The caller holds a.mu so the
// revision and values are a single consistent snapshot.
func snapshotLocked(appID string, schema ManifestSchema, st *appState) AppSnapshot {
	out := AppSnapshot{AppID: appID, Revision: st.rev}
	for _, def := range schema.Settings {
		view := AppSettingView{Key: def.Key}
		raw, ok := st.values[def.Key]
		if def.Type == "text_masked" {
			view.HasValue = ok
			out.Values = append(out.Values, view)
			continue
		}
		if ok {
			view.Value = string(raw)
			view.HasValue = true
		} else if def.DefaultJSON != "" {
			view.Value = def.DefaultJSON
			view.HasValue = true
		}
		out.Values = append(out.Values, view)
	}
	return out
}

// Private returns effective values including masked secrets for delivery to
// the owning app over its own bus connection. Callers must not log the result.
func (a *AppStore) Private(appID string) (uint64, map[string]json.RawMessage, error) {
	schema, ok := a.look(appID)
	if !ok {
		return 0, nil, &NotFoundError{AppID: appID}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.stateLocked(appID)
	if err != nil {
		return 0, nil, err
	}
	out := map[string]json.RawMessage{}
	for _, def := range schema.Settings {
		if raw, ok := st.values[def.Key]; ok {
			out[def.Key] = raw
		} else if def.DefaultJSON != "" {
			out[def.Key] = json.RawMessage(def.DefaultJSON)
		}
	}
	return st.rev, out, nil
}

type Change struct {
	Key   string
	Value any
	Clear bool
}

// Update applies validated changes with a compare-and-swap revision and
// returns the redacted snapshot plus the private values to push to the app.
func (a *AppStore) Update(appID string, expected uint64, changes []Change) (AppSnapshot, uint64, map[string]json.RawMessage, error) {
	schema, ok := a.look(appID)
	if !ok {
		return AppSnapshot{}, 0, nil, &NotFoundError{AppID: appID}
	}
	defs := map[string]SchemaSetting{}
	for _, d := range schema.Settings {
		defs[d.Key] = d
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.stateLocked(appID)
	if err != nil {
		return AppSnapshot{}, 0, nil, err
	}
	if expected != 0 && expected != st.rev {
		return AppSnapshot{}, 0, nil, &StaleRevisionError{Expected: expected, Current: st.rev}
	}
	next := make(map[string]json.RawMessage, len(st.values))
	for k, v := range st.values {
		next[k] = v
	}
	for _, ch := range changes {
		def, ok := defs[ch.Key]
		if !ok {
			return AppSnapshot{}, 0, nil, &InvalidValueError{Err: fmt.Errorf("setting %s is not declared by %s", ch.Key, appID)}
		}
		if ch.Clear {
			delete(next, ch.Key)
			continue
		}
		if def.Type == "text_masked" {
			if s, ok := ch.Value.(string); ok && s == "" {
				return AppSnapshot{}, 0, nil, &InvalidValueError{Err: fmt.Errorf("setting %s: clearing a masked value requires an explicit clear", ch.Key)}
			}
		}
		if err := validateJSONValue(def, ch.Value); err != nil {
			return AppSnapshot{}, 0, nil, &InvalidValueError{Err: err}
		}
		raw, err := json.Marshal(ch.Value)
		if err != nil {
			return AppSnapshot{}, 0, nil, err
		}
		next[ch.Key] = raw
	}
	if a.validate != nil {
		if err := a.validate(appID, next); err != nil {
			return AppSnapshot{}, 0, nil, &InvalidValueError{Err: err}
		}
	}
	newRev := st.rev + 1
	if a.root != "" {
		path := filepath.Join(a.root, appID, "settings.json")
		doc := struct {
			Revision uint64                     `json:"revision"`
			Values   map[string]json.RawMessage `json:"values"`
		}{Revision: newRev, Values: next}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return AppSnapshot{}, 0, nil, err
		}
		if err := writeAtomic(path, data); err != nil {
			return AppSnapshot{}, 0, nil, err
		}
	}
	st.rev = newRev
	st.values = next

	// Build outputs without re-entering the lock helpers.
	priv := map[string]json.RawMessage{}
	for _, def := range schema.Settings {
		if raw, ok := next[def.Key]; ok {
			priv[def.Key] = raw
		} else if def.DefaultJSON != "" {
			priv[def.Key] = json.RawMessage(def.DefaultJSON)
		}
	}
	out := AppSnapshot{AppID: appID, Revision: newRev}
	for _, def := range schema.Settings {
		view := AppSettingView{Key: def.Key}
		raw, ok := next[def.Key]
		if def.Type == "text_masked" {
			view.HasValue = ok
		} else if ok {
			view.Value = string(raw)
			view.HasValue = true
		} else if def.DefaultJSON != "" {
			view.Value = def.DefaultJSON
			view.HasValue = true
		}
		out.Values = append(out.Values, view)
	}
	if subs := a.subs[appID]; len(subs) > 0 {
		for _, ch := range subs {
			pushLatestSnapshot(ch, out)
		}
	}
	return out, newRev, priv, nil
}

// pushLatestSnapshot keeps a bounded watcher converged on the newest state:
// when the buffer is full the oldest snapshot is discarded. The caller holds
// a.mu, so sends and the channel close cannot race.
func pushLatestSnapshot(ch chan AppSnapshot, snap AppSnapshot) {
	select {
	case ch <- snap:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- snap:
	default:
	}
}

func validateJSONValue(def SchemaSetting, v any) error {
	switch def.Type {
	case "text", "text_masked":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("setting %s must be text", def.Key)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("setting %s must be boolean", def.Key)
		}
	case "integer":
		if n, ok := toFloat(v); !ok || n != float64(int64(n)) {
			return fmt.Errorf("setting %s must be an integer", def.Key)
		}
	case "number":
		if _, ok := toFloat(v); !ok {
			return fmt.Errorf("setting %s must be a number", def.Key)
		}
	case "select":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("setting %s must be a select value", def.Key)
		}
	default:
		return fmt.Errorf("setting %s has unsupported type %s", def.Key, def.Type)
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// WatchApp streams redacted snapshots for one app (Web UI / diagnostics). The
// initial snapshot and the subscription are installed under one lock, so a
// concurrent Update can never fall between them and be lost.
func (a *AppStore) WatchApp(appID string) (AppSnapshot, <-chan AppSnapshot, func(), error) {
	schema, ok := a.look(appID)
	if !ok {
		return AppSnapshot{}, nil, nil, &NotFoundError{AppID: appID}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.stateLocked(appID)
	if err != nil {
		return AppSnapshot{}, nil, nil, err
	}
	snap := snapshotLocked(appID, *schema, st)
	if a.subs[appID] == nil {
		a.subs[appID] = map[int]chan AppSnapshot{}
	}
	a.next++
	id := a.next
	ch := make(chan AppSnapshot, 4)
	a.subs[appID][id] = ch
	ch <- snap
	return snap, ch, func() {
		a.mu.Lock()
		if m := a.subs[appID]; m != nil {
			if cur, ok := m[id]; ok {
				close(cur)
				delete(m, id)
			}
		}
		a.mu.Unlock()
	}, nil
}

// Revision returns the current revision for an app.
func (a *AppStore) Revision(appID string) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.stateLocked(appID)
	if err != nil {
		return 0, err
	}
	return st.rev, nil
}

// IDs lists apps with stored settings.
func (a *AppStore) IDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.state))
	for id := range a.state {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
