package bluetooth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func obj(iface string, props map[string]dbus.Variant) map[string]map[string]dbus.Variant {
	return map[string]map[string]dbus.Variant{iface: props}
}

func playerObjects(playing bool) map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
	status := "paused"
	if playing {
		status = "playing"
	}
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	track := map[string]dbus.Variant{
		"Title":    dbus.MakeVariant("Scarlet Fire"),
		"Artist":   dbus.MakeVariant("Otis McDonald"),
		"Album":    dbus.MakeVariant("Single"),
		"Duration": dbus.MakeVariant(uint32(215000)),
	}
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci0": obj("org.bluez.Adapter1", map[string]dbus.Variant{
			"Powered": dbus.MakeVariant(true), "Discoverable": dbus.MakeVariant(false),
			"Pairable": dbus.MakeVariant(true), "Address": dbus.MakeVariant("8A:7C:50:49:22:22"),
		}),
		dev: obj("org.bluez.Device1", map[string]dbus.Variant{
			"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF"), "Alias": dbus.MakeVariant("Test phone"),
			"Paired": dbus.MakeVariant(true), "Connected": dbus.MakeVariant(true),
		}),
		dev + "/player0": obj("org.bluez.MediaPlayer1", map[string]dbus.Variant{
			"Status": dbus.MakeVariant(status), "Position": dbus.MakeVariant(uint32(42000)),
			"Track": dbus.MakeVariant(track), "Device": dbus.MakeVariant(dev),
		}),
	}
}

func TestMediaFromPlayerCarriesMetadata(t *testing.T) {
	state := mediaFrom(playerObjects(true))
	if !state.Present || !state.Playing {
		t.Fatalf("state = %+v", state)
	}
	if state.Title != "Scarlet Fire" || state.Artist != "Otis McDonald" || state.Album != "Single" {
		t.Fatalf("metadata = %+v", state)
	}
	if state.DurationMS != 215000 || state.PositionMS != 42000 {
		t.Fatalf("timing = %+v", state)
	}
	if state.DeviceAddress != "AA:BB:CC:DD:EE:FF" || state.DeviceName != "Test phone" {
		t.Fatalf("device = %+v", state)
	}
}

func TestMediaFromControlFallback(t *testing.T) {
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci0/dev_AA": {
			"org.bluez.Device1":       {"Address": dbus.MakeVariant("AA:BB"), "Alias": dbus.MakeVariant("Phone"), "Connected": dbus.MakeVariant(true)},
			"org.bluez.MediaControl1": {"Connected": dbus.MakeVariant(true)},
		},
	}
	state := mediaFrom(objs)
	if !state.Present || state.Detail == "" {
		t.Fatalf("control fallback = %+v", state)
	}
}

func TestMediaFromTransportFallback(t *testing.T) {
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci0/dev_AA/sep1/fd0": {
			"org.bluez.MediaTransport1": {"State": dbus.MakeVariant("active"), "Device": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0/dev_AA"))},
		},
	}
	state := mediaFrom(objs)
	if !state.Present {
		t.Fatalf("transport fallback = %+v", state)
	}
	if empty := mediaFrom(map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}); empty.Present {
		t.Fatalf("empty tree reported a source: %+v", empty)
	}
}

func TestDevicesFromAndUnpairLookup(t *testing.T) {
	objs := playerObjects(true)
	devices := devicesFrom(objs)
	if len(devices) != 1 || devices[0].Name != "Test phone" || !devices[0].Paired || !devices[0].Connected {
		t.Fatalf("devices = %+v", devices)
	}
	if path := findDevicePath(objs, "aa:bb:cc:dd:ee:ff"); path == "" {
		t.Fatal("address lookup is not case-insensitive")
	}
}

func TestMemoryManagerEventsAndCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemory()
	events := m.Watch(ctx)

	m.SimulateConnect(Device{Address: "AA:BB", Name: "Phone"}, MediaState{Title: "Song", Playing: true})
	select {
	case ev := <-events:
		if ev.Kind != EventConnected || ev.DeviceName != "Phone" || !ev.Media.Present {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no connect event")
	}

	if err := m.Command(ctx, CommandPause); err != nil {
		t.Fatal(err)
	}
	state, _ := m.Media(ctx)
	if state.Playing {
		t.Fatal("pause did not change state")
	}
	devices, _ := m.PairedDevices(ctx)
	if len(devices) != 1 {
		t.Fatalf("paired devices = %+v", devices)
	}
	if err := m.Unpair(ctx, "AA:BB"); err != nil {
		t.Fatal(err)
	}
	if devices, _ = m.PairedDevices(ctx); len(devices) != 0 {
		t.Fatalf("device still paired: %+v", devices)
	}
}

func TestTransportVolumeAndUsableTransport(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA/sep2/fd0")
	transport := func(state string) map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
		return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
			path: {
				"org.bluez.MediaTransport1": {
					"UUID":   dbus.MakeVariant(a2dpSinkUUID),
					"Codec":  dbus.MakeVariant(byte(0)),
					"State":  dbus.MakeVariant(state),
					"Volume": dbus.MakeVariant(uint16(64)),
				},
			},
		}
	}
	if volume, ok := transportVolume(transport("pending")); !ok || volume != 64 {
		t.Fatalf("pending volume = %d, ok=%v", volume, ok)
	}
	if key := usableTransportKey(transport("pending")); key == "" {
		t.Fatal("pending transport was not reported as usable")
	}
	if key := usableTransportKey(transport("active")); key == "" {
		t.Fatal("active transport was not reported as usable")
	}
	// An idle transport exists before playback but acquiring it blocks forever.
	if key := usableTransportKey(transport("idle")); key != "" {
		t.Fatalf("idle transport reported usable: %q", key)
	}
	if _, ok := transportVolume(map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		path: {"org.bluez.MediaTransport1": {"State": dbus.MakeVariant("active")}},
	}); ok {
		t.Fatal("transport without Volume reported a level")
	}
}

func TestTransportVolumeSignalRecognition(t *testing.T) {
	volumeChanged := &dbus.Signal{
		Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
		Body: []any{
			"org.bluez.MediaTransport1",
			map[string]dbus.Variant{"Volume": dbus.MakeVariant(uint16(12))},
			[]string{},
		},
	}
	if !transportVolumeSignal(volumeChanged) {
		t.Fatal("transport volume change was not recognized")
	}
	stateChanged := &dbus.Signal{
		Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
		Body: []any{"org.bluez.MediaTransport1", map[string]dbus.Variant{"State": dbus.MakeVariant("active")}, []string{}},
	}
	if transportVolumeSignal(stateChanged) {
		t.Fatal("non-volume property change was treated as a volume change")
	}
	if transportVolumeSignal(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Body: []any{"org.bluez.Adapter1", map[string]dbus.Variant{"Volume": dbus.MakeVariant(uint16(1))}}}) {
		t.Fatal("volume on another interface was treated as transport volume")
	}

	restart := &dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []any{"org.bluez", ":1.1", ":1.2"}}
	if !bluezRestarted(restart) {
		t.Fatal("BlueZ owner change was not recognized")
	}
	if bluezRestarted(&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []any{"org.freedesktop.login1", "", ":1.3"}}) {
		t.Fatal("unrelated name owner change treated as a BlueZ restart")
	}
}

