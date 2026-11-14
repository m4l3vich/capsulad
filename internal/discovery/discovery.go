// Package discovery advertises the device on the local network as
// <hostname>.local over mDNS.
//
// On the stock Linux image Avahi owns multicast DNS, so this package changes
// its host name over the system D-Bus instead of opening the 5353 socket a
// second time. Elsewhere it reports an explicit unsupported status. The
// configured host name is durable in the user settings store; callers apply it
// at startup and after every settings change.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"capsulad/internal/logbus"
)

// ErrUnsupported means no mDNS responder is available in this build or on
// this machine. The configured host name is retained; advertisement is not
// running.
var ErrUnsupported = errors.New("mDNS advertisement is unavailable")

// MaxLabelLength is the DNS single-label limit.
const MaxLabelLength = 63

// Status is the advertisement state a UI can show. Hostname is the requested
// label; Advertised is the fully-qualified name the responder last confirmed.
// On a responder error Advertised is cleared because the previous registration
// may have been lost; the error text carries the observed responder state.
type Status struct {
	Supported  bool
	Hostname   string // requested label (normalized)
	Advertised string // FQDN last confirmed by the responder
	Detail     string // why advertisement is unavailable or failed
}

// Manager applies a host name to the local mDNS responder and keeps it applied
// across responder restarts.
type Manager interface {
	// Start subscribes to responder state changes and reconciles the requested
	// label on each transition, including clearing an advertisement the
	// responder can no longer confirm. It is idempotent.
	Start(ctx context.Context) error
	Apply(ctx context.Context, hostname string) error
	Status() Status
	// Close stops the monitor and releases the bus match. It is idempotent.
	Close() error
}

// responderState mirrors the Avahi server states used on the device.
const (
	stateRunning int32 = 2
)

type backend interface {
	// setHostname applies label and returns the responder's fully-qualified
	// advertised name.
	setHostname(ctx context.Context, label string) (string, error)
}

// stateWatcher is implemented by responders whose registration can be lost
// without a daemon restart. watch must install the subscription *before*
// reading the initial state so a transition in that window is delivered.
type stateWatcher interface {
	watch(ctx context.Context) (states <-chan int32, initial int32, stop func(), err error)
}

type advertiser struct {
	logs    *logbus.Bus
	backend backend

	mu         sync.Mutex
	applyMu    sync.Mutex
	status     Status
	requested  string
	generation uint64

	started     bool
	closed      bool
	runCancel   context.CancelFunc
	monitorDone chan struct{}
	stopWatch   func()
}

// Open returns the platform mDNS manager. It never opens a network socket:
// Avahi on Linux answers the system D-Bus call. Simulator and unknown
// platforms get an explicit unsupported manager so a developer run cannot
// rename the host machine's mDNS name while the setting still renders and
// persists.
func Open(logs *logbus.Bus, platform string) Manager {
	m, initial := openBackend(platform)
	return newAdvertiser(logs, m, initial)
}

func unsupported(reason string) (backend, Status) {
	return unsupportedBackend{reason: reason}, Status{Supported: false, Detail: reason}
}

type unsupportedBackend struct{ reason string }

func (b unsupportedBackend) setHostname(context.Context, string) (string, error) {
	return "", fmt.Errorf("%w: %s", ErrUnsupported, b.reason)
}

func newAdvertiser(logs *logbus.Bus, b backend, initial Status) *advertiser {
	return &advertiser{logs: logs, backend: b, status: initial}
}

// NormalizeHostname trims surrounding space and folds a DNS label to lower
// case. mDNS host names are case-insensitive; store and advertise one form.
func NormalizeHostname(hostname string) string {
	return strings.ToLower(strings.TrimSpace(hostname))
}

// ValidateHostname accepts exactly one DNS label. The ".local" suffix is
// added by the responder, so a dotted name is rejected rather than advertised
// as "name.local.local".
func ValidateHostname(hostname string) error {
	if hostname == "" {
		return errors.New("hostname is required")
	}
	if len(hostname) > MaxLabelLength {
		return fmt.Errorf("hostname must be at most %d characters", MaxLabelLength)
	}
	if strings.Contains(hostname, ".") {
		return errors.New("hostname must be a single label; the .local suffix is added automatically")
	}
	for i := range len(hostname) {
		c := hostname[i]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit := c >= '0' && c <= '9'
		if !letter && !digit && c != '-' {
			return errors.New("hostname may contain only letters, digits and hyphens")
		}
	}
	if hostname[0] == '-' || hostname[len(hostname)-1] == '-' {
		return errors.New("hostname must not start or end with a hyphen")
	}
	return nil
}

// Start subscribes to responder state changes before reading its snapshot and
// reconciles the requested label on every transition. A backend without a
// state subscription (simulator/unsupported) only supports explicit Apply and
// returns nil here.
func (a *advertiser) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.closed || a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	watcher, ok := a.backend.(stateWatcher)
	if !ok {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	// The watch call installs the bus match before taking its state snapshot.
	runCtx, cancel := context.WithCancel(ctx)
	states, initial, stop, err := watcher.watch(runCtx)
	if err != nil {
		cancel()
		a.mu.Lock()
		a.started = false
		a.mu.Unlock()
		return fmt.Errorf("watch mDNS responder: %w", err)
	}
	done := make(chan struct{})

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		cancel()
		stop()
		return nil
	}
	a.runCancel = cancel
	a.monitorDone = done
	a.stopWatch = stop
	// Decide the initial pass from the request present when Start was called,
	// not when the monitor goroutine happens to run. A later Apply performs its
	// own reconciliation.
	reconcileInitial := a.requested != ""
	a.mu.Unlock()

	go a.monitor(runCtx, states, initial, reconcileInitial, done)
	return nil
}

