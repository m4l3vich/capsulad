package discovery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAvahi models the public Avahi Server methods an apply uses. stateSeq
// drives the bounded REGISTERING wait; raceTo models a different writer
// reaching the requested name between the pre-read and SetHostName.
type fakeAvahi struct {
	mu       sync.Mutex
	state    int32
	stateSeq []int32
	hostname string
	fqdn     string
	stateErr error
	fqdnErr  error
	setErr   error
	raceTo   string
	// postSetStates models the REGISTERING/probing states a successful
	// SetHostName can produce before the name is advertised.
	postSetStates []int32
	setCalls      []string
	stateReads    int
}

func newFakeAvahi(state int32, hostname, fqdn string) *fakeAvahi {
	return &fakeAvahi{state: state, hostname: hostname, fqdn: fqdn}
}

func (f *fakeAvahi) getState(context.Context) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateReads++
	if f.stateErr != nil {
		return 0, f.stateErr
	}
	if len(f.stateSeq) > 0 {
		state := f.stateSeq[0]
		if len(f.stateSeq) > 1 {
			f.stateSeq = f.stateSeq[1:]
		}
		return state, nil
	}
	return f.state, nil
}

func (f *fakeAvahi) getHostName(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostname, nil
}

func (f *fakeAvahi) getHostNameFqdn(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fqdn, f.fqdnErr
}

func (f *fakeAvahi) setHostName(_ context.Context, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, label)
	if f.setErr != nil {
		if f.raceTo != "" {
			f.hostname = f.raceTo
			f.fqdn = f.raceTo + ".local"
		}
		return f.setErr
	}
	f.hostname = label
	f.fqdn = label + ".local"
	if len(f.postSetStates) > 0 {
		f.stateSeq = append([]int32(nil), f.postSetStates...)
	}
	return nil
}

func (f *fakeAvahi) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setCalls...)
}

func (f *fakeAvahi) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateReads
}

func (f *fakeAvahi) resetHostname(hostname string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostname = hostname
	f.fqdn = hostname + ".local"
}

func (f *fakeAvahi) setCurrent(hostname string, state int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostname = hostname
	f.fqdn = hostname + ".local"
	f.state = state
}

// avahiRedundantReply is Avahi's answer to SetHostName for the name it already
// owns. The message is the one observed on the device; the name is wrapped so
// classification uses the D-Bus name, never the message text.
func avahiRedundantReply() error {
	return avahiDMSError{
		name: "org.freedesktop.Avahi.InvalidOperationError",
		err:  errors.New("The requested operation is invalid because redundant."),
	}
}

// Opening Settings reads discovery.Status. Startup applies the persisted label
// first; when Avahi already owns that label the apply must succeed without a
// SetHostName write, so the page shows the live advertisement and no warning.
func TestApplyHostNameSkipsWriteWhenRequestedLabelIsActive(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula", "capsula.local")
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err != nil {
		t.Fatalf("apply active label: %v", err)
	}
	if got != "capsula.local" {
		t.Fatalf("advertised name = %q, want capsula.local", got)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("active label reached SetHostName: %v", writes)
	}
}

// A concurrent writer can make our SetHostName redundant: the responder then
// reports the requested label as current, which is the advertised state.
func TestApplyHostNameAcceptsRedundantReplyOnlyAfterCurrentNameVerification(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.setErr = avahiRedundantReply()
	srv.raceTo = "capsula"
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err != nil {
		t.Fatalf("verified redundant reply: %v", err)
	}
	if got != "capsula.local" {
		t.Fatalf("advertised name = %q, want capsula.local", got)
	}
	if writes := srv.writes(); len(writes) != 1 || writes[0] != "capsula" {
		t.Fatalf("attempted writes = %v", writes)
	}
}

