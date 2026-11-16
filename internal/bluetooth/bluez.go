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

// Evidence a transport command's direction came from. They are the reasons
// logged with a resolved toggle so a field report can tell a lagging peer status
// from a sink-side receipt, and the evidence a published playback direction
// carries.
const (
	// EvidenceStatus is the source's cached MediaPlayer1.Status.
	EvidenceStatus = "status"
	// EvidenceTransport is the source's A2DP transport state as BlueZ sees it.
	EvidenceTransport = "transport"
	// EvidenceReceipt is this sink's own packet receipts on the acquired
	// transport.
	EvidenceReceipt = "receipt"
	// EvidenceMemory is the direction this daemon commanded moments ago.
	EvidenceMemory = "memory"
)

// commandMemoryWindow is how long a direction this daemon commanded stays
// authoritative over a lagging peer status. It has to cover the peer's AVRCP
// notification latency (tens to hundreds of milliseconds, worst case a couple of
// seconds on a congested link) without overriding a deliberate later tap.
const commandMemoryWindow = 2 * time.Second

// commandMemoryDevices caps the per-device direction memory so a long bond list
// cannot grow it without bound.
const commandMemoryDevices = 8

// commandRecord is the direction this daemon last commanded for one device.
type commandRecord struct {
	method string
	at     time.Time
}

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
	// volumePending is the level the framework asked for while the source had
	// not reported absolute volume yet. BlueZ creates MediaTransport1.Volume
	// only from a peer report and refuses a write that would create it, so the
	// request is remembered and re-asserted when the property finally appears
	// instead of adopting the peer's stale level.
	volumePending       uint16
	volumePendingDevice string
	// volumeAbsent is set when a write was refused for lack of the property; it
	// feeds the media/status detail and the once-per-session warning. It is
	// cleared by the first observed level for the same source.
	volumeAbsent         bool
	volumeAbsentDevice   string
	volumeAbsentNotified string
	// volumeMissingDevice and volumeMissingSince record the source whose
	// transport exists but has not reported a level yet, and when a write was
	// first refused for that. Inside volumeGraceWindow it is "not reported
	// yet", not a verdict: the peer commonly reports a moment after it starts
	// streaming, and concluding earlier is what told the user the source
	// "does not expose" volume while it was about to.
	volumeMissingDevice string
	volumeMissingSince  time.Time
	transportKey        string

	// Command direction memory. A source's MediaPlayer1.Status is BlueZ's cache
	// of the peer's AVRCP notifications and stays stale for a moment after a
	// command this daemon sent; remembering our own decision long enough to
	// invert it keeps a second tap from repeating the first.
	cmdMu       sync.Mutex
	cmdMemory   map[string]commandRecord
	cmdObserver func(CommandObservation)
	// now is the wall clock used for the command-memory window. Tests replace
	// it; production uses time.Now.
	now func() time.Time

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
	// acquired transport is receiving audio right now. While this sink owns the
	// transport it is the authoritative toggle evidence (an acquired
	// MediaTransport1 does stay in BlueZ 5.65's object tree, but its State does
	// not change when the source stops sending, and a source may expose no
	// MediaPlayer1.Status at all). It is consulted for a toggle only; it never
	// becomes published playback state.
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

