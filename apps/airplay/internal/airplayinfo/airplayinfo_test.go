package airplayinfo

import (
	"bytes"
	"reflect"
	"sort"
	"testing"

	"capsulad/apps/airplay/internal/plist"
)

// stageKeys is the exact key set of the /info response body, transcribed from
// the reference receivers per key. The reference builds the same document for
// both request stages: handle_get_info merges the TXT records in as txtAirPlay
// on the stage 1 path (rtsp.c:1339-1340) and again on the stage 2 path
// (rtsp.c:1366-1367), so both lists are identical by construction and are kept
// separate here to pin each stage against its own reference line range.
var (
	stage1Keys = []string{
		"audioLatencies",           // ap2-receiver.py:80-94
		"deviceID",                 // rtsp.c:1198
		"features",                 // rtsp.c:1196
		"featuresEx",               // rtsp.c:1194
		"initialVolume",            // rtsp.c:1208
		"keepAliveLowPower",        // ap2-receiver.py:102
		"keepAliveSendStatsAsBody", // template plists/get_info_response.xml:18-19
		"manufacturer",             // ap2-receiver.py:104
		"model",                    // rtsp.c:1201
		"name",                     // rtsp.c:1200
		"nameIsFactoryDefault",     // ap2-receiver.py:107
		"pi",                       // rtsp.c:1199
		"pk",                       // rtsp.c:1202-1203
		"playbackCapabilities",     // template plists/get_info_response.xml:7-15
		"protocolVersion",          // template plists/get_info_response.xml:20-21
		"psi",                      // rtsp.c:1192
		"receiverHDRCapability",    // template plists/get_info_response.xml:53-54
		"sdk",                      // ap2-receiver.py:110
		"senderAddress",            // rtsp.c:1204-1207
		"sourceVersion",            // protocols.md:1783; ap2-receiver.py:111
		"statusFlags",              // rtsp.c:1197
		"supportedFormats",         // rtsp.c:1215-1248
		"txtAirPlay",               // rtsp.c:1339-1340
		"volumeControlType",        // rtsp.c:1210-1211
		"vv",                       // rtsp.c:1209
	}
	// stage2Keys is the same set from the stage 2 path (rtsp.c:1359-1368).
	stage2Keys = append([]string(nil), stage1Keys...)
)

const (
	testLTPKHex = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	// testFeatures is the captured Apple TV mask: features=0x4A7FDFD5,0x3C155FDE
	// (protocols.md:1783).
	testFeatures = 0x3C155FDE4A7FDFD5
	// testDeviceID is the captured Apple TV device id.
	testDeviceID = "AA:BB:CC:DD:EE:FF"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	pk := make([]byte, 32)
	for i := range pk {
		pk[i] = byte(i)
	}
	return Config{
		Name:                "Kitchen",
		DeviceID:            testDeviceID,
		PairingID:           "aabbccdd-eeff-0011-2233-445566778899",
		SystemPairingID:     "66eaadc6-18a1-4802-aada-aabd84b22c58",
		Model:               "AudioAccessory5,1",
		Manufacturer:        "capsulad",
		SourceVersion:       "366.0",
		Features:            testFeatures,
		StatusFlags:         0x4,
		VolumeDB:            -20,
		VV:                  DefaultVV,
		VolumeControlType:   DefaultVolumeControlType,
		AudioStreamFormats:  DefaultAudioStreamFormats,
		BufferStreamFormats: 0x200000, // test fixture: ALAC 48K/F24/stereo
		OutputLatencyMicros: 400000,
		PairingLTPK:         pk,
		SenderAddress:       "10.0.0.7:51000",
	}
}

