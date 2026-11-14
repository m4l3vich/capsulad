package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// avahiServerRegistering is AVAHI_SERVER_REGISTERING; RUNNING is shared with
// the manager.
const avahiServerRegistering int32 = 1

// avahiRegisteringWait bounds how long an apply waits for D-Bus-activated
// Avahi to finish registering. A variable so ordering tests do not sleep for
// the boot timeout.
var avahiRegisteringWait = 3 * time.Second

// avahiResponder is the subset of Avahi's public org.freedesktop.Avahi.Server
// interface this package uses. The Linux adapter forwards each method to the
// system D-Bus object; tests substitute a fake responder.
type avahiResponder interface {
	getState(ctx context.Context) (int32, error)
	getHostName(ctx context.Context) (string, error)
	getHostNameFqdn(ctx context.Context) (string, error)
	setHostName(ctx context.Context, label string) error
}

// avahiDMSError keeps the D-Bus error name for operator-facing text without
// making the apply ordering depend on the D-Bus library at build time.
type avahiDMSError struct {
	name string
	err  error
}

func (e avahiDMSError) Error() string     { return e.err.Error() }
func (e avahiDMSError) Unwrap() error     { return e.err }
func (e avahiDMSError) avahiName() string { return e.name }

// applyHostName makes label the responder's active host name and returns the
// advertised FQDN. The responder already owning the requested label is a
// success: Avahi answers a redundant SetHostName with an invalid-operation
// error, and a concurrent writer can reach the requested name between our read
// and our write. Both cases are accepted only after an authoritative readback
// proves the responder is RUNNING and owns the requested label; a rejected
// label, a collision or a missing responder still returns the responder's
// error, and no advertisement is claimed while the responder is unready.
func applyHostName(ctx context.Context, srv avahiResponder, label string) (string, error) {
	state, err := srv.getState(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: Avahi is not reachable: %v", ErrUnsupported, err)
	}
	state, err = awaitAvahiRunning(ctx, srv, state)
	if err != nil {
		return "", err
	}
	// REGISTERING after the bounded boot wait, COLLISION and FAILURE all mean
	// the name is not being advertised right now. Report that state instead of
	// claiming the FQDN or issuing a write that cannot be confirmed.
	if state != stateRunning {
		return "", avahiNotReady(label, state)
	}

	// The state monitor replays the persisted label after a responder restart.
	// When the responder still owns that label there is nothing to write; this
	// is the normal startup case for the default name and must not surface
	// Avahi's redundant-write error as an advertisement failure.
	if current, err := srv.getHostName(ctx); err == nil && avahiLabelMatches(current, label) {
		return avahiConfirmed(ctx, srv, label)
	}

	if err := srv.setHostName(ctx, label); err != nil {
		// A different writer may have reached the requested name before our
		// write (another apply, or an operator using avahi-set-host-name).
		// Only an authoritative RUNNING readback may turn the failed write into
		// success; otherwise the setter's error surfaces unchanged.
		if current, readErr := srv.getHostName(ctx); readErr == nil && avahiLabelMatches(current, label) {
			return avahiConfirmed(ctx, srv, label)
		}
		fqdn, _ := srv.getHostNameFqdn(ctx)
		return "", avahiError(label, err, fqdn)
	}
	return avahiConfirmed(ctx, srv, label)
}

// awaitAvahiRunning waits out D-Bus activation during boot. It returns the last
// state it observed; cancelling ctx aborts the wait instead of sleeping through
// Close.
func awaitAvahiRunning(ctx context.Context, srv avahiResponder, state int32) (int32, error) {
	if state != avahiServerRegistering {
		return state, nil
	}
	deadline := time.Now().Add(avahiRegisteringWait)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for state != stateRunning && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-ticker.C:
		}
		next, err := srv.getState(ctx)
		if err != nil {
			return state, nil
		}
		state = next
	}
	return state, nil
}

