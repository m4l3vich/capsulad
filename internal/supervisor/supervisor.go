// Package supervisor starts, tracks and stops supervised application
// processes. It deliberately has no policy for singletons or activation; the
// daemon owns those decisions, while the supervisor owns process mechanics,
// graceful stop, restart backoff and streamed log collection.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/version"
)

// Clock supplies monotonic nanoseconds. The daemon passes ids.Clock; tests use
// a controlled clock so backoff/readiness transitions need no wall-clock wait.
type Clock interface {
	NowNS() uint64
}

// stableReadyUptimeNS is how long a run must stay READY before a later exit
// resets the crash-loop backoff. A provider that reports READY and dies
// immediately must keep backing off instead of looping at zero delay.
const stableReadyUptimeNS uint64 = uint64(30 * time.Second)

type State int

const (
	Starting State = iota + 1
	Ready
	Stopping
	Exited
	Failed
)

func (s State) String() string {
	switch s {
	case Starting:
		return "STARTING"
	case Ready:
		return "READY"
	case Stopping:
		return "STOPPING"
	case Exited:
		return "EXITED"
	default:
		return "FAILED"
	}
}

type Run struct {
	RunID        string
	AppID        string
	PID          int
	State        State
	StartedNS    uint64
	ExitCode     int
	Detail       string
	Runtime      manifest.Runtime
	Sandbox      string
	InvocationID string
	Restarts     int
	// Virtual marks an in-process system application. It has no child process
	// but the same run record, state events and stop/launch semantics.
	Virtual bool

	cmd *exec.Cmd
	// exited is closed once Wait has reaped the child. The graceful-stop
	// escalation timer waits on it instead of reading cmd.ProcessState, which
	// Wait writes without synchronization.
	exited chan struct{}
	// restartAfterStop marks a run the framework terminated because it lost its
	// bus connection (a crash). A deliberate Stop leaves it false: only a
	// failure restarts, so an explicitly stopped app stays stopped.
	restartAfterStop bool
	// readySinceNS records when the run became READY. The crash-loop backoff is
	// reset only when that readiness lasted long enough to be considered
	// stable, so a READY-then-crash loop still backs off. The clock origin can
	// legitimately be 0, so presence is tracked separately.
	readySinceNS  uint64
	readySinceSet bool
	// selectionOrigin records that the run was an installed, selected default
	// when the framework began stopping it for a lost connection. The selection
	// is sampled before SIGTERM so a deselection or uninstall during a delayed
	// termination still suppresses the scheduled relaunch, even when the
	// manifest asks for restart_on_failure.
	selectionOrigin bool
	// restartForced marks a run the daemon stopped as a recovery step for the
	// operator-selected default provider. That run is started again even when
	// its manifest does not declare restart_on_failure: the selection, not the
	// manifest, is what the operator asked to keep available. The scheduled
	// relaunch still re-checks that selection, so a concurrent deselection or
	// uninstall wins.
	restartForced bool
}

func (r *Run) clone() Run {
	cp := *r
	cp.cmd = nil
	cp.exited = nil
	return cp
}

// Spawner builds the concrete command. Simulator mode returns a plain command;
// the Linux sandbox spawner returns a re-executed helper that constructs a
// read-only mount namespace before executing the app.
type Spawner interface {
	Command(m *manifest.Manifest, appDir, runID, appID string) (*exec.Cmd, error)
	Mode() string
}

type Supervisor struct {
	ctx     context.Context
	cancel  context.CancelFunc
	logs    *logbus.Bus
	clock   Clock
	sock    string
	spawner Spawner

	mu      sync.Mutex
	runs    map[string]*Run
	byApp   map[string][]string
	virtual map[string]func()
	onState func(Run)
	onExit  func(Run)
	// restartPolicy can require a restart the manifest did not ask for. The
	// daemon uses it to keep an operator-selected provider alive after an
	// abnormal exit or a lost framework connection.
	restartPolicy func(appID string) bool
	backoff       map[string]int
	wg            sync.WaitGroup

	// startMu/stopping/startWG separate "a Start is in flight" from
	// wg.Wait: Shutdown blocks new starts, waits for in-flight Start calls to
	// finish registering their goroutines, and only then waits on wg. Without
	// this, a restart racing Shutdown could Add to wg while it is being
	// waited on.
	startMu  sync.Mutex
	stopping bool
	startWG  sync.WaitGroup
}