// A redundant reply is not proof of success: the responder must actually own
// the requested name. A stale current name keeps the genuine failure visible.
func TestApplyHostNameRedundantReplyWithStaleNameStillFails(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.setErr = avahiRedundantReply()
	_, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil {
		t.Fatal("redundant reply with a stale current name was treated as success")
	}
	if !strings.Contains(err.Error(), "redundant") || !strings.Contains(err.Error(), "capsula-old.local") {
		t.Fatalf("error did not preserve the responder state: %v", err)
	}
}

func TestApplyHostNameCollisionStillFails(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-2", "capsula-2.local")
	srv.setErr = avahiDMSError{
		name: "org.freedesktop.Avahi.HostNameCollisionError",
		err:  errors.New("Host name collision"),
	}
	_, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil {
		t.Fatal("collision was reported as success")
	}
	if !strings.Contains(err.Error(), "already in use") || !strings.Contains(err.Error(), "capsula-2.local") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestApplyHostNameInvalidLabelStillFails(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula", "capsula.local")
	srv.setErr = avahiDMSError{
		name: "org.freedesktop.Avahi.InvalidHostNameError",
		err:  errors.New("Invalid host name"),
	}
	_, err := applyHostName(context.Background(), srv, "bad_name")
	if err == nil {
		t.Fatal("invalid label was reported as success")
	}
	if !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("invalid label error = %v", err)
	}
}

func TestApplyHostNameMissingResponderIsUnsupported(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "", "")
	srv.stateErr = errors.New("system bus unavailable")
	_, err := applyHostName(context.Background(), srv, "capsula")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("apply error = %v, want ErrUnsupported", err)
	}
}

// At boot the system bus may activate Avahi while it is still REGISTERING.
// The apply waits for RUNNING before deciding that the active label needs no
// write, instead of surfacing the responder's transient state as a warning.
func TestApplyHostNameWaitsForRunningBeforeSkippingWrite(t *testing.T) {
	srv := newFakeAvahi(avahiServerRegistering, "capsula", "capsula.local")
	srv.stateSeq = []int32{avahiServerRegistering, stateRunning}
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err != nil {
		t.Fatalf("apply while registering: %v", err)
	}
	if got != "capsula.local" {
		t.Fatalf("advertised name = %q", got)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("active label reached SetHostName: %v", writes)
	}
	if reads := srv.reads(); reads < 2 {
		t.Fatalf("state was read %d times, want the REGISTERING wait to observe RUNNING", reads)
	}
}

// avahiApplyBackend routes the manager's responder writes through the same
// apply ordering the Avahi adapter uses. probes lets a test synchronize with
// the state monitor without polling the manager.
type avahiApplyBackend struct {
	srv     *fakeAvahi
	states  chan int32
	probes  chan struct{}
	initial int32
}

func newAvahiApplyBackend(srv *fakeAvahi, initial int32) *avahiApplyBackend {
	return &avahiApplyBackend{
		srv:     srv,
		states:  make(chan int32, 8),
		probes:  make(chan struct{}, 8),
		initial: initial,
	}
}

func (b *avahiApplyBackend) setHostname(ctx context.Context, label string) (string, error) {
	fqdn, err := applyHostName(ctx, b.srv, label)
	b.probes <- struct{}{}
	return fqdn, err
}

func (b *avahiApplyBackend) watch(context.Context) (<-chan int32, int32, func(), error) {
	return b.states, b.initial, func() {}, nil
}

func (b *avahiApplyBackend) waitProbe(t *testing.T) {
	t.Helper()
	select {
	case <-b.probes:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the responder apply")
	}
}

// The daemon starts the monitor before applying persisted settings. With
// Avahi RUNNING on the default name, that first apply reports the live
// advertisement and does not write. This is the Settings-page state after a
// restart: no Redundant warning, no needless responder write.
func TestStartupApplyOfActiveLabelReportsAdvertisementWithoutWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newFakeAvahi(stateRunning, "capsula", "capsula.local")
	be := newAvahiApplyBackend(srv, stateRunning)
	m := newAdvertiser(nil, be, Status{Supported: true, Detail: "mDNS host name has not been applied yet"})
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Close()

	if err := m.Apply(ctx, "capsula"); err != nil {
		t.Fatalf("startup apply: %v", err)
	}
	status := m.Status()
	if !status.Supported || status.Hostname != "capsula" || status.Advertised != "capsula.local" || status.Detail != "" {
		t.Fatalf("startup status = %+v", status)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("startup apply wrote the active label: %v", writes)
	}
}

