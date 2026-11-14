package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"capsulad/internal/bluetooth"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// Close must cancel an in-flight command, drop queued commands, and return
// only after the worker stops and the client is safe to release. This uses the
// controller's execution seam so the ordering is deterministic without a live
// app bus.
func TestSystemMediaCloseDrainsBeforeClosingClient(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{ctx: ctx, logs: logs}
	s := newSystemMediaController(d)

	started := make(chan mediaAction, 2)
	var mu sync.Mutex
	var ran []mediaAction
	s.executeAction = func(ctx context.Context, action mediaAction) {
		mu.Lock()
		ran = append(ran, action)
		mu.Unlock()
		started <- action
		<-ctx.Done()
	}
	go s.Run(ctx)

	s.Toggle()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first command did not start")
	}
	s.Previous() // queued behind the in-flight command
	s.Close()

	mu.Lock()
	got := append([]mediaAction(nil), ran...)
	mu.Unlock()
	if len(got) != 1 || got[0] != mediaToggle {
		t.Fatalf("commands run across Close = %v, want only the in-flight toggle", got)
	}

	// A command after Close must not be executed, regardless of the queue.
	s.Next()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got = append([]mediaAction(nil), ran...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("post-close command ran: %v", got)
	}
}

// clientConn never returns nil; after Close it returns the closed client
// rather than a nil pointer that a service constructor would accept and then
// and dereference.
func TestSystemMediaClientConnIsNeverNil(t *testing.T) {
	clock := ids.NewClock()
	logs, _ := logbus.New(clock, 100, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{ctx: ctx, logs: logs}
	s := newSystemMediaController(d)
	t.Cleanup(s.Close)
	if conn := s.clientConn(); conn == nil {
		t.Fatal("clientConn returned nil")
	}
}

// The sink's receipt state is the only honest evidence that a source without a
// MediaPlayer1.Status is playing. It must belong to the current stream and the
// asked-for device, go stale when packets stop, and be cleared when the stream
// stops; nothing else may report "receiving".
func TestBluetoothReceivingAudioUsesCurrentStreamReceipts(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{clock: clock, logs: logs}
	if d.bluetoothReceivingAudio("AA:BB") {
		t.Fatal("no stream reported receiving audio")
	}

	stream := &a2dpStream{d: d, address: "AA:BB:CC:DD:EE:FF", done: make(chan struct{})}
	d.btStream = stream
	if d.bluetoothReceivingAudio("AA:BB:CC:DD:EE:FF") {
		t.Fatal("a stream that never delivered a packet reported playing")
	}
	stream.notePacket()
	if !d.bluetoothReceivingAudio("AA:BB:CC:DD:EE:FF") {
		t.Fatal("fresh packet receipt not reported")
	}
	if d.bluetoothReceivingAudio("11:22:33:44:55:66") {
		t.Fatal("another device's receipts answered for an unrelated source")
	}
	// A paused source keeps the transport open but stops sending; the receipt
	// goes stale after the grace window.
	stream.lastPacketNS.Store(int64(clock.NowNS()) - int64(2*a2dpReceiptGrace))
	if d.bluetoothReceivingAudio("") {
		t.Fatal("stale receipt treated as playing")
	}

	// A stop clears the evidence even if the object is still referenced.
	stream.notePacket()
	stream.stopWithRelease(false)
	if d.bluetoothReceivingAudio("") {
		t.Fatal("stopped stream still reported receiving audio")
	}
}

// Switching sources must not inherit the previous source's receipts.
func TestBluetoothReceivingAudioIsPerStream(t *testing.T) {
	clock := ids.NewClock()
	logs, _ := logbus.New(clock, 100, "")
	d := &Daemon{clock: clock, logs: logs}
	first := &a2dpStream{d: d, address: "AA:BB:CC:DD:EE:01"}
	first.notePacket()
	d.btStream = first
	d.btStream = &a2dpStream{d: d, address: "AA:BB:CC:DD:EE:02"}
	if d.bluetoothReceivingAudio("AA:BB:CC:DD:EE:01") {
		t.Fatal("new source inherited a stale source's receipts")
	}
	if d.bluetoothReceivingAudio("AA:BB:CC:DD:EE:02") {
		t.Fatal("new source reported receipts it never received")
	}
}

// stubBluetooth accepts the receipt probe while delegating every other manager
// method to the embedded interface, so controller wiring can be observed.
type stubBluetooth struct {
	bluetooth.Manager
	probe func(address string) bool
}

func (s *stubBluetooth) SetAudioStreamProbe(probe func(address string) bool) { s.probe = probe }

// The media controller must hand the Bluetooth facade the daemon's sink
// receipt evidence; otherwise a source with only MediaControl1 can never be
// paused by a circle tap.
func TestSystemMediaControllerInstallsReceiptProbe(t *testing.T) {
	clock := ids.NewClock()
	logs, _ := logbus.New(clock, 100, "")
	d := &Daemon{clock: clock, logs: logs}
	stub := &stubBluetooth{}
	d.bt = stub
	s := newSystemMediaController(d)
	t.Cleanup(s.Close)
	if stub.probe == nil {
		t.Fatal("receipt probe was not installed")
	}
	stream := &a2dpStream{d: d, address: "AA:BB:CC:DD:EE:FF"}
	stream.notePacket()
	d.btStream = stream
	if !stub.probe("AA:BB:CC:DD:EE:FF") {
		t.Fatal("installed probe did not report the current stream's receipts")
	}
	if stub.probe("11:22:33:44:55:66") {
		t.Fatal("installed probe answered for an unrelated device")
	}
}
