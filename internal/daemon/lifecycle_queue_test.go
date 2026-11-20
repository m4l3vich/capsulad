package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
)

const quietInvocationManifest = `
id = "test.quietlaunch"
name = "Quiet launch fixture"
version = "0.1.0"
background = false
start_on_boot = false
restart_on_failure = false

[entrypoint]
runtime = "native"
path = "quiet"
architecture = "aarch64"
abi = "linux-musl"

[[arguments]]
name = "seq"
type = "text"
`

// nextInvocationID reads the run's inbound stream until the daemon delivers an
// invocation, and returns its ID. Settings and other control frames are
// skipped; the test only cares which invocation reached the process.
func nextInvocationID(t *testing.T, out <-chan *frameworkv1.AppInbound) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-out:
			if inv := msg.GetInvocation(); inv != nil {
				return inv.GetInvocationId()
			}
		case <-deadline:
			t.Fatal("invocation was not delivered to the run")
			return ""
		}
	}
}

// TestInvocationAfterReadyFlushIsNotStranded pins the ordering between the
// APP_READY flush and a launch that resolved the run while it was still
// starting. The run is connected but has not reported ready; the test then
// forces the daemon's ready transition before handing the launch to
// deliverOrQueue. That is the interleaving that used to strand the invocation:
// the old readiness check read the bus outside invMu, the flush drained an
// empty queue, and the launch appended afterwards with nothing left to send
// it. The invocation must now be delivered in place.
func TestInvocationAfterReadyFlushIsNotStranded(t *testing.T) {
	const appID = "test.quietlaunch"
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "quiet")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, cancel, _ := newTestDaemonWith(t, appInstall{
		id: appID, binaryName: "quiet", binaryPath: script,
		manifest: []byte(quietInvocationManifest),
	})
	defer cancel()

	runID, firstInvocation, spawned, err := d.Launch(appID, map[string]string{"seq": "first"})
	if err != nil || !spawned {
		t.Fatalf("launch quiet fixture: spawned=%v err=%v", spawned, err)
	}
	if _, ready := d.sup.ReadyRun(appID); ready {
		t.Fatal("quiet fixture reported ready before the test forced the transition")
	}

	// Attach a real bus connection for the run, but do not send APP_READY.
	// This is exactly the transport state the old check read while the daemon
	// was already completing the ready handshake.
	stream := &fakeActivationStream{
		ctx: d.ctx,
		in:  make(chan *frameworkv1.AppOutbound, 8),
		out: make(chan *frameworkv1.AppInbound, 8),
	}
	go func() { _ = d.bus.Connect(stream) }()
	stream.in <- &frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_Hello{Hello: &frameworkv1.AppHello{RunId: runID}}}
	waitFor(t, 2*time.Second, "quiet fixture bus connection", func() bool {
		return d.bus.Connected(runID)
	})

	d.onAppReady(runID, appID)
	if got := nextInvocationID(t, stream.out); got != firstInvocation {
		t.Fatalf("flushed invocation = %s, want %s", got, firstInvocation)
	}

	secondRunID, secondInvocation, spawnedAgain, err := d.Launch(appID, map[string]string{"seq": "second"})
	if err != nil {
		t.Fatalf("second launch: %v", err)
	}
	if spawnedAgain {
		t.Fatal("second launch spawned another process for a live run")
	}
	if secondRunID != runID {
		t.Fatalf("second launch routed to run %s, want %s", secondRunID, runID)
	}
	if got := nextInvocationID(t, stream.out); got != secondInvocation {
		t.Fatalf("late invocation = %s, want %s", got, secondInvocation)
	}

	d.invMu.Lock()
	pending := len(d.pending[runID])
	delivery := d.delivery[runID]
	d.invMu.Unlock()
	if pending != 0 {
		t.Fatalf("invocations left queued after the ready flush: %d", pending)
	}
	if delivery != deliveryReady {
		t.Fatalf("run delivery state = %v, want deliveryReady", delivery)
	}

	// Once the run is gone, a launch that still resolves its record must be
	// refused instead of queued: there is no later flush to carry it.
	if !d.sup.Stop(runID) {
		t.Fatal("stop quiet fixture")
	}
	waitFor(t, 5*time.Second, "quiet fixture run ended", func() bool {
		d.invMu.Lock()
		defer d.invMu.Unlock()
		return d.delivery[runID] == deliveryEnded
	})
	orphan := &invocationRecord{ID: "inv_orphan", AppID: appID, RunID: runID}
	if err := d.deliverOrQueue(orphan); err == nil {
		t.Fatal("deliverOrQueue accepted an invocation for an ended run")
	}
	d.invMu.Lock()
	pending = len(d.pending[runID])
	d.invMu.Unlock()
	if pending != 0 {
		t.Fatalf("ended run queued %d invocations that no flush can carry", pending)
	}
}