// SetAdapterName writes Adapter1.Alias. BlueZ persists the alias across
// restarts and advertises it, so the adapter is not power-cycled and no
// policy state is touched.
func (b *BlueZ) SetAdapterName(ctx context.Context, name string) error {
	trimmed, err := ValidateAdapterName(name)
	if err != nil {
		return err
	}
	// Resolve the current adapter path first: a BlueZ restart can leave the
	// cached path stale, and the validation rejection above already avoided
	// any bus round trip for a bad name.
	if err := b.refresh(ctx); err != nil {
		return err
	}
	if err := b.setAdapterProperty(ctx, "Alias", trimmed); err != nil {
		return err
	}
	if err := b.refresh(ctx); err != nil {
		return err
	}
	// The Alias PropertiesChanged signal also refreshes, but publishing here
	// makes the change observable before the watcher re-arms.
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

// SetTransportVolume writes the AVRCP absolute volume of the displayed source's
// transport. With avrcp_absolute_volume=0 in PulseAudio this changes only the
// remote volume, not the local source gain.
//
// The target is resolved without depending on the object tree alone: the
// transport this sink acquired is tried first (an acquired MediaTransport1 stays
// in the tree in BlueZ 5.65, but the recorded path cannot lag the Acquire reply),
// then the tree lookup, then the source's A2DP transport even when it exposes no
// Volume property - which is how a source without absolute volume is told apart
// from a source without a transport.
//
// The write is always attempted when a transport exists: a source that has not
// reported its level yet is common right after it starts streaming, and only a
// refusal (or the grace window) is evidence that it has none.
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
	if !media.Present || media.DeviceAddress == "" {
		return ErrNoSource
	}
	path, _, _ := b.volumeTarget(objs, media.DeviceAddress)
	if path == "" {
		return ErrNoTransport
	}
	value := uint16(math.Round(level * 127))
	if err := b.writeTransportVolume(ctx, path, value); err != nil {
		if !isUnknownProperty(err) {
			return fmt.Errorf("set transport volume: %w", err)
		}
		// BlueZ refused the property: it has not been created yet (or is
		// read-only). Record the request so it can be re-asserted when the peer
		// finally reports a level, and only diagnose a source that stays
		// silent past the grace window.
		if b.noteAbsoluteVolumeMissing(media.DeviceAddress, value) {
			return fmt.Errorf("%w (source %s): %v", ErrNoAbsoluteVolume, media.DeviceAddress, err)
		}
		return fmt.Errorf("%w: source %s has not reported AVRCP absolute volume yet",
			ErrNoAbsoluteVolume, media.DeviceAddress)
	}
	// Record the level we just wrote so the PropertiesChanged echo of our own
	// write is not republished as a source-side change, even when no snapshot
	// of the source's level existed before the write. The device identity keeps
	// a source switch from treating the old level as this source's echo.
	b.mu.Lock()
	b.volume, b.volumeSet, b.volumeWrote = value, true, true
	b.volumeDevice = media.DeviceAddress
	b.volumePending, b.volumePendingDevice = 0, ""
	b.volumeAbsent, b.volumeAbsentDevice = false, ""
	b.volumeAbsentNotified = ""
	b.volumeMissingDevice, b.volumeMissingSince = "", time.Time{}
	b.mu.Unlock()
	return nil
}

// writeTransportVolume is the single org.bluez.MediaTransport1.Volume write
// site. BlueZ answers success before the AVRCP SetAbsoluteVolume reaches the
// peer, so a nil error means the property was written, not that the peer obeyed.
func (b *BlueZ) writeTransportVolume(ctx context.Context, path dbus.ObjectPath, value uint16) error {
	return b.conn.Object(bluezService, path).CallWithContext(ctx,
		"org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.MediaTransport1", "Volume", dbus.MakeVariant(value)).Err
}

// volumeTarget resolves the transport that carries the displayed source's AVRCP
// absolute volume. hasVolume reports whether that transport currently exposes
// the Volume property, which BlueZ only creates from a peer report.
func (b *BlueZ) volumeTarget(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string) (dbus.ObjectPath, map[string]dbus.Variant, bool) {
	if device == "" {
		return "", nil, false
	}
	// The transport this sink acquired, when it belongs to the displayed source.
	b.endpointMu.Lock()
	owned := b.transport
	b.endpointMu.Unlock()
	if owned != "" {
		if props, ok := objs[owned]["org.bluez.MediaTransport1"]; ok {
			dev := objectPathProp(props, "Device")
			if dev == "" || strings.EqualFold(deviceAddressAt(objs, dev), device) {
				_, hasVolume := props["Volume"]
				return owned, props, hasVolume
			}
		}
	}
	if path, props := findVolumeTransport(objs, device); path != "" {
		return path, props, true
	}
	// No transport exposes Volume: report the source's A2DP transport anyway so
	// the caller can distinguish "no absolute volume" from "no transport".
	if path, props := a2dpTransportFor(objs, findDevicePath(objs, device)); path != "" {
		return path, props, false
	}
	return "", nil, false
}

