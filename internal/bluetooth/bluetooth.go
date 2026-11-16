// Package bluetooth is the framework's Bluetooth facade.
//
// On the device it drives BlueZ over the system D-Bus: adapter power, the
// incoming-connection and discoverability policy, paired-device management,
// and the AVRCP state of a connected audio source (playback commands, track
// metadata and absolute volume). The framework never drives the HCI device
// directly; BlueZ owns profiles and pairing. BR/EDR connectability, which BlueZ
// 5.65 does not expose as an Adapter1 property, is controlled through the
// public Linux Bluetooth management API's Set Connectable command.
//
// The manager is an interface so the simulator and tests use the in-memory
// implementation while the stock image uses BlueZ.
package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

var (
	// ErrUnavailable means there is no system bus or no BlueZ adapter.
	ErrUnavailable = errors.New("bluetooth is unavailable")
	// ErrNoSource means no audio source is connected right now.
	ErrNoSource = errors.New("no Bluetooth media source is connected")
	// ErrUnsupported means the connected source exposes no usable control.
	ErrUnsupported = errors.New("the connected source does not support this operation")
	// ErrNoTransport means the A2DP endpoint is registered but no transport is
	// configured yet; callers should retry while the profile comes up.
	ErrNoTransport = errors.New("no A2DP transport is configured")
	// ErrNoAbsoluteVolume means the source is connected but does not expose
	// AVRCP absolute volume: BlueZ creates MediaTransport1.Volume only after the
	// peer reports a level, and a write cannot create it. The framework level
	// then stays local; there is no relative-volume fallback on purpose.
	ErrNoAbsoluteVolume = errors.New("the source does not expose AVRCP absolute volume")
	// ErrIncomingDisabled means the user disabled incoming Bluetooth
	// connections. The adapter stays powered for BLE work, but no inbound link
	// is accepted, including from already-paired devices.
	ErrIncomingDisabled = errors.New("Bluetooth incoming connections are disabled")
	// ErrPairingDisabled means a new remote device tried to bond while the
	// adapter was not discoverable. An existing bond does not depend on this
	// window.
	ErrPairingDisabled = errors.New("Bluetooth discoverability is disabled")
)

// A2DPTransport is an acquired A2DP media transport. The caller owns File and
// must close it to release the stream; Config is the negotiated SBC media
// configuration.
type A2DPTransport struct {
	File    *os.File
	ReadMTU uint16
	Config  []byte
}

// TrustSetter marks a paired source trusted so BlueZ authorizes AVRCP. It does
// not change any setting on the source device.
type TrustSetter interface {
	SetTrusted(ctx context.Context, address string, trusted bool) error
}

// A2DPSink is implemented by a manager that owns the A2DP sink endpoint and
// can acquire its transport. It is separate from Manager so the simulator and
// callers that only need control state do not have to implement it.
type A2DPSink interface {
	// RegisterA2DPSink exports and registers the local A2DP SBC endpoint with
	// BlueZ and returns the real registration outcome. It is idempotent and
	// must not report success for an endpoint that is not armed. Implementations
	// register before enabling page scan so a first incoming source cannot
	// connect without an audio profile.
	RegisterA2DPSink(ctx context.Context) error
	// AcquireA2DP acquires the configured transport. It returns ErrNoTransport
	// until BlueZ has created one.
	AcquireA2DP(ctx context.Context) (*A2DPTransport, error)
	// ReleaseA2DP releases the current transport. It is safe to call when none
	// is acquired.
	ReleaseA2DP(ctx context.Context) error
}

// A2DPRegistrar is optionally implemented by a manager that can report whether
// its sink endpoint is currently advertised. Registration is generation-scoped:
// after a BlueZ restart or adapter power cycle it reports false until the
// endpoint is armed against the new adapter state. The daemon uses it to decide
// when profile advertisement still has to be prepared before incoming
// connections or discoverability are enabled, and never treats a stale
// registration as ready.
type A2DPRegistrar interface {
	SinkRegistered() bool
}

