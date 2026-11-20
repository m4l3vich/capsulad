package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/settings"
	"capsulad/internal/supervisor"
)

// startAssistantTurn starts a real wakeword turn and waits until the assistant
// owns the ring and a microphone stream, so a stop/kill below happens with
// active ownership rather than between turns.
func startAssistantTurn(t *testing.T, d *Daemon, appID string) {
	t.Helper()
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "assistant turn owns the ring", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "assistant turn owns the microphone", func() bool {
		return inputOpen(d, appID)
	})
}

// noRunOwnership waits until the dead run's capture, microphone stream and
// conversation are released, so a restarted provider cannot inherit them.
func noRunOwnership(t *testing.T, d *Daemon, appID, runID string) {
	t.Helper()
	waitFor(t, 3*time.Second, "terminated run released ring ownership", func() bool {
		for _, capture := range d.router.ActiveCaptures("light.ring.main") {
			if capture.RunID == runID {
				return false
			}
		}
		return true
	})
	waitFor(t, 3*time.Second, "terminated run released its microphone", func() bool {
		return !inputOpen(d, appID) && !playbackOpen(d, appID)
	})
}

// waitRestartedAssistant waits for a new READY run of the app and returns it.
func waitRestartedAssistant(t *testing.T, d *Daemon, appID, previousRunID string, timeout time.Duration) supervisor.Run {
	t.Helper()
	var next supervisor.Run
	waitFor(t, timeout, appID+" restarts with a new ready run", func() bool {
		run, ok := d.sup.ReadyRun(appID)
		if ok && run.RunID != previousRunID {
			next = run
			return true
		}
		return false
	})
	return next
}

func stopOverRPC(t *testing.T, d *Daemon, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := frameworkv1.NewApplicationServiceClient(dialTest(t, d.cfg.SocketPath))
	res, err := client.Stop(ctx, &frameworkv1.StopRequest{RunId: runID})
	if err != nil {
		t.Fatalf("Stop(%s): %v", runID, err)
	}
	if !res.GetRequested() {
		t.Fatalf("Stop(%s) was not requested", runID)
	}
}

// A public Stop of the selected assistant is a recovery event, not an off
// switch: the run is stopped mid-turn, everything it owns is released, and a
// fresh ready run receives the next manual activation. Deselection is the way
// to keep an assistant stopped.
func TestStopSelectedAssistantRecoversAndRoutesManual(t *testing.T) {
	d, appID := startDumbAssistant(t)
	first, ok := d.sup.ReadyRun(appID)
	if !ok {
		t.Fatal("assistant is not ready")
	}
	startAssistantTurn(t, d, appID)

	stopOverRPC(t, d, first.RunID)
	noRunOwnership(t, d, appID, first.RunID)
	next := waitRestartedAssistant(t, d, appID, first.RunID, 5*time.Second)
	if !strings.Contains(logText(d), "restarting after failure immediately") {
		t.Fatalf("selected assistant did not restart immediately:\n%s", logText(d))
	}
	waitFor(t, 3*time.Second, "bus routes the restarted run", func() bool {
		readyID, ok := d.bus.ReadyApp(appID)
		return ok && readyID == next.RunID
	})
	if runs := d.sup.List(appID); len(runs) != 1 || runs[0].RunID != next.RunID {
		t.Fatalf("runs after recovery = %+v, want only the new run", runs)
	}

	listening := strings.Count(logText(d), "microphone listening")
	if _, ok := d.wake.Trigger("touch", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator manual trigger failed")
	}
	waitFor(t, 5*time.Second, "manual activation reaches the restarted assistant", func() bool {
		return strings.Count(logText(d), "microphone listening") > listening
	})
	waitFor(t, 3*time.Second, "manual turn owns the microphone", func() bool {
		return inputOpen(d, appID)
	})
	if !strings.Contains(logText(d), "origin=ACTIVATION_TOUCH queued") {
		t.Fatalf("manual activation did not route to the restarted assistant:\n%s", logText(d))
	}
}

