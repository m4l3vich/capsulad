package bluetooth

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
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
	a2dpEndpointID = dbus.ObjectPath("/org/capsula/a2dp/sbc")
)

// SBC capability bits (A2DP section 4.3.2).
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
func (b *BlueZ) RegisterA2DPSink(ctx context.Context) error {
	if err := b.EnsurePowered(ctx); err != nil {
		return err
	}
	b.endpointMu.Lock()
	if b.endpoint != nil {
		b.endpointMu.Unlock()
		return nil
	}
	b.endpointMu.Unlock()

	ep := &sbcEndpoint{b: b}
	if err := b.conn.Export(ep, a2dpEndpointID, "org.bluez.MediaEndpoint1"); err != nil {
		return fmt.Errorf("export A2DP endpoint: %w", err)
	}
	props := map[string]dbus.Variant{
		"UUID":  dbus.MakeVariant(a2dpSinkUUID),
		"Codec": dbus.MakeVariant(byte(0)),
		// Sampling frequency, channel mode, block length, subbands,
		// allocation method, minimum and maximum bitpool.
		"Capabilities": dbus.MakeVariant([]byte{0xF0, 0x0F, 0xF0, 0x0C, 0x03, 0x02, 0x35}),
	}
	adapter := b.adapterPath()
	if adapter == "" {
		return ErrUnavailable
	}
	if err := b.conn.Object(bluezService, adapter).CallWithContext(ctx,
		"org.bluez.Media1.RegisterEndpoint", 0, a2dpEndpointID, props).Err; err != nil {
		return fmt.Errorf("register A2DP endpoint: %w", err)
	}
	b.endpointMu.Lock()
	b.endpoint = ep
	b.endpointPath = a2dpEndpointID
	b.endpointMu.Unlock()
	return nil
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
	// BlueZ 5.65's MediaTransport1 has no Endpoint property, but
	// SetConfiguration gives us the exact path it will use.
	if transportPath != "" {
		if props, ok := objs[transportPath]["org.bluez.MediaTransport1"]; ok {
			return transportPath, props
		}
	}
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.MediaTransport1"]
		if !ok {
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
	b.endpointMu.Lock()
	path := b.endpointPath
	b.endpointPath = ""
	b.endpoint = nil
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

func selectSBCConfig(caps []byte) []byte {
	if len(caps) < 4 {
		return []byte{0x21, 0x15, 0x02, 0x35}
	}
	var freqMask, modeMask, blockMask, subMask, allocMask byte
	var minBP, maxBP byte
	if len(caps) >= 7 {
		// Local endpoint capability form: one bitmask per field.
		freqMask, modeMask = caps[0], caps[1]
		blockMask, subMask, allocMask = caps[2], caps[3], caps[4]
		minBP, maxBP = caps[5], caps[6]
	} else {
		// BlueZ passes the remote SEP's four-byte SBC capabilities:
		// frequency+channel mode, block+subbands+allocation, min/max bitpool.
		freqMask, modeMask = caps[0], caps[0]
		blockMask, subMask, allocMask = caps[1], caps[1], caps[1]
		minBP, maxBP = caps[2], caps[3]
	}
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
	if maxBP > 53 {
		maxBP = 53
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
	frames := int(payload[0] >> 4)
	if frames == 0 {
		return nil, 0
	}
	return payload[1:], frames
}
