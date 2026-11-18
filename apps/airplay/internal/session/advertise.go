package session

import (
	"capsulad/apps/airplay/internal/aac"

	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultSourceVersion is the AirPlay srcvers this receiver advertises.
//
// The value matters: senders switch behaviour on it (remote control above 360,
// realtime streams at or below 355, "shk" stream keys above 350). Modules that
// peer with a real sender should treat the exact value as an interop gate; it
// is configurable through the app's `source_version` setting.
const DefaultSourceVersion = "366.0"

// DefaultModel is the AirPlay model string this receiver advertises. Upstream
// patches Shairport Sync to AudioAccessory5,1 so senders draw the compact
// speaker icon instead of a generic one; it is a compatibility string, not a
// claim that the hardware is an Apple accessory.
const DefaultModel = "AudioAccessory5,1"

// Feature bits advertised in the `features` TXT record and in /info. The names
// and numbering follow the AirPlay 2 feature table as reverse engineered by the
// community (see docs/app_development and the app README for the citation):
// only the bits this receiver actually implements are set.
const (
	FeatureAirPlayAudio       = 1 << 9  // AirPlay audio
	FeatureAudioMetaProgress  = 1 << 16 // progress metadata
	FeatureAudioMetaTxtDAAP   = 1 << 17 // DMAP/DACP text metadata
	FeatureReceiveAudioPCM    = 1 << 18 // PCM audio
	FeatureReceiveAudioALAC   = 1 << 19 // ALAC audio
	FeatureReceiveAudioAACLC  = 1 << 20 // AAC-LC audio (FFmpeg decoder builds only)
	FeatureAudioUnencrypted   = 1 << 22 // audio may arrive unencrypted
	FeaturePlistMetadata      = 1 << 29 // plist metadata
	FeatureUnifiedAdvertising = 1 << 30 // unified advertising info
	FeatureControlEncrypt     = 1 << 38 // encrypted control channel
	FeatureHomeKitPairing     = 1 << 46 // HomeKit pairing
	FeaturePeerManagement     = 1 << 47 // peer management (PTP timing)
	FeatureTransientPairing   = 1 << 48 // transient (no-code) pairing
	FeatureNowPlayingInfo     = 1 << 50 // now-playing information
	FeatureAudioStreamConn    = 1 << 59 // stream connections
	FeatureRFC2198Redundant   = 1 << 61 // RFC 2198 RTP redundancy
	// Deliberately absent: buffered audio (bit 40) and the PTP clock (bit 41).
	// This build decodes the realtime stream (and ALAC in a buffered stream
	// container); it has no AAC/AAC-ELD decoder, so advertising buffered audio
	// would promise a codec it cannot decode, and PTP is advertised by
	// `ptp_enabled` only when the daemon grants a multicast listener.
)

// DefaultFeatures is the advertised feature set for a stock receiver without
// PTP: realtime audio, ALAC and PCM, encrypted control channel, plist and DMAP
// metadata, transient HomeKit pairing, remote control.
const DefaultFeatures = FeatureAirPlayAudio |
	FeatureAudioMetaProgress |
	FeatureAudioMetaTxtDAAP |
	FeatureReceiveAudioPCM |
	FeatureReceiveAudioALAC |
	FeatureAudioUnencrypted |
	FeaturePlistMetadata |
	FeatureUnifiedAdvertising |
	FeatureControlEncrypt |
	FeatureHomeKitPairing |
	FeatureTransientPairing |
	FeatureNowPlayingInfo

// Status flags published in the `sf` TXT record and returned by /info.
const (
	StatusProblemsExist        = 1 << 0
	StatusSupportsAirPlayCloud = 1 << 6
	StatusReceiverSessionIsOn  = 1 << 17
)

// cnList is the legacy RAOP codec list: 0 PCM, 1 ALAC, 2 AAC-LC.
func cnList() string {
	if aac.Available() {
		return "0,1,2"
	}
	return "0,1"
}

func hexFeatures(v uint64) string {
	if v <= 0xffffffff {
		return fmt.Sprintf("0x%x", v)
	}
	return fmt.Sprintf("0x%x,0x%x", v&0xffffffff, v>>32)
}

func hexStatus(v uint64) string { return fmt.Sprintf("0x%x", v) }

// advertiseTXT builds the `_airplay._tcp` record set. Only facts this receiver
// can honour are published; the device key is the ed25519 long-term public key
// from the HomeKit pairing identity, `pi` the pairing identifier. The `name`
// value is the DNS-SD-projected instance label, so the TXT record and the
// instance agree even when a long saved name was shortened for the label.
func (m *Manager) advertiseTXT(active bool) map[string]string {
	// One feature source for mDNS and /info: the PTP bits appear only when a
	// clock exists, because the playout path consumes it.
	features := m.advertisedFeatures()
	status := uint64(0)
	if active {
		status |= StatusReceiverSessionIsOn
	}
	m.mu.Lock()
	groupID := m.groupID
	m.mu.Unlock()
	txt := map[string]string{
		"deviceid": m.cfg.DeviceID,
		"features": hexFeatures(features),
		"flags":    hexStatus(status),
		// The reference publishes the AP2 pairing key as lowercase hex in the
		// mDNS TXT record; /info carries the same key as binary data.
		"pk":           hex.EncodeToString(m.store.Device.LTPK[:]),
		"pi":           m.store.Device.PairingID,
		"psi":          m.systemPairingID(),
		"srcvers":      m.cfg.SourceVersion,
		"model":        m.cfg.Model,
		"protovers":    "1.1",
		"acl":          "0",
		"serialNumber": strings.ToUpper(m.cfg.DeviceID),
		"gcgl":         "0",
		"name":         m.instanceLabel(),
		"manufacturer": "capsulad",
		"sf":           hexStatus(status),
	}
	if groupID != "" {
		txt["gid"] = groupID
	}
	return txt
}

// advertiseRAOPTXT builds the `_raop._tcp` record set. AirPlay 2 receivers
// publish the same 32-byte ed25519 long-term public key here as in
// `_airplay._tcp` (64 hex characters), which is what the reference receiver
// and every AirPlay 2 device on the link advertise. The legacy 2048-bit RSA
// public key is not used: its base64 form is roughly 360 bytes, Avahi rejects
// any single TXT item over 255 bytes with InvalidRecordError, and splitting a
// key across TXT strings is not interoperable. A legacy sender therefore uses
// the unencrypted (`et=0`) path; the RSA decryption code remains for an
// ANNOUNCE that carries rsaaeskey.
func (m *Manager) advertiseRAOPTXT(active bool) map[string]string {
	status := uint64(0)
	if active {
		status |= StatusReceiverSessionIsOn
	}
	return map[string]string{
		"am": m.cfg.Model,
		"cn": cnList(), // PCM, ALAC, and AAC-LC when a decoder is compiled in
		"da": "true",   // digest authentication supported for the RTSP session
		"et": "0,1",    // no encryption, RSA
		"ft": hexFeatures(m.advertisedFeatures()),
		"md": "0,1,2", // text, artwork, progress metadata
		"pk": hex.EncodeToString(m.store.Device.LTPK[:]),
		"sf": hexStatus(status),
		"tp": "UDP,TCP", // transports offered
		"vn": "65537",   // protocol version 1.1
		"vs": m.cfg.SourceVersion,
	}
}

// publish (re)publishes both advertisements. The AirPlay record is republished
// whenever the session-active status flag changes, because senders read `sf`
// from the advertisement rather than polling /info.
//
// Session lifecycle, the group-id update and the Re-advertise action can call
// this concurrently. advMu makes one publish withdraw and replace both records
// atomically, so two callers cannot both see the previous ids as empty and
// race AddService for the same instance (which Avahi reports as a collision).
//
// The session-active flag is read from the lifecycle state here, under the
// same m.mu critical section that takes the previous ids, rather than passed
// in by an asynchronous caller. Lock acquisition order therefore decides only
// which caller publishes next; it cannot make a delayed session-end caller
// publish an inactive record after a newer session has already started.
func (m *Manager) publish(port int) error {
	m.advMu.Lock()
	defer m.advMu.Unlock()
	return m.publishLocked(port)
}

// publishLocked is publish while the caller holds advMu.
func (m *Manager) publishLocked(port int) error {
	if m.advertiser == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		// Close owns the final state; a publish that queued behind it must not
		// resurrect a record after the withdrawal.
		m.mu.Unlock()
		return nil
	}
	active := m.sessionActive
	previousAirplay, previousRAOP := m.airplay, m.raopID
	m.airplay, m.raopID = "", ""
	m.mu.Unlock()
	if previousAirplay != "" {
		if err := m.advertiser.Unpublish(previousAirplay); err != nil {
			m.logf("withdrawing the previous _airplay._tcp record: %v", err)
		}
	}
	if previousRAOP != "" {
		if err := m.advertiser.Unpublish(previousRAOP); err != nil {
			m.logf("withdrawing the previous _raop._tcp record: %v", err)
		}
	}
	// DNS-SD instances are one label, at most 63 bytes. The saved name is
	// unchanged and /info still reports it in full; only the mDNS labels are
	// projected, because Avahi refuses a longer instance instead of advertising
	// it. The RAOP instance adds "<DEVICEID>@" in front, so its display part
	// gets the smaller budget.
	airplayID, err := m.advertiser.Publish(Advert{
		Type:     "_airplay._tcp",
		Instance: m.instanceLabel(),
		Port:     port,
		TXT:      m.advertiseTXT(active),
	})
	if err != nil {
		return fmt.Errorf("publish _airplay._tcp: %w", err)
	}
	raopID, err := m.advertiser.Publish(Advert{
		Type:     "_raop._tcp",
		Instance: raopInstanceName(m.cfg.DeviceID, m.displayName()),
		Port:     port,
		TXT:      m.advertiseRAOPTXT(active),
	})
	m.mu.Lock()
	m.airplay = airplayID
	if err == nil {
		m.raopID = raopID
	}
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("publish _raop._tcp: %w", err)
	}
	return nil
}