// A2DPSession is optionally implemented by a manager that can assert what audio
// link a connected source actually got, and ask for the missing profile once. A
// successful ConnectProfile is not evidence of an A2DP transport; Session is.
type A2DPSession interface {
	// Session reports the source's observed audio link. A source that is
	// connected but has no A2DP transport reports an empty A2DPState.
	Session(ctx context.Context, address string) (AudioLink, error)
	// RequestA2DPProfile asks a connected source to bring its A2DP source
	// profile up. It never disconnects the source; the caller bounds it and
	// decides how often it may run.
	RequestA2DPProfile(ctx context.Context, address string) error
}

// CommandObservation is one resolved AVRCP command, for diagnostics only.
// Evidence names the fact the direction came from (EvidenceStatus,
// EvidenceTransport, EvidenceReceipt or EvidenceMemory). It is empty for
// commands that need no direction.
type CommandObservation struct {
	Device   string
	Command  Command
	Method   string
	Evidence string
	Err      error
}

// Direction is the playback direction of the displayed source, with the fact it
// was derived from. Present is false when no source is connected.
type Direction struct {
	Present  bool
	Playing  bool
	Evidence string
}

// DirectionResolver is optionally implemented by a manager that can resolve the
// playback direction of the displayed source from the same evidence its own
// toggle uses. It is the single authority for that question: a consumer that
// publishes a playback state from it (the compiled-in media provider, and
// therefore the system media controller that reads that state) can never invert
// a direction the manager's next toggle would repeat, which is the whole point
// of the direction memory.
//
// A manager that does not implement it publishes its own exact state instead
// (the simulator), so there is nothing to invert there.
type DirectionResolver interface {
	PlaybackDirection(ctx context.Context) (Direction, error)
}

// CommandObserver is optionally implemented by a manager that can report how it
// resolved a transport command. Installations are read-only: an observer can
// log a decision but cannot change it.
type CommandObserver interface {
	SetCommandObserver(func(CommandObservation))
}

// Status is the adapter state a UI can show.
type Status struct {
	Present bool
	Powered bool
	// Discoverable is the observed BlueZ Adapter1.Discoverable property.
	// Discovering and connecting a new device requires this.
	Discoverable bool
	// IncomingConnections is the observed BR/EDR connectability setting from
	// the Linux Bluetooth management API. It is not fabricated from the power
	// state: the adapter stays Powered when incoming connections are denied.
	IncomingConnections bool
	Adapter             string
	// Name is the advertised adapter name (BlueZ Adapter1.Alias, falling back
	// to Adapter1.Name and then the adapter id).
	Name   string
	Detail string
}

// Device is one paired remote device.
type Device struct {
	Address   string
	Name      string
	Paired    bool
	Connected bool
	Trusted   bool
}

// MediaState is the AVRCP view of the connected source. Present is true when a
// source is connected, even if it exposes no track metadata.
type MediaState struct {
	Present       bool
	Playing       bool
	DeviceAddress string
	DeviceName    string
	Title         string
	Artist        string
	Album         string
	DurationMS    uint64
	PositionMS    uint64
	Revision      uint64
	Detail        string
}

// Command is one AVRCP transport command.
type Command int

const (
	CommandPlay Command = iota + 1
	CommandPause
	CommandToggle
	CommandNext
	CommandPrevious
)

func (c Command) String() string {
	switch c {
	case CommandPlay:
		return "play"
	case CommandPause:
		return "pause"
	case CommandToggle:
		return "toggle"
	case CommandNext:
		return "next"
	case CommandPrevious:
		return "previous"
	default:
		return "unknown"
	}
}

// EventKind is one observable change.
type EventKind int