// A selected assistant killed with SIGKILL mid-turn must produce a new ready
// run immediately and route both wakeword and manual activations to it. The
// old run's ring and microphone ownership must not survive as phantom state.
func TestKilledSelectedAssistantRestartsAndRoutesActivations(t *testing.T) {
	d, appID := startDumbAssistant(t)
	first, ok := d.sup.ReadyRun(appID)
	if !ok {
		t.Fatal("assistant is not ready")
	}
	startAssistantTurn(t, d, appID)

	process, err := os.FindProcess(first.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	next := waitRestartedAssistant(t, d, appID, first.RunID, 5*time.Second)
	if !strings.Contains(logText(d), "restarting after failure immediately") {
		t.Fatalf("killed selected assistant did not restart immediately:\n%s", logText(d))
	}
	noRunOwnership(t, d, appID, first.RunID)
	waitFor(t, 3*time.Second, "ring ownership returned to the baseline", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID != appID
	})
	waitFor(t, 3*time.Second, "bus routes the restarted run", func() bool {
		readyID, ok := d.bus.ReadyApp(appID)
		return ok && readyID == next.RunID
	})

	wakeword := strings.Count(logText(d), "origin=ACTIVATION_WAKEWORD queued")
	manual := strings.Count(logText(d), "origin=ACTIVATION_TOUCH queued")
	listening := strings.Count(logText(d), "microphone listening")
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "wakeword activation reaches the restarted assistant", func() bool {
		return strings.Count(logText(d), "origin=ACTIVATION_WAKEWORD queued") > wakeword
	})
	if _, ok := d.wake.Trigger("touch", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator manual trigger failed")
	}
	waitFor(t, 3*time.Second, "manual activation reaches the restarted assistant", func() bool {
		return strings.Count(logText(d), "origin=ACTIVATION_TOUCH queued") > manual
	})
	// The first turn ends on its no-speech window and the queued manual turn
	// follows, so the restarted run must serve both activations.
	waitFor(t, 10*time.Second, "both queued turns listen on the restarted run", func() bool {
		return strings.Count(logText(d), "microphone listening") >= listening+2
	})
}

// Deselecting the assistant is the off switch: an in-flight turn is stopped,
// nothing is recovered while no assistant is selected, and a later trigger is
// dropped instead of resurrecting the provider. Selecting it again starts a
// fresh ready run.
func TestDeselectedAssistantStopsAndIsNotRecovered(t *testing.T) {
	d, appID := startDumbAssistant(t)
	first, ok := d.sup.ReadyRun(appID)
	if !ok {
		t.Fatal("assistant is not ready")
	}
	startAssistantTurn(t, d, appID)

	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = ""
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "deselected assistant stops", func() bool {
		return len(d.sup.List(appID)) == 0
	})
	noRunOwnership(t, d, appID, first.RunID)

	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "activation is dropped without a selected assistant", func() bool {
		return strings.Contains(logText(d), "dropped: no selected assistant")
	})
	time.Sleep(300 * time.Millisecond)
	if runs := d.sup.List(appID); len(runs) != 0 {
		t.Fatalf("activation resurrected a deselected assistant: %+v", runs)
	}

	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = appID
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "reselected assistant starts a fresh run", func() bool {
		run, ok := d.sup.ReadyRun(appID)
		return ok && run.RunID != first.RunID
	})
}

