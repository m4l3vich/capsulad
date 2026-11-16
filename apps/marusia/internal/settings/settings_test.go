package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/apps/marusia/internal/vkclient"
	"capsulad/sdk/capsulad"
)

func snapshot(values map[string]string) *capsulad.SettingsChanged {
	changed := &capsulad.SettingsChanged{Revision: 1}
	for key, value := range values {
		encoded := `"` + value + `"`
		changed.Values = append(changed.Values, capsulad.SettingValue{Key: key, ValueJSON: encoded})
	}
	return changed
}

func TestReadAppliesDefaults(t *testing.T) {
	values, err := Read(snapshot(map[string]string{KeyXToken: "tok"}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if values.Endpoint != DefaultEndpoint || values.AudioFormat != DefaultAudioFormat ||
		values.ClientVersion != DefaultClientVersion || values.Keyword != DefaultKeyword ||
		values.HWID != DefaultHWID || values.SessionFile != DefaultSessionFile {
		t.Fatalf("values = %+v", values)
	}
	if values.XToken != "tok" {
		t.Fatalf("xtoken = %q", values.XToken)
	}
}

func TestReadRejectsMalformedJSON(t *testing.T) {
	changed := &capsulad.SettingsChanged{Values: []capsulad.SettingValue{
		{Key: KeyEndpoint, ValueJSON: `{not a string}`},
	}}
	if _, err := Read(changed); err == nil {
		t.Fatal("malformed value must be rejected")
	}

	// A malformed masked value must not be echoed back through the error.
	secret := &capsulad.SettingsChanged{Values: []capsulad.SettingValue{
		{Key: KeySessionSecret, ValueJSON: `leaked-secret-value`},
	}}
	_, err := Read(secret)
	if err == nil {
		t.Fatal("malformed secret must be rejected")
	}
	if strings.Contains(err.Error(), "leaked-secret-value") {
		t.Fatalf("the error echoed the credential: %v", err)
	}
}

func TestBuildEndpoints(t *testing.T) {
	config, warnings, err := Build(Values{
		Endpoint: "prod", AudioFormat: "wave", DeviceID: "dev-1", XToken: "tok",
	}, Fallback{Serial: "SN1"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.BaseURL != vkclient.Endpoints["prod"] {
		t.Fatalf("base = %q", config.BaseURL)
	}
	if config.Serial != "SN1" || config.SerialSource != SerialSourceCPUInfo || config.DeviceID != "dev-1" || !config.Provisioned() {
		t.Fatalf("config = %+v", config)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}

	custom, _, err := Build(Values{
		Endpoint: "prod", EndpointCustom: "https://vc1.example", AudioFormat: "wave",
		DeviceID: "dev-1",
	}, Fallback{})
	if err != nil {
		t.Fatalf("custom: %v", err)
	}
	if custom.BaseURL != "https://vc1.example" {
		t.Fatalf("custom base = %q", custom.BaseURL)
	}

	if _, _, err := Build(Values{Endpoint: "nowhere", AudioFormat: "wave"}, Fallback{}); err == nil {
		t.Fatal("unknown endpoint must be rejected")
	}
	if _, _, err := Build(Values{
		Endpoint: "prod", EndpointCustom: "https://host/path", AudioFormat: "wave",
	}, Fallback{}); err == nil {
		t.Fatal("endpoint with a path must be rejected")
	}
}

func TestBuildWarnsForGeneratedIdentity(t *testing.T) {
	config, warnings, err := Build(Values{Endpoint: "prod", AudioFormat: "wave"}, Fallback{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.DeviceID == "" {
		t.Fatal("a device id must be generated")
	}
	if config.Serial != "" || config.SerialSource != "" {
		t.Fatalf("serial = %q source = %q; a framework device id must never stand in for the SoC serial",
			config.Serial, config.SerialSource)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "device_id is empty") || !strings.Contains(joined, "device serial is missing") {
		t.Fatalf("warnings = %v", warnings)
	}
	if !strings.Contains(joined, "per-process id") {
		t.Fatalf("generated device id must be reported as non-persistent: %v", warnings)
	}
	if strings.Contains(joined, config.DeviceID) {
		t.Fatalf("the generated device id must not appear in a log warning: %v", warnings)
	}
}

func TestBuildSerialPrecedence(t *testing.T) {
	fallback := Fallback{Serial: "11bfcbc5d63d55fc", DeviceID: "dev-1"}

	// The explicit setting is authoritative over the SoC serial.
	config, warnings, err := Build(Values{
		Endpoint: "prod", AudioFormat: "wave", Serial: "SN-setting",
	}, fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.Serial != "SN-setting" || config.SerialSource != SerialSourceSettings {
		t.Fatalf("serial = %q source = %q", config.Serial, config.SerialSource)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}

	// With no setting the SoC serial from /proc/cpuinfo is used.
	config, warnings, err = Build(Values{Endpoint: "prod", AudioFormat: "wave"}, fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.Serial != "11bfcbc5d63d55fc" || config.SerialSource != SerialSourceCPUInfo {
		t.Fatalf("serial = %q source = %q", config.Serial, config.SerialSource)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestBuildWarnsForOpus(t *testing.T) {
	_, warnings, err := Build(Values{
		Endpoint: "prod", AudioFormat: "ogg_opus", DeviceID: "d", Serial: "s",
	}, Fallback{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ogg_opus") {
		t.Fatalf("warnings = %v", warnings)
	}
	if _, _, err := Build(Values{Endpoint: "prod", AudioFormat: "flac"}, Fallback{}); err == nil {
		t.Fatal("unknown audio format must be rejected")
	}
}

func TestLoadVendorSession(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.json")
	devicePath := filepath.Join(dir, "device_settings.json")
	if _, err := LoadVendorSession(sessionPath); err != nil {
		t.Fatalf("missing file should mean absent session: %v", err)
	}
	if err := os.WriteFile(sessionPath, []byte(`{"account_id":"account","session":"session-id","secret":"secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicePath, []byte(`{"general":{"id":":c:d:capsula_mini:device-id"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadVendorSession(sessionPath)
	if err != nil {
		t.Fatalf("load valid session: %v", err)
	}
	if got.SessionID != "session-id" || got.Secret != "secret-value" || got.DeviceID != ":c:d:capsula_mini:device-id" || got.AccountID != "account" {
		t.Fatalf("session = %+v", got)
	}

	if err := os.WriteFile(sessionPath, []byte(`{"session":"only-id"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadVendorSession(sessionPath)
	if err == nil || strings.Contains(err.Error(), "only-id") {
		t.Fatalf("invalid file must return a redacted error, got session=%+v error=%v", got, err)
	}

	if err := os.WriteFile(sessionPath, []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVendorSession(sessionPath); err == nil || strings.Contains(err.Error(), "not-json") {
		t.Fatalf("malformed file error must be redacted, got %v", err)
	}
}

func TestReadCPUSerial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpuinfo")
	content := "processor\t: 0\nHardware\t: Rockchip\nSerial\t\t: 11bfcbc5d63d55fc\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	serial, err := readCPUSerial(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if serial != "11bfcbc5d63d55fc" {
		t.Fatalf("serial = %q", serial)
	}

	spaces := filepath.Join(dir, "cpuinfo-spaces")
	if err := os.WriteFile(spaces, []byte("Serial  :   spaced-serial  \n"), 0o600); err != nil {
		t.Fatalf("write spaces: %v", err)
	}
	if serial, err := readCPUSerial(spaces); err != nil || serial != "spaced-serial" {
		t.Fatalf("spaces serial = %q, %v", serial, err)
	}

	missing := filepath.Join(dir, "no-serial")
	if err := os.WriteFile(missing, []byte("processor\t: 0\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if serial, err := readCPUSerial(missing); err != nil || serial != "" {
		t.Fatalf("missing serial = %q, %v", serial, err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if serial, err := readCPUSerial(empty); err != nil || serial != "" {
		t.Fatalf("empty file serial = %q, %v", serial, err)
	}
}

func TestBuildRequiresCompleteSessionPair(t *testing.T) {
	_, _, err := Build(Values{Endpoint: "prod", AudioFormat: "wave", SessionID: "sid"},
		Fallback{Serial: "s", DeviceID: "d"})
	if err == nil || !strings.Contains(err.Error(), KeySessionSecret) {
		t.Fatalf("a session id without a secret must be rejected, got %v", err)
	}
	if strings.Contains(err.Error(), "sid") {
		t.Fatalf("the error must not echo the credential: %v", err)
	}

	config, warnings, err := Build(Values{
		Endpoint: "prod", AudioFormat: "wave",
		SessionID: "sid", SessionSecret: "secret", DeviceID: "d",
	}, Fallback{Serial: "s"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !config.HasSession() || config.SessionID != "sid" || config.SessionSecret != "secret" {
		t.Fatalf("config = %+v", config)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	// Default() supplies the file path; Build copies whatever it is given, so an
	// explicit empty value is the operator's way of disabling the fallback.
	if Default().SessionFile != DefaultSessionFile {
		t.Fatalf("default session file = %q", Default().SessionFile)
	}

	// An explicit empty file setting disables the fallback without an error.
	config, _, err = Build(Values{
		Endpoint: "prod", AudioFormat: "wave", SessionFile: "", DeviceID: "d", Serial: "s",
	}, Fallback{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.SessionFile != "" {
		t.Fatalf("session file = %q, want empty", config.SessionFile)
	}
}

// TestPlaintextWarningOnCredentialCarryingEndpoints pins the decision behind
// the one startup warning: a plaintext base is kept (test2 is a deliberate
// operator test choice) but it is never silent when a credential would travel
// over it.
func TestPlaintextWarningOnCredentialCarryingEndpoints(t *testing.T) {
	file := "/data/voice-agent/session.json"
	cases := []struct {
		name    string
		config  Config
		want    string // a substring the warning must carry; "" means no warning
		notWant string
	}{
		{
			name:   "https with a configured session is silent",
			config: Config{BaseURL: "https://vc.go.mail.ru", SessionID: "sid", SessionSecret: "secret"},
		},
		{
			name:   "plaintext without any credential source is silent",
			config: Config{BaseURL: "http://voice-test2.example"},
		},
		{
			name:    "plaintext with a configured session names the bearer token",
			config:  Config{BaseURL: "http://voice-test2.example", SessionID: "sid", SessionSecret: "secret"},
			want:    "unencrypted bearer token",
			notWant: "secret",
		},
		{
			name:    "plaintext with an xtoken names the attach body",
			config:  Config{BaseURL: "http://voice-test2.example", XTokenID: "TOKEN-VALUE"},
			want:    "xtoken_id is sent in the attach body",
			notWant: "TOKEN-VALUE",
		},
		{
			name:    "plaintext with only a session file warns about the eventual session",
			config:  Config{BaseURL: "http://voice-test2.example", SessionFile: file},
			want:    file,
			notWant: "session_id",
		},
		{
			name:   "uppercase scheme is still plaintext",
			config: Config{BaseURL: "HTTP://voice-test2.example", SessionID: "sid", SessionSecret: "s"},
			// The base is echoed as written; only the scheme test is normalised.
			want: "unencrypted bearer token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warning := PlaintextWarning(tc.config)
			if tc.want == "" {
				if warning != "" {
					t.Fatalf("warning = %q, want none", warning)
				}
				return
			}
			if !strings.Contains(warning, tc.want) {
				t.Fatalf("warning = %q, want it to mention %q", warning, tc.want)
			}
			if tc.notWant != "" && strings.Contains(warning, tc.notWant) {
				t.Fatalf("warning = %q leaked %q", warning, tc.notWant)
			}
			if !strings.Contains(warning, tc.config.BaseURL) {
				t.Fatalf("warning = %q, want the endpoint named", warning)
			}
		})
	}
}

// The capability payload defaults to the recovered Capsula Mini token, survives
// Build unchanged, and a malformed operator override is rejected instead of
// being sent.
func TestBuildCarriesTheCapabilitiesPayload(t *testing.T) {
	config, _, err := Build(Values{Endpoint: "prod", AudioFormat: "wave", DeviceID: "dev-1",
		Serial: "SN1"}, Fallback{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := string(config.CapabilitiesJSON); got != DefaultCapabilitiesJSON {
		t.Fatalf("capabilities = %q, want %q", got, DefaultCapabilitiesJSON)
	}
	if !strings.Contains(DefaultCapabilitiesJSON, vkclient.DefaultCapabilitiesHex) {
		t.Fatalf("the model default does not carry the recovered token: %q", DefaultCapabilitiesJSON)
	}
	if len(config.ClientDataJSON) != 0 {
		t.Fatalf("client_data = %q, want unset", config.ClientDataJSON)
	}

	custom := `{"capabilities_hex":"deadbeef"}`
	config, _, err = Build(Values{Endpoint: "prod", AudioFormat: "wave", DeviceID: "dev-1",
		Serial: "SN1", Capabilities: custom, ClientData: `{"client":"capsula"}`}, Fallback{})
	if err != nil {
		t.Fatalf("build with an override: %v", err)
	}
	if string(config.CapabilitiesJSON) != custom {
		t.Fatalf("capabilities = %q, want %q", config.CapabilitiesJSON, custom)
	}
	if string(config.ClientDataJSON) != `{"client":"capsula"}` {
		t.Fatalf("client_data = %q", config.ClientDataJSON)
	}

	for _, payload := range []string{"not-json", `{"capabilities_hex":"abc"}`, `{"capabilities_hex":"zz"}`} {
		if _, _, err := Build(Values{Endpoint: "prod", AudioFormat: "wave", DeviceID: "dev-1",
			Serial: "SN1", Capabilities: payload}, Fallback{}); err == nil {
			t.Fatalf("capabilities_json=%q must be rejected", payload)
		}
	}
}

// kw_params is only valid for audio that begins with the keyword, and this app
// only gets that for wake-word captures (the 1 s pre-roll attached to any
// activation-id capture is room audio ahead of the command for touch and API
// turns). The default policy is no_kw; kw_params is opt-in and validated.
func TestBuildKeywordMode(t *testing.T) {
	config, warnings, err := Build(Values{Endpoint: "prod", AudioFormat: "wave", DeviceID: "dev-1",
		Serial: "SN1"}, Fallback{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.KeywordMode != KeywordModeNoKeyword {
		t.Fatalf("keyword_mode = %q, want %q", config.KeywordMode, KeywordModeNoKeyword)
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "keyword_mode") {
			t.Fatalf("the default policy must not warn: %q", warning)
		}
	}

	config, warnings, err = Build(Values{Endpoint: "prod", AudioFormat: "wave", DeviceID: "dev-1",
		Serial: "SN1", KeywordMode: KeywordModeParams}, Fallback{})
	if err != nil {
		t.Fatalf("build with kw_params: %v", err)
	}
	if config.KeywordMode != KeywordModeParams {
		t.Fatalf("keyword_mode = %q", config.KeywordMode)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "keyword not confirmed") {
		t.Fatalf("kw_params must warn about the keyword confirmation: %v", warnings)
	}

	if _, _, err := Build(Values{Endpoint: "prod", AudioFormat: "wave", DeviceID: "dev-1",
		Serial: "SN1", KeywordMode: "strict_kw"}, Fallback{}); err == nil {
		t.Fatal("an unknown keyword mode must be rejected")
	}
}