const (
	EventConnected EventKind = iota + 1
	EventDisconnected
	EventMedia
	// EventVolume reports an AVRCP absolute-volume change made on the source.
	// It is emitted only when the level actually changes, so the framework
	// mirroring it cannot loop back into another change.
	EventVolume
	// EventTransport reports an A2DP transport that became usable (pending or
	// active). Callers acquire the stream when they see it instead of polling
	// for the transport.
	EventTransport
	// EventResync reports that the watcher fell behind and at least one
	// lifecycle event was coalesced away. Media is the newest snapshot the
	// manager had when the queue overflowed. Consumers must reconcile their
	// session (active stream, routes) against the manager instead of assuming
	// the missing transition happened; the queue keeps emitting a fresh marker
	// for as long as it stays full.
	EventResync
	// EventAdapter reports an observed adapter policy change (power,
	// discoverability, connectability or advertised name). It carries no media
	// state; consumers refresh the status they display.
	EventAdapter
	// EventVolumeUnavailable reports, once per source session, that the source
	// has no writable AVRCP absolute volume: BlueZ has no MediaTransport1.Volume
	// and refuses to create one, so the framework level stays local. It is only
	// published after the grace window, never for a source that simply has not
	// reported a level yet. It is a diagnostic transition, not playback state;
	// Detail carries the address and the reason for the log, and Media carries
	// the current snapshot so a watcher that only sees this event (under queue
	// overflow) cannot read it as "no source".
	EventVolumeUnavailable
)

// Event carries the state after a change. Media is set for EventMedia, on
// connect/disconnect when a state is available, and on a volume or diagnostic
// event for the displayed source. DeviceAddress/DeviceName identify the source
// for connect, disconnect and volume events; a volume event always belongs to
// the displayed source, never a background phone.
type Event struct {
	Kind          EventKind
	DeviceAddress string
	DeviceName    string
	Media         MediaState
	// Volume is the AVRCP absolute volume (0..127) for EventVolume.
	Volume uint16
	// Detail is a short explanation for a diagnostic event
	// (EventVolumeUnavailable). It is never playback state.
	Detail string
}

// watchEventBuffer is the per-subscriber queue depth. A watcher can fall behind
// (a slow app stream, or the daemon blocked while tearing an audio route down),
// so the queue is a conflation window: it must retain the newest transitions,
// never the oldest ones.
const watchEventBuffer = 16

// deliverEvent publishes one event to a watcher without blocking the manager.
// The manager is the only sender of a watcher channel; the consumer only
// receives.
//
// The queue is a conflation window, not a lossy tail: on overflow the OLDEST
// queued state is dropped and an EventResync marker carrying this event's media
// snapshot is appended instead of the plain event. A watcher that has fallen
// behind therefore learns that at least one transition was coalesced and
// reconciles its session against the manager (the marker is newer than
// everything it replaced). While the consumer stays behind, every further
// publish leaves another marker, so the tail is always the newest snapshot and
// no stale connect/disconnect can be acted on as if it were current.
func deliverEvent(ch chan Event, event Event) {
	select {
	case ch <- event:
		return
	default:
	}
	marker := Event{
		Kind: EventResync, DeviceAddress: event.DeviceAddress,
		DeviceName: event.DeviceName, Media: event.Media,
	}
	// Removing the oldest queued state is safe: the consumer may drain it
	// first, in which case this receive takes the next-oldest one, and the
	// marker below still fits. A closed channel means the watcher ended while
	// this publish was in flight.
	select {
	case _, ok := <-ch:
		if !ok {
			return
		}
	default:
	}
	select {
	case ch <- marker:
		return
	default:
	}
	// A concurrent publisher (the in-memory manager used by tests and the
	// simulator) refilled the freed slot. Evict once more; the marker must not
	// be lost behind a burst.
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- marker:
	default:
		// Still full under concurrent publishers; the next overflow carries a
		// newer snapshot anyway.
	}
}

