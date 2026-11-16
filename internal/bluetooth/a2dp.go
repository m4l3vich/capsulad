package bluetooth

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// A2DP SBC sink endpoint.
//
// BlueZ owns the AVDTP signalling and AVRCP. It calls back into this exported
// endpoint to negotiate an SBC configuration, then creates a MediaTransport1
// object; AcquireA2DP hands the stream socket to the framework, which decodes
// and mixes the PCM itself instead of routing it through PulseAudio's bluez5
// source and a loopback.

const (
	a2dpSinkUUID   = "0000110b-0000-1000-8000-00805f9b34fb"
	a2dpEndpointID = dbus.ObjectPath("/org/capsulad/a2dp/sbc")
)

// SBC capability bits (A2DP section 4.3.2). The media codec capabilities of an
// SBC SEP are exactly four octets:
//
//	[0] sampling frequency (bits 7-4) | channel mode (bits 3-0)
//	[1] block length (7-4) | subbands (3-2) | allocation method (1-0)
//	[2] minimum bitpool
//	[3] maximum bitpool
const (
	sbcFreq44k    = 0x20
	sbcFreq48k    = 0x10
	sbcFreq32k    = 0x40
	sbcFreq16k    = 0x80
	sbcModeJoint  = 0x01
	sbcModeStereo = 0x02
	sbcModeDual   = 0x04
	sbcModeMono   = 0x08
	sbcBlock16    = 0x10
	sbcBlock12    = 0x20
	sbcBlock8     = 0x40
	sbcBlock4     = 0x80
	sbcSub8       = 0x04
	sbcSub4       = 0x08
	sbcAllocLoud  = 0x01
	sbcAllocSNR   = 0x02
)

// SBC parameters this sink advertises and negotiates.
//
// The maximum bitpool is 53, the A2DP-recommended ceiling for 44.1 kHz joint
// stereo with 16 blocks and 8 subbands (about 328 kbit/s). Bitpool is a
// frame-sized budget shared by all subbands, so raising it past 53 buys no
// audible quality for SBC at this rate and only spends Bluetooth airtime that
// the E0/E1 retransmission window cannot afford on the 1 GHz A35. 53 is the
// highest value every SBC source is required to support, so no peer is excluded
// by it.
const (
	sbcMinBitpool = 0x02
	sbcMaxBitpool = 0x35
)

// sbcSinkCapabilities builds the four-octet SBC media codec capabilities of the
// local sink. BlueZ copies this blob verbatim into the local SEP, so it must use
// the spec layout: frequencies 44.1/48/32/16 kHz (44.1 kHz is negotiated first),
// joint stereo and stereo, 16/12/8 blocks, 8/4 subbands, loudness and SNR.
func sbcSinkCapabilities() []byte {
	freq := byte(sbcFreq44k | sbcFreq48k | sbcFreq32k | sbcFreq16k)
	mode := byte(sbcModeJoint | sbcModeStereo)
	block := byte(sbcBlock16 | sbcBlock12 | sbcBlock8)
	subbands := byte(sbcSub8 | sbcSub4)
	alloc := byte(sbcAllocLoud | sbcAllocSNR)
	return []byte{freq | mode, block | subbands | alloc, sbcMinBitpool, sbcMaxBitpool}
}

type sbcEndpoint struct {
	b *BlueZ

	mu        sync.Mutex
	config    []byte
	transport dbus.ObjectPath
}

// SelectConfiguration chooses a concrete SBC configuration from the remote's
// advertised capabilities. BlueZ 5.65 passes the remote SEP's four-byte
// capability array directly (44.1 kHz joint stereo, 16 blocks / 8 subbands,
// loudness, bitpool 2..250 on the Mac), so the method signature is (ay) -> ay.
func (e *sbcEndpoint) SelectConfiguration(capabilities []byte) ([]byte, *dbus.Error) {
	return selectSBCConfig(capabilities), nil
}

