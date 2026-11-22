package config

import (
	"strings"
	"testing"
	"time"

	"capsulad/sdk/capsulad"
)

func value(key, raw string) capsulad.SettingValue {
	return capsulad.SettingValue{Key: key, ValueJSON: raw}
}

func TestDefaultMatchesManifest(t *testing.T) {
	cfg := Default()
	if cfg.ServerURL != "ws://127.0.0.1:8080/v1/stream" || cfg.Language != "ru-RU" ||
		!cfg.FollowUp || cfg.FollowUpTimeout != 4*time.Second || !cfg.Earcons {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestApplyAcceptsEveryDeclaredSetting(t *testing.T) {
	cfg, rejected := Apply(Default(), &capsulad.SettingsChanged{Values: []capsulad.SettingValue{
		value(KeyServerURL, `"wss://example.test/voice/stream"`),
		value(KeyToken, `" secret "`),
		value(KeyDeviceID, `" kitchen "`),
		value(KeyLanguage, `"en-US"`),
		value(KeyLocation, `"Ashdod"`),
		value(KeyTimezone, `"Asia/Jerusalem"`),
		value(KeyFollowUp, `false`),
		value(KeyFollowUpTimeout, `2500`),
		value(KeyEarcons, `false`),
	}})
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v", rejected)
	}
	if cfg.ServerURL != "wss://example.test/voice/stream" || cfg.Token != "secret" ||
		cfg.DeviceID != "kitchen" || cfg.Language != "en-US" || cfg.Location != "Ashdod" ||
		cfg.Timezone != "Asia/Jerusalem" || cfg.FollowUp || cfg.Earcons ||
		cfg.FollowUpTimeout != 2500*time.Millisecond {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestApplyKeepsLastGoodValueForAnInvalidField(t *testing.T) {
	current := Default()
	current.Token = "old"
	cfg, rejected := Apply(current, &capsulad.SettingsChanged{Values: []capsulad.SettingValue{
		value(KeyServerURL, `"http://example.test/v1/stream"`),
		value(KeyToken, `"new"`),
		value(KeyFollowUpTimeout, `60001`),
		value(KeyFollowUp, `"yes"`),
		value(KeyTimezone, `"Mars/Olympus"`),
	}})
	if len(rejected) != 4 {
		t.Fatalf("rejected = %v, want four", rejected)
	}
	if cfg.ServerURL != DefaultServerURL || cfg.Token != "new" ||
		cfg.FollowUpTimeout != DefaultFollowUpTimeout || !cfg.FollowUp || cfg.Timezone != "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestApplyRejectsWrongJSONTypes(t *testing.T) {
	_, rejected := Apply(Default(), &capsulad.SettingsChanged{Values: []capsulad.SettingValue{
		value(KeyDeviceID, `42`),
		value(KeyFollowUp, `"true"`),
		value(KeyFollowUpTimeout, `"4000"`),
	}})
	if len(rejected) != 3 {
		t.Fatalf("rejected = %v, want three", rejected)
	}
	for _, reason := range rejected {
		if strings.Contains(reason, "Mars") {
			t.Fatalf("rejection leaked a value: %s", reason)
		}
	}
}

func TestApplyRejectsHeaderInjectionInToken(t *testing.T) {
	cfg, rejected := Apply(Default(), &capsulad.SettingsChanged{Values: []capsulad.SettingValue{
		value(KeyToken, `"secret\r\nX-Injected: yes"`),
	}})
	if len(rejected) != 1 {
		t.Fatalf("rejected = %v", rejected)
	}
	if cfg.Token != "" {
		t.Fatalf("token = %q", cfg.Token)
	}
}

func TestApplyClearsUnsetMaskedValue(t *testing.T) {
	current := Default()
	current.Token = "old"
	cfg, rejected := Apply(current, &capsulad.SettingsChanged{Values: []capsulad.SettingValue{
		value(KeyToken, ``),
		value(KeyDeviceID, `""`),
	}})
	if len(rejected) != 0 {
		t.Fatalf("rejected = %v", rejected)
	}
	if cfg.Token != "" || cfg.DeviceID != "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		raw  string
		want Endpoint
		bad  string
	}{
		{raw: "ws://127.0.0.1:8080/v1/stream", want: Endpoint{
			Host: "127.0.0.1", Port: 8080, HostHeader: "127.0.0.1:8080", Path: "/v1/stream",
			Raw: "ws://127.0.0.1:8080/v1/stream",
		}},
		{raw: "wss://voice.example.test/stream", want: Endpoint{
			TLS: true, Host: "voice.example.test", Port: 443, HostHeader: "voice.example.test",
			Path: "/stream", Raw: "wss://voice.example.test/stream",
		}},
		{raw: "ws://voice.example.test:9000/a/b", want: Endpoint{
			Host: "voice.example.test", Port: 9000, HostHeader: "voice.example.test:9000",
			Path: "/a/b", Raw: "ws://voice.example.test:9000/a/b",
		}},
		{raw: "http://voice.example.test/v1/stream", bad: "scheme"},
		{raw: "ws:///v1/stream", bad: "host"},
		{raw: "ws://voice.example.test", bad: "path"},
		{raw: "ws://user:pass@voice.example.test/v1/stream", bad: "user info"},
		{raw: "ws://voice.example.test/v1/stream?x=1", bad: "query"},
		{raw: "ws://voice.example.test:0/v1/stream", bad: "port"},
	}
	for _, tc := range cases {
		got, err := ParseEndpoint(tc.raw)
		if tc.bad != "" {
			if err == nil || !strings.Contains(err.Error(), tc.bad) {
				t.Errorf("%s: err = %v, want %q", tc.raw, err, tc.bad)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}
