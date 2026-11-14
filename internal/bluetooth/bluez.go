package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	bluezService   = "org.bluez"
	bluezRoot      = dbus.ObjectPath("/")
	bluezNamespace = "/org/bluez"
)

// BlueZ is the real manager. It reads the object manager and subscribes to
// PropertiesChanged/ObjectManager/NameOwnerChanged signals; every notification
// refreshes the whole small object tree instead of trying to merge partial
// updates. Transport volume changes refresh without the signal coalescing delay,
// and connect/disconnect, volume and transport readiness are published as
// events so callers never poll.
type BlueZ struct {
	conn *dbus.Conn

	refreshMu sync.Mutex
	mu        sync.Mutex
	adapter   dbus.ObjectPath
	status    Status
	devices   []Device
	media     MediaState
	connected map[string]string
	revision  uint64
	subs      map[int]chan Event
	nextID    int
	closed    bool

	// policyMu serializes the two dependent user controls and the automatic
	// re-application after power/restart. incomingConfigured is false until the
	// daemon applies persisted settings, so startup never guesses connectable.
	policyMu           sync.Mutex
	incomingEnabled    bool
	incomingConfigured bool

	cancel   context.CancelFunc
	powering atomic.Bool

	agentExported   atomic.Bool
	agentRegistered atomic.Bool

	volume       uint16
	volumeSet    bool
	volumeWrote  bool
	volumeDevice string
	transportKey string

	registrationMu sync.Mutex
	endpointMu     sync.Mutex
	endpointPath   dbus.ObjectPath
	endpoint       *sbcEndpoint
	armed          bool
	// armedGen is the endpoint generation the current registration belongs
	// to. endpointGen advances whenever BlueZ or the adapter can have dropped
	// the registration, so a stale armed flag can never report readiness.
	armedGen    uint64
	endpointGen uint64
	transport   dbus.ObjectPath

	// audioStreamProbe is read-only evidence from the sink owner that the
	// acquired transport is receiving audio right now. It is needed because an
	// acquired MediaTransport1 disappears from the object tree and a source may
	// expose no MediaPlayer1.Status, leaving BlueZ unable to resolve a toggle
	// direction. It is consulted for a toggle only; it never becomes published
	// playback state.
	probeMu          sync.RWMutex
	audioStreamProbe func(address string) bool

	// registerEndpoint is the org.bluez.Media1.RegisterEndpoint call. Tests
	// replace it; production always reaches BlueZ.
	registerEndpoint func(ctx context.Context, adapter, path dbus.ObjectPath) error
}

// Open attaches to the system bus and requires a BlueZ adapter. It returns
// ErrUnavailable when there is no bus, no BlueZ service, or no adapter.
func Open(ctx context.Context) (*BlueZ, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	b := &BlueZ{
		conn: conn, connected: map[string]string{}, subs: map[int]chan Event{},
	}
	if err := b.refresh(ctx); err != nil {
		return nil, err
	}
	if b.adapter == "" {
		return nil, fmt.Errorf("%w: no BlueZ adapter", ErrUnavailable)
	}
	// A registered agent is required for a remote device to bond. Best effort
	// here so a transient BlueZ state cannot disable adapter control; enabling
	// discoverability retries and reports the error instead of claiming
	// success.
	_ = b.registerAgent(ctx)
	runCtx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	go b.run(runCtx)
	return b, nil
}

func (b *BlueZ) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	b.unregisterAgent()
	b.unregisterA2DP()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	for id, ch := range b.subs {
		close(ch)
		delete(b.subs, id)
	}
	return nil
}

func (b *BlueZ) Status(ctx context.Context) (Status, error) {
	if err := b.refresh(ctx); err != nil {
		return Status{}, err
	}
	b.mu.Lock()
	status := b.status
	b.mu.Unlock()
	// BlueZ 5.65 has no Adapter1.Connectable. Read the kernel management
	// setting so the UI never infers connectability from power or discoverable.
	if status.Present && status.Powered {
		if on, ok, err := adapterReadConnectable(ctx, status.Adapter); ok {
			status.IncomingConnections = on
			b.mu.Lock()
			b.status.IncomingConnections = on
			b.mu.Unlock()
		} else if err != nil {
			if status.Detail != "" {
				status.Detail += "; "
			}
			status.Detail += "connectable state unavailable: " + err.Error()
		}
	}
	return status, nil
}

func (b *BlueZ) SetDiscoverable(ctx context.Context, on bool) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if on {
		// Discoverability is the window in which a new device both finds the
		// speaker and bonds. Require incoming connections and the pairing agent
		// before advertising pairability we cannot honor.
		if err := b.enableIncomingLocked(ctx); err != nil {
			return err
		}
		if err := b.setAdapterProperty(ctx, "Pairable", true); err != nil {
			return err
		}
		if err := b.setAdapterProperty(ctx, "Discoverable", true); err != nil {
			return err
		}
	} else {
		if err := b.refresh(ctx); err != nil {
			return err
		}
		b.mu.Lock()
		powered := b.status.Powered
		b.mu.Unlock()
		if !powered {
			// An unpowered adapter cannot be discoverable; the management
			// setting already denies inbound links and survives until power-on.
			return nil
		}
		if err := b.setAdapterProperty(ctx, "Discoverable", false); err != nil {
			return err
		}
		if err := b.setAdapterProperty(ctx, "Pairable", false); err != nil {
			return err
		}
		// BlueZ 5.65 may clear page scan when discoverability is turned off.
		// Keep paired reconnects available only when incoming is enabled.
		if err := b.applyConnectablePolicyLocked(ctx); err != nil {
			return err
		}
	}
	if err := b.refresh(ctx); err != nil {
		return err
	}
	return nil
}

