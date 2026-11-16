package daemon

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"capsulad/internal/bluetooth"
)

// The unit's volume contract, all of it exercised through the simulator:
//
//	trigger     any master change (dial, keys, Web UI, an app) or a new source
//	effect      the master is written to the source's AVRCP register so the
//	            source's own level display follows the speaker
//	gain        local only — the write must not change how loud the stream is
//	echo        the source's report of that write is not a slider move and is
//	            dropped; a report outside it still drives the master
//	no register a source without a writable register keeps working silently

const volumeTestAddress = "AA:BB:CC:DD:EE:FF"

// volumeTestDaemon returns the running daemon with its simulator manager and a
// connected source. Native mode is on so the local gain path never reaches a
// host PulseAudio; the simulators do not implement the A2DP sink, so the stream
// itself stays absent and the trim is a no-op — which is exactly the point: only
// the mirror write is under test.
func volumeTestDaemon(t *testing.T) (*Daemon, *bluetooth.Memory) {
	t.Helper()
	d, cancel, _ := newTestDaemon(t)
	t.Cleanup(cancel)
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	d.setBluetoothNative(true)
	waitFor(t, 3*time.Second, "bluetooth watcher", func() bool { return mem.SubscriberCount() > 0 })
	mem.SimulateConnect(bluetooth.Device{Address: volumeTestAddress, Name: "Phone"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: volumeTestAddress, DeviceName: "Phone"})
	waitFor(t, 3*time.Second, "source connected", func() bool {
		state, err := mem.Media(context.Background())
		return err == nil && state.Present
	})
	return d, mem
}

func volumeTestGain(t *testing.T, d *Daemon) float64 {
	t.Helper()
	d.btMu.Lock()
	defer d.btMu.Unlock()
	return d.btGain
}

func volumeTestMaster(d *Daemon) float64 {
	return d.user.Get().Values.MasterVolume
}

func waitVolumeWritten(t *testing.T, mem *bluetooth.Memory, want uint16) {
	t.Helper()
	waitFor(t, 3*time.Second, fmt.Sprintf("the source register to reach %d/127", want), func() bool {
		level, ok := mem.TransportVolumeWritten()
		return ok && level == want
	})
}

// TestBluetoothVolumeMirrorsMasterToTheSource pins the display direction: a
// speaker-side change reaches the source's register, and the local gain stays
// exactly the level the user asked for. There is no compensation factor and no
// second gain stage: the write must not change how loud the stream is.
func TestBluetoothVolumeMirrorsMasterToTheSource(t *testing.T) {
	d, mem := volumeTestDaemon(t)

	const level = 0.25
	if err := d.applyMasterVolume(level); err != nil {
		t.Fatal(err)
	}
	waitVolumeWritten(t, mem, bluetoothVolumeStep(level))
	if have := volumeTestGain(t, d); have != level {
		t.Fatalf("local gain = %v, want the requested %v", have, level)
	}
	waitFor(t, 3*time.Second, "the mirror to leave the master alone", func() bool {
		return math.Abs(volumeTestMaster(d)-level) < 0.001
	})
}

// TestBluetoothExactEchoIsSuppressed pins the narrow half of the filter: the
// source reports exactly the step written, or within one step of it because it
// rounds to its own grid, and that report is our echo — adopting it would move
// the master to the peer's neighbour.
func TestBluetoothExactEchoIsSuppressed(t *testing.T) {
	d, mem := volumeTestDaemon(t)

	const level = 0.4
	if err := d.applyMasterVolume(level); err != nil {
		t.Fatal(err)
	}
	step := bluetoothVolumeStep(level)
	waitVolumeWritten(t, mem, step)

	for _, report := range []uint16{step, step - 1, step + 1} {
		mem.SimulateVolume(report)
	}
	// An ignored report produces no event of its own, so the absence is checked
	// over a bounded window rather than by polling for a state change. It stays
	// inside the echo window: a gesture must not have to wait for it.
	select {
	case <-time.After(300 * time.Millisecond):
	}
	if have := volumeTestMaster(d); math.Abs(have-level) > 0.001 {
		t.Fatalf("the source's echo moved the master to %v, want %v", have, level)
	}
}

