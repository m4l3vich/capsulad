package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// Pairing agent.
//
// The device is headless, so SSP "Just Works" (NoInputNoOutput) is the only
// supported pairing method. BlueZ requires a registered agent for a remote
// device to bond at all; without one a first-time source sees a pairing timeout
// or a rejected connection. Discoverability is the user's pairing window: a new
// bond is accepted only while the adapter is discoverable (which also implies
// incoming connections are enabled), and a device that is already bonded may
// authorize its services at any time.

const (
	bluezAgentManager = "org.bluez.AgentManager1"
	pairingAgentPath  = dbus.ObjectPath("/org/capsulad/agent1")
	pairingAgentCaps  = "NoInputNoOutput"
)

type pairingAgent struct {
	b *BlueZ
}

func (a *pairingAgent) Release() *dbus.Error { return nil }

func (a *pairingAgent) Cancel() *dbus.Error { return nil }

func (a *pairingAgent) DisplayPasskey(dbus.ObjectPath, uint32, uint16) *dbus.Error { return nil }

// RequestPinCode and RequestPasskey are legacy PIN flows; a headless device has
// no way to display or enter a code, so they cancel pairing instead of
// pretending to succeed.
func (a *pairingAgent) RequestPinCode(dbus.ObjectPath) (string, *dbus.Error) {
	return "", errPairingRejected()
}

func (a *pairingAgent) RequestPasskey(dbus.ObjectPath) (uint32, *dbus.Error) {
	return 0, errPairingRejected()
}

func (a *pairingAgent) RequestConfirmation(dbus.ObjectPath, uint32) *dbus.Error {
	return a.allowPairing()
}

func (a *pairingAgent) RequestAuthorization(dbus.ObjectPath) *dbus.Error {
	return a.allowPairing()
}

// AuthorizeService covers services of a remote device that is already paired;
// the bond exists, so it does not depend on the pairing switch.
func (a *pairingAgent) AuthorizeService(dbus.ObjectPath, string) *dbus.Error { return nil }

func (a *pairingAgent) allowPairing() *dbus.Error {
	a.b.mu.Lock()
	allowed := a.b.status.Present && a.b.status.IncomingConnections && a.b.status.Discoverable
	a.b.mu.Unlock()
	if !allowed {
		return errPairingRejected()
	}
	return nil
}

func errPairingRejected() *dbus.Error {
	return &dbus.Error{
		Name: "org.bluez.Error.Rejected",
		Body: []any{"pairing is disabled; make the speaker discoverable first"},
	}
}

// registerAgent exports the agent object once and registers it with BlueZ. It
// is safe to call again after a BlueZ restart; an AlreadyExists reply means the
// service already has the registration.
func (b *BlueZ) registerAgent(ctx context.Context) error {
	if b.agentExported.CompareAndSwap(false, true) {
		if err := b.conn.Export(&pairingAgent{b: b}, pairingAgentPath, "org.bluez.Agent1"); err != nil {
			b.agentExported.Store(false)
			return fmt.Errorf("export pairing agent: %w", err)
		}
	}
	manager := b.conn.Object(bluezService, dbus.ObjectPath("/org/bluez"))
	err := manager.CallWithContext(ctx, bluezAgentManager+".RegisterAgent", 0, pairingAgentPath, pairingAgentCaps).Err
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("register pairing agent: %w", err)
	}
	// Another agent may already be the default; that is not a failure. Requests
	// still reach the registered agent when it is selected.
	_ = manager.CallWithContext(ctx, bluezAgentManager+".RequestDefaultAgent", 0, pairingAgentPath).Err
	b.agentRegistered.Store(true)
	return nil
}

func (b *BlueZ) unregisterAgent() {
	if !b.agentRegistered.CompareAndSwap(true, false) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.conn.Object(bluezService, dbus.ObjectPath("/org/bluez")).CallWithContext(ctx,
		bluezAgentManager+".UnregisterAgent", 0, pairingAgentPath).Err
	_ = b.conn.ExportMethodTable(nil, pairingAgentPath, "org.bluez.Agent1")
	b.agentExported.Store(false)
}

func isAlreadyExists(err error) bool {
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		return strings.Contains(dbusErr.Name, "AlreadyExists")
	}
	return false
}
