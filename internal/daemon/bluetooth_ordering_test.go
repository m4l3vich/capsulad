package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/bluetooth"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/settings"
)

// Bluetooth A2DP startup ordering contract.
//
// Trigger: daemon startup, adapter power/incoming/discoverability changes, a
// BlueZ restart, and a source's first incoming connection.
// Feedback: the sink endpoint is registered before the adapter is made
// connectable or discoverable, and before a source is asked for its audio
// profile; a failed registration is logged and never reported ready.
// Ownership: the framework owns the native SBC sink. PulseAudio remains the
// fallback and keeps a source it is already routing.
// Interruption: BlueZ restarts and adapter power cycles advance the endpoint
// generation, so a stale registration reply cannot claim readiness.
// Dismissal: not applicable; there is no user-facing prompt.
// Final state: SinkRegistered is true only while the endpoint is armed for the
// current adapter generation; otherwise pairing stays off and the audio owner
// falls back to PulseAudio or reports an error honestly.

type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	l.mu.Unlock()
}

func (l *callLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *callLog) index(call string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, got := range l.calls {
		if got == call {
			return i
		}
	}
	return -1
}

func (l *callLog) count(call string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, got := range l.calls {
		if got == call {
			n++
		}
	}
	return n
}

// fakeSinkBluetooth is a Manager plus the native A2DP lifecycle, with the
// registration outcome scripted so ordering can be asserted without a bus.
type fakeSinkBluetooth struct {
	*bluetooth.Memory
	log *callLog

	mu          sync.Mutex
	registered  bool
	registerErr error
	connectErr  error
}

func newFakeSinkBluetooth() *fakeSinkBluetooth {
	return &fakeSinkBluetooth{Memory: bluetooth.NewMemory(), log: &callLog{}}
}

func (f *fakeSinkBluetooth) SinkRegistered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registered
}

func (f *fakeSinkBluetooth) setRegistered(v bool) {
	f.mu.Lock()
	f.registered = v
	f.mu.Unlock()
}

func (f *fakeSinkBluetooth) RegisterA2DPSink(context.Context) error {
	f.log.add("register-sink")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerErr != nil {
		return f.registerErr
	}
	f.registered = true
	return nil
}

func (f *fakeSinkBluetooth) AcquireA2DP(context.Context) (*bluetooth.A2DPTransport, error) {
	return nil, bluetooth.ErrNoTransport
}

func (f *fakeSinkBluetooth) ReleaseA2DP(context.Context) error { return nil }

func (f *fakeSinkBluetooth) SetTrusted(context.Context, string, bool) error {
	f.log.add("trust")
	return nil
}

func (f *fakeSinkBluetooth) ConnectAudio(context.Context, string) error {
	f.log.add("connect-audio")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectErr
}

func (f *fakeSinkBluetooth) EnsurePowered(ctx context.Context) error {
	f.log.add("power")
	return f.Memory.EnsurePowered(ctx)
}

func (f *fakeSinkBluetooth) SetDiscoverable(ctx context.Context, on bool) error {
	f.log.add(fmt.Sprintf("discoverable:%v", on))
	return f.Memory.SetDiscoverable(ctx, on)
}

func (f *fakeSinkBluetooth) SetIncomingConnections(ctx context.Context, on bool) error {
	f.log.add(fmt.Sprintf("incoming:%v", on))
	return f.Memory.SetIncomingConnections(ctx, on)
}

type fixedPlatformBackend struct {
	backend.Backend
	platform backend.Platform
}

func (b fixedPlatformBackend) Platform() backend.Platform { return b.platform }