// SetConfiguration is called as (o transport, a{sv} properties) on BlueZ 5.65.
// The negotiated SBC configuration is in the Configuration property.
func (e *sbcEndpoint) SetConfiguration(transport dbus.ObjectPath, props map[string]dbus.Variant) *dbus.Error {
	e.mu.Lock()
	e.config = append([]byte(nil), bytesProp(props, "Configuration")...)
	e.transport = transport
	e.mu.Unlock()
	return nil
}

func (e *sbcEndpoint) ClearConfiguration() *dbus.Error {
	e.mu.Lock()
	e.config = nil
	e.transport = ""
	e.mu.Unlock()
	return nil
}

func (e *sbcEndpoint) Release() *dbus.Error {
	e.mu.Lock()
	e.transport = ""
	e.mu.Unlock()
	return nil
}

func (e *sbcEndpoint) Transport() dbus.ObjectPath {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.transport
}

func (e *sbcEndpoint) Config() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]byte(nil), e.config...)
}

// RegisterA2DPSink exports and registers the endpoint with the adapter.
//
// Registration is ordered deliberately: the adapter is powered first (BlueZ
// rejects RegisterEndpoint while it is off) but connectability is applied only
// after the Audio Sink SEP exists, and only if the persisted incoming policy
// enables it. Otherwise a paired source can make its first incoming connection
// in the window where the adapter is connectable and has no audio profile, and
// it does not retry just because the endpoint appears later. The call is
// idempotent and returns the real registration outcome, so a failure is never
// reported as an advertised sink.
func (b *BlueZ) RegisterA2DPSink(ctx context.Context) error {
	b.registrationMu.Lock()
	defer b.registrationMu.Unlock()
	if err := b.ensurePowered(ctx); err != nil {
		return err
	}
	if err := b.registerA2DPEndpoint(ctx); err != nil {
		return err
	}
	// Re-apply the current incoming policy; this is the first point at which
	// page scan is safe for a paired source.
	if err := b.applyConnectablePolicy(ctx); err != nil {
		return err
	}
	return nil
}

// registerA2DPEndpoint exports the endpoint object once and registers it
// against the current adapter. BlueZ discards registrations on a service
// restart or adapter power cycle while our exported object survives, so the
// object is reused and only the D-Bus registration is repeated.
func (b *BlueZ) registerA2DPEndpoint(ctx context.Context) error {
	adapter := b.adapterPath()
	if adapter == "" {
		return ErrUnavailable
	}
	b.endpointMu.Lock()
	ep := b.endpoint
	path := b.endpointPath
	b.endpointMu.Unlock()
	if ep == nil {
		ep = &sbcEndpoint{b: b}
		if err := b.conn.Export(ep, a2dpEndpointID, "org.bluez.MediaEndpoint1"); err != nil {
			return fmt.Errorf("export A2DP endpoint: %w", err)
		}
		path = a2dpEndpointID
		b.endpointMu.Lock()
		b.endpoint = ep
		b.endpointPath = path
		b.endpointMu.Unlock()
	}
	if path == "" {
		return fmt.Errorf("export A2DP endpoint: empty object path")
	}
	// Capture the generation before the D-Bus round trip. A BlueZ restart or
	// adapter power cycle that lands during the call advances it, and the
	// reply must not arm an endpoint BlueZ has already discarded.
	gen := b.endpointGeneration()
	err := b.callRegisterEndpoint(ctx, adapter, path)
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("register A2DP endpoint: %w", err)
	}
	if !b.armA2DP(gen) {
		return errors.New("A2DP endpoint changed generation during registration")
	}
	return nil
}

// callRegisterEndpoint is the single org.bluez.Media1.RegisterEndpoint call
// site. Tests replace the hook; production goes straight to BlueZ.
func (b *BlueZ) callRegisterEndpoint(ctx context.Context, adapter, path dbus.ObjectPath) error {
	if b.registerEndpoint != nil {
		return b.registerEndpoint(ctx, adapter, path)
	}
	return b.conn.Object(bluezService, adapter).CallWithContext(ctx,
		"org.bluez.Media1.RegisterEndpoint", 0, path, a2dpEndpointProps()).Err
}

