package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/bus"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/presentations"
	"capsulad/internal/registry"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
	"capsulad/internal/wakeword"
)

// newTestUserStore builds an in-memory settings store with the documented
// defaults. Persisting would need a path and a running daemon.
func newTestUserStore(t *testing.T, logs *logbus.Bus) *settings.UserStore {
	t.Helper()
	store, err := settings.OpenUser("", logs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// newWakewordTestDaemon builds the smallest daemon the wakeword model layer
// needs: a data directory, a settings store, an audio manager with the processed
// tap and a wakeword manager.
func newWakewordTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	t.Cleanup(func() { _ = sim.Close() })
	mgr := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	t.Cleanup(mgr.Close)
	pres := presentations.NewManager(clock, logs, nil)
	d := &Daemon{
		ctx: ctx, cfg: Config{DataDir: t.TempDir()},
		clock: clock, logs: logs, audio: mgr, dev: sim,
		pres: pres, router: routing.New(clock, logs, pres),
	}
	d.user = newTestUserStore(t, logs)
	d.wake = wakeword.NewManager(ctx, clock, logs, nil)
	d.wake.SetEnabled(true)
	t.Cleanup(func() {
		if d.wakeCancel != nil {
			d.wakeCancel()
		}
	})
	return d
}

// writeWakewordArtifact stores one valid artifact through the daemon's store.
func writeWakewordArtifact(t *testing.T, d *Daemon, id, display string, seed uint64) []byte {
	t.Helper()
	clf := wakeword.NewClassifier(id, display, "ru", seed)
	raw, err := json.Marshal(clf)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.writeClassifier(context.Background(), id, raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func setWakewords(t *testing.T, d *Daemon, list []settings.WakewordBinding) {
	t.Helper()
	fields := []string{"wakewords"}
	if list == nil {
		// nil is the legacy state; a whole-list patch with the field present
		// writes exactly what the caller supplied.
		fields = []string{"wakewords"}
	}
	if _, err := d.user.Update(0, settings.UserSettings{Wakewords: list}, fields); err != nil {
		t.Fatal(err)
	}
	d.refreshWakewordModels()
}

func modelByID(t *testing.T, d *Daemon, id string) wakeword.Model {
	t.Helper()
	for _, model := range d.wake.Models() {
		if model.ID == id {
			return model
		}
	}
	t.Fatalf("model %q is not listed: %+v", id, d.wake.Models())
	return wakeword.Model{}
}

// The previously installed single artifact is moved into the models directory on
// the first scan: same bytes, no rewrite, no retrain, no settings write.
func TestLegacyModelMigratesOnFirstScan(t *testing.T) {
	d := newWakewordTestDaemon(t)
	clf := wakeword.NewClassifier("word.personal", "Маруся", "ru", 7)
	clf.Threshold = 0.59
	legacyRaw, err := json.Marshal(clf)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(d.cfg.DataDir, "wakeword", "model.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, legacyRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	settingsBefore := d.user.Get()

	d.startWakewordEngine()

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy artifact was not moved: %v", err)
	}
	moved := filepath.Join(d.cfg.DataDir, "wakeword", "models", "word.personal.json")
	got, err := os.ReadFile(moved)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(legacyRaw) {
		t.Fatal("migration changed the artifact bytes")
	}
	settingsAfter := d.user.Get()
	if settingsAfter.Revision != settingsBefore.Revision || settingsAfter.Values.Wakewords != nil {
		t.Fatalf("migration wrote settings: %+v -> %+v", settingsBefore, settingsAfter)
	}
	model := modelByID(t, d, "word.personal")
	if !model.Available || !model.Enabled || model.AppID != "" {
		t.Fatalf("legacy model after migration = %+v, want available, live and unbound", model)
	}
	if model.Threshold != 0.59 {
		t.Fatalf("threshold = %v", model.Threshold)
	}
}

// The legacy state must reproduce the previous behaviour exactly: the global
// switch decides, every installed word is live, nothing is bound to an app.
func TestLegacyBindingsKeepEveryInstalledWordLive(t *testing.T) {
	d := newWakewordTestDaemon(t)
	writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	writeWakewordArtifact(t, d, "word.beta", "Beta", 2)

	d.refreshWakewordModels()
	for _, id := range []string{"word.alpha", "word.beta"} {
		model := modelByID(t, d, id)
		if !model.Enabled || !model.Available || model.AppID != "" {
			t.Fatalf("%s = %+v, want live and unbound under the legacy state", id, model)
		}
	}

	// The global switch still turns everything off.
	if _, err := d.user.Update(0, settings.UserSettings{WakewordEnabled: false}, []string{"wakeword_enabled"}); err != nil {
		t.Fatal(err)
	}
	d.wake.SetEnabled(false)
	d.refreshWakewordModels()
	for _, id := range []string{"word.alpha", "word.beta"} {
		if model := modelByID(t, d, id); model.Enabled {
			t.Fatalf("%s still enabled with the global switch off: %+v", id, model)
		}
	}
}

// A present per-word list is authoritative: the entry decides enablement and the
// binding, a missing entry is disabled, and the global switch still wins.
func TestExplicitBindingsDecorateModels(t *testing.T) {
	d := newWakewordTestDaemon(t)
	writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	writeWakewordArtifact(t, d, "word.beta", "Beta", 2)
	writeWakewordArtifact(t, d, "word.gamma", "Gamma", 3)
	setWakewords(t, d, []settings.WakewordBinding{
		{ModelID: "word.alpha", AppID: "app.lights", Enabled: true},
		{ModelID: "word.beta", Enabled: false},
	})

	alpha := modelByID(t, d, "word.alpha")
	if !alpha.Enabled || alpha.AppID != "app.lights" {
		t.Fatalf("bound word = %+v", alpha)
	}
	beta := modelByID(t, d, "word.beta")
	if beta.Enabled || beta.AppID != "" {
		t.Fatalf("disabled word = %+v", beta)
	}
	gamma := modelByID(t, d, "word.gamma")
	if gamma.Enabled {
		t.Fatalf("word missing from the list is live: %+v", gamma)
	}
	if gamma.Detail == "" {
		t.Fatal("a word missing from the list has no explanation")
	}

	// The global switch gates an enabled entry.
	if _, err := d.user.Update(0, settings.UserSettings{WakewordEnabled: false}, []string{"wakeword_enabled"}); err != nil {
		t.Fatal(err)
	}
	d.refreshWakewordModels()
	if alpha := modelByID(t, d, "word.alpha"); alpha.Enabled || alpha.Detail == "" {
		t.Fatalf("global switch did not gate the entry: %+v", alpha)
	}
	// ...and a firing check agrees: the word cannot activate.
	if _, ok := d.wake.Trigger("wakeword", "word.alpha"); ok {
		t.Fatal("disabled word activated")
	}
}

// An unreadable artifact is reported and never silently replaced or deleted, and
// it is never advertised as available.
func TestUnreadableArtifactIsReported(t *testing.T) {
	d := newWakewordTestDaemon(t)
	writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	broken := d.classifierPath("word.broken")
	if err := os.WriteFile(broken, []byte(`{"version":3,`), 0o644); err != nil {
		t.Fatal(err)
	}
	d.refreshWakewordModels()
	model := modelByID(t, d, "word.broken")
	if model.Available || model.Enabled || model.Detail == "" {
		t.Fatalf("unreadable artifact advertised as usable: %+v", model)
	}
	if _, err := os.Stat(broken); err != nil {
		t.Fatalf("refused artifact was deleted: %v", err)
	}
	if _, ok := d.wake.Trigger("wakeword", "word.broken"); ok {
		t.Fatal("unreadable artifact activated")
	}
}

// Installing one word must leave every other artifact byte-identical, must not
// re-gate another word and must install the new word disabled.
func TestInstallLeavesOtherArtifactsByteIdentical(t *testing.T) {
	d := newWakewordTestDaemon(t)
	alpha := writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	beta := writeWakewordArtifact(t, d, "word.beta", "Beta", 2)
	setWakewords(t, d, []settings.WakewordBinding{
		{ModelID: "word.alpha", AppID: "app.lights", Enabled: true},
		{ModelID: "word.beta", Enabled: true},
	})
	d.startWakewordEngine()

	// Retrain word.beta in place: only its own file may change.
	replacement := wakeword.NewClassifier("word.beta", "Beta v2", "ru", 22)
	if err := d.InstallClassifier(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(d.classifierPath("word.alpha")); err != nil || string(got) != string(alpha) {
		t.Fatalf("installing another word changed word.alpha: %v", err)
	}
	updated, err := os.ReadFile(d.classifierPath("word.beta"))
	if err != nil {
		t.Fatal(err)
	}
	if string(updated) == string(beta) {
		t.Fatal("retraining in place did not replace the artifact")
	}
	values := d.user.Get().Values
	if len(values.Wakewords) != 2 {
		t.Fatalf("per-word list changed on retrain: %+v", values.Wakewords)
	}
	for _, entry := range values.Wakewords {
		if entry.ModelID == "word.alpha" && (!entry.Enabled || entry.AppID != "app.lights") {
			t.Fatalf("retraining word.beta re-gated word.alpha: %+v", entry)
		}
		if entry.ModelID == "word.beta" && !entry.Enabled {
			t.Fatalf("retraining in place disabled the word: %+v", entry)
		}
	}

	// A brand-new word is appended to the list disabled and stays unbound.
	fresh := wakeword.NewClassifier("word.gamma", "Gamma", "ru", 33)
	if err := d.InstallClassifier(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	values = d.user.Get().Values
	if len(values.Wakewords) != 3 {
		t.Fatalf("new word was not materialized: %+v", values.Wakewords)
	}
	for _, entry := range values.Wakewords {
		if entry.ModelID == "word.gamma" && (entry.Enabled || entry.AppID != "") {
			t.Fatalf("new word is not installed disabled: %+v", entry)
		}
	}
	gamma := modelByID(t, d, "word.gamma")
	if gamma.Enabled {
		t.Fatalf("new word is live before it was enabled: %+v", gamma)
	}
	if got, err := os.ReadFile(d.classifierPath("word.alpha")); err != nil || string(got) != string(alpha) {
		t.Fatalf("installing a third word changed word.alpha: %v", err)
	}
}

// Deleting a word removes its artifact and its entry, keeps every other word and
// never deletes the recordings.
func TestDeleteWakewordClassifierKeepsOtherWordsAndTakes(t *testing.T) {
	d := newWakewordTestDaemon(t)
	alpha := writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	writeWakewordArtifact(t, d, "word.beta", "Beta", 2)
	setWakewords(t, d, []settings.WakewordBinding{
		{ModelID: "word.alpha", Enabled: true},
		{ModelID: "word.beta", Enabled: true},
	})
	takes := recordFakeTakes(t, d, "word", 3)

	if err := d.DeleteWakewordClassifier("word.beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.classifierPath("word.beta")); !os.IsNotExist(err) {
		t.Fatal("deleted artifact still exists")
	}
	if got, err := os.ReadFile(d.classifierPath("word.alpha")); err != nil || string(got) != string(alpha) {
		t.Fatalf("deleting one word changed another: %v", err)
	}
	values := d.user.Get().Values
	if len(values.Wakewords) != 1 || values.Wakewords[0].ModelID != "word.alpha" {
		t.Fatalf("per-word list after delete = %+v", values.Wakewords)
	}
	for _, id := range takes {
		if _, err := os.Stat(filepath.Join(d.sampleDir(), id+".wav")); err != nil {
			t.Fatalf("deleting a word removed a recording: %v", err)
		}
	}
	samples, err := d.listSamples()
	if err != nil || len(samples) != len(takes) {
		t.Fatalf("samples after delete = %d, %v", len(samples), err)
	}

	// Invalid ids are refused before touching anything.
	if err := d.DeleteWakewordClassifier("../etc/passwd"); err == nil {
		t.Fatal("invalid model id accepted")
	}
}

// A delete must not race a running fit: the training lock is the same mutex.
func TestDeleteWakewordClassifierRefusedWhileTraining(t *testing.T) {
	d := newWakewordTestDaemon(t)
	raw := writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	wakeTrainMu.Lock()
	defer wakeTrainMu.Unlock()
	if err := d.DeleteWakewordClassifier("word.alpha"); err == nil ||
		!strings.Contains(err.Error(), "training is running") {
		t.Fatalf("delete during training = %v, want a refusal", err)
	}
	if got, err := os.ReadFile(d.classifierPath("word.alpha")); err != nil || string(got) != string(raw) {
		t.Fatalf("refused delete touched the artifact: %v", err)
	}
}

// Training must target the requested model and refuse an invalid id before any
// expensive work starts.
func TestTrainWakewordRejectsInvalidModelID(t *testing.T) {
	d := newWakewordTestDaemon(t)
	_, err := d.TrainWakeword(context.Background(), trainSelection{ModelID: "Word Bad", Display: "Маруся"}, nil)
	var failure *wakeword.TrainingFailure
	if !asTrainingFailure(err, &failure) || failure.Cause() != wakeword.CauseInvalidRequest {
		t.Fatalf("invalid model id error = %v, want an invalid_request refusal", err)
	}
	if d.user.Get().Revision != 1 {
		t.Fatal("refused training wrote settings")
	}
}

func asTrainingFailure(err error, out **wakeword.TrainingFailure) bool {
	if err == nil {
		return false
	}
	failure, ok := err.(*wakeword.TrainingFailure)
	if ok {
		*out = failure
	}
	return ok
}

// A generated id never collides with an installed one.
func TestNewWakewordIDIsUnique(t *testing.T) {
	d := newWakewordTestDaemon(t)
	first := d.newWakewordID(nil)
	if err := wakeword.ValidateModelID(first); err != nil {
		t.Fatalf("generated id %q is invalid: %v", first, err)
	}
	valid := []wakeword.StoredModel{{ID: first}}
	second := d.newWakewordID(valid)
	if second == first {
		t.Fatalf("generated id %q collides with an installed artifact", second)
	}
	if err := wakeword.ValidateModelID(second); err != nil {
		t.Fatalf("generated second id %q is invalid: %v", second, err)
	}
}

// Delivery targets the bound app, falls back to the selected assistant only for
// an unbound word, and never reroutes a bound word onto another app.
func TestWakewordDeliveryTarget(t *testing.T) {
	readyAll := func(string) bool { return true }
	readyNone := func(string) bool { return false }
	cases := []struct {
		name     string
		bound    string
		selected string
		ready    func(string) bool
		wantApp  string
		wantWhy  string
	}{
		{"bound", "app.lights", "app.assistant", readyAll, "app.lights", ""},
		{"unbound", "", "app.assistant", readyAll, "app.assistant", ""},
		{"bound unready is not rerouted", "app.lights", "app.assistant", func(id string) bool {
			return id == "app.assistant"
		}, "", dropBoundUnready},
		{"unbound unready", "", "app.assistant", readyNone, "", dropSelectedUnready},
		{"nothing selected", "", "", readyAll, "", dropNoSelection},
		{"bound with no selection", "app.lights", "", readyAll, "app.lights", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, why := wakewordDeliveryTarget(tc.bound, tc.selected, tc.ready)
			if app != tc.wantApp || why != tc.wantWhy {
				t.Fatalf("target = (%q, %q), want (%q, %q)", app, why, tc.wantApp, tc.wantWhy)
			}
		})
	}
}

// Detection pauses while any app a word can address holds audio, not only the
// selected assistant.
func TestVoiceAppIDsCoversSelectedAndBoundApps(t *testing.T) {
	d := newWakewordTestDaemon(t)
	writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	writeWakewordArtifact(t, d, "word.beta", "Beta", 2)
	writeWakewordArtifact(t, d, "word.gamma", "Gamma", 3)
	setWakewords(t, d, []settings.WakewordBinding{
		{ModelID: "word.alpha", AppID: "app.lights", Enabled: true},
		{ModelID: "word.beta", AppID: "app.lights", Enabled: false},
		{ModelID: "word.gamma", AppID: "app.music", Enabled: true},
	})
	if _, err := d.user.Update(0, settings.UserSettings{DefaultAssistantAppID: "app.assistant"}, []string{"default_assistant_app_id"}); err != nil {
		t.Fatal(err)
	}
	// The router has no selection for the assistant role in this harness, so the
	// persisted default is what names it.
	got := d.voiceAppIDs()
	want := []string{"app.assistant", "app.lights", "app.music"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("voice apps = %v, want %v", got, want)
	}
}

// The settings patch is a whole-list replacement and the store refuses a shape it
// cannot represent.
func TestWakewordsSettingsValidation(t *testing.T) {
	d := newWakewordTestDaemon(t)
	over := make([]settings.WakewordBinding, 0, settings.MaxWakewordBindings+1)
	for i := 0; i <= settings.MaxWakewordBindings; i++ {
		over = append(over, settings.WakewordBinding{ModelID: "word.model" + string(rune('a'+i))})
	}
	if _, err := d.user.Update(0, settings.UserSettings{Wakewords: over}, []string{"wakewords"}); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Fatalf("over-long list = %v", err)
	}
	if _, err := d.user.Update(0, settings.UserSettings{Wakewords: []settings.WakewordBinding{
		{ModelID: "word.a"}, {ModelID: "word.a"},
	}}, []string{"wakewords"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate list = %v", err)
	}
	// The legacy state survives a reload unchanged (nil, not an empty list).
	snap, err := d.user.Update(0, settings.UserSettings{}, []string{"wakeword_enabled"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Values.Wakewords != nil {
		t.Fatalf("legacy state became %+v", snap.Values.Wakewords)
	}
}

// A bound app is part of the provider selection: it is recovered after a crash
// and only deselection (or unbinding) stops it.
func TestBoundAppCountsAsSelectedProvider(t *testing.T) {
	d := newWakewordTestDaemon(t)
	reg, err := registry.Open(t.TempDir(), d.logs)
	if err != nil {
		t.Fatal(err)
	}
	d.reg = reg
	for _, id := range []string{"app.lights", "app.music"} {
		if err := reg.RegisterBuiltin(&registry.App{Manifest: &manifest.Manifest{
			ID: id, Name: id, Version: "1.0.0",
			Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	setWakewords(t, d, []settings.WakewordBinding{
		{ModelID: "word.alpha", AppID: "app.lights", Enabled: true},
		{ModelID: "word.beta", Enabled: true},
	})
	if !d.isSelectedDefault("app.lights") {
		t.Fatal("a bound app is not treated as selected, so it would not be recovered")
	}
	if d.isSelectedDefault("app.music") {
		t.Fatal("an unbound app is treated as selected")
	}
	// Unbinding is what stops it.
	setWakewords(t, d, []settings.WakewordBinding{{ModelID: "word.alpha", Enabled: true}})
	if d.isSelectedDefault("app.lights") {
		t.Fatal("an unbound app is still treated as selected")
	}
}

// Training a second word on a device that still runs in the legacy state must
// materialize the list with the new word switched off. The existing word keeps
// its legacy live state, so upgrading does not silently pause detection.
func TestFreshInstallUnderLegacyKeepsExistingWordLiveAndNewWordOff(t *testing.T) {
	d := newWakewordTestDaemon(t)
	writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	d.refreshWakewordModels()
	if model := modelByID(t, d, "word.alpha"); !model.Enabled {
		t.Fatalf("the only installed word is not live before the install: %+v", model)
	}

	fresh := wakeword.NewClassifier("word.beta", "Beta", "ru", 2)
	if err := d.InstallClassifier(context.Background(), fresh); err != nil {
		t.Fatalf("install: %v", err)
	}
	values := d.user.Get().Values
	if values.Wakewords == nil {
		t.Fatal("a fresh install left the legacy state, so the new word inherited enablement")
	}
	enabled := map[string]bool{}
	for _, entry := range values.Wakewords {
		enabled[entry.ModelID] = entry.Enabled
	}
	if len(values.Wakewords) != 2 || !enabled["word.alpha"] || enabled["word.beta"] {
		t.Fatalf("materialized list = %+v, want alpha live and beta off", values.Wakewords)
	}
	if model := modelByID(t, d, "word.alpha"); !model.Enabled {
		t.Fatalf("the existing word lost its live state on install: %+v", model)
	}
	if model := modelByID(t, d, "word.beta"); model.Enabled {
		t.Fatalf("the freshly installed word is already live: %+v", model)
	}
}

// Materializing the list after a delete or install drops entries whose artifact
// is gone: an entry can never name a word that is not installed.
func TestMaterializeWakewordsDropsUninstalledEntry(t *testing.T) {
	d := newWakewordTestDaemon(t)
	setWakewords(t, d, []settings.WakewordBinding{
		{ModelID: "word.alpha", AppID: "app.lights", Enabled: true},
		{ModelID: "word.beta", Enabled: true},
	})
	d.materializeWakewords([]string{"word.alpha"}, "")

	values := d.user.Get().Values
	if values.Wakewords == nil {
		t.Fatal("materialize collapsed the list to the legacy state")
	}
	if len(values.Wakewords) != 1 || values.Wakewords[0].ModelID != "word.alpha" {
		t.Fatalf("list keeps an entry for an uninstalled word: %+v", values.Wakewords)
	}
	if !values.Wakewords[0].Enabled || values.Wakewords[0].AppID != "app.lights" {
		t.Fatalf("materialize rewrote the surviving entry: %+v", values.Wakewords[0])
	}
}

// Deleting the last word must leave an explicit empty list behind, not the
// legacy state in which the next installed word would be live.
func TestDeleteLastWordKeepsExplicitEmptyList(t *testing.T) {
	d := newWakewordTestDaemon(t)
	writeWakewordArtifact(t, d, "word.alpha", "Alpha", 1)
	setWakewords(t, d, []settings.WakewordBinding{{ModelID: "word.alpha", Enabled: true}})

	if err := d.DeleteWakewordClassifier("word.alpha"); err != nil {
		t.Fatal(err)
	}
	values := d.user.Get().Values
	if values.Wakewords == nil {
		t.Fatal("deleting the last word restored the legacy state")
	}
	if len(values.Wakewords) != 0 {
		t.Fatalf("list after deleting the last word = %+v", values.Wakewords)
	}
	if snap := d.user.Get(); snap.Values.Wakewords == nil {
		t.Fatal("a later clone collapsed the explicit empty list")
	}
}

// A legacy artifact the store refuses is advertised as needing attention rather
// than hidden behind "no wakeword yet": the operator still has that file.
func TestUnreadableLegacyArtifactIsAdvertised(t *testing.T) {
	d := newWakewordTestDaemon(t)
	legacy := filepath.Join(d.cfg.DataDir, "wakeword", "model.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	// A v2-shaped document: present, loadable JSON, and refused because it
	// predates the embedding classifier and must be retrained.
	if err := os.WriteFile(legacy, []byte(`{"version":2,"id":"word.legacy"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	d.refreshWakewordModels()

	model := modelByID(t, d, "model")
	if model.Available || model.Enabled {
		t.Fatalf("refused legacy artifact advertised as usable: %+v", model)
	}
	if model.Detail == "" {
		t.Fatal("refused legacy artifact has no explanation")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("refused legacy artifact was moved or deleted: %v", err)
	}
}

// fakeActivationStream is one app-bus connection the test drives directly: the
// test pushes outbound frames and reads the inbound activations the daemon sends.
type fakeActivationStream struct {
	grpc.ServerStream
	ctx context.Context
	in  chan *frameworkv1.AppOutbound
	out chan *frameworkv1.AppInbound
}

func (s *fakeActivationStream) Recv() (*frameworkv1.AppOutbound, error) {
	select {
	case msg, ok := <-s.in:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-s.ctx.Done():
		return nil, io.EOF
	}
}

func (s *fakeActivationStream) Send(msg *frameworkv1.AppInbound) error {
	select {
	case s.out <- msg:
		return nil
	case <-s.ctx.Done():
		return io.EOF
	}
}

func (s *fakeActivationStream) Context() context.Context { return s.ctx }

// connectFakeApp registers one ready app on the daemon's bus and returns the
// stream its inbound frames arrive on.
func connectFakeApp(t *testing.T, d *Daemon, appID, runID string) *fakeActivationStream {
	t.Helper()
	stream := &fakeActivationStream{
		ctx: d.ctx, in: make(chan *frameworkv1.AppOutbound, 8), out: make(chan *frameworkv1.AppInbound, 8),
	}
	go func() { _ = d.bus.Connect(stream) }()
	stream.in <- &frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_Hello{Hello: &frameworkv1.AppHello{RunId: runID}}}
	stream.in <- &frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_Ready{Ready: &frameworkv1.AppReady{}}}
	waitFor(t, 2*time.Second, appID+" becomes ready on the bus", func() bool {
		ready, ok := d.bus.ReadyApp(appID)
		return ok && ready == runID
	})
	return stream
}

// newBusWakewordDaemon adds an app bus to the wakeword harness, so delivery can
// be observed end to end instead of only through the target resolver.
func newBusWakewordDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := newWakewordTestDaemon(t)
	d.bus = bus.New(d.logs, func(runID string) (string, int, bool) {
		switch runID {
		case "run.lights":
			return "app.lights", 100, true
		case "run.assistant":
			return "app.assistant", 101, true
		}
		return "", 0, false
	}, bus.Hooks{})
	return d
}

// A bound activation reaches the bound app through the real bus, carrying the
// delivery target in app_id, and an unready bound app is dropped instead of
// being rerouted to the ready selected assistant.
func TestDeliverActivationThroughBus(t *testing.T) {
	d := newBusWakewordDaemon(t)
	if _, err := d.user.Update(0, settings.UserSettings{DefaultAssistantAppID: "app.assistant"}, []string{"default_assistant_app_id"}); err != nil {
		t.Fatal(err)
	}
	assistant := connectFakeApp(t, d, "app.assistant", "run.assistant")
	lights := connectFakeApp(t, d, "app.lights", "run.lights")

	expires := d.clock.NowNS() + uint64(5*time.Second)
	d.deliverActivation(wakeword.Activation{
		ID: "act.bound", Origin: "api", ModelID: "word.alpha", AppID: "app.lights", ExpiresNS: expires,
	})
	select {
	case msg := <-lights.out:
		act := msg.GetActivation()
		if act == nil {
			t.Fatalf("delivered frame is not an activation: %+v", msg)
		}
		if act.GetActivationId() != "act.bound" || act.GetAppId() != "app.lights" {
			t.Fatalf("bound activation = %+v, want id and app_id set", act)
		}
		if act.GetMatchedModelId() != "word.alpha" {
			t.Fatalf("matched model = %q", act.GetMatchedModelId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the bound app received no activation")
	}

	// A word bound to an app that is not ready is dropped, never rerouted.
	d.deliverActivation(wakeword.Activation{
		ID: "act.unready", Origin: "api", ModelID: "word.gone", AppID: "app.gone", ExpiresNS: expires,
	})
	select {
	case msg := <-assistant.out:
		t.Fatalf("an unready bound activation was rerouted to the selected assistant: %+v", msg.GetActivation())
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(logText(d), "bound to app.gone, which is not ready (not rerouted)") {
		t.Fatalf("drop reason was not logged:\n%s", logText(d))
	}
}

// A manual trigger carries no matched model. The wake-word manager synthesizes a
// placeholder id for its own bookkeeping; an app that reads a model id as
// keyword evidence would then claim a keyword on a key press, and the vendor
// service answers "keyword not confirmed" instead of transcribing the phrase.
func TestDeliverManualActivationCarriesNoMatchedModel(t *testing.T) {
	d := newBusWakewordDaemon(t)
	if _, err := d.user.Update(0, settings.UserSettings{DefaultAssistantAppID: "app.assistant"}, []string{"default_assistant_app_id"}); err != nil {
		t.Fatal(err)
	}
	assistant := connectFakeApp(t, d, "app.assistant", "run.assistant")
	expires := d.clock.NowNS() + uint64(5*time.Second)

	d.deliverActivation(wakeword.Activation{
		ID: "act.touch", Origin: "touch", ModelID: "manual.button", ExpiresNS: expires,
	})
	select {
	case msg := <-assistant.out:
		act := msg.GetActivation()
		if act == nil {
			t.Fatalf("delivered frame is not an activation: %+v", msg)
		}
		if act.GetOrigin() != frameworkv1.ActivationOrigin_ACTIVATION_TOUCH {
			t.Fatalf("origin = %v, want touch", act.GetOrigin())
		}
		if got := act.GetMatchedModelId(); got != "" {
			t.Fatalf("manual activation carried matched model %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the selected assistant received no activation")
	}
}

// A repeated proto field decodes as nil both when it was absent and when it was
// empty, so the RPC layer has to use the field mask to represent "no word is
// live": an update that owns `wakewords` and lists nothing writes the explicit
// empty list, while an update that ignores the field leaves the state alone.
func TestUpdateSettingsOwnedEmptyWakewordsIsExplicit(t *testing.T) {
	d := newWakewordTestDaemon(t)
	svc := &settingsService{d: d}
	res, err := svc.UpdateSettings(context.Background(), &frameworkv1.UpdateSettingsRequest{
		Values:       &frameworkv1.UserSettings{},
		UpdateFields: []string{"wakewords"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.GetValues().GetWakewords() == nil {
		t.Fatal("the snapshot collapsed the explicit empty list")
	}
	values := d.user.Get().Values
	if values.Wakewords == nil || len(values.Wakewords) != 0 {
		t.Fatalf("stored list = %#v, want an explicit empty list", values.Wakewords)
	}

	// An update that does not own the field must not reset it to legacy.
	if _, err := svc.UpdateSettings(context.Background(), &frameworkv1.UpdateSettingsRequest{
		Values:       &frameworkv1.UserSettings{MasterVolume: 0.5},
		UpdateFields: []string{"master_volume"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := d.user.Get().Values.Wakewords; got == nil || len(got) != 0 {
		t.Fatalf("an unrelated update changed the list to %#v", got)
	}
	// ...and re-opening the store keeps it.
	reopened := d.user
	if got := reopened.Get().Values.Wakewords; got == nil {
		t.Fatal("a clone turned the explicit empty list into the legacy state")
	}
}
