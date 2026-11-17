// Package airplayinfo renders the device information documents the AirPlay 2
// control protocol exchanges with a sender: the GET /info response body and the
// event channel's "updateInfo" message.
//
// Every key, value type and numeric encoding here comes from the reference
// receivers, not from a sender:
//
//   - Shairport Sync 5.6.3: generateInfoPlist (rtsp.c:1178-1250) builds the
//     document, generateTxtDataValueInfo (rtsp.c:1065-1175) builds the TXT
//     record blob and fixes its record order, handle_get_info (rtsp.c:1254-1370)
//     serves both request stages, and the template plist
//     (plists/get_info_response.xml:8-55) supplies the keys generateInfoPlist
//     does not set (playbackCapabilities, keepAliveSendStatsAsBody,
//     protocolVersion, receiverHDRCapability; its canRecordScreenStream and
//     screenDemoMode are deliberately not served — see DeviceInfo).
//   - The OpenAirplay receiver (ckdo ap2-receiver.py:79-113 and its fork
//     ap2py/ap2-receiver.py:152-196) is the reference for the keys Shairport
//     Sync does not emit at all: keepAliveLowPower, audioLatencies,
//     manufacturer, nameIsFactoryDefault, sdk.
//   - pyatv docs/documentation/protocols.md:1770-1786 is a captured updateInfo
//     document from an Apple TV; it fixes the message shape, confirms the
//     txtAirPlay blob framing byte for byte, and pins the featuresEx token.
//
// Wire facts are cited where they are used. Where the references disagree the
// divergence is called out and the value chosen for this receiver is explained.
package airplayinfo