// SetIncomingConnections controls BR/EDR page scan and LE connectable
// advertising through the public Linux management API. Turning it off denies
// every inbound link, including paired devices, while the adapter stays
// powered for BLE work; existing connected links are not torn down. Turning it
// off also clears discoverability.
func (b *BlueZ) SetIncomingConnections(ctx context.Context, on bool) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if on {
		if err := b.enableIncomingLocked(ctx); err != nil {
			return err
		}
		if err := b.refresh(ctx); err != nil {
			return err
		}
		b.publishAdapterEvent()
		return nil
	}
	if err := b.refresh(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	prevEnabled, prevConfigured := b.incomingEnabled, b.incomingConfigured
	b.incomingEnabled, b.incomingConfigured = false, true
	b.mu.Unlock()
	if err := b.applyConnectablePolicyLocked(ctx); err != nil {
		b.mu.Lock()
		b.incomingEnabled, b.incomingConfigured = prevEnabled, prevConfigured
		b.mu.Unlock()
		return err
	}
	// Keep BlueZ's own view coherent with the kernel: connectable off also
	// switches discoverability off. An unpowered adapter has no discoverable
	// state to clear, and BlueZ rejects those property writes while off.
	b.mu.Lock()
	powered := b.status.Powered
	b.mu.Unlock()
	if powered {
		if err := b.setAdapterProperty(ctx, "Discoverable", false); err != nil {
			return err
		}
		if err := b.setAdapterProperty(ctx, "Pairable", false); err != nil {
			return err
		}
	}
	if err := b.refresh(ctx); err != nil {
		return err
	}
	b.publishAdapterEvent()
	return nil
}

// enableIncomingLocked powers the adapter, arms the pairing agent and enables
// connectability. policyMu must be held.
func (b *BlueZ) enableIncomingLocked(ctx context.Context) error {
	if err := b.ensurePowered(ctx); err != nil {
		return err
	}
	// A registered agent is required for a remote device to bond; enabling
	// connectability is a user action, so report a failure instead of claiming
	// success.
	if err := b.registerAgent(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	prevEnabled, prevConfigured := b.incomingEnabled, b.incomingConfigured
	b.incomingEnabled, b.incomingConfigured = true, true
	b.mu.Unlock()
	if err := b.applyConnectablePolicyLocked(ctx); err != nil {
		b.mu.Lock()
		b.incomingEnabled, b.incomingConfigured = prevEnabled, prevConfigured
		b.mu.Unlock()
		return err
	}
	return nil
}

// applyConnectablePolicyLocked applies the stored intent. Before the daemon
// applies persisted settings there is no intent, so it does nothing rather than
// making the adapter connectable by guess. policyMu must be held.
func (b *BlueZ) applyConnectablePolicyLocked(ctx context.Context) error {
	b.mu.Lock()
	configured, enabled, adapter := b.incomingConfigured, b.incomingEnabled, b.adapter
	b.mu.Unlock()
	if !configured {
		return nil
	}
	if adapter == "" {
		return ErrUnavailable
	}
	if err := adapterSetConnectable(ctx, adapterName(adapter), enabled); err != nil {
		return err
	}
	if !enabled {
		// BlueZ adds known/paired devices to the kernel action list, and the
		// kernel keeps page scan alive for those entries even with the global
		// connectable setting off. Clear the list so "incoming off" really
		// denies paired devices instead of letting them bypass the setting.
		if err := adapterClearAcceptList(ctx, adapterName(adapter)); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.status.IncomingConnections = enabled
	b.mu.Unlock()
	return nil
}

// applyConnectablePolicy is the automatic path used after a power cycle or
// restart. The sink must already be armed when enabling, so callers order this
// after rearmA2DP.
func (b *BlueZ) applyConnectablePolicy(ctx context.Context) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	return b.applyConnectablePolicyLocked(ctx)
}

// incomingPolicyState reports the stored intent. It is used by restart paths
// to deny immediately even when the A2DP re-arm failed, while never enabling
// page scan before the sink is ready.
func (b *BlueZ) incomingPolicyState() (configured, enabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.incomingConfigured, b.incomingEnabled
}

// EnsurePowered turns the adapter on so BLE work remains possible even while
// incoming connections are disabled. It never enables page scan by itself:
// connectability belongs to the explicit incoming policy, and registration
// must arm the Audio Sink SEP before page scan makes the adapter connectable.
func (b *BlueZ) EnsurePowered(ctx context.Context) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	return b.ensurePowered(ctx)
}