// launchQuietFixture starts the quiet launch fixture and returns the daemon,
// its live run, and the invocation queued for that run's start.
func launchQuietFixture(t *testing.T) (*Daemon, string, string) {
	t.Helper()
	const appID = "test.quietlaunch"
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "quiet")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, cancel, _ := newTestDaemonWith(t, appInstall{
		id: appID, binaryName: "quiet", binaryPath: script,
		manifest: []byte(quietInvocationManifest),
	})
	t.Cleanup(cancel)
	runID, invocationID, spawned, err := d.Launch(appID, map[string]string{"seq": "first"})
	if err != nil || !spawned {
		t.Fatalf("launch quiet fixture: spawned=%v err=%v", spawned, err)
	}
	return d, runID, invocationID
}

// TestLiveRunLaunchHistoryIsBounded pins retention on the reuse path. A launch
// against an already-ready app never spawns; its history record is inserted by
// newInvocation. Bounding only the spawn path let repeated launches grow that
// map for as long as the app stayed live.
func TestLiveRunLaunchHistoryIsBounded(t *testing.T) {
	const appID = "test.quietlaunch"
	d, runID, initialInvocation := launchQuietFixture(t)

	stream := &fakeActivationStream{
		ctx: d.ctx,
		in:  make(chan *frameworkv1.AppOutbound, 8),
		out: make(chan *frameworkv1.AppInbound, 8),
	}
	go func() { _ = d.bus.Connect(stream) }()
	stream.in <- &frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_Hello{Hello: &frameworkv1.AppHello{RunId: runID}}}
	waitFor(t, 2*time.Second, "quiet fixture bus connection", func() bool {
		return d.bus.Connected(runID)
	})
	d.onAppReady(runID, appID)
	if got := nextInvocationID(t, stream.out); got != initialInvocation {
		t.Fatalf("initial invocation = %s, want %s", got, initialInvocation)
	}
	if _, ready := d.sup.ReadyRun(appID); !ready {
		t.Fatal("quiet fixture did not become ready for in-place delivery")
	}

	var firstLive, lastLive string
	for i := 0; i < maxInvocationRecords+8; i++ {
		gotRunID, invocationID, spawned, err := d.Launch(appID, map[string]string{"seq": fmt.Sprintf("live-%d", i)})
		if err != nil {
			t.Fatalf("launch %d on live run: %v", i, err)
		}
		if spawned {
			t.Fatalf("launch %d spawned a second process for run %s", i, runID)
		}
		if gotRunID != runID {
			t.Fatalf("launch %d routed to run %s, want %s", i, gotRunID, runID)
		}
		if i == 0 {
			firstLive = invocationID
		}
		lastLive = invocationID
		if delivered := nextInvocationID(t, stream.out); delivered != invocationID {
			t.Fatalf("launch %d delivered invocation = %s, want %s", i, delivered, invocationID)
		}
	}

	d.invMu.Lock()
	retained := len(d.invocations)
	_, initialInHistory := d.invocations[initialInvocation]
	_, firstLiveInHistory := d.invocations[firstLive]
	_, lastLiveInHistory := d.invocations[lastLive]
	pending := len(d.pending[runID])
	d.invMu.Unlock()

	if retained != maxInvocationRecords {
		t.Fatalf("retained invocation records = %d, want %d", retained, maxInvocationRecords)
	}
	if initialInHistory {
		t.Fatal("initial spawn invocation survived past the retention bound")
	}
	if firstLiveInHistory {
		t.Fatal("oldest live-run invocation survived past the retention bound")
	}
	if !lastLiveInHistory {
		t.Fatal("newest live-run invocation was evicted by retention")
	}
	if pending != 0 {
		t.Fatalf("ready run left %d invocations pending", pending)
	}

	// The newest record must still correlate the app's result, not merely
	// occupy a map slot.
	d.onInvocationResult(runID, &frameworkv1.AppInvocationResult{
		InvocationId: lastLive, Accepted: true, Detail: "retained",
	})
	d.invMu.Lock()
	rec := d.invocations[lastLive]
	accepted := rec != nil && rec.Accepted != nil && *rec.Accepted
	detail := ""
	if rec != nil {
		detail = rec.Detail
	}
	d.invMu.Unlock()
	if !accepted || detail != "retained" {
		t.Fatalf("newest invocation result = accepted:%v detail:%q, want accepted:true detail:\"retained\"", accepted, detail)
	}
}