import (
	"crypto/ed25519"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ProtocolVersion is the protocolVersion the reference reports in every
// document: the template literally carries "1.1" (plists/get_info_response.xml:20-21),
// as does generateTxtDataValueInfo's "protovers=1.1" record (rtsp.c:1145), the
// OpenAirplay receiver (ap2-receiver.py:99) and the captured Apple TV document
// (protocols.md:1783).
const ProtocolVersion = "1.1"

// DefaultSDK is the `sdk` value the OpenAirplay receiver reports
// (ap2-receiver.py:110, ap2py/ap2-receiver.py:185). Shairport Sync does not emit
// this key at all.
const DefaultSDK = "AirPlay;2.0.2"

// ReceiverHDRCapability is the template's fixed value
// (plists/get_info_response.xml:53-54); the captured Apple TV reports the same
// string (protocols.md:1783).
const ReceiverHDRCapability = "4k60"

// Named values the document always carries, with the reference they follow.
//
//   - KeepAliveLowPower and KeepAliveSendStatsAsBody are true in the
//     OpenAirplay reference (ap2-receiver.py:102-103), which is also where
//     keepAliveLowPower comes from. Shairport Sync's template ships
//     keepAliveSendStatsAsBody as false (plists/get_info_response.xml:18-19);
//     this receiver follows OpenAirplay, which is the only reference that
//     declares both keys.
//   - NameIsFactoryDefault is false because the advertised name is the
//     operator's setting, not the accessory's factory name
//     (ap2-receiver.py:107).
//   - The playbackCapabilities sub-dictionary is the template's: this receiver
//     streams no video or interstitial content, so all three flags are false
//     (plists/get_info_response.xml:7-15). The captured Apple TV sets all three
//     true (protocols.md:1783) because it can play them.
const (
	KeepAliveLowPower = true
	// KeepAliveSendStatsAsBody is false: the reference receiver this app
	// matches (Shairport Sync) reports false, and this receiver implements no
	// stats-body handling, so true would promise behaviour that does not exist.
	KeepAliveSendStatsAsBody = false
	NameIsFactoryDefault     = false
)

// DefaultVV is the `vv` ("vodka version") this receiver reports. The captured
// Apple TV reports 2 (protocols.md:1783) and the OpenAirplay receiver records 2
// (ap2py/ap2-receiver.py:196); stock Shairport Sync initialises vv to 0
// (shairport.c:640) and reports that, so the value is a Config field with this
// default offered for callers that want the Apple-device behaviour.
const DefaultVV = 2

// DefaultVolumeControlType is stock Shairport Sync's volumeControlType: 3
// "seems normal" for AirPlay 2 (shairport.c:636). The captured Apple TV reports
// 4 (protocols.md:1783).
const DefaultVolumeControlType = 3

// DefaultAudioStreamFormats is the realtime (type 96) format mask; the
// reference hard-codes 262144 = 0x40000, "just ALAC/S16/2"
// (rtsp.c:1219-1222).
const DefaultAudioStreamFormats = 262144

// DefaultBufferStreamFormats advertises the buffered (type 103) container as
// carrying ALAC/44100/S16/2. 0x40000 is the record Shairport Sync always sets
// on top of its configurable bits (rtsp.c:1226-1227); the other bits it ORs in
// are conditional on decoders and channel layouts this receiver does not
// promise:
//
//	0x00000400000 AAC-LC/44.1K/F24/2   rtsp.c:1225
//	0x00000800000 AAC-LC/48K/F24/2     rtsp.c:1237
//	0x00000200000 ALAC/48K/F24/2       rtsp.c:1235 (lossless mode only)
//	0x10000000000 AAC-LC/48K/F24/7.1   rtsp.c:1241
//	0x08000000000 AAC-LC/48K/F24/5.1   rtsp.c:1243
const DefaultBufferStreamFormats = 0x40000

// Config is the receiver's identity and capability set. Every field is a fact
// about this device, never a value read back from a sender.
type Config struct {
	// Name is the display name, the document's `name`.
	Name string
	// DeviceID is the AirPlay device id: twelve hexadecimal digits, optionally
	// colon separated. It is reported verbatim as `deviceID`, so a sender reads
	// the same spelling in `_airplay._tcp` deviceid and in /info; this
	// application stores and advertises the plain form (manager.go
	// randomDeviceID, advertise.go advertiseTXT), while the reference splits it
	// with colons (shairport.c:1936-1948, "aa:bb:cc:dd:ee:ff"). The form is the
	// caller's decision; the six bytes are what the identity derivations use.
	DeviceID string
	// PairingID is the accessory pairing identifier, the document's `pi`. The
	// repository's pairing store keeps it in Device.PairingID.
	PairingID string
	// SystemPairingID is the "type 4 fixed UUID" identity, the document's
	// `psi` (common.h:399, rtsp.c:1192). Empty means DefaultSystemPairingID of
	// DeviceID, the derivation Shairport Sync uses (shairport.c:1949-1977).
	SystemPairingID string
	// Model is the AirPlay model string, the document's `model`.
	Model string
	// Manufacturer is the document's `manufacturer`; the OpenAirplay
	// reference reports its own name here (ap2-receiver.py:104).
	Manufacturer string
	// SourceVersion is the AirPlay `srcvers`, the document's `sourceVersion`.
	// Shairport Sync's template claims this key is added programmatically
	// (plists/get_info_response.xml:49) but 5.6.3's generateInfoPlist does not
	// set it (rtsp.c:1190-1211); both other references do report it — the
	// captured Apple TV (protocols.md:1783) and the OpenAirplay receiver
	// (ap2-receiver.py:111, "366.0").
	SourceVersion string
	// SDK overrides the `sdk` string; empty selects DefaultSDK.
	SDK string
	// Features is the 64-bit feature mask, the document's `features`. It is
	// encoded as a plist integer (see DeviceInfo).
	Features uint64
	// FeaturesEx overrides the `featuresEx` token; empty selects
	// FeaturesEx(Features).
	FeaturesEx string
	// StatusFlags is the `statusFlags` mask.
	StatusFlags uint64
	// VolumeDB is the initial volume in dB, the document's `initialVolume`
	// (a plist real). Shairport Sync fills it from suggested_volume
	// (rtsp.c:1208).
	VolumeDB float64
	// VV is the document's `vv`; DefaultVV is the Apple-device value.
	VV uint64
	// VolumeControlType is the document's `volumeControlType`; 0 is meaningful
	// to the reference ("don't show the volume controls"), so this field has no
	// default (shairport.c:636-637).
	VolumeControlType uint64
	// AudioStreamFormats is supportedFormats.audioStream; 0 is meaningful to a
	// sender, so this field has no default. DefaultAudioStreamFormats is the
	// reference's value.
	AudioStreamFormats uint64
	// BufferStreamFormats is supportedFormats.bufferStream; see
	// DefaultBufferStreamFormats for the reference's bit definitions.
	BufferStreamFormats uint64
	// OutputLatencyMicros is the audioLatencies entry's outputLatencyMicros.
	// The OpenAirplay reference reports 400000 (ap2-receiver.py:80-94).
	OutputLatencyMicros uint64
	// PairingLTPK is the accessory's ed25519 long-term public key, the
	// document's `pk` data (32 bytes; rtsp.c:1202-1203). /info and the event
	// channel carry the key as data; the TXT records carry the same key as text,
	// which this application base64-encodes (advertise.go advertiseTXT) where
	// the reference writes lowercase hex (shairport.c:1984-1991) — the caller
	// supplies the `pk` record, this package only frames it. The RAOP RSA key is
	// a different key and does not belong here.
	PairingLTPK []byte
	// SenderAddress is the sender's "ip:port" for this connection. The document
	// carries `senderAddress` only when it is non-empty: the reference formats
	// it from the live connection (rtsp.c:1204-1207) and the event-channel
	// document carries it too (protocols.md:1783).
	SenderAddress string
}

// TXT is the AirPlay TXT record set: the same key/value pairs the receiver
// publishes in `_airplay._tcp`, in the form the reference puts in the
// txtAirPlay record (rtsp.c:1065-1175). The caller owns the map; nothing here
// mutates it.
type TXT map[string]string

// defaultPlaybackCapabilities is the template's sub-dictionary
// (plists/get_info_response.xml:7-15). It is rebuilt per document so callers
// cannot share mutable state.
func playbackCapabilities() map[string]any {
	return map[string]any{
		"supportsInterstitials":         false,
		"supportsFPSSecureStop":         false,
		"supportsUIForAudioOnlyContent": false,
	}
}

// DeviceInfo renders the GET /info response body: a dictionary the caller
// encodes as a binary plist and serves as application/x-apple-binary-plist.
//
// qualifier tells the caller's stage: the stage 1 request carries a plist body
// whose `qualifier` array names the records the sender wants, the stage 2
// request carries no body. The reference replies with the same document in both
// stages — generateTxtDataValueInfo is merged into the response as txtAirPlay at
// rtsp.c:1339-1340 (stage 1) and again at rtsp.c:1366-1367 (stage 2), and the
// two branches differ only in how they validate the request — so both stages
// produce the same key set here. The flag is taken so the stage is explicit at
// the call site and so a reference that withholds a qualifier-only key later
// can be honoured in this one function.
//
// Four keys the references emit elsewhere are deliberately absent, because this
// receiver mirrors no screen and serves no video: canRecordScreenStream
// (plists/get_info_response.xml:16-17) and screenDemoMode (:26-27), both also in
// the captured Apple TV document (protocols.md:1783); macAddress (also in that
// document, and commented out of the template at :51); and
// hasUDPMirroringSupport (that document only). Everything this package does emit
// is a key a reference emits.
//
// Numbers are int64 (and the volume a float64) so the plist encoder writes
// plist integers and reals: plist.EncodeBinary writes a uint64 as a plist UID
// (internal/plist/plist.go, addValue), which senders do not accept for
// `features`, `statusFlags`, `vv`, `volumeControlType` or the supportedFormats
// values. The reference writes all of them with plist_new_uint
// (rtsp.c:1196-1211, :1220-1248).
//
// The returned document aliases cfg.PairingLTPK.
func DeviceInfo(cfg Config, txt TXT, qualifier bool) (map[string]any, error) {
	return infoDocument(cfg, txt)
}

// UpdateInfo renders the event-channel "updateInfo" message body: the
// `{"type": "updateInfo", "value": <info document>}` dictionary the receiver
// POSTs to /command as soon as the sender has connected to the event port
// (ap2_event_receiver.c:126-153; the wire example is pyatv
// docs/documentation/protocols.md:1770-1786, and the same message is what
// atvproxy.py:647-660 rewrites). The caller sends it as
// application/x-apple-binary-plist on the encrypted event channel.
//
// The value is the same document GET /info serves, txtAirPlay included: the
// reference builds it from generateInfoPlist plus the TXT records
// (ap2_event_receiver.c:128-135).
func UpdateInfo(cfg Config, txt TXT) (map[string]any, error) {
	value, err := infoDocument(cfg, txt)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type":  "updateInfo",
		"value": value,
	}, nil
}