// volumeGraceWindow is how long a source may have a transport without a
// writable absolute volume before that counts as a verdict. BlueZ creates
// MediaTransport1.Volume only from the peer's first report, which commonly
// arrives a moment after the transport is configured; concluding earlier told
// the user the source "does not expose" volume while it was about to.
const volumeGraceWindow = 3 * time.Second

// noteAbsoluteVolumeMissing records a refused volume write for the source and
// remembers the level to reach once it reports one. It reports whether the
// source is now diagnosed as having no writable absolute volume: only then does
// the first such record in a source session publish EventVolumeUnavailable, so
// the user and the log learn why the host volume did not move. Inside the grace
// window the refusal is "not reported yet" and stays quiet.
func (b *BlueZ) noteAbsoluteVolumeMissing(device string, level uint16) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clockNow()
	b.volumePending, b.volumePendingDevice = level, device
	if !strings.EqualFold(b.volumeMissingDevice, device) {
		b.volumeMissingDevice, b.volumeMissingSince = device, now
		return false
	}
	if now.Before(b.volumeMissingSince) {
		// A clock that went backwards must not shorten the window; restart it.
		b.volumeMissingSince = now
		return false
	}
	if now.Sub(b.volumeMissingSince) < volumeGraceWindow {
		return false
	}
	b.volumeAbsent, b.volumeAbsentDevice = true, device
	if strings.EqualFold(b.volumeAbsentNotified, device) {
		return true
	}
	b.volumeAbsentNotified = device
	event := Event{
		Kind: EventVolumeUnavailable, DeviceAddress: device, Media: b.media,
		Detail: fmt.Sprintf("source %s does not expose AVRCP absolute volume: BlueZ has no MediaTransport1.Volume to write, so the master level is applied locally only", device),
	}
	for _, ch := range b.subs {
		deliverEvent(ch, event)
	}
	return true
}

// pendingVolumeLocked consumes the level requested before the source reported
// absolute volume. It reports false when that level already equals what the peer
// reported, in which case there is nothing to re-assert.
func (b *BlueZ) pendingVolumeLocked(device string, observed uint16) (uint16, bool) {
	if b.volumePendingDevice == "" || !strings.EqualFold(b.volumePendingDevice, device) {
		return 0, false
	}
	level := b.volumePending
	b.volumePending, b.volumePendingDevice = 0, ""
	return level, level != observed
}

// volumeAbsentDetailLocked is the bounded sentence the media state carries when
// the displayed source has no writable absolute volume. hasVolume is the current
// observation: the sentence disappears the moment the peer reports a level.
// Until the grace window passes it says the level has not been reported yet,
// which is what is actually known.
func (b *BlueZ) volumeAbsentDetailLocked(device string, hasVolume bool) string {
	if hasVolume {
		return ""
	}
	if b.volumeAbsent && strings.EqualFold(b.volumeAbsentDevice, device) {
		return "the source does not expose AVRCP absolute volume"
	}
	if b.volumeMissingDevice != "" && strings.EqualFold(b.volumeMissingDevice, device) {
		return "the source has not reported AVRCP absolute volume yet"
	}
	return ""
}

// TransportVolume reads the displayed source's AVRCP transport volume.
func (b *BlueZ) TransportVolume(ctx context.Context) (uint16, error) {
	objs, err := b.objects(ctx)
	if err != nil {
		return 0, err
	}
	media := mediaFrom(objs)
	path, props, hasVolume := b.volumeTarget(objs, media.DeviceAddress)
	if path == "" {
		return 0, ErrNoTransport
	}
	if !hasVolume {
		return 0, ErrNoAbsoluteVolume
	}
	switch v := props["Volume"].Value().(type) {
	case uint16:
		return v, nil
	case uint32:
		return uint16(v), nil
	}
	return 0, ErrNoAbsoluteVolume
}

