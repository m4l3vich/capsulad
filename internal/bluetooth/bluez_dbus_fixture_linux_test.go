//go:build linux && bluezfixture

// This fixture runs the real Linux D-Bus adapter against a fake org.bluez. It
// is excluded from normal test runs because it needs dbus-daemon and owns
// org.bluez on a private bus:
//
//	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -tags bluezfixture \
//	    -o /tmp/bluez-verify/bluetooth.test ./internal/bluetooth
//	docker run --rm -v /tmp/bluez-verify:/v:ro debian:latest sh -c '
//	    apt-get -qq update && apt-get -qq install -y dbus &&
//	    /v/bluetooth.test -test.run TestBlueZDbusMediaControlFixture -test.v'
//
// It proves the wire target (interface, method and object path) for a source
// that exposes only the deprecated MediaControl1, that an ambiguous toggle
// direction comes from the sink's receipt evidence, and that another device's
// active transport never changes the displayed source's direction.
package bluetooth

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const fixtureDevice = dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")

type fixtureObjects struct {
	mu   sync.Mutex
	objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
}

func (f *fixtureObjects) set(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) {
	f.mu.Lock()
	f.objs = objs
	f.mu.Unlock()
}

func (f *fixtureObjects) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs, nil
}

type fixtureMediaControl struct {
	mu    sync.Mutex
	calls []string
}

func (f *fixtureMediaControl) record(path, method string) *dbus.Error {
	f.mu.Lock()
	f.calls = append(f.calls, path+"."+method)
	f.mu.Unlock()
	return nil
}

// exportedControl is one object exported at a device path; godbus passes no
// object path to the method, so each device gets its own bound value.
type exportedControl struct {
	ctl  *fixtureMediaControl
	path dbus.ObjectPath
}

func (e *exportedControl) Play() *dbus.Error     { return e.ctl.record(string(e.path), "Play") }
func (e *exportedControl) Pause() *dbus.Error    { return e.ctl.record(string(e.path), "Pause") }
func (e *exportedControl) Next() *dbus.Error     { return e.ctl.record(string(e.path), "Next") }
func (e *exportedControl) Previous() *dbus.Error { return e.ctl.record(string(e.path), "Previous") }

func (f *fixtureMediaControl) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func fixtureControlSource(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, path dbus.ObjectPath, address string) {
	objs[path] = map[string]map[string]dbus.Variant{
		"org.bluez.Device1": {
			"Address": dbus.MakeVariant(address), "Alias": dbus.MakeVariant("Fixture source"),
			"Connected": dbus.MakeVariant(true), "Paired": dbus.MakeVariant(true),
		},
		"org.bluez.MediaControl1": {"Connected": dbus.MakeVariant(true)},
	}
}

func TestBlueZDbusMediaControlFixture(t *testing.T) {
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon is not installed")
	}
	// The descriptor must be given explicitly: dbus-daemon 1.12 treats the token
	// after a bare --print-address as that flag's optional descriptor, so
	// "--print-address --print-pid" fails with "Invalid file descriptor".
	out, err := exec.Command("dbus-daemon", "--session", "--fork", "--print-address=1", "--print-pid=1").Output()
	if err != nil {
		t.Fatalf("start dbus-daemon: %v", err)
	}
	lines := strings.Fields(string(out))
	if len(lines) < 2 {
		t.Fatalf("dbus-daemon returned %q, want address and pid", out)
	}
	pid, err := strconv.Atoi(lines[1])
	if err != nil {
		t.Fatalf("dbus-daemon pid %q: %v", lines[1], err)
	}
	t.Cleanup(func() {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	})
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", lines[0])
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", lines[0])

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatalf("session bus: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	reply, err := conn.RequestName("org.bluez", dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own org.bluez: reply=%v err=%v", reply, err)
	}

	adapter := dbus.ObjectPath("/org/bluez/hci0")
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		adapter: {"org.bluez.Adapter1": {
			"Address": dbus.MakeVariant("8A:7C:50:49:22:22"), "Powered": dbus.MakeVariant(true),
			"Discoverable": dbus.MakeVariant(false), "Pairable": dbus.MakeVariant(true),
		}},
	}
	fixtureControlSource(objs, fixtureDevice, "AA:BB:CC:DD:EE:FF")
	objects := &fixtureObjects{objs: objs}
	if err := conn.Export(objects, "/", "org.freedesktop.DBus.ObjectManager"); err != nil {
		t.Fatalf("export object manager: %v", err)
	}
	control := &fixtureMediaControl{}
	if err := conn.Export(&exportedControl{ctl: control, path: fixtureDevice}, fixtureDevice, "org.bluez.MediaControl1"); err != nil {
		t.Fatalf("export media control: %v", err)
	}

	b := &BlueZ{conn: conn, connected: map[string]string{}, subs: map[int]chan Event{}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The source exposes no MediaPlayer1.Status. Receipt evidence says it is
	// playing, so a toggle must reach the device object's MediaControl1.Pause.
	b.SetAudioStreamProbe(func(address string) bool {
		return strings.EqualFold(address, "AA:BB:CC:DD:EE:FF")
	})
	if err := b.Command(ctx, CommandToggle); err != nil {
		t.Fatalf("toggle with receipt evidence: %v", err)
	}
	if got := control.snapshot(); len(got) != 1 || got[0] != string(fixtureDevice)+".Pause" {
		t.Fatalf("toggle calls = %v, want [%s.Pause]", got, fixtureDevice)
	}

	// No receipt evidence: the same toggle must send Play.
	b.SetAudioStreamProbe(func(string) bool { return false })
	if err := b.Command(ctx, CommandToggle); err != nil {
		t.Fatalf("toggle without evidence: %v", err)
	}
	if got := control.snapshot(); len(got) != 2 || got[1] != string(fixtureDevice)+".Play" {
		t.Fatalf("toggle calls = %v, want a second call Play", got)
	}

	if err := b.Command(ctx, CommandNext); err != nil {
		t.Fatalf("next: %v", err)
	}
	if err := b.Command(ctx, CommandPrevious); err != nil {
		t.Fatalf("previous: %v", err)
	}
	if got := control.snapshot(); len(got) != 4 ||
		got[2] != string(fixtureDevice)+".Next" || got[3] != string(fixtureDevice)+".Previous" {
		t.Fatalf("transport calls = %v", got)
	}

	// A second connected device with an active transport must not flip the
	// displayed source's direction: the displayed device is still the first
	// deterministic MediaControl1 source.
	other := dbus.ObjectPath("/org/bluez/hci0/dev_BB_CC_DD_EE_FF_00")
	updated := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}
	for path, ifaces := range objs {
		updated[path] = ifaces
	}
	fixtureControlSource(updated, other, "BB:CC:DD:EE:FF:00")
	updated[other+"/sep1/fd0"] = map[string]map[string]dbus.Variant{
		"org.bluez.MediaTransport1": {
			"State": dbus.MakeVariant("active"), "Device": dbus.MakeVariant(other),
		},
	}
	objects.set(updated)
	if err := b.Command(ctx, CommandToggle); err != nil {
		t.Fatalf("toggle with another active transport: %v", err)
	}
	if got := control.snapshot(); len(got) != 5 || got[4] != string(fixtureDevice)+".Play" {
		t.Fatalf("cross-device transport changed direction: %v", got)
	}
}
