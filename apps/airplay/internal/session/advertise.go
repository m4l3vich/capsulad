package session

import (
	"capsulad/apps/airplay/internal/aac"

	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
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
// from the HomeKit pairing identity, `pi` the pairing identifier.
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
		"name":         m.cfg.Name,
		"manufacturer": "capsulad",
		"sf":           hexStatus(status),
	}
	if groupID != "" {
		txt["gid"] = groupID
	}
	return txt
}

// advertiseRAOPTXT builds the legacy `_raop._tcp` record set. The `pk` here is
// the RSA public key the legacy key exchange uses, which is a different key and
// format from the HomeKit `pk` above.
func (m *Manager) advertiseRAOPTXT() map[string]string {
	txt := map[string]string{
		"ch": "2",       // 2 channels
		"cn": cnList(),  // PCM, ALAC, and AAC-LC when a decoder is compiled in
		"da": "true",    // digest authentication supported for the RTSP session
		"et": "0,1",     // no encryption, RSA
		"md": "0,1,2",   // text, artwork, progress metadata
		"pw": "false",   // no password
		"sv": "false",   // software volume is not used
		"sr": "44100",   // default sample rate
		"ss": "16",      // default sample size
		"tp": "UDP,TCP", // transports offered
		"vn": "65537",   // protocol version 1.1
		"vs": m.cfg.SourceVersion,
		"pk": m.raop.PublicTXT,
	}
	return txt
}

// publish (re)publishes both advertisements. The AirPlay record is republished
// whenever the session-active status flag changes, because senders read `sf`
// from the advertisement rather than polling /info.
func (m *Manager) publish(port int, active bool) error {
	if m.advertiser == nil {
		return nil
	}
	m.mu.Lock()
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
	name := m.cfg.Name
	if name == "" {
		name = "AirPlay Receiver"
	}
	airplayID, err := m.advertiser.Publish(Advert{
		Type:     "_airplay._tcp",
		Instance: name,
		Port:     port,
		TXT:      m.advertiseTXT(active),
	})
	if err != nil {
		return fmt.Errorf("publish _airplay._tcp: %w", err)
	}
	raopID, err := m.advertiser.Publish(Advert{
		Type:     "_raop._tcp",
		Instance: strings.ToUpper(m.cfg.DeviceID) + "@" + name,
		Port:     port,
		TXT:      m.advertiseRAOPTXT(),
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

func (m *Manager) republishStatus(active bool) {
	m.mu.Lock()
	listener := m.listener
	closed := m.closed
	m.mu.Unlock()
	if closed || listener == nil {
		return
	}
	_, port := listener.Addr()
	if err := m.publish(port, active); err != nil {
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

func formatUint64(v uint64) string { return strconv.FormatUint(v, 10) }

func hexKey(key []byte) string { return hex.EncodeToString(key) }
