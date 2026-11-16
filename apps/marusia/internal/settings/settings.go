// Package settings parses this app's manifest settings into the one immutable
// configuration a turn uses. Every value has a documented default; invalid
// input is rejected with a reason instead of being silently replaced.
package settings

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"capsula/apps/marusia/internal/vkclient"
	"capsula/sdk/capsula"
)

// Keys are the manifest setting keys, in one place.
const (
	KeyEndpoint       = "endpoint"
	KeyEndpointCustom = "endpoint_custom"
	KeyXToken         = "xtoken_id"
	KeySerial         = "serial"
	KeyDeviceID       = "device_id"
	KeySessionID      = "session_id"
	KeySessionSecret  = "session_secret"
	KeySessionFile    = "session_file"
	KeyAudioFormat    = "audio_format"
	KeyClientVersion  = "client_version"
	KeyKeyword        = "keyword"
	KeyHWID           = "hw_id"
)

// Defaults mirror manifest.toml.
const (
	DefaultEndpoint      = "prod"
	DefaultAudioFormat   = "wave"
	DefaultClientVersion = "44.91"
	DefaultKeyword       = "marusya"
	DefaultHWID          = "9F25725B"
	// DefaultSessionFile is where the shipped vendor agent left its own
	// session and device identity on the device. The path is a manifest
	// setting so an operator can point at a different file or disable it.
	DefaultSessionFile = "/data/voice-agent/session.json"
)

// Serial sources name where Config.Serial came from. They label one log line
// per process; the serial value itself is never logged.
const (
	SerialSourceSettings  = "settings"
	SerialSourceEnv       = "env"
	SerialSourceFramework = "framework"
	SerialSourceCPUInfo   = "cpuinfo"
)

// Values is the raw text of every setting.
type Values struct {
	Endpoint       string
	EndpointCustom string
	XToken         string
	Serial         string
	DeviceID       string
	SessionID      string
	SessionSecret  string
	SessionFile    string
	AudioFormat    string
	ClientVersion  string
	Keyword        string
	HWID           string
}

// Default returns the manifest defaults for a fresh app instance.
func Default() Values {
	return Values{
		Endpoint:      DefaultEndpoint,
		AudioFormat:   DefaultAudioFormat,
		ClientVersion: DefaultClientVersion,
		Keyword:       DefaultKeyword,
		HWID:          DefaultHWID,
		SessionFile:   DefaultSessionFile,
	}
}

// Read decodes the framework's app-private settings snapshot. Unknown keys are
// ignored; a malformed value is an error so the app can keep its last good
// configuration instead of starting a turn with a wrong endpoint.
func Read(changed *capsula.SettingsChanged) (Values, error) {
	values := Default()
	if changed == nil {
		return values, nil
	}
	for _, setting := range changed.Values {
		var text string
		if err := json.Unmarshal([]byte(setting.ValueJSON), &text); err != nil {
			// The decoder's error can quote the input, and the input may be a
			// credential, so only the key is reported.
			return values, fmt.Errorf("setting %q is not a JSON string", setting.Key)
		}
		switch setting.Key {
		case KeyEndpoint:
			values.Endpoint = text
		case KeyEndpointCustom:
			values.EndpointCustom = text
		case KeyXToken:
			values.XToken = text
		case KeySerial:
			values.Serial = text
		case KeyDeviceID:
			values.DeviceID = text
		case KeySessionID:
			values.SessionID = text
		case KeySessionSecret:
			values.SessionSecret = text
		case KeySessionFile:
			values.SessionFile = text
		case KeyAudioFormat:
			values.AudioFormat = text
		case KeyClientVersion:
			values.ClientVersion = text
		case KeyKeyword:
			values.Keyword = text
		case KeyHWID:
			values.HWID = text
		}
	}
	return values, nil
}