// infoDocument is the shared builder: the reference's generateInfoPlist
// (rtsp.c:1178-1250) with the template keys the reference merges in
// (plists/get_info_response.xml) and the TXT records as txtAirPlay.
func infoDocument(cfg Config, txt TXT) (map[string]any, error) {
	id, err := deviceIDBytes(cfg.DeviceID)
	if err != nil {
		return nil, err
	}
	if len(cfg.PairingLTPK) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("airplayinfo: PairingLTPK is %d bytes, want %d (the ed25519 long-term public key the reference reports as pk)",
			len(cfg.PairingLTPK), ed25519.PublicKeySize)
	}
	psi := cfg.SystemPairingID
	if psi == "" {
		psi = systemPairingID(id)
	}
	sdk := cfg.SDK
	if sdk == "" {
		sdk = DefaultSDK
	}
	fex := cfg.FeaturesEx
	if fex == "" {
		fex = FeaturesEx(cfg.Features)
	}
	doc := map[string]any{
		"deviceID":                 cfg.DeviceID,
		"features":                 int64(cfg.Features),
		"model":                    cfg.Model,
		"name":                     cfg.Name,
		"pi":                       cfg.PairingID,
		"psi":                      psi,
		"pk":                       cfg.PairingLTPK,
		"protocolVersion":          ProtocolVersion,
		"sourceVersion":            cfg.SourceVersion,
		"statusFlags":              int64(cfg.StatusFlags),
		"keepAliveLowPower":        KeepAliveLowPower,
		"keepAliveSendStatsAsBody": KeepAliveSendStatsAsBody,
		"audioLatencies": []any{
			map[string]any{
				"inputLatencyMicros":  int64(0),
				"outputLatencyMicros": int64(cfg.OutputLatencyMicros),
			},
		},
		"supportedFormats": map[string]any{
			"audioStream":  int64(cfg.AudioStreamFormats),
			"bufferStream": int64(cfg.BufferStreamFormats),
		},
		"txtAirPlay":            EncodeTXT(txt),
		"featuresEx":            fex,
		"vv":                    int64(cfg.VV),
		"volumeControlType":     int64(cfg.VolumeControlType),
		"playbackCapabilities":  playbackCapabilities(),
		"receiverHDRCapability": ReceiverHDRCapability,
		"initialVolume":         cfg.VolumeDB,
		"nameIsFactoryDefault":  NameIsFactoryDefault,
		"sdk":                   sdk,
		"manufacturer":          cfg.Manufacturer,
	}
	if cfg.SenderAddress != "" {
		doc["senderAddress"] = cfg.SenderAddress
	}
	return doc, nil
}

