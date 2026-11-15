package wakeword

import (
	"context"
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// The stock device has no usable wakeword engine, but its talk button must still
// summon the assistant.
func TestManualOriginBypassesModelAvailability(t *testing.T) {
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	m := NewManager(context.Background(), ids.NewClock(), logs, []Model{
		{ID: "vendor.voice-agent", DisplayName: "Stock model", EngineID: "vendor", Available: false},
	})
	defer m.Close()
	var fired bool
	m.SetOnFire(func(Activation) { fired = true })
	if _, ok := m.Trigger("wakeword-engine", "vendor.voice-agent"); ok {
		t.Fatal("engine detection accepted an unavailable model")
	}
	act, ok := m.Trigger("play_pause_key", "")
	if !ok {
		t.Fatal("manual button activation was rejected")
	}
	if !fired || act.ModelID != "manual.button" {
		t.Fatalf("manual activation = %+v fired=%v", act, fired)
	}
}

// The browser's recent-detection list is real engine history, bounded and
// newest-first. Manual buttons and the API are activations, not detections.
func TestRecentDetectionsRecordOnlyEngineOrigin(t *testing.T) {
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	m := NewManager(context.Background(), ids.NewClock(), logs, []Model{
		{ID: "word.personal", DisplayName: "Hey Capsula", EngineID: "framework.embedding-mlp", Available: true},
	})
	defer m.Close()
	m.SetEnabled(true)
	if _, ok := m.Trigger("wakeword", "word.personal"); !ok {
		t.Fatal("engine activation rejected")
	}
	if _, ok := m.Trigger("play_pause_key", ""); !ok {
		t.Fatal("manual activation rejected")
	}
	if _, ok := m.Trigger("api", "word.personal"); !ok {
		t.Fatal("api activation rejected")
	}
	got := m.RecentDetections(20)
	if len(got) != 1 {
		t.Fatalf("detections = %d, want only the engine activation", len(got))
	}
	if got[0].ModelID != "word.personal" || got[0].DisplayName != "Hey Capsula" || got[0].UnixMS <= 0 {
		t.Fatalf("detection = %+v, want model, display name and wall-clock time", got[0])
	}
	for i := 0; i < recentDetectionLimit+3; i++ {
		if _, ok := m.Trigger("wakeword", "word.personal"); !ok {
			t.Fatal("engine activation rejected")
		}
	}
	if got := m.RecentDetections(100); len(got) != recentDetectionLimit {
		t.Fatalf("bounded detections = %d, want %d", len(got), recentDetectionLimit)
	}
	if got := m.RecentDetections(3); len(got) != 3 {
		t.Fatalf("limited detections = %d, want 3", len(got))
	}
}

// Dismissing a false detection removes exactly that entry, keeps the rest in
// newest-first order, and reports an unknown ID as absent.
func TestDismissDetection(t *testing.T) {
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	m := NewManager(context.Background(), ids.NewClock(), logs, []Model{
		{ID: "word.personal", DisplayName: "Hey Capsula", EngineID: "framework.embedding-mlp", Available: true},
	})
	defer m.Close()
	m.SetEnabled(true)
	for i := 0; i < 3; i++ {
		if _, ok := m.Trigger("wakeword", "word.personal"); !ok {
			t.Fatal("engine activation rejected")
		}
	}
	before := m.RecentDetections(20)
	if len(before) != 3 {
		t.Fatalf("detections = %d, want 3", len(before))
	}
	for _, d := range before {
		if d.ID == "" {
			t.Fatalf("detection %+v has no id", d)
		}
	}

	if !m.DismissDetection(before[1].ID) {
		t.Fatal("stored detection was not dismissed")
	}
	after := m.RecentDetections(20)
	if len(after) != 2 {
		t.Fatalf("detections after dismissal = %d, want 2", len(after))
	}
	if after[0].ID != before[0].ID || after[1].ID != before[2].ID {
		t.Fatalf("order changed after dismissal: %+v", after)
	}
	if m.DismissDetection(before[1].ID) {
		t.Fatal("dismissing an already-dismissed detection reported success")
	}
	if m.DismissDetection("") {
		t.Fatal("empty id was accepted")
	}
}
