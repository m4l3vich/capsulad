package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	const address = "AA:BB:CC:DD:EE:01"
	device := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	path := device + "/sep2/fd0"
	objects := func(state string, volume bool) map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
		props := map[string]dbus.Variant{
			"UUID":   dbus.MakeVariant(a2dpSinkUUID),
			"Codec":  dbus.MakeVariant(byte(0)),
			"State":  dbus.MakeVariant(state),
			"Device": dbus.MakeVariant(device),
		}
		if volume {
			props["Volume"] = dbus.MakeVariant(uint16(64))
		}
		return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
			device: obj("org.bluez.Device1", map[string]dbus.Variant{
				"Address": dbus.MakeVariant(address), "Alias": dbus.MakeVariant("Phone"),
				"Paired": dbus.MakeVariant(true), "Connected": dbus.MakeVariant(true),
			}),
			path: {"org.bluez.MediaTransport1": props},
		}
	}
	if volume, ok := transportVolume(objects("pending", true), address); !ok || volume != 64 {
		t.Fatalf("pending volume = %d, ok=%v", volume, ok)
	}
	if volume, ok := transportVolume(objects("pending", true), "AA:BB:CC:DD:EE:FF"); ok || volume != 0 {
		t.Fatalf("foreign source volume = %d, ok=%v; want no transport", volume, ok)
	}
	if key := usableTransportKey(objects("pending", true)); key == "" {
		t.Fatal("pending transport was not reported as usable")
	}
	if key := usableTransportKey(objects("active", true)); key == "" {
		t.Fatal("active transport was not reported as usable")
	}
	// An idle transport exists before playback but acquiring it blocks forever.
	if key := usableTransportKey(objects("idle", true)); key != "" {
		t.Fatalf("idle transport reported usable: %q", key)
	}
	if _, ok := transportVolume(map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		path: {"org.bluez.MediaTransport1": {"State": dbus.MakeVariant("active")}},
	}, address); ok {
		t.Fatal("transport without Volume reported a level")
	}
}

// TestVolumeTransportTargetsDisplayedSource pins the two-source rule: an
// active transport from a background phone must not win over the displayed
// source's pending/idle transport.
func TestVolumeTransportTargetsDisplayedSource(t *testing.T) {
	displayed := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	foreign := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_02")
	displayedTransport := displayed + "/sep2/fd0"
	foreignTransport := foreign + "/sep2/fd0"
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		displayed: obj("org.bluez.Device1", map[string]dbus.Variant{
			"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:01"), "Alias": dbus.MakeVariant("Displayed"),
			"Paired": dbus.MakeVariant(true), "Connected": dbus.MakeVariant(true),
		}),
		foreign: obj("org.bluez.Device1", map[string]dbus.Variant{
			"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:02"), "Alias": dbus.MakeVariant("Background"),
			"Paired": dbus.MakeVariant(true), "Connected": dbus.MakeVariant(true),
		}),
		displayedTransport: obj("org.bluez.MediaTransport1", map[string]dbus.Variant{
			"Device": dbus.MakeVariant(displayed), "State": dbus.MakeVariant("pending"), "Volume": dbus.MakeVariant(uint16(40)),
		}),
		foreignTransport: obj("org.bluez.MediaTransport1", map[string]dbus.Variant{
			"Device": dbus.MakeVariant(foreign), "State": dbus.MakeVariant("active"), "Volume": dbus.MakeVariant(uint16(100)),
		}),
	}
	path, props := findVolumeTransport(objs, "AA:BB:CC:DD:EE:01")
	if path != displayedTransport || uint16PropOut(props) != 40 {
		t.Fatalf("displayed volume transport = %q volume=%d", path, uint16PropOut(props))
	}
	if volume, ok := transportVolume(objs, "AA:BB:CC:DD:EE:01"); !ok || volume != 40 {
		t.Fatalf("displayed volume = %d ok=%v", volume, ok)
	}
	if volume, ok := transportVolume(objs, "AA:BB:CC:DD:EE:02"); !ok || volume != 100 {
		t.Fatalf("background volume = %d ok=%v", volume, ok)
	}
	if path, _ := findVolumeTransport(objs, ""); path != "" {
		t.Fatalf("empty device selected %q", path)
	}
}

