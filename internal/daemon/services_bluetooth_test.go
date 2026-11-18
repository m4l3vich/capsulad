package daemon

import (
	"context"
	"testing"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/bluetooth"
)

// directionFake is a manager whose resolved direction and exact media state can
// disagree, which is the stale-status case a now-playing projection must not
// turn into a row.
type directionFake struct {
	*bluetooth.Memory
	direction bluetooth.Direction
}

func (f *directionFake) PlaybackDirection(context.Context) (bluetooth.Direction, error) {
	return f.direction, nil
}

// TestBluetoothObservedPlayingExcludesCommandMemory pins the projection's
// evidence rule: a direction resolved from receipt/transport/status evidence is
// observed playback, while command memory is only the next toggle's choice. In
// particular a paused source whose cached AVRCP status still says playing must
// not be published as observed playing.
func TestBluetoothObservedPlayingExcludesCommandMemory(t *testing.T) {
	const address = "AA:BB:CC:DD:EE:40"
	fake := &directionFake{Memory: bluetooth.NewMemory()}
	fake.Memory.SimulateConnect(bluetooth.Device{Address: address, Name: "Phone"},
		bluetooth.MediaState{Present: true, Playing: false, DeviceAddress: address, DeviceName: "Phone"})
	service := &bluetoothMediaService{d: &Daemon{bt: fake}}
	state := func() *frameworkv1.BluetoothMediaState {
		t.Helper()
		media, err := fake.Memory.Media(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return service.stateProto(context.Background(), media)
	}

	// A MediaControl1-only source publishes no AVRCP status; sink receipts are
	// the observation that keeps it visible.
	fake.direction = bluetooth.Direction{Present: true, Playing: true, Evidence: bluetooth.EvidenceReceipt}
	if got := state().GetObservedPlaying(); !got {
		t.Fatal("receipt-backed playback is not observed")
	}

	// A command this daemon sent is not evidence the peer acted on it.
	fake.direction = bluetooth.Direction{Present: true, Playing: true, Evidence: bluetooth.EvidenceMemory}
	if got := state().GetObservedPlaying(); got {
		t.Fatal("command-memory direction reported as observed playback")
	}

	// The stale-status case: state.Playing still true after this daemon paused
	// the source, while the resolved direction is command memory saying paused.
	fake.Memory.SimulateConnect(bluetooth.Device{Address: address, Name: "Phone"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: address, DeviceName: "Phone"})
	fake.direction = bluetooth.Direction{Present: true, Playing: false, Evidence: bluetooth.EvidenceMemory}
	if got := state().GetObservedPlaying(); got {
		t.Fatal("stale AVRCP status reported as observed playback after a pause command")
	}
}