func (b *BlueZ) ensurePowered(ctx context.Context) error {
	if err := b.refresh(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	powered, configured, enabled, adapter := b.status.Powered, b.incomingConfigured, b.incomingEnabled, b.adapter
	b.mu.Unlock()
	if !powered {
		// A powered controller may come up connectable. Deny first when the
		// persisted policy says so; enabling is applied only after the sink is
		// armed so a paired source cannot connect without an audio profile.
		// The public management API documents these commands as usable while
		// unpowered, but an unsupported implementation must not prevent the
		// radio from powering: the authoritative deny runs again below, after
		// power, and surfaces a real failure without leaving the radio off.
		if configured && !enabled && adapter != "" {
			_ = adapterSetConnectable(ctx, adapterName(adapter), false)
			_ = adapterClearAcceptList(ctx, adapterName(adapter))
		}
		if err := b.setAdapterProperty(ctx, "Powered", true); err != nil {
			return err
		}
		if err := b.refresh(ctx); err != nil {
			return err
		}
	}
	// BlueZ programs connectability as part of power-on. Re-assert a denied
	// policy after the fact; an enabled policy waits for the caller's arm step.
	if configured && !enabled && adapter != "" {
		if err := adapterSetConnectable(ctx, adapterName(adapter), false); err != nil {
			return err
		}
		if err := adapterClearAcceptList(ctx, adapterName(adapter)); err != nil {
			return err
		}
	}
	return nil
}

func (b *BlueZ) PairedDevices(ctx context.Context) ([]Device, error) {
	if err := b.refresh(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Device, 0, len(b.devices))
	for _, d := range b.devices {
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

func (b *BlueZ) Unpair(ctx context.Context, address string) error {
	address = strings.ToUpper(strings.TrimSpace(address))
	if address == "" {
		return fmt.Errorf("device address is required")
	}
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	path := findDevicePath(objs, address)
	if path == "" {
		return fmt.Errorf("unknown device %s", address)
	}
	// A connected device must be disconnected before its bond is removed;
	// RemoveDevice also drops the pairing.
	_ = b.conn.Object(bluezService, path).CallWithContext(ctx, "org.bluez.Device1.Disconnect", 0).Err
	if err := b.conn.Object(bluezService, b.adapterPath()).CallWithContext(ctx,
		"org.bluez.Adapter1.RemoveDevice", 0, path).Err; err != nil {
		return fmt.Errorf("remove device: %w", err)
	}
	return b.refresh(ctx)
}

// ConnectAudio asks the source to connect its Audio Source and AVRCP profiles.
// macOS and some phones otherwise leave the link at ACL/HFP and the A2DP sink
// never appears. The two profiles are connected concurrently: sequentially they
// added ~3 s of latency on a source that was already streaming, and one slow
// profile must not delay the other. The watcher publishes the resulting
// transport through signals, so no object-tree refresh is forced here.
func (b *BlueZ) ConnectAudio(ctx context.Context, address string) error {
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	path := findDevicePath(objs, address)
	if path == "" {
		return ErrNoSource
	}
	profiles := [2]struct{ name, uuid string }{
		{"a2dp-source", "0000110a-0000-1000-8000-00805f9b34fb"},
		{"avrcp", "0000110e-0000-1000-8000-00805f9b34fb"},
	}
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var failures []string
	for _, profile := range profiles {
		wg.Add(1)
		go func(name, uuid string) {
			defer wg.Done()
			var last error
			// An already-connected profile reports an error that is not a
			// failure. A real failure is retried once: on the first connection
			// after a bond the remote's profile handler is sometimes not ready
			// when BlueZ asks, and giving up left the source with no audio
			// profile until it reconnected.
			for attempt := 0; attempt < 2; attempt++ {
				callCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
				err := b.conn.Object(bluezService, path).CallWithContext(callCtx,
					"org.bluez.Device1.ConnectProfile", 0, uuid).Err
				cancel()
				if err == nil || isAlreadyConnected(err) {
					return
				}
				last = err
				if attempt == 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(250 * time.Millisecond):
					}
				}
			}
			errMu.Lock()
			failures = append(failures, fmt.Sprintf("%s: %v", name, last))
			errMu.Unlock()
		}(profile.name, profile.uuid)
	}
	wg.Wait()
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("connect audio profiles: %s", strings.Join(failures, "; "))
	}
	return nil
}

// isAlreadyConnected recognizes the benign errors BlueZ returns for a profile
// that is up or being connected by the source itself.
func isAlreadyConnected(err error) bool {
	var dbusErr dbus.Error
	if !errors.As(err, &dbusErr) {
		return false
	}
	detail := strings.ToLower(dbusErr.Name + " " + fmt.Sprint(dbusErr.Body))
	return strings.Contains(detail, "already") ||
		strings.Contains(detail, "inprogress") ||
		strings.Contains(detail, "in progress")
}

// SetTrusted marks a paired device trusted in the local BlueZ bond. Source
// devices then authorize AVRCP without an interactive agent. This is local
// state: it does not write any property on the remote device.
func (b *BlueZ) SetTrusted(ctx context.Context, address string, trusted bool) error {
	address = strings.ToUpper(strings.TrimSpace(address))
	if address == "" {
		return fmt.Errorf("device address is required")
	}
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	path := findDevicePath(objs, address)
	if path == "" {
		return fmt.Errorf("unknown device %s", address)
	}
	if err := b.conn.Object(bluezService, path).CallWithContext(ctx,
		"org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.Device1", "Trusted", dbus.MakeVariant(trusted)).Err; err != nil {
		return fmt.Errorf("set trusted: %w", err)
	}
	return b.refresh(ctx)
}

// SetTransportVolume writes the AVRCP absolute volume of the displayed
// source's transport. With avrcp_absolute_volume=0 in PulseAudio this changes
// only the remote volume, not the local source gain.
func (b *BlueZ) SetTransportVolume(ctx context.Context, level float64) error {
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	media := mediaFrom(objs)
	path, props := findVolumeTransport(objs, media.DeviceAddress)
	if path == "" {
		return ErrUnsupported
	}
	if _, ok := props["Volume"]; !ok {
		return ErrUnsupported
	}
	value := uint16(math.Round(level * 127))
	if err := b.conn.Object(bluezService, path).CallWithContext(ctx,
		"org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.MediaTransport1", "Volume", dbus.MakeVariant(value)).Err; err != nil {
		return fmt.Errorf("set transport volume: %w", err)
	}
	// Record the level we just wrote so the PropertiesChanged echo of our own
	// write is not republished as a source-side change, even when no snapshot
	// of the source's level existed before the write. The device identity keeps
	// a source switch from treating the old level as this source's echo.
	b.mu.Lock()
	b.volume, b.volumeSet, b.volumeWrote = value, true, true
	b.volumeDevice = media.DeviceAddress
	b.mu.Unlock()
	return nil
}

// TransportVolume reads the displayed source's AVRCP transport volume.
func (b *BlueZ) TransportVolume(ctx context.Context) (uint16, error) {
	objs, err := b.objects(ctx)
	if err != nil {
		return 0, err
	}
	media := mediaFrom(objs)
	path, props := findVolumeTransport(objs, media.DeviceAddress)
	if path == "" {
		return 0, ErrUnsupported
	}
	value, ok := props["Volume"]
	if !ok {
		return 0, ErrUnsupported
	}
	switch v := value.Value().(type) {
	case uint16:
		return v, nil
	case uint32:
		return uint16(v), nil
	}
	return 0, ErrUnsupported
}