func newBluetoothOrderingDaemon(t *testing.T, bt bluetooth.Manager) *Daemon {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := settings.OpenUser("", logs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Daemon{
		ctx:   context.Background(),
		clock: clock,
		logs:  logs,
		user:  user,
		bt:    bt,
		dev: fixedPlatformBackend{
			Backend:  backend.NewSimulator(backend.SimulatorOptions{}),
			platform: backend.PlatformRockchipStock,
		},
	}
}

// stubPulse takes the host pactl out of the owner-selection tests and records
// the calls with a "pulse:" prefix.
func stubPulse(t *testing.T, log *callLog, fn func(args ...string) ([]byte, error)) {
	t.Helper()
	old := pulseCommand
	pulseCommand = func(args ...string) ([]byte, error) {
		if log != nil {
			log.add("pulse:" + strings.Join(args, " "))
		}
		return fn(args...)
	}
	t.Cleanup(func() { pulseCommand = old })
}

func waitForCall(t *testing.T, log *callLog, call string) {
	t.Helper()
	waitFor(t, 3*time.Second, call, func() bool { return log.index(call) >= 0 })
}

func pulseModules() ([]byte, error) {
	return []byte("1\tmodule-bluez5-discover\n"), nil
}

// TestBluetoothAdvertisementOrderBeforeIncoming pins the startup order: the
// sink is registered before the adapter is powered or made connectable, because
// powering first can accept a source before its audio profile exists.
func TestBluetoothAdvertisementOrderBeforeIncoming(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) { return nil, nil })
	// Model a later pairing change: the startup registration pass already ran.
	d.btMu.Lock()
	d.btInit = true
	d.btMu.Unlock()

	v := settings.DefaultUserSettings()
	v.BluetoothIncomingConnections = true
	d.applyBluetoothSettings(v)
	waitForCall(t, bt.log, "incoming:true")

	log := bt.log.list()
	register := bt.log.index("register-sink")
	power := bt.log.index("power")
	incoming := bt.log.index("incoming:true")
	if register < 0 || power < 0 || incoming < 0 {
		t.Fatalf("calls = %v", log)
	}
	if !(register < power && power < incoming) {
		t.Fatalf("order = %v; want register before power before incoming", log)
	}
	if !bt.SinkRegistered() {
		t.Fatal("pairing was enabled without an armed sink")
	}
}

// TestBluetoothAdvertisementFailureDoesNotEnableIncoming covers the honest
// failure path: no sink profile means no power/page-scan and incoming
// connections stay off.
func TestBluetoothAdvertisementFailureDoesNotEnableIncoming(t *testing.T) {
	bt := newFakeSinkBluetooth()
	bt.registerErr = errors.New("org.bluez.Error.NotReady")
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) {
		return nil, errors.New("pulseaudio is not running")
	})
	// Model a later pairing change: the startup registration pass already ran.
	d.btMu.Lock()
	d.btInit = true
	d.btMu.Unlock()

	v := settings.DefaultUserSettings()
	v.BluetoothIncomingConnections = true
	d.applyBluetoothSettings(v)
	waitForCall(t, bt.log, "discoverable:false")

	status, err := bt.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Powered {
		t.Fatal("adapter was left off after a failed advertisement; the radio must stay available for BLE")
	}
	if status.IncomingConnections || status.Discoverable {
		t.Fatalf("failed advertisement accepted an inbound path: %+v", status)
	}
	d.btMu.Lock()
	incoming := d.btIncoming
	d.btMu.Unlock()
	if incoming {
		t.Fatal("incoming state claims success after a failed advertisement")
	}
	if d.bluetoothNative() {
		t.Fatal("native owner claimed after a failed endpoint registration")
	}
	if d.bluetoothAudioReady() {
		t.Fatal("readiness claimed after a failed advertisement and no PulseAudio fallback")
	}
}

// TestBluetoothIncomingOffKeepsRadioPowered covers restoration and the
// dependent toggles: incoming off at startup must not power the radio off, must
// not advertise the sink, and must clear discoverability; enabling incoming
// again restores paired reconnects, and discoverability then opens the pairing
// window.
func TestBluetoothIncomingOffKeepsRadioPowered(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) { return nil, nil })

	v := settings.DefaultUserSettings()
	v.BluetoothIncomingConnections = false
	v.BluetoothDiscoverable = false
	d.applyBluetoothSettings(v)
	waitForCall(t, bt.log, "discoverable:false")

	status, err := bt.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Powered || status.IncomingConnections || status.Discoverable {
		t.Fatalf("incoming off status = %+v; want powered but not connectable or discoverable", status)
	}
	if bt.log.index("register-sink") >= 0 {
		t.Fatalf("incoming off advertised an audio profile: %v", bt.log.list())
	}
	if got := bt.log.index("power"); got < 0 {
		t.Fatalf("incoming off did not power the radio: %v", bt.log.list())
	}

	d.applyBluetoothSettings(settings.DefaultUserSettings())
	waitForCall(t, bt.log, "incoming:true")
	status, err = bt.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Powered || !status.IncomingConnections || status.Discoverable {
		t.Fatalf("incoming on status = %+v; want powered, connectable and hidden", status)
	}

	v = settings.DefaultUserSettings()
	v.BluetoothDiscoverable = true
	d.applyBluetoothSettings(v)
	waitForCall(t, bt.log, "discoverable:true")
	status, err = bt.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Powered || !status.IncomingConnections || !status.Discoverable {
		t.Fatalf("discoverable status = %+v; want powered, connectable and discoverable", status)
	}
}

