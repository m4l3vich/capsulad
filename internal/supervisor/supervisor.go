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
}

func (r *Run) clone() Run {
	cp := *r
	cp.cmd = nil
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
	clock   *ids.Clock
	sock    string
	spawner Spawner

	mu      sync.Mutex
	runs    map[string]*Run
	byApp   map[string][]string
	virtual map[string]func()
	onState func(Run)
	onExit  func(Run)
	backoff map[string]int
	wg      sync.WaitGroup
}

func New(parent context.Context, logs *logbus.Bus, clock *ids.Clock, sock string, spawner Spawner) *Supervisor {
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
	runID := ids.New("run")
	cmd, err := s.spawner.Command(m, m.Dir, runID, m.ID)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env,
		"CAPSULA_SOCKET="+s.sock,
		"CAPSULA_RUN_ID="+runID,
		"CAPSULA_APP_ID="+m.ID,
		"CAPSULA_SDK="+version.SDKVersion,
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
	shouldRestart := prev != Stopping && m.RestartOnFailure && s.ctx.Err() == nil
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
		go s.restart(m)
	}
}

func (s *Supervisor) restart(m *manifest.Manifest) {
	s.mu.Lock()
	s.backoff[m.ID]++
	exp := s.backoff[m.ID]
	if exp > 5 {
		exp = 5
	}
	delay := time.Duration(1<<uint(exp-1)) * time.Second
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
	s.logs.Infof("supervisor", m.ID, "restarting after failure in %s", delay)
	if _, err := s.Start(m, ids.New("inv")); err != nil {
		s.logs.Errorf("supervisor", m.ID, "restart failed: %v", err)
	}
}

// MarkReady records that the supervised app completed its AppReady handshake.
func (s *Supervisor) MarkReady(runID string) bool {
	s.mu.Lock()
	run, ok := s.runs[runID]
	if ok {
		if run.State == Starting {
			run.State = Ready
		}
		s.backoff[run.AppID] = 0
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
	proc := run.cmd.Process
	appID := run.AppID
	cmd := run.cmd
	s.mu.Unlock()

	s.logs.Infof("supervisor", appID, "stopping run=%s pid=%d", runID, proc.Pid)
	_ = proc.Signal(syscall.SIGTERM)
	time.AfterFunc(5*time.Second, func() {
		if cmd.ProcessState == nil {
			s.logs.Warnf("supervisor", appID, "run=%s did not exit; killing", runID)
			_ = proc.Kill()
		}
	})
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

// Shutdown stops all processes and waits for exit.
func (s *Supervisor) Shutdown(timeout time.Duration) {
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
	s.cancel()
}

// RunCount returns the number of live processes.
func (s *Supervisor) RunCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}
