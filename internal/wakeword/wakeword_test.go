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
