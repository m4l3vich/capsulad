package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"capsulad/internal/grpcpeer"
	"capsulad/internal/supervisor"
)

// The daemon's own PID is shared by every connection the daemon opens to its
// own socket (the Web UI, framework cues and the built-in app clients) and by
// every compiled-in run. A connection without a run ID must therefore read as a
// local administrator — with or without the internal token — and never as
// whichever built-in happened to be registered first.
func TestDaemonPIDConnectionIsAdministratorNotBuiltin(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()

	waitFor(t, 5*time.Second, "built-in runs ready", func() bool {
		for _, id := range []string{clockAppID, ringAppID} {
			runs := d.sup.List(id)
			if len(runs) != 1 || !runs[0].Virtual || runs[0].PID != os.Getpid() {
				return false
			}
		}
		return true
	})

	// The Web UI's exact connection: the daemon's local client carries the
	// per-boot internal token but no run ID, and it shares the daemon PID.
	webuiCtx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(internalTokenKey, d.internalToken))
	admin := grpcpeer.WithCreds(webuiCtx, grpcpeer.Creds{PID: os.Getpid(), OK: true})
	if appID, runID, ok := d.callerIdentity(admin); ok {
		t.Fatalf("daemon pid resolved to built-in app=%q run=%q", appID, runID)
	}
	if appID, isApp := d.applicationCaller(admin); isApp {
		t.Fatalf("daemon pid classified as application %q", appID)
	}
	if err := d.requireAdministrativeCaller(admin, "denied"); err != nil {
		t.Fatalf("administrator over the daemon pid was refused: %v", err)
	}
	// The Web UI's real call shape: an administrator reading another app's
	// state. Before the PID fallback excluded virtual runs this was a
	// cross-application denial against the first registered built-in.
	if err := d.requireAppScope(admin, "org.capsula.marusia"); err != nil {
		t.Fatalf("administrator app scope refused: %v", err)
	}

	// A built-in identity still needs both halves: its stable run ID and the
	// per-boot token. The daemon PID alone proves nothing.
	spoofed := metadata.NewIncomingContext(admin, metadata.Pairs("capsula-run-id", d.SystemAppRunID(clockAppID)))
	if appID, runID, ok := d.callerIdentity(spoofed); ok {
		t.Fatalf("untokened built-in claim accepted: app=%q run=%q", appID, runID)
	}
	if appID, isApp := d.applicationCaller(spoofed); isApp {
		t.Fatalf("untokened built-in claim classified as application %q", appID)
	}
}

// A real supervised child is still resolved from its socket peer PID, and the
// same connection stays an application for administrator-only guards. This is
// the warm-run path an app's own SDK relies on when it sends no run ID.
func TestSupervisedSocketPIDResolvesItsOwnRun(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()

	if _, _, _, err := d.Launch("test.probe", map[string]string{"mode": "serve"}); err != nil {
		t.Fatalf("launch test.probe: %v", err)
	}
	waitReady(t, d, "test.probe", 10*time.Second)
	runs := d.sup.List("test.probe")
	if len(runs) != 1 || runs[0].Virtual {
		t.Fatalf("test.probe runs = %+v, want one supervised run", runs)
	}

	peer := grpcpeer.WithCreds(context.Background(), grpcpeer.Creds{PID: runs[0].PID, OK: true})
	appID, runID, ok := d.callerIdentity(peer)
	if !ok || appID != "test.probe" || runID != runs[0].RunID {
		t.Fatalf("supervised pid resolved to app=%q run=%q ok=%v, want test.probe/%s",
			appID, runID, ok, runs[0].RunID)
	}
	if _, isApp := d.applicationCaller(peer); !isApp {
		t.Fatal("supervised pid was not classified as an application")
	}
	if err := d.requireAdministrativeCaller(peer, "denied"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("supervised pid administrator guard = %v, want PermissionDenied", status.Code(err))
	}
}

// A compiled-in run has no child process, so its wire record must not publish
// the daemon's PID: that number belongs to every in-process client and is not
// an app identity. A supervised run keeps its real child PID.
func TestVirtualRunReportsNoChildPID(t *testing.T) {
	builtin := runProto(supervisor.Run{
		RunID: "builtin:capsula.clock", AppID: "capsula.clock",
		PID: os.Getpid(), State: supervisor.Ready, Virtual: true,
	})
	if builtin.GetPid() != 0 {
		t.Fatalf("virtual run pid = %d, want 0 (no child process)", builtin.GetPid())
	}
	if builtin.GetRunId() != "builtin:capsula.clock" || builtin.GetAppId() != "capsula.clock" {
		t.Fatalf("virtual run identity = %+v", builtin)
	}

	child := runProto(supervisor.Run{RunID: "run-1", AppID: "test.probe", PID: 4242, State: supervisor.Ready})
	if child.GetPid() != 4242 {
		t.Fatalf("supervised run pid = %d, want 4242", child.GetPid())
	}
}
