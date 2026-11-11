package bluetooth

import (
	"context"
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
	if err := m.SetAbsoluteVolume(ctx, 0.5); err != nil {
		t.Fatal(err)
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
