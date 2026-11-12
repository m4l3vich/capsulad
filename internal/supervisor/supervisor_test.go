package supervisor

import (
	"context"
	"testing"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
)

// A compiled-in app is a real run: READY on registration, EXITED after Stop,
// with its teardown hook called exactly once and the stable run ID released.
func TestBuiltinRunLifecycle(t *testing.T) {
	logs, err := logbus.New(ids.NewClock(), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := New(ctx, logs, ids.NewClock(), "", nil)
	var states []State
	sup.SetCallbacks(func(run Run) { states = append(states, run.State) }, nil)

	m := &manifest.Manifest{
		ID: "capsula.clock", Name: "Clock", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
	}
	runID := "builtin:capsula.clock"
	run, err := sup.RegisterBuiltin(m, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != Ready || !run.Virtual {
		t.Fatalf("builtin run = %+v", run)
	}
	if _, ok := sup.ReadyRun("capsula.clock"); !ok {
		t.Fatal("builtin is not ready")
	}
	if got := len(sup.List("capsula.clock")); got != 1 {
		t.Fatalf("runs = %d", got)
	}
	stopped := 0
	sup.BindBuiltinStop(runID, func() { stopped++ })
	if !sup.Stop(runID) {
		t.Fatal("stop refused")
	}
	if stopped != 1 {
		t.Fatalf("stop hook called %d times", stopped)
	}
	if _, ok := sup.Get(runID); ok {
		t.Fatal("stopped builtin still has a run")
	}
	if got := len(sup.List("capsula.clock")); got != 0 {
		t.Fatalf("runs after stop = %d", got)
	}
	if len(states) < 2 || states[len(states)-1] != Exited {
		t.Fatalf("states = %v, want a final EXITED", states)
	}
	if _, err := sup.RegisterBuiltin(m, runID); err != nil {
		t.Fatalf("restart: %v", err)
	}
}