func (b *BlueZ) Media(ctx context.Context) (MediaState, error) {
	if err := b.refresh(ctx); err != nil {
		return MediaState{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.media, nil
}

// SetAudioStreamProbe installs the sink owner's read-only evidence that the
// acquired transport is receiving audio for a device. It is used only to pick
// a direction for an ambiguous toggle; a command never writes it and a failed
// command leaves it untouched.
func (b *BlueZ) SetAudioStreamProbe(probe func(address string) bool) {
	b.probeMu.Lock()
	b.audioStreamProbe = probe
	b.probeMu.Unlock()
}

func (b *BlueZ) audioStreaming(address string) bool {
	b.probeMu.RLock()
	probe := b.audioStreamProbe
	b.probeMu.RUnlock()
	return probe != nil && probe(address)
}

func (b *BlueZ) Command(ctx context.Context, cmd Command) error {
	if err := b.refresh(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	present, device := b.media.Present, b.media.DeviceAddress
	b.mu.Unlock()
	if !present {
		return ErrNoSource
	}
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	target, ok := b.mediaTarget(objs, device, cmd)
	if !ok {
		return ErrUnsupported
	}
	if err := b.conn.Object(bluezService, target.path).CallWithContext(ctx, target.iface+"."+target.method, 0).Err; err != nil {
		return fmt.Errorf("%s %s: %w", target.iface, target.method, err)
	}
	// AVRCP applies commands asynchronously; the signal watcher publishes the
	// resulting state. Refresh once so the immediate reply is as fresh as BlueZ
	// allows.
	return b.refresh(ctx)
}

// mediaTarget is the public BlueZ object and method one transport command uses.
type mediaTarget struct {
	path   dbus.ObjectPath
	iface  string
	method string
}

// mediaTarget resolves a command without crossing devices: when the framework
// is showing a source, only that source's MediaPlayer1 or MediaControl1 may
// receive the command, so a second connected device that happens to be playing
// cannot capture the user's toggle or skip. A source that exposes MediaControl1
// but no MediaPlayer1 is controlled through that public interface.
func (b *BlueZ) mediaTarget(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string, cmd Command) (mediaTarget, bool) {
	// Only a toggle needs a direction; Play/Pause/Next/Previous are explicit.
	var playing bool
	if cmd == CommandToggle {
		playing = b.playingFor(objs, nil, device)
	}
	if path := findPlayerPath(objs, device); path != "" {
		props := objs[path]["org.bluez.MediaPlayer1"]
		if cmd == CommandToggle {
			playing = b.playingFor(objs, props, device)
		}
		method := playerMethod(cmd, playing)
		if method == "" {
			return mediaTarget{}, false
		}
		return mediaTarget{path: path, iface: "org.bluez.MediaPlayer1", method: method}, true
	}
	if path, _ := findControlPath(objs, device); path != "" {
		// MediaControl1 publishes no playback status; direction comes from the
		// transport or the sink receipt evidence for the same device.
		method := controlMethod(cmd, playing)
		if method == "" {
			return mediaTarget{}, false
		}
		return mediaTarget{path: path, iface: "org.bluez.MediaControl1", method: method}, true
	}
	return mediaTarget{}, false
}

// playingFor resolves playback direction from the strongest public evidence:
// a MediaPlayer1.Status when the source publishes one, then the source's own
// A2DP transport state, then the sink's packet-receipt probe (an acquired
// transport is absent from the object tree). Unknown resolves to false, which
// sends Play; it never invents a paused state after a failed command.
func (b *BlueZ) playingFor(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, playerProps map[string]dbus.Variant, device string) bool {
	if playerProps != nil {
		if status := strProp(playerProps, "Status"); status != "" {
			return status == "playing"
		}
	}
	if device == "" {
		if path, props := findPlayer(objs); path != "" {
			if status := strProp(props, "Status"); status != "" {
				return status == "playing"
			}
			device = playerDeviceAddress(objs, props)
		}
	}
	if transportPlaying(objs, device) {
		return true
	}
	return b.audioStreaming(device)
}

// transportPlaying reports whether BlueZ currently shows the same device's
// A2DP transport as actively streaming. An idle transport means the source is
// paused even though its connection stays open.
func transportPlaying(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string) bool {
	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaTransport1"]
		if !ok || strProp(props, "State") != "active" {
			continue
		}
		if device == "" {
			return true
		}
		if strings.EqualFold(deviceAddressAt(objs, objectPathProp(props, "Device")), device) {
			return true
		}
	}
	return false
}

// Watch subscribes to connect/disconnect and media changes.
func (b *BlueZ) Watch(ctx context.Context) <-chan Event {
	ch := make(chan Event, watchEventBuffer)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch
	}
	b.nextID++
	id := b.nextID
	b.subs[id] = ch
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub)
		}
		b.mu.Unlock()
	}()
	return ch
}

// --- BlueZ details ----------------------------------------------------------

func (b *BlueZ) adapterPath() dbus.ObjectPath {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.adapter
}

func (b *BlueZ) setAdapterProperty(ctx context.Context, name string, value any) error {
	path := b.adapterPath()
	if path == "" {
		return ErrUnavailable
	}
	err := b.conn.Object(bluezService, path).CallWithContext(ctx,
		"org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.Adapter1", name, dbus.MakeVariant(value)).Err
	if err != nil {
		return fmt.Errorf("set adapter %s: %w", name, err)
	}
	return nil
}

func (b *BlueZ) objects(ctx context.Context) (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := b.conn.Object(bluezService, bluezRoot).CallWithContext(ctx,
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs)
	if err != nil {
		var dbusErr dbus.Error
		if errors.As(err, &dbusErr) && dbusErr.Name == "org.freedesktop.DBus.Error.ServiceUnknown" {
			return nil, fmt.Errorf("%w: org.bluez is not running", ErrUnavailable)
		}
		return nil, fmt.Errorf("bluez objects: %w", err)
	}
	return objs, nil
}

