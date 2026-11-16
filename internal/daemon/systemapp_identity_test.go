package daemon

import (
	"context"
	"os"
	"testing"

	"google.golang.org/grpc/metadata"

	"capsulad/internal/grpcpeer"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/supervisor"
)

// A virtual system-app run is bound to the daemon's real peer identity. The
// public in-process client connects over the daemon socket with the daemon PID,
// which must pass while a different local process claiming the same run ID must
// fail through the existing SO_PEERCRED check.
func TestVirtualRunIdentityRequiresDaemonPeer(t *testing.T) {
	logs, err := logbus.New(ids.NewClock(), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(context.Background(), logs, ids.NewClock(), "", nil)
	d := &Daemon{sup: sup, logs: logs, internalToken: "test-token"}
	m := &manifest.Manifest{
		ID: "capsulad.clock", Name: "Clock", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
	}
	runID := d.SystemAppRunID("capsulad.clock")
	run, err := sup.RegisterBuiltin(m, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PID != os.Getpid() {
		t.Fatalf("virtual run pid = %d, want daemon pid %d", run.PID, os.Getpid())
	}

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("capsulad-run-id", runID, internalTokenKey, "test-token"))
	appID, gotRun, ok := d.callerIdentity(grpcpeer.WithCreds(ctx, grpcpeer.Creds{PID: os.Getpid(), OK: true}))
	if !ok || appID != "capsulad.clock" || gotRun != runID {
		t.Fatalf("daemon peer rejected: app=%q run=%q ok=%v", appID, gotRun, ok)
	}

	foreign := os.Getpid() + 1
	if appID, gotRun, ok := d.callerIdentity(grpcpeer.WithCreds(ctx, grpcpeer.Creds{PID: foreign, OK: true})); ok {
		t.Fatalf("foreign peer %d impersonated %q/%q", foreign, appID, gotRun)
	}
}