// TestBluetoothNeighbouringReportIsAdoptedAtOnce is the other half: two steps away
// is outside the rounding allowance, so it is the user's slider and the speaker
// follows it without waiting for the echo window to expire.
func TestBluetoothNeighbouringReportIsAdoptedAtOnce(t *testing.T) {
	d, mem := volumeTestDaemon(t)

	const level = 0.4
	if err := d.applyMasterVolume(level); err != nil {
		t.Fatal(err)
	}
	step := bluetoothVolumeStep(level)
	waitVolumeWritten(t, mem, step)

	mem.SimulateVolume(step - 2)
	want := float64(step-2) / 127
	waitFor(t, 2*time.Second, "master follows a report two steps away", func() bool {
		return math.Abs(volumeTestMaster(d)-want) < 0.002
	})
}

// TestBluetoothSliderDragIsAdoptedFromItsFirstNonEcho pins the regression the
// field report named: a phone slider drag is a run of reports, and a wide filter
// swallowed the whole run so the speaker lagged behind the phone. Every report
// outside the rounding allowance is adopted as it arrives, so the master reaches
// the newest level while the drag is still going.
func TestBluetoothSliderDragIsAdoptedFromItsFirstNonEcho(t *testing.T) {
	d, mem := volumeTestDaemon(t)

	const level = 0.4
	if err := d.applyMasterVolume(level); err != nil {
		t.Fatal(err)
	}
	step := bluetoothVolumeStep(level)
	waitVolumeWritten(t, mem, step)

	// The drag leaves the echo step behind in two-step increments.
	for _, report := range []uint16{step - 2, step - 4, step - 6} {
		mem.SimulateVolume(report)
		want := float64(report) / 127
		waitFor(t, 2*time.Second, fmt.Sprintf("master follows the drag to %d/127", report), func() bool {
			return math.Abs(volumeTestMaster(d)-want) < 0.002
		})
	}
}

// TestBluetoothLateEchoIsAdoptedAfterTheWindow pins the window's bound: the memory
// is a fraction of a second, not seconds. Once it expires, a report of the level we
// wrote is just a genuine change and is adopted — the filter can never hold a
// deliberate change back.
func TestBluetoothLateEchoIsAdoptedAfterTheWindow(t *testing.T) {
	d, mem := volumeTestDaemon(t)

	const level = 0.5
	if err := d.applyMasterVolume(level); err != nil {
		t.Fatal(err)
	}
	step := bluetoothVolumeStep(level)
	waitVolumeWritten(t, mem, step)

	// Fresh, the same report is the echo and changes nothing.
	mem.SimulateVolume(step)
	select {
	case <-time.After(300 * time.Millisecond):
	}
	if have := volumeTestMaster(d); math.Abs(have-level) > 0.001 {
		t.Fatalf("the fresh echo moved the master to %v, want %v", have, level)
	}

	// The settings watcher may mirror the same level more than once; the window is
	// measured from the newest write, so wait past it plus room for that duplicate.
	time.Sleep(bluetoothVolumeEchoWindow + 500*time.Millisecond)
	mem.SimulateVolume(step)
	waitFor(t, 2*time.Second, "the late report is adopted", func() bool {
		return math.Abs(volumeTestMaster(d)-float64(step)/127) < 1e-9
	})
}

// gatedVolumeBluetooth blocks the mirror write of one level until the test lets it
// finish, so adoption can be proven not to wait on the round trip.
type gatedVolumeBluetooth struct {
	*bluetooth.Memory
	mu       sync.Mutex
	gate     float64
	gateSet  bool
	entered  chan struct{}
	enterOne sync.Once
	release  chan struct{}
	relOne   sync.Once
}

