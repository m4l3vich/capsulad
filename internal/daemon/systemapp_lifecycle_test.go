package daemon

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/supervisor"
)

func TestLaunchDoesNotSucceedDuringBuiltinStop(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	// Hold the real clock's teardown boundary so Stop and Launch overlap
	// deterministically without replacing either lifecycle implementation.
	d.builtins.clock.alertMu.Lock()
	locked := true
	defer func() {
		if locked {
			d.builtins.clock.alertMu.Unlock()
		}
	}()
	done := make(chan bool, 1)
	runID := d.SystemAppRunID(clockAppID)
	go func() { done <- d.Stop(runID) }()
	waitFor(t, time.Second, "clock stopping", func() bool {
		run, exists := d.sup.Get(runID)
		return exists && run.State == supervisor.Stopping
	})
	if _, _, _, err := d.Launch(clockAppID, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Launch during Stop returned %v; must not promise a run about to disappear", err)
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
	if _, _, _, err := d.Launch(clockAppID, nil); err != nil {
		t.Fatal(err)
	}
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
}

func TestBuiltinWatchReportsStoppedRun(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	conn := dialTest(t, d.cfg.SocketPath)
	client := frameworkv1.NewApplicationServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch, err := client.WatchRuns(ctx, &frameworkv1.WatchRunsRequest{AppId: clockAppID})
	if err != nil {
		t.Fatal(err)
	}
	first, err := watch.Recv()
	if err != nil || first.GetRun().GetState() != frameworkv1.AppRunState_APP_READY {
		t.Fatalf("initial clock run: %v, %v", first, err)
	}
	if _, err := client.Stop(ctx, &frameworkv1.StopRequest{RunId: d.SystemAppRunID(clockAppID)}); err != nil {
		t.Fatal(err)
	}
	for {
		event, err := watch.Recv()
		if err != nil {
			t.Fatalf("watch never reported the stopped run: %v", err)
		}
		if event.GetRun().GetState() == frameworkv1.AppRunState_APP_EXITED {
			if event.GetRun().GetRunId() != d.SystemAppRunID(clockAppID) {
				t.Fatalf("wrong terminal run: %v", event)
			}
			break
		}
	}
}
