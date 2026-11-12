package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
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

	cancel   context.CancelFunc
	powering atomic.Bool

	agentExported   atomic.Bool
	agentRegistered atomic.Bool

	volume       uint16
	volumeSet    bool
	volumeWrote  bool
	transportKey string

	endpointMu   sync.Mutex
	endpointPath dbus.ObjectPath
	endpoint     *sbcEndpoint
	armed        bool
	transport    dbus.ObjectPath
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
	// pairing retries and reports the error instead of claiming success.
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
	defer b.mu.Unlock()
	return b.status, nil
}

func (b *BlueZ) SetDiscoverable(ctx context.Context, on bool) error {
	if err := b.EnsurePowered(ctx); err != nil {
		return err
	}
	if err := b.setAdapterProperty(ctx, "Discoverable", on); err != nil {
		return err
	}
	if err := b.refresh(ctx); err != nil {
		return err
	}
	// Pairing visibility is separate from connectability on this stack.
	b.ensurePageScan()
	return nil
}

func (b *BlueZ) SetPairable(ctx context.Context, on bool) error {
	if err := b.EnsurePowered(ctx); err != nil {
		return err
	}
	if on {
		// Without a registered agent BlueZ cannot complete a new bond, so do
		// not advertise pairability we cannot honor.
		if err := b.registerAgent(ctx); err != nil {
			return err
		}
	}
	if err := b.setAdapterProperty(ctx, "Pairable", on); err != nil {
		return err
	}
	return b.refresh(ctx)
}