func testTXT() TXT {
	return TXT{
		"acl":       "0",
		"btaddr":    "00:00:00:00:00:00",
		"deviceid":  "aabbccddeeff",
		"fex":       "1d9/St5fFTw",
		"features":  "0x4a7fdfd5,0x3c155fde",
		"flags":     "0x4",
		"gid":       "66eaadc6-18a1-4802-aada-aabd84b22c58",
		"model":     "AudioAccessory5,1",
		"protovers": "1.1",
		"pi":        "aabbccdd-eeff-0011-2233-445566778899",
		"psi":       "66eaadc6-18a1-4802-aada-aabd84b22c58",
		"srcvers":   "366.0",
		"vv":        "2",
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestDeviceInfoStageKeySets pins the exact key set of both /info stages
// against the reference lists above, without senderAddress and with it.
func TestDeviceInfoStageKeySets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		qualifier bool
		want      []string
	}{
		{"stage 1 (qualifier)", true, stage1Keys},
		{"stage 2", false, stage2Keys},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := DeviceInfo(testConfig(t), testTXT(), tc.qualifier)
			if err != nil {
				t.Fatalf("DeviceInfo: %v", err)
			}
			if got := keysOf(doc); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("key set mismatch:\n got %v\nwant %v", got, tc.want)
			}
			// Without a sender address the reference has nothing to put in
			// senderAddress (rtsp.c:1204-1207 formats it from the connection),
			// and no other key may appear or disappear.
			cfg := testConfig(t)
			cfg.SenderAddress = ""
			doc, err = DeviceInfo(cfg, testTXT(), tc.qualifier)
			if err != nil {
				t.Fatalf("DeviceInfo without senderAddress: %v", err)
			}
			want := make([]string, 0, len(tc.want)-1)
			for _, key := range tc.want {
				if key != "senderAddress" {
					want = append(want, key)
				}
			}
			if got := keysOf(doc); !reflect.DeepEqual(got, want) {
				t.Errorf("key set without senderAddress:\n got %v\nwant %v", got, want)
			}
			if _, ok := doc["senderAddress"]; ok {
				t.Error("senderAddress present although the config carries none")
			}
		})
	}
}

// TestDeviceInfoValueTypes checks every value's Go type, which is what fixes
// the plist type the encoder will write.
func TestDeviceInfoValueTypes(t *testing.T) {
	doc, err := DeviceInfo(testConfig(t), testTXT(), true)
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	wantTypes := map[string]any{
		"deviceID":                 "",
		"features":                 int64(0),
		"model":                    "",
		"name":                     "",
		"pi":                       "",
		"psi":                      "",
		"pk":                       []byte(nil),
		"protocolVersion":          "",
		"sourceVersion":            "",
		"statusFlags":              int64(0),
		"keepAliveLowPower":        true,
		"keepAliveSendStatsAsBody": false,
		"audioLatencies":           []any(nil),
		"supportedFormats":         map[string]any(nil),
		"txtAirPlay":               []byte(nil),
		"featuresEx":               "",
		"vv":                       int64(0),
		"volumeControlType":        int64(0),
		"senderAddress":            "",
		"playbackCapabilities":     map[string]any(nil),
		"receiverHDRCapability":    "",
		"initialVolume":            float64(0),
		"nameIsFactoryDefault":     false,
		"sdk":                      "",
		"manufacturer":             "",
	}
	for key, exemplar := range wantTypes {
		value, ok := doc[key]
		if !ok {
			t.Errorf("%s missing", key)
			continue
		}
		want := reflect.TypeOf(exemplar)
		if got := reflect.TypeOf(value); got != want {
			t.Errorf("%s: got %v (%v), want %v", key, got, value, want)
		}
	}
}