// SinkRegistered reports whether the sink endpoint is currently registered with
// the adapter. Readiness is generation-scoped: a registration is only valid
// for the adapter power state it was armed under. After a BlueZ restart or an
// adapter power cycle the flag reports false until a re-arm succeeds.
func (b *BlueZ) SinkRegistered() bool {
	b.endpointMu.Lock()
	defer b.endpointMu.Unlock()
	return b.endpoint != nil && b.armed && b.armedGen == b.endpointGen
}

// markA2DPUnarmed records that BlueZ may no longer hold the registration, e.g.
// after a service restart or adapter power cycle. It advances the endpoint
// generation unconditionally so a reply from any registration attempt that was
// in flight when the invalidation happened cannot mark the endpoint ready.
func (b *BlueZ) markA2DPUnarmed() {
	b.endpointMu.Lock()
	b.armed = false
	b.armedGen = 0
	b.endpointGen++
	b.endpointMu.Unlock()
}

// rearmA2DP registers the exported endpoint again after BlueZ restarted or the
// adapter was power-cycled; either event discards the registration while our
// exported D-Bus object survives. It returns the real error: callers must not
// assume readiness from a failed re-arm. An adapter that is not powered cannot
// hold an endpoint, so it is left for reconcilePower to re-arm after powering.
func (b *BlueZ) rearmA2DP(ctx context.Context) error {
	b.registrationMu.Lock()
	defer b.registrationMu.Unlock()
	b.endpointMu.Lock()
	registered := b.endpoint != nil
	path := b.endpointPath
	b.endpointMu.Unlock()
	if !registered || path == "" {
		return nil
	}
	b.mu.Lock()
	ready := b.adapter != "" && b.status.Present && b.status.Powered
	b.mu.Unlock()
	if !ready {
		return nil
	}
	return b.registerA2DPEndpoint(ctx)
}

func (b *BlueZ) endpointGeneration() uint64 {
	b.endpointMu.Lock()
	defer b.endpointMu.Unlock()
	return b.endpointGen
}

// armA2DP marks the endpoint registered only while the generation captured
// before the D-Bus call is still current. A registration reply that raced a
// BlueZ restart or adapter power cycle is not readiness.
func (b *BlueZ) armA2DP(gen uint64) bool {
	b.endpointMu.Lock()
	defer b.endpointMu.Unlock()
	if b.endpoint == nil || gen != b.endpointGen {
		return false
	}
	b.armed = true
	b.armedGen = gen
	return true
}

// endpointRegistrationInvalidated reports whether a refresh observed an
// adapter identity or power transition. BlueZ discards endpoint registrations
// on any power change (off discards them; on requires a fresh registration),
// so readiness must not survive it.
func endpointRegistrationInvalidated(prevAdapter, adapter dbus.ObjectPath, prev, next Status) bool {
	return prevAdapter != adapter ||
		prev.Present != next.Present ||
		prev.Powered != next.Powered
}

// a2dpEndpointProps is the local SBC sink capability advertised to BlueZ. The
// capabilities are the spec's four-octet SBC media codec capabilities; BlueZ
// does not validate them, it copies them into the local SEP and a strict source
// parses them according to A2DP section 4.3.2.
func a2dpEndpointProps() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"UUID":         dbus.MakeVariant(a2dpSinkUUID),
		"Codec":        dbus.MakeVariant(byte(0)),
		"Capabilities": dbus.MakeVariant(sbcSinkCapabilities()),
	}
}

