package webui

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
)

// The Web UI's run table is the only source of the "Running" badge and the
// Launch button. Each compiled-in app must appear once under its own app id,
// with the selection flag that belongs to it.
func TestOverviewReportsBuiltinRunsUnderTheirOwnApp(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.runs = append(env.back.runs, &frameworkv1.AppRun{
		RunId: "builtin:capsulad.ring", AppId: "capsulad.ring", Pid: 42,
		State: frameworkv1.AppRunState_APP_READY, Runtime: frameworkv1.ApplicationRuntime_RUNTIME_BUILTIN,
	})
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 60, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 60).GetOverview()

	seen := map[string]*webv1.RunView{}
	for _, run := range overview.GetRuns() {
		if _, duplicated := seen[run.GetAppId()]; duplicated {
			t.Fatalf("%s is listed more than once: %v", run.GetAppId(), overview.GetRuns())
		}
		seen[run.GetAppId()] = run
	}
	clock, ok := seen["capsulad.clock"]
	if !ok {
		t.Fatalf("the running clock is missing from the run table: %v", overview.GetRuns())
	}
	if clock.GetState() != frameworkv1.AppRunState_APP_READY.String() {
		t.Fatalf("clock state = %q, want APP_READY", clock.GetState())
	}
	if !clock.GetSelectedDefault() {
		t.Fatal("the clock is the selected display provider but its run is not marked as a default")
	}
	ring, ok := seen["capsulad.ring"]
	if !ok {
		t.Fatalf("the ring run is missing from the run table: %v", overview.GetRuns())
	}
	if ring.GetSelectedDefault() {
		t.Fatal("the ring is not a selected default but its run claims to be one")
	}
}

// Only a READY run can serve its declared services. A stopping or failed run is
// still reported with its real state, but it must not mark the app as running:
// otherwise the overview hides Launch and offers a media provider nothing can
// route to.
func TestOverviewTreatsOnlyReadyRunsAsRunning(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.runs = append(env.back.runs, &frameworkv1.AppRun{
		RunId: "run-idle", AppId: "org.example.idle", Pid: 44,
		State: frameworkv1.AppRunState_APP_STOPPING, Runtime: frameworkv1.ApplicationRuntime_RUNTIME_NATIVE,
	})
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 61, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 61).GetOverview()

	for _, provider := range overview.GetProviders() {
		if provider.GetAppId() == "org.example.idle" {
			t.Fatalf("a stopping run was offered as a media provider: %v", overview.GetProviders())
		}
	}
	var idle *webv1.RunView
	for _, run := range overview.GetRuns() {
		if run.GetAppId() == "org.example.idle" {
			idle = run
		}
	}
	if idle == nil {
		t.Fatalf("the stopping run is missing from the run table: %v", overview.GetRuns())
	}
	if idle.GetState() != frameworkv1.AppRunState_APP_STOPPING.String() {
		t.Fatalf("stopping run state = %q, want APP_STOPPING", idle.GetState())
	}
}

// A stop names both the app and the run. The run must belong to that app:
// a stale row or a built-in provider's stable run id can never be stopped from
// another app's page, and the daemon is never asked to try.
func TestStopRequiresARunOfTheRequestedApp(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 62, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Stop{Stop: &webv1.StopRun{AppId: "capsulad.clock", RunId: "run-2"}},
	}}})
	for {
		frame := conn.read()
		if frame.GetRequestId() != 62 {
			continue
		}
		if e := frame.GetError(); e != nil {
			if e.GetCode() != webv1.ErrorCode_ERROR_CODE_CONFLICT {
				t.Fatalf("foreign run stop error = %v, want CONFLICT", e)
			}
			break
		}
	}
	env.back.mu.Lock()
	calls := append([]string(nil), env.back.stopCalls...)
	env.back.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("daemon stop calls = %v, want none for a foreign run id", calls)
	}

	// The socket survives the refusal, so the page can refresh and try again.
	conn.send(&webv1.ClientFrame{RequestId: 63, Body: &webv1.ClientFrame_Ping{Ping: &webv1.Ping{}}})
	for {
		if conn.read().GetPong() != nil {
			break
		}
	}
}

// A refused live read is a per-request error: the snapshot already carries it
// to the page, and the socket must stay open. Closing it would turn one denied
// read into a "disconnected" banner and a reconnect loop that re-issues the
// same refused call.
func TestRefusedRunWatchKeepsSocketAlive(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.watchRunsErr = status.Error(codes.PermissionDenied, "denied run watch")
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 64, Body: &webv1.ClientFrame_Subscribe{
		Subscribe: &webv1.Subscribe{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	conn.waitSnapshot(t, 64)

	conn.send(&webv1.ClientFrame{RequestId: 65, Body: &webv1.ClientFrame_Ping{Ping: &webv1.Ping{}}})
	for {
		if conn.read().GetPong() != nil {
			break
		}
	}
	// Give the refused watcher time to finish. A socket that was going to be
	// torn down for the refusal is closed by now, so the second round trip
	// proves the connection is genuinely still serving, not merely faster than
	// the close.
	time.Sleep(150 * time.Millisecond)
	conn.send(&webv1.ClientFrame{RequestId: 66, Body: &webv1.ClientFrame_Ping{Ping: &webv1.Ping{}}})
	for {
		if conn.read().GetPong() != nil {
			break
		}
	}
}