// isUnknownProperty recognizes the D-Bus answers for a property that does not
// exist (BlueZ's Volume before the peer reports one, or a read-only one).
func isUnknownProperty(err error) bool {
	var dbusErr dbus.Error
	if !errors.As(err, &dbusErr) {
		return false
	}
	detail := strings.ToLower(dbusErr.Name + " " + fmt.Sprint(dbusErr.Body))
	return strings.Contains(detail, "unknownproperty") ||
		strings.Contains(detail, "unknown property") ||
		strings.Contains(detail, "invalidargs") ||
		strings.Contains(detail, "invalid arguments") ||
		strings.Contains(detail, "read-only") ||
		strings.Contains(detail, "notsupported")
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

// SetCommandObserver installs a read-only observer for resolved transport
// commands. The daemon uses it to log the direction and the evidence behind it,
// and to report an AVRCP call error at Warn. It never changes a command.
func (b *BlueZ) SetCommandObserver(fn func(CommandObservation)) {
	b.mu.Lock()
	b.cmdObserver = fn
	b.mu.Unlock()
}

func (b *BlueZ) observeCommand(obs CommandObservation) {
	b.mu.Lock()
	fn := b.cmdObserver
	b.mu.Unlock()
	if fn != nil {
		fn(obs)
	}
}

func (b *BlueZ) clockNow() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// rememberCommand records the direction this daemon just commanded so the next
// toggle can invert it instead of trusting a peer status that has not caught up.
// Only Play/Pause carry a direction; a skip is not a playback state.
func (b *BlueZ) rememberCommand(device, method string) {
	if device == "" || (method != "Play" && method != "Pause") {
		return
	}
	key := strings.ToUpper(strings.TrimSpace(device))
	now := b.clockNow()
	b.cmdMu.Lock()
	defer b.cmdMu.Unlock()
	if b.cmdMemory == nil {
		b.cmdMemory = make(map[string]commandRecord, commandMemoryDevices)
	}
	b.cmdMemory[key] = commandRecord{method: method, at: now}
	for addr, rec := range b.cmdMemory {
		if now.Sub(rec.at) > commandMemoryWindow {
			delete(b.cmdMemory, addr)
		}
	}
	for len(b.cmdMemory) > commandMemoryDevices {
		oldest, oldestAt := "", now
		for addr, rec := range b.cmdMemory {
			if addr == key {
				continue
			}
			if oldest == "" || rec.at.Before(oldestAt) {
				oldest, oldestAt = addr, rec.at
			}
		}
		if oldest == "" {
			break
		}
		delete(b.cmdMemory, oldest)
	}
}

// rememberedCommand reports the direction this daemon last commanded for the
// device while it is still recent enough to outrank the peer's lagging status.
func (b *BlueZ) rememberedCommand(device string) (string, bool) {
	if device == "" {
		return "", false
	}
	key := strings.ToUpper(strings.TrimSpace(device))
	now := b.clockNow()
	b.cmdMu.Lock()
	defer b.cmdMu.Unlock()
	rec, ok := b.cmdMemory[key]
	if !ok {
		return "", false
	}
	// A clock that went backwards (or a test that reset it) must not keep a
	// stale direction authoritative.
	if now.Before(rec.at) || now.Sub(rec.at) > commandMemoryWindow {
		delete(b.cmdMemory, key)
		return "", false
	}
	return rec.method, true
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
		b.observeCommand(CommandObservation{Device: device, Command: cmd, Err: ErrUnsupported})
		return ErrUnsupported
	}
	if err := b.sendCommand(ctx, target, cmd); err != nil {
		callErr := fmt.Errorf("%s %s: %w", target.iface, target.method, err)
		b.observeCommand(CommandObservation{
			Device: device, Command: cmd, Method: target.method, Evidence: target.evidence, Err: callErr,
		})
		return callErr
	}
	b.rememberCommand(device, target.method)
	method := target.method
	if target.iface == "org.bluez.MediaPlayer1" && panelOperation(cmd) != 0 {
		method = "Press+Release"
	}
	b.observeCommand(CommandObservation{
		Device: device, Command: cmd, Method: method, Evidence: target.evidence,
	})
	// AVRCP applies commands asynchronously; the signal watcher publishes the
	// resulting state. Refresh once so the immediate reply is as fresh as BlueZ
	// allows.
	return b.refresh(ctx)
}

