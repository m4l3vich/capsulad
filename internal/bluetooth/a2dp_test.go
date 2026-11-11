package bluetooth

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestSelectSBCConfigPrefersBoardProfile(t *testing.T) {
	caps := []byte{0xF0, 0x0F, 0xF0, 0x0C, 0x03, 0x02, 0x35}
	got := selectSBCConfig(caps)
	want := []byte{0x21, 0x15, 0x02, 0x35}
	if !bytes.Equal(got, want) {
		t.Fatalf("config = %x, want %x", got, want)
	}
}

func TestSelectSBCConfigBlueZRemoteCapabilities(t *testing.T) {
	// The Mac's SBC SEP as BlueZ 5.65 passes it: 44.1 kHz joint stereo,
	// 16 blocks / 8 subbands, loudness, bitpool 2..250.
	caps := []byte{0x21, 0x15, 0x02, 0xfa}
	got := selectSBCConfig(caps)
	want := []byte{0x21, 0x15, 0x02, 0x35}
	if !bytes.Equal(got, want) {
		t.Fatalf("config = %x, want %x", got, want)
	}
}

func TestSelectSBCConfigFallsBackToAdvertised(t *testing.T) {
	// 48 kHz mono, 4 blocks / 4 subbands, SNR, bitpool 2..32.
	caps := []byte{0x10, 0x08, 0x80, 0x08, 0x02, 0x02, 0x20}
	got := selectSBCConfig(caps)
	want := []byte{0x18, 0x8A, 0x02, 0x20}
	if !bytes.Equal(got, want) {
		t.Fatalf("config = %x, want %x", got, want)
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
	payload := append([]byte{0x20}, sbc...) // two frames (nibble high)
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
	payload := append([]byte{0x10}, sbc...)
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
	// Header says zero frames.
	packet := make([]byte, 14)
	packet[0] = 0x80
	packet[12] = 0x00
	if got := A2DPSBCPayload(packet); got != nil {
		t.Fatalf("zero-frame packet returned %x", got)
	}
}

func TestFindA2DPTransportUsesConfiguredPathWithoutEndpointProperty(t *testing.T) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_BC_D0_74_32_51_DC/sep2/fd1")
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
	got, props := b.findA2DPTransport(objs, "/org/capsula/a2dp/sbc", path)
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
			},
		},
	}
	b := &BlueZ{}
	got, _ := b.findA2DPTransport(objs, "/org/capsula/a2dp/sbc", "")
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
				"Endpoint": dbus.MakeVariant("/MediaEndpoint/A2DPSink/sbc"),
			},
		},
	}
	b := &BlueZ{}
	got, _ := b.findA2DPTransport(objs, "/org/capsula/a2dp/sbc", "")
	if got != "" {
		t.Fatalf("path = %q, want empty", got)
	}
}