// TestDeviceInfoFixedValues pins the constants the references fix.
func TestDeviceInfoFixedValues(t *testing.T) {
	cfg := testConfig(t)
	doc, err := DeviceInfo(cfg, testTXT(), true)
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	for key, want := range map[string]any{
		"deviceID":                 cfg.DeviceID,
		"protocolVersion":          "1.1",
		"receiverHDRCapability":    "4k60",
		"sdk":                      "AirPlay;2.0.2",
		"keepAliveLowPower":        true,
		"keepAliveSendStatsAsBody": false,
		"nameIsFactoryDefault":     false,
		"manufacturer":             "capsulad",
		"sourceVersion":            "366.0",
		"name":                     "Kitchen",
		"model":                    "AudioAccessory5,1",
		"pi":                       cfg.PairingID,
		"psi":                      cfg.SystemPairingID,
		"senderAddress":            cfg.SenderAddress,
		"features":                 int64(testFeatures),
		"statusFlags":              int64(0x4),
		"vv":                       int64(DefaultVV),
		"volumeControlType":        int64(DefaultVolumeControlType),
		"initialVolume":            -20.0,
		"featuresEx":               "1d9/St5fFTw",
	} {
		if got := doc[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if got := doc["pk"]; !bytes.Equal(got.([]byte), cfg.PairingLTPK) {
		t.Errorf("pk = %x, want the ed25519 public key %x", got, cfg.PairingLTPK)
	}
	if got := doc["playbackCapabilities"]; !reflect.DeepEqual(got, map[string]any{
		"supportsInterstitials":         false,
		"supportsFPSSecureStop":         false,
		"supportsUIForAudioOnlyContent": false,
	}) {
		t.Errorf("playbackCapabilities = %#v", got)
	}
}

// TestAudioLatencies pins the one entry the OpenAirplay reference sends: input
// zero, output from the configuration (ap2-receiver.py:80-94, whose other
// entries only repeat it per audioType).
func TestAudioLatencies(t *testing.T) {
	cfg := testConfig(t)
	cfg.OutputLatencyMicros = 400000
	doc, err := DeviceInfo(cfg, testTXT(), true)
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	want := []any{map[string]any{"inputLatencyMicros": int64(0), "outputLatencyMicros": int64(400000)}}
	if got := doc["audioLatencies"]; !reflect.DeepEqual(got, want) {
		t.Errorf("audioLatencies = %#v, want %#v", got, want)
	}
}

// TestSupportedFormatsBits pins the supportedFormats values and their encodings:
// audioStream is the reference's 262144, and bufferStream is the caller's
// explicit capability mask. This package has no second buffered default.
func TestSupportedFormatsBits(t *testing.T) {
	cfg := testConfig(t)
	doc, err := DeviceInfo(cfg, testTXT(), false)
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	formats, ok := doc["supportedFormats"].(map[string]any)
	if !ok {
		t.Fatalf("supportedFormats is %T", doc["supportedFormats"])
	}
	if got := formats["audioStream"]; got != int64(262144) {
		t.Errorf("audioStream = %v, want int64(262144)", got)
	}
	if got := formats["bufferStream"]; got != int64(0x200000) {
		t.Errorf("bufferStream = %v, want int64(0x200000)", got)
	}
	// The configurable bits are OR-able without disturbing the encodings.
	cfg.AudioStreamFormats = 21235712
	cfg.BufferStreamFormats = 0x200000 | 0x400000 | 0x800000
	doc, err = DeviceInfo(cfg, testTXT(), false)
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	formats = doc["supportedFormats"].(map[string]any)
	if got := formats["audioStream"]; got != int64(21235712) {
		t.Errorf("audioStream = %v, want int64(21235712)", got)
	}
	if got := formats["bufferStream"]; got != int64(0xE00000) {
		t.Errorf("bufferStream = %v, want int64(0xE00000)", got)
	}
}

// TestDeviceInfoPlistEncoding asserts the documents survive a binary plist
// round trip with the reference's value kinds: integers (not UIDs, which is
// what a uint64 would encode to), reals, data and booleans.
func TestDeviceInfoPlistEncoding(t *testing.T) {
	cfg := testConfig(t)
	doc, err := DeviceInfo(cfg, testTXT(), true)
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	body, err := plist.EncodeBinary(doc)
	if err != nil {
		t.Fatalf("EncodeBinary: %v", err)
	}
	decoded, err := plist.Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decoded %T, want map[string]any", decoded)
	}
	if keys := keysOf(got); !reflect.DeepEqual(keys, stage1Keys) {
		t.Errorf("round-tripped key set = %v, want %v", keys, stage1Keys)
	}
	for _, key := range []string{"features", "statusFlags", "vv", "volumeControlType"} {
		if _, ok := got[key].(int64); !ok {
			t.Errorf("%s round-tripped as %T, want int64 (a plist integer, not a UID)", key, got[key])
		}
	}
	if _, ok := got["initialVolume"].(float64); !ok {
		t.Errorf("initialVolume round-tripped as %T, want float64", got["initialVolume"])
	}
	for _, key := range []string{"pk", "txtAirPlay"} {
		if _, ok := got[key].([]byte); !ok {
			t.Errorf("%s round-tripped as %T, want []byte (plist data)", key, got[key])
		}
	}
	for _, key := range []string{"keepAliveLowPower", "keepAliveSendStatsAsBody", "nameIsFactoryDefault"} {
		if _, ok := got[key].(bool); !ok {
			t.Errorf("%s round-tripped as %T, want bool", key, got[key])
		}
	}
	formats, ok := got["supportedFormats"].(map[string]any)
	if !ok {
		t.Fatalf("supportedFormats round-tripped as %T", got["supportedFormats"])
	}
	for _, key := range []string{"audioStream", "bufferStream"} {
		if _, ok := formats[key].(int64); !ok {
			t.Errorf("supportedFormats[%s] round-tripped as %T, want int64", key, formats[key])
		}
	}
	latencies, ok := got["audioLatencies"].([]any)
	if !ok || len(latencies) != 1 {
		t.Fatalf("audioLatencies round-tripped as %T (%v)", got["audioLatencies"], got["audioLatencies"])
	}
	entry, ok := latencies[0].(map[string]any)
	if !ok {
		t.Fatalf("audioLatencies[0] round-tripped as %T", latencies[0])
	}
	for _, key := range []string{"inputLatencyMicros", "outputLatencyMicros"} {
		if _, ok := entry[key].(int64); !ok {
			t.Errorf("audioLatencies[0][%s] round-tripped as %T, want int64", key, entry[key])
		}
	}
	if !bytes.Equal(got["txtAirPlay"].([]byte), EncodeTXT(testTXT())) {
		t.Error("txtAirPlay does not carry the encoded TXT records")
	}
}

// TestUpdateInfo pins the event-channel message: the {"type","value"} wrapper of
// ap2_event_receiver.c:138-140 with the /info document inside it.
func TestUpdateInfo(t *testing.T) {
	cfg := testConfig(t)
	msg, err := UpdateInfo(cfg, testTXT())
	if err != nil {
		t.Fatalf("UpdateInfo: %v", err)
	}
	if keys := keysOf(msg); !reflect.DeepEqual(keys, []string{"type", "value"}) {
		t.Fatalf("updateInfo key set = %v, want [type value]", keys)
	}
	if msg["type"] != "updateInfo" {
		t.Errorf("type = %v, want updateInfo", msg["type"])
	}
	value, ok := msg["value"].(map[string]any)
	if !ok {
		t.Fatalf("value is %T, want map[string]any", msg["value"])
	}
	if keys := keysOf(value); !reflect.DeepEqual(keys, stage1Keys) {
		t.Errorf("updateInfo value key set = %v, want %v", keys, stage1Keys)
	}
	body, err := plist.EncodeBinary(msg)
	if err != nil {
		t.Fatalf("EncodeBinary: %v", err)
	}
	decoded, err := plist.Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	root, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decoded %T", decoded)
	}
	if _, ok := root["value"].(map[string]any); !ok {
		t.Errorf("value round-tripped as %T, want a dictionary", root["value"])
	}
}