func New(parent context.Context, logs *logbus.Bus, clock Clock, sock string, spawner Spawner) *Supervisor {
	ctx, cancel := context.WithCancel(parent)
	return &Supervisor{
		ctx: ctx, cancel: cancel, logs: logs, clock: clock, sock: sock, spawner: spawner,
		runs: map[string]*Run{}, byApp: map[string][]string{}, backoff: map[string]int{},
		virtual: map[string]func(){},
	}
}

func (s *Supervisor) SetCallbacks(onState, onExit func(Run)) {
	s.mu.Lock()
	s.onState, s.onExit = onState, onExit
	s.mu.Unlock()
}

// SetRestartPolicy installs the daemon's selected-provider predicate. It is
// consulted at crash time, not at launch: a provider that stopped being the
// operator's selection (deselected, uninstalled) is not resurrected, while an
// installed selected default is restarted even when its manifest does not ask
// for restart_on_failure. The predicate reads daemon state only; it must not
// call back into the supervisor.
func (s *Supervisor) SetRestartPolicy(fn func(appID string) bool) {
	s.mu.Lock()
	s.restartPolicy = fn
	s.mu.Unlock()
}

func (s *Supervisor) restartAllowed(appID string) bool {
	s.mu.Lock()
	fn := s.restartPolicy
	s.mu.Unlock()
	return fn != nil && fn(appID)
}

func (s *Supervisor) notifyState(r Run) {
	s.mu.Lock()
	fn := s.onState
	s.mu.Unlock()
	if fn != nil {
		fn(r)
	}
}

// Start spawns an app process. invocationID is recorded so the daemon can
// deliver the initial AppLaunchInvocation after the app reports READY.
func (s *Supervisor) Start(m *manifest.Manifest, invocationID string) (*Run, error) {
	s.startMu.Lock()
	if s.stopping || s.ctx.Err() != nil {
		s.startMu.Unlock()
		return nil, fmt.Errorf("supervisor is shutting down")
	}
	s.startWG.Add(1)
	s.startMu.Unlock()
	defer s.startWG.Done()
	runID := ids.New("run")
	cmd, err := s.spawner.Command(m, m.Dir, runID, m.ID)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env,
		"CAPSULAD_SOCKET="+s.sock,
		"CAPSULAD_RUN_ID="+runID,
		"CAPSULAD_APP_ID="+m.ID,
		"CAPSULAD_SDK="+version.SDKVersion,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", m.ID, err)
	}

	run := &Run{
		RunID: runID, AppID: m.ID, PID: cmd.Process.Pid, State: Starting,
		StartedNS: s.clock.NowNS(), Runtime: m.Entrypoint.Runtime,
		Sandbox: s.spawner.Mode(), InvocationID: invocationID, cmd: cmd, ExitCode: -1,
		exited: make(chan struct{}),
	}
	s.mu.Lock()
	s.runs[runID] = run
	s.byApp[m.ID] = append(s.byApp[m.ID], runID)
	snapshot := run.clone()
	s.mu.Unlock()

	s.logs.Infof("supervisor", m.ID, "started %s run=%s pid=%d runtime=%s sandbox=%s",
		m.ID, runID, run.PID, m.Entrypoint.Runtime, run.Sandbox)
	s.notifyState(snapshot)

	s.wg.Add(2)
	go s.pump(run, stdout, "stdout")
	go s.pump(run, stderr, "stderr")
	s.wg.Add(1)
	go s.wait(run, m)
	return &snapshot, nil
}

func (s *Supervisor) pump(run *Run, r io.Reader, stream string) {
	defer s.wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		if len(line) > 8000 {
			line = line[:8000] + "…"
		}
		s.logs.Infof("app."+stream, run.AppID, "%s", line)
	}
}