// refresh reads the object tree and publishes connect/disconnect/media changes.
func (b *BlueZ) refresh(ctx context.Context) error {
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()

	objs, err := b.objects(ctx)
	if err != nil {
		var unavailable error
		if errors.Is(err, ErrUnavailable) {
			unavailable = err
		}
		b.mu.Lock()
		b.status = Status{Detail: err.Error()}
		b.mu.Unlock()
		// The adapter could not be inspected; a registration from an earlier
		// read is no longer verifiable, so do not report it as ready.
		b.markA2DPUnarmed()
		return unavailable
	}

	adapterPath, adapterProps := findAdapter(objs)
	b.mu.Lock()
	prevAdapter, prevStatus := b.adapter, b.status
	b.mu.Unlock()
	status := Status{
		Present: adapterPath != "",
		Adapter: adapterName(adapterPath),
		Detail:  "BlueZ",
	}
	if status.Present {
		status.Powered = boolProp(adapterProps, "Powered")
		status.Discoverable = boolProp(adapterProps, "Discoverable")
		status.Name = adapterDisplayName(adapterProps, status.Adapter)
		if !status.Powered {
			status.Detail = "BlueZ adapter is powered off"
		}
		// Connectability is observed through the kernel management API, not
		// Adapter1. Carry the last observation only for the same powered
		// adapter; a replacement or power cycle invalidates it.
		if adapterPath == prevAdapter && status.Powered {
			status.IncomingConnections = prevStatus.IncomingConnections
		}
	} else {
		status.Detail = "no BlueZ adapter"
	}
	devices := devicesFrom(objs)
	media := mediaFrom(objs)

	b.mu.Lock()
	b.adapter = adapterPath
	b.status = status
	b.devices = devices
	nextConnected := map[string]string{}
	for _, d := range devices {
		if d.Connected {
			nextConnected[d.Address] = d.Name
		}
	}
	var events []Event
	for addr, name := range nextConnected {
		if _, ok := b.connected[addr]; !ok {
			events = append(events, Event{Kind: EventConnected, DeviceAddress: addr, DeviceName: name, Media: media})
		}
	}
	for addr, name := range b.connected {
		if _, ok := nextConnected[addr]; !ok {
			events = append(events, Event{Kind: EventDisconnected, DeviceAddress: addr, DeviceName: name, Media: media})
		}
	}
	b.connected = nextConnected

	if prevStatus.Present != status.Present || prevStatus.Powered != status.Powered ||
		prevStatus.Discoverable != status.Discoverable || prevStatus.Name != status.Name ||
		prevStatus.Adapter != status.Adapter {
		events = append(events, Event{Kind: EventAdapter, Media: media})
	}

	if mediaChanged(b.media, media) {
		b.revision++
		media.Revision = b.revision
		events = append(events, Event{Kind: EventMedia, Media: media})
	} else {
		media.Revision = b.media.Revision
	}

	// AVRCP absolute volume, scoped to the same connected source the framework
	// displays. A background phone's transport must not move the master.
	if media.DeviceAddress != b.volumeDevice {
		b.volume, b.volumeSet, b.volumeWrote = 0, false, false
		b.volumeDevice = media.DeviceAddress
	}
	if volume, ok := transportVolume(objs, media.DeviceAddress); ok {
		publish, echo := volumeObservation(b.volume, b.volumeSet, b.volumeWrote, volume)
		switch {
		case echo:
			b.volumeSet = true
		case publish:
			b.volume, b.volumeSet = volume, true
			events = append(events, Event{
				Kind: EventVolume, Volume: volume, Media: media,
				DeviceAddress: media.DeviceAddress, DeviceName: media.DeviceName,
			})
		}
		b.volumeWrote = false
	} else {
		// No transport: the next source's first level is initial state again.
		b.volumeSet = false
		b.volumeWrote = false
		b.volume = 0
	}
	// A pending or active transport is the point at which the stream can be
	// acquired; publish the transition instead of having callers poll for it.
	if key := usableTransportKey(objs); key != b.transportKey {
		b.transportKey = key
		if key != "" {
			events = append(events, Event{
				Kind: EventTransport, Media: media,
				DeviceAddress: media.DeviceAddress, DeviceName: media.DeviceName,
			})
		}
	}

	b.media = media
	b.mu.Unlock()

	if endpointRegistrationInvalidated(prevAdapter, adapterPath, prevStatus, status) {
		// BlueZ drops endpoint registrations when the adapter is power-cycled
		// or replaced. The watcher re-arms on the next signal; until that
		// succeeds SinkRegistered reports not ready.
		b.markA2DPUnarmed()
	}

	if len(events) > 0 {
		// Deliver under the same mutex that closes subscriptions. deliverEvent
		// is non-blocking, so a concurrent Watch cancellation cannot panic on
		// a closed channel and this cannot stall the refresh.
		b.mu.Lock()
		for _, event := range events {
			for _, ch := range b.subs {
				deliverEvent(ch, event)
			}
		}
		b.mu.Unlock()
	}
	return nil
}

// publishAdapterEvent invalidates observers after a user-visible adapter policy
// change that has no D-Bus property signal (connectability).
func (b *BlueZ) publishAdapterEvent() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		deliverEvent(ch, Event{Kind: EventAdapter})
	}
}

func (b *BlueZ) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := b.watchOnce(ctx); err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			// Re-seed after a bus gap so the next refresh compares real state.
			_ = b.refresh(ctx)
		}
	}
}