// TestFeaturesExMatchesAppleTVCapture derives the token for the captured Apple
// TV feature mask; "1d9/St5fFTw" is the string that device sends
// (docs/documentation/protocols.md:1783).
func TestFeaturesExMatchesAppleTVCapture(t *testing.T) {
	if got := FeaturesEx(testFeatures); got != "1d9/St5fFTw" {
		t.Errorf("FeaturesEx(%#x) = %q, want %q", testFeatures, got, "1d9/St5fFTw")
	}
	// The padding is stripped, so an all-zero mask yields a short token and
	// never a '='.
	if got := FeaturesEx(0); got != "AAAAAAAAAAA" || len(got) != 11 {
		t.Errorf("FeaturesEx(0) = %q", got)
	}
	if got := FeaturesEx(0x40000); bytes.Contains([]byte(got), []byte("=")) {
		t.Errorf("FeaturesEx(0x40000) = %q, want no padding", got)
	}
}

// TestDefaultSystemPairingID pins the psi derivation against a hand-computed
// value: MD5 of the device id's six bytes with the version nibble forced to 4
// and the variant bits to 10 (shairport.c:1949-1977).
func TestDefaultSystemPairingID(t *testing.T) {
	for _, deviceID := range []string{"aabbccddeeff", "AA:BB:CC:DD:EE:FF"} {
		got, err := DefaultSystemPairingID(deviceID)
		if err != nil {
			t.Fatalf("DefaultSystemPairingID(%q): %v", deviceID, err)
		}
		if want := "66eaadc6-18a1-4802-aada-aabd84b22c58"; got != want {
			t.Errorf("DefaultSystemPairingID(%q) = %q, want %q", deviceID, got, want)
		}
	}
	// An empty SystemPairingID falls back to the derivation.
	cfg := testConfig(t)
	cfg.DeviceID = "aabbccddeeff"
	cfg.SystemPairingID = ""
	doc, err := DeviceInfo(cfg, testTXT(), true)
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	if got := doc["psi"]; got != "66eaadc6-18a1-4802-aada-aabd84b22c58" {
		t.Errorf("psi = %v, want the derived UUID", got)
	}
}