// A responder restart transitions through REGISTERING and back to RUNNING.
// Replaying the already active label must not produce a redundant write or a
// stored warning that a later Settings GET would display.
func TestRunningReapplyOfActiveLabelKeepsStatusClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newFakeAvahi(stateRunning, "capsula", "capsula.local")
	be := newAvahiApplyBackend(srv, stateRunning)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Close()
	if err := m.Apply(ctx, "capsula"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	be.waitProbe(t) // the initial apply

	be.states <- stateRunning // duplicate snapshot: not a transition
	be.states <- avahiServerRegistering
	be.states <- stateRunning
	be.waitProbe(t) // the restart replay

	status := m.Status()
	if status.Hostname != "capsula" || status.Advertised != "capsula.local" || status.Detail != "" {
		t.Fatalf("status after replay = %+v", status)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("restart replay wrote the active label: %v", writes)
	}
}

// A responder restart loses the runtime rename. The monitor must still restore
// the persisted label: functional reapplication is not traded for idempotence.
func TestResponderRestartReappliesChangedLabel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newFakeAvahi(stateRunning, "capsula", "capsula.local")
	be := newAvahiApplyBackend(srv, stateRunning)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(ctx, "kitchen"); err != nil {
		t.Fatalf("apply kitchen: %v", err)
	}
	be.waitProbe(t)
	srv.resetHostname("capsula") // Avahi restarted and reloaded its own config

	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Close()
	be.waitProbe(t)

	status := m.Status()
	if status.Hostname != "kitchen" || status.Advertised != "kitchen.local" || status.Detail != "" {
		t.Fatalf("status after restart = %+v", status)
	}
	writes := srv.writes()
	if len(writes) != 2 || writes[0] != "kitchen" || writes[1] != "kitchen" {
		t.Fatalf("responder writes = %v, want the initial apply and the restart reapply", writes)
	}
}

// A failed write whose current state does not match the request stays a real
// failure at the consumer boundary: no advertised claim, and the reason is
// stored for the next Settings GET.
func TestUnverifiedRedundantWriteRemainsVisibleFailure(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.setErr = avahiRedundantReply()
	be := newAvahiApplyBackend(srv, avahiServerRegistering)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(context.Background(), "capsula"); err == nil {
		t.Fatal("unverified redundant write was reported as success")
	}
	status := m.Status()
	if !status.Supported || status.Hostname != "capsula" || status.Advertised != "" {
		t.Fatalf("failed apply kept an advertisement claim: %+v", status)
	}
	if !strings.Contains(status.Detail, "redundant") {
		t.Fatalf("failure detail = %q", status.Detail)
	}
}

// shortRegisteringWait keeps the boot-wait ordering testable without sleeping
// through the production timeout.
func shortRegisteringWait(t *testing.T) {
	t.Helper()
	previous := avahiRegisteringWait
	avahiRegisteringWait = 20 * time.Millisecond
	t.Cleanup(func() { avahiRegisteringWait = previous })
}

// A responder that is still REGISTERING when the bounded boot wait expires
// must not be reported as advertising the requested name, even when its
// current name already matches, and must not receive a write that cannot be
// confirmed.
func TestApplyHostNameDoesNotClaimAdvertisementWhileStillRegistering(t *testing.T) {
	shortRegisteringWait(t)
	srv := newFakeAvahi(avahiServerRegistering, "capsula", "capsula.local")
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil {
		t.Fatal("still-registering responder was reported as advertised")
	}
	if got != "" {
		t.Fatalf("advertised name = %q, want none", got)
	}
	if !strings.Contains(err.Error(), "registering") || !strings.Contains(err.Error(), "not advertised") {
		t.Fatalf("not-ready error = %v", err)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("unready responder received writes: %v", writes)
	}
}

