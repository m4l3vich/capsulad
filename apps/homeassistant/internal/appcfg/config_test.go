package appcfg

import (
	"strings"
	"testing"
	"time"
)

func valuesFrom(pairs map[string]string) Values {
	values := Default()
	for key, raw := range pairs {
		values[key] = raw
	}
	return values
}

func TestDefaultBuildsAValidConfiguration(t *testing.T) {
	cfg, warnings, err := Build(Default())
	if err != nil {
		t.Fatalf("Build(defaults): %v", err)
	}
	if cfg.Port != DefaultBrokerPort || cfg.TLS || cfg.TLSRoots != "system" {
		t.Fatalf("unexpected broker defaults: %+v", cfg)
	}
	if !cfg.ExposeRing || !cfg.ExposeDisplay || !cfg.ExposeSensors || !cfg.ExposeBluetooth || !cfg.ExposeDiagnostics {
		t.Fatalf("exposure defaults are not permissive: %+v", cfg)
	}
	if cfg.ExposeTouch {
		t.Fatal("touch exposure is on by default")
	}
	if cfg.Host != "" {
		t.Fatal("an empty host was accepted as a broker address")
	}
	if cfg.StateInterval != DefaultStateInterval || cfg.SensorInterval != DefaultSensorInterval {
		t.Fatalf("cadence defaults: %+v", cfg)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "tls is off") {
		t.Fatalf("warnings = %v, want the plaintext warning", warnings)
	}
	if strings.Contains(cfg.Redacted(), "secret") {
		t.Fatal("Redacted leaked a secret")
	}
}

