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

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/settings"
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
	if err := os.MkdirAll(filepath.Dir(d.legacyClassifierPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.legacyClassifierPath(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	d.user = newTestUserStore(t, logs)
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
	path := d.classifierPath("word.legacy")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"version":2,"id":"word.legacy"}`)
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
	if err := d.writeClassifier(context.Background(), clf.ID, raw); err != nil {
		t.Fatal(err)
	}
	loaded, err := wakeword.LoadClassifier(d.classifierPath(clf.ID))
	if err != nil {
		t.Fatalf("load installed classifier: %v", err)
	}
	if loaded.ID != clf.ID || loaded.Display != clf.Display || loaded.Threshold != clf.Threshold {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	if _, err := os.Stat(d.classifierPath(clf.ID) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary model file left behind")
	}

	legacy := []byte(`{"version":2,"id":"word.legacy"}`)
	if err := d.writeClassifier(context.Background(), "word.legacy", legacy); err != nil {
		t.Fatal(err)
	}
	_, err = wakeword.LoadClassifier(d.classifierPath("word.legacy"))
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

	// A gate refusal is a precondition of the takes, and its status text names
	// the cause and every take the operator must fix.
	gate := trainRPCStatus(&wakeword.TrainingFailure{
		Causes: []wakeword.FailureCause{wakeword.CauseTuneGateFailed},
		Takes:  []wakeword.FailingTake{{Partition: "word", Index: 3, Cause: wakeword.CauseTuneGateFailed}},
		Splits: []wakeword.SplitMetrics{{Split: wakeword.SplitTune, WordRecordings: 2, Detected: 1}},
		Detail: "no threshold met the quality gate",
	})
	if status.Code(gate) != codes.FailedPrecondition {
		t.Fatalf("gate code = %v, want FailedPrecondition", status.Code(gate))
	}
	if !strings.Contains(gate.Error(), string(wakeword.CauseTuneGateFailed)) {
		t.Fatalf("gate message lost the cause: %v", gate)
	}
	if !strings.Contains(gate.Error(), "word #3") {
		t.Fatalf("gate message lost the failing take: %v", gate)
	}

	// An unusable selection is the caller's to fix.
	for _, cause := range []wakeword.FailureCause{
		wakeword.CauseTooFewWordTakes, wakeword.CauseDuplicateTakes,
		wakeword.CauseTakeTooShort, wakeword.CauseInsufficientHeldout,
		wakeword.CauseUnusableTake, wakeword.CauseInvalidRequest,
	} {
		err := trainRPCStatus(&wakeword.TrainingFailure{Causes: []wakeword.FailureCause{cause}, Detail: "x"})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s code = %v, want InvalidArgument", cause, status.Code(err))
		}
	}

	// Missing device inputs and trainer faults keep their own codes.
	for _, tc := range []struct {
		cause wakeword.FailureCause
		code  codes.Code
	}{
		{wakeword.CauseCorpusMissing, codes.FailedPrecondition},
		{wakeword.CauseCorpusUnusable, codes.FailedPrecondition},
		{wakeword.CauseConfiguration, codes.FailedPrecondition},
		{wakeword.CauseFitFailed, codes.Internal},
		{wakeword.CauseInternal, codes.Internal},
	} {
		err := trainRPCStatus(&wakeword.TrainingFailure{Causes: []wakeword.FailureCause{tc.cause}, Detail: "x"})
		if status.Code(err) != tc.code {
			t.Fatalf("%s code = %v, want %v", tc.cause, status.Code(err), tc.code)
		}
	}

	// An untyped fault from the trainer is INTERNAL, not a policy refusal.
	unknown := trainRPCStatus(fmt.Errorf("embedding frontend failed"))
	if status.Code(unknown) != codes.Internal {
		t.Fatalf("unknown fault code = %v, want Internal", status.Code(unknown))
	}
}

// TestTrainingFailureFrameCarriesStructuredDetail pins the terminal event that
// precedes the stream's error status.
func TestTrainingFailureFrameCarriesStructuredDetail(t *testing.T) {
	failure := &wakeword.TrainingFailure{
		Causes: []wakeword.FailureCause{wakeword.CauseFinalGateFailed, wakeword.CauseAuxiliaryGateFailed},
		Takes: []wakeword.FailingTake{
			{Partition: "word", Index: 2, Cause: wakeword.CauseFinalGateFailed, Detail: "missed"},
			{Partition: "room", Index: 1, Cause: wakeword.CauseFinalGateFailed, Detail: "false fired"},
		},
		Splits: []wakeword.SplitMetrics{
			{Split: wakeword.SplitFinalTest, WordRecordings: 2, Detected: 1, Recall: 0.5, FalseActivations: 1},
			{Split: wakeword.SplitAuxTune, FalseActivations: 2},
		},
		Threshold: 0.59,
		Detail:    "final test failed at threshold 0.590",
	}
	var frames []*frameworkv1.TrainWakewordEvent
	sendTrainingFailure(func(ev *frameworkv1.TrainWakewordEvent) error {
		frames = append(frames, ev)
		return nil
	}, failure, 0.93)
	if len(frames) != 1 {
		t.Fatalf("sent %d frames, want one terminal failure frame", len(frames))
	}
	frame := frames[0]
	if frame.GetStage() != "failed" {
		t.Fatalf("stage = %q, want failed", frame.GetStage())
	}
	if frame.GetFraction() != 0.93 {
		t.Fatalf("fraction = %v, want the last progress fraction", frame.GetFraction())
	}
	if frame.GetLog() != failure.Detail {
		t.Fatalf("log = %q", frame.GetLog())
	}
	structured := frame.GetFailure()
	if structured == nil {
		t.Fatal("terminal frame has no structured failure")
	}
	if len(structured.GetCauses()) != 2 || structured.GetCauses()[0] != string(wakeword.CauseFinalGateFailed) {
		t.Fatalf("causes = %v", structured.GetCauses())
	}
	if len(structured.GetTakes()) != 2 || structured.GetTakes()[0].GetIndex() != 2 ||
		structured.GetTakes()[1].GetPartition() != "room" {
		t.Fatalf("takes = %+v", structured.GetTakes())
	}
	if structured.GetThreshold() != 0.59 {
		t.Fatalf("threshold = %v", structured.GetThreshold())
	}
	if len(structured.GetSplits()) != 2 || structured.GetSplits()[0].GetDetected() != 1 ||
		structured.GetSplits()[1].GetFalseActivations() != 2 {
		t.Fatalf("splits = %+v", structured.GetSplits())
	}
}

// TestInstalledEventCarriesGateMetrics pins the success-side surface: the
// certificate stored in the artifact travels on the final event.
func TestInstalledEventCarriesGateMetrics(t *testing.T) {
	clf := wakeword.NewClassifier("test.word", "Test", "en", 7)
	if err := clf.Validate(); err != nil {
		t.Fatal(err)
	}
	// A model trained before metrics were recorded exposes no numbers.
	event := installedEvent(clf)
	if event.GetStage() != "installed" || event.GetClassifier() == nil {
		t.Fatalf("installed event = %+v", event)
	}
	if len(event.GetMetrics()) != 0 {
		t.Fatalf("legacy artifact reported metrics: %+v", event.GetMetrics())
	}

	clf.Threshold = 0.59
	clf.Trained = map[string]any{
		"metrics": map[string]any{
			"threshold":  0.59,
			"tune":       map[string]any{"word_recordings": 2, "detected": 2, "recall": 1.0, "false_activations": 0},
			"final_test": map[string]any{"word_recordings": 2, "detected": 2, "recall": 1.0, "false_activations": 0},
			"auxiliary_offpolicy": map[string]any{
				"tune":       map[string]any{"false_activations": 0},
				"final_test": map[string]any{"false_activations": 0},
			},
		},
	}
	event = installedEvent(clf)
	metrics := event.GetMetrics()
	if len(metrics) != 4 {
		t.Fatalf("metrics = %+v, want four splits", metrics)
	}
	if metrics[0].GetSplit() != wakeword.SplitTune || metrics[0].GetDetected() != 2 {
		t.Fatalf("tune metrics = %+v", metrics[0])
	}
	if metrics[3].GetSplit() != wakeword.SplitAuxFinalTest {
		t.Fatalf("last split = %q", metrics[3].GetSplit())
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
	d.user = newTestUserStore(t, logs)
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.writeClassifier(context.Background(), old.ID, raw); err != nil {
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
	got, err := os.ReadFile(d.classifierPath(old.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(oldRaw) {
		t.Fatalf("cancelled install replaced the installed model: %s", got)
	}
	if _, err := os.Stat(d.classifierPath(old.ID) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("cancelled install left the temporary model behind")
	}
	loaded, err := wakeword.LoadClassifier(d.classifierPath(old.ID))
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
	oldRaw, err := os.ReadFile(d.classifierPath(old.ID))
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
	if _, err := os.Stat(d.classifierPath(candidate.ID) + ".tmp"); err != nil {
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
	oldRaw, err := os.ReadFile(d.classifierPath(old.ID))
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
	loaded, err := wakeword.LoadClassifier(d.classifierPath(candidate.ID))
	if err != nil {
		t.Fatalf("load committed classifier: %v", err)
	}
	if loaded.ID != candidate.ID {
		t.Fatalf("committed model = %q, want %q", loaded.ID, candidate.ID)
	}
	if _, err := os.Stat(d.classifierPath(candidate.ID) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("committed install left the temporary model behind")
	}
	models := d.wake.Models()
	if len(models) != 2 {
		t.Fatalf("running models = %+v, want the old word and the new one", models)
	}
	// The pre-existing word keeps running; the new one is installed disabled
	// because the durable per-word list is legacy (nil) only in this helper...
	// both are live under the global switch there, and neither is bound.
	for _, model := range models {
		if !model.Available || model.AppID != "" {
			t.Fatalf("model after install = %+v", model)
		}
	}
}

// The settings layer caps the per-word list at MaxWakewordBindings, so a fit
// that would install one more artifact is refused before the expensive work:
// that artifact could never be named by a binding.
func TestTrainWakewordRefusesWhenEverySlotIsUsed(t *testing.T) {
	d := newWakewordTestDaemon(t)
	for i := 0; i < settings.MaxWakewordBindings; i++ {
		writeWakewordArtifact(t, d, fmt.Sprintf("word.n%d", i), fmt.Sprintf("Word %d", i), uint64(i+1))
	}
	_, err := d.TrainWakeword(context.Background(), trainSelection{Display: "New word"}, nil)
	var failure *wakeword.TrainingFailure
	if !asTrainingFailure(err, &failure) || failure.Cause() != wakeword.CauseConfiguration {
		t.Fatalf("training at capacity = %v, want a configuration refusal", err)
	}
	if !strings.Contains(failure.Detail, "delete a word before training another") {
		t.Fatalf("refusal is not actionable: %q", failure.Detail)
	}
	if !strings.Contains(failure.Detail, fmt.Sprintf("%d", settings.MaxWakewordBindings)) {
		t.Fatalf("refusal does not name the limit: %q", failure.Detail)
	}
	// Nothing was written for the refused run.
	valid, _ := d.rescanWakewords()
	if len(valid) != settings.MaxWakewordBindings {
		t.Fatalf("refused training changed the installed set: %d artifacts", len(valid))
	}
}

// An unusable id is the caller's mistake (INVALID_ARGUMENT), a running fit is a
// device precondition (FAILED_PRECONDITION), and a store fault is INTERNAL.
func TestDeleteWakewordRPCStatusCodes(t *testing.T) {
	bad := deleteWakewordRPCStatus(wakeword.ValidateModelID("Word Bad"))
	if status.Code(bad) != codes.InvalidArgument {
		t.Fatalf("invalid id code = %v", status.Code(bad))
	}
	if !strings.Contains(bad.Error(), "must match") {
		t.Fatalf("invalid id message lost its reason: %v", bad)
	}
	busy := deleteWakewordRPCStatus(fmt.Errorf("%w; delete the wakeword after it finishes", errWakeTrainingBusy))
	if status.Code(busy) != codes.FailedPrecondition {
		t.Fatalf("training-busy code = %v", status.Code(busy))
	}
	if !strings.Contains(busy.Error(), "training is running") {
		t.Fatalf("training-busy message lost its reason: %v", busy)
	}
	broken := deleteWakewordRPCStatus(errors.New("remove artifact: permission denied"))
	if status.Code(broken) != codes.Internal {
		t.Fatalf("store fault code = %v", status.Code(broken))
	}
}
