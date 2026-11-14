//go:build linux

package discovery

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	avahiService = "org.freedesktop.Avahi"
	avahiServer  = avahiService + ".Server"
	avahiPath    = dbus.ObjectPath("/")
)

type avahiBackend struct{}

func openBackend(platform string) (backend, Status) {
	if platform == "" || platform == "simulator" {
		return unsupported("mDNS advertising is disabled in the simulator")
	}
	return avahiBackend{}, Status{Supported: true, Detail: "mDNS host name has not been applied yet"}
}

// dbusAvahiServer adapts the public org.freedesktop.Avahi.Server methods on
// the system bus to the apply ordering in avahi_apply.go.
type dbusAvahiServer struct{ obj dbus.BusObject }

func (s dbusAvahiServer) getState(ctx context.Context) (int32, error) {
	return avahiState(ctx, s.obj)
}

func (s dbusAvahiServer) getHostName(ctx context.Context) (string, error) {
	return avahiStringCall(ctx, s.obj, avahiServer+".GetHostName")
}

func (s dbusAvahiServer) getHostNameFqdn(ctx context.Context) (string, error) {
	return avahiStringCall(ctx, s.obj, avahiServer+".GetHostNameFqdn")
}

func (s dbusAvahiServer) setHostName(ctx context.Context, label string) error {
	call := s.obj.CallWithContext(ctx, avahiServer+".SetHostName", 0, label)
	if call.Err == nil {
		return nil
	}
	return avahiDMSError{name: dbusErrorName(call.Err), err: call.Err}
}

func avahiStringCall(ctx context.Context, obj dbus.BusObject, method string) (string, error) {
	call := obj.CallWithContext(ctx, method, 0)
	if call.Err != nil {
		return "", call.Err
	}
	var value string
	if err := call.Store(&value); err != nil {
		return "", err
	}
	return value, nil
}

func dbusErrorName(err error) string {
	switch e := err.(type) {
	case dbus.Error:
		return e.Name
	case *dbus.Error:
		return e.Name
	}
	return ""
}

// watch subscribes to Avahi state changes before taking the initial snapshot,
// so a restart that lands in the subscribe/snapshot window is still replayed.
// It never polls: transitions arrive as StateChanged signals.
func (avahiBackend) watch(ctx context.Context) (<-chan int32, int32, func(), error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("system D-Bus is unavailable: %w", err)
	}
	obj := conn.Object(avahiService, avahiPath)
	options := []dbus.MatchOption{
		dbus.WithMatchSender(avahiService),
		dbus.WithMatchObjectPath(avahiPath),
		dbus.WithMatchInterface(avahiServer),
		dbus.WithMatchMember("StateChanged"),
	}

	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	subCtx, subCancel := context.WithTimeout(ctx, 2*time.Second)
	err = conn.AddMatchSignalContext(subCtx, options...)
	subCancel()
	if err != nil {
		conn.RemoveSignal(signals)
		return nil, 0, nil, err
	}

	// Snapshot strictly after AddMatch. A transition between the two is
	// delivered to signals instead of being lost. An unavailable snapshot is
	// not fatal: the StateChanged stream can still recover it later.
	snapCtx, snapCancel := context.WithTimeout(ctx, 2*time.Second)
	initial, _ := avahiState(snapCtx, obj)
	snapCancel()

	relayCtx, relayCancel := context.WithCancel(ctx)
	out := make(chan int32, 8)
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		defer close(out)
		for {
			select {
			case <-relayCtx.Done():
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				state, ok := avahiStateFromSignal(signal)
				if !ok {
					continue
				}
				select {
				case out <- state:
				case <-relayCtx.Done():
					return
				}
			}
		}
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			relayCancel()
			<-relayDone
			removeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = conn.RemoveMatchSignalContext(removeCtx, options...)
			cancel()
			conn.RemoveSignal(signals)
		})
	}
	return out, initial, stop, nil
}

func avahiStateFromSignal(signal *dbus.Signal) (int32, bool) {
	if signal == nil || signal.Name != avahiServer+".StateChanged" || len(signal.Body) != 1 {
		return 0, false
	}
	state, ok := signal.Body[0].(int32)
	return state, ok
}

func avahiState(ctx context.Context, obj dbus.BusObject) (int32, error) {
	call := obj.CallWithContext(ctx, avahiServer+".GetState", 0)
	if call.Err != nil {
		return 0, call.Err
	}
	var state int32
	if err := call.Store(&state); err != nil {
		return 0, err
	}
	return state, nil
}

// setHostname changes Avahi's runtime host name. Avahi owns multicast DNS on
// the stock image and already answers A/AAAA and reverse queries for it, so
// publishing a second responder on 5353 would be a competing owner. The
// setting is runtime-only in Avahi; the daemon re-applies the durable settings
// value after every restart, and the manager's state monitor re-applies it
// when the responder itself restarts. applyHostName first reads the public
// GetHostName state, so a re-apply of the already active label is a confirmed
// success with no redundant SetHostName call.
func (avahiBackend) setHostname(ctx context.Context, label string) (string, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return "", fmt.Errorf("%w: system D-Bus is unavailable: %v", ErrUnsupported, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	obj := conn.Object(avahiService, avahiPath)
	return applyHostName(ctx, dbusAvahiServer{obj: obj}, label)
}