// avcForward and avcBackward are the AV/C panel operation ids a skip uses. They
// are the same ids BlueZ's own Next/Previous convenience methods send.
const (
	avcForward  = byte(0x4b)
	avcBackward = byte(0x4c)
)

// panelOperation maps a command to the AV/C panel operation id it presses, for
// the commands that are panel keys rather than named player methods. A skip is
// the one that must always carry a release.
func panelOperation(cmd Command) byte {
	switch cmd {
	case CommandNext:
		return avcForward
	case CommandPrevious:
		return avcBackward
	default:
		return 0
	}
}

// sendCommand performs one transport command and, for a panel operation on a
// MediaPlayer1 target, always sends the release half of the AV/C passthrough.
//
// BlueZ's Next()/Previous() convenience methods send the press and rely on the
// peer answering ACCEPTED to send the release. A peer that answers INTERIM,
// NOT_IMPLEMENTED or an operand-less ACCEPTED leaves the key held for the rest of
// the session, and every later press of the same op is then ignored — while
// Play/Pause keeps working because alternating Play and Pause forces a release
// of the previous key. Press plus an unconditional Release is the same AV/C
// sequence a hardware remote sends, and it cannot latch.
func (b *BlueZ) sendCommand(ctx context.Context, target mediaTarget, cmd Command) error {
	object := b.conn.Object(bluezService, target.path)
	if target.iface == "org.bluez.MediaPlayer1" && panelOperation(cmd) != 0 {
		op := panelOperation(cmd)
		if err := object.CallWithContext(ctx, "org.bluez.MediaPlayer1.Press", 0, op).Err; err != nil {
			return err
		}
		// Release is sent even when the press failed to be acknowledged by the
		// peer: a latched key is worse than a packet the peer ignores.
		releaseCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return object.CallWithContext(releaseCtx, "org.bluez.MediaPlayer1.Release", 0).Err
	}
	return object.CallWithContext(ctx, target.iface+"."+target.method, 0).Err
}

// mediaTarget is the public BlueZ object and method one transport command uses.
// evidence names the fact a toggle direction came from, for the command log.
type mediaTarget struct {
	path     dbus.ObjectPath
	iface    string
	method   string
	evidence string
}

// mediaTarget resolves a command without crossing devices: when the framework
// is showing a source, only that source's MediaPlayer1 or MediaControl1 may
// receive the command, so a second connected device that happens to be playing
// cannot capture the user's toggle or skip. A source that exposes MediaControl1
// but no MediaPlayer1 is controlled through that public interface.
func (b *BlueZ) mediaTarget(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string, cmd Command) (mediaTarget, bool) {
	// Only a toggle needs a direction; Play/Pause/Next/Previous are explicit.
	var playing bool
	var evidence string
	if path := findPlayerPath(objs, device); path != "" {
		props := objs[path]["org.bluez.MediaPlayer1"]
		if cmd == CommandToggle {
			playing, evidence = b.playingFor(objs, props, device)
		}
		method := playerMethod(cmd, playing)
		if method == "" {
			return mediaTarget{}, false
		}
		return mediaTarget{path: path, iface: "org.bluez.MediaPlayer1", method: method, evidence: evidence}, true
	}
	if path, _ := findControlPath(objs, device); path != "" {
		// MediaControl1 publishes no playback status; direction comes from the
		// owned transport's receipts or the transport state for the same device.
		if cmd == CommandToggle {
			playing, evidence = b.playingFor(objs, nil, device)
		}
		method := controlMethod(cmd, playing)
		if method == "" {
			return mediaTarget{}, false
		}
		return mediaTarget{path: path, iface: "org.bluez.MediaControl1", method: method, evidence: evidence}, true
	}
	return mediaTarget{}, false
}