// AcquireA2DP finds the transport belonging to our endpoint and acquires it.
func (b *BlueZ) AcquireA2DP(ctx context.Context) (*A2DPTransport, error) {
	b.endpointMu.Lock()
	endpointPath := b.endpointPath
	endpoint := b.endpoint
	b.endpointMu.Unlock()
	if endpointPath == "" || endpoint == nil {
		return nil, ErrUnsupported
	}
	objs, err := b.objects(ctx)
	if err != nil {
		return nil, err
	}
	path, props := b.findA2DPTransport(objs, endpointPath, endpoint.Transport())
	if path == "" {
		return nil, ErrNoTransport
	}
	var fd dbus.UnixFD
	var readMTU, writeMTU uint16
	if err := b.conn.Object(bluezService, path).CallWithContext(ctx,
		"org.bluez.MediaTransport1.Acquire", 0).Store(&fd, &readMTU, &writeMTU); err != nil {
		var dbusErr dbus.Error
		if errors.As(err, &dbusErr) && (strings.Contains(dbusErr.Name, "NotAvailable") ||
			strings.Contains(dbusErr.Name, "Busy") || strings.Contains(dbusErr.Name, "NotAuthorized")) {
			return nil, ErrNoTransport
		}
		return nil, fmt.Errorf("acquire A2DP transport: %w", err)
	}
	if fd < 0 {
		return nil, fmt.Errorf("acquire A2DP transport: invalid fd %d", fd)
	}
	file := os.NewFile(uintptr(fd), "a2dp")
	if file == nil {
		return nil, fmt.Errorf("acquire A2DP transport: cannot wrap fd %d", fd)
	}
	b.endpointMu.Lock()
	b.transport = path
	b.endpointMu.Unlock()
	return &A2DPTransport{File: file, ReadMTU: readMTU, Config: bytesProp(props, "Configuration")}, nil
}

// ReleaseA2DP releases the owned transport. The caller should close the fd
// first; Release is a BlueZ bookkeeping call and a missing transport is not an
// error.
func (b *BlueZ) ReleaseA2DP(ctx context.Context) error {
	b.endpointMu.Lock()
	path := b.transport
	b.transport = ""
	b.endpointMu.Unlock()
	if path == "" {
		return nil
	}
	err := b.conn.Object(bluezService, path).CallWithContext(ctx,
		"org.bluez.MediaTransport1.Release", 0).Err
	if err == nil {
		return nil
	}
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) && strings.Contains(dbusErr.Name, "NotAcquired") {
		return nil
	}
	return fmt.Errorf("release A2DP transport: %w", err)
}

func (b *BlueZ) findA2DPTransport(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, endpointPath, transportPath dbus.ObjectPath) (dbus.ObjectPath, map[string]dbus.Variant) {
	// Only a pending/active transport carries the A2DP stream. An idle one is
	// created for the configured stream but stays silent while the source is
	// paused; acquiring it blocks forever and the real active transport is
	// never read.
	usable := func(props map[string]dbus.Variant) bool {
		state := strProp(props, "State")
		return state == "pending" || state == "active"
	}
	// BlueZ 5.65's MediaTransport1 has no Endpoint property, but
	// SetConfiguration gives us the exact path it will use.
	if transportPath != "" {
		if props, ok := objs[transportPath]["org.bluez.MediaTransport1"]; ok && usable(props) {
			return transportPath, props
		}
	}
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaTransport1"]
		if !ok || !usable(props) {
			continue
		}
		if strProp(props, "UUID") != a2dpSinkUUID {
			continue
		}
		if codec, ok := props["Codec"]; !ok || byteValue(codec) != 0 {
			continue
		}
		if ep := strProp(props, "Endpoint"); ep != "" && ep != string(endpointPath) {
			continue
		}
		return path, props
	}
	return "", nil
}

func byteValue(v dbus.Variant) byte {
	switch value := v.Value().(type) {
	case byte:
		return value
	case uint32:
		return byte(value)
	case int32:
		return byte(value)
	}
	return 0
}

func (b *BlueZ) unregisterA2DP() {
	b.registrationMu.Lock()
	defer b.registrationMu.Unlock()
	b.endpointMu.Lock()
	path := b.endpointPath
	b.endpointPath = ""
	b.endpoint = nil
	b.armed = false
	b.armedGen = 0
	b.endpointGen++
	b.transport = ""
	b.endpointMu.Unlock()
	if path == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.conn.Object(bluezService, b.adapterPath()).CallWithContext(ctx,
		"org.bluez.Media1.UnregisterEndpoint", 0, path).Err
	_ = b.conn.ExportMethodTable(nil, path, "org.bluez.MediaEndpoint1")
}

