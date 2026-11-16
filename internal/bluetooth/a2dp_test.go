package bluetooth

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/godbus/dbus/v5"
)

// The advertised capability IE must use the spec's four-octet SBC layout. BlueZ
// copies it verbatim into the local SEP, so a malformed blob is a SEP a strict
// source rejects (and then it has only SCO to fall back on).
func TestA2DPSinkCapabilitiesAreConformant(t *testing.T) {
	caps := sbcSinkCapabilities()
	if len(caps) != 4 {
		t.Fatalf("capability IE = %x (%d octets), want the spec's 4 octets", caps, len(caps))
	}
	freq, mode := caps[0]&0xF0, caps[0]&0x0F
	for bit, name := range map[byte]string{
		sbcFreq44k: "44.1 kHz", sbcFreq48k: "48 kHz", sbcFreq32k: "32 kHz", sbcFreq16k: "16 kHz",
	} {
		if freq&bit == 0 {
			t.Fatalf("frequency mask %#02x does not advertise %s", freq, name)
		}
	}
	if mode&sbcModeJoint == 0 || mode&sbcModeStereo == 0 {
		t.Fatalf("channel-mode mask %#02x does not advertise joint stereo and stereo", mode)
	}
	block, sub, alloc := caps[1]&0xF0, caps[1]&0x0C, caps[1]&0x03
	if block&sbcBlock16 == 0 || block&sbcBlock12 == 0 || block&sbcBlock8 == 0 {
		t.Fatalf("block mask %#02x does not advertise 16/12/8 blocks", block)
	}
	if sub&sbcSub8 == 0 || sub&sbcSub4 == 0 {
		t.Fatalf("subband mask %#02x does not advertise 8/4 subbands", sub)
	}
	if alloc&sbcAllocLoud == 0 || alloc&sbcAllocSNR == 0 {
		t.Fatalf("allocation mask %#02x does not advertise loudness and SNR", alloc)
	}
	if caps[2] != 2 {
		t.Fatalf("minimum bitpool = %d, want 2", caps[2])
	}
	if caps[2] > caps[3] {
		t.Fatalf("minimum bitpool %d exceeds maximum %d", caps[2], caps[3])
	}
	if caps[3] < 53 {
		t.Fatalf("maximum bitpool = %d, want at least the A2DP high-quality 53", caps[3])
	}
	// The endpoint properties must carry exactly that blob.
	props := a2dpEndpointProps()
	if got := variantBytes(props["Capabilities"]); !bytes.Equal(got, caps) {
		t.Fatalf("endpoint capabilities = %x, want %x", got, caps)
	}
}

// Our own advertised capabilities must also be negotiable by our own selection
// rule: the local SEP and the local answer cannot contradict each other.
func TestSelectSBCConfigAcceptsOwnCapabilities(t *testing.T) {
	config := selectSBCConfig(sbcSinkCapabilities())
	advertised := sbcSinkCapabilities()
	if config[0]&advertised[0] != config[0] {
		t.Fatalf("config %x selects bits the sink does not advertise (%x)", config[0], advertised[0])
	}
	if config[1]&advertised[1] != config[1] {
		t.Fatalf("config %x selects bits the sink does not advertise (%x)", config[1], advertised[1])
	}
	if config[2] < advertised[2] || config[3] > advertised[3] {
		t.Fatalf("config bitpool %d..%d outside advertised %d..%d",
			config[2], config[3], advertised[2], advertised[3])
	}
}