// TestDeviceInfoRejectsInvalidConfig: an id or key a sender could not use must
// fail loudly rather than be advertised.
func TestDeviceInfoRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.DeviceID = "aabbccddee"
	if _, err := DeviceInfo(cfg, testTXT(), true); err == nil {
		t.Error("DeviceInfo accepted an 11-digit device id")
	}
	cfg = testConfig(t)
	cfg.DeviceID = "aabbccddeezz"
	if _, err := DeviceInfo(cfg, testTXT(), true); err == nil {
		t.Error("DeviceInfo accepted a non-hexadecimal device id")
	}
	cfg = testConfig(t)
	cfg.PairingLTPK = cfg.PairingLTPK[:31]
	if _, err := DeviceInfo(cfg, testTXT(), true); err == nil {
		t.Error("DeviceInfo accepted a 31-byte pk")
	}
	cfg = testConfig(t)
	cfg.PairingLTPK = nil
	if _, err := UpdateInfo(cfg, testTXT()); err == nil {
		t.Error("UpdateInfo accepted an empty pk")
	}
}

// TestEncodeTXTMatchesAppleTVCapture checks the encoder against the first
// records of the blob the captured Apple TV sends
// (docs/documentation/protocols.md:1783):
//
//	txtAirPlay b"\x05acl=0\x18btaddr=FF:EE:DD:CC:BB:AA\x1adeviceid=AA:BB:CC:DD:EE:FF…
func TestEncodeTXTMatchesAppleTVCapture(t *testing.T) {
	txt := TXT{
		"acl":      "0",
		"btaddr":   "FF:EE:DD:CC:BB:AA",
		"deviceid": "AA:BB:CC:DD:EE:FF",
		"fex":      "1d9/St5fFTw",
		"features": "0x4A7FDFD5,0x3C155FDE",
		"flags":    "0x244",
		"gid":      "4D826039-0F40-4605-AD11-A6516183BAA6",
	}
	want := []byte("\x05acl=0" +
		"\x18btaddr=FF:EE:DD:CC:BB:AA" +
		"\x1adeviceid=AA:BB:CC:DD:EE:FF" +
		"\x0ffex=1d9/St5fFTw" +
		"\x1efeatures=0x4A7FDFD5,0x3C155FDE" +
		"\x0bflags=0x244" +
		"\x28gid=4D826039-0F40-4605-AD11-A6516183BAA6")
	if got := EncodeTXT(txt); !bytes.Equal(got, want) {
		t.Errorf("EncodeTXT:\n got %q\nwant %q", got, want)
	}
}