// Manager is the Bluetooth facade. A nil Manager means the subsystem is absent.
type Manager interface {
	Status(ctx context.Context) (Status, error)
	SetDiscoverable(ctx context.Context, on bool) error
	// SetIncomingConnections controls BR/EDR connectability without powering
	// the radio off. Enabling discoverability also enables incoming
	// connections; disabling incoming connections disables discoverability.
	SetIncomingConnections(ctx context.Context, on bool) error
	EnsurePowered(ctx context.Context) error
	PairedDevices(ctx context.Context) ([]Device, error)
	Unpair(ctx context.Context, address string) error
	Media(ctx context.Context) (MediaState, error)
	// SetTransportVolume writes the AVRCP transport volume. The daemon uses it as
	// a display mirror of the framework master only: the local attenuation stays
	// the single gain stage, and the source's report of the write is filtered as
	// an echo instead of being adopted as a source-side change.
	SetTransportVolume(ctx context.Context, level float64) error
	// TransportVolume reads the current AVRCP transport volume (0..127).
	TransportVolume(ctx context.Context) (uint16, error)
	// ConnectAudio asks a connected source to bring up its audio profiles
	// (A2DP source and AVRCP). Sources do not always do this on their own.
	ConnectAudio(ctx context.Context, address string) error
	Command(ctx context.Context, cmd Command) error
	Watch(ctx context.Context) <-chan Event
	Close() error
}

// --- in-memory manager (simulator and tests) --------------------------------

// Memory is a scriptable manager. It stores state and broadcasts events exactly
// like the real facade, so app and RPC paths can be tested without a bus.
type Memory struct {
	mu            sync.Mutex
	status        Status
	devices       []Device
	media         MediaState
	volume        uint16
	hasVolume     bool
	writtenVolume uint16
	wroteVolume   bool
	revision      uint64
	subs          map[int]chan Event
	nextID        int
	closed        bool

	// Simulator-only playlist. It is empty unless a test installs one, so
	// Next/Previous keep their historical no-op behavior by default.
	playlist []string
	track    int
}

// NewMemory returns a powered simulator adapter with no devices. Incoming
// connections are enabled and the adapter is hidden, matching the persisted
// defaults.
func NewMemory() *Memory {
	return &Memory{
		status: Status{
			Present: true, Powered: true, IncomingConnections: true,
			Adapter: "sim-hci0", Name: "Capsula", Detail: "simulated adapter",
		},
		subs: map[int]chan Event{},
	}
}

func (m *Memory) Status(context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, nil
}

func (m *Memory) SetDiscoverable(_ context.Context, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.status.Powered {
		return fmt.Errorf("bluetooth adapter is powered off")
	}
	if on {
		// Discoverability is the pairing window: a new device cannot connect
		// to an adapter that accepts no incoming links.
		m.status.IncomingConnections = true
	}
	m.status.Discoverable = on
	return nil
}

func (m *Memory) SetIncomingConnections(_ context.Context, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.status.Powered {
		return fmt.Errorf("bluetooth adapter is powered off")
	}
	m.status.IncomingConnections = on
	if !on {
		// The two controls are dependent: hidden is the only coherent state
		// when no inbound link is accepted.
		m.status.Discoverable = false
	}
	return nil
}

func (m *Memory) EnsurePowered(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Powered = true
	return nil
}

