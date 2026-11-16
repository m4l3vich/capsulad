package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
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

// With no media provider running, the physical circle and panel swipes must
// still control the connected Bluetooth source through the public
// BluetoothMediaService. This daemon ships no Bluetooth provider app, so this is
// the zero-provider path; it drives the real input manager, not execute().
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

	// The measured hardware shape of a skip: the panel reports the centre key for
	// the anchored start of the contact and the volume edge for its end, with no
	// overlap. One centre-gated swipe must skip exactly one track and step volume
	// zero times, because the completing edge is the only contact suppressed.
	volumeBefore := d.user.Get().Values.MasterVolume
	base := d.clock.NowNS()
	panelAt := func(offset time.Duration, key string, pressed bool) {
		d.input.Handle(backend.InputEvent{MonotonicNS: base + uint64(offset),
			Key: &backend.KeyEvent{Key: key, Pressed: pressed}})
	}
	panelAt(0, "center", true)
	panelAt(70*time.Millisecond, "center", false)
	panelAt(150*time.Millisecond, "volume_up", true) // 80 ms after the centre release
	panelAt(230*time.Millisecond, "volume_up", false)
	waitFor(t, 5*time.Second, "Bluetooth next track after a centre-gated swipe", func() bool {
		return state().GetTitle() == "track-two"
	})
	time.Sleep(250 * time.Millisecond)
	if got := state().GetTitle(); got != "track-two" {
		t.Fatalf("one centre-gated swipe skipped more than one track: title %q", got)
	}
	if got := d.user.Get().Values.MasterVolume; got != volumeBefore {
		t.Fatalf("centre-gated swipe stepped volume: %v -> %v", volumeBefore, got)
	}
}

// staleStatusBluetooth models the BlueZ shape the direction memory exists for:
// the source's cached AVRCP status keeps reporting "playing" after this daemon
// paused it, so MediaState.Playing is stale while the manager's resolved
// direction follows the command it actually sent.
type staleStatusBluetooth struct {
	*bluetooth.Memory

	mu        sync.Mutex
	commands  []bluetooth.Command
	direction bluetooth.Direction
}

func newStaleStatusBluetooth(address, name string) *staleStatusBluetooth {
	f := &staleStatusBluetooth{Memory: bluetooth.NewMemory()}
	f.Memory.SimulateConnect(bluetooth.Device{Address: address, Name: name},
		bluetooth.MediaState{
			Present: true, Playing: true, DeviceAddress: address, DeviceName: name,
			Title: "Track",
		})
	f.direction = bluetooth.Direction{Present: true, Playing: true, Evidence: bluetooth.EvidenceStatus}
	return f
}

func (f *staleStatusBluetooth) PlaybackDirection(context.Context) (bluetooth.Direction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.direction, nil
}

func (f *staleStatusBluetooth) Command(_ context.Context, cmd bluetooth.Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
	if !f.direction.Present {
		return bluetooth.ErrNoSource
	}
	switch cmd {
	case bluetooth.CommandPlay:
		f.direction = bluetooth.Direction{Present: true, Playing: true, Evidence: bluetooth.EvidenceMemory}
	case bluetooth.CommandPause:
		f.direction = bluetooth.Direction{Present: true, Playing: false, Evidence: bluetooth.EvidenceMemory}
	case bluetooth.CommandToggle:
		f.direction.Playing = !f.direction.Playing
	}
	return nil
}

func (f *staleStatusBluetooth) commandLog() []bluetooth.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bluetooth.Command(nil), f.commands...)
}

// A stale cached status must not invert the next tap. The source keeps reporting
// "playing" after the framework paused it, so a consumer that mapped the
// source-published status would stay PLAYING, the controller would send Pause
// again, and the speaker would show the field defect: pause works, play does
// not. The framework publishes the manager's resolved direction on
// BluetoothMediaService, and the physical key resolves its direction inside the
// manager, so the two can never disagree.
func TestSystemMediaToggleFollowsTheResolvedDirection(t *testing.T) {
	const address = "AA:BB:CC:DD:EE:30"
	stale := newStaleStatusBluetooth(address, "Phone")
	// Install the manager the framework resolves against before Run starts.
	// There is no provider app to restart: BluetoothMediaService reads the
	// manager directly, and the system media controller reaches it through the
	// zero-provider fallback.
	d, cancel, _ := newTestDaemonPrepared(t, func(d *Daemon) { d.bt = stale })
	defer cancel()

	conn := dialTest(t, d.cfg.SocketPath)
	state := func() *frameworkv1.BluetoothMediaState {
		t.Helper()
		s, err := frameworkv1.NewBluetoothMediaServiceClient(conn).GetState(context.Background(),
			&frameworkv1.GetBluetoothMediaStateRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	waitDirection := func(want bool) {
		t.Helper()
		waitFor(t, 5*time.Second, "resolved playback direction", func() bool {
			s := state()
			return s.GetDirectionKnown() && s.GetDirectionPlaying() == want
		})
	}
	waitDirection(true)

	toggleSystemMedia(t, d)
	waitFor(t, 5*time.Second, "Bluetooth toggle command", func() bool {
		log := stale.commandLog()
		return len(log) > 0 && log[len(log)-1] == bluetooth.CommandToggle
	})
	// The cached status never changed, but the published direction did.
	waitDirection(false)
	if media, err := stale.Media(context.Background()); err != nil || !media.Playing {
		t.Fatalf("test setup lost its stale status: %+v err=%v", media, err)
	}

	// The next tap must resume, not repeat the pause the direction already shows.
	toggleSystemMedia(t, d)
	waitFor(t, 5*time.Second, "second Bluetooth toggle command", func() bool {
		log := stale.commandLog()
		return len(log) > 1 && log[len(log)-1] == bluetooth.CommandToggle
	})
	waitDirection(true)

	got := stale.commandLog()
	if len(got) != 2 || got[0] != bluetooth.CommandToggle || got[1] != bluetooth.CommandToggle {
		t.Fatalf("commands = %v, want two toggles resolved by the manager", got)
	}
}

// toggleSystemMedia drives the same circle-key action media_control exposes.
func toggleSystemMedia(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.sysMedia.execute(ctx, mediaToggle)
}