// TestBluetoothReadinessRequiresArmedSink is the core regression for a first
// connection without an audio profile: a native claim whose registration BlueZ
// dropped must not report ready.
func TestBluetoothReadinessRequiresArmedSink(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) { return nil, nil })
	d.setBluetoothNative(true)

	if d.bluetoothAudioReady() {
		t.Fatal("an unarmed native sink reported ready")
	}
	bt.setRegistered(true)
	if !d.bluetoothAudioReady() {
		t.Fatal("an armed native sink did not report ready")
	}
}

// TestPrepareBluetoothAudioRegistersBeforeRemovingPulseAudio pins the switch
// order: the adapter keeps advertising an Audio Sink while PulseAudio's
// competing endpoints are removed.
func TestPrepareBluetoothAudioRegistersBeforeRemovingPulseAudio(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			return pulseModules()
		}
		return nil, nil
	})

	if err := d.prepareBluetoothAudio(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !bt.SinkRegistered() || !d.bluetoothNative() {
		t.Fatal("native owner was not claimed after a successful registration")
	}
	register := bt.log.index("register-sink")
	unload := bt.log.index("pulse:unload-module 1")
	if register < 0 || unload < 0 {
		t.Fatalf("calls = %v", bt.log.list())
	}
	if register > unload {
		t.Fatalf("PulseAudio was removed before the sink was registered: %v", bt.log.list())
	}
}

// TestPrepareBluetoothAudioKeepsConnectedPulseAudioSession covers the daemon
// restart with an already-connected source: the working PulseAudio route is
// not torn down, and the native switch is completed after the source leaves.
func TestPrepareBluetoothAudioKeepsConnectedPulseAudioSession(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			return pulseModules()
		}
		return nil, nil
	})
	const address = "AA:BB:CC:DD:EE:01"
	bt.SimulateConnect(bluetooth.Device{Address: address, Name: "Mac"}, bluetooth.MediaState{})

	if err := d.prepareBluetoothAudio(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !bt.SinkRegistered() {
		t.Fatal("the native sink was not advertised for future connections")
	}
	if d.bluetoothNative() {
		t.Fatal("the native owner replaced a source PulseAudio was already routing")
	}
	if got := bt.log.index("pulse:unload-module 1"); got >= 0 {
		t.Fatalf("PulseAudio was unloaded while a source was connected: %v", bt.log.list())
	}
	if !d.bluetoothAudioReady() {
		t.Fatal("the advertised sink is not reported ready")
	}

	bt.SimulateDisconnect(address)
	if err := d.prepareBluetoothAudio(); err != nil {
		t.Fatalf("prepare after disconnect: %v", err)
	}
	if !d.bluetoothNative() {
		t.Fatal("the native owner did not take over after the source left")
	}
	register := bt.log.index("register-sink")
	unload := bt.log.index("pulse:unload-module 1")
	if register < 0 || unload < 0 || register > unload {
		t.Fatalf("native switch order = %v; want register before PulseAudio unload", bt.log.list())
	}
}