func variantBytes(v dbus.Variant) []byte {
	if b, ok := v.Value().([]byte); ok {
		return b
	}
	return nil
}

func bytesProp(props map[string]dbus.Variant, key string) []byte {
	if v, ok := props[key]; ok {
		return variantBytes(v)
	}
	return nil
}

// AudioLink is the observed audio path of one connected source. A2DPState is
// the state of a MediaTransport1 that carries the A2DP sink UUID and the SBC
// codec; an empty state means the source has no A2DP transport at all, which is
// what a Headset-only (or ACL-only) link looks like even though ConnectProfile
// succeeded for its profiles.
type AudioLink struct {
	Address     string
	Connected   bool
	AudioSource bool
	// A2DPState is "idle", "pending" or "active" for the source's A2DP sink
	// transport, empty when the source has none.
	A2DPState string
	// AVRCP reports an observable AVRCP session (a MediaControl1 that is
	// connected, or the source's MediaPlayer1).
	AVRCP bool
	// Advertised names the audio-relevant services the peer advertises, so a
	// log names what the source could have brought up instead.
	Advertised []string
}

// A2DP reports whether the source actually has an A2DP transport. It is the
// assertion the daemon makes after a connection: "the audio profiles
// connected" without a transport is not an audio link.
func (l AudioLink) A2DP() bool { return l.A2DPState != "" }

// ProfilesUp names the audio profiles this link observably has up.
func (l AudioLink) ProfilesUp() []string {
	up := make([]string, 0, 2)
	if l.A2DP() {
		up = append(up, "a2dp")
	}
	if l.AVRCP {
		up = append(up, "avrcp")
	}
	if len(up) == 0 {
		up = append(up, "acl-only")
	}
	return up
}

// Detail is the bounded status sentence for a connected source without an A2DP
// transport, empty when the link is fine.
func (l AudioLink) Detail() string {
	if !l.Connected || !l.AudioSource || l.A2DP() {
		return ""
	}
	return "audio link is not A2DP"
}

// String is the one-line diagnostic form for logs.
func (l AudioLink) String() string {
	state := l.A2DPState
	if state == "" {
		state = "none"
	}
	peer := strings.Join(l.Advertised, ",")
	if peer == "" {
		peer = "none"
	}
	return fmt.Sprintf("a2dp=%s profiles=%s peer=%s", state, strings.Join(l.ProfilesUp(), ","), peer)
}

// audioServiceNames maps the remote audio services a source advertises to the
// names used in diagnostics. A peer that advertises Headset but no Audio Source
// can only offer a narrowband link to us in the first place.
var audioServiceNames = map[string]string{
	"0000110a-0000-1000-8000-00805f9b34fb": "a2dp-source",
	"0000110c-0000-1000-8000-00805f9b34fb": "avrcp-target",
	"0000110e-0000-1000-8000-00805f9b34fb": "avrcp-controller",
	"00001108-0000-1000-8000-00805f9b34fb": "headset",
	"0000111e-0000-1000-8000-00805f9b34fb": "handsfree",
	"0000111f-0000-1000-8000-00805f9b34fb": "handsfree-gateway",
}