// COLLISION and FAILURE are authoritative non-advertising states: a matching
// current name is not enough to claim an advertisement, and the apply must not
// write into a responder that cannot confirm it.
func TestApplyHostNameDoesNotClaimAdvertisementInUnreadyStates(t *testing.T) {
	for _, tc := range []struct {
		state int32
		want  string
	}{
		{3, "in collision"},
		{4, "in failure"},
	} {
		srv := newFakeAvahi(tc.state, "capsula", "capsula.local")
		got, err := applyHostName(context.Background(), srv, "capsula")
		if err == nil {
			t.Fatalf("state %d reported an advertisement", tc.state)
		}
		if got != "" || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("state %d: got %q err %v, want %q", tc.state, got, err, tc.want)
		}
		if writes := srv.writes(); len(writes) != 0 {
			t.Fatalf("state %d received writes: %v", tc.state, writes)
		}
	}
}

// A setter error whose readback matches the requested name is not success
// while the responder has left RUNNING.
func TestApplyHostNameSetterErrorReadbackRequiresRunning(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.setErr = avahiRedundantReply()
	srv.raceTo = "capsula"
	srv.stateSeq = []int32{stateRunning, 3}
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil {
		t.Fatal("setter-error readback claimed success without RUNNING")
	}
	if got != "" || !strings.Contains(err.Error(), "in collision") {
		t.Fatalf("got %q err %v, want a collision not-ready error", got, err)
	}
	if writes := srv.writes(); len(writes) != 1 || writes[0] != "capsula" {
		t.Fatalf("attempted writes = %v", writes)
	}
}

// The setter may succeed while the responder is no longer RUNNING; without an
// authoritative RUNNING confirmation no advertisement is claimed.
func TestApplyHostNameSuccessfulSetterRequiresRunningConfirmation(t *testing.T) {
	shortRegisteringWait(t)
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.stateSeq = []int32{stateRunning, avahiServerRegistering}
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil {
		t.Fatal("successful setter claimed an advertisement while registering")
	}
	if got != "" || !strings.Contains(err.Error(), "registering") {
		t.Fatalf("got %q err %v, want a registering not-ready error", got, err)
	}
	if writes := srv.writes(); len(writes) != 1 || writes[0] != "capsula" {
		t.Fatalf("attempted writes = %v", writes)
	}
}

// A responder FQDN read failure after a write keeps Advertised empty and
// reports the verification failure; no synthetic label+".local" is invented.
func TestApplyHostNameUnconfirmedFQDNIsNotFabricated(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.fqdnErr = errors.New("D-Bus read failed")
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil {
		t.Fatal("unconfirmed advertised name was reported as success")
	}
	if got != "" {
		t.Fatalf("advertised name = %q, want none", got)
	}
	if !strings.Contains(err.Error(), "unconfirmed") || !strings.Contains(err.Error(), "D-Bus read failed") {
		t.Fatalf("unconfirmed error = %v", err)
	}
	if strings.Contains(err.Error(), ".local") {
		t.Fatalf("error fabricated an advertised FQDN: %v", err)
	}
	if writes := srv.writes(); len(writes) != 1 {
		t.Fatalf("attempted writes = %v", writes)
	}
}

// An empty FQDN from an otherwise RUNNING responder is not an advertisement
// confirmation and is not replaced with a synthetic name.
func TestApplyHostNameEmptyFQDNIsUnconfirmed(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula", "")
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil || got != "" {
		t.Fatalf("empty FQDN: got %q err %v", got, err)
	}
	if !strings.Contains(err.Error(), "unconfirmed") || !strings.Contains(err.Error(), "no advertised name") {
		t.Fatalf("empty FQDN error = %v", err)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("empty FQDN caused a write: %v", writes)
	}
}

