package discovery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateHostname(t *testing.T) {
	valid := []string{
		"capsula",
		"capsula-2",
		"a",
		"Capsula",
		strings.Repeat("a", MaxLabelLength),
		"0device",
	}
	for _, name := range valid {
		if err := ValidateHostname(name); err != nil {
			t.Errorf("ValidateHostname(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", MaxLabelLength+1),
		"capsula.local",
		".capsula",
		"-capsula",
		"capsula-",
		"capsula_2",
		"capsula 2",
		"капсула",
	}
	for _, name := range invalid {
		if err := ValidateHostname(name); err == nil {
			t.Errorf("ValidateHostname(%q) = nil, want an error", name)
		}
	}
}

func TestNormalizeHostname(t *testing.T) {
	if got := NormalizeHostname("  Living-Room  "); got != "living-room" {
		t.Fatalf("NormalizeHostname = %q", got)
	}
}

type fakeBackend struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeBackend) setHostname(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "applied.local", nil
}

func (f *fakeBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A save always reconciles with the live responder instead of trusting a
// cached label: that is what repairs an advertisement lost while this process
// was not monitoring the responder. Responder-side idempotence (an active
// label is confirmed without a redundant write) is covered in avahi_apply_test.go.
func TestApplyReconcilesRepeatedSaveWithResponder(t *testing.T) {
	be := &fakeBackend{}
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(context.Background(), " Living-Room "); err != nil {
		t.Fatalf("apply: %v", err)
	}
	status := m.Status()
	if !status.Supported || status.Hostname != "living-room" || status.Advertised != "applied.local" || status.Detail != "" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if err := m.Apply(context.Background(), "living-room"); err != nil {
		t.Fatalf("repeat apply: %v", err)
	}
	if got := be.callCount(); got != 2 {
		t.Fatalf("repeated save reached the responder %d times, want one reconciliation per save", got)
	}
}

func TestInvalidHostnameRetainsPriorAdvertisement(t *testing.T) {
	be := &fakeBackend{}
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(context.Background(), "capsula"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, bad := range []string{"", "two.labels", "-bad", "bad-", "bad_name"} {
		if err := m.Apply(context.Background(), bad); err == nil {
			t.Fatalf("invalid hostname %q was accepted", bad)
		}
		status := m.Status()
		if status.Hostname != "capsula" || status.Advertised != "applied.local" || !status.Supported {
			t.Fatalf("rejected %q changed the live advertisement: %+v", bad, status)
		}
		if status.Detail == "" {
			t.Fatalf("rejected %q left no explanation", bad)
		}
	}
	if got := be.callCount(); got != 1 {
		t.Fatalf("invalid hostnames reached the responder: %d calls", got)
	}
}

func TestApplyUnsupportedReportsStatus(t *testing.T) {
	be := &fakeBackend{err: ErrUnsupported}
	m := newAdvertiser(nil, be, Status{Supported: false, Detail: "no responder"})
	// A wrapped unsupported error is still classified as unsupported.
	be.err = errors.Join(ErrUnsupported, errors.New("no bus"))
	if err := m.Apply(context.Background(), "capsula"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("apply error = %v, want ErrUnsupported", err)
	}
	status := m.Status()
	if status.Supported {
		t.Fatalf("unsupported apply reported supported: %+v", status)
	}
	if status.Detail == "" || status.Hostname != "capsula" || status.Advertised != "" {
		t.Fatalf("unexpected unsupported status: %+v", status)
	}
}

func TestUnsupportedManagerKeepsConfiguredStatus(t *testing.T) {
	b, initial := unsupported("disabled in the simulator")
	if initial.Supported || !strings.Contains(initial.Detail, "simulator") {
		t.Fatalf("initial unsupported status: %+v", initial)
	}
	m := newAdvertiser(nil, b, initial)
	if err := m.Apply(context.Background(), "capsula"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("apply error = %v, want ErrUnsupported", err)
	}
	status := m.Status()
	if status.Supported || status.Hostname != "capsula" || status.Advertised != "" || !strings.Contains(status.Detail, "simulator") {
		t.Fatalf("unsupported status: %+v", status)
	}
}

func TestApplyFailureClearsUnconfirmedAdvertisement(t *testing.T) {
	be := &fakeBackend{}
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(context.Background(), "capsula"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	be.err = errors.New("hostname is already in use on the local network")
	if err := m.Apply(context.Background(), "taken"); err == nil {
		t.Fatal("collision was reported as success")
	}
	status := m.Status()
	if status.Hostname != "taken" || status.Advertised != "" || !status.Supported {
		t.Fatalf("collision must not claim a confirmed advertisement: %+v", status)
	}
	if !strings.Contains(status.Detail, "already in use") {
		t.Fatalf("collision detail = %q", status.Detail)
	}
}

type lifecycleBackend struct {
	mu         sync.Mutex
	calls      []string
	callCh     chan string
	entered    chan string
	release    chan struct{}
	events     chan int32
	initial    int32
	watchCount int
	stopCount  int
}

func newLifecycleBackend(initial int32) *lifecycleBackend {
	return newLifecycleBackendWithStates(initial, 8)
}

func newLifecycleBackendWithStates(initial int32, statesCap int) *lifecycleBackend {
	return &lifecycleBackend{
		callCh:  make(chan string, 32),
		entered: make(chan string, 1),
		events:  make(chan int32, statesCap),
		initial: initial,
	}
}

func (b *lifecycleBackend) setHostname(ctx context.Context, label string) (string, error) {
	b.mu.Lock()
	b.calls = append(b.calls, label)
	release := b.release
	b.mu.Unlock()
	b.callCh <- label
	if release != nil {
		b.entered <- label
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return label + ".local", nil
}

func (b *lifecycleBackend) watch(context.Context) (<-chan int32, int32, func(), error) {
	b.mu.Lock()
	b.watchCount++
	initial := b.initial
	b.mu.Unlock()
	return b.events, initial, func() {
		b.mu.Lock()
		b.stopCount++
		b.mu.Unlock()
	}, nil
}

func (b *lifecycleBackend) setRelease(ch chan struct{}) {
	b.mu.Lock()
	b.release = ch
	b.mu.Unlock()
}

func (b *lifecycleBackend) joinedCalls() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.calls, ",")
}

func (b *lifecycleBackend) lifecycleCounts() (watch, stop int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.watchCount, b.stopCount
}

func waitLabel(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("responder write = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for responder write %q", want)
	}
}

// The subscription must replay the persisted label after the responder
// restarts, while a later user selection remains authoritative. Applying is
// serialized, so the monitor cannot overwrite that newer selection.
func TestMonitorReappliesLatestHostnameAfterResponderRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := newLifecycleBackend(1) // REGISTERING: monitor has nothing to replay yet
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := m.Apply(ctx, "kitchen"); err != nil {
		t.Fatalf("apply kitchen: %v", err)
	}
	waitLabel(t, be.callCh, "kitchen")

	be.events <- stateRunning
	waitLabel(t, be.callCh, "kitchen")

	if err := m.Apply(ctx, "office"); err != nil {
		t.Fatalf("apply office: %v", err)
	}
	waitLabel(t, be.callCh, "office")

	be.events <- 1
	be.events <- stateRunning
	// The monitor reconciles on leaving RUNNING (the responder is unready) and
	// again on entering RUNNING. Those transitions may coalesce into one pass,
	// so wait for the replay rather than a fixed number of backend calls.
	waitLabel(t, be.callCh, "office")

	if status := m.Status(); status.Hostname != "office" || status.Advertised != "office.local" {
		t.Fatalf("status after restart = %+v", status)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if watch, stop := be.lifecycleCounts(); watch != 1 || stop != 1 {
		t.Fatalf("watch=%d stop=%d, want one each", watch, stop)
	}
	calls := be.joinedCalls()
	if !strings.HasPrefix(calls, "kitchen,kitchen,office,") {
		t.Fatalf("responder consults = %q", calls)
	}
	// The monitor is gone; a late transition cannot produce another write.
	be.events <- stateRunning
	if got := be.joinedCalls(); got != calls {
		t.Fatalf("write after close: %q, want %q", got, calls)
	}
}

// A responder that is already RUNNING when the monitor subscribes still gets
// the requested label replayed; this is the daemon-restart/avahi-restart case.
func TestStartReplaysRequestedNameWhenAlreadyRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := newLifecycleBackend(stateRunning)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(ctx, "capsula"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	waitLabel(t, be.callCh, "capsula")
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitLabel(t, be.callCh, "capsula")
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// An in-flight restart reapply must finish before a later user selection and
// must not overwrite it afterwards.
func TestLaterSelectionWinsOverInFlightReapply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := newLifecycleBackend(1)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Apply(ctx, "kitchen"); err != nil {
		t.Fatalf("apply kitchen: %v", err)
	}
	waitLabel(t, be.callCh, "kitchen")

	release := make(chan struct{})
	be.setRelease(release)
	be.events <- stateRunning
	waitLabel(t, be.callCh, "kitchen")
	waitLabel(t, be.entered, "kitchen")

	errCh := make(chan error, 1)
	go func() { errCh <- m.Apply(ctx, "office") }()
	close(release)
	be.setRelease(nil)
	waitLabel(t, be.callCh, "office")
	if err := <-errCh; err != nil {
		t.Fatalf("apply office: %v", err)
	}
	if status := m.Status(); status.Hostname != "office" || status.Advertised != "office.local" {
		t.Fatalf("later selection was overwritten: %+v", status)
	}
	if got := be.joinedCalls(); got != "kitchen,kitchen,office" {
		t.Fatalf("responder writes = %q", got)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func sendStateWithin(t *testing.T, ch chan<- int32, state int32) {
	t.Helper()
	select {
	case ch <- state:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor stopped draining StateChanged while an apply was in flight")
	}
}

// The worker may spend the bounded boot wait and a responder call inside one
// apply. The monitor must keep consuming StateChanged during that window; an
// unbuffered state channel means every send here completes only when the
// monitor accepts it, so a blocking monitor would fail the sends. The pending
// wake coalesces both transitions into one follow-up pass.
func TestMonitorKeepsDrainingAndCoalescesWhileApplyIsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := newLifecycleBackendWithStates(avahiServerRegistering, 0)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(ctx, "kitchen"); err != nil {
		t.Fatalf("apply kitchen: %v", err)
	}
	waitLabel(t, be.callCh, "kitchen")

	release := make(chan struct{})
	be.setRelease(release)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Start precedes any stored request here, so drive the monitor with the
	// RUNNING transition that begins the replay.
	sendStateWithin(t, be.events, stateRunning)
	waitLabel(t, be.callCh, "kitchen")
	waitLabel(t, be.entered, "kitchen")

	// Two transitions arrive while the worker is blocked. The trailing duplicate
	// is a barrier: its send returns only after the monitor has processed the
	// preceding transition and signalled its coalesced wake.
	sendStateWithin(t, be.events, avahiServerRegistering)
	sendStateWithin(t, be.events, stateRunning)
	sendStateWithin(t, be.events, stateRunning)
	close(release)
	be.setRelease(nil)
	waitLabel(t, be.callCh, "kitchen") // single coalesced follow-up pass

	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	status := m.Status()
	if status.Hostname != "kitchen" || status.Advertised != "kitchen.local" || status.Detail != "" {
		t.Fatalf("status after burst = %+v", status)
	}
	if calls := be.joinedCalls(); calls != "kitchen,kitchen,kitchen" {
		t.Fatalf("state burst caused per-event applies: %q", calls)
	}
}

// A transition burst while an apply is blocked must not lose a later selection:
// after release the manager converges on the latest requested label, with a
// bounded number of consults rather than one per queued event.
func TestOverflowBurstConvergesOnLatestSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	be := newLifecycleBackendWithStates(avahiServerRegistering, 0)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(ctx, "kitchen"); err != nil {
		t.Fatalf("apply kitchen: %v", err)
	}
	waitLabel(t, be.callCh, "kitchen")

	release := make(chan struct{})
	be.setRelease(release)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Start precedes any stored request here, so drive the monitor with the
	// RUNNING transition that begins the replay.
	sendStateWithin(t, be.events, stateRunning)
	waitLabel(t, be.callCh, "kitchen")
	waitLabel(t, be.entered, "kitchen")

	for i := range 64 {
		if i%2 == 0 {
			sendStateWithin(t, be.events, avahiServerRegistering)
		} else {
			sendStateWithin(t, be.events, stateRunning)
		}
	}
	// Barrier: this duplicate send returns only after the monitor processed the
	// preceding transition, so no further wake can appear after it.
	sendStateWithin(t, be.events, stateRunning)

	applyDone := make(chan error, 1)
	go func() { applyDone <- m.Apply(ctx, "office") }()
	select {
	case err := <-applyDone:
		t.Fatalf("Apply completed while the responder call was blocked: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	be.setRelease(nil)
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatalf("apply office: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("latest selection did not complete after the burst")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	status := m.Status()
	if status.Hostname != "office" || status.Advertised != "office.local" || status.Detail != "" {
		t.Fatalf("status after overflow = %+v", status)
	}
	if calls := be.joinedCalls(); strings.Count(calls, "office") < 1 || strings.Count(calls, ",") > 4 {
		t.Fatalf("overflow burst produced %q; want the latest selection with a bounded number of applies", calls)
	}
}

// waitStatus polls the consumer-visible status until cond holds. It is test
// synchronization only; the manager itself never polls the responder.
func waitStatus(t *testing.T, m Manager, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if status := m.Status(); cond(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("status never reached expectation: %+v", m.Status())
	return Status{}
}

// A transition out of RUNNING is authoritative: the monitor clears the
// advertised claim instead of continuing to present the old FQDN, and it does
// not write into the unready responder. Returning to RUNNING restores the
// persisted label through a normal write.
func TestMonitorClearsAdvertisementWhenResponderBecomesUnready(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newFakeAvahi(stateRunning, "capsula", "capsula.local")
	be := newAvahiApplyBackend(srv, stateRunning)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(ctx, "capsula"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	be.waitProbe(t)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Close()
	be.waitProbe(t)
	status := waitStatus(t, m, func(s Status) bool { return s.Advertised == "capsula.local" })
	if status.Detail != "" {
		t.Fatalf("initial status = %+v", status)
	}

	// Avahi reports a collision and renames itself; it is no longer advertising
	// the requested label.
	srv.setCurrent("capsula-2", 3)
	be.states <- 3
	be.waitProbe(t)
	status = waitStatus(t, m, func(s Status) bool { return s.Advertised == "" })
	if !status.Supported || status.Hostname != "capsula" || !strings.Contains(status.Detail, "in collision") {
		t.Fatalf("unready status = %+v", status)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("unready responder received writes: %v", writes)
	}

	// Reaching RUNNING again restores the persisted label with one write.
	srv.setCurrent("capsula-2", stateRunning)
	be.states <- stateRunning
	be.waitProbe(t)
	status = waitStatus(t, m, func(s Status) bool { return s.Advertised == "capsula.local" })
	if status.Detail != "" {
		t.Fatalf("recovered status = %+v", status)
	}
	if writes := srv.writes(); len(writes) != 1 || writes[0] != "capsula" {
		t.Fatalf("recovery writes = %v", writes)
	}
}

// Close cancels the monitor context while a responder call is in flight. The
// cancellation must not be stored as a status warning, and a save arriving
// after Close must not resurrect a claim or warning.
func TestCloseCancellationPublishesNoWarningOrStaleStatus(t *testing.T) {
	startCtx, startCancel := context.WithCancel(context.Background())
	defer startCancel()
	be := newLifecycleBackend(avahiServerRegistering)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(startCtx, "capsula"); err != nil {
		t.Fatalf("apply capsula: %v", err)
	}
	waitLabel(t, be.callCh, "capsula")

	release := make(chan struct{}) // never closed: only Close's cancellation releases the call
	be.setRelease(release)
	if err := m.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	sendStateWithin(t, be.events, stateRunning)
	waitLabel(t, be.callCh, "capsula")
	waitLabel(t, be.entered, "capsula")

	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	status := m.Status()
	if status.Hostname != "capsula" || status.Advertised != "capsula.local" || status.Detail != "" {
		t.Fatalf("Close published a canceled result: %+v", status)
	}
	calls := be.joinedCalls()
	if calls != "capsula,capsula" {
		t.Fatalf("Close allowed a post-cancellation apply: %q", calls)
	}

	if err := m.Apply(context.Background(), "office"); err != nil {
		t.Fatalf("apply after close: %v", err)
	}
	if status := m.Status(); status.Hostname != "capsula" || status.Advertised != "capsula.local" || status.Detail != "" {
		t.Fatalf("save after close changed status: %+v", status)
	}
	if got := be.joinedCalls(); got != calls {
		t.Fatalf("save after close reached the responder: %q", got)
	}
}