func uint16PropOut(props map[string]dbus.Variant) uint16 {
	switch v := props["Volume"].Value().(type) {
	case uint16:
		return v
	case uint32:
		return uint16(v)
	}
	return 0
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

// TestMemoryIncomingPolicy pins the two-control contract shared by the
// simulator and the daemon tests: incoming off keeps the radio powered, denies
// paired and new inbound links, and disables discoverability; discoverability
// implies incoming; hidden with incoming on still allows paired reconnects.
func TestMemoryIncomingPolicy(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	// Paired devices reconnect while hidden.
	paired := Device{Address: "AA:BB:CC:DD:EE:01", Name: "Phone", Paired: true}
	if err := m.SimulateIncomingConnection(paired, MediaState{}); err != nil {
		t.Fatalf("paired reconnect while hidden: %v", err)
	}
	m.SimulateDisconnect(paired.Address)

	// A new device needs the discoverability window.
	newDevice := Device{Address: "AA:BB:CC:DD:EE:02", Name: "New phone"}
	if err := m.SimulateIncomingConnection(newDevice, MediaState{}); err != ErrPairingDisabled {
		t.Fatalf("new device while hidden: err=%v, want ErrPairingDisabled", err)
	}
	if err := m.SetDiscoverable(ctx, true); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Status(ctx)
	if !status.IncomingConnections || !status.Discoverable {
		t.Fatalf("discoverability did not imply incoming: %+v", status)
	}
	if err := m.SimulateIncomingConnection(newDevice, MediaState{}); err != nil {
		t.Fatalf("new device while discoverable: %v", err)
	}
	m.SimulateDisconnect(newDevice.Address)

	// Incoming off denies even the bonded device and clears discoverability,
	// while the adapter stays powered.
	if err := m.SetIncomingConnections(ctx, false); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status(ctx)
	if !status.Powered || status.IncomingConnections || status.Discoverable {
		t.Fatalf("incoming off status = %+v; want powered, hidden and non-connectable", status)
	}
	if err := m.SimulateIncomingConnection(paired, MediaState{}); err != ErrIncomingDisabled {
		t.Fatalf("paired inbound while incoming off: err=%v, want ErrIncomingDisabled", err)
	}

	// Enabling incoming alone does not re-open the discovery window.
	if err := m.SetIncomingConnections(ctx, true); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Status(ctx)
	if !status.IncomingConnections || status.Discoverable {
		t.Fatalf("incoming on status = %+v; want connectable and hidden", status)
	}
}

func TestMgmtCommandCodec(t *testing.T) {
	packet := encodeMgmtCommand(mgmtOpSetConnectable, 0, mgmtSetConnectableParams(false))
	if len(packet) != 7 || packet[0] != 0x07 || packet[1] != 0x00 || packet[2] != 0x00 ||
		packet[3] != 0x00 || packet[4] != 0x01 || packet[5] != 0x00 || packet[6] != 0x00 {
		t.Fatalf("set connectable packet = %v", packet)
	}
	if got := mgmtSetConnectableParams(true); len(got) != 1 || got[0] != 1 {
		t.Fatalf("connectable on params = %v", got)
	}
	if got := mgmtRemoveAllDevicesParams(); len(got) != 7 {
		t.Fatalf("remove-all params = %v", got)
	} else {
		for _, b := range got {
			if b != 0 {
				t.Fatalf("remove-all params = %v, want an all-zero address and BR/EDR type", got)
			}
		}
	}

	// Command Complete: event 0x0001, index, len, opcode, status, current
	// settings. Connectable is bit 1.
	reply := []byte{0x01, 0x00, 0x00, 0x00, 0x07, 0x00, 0x07, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	event, ok := decodeMgmtEvent(reply)
	if !ok {
		t.Fatal("command complete did not decode")
	}
	result, ok := parseMgmtCommandResult(event, mgmtOpSetConnectable)
	if !ok || result.Status != mgmtStatusSuccess {
		t.Fatalf("result = %+v ok=%v", result, ok)
	}
	if len(result.Data) != 4 || result.Data[0]&0x02 == 0 {
		t.Fatalf("set-connectable current settings = %v", result.Data)
	}
	// Read Controller Information carries settings after address, version,
	// manufacturer and supported settings.
	controllerInfo := make([]byte, 17)
	controllerInfo[13] = 0x02
	if on, ok := parseConnectableSettings(controllerInfo); !ok || !on {
		t.Fatalf("connectable settings = %v ok=%v", on, ok)
	}
	if on, ok := parseConnectableSettings([]byte{0, 0, 0, 0}); ok || on {
		t.Fatal("short controller info reply reported a setting")
	}

	// Command Status only carries the opcode and status.
	statusReply := []byte{0x02, 0x00, 0x00, 0x00, 0x03, 0x00, 0x07, 0x00, 0x0c}
	event, ok = decodeMgmtEvent(statusReply)
	if !ok {
		t.Fatal("command status did not decode")
	}
	result, ok = parseMgmtCommandResult(event, mgmtOpSetConnectable)
	if !ok || result.Status != 0x0c || result.Data != nil {
		t.Fatalf("status result = %+v ok=%v", result, ok)
	}
	if idx, err := controllerIndex("hci0"); err != nil || idx != 0 {
		t.Fatalf("hci0 index = %d err=%v", idx, err)
	}
	if _, err := controllerIndex("usb0"); err == nil {
		t.Fatal("non-hci adapter id accepted")
	}
}

// TestMemoryWatcherClosePublishConcurrent pins the subscription ownership
// rule: closes and publishes share the manager mutex, so a watcher cancelled
// while a source update is being published cannot panic on a closed channel.
func TestMemoryWatcherClosePublishConcurrent(t *testing.T) {
	m := NewMemory()
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		ch := m.Watch(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := range 20 {
				m.SimulateVolume(uint16(i))
			}
		}()
		cancel()
		for range ch {
		}
		<-done
	}
}

// TestBlueZAdapterPublisherCloseConcurrent covers the real manager's event
// publisher, not only the simulator.
func TestBlueZAdapterPublisherCloseConcurrent(t *testing.T) {
	b := &BlueZ{subs: map[int]chan Event{}}
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		ch := b.Watch(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 20 {
				b.publishAdapterEvent()
			}
		}()
		cancel()
		for range ch {
		}
		<-done
	}
}