// TestEncodeTXTReferenceOrder checks the full record set in the order
// generateTxtDataValueInfo writes it (rtsp.c:1069-1173).
func TestEncodeTXTReferenceOrder(t *testing.T) {
	txt := TXT{
		"acl":       "0",
		"btaddr":    "00:00:00:00:00:00",
		"deviceid":  "aabbccddeeff",
		"fex":       "1d9/St5fFTw",
		"features":  "0x4a7fdfd5,0x3c155fde",
		"flags":     "0x20000",
		"gid":       "66eaadc6-18a1-4802-aada-aabd84b22c58",
		"igl":       "0",
		"gcgl":      "0",
		"pgid":      "66eaadc6-18a1-4802-aada-aabd84b22c58",
		"pgcgl":     "0",
		"model":     "AudioAccessory5,1",
		"protovers": "1.1",
		"pi":        "aabbccdd-eeff-0011-2233-445566778899",
		"psi":       "66eaadc6-18a1-4802-aada-aabd84b22c58",
		"pk":        testLTPKHex,
		"srcvers":   "366.0",
		"vv":        "2",
	}
	want := []byte("\x05acl=0" +
		"\x18btaddr=00:00:00:00:00:00" +
		"\x15deviceid=aabbccddeeff" +
		"\x0ffex=1d9/St5fFTw" +
		"\x1efeatures=0x4a7fdfd5,0x3c155fde" +
		"\x0dflags=0x20000" +
		"\x28gid=66eaadc6-18a1-4802-aada-aabd84b22c58" +
		"\x05igl=0" +
		"\x06gcgl=0" +
		"\x29pgid=66eaadc6-18a1-4802-aada-aabd84b22c58" +
		"\x07pgcgl=0" +
		"\x17model=AudioAccessory5,1" +
		"\x0dprotovers=1.1" +
		"\x27pi=aabbccdd-eeff-0011-2233-445566778899" +
		"\x28psi=66eaadc6-18a1-4802-aada-aabd84b22c58" +
		"\x43pk=" + testLTPKHex +
		"\x0dsrcvers=366.0" +
		"\x04vv=2")
	if got := EncodeTXT(txt); !bytes.Equal(got, want) {
		t.Errorf("EncodeTXT:\n got %q\nwant %q", got, want)
	}
}

// TestEncodeTXTSortsUnorderedKeys: records outside the reference's list are
// appended in ascending key order, so the receiver's _airplay._tcp set — which
// adds name, serialNumber, sf, manufacturer, acl, gcgl and gid — still encodes
// deterministically.
func TestEncodeTXTSortsUnorderedKeys(t *testing.T) {
	txt := TXT{"name": "Kitchen", "serialNumber": "AABBCCDDEEFF", "sf": "0x20000"}
	want := []byte("\x0cname=Kitchen\x19serialNumber=AABBCCDDEEFF\x0asf=0x20000")
	if got := EncodeTXT(txt); !bytes.Equal(got, want) {
		t.Errorf("EncodeTXT:\n got %q\nwant %q", got, want)
	}
	// Order within the reference list is the reference's, not alphabetical:
	// acl precedes features even though 'f' < 'a' is false and btaddr precedes
	// deviceid.
	txt = TXT{"vv": "2", "acl": "0", "gcgl": "0", "srcvers": "366.0"}
	want = []byte("\x05acl=0\x06gcgl=0\x0dsrcvers=366.0\x04vv=2")
	if got := EncodeTXT(txt); !bytes.Equal(got, want) {
		t.Errorf("EncodeTXT:\n got %q\nwant %q", got, want)
	}
}

// TestEncodeTXTSkipsRecordsThatCannotBeSent: a TXT record is one length byte
// plus at most 255 bytes, so an oversized or empty-keyed record has no wire
// form and is skipped instead of truncated.
func TestEncodeTXTSkipsRecordsThatCannotBeSent(t *testing.T) {
	txt := TXT{
		"acl":      "0",
		"features": "0x" + string(bytes.Repeat([]byte("a"), 300)),
		"":         "ignored",
	}
	want := []byte("\x05acl=0")
	if got := EncodeTXT(txt); !bytes.Equal(got, want) {
		t.Errorf("EncodeTXT:\n got %q\nwant %q", got, want)
	}
	if got := EncodeTXT(nil); got != nil {
		t.Errorf("EncodeTXT(nil) = %q, want nil", got)
	}
	// A record of exactly 255 bytes still fits.
	txt = TXT{"vv": string(bytes.Repeat([]byte("2"), 252))}
	got := EncodeTXT(txt)
	if len(got) != 256 || got[0] != 255 {
		t.Errorf("EncodeTXT(255-byte record) length = %d, first byte %d", len(got), got[0])
	}
}