// EnsurePowered turns the adapter on. BlueZ reports "Network is down" from the
// legacy HCI ioctls when the adapter is off, so every operation that depends on
// the adapter being usable starts here.
func (b *BlueZ) EnsurePowered(ctx context.Context) error {
	if err := b.refresh(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	powered := b.status.Powered
	b.mu.Unlock()
	if !powered {
		if err := b.setAdapterProperty(ctx, "Powered", true); err != nil {
			return err
		}
		if err := b.refresh(ctx); err != nil {
			return err
		}
	}
	b.ensurePageScan()
	return nil
}

// ensurePageScan keeps the adapter connectable while hidden. BlueZ 5.65 on
// this kernel clears page scan when the adapter is made non-discoverable, so a
// paired device cannot reconnect. The legacy ioctl restores it and persists
// until the next adapter state change; non-Linux platforms simply ignore the
// missing tool.
func (b *BlueZ) ensurePageScan() {
	name := adapterName(b.adapterPath())
	if name == "" {
		return
	}
	_ = exec.Command("hciconfig", name, "pscan").Run()
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
// never appears as an audio output.
func (b *BlueZ) ConnectAudio(ctx context.Context, address string) error {
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	path := findDevicePath(objs, address)
	if path == "" {
		return ErrNoSource
	}
	for _, uuid := range []string{
		"0000110a-0000-1000-8000-00805f9b34fb", // Audio Source
		"0000110e-0000-1000-8000-00805f9b34fb", // A/V Remote Control
	} {
		// Already-connected profiles return an error; only the effect matters.
		_ = b.conn.Object(bluezService, path).CallWithContext(ctx, "org.bluez.Device1.ConnectProfile", 0, uuid).Err
	}
	return b.refresh(ctx)
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

// SetTransportVolume writes the AVRCP absolute volume of the active transport.
// With avrcp_absolute_volume=0 in PulseAudio this changes only the remote
// volume, not the local source gain.
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
	path, props := findVolumeTransport(objs)
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
	// of the source's level existed before the write.
	b.mu.Lock()
	b.volume, b.volumeSet, b.volumeWrote = value, true, true
	b.mu.Unlock()
	return nil
}

// TransportVolume reads the active AVRCP transport volume.
func (b *BlueZ) TransportVolume(ctx context.Context) (uint16, error) {
	objs, err := b.objects(ctx)
	if err != nil {
		return 0, err
	}
	path, props := findVolumeTransport(objs)
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

func (b *BlueZ) Command(ctx context.Context, cmd Command) error {
	if err := b.refresh(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	present, playing := b.media.Present, b.media.Playing
	b.mu.Unlock()
	if !present {
		return ErrNoSource
	}
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	if path := findPlayerPath(objs, playing); path != "" {
		method := playerMethod(cmd, playing)
		if method == "" {
			return ErrUnsupported
		}
		if err := b.conn.Object(bluezService, path).CallWithContext(ctx, "org.bluez.MediaPlayer1."+method, 0).Err; err != nil {
			return fmt.Errorf("media player %s: %w", method, err)
		}
	} else if path := findControlPath(objs); path != "" {
		method := controlMethod(cmd, playing)
		if method == "" {
			return ErrUnsupported
		}
		if err := b.conn.Object(bluezService, path).CallWithContext(ctx, "org.bluez.MediaControl1."+method, 0).Err; err != nil {
			return fmt.Errorf("media control %s: %w", method, err)
		}
	} else {
		return ErrUnsupported
	}
	// AVRCP applies commands asynchronously; the signal watcher publishes the
	// resulting state. Refresh once so the immediate reply is as fresh as BlueZ
	// allows.
	return b.refresh(ctx)
}

// Watch subscribes to connect/disconnect and media changes.
func (b *BlueZ) Watch(ctx context.Context) <-chan Event {
	ch := make(chan Event, 16)
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
		return unavailable
	}

	adapterPath, adapterProps := findAdapter(objs)
	status := Status{
		Present: adapterPath != "",
		Adapter: adapterName(adapterPath),
		Detail:  "BlueZ",
	}
	if status.Present {
		status.Powered = boolProp(adapterProps, "Powered")
		status.Discoverable = boolProp(adapterProps, "Discoverable")
		status.Pairable = boolProp(adapterProps, "Pairable")
		if !status.Powered {
			status.Detail = "BlueZ adapter is powered off"
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

	if mediaChanged(b.media, media) {
		b.revision++
		media.Revision = b.revision
		events = append(events, Event{Kind: EventMedia, Media: media})
	} else {
		media.Revision = b.media.Revision
	}

	// AVRCP absolute volume. A first observation is still published: a source
	// change that happens before any snapshot (including between connect and
	// the first refresh) must not be dropped. Only the exact level this manager
	// wrote before a snapshot is suppressed, because that read is our echo.
	if volume, ok := transportVolume(objs); ok {
		publish, echo := volumeObservation(b.volume, b.volumeSet, b.volumeWrote, volume)
		switch {
		case echo:
			b.volumeSet = true
		case publish:
			b.volume, b.volumeSet = volume, true
			events = append(events, Event{Kind: EventVolume, Volume: volume, Media: media})
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
	subs := make([]chan Event, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, event := range events {
		for _, ch := range subs {
			select {
			case ch <- event:
			default:
			}
		}
	}
	return nil
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
// power-off. The framework wants Bluetooth available; the user-facing switch is
// pairability, not adapter power.
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
	b.ensurePageScan()
	// Powering the adapter discards the endpoints registered with it, so the
	// sink profile must be advertised again before a source can connect.
	b.rearmA2DP(callCtx)
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
				b.rearmA2DP(ctx)
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
			if !b.SinkRegistered() {
				b.rearmA2DP(ctx)
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
			state.DeviceAddress, state.DeviceName = deviceIdentity(objs, dev)
		}
		if state.Title == "" && state.Artist == "" && state.Album == "" {
			state.Detail = "the source reports playback state without track metadata"
		}
		return state
	}

	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaControl1"]
		if !ok || !boolProp(props, "Connected") {
			continue
		}
		state := MediaState{Present: true, Detail: "source exposes AVRCP controls but no track metadata"}
		state.DeviceAddress, state.DeviceName = deviceIdentity(objs, dbus.ObjectPath(""))
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
				state.DeviceAddress, state.DeviceName = deviceIdentity(objs, dev)
			}
			return state
		}
	}
	return MediaState{Detail: "no Bluetooth source connected"}
}

func findPlayer(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) (dbus.ObjectPath, map[string]dbus.Variant) {
	var best dbus.ObjectPath
	var bestProps map[string]dbus.Variant
	bestPlaying := false
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaPlayer1"]
		if !ok {
			continue
		}
		playing := strProp(props, "Status") == "playing"
		if best == "" || (playing && !bestPlaying) {
			best, bestProps, bestPlaying = path, props, playing
		}
	}
	return best, bestProps
}

func findPlayerPath(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, playing bool) dbus.ObjectPath {
	path, _ := findPlayer(objs)
	return path
}

func findControlPath(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) dbus.ObjectPath {
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaControl1"]
		if ok && boolProp(props, "Connected") {
			return path
		}
	}
	return ""
}

// findVolumeTransport prefers an active transport, then pending, then any
// transport that exposes Volume. AVRCP absolute volume is available before the
// audio stream becomes active (the property exists while State=idle), and the
// framework volume must reach the source at that point too.
func findVolumeTransport(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) (dbus.ObjectPath, map[string]dbus.Variant) {
	var idlePath, pendingPath dbus.ObjectPath
	var idleProps, pendingProps map[string]dbus.Variant
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaTransport1"]
		if !ok {
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

// transportVolume reads the AVRCP absolute volume of the transport the volume
// path targets. ok is false when no transport exposes the property, i.e. the
// source does not support absolute volume.
func transportVolume(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) (uint16, bool) {
	path, props := findVolumeTransport(objs)
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
