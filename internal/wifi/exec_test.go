package wifi

import (
	"context"
	"testing"
	"time"
)

func TestExecRunnerStartHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := (execRunner{}).Start(ctx, "sleep", "60")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.Kill()
		t.Fatal("long-lived child did not stop when its context was cancelled")
	}
}