// playingFor resolves playback direction from the strongest evidence available
// and names it. It is the single resolver both consumers use: mediaTarget for a
// toggle, and PlaybackDirection for a published playback state, so a consumer
// that reports PLAYING cannot be inverted by a toggle that would send Play.
//
//   - memory: a direction this daemon commanded inside commandMemoryWindow
//     outranks every cached peer fact, because the next toggle must invert that
//     command, not repeat it.
//   - receipt: while this daemon owns the source's acquired transport, the sink's
//     own packet receipts are authoritative - a fresh receipt means the source is
//     playing and no receipt while we hold the transport means it is paused.
//   - status: otherwise the source's MediaPlayer1.Status, which is only BlueZ's
//     cache of the peer's AVRCP notifications.
//   - transport: without a status, the source's observed transport state.
//
// Unknown resolves to false, which sends Play; it never invents a paused state
// after a failed command.
func (b *BlueZ) playingFor(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, playerProps map[string]dbus.Variant, device string) (bool, string) {
	if method, ok := b.rememberedCommand(device); ok {
		// Play was the last direction we commanded, so the source is playing as
		// far as this daemon knows and the next toggle pauses it; Pause means
		// the opposite. This is the inversion, not a repetition of the cache.
		return method == "Play", EvidenceMemory
	}
	if b.ownsA2DPTransportFor(objs, device) {
		return b.audioStreaming(device), EvidenceReceipt
	}
	if playerProps != nil {
		if status := strProp(playerProps, "Status"); status != "" {
			return status == "playing", EvidenceStatus
		}
	}
	if device == "" {
		if path, props := findPlayer(objs); path != "" {
			if status := strProp(props, "Status"); status != "" {
				return status == "playing", EvidenceStatus
			}
			device = playerDeviceAddress(objs, props)
		}
	}
	if transportPlaying(objs, device) {
		return true, EvidenceTransport
	}
	return b.audioStreaming(device), EvidenceReceipt
}

// PlaybackDirection resolves the displayed source's playback direction from the
// same evidence a toggle uses, so a consumer that publishes it as PLAYING or
// PAUSED cannot disagree with the direction the next toggle takes. It reads the
// object tree instead of the cached media state because the cached status is
// exactly the lagging fact the direction memory exists to outrank.
func (b *BlueZ) PlaybackDirection(ctx context.Context) (Direction, error) {
	objs, err := b.objects(ctx)
	if err != nil {
		return Direction{}, err
	}
	media := mediaFrom(objs)
	if !media.Present {
		return Direction{}, nil
	}
	// Mirror mediaTarget: a MediaPlayer1 exists for this source on the player
	// path, so its status is read; the MediaControl1 path has none.
	var playerProps map[string]dbus.Variant
	if path := findPlayerPath(objs, media.DeviceAddress); path != "" {
		playerProps = objs[path]["org.bluez.MediaPlayer1"]
	}
	playing, evidence := b.playingFor(objs, playerProps, media.DeviceAddress)
	return Direction{Present: true, Playing: playing, Evidence: evidence}, nil
}

// --- A2DP repair -------------------------------------------------------------