// FeaturesEx returns the `featuresEx` token for a feature mask: the standard
// base64 of the mask's eight bytes little endian, with the '=' padding removed.
// Shairport Sync derives it exactly that way from the same mask it advertises
// (shairport.c:2044-2062: fexbytes[i] = temp & 0xff, temp >>= 8, base64_enc,
// then the padding is truncated at the first '=').
//
// The captured Apple TV confirms the encoding: features 0x3C155FDE4A7FDFD5
// (protocols.md:1783 "features=0x4A7FDFD5,0x3C155FDE") yields "1d9/St5fFTw",
// which is exactly this function's output for that mask, including the standard
// (not URL-safe) alphabet — the '/' in the token.
func FeaturesEx(features uint64) string {
	var raw [8]byte
	for i := range raw {
		raw[i] = byte(features >> (8 * uint(i)))
	}
	return strings.TrimRight(base64.StdEncoding.EncodeToString(raw[:]), "=")
}

// DefaultSystemPairingID returns the `psi` value for a device id: an MD5 of the
// device id's six bytes, shaped into a "type 4 fixed UUID", which is what
// Shairport Sync derives from the same six bytes (shairport.c:1949-1977 — the
// md5 is taken over the raw id, then the version nibble is forced to 4 and the
// variant bits to 10).
func DefaultSystemPairingID(deviceID string) (string, error) {
	id, err := deviceIDBytes(deviceID)
	if err != nil {
		return "", err
	}
	return systemPairingID(id), nil
}

