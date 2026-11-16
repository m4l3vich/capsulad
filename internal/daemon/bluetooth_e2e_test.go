package daemon

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/bluetooth"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
)

// TestBluetoothAdminSurface exercises the app-facing adapter controls and the
// administrator-only paired-device management. Paired-device RPCs must reject
// supervised callers even though the adapter controls do not.
func TestBluetoothAdminSurface(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	mem.SimulateConnect(bluetooth.Device{Address: "11:22:33:44:55:66", Name: "Phone"}, bluetooth.MediaState{Title: "Track"})
	conn := dialTest(t, d.cfg.SocketPath)
	client := frameworkv1.NewSystemServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	statusReply, err := client.GetBluetoothStatus(ctx, &frameworkv1.GetBluetoothStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !statusReply.GetPresent() || !statusReply.GetPowered() {
		t.Fatalf("adapter status = %+v", statusReply)
	}
	if statusReply.GetName() != "Capsula" {
		t.Fatalf("adapter advertised name = %q", statusReply.GetName())
	}

	// App-facing: discoverability and incoming connections. Discoverability
	// implies incoming; disabling incoming clears discoverability.
	if reply, err := client.SetBluetoothDiscoverable(ctx, &frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: true}); err != nil || !reply.GetDiscoverable() {
		t.Fatalf("discoverable: reply=%+v err=%v", reply, err)
	}
	reply, err := client.SetBluetoothIncomingConnections(ctx, &frameworkv1.SetBluetoothIncomingConnectionsRequest{IncomingConnections: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.GetIncomingConnections() {
		t.Fatalf("incoming connections not applied: %+v", reply)
	}
	if !d.user.Get().Values.BluetoothIncomingConnections || !d.user.Get().Values.BluetoothDiscoverable {
		t.Fatal("Bluetooth switches were not persisted")
	}
	reply, err = client.SetBluetoothIncomingConnections(ctx, &frameworkv1.SetBluetoothIncomingConnectionsRequest{IncomingConnections: false})
	if err != nil {
		t.Fatal(err)
	}
	if reply.GetIncomingConnections() || reply.GetDiscoverable() || !reply.GetPowered() {
		t.Fatalf("incoming off did not keep the radio on and deny/discover off: %+v", reply)
	}
	if d.user.Get().Values.BluetoothIncomingConnections || d.user.Get().Values.BluetoothDiscoverable {
		t.Fatal("incoming off did not persist the dependent discoverable state")
	}

	// Paired-device management is local-admin only.
	appCtx := metadata.AppendToOutgoingContext(ctx, "capsula-run-id", "run_fake")
	if _, err := client.ListPairedDevices(appCtx, &frameworkv1.ListPairedDevicesRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("app caller listed paired devices: %v", err)
	}
	if _, err := client.UnpairDevice(appCtx, &frameworkv1.UnpairDeviceRequest{Address: "11:22:33:44:55:66"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("app caller unpaired a device: %v", err)
	}

	paired, err := client.ListPairedDevices(ctx, &frameworkv1.ListPairedDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(paired.GetDevices()) != 1 || paired.GetDevices()[0].GetName() != "Phone" {
		t.Fatalf("paired devices = %+v", paired.GetDevices())
	}
	if _, err := client.UnpairDevice(ctx, &frameworkv1.UnpairDeviceRequest{Address: "11:22:33:44:55:66"}); err != nil {
		t.Fatal(err)
	}
	paired, err = client.ListPairedDevices(ctx, &frameworkv1.ListPairedDevicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(paired.GetDevices()) != 0 {
		t.Fatalf("device still paired: %+v", paired.GetDevices())
	}
}

// TestBluetoothPulseCompletesThreeFades pins the connect/disconnect animation:
// three fade-in/out cycles, ending dark before the presentation is released.
func TestBluetoothPulseCompletesThreeFades(t *testing.T) {
	content, life, err := bluetoothPulseContent(bluetoothPulseCount)
	if err != nil {
		t.Fatal(err)
	}
	period := time.Duration(float64(time.Second) / bluetoothPulseSpeed)
	if want := time.Duration(bluetoothPulseCount)*period + bluetoothPulseGuard; life != want {
		t.Fatalf("pulse lifetime = %v, want %v", life, want)
	}
	above := false
	pulses := 0
	for ns := time.Duration(0); ns < time.Duration(bluetoothPulseCount)*period; ns += 10 * time.Millisecond {
		px, err := presets.RingFrame(content.PresetID, content.Params, 12, ns.Seconds())
		if err != nil {
			t.Fatal(err)
		}
		lit := px[0].B > 127 // brighter than half intensity
		if lit && !above {
			pulses++
		}
		above = lit
	}
	if pulses != bluetoothPulseCount {
		t.Fatalf("pulses = %d, want %d", pulses, bluetoothPulseCount)
	}
	final, err := presets.RingFrame(content.PresetID, content.Params, 12, (life - bluetoothPulseGuard).Seconds())
	if err != nil {
		t.Fatal(err)
	}
	if final[0] != (backend.RGB{}) {
		t.Fatalf("ring is not dark at the end of the final fade: %+v", final[0])
	}
}

func TestBluetoothIndicatorContent(t *testing.T) {
	content, life := bluetoothIndicatorContent(4)
	if life != 2*time.Second {
		t.Fatalf("indicator lifetime = %v, want 2s", life)
	}
	if len(content.Cells) != 4 || content.Cells[0] != 0 || content.Cells[1] != 0 {
		t.Fatalf("cells = %#v", content.Cells)
	}
	if content.Cells[2] != bluetoothGlyphs[0] || content.Cells[3] != bluetoothGlyphs[1] {
		t.Fatalf("rightmost cells = %#v, want %#v", content.Cells[2:], bluetoothGlyphs)
	}
	if content.Colon {
		t.Fatal("bt indicator lights the colon")
	}
}

// TestBluetoothNoticeOwnership proves the notification primitives are ordinary
// presentation captures: holding one wins routing and releasing it restores the
// resource to its previous owner.
func TestBluetoothNoticeOwnership(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()
	ringID := d.ringResourceID()
	if ringID == "" {
		t.Fatal("no ring resource")
	}
	content, life, err := bluetoothPulseContent(bluetoothPulseCount)
	if err != nil {
		t.Fatal(err)
	}
	n := &bluetoothNotice{}
	n.show(d, ringID, content, life)
	if n.capture == "" {
		t.Fatal("notice acquired no capture")
	}
	capture := n.capture
	if got := d.router.Current(ringID).SelectedCaptureID; got != capture {
		t.Fatalf("selected capture = %q, want %q", got, capture)
	}
	n.close(d)
	if got := d.router.Current(ringID).SelectedCaptureID; got == capture {
		t.Fatalf("notice capture %q still selected after release", capture)
	}
}

// TestBluetoothConnectNotifications drives a simulated source connect through
// the real watcher and checks the ring pulse and the two-cell bt indicator are
// presented, then released.
func TestBluetoothConnectNotifications(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	ringID := d.ringResourceID()
	displayID := d.segmentDisplayResourceID()
	if ringID == "" || displayID == "" {
		t.Fatalf("resources ring=%q display=%q", ringID, displayID)
	}
	if sel := d.router.Current(displayID); sel.SelectedAppID == routing.SystemAppID {
		t.Fatalf("display already had a system capture: %+v", sel)
	}

	// The watcher is started asynchronously by Run; connect only once it is
	// subscribed, otherwise the simulator has nobody to deliver the event to.
	waitFor(t, 3*time.Second, "bluetooth watcher", func() bool {
		return mem.SubscriberCount() > 0
	})
	mem.SimulateConnect(bluetooth.Device{Address: "11:22:33:44:55:66", Name: "Mac"}, bluetooth.MediaState{Title: "Track"})

	waitFor(t, 3*time.Second, "ring pulse capture", func() bool {
		return d.router.Current(ringID).SelectedAppID == routing.SystemAppID
	})
	waitFor(t, 3*time.Second, "bt indicator capture", func() bool {
		return d.router.Current(displayID).SelectedAppID == routing.SystemAppID
	})

	sel := d.router.Current(displayID)
	pres, ok := d.pres.Get(sel.SelectedPresentation)
	if !ok {
		t.Fatalf("selected presentation %q not found", sel.SelectedPresentation)
	}
	digitCount := 4
	if info := d.resources[displayID]; info != nil && info.GetSegment() != nil {
		digitCount = int(info.GetSegment().GetDigitCount())
	}
	cells, colon, ok := pres.RenderSegment(backend.SegmentCaps{DigitCount: digitCount, SegmentsPerDigit: 7, SupportsColon: true})
	if !ok || colon {
		t.Fatalf("segment frame ok=%v colon=%v", ok, colon)
	}
	want := bluetoothIndicatorCells(digitCount)
	if len(cells) != len(want) {
		t.Fatalf("cells = %#v, want %#v", cells, want)
	}
	for i := range want {
		if cells[i] != want[i] {
			t.Fatalf("cells = %#v, want %#v", cells, want)
		}
	}

	waitFor(t, 4*time.Second, "bt indicator released", func() bool {
		return d.router.Current(displayID).SelectedAppID != routing.SystemAppID
	})
	waitFor(t, 3*time.Second, "ring pulse released", func() bool {
		return d.router.Current(ringID).SelectedAppID != routing.SystemAppID
	})
	// A source-side AVRCP change reaches the daemon through the event path (no
	// polling ticker) and becomes the framework master: the phone's slider drives
	// the speaker. The mirror then carries the same level back, so the source's
	// display agrees with the master, and the peer's echo of that write is not
	// adopted as a second change.
	mem.SimulateVolume(64)
	waitFor(t, 2*time.Second, "master volume synced from source", func() bool {
		return math.Abs(d.user.Get().Values.MasterVolume-64.0/127) < 0.001
	})
	waitFor(t, 2*time.Second, "the source register mirrors the master", func() bool {
		level, ok := mem.TransportVolumeWritten()
		return ok && level == 64
	})
}

// bluetoothWatchStream is the minimal server stream the media watcher needs:
// it records each state and is ended through its context.
type bluetoothWatchStream struct {
	ctx  context.Context
	sent chan *frameworkv1.BluetoothMediaState
}

func (s *bluetoothWatchStream) Send(m *frameworkv1.BluetoothMediaState) error {
	s.sent <- m
	return nil
}

func (s *bluetoothWatchStream) SetHeader(metadata.MD) error  { return nil }
func (s *bluetoothWatchStream) SendHeader(metadata.MD) error { return nil }
func (s *bluetoothWatchStream) SetTrailer(metadata.MD)       {}
func (s *bluetoothWatchStream) Context() context.Context     { return s.ctx }
func (s *bluetoothWatchStream) SendMsg(any) error            { return nil }
func (s *bluetoothWatchStream) RecvMsg(any) error            { return nil }

func nextBluetoothState(t *testing.T, stream *bluetoothWatchStream, what string) *frameworkv1.BluetoothMediaState {
	t.Helper()
	select {
	case state := <-stream.sent:
		return state
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// TestBluetoothMediaWatchTracksSourceLifecycle drives the app-facing watch
// stream end to end. AVRCP volume events carry the unchanged media revision
// and must not be republished as media, while connect and disconnect must
// reach the subscriber even though a provider may attach at any time.
func TestBluetoothMediaWatchTracksSourceLifecycle(t *testing.T) {
	// The watcher only reads the manager, so this needs no resources, apps or
	// audio subsystem.
	mem := bluetooth.NewMemory()
	d := &Daemon{bt: mem}
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	stream := &bluetoothWatchStream{ctx: streamCtx, sent: make(chan *frameworkv1.BluetoothMediaState, 16)}
	done := make(chan error, 1)
	go func() {
		done <- (&bluetoothMediaService{d: d}).WatchState(&frameworkv1.WatchBluetoothMediaStateRequest{}, stream)
	}()

	// Receiving the snapshot proves the subscription exists, so the connect
	// below cannot race the registration.
	if initial := nextBluetoothState(t, stream, "initial Bluetooth state"); initial.GetPresent() {
		t.Fatalf("initial state reports a source: %+v", initial)
	}

	const address = "11:22:33:44:55:66"
	mem.SimulateConnect(bluetooth.Device{Address: address, Name: "Phone"}, bluetooth.MediaState{Title: "Track"})
	connected := nextBluetoothState(t, stream, "connected state")
	if !connected.GetPresent() || connected.GetDeviceAddress() != address {
		t.Fatalf("connected state = %+v", connected)
	}

	// A source-side volume change is not a media change.
	mem.SimulateVolume(64)
	select {
	case state := <-stream.sent:
		t.Fatalf("volume change republished media: %+v", state)
	case <-time.After(150 * time.Millisecond):
	}

	mem.SimulateDisconnect(address)
	disconnected := nextBluetoothState(t, stream, "disconnected state")
	if disconnected.GetPresent() {
		t.Fatalf("disconnected state still reports a source: %+v", disconnected)
	}

	stopStream()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not end with its stream")
	}
}

// TestBluetoothDisconnectNotifications pins the disconnect notice: one ring
// pulse and no segment glyph. The display must return to its selected baseline
// immediately instead of showing "bt".
func TestBluetoothDisconnectNotifications(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	ringID := d.ringResourceID()
	displayID := d.segmentDisplayResourceID()
	if ringID == "" || displayID == "" {
		t.Fatalf("resources ring=%q display=%q", ringID, displayID)
	}
	waitFor(t, 3*time.Second, "bluetooth watcher", func() bool {
		return mem.SubscriberCount() > 0
	})
	const address = "11:22:33:44:55:66"
	mem.SimulateConnect(bluetooth.Device{Address: address, Name: "Mac"}, bluetooth.MediaState{Title: "Track"})
	waitFor(t, 3*time.Second, "connect notifications", func() bool {
		return d.router.Current(ringID).SelectedAppID == routing.SystemAppID &&
			d.router.Current(displayID).SelectedAppID == routing.SystemAppID
	})

	// Let both connect notices expire so the next system capture can only be
	// the disconnect pulse.
	waitFor(t, 4*time.Second, "connect bt indicator released", func() bool {
		return d.router.Current(displayID).SelectedAppID != routing.SystemAppID
	})
	waitFor(t, 5*time.Second, "connect pulse released", func() bool {
		return d.router.Current(ringID).SelectedAppID != routing.SystemAppID
	})

	mem.SimulateDisconnect(address)
	waitFor(t, 3*time.Second, "disconnect ring pulse", func() bool {
		return d.router.Current(ringID).SelectedAppID == routing.SystemAppID
	})
	// The disconnect notice never captures the segment display.
	time.Sleep(100 * time.Millisecond)
	if sel := d.router.Current(displayID); sel.SelectedAppID == routing.SystemAppID {
		t.Fatalf("disconnect captured the display: %+v", sel)
	}

	// One pulse, not three: the notice is released after about a second.
	start := time.Now()
	waitFor(t, 2*time.Second, "disconnect pulse released", func() bool {
		return d.router.Current(ringID).SelectedAppID != routing.SystemAppID
	})
	if elapsed := time.Since(start); elapsed > 1800*time.Millisecond {
		t.Fatalf("disconnect pulse lasted %s; one pulse is expected", elapsed)
	}
}

// TestBluetoothStreamStopsOnlyForItsSource pins disconnect scoping in the
// single-stream sink: a second paired device leaving must not cut off the
// source that is streaming, because no transport event reacquires it until
// that source pauses or resumes. Only an event for the stream's own address
// tears it down (case-insensitively).
func TestBluetoothStreamStopsOnlyForItsSource(t *testing.T) {
	d := &Daemon{bt: bluetooth.NewMemory()}
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	stream := &a2dpStream{
		d:         d,
		address:   "AA:BB:CC:DD:EE:01",
		transport: &bluetooth.A2DPTransport{File: file},
		done:      make(chan struct{}),
	}
	d.btStream = stream

	d.stopBluetoothStreamFor("AA:BB:CC:DD:EE:02")
	if d.btStream != stream {
		t.Fatal("a different device's disconnect stopped the active stream")
	}

	d.stopBluetoothStreamFor("aa:bb:cc:dd:ee:01")
	if d.btStream != nil {
		t.Fatal("the streaming source's own disconnect did not stop it")
	}
	if d.btStreamGen == 0 {
		t.Fatal("stopping the stream did not invalidate pending acquisitions")
	}
}

// TestBluetoothResyncStopsSourceLostToOverflow covers the daemon side of the
// bounded watcher queue. A disconnects while the watcher is behind, then more
// than a full queue of events arrives for B; A's disconnect transition itself
// is coalesced away. The retained resync marker must make the daemon reconcile
// against the connected-device list, so A's stream and route are still cleaned
// and B's route is kept.
func TestBluetoothResyncStopsSourceLostToOverflow(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	// Only the Bluetooth session fields matter here; no daemon loops start.
	d := &Daemon{ctx: context.Background(), clock: clock, logs: logs}
	mem := bluetooth.NewMemory()
	d.bt = mem
	d.setBluetoothNative(true)
	d.dev = backend.NewSimulator(backend.SimulatorOptions{})

	const sourceA, sourceB = "AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"
	ctx, stopEvents := context.WithCancel(context.Background())
	defer stopEvents()
	events := mem.Watch(ctx)
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	d.btStream = &a2dpStream{
		d: d, address: sourceA,
		transport: &bluetooth.A2DPTransport{File: file}, done: make(chan struct{}),
	}
	d.btRoutes = map[string]string{sourceA: "42", sourceB: "43"}

	mem.SimulateConnect(bluetooth.Device{Address: sourceA, Name: "A"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: sourceA})
	mem.SimulateConnect(bluetooth.Device{Address: sourceB, Name: "B"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: sourceB})
	mem.SimulateDisconnect(sourceA)
	// Far more events than the watcher window, all for the source that stays.
	for range 64 {
		mem.SetMedia(bluetooth.MediaState{
			Present: true, Playing: true, DeviceAddress: sourceB, Title: "b",
		})
	}

	// The consumer was not reading, so the tail is a resync marker, not A's
	// disconnect event.
	var last bluetooth.Event
	for {
		select {
		case ev := <-events:
			last = ev
			continue
		default:
		}
		break
	}
	if last.Kind != bluetooth.EventResync {
		t.Fatalf("queue tail = %+v, want a resync marker", last.Kind)
	}
	if !last.Media.Present || last.Media.DeviceAddress != sourceB {
		t.Fatalf("marker snapshot = %+v, want the surviving source", last.Media)
	}

	d.reconcileBluetoothSession(last.Media)
	if d.btStream != nil {
		t.Fatal("the disconnected source's stream survived the resync")
	}
	if _, ok := d.btRoutes[sourceA]; ok {
		t.Fatal("the disconnected source's route survived the resync")
	}
	if _, ok := d.btRoutes[sourceB]; !ok {
		t.Fatal("the connected source's route was removed by the resync")
	}
}

// A disconnect immediately clears a connect glyph that is still on screen
// instead of leaving "bt" up for the rest of its two-second notice.
func TestBluetoothDisconnectClearsConnectGlyph(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	ringID := d.ringResourceID()
	displayID := d.segmentDisplayResourceID()
	waitFor(t, 3*time.Second, "bluetooth watcher", func() bool {
		return mem.SubscriberCount() > 0
	})
	const address = "11:22:33:44:55:66"
	mem.SimulateConnect(bluetooth.Device{Address: address, Name: "Mac"}, bluetooth.MediaState{})
	waitFor(t, 3*time.Second, "connect bt glyph", func() bool {
		return d.router.Current(displayID).SelectedAppID == routing.SystemAppID
	})
	mem.SimulateDisconnect(address)
	waitFor(t, 500*time.Millisecond, "disconnect clears the glyph", func() bool {
		return d.router.Current(displayID).SelectedAppID != routing.SystemAppID
	})
	waitFor(t, 3*time.Second, "disconnect ring pulse", func() bool {
		return d.router.Current(ringID).SelectedAppID == routing.SystemAppID
	})
}