// TestApplyConnectablePolicyOrder pins the deny ordering: connectable off is
// written before the action list is cleared, and the clear is skipped when
// incoming is enabled.
func TestApplyConnectablePolicyOrder(t *testing.T) {
	oldSet, oldClear := adapterSetConnectable, adapterClearAcceptList
	t.Cleanup(func() { adapterSetConnectable, adapterClearAcceptList = oldSet, oldClear })
	var calls []string
	adapterSetConnectable = func(_ context.Context, adapter string, on bool) error {
		calls = append(calls, fmt.Sprintf("set:%v:%s", on, adapter))
		return nil
	}
	adapterClearAcceptList = func(_ context.Context, adapter string) error {
		calls = append(calls, "clear:"+adapter)
		return nil
	}

	b := &BlueZ{adapter: "/org/bluez/hci0", incomingConfigured: true, incomingEnabled: false}
	if err := b.applyConnectablePolicyLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "set:false:hci0" || calls[1] != "clear:hci0" {
		t.Fatalf("incoming off calls = %v; want connectable off then action-list clear", calls)
	}
	calls = nil
	b.incomingEnabled = true
	if err := b.applyConnectablePolicyLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "set:true:hci0" {
		t.Fatalf("incoming on calls = %v; want only connectable on", calls)
	}
	calls = nil
	b.incomingConfigured = false
	if err := b.applyConnectablePolicyLocked(context.Background()); err != nil || len(calls) != 0 {
		t.Fatalf("unconfigured policy touched the adapter: calls=%v err=%v", calls, err)
	}

	// A kernel/permission failure must surface instead of claiming the deny.
	b.incomingConfigured, b.incomingEnabled = true, false
	wantErr := errors.New("management unavailable")
	adapterSetConnectable = func(_ context.Context, _ string, _ bool) error { return wantErr }
	if err := b.applyConnectablePolicyLocked(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("connectable failure = %v, want the backend error", err)
	}
	adapterSetConnectable = func(_ context.Context, _ string, _ bool) error { return nil }
	adapterClearAcceptList = func(_ context.Context, _ string) error { return wantErr }
	if err := b.applyConnectablePolicyLocked(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("action-list clear failure = %v, want the backend error", err)
	}
}

