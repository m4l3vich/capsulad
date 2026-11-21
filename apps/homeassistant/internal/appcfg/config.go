// Package appcfg decodes and validates the bridge's own manifest settings.
//
// Settings reach an app as an app-private snapshot on the app bus. The
// framework enforces the stored JSON type but not the semantic bounds a
// generated form would, so every value is validated here and an unusable one
// keeps the previous configuration instead of leaving the bridge half
// configured.
package appcfg

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"capsulad/apps/homeassistant/internal/ha"
)

// Setting keys, as declared in manifest.toml.
const (
	KeyBrokerHost        = "broker_host"
	KeyBrokerPort        = "broker_port"
	KeyBrokerTLS         = "broker_tls"
	KeyBrokerTLSRoots    = "broker_tls_roots"
	KeyBrokerCAPEM       = "broker_ca_pem"
	KeyBrokerServerName  = "broker_tls_server_name"
	KeyBrokerUsername    = "broker_username"
	KeyBrokerPassword    = "broker_password"
	KeyBrokerDiscover    = "broker_discover"
	KeyKeepAliveSeconds  = "keep_alive_seconds"
	KeyClientID          = "client_id"
	KeyDiscoveryPrefix   = "discovery_prefix"
	KeyBaseTopic         = "base_topic"
	KeyNodeID            = "node_id"
	KeyStateInterval     = "state_interval_seconds"
	KeySensorInterval    = "sensor_interval_seconds"
	KeyExposeRing        = "expose_ring"
	KeyExposeDisplay     = "expose_display"
	KeyExposeSensors     = "expose_sensors"
	KeyExposeBluetooth   = "expose_bluetooth"
	KeyExposeTouch       = "expose_touch"
	KeyExposeDiagnostics = "expose_diagnostics"
)

// Defaults. They mirror the manifest defaults, so a missing setting behaves
// like an unset one.
const (
	DefaultBrokerPort     = 1883
	DefaultTLSRoots       = "system"
	DefaultKeepAlive      = 30 * time.Second
	DefaultStateInterval  = 30 * time.Second
	DefaultSensorInterval = 60 * time.Second
)

// Config is the validated bridge configuration.
type Config struct {
	// Broker connection.
	Host           string
	Port           int
	TLS            bool
	TLSRoots       string
	CAPEM          string
	TLSServerName  string
	Username       string
	Password       string
	ClientID       string
	DiscoverBroker bool
	KeepAlive      time.Duration

	// Topic layout.
	DiscoveryPrefix string
	BaseTopic       string
	NodeID          string

	// Cadence.
	StateInterval  time.Duration
	SensorInterval time.Duration

	// Exposure.
	ExposeRing        bool
	ExposeDisplay     bool
	ExposeSensors     bool
	ExposeBluetooth   bool
	ExposeTouch       bool
	ExposeDiagnostics bool
}

// Values is the raw snapshot, keyed by setting key.
type Values map[string]string

// Default returns the built-in defaults for every key.
func Default() Values {
	// Every value is the JSON document the framework stores for the declared
	// setting type: a string is quoted, a number or boolean is bare. An empty
	// text value stays "" because the framework omits it.
	return Values{
		KeyBrokerHost:        "",
		KeyBrokerPort:        "1883",
		KeyBrokerTLS:         "false",
		KeyBrokerTLSRoots:    `"system"`,
		KeyBrokerCAPEM:       "",
		KeyBrokerServerName:  "",
		KeyBrokerUsername:    "",
		KeyBrokerPassword:    "",
		KeyBrokerDiscover:    "true",
		KeyKeepAliveSeconds:  "30",
		KeyClientID:          "",
		KeyDiscoveryPrefix:   `"homeassistant"`,
		KeyBaseTopic:         `"capsulad"`,
		KeyNodeID:            "",
		KeyStateInterval:     "30",
		KeySensorInterval:    "60",
		KeyExposeRing:        "true",
		KeyExposeDisplay:     "true",
		KeyExposeSensors:     "true",
		KeyExposeBluetooth:   "true",
		KeyExposeTouch:       "false",
		KeyExposeDiagnostics: "true",
	}
}

// Read decodes the framework's app-private settings snapshot. Unknown keys are
// ignored so a future manifest field cannot break an older binary. A value
// that is not the declared JSON type is an error: the caller keeps its last
// good configuration rather than starting with a half-applied one.
func Read(changed []SettingValue) (Values, error) {
	values := Default()
	for _, setting := range changed {
		if _, known := values[setting.Key]; !known {
			continue
		}
		values[setting.Key] = setting.ValueJSON
	}
	return values, nil
}

// SettingValue mirrors capsulad.SettingValue without importing the SDK, so
// this package stays unit-testable without cgo.
type SettingValue struct {
	Key       string
	ValueJSON string
}

