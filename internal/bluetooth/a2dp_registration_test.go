package bluetooth

import (
	"context"
	"errors"
	"testing"

	"github.com/godbus/dbus/v5"
)

// Registration readiness is generation-scoped. BlueZ drops endpoint
// registrations when the service restarts or the adapter is power-cycled while
// the exported object survives, so an armed flag must never outlive the
// adapter state it was created under.
func TestSinkReadinessIsGenerationScoped(t *testing.T) {
	b := &BlueZ{}
	b.endpoint = &sbcEndpoint{b: b}
	b.endpointPath = a2dpEndpointID

	gen := b.endpointGeneration()
	if !b.armA2DP(gen) {
		t.Fatal("arming the current generation failed")
	}
	if !b.SinkRegistered() {
		t.Fatal("armed endpoint is not reported ready")
	}

	b.markA2DPUnarmed()
	if b.SinkRegistered() {
		t.Fatal("a BlueZ restart/power cycle left the sink reported ready")
	}
	if b.armA2DP(gen) {
		t.Fatal("a registration reply from the previous generation re-armed the sink")
	}
	if !b.armA2DP(b.endpointGeneration()) {
		t.Fatal("a fresh registration in the current generation did not arm the sink")
	}
	if !b.SinkRegistered() {
		t.Fatal("freshly armed endpoint is not reported ready")
	}
}

// A RegisterEndpoint failure must leave the sink unready, and a successful
// re-arm must make it ready again: the daemon falls back to PulseAudio only on
// the former.
func TestRearmFailureKeepsSinkUnready(t *testing.T) {
	b := &BlueZ{connected: map[string]string{}, subs: map[int]chan Event{}}
	b.adapter = dbus.ObjectPath("/org/bluez/hci0")
	b.status = Status{Present: true, Powered: true}
	b.endpoint = &sbcEndpoint{b: b}
	b.endpointPath = a2dpEndpointID

	b.registerEndpoint = func(context.Context, dbus.ObjectPath, dbus.ObjectPath) error {
		return errors.New("org.bluez.Error.NotReady")
	}
	if err := b.rearmA2DP(context.Background()); err == nil {
		t.Fatal("a failed re-registration reported success")
	}
	if b.SinkRegistered() {
		t.Fatal("a failed re-registration looked ready")
	}

	b.registerEndpoint = func(context.Context, dbus.ObjectPath, dbus.ObjectPath) error {
		return nil
	}
	if err := b.rearmA2DP(context.Background()); err != nil {
		t.Fatalf("re-registration: %v", err)
	}
	if !b.SinkRegistered() {
		t.Fatal("successful re-registration did not report ready")
	}
}

// A RegisterEndpoint reply that races a BlueZ restart or adapter power cycle
// must not arm the endpoint: the invalidation happened while the call was in
// flight, so BlueZ may already have discarded it.
func TestRegistrationReplyRacingRestartDoesNotArm(t *testing.T) {
	b := &BlueZ{connected: map[string]string{}, subs: map[int]chan Event{}}
	b.adapter = dbus.ObjectPath("/org/bluez/hci0")
	b.status = Status{Present: true, Powered: true}
	b.endpoint = &sbcEndpoint{b: b}
	b.endpointPath = a2dpEndpointID
	b.registerEndpoint = func(context.Context, dbus.ObjectPath, dbus.ObjectPath) error {
		b.markA2DPUnarmed()
		return nil
	}
	if err := b.registerA2DPEndpoint(context.Background()); err == nil {
		t.Fatal("a registration that raced an invalidation reported success")
	}
	if b.SinkRegistered() {
		t.Fatal("a registration that raced an invalidation looked ready")
	}
}

// An adapter that is off cannot hold an endpoint; re-arm is a no-op and the
// sink stays unready instead of calling BlueZ with a guarantee it cannot keep.
func TestRearmSkipsUnpoweredAdapter(t *testing.T) {
	b := &BlueZ{connected: map[string]string{}, subs: map[int]chan Event{}}
	b.adapter = dbus.ObjectPath("/org/bluez/hci0")
	b.status = Status{Present: true, Powered: false}
	b.endpoint = &sbcEndpoint{b: b}
	b.endpointPath = a2dpEndpointID
	called := false
	b.registerEndpoint = func(context.Context, dbus.ObjectPath, dbus.ObjectPath) error {
		called = true
		return nil
	}
	if err := b.rearmA2DP(context.Background()); err != nil {
		t.Fatalf("rearm while unpowered: %v", err)
	}
	if called {
		t.Fatal("rearm registered against an unpowered adapter")
	}
	if b.SinkRegistered() {
		t.Fatal("an unpowered adapter reported a ready sink")
	}
}

// An adapter power transition or identity change invalidates the registration;
// an unchanged refresh does not.
func TestEndpointInvalidationRule(t *testing.T) {
	adapter := dbus.ObjectPath("/org/bluez/hci0")
	on := Status{Present: true, Powered: true}
	off := Status{Present: true, Powered: false}
	if endpointRegistrationInvalidated(adapter, adapter, on, on) {
		t.Fatal("an unchanged adapter invalidated the registration")
	}
	if !endpointRegistrationInvalidated(adapter, adapter, on, off) {
		t.Fatal("power loss did not invalidate the registration")
	}
	if !endpointRegistrationInvalidated(adapter, adapter, off, on) {
		t.Fatal("power gain did not invalidate the stale registration")
	}
	if !endpointRegistrationInvalidated(adapter, "/org/bluez/hci1", on, on) {
		t.Fatal("an adapter change did not invalidate the registration")
	}
	if !endpointRegistrationInvalidated(adapter, "", on, Status{}) {
		t.Fatal("a vanished adapter did not invalidate the registration")
	}
}