func TestAdapterDisplayName(t *testing.T) {
	for _, tc := range []struct {
		props map[string]dbus.Variant
		want  string
	}{
		{map[string]dbus.Variant{"Alias": dbus.MakeVariant("Capsula"), "Name": dbus.MakeVariant("linux")}, "Capsula"},
		{map[string]dbus.Variant{"Alias": dbus.MakeVariant(""), "Name": dbus.MakeVariant("linux")}, "linux"},
		{map[string]dbus.Variant{}, "hci0"},
	} {
		if got := adapterDisplayName(tc.props, "hci0"); got != tc.want {
			t.Fatalf("adapterDisplayName(%v) = %q, want %q", tc.props, got, tc.want)
		}
	}
}

// TestPairingAgentGatesNewBondsOnDiscoverable pins the pairing-window
// contract: a new bond is accepted only while the adapter accepts incoming
// connections and is discoverable, while an already-paired source may always
// authorize its services.
func TestPairingAgentGatesNewBondsOnDiscoverable(t *testing.T) {
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

	// Paired reconnects alone do not open the pairing window.
	b.status.IncomingConnections = true
	if err := agent.RequestConfirmation(device, 123456); err == nil {
		t.Fatal("pairing accepted while hidden")
	}
	b.status.Discoverable = true
	if err := agent.RequestConfirmation(device, 123456); err != nil {
		t.Fatalf("pairing rejected while discoverable: %v", err)
	}
	if err := agent.RequestAuthorization(device); err != nil {
		t.Fatalf("authorization rejected while discoverable: %v", err)
	}
	if err := agent.AuthorizeService(device, "0000110b-0000-1000-8000-00805f9b34fb"); err != nil {
		t.Fatalf("service authorization of a bonded device failed: %v", err)
	}

	// Incoming off dominates discoverability even if a stale property claims it.
	b.status.IncomingConnections = false
	if err := agent.RequestConfirmation(device, 123456); err == nil {
		t.Fatal("pairing accepted while incoming connections were off")
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
	// A player that is playing on another device must not capture the command:
	// the framework shows one source and that source receives the transport
	// action, so a second connected device cannot answer the user's skip.
	objs["/org/bluez/hci0/dev_AA/player0"] = player("playing", "/org/bluez/hci0/dev_AA")
	if got := findPlayerPath(objs, "AA:BB:CC:DD:EE:02"); got != "/org/bluez/hci0/dev_BB/player0" {
		t.Fatalf("command crossed to another device: %q", got)
	}
	// Within the displayed device, its playing player still wins.
	objs["/org/bluez/hci0/dev_BB/player1"] = player("playing", "/org/bluez/hci0/dev_BB")
	if got := findPlayerPath(objs, "AA:BB:CC:DD:EE:02"); got != "/org/bluez/hci0/dev_BB/player1" {
		t.Fatalf("playing player of the displayed source not preferred: %q", got)
	}
	// A displayed source with no player is never replaced by another device's
	// player; the caller falls back to that source's MediaControl1.
	if got := findPlayerPath(objs, "AA:BB:CC:DD:EE:03"); got != "" {
		t.Fatalf("player of an unrelated device selected: %q", got)
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

// controlSource builds the object shape the stock unit actually exposes: a
// connected device with the deprecated MediaControl1 interface and no
// MediaPlayer1 when the native sink owns the acquired transport.
func controlSource(addr, transportState string) map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_" + strings.ReplaceAll(addr, ":", "_"))
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		dev: {
			"org.bluez.Device1": map[string]dbus.Variant{
				"Address": dbus.MakeVariant(addr), "Alias": dbus.MakeVariant("Phone"),
				"Connected": dbus.MakeVariant(true),
			},
			"org.bluez.MediaControl1": map[string]dbus.Variant{"Connected": dbus.MakeVariant(true)},
		},
	}
	if transportState != "" {
		objs[dev+"/sep1/fd0"] = map[string]map[string]dbus.Variant{
			"org.bluez.MediaTransport1": {
				"State":  dbus.MakeVariant(transportState),
				"Device": dbus.MakeVariant(dev),
			},
		}
	}
	return objs
}

func mediaTargetFor(t *testing.T, b *BlueZ, objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string, cmd Command) mediaTarget {
	t.Helper()
	target, ok := b.mediaTarget(objs, device, cmd)
	if !ok {
		t.Fatalf("no media target for %v device %q", cmd, device)
	}
	return target
}

// A source that exposes MediaControl1 but no MediaPlayer1 must still receive
// transport commands, and an ambiguous toggle direction must come from the
// source's public transport state: an active A2DP transport is playing (Pause),
// an idle transport is paused (Play).
func TestControlOnlySourceToggleUsesTransportState(t *testing.T) {
	const addr = "BC:D0:74:32:51:DC"
	b := &BlueZ{}
	active := controlSource(addr, "active")
	if target := mediaTargetFor(t, b, active, addr, CommandToggle); target.method != "Pause" || target.iface != "org.bluez.MediaControl1" {
		t.Fatalf("active transport toggle = %+v, want MediaControl1.Pause", target)
	}
	if target := mediaTargetFor(t, b, active, addr, CommandNext); target.method != "Next" {
		t.Fatalf("next = %+v", target)
	}
	if target := mediaTargetFor(t, b, active, addr, CommandPrevious); target.method != "Previous" {
		t.Fatalf("previous = %+v", target)
	}
	idle := controlSource(addr, "idle")
	if target := mediaTargetFor(t, b, idle, addr, CommandToggle); target.method != "Play" {
		t.Fatalf("idle transport toggle = %+v, want Play", target)
	}
}

// When BlueZ publishes neither a player status nor a transport (the acquired
// transport is absent from the object tree), the sink's own receipt evidence
// decides the toggle. It is read-only and must be asked for the same device.
func TestControlOnlySourceToggleUsesSinkReceiptEvidence(t *testing.T) {
	const addr = "BC:D0:74:32:51:DC"
	var asked []string
	b := &BlueZ{}
	b.SetAudioStreamProbe(func(a string) bool {
		asked = append(asked, a)
		return strings.EqualFold(a, addr)
	})
	if target := mediaTargetFor(t, b, controlSource(addr, ""), addr, CommandToggle); target.method != "Pause" {
		t.Fatalf("receiving source toggle = %+v, want Pause", target)
	}
	b.SetAudioStreamProbe(func(a string) bool {
		asked = append(asked, a)
		return !strings.EqualFold(a, addr)
	})
	if target := mediaTargetFor(t, b, controlSource(addr, ""), addr, CommandToggle); target.method != "Play" {
		t.Fatalf("silent source toggle = %+v, want Play", target)
	}
	for _, got := range asked {
		if !strings.EqualFold(got, addr) {
			t.Fatalf("probe asked for %q, want the displayed device", got)
		}
	}
}

// A published MediaPlayer1.Status is authoritative: the receipt probe must not
// turn a paused player into Pause or a playing player into Play.
func TestPlayerStatusWinsOverSinkEvidence(t *testing.T) {
	const addr = "AA:BB:CC:DD:EE:FF"
	b := &BlueZ{}
	b.SetAudioStreamProbe(func(string) bool { return true })
	objs := playerObjects(false) // Status=paused
	if target := mediaTargetFor(t, b, objs, addr, CommandToggle); target.method != "Play" {
		t.Fatalf("paused player toggle = %+v, want Play", target)
	}
	objs = playerObjects(true) // Status=playing
	b.SetAudioStreamProbe(func(string) bool { return false })
	if target := mediaTargetFor(t, b, objs, addr, CommandToggle); target.method != "Pause" {
		t.Fatalf("playing player toggle = %+v, want Pause", target)
	}
}

// A player with no Status property (macOS leaves it out at rest) must not be
// assumed paused: the sink receipt evidence decides.
func TestPlayerWithoutStatusUsesSinkEvidence(t *testing.T) {
	const addr = "AA:BB:CC:DD:EE:FF"
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		dev: obj("org.bluez.Device1", map[string]dbus.Variant{
			"Address": dbus.MakeVariant(addr), "Alias": dbus.MakeVariant("MacBook"),
			"Connected": dbus.MakeVariant(true),
		}),
		dev + "/player0": obj("org.bluez.MediaPlayer1", map[string]dbus.Variant{
			"Device": dbus.MakeVariant(dev), "Position": dbus.MakeVariant(uint32(1200)),
		}),
	}
	b := &BlueZ{}
	b.SetAudioStreamProbe(func(string) bool { return true })
	if target := mediaTargetFor(t, b, objs, addr, CommandToggle); target.method != "Pause" || target.iface != "org.bluez.MediaPlayer1" {
		t.Fatalf("stat-less playing player toggle = %+v, want MediaPlayer1.Pause", target)
	}
	b.SetAudioStreamProbe(func(string) bool { return false })
	if target := mediaTargetFor(t, b, objs, addr, CommandToggle); target.method != "Play" {
		t.Fatalf("stat-less silent player toggle = %+v, want Play", target)
	}
}