// Real four-octet SBC capabilities as sources actually advertise them. Every
// answer must be a configuration that source offered, with the highest usable
// bitpool it allows (bounded by the sink's high-quality ceiling).
func TestSelectSBCConfigVendorCapabilities(t *testing.T) {
	cases := []struct {
		name string
		caps []byte
		want []byte
	}{
		{
			// macOS: 44.1 kHz joint stereo, 16 blocks / 8 subbands, loudness,
			// bitpool 2..250. The ceiling is clamped to 53.
			name: "apple", caps: []byte{0x21, 0x15, 0x02, 0xfa},
			want: []byte{0x21, 0x15, 0x02, 0x35},
		},
		{
			// Android/Qualcomm: 44.1+48 kHz, every channel mode, every block
			// length and subband count, both allocation methods, 2..53.
			name: "android", caps: []byte{0x3f, 0xff, 0x02, 0x35},
			want: []byte{0x21, 0x15, 0x02, 0x35},
		},
		{
			// Older Android: joint+dual mode, 16/8 blocks, 8 subbands,
			// loudness, 2..53.
			name: "android-legacy", caps: []byte{0x35, 0x55, 0x02, 0x35},
			want: []byte{0x21, 0x15, 0x02, 0x35},
		},
		{
			// Windows: 44.1/32/16 kHz, stereo only, 16/8 blocks, 8 subbands,
			// SNR, bitpool 2..45.
			name: "windows", caps: []byte{0xe2, 0x56, 0x02, 0x2d},
			want: []byte{0x22, 0x16, 0x02, 0x2d},
		},
		{
			// A conservative phone: 48 kHz stereo, 4 blocks / 4 subbands, SNR,
			// bitpool 2..32.
			name: "conservative", caps: []byte{0x12, 0x8a, 0x02, 0x20},
			want: []byte{0x12, 0x8a, 0x02, 0x20},
		},
		{
			// 32 kHz mono, 8 blocks / 4 subbands, loudness, 2..35.
			name: "32k-mono", caps: []byte{0x48, 0x49, 0x02, 0x23},
			want: []byte{0x48, 0x49, 0x02, 0x23},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectSBCConfig(tc.caps)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("config = %x, want %x", got, tc.want)
			}
			// Whatever we choose has to be offered by the remote SEP.
			if got[0]&tc.caps[0] != got[0] {
				t.Fatalf("config %x selects frequency/mode bits the source did not offer (%x)", got[0], tc.caps[0])
			}
			if got[1]&tc.caps[1] != got[1] {
				t.Fatalf("config %x selects frame bits the source did not offer (%x)", got[1], tc.caps[1])
			}
			if got[2] < tc.caps[2] || got[3] > tc.caps[3] || got[2] > got[3] {
				t.Fatalf("config bitpool %d..%d outside the source's %d..%d", got[2], got[3], tc.caps[2], tc.caps[3])
			}
		})
	}
}

func TestSelectSBCConfigDefaultsOnShortCapabilities(t *testing.T) {
	got := selectSBCConfig(nil)
	want := []byte{0x21, 0x15, 0x02, 0x35}
	if !bytes.Equal(got, want) {
		t.Fatalf("config = %x, want %x", got, want)
	}
}

func TestA2DPSBCPayloadStripsHeaders(t *testing.T) {
	sbc := []byte{0x9c, 0x21, 0x15, 0x02, 0x35, 0xaa, 0xbb}
	payload := append([]byte{0x02}, sbc...) // two frames in bits 3-0 (nibble high)
	packet := make([]byte, 12+len(payload))
	packet[0] = 0x80 // RTP v2, no CSRC/extension/padding
	packet[1] = 96
	binary.BigEndian.PutUint16(packet[2:], 7)
	binary.BigEndian.PutUint32(packet[4:], 44100)
	binary.BigEndian.PutUint32(packet[8:], 0x11223344)
	copy(packet[12:], payload)
	got := A2DPSBCPayload(packet)
	if !bytes.Equal(got, sbc) {
		t.Fatalf("payload = %x, want %x", got, sbc)
	}
}

func TestA2DPSBCPayloadHandlesCSRCAndPadding(t *testing.T) {
	sbc := []byte{0x9c, 0x21, 0x15, 0x02, 0x35}
	payload := append([]byte{0x01}, sbc...)
	packet := make([]byte, 12+8+len(payload)+4)
	packet[0] = 0x80 | 0x02 | 0x20 // two CSRC words + padding
	packet[1] = 96
	copy(packet[12+8:], payload)
	packet[len(packet)-1] = 4
	got := A2DPSBCPayload(packet)
	if !bytes.Equal(got, sbc) {
		t.Fatalf("payload = %x, want %x", got, sbc)
	}
}

func TestA2DPSBCPayloadRejectsEmptyAndGarbage(t *testing.T) {
	if got := A2DPSBCPayload(nil); got != nil {
		t.Fatalf("nil packet returned %x", got)
	}
	if got := A2DPSBCPayload(make([]byte, 12)); got != nil {
		t.Fatalf("empty payload returned %x", got)
	}
	// Header-only payload: zero frames and no frame data.
	packet := make([]byte, 13)
	packet[0] = 0x80
	packet[12] = 0x00
	if got := A2DPSBCPayload(packet); got != nil {
		t.Fatalf("header-only packet returned %x", got)
	}
}

