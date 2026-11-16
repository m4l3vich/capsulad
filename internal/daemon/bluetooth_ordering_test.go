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

	// Session scripting for the A2DP link assertion: the manager answers with
	// whatever the test set, and counts the A2DP profile requests it was asked to
	// perform. The daemon's A2DP decision surface has no disconnect to call: a
	// missing profile is requested, never repaired by dropping the source.
	session     bluetooth.AudioLink
	sessionErr  error
	sessionReqs int
	requests    int
	requestErr  error
	// requestHook, when set, replaces the immediate request result so a test can
	// observe the context the request was given.
	requestHook func(ctx context.Context) error
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

// pulseStreamingSources is the pactl answer for a source PulseAudio is carrying
// right now: its bluez5 source is RUNNING.
func pulseStreamingSources(address string) []byte {
	return []byte("4\tbluez_source." + strings.ReplaceAll(strings.ToUpper(address), ":", "_") +
		".a2dp_source\tmodule-bluez5-device.c\ts16le 2ch 44100Hz\tRUNNING\n")
}

// TestPrepareBluetoothAudioKeepsStreamingPulseAudioSession covers the daemon
// restart with an already-connected source PulseAudio is actively carrying: that
// route is not torn down, and the native switch is completed after the source
// leaves.
func TestPrepareBluetoothAudioKeepsStreamingPulseAudioSession(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	const address = "AA:BB:CC:DD:EE:01"
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			if len(args) > 2 && args[2] == "sources" {
				return pulseStreamingSources(address), nil
			}
			return pulseModules()
		}
		return nil, nil
	})
	bt.SimulateConnect(bluetooth.Device{Address: address, Name: "Mac"}, bluetooth.MediaState{})

	if err := d.prepareBluetoothAudio(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !bt.SinkRegistered() {
		t.Fatal("the native sink was not advertised for future connections")
	}
	if d.bluetoothNative() {
		t.Fatal("the native owner replaced a source PulseAudio was already streaming")
	}
	if got := bt.log.index("pulse:unload-module 1"); got >= 0 {
		t.Fatalf("PulseAudio was unloaded while it was streaming the source: %v", bt.log.list())
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

// TestPrepareBluetoothAudioRetiresIdlePulseAudioStack is the narrowband-trap
// fix: a loaded PulseAudio module set with a connected source it is not
// carrying audio for must not stay advertised next to our armed sink, because
// the next connection can land on its Headset endpoint.
func TestPrepareBluetoothAudioRetiresIdlePulseAudioStack(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	stubPulse(t, bt.log, func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			if len(args) > 2 && args[2] == "sources" {
				// The card exists but the source is idle: nothing is streaming.
				return []byte("4\tbluez_source.AA_BB_CC_DD_EE_01.a2dp_source\tmodule-bluez5-device.c\ts16le 2ch 44100Hz\tIDLE\n"), nil
			}
			return pulseModules()
		}
		return nil, nil
	})
	const address = "AA:BB:CC:DD:EE:01"
	bt.SimulateConnect(bluetooth.Device{Address: address, Name: "Phone"}, bluetooth.MediaState{})

	if err := d.prepareBluetoothAudio(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !bt.SinkRegistered() || !d.bluetoothNative() {
		t.Fatalf("native owner = %v, want the armed sink to own audio", d.bluetoothNative())
	}
	if got := bt.log.index("pulse:unload-module 1"); got < 0 {
		t.Fatalf("the idle PulseAudio stack was left advertising a Headset profile: %v", bt.log.list())
	}
	register := bt.log.index("register-sink")
	unload := bt.log.index("pulse:unload-module 1")
	if register < 0 || register > unload {
		t.Fatalf("PulseAudio was retired before the sink was armed: %v", bt.log.list())
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

// --- session assertion and single-repair policy ------------------------------

func (f *fakeSinkBluetooth) Session(context.Context, string) (bluetooth.AudioLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionReqs++
	return f.session, f.sessionErr
}

func (f *fakeSinkBluetooth) RequestA2DPProfile(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.requests++
	hook, err := f.requestHook, f.requestErr
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return err
}

func (f *fakeSinkBluetooth) setSession(link bluetooth.AudioLink) {
	f.mu.Lock()
	f.session = link
	f.mu.Unlock()
}

func (f *fakeSinkBluetooth) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// shortSessionWindows shrinks the verification timings so the policy tests do
// not wait on the production deadlines.
func shortSessionWindows(t *testing.T) {
	t.Helper()
	verify, request, window := btSessionVerifyDeadline, btSessionRequestDeadline, btRequestWindow
	btSessionVerifyDeadline = 40 * time.Millisecond
	btSessionRequestDeadline = 2 * time.Second
	btRequestWindow = time.Minute
	t.Cleanup(func() {
		btSessionVerifyDeadline, btSessionRequestDeadline, btRequestWindow = verify, request, window
	})
}

func closedChannel() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

const sessionTestAddress = "AA:BB:CC:DD:EE:01"

// A source that stays connected without an A2DP transport has its A2DP profile
// requested exactly once; the request creates a new generation, and the guard
// must refuse a second request so a source that keeps refusing A2DP cannot be
// asked in a loop.
func TestBluetoothSessionProfileRequestRunsOncePerConnection(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	shortSessionWindows(t)
	bt.setSession(bluetooth.AudioLink{
		Address: sessionTestAddress, Connected: true, AudioSource: true,
		Advertised: []string{"a2dp-source", "avrcp-controller"},
	})

	guard := newBTSessionGuard()
	current := func() bool { return true }
	// The wait is event-driven: one manager event is one reason to re-read the
	// link, and the deadline is the only timer.
	go func() {
		time.Sleep(5 * time.Millisecond)
		bt.Memory.SetMedia(bluetooth.MediaState{
			Present: true, DeviceAddress: sessionTestAddress, DeviceName: "Phone",
		})
	}()
	d.verifyBluetoothSession(context.Background(), sessionTestAddress, closedChannel(), guard, 1, current)
	if got := bt.requestCount(); got != 1 {
		t.Fatalf("profile requests = %d, want exactly one", got)
	}
	bt.mu.Lock()
	requests := bt.sessionReqs
	bt.mu.Unlock()
	if requests < 2 {
		t.Fatalf("session was inspected %d times, want a re-read after the manager event", requests)
	}

	// A second verdict inside the same connection does not ask again.
	d.verifyBluetoothSession(context.Background(), sessionTestAddress, closedChannel(), guard, 1, current)
	if got := bt.requestCount(); got != 1 {
		t.Fatalf("profile requests = %d within one connection, want no loop", got)
	}
	// A new connection is a new negotiation and must be able to ask again: a
	// source refused because the previous connection's request was remembered is
	// left with no audio output at all.
	d.verifyBluetoothSession(context.Background(), sessionTestAddress, closedChannel(), guard, 2, current)
	if got := bt.requestCount(); got != 2 {
		t.Fatalf("profile requests = %d after a new connection, want a fresh request", got)
	}
}

// Only a connected audio source that is missing its A2DP transport is asked.
func TestBluetoothSessionProfileRequestSkipsHealthyAndNonAudioLinks(t *testing.T) {
	cases := []struct {
		name string
		link bluetooth.AudioLink
	}{
		{"a2dp", bluetooth.AudioLink{Connected: true, AudioSource: true, A2DPState: "active"}},
		{"disconnected", bluetooth.AudioLink{Connected: false, AudioSource: true}},
		{"peripheral", bluetooth.AudioLink{Connected: true, AudioSource: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bt := newFakeSinkBluetooth()
			d := newBluetoothOrderingDaemon(t, bt)
			shortSessionWindows(t)
			tc.link.Address = sessionTestAddress
			bt.setSession(tc.link)
			d.verifyBluetoothSession(context.Background(), sessionTestAddress, closedChannel(),
				newBTSessionGuard(), 1, func() bool { return true })
			if got := bt.requestCount(); got != 0 {
				t.Fatalf("profile requests = %d for a %s link, want none", got, tc.name)
			}
		})
	}
}

// A verdict that a newer connection generation superseded must not touch the
// device: the request belongs to the old link.
func TestBluetoothSessionProfileRequestHonoursGeneration(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	shortSessionWindows(t)
	bt.setSession(bluetooth.AudioLink{Address: sessionTestAddress, Connected: true, AudioSource: true})
	d.verifyBluetoothSession(context.Background(), sessionTestAddress, closedChannel(),
		newBTSessionGuard(), 1, func() bool { return false })
	if got := bt.requestCount(); got != 0 {
		t.Fatalf("profile requests = %d for a superseded generation, want none", got)
	}
}

// The link is judged after the profile request finished, never while AVDTP setup
// is still in flight.
func TestBluetoothSessionWaitsForTheProfileRequest(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	shortSessionWindows(t)
	bt.setSession(bluetooth.AudioLink{Address: sessionTestAddress, Connected: true, AudioSource: true})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	d.verifyBluetoothSession(ctx, sessionTestAddress, make(chan struct{}), newBTSessionGuard(), 1, func() bool { return true })
	if got := bt.requestCount(); got != 0 {
		t.Fatalf("profile requests = %d before the profile request finished, want none", got)
	}
	bt.mu.Lock()
	requests := bt.sessionReqs
	bt.mu.Unlock()
	if requests != 0 {
		t.Fatalf("session was inspected %d times before the request finished", requests)
	}
}

// A failed profile request is logged and stops; it is never retried.
func TestBluetoothSessionProfileRequestFailureStops(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	shortSessionWindows(t)
	bt.setSession(bluetooth.AudioLink{Address: sessionTestAddress, Connected: true, AudioSource: true})
	bt.mu.Lock()
	bt.requestErr = errors.New("org.bluez.Error.Failed")
	bt.mu.Unlock()
	guard := newBTSessionGuard()
	d.verifyBluetoothSession(context.Background(), sessionTestAddress, closedChannel(),
		guard, 1, func() bool { return true })
	if got := bt.requestCount(); got != 1 {
		t.Fatalf("profile requests = %d, want exactly one attempt", got)
	}
	// The guard that actually ran the request still records the attempt, so a
	// later verdict for the same source is refused instead of cycling it.
	if guard.claimRequest(sessionTestAddress, 1, time.Now()) {
		t.Fatal("the guard that performed the request allowed a second one")
	}
}

// A profile request belongs to the verdict that ordered it. When a newer
// connection generation supersedes that verdict (or the daemon shuts down), the
// request's context is canceled, so it cannot act on a link the user changed.
// The request context must therefore derive from the verdict, not from the
// daemon's lifetime context.
func TestBluetoothSessionProfileRequestIsCancelledByANewerGeneration(t *testing.T) {
	bt := newFakeSinkBluetooth()
	d := newBluetoothOrderingDaemon(t, bt)
	shortSessionWindows(t)
	bt.setSession(bluetooth.AudioLink{Address: sessionTestAddress, Connected: true, AudioSource: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	aborted := make(chan error, 1)
	bt.mu.Lock()
	bt.requestHook = func(repairCtx context.Context) error {
		close(started)
		<-repairCtx.Done()
		aborted <- repairCtx.Err()
		return repairCtx.Err()
	}
	bt.mu.Unlock()

	go d.verifyBluetoothSession(ctx, sessionTestAddress, closedChannel(), newBTSessionGuard(), 1,
		func() bool { return true })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the profile request never started")
	}
	// This is what the watcher does when a new generation supersedes the
	// pending verdict: stopVerify() cancels the verification context.
	cancel()
	select {
	case err := <-aborted:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request context = %v, want it canceled by the superseded generation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight profile request was not aborted")
	}
}

// The guard admits one A2DP profile request per connection generation, not one
// per source: a fresh connection must be able to ask again, which is exactly the
// case the field turned up (a third reconnect was refused and the host showed no
// audio output). Within one generation the attempt stays bounded.
func TestBTSessionGuardAllowsOneRequestPerConnection(t *testing.T) {
	guard := newBTSessionGuard()
	const address = sessionTestAddress
	now := time.Now()
	if !guard.claimRequest(address, 1, now) {
		t.Fatal("the first request was refused")
	}
	if guard.claimRequest(strings.ToLower(address), 1, now) {
		t.Fatal("a request in flight admitted a second one")
	}
	guard.finishRequest(address, 1, now)
	if guard.claimRequest(address, 1, now.Add(time.Second)) {
		t.Fatal("a second request was admitted in the same connection")
	}
	// A new connection is a new negotiation and may ask again immediately.
	if !guard.claimRequest(address, 2, now.Add(2*time.Second)) {
		t.Fatal("a new connection was refused its own request")
	}
	guard.finishRequest(address, 2, now.Add(2*time.Second))
	if !guard.claimRequest(address, 3, now.Add(3*time.Second)) {
		t.Fatal("a third connection was refused its own request")
	}
}