// TestConnectBluetoothAudioRegistersBeforeProfileRequest covers the incoming
// connect path: a missing registration is repaired before the source is asked
// for its A2DP profile, and a healthy connect never re-registers.
func TestConnectBluetoothAudioRegistersBeforeProfileRequest(t *testing.T) {
	const address = "AA:BB:CC:DD:EE:02"
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) { return nil, nil })
	d.setBluetoothNative(true)

	d.connectBluetoothAudio(address)
	waitForCall(t, bt.log, "connect-audio")
	register := bt.log.index("register-sink")
	connect := bt.log.index("connect-audio")
	if register < 0 || connect < 0 || register > connect {
		t.Fatalf("calls = %v; want one repair before the profile request", bt.log.list())
	}
	if got := bt.log.count("register-sink"); got != 1 {
		t.Fatalf("register calls = %d, want 1", got)
	}

	// A healthy registration is not repaired again on the next connect.
	d.connectBluetoothAudio(address)
	waitFor(t, 3*time.Second, "second profile request", func() bool {
		return bt.log.count("connect-audio") == 2
	})
	if got := bt.log.count("register-sink"); got != 1 {
		t.Fatalf("healthy connect re-registered the sink: calls = %v", bt.log.list())
	}
}

// TestBluetoothDiscoverableRequiresAudioProfile covers the API path directly:
// discoverability is the window in which a source makes its first incoming
// connection, so enabling it without an advertised Audio Sink is refused.
// Disabling it never requires the advertisement.
func TestBluetoothDiscoverableRequiresAudioProfile(t *testing.T) {
	bt := newFakeSinkBluetooth()
	bt.registerErr = errors.New("org.bluez.Error.NotReady")
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) {
		return nil, errors.New("pulseaudio is not running")
	})
	svc := &systemService{d: d}
	ctx := context.Background()

	if _, err := svc.SetBluetoothDiscoverable(ctx,
		&frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("discoverable=true without an audio profile: code=%v err=%v", status.Code(err), err)
	}
	if bt.log.index("discoverable:true") >= 0 {
		t.Fatalf("adapter was made discoverable without an audio profile: %v", bt.log.list())
	}

	if _, err := svc.SetBluetoothDiscoverable(ctx,
		&frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: false}); err != nil {
		t.Fatalf("discoverable=false: %v", err)
	}
	if bt.log.index("discoverable:false") < 0 {
		t.Fatalf("discoverable=false was not applied: %v", bt.log.list())
	}

	// A registered sink makes the same request succeed.
	bt.setRegistered(true)
	reply, err := svc.SetBluetoothDiscoverable(ctx,
		&frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: true})
	if err != nil {
		t.Fatalf("discoverable=true with an armed sink: %v", err)
	}
	if !reply.GetDiscoverable() {
		t.Fatalf("discoverable was not applied: %+v", reply)
	}
	if !reply.GetIncomingConnections() {
		t.Fatalf("discoverability did not imply incoming connections: %+v", reply)
	}
	if v := d.user.Get().Values; !v.BluetoothDiscoverable || !v.BluetoothIncomingConnections {
		t.Fatalf("discoverability and its implied incoming state were not persisted: %+v", v)
	}
	if _, err := svc.SetBluetoothDiscoverable(ctx,
		&frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: false}); err != nil {
		t.Fatalf("discoverable=false after enabling: %v", err)
	}
	status, err := bt.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Discoverable || !status.IncomingConnections {
		t.Fatalf("discoverability off status = %+v; want hidden with paired reconnects still allowed", status)
	}
	if v := d.user.Get().Values; v.BluetoothDiscoverable || !v.BluetoothIncomingConnections {
		t.Fatalf("discoverability off did not preserve incoming: %+v", v)
	}
}

// TestBluetoothStatusReportsAudioProfileNotReady uses the existing Detail
// surface: an adapter that is up but cannot advertise the sink must not look
// ready, because pairing/discoverability cannot deliver audio then.
func TestBluetoothStatusReportsAudioProfileNotReady(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) {
		return nil, errors.New("pulseaudio is not running")
	})
	svc := &systemService{d: d}

	reply, err := svc.GetBluetoothStatus(context.Background(), &frameworkv1.GetBluetoothStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.GetPresent() {
		t.Fatalf("status = %+v", reply)
	}
	if !strings.Contains(reply.GetDetail(), "audio profile is not advertised") {
		t.Fatalf("detail = %q, want the readiness note", reply.GetDetail())
	}

	bt.setRegistered(true)
	reply, err = svc.GetBluetoothStatus(context.Background(), &frameworkv1.GetBluetoothStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply.GetDetail(), "audio profile is not advertised") {
		t.Fatalf("armed sink still reported not ready: %q", reply.GetDetail())
	}
}