func TestFindA2DPTransportUsesConfiguredPathWithoutEndpointProperty(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_BC_D0_74_32_51_DC/sep2/fd1")
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		path: {
			"org.bluez.MediaTransport1": {
				"UUID":  dbus.MakeVariant("0000110b-0000-1000-8000-00805f9b34fb"),
				"Codec": dbus.MakeVariant(byte(0)),
				"State": dbus.MakeVariant("active"),
			},
		},
	}
	b := &BlueZ{}
	got, props := b.findA2DPTransport(objs, "/org/capsulad/a2dp/sbc", path)
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	if props == nil {
		t.Fatal("missing transport properties")
	}
}

func TestFindA2DPTransportFallsBackToUUIDAndCodec(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_BC_D0_74_32_51_DC/sep2/fd0")
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		path: {
			"org.bluez.MediaTransport1": {
				"UUID":  dbus.MakeVariant("0000110b-0000-1000-8000-00805f9b34fb"),
				"Codec": dbus.MakeVariant(byte(0)),
				"State": dbus.MakeVariant("pending"),
			},
		},
	}
	b := &BlueZ{}
	got, _ := b.findA2DPTransport(objs, "/org/capsulad/a2dp/sbc", "")
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
}

func TestFindA2DPTransportSkipsOtherEndpoints(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_BC_D0_74_32_51_DC/sep2/fd0")
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		path: {
			"org.bluez.MediaTransport1": {
				"UUID":     dbus.MakeVariant("0000110b-0000-1000-8000-00805f9b34fb"),
				"Codec":    dbus.MakeVariant(byte(0)),
				"State":    dbus.MakeVariant("active"),
				"Endpoint": dbus.MakeVariant("/MediaEndpoint/A2DPSink/sbc"),
			},
		},
	}
	b := &BlueZ{}
	got, _ := b.findA2DPTransport(objs, "/org/capsulad/a2dp/sbc", "")
	if got != "" {
		t.Fatalf("path = %q, want empty", got)
	}
}

func TestA2DPSBCPayloadFrames(t *testing.T) {
	sbc := []byte{0x9c, 0xbd, 0x35, 0x03}
	payload := append([]byte{0x03}, sbc...) // three frames in bits 3-0
	packet := make([]byte, 12+len(payload))
	packet[0] = 0x80
	copy(packet[12:], payload)
	data, frames := A2DPSBCPayloadFrames(packet)
	if frames != 3 {
		t.Fatalf("frames = %d, want 3", frames)
	}
	if !bytes.Equal(data, sbc) {
		t.Fatalf("data = %x, want %x", data, sbc)
	}
}

func TestFindA2DPTransportRejectsIdleStoredPath(t *testing.T) {
	stored := dbus.ObjectPath("/org/bluez/hci0/dev_BC_D0_74_32_51_DC/sep2/fd0")
	active := dbus.ObjectPath("/org/bluez/hci0/dev_BC_D0_74_32_51_DC/sep2/fd1")
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		stored: {
			"org.bluez.MediaTransport1": {
				"UUID":  dbus.MakeVariant("0000110b-0000-1000-8000-00805f9b34fb"),
				"Codec": dbus.MakeVariant(byte(0)),
				"State": dbus.MakeVariant("idle"),
			},
		},
		active: {
			"org.bluez.MediaTransport1": {
				"UUID":  dbus.MakeVariant("0000110b-0000-1000-8000-00805f9b34fb"),
				"Codec": dbus.MakeVariant(byte(0)),
				"State": dbus.MakeVariant("active"),
			},
		},
	}
	b := &BlueZ{}
	got, _ := b.findA2DPTransport(objs, "/org/capsulad/a2dp/sbc", stored)
	if got != active {
		t.Fatalf("path = %q, want active %q", got, active)
	}
}

func TestFindA2DPTransportReturnsNothingWhileIdle(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_BC_D0_74_32_51_DC/sep2/fd0")
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		path: {
			"org.bluez.MediaTransport1": {
				"UUID":  dbus.MakeVariant("0000110b-0000-1000-8000-00805f9b34fb"),
				"Codec": dbus.MakeVariant(byte(0)),
				"State": dbus.MakeVariant("idle"),
			},
		},
	}
	b := &BlueZ{}
	got, _ := b.findA2DPTransport(objs, "/org/capsulad/a2dp/sbc", path)
	if got != "" {
		t.Fatalf("path = %q, want empty while idle", got)
	}
}
