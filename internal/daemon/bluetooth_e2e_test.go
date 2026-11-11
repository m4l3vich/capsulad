package daemon

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/bluetooth"
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