// TestMemoryVolumeEvents pins the volume contract the daemon relies on: a
// framework write never comes back as a source change, while a simulated
// source change is published once.
func TestMemoryVolumeEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemory()
	events := m.Watch(ctx)
	m.SimulateConnect(Device{Address: "AA:BB", Name: "Phone"}, MediaState{})
	select {
	case ev := <-events:
		if ev.Kind != EventConnected {
			t.Fatalf("first event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no connect event")
	}

	if err := m.SetTransportVolume(ctx, 0.5); err != nil {
		t.Fatal(err)
	}
	if level, err := m.TransportVolume(ctx); err != nil || level != 64 {
		t.Fatalf("stored volume = %d, err=%v", level, err)
	}
	select {
	case ev := <-events:
		t.Fatalf("framework volume write was echoed: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	m.SimulateVolume(127)
	select {
	case ev := <-events:
		if ev.Kind != EventVolume || ev.Volume != 127 {
			t.Fatalf("volume event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no volume event for a source change")
	}

	m.SimulateDisconnect("AA:BB")
	select {
	case ev := <-events:
		if ev.Kind != EventDisconnected {
			t.Fatalf("disconnect event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no disconnect event")
	}
	if _, err := m.TransportVolume(ctx); err == nil {
		t.Fatal("volume still available after disconnect")
	}
}

// TestPairingAgentGatesNewBondsOnPairable pins the pairing-mode contract: a new
// bond is accepted only while the adapter advertises pairability, while an
// already-paired source may always authorize its services.
func TestPairingAgentGatesNewBondsOnPairable(t *testing.T) {
	const device = dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	b := &BlueZ{status: Status{Present: true}}
	agent := &pairingAgent{b: b}

	if err := agent.RequestConfirmation(device, 123456); err == nil || err.Name != "org.bluez.Error.Rejected" {
		t.Fatalf("pairing accepted while disabled: %v", err)
	}
	if err := agent.RequestAuthorization(device); err == nil {
		t.Fatal("authorization accepted while disabled")
	}
	if _, err := agent.RequestPinCode(device); err == nil {
		t.Fatal("PIN pairing accepted on a headless device")
	}
	if _, err := agent.RequestPasskey(device); err == nil {
		t.Fatal("passkey pairing accepted on a headless device")
	}

	b.status.Pairable = true
	if err := agent.RequestConfirmation(device, 123456); err != nil {
		t.Fatalf("pairing rejected while pairable: %v", err)
	}
	if err := agent.RequestAuthorization(device); err != nil {
		t.Fatalf("authorization rejected while pairable: %v", err)
	}
	if err := agent.AuthorizeService(device, "0000110b-0000-1000-8000-00805f9b34fb"); err != nil {
		t.Fatalf("service authorization of a bonded device failed: %v", err)
	}

	b.status = Status{Present: false}
	if err := agent.RequestConfirmation(device, 123456); err == nil {
		t.Fatal("pairing accepted without an adapter")
	}
}

// TestVolumeObservationKeepsFirstSourceChange covers the connect edge where no
// transport-volume snapshot exists yet: the first observed level must reach the
// framework, and only the exact level this manager wrote may be swallowed as an
// echo.
func TestVolumeObservationKeepsFirstSourceChange(t *testing.T) {
	// No snapshot and no framework write: the source's first level is adopted.
	if publish, echo := volumeObservation(0, false, false, 40); !publish || echo {
		t.Fatalf("first observation dropped: publish=%v echo=%v", publish, echo)
	}
	// The framework wrote 64 before any snapshot; reading 64 back is our echo.
	if publish, echo := volumeObservation(64, false, true, 64); publish || !echo {
		t.Fatalf("own first write republished: publish=%v echo=%v", publish, echo)
	}
	// The source changed to 50 before we ever snapshotted it.
	if publish, echo := volumeObservation(64, false, true, 50); !publish || echo {
		t.Fatalf("source change before first snapshot dropped: publish=%v echo=%v", publish, echo)
	}
	// Steady state: re-reading the known level publishes nothing.
	if publish, echo := volumeObservation(40, true, false, 40); publish || echo {
		t.Fatalf("unchanged level republished: publish=%v echo=%v", publish, echo)
	}
	// Established source change.
	if publish, _ := volumeObservation(40, true, false, 41); !publish {
		t.Fatal("source change after a snapshot was dropped")
	}
}

// TestMediaFromConnectedSourceWithoutPlayer pins the MediaState contract: a
// connected audio source is present even before BlueZ publishes a player or a
// transport, so pausing (which can remove the player object) does not make the
// source vanish from the framework and the UI. A non-audio device is not media.
func TestMediaFromConnectedSourceWithoutPlayer(t *testing.T) {
	const addr = "AA:BB:CC:DD:EE:FF"
	source := func(uuids []string) map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
		return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
			"/org/bluez/hci0": obj("org.bluez.Adapter1", map[string]dbus.Variant{
				"Powered": dbus.MakeVariant(true), "Pairable": dbus.MakeVariant(true),
			}),
			"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF": obj("org.bluez.Device1", map[string]dbus.Variant{
				"Address": dbus.MakeVariant(addr), "Alias": dbus.MakeVariant("Test phone"),
				"Paired": dbus.MakeVariant(true), "Connected": dbus.MakeVariant(true),
				"UUIDs": dbus.MakeVariant(uuids),
			}),
		}
	}
	state := mediaFrom(source([]string{
		"0000110a-0000-1000-8000-00805f9b34fb",
		"0000110c-0000-1000-8000-00805f9b34fb",
		"0000110e-0000-1000-8000-00805f9b34fb",
	}))
	if !state.Present || state.Playing {
		t.Fatalf("connected paused source = %+v", state)
	}
	if state.DeviceAddress != addr || state.DeviceName != "Test phone" {
		t.Fatalf("device identity = %q/%q", state.DeviceAddress, state.DeviceName)
	}
	if state.Detail == "" {
		t.Fatal("connected source without a player has no detail")
	}

	// A keyboard is connected but is not an audio source.
	keyboard := mediaFrom(source([]string{"00001124-0000-1000-8000-00805f9b34fb"}))
	if keyboard.Present {
		t.Fatalf("keyboard reported as media: %+v", keyboard)
	}
	// A source already reported through a player keeps its metadata path.
	withPlayer := playerObjects(true)
	if state := mediaFrom(withPlayer); !state.Present || state.Title != "Scarlet Fire" {
		t.Fatalf("player state = %+v", state)
	}
}

// TestFindPlayerIsDeterministicWithTwoPausedSources pins source switching: the
// object map has no order, and two connected paused sources must not make the
// framework flap between their device and metadata on every refresh. A player
// that is actually playing still wins.
func TestFindPlayerIsDeterministicWithTwoPausedSources(t *testing.T) {
	player := func(status, device string) map[string]map[string]dbus.Variant {
		return obj("org.bluez.MediaPlayer1", map[string]dbus.Variant{
			"Status": dbus.MakeVariant(status), "Device": dbus.MakeVariant(dbus.ObjectPath(device)),
		})
	}
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci0/dev_AA/player0": player("paused", "/org/bluez/hci0/dev_AA"),
		"/org/bluez/hci0/dev_BB/player0": player("paused", "/org/bluez/hci0/dev_BB"),
	}
	want := dbus.ObjectPath("/org/bluez/hci0/dev_AA/player0")
	for i := range 50 {
		if got, _ := findPlayer(objs); got != want {
			t.Fatalf("attempt %d picked %q, want stable %q", i, got, want)
		}
	}

	objs["/org/bluez/hci0/dev_BB/player0"] = player("playing", "/org/bluez/hci0/dev_BB")
	if got, _ := findPlayer(objs); got != "/org/bluez/hci0/dev_BB/player0" {
		t.Fatalf("playing player not preferred: %q", got)
	}
}

// TestFindPlayerPathTargetsDisplayedSource pins command routing with two paused
// sources: a play command goes to the source the framework reports, not to a
// random map entry.
func TestFindPlayerPathTargetsDisplayedSource(t *testing.T) {
	player := func(status, device string) map[string]map[string]dbus.Variant {
		return obj("org.bluez.MediaPlayer1", map[string]dbus.Variant{
			"Status": dbus.MakeVariant(status), "Device": dbus.MakeVariant(dbus.ObjectPath(device)),
		})
	}
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		"/org/bluez/hci0/dev_AA": obj("org.bluez.Device1", map[string]dbus.Variant{
			"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:01"), "Connected": dbus.MakeVariant(true),
		}),
		"/org/bluez/hci0/dev_BB": obj("org.bluez.Device1", map[string]dbus.Variant{
			"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:02"), "Connected": dbus.MakeVariant(true),
		}),
		"/org/bluez/hci0/dev_AA/player0": player("paused", "/org/bluez/hci0/dev_AA"),
		"/org/bluez/hci0/dev_BB/player0": player("paused", "/org/bluez/hci0/dev_BB"),
	}
	if got := findPlayerPath(objs, "AA:BB:CC:DD:EE:02"); got != "/org/bluez/hci0/dev_BB/player0" {
		t.Fatalf("command went to %q, want the displayed source", got)
	}
	// A playing player wins even when another source is displayed.
	objs["/org/bluez/hci0/dev_AA/player0"] = player("playing", "/org/bluez/hci0/dev_AA")
	if got := findPlayerPath(objs, "AA:BB:CC:DD:EE:02"); got != "/org/bluez/hci0/dev_AA/player0" {
		t.Fatalf("command ignored the playing source: %q", got)
	}
}