// avahiConfirmed turns a label believed applied into the advertised FQDN. A
// successful SetHostName may return while Avahi is still probing the changed
// name, so the same bounded, ctx-cancellable wait used before the write is
// applied here before verifying. It then requires an authoritative RUNNING
// responder that still owns the requested label and a responder-supplied FQDN.
// A stuck probe or other unready state yields no claim; a read failure is
// reported as unconfirmed and never replaced with a synthetic FQDN.
func avahiConfirmed(ctx context.Context, srv avahiResponder, label string) (string, error) {
	state, err := srv.getState(ctx)
	if err != nil {
		return "", avahiUnconfirmed(label, err)
	}
	state, err = awaitAvahiRunning(ctx, srv, state)
	if err != nil {
		return "", err
	}
	if state != stateRunning {
		return "", avahiNotReady(label, state)
	}
	current, hostErr := srv.getHostName(ctx)
	if hostErr != nil {
		return "", avahiUnconfirmed(label, hostErr)
	}
	if !avahiLabelMatches(current, label) {
		return "", avahiUnconfirmed(label, fmt.Errorf("responder reports %q instead", current))
	}
	fqdn, fqdnErr := srv.getHostNameFqdn(ctx)
	if fqdnErr != nil {
		return "", avahiUnconfirmed(label, fqdnErr)
	}
	state, stateErr := srv.getState(ctx)
	if stateErr != nil {
		return "", avahiUnconfirmed(label, stateErr)
	}
	if state != stateRunning {
		return "", avahiNotReady(label, state)
	}
	if fqdn == "" {
		return "", avahiUnconfirmed(label, errors.New("responder returned no advertised name"))
	}
	return fqdn, nil
}

// avahiLabelMatches compares the responder's current host name with the
// requested single-label name. Avahi returns the bare label; tolerate a
// trailing root dot and case differences.
func avahiLabelMatches(current, label string) bool {
	return NormalizeHostname(strings.TrimSuffix(current, ".")) == NormalizeHostname(label)
}

func avahiErrorName(err error) string {
	var named interface{ avahiName() string }
	if errors.As(err, &named) {
		return named.avahiName()
	}
	return ""
}

// avahiNotReady reports an unready responder without claiming an
// advertisement. Supported stays true: an Avahi instance exists, it is simply
// not advertising this label yet.
func avahiNotReady(label string, state int32) error {
	return fmt.Errorf("mDNS responder is %s; host name %q is not advertised", avahiStateName(state), label)
}

func avahiStateName(state int32) string {
	switch state {
	case 0:
		return "invalid"
	case avahiServerRegistering:
		return "registering"
	case stateRunning:
		return "running"
	case 3:
		return "in collision"
	case 4:
		return "in failure"
	}
	return fmt.Sprintf("in state %d", state)
}

// avahiUnconfirmed reports that the host name may be applied but the
// advertised name was not verified. It never substitutes a synthetic FQDN and
// keeps the underlying read failure visible.
func avahiUnconfirmed(label string, err error) error {
	return fmt.Errorf("mDNS advertisement for %q is unconfirmed: %v", label, err)
}

// avahiError turns the responder's collision/invalid-name errors into text an
// operator can act on instead of a bare D-Bus error message. current is the
// responder's observed FQDN after the failed call, when it is available.
func avahiError(label string, err error, current string) error {
	observed := ""
	if current != "" && !strings.EqualFold(current, label+".local") {
		observed = fmt.Sprintf(" (responder currently reports %q)", current)
	}
	switch avahiErrorName(err) {
	case "org.freedesktop.Avahi.InvalidHostNameError":
		return fmt.Errorf("hostname %q is not accepted by Avahi%s", label, observed)
	case "org.freedesktop.Avahi.HostNameCollisionError":
		return fmt.Errorf("hostname %q is already in use on the local network%s", label, observed)
	case "org.freedesktop.Avahi.NotRunningError", "org.freedesktop.DBus.Error.ServiceUnknown":
		return fmt.Errorf("%w: Avahi is not running%s", ErrUnsupported, observed)
	}
	return fmt.Errorf("set mDNS hostname %q: %v%s", label, err, observed)
}
