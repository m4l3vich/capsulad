package daemon

import (
	"context"
	"math"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/bluetooth"
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

	// App-facing: discoverability and pairability.
	if reply, err := client.SetBluetoothDiscoverable(ctx, &frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: true}); err != nil || !reply.GetDiscoverable() {
		t.Fatalf("discoverable: reply=%+v err=%v", reply, err)
	}
	reply, err := client.SetBluetoothPairable(ctx, &frameworkv1.SetBluetoothPairableRequest{Pairable: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.GetPairable() {
		t.Fatalf("pairable not applied: %+v", reply)
	}
	if !d.user.Get().Values.BluetoothPairable {
		t.Fatal("pairable switch was not persisted")
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
	content, life, err := bluetoothPulseContent()
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
	content, life, err := bluetoothPulseContent()
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
	// A source-side AVRCP change reaches the master through the event path
	// (no polling ticker) and the mirrored level is not pushed back.
	mem.SimulateVolume(64)
	waitFor(t, 2*time.Second, "master volume synced from source", func() bool {
		return math.Abs(d.user.Get().Values.MasterVolume-64.0/127) < 0.001
	})
}