// Two connected sources must not capture each other's commands. The displayed
// source is the only target; MediaControl1 is scoped the same way.
func TestMediaTargetNeverCrossesDevices(t *testing.T) {
	const shown, other = "AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"
	shownObjs := controlSource(shown, "idle")
	otherDev := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_02")
	for path, ifaces := range controlSource(other, "") {
		shownObjs[path] = ifaces
	}
	shownObjs[otherDev+"/player0"] = obj("org.bluez.MediaPlayer1", map[string]dbus.Variant{
		"Status": dbus.MakeVariant("playing"), "Device": dbus.MakeVariant(otherDev),
	})
	b := &BlueZ{}
	target := mediaTargetFor(t, b, shownObjs, shown, CommandNext)
	shownDev := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	if target.path != shownDev || target.iface != "org.bluez.MediaControl1" {
		t.Fatalf("command crossed devices: %+v", target)
	}

	// A displayed source with neither player nor connected control reports
	// unsupported instead of silently commanding another device.
	only := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}
	for path, ifaces := range controlSource(other, "") {
		only[path] = ifaces
	}
	if target, ok := b.mediaTarget(only, shown, CommandNext); ok {
		t.Fatalf("unsupported command targeted another device: %+v", target)
	}
}

// MediaControl1 is exported on the device object; the advertised source must be
// that object's device, not whichever connected device a map iteration finds.
func TestMediaFromControlUsesControlObjectDevice(t *testing.T) {
	objs := controlSource("AA:BB:CC:DD:EE:01", "")
	second := controlSource("AA:BB:CC:DD:EE:02", "")
	for path, ifaces := range second {
		objs[path] = ifaces
	}
	for i := 0; i < 50; i++ {
		state := mediaFrom(objs)
		if state.DeviceAddress != "AA:BB:CC:DD:EE:01" {
			t.Fatalf("attempt %d advertised %q, want the deterministic first source", i, state.DeviceAddress)
		}
		path, _ := findControlPath(objs, "AA:BB:CC:DD:EE:02")
		if want := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_02"); path != want {
			t.Fatalf("attempt %d control path = %q, want %q", i, path, want)
		}
	}
}