// reconcilePower turns the adapter back on after a BlueZ restart or a manual
// power-off. The framework wants the radio available for BLE even while
// incoming connections are disabled; the user-facing switches are incoming
// connections and discoverability, not adapter power.
func (b *BlueZ) reconcilePower(ctx context.Context) {
	if !b.powering.CompareAndSwap(false, true) {
		return
	}
	defer b.powering.Store(false)
	b.mu.Lock()
	present, powered := b.status.Present, b.status.Powered
	b.mu.Unlock()
	if !present || powered {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.setAdapterProperty(callCtx, "Powered", true); err != nil {
		return
	}
	_ = b.refresh(callCtx)
	// Powering the adapter discards the endpoints registered with it, so the
	// sink profile must be advertised again before page scan makes the adapter
	// connectable again; a source that connects in between finds no profile.
	if err := b.rearmA2DP(callCtx); err != nil {
		if configured, enabled := b.incomingPolicyState(); configured && enabled {
			return
		}
	}
	_ = b.applyConnectablePolicy(callCtx)
}

// watchOnce installs the match rules and refreshes on any relevant signal.
// Transport volume changes refresh immediately so a source-side volume change
// reaches the framework without the coalescing delay used for the discovery
// signal bursts.
func (b *BlueZ) watchOnce(ctx context.Context) error {
	sigCh := make(chan *dbus.Signal, 128)
	b.conn.Signal(sigCh)
	defer b.conn.RemoveSignal(sigCh)

	matches := [][]dbus.MatchOption{
		{dbus.WithMatchSender(bluezService), dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchPathNamespace(bluezNamespace)},
		{dbus.WithMatchSender(bluezService), dbus.WithMatchInterface("org.freedesktop.DBus.ObjectManager"), dbus.WithMatchPathNamespace(bluezNamespace)},
		{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged")},
	}
	for _, opts := range matches {
		if err := b.conn.AddMatchSignal(opts...); err != nil {
			return err
		}
	}
	defer func() {
		for _, opts := range matches {
			_ = b.conn.RemoveMatchSignal(opts...)
		}
	}()

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case sig, ok := <-sigCh:
			if !ok {
				return fmt.Errorf("bluez signal channel closed")
			}
			if transportVolumeSignal(sig) {
				_ = b.refresh(ctx)
				b.reconcilePower(ctx)
				continue
			}
			if bluezRestarted(sig) {
				// BlueZ forgets endpoint registrations and transports when
				// the service restarts; advertise the sink profile again.
				b.mu.Lock()
				b.transportKey = ""
				b.mu.Unlock()
				b.markA2DPUnarmed()
				_ = b.refresh(ctx)
				b.reconcilePower(ctx)
				if err := b.rearmA2DP(ctx); err != nil {
					if configured, enabled := b.incomingPolicyState(); configured && enabled {
						_ = b.registerAgent(ctx)
						continue
					}
				}
				_ = b.applyConnectablePolicy(ctx)
				_ = b.registerAgent(ctx)
				continue
			}
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(120 * time.Millisecond)
		case <-debounce.C:
			_ = b.refresh(ctx)
			b.reconcilePower(ctx)
			configured, enabled := b.incomingPolicyState()
			if configured && !enabled {
				// A manual power cycle or another BlueZ client can re-enable
				// connectability and repopulate the known-device list. Reassert
				// the user's deny decision on the resulting signal.
				_ = b.applyConnectablePolicy(ctx)
				continue
			}
			if configured {
				b.mu.Lock()
				hidden, adapter := !b.status.Discoverable, b.adapter
				b.mu.Unlock()
				// BlueZ 5.65 can clear page scan when discoverability is
				// turned off. While hidden, incoming must stay enabled for
				// paired reconnects; reassert only when the kernel reports it
				// was cleared instead of writing on every signal.
				if hidden && adapter != "" {
					if on, ok, err := adapterReadConnectable(ctx, adapterName(adapter)); ok && !on && err == nil {
						_ = b.applyConnectablePolicy(ctx)
					}
				}
			}
			if !b.SinkRegistered() {
				if err := b.rearmA2DP(ctx); err != nil {
					if configured, enabled := b.incomingPolicyState(); configured && enabled {
						continue
					}
				}
				_ = b.applyConnectablePolicy(ctx)
			}
		}
	}
}

// --- object parsing ---------------------------------------------------------

func findAdapter(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) (dbus.ObjectPath, map[string]dbus.Variant) {
	for path, ifaces := range objs {
		if props, ok := ifaces["org.bluez.Adapter1"]; ok {
			return path, props
		}
	}
	return "", nil
}

func adapterName(path dbus.ObjectPath) string {
	name := strings.TrimPrefix(string(path), bluezNamespace+"/")
	return name
}

// adapterDisplayName returns the name the adapter actually advertises. Alias is
// the user-visible BlueZ name; Name is the system fallback. The Wi-Fi AP uses
// this value as its SSID so the speaker is recognisable on both radios.
func adapterDisplayName(props map[string]dbus.Variant, fallback string) string {
	if name := strings.TrimSpace(strProp(props, "Alias")); name != "" {
		return name
	}
	if name := strings.TrimSpace(strProp(props, "Name")); name != "" {
		return name
	}
	return fallback
}