func advertisedAudioServices(props map[string]dbus.Variant) []string {
	value, ok := props["UUIDs"]
	if !ok {
		return nil
	}
	uuids, _ := value.Value().([]string)
	out := make([]string, 0, len(uuids))
	for _, uuid := range uuids {
		if name, ok := audioServiceNames[strings.ToLower(uuid)]; ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// sortedTransportPaths returns every MediaTransport1 object path in
// deterministic order so a link inspection cannot depend on map iteration.
func sortedTransportPaths(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) []dbus.ObjectPath {
	paths := make([]dbus.ObjectPath, 0, len(objs))
	for path, ifaces := range objs {
		if _, ok := ifaces["org.bluez.MediaTransport1"]; ok {
			paths = append(paths, path)
		}
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
	return paths
}

// a2dpTransportFor finds the A2DP sink transport of exactly one device: the
// A2DP sink UUID plus the SBC codec, exactly what our endpoint registers. With
// no device there is no answer - a background phone's transport must never be
// mistaken for the displayed source's.
func a2dpTransportFor(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, device dbus.ObjectPath) (dbus.ObjectPath, map[string]dbus.Variant) {
	if device == "" {
		return "", nil
	}
	var idlePath dbus.ObjectPath
	var idleProps map[string]dbus.Variant
	for _, path := range sortedTransportPaths(objs) {
		props := objs[path]["org.bluez.MediaTransport1"]
		if strProp(props, "UUID") != a2dpSinkUUID {
			continue
		}
		if codec, ok := props["Codec"]; ok && byteValue(codec) != 0 {
			continue
		}
		if dev := objectPathProp(props, "Device"); dev != "" && dev != device {
			continue
		}
		switch strProp(props, "State") {
		case "active", "pending":
			return path, props
		default:
			if idlePath == "" {
				idlePath, idleProps = path, props
			}
		}
	}
	return idlePath, idleProps
}

// audioLinkFrom inspects the object tree for one source. It is a pure read so a
// unit test can pin the assertion without a bus.
func audioLinkFrom(objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant, address string) AudioLink {
	link := AudioLink{Address: address}
	dev := findDevicePath(objs, address)
	if dev == "" {
		return link
	}
	props := objs[dev]["org.bluez.Device1"]
	link.Connected = boolProp(props, "Connected")
	link.AudioSource = audioSourceDevice(props)
	link.Advertised = advertisedAudioServices(props)
	if control, ok := objs[dev]["org.bluez.MediaControl1"]; ok && boolProp(control, "Connected") {
		link.AVRCP = true
	}
	if path, _ := a2dpTransportFor(objs, dev); path != "" {
		link.A2DPState = strProp(objs[path]["org.bluez.MediaTransport1"], "State")
		if link.A2DPState == "" {
			link.A2DPState = "idle"
		}
	}
	if path := findPlayerPath(objs, address); path != "" {
		link.AVRCP = true
	}
	return link
}

// Session reports the audio link BlueZ currently has for one connected source.
// It answers "is this session actually A2DP", not "did ConnectProfile reply".
func (b *BlueZ) Session(ctx context.Context, address string) (AudioLink, error) {
	objs, err := b.objects(ctx)
	if err != nil {
		return AudioLink{Address: address}, err
	}
	if findDevicePath(objs, address) == "" {
		return AudioLink{Address: address}, nil
	}
	return audioLinkFrom(objs, address), nil
}

// a2dpSourceProfile is the peer's Audio Source profile: the profile a speaker
// asks a phone to bring up when the two connected over AVRCP only.
const a2dpSourceProfile = "0000110a-0000-1000-8000-00805f9b34fb"

// RequestA2DPProfile asks one connected source to bring its A2DP source profile
// up. It is a request, not a result: BlueZ answers the profile request before
// AVDTP finishes, so the caller still judges the link by Session.
//
// It never disconnects the source. A peer that will not negotiate A2DP keeps its
// link and the daemon reports the missing transport: dropping a user's phone off
// the adapter to force a renegotiation stops their audio mid-sentence and can
// leave the link on an even worse profile.
func (b *BlueZ) RequestA2DPProfile(ctx context.Context, address string) error {
	objs, err := b.objects(ctx)
	if err != nil {
		return err
	}
	path := findDevicePath(objs, address)
	if path == "" {
		return fmt.Errorf("%w: unknown device %s", ErrNoSource, address)
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = b.conn.Object(bluezService, path).CallWithContext(callCtx,
		"org.bluez.Device1.ConnectProfile", 0, a2dpSourceProfile).Err
	if err != nil {
		return fmt.Errorf("request the A2DP source profile of %s: %w", address, err)
	}
	return nil
}

// selectSBCConfig chooses a concrete SBC configuration from the remote SEP's
// four-octet media codec capabilities. BlueZ 5.65 passes exactly what the peer
// advertised (a2dp.c: setup->rsep -> codec->data) and uses the returned blob
// verbatim as the stream configuration, so the answer must be a configuration
// the peer offered: every selected bit comes from its own masks.
func selectSBCConfig(caps []byte) []byte {
	if len(caps) < 4 {
		// No usable capabilities: a source is only here if it offered SBC, so
		// answer with the sink's own preferred configuration.
		return []byte{sbcFreq44k | sbcModeJoint, sbcBlock16 | sbcSub8 | sbcAllocLoud, sbcMinBitpool, sbcMaxBitpool}
	}
	freqMask, modeMask := caps[0], caps[0]
	blockMask, subMask, allocMask := caps[1], caps[1], caps[1]
	minBP, maxBP := caps[2], caps[3]
	pick := func(mask byte, prefs ...byte) byte {
		for _, p := range prefs {
			if mask&p != 0 {
				return p
			}
		}
		return 0
	}
	freq := pick(freqMask, sbcFreq44k, sbcFreq48k, sbcFreq32k, sbcFreq16k)
	if freq == 0 {
		freq = sbcFreq44k
	}
	mode := pick(modeMask, sbcModeJoint, sbcModeStereo, sbcModeDual, sbcModeMono)
	if mode == 0 {
		mode = sbcModeJoint
	}
	block := pick(blockMask, sbcBlock16, sbcBlock12, sbcBlock8, sbcBlock4)
	if block == 0 {
		block = sbcBlock16
	}
	sub := pick(subMask, sbcSub8, sbcSub4)
	if sub == 0 {
		sub = sbcSub8
	}
	alloc := pick(allocMask, sbcAllocLoud, sbcAllocSNR)
	if alloc == 0 {
		alloc = sbcAllocLoud
	}
	if minBP < 2 {
		minBP = 2
	}
	if maxBP < minBP {
		maxBP = minBP
	}
	if maxBP > sbcMaxBitpool {
		maxBP = sbcMaxBitpool
	}
	if maxBP < minBP {
		maxBP = minBP
	}
	return []byte{freq | mode, block | sub | alloc, minBP, maxBP}
}

// A2DPSBCPayload strips the RTP and A2DP media headers and returns the SBC
// frames. It returns nil for malformed or empty packets.
func A2DPSBCPayload(packet []byte) []byte {
	data, _ := A2DPSBCPayloadFrames(packet)
	return data
}

// A2DPSBCPayloadFrames is A2DPSBCPayload plus the A2DP media header's frame
// count, which a chunked decoder needs to discard a warm-up frame.
func A2DPSBCPayloadFrames(packet []byte) ([]byte, int) {
	if len(packet) < 13 || packet[0]>>6 != 2 {
		return nil, 0
	}
	cc := int(packet[0] & 0x0f)
	off := 12 + cc*4
	if packet[0]&0x10 != 0 {
		if len(packet) < off+4 {
			return nil, 0
		}
		ext := int(binary.BigEndian.Uint16(packet[off+2 : off+4]))
		off += 4 + ext*4
	}
	if off >= len(packet) {
		return nil, 0
	}
	if packet[0]&0x20 != 0 {
		pad := int(packet[len(packet)-1])
		if pad > 0 && pad <= len(packet)-off {
			packet = packet[:len(packet)-pad]
		}
	}
	payload := packet[off:]
	if len(payload) < 2 {
		return nil, 0
	}
	// A2DP SBC media header: bits 3-0 are the number of frames; bits 7/6/5
	// are F/S/L. A one-frame packet therefore has a zero high nibble.
	frames := int(payload[0] & 0x0f)
	return payload[1:], frames
}
