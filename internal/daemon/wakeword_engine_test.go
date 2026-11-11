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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/wakeword"
)

func newTrainingDaemon(t *testing.T) *Daemon {
	t.Helper()
	return &Daemon{cfg: Config{DataDir: t.TempDir()}}
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
	en, err := d.loadSelection(sel)
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
			_, err := d.loadSelection(sel)
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
	if err := d.InstallClassifier(nil); err == nil {
		t.Fatal("nil classifier installed")
	}
	if err := d.InstallClassifier(&wakeword.Classifier{Version: 2, ID: "legacy"}); err == nil {
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
	if err := d.writeClassifier(raw); err != nil {
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
	if err := d.writeClassifier(legacy); err != nil {
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