func (m *Memory) PairedDevices(context.Context) ([]Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		if d.Paired {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Connected != out[j].Connected {
			return out[i].Connected
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *Memory) Unpair(_ context.Context, address string) error {
	m.mu.Lock()
	for i := range m.devices {
		if m.devices[i].Address != address {
			continue
		}
		if !m.devices[i].Paired {
			m.mu.Unlock()
			return fmt.Errorf("device %s is not paired", address)
		}
		wasConnected, name := m.devices[i].Connected, m.devices[i].Name
		m.devices[i].Paired = false
		m.devices[i].Connected = false
		var event Event
		if wasConnected {
			// The real manager disconnects a connected device before removing
			// the bond, so consumers see the same disconnect transition.
			if m.media.DeviceAddress == address {
				m.media = MediaState{}
			}
			m.hasVolume, m.volume = false, 0
			m.revision++
			m.media.Revision = m.revision
			event = Event{Kind: EventDisconnected, DeviceAddress: address, DeviceName: name, Media: m.media}
		}
		m.mu.Unlock()
		if wasConnected {
			m.broadcast(event)
		}
		return nil
	}
	m.mu.Unlock()
	return fmt.Errorf("unknown device %s", address)
}

func (m *Memory) ConnectAudio(context.Context, string) error { return nil }

// SetTransportVolume records the framework-driven level. Like the real
// manager it does not emit an event: the source did not change.
func (m *Memory) SetTransportVolume(_ context.Context, level float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}
	m.volume = uint16(level*127 + 0.5)
	m.hasVolume = true
	m.writtenVolume, m.wroteVolume = m.volume, true
	return nil
}

func (m *Memory) TransportVolume(context.Context) (uint16, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasVolume {
		return 0, ErrUnsupported
	}
	return m.volume, nil
}

func (m *Memory) Media(context.Context) (MediaState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.media, nil
}

// Command applies a transport command to the simulated source. The simulator
// owns the playback state, so it applies the command directly (and a toggle
// flips the state it holds); it must not emulate the real manager's direction
// inference, whose whole point is that a BlueZ source's cached status is not
// authoritative. A test that needs the stale-status case drives the real path
// over a D-Bus fixture instead.
func (m *Memory) Command(_ context.Context, cmd Command) error {
	m.mu.Lock()
	if !m.media.Present {
		m.mu.Unlock()
		return ErrNoSource
	}
	switch cmd {
	case CommandPlay:
		m.media.Playing = true
	case CommandPause:
		m.media.Playing = false
	case CommandToggle:
		m.media.Playing = !m.media.Playing
	case CommandNext:
		m.advanceTrackLocked(1)
	case CommandPrevious:
		m.advanceTrackLocked(-1)
	default:
		m.mu.Unlock()
		return ErrUnsupported
	}
	m.revision++
	m.media.Revision = m.revision
	event := Event{Kind: EventMedia, Media: m.media}
	m.mu.Unlock()
	m.broadcast(event)
	return nil
}

// SubscriberCount reports how many Watch channels are registered. It is a
// simulator diagnostic used to tell when a consumer has attached.
func (m *Memory) SubscriberCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

func (m *Memory) Watch(ctx context.Context) <-chan Event {
	ch := make(chan Event, watchEventBuffer)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		close(ch)
		return ch
	}
	m.nextID++
	id := m.nextID
	m.subs[id] = ch
	m.mu.Unlock()
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		if sub, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(sub)
		}
		m.mu.Unlock()
	}()
	return ch
}

func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	for id, ch := range m.subs {
		close(ch)
		delete(m.subs, id)
	}
	return nil
}

// SimulateIncomingConnection models the kernel/agent decision for an inbound
// link and, when allowed, connects the device. Incoming off denies every
// device, paired or not, while the adapter stays powered. A new (unpaired)
// device additionally requires discoverability, the pairing window.
func (m *Memory) SimulateIncomingConnection(dev Device, media MediaState) error {
	m.mu.Lock()
	if !m.status.Present {
		m.mu.Unlock()
		return ErrUnavailable
	}
	if !m.status.Powered {
		m.mu.Unlock()
		return fmt.Errorf("bluetooth adapter is powered off")
	}
	if !m.status.IncomingConnections {
		m.mu.Unlock()
		return ErrIncomingDisabled
	}
	if !dev.Paired && !m.status.Discoverable {
		m.mu.Unlock()
		return ErrPairingDisabled
	}
	m.mu.Unlock()
	m.SimulateConnect(dev, media)
	return nil
}

