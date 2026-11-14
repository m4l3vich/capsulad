package daemon

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsula/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/bluetooth"
)

// The physical circle key and left/right swipes drive the standard provider,
// not a private daemon transport. This exercises the same routed call an app
// would receive and verifies that toggling follows the published state.
func TestSystemMediaUsesStandardProvider(t *testing.T) {
	binary := buildTestdataApp(t, "mediafixture")
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: mediaFixture, binaryName: "mediafixture", binaryPath: binary,
		manifest: []byte(mediaManifest),
		files:    map[string][]byte{"schemas/media.protoset": mediaDescriptorSet(t)},
	})
	conn := dialTest(t, d.cfg.SocketPath)
	_ = waitReady(t, d, mediaFixture, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	players := func() []*mediav1.PlayerStatus {
		t.Helper()
		req, _ := proto.Marshal(&mediav1.ListPlayersRequest{})
		raw := routedCall(t, conn, mediaFixture, mediaService, "ListPlayers", [][]byte{req})
		if len(raw) != 1 {
			t.Fatalf("ListPlayers returned %d messages", len(raw))
		}
		var resp mediav1.ListPlayersResponse
		if err := proto.Unmarshal(raw[0], &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Players
	}
	waitState := func(want mediav1.PlaybackState) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, p := range players() {
				if p.PlayerId == "local.player" && p.PlaybackState == want {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("provider never reached %v: %v", want, players())
	}

	d.sysMedia.execute(ctx, mediaToggle)
	waitState(mediav1.PlaybackState_PLAYBACK_PLAYING)
	d.sysMedia.execute(ctx, mediaToggle)
	waitState(mediav1.PlaybackState_PLAYBACK_PAUSED)

	waitTitle := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, p := range players() {
				if p.PlayerId == "local.player" && p.GetTitle() == want {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("provider title never became %q: %v", want, players())
	}
	if got := players(); len(got) == 0 || got[0].GetTitle() != "Aurora" {
		t.Fatalf("fixture did not start on Aurora: %v", got)
	}
	// The standard-provider path must preserve NEXT/PREVIOUS direction, not
	// merely report an accepted command.
	d.sysMedia.execute(ctx, mediaNext)
	waitTitle("Borealis")
	d.sysMedia.execute(ctx, mediaPrevious)
	waitTitle("Aurora")
}

// With no media provider installed, the physical circle and panel swipes must
// still control the connected Bluetooth source through the public
// BluetoothMediaService. This drives the real input manager, not execute()
// directly.
func TestSystemMediaFallsBackToConnectedBluetooth(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	const address = "AA:BB:CC:DD:EE:10"
	mem.SimulateConnect(bluetooth.Device{Address: address, Name: "Phone"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: address})
	mem.SetPlaylist([]string{"track-one", "track-two", "track-three"})
	conn := dialTest(t, d.cfg.SocketPath)
	btClient := frameworkv1.NewBluetoothMediaServiceClient(conn)

	state := func() *frameworkv1.BluetoothMediaState {
		t.Helper()
		s, err := btClient.GetState(context.Background(), &frameworkv1.GetBluetoothMediaStateRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	waitPlaying := func(want bool) {
		t.Helper()
		waitFor(t, 5*time.Second, "Bluetooth playing state", func() bool {
			return state().GetPlaying() == want
		})
	}
	tapCircle := func() {
		d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
		time.Sleep(60 * time.Millisecond)
		d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: false}})
	}
	swipe := func(keys ...string) {
		for _, key := range keys {
			d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: key, Pressed: true}})
		}
		for _, key := range keys {
			d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: key, Pressed: false}})
		}
	}

	tapCircle()
	waitPlaying(false)
	tapCircle()
	waitPlaying(true)

	// Swipes are discrete key sequences. The simulator playlist publishes the
	// resulting track through the ordinary public MediaState, so right must be
	// NEXT (track-two) and left must be PREVIOUS (back to track-one); a reversed
	// or swapped command cannot satisfy both transitions.
	if got := state().GetTitle(); got != "track-one" {
		t.Fatalf("playlist did not start at track-one: %q", got)
	}
	swipe("volume_down", "center", "volume_up") // right -> next
	waitFor(t, 5*time.Second, "Bluetooth next track", func() bool {
		return state().GetTitle() == "track-two"
	})
	swipe("volume_up", "center", "volume_down") // left -> previous
	waitFor(t, 5*time.Second, "Bluetooth previous track", func() bool {
		return state().GetTitle() == "track-one"
	})
}