// Build validates a raw snapshot into a Config. The returned warnings are
// non-fatal observations worth one log line each.
func Build(values Values) (Config, []string, error) {
	var cfg Config
	var warnings []string
	fail := func(key, reason string) (Config, []string, error) {
		return Config{}, nil, fmt.Errorf("setting %s: %s", key, reason)
	}

	host, err := textValue(values, KeyBrokerHost)
	if err != nil {
		return fail(KeyBrokerHost, err.Error())
	}
	cfg.Host = strings.TrimSpace(host)

	port, err := intValue(values, KeyBrokerPort)
	if err != nil {
		return fail(KeyBrokerPort, err.Error())
	}
	if port < 1 || port > 65535 {
		return fail(KeyBrokerPort, fmt.Sprintf("%d is outside 1..65535", port))
	}
	cfg.Port = int(port)

	if cfg.TLS, err = boolValue(values, KeyBrokerTLS); err != nil {
		return fail(KeyBrokerTLS, err.Error())
	}
	if cfg.TLSRoots, err = textValue(values, KeyBrokerTLSRoots); err != nil {
		return fail(KeyBrokerTLSRoots, err.Error())
	}
	switch cfg.TLSRoots {
	case "system", "webpki", "pem":
	default:
		return fail(KeyBrokerTLSRoots, fmt.Sprintf("%q is not system, webpki or pem", cfg.TLSRoots))
	}
	if cfg.CAPEM, err = textValue(values, KeyBrokerCAPEM); err != nil {
		return fail(KeyBrokerCAPEM, err.Error())
	}
	cfg.CAPEM = strings.TrimSpace(cfg.CAPEM)
	if cfg.CAPEM != "" && !strings.Contains(cfg.CAPEM, "BEGIN CERTIFICATE") {
		return fail(KeyBrokerCAPEM, "does not contain a PEM CERTIFICATE block")
	}
	if cfg.TLSRoots == "pem" && cfg.CAPEM == "" {
		return fail(KeyBrokerTLSRoots, `roots "pem" trusts only the configured certificates, so broker_ca_pem must not be empty`)
	}
	if !cfg.TLS && cfg.CAPEM != "" {
		warnings = append(warnings, "broker_ca_pem is set but broker_tls is off; the certificates are unused")
	}
	if !cfg.TLS {
		warnings = append(warnings, "broker_tls is off: the MQTT session, its credentials and every state travel unencrypted on the local network")
	}

	if cfg.TLSServerName, err = textValue(values, KeyBrokerServerName); err != nil {
		return fail(KeyBrokerServerName, err.Error())
	}
	cfg.TLSServerName = strings.TrimSpace(cfg.TLSServerName)
	if cfg.TLSServerName != "" && !cfg.TLS {
		warnings = append(warnings, "broker_tls_server_name is set but broker_tls is off")
	}

	if cfg.Username, err = textValue(values, KeyBrokerUsername); err != nil {
		return fail(KeyBrokerUsername, err.Error())
	}
	if cfg.Password, err = textValue(values, KeyBrokerPassword); err != nil {
		return fail(KeyBrokerPassword, err.Error())
	}
	if cfg.Password != "" && cfg.Username == "" {
		// MQTT 3.1.1 forbids a password flag without a user name flag.
		return fail(KeyBrokerUsername, "a password is configured without a user name")
	}

	if cfg.ClientID, err = textValue(values, KeyClientID); err != nil {
		return fail(KeyClientID, err.Error())
	}
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)

	if cfg.DiscoverBroker, err = boolValue(values, KeyBrokerDiscover); err != nil {
		return fail(KeyBrokerDiscover, err.Error())
	}

	keepAlive, err := intValue(values, KeyKeepAliveSeconds)
	if err != nil {
		return fail(KeyKeepAliveSeconds, err.Error())
	}
	if keepAlive < 5 || keepAlive > 600 {
		return fail(KeyKeepAliveSeconds, fmt.Sprintf("%d is outside 5..600", keepAlive))
	}
	cfg.KeepAlive = time.Duration(keepAlive) * time.Second

	if cfg.DiscoveryPrefix, err = textValue(values, KeyDiscoveryPrefix); err != nil {
		return fail(KeyDiscoveryPrefix, err.Error())
	}
	if cfg.BaseTopic, err = textValue(values, KeyBaseTopic); err != nil {
		return fail(KeyBaseTopic, err.Error())
	}
	if cfg.NodeID, err = textValue(values, KeyNodeID); err != nil {
		return fail(KeyNodeID, err.Error())
	}
	cfg.NodeID = strings.TrimSpace(cfg.NodeID)

	stateInterval, err := intValue(values, KeyStateInterval)
	if err != nil {
		return fail(KeyStateInterval, err.Error())
	}
	if stateInterval < 5 || stateInterval > 3600 {
		return fail(KeyStateInterval, fmt.Sprintf("%d is outside 5..3600", stateInterval))
	}
	cfg.StateInterval = time.Duration(stateInterval) * time.Second

	sensorInterval, err := intValue(values, KeySensorInterval)
	if err != nil {
		return fail(KeySensorInterval, err.Error())
	}
	if sensorInterval < 5 || sensorInterval > 3600 {
		return fail(KeySensorInterval, fmt.Sprintf("%d is outside 5..3600", sensorInterval))
	}
	cfg.SensorInterval = time.Duration(sensorInterval) * time.Second

	for _, toggle := range []struct {
		key    string
		target *bool
	}{
		{KeyExposeRing, &cfg.ExposeRing},
		{KeyExposeDisplay, &cfg.ExposeDisplay},
		{KeyExposeSensors, &cfg.ExposeSensors},
		{KeyExposeBluetooth, &cfg.ExposeBluetooth},
		{KeyExposeTouch, &cfg.ExposeTouch},
		{KeyExposeDiagnostics, &cfg.ExposeDiagnostics},
	} {
		value, err := boolValue(values, toggle.key)
		if err != nil {
			return fail(toggle.key, err.Error())
		}
		*toggle.target = value
	}
	if cfg.ExposeTouch {
		warnings = append(warnings,
			"expose_touch is on: the integration holds a partial touch capture, so the panel's play_pause and "+
				"centre controls belong to Home Assistant while it is connected; the volume and microphone "+
				"gestures stay with the device")
	}

	// Validate the topic layout now, using a placeholder node when the node id
	// is derived from the device identity later: a prefix that cannot form a
	// topic must never reach the broker.
	node := cfg.NodeID
	if node == "" {
		node = "capsula"
	}
	if _, err := cfg.TopicLayout(node); err != nil {
		return Config{}, nil, err
	}
	return cfg, warnings, nil
}