func newGatedVolumeBluetooth() *gatedVolumeBluetooth {
	return &gatedVolumeBluetooth{
		Memory:  bluetooth.NewMemory(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *gatedVolumeBluetooth) gateOn(level float64) {
	b.mu.Lock()
	b.gate, b.gateSet = level, true
	b.mu.Unlock()
}

func (b *gatedVolumeBluetooth) letGo() {
	b.relOne.Do(func() { close(b.release) })
}

func (b *gatedVolumeBluetooth) SetTransportVolume(ctx context.Context, level float64) error {
	b.mu.Lock()
	gated := b.gateSet && level == b.gate
	b.mu.Unlock()
	if gated {
		b.enterOne.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.Memory.SetTransportVolume(ctx, level)
}

// TestBluetoothSourceChangeAdoptedWhileTheMirrorWriteIsInFlight pins that the
// off switch of the filter is immediate: a report outside the echo allowance is
// adopted even while a write to the peer is still open, so a slow AVRCP round trip
// can never hold the speaker's level.
func TestBluetoothSourceChangeAdoptedWhileTheMirrorWriteIsInFlight(t *testing.T) {
	mgr := newGatedVolumeBluetooth()
	mem := mgr.Memory
	d, cancel, _ := newTestDaemonPrepared(t, func(d *Daemon) { d.bt = mgr })
	defer cancel()
	t.Cleanup(mgr.letGo)
	d.setBluetoothNative(true)
	waitFor(t, 3*time.Second, "bluetooth watcher", func() bool { return mem.SubscriberCount() > 0 })
	mem.SimulateConnect(bluetooth.Device{Address: volumeTestAddress, Name: "Phone"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: volumeTestAddress, DeviceName: "Phone"})
	waitFor(t, 3*time.Second, "source connected", func() bool {
		state, err := mem.Media(context.Background())
		return err == nil && state.Present
	})

	const level = 0.3
	mgr.gateOn(level)
	if err := d.applyMasterVolume(level); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mgr.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the mirror write never started")
	}

	// While that write is still open, the source reports a level far from it.
	mem.SimulateVolume(70)
	want := float64(70) / 127
	waitFor(t, 2*time.Second, "master follows the source during the write", func() bool {
		return math.Abs(volumeTestMaster(d)-want) < 0.002
	})
	mgr.letGo()
}

// TestBluetoothSourceSliderStillDrivesTheMaster covers the other direction on its
// own: a change the source made itself moves the framework master, and the mirror
// then carries that same level back, which is what keeps the two displays in step.
func TestBluetoothSourceSliderStillDrivesTheMaster(t *testing.T) {
	d, mem := volumeTestDaemon(t)

	mem.SimulateVolume(96)
	want := float64(96) / 127
	waitFor(t, 3*time.Second, "master follows the source slider", func() bool {
		return math.Abs(volumeTestMaster(d)-want) < 0.002
	})
	waitVolumeWritten(t, mem, bluetoothVolumeStep(want))
	if have := volumeTestGain(t, d); math.Abs(have-want) > 0.002 {
		t.Fatalf("local gain = %v, want %v", have, want)
	}
}

// TestBluetoothEchoMemoryDoesNotOutliveTheConnection pins the session boundary: a
// level written before a reconnect must not be mistaken for the new session's
// echo. The previous session's step is reported right after reconnecting — inside
// the echo window — and must be adopted, which only happens if the memory was
// cleared.
func TestBluetoothEchoMemoryDoesNotOutliveTheConnection(t *testing.T) {
	d, mem := volumeTestDaemon(t)

	if err := d.applyMasterVolume(0.5); err != nil {
		t.Fatal(err)
	}
	oldStep := bluetoothVolumeStep(0.5)
	waitVolumeWritten(t, mem, oldStep)

	mem.SimulateDisconnect(volumeTestAddress)
	waitFor(t, 3*time.Second, "source disconnected", func() bool {
		state, err := mem.Media(context.Background())
		return err == nil && !state.Present
	})
	// A different level while the source is away, so the report below cannot be
	// mistaken for the current master either.
	if err := d.applyMasterVolume(0.2); err != nil {
		t.Fatal(err)
	}

	mem.SimulateConnect(bluetooth.Device{Address: volumeTestAddress, Name: "Phone"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: volumeTestAddress, DeviceName: "Phone"})
	waitFor(t, 3*time.Second, "source reconnected", func() bool {
		state, err := mem.Media(context.Background())
		return err == nil && state.Present
	})

	// The level the previous session had written is a genuine report now. The
	// connect event precedes this report in the watcher's queue, so the clearing
	// has already run.
	mem.SimulateVolume(oldStep)
	want := float64(oldStep) / 127
	waitFor(t, 2*time.Second, "the new session's report is adopted", func() bool {
		return math.Abs(volumeTestMaster(d)-want) < 0.002
	})
}

// noAbsoluteVolumeBluetooth is a source whose AVRCP register is not writable:
// BlueZ refuses the write with ErrNoAbsoluteVolume. The manager records the
// attempt so the test can assert the daemon tried and then kept the level local.
type noAbsoluteVolumeBluetooth struct {
	*bluetooth.Memory
	mu       sync.Mutex
	attempts int
}

func (b *noAbsoluteVolumeBluetooth) SetTransportVolume(context.Context, float64) error {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()
	return fmt.Errorf("%w (source %s)", bluetooth.ErrNoAbsoluteVolume, volumeTestAddress)
}

func (b *noAbsoluteVolumeBluetooth) writeAttempts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// TestBluetoothVolumeMirrorQuietWithoutAbsoluteVolume pins the degraded path: a
// source with no writable register still gets the local level, a source slider
// still drives the master, and the refused write leaves no echo memory behind
// that could swallow the next genuine report.
func TestBluetoothVolumeMirrorQuietWithoutAbsoluteVolume(t *testing.T) {
	mem := bluetooth.NewMemory()
	mgr := &noAbsoluteVolumeBluetooth{Memory: mem}
	d, cancel, _ := newTestDaemonPrepared(t, func(d *Daemon) { d.bt = mgr })
	defer cancel()
	d.setBluetoothNative(true)
	waitFor(t, 3*time.Second, "bluetooth watcher", func() bool { return mem.SubscriberCount() > 0 })
	mem.SimulateConnect(bluetooth.Device{Address: volumeTestAddress, Name: "Phone"},
		bluetooth.MediaState{Present: true, Playing: true, DeviceAddress: volumeTestAddress, DeviceName: "Phone"})
	waitFor(t, 3*time.Second, "source connected", func() bool {
		state, err := mem.Media(context.Background())
		return err == nil && state.Present
	})

	const level = 0.25
	if err := d.applyMasterVolume(level); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the refused mirror write", func() bool { return mgr.writeAttempts() > 0 })
	if have := volumeTestGain(t, d); have != level {
		t.Fatalf("local gain = %v, want %v", have, level)
	}
	if written, ok := mem.TransportVolumeWritten(); ok {
		t.Fatalf("a refused write reported success: %d/127", written)
	}
	// A provisional echo record exists while a write is in flight; once the writer
	// is idle every refused write has dropped its own record.
	waitFor(t, 3*time.Second, "the mirror writer to go idle", func() bool {
		d.btMu.Lock()
		defer d.btMu.Unlock()
		return !d.btMirrorBusy
	})
	d.btMu.Lock()
	echoes := 0
	for _, echo := range d.btEchoes {
		if !echo.at.IsZero() {
			echoes++
		}
	}
	d.btMu.Unlock()
	if echoes != 0 {
		t.Fatalf("a refused write left %d echo records behind", echoes)
	}

	// The source's own slider still drives the master.
	mem.SimulateVolume(20)
	want := float64(20) / 127
	waitFor(t, 3*time.Second, "master follows the source slider", func() bool {
		return math.Abs(volumeTestMaster(d)-want) < 0.002
	})
}