func systemPairingID(id [6]byte) string {
	sum := md5.Sum(id[:])
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// deviceIDBytes parses the twelve hexadecimal digits of a device id, accepting
// either the plain form this application stores and advertises
// (manager.go randomDeviceID) or the colon separated form the reference uses
// internally (shairport.c:1936-1948).
func deviceIDBytes(deviceID string) ([6]byte, error) {
	var id [6]byte
	plain := strings.ReplaceAll(deviceID, ":", "")
	if len(plain) != 12 {
		return id, fmt.Errorf("airplayinfo: DeviceID %q is not twelve hexadecimal digits", deviceID)
	}
	raw, err := hex.DecodeString(plain)
	if err != nil {
		return id, fmt.Errorf("airplayinfo: DeviceID %q is not hexadecimal: %w", deviceID, err)
	}
	copy(id[:], raw)
	return id, nil
}

// txtOrder is the order the reference writes the TXT records in:
// generateTxtDataValueInfo appends exactly these keys, in this order
// (rtsp.c:1069-1173).
var txtOrder = [...]string{
	"acl", "btaddr", "deviceid", "fex", "features", "flags", "gid", "igl", "gcgl",
	"pgid", "pgcgl", "model", "protovers", "pi", "psi", "pk", "srcvers", "vv",
}

// EncodeTXT builds the value the reference puts in the txtAirPlay record: the
// canonical DNS-SD TXT blob, one record per entry as a single length byte
// followed by the "key=value" ASCII bytes (rtsp.c add_pstring_to_malloc
// :1045-1065 — note there is no terminating NUL and no base64: the blob is
// stored as plist *data* at rtsp.c:1339-1340 and appears as raw bytes in the
// captured Apple TV document, protocols.md:1783
// txtAirPlay b"\x05acl=0\x18btaddr=FF:EE:DD:CC:BB:AA...").
//
// Records follow txtOrder, which is the reference's own emission order; keys
// outside that list are appended in ascending order so a caller's record set —
// for example the receiver's `_airplay._tcp` set, which adds name,
// serialNumber and sf — still encodes to one deterministic blob.
//
// A record cannot exceed the TXT format's 255-byte limit, so entries whose
// "key=value" does not fit, and keys that are empty, are skipped rather than
// truncated.
func EncodeTXT(txt TXT) []byte {
	if len(txt) == 0 {
		return nil
	}
	blob := make([]byte, 0, len(txt)*16)
	write := func(key string) {
		if key == "" {
			return
		}
		value, ok := txt[key]
		if !ok {
			return
		}
		record := key + "=" + value
		if len(record) > 255 {
			return
		}
		blob = append(blob, byte(len(record)))
		blob = append(blob, record...)
	}
	for _, key := range txtOrder {
		write(key)
	}
	rest := make([]string, 0, len(txt))
	for key := range txt {
		if !isOrderedTXTKey(key) {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		write(key)
	}
	return blob
}

func isOrderedTXTKey(key string) bool {
	for _, ordered := range txtOrder {
		if key == ordered {
			return true
		}
	}
	return false
}
