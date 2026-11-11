package daemon

import (
	"context"
	"strings"
	"testing"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

func TestRecordSampleRejectsMutedMicrophone(t *testing.T) {
	ctx := context.Background()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	defer sim.Close()
	manager := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	defer manager.Close()
	manager.SetMuted(true)
	d := &Daemon{ctx: ctx, audio: manager}
	_, err = d.RecordSample(ctx, "word", 400)
	if err == nil || !strings.Contains(err.Error(), "microphone is muted") {
		t.Fatalf("muted recording should fail immediately, got %v", err)
	}
	d.wkSampleMu.Lock()
	defer d.wkSampleMu.Unlock()
	if d.recording {
		t.Fatal("rejected recording retained the recording lock")
	}
}
