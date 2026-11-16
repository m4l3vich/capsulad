package daemon

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/settings"
	"capsulad/internal/uichanges"
	"capsulad/internal/wakeword"
)

// Trained wake words are stored one v3 artifact per word under
// <data-dir>/wakeword/models/<id>.json. The file name stem is the identity the
// binding names, the activation reports and the Web UI edits, so an artifact
// cannot be rebound by renaming a display label.
func (d *Daemon) wakewordDir() string {
	return filepath.Join(d.cfg.DataDir, "wakeword")
}

// wakewordStore is the artifact store. The directory is derived from the data
// directory only: the retired -wakeword-model path is a legacy import source,
// not a second live location.
func (d *Daemon) wakewordStore() *wakeword.ModelStore {
	return wakeword.NewModelStore(filepath.Join(d.wakewordDir(), "models"))
}

// legacyClassifierPath is the single-model location this build migrates from:
// the explicit -wakeword-model override when set, otherwise
// <data-dir>/wakeword/model.json. It is only read during migration.
func (d *Daemon) legacyClassifierPath() string {
	if d.cfg.WakewordModel != "" {
		return d.cfg.WakewordModel
	}
	return filepath.Join(d.wakewordDir(), "model.json")
}

// classifierPath is the current artifact path for one model id.
func (d *Daemon) classifierPath(id string) string {
	return d.wakewordStore().Path(id)
}

// wakewordArtifactMu guards the scan cache. Settings writes are frequent (the
// volume ramp commits settings), so the installed set is cached and only
// re-read when this daemon installs or deletes an artifact.
type wakewordCache struct {
	mu      sync.Mutex
	scanned bool
	valid   []wakeword.StoredModel
	invalid []wakeword.InvalidModel
	// unavailable explains why no ensemble runs (missing capture preprocessing,
	// a frontend that could not build). It outlives a settings refresh so a
	// per-word toggle cannot advertise a word as available while nothing scores.
	unavailable string
}

// rescanWakewords migrates a legacy single artifact on first scan and returns the
// installed set. The migration moves bytes; it never rewrites the artifact, the
// settings or the trained weights.
func (d *Daemon) rescanWakewords() ([]wakeword.StoredModel, []wakeword.InvalidModel) {
	d.wkArtifacts.mu.Lock()
	defer d.wkArtifacts.mu.Unlock()
	d.scanWakewordsLocked()
	return d.wkArtifacts.valid, d.wkArtifacts.invalid
}

// invalidateWakewords forces the next scan to re-read the directory.
func (d *Daemon) invalidateWakewords() {
	d.wkArtifacts.mu.Lock()
	d.wkArtifacts.scanned = false
	d.wkArtifacts.mu.Unlock()
}

func (d *Daemon) scanWakewordsLocked() {
	if d.wkArtifacts.scanned {
		return
	}
	store := d.wakewordStore()
	legacyPath := d.legacyClassifierPath()
	var refusedLegacy *wakeword.InvalidModel
	if migrated, err := store.Migrate(legacyPath); err != nil {
		// The legacy artifact stays where it is and is reported as unusable,
		// rather than being loaded from a second live location. It is also
		// advertised as needing attention: the operator has an artifact on the
		// device, so "no wakeword yet" would be a lie.
		d.logs.Warnf("wakeword", "", "migrating %s into %s: %v", legacyPath, store.Dir(), err)
		refusedLegacy = &wakeword.InvalidModel{
			ID: legacyArtifactID(legacyPath), Path: legacyPath, Err: err,
		}
	} else if migrated {
		d.logs.Infof("wakeword", "", "migrated the single installed model into %s", store.Dir())
	}
	valid, invalid, err := store.Load()
	if err != nil {
		d.logs.Warnf("wakeword", "", "reading %s: %v", store.Dir(), err)
		valid, invalid = nil, nil
	}
	if refusedLegacy != nil && !invalidModelReported(invalid, refusedLegacy.Path) {
		invalid = append(invalid, *refusedLegacy)
	}
	for _, entry := range invalid {
		d.logs.Warnf("wakeword", "", "artifact %s is unusable and is not loaded: %v", entry.Path, entry.Err)
	}
	d.wkArtifacts.valid, d.wkArtifacts.invalid = valid, invalid
	d.wkArtifacts.scanned = true
}