func devicesFrom(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) []Device {
	out := make([]Device, 0, len(objs))
	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		address := strProp(props, "Address")
		name := strProp(props, "Alias")
		if name == "" {
			name = strProp(props, "Name")
		}
		if name == "" {
			name = address
		}
		out = append(out, Device{
			Address: address, Name: name, Paired: boolProp(props, "Paired"),
			Connected: boolProp(props, "Connected"), Trusted: boolProp(props, "Trusted"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

func mediaFrom(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) MediaState {
	playerPath, playerProps := findPlayer(objs)
	if playerPath != "" {
		state := MediaState{
			Present: true, Playing: strProp(playerProps, "Status") == "playing",
			PositionMS: uint64(uint32Prop(playerProps, "Position")),
		}
		if trackVar, ok := playerProps["Track"]; ok {
			if track, ok := trackVar.Value().(map[string]dbus.Variant); ok {
				state.Title = strProp(track, "Title")
				state.Artist = strProp(track, "Artist")
				state.Album = strProp(track, "Album")
				state.DurationMS = uint64(uint32Prop(track, "Duration"))
			}
		}
		if dev := objectPathProp(playerProps, "Device"); dev != "" {
			if _, ok := objs[dev]; ok {
				state.DeviceAddress, state.DeviceName = deviceIdentity(objs, dev)
			}
		}
		if state.Title == "" && state.Artist == "" && state.Album == "" {
			state.Detail = "the source reports playback state without track metadata"
		}
		return state
	}

	// MediaControl1 is exported on the device object itself, so the identity
	// comes from that object. Using any connected device here would show (and
	// later command) the wrong source with two devices connected.
	for _, path := range devicePaths(objs) {
		props, ok := objs[path]["org.bluez.MediaControl1"]
		if !ok || !boolProp(props, "Connected") {
			continue
		}
		state := MediaState{Present: true, Detail: "source exposes AVRCP controls but no track metadata"}
		state.DeviceAddress, state.DeviceName = deviceIdentity(objs, path)
		return state
	}

	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaTransport1"]
		if !ok {
			continue
		}
		switch strProp(props, "State") {
		case "active", "pending":
			state := MediaState{Present: true, Detail: "audio transport is active; the source exposes no AVRCP player"}
			if dev := objectPathProp(props, "Device"); dev != "" {
				if _, ok := objs[dev]; ok {
					state.DeviceAddress, state.DeviceName = deviceIdentity(objs, dev)
				}
			}
			return state
		}
	}

	// A source is connected before BlueZ publishes a player, control or
	// transport (an ACL-only connect, or a paused source whose player object is
	// gone). Present means a source is connected, not that it is playing, so a
	// device that exposes an audio-source or AVRCP service still counts. It must
	// not disappear from the framework and the UI just because it paused.
	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if !ok || !boolProp(props, "Connected") || !audioSourceDevice(props) {
			continue
		}
		name := strProp(props, "Alias")
		if name == "" {
			name = strProp(props, "Name")
		}
		return MediaState{
			Present: true, DeviceAddress: strProp(props, "Address"), DeviceName: name,
			Detail: "source connected; it exposes no media player",
		}
	}
	return MediaState{Detail: "no Bluetooth source connected"}
}

// audioSourceUUIDs are the remote services that make a connected device an
// audio source for this sink: Audio Source, AVRCP Target and AVRCP Controller.
// A keyboard or a gamepad exposes none of them and is not reported as media.
var audioSourceUUIDs = []string{
	"0000110a-0000-1000-8000-00805f9b34fb",
	"0000110c-0000-1000-8000-00805f9b34fb",
	"0000110e-0000-1000-8000-00805f9b34fb",
}

func audioSourceDevice(props map[string]dbus.Variant) bool {
	v, ok := props["UUIDs"]
	if !ok {
		return false
	}
	uuids, _ := v.Value().([]string)
	for _, uuid := range uuids {
		for _, want := range audioSourceUUIDs {
			if strings.EqualFold(uuid, want) {
				return true
			}
		}
	}
	return false
}

// findPlayer picks the player the framework shows. Object maps have no order,
// so candidates are sorted by path: with two connected sources and neither
// playing, repeated refreshes must not flap between their metadata and device.
// A playing player still wins over a paused one.
func findPlayer(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) (dbus.ObjectPath, map[string]dbus.Variant) {
	paths := make([]dbus.ObjectPath, 0, len(objs))
	for path, ifaces := range objs {
		if _, ok := ifaces["org.bluez.MediaPlayer1"]; ok {
			paths = append(paths, path)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
	var best dbus.ObjectPath
	var bestProps map[string]dbus.Variant
	bestPlaying := false
	for _, path := range paths {
		props := objs[path]["org.bluez.MediaPlayer1"]
		playing := strProp(props, "Status") == "playing"
		if best == "" || (playing && !bestPlaying) {
			best, bestProps, bestPlaying = path, props, playing
		}
	}
	return best, bestProps
}

// findPlayerPath picks the player a transport command targets. When the
// framework is showing a source, only that source's players are eligible (the
// one already playing first, then deterministic path order): a player
// belonging to another connected device must never capture the command, which
// is how a second connected phone could otherwise receive the user's skip.
// Without a displayed source the playing player, then the deterministic first,
// is used.
func findPlayerPath(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, preferredAddress string) dbus.ObjectPath {
	if preferredAddress == "" {
		best, _ := findPlayer(objs)
		return best
	}
	var fallback dbus.ObjectPath
	for _, path := range playerPaths(objs) {
		props := objs[path]["org.bluez.MediaPlayer1"]
		if !strings.EqualFold(playerDeviceAddress(objs, props), preferredAddress) {
			continue
		}
		if strProp(props, "Status") == "playing" {
			return path
		}
		if fallback == "" {
			fallback = path
		}
	}
	return fallback
}

// playerPaths returns every MediaPlayer1 object path in deterministic order.
func playerPaths(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) []dbus.ObjectPath {
	paths := make([]dbus.ObjectPath, 0, len(objs))
	for path, ifaces := range objs {
		if _, ok := ifaces["org.bluez.MediaPlayer1"]; ok {
			paths = append(paths, path)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
	return paths
}

// playerDeviceAddress resolves a MediaPlayer1 object's Device property to the
// remote device address without falling back to any connected device.
func playerDeviceAddress(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, playerProps map[string]dbus.Variant) string {
	dev := objectPathProp(playerProps, "Device")
	if dev == "" {
		return ""
	}
	ifaces, ok := objs[dev]
	if !ok {
		return ""
	}
	return strProp(ifaces["org.bluez.Device1"], "Address")
}

// findControlPath picks the deprecated org.bluez.MediaControl1 object for a
// command. That interface is exported on the device object itself and publishes
// no status, only Connected, so a connected control for a *different* source is
// never used: with a displayed source, a missing control means the command is
// unsupported rather than silently sent to the wrong device.
func findControlPath(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, preferredAddress string) (dbus.ObjectPath, map[string]dbus.Variant) {
	var first dbus.ObjectPath
	var firstProps map[string]dbus.Variant
	for _, path := range devicePaths(objs) {
		props, ok := objs[path]["org.bluez.MediaControl1"]
		if !ok || !boolProp(props, "Connected") {
			continue
		}
		if first == "" {
			first, firstProps = path, props
		}
		if preferredAddress != "" {
			if strings.EqualFold(deviceAddressAt(objs, path), preferredAddress) {
				return path, props
			}
		}
	}
	if preferredAddress != "" {
		return "", nil
	}
	return first, firstProps
}

// deviceAddressAt resolves the address of exactly the given device object and
// never falls back to another connected device.
func deviceAddressAt(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, path dbus.ObjectPath) string {
	if path == "" {
		return ""
	}
	return strProp(objs[path]["org.bluez.Device1"], "Address")
}

// devicePaths returns every org.bluez.Device1 object path in deterministic
// order so a media fallback cannot depend on map iteration.
func devicePaths(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) []dbus.ObjectPath {
	paths := make([]dbus.ObjectPath, 0, len(objs))
	for path, ifaces := range objs {
		if _, ok := ifaces["org.bluez.Device1"]; ok {
			paths = append(paths, path)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
	return paths
}

// findVolumeTransport prefers an active transport, then pending, then any
// transport that exposes Volume, but only for the connected source the
// framework displays. AVRCP absolute volume is available before the audio
// stream becomes active (the property exists while State=idle), and the
// framework volume must reach the displayed source at that point too. A
// background source's transport must never receive the framework write or
// report its own level as the framework's source change.
func findVolumeTransport(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string) (dbus.ObjectPath, map[string]dbus.Variant) {
	if device == "" {
		return "", nil
	}
	var idlePath, pendingPath dbus.ObjectPath
	var idleProps, pendingProps map[string]dbus.Variant
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaTransport1"]
		if !ok {
			continue
		}
		if !strings.EqualFold(deviceAddressAt(objs, objectPathProp(props, "Device")), device) {
			continue
		}
		if _, hasVolume := props["Volume"]; !hasVolume {
			continue
		}
		switch strProp(props, "State") {
		case "active":
			return path, props
		case "pending":
			pendingPath, pendingProps = path, props
		default:
			if idlePath == "" {
				idlePath, idleProps = path, props
			}
		}
	}
	if pendingPath != "" {
		return pendingPath, pendingProps
	}
	return idlePath, idleProps
}

// transportVolume reads the AVRCP absolute volume of the display source's
// transport. ok is false when no transport exposes the property, i.e. the
// source does not support absolute volume.
func transportVolume(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string) (uint16, bool) {
	path, props := findVolumeTransport(objs, device)
	if path == "" {
		return 0, false
	}
	switch v := props["Volume"].Value().(type) {
	case uint16:
		return v, true
	case uint32:
		return uint16(v), true
	}
	return 0, false
}

// volumeObservation decides what an observed transport level means.
//
//	publish: the level is a source-side change (or the first observation, which
//	         must still reach the framework even without a prior snapshot).
//	echo:    the level is exactly what this manager wrote before any snapshot,
//	         so publishing it would loop the framework's own write back.
func volumeObservation(last uint16, haveLast, wrote bool, observed uint16) (publish, echo bool) {
	if !haveLast {
		if wrote && last == observed {
			return false, true
		}
		return true, false
	}
	return last != observed, false
}

// usableTransportKey identifies the A2DP transport a stream can be acquired
// from right now. It is empty while the endpoint is configured but idle: an
// idle transport stays silent and acquiring it blocks.
func usableTransportKey(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) string {
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaTransport1"]
		if !ok || strProp(props, "UUID") != a2dpSinkUUID {
			continue
		}
		state := strProp(props, "State")
		if state != "pending" && state != "active" {
			continue
		}
		if codec, ok := props["Codec"]; ok && byteValue(codec) != 0 {
			continue
		}
		return string(path) + "|" + state
	}
	return ""
}

// transportVolumeSignal matches the PropertiesChanged BlueZ emits when the
// source changes its AVRCP absolute volume.
func transportVolumeSignal(sig *dbus.Signal) bool {
	if sig == nil || sig.Name != "org.freedesktop.DBus.Properties.PropertiesChanged" || len(sig.Body) < 2 {
		return false
	}
	iface, _ := sig.Body[0].(string)
	if iface != "org.bluez.MediaTransport1" {
		return false
	}
	changed, _ := sig.Body[1].(map[string]dbus.Variant)
	_, ok := changed["Volume"]
	return ok
}

// bluezRestarted reports a NameOwnerChanged for the BlueZ service.
func bluezRestarted(sig *dbus.Signal) bool {
	if sig == nil || sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) < 1 {
		return false
	}
	name, _ := sig.Body[0].(string)
	return name == bluezService
}

func findDevicePath(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, address string) dbus.ObjectPath {
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if ok && strings.EqualFold(strProp(props, "Address"), address) {
			return path
		}
	}
	return ""
}

func deviceIdentity(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, dev dbus.ObjectPath) (string, string) {
	if dev != "" {
		if ifaces, ok := objs[dev]; ok {
			if props, ok := ifaces["org.bluez.Device1"]; ok {
				name := strProp(props, "Alias")
				if name == "" {
					name = strProp(props, "Name")
				}
				return strProp(props, "Address"), name
			}
		}
	}
	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if ok && boolProp(props, "Connected") {
			name := strProp(props, "Alias")
			if name == "" {
				name = strProp(props, "Name")
			}
			return strProp(props, "Address"), name
		}
	}
	return "", ""
}

func playerMethod(cmd Command, playing bool) string {
	switch cmd {
	case CommandPlay:
		return "Play"
	case CommandPause:
		return "Pause"
	case CommandNext:
		return "Next"
	case CommandPrevious:
		return "Previous"
	case CommandToggle:
		if playing {
			return "Pause"
		}
		return "Play"
	default:
		return ""
	}
}

func controlMethod(cmd Command, playing bool) string {
	switch cmd {
	case CommandPlay:
		return "Play"
	case CommandPause:
		return "Pause"
	case CommandNext:
		return "Next"
	case CommandPrevious:
		return "Previous"
	case CommandToggle:
		if playing {
			return "Pause"
		}
		return "Play"
	default:
		return ""
	}
}

func mediaChanged(prev, next MediaState) bool {
	return prev.Present != next.Present ||
		prev.Playing != next.Playing ||
		prev.DeviceAddress != next.DeviceAddress ||
		prev.DeviceName != next.DeviceName ||
		prev.Title != next.Title ||
		prev.Artist != next.Artist ||
		prev.Album != next.Album ||
		prev.DurationMS != next.DurationMS ||
		prev.Detail != next.Detail
}

// --- variant helpers --------------------------------------------------------

func strProp(props map[string]dbus.Variant, key string) string {
	if v, ok := props[key]; ok {
		s, _ := v.Value().(string)
		return s
	}
	return ""
}

func boolProp(props map[string]dbus.Variant, key string) bool {
	if v, ok := props[key]; ok {
		b, _ := v.Value().(bool)
		return b
	}
	return false
}

func uint32Prop(props map[string]dbus.Variant, key string) uint32 {
	if v, ok := props[key]; ok {
		switch n := v.Value().(type) {
		case uint32:
			return n
		case uint64:
			return uint32(n)
		case int32:
			return uint32(n)
		case int64:
			return uint32(n)
		}
	}
	return 0
}

func objectPathProp(props map[string]dbus.Variant, key string) dbus.ObjectPath {
	if v, ok := props[key]; ok {
		p, _ := v.Value().(dbus.ObjectPath)
		return p
	}
	return ""
}
