package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/wakeword"
)

func newTrainingDaemon(t *testing.T) *Daemon {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	mgr := audio.NewManager(context.Background(), clock, logs, sim.Audio(), audio.Options{})
	t.Cleanup(func() {
		mgr.Close()
		sim.Close()
	})
	return &Daemon{cfg: Config{DataDir: t.TempDir()}, audio: mgr}
}

// recordFakeTakes stores canonical WAV files directly. Reading and grouping is
// what these tests cover; recording itself has its own muted-microphone test.
func recordFakeTakes(t *testing.T, d *Daemon, label string, n int) []string {
	t.Helper()
	dir := d.sampleDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, 1600)
	for i := range pcm {
		pcm[i] = int16((i * 37) % 1000)
	}
	ids := make([]string, 0, n)
	for i := range n {
		id := fmt.Sprintf("wk_test_%02d_%s", i, label)
		if err := writeWAV(filepath.Join(dir, id+".wav"), pcm); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func sampleIDs(ids []string) []string { return append([]string(nil), ids...) }

func TestStartWakewordEngineKeepsLegacyModelUsable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	defer sim.Close()
	mgr := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	defer mgr.Close()
	d := &Daemon{
		ctx: ctx, cfg: Config{DataDir: t.TempDir()}, audio: mgr,
		clock: clock, logs: logs,
		wake: wakeword.NewManager(ctx, clock, logs, nil),
	}
	// A model from the previous build has no input_audio provenance: it was
	// trained on the raw vendor tap. It must still load, stay enabled and run
	// on the now-mandatory processed tap, with the mismatch only reported.
	clf := wakeword.NewClassifier("word.personal", "word", "en", 7)
	clf.Threshold = 0.8
	raw, err := json.Marshal(clf)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.writeClassifier(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	d.startWakewordEngine()
	defer func() {
		if d.wakeCancel != nil {
			d.wakeCancel()
		}
	}()
	models := d.wake.Models()
	if len(models) != 1 || models[0].ID != "word.personal" || !models[0].Available {
		t.Fatalf("legacy model was not kept usable: %+v", models)
	}
}

func TestLoadSelectionUsesOnlySelectedTakes(t *testing.T) {
	d := newTrainingDaemon(t)
	word := recordFakeTakes(t, d, "word", 9)
	room := recordFakeTakes(t, d, "background", 6) // background filenames
	// Speech takes are background too; use distinct ids by copying files.
	speech := make([]string, 0, 3)
	for i := range 3 {
		id := fmt.Sprintf("wk_speech_%d_background", i)
		raw, err := os.ReadFile(filepath.Join(d.sampleDir(), room[i]+".wav"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d.sampleDir(), id+".wav"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		speech = append(speech, id)
	}
	sel := trainSelection{
		Display: "Маруся",
		Word:    sampleIDs(word[:8]),
		Room:    sampleIDs(room[3:]),
		Speech:  speech,
	}
	en, inputs, err := d.loadSelection(sel)
	if err != nil {
		t.Fatalf("loadSelection: %v", err)
	}
	if len(en.Takes) != 8 {
		t.Fatalf("selected 8 word takes, loaded %d", len(en.Takes))
	}
	if len(en.Ambient) != 3 || len(en.Speech) != 3 {
		t.Fatalf("ambient=%d speech=%d, want 3 and 3", len(en.Ambient), len(en.Speech))
	}
	if len(en.False) != 0 {
		t.Fatalf("unselected false takes loaded: %d", len(en.False))
	}
	if inputs.Tap != audio.CaptureProcessingTap || inputs.RawTakesConverted != 14 {
		t.Fatalf("inputs = %+v, want tap %q and 14 converted raw takes", inputs, audio.CaptureProcessingTap)
	}
	// The legacy take is converted in memory, not rewritten, and it is not
	// passed through unchanged.
	path := filepath.Join(d.sampleDir(), sel.Word[0]+".wav")
	legacy, err := readWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := d.audio.ProcessCaptureS16(legacy)
	if len(en.Takes[0]) != len(want) {
		t.Fatalf("converted take length = %d, want %d", len(en.Takes[0]), len(want))
	}
	same := true
	for i := range want {
		if en.Takes[0][i] != want[i] {
			t.Fatalf("converted sample %d = %d, want %d", i, en.Takes[0][i], want[i])
		}
		if en.Takes[0][i] != legacy[i] {
			same = false
		}
	}
	if same {
		t.Fatal("legacy raw take was used without conversion")
	}
	if _, err := os.Stat(audio.CaptureProvenancePath(path)); !os.IsNotExist(err) {
		t.Fatalf("conversion rewrote provenance on disk: %v", err)
	}
}

func declareProcessed(t *testing.T, d *Daemon, ids []string, tap string) {
	t.Helper()
	for _, id := range ids {
		path := filepath.Join(d.sampleDir(), id+".wav")
		if err := audio.WriteCaptureProvenance(path, audio.CaptureProvenance{
			ProcessingTap: tap, SourceEndpoint: audio.CaptureEndpoint, RateHz: sampleRate, Channels: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadSelectionUsesDeclaredProcessedTakesAsIs(t *testing.T) {
	d := newTrainingDaemon(t)
	word := recordFakeTakes(t, d, "word", 8)
	room := recordFakeTakes(t, d, "background", 6)
	declareProcessed(t, d, word, audio.CaptureProcessingTap)
	declareProcessed(t, d, room, audio.CaptureProcessingTap)
	sel := trainSelection{Display: "Маруся", Word: word, Room: room[:3], Speech: room[3:]}
	en, inputs, err := d.loadSelection(sel)
	if err != nil {
		t.Fatalf("loadSelection: %v", err)
	}
	if inputs.RawTakesConverted != 0 {
		t.Fatalf("declared processed takes were converted: %d", inputs.RawTakesConverted)
	}
	raw, err := readWAV(filepath.Join(d.sampleDir(), word[0]+".wav"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		if en.Takes[0][i] != raw[i] {
			t.Fatalf("processed take changed at sample %d: %d != %d", i, en.Takes[0][i], raw[i])
		}
	}
}

func TestLoadSelectionRejectsUnknownCaptureTap(t *testing.T) {
	d := newTrainingDaemon(t)
	word := recordFakeTakes(t, d, "word", 8)
	room := recordFakeTakes(t, d, "background", 6)
	declareProcessed(t, d, word, "framework.dsp.v2")
	sel := trainSelection{Display: "Маруся", Word: word, Room: room[:3], Speech: room[3:]}
	_, _, err := d.loadSelection(sel)
	if err == nil || !strings.Contains(err.Error(), "record a new take") {
		t.Fatalf("unknown tap was not refused explicitly: %v", err)
	}
	var bad badSelectionError
	if !errors.As(err, &bad) {
		t.Fatalf("error %v is not actionable to the caller", err)
	}
}

func TestLoadSelectionRejectsDuplicatesWrongKindsAndMissing(t *testing.T) {
	d := newTrainingDaemon(t)
	word := recordFakeTakes(t, d, "word", 8)
	room := recordFakeTakes(t, d, "background", 6)
	base := trainSelection{Display: "Маруся", Word: word, Room: room[:3], Speech: room[3:6]}

	cases := []struct {
		name string
		sel  func(trainSelection) trainSelection
		want string
	}{
		{"too few words", func(s trainSelection) trainSelection { s.Word = s.Word[:minWordTakes-1]; return s }, fmt.Sprintf("at least %d", minWordTakes)},
		{"too few room", func(s trainSelection) trainSelection { s.Room = s.Room[:2]; return s }, "at least 3"},
		{"too few speech", func(s trainSelection) trainSelection { s.Speech = s.Speech[:2]; return s }, "at least 3"},
		{"duplicate across kinds", func(s trainSelection) trainSelection { s.Room = append(s.Room, s.Word[0]); return s }, "selected twice"},
		{"wrong kind", func(s trainSelection) trainSelection { s.Word = replaceAt(s.Word, 0, s.Room[0]); return s }, "selected as wakeword"},
		{"missing", func(s trainSelection) trainSelection { s.Word = replaceAt(s.Word, 0, "wk_gone"); return s }, "not on this device"},
		{"empty word", func(s trainSelection) trainSelection { s.Display = "  "; return s }, "choose the wake word"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel := tc.sel(base)
			sel.Word = sampleIDs(sel.Word)
			sel.Room = sampleIDs(sel.Room)
			sel.Speech = sampleIDs(sel.Speech)
			_, _, err := d.loadSelection(sel)
			if err == nil {
				t.Fatal("expected a selection error")
			}
			var bad badSelectionError
			if !errors.As(err, &bad) {
				t.Fatalf("error %v is not a badSelectionError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func replaceAt(ids []string, i int, value string) []string {
	out := append([]string(nil), ids...)
	out[i] = value
	return out
}

func TestInstallRejectsInvalidWithoutTouchingModelFile(t *testing.T) {
	d := newTrainingDaemon(t)
	path := d.classifierPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"version":2,"id":"legacy"}`)
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.InstallClassifier(context.Background(), nil); err == nil {
		t.Fatal("nil classifier installed")
	}
	if err := d.InstallClassifier(context.Background(), &wakeword.Classifier{Version: 2, ID: "legacy"}); err == nil {
		t.Fatal("legacy classifier installed")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("failed install changed the model file: %s", got)
	}
}

func TestWriteClassifierRoundTripsAndRejectsLegacy(t *testing.T) {
	d := newTrainingDaemon(t)
	clf := wakeword.NewClassifier("word.personal", "Маруся", "ru", 7)
	clf.Threshold = 0.83
	raw, err := json.Marshal(clf)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.writeClassifier(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	loaded, err := wakeword.LoadClassifier(d.classifierPath())
	if err != nil {
		t.Fatalf("load installed classifier: %v", err)
	}
	if loaded.ID != clf.ID || loaded.Display != clf.Display || loaded.Threshold != clf.Threshold {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	if _, err := os.Stat(d.classifierPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary model file left behind")
	}

	legacy := []byte(`{"version":2,"id":"old"}`)
	if err := d.writeClassifier(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	_, err = wakeword.LoadClassifier(d.classifierPath())
	if err == nil || !strings.Contains(err.Error(), "retrain") {
		t.Fatalf("legacy model load error not actionable: %v", err)
	}
}

func TestTrainWakewordIsSerialized(t *testing.T) {
	d := newTrainingDaemon(t)
	wakeTrainMu.Lock()
	defer wakeTrainMu.Unlock()
	called := false
	_, err := d.TrainWakeword(context.Background(), trainSelection{Display: "Маруся"}, func(string, float64) {
		called = true
	})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second training call should be rejected, got %v", err)
	}
	if called {
		t.Fatal("rejected training reported progress")
	}
}

func TestTrainRPCStatusCodes(t *testing.T) {
	bad := trainRPCStatus(badSelection("record more takes"))
	if status.Code(bad) != codes.InvalidArgument {
		t.Fatalf("selection error code = %v", status.Code(bad))
	}
	if !strings.Contains(bad.Error(), "record more takes") {
		t.Fatalf("selection message lost: %v", bad)
	}
	cancelled := trainRPCStatus(context.Canceled)
	if status.Code(cancelled) != codes.Canceled {
		t.Fatalf("cancel code = %v", status.Code(cancelled))
	}
	gate := trainRPCStatus(fmt.Errorf("held-out recall 60%% below 80%%"))
	if status.Code(gate) != codes.FailedPrecondition {
		t.Fatalf("gate code = %v", status.Code(gate))
	}
	if !strings.Contains(gate.Error(), "held-out recall") {
		t.Fatalf("gate message lost: %v", gate)
	}
}

// newInstalledWakewordDaemon persists old as the installed model and starts the
// engine from it, so a cancelled install can be checked against both the file
// on disk and the running model list.
func newInstalledWakewordDaemon(t *testing.T, old *wakeword.Classifier) *Daemon {
	t.Helper()
	d := newTrainingDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	d.clock = ids.NewClock()
	logs, err := logbus.New(d.clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	d.logs = logs
	d.wake = wakeword.NewManager(ctx, d.clock, logs, nil)
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.writeClassifier(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	d.startWakewordEngine()
	t.Cleanup(func() {
		if d.wakeCancel != nil {
			d.wakeCancel()
		}
	})
	models := d.wake.Models()
	if len(models) != 1 || models[0].ID != old.ID || !models[0].Available {
		t.Fatalf("old model did not start: %+v", models)
	}
	return d
}

// blockingCancelContext stops InstallClassifier at one chosen ctx.Err check
// until the test cancels the parent, then reports the cancellation. It is a
// test double for an in-flight browser cancellation, not a production hook.
type blockingCancelContext struct {
	context.Context
	at      int
	calls   int
	reached chan struct{}
	release chan struct{}
}

func (c *blockingCancelContext) Err() error {
	c.calls++
	if c.calls < c.at {
		return nil
	}
	if c.calls == c.at {
		close(c.reached)
		<-c.release
	}
	return c.Context.Err()
}

// passingCancelGate reports a live context even after the parent is cancelled,
// which deterministically models a cancellation that lands after the final
// pre-rename check has already passed.
type passingCancelGate struct {
	context.Context
	at      int
	calls   int
	reached chan struct{}
	release chan struct{}
}

func (c *passingCancelGate) Err() error {
	c.calls++
	if c.calls == c.at {
		close(c.reached)
		<-c.release
	}
	return nil
}

func assertOldModelStillInstalled(t *testing.T, d *Daemon, old *wakeword.Classifier, oldRaw []byte) {
	t.Helper()
	got, err := os.ReadFile(d.classifierPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(oldRaw) {
		t.Fatalf("cancelled install replaced the installed model: %s", got)
	}
	if _, err := os.Stat(d.classifierPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("cancelled install left the temporary model behind")
	}
	loaded, err := wakeword.LoadClassifier(d.classifierPath())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != old.ID {
		t.Fatalf("persisted model = %q, want %q", loaded.ID, old.ID)
	}
	models := d.wake.Models()
	if len(models) != 1 || models[0].ID != old.ID || !models[0].Available {
		t.Fatalf("running model changed on a precommit cancel: %+v", models)
	}
}

func TestInstallClassifierCancelledBeforeCommitKeepsInstalledModel(t *testing.T) {
	old := wakeword.NewClassifier("word.personal", "Маруся", "ru", 7)
	old.Threshold = 0.8
	d := newInstalledWakewordDaemon(t, old)
	oldRaw, err := os.ReadFile(d.classifierPath())
	if err != nil {
		t.Fatal(err)
	}

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The second Err check is the one writeClassifier performs after the
	// temporary file exists and immediately before the rename. Blocking there
	// makes "cancel before the commit" deterministic instead of a timing race.
	ctx := &blockingCancelContext{
		Context: parent, at: 2,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	candidate := wakeword.NewClassifier("word.proposed", "Предложенная", "ru", 11)
	errc := make(chan error, 1)
	go func() { errc <- d.InstallClassifier(ctx, candidate) }()

	select {
	case <-ctx.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("install never reached the pre-rename cancellation check")
	}
	if _, err := os.Stat(d.classifierPath() + ".tmp"); err != nil {
		t.Fatalf("temporary model missing at the pre-rename check: %v", err)
	}
	cancel()
	close(ctx.release)
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("install error = %v, want context.Canceled", err)
	}
	assertOldModelStillInstalled(t, d, old, oldRaw)
}

func TestInstallClassifierCancelledDuringPreparationKeepsInstalledModel(t *testing.T) {
	old := wakeword.NewClassifier("word.personal", "Маруся", "ru", 7)
	old.Threshold = 0.8
	d := newInstalledWakewordDaemon(t, old)
	oldRaw, err := os.ReadFile(d.classifierPath())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	candidate := wakeword.NewClassifier("word.proposed", "Предложенная", "ru", 11)
	if err := d.InstallClassifier(ctx, candidate); !errors.Is(err, context.Canceled) {
		t.Fatalf("install error = %v, want context.Canceled", err)
	}
	assertOldModelStillInstalled(t, d, old, oldRaw)
}

func TestInstallClassifierCancelledAfterFinalGateStillCommits(t *testing.T) {
	old := wakeword.NewClassifier("word.personal", "Маруся", "ru", 7)
	old.Threshold = 0.8
	d := newInstalledWakewordDaemon(t, old)

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The install is held immediately after the last cancellation check. The
	// cancellation then lands too late to stop the commit, so success and an
	// installed model are the truthful outcome rather than "unchanged".
	ctx := &passingCancelGate{
		Context: parent, at: 2,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	candidate := wakeword.NewClassifier("word.proposed", "Предложенная", "ru", 11)
	errc := make(chan error, 1)
	go func() { errc <- d.InstallClassifier(ctx, candidate) }()

	select {
	case <-ctx.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("install never reached the pre-rename cancellation check")
	}
	cancel()
	close(ctx.release)
	if err := <-errc; err != nil {
		t.Fatalf("install that won the commit race returned %v", err)
	}
	loaded, err := wakeword.LoadClassifier(d.classifierPath())
	if err != nil {
		t.Fatalf("load committed classifier: %v", err)
	}
	if loaded.ID != candidate.ID {
		t.Fatalf("committed model = %q, want %q", loaded.ID, candidate.ID)
	}
	if _, err := os.Stat(d.classifierPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("committed install left the temporary model behind")
	}
	models := d.wake.Models()
	if len(models) != 1 || models[0].ID != candidate.ID || !models[0].Available {
		t.Fatalf("running model did not follow the committed install: %+v", models)
	}
}