// SimulateConnect marks a device connected and publishes the given media state.
// It bypasses the incoming-connection gate; tests of that policy use
// SimulateIncomingConnection instead.
func (m *Memory) SimulateConnect(dev Device, media MediaState) {
	m.mu.Lock()
	dev.Paired = true
	dev.Connected = true
	found := false
	for i := range m.devices {
		if m.devices[i].Address == dev.Address {
			m.devices[i] = dev
			found = true
		}
	}
	if !found {
		m.devices = append(m.devices, dev)
	}
	media.Present = true
	media.DeviceAddress = dev.Address
	media.DeviceName = dev.Name
	m.revision++
	media.Revision = m.revision
	m.media = media
	m.playlist = nil
	m.track = 0
	m.hasVolume = true
	m.volume = 100
	event := Event{Kind: EventConnected, DeviceAddress: dev.Address, DeviceName: dev.Name, Media: media}
	m.mu.Unlock()
	m.broadcast(event)
}

// TransportVolumeWritten reports the register value the daemon last wrote to
// the source, if any. It is the simulator's record of a write, not a state a
// consumer reads.
func (m *Memory) TransportVolumeWritten() (uint16, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writtenVolume, m.wroteVolume
}

// SimulateVolume publishes an AVRCP absolute-volume change made on the source.
func (m *Memory) SimulateVolume(volume uint16) {
	if volume > 127 {
		volume = 127
	}
	m.mu.Lock()
	m.volume, m.hasVolume = volume, true
	event := Event{Kind: EventVolume, Volume: volume, Media: m.media, DeviceAddress: m.media.DeviceAddress, DeviceName: m.media.DeviceName}
	m.mu.Unlock()
	m.broadcast(event)
}

// SimulateDisconnect marks a device disconnected and clears media presence.
func (m *Memory) SimulateDisconnect(address string) {
	m.mu.Lock()
	for i := range m.devices {
		if m.devices[i].Address == address {
			m.devices[i].Connected = false
		}
	}
	if m.media.DeviceAddress == address {
		m.media = MediaState{}
		m.playlist = nil
		m.track = 0
	}
	m.hasVolume = false
	m.volume = 0
	m.revision++
	m.media.Revision = m.revision
	event := Event{Kind: EventDisconnected, DeviceAddress: address, Media: m.media}
	m.mu.Unlock()
	m.broadcast(event)
}

// SetPlaylist installs a deterministic simulator playlist. Next/Previous move
// through it and publish the new Title through the ordinary MediaState, giving
// integration tests an observable command oracle without a production-only
// echo field. An empty playlist restores the historical no-op transport.
func (m *Memory) SetPlaylist(titles []string) {
	m.mu.Lock()
	m.playlist = append([]string(nil), titles...)
	m.track = 0
	if len(m.playlist) > 0 {
		m.media.Title = m.playlist[0]
		m.media.PositionMS = 0
	}
	m.revision++
	m.media.Revision = m.revision
	event := Event{Kind: EventMedia, Media: m.media}
	m.mu.Unlock()
	m.broadcast(event)
}

func (m *Memory) advanceTrackLocked(delta int) {
	if len(m.playlist) == 0 {
		return
	}
	m.track = (m.track + delta + len(m.playlist)) % len(m.playlist)
	m.media.Title = m.playlist[m.track]
	m.media.PositionMS = 0
}

// SetMedia replaces the media state, e.g. after a track change.
func (m *Memory) SetMedia(media MediaState) {
	m.mu.Lock()
	m.playlist = nil
	m.track = 0
	m.revision++
	media.Revision = m.revision
	m.media = media
	event := Event{Kind: EventMedia, Media: media}
	m.mu.Unlock()
	m.broadcast(event)
}

func (m *Memory) broadcast(event Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Deliver under the same mutex that closes subscriptions. deliverEvent is
	// non-blocking, so this cannot stall a publisher, and a watcher cancelled
	// concurrently cannot turn the send into a panic on a closed channel.
	for _, ch := range m.subs {
		deliverEvent(ch, event)
	}
}