// TestInvocationRetentionKeepsPendingDelivery pins the interaction between
// trimming and the starting-run queue: an old record may leave the bounded
// history while its invocation is still pending, and the ready flush must
// deliver it from the queue. The launch that reuses the starting run inserts
// through newInvocation and must remain in history.
func TestInvocationRetentionKeepsPendingDelivery(t *testing.T) {
	const appID = "test.quietlaunch"
	d, runID, firstInvocation := launchQuietFixture(t)

	// Fill the bounded history through the same insertion path a live-run
	// launch uses, while the run has not reported ready and its invocation is
	// still queued.
	for i := 0; i < maxInvocationRecords+8; i++ {
		d.newInvocation(appID, runID, map[string]string{"seq": fmt.Sprintf("filler-%d", i)})
	}

	d.invMu.Lock()
	retained := len(d.invocations)
	_, firstInHistory := d.invocations[firstInvocation]
	pending := len(d.pending[runID])
	d.invMu.Unlock()
	if retained != maxInvocationRecords {
		t.Fatalf("retained invocation records = %d, want %d", retained, maxInvocationRecords)
	}
	if firstInHistory {
		t.Fatal("oldest pending invocation survived past the retention bound")
	}
	if pending != 1 {
		t.Fatalf("pending invocations after trimming = %d, want 1", pending)
	}

	_, secondInvocation, spawned, err := d.Launch(appID, map[string]string{"seq": "second"})
	if err != nil {
		t.Fatalf("second launch on starting run: %v", err)
	}
	if spawned {
		t.Fatal("second launch spawned another process for a live run")
	}

	d.invMu.Lock()
	retained = len(d.invocations)
	_, secondInHistory := d.invocations[secondInvocation]
	pending = len(d.pending[runID])
	d.invMu.Unlock()
	if retained != maxInvocationRecords {
		t.Fatalf("retained invocation records after reuse = %d, want %d", retained, maxInvocationRecords)
	}
	if !secondInHistory {
		t.Fatal("new invocation from a starting-run reuse was evicted by retention")
	}
	if pending != 2 {
		t.Fatalf("pending invocations = %d, want 2", pending)
	}

	stream := &fakeActivationStream{
		ctx: d.ctx,
		in:  make(chan *frameworkv1.AppOutbound, 8),
		out: make(chan *frameworkv1.AppInbound, 8),
	}
	go func() { _ = d.bus.Connect(stream) }()
	stream.in <- &frameworkv1.AppOutbound{Message: &frameworkv1.AppOutbound_Hello{Hello: &frameworkv1.AppHello{RunId: runID}}}
	waitFor(t, 2*time.Second, "quiet fixture bus connection", func() bool {
		return d.bus.Connected(runID)
	})
	d.onAppReady(runID, appID)
	if got := nextInvocationID(t, stream.out); got != firstInvocation {
		t.Fatalf("first pending invocation = %s, want %s", got, firstInvocation)
	}
	if got := nextInvocationID(t, stream.out); got != secondInvocation {
		t.Fatalf("second pending invocation = %s, want %s", got, secondInvocation)
	}

	d.invMu.Lock()
	pending = len(d.pending[runID])
	delivery := d.delivery[runID]
	d.invMu.Unlock()
	if pending != 0 {
		t.Fatalf("invocations left queued after the ready flush: %d", pending)
	}
	if delivery != deliveryReady {
		t.Fatalf("run delivery state = %v, want deliveryReady", delivery)
	}
}