// A compiled-in provider is a real default run too. Stopping it while it is
// selected re-registers a ready run with the same stable ID; after the
// operator deselects it, a stop sticks, and reselecting starts it again.
func TestSelectedBuiltinStopRecoversUnlessDeselected(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	runID := d.SystemAppRunID(clockAppID)
	first, ok := d.sup.Get(runID)
	if !ok || first.State != supervisor.Ready {
		t.Fatalf("clock run = %+v, want READY", first)
	}

	stopOverRPC(t, d, runID)
	var restarted supervisor.Run
	waitFor(t, 3*time.Second, "selected built-in restarts after Stop", func() bool {
		run, ok := d.sup.Get(runID)
		if ok && run.State == supervisor.Ready && run.StartedNS > first.StartedNS {
			restarted = run
			return true
		}
		return false
	})
	if restarted.RunID != runID || !restarted.Virtual {
		t.Fatalf("restarted built-in = %+v, want the stable virtual run", restarted)
	}
	waitForAppSelection(t, d, "display.segment.front", clockAppID)

	if err := d.writeUserSettings([]string{"default_app_by_resource"}, func(v *settings.UserSettings) {
		v.DefaultAppByResource["display.segment.front"] = ""
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "clock is no longer the selected display provider", func() bool {
		return d.router.Current("display.segment.front").SelectedAppID != clockAppID
	})

	// A deselected provider is not a selected default any more; its stop must
	// stick instead of being treated as a recovery event.
	d.Stop(runID)
	time.Sleep(500 * time.Millisecond)
	if run, ok := d.sup.Get(runID); ok {
		t.Fatalf("deselected built-in was resurrected: %+v", run)
	}

	if err := d.writeUserSettings([]string{"default_app_by_resource"}, func(v *settings.UserSettings) {
		v.DefaultAppByResource["display.segment.front"] = clockAppID
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "reselected built-in starts again", func() bool {
		run, ok := d.sup.Get(runID)
		return ok && run.State == supervisor.Ready
	})
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
}

// testOptOutProviderManifest is a selectable provider that explicitly declines
// restart_on_failure. The operator's selection, not this flag, must decide
// whether a selected default is recovered.
const testOptOutProviderManifest = `
id = "test.selectedonly"
name = "Selected only provider"
version = "0.1.0"
background = false
start_on_boot = false
restart_on_failure = false
provides = ["voice_assistant"]

[entrypoint]
runtime = "native"
path = "probe"
architecture = "aarch64"
abi = "linux-musl"
`

// A selected default is recovered from both an abnormal process exit and a
// lost framework connection even when the manifest sets
// restart_on_failure=false. Backoff still applies (the first restart is
// immediate) and exactly one run remains.
func TestSelectedOptOutProviderRecoversFromCrashAndLostConnection(t *testing.T) {
	binary := buildProbe(t)
	const appID = "test.selectedonly"
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: appID, binaryName: "probe", binaryPath: binary,
		manifest: []byte(testOptOutProviderManifest),
	})
	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = appID
	}); err != nil {
		t.Fatal(err)
	}
	first := waitRestartedAssistant(t, d, appID, "", 10*time.Second)

	process, err := os.FindProcess(first.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	second := waitRestartedAssistant(t, d, appID, first.RunID, 5*time.Second)
	if !strings.Contains(logText(d), "restarting after failure immediately") {
		t.Fatalf("selected opt-out provider did not restart immediately after a crash:\n%s", logText(d))
	}

	// The lost-framework-connection path is a failure too: the supervisor stops
	// the orphaned process and must relaunch the selected provider.
	d.onAppDisconnect(second.RunID, appID)
	third := waitRestartedAssistant(t, d, appID, second.RunID, 5*time.Second)
	if runs := d.sup.List(appID); len(runs) != 1 || runs[0].RunID != third.RunID {
		t.Fatalf("runs after recovery = %+v, want only the final run", runs)
	}
}

// An unselected provider with the same opt-out manifest is not resurrected:
// selection decides recovery, not the mere presence of a provider.
func TestUnselectedOptOutProviderStaysStopped(t *testing.T) {
	binary := buildProbe(t)
	const appID = "test.selectedonly"
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: appID, binaryName: "probe", binaryPath: binary,
		manifest: []byte(testOptOutProviderManifest),
	})
	if _, _, _, err := d.Launch(appID, nil); err != nil {
		t.Fatal(err)
	}
	run := waitRestartedAssistant(t, d, appID, "", 10*time.Second)
	process, err := os.FindProcess(run.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "unselected run is removed", func() bool {
		return len(d.sup.List(appID)) == 0
	})
	time.Sleep(500 * time.Millisecond)
	if runs := d.sup.List(appID); len(runs) != 0 {
		t.Fatalf("unselected provider was resurrected: %+v", runs)
	}
}

// An uninstalled selected provider cannot be recovered: the package directory
// is the framework's uninstall signal even though the daemon still has the
// registry entry cached from boot.
func TestUninstalledSelectedProviderStaysStopped(t *testing.T) {
	binary := buildProbe(t)
	const appID = "test.selectedonly"
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: appID, binaryName: "probe", binaryPath: binary,
		manifest: []byte(testOptOutProviderManifest),
	})
	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = appID
	}); err != nil {
		t.Fatal(err)
	}
	run := waitRestartedAssistant(t, d, appID, "", 10*time.Second)
	if err := os.RemoveAll(filepath.Join(d.cfg.AppsDir, appID)); err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(run.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "uninstalled run is removed", func() bool {
		return len(d.sup.List(appID)) == 0
	})
	time.Sleep(500 * time.Millisecond)
	if run, ok := d.sup.ReadyRun(appID); ok {
		t.Fatalf("uninstalled selected provider was resurrected: %+v", run)
	}
}