// TestMemoryUnpairPublishesDisconnect pins simulator parity with the real
// manager: unpairing a connected source disconnects it and publishes the same
// transition, so consumers do not keep a stale source.
func TestMemoryUnpairPublishesDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemory()
	events := m.Watch(ctx)
	m.SimulateConnect(Device{Address: "AA:BB", Name: "Phone"}, MediaState{Playing: true})
	if ev := <-events; ev.Kind != EventConnected {
		t.Fatalf("first event = %+v", ev)
	}
	if err := m.Unpair(ctx, "AA:BB"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Kind != EventDisconnected || ev.DeviceAddress != "AA:BB" || ev.Media.Present {
			t.Fatalf("unpair event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("unpair published no disconnect")
	}
	if state, _ := m.Media(ctx); state.Present {
		t.Fatalf("media still present after unpair: %+v", state)
	}
	if _, err := m.TransportVolume(ctx); err == nil {
		t.Fatal("transport volume survived unpair")
	}
	if devices, _ := m.PairedDevices(ctx); len(devices) != 0 {
		t.Fatalf("device still paired: %+v", devices)
	}

	// Watching a closed manager ends immediately, like the BlueZ manager.
	m.Close()
	closed := m.Watch(ctx)
	select {
	case _, ok := <-closed:
		if ok {
			t.Fatal("closed manager delivered an event")
		}
	case <-time.After(time.Second):
		t.Fatal("watch on a closed manager did not close")
	}
}

// TestWatchCoalescesToResyncWhenConsumerFallsBehind pins the bounded overflow
// contract: a watcher that stops reading must never be left with a stale
// transition to act on. Once the queue overflows, the retained window is
// resync markers carrying the newest media snapshots, so the consumer
// reconciles from the manager instead of trusting the queue contents.
func TestWatchCoalescesToResyncWhenConsumerFallsBehind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewMemory()
	events := m.Watch(ctx)

	const flood = 40
	m.SimulateConnect(Device{Address: "AA:BB", Name: "Phone"}, MediaState{Present: true, Playing: true, Title: "first"})
	for i := range flood {
		m.SetMedia(MediaState{
			Present: true, Playing: true, DeviceAddress: "AA:BB",
			Title: fmt.Sprintf("track-%02d", i),
		})
	}
	m.SimulateDisconnect("AA:BB")

	// Drain what the queue retained without blocking; nothing else publishes.
	var got []Event
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
			continue
		default:
		}
		break
	}

	if len(got) != watchEventBuffer {
		t.Fatalf("drained %d events, want the bounded window of %d", len(got), watchEventBuffer)
	}
	last := got[len(got)-1]
	if last.Kind != EventResync || last.DeviceAddress != "AA:BB" || last.Media.Present {
		t.Fatalf("queue tail does not describe the disconnect: %+v", last)
	}
	// Markers are the newest contiguous tail: revisions only grow, and no stale
	// connect/disconnect transition survived the overflow.
	newest := fmt.Sprintf("track-%02d", flood-1)
	found := false
	for i, ev := range got {
		if i > 0 && ev.Media.Revision <= got[i-1].Media.Revision {
			t.Fatalf("markers out of order at %d: %d then %d", i, got[i-1].Media.Revision, ev.Media.Revision)
		}
		if ev.Kind != EventResync {
			t.Fatalf("a stale transition survived the overflow: %+v", ev)
		}
		if ev.Media.Title == newest {
			found = true
		}
	}
	if !found {
		t.Fatalf("newest media state %q was not retained", newest)
	}
}