// The consumer-visible status for an unready responder has no advertisement
// claim and carries the authoritative state, with no responder write.
func TestUnreadyApplyKeepsStatusHonest(t *testing.T) {
	srv := newFakeAvahi(3, "capsula", "capsula.local")
	be := newAvahiApplyBackend(srv, avahiServerRegistering)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(context.Background(), "capsula"); err == nil {
		t.Fatal("collision-state responder was reported as advertised")
	}
	status := m.Status()
	if !status.Supported || status.Hostname != "capsula" || status.Advertised != "" || !strings.Contains(status.Detail, "in collision") {
		t.Fatalf("unready status = %+v", status)
	}
	if writes := srv.writes(); len(writes) != 0 {
		t.Fatalf("unready responder received writes: %v", writes)
	}
}

// An unconfirmed advertised name stays visibly unconfirmed at the consumer
// boundary instead of claiming the conventional FQDN.
func TestUnconfirmedApplyKeepsStatusHonest(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula", "capsula.local")
	srv.fqdnErr = errors.New("D-Bus read failed")
	be := newAvahiApplyBackend(srv, avahiServerRegistering)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(context.Background(), "capsula"); err == nil {
		t.Fatal("unconfirmed advertised name was reported as success")
	}
	status := m.Status()
	if !status.Supported || status.Hostname != "capsula" || status.Advertised != "" || !strings.Contains(status.Detail, "unconfirmed") {
		t.Fatalf("unconfirmed status = %+v", status)
	}
}

// A successful SetHostName can return while Avahi is probing the changed name
// (RUNNING → Set → REGISTERING → RUNNING). The confirmation reuses the bounded
// wait and succeeds once RUNNING, with exactly one setter call.
func TestApplyHostNameWaitsOutPostSetterProbing(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.postSetStates = []int32{avahiServerRegistering, stateRunning}
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err != nil {
		t.Fatalf("post-setter probing apply: %v", err)
	}
	if got != "capsula.local" {
		t.Fatalf("advertised name = %q, want capsula.local", got)
	}
	if writes := srv.writes(); len(writes) != 1 || writes[0] != "capsula" {
		t.Fatalf("setter calls = %v, want exactly one", writes)
	}
}

// A probe that never reaches RUNNING within the bounded wait is honest: no
// advertisement claim, one setter attempt, no retry loop.
func TestApplyHostNamePostSetterProbingTimeoutKeepsClaimHonest(t *testing.T) {
	shortRegisteringWait(t)
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.postSetStates = []int32{avahiServerRegistering}
	got, err := applyHostName(context.Background(), srv, "capsula")
	if err == nil || got != "" {
		t.Fatalf("stuck probe: got %q err %v, want an empty claim", got, err)
	}
	if !strings.Contains(err.Error(), "registering") {
		t.Fatalf("stuck probe error = %v", err)
	}
	if writes := srv.writes(); len(writes) != 1 {
		t.Fatalf("setter calls = %v, want exactly one attempt", writes)
	}
}

// The consumer-visible result of a legitimate post-setter probe is a clean
// advertisement, not a temporary "saved but advertising failed" warning.
func TestPostSetterProbingApplyKeepsStatusClean(t *testing.T) {
	srv := newFakeAvahi(stateRunning, "capsula-old", "capsula-old.local")
	srv.postSetStates = []int32{avahiServerRegistering, stateRunning}
	be := newAvahiApplyBackend(srv, avahiServerRegistering)
	m := newAdvertiser(nil, be, Status{Supported: true})
	if err := m.Apply(context.Background(), "capsula"); err != nil {
		t.Fatalf("post-setter probing apply: %v", err)
	}
	status := m.Status()
	if !status.Supported || status.Hostname != "capsula" || status.Advertised != "capsula.local" || status.Detail != "" {
		t.Fatalf("post-setter probing status = %+v", status)
	}
	if writes := srv.writes(); len(writes) != 1 {
		t.Fatalf("setter calls = %v, want exactly one", writes)
	}
}