// ownsA2DPTransportFor reports whether the transport this sink acquired belongs
// to the given source. Only then are the sink's packet receipts authoritative:
// while PulseAudio owns the stream, or before a transport is acquired, the sink
// has no receipts to speak for the source.
func (b *BlueZ) ownsA2DPTransportFor(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device string) bool {
	b.endpointMu.Lock()
	path := b.transport
	b.endpointMu.Unlock()
	if path == "" {
		return false
	}
	props, ok := objs[path]["org.bluez.MediaTransport1"]
	if !ok {
		// Acquired transports stay in BlueZ 5.65's object tree; an object map
		// that does not have it is a test or a partially read snapshot, and the
		// recorded path is still this sink's single transport.
		return true
	}
	dev := objectPathProp(props, "Device")
	if dev == "" || device == "" {
		return true
	}
	return strings.EqualFold(deviceAddressAt(objs, dev), device)
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
	// The session assertion is part of the adapter status: a source that is
	// connected without an A2DP transport is the state that produced
	// "telephone quality" while every ConnectProfile call had succeeded.
	link := audioLinkFrom(objs, media.DeviceAddress)
	if detail := link.Detail(); detail != "" {
		status.Detail = joinDetail(status.Detail, detail)
	}

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

	// AVRCP absolute volume, scoped to the same connected source the framework
	// displays. A background phone's transport must not move the master.
	if media.DeviceAddress != b.volumeDevice {
		b.volume, b.volumeSet, b.volumeWrote = 0, false, false
		b.volumeDevice = media.DeviceAddress
		if !strings.EqualFold(b.volumeAbsentDevice, media.DeviceAddress) {
			b.volumeAbsent, b.volumeAbsentDevice = false, ""
		}
		if !strings.EqualFold(b.volumeMissingDevice, media.DeviceAddress) {
			// The grace window belongs to one source session.
			b.volumeMissingDevice, b.volumeMissingSince = "", time.Time{}
		}
		b.volumeAbsentNotified = ""
	}
	volumePath, volumeProps, hasVolume := b.volumeTarget(objs, media.DeviceAddress)
	var reassert bool
	var reassertValue uint16
	if hasVolume {
		volume := transportVolumeValue(volumeProps)
		if strings.EqualFold(b.volumeAbsentDevice, media.DeviceAddress) {
			b.volumeAbsent, b.volumeAbsentDevice = false, ""
		}
		publish, echo := volumeObservation(b.volume, b.volumeSet, b.volumeWrote, volume)
		switch {
		case echo:
			b.volumeSet = true
		case publish:
			if level, ok := b.pendingVolumeLocked(media.DeviceAddress, volume); ok {
				// The peer's first report created Volume after our write was
				// refused for lack of it. Reach the level the framework asked
				// for instead of adopting the peer's stale one; recording it as
				// the observed level suppresses the echo of the write below.
				b.volume, b.volumeSet = level, true
				reassert, reassertValue = true, level
			} else {
				b.volume, b.volumeSet = volume, true
				events = append(events, Event{
					Kind: EventVolume, Volume: volume, Media: media,
					DeviceAddress: media.DeviceAddress, DeviceName: media.DeviceName,
				})
			}
		}
		b.volumeWrote = false
	} else {
		// No transport volume: the next source's first level is initial state
		// again. A remembered request survives, so a source that only reports
		// volume later still converges on the configured level.
		b.volumeSet = false
		b.volumeWrote = false
		b.volume = 0
	}

	// The source-scoped part of the bounded detail: a source that refuses
	// absolute volume is why its own slider does not move with the master.
	if detail := b.volumeAbsentDetailLocked(media.DeviceAddress, hasVolume); detail != "" {
		media.Detail = joinDetail(media.Detail, detail)
	}

	if mediaChanged(b.media, media) {
		b.revision++
		media.Revision = b.revision
		events = append(events, Event{Kind: EventMedia, Media: media})
	} else {
		media.Revision = b.media.Revision
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

	// The peer's first report materialised Volume: assert the configured level
	// once. The write happens outside the state lock; the level recorded above
	// keeps its PropertiesChanged echo from being published as a source change.
	if reassert && volumePath != "" {
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = b.writeTransportVolume(wctx, volumePath, reassertValue)
		cancel()
	}

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

// transportVolumeValue decodes a transport's Volume variant (BlueZ publishes a
// uint16; a uint32 is tolerated for a differently built BlueZ).
func transportVolumeValue(props map[string]dbus.Variant) uint16 {
	v, ok := props["Volume"]
	if !ok {
		return 0
	}
	switch value := v.Value().(type) {
	case uint16:
		return value
	case uint32:
		return uint16(value)
	}
	return 0
}

// joinDetail appends one bounded sentence to a status detail.
func joinDetail(detail, sentence string) string {
	if sentence == "" {
		return detail
	}
	if detail == "" {
		return sentence
	}
	return detail + "; " + sentence
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
