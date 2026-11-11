// Package bluetooth is the framework's Bluetooth facade.
//
// On the device it drives BlueZ over the system D-Bus: adapter power,
// discoverability and pairability, paired-device management, and the AVRCP
// state of a connected audio source (playback commands, track metadata and
// absolute volume). The framework never drives the HCI device directly; BlueZ
// owns profiles and pairing.
//
// The manager is an interface so the simulator and tests use the in-memory
// implementation while the stock image uses BlueZ.
package bluetooth

import (
	"context"
	"errors"
	"fmt"
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
)

// Status is the adapter state a UI can show.
type Status struct {
	Present      bool
	Powered      bool
	Discoverable bool
	Pairable     bool
	Adapter      string
	Detail       string
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
)

// Event carries the state after a change. Media is set for EventMedia and on
// connect/disconnect when a state is available.
type Event struct {
	Kind          EventKind
	DeviceAddress string
	DeviceName    string
	Media         MediaState
}

// Manager is the Bluetooth facade. A nil Manager means the subsystem is absent.
type Manager interface {
	Status(ctx context.Context) (Status, error)
	SetDiscoverable(ctx context.Context, on bool) error
	SetPairable(ctx context.Context, on bool) error
	EnsurePowered(ctx context.Context) error
	PairedDevices(ctx context.Context) ([]Device, error)
	Unpair(ctx context.Context, address string) error
	Media(ctx context.Context) (MediaState, error)
	// SetTransportVolume writes the AVRCP transport volume; it is the single
	// Bluetooth listening gain, mirrored by the framework master.
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
	mu       sync.Mutex
	status   Status
	devices  []Device
	media    MediaState
	revision uint64
	subs     map[int]chan Event
	nextID   int
	closed   bool
}

// NewMemory returns a powered simulator adapter with no devices.
func NewMemory() *Memory {
	return &Memory{
		status: Status{Present: true, Powered: true, Adapter: "sim-hci0", Detail: "simulated adapter"},
		subs:   map[int]chan Event{},
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
	m.status.Discoverable = on
	return nil
}

func (m *Memory) SetPairable(_ context.Context, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.status.Powered {
		return fmt.Errorf("bluetooth adapter is powered off")
	}
	m.status.Pairable = on
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
	defer m.mu.Unlock()
	for i := range m.devices {
		if m.devices[i].Address == address {
			if !m.devices[i].Paired {
				return fmt.Errorf("device %s is not paired", address)
			}
			m.devices[i].Paired = false
			m.devices[i].Connected = false
			return nil
		}
	}
	return fmt.Errorf("unknown device %s", address)
}

func (m *Memory) ConnectAudio(context.Context, string) error { return nil }

func (m *Memory) SetTransportVolume(context.Context, float64) error { return nil }

func (m *Memory) TransportVolume(context.Context) (uint16, error) { return 0, nil }

func (m *Memory) Media(context.Context) (MediaState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.media, nil
}

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
	case CommandNext, CommandPrevious:
		// Track changes are injected by tests through SetMedia.
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

func (m *Memory) Watch(ctx context.Context) <-chan Event {
	ch := make(chan Event, 16)
	m.mu.Lock()
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

// SimulateConnect marks a device connected and publishes the given media state.
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
	event := Event{Kind: EventConnected, DeviceAddress: dev.Address, DeviceName: dev.Name, Media: media}
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
	}
	m.revision++
	m.media.Revision = m.revision
	event := Event{Kind: EventDisconnected, DeviceAddress: address, Media: m.media}
	m.mu.Unlock()
	m.broadcast(event)
}

// SetMedia replaces the media state, e.g. after a track change.
func (m *Memory) SetMedia(media MediaState) {
	m.mu.Lock()
	m.revision++
	media.Revision = m.revision
	m.media = media
	event := Event{Kind: EventMedia, Media: media}
	m.mu.Unlock()
	m.broadcast(event)
}

func (m *Memory) broadcast(event Event) {
	m.mu.Lock()
	subs := make([]chan Event, 0, len(m.subs))
	for _, ch := range m.subs {
		subs = append(subs, ch)
	}
	m.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- event:
		default:
		}
	}
}