// A built-in default that is deselected while its teardown is still running
// must not be relaunched: the recovery check happens again immediately before
// the in-place re-registration, after the settings update has been persisted.
func TestBuiltinStopDoesNotResurrectAfterDeselectionDuringTeardown(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	runID := d.SystemAppRunID(clockAppID)

	// Hold the clock's teardown boundary so the deselection lands between the
	// stop decision and the recovery relaunch.
	d.builtins.clock.alertMu.Lock()
	locked := true
	defer func() {
		if locked {
			d.builtins.clock.alertMu.Unlock()
		}
	}()
	done := make(chan bool, 1)
	go func() { done <- d.Stop(runID) }()
	waitFor(t, time.Second, "clock stopping", func() bool {
		run, ok := d.sup.Get(runID)
		return ok && run.State == supervisor.Stopping
	})
	if err := d.writeUserSettings([]string{"default_app_by_resource"}, func(v *settings.UserSettings) {
		v.DefaultAppByResource["display.segment.front"] = ""
	}); err != nil {
		t.Fatal(err)
	}
	d.builtins.clock.alertMu.Unlock()
	locked = false
	select {
	case stopped := <-done:
		if !stopped {
			t.Fatal("clock Stop failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clock Stop did not finish")
	}
	time.Sleep(300 * time.Millisecond)
	if run, ok := d.sup.Get(runID); ok {
		t.Fatalf("deselected built-in was resurrected during Stop: %+v", run)
	}
}

// A manual launch that races a crash recovery must attach its invocation to
// the recovered run instead of spawning a second process. The recovery is
// parked inside the supervisor's start reservation (via the control-channel
// factory), so the manual Launch arrives after the recovery decided to start
// but before it published a run — the exact window where two starts used to
// both spawn. The launch must report spawned=false for the recovery run and
// still deliver its invocation once that run is ready.
func TestManualLaunchRacingRecoveryRoutesToRecoveredRun(t *testing.T) {
	const appID = "test.recoveryrace"
	var (
		mu       sync.Mutex
		controls int
	)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	d, cancel, _ := newTestDaemonPrepared(t, func(d *Daemon) {
		channel := d.appControl.Channel
		d.sup.SetControlChannel(func(runID, id string) (*os.File, func(), error) {
			if id == appID {
				mu.Lock()
				controls++
				n := controls
				mu.Unlock()
				if n == 2 {
					close(entered)
					<-release
				}
			}
			return channel(runID, id)
		})
	}, appInstall{
		id: appID, binaryName: "probe", binaryPath: buildProbe(t),
		manifest: []byte(`
id = "test.recoveryrace"
name = "Recovery race"
version = "0.1.0"
restart_on_failure = true

[entrypoint]
runtime = "native"
path = "probe"
architecture = "aarch64"
abi = "linux-musl"

[[arguments]]
name = "note"
type = "text"
`),
	})
	defer cancel()

	first, _, spawned, err := d.Launch(appID, nil)
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if !spawned {
		t.Fatal("first launch did not spawn")
	}
	waitFor(t, 5*time.Second, "recovery race app ready", func() bool {
		_, ok := d.sup.ReadyRun(appID)
		return ok
	})
	firstRun, ok := d.sup.Get(first)
	if !ok {
		t.Fatalf("first run %s disappeared before the crash", first)
	}
	if err := syscall.Kill(firstRun.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill first run: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not reach the start reservation")
	}

	type launchResult struct {
		runID, invocationID string
		spawned             bool
		err                 error
	}
	manual := make(chan launchResult, 1)
	go func() {
		runID, invocationID, spawned, err := d.Launch(appID, map[string]string{"note": "manual"})
		manual <- launchResult{runID: runID, invocationID: invocationID, spawned: spawned, err: err}
	}()
	select {
	case res := <-manual:
		t.Fatalf("manual launch returned while the recovery held the reservation: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	var res launchResult
	select {
	case res = <-manual:
	case <-time.After(5 * time.Second):
		t.Fatal("manual launch did not finish after the recovery published")
	}
	if res.err != nil {
		t.Fatalf("manual launch: %v", res.err)
	}
	if res.spawned {
		t.Fatal("manual launch spawned a second process instead of routing to the recovery")
	}
	runs := d.sup.List(appID)
	if len(runs) != 1 || runs[0].RunID != res.runID || res.runID == first {
		t.Fatalf("runs after the race = %+v, manual result run=%s first=%s", runs, res.runID, first)
	}
	waitFor(t, 5*time.Second, "manual invocation delivered to the recovery run", func() bool {
		return strings.Contains(logText(d), "invocation="+res.invocationID+" delivered to run="+res.runID)
	})
	if runs := d.sup.List(appID); len(runs) != 1 || runs[0].RunID != res.runID {
		t.Fatalf("runs after invocation delivery = %+v, want only %s", runs, res.runID)
	}
}
