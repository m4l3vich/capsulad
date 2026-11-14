package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// testSpawner starts a host process for supervisor tests, so the restart
// contract is exercised against real child lifecycles instead of a mock.
type testSpawner struct {
	mu     sync.Mutex
	starts []time.Time
	args   []string
}

func (s *testSpawner) Command(_ *manifest.Manifest, _, _, _ string) (*exec.Cmd, error) {
	s.mu.Lock()
	s.starts = append(s.starts, time.Now())
	s.mu.Unlock()
	return exec.Command(s.args[0], s.args[1:]...), nil
}

func (s *testSpawner) Mode() string { return "test" }

func (s *testSpawner) startTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.starts...)
}

type fakeClock struct{ ns atomic.Uint64 }

func (c *fakeClock) NowNS() uint64 { return c.ns.Load() }

func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(uint64(d)) }

func newSupervisor(t *testing.T, spawner Spawner) *Supervisor {
	t.Helper()
	return newSupervisorWithClock(t, spawner, ids.NewClock())
}

func newSupervisorWithClock(t *testing.T, spawner Spawner, clock Clock) *Supervisor {
	t.Helper()
	logs, err := logbus.New(ids.NewClock(), 64, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup := New(ctx, logs, clock, "", spawner)
	sup.SetCallbacks(func(Run) {}, func(Run) {})
	t.Cleanup(func() { sup.Shutdown(2 * time.Second) })
	return sup
}

func testManifest(restartOnFailure bool) *manifest.Manifest {
	return &manifest.Manifest{
		ID: "test.supervisor", Name: "Supervisor test", Version: "1.0.0",
		Entrypoint:       manifest.Entrypoint{Runtime: manifest.RuntimeNative},
		RestartOnFailure: restartOnFailure,
	}
}

func waitStarts(t *testing.T, spawner *testSpawner, want int, timeout time.Duration) []time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if times := spawner.startTimes(); len(times) >= want {
			return times
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("saw %d starts, want %d", len(spawner.startTimes()), want)
	return nil
}

// A crash loop starts the first replacement at once, then backs off so a
// broken app cannot spin the process table, and shutdown stops the loop.
func TestCrashRestartsImmediatelyThenBacksOff(t *testing.T) {
	spawner := &testSpawner{args: []string{"/bin/sh", "-c", "exit 1"}}
	sup := newSupervisor(t, spawner)
	if _, err := sup.Start(testManifest(true), "inv-1"); err != nil {
		t.Fatal(err)
	}

	times := waitStarts(t, spawner, 2, 2*time.Second)
	if gap := times[1].Sub(times[0]); gap > 500*time.Millisecond {
		t.Fatalf("first restart waited %s; a crashed provider must restart immediately", gap)
	}

	times = waitStarts(t, spawner, 3, 3*time.Second)
	if gap := times[2].Sub(times[1]); gap < 800*time.Millisecond {
		t.Fatalf("second failure restarted after %s; the 1 s backoff was skipped", gap)
	}

	sup.Shutdown(2 * time.Second)
	stopped := len(spawner.startTimes())
	time.Sleep(2500 * time.Millisecond)
	if got := len(spawner.startTimes()); got != stopped {
		t.Fatalf("starts continued after shutdown: %d -> %d", stopped, got)
	}
}

// StopForRestart is the selected-provider recovery path: it starts the app
// again even when the manifest does not ask for restart_on_failure. A plain
// Stop still leaves the app stopped.
func TestStopForRestartRelaunchesProvider(t *testing.T) {
	spawner := &testSpawner{args: []string{"/bin/sleep", "30"}}
	sup := newSupervisor(t, spawner)
	sup.SetRestartPolicy(func(string) bool { return true })
	m := testManifest(false)
	first, err := sup.Start(m, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !sup.StopForRestart(first.RunID) {
		t.Fatal("recovery stop refused")
	}
	times := waitStarts(t, spawner, 2, 3*time.Second)
	if gap := times[1].Sub(times[0]); gap > 500*time.Millisecond {
		t.Fatalf("recovery stop took %s to relaunch; the first restart must be immediate", gap)
	}
	runs := sup.List(m.ID)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (len(runs) != 1 || runs[0].RunID == first.RunID) {
		time.Sleep(5 * time.Millisecond)
		runs = sup.List(m.ID)
	}
	if len(runs) != 1 || runs[0].RunID == first.RunID {
		t.Fatalf("runs after recovery stop = %+v, want one new run", runs)
	}

	if !sup.Stop(runs[0].RunID) {
		t.Fatal("plain stop refused")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(sup.List(m.ID)) > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if runs := sup.List(m.ID); len(runs) != 0 {
		t.Fatalf("deliberate stop was resurrected: %+v", runs)
	}
}

// The daemon's selected-provider policy restarts an app whose manifest declines
// restart_on_failure. A deselection during the backoff delay is re-checked
// before the scheduled start, so the policy cannot resurrect a provider the
// operator has just deselected.
func TestRestartPolicyRecoversSelectedProviderRegardlessOfManifest(t *testing.T) {
	spawner := &testSpawner{args: []string{"/bin/sh", "-c", "exit 1"}}
	sup := newSupervisor(t, spawner)
	var selected atomic.Bool
	selected.Store(true)
	sup.SetRestartPolicy(func(string) bool { return selected.Load() })

	if _, err := sup.Start(testManifest(false), "inv-1"); err != nil {
		t.Fatal(err)
	}
	waitStarts(t, spawner, 2, 2*time.Second)

	// The second run crashes on its own; its replacement is now behind the 1 s
	// backoff. Deselect before that delay expires.
	selected.Store(false)
	time.Sleep(1500 * time.Millisecond)
	if got := len(spawner.startTimes()); got != 2 {
		t.Fatalf("starts after deselection = %d, want 2", got)
	}
}

// waitSingleRun returns the one live run of appID that is not notRunID.
func waitSingleRun(t *testing.T, sup *Supervisor, appID, notRunID string) Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs := sup.List(appID)
		if len(runs) == 1 && runs[0].RunID != notRunID {
			return runs[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no single run for %s other than %s", appID, notRunID)
	return Run{}
}

// A run that reports READY and dies immediately must still back off: only a
// stable READY period resets the crash-loop delay. The clock is controlled, so
// the stable case needs no real 30 s wait.
func TestReadyThenCrashKeepsBackoffAndStableRunResets(t *testing.T) {
	spawner := &testSpawner{args: []string{"/bin/sleep", "30"}}
	clock := &fakeClock{}
	sup := newSupervisorWithClock(t, spawner, clock)
	m := testManifest(true)

	first, err := sup.Start(m, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !sup.MarkReady(first.RunID) {
		t.Fatal("first run was not marked ready")
	}
	sup.StopCrashed(first.RunID)
	times := waitStarts(t, spawner, 2, 2*time.Second)
	if gap := times[1].Sub(times[0]); gap > 500*time.Millisecond {
		t.Fatalf("first restart waited %s; a fresh backoff starts immediately", gap)
	}

	second := waitSingleRun(t, sup, m.ID, first.RunID)
	if !sup.MarkReady(second.RunID) {
		t.Fatal("second run was not marked ready")
	}
	sup.StopCrashed(second.RunID)
	times = waitStarts(t, spawner, 3, 3*time.Second)
	if gap := times[2].Sub(times[1]); gap < 800*time.Millisecond {
		t.Fatalf("READY-then-crash restarted after %s; the 1 s backoff was reset", gap)
	}

	third := waitSingleRun(t, sup, m.ID, second.RunID)
	if !sup.MarkReady(third.RunID) {
		t.Fatal("third run was not marked ready")
	}
	clock.Advance(30*time.Second + time.Second)
	sup.StopCrashed(third.RunID)
	times = waitStarts(t, spawner, 4, 3*time.Second)
	if gap := times[3].Sub(times[2]); gap > 500*time.Millisecond {
		t.Fatalf("stable run restarted after %s; a stable READY period must reset the backoff", gap)
	}
}

// A selected app whose manifest declares restart_on_failure must still re-check
// the selection after the backoff delay: the shipped assistant could otherwise
// be resurrected after deselection. A never-selected manifest app keeps its
// ordinary restart contract (covered by TestCrashRestartsImmediatelyThenBacksOff).
func TestSelectedManifestRestartRechecksSelection(t *testing.T) {
	spawner := &testSpawner{args: []string{"/bin/sleep", "30"}}
	sup := newSupervisor(t, spawner)
	var selected atomic.Bool
	selected.Store(true)
	sup.SetRestartPolicy(func(string) bool { return selected.Load() })
	m := testManifest(true)

	first, err := sup.Start(m, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	// Both stops are processed while the provider is still selected, so the
	// restarts are selected-origin even though the manifest also asks for one.
	sup.StopCrashed(first.RunID)
	second := waitSingleRun(t, sup, m.ID, first.RunID)
	sup.StopCrashed(second.RunID)

	// The second crash scheduled the next start behind the 1 s backoff; the
	// run record is gone before that goroutine fires.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(sup.List(m.ID)) > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	selected.Store(false)

	time.Sleep(1500 * time.Millisecond)
	if got := len(spawner.startTimes()); got != 2 {
		t.Fatalf("starts after deselection = %d, want 2", got)
	}
}

// A forced recovery stop re-checks the provider selection at the scheduled
// relaunch. A deselection or uninstall racing the stop must not resurrect it,
// even when the restart delay is zero and even when the manifest declares
// restart_on_failure.
func TestStopForRestartRechecksSelection(t *testing.T) {
	spawner := &testSpawner{args: []string{"/bin/sleep", "30"}}
	sup := newSupervisor(t, spawner)
	var selected atomic.Bool // already deselected when the recovery stop runs
	sup.SetRestartPolicy(func(string) bool { return selected.Load() })
	m := testManifest(true)
	first, err := sup.Start(m, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if !sup.StopForRestart(first.RunID) {
		t.Fatal("recovery stop refused")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(sup.List(m.ID)) > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	if got := len(spawner.startTimes()); got != 1 {
		t.Fatalf("deselected provider was resurrected by a forced recovery stop: %d starts", got)
	}
}

// The selection is sampled before SIGTERM, not after the process is reaped. A
// process that ignores SIGTERM would otherwise let a deselection during the
// termination window turn a selected-origin recovery into an ungated manifest
// restart.
func TestDeselectDuringBlockedTerminationSuppressesManifestRestart(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "exit")
	spawner := &testSpawner{args: []string{"/bin/sh", "-c",
		"trap '' TERM; : > '" + ready + "'; while [ ! -e '" + release + "' ]; do /bin/sleep 0.02; done"}}
	sup := newSupervisor(t, spawner)
	var selected atomic.Bool
	selected.Store(true)
	sup.SetRestartPolicy(func(string) bool { return selected.Load() })
	m := testManifest(true)

	first, err := sup.Start(m, "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("barrier process did not install its TERM trap: %v", err)
	}

	if !sup.StopCrashed(first.RunID) {
		t.Fatal("crash stop refused")
	}
	// The process is still alive and ignoring SIGTERM, so wait has not sampled
	// the terminal transition yet. Deselect before it does.
	selected.Store(false)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sup.List(m.ID)) > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	if got := len(spawner.startTimes()); got != 1 {
		t.Fatalf("deselection during termination was overridden by the manifest: %d starts", got)
	}
}