// republishStatus refreshes both records after a lifecycle or group-id change.
// The active flag is not captured here: publish reads the current session state
// under the lifecycle lock, so an older asynchronous caller cannot publish a
// stale inactive record after a newer session started.
func (m *Manager) republishStatus() {
	m.mu.Lock()
	listener := m.listener
	closed := m.closed
	m.mu.Unlock()
	if closed || listener == nil {
		return
	}
	_, port := listener.Addr()
	if err := m.publish(port); err != nil {
		m.logf("republishing mDNS: %v", err)
	}
}

// advertisedPort is the RTSP port senders are told to use; it is what the
// daemon bound, never a value this app chose.
func (m *Manager) advertisedPort() int {
	m.mu.Lock()
	listener := m.listener
	m.mu.Unlock()
	if listener == nil {
		return 0
	}
	_, port := listener.Addr()
	return port
}

// maxServiceInstanceBytes is the DNS-SD instance label limit. Avahi rejects a
// longer name with InvalidServiceNameError, so an advertisement cannot carry
// an arbitrary saved display name.
const maxServiceInstanceBytes = 63

// displayName is the configured name with the published default applied.
func (m *Manager) displayName() string {
	if m.cfg.Name != "" {
		return m.cfg.Name
	}
	return "AirPlay Receiver"
}

// instanceLabel is the display label the AirPlay record uses. It is a rune-safe
// projection of the saved name; the setting and /info keep the full value.
func (m *Manager) instanceLabel() string {
	return truncateUTF8(m.displayName(), maxServiceInstanceBytes)
}

// truncateUTF8 returns the longest prefix of s that is at most max bytes and
// ends on a rune boundary. It never splits a multi-byte character.
func truncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// raopInstanceName builds the legacy RAOP "<DEVICEID>@<name>" instance within
// the DNS-SD label limit. The device-id prefix is preserved so a RAOP sender
// still sees the same identity; only the display part is shortened, on a rune
// boundary.
func raopInstanceName(deviceID, name string) string {
	prefix := strings.ToUpper(deviceID) + "@"
	budget := maxServiceInstanceBytes - len(prefix)
	if budget < 0 {
		return truncateUTF8(prefix, maxServiceInstanceBytes)
	}
	return prefix + truncateUTF8(name, budget)
}

func formatUint64(v uint64) string { return strconv.FormatUint(v, 10) }

func hexKey(key []byte) string { return hex.EncodeToString(key) }