// monitor turns responder state notifications into a coalesced reconciliation.
// The monitor itself never performs a responder call, so a slow or waiting
// apply cannot block StateChanged delivery. One worker applies the latest
// requested label; transitions received while it runs leave a single pending
// wake, so a burst converges on the newest state instead of queueing one apply
// per event. It never polls.
func (a *advertiser) monitor(ctx context.Context, states <-chan int32, initial int32, reconcileInitial bool, done chan struct{}) {
	defer close(done)
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	wake := make(chan struct{}, 1)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for {
			select {
			case <-workCtx.Done():
				return
			case <-wake:
				a.reconcile(workCtx, wake)
			}
		}
	}()
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	// A request present at Start gets one authoritative pass; otherwise the
	// first transition (or a later Apply) triggers reconciliation.
	if reconcileInitial {
		signal()
	}
	last := initial
	for {
		select {
		case <-ctx.Done():
			workCancel()
			<-workerDone
			return
		case state, ok := <-states:
			if !ok {
				// The subscription ended without a Close. Cancel the worker and
				// join it so no responder call or status update outlives the
				// monitor.
				workCancel()
				<-workerDone
				return
			}
			// Every transition can change whether the name is advertised, so
			// reconcile on leaving RUNNING as well as entering it. Duplicate
			// snapshots collapse into the same wake.
			if state != last {
				signal()
			}
			last = state
		}
	}
}

// reconcile applies the latest requested label. A transition delivered while
// the responder call is in flight leaves one signal in wake, so this makes one
// more pass and then waits for the next transition; it never spins or polls.
func (a *advertiser) reconcile(ctx context.Context, wake <-chan struct{}) {
	for {
		if ctx.Err() != nil {
			return
		}
		a.reapply(ctx)
		select {
		case <-wake:
		case <-ctx.Done():
			return
		default:
			return
		}
	}
}

// reapply publishes the latest requested label. It takes the same apply lock
// as Apply, so a later user selection cannot be overwritten by an older
// responder-restart callback. A closed manager makes no responder call.
func (a *advertiser) reapply(ctx context.Context) {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	label, gen := a.requested, a.generation
	a.mu.Unlock()
	if label == "" {
		return
	}
	_ = a.publishLocked(ctx, label, gen)
}

// Close stops the monitor, removes the bus subscription and is idempotent.
func (a *advertiser) Close() error {
	a.mu.Lock()
	a.closed = true
	cancel, done, stop := a.runCancel, a.monitorDone, a.stopWatch
	a.runCancel, a.monitorDone, a.stopWatch = nil, nil, nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if stop != nil {
		stop()
	}
	return nil
}

// Apply validates and reconciles the label with the live responder. A local
// validation rejection does not touch the responder, so the previous
// advertisement remains; a responder failure clears Advertised because the old
// registration may be gone. Every call consults the responder, so a save also
// repairs an advertisement lost while this process was not monitoring it. The
// backend treats a label the responder already owns as a confirmed success
// without a redundant write.
func (a *advertiser) Apply(ctx context.Context, hostname string) error {
	// Serialize responder writes so two callers (responder monitor and a Web UI
	// save) cannot complete out of order and leave the older label live.
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	a.mu.Lock()
	if a.closed {
		// Close owns the final state; a late save must not resurrect a warning
		// or advertisement after the monitor and bus match are gone.
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	label := NormalizeHostname(hostname)
	if err := ValidateHostname(label); err != nil {
		a.mu.Lock()
		if !a.closed {
			// No responder call was made, so the previous state is still observed.
			a.status.Detail = err.Error()
		}
		a.mu.Unlock()
		return fmt.Errorf("hostname %q: %w", hostname, err)
	}

	a.mu.Lock()
	a.requested = label
	a.generation++
	gen := a.generation
	a.mu.Unlock()

	return a.publishLocked(ctx, label, gen)
}

// publishLocked reconciles one label with the responder. The caller holds
// applyMu.
func (a *advertiser) publishLocked(ctx context.Context, label string, gen uint64) error {
	fqdn, err := a.backend.setHostname(ctx, label)
	a.mu.Lock()
	if a.closed {
		// Close has begun. Its cancellation must not leave a "context canceled"
		// warning or a stale advertisement claim in Status.
		a.mu.Unlock()
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A canceled caller did not observe the result; keep the last confirmed
		// state and let the next apply reconcile instead of publishing a
		// cancellation as an advertisement failure.
		a.mu.Unlock()
		return ctxErr
	}
	if a.generation != gen || a.requested != label {
		// A newer selection won while this write was in flight; its own
		// publish owns the status.
		a.mu.Unlock()
		return nil
	}
	if err != nil {
		a.status = Status{
			Supported: !errors.Is(err, ErrUnsupported),
			Hostname:  label,
			Detail:    err.Error(),
		}
		a.mu.Unlock()
		return err
	}
	a.status = Status{Supported: true, Hostname: label, Advertised: fqdn}
	a.mu.Unlock()
	if a.logs != nil {
		a.logs.Infof("mdns", "", "advertising hostname %s", fqdn)
	}
	return nil
}

// Status returns a copy of the current advertisement state.
func (a *advertiser) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}