// TopicLayout renders the validated topic layout, or an error when a published
// prefix cannot form a valid topic.
func (c Config) TopicLayout(nodeID string) (ha.Topics, error) {
	return ha.NewTopics(c.DiscoveryPrefix, c.BaseTopic, nodeID)
}

// BuildOptions maps the exposure settings onto the catalog builder.
func (c Config) BuildOptions() ha.BuildOptions {
	return ha.BuildOptions{
		Ring:        c.ExposeRing,
		Display:     c.ExposeDisplay,
		Sensors:     c.ExposeSensors,
		Bluetooth:   c.ExposeBluetooth,
		Touch:       c.ExposeTouch,
		Diagnostics: c.ExposeDiagnostics,
	}
}

// Redacted renders the configuration for a log line, without the broker
// password or the CA bundle.
func (c Config) Redacted() string {
	credentials := "none"
	if c.Username != "" {
		credentials = "username set"
	}
	return fmt.Sprintf("broker=%s port=%d tls=%v roots=%s credentials=%s discover=%v topics=%s/%s node=%q "+
		"intervals=state %s sensor %s exposure=ring:%v display:%v sensors:%v bluetooth:%v touch:%v diagnostics:%v",
		c.Host, c.Port, c.TLS, c.TLSRoots, credentials, c.DiscoverBroker,
		c.DiscoveryPrefix, c.BaseTopic, c.NodeID,
		c.StateInterval, c.SensorInterval,
		c.ExposeRing, c.ExposeDisplay, c.ExposeSensors, c.ExposeBluetooth, c.ExposeTouch, c.ExposeDiagnostics)
}

func textValue(values Values, key string) (string, error) {
	raw, ok := values[key]
	if !ok || raw == "" {
		return "", nil
	}
	var out string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", fmt.Errorf("expected a JSON string")
	}
	return out, nil
}

func boolValue(values Values, key string) (bool, error) {
	raw, ok := values[key]
	if !ok || raw == "" {
		return false, nil
	}
	var out bool
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return false, fmt.Errorf("expected a JSON boolean")
	}
	return out, nil
}

func intValue(values Values, key string) (int64, error) {
	raw, ok := values[key]
	if !ok || raw == "" {
		return 0, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return 0, fmt.Errorf("expected a JSON integer")
	}
	number, ok := decoded.(json.Number)
	if !ok {
		// A quoted number is the wrong stored type for an integer setting.
		return 0, fmt.Errorf("expected a JSON integer")
	}
	if out, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
		return out, nil
	}
	// A direct gRPC update may store an integral value as 30.0; a non-integral
	// value is still refused.
	asFloat, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || asFloat != float64(int64(asFloat)) {
		return 0, fmt.Errorf("expected a JSON integer")
	}
	return int64(asFloat), nil
}