// Fallback is the identity used when the serial or device_id setting is empty.
// The serial order is launch environment, framework hardware_serial, then a
// local /proc/cpuinfo read. The stable device_id comes from DeviceService.GetInfo.
type Fallback struct {
	// Serial is the SoC serial from the launch environment, the framework's
	// hardware_serial, or local /proc/cpuinfo, in that order; empty when no
	// source has it. It is never used as the device id.
	Serial string
	// SerialSource labels where Serial came from (env, framework or cpuinfo); empty
	// means no serial was resolved.
	SerialSource string
	// DeviceID is the stable device_id reported by DeviceService.GetInfo;
	// empty when that call failed or reported nothing.
	DeviceID string
	// Generated is a process-local id owned by the caller and reused across
	// settings updates, so an empty identity cannot rotate mid-process.
	Generated string
}

// VendorSession is the runtime credential recovered from the vendor agent's
// private state file. Secret is never included in its string representation or
// in errors returned by LoadVendorSession.
type VendorSession struct {
	SessionID string
	Secret    string
	AccountID string
	DeviceID  string
}

// LoadVendorSession reads the agent session and matching device_settings.json.
// A missing session file means no session is available; malformed or unreadable
// files return a credential-free error.
func LoadVendorSession(path string) (VendorSession, error) {
	if strings.TrimSpace(path) == "" {
		return VendorSession{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return VendorSession{}, nil
	}
	if err != nil {
		return VendorSession{}, fmt.Errorf("cannot read vendor session file")
	}
	var session struct {
		SessionID string `json:"session_id"`
		Session   string `json:"session"`
		Secret    string `json:"secret"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(data, &session); err != nil {
		return VendorSession{}, fmt.Errorf("vendor session file is invalid JSON")
	}
	if session.SessionID == "" {
		session.SessionID = session.Session
	}
	if (session.SessionID == "") != (session.Secret == "") {
		return VendorSession{}, fmt.Errorf("vendor session file is missing session or secret")
	}
	deviceID := ""
	devicePath := filepath.Join(filepath.Dir(path), "device_settings.json")
	if deviceData, err := os.ReadFile(devicePath); err == nil {
		var device struct {
			General struct {
				ID string `json:"id"`
			} `json:"general"`
		}
		if json.Unmarshal(deviceData, &device) != nil {
			return VendorSession{}, fmt.Errorf("vendor device settings file is invalid JSON")
		}
		deviceID = strings.TrimSpace(device.General.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return VendorSession{}, fmt.Errorf("cannot read vendor device settings file")
	}
	return VendorSession{SessionID: strings.TrimSpace(session.SessionID), Secret: strings.TrimSpace(session.Secret),
		AccountID: strings.TrimSpace(session.AccountID), DeviceID: deviceID}, nil
}

// Config is the validated configuration for one turn.
type Config struct {
	BaseURL  string
	XTokenID string
	Serial   string
	// SerialSource labels where Serial came from (SerialSourceSettings,
	// SerialSourceEnv, SerialSourceFramework or SerialSourceCPUInfo) and is ""
	// when no serial was resolved.
	SerialSource  string
	DeviceID      string
	SessionID     string
	SessionSecret string
	SessionFile   string
	Format        vkclient.AudioFormat
	ClientVersion string
	Keyword       string
	HWID          string
}

// HasSession reports whether both explicit session credentials are configured.
func (c Config) HasSession() bool {
	return c.SessionID != "" && c.SessionSecret != ""
}

// Provisioned reports whether attach can be attempted.
func (c Config) Provisioned() bool { return strings.TrimSpace(c.XTokenID) != "" }

// Build validates the values and resolves the identity. The explicit serial
// setting wins; an empty one falls back to the SoC serial, while an empty
// device_id setting falls back to the framework device identity and then to a
// process-local id. Warnings describe a usable but degraded configuration and
// never contain a setting value. An absent SoC serial remains empty rather
// than borrowing the framework device_id, because the vendor validates
// x-device-sn as its own hardware identity.
func Build(values Values, fallback Fallback) (Config, []string, error) {
	var warnings []string
	if (strings.TrimSpace(values.SessionID) == "") != (strings.TrimSpace(values.SessionSecret) == "") {
		return Config{}, warnings, fmt.Errorf("settings %s and %s must be supplied together", KeySessionID, KeySessionSecret)
	}

	base := strings.TrimSpace(values.EndpointCustom)
	if base == "" {
		endpoint, ok := vkclient.Endpoints[strings.TrimSpace(values.Endpoint)]
		if !ok {
			return Config{}, warnings, fmt.Errorf("setting %s=%q is not a known endpoint", KeyEndpoint, values.Endpoint)
		}
		base = endpoint
	}
	if _, err := vkclient.ParseBaseURL(base); err != nil {
		return Config{}, warnings, err
	}

	format, err := vkclient.ParseAudioFormat(strings.TrimSpace(values.AudioFormat))
	if err != nil {
		return Config{}, warnings, err
	}
	if format != vkclient.FormatWave {
		warnings = append(warnings, "audio_format=ogg_opus is not implemented in this build; "+
			"turns will be refused until it is set back to wave")
	}

	// The device serial is the one identity the service validates by name:
	// x-device-sn must carry the SoC serial, not an invented placeholder.
	// The explicit setting wins; otherwise the caller's resolved serial is
	// used, which is CAPSULA_DEVICE_SERIAL, then framework hardware_serial,
	// then a local /proc/cpuinfo read. There is deliberately no further
	// fallback: a fabricated value is rejected by the service anyway.
	serial := strings.TrimSpace(values.Serial)
	serialSource := ""
	if serial != "" {
		serialSource = SerialSourceSettings
	} else if serial = strings.TrimSpace(fallback.Serial); serial != "" {
		serialSource = fallback.SerialSource
		if serialSource == "" {
			serialSource = SerialSourceCPUInfo
		}
	} else {
		warnings = append(warnings, "the device serial is missing: set the "+KeySerial+" setting or have the "+
			"launcher export CAPSULA_DEVICE_SERIAL (/proc/cpuinfo is unreadable in the stock sandbox); "+
			"without it x-device-sn is empty and the service rejects the turn")
	}

	deviceID := strings.TrimSpace(values.DeviceID)
	if deviceID == "" {
		deviceID = strings.TrimSpace(fallback.DeviceID)
	}
	if deviceID == "" {
		// The sandbox has no writable storage and the SDK does not write app
		// settings, so the generated id cannot outlive this process.
		deviceID = strings.TrimSpace(fallback.Generated)
		if deviceID == "" {
			deviceID = vkclient.NewUUID()
		}
		warnings = append(warnings, "device_id is empty and the framework reported no device id; the "+
			"per-process id changes on restart (the sandbox has no writable storage). "+
			"Set the device_id setting to keep the identity stable.")
	}

	clientVersion := strings.TrimSpace(values.ClientVersion)
	if clientVersion == "" {
		clientVersion = DefaultClientVersion
	}
	keyword := strings.TrimSpace(values.Keyword)
	if keyword == "" {
		keyword = DefaultKeyword
	}
	hwid := strings.TrimSpace(values.HWID)
	if hwid == "" {
		hwid = DefaultHWID
	}

	return Config{
		BaseURL:       base,
		XTokenID:      strings.TrimSpace(values.XToken),
		Serial:        serial,
		SerialSource:  serialSource,
		DeviceID:      deviceID,
		SessionID:     strings.TrimSpace(values.SessionID),
		SessionSecret: strings.TrimSpace(values.SessionSecret),
		SessionFile:   strings.TrimSpace(values.SessionFile),
		Format:        format,
		ClientVersion: clientVersion,
		Keyword:       keyword,
		HWID:          hwid,
	}, warnings, nil
}

// cpuInfoPath is a variable so a test can point the reader at a fixture.
var cpuInfoPath = "/proc/cpuinfo"

// ReadCPUSerial extracts the SoC serial the vendor reads from /proc/cpuinfo
// with the scan format "Serial : %s". ARM kernels print "Serial\t\t: <hex>";
// x86 hosts generally have no such line, and a sandbox without /proc at all
// reports ""; both mean "no serial here", and Build then needs the serial
// setting instead.
func ReadCPUSerial() string {
	serial, _ := readCPUSerial(cpuInfoPath)
	return serial
}

func readCPUSerial(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "Serial" && key != "Serial Number" {
			continue
		}
		if serial := strings.TrimSpace(value); serial != "" {
			return serial, nil
		}
	}
	return "", scanner.Err()
}