// legacyArtifactID names the refused legacy artifact by its file name stem, the
// same identity every other artifact uses, so the Wakeword section can show one
// entry per file.
func legacyArtifactID(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// invalidModelReported reports whether the store already refused that path, so a
// legacy artifact inside the models directory is not listed twice.
func invalidModelReported(invalid []wakeword.InvalidModel, path string) bool {
	for _, entry := range invalid {
		if entry.Path == path {
			return true
		}
	}
	return false
}

// wakewordsProto copies the durable per-word list onto the wire. A nil list is
// the legacy state and stays absent on the wire; an empty non-nil list is the
// explicit "no word is live" state and stays an empty list.
func wakewordsProto(list []settings.WakewordBinding) []*frameworkv1.WakewordBinding {
	if list == nil {
		return nil
	}
	out := make([]*frameworkv1.WakewordBinding, 0, len(list))
	for _, entry := range list {
		out = append(out, &frameworkv1.WakewordBinding{
			ModelId: entry.ModelID, AppId: entry.AppID, Enabled: entry.Enabled,
		})
	}
	return out
}

// wakewordsFromProto is the reverse of wakewordsProto. A nil wire list stays nil
// (legacy) rather than becoming an empty authoritative list that would disable
// every word. An update that *owns* the `wakewords` mask states "no word is
// live" by listing nothing; because the repeated field cannot carry that
// difference, the RPC layer builds the empty non-nil list from the mask.
func wakewordsFromProto(list []*frameworkv1.WakewordBinding) []settings.WakewordBinding {
	if list == nil {
		return nil
	}
	out := make([]settings.WakewordBinding, 0, len(list))
	for _, entry := range list {
		if entry == nil {
			continue
		}
		out = append(out, settings.WakewordBinding{
			ModelID: entry.GetModelId(), AppID: entry.GetAppId(), Enabled: entry.GetEnabled(),
		})
	}
	return out
}

// wakewordBindingIndex maps the configured entries by model id.
func wakewordBindingIndex(list []settings.WakewordBinding) map[string]settings.WakewordBinding {
	if list == nil {
		return nil
	}
	index := make(map[string]settings.WakewordBinding, len(list))
	for _, entry := range list {
		index[entry.ModelID] = entry
	}
	return index
}

// wakewordEffective resolves each installed word's live policy.
//
// values.Wakewords == nil is the legacy state: every installed model is live
// under the global switch and bound to the selected assistant. That reproduces
// the previous single-model behaviour exactly (the retired selector never gated
// anything, so it is not consulted). A non-nil list is authoritative: a model
// without an entry is disabled, never live. Effective enablement is
// global switch && entry.Enabled && artifact available.
func wakewordEffective(valid []wakeword.StoredModel, invalid []wakeword.InvalidModel, values settings.UserSettings) []wakeword.Model {
	index := wakewordBindingIndex(values.Wakewords)
	models := make([]wakeword.Model, 0, len(valid)+len(invalid))
	for _, stored := range valid {
		model := wakeword.Model{
			ID: stored.ID, DisplayName: stored.Classifier.Display,
			Language: stored.Classifier.Language, EngineID: "framework.embedding-mlp",
			Available: true, Threshold: stored.Classifier.Threshold,
			Gate: classifierGate(stored.Classifier),
		}
		switch {
		case index == nil:
			model.Enabled = values.WakewordEnabled
		default:
			entry, ok := index[stored.ID]
			if !ok {
				model.Enabled = false
				model.Detail = "not in the wakeword list; save the wakeword settings to add it"
				break
			}
			model.Enabled = values.WakewordEnabled && entry.Enabled
			model.AppID = entry.AppID
			if entry.Enabled && !values.WakewordEnabled {
				model.Detail = "detection is switched off globally"
			}
		}
		if model.Enabled && !model.Available {
			model.Detail = "artifact unavailable"
		}
		models = append(models, model)
	}
	for _, entry := range invalid {
		models = append(models, wakeword.Model{
			ID: entry.ID, DisplayName: "Unreadable wakeword artifact",
			EngineID: "framework.embedding-mlp", Available: false,
			Enabled: false, Detail: entry.Err.Error(),
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

// refreshWakewordModels recomputes the advertised set from the installed
// artifacts and the current bindings. It never rebuilds the engine: a per-word
// enable toggle or rebinding takes effect on the next activation, with no
// re-subscribe and no warm-up.
func (d *Daemon) refreshWakewordModels() {
	d.wkArtifacts.mu.Lock()
	detail := d.wkArtifacts.unavailable
	d.wkArtifacts.mu.Unlock()
	valid, invalid := d.rescanWakewords()
	d.applyWakewordModels(valid, invalid, detail)
}

// applyWakewordModels installs the effective model list. unavailableDetail
// forces every word to be listed as unusable, for example when the declared
// capture preprocessing is missing.
func (d *Daemon) applyWakewordModels(valid []wakeword.StoredModel, invalid []wakeword.InvalidModel, unavailableDetail string) {
	if d.wake == nil {
		return
	}
	if len(valid) == 0 && len(invalid) == 0 {
		// Nothing is installed. Advertise the honest base list (the simulator's
		// manual trigger on a simulator, "record a wakeword" on a device) rather
		// than an empty list, which would also forget the simulator trigger.
		d.wake.SetModels(d.wakeModels())
		return
	}
	values := d.user.Get().Values
	models := wakewordEffective(valid, invalid, values)
	if unavailableDetail != "" {
		for i := range models {
			if !models[i].Available {
				continue
			}
			models[i].Available = false
			models[i].Enabled = false
			models[i].Detail = unavailableDetail
		}
	}
	previous := d.wake.Models()
	changed := !sameWakewordModels(previous, models)
	d.wake.SetModels(models)
	if changed {
		// The browser's wakeword view carries the effective enablement and
		// binding, so a settings change that alters them has to refresh that
		// topic rather than waiting for the next detection.
		d.publishUI(uichanges.Wakeword)
	}
}

// sameWakewordModels compares the fields the advertised list exposes. The gate
// certificate is immutable per artifact, so it cannot differ between two
// rebuilds of the same installed set.
func sameWakewordModels(a, b []wakeword.Model) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].DisplayName != b[i].DisplayName ||
			a[i].Language != b[i].Language || a[i].EngineID != b[i].EngineID ||
			a[i].Available != b[i].Available || a[i].Enabled != b[i].Enabled ||
			a[i].AppID != b[i].AppID || a[i].Threshold != b[i].Threshold ||
			a[i].Detail != b[i].Detail {
			return false
		}
	}
	return true
}

// voiceAppIDs is the set of apps a spoken or pressed activation may address: the
// selected assistant plus every app a wake word is bound to. Detection pauses
// while any of them holds audio (so the device cannot hear its own reply), and
// dismissal resolves the turn owner inside the same set.
func (d *Daemon) voiceAppIDs() []string {
	out := make([]string, 0, 1+settings.MaxWakewordBindings)
	seen := map[string]bool{}
	add := func(appID string) {
		if appID == "" || seen[appID] {
			return
		}
		seen[appID] = true
		out = append(out, appID)
	}
	add(d.assistantAppID())
	if d.wake == nil {
		// No manager means nothing is advertised, exactly as in
		// applyWakewordModels: recording and capture paths run without a wake
		// manager in tests and in a daemon whose engine never started.
		return out
	}
	for _, model := range d.wake.Models() {
		if model.AppID != "" && model.Available {
			add(model.AppID)
		}
	}
	return out
}

// voiceAudioActive reports whether any app reachable by an activation holds a
// capture or playback stream. Detection pauses while one does.
func (d *Daemon) voiceAudioActive() bool {
	apps := d.voiceAppIDs()
	if len(apps) == 0 {
		return false
	}
	for _, st := range d.audio.ActiveStreams() {
		for _, appID := range apps {
			if st.AppID != appID {
				continue
			}
			if d.cfg.WakewordTrace {
				d.logs.Debugf("wakeword", "", "gate held by stream=%s app=%s input=%t", st.StreamID, st.AppID, st.Input)
			}
			return true
		}
	}
	return false
}

// materializeWakewords rewrites the per-word list after an install or delete so
// the durable record keeps containing every installed word.
//
// A legacy nil list is normally left untouched, because materializing it would
// turn the legacy state into an explicit one and change which words are live.
// The exception is an install (freshID non-empty): a new word must start
// disabled, so the list is materialized with the existing words keeping their
// legacy live state and the new word switched off.
func (d *Daemon) materializeWakewords(installed []string, freshID string) {
	values := d.user.Get().Values
	if values.Wakewords == nil {
		if freshID == "" {
			return
		}
		next := make([]settings.WakewordBinding, 0, len(installed))
		for _, id := range installed {
			next = append(next, settings.WakewordBinding{ModelID: id, Enabled: id != freshID})
		}
		if err := d.writeUserSettings([]string{"wakewords"}, func(v *settings.UserSettings) {
			v.Wakewords = next
		}); err != nil {
			d.logs.Warnf("wakeword", "", "materializing the wakeword list: %v", err)
		}
		return
	}
	want := make(map[string]bool, len(installed))
	for _, id := range installed {
		want[id] = true
	}
	next := make([]settings.WakewordBinding, 0, len(installed))
	for _, entry := range values.Wakewords {
		if !want[entry.ModelID] {
			continue
		}
		next = append(next, entry)
	}
	present := make(map[string]bool, len(next))
	for _, entry := range next {
		present[entry.ModelID] = true
	}
	for _, id := range installed {
		if present[id] {
			continue
		}
		// A newly installed word starts disabled: it becomes live only when the
		// operator enables it, exactly like any other detection source.
		next = append(next, settings.WakewordBinding{ModelID: id})
	}
	if len(next) == len(values.Wakewords) {
		same := true
		for i := range next {
			if next[i] != values.Wakewords[i] {
				same = false
				break
			}
		}
		if same {
			return
		}
	}
	if err := d.writeUserSettings([]string{"wakewords"}, func(v *settings.UserSettings) {
		v.Wakewords = next
	}); err != nil {
		d.logs.Warnf("wakeword", "", "materializing the wakeword list: %v", err)
	}
}

// installedWakewordIDs lists the valid artifact ids, sorted.
func installedWakewordIDs(valid []wakeword.StoredModel) []string {
	out := make([]string, 0, len(valid))
	for _, model := range valid {
		out = append(out, model.ID)
	}
	return out
}

// wakewordInstalled reports whether id already has an artifact, so a retrain in
// place is never mistaken for a new word.
func wakewordInstalled(valid []wakeword.StoredModel, id string) bool {
	for _, model := range valid {
		if model.ID == id {
			return true
		}
	}
	return false
}

// newWakewordID derives an unused artifact id for a training run that did not
// name one. It is a stable lowercase file-name stem; the display name carries
// the spoken word.
func (d *Daemon) newWakewordID(valid []wakeword.StoredModel) string {
	taken := make(map[string]bool, len(valid))
	for _, model := range valid {
		taken[model.ID] = true
	}
	base := fmt.Sprintf("word.%x", time.Now().UnixMilli())
	id := base
	for i := 2; taken[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	return id
}

// trimModelID normalizes a caller-supplied id without changing it, so an id that
// only differs by whitespace is refused instead of silently renamed.
func trimModelID(id string) string { return strings.TrimSpace(id) }