func (s *Supervisor) wait(run *Run, m *manifest.Manifest) {
	defer s.wg.Done()
	err := run.cmd.Wait()
	close(run.exited)
	// Read the daemon's selection before taking the lock: the predicate reads
	// settings and the registry, never the supervisor.
	selected := s.restartAllowed(run.AppID)

	s.mu.Lock()
	prev := run.State
	run.State = Exited
	code := 0
	detail := ""
	if err != nil {
		code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
				detail = "terminated by " + ws.Signal().String()
			}
		} else {
			detail = err.Error()
		}
	}
	if prev == Stopping {
		detail = "stopped"
	} else if code != 0 {
		run.State = Failed
	}
	if detail != "" && run.Detail == "" {
		run.Detail = detail
	}
	run.ExitCode = code
	// An abnormal exit or a lost framework connection is a failure. A selected
	// default provider is recovered even when its manifest does not ask for
	// restart_on_failure; a deliberate Stop stays stopped unless the daemon
	// explicitly requested recovery for the selected provider. Shutdown cancels
	// the context first and suppresses every restart.
	recovered := prev != Stopping || run.restartAfterStop
	manifestRestart := m.RestartOnFailure && recovered
	selectedRestart := selected && recovered
	shouldRestart := s.ctx.Err() == nil && (run.restartForced || manifestRestart || selectedRestart)
	// A stable READY period proves the app can run, so a later crash starts a
	// fresh backoff. READY-then-immediate-crash keeps accumulating.
	if now := s.clock.NowNS(); run.readySinceSet && now >= run.readySinceNS &&
		now-run.readySinceNS >= stableReadyUptimeNS {
		s.backoff[run.AppID] = 0
	}
	// A restart caused by the provider's selection (a selected-origin crash or
	// a forced recovery stop) must re-check that selection at the scheduled
	// start, regardless of restart_on_failure: a deselection or uninstall
	// racing the stop or the backoff delay must not resurrect it. A manifest
	// restart of an app that was never selected keeps its ordinary contract.
	requireSelection := selectedRestart || run.selectionOrigin || run.restartForced
	delete(s.runs, run.RunID)
	list := s.byApp[run.AppID]
	for i, id := range list {
		if id == run.RunID {
			s.byApp[run.AppID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	snapshot := run.clone()
	fn := s.onExit
	s.mu.Unlock()

	sev := "exited"
	if run.State == Failed {
		sev = "failed"
	}
	s.logs.Infof("supervisor", run.AppID, "%s run=%s code=%d %s", sev, run.RunID, code, detail)
	s.notifyState(snapshot)
	if fn != nil {
		fn(snapshot)
	}
	if shouldRestart {
		go s.restart(m, requireSelection)
	}
}

func (s *Supervisor) restart(m *manifest.Manifest, requireSelection bool) {
	s.mu.Lock()
	s.backoff[m.ID]++
	exp := s.backoff[m.ID]
	if exp > 5 {
		exp = 5
	}
	// The first failure after a healthy run restarts at once: a default
	// provider that was killed mid-operation must be usable again without a
	// visible outage. Repeated failures still back off so a crash loop cannot
	// spin the process table.
	delay := time.Duration(0)
	if exp > 1 {
		delay = time.Duration(1<<uint(exp-2)) * time.Second
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	s.mu.Unlock()
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(delay):
	}
	if s.ctx.Err() != nil {
		return
	}
	if requireSelection && !s.restartAllowed(m.ID) {
		s.logs.Infof("supervisor", m.ID, "not restarting the provider: it is no longer selected")
		return
	}
	if delay == 0 {
		s.logs.Infof("supervisor", m.ID, "restarting after failure immediately")
	} else {
		s.logs.Infof("supervisor", m.ID, "restarting after failure in %s", delay)
	}
	if _, err := s.Start(m, ids.New("inv")); err != nil {
		s.logs.Errorf("supervisor", m.ID, "restart failed: %v", err)
	}
}

// MarkReady records that the supervised app completed its AppReady handshake.
// It never resets the crash-loop backoff: that happens only when a run is
// reaped after a stable READY period, so an app that reports READY and dies
// immediately still backs off.
func (s *Supervisor) MarkReady(runID string) bool {
	s.mu.Lock()
	run, ok := s.runs[runID]
	if ok && run.State == Starting {
		run.State = Ready
		run.readySinceNS = s.clock.NowNS()
		run.readySinceSet = true
	}
	var snapshot Run
	if ok {
		snapshot = run.clone()
	}
	s.mu.Unlock()
	if ok {
		s.notifyState(snapshot)
	}
	return ok
}

// Get returns a run snapshot.
func (s *Supervisor) Get(runID string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return Run{}, false
	}
	return run.clone(), true
}

// List returns runs for one app, or every live run when appID is empty.
func (s *Supervisor) List(appID string) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	if appID == "" {
		for id := range s.runs {
			ids = append(ids, id)
		}
	} else {
		ids = append(ids, s.byApp[appID]...)
	}
	out := make([]Run, 0, len(ids))
	for _, id := range ids {
		if run, ok := s.runs[id]; ok {
			out = append(out, run.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedNS < out[j].StartedNS })
	return out
}

// ReadyRun returns the currently READY run for an app.
func (s *Supervisor) ReadyRun(appID string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.byApp[appID] {
		if run, ok := s.runs[id]; ok && run.State == Ready {
			return run.clone(), true
		}
	}
	return Run{}, false
}

// RegisterBuiltin records a compiled-in system app as a READY run. The caller
// supplies a stable run ID so app-bus callers keep their target across a
// stop/start cycle. Registration is idempotent.
func (s *Supervisor) RegisterBuiltin(m *manifest.Manifest, runID string) (Run, error) {
	if m == nil || m.Entrypoint.Runtime != manifest.RuntimeBuiltin {
		return Run{}, fmt.Errorf("builtin run requires a builtin manifest")
	}
	if runID == "" {
		return Run{}, fmt.Errorf("builtin run id is required")
	}
	s.mu.Lock()
	if existing, ok := s.runs[runID]; ok {
		snap := existing.clone()
		s.mu.Unlock()
		return snap, nil
	}
	run := &Run{
		RunID: runID, AppID: m.ID, State: Ready, StartedNS: s.clock.NowNS(),
		Runtime: manifest.RuntimeBuiltin, Sandbox: "builtin", ExitCode: -1,
		// The virtual run executes inside the daemon process. Recording the real
		// daemon PID lets the existing SO_PEERCRED check bind the run identity:
		// only a connection from this process can claim it, so an external app
		// cannot impersonate a system app by sending its run ID.
		PID: os.Getpid(), Virtual: true,
	}
	s.runs[runID] = run
	s.byApp[m.ID] = append(s.byApp[m.ID], runID)
	snapshot := run.clone()
	s.mu.Unlock()
	s.logs.Infof("supervisor", m.ID, "started built-in run=%s", runID)
	s.notifyState(snapshot)
	return snapshot, nil
}

// BindBuiltinStop attaches the teardown hook invoked by Stop.
func (s *Supervisor) BindBuiltinStop(runID string, stop func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[runID]; ok {
		s.virtual[runID] = stop
	}
}

// Stop requests graceful termination and escalates to SIGKILL after timeout.
func (s *Supervisor) Stop(runID string) bool {
	return s.stop(runID, false, false)
}

// StopCrashed terminates a run that lost its framework connection. It is a
// deliberate stop from the process's point of view, but the exit is treated as
// a failure: a restart_on_failure application, or the daemon's selected
// provider, is started again immediately instead of remaining unavailable. A
// compiled-in app has no process to restart, so it is stopped like Stop.
func (s *Supervisor) StopCrashed(runID string) bool {
	return s.stop(runID, true, false)
}

// StopForRestart terminates the run of the operator-selected default provider
// and starts it again without asking the manifest. The daemon has already
// decided that the provider must remain available; deselecting it, shutting
// down or uninstalling it is the way to keep it stopped, so the scheduled
// relaunch re-checks the restart policy and is suppressed when the provider is
// no longer selected. A compiled-in app has no child process here: the daemon
// re-registers built-in providers itself.
func (s *Supervisor) StopForRestart(runID string) bool {
	return s.stop(runID, true, true)
}

func (s *Supervisor) stop(runID string, crashed, forced bool) bool {
	// Sample the provider selection before SIGTERM. A process may ignore or
	// delay the signal for up to the graceful-stop timeout; a deselection or
	// uninstall during that window must not downgrade a selected-origin
	// recovery into an ungated manifest restart.
	selectedAtStop := false
	if crashed {
		s.mu.Lock()
		appID := ""
		if run, ok := s.runs[runID]; ok {
			appID = run.AppID
		}
		s.mu.Unlock()
		if appID != "" {
			selectedAtStop = s.restartAllowed(appID)
		}
	}
	s.mu.Lock()
	run, ok := s.runs[runID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	if run.Virtual {
		if run.State == Stopping {
			s.mu.Unlock()
			return true
		}
		run.State = Stopping
		appID := run.AppID
		stop := s.virtual[runID]
		stopping := run.clone()
		s.mu.Unlock()

		s.notifyState(stopping)
		if stop != nil {
			stop()
		}
		s.mu.Lock()
		run.State = Exited
		run.ExitCode = 0
		run.Detail = "stopped"
		delete(s.runs, runID)
		delete(s.virtual, runID)
		list := s.byApp[appID]
		for i, id := range list {
			if id == runID {
				s.byApp[appID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		exited := run.clone()
		s.mu.Unlock()
		s.logs.Infof("supervisor", appID, "stopped built-in run=%s", runID)
		s.notifyState(exited)
		return true
	}
	run.State = Stopping
	run.restartAfterStop = run.restartAfterStop || crashed
	run.selectionOrigin = run.selectionOrigin || selectedAtStop
	run.restartForced = run.restartForced || forced
	proc := run.cmd.Process
	appID := run.AppID
	s.mu.Unlock()

	s.logs.Infof("supervisor", appID, "stopping run=%s pid=%d", runID, proc.Pid)
	_ = proc.Signal(syscall.SIGTERM)
	time.AfterFunc(5*time.Second, func() {
		select {
		case <-run.exited:
			return
		default:
		}
		s.logs.Warnf("supervisor", appID, "run=%s did not exit; killing", runID)
		_ = proc.Kill()
	})
	return true
}

// FailBuiltin marks a compiled-in run failed, tears it down through the bound
// stop hook and notifies the exit callback. It is used when the run's internal
// output stream dies; the daemon then owns restart with its normal policy.
func (s *Supervisor) FailBuiltin(runID, detail string) bool {
	s.mu.Lock()
	run, ok := s.runs[runID]
	if !ok || !run.Virtual || run.State == Failed || run.State == Stopping {
		s.mu.Unlock()
		return false
	}
	run.State = Failed
	run.ExitCode = -1
	run.Detail = detail
	appID := run.AppID
	stop := s.virtual[runID]
	delete(s.runs, runID)
	delete(s.virtual, runID)
	list := s.byApp[appID]
	for i, id := range list {
		if id == runID {
			s.byApp[appID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	snapshot := run.clone()
	s.mu.Unlock()
	s.logs.Errorf("supervisor", appID, "failed built-in run=%s: %s", runID, detail)
	s.notifyState(snapshot)
	if stop != nil {
		stop()
	}
	if fn := s.onExit; fn != nil {
		fn(snapshot)
	}
	return true
}

// StopApp stops every run of an application.
func (s *Supervisor) StopApp(appID string) int {
	runs := s.List(appID)
	for _, r := range runs {
		s.Stop(r.RunID)
	}
	return len(runs)
}

// Shutdown stops all processes and waits for exit. The supervisor context is
// cancelled first: a bus disconnect during shutdown must not be mistaken for a
// crash and restart an app while the daemon is going away.
func (s *Supervisor) Shutdown(timeout time.Duration) {
	s.cancel()
	// Refuse new starts and wait for Start calls already in flight to register
	// their goroutines before waiting on wg. The runs they added are included
	// in the stop pass below.
	s.startMu.Lock()
	s.stopping = true
	s.startMu.Unlock()
	s.startWG.Wait()
	s.mu.Lock()
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Stop(id)
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.logs.Warnf("supervisor", "", "shutdown timed out; killing remaining processes")
		s.mu.Lock()
		for _, run := range s.runs {
			_ = run.cmd.Process.Kill()
		}
		s.mu.Unlock()
	}
}

// RunCount returns the number of live processes.
func (s *Supervisor) RunCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}