func TestBuildAppliesValues(t *testing.T) {
	cfg, warnings, err := Build(valuesFrom(map[string]string{
		KeyBrokerHost:        `"mqtt.example.lan"`,
		KeyBrokerPort:        `8883`,
		KeyBrokerTLS:         `true`,
		KeyBrokerTLSRoots:    `"pem"`,
		KeyBrokerCAPEM:       `"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"`,
		KeyBrokerUsername:    `"capsula"`,
		KeyBrokerPassword:    `"s3cret"`,
		KeyBrokerDiscover:    `false`,
		KeyKeepAliveSeconds:  `45`,
		KeyDiscoveryPrefix:   `"ha"`,
		KeyBaseTopic:         `"capsula"`,
		KeyNodeID:            `"living-room"`,
		KeyStateInterval:     `15`,
		KeySensorInterval:    `120`,
		KeyExposeTouch:       `true`,
		KeyExposeBluetooth:   `false`,
		KeyExposeDiagnostics: `false`,
	}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.Host != "mqtt.example.lan" || cfg.Port != 8883 || !cfg.TLS || cfg.TLSRoots != "pem" {
		t.Fatalf("broker settings: %+v", cfg)
	}
	if cfg.Username != "capsula" || cfg.Password != "s3cret" {
		t.Fatalf("credentials: %+v", cfg)
	}
	if cfg.KeepAlive != 45*time.Second || cfg.StateInterval != 15*time.Second || cfg.SensorInterval != 120*time.Second {
		t.Fatalf("cadence: %+v", cfg)
	}
	if cfg.ExposeBluetooth || cfg.ExposeDiagnostics || !cfg.ExposeTouch {
		t.Fatalf("exposure: %+v", cfg)
	}
	if cfg.Host != "mqtt.example.lan" || cfg.Port != 8883 {
		t.Fatalf("broker address %s:%d", cfg.Host, cfg.Port)
	}
	if strings.Contains(cfg.Redacted(), "s3cret") || strings.Contains(cfg.Redacted(), "BEGIN CERTIFICATE") {
		t.Fatalf("Redacted leaked a secret: %s", cfg.Redacted())
	}
	// The touch warning is the only one: TLS is on and the CA is used.
	for _, warning := range warnings {
		if strings.Contains(warning, "tls is off") {
			t.Fatalf("unexpected plaintext warning: %v", warnings)
		}
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "expose_touch") {
		t.Fatalf("touch warning missing: %v", warnings)
	}
}

func TestBuildAcceptsIntegralFloats(t *testing.T) {
	for _, raw := range []string{`30`, `30.0`} {
		values := valuesFrom(map[string]string{KeyKeepAliveSeconds: raw})
		cfg, _, err := Build(values)
		if err != nil {
			t.Fatalf("keep_alive_seconds %s: %v", raw, err)
		}
		if cfg.KeepAlive != 30*time.Second {
			t.Fatalf("keep_alive_seconds %s -> %s", raw, cfg.KeepAlive)
		}
	}
	values := valuesFrom(map[string]string{KeyKeepAliveSeconds: `30.5`})
	if _, _, err := Build(values); err == nil {
		t.Fatal("a fractional keep alive was accepted")
	}
}

func TestBuildRejectsUnusableValues(t *testing.T) {
	cases := map[string]map[string]string{
		"wrong type":           {KeyBrokerPort: `"1883"`},
		"port zero":            {KeyBrokerPort: `0`},
		"port too high":        {KeyBrokerPort: `70000`},
		"unknown roots":        {KeyBrokerTLSRoots: `"magic"`},
		"pem roots no bundle":  {KeyBrokerTLSRoots: `"pem"`},
		"ca without a cert":    {KeyBrokerCAPEM: `"not a certificate"`},
		"password no user":     {KeyBrokerPassword: `"x"`},
		"keepalive too small":  {KeyKeepAliveSeconds: `1`},
		"keepalive too large":  {KeyKeepAliveSeconds: `1000`},
		"state interval small": {KeyStateInterval: `1`},
		"state interval large": {KeyStateInterval: `99999`},
		"sensor interval bad":  {KeySensorInterval: `0`},
		"boolean as string":    {KeyExposeRing: `"yes"`},
		"wildcard base topic":  {KeyBaseTopic: `"capsulad/#"`},
		"wildcard discovery":   {KeyDiscoveryPrefix: `"ha/+"`},
		"empty base level":     {KeyBaseTopic: `"capsulad//x"`},
	}
	for name, pairs := range cases {
		if _, _, err := Build(valuesFrom(pairs)); err == nil {
			t.Fatalf("%s: Build accepted %v", name, pairs)
		}
	}
}

func TestReadIgnoresUnknownKeysAndKeepsDefaults(t *testing.T) {
	values, err := Read([]SettingValue{
		{Key: KeyBrokerHost, ValueJSON: `"broker.local"`},
		{Key: "future_setting", ValueJSON: `42`},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	cfg, _, err := Build(values)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.Host != "broker.local" {
		t.Fatalf("host %q", cfg.Host)
	}
	if cfg.Port != DefaultBrokerPort {
		t.Fatalf("unspecified settings did not keep their defaults: %+v", cfg)
	}
}

func TestTopicLayoutUsesTheConfiguredNode(t *testing.T) {
	cfg, _, err := Build(valuesFrom(map[string]string{KeyNodeID: `"kitchen"`}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	topics, err := cfg.TopicLayout(cfg.NodeID)
	if err != nil {
		t.Fatalf("TopicLayout: %v", err)
	}
	if got, want := topics.StatusTopic(), "capsulad/kitchen/status"; got != want {
		t.Fatalf("StatusTopic = %q, want %q", got, want)
	}
	cfg.BaseTopic = "bad/#"
	if _, err := cfg.TopicLayout("kitchen"); err == nil {
		t.Fatal("an invalid base topic produced a layout")
	}
}

func TestBuildOptionsFollowExposure(t *testing.T) {
	cfg, _, err := Build(valuesFrom(map[string]string{
		KeyExposeRing:    `false`,
		KeyExposeDisplay: `false`,
		KeyExposeTouch:   `true`,
	}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	opts := cfg.BuildOptions()
	if opts.Ring || opts.Display || !opts.Touch || !opts.Sensors {
		t.Fatalf("unexpected options %+v", opts)
	}
}
