package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/discovery"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

func testLogs(t *testing.T) *logbus.Bus {
	t.Helper()
	logs, err := logbus.New(ids.NewClock(), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestUserSettingsCASAndValidation(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenUser(filepath.Join(dir, "settings.json"), testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Get()
	patch := snap.Values
	patch.MasterVolume = 0.42
	next, err := store.Update(snap.Revision, patch, []string{"master_volume"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if next.Values.MasterVolume != 0.42 || next.Revision != snap.Revision+1 {
		t.Fatalf("unexpected snapshot: %+v", next)
	}
	if _, err := store.Update(snap.Revision, patch, []string{"master_volume"}); err == nil {
		t.Fatal("stale compare-and-swap was accepted")
	}
	patch.MasterVolume = 2
	if _, err := store.Update(0, patch, []string{"master_volume"}); err == nil {
		t.Fatal("out-of-range volume was accepted")
	}
	// Reload from disk.
	reopened, err := OpenUser(filepath.Join(dir, "settings.json"), testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get(); got.Values.MasterVolume != 0.42 || got.Revision != next.Revision {
		t.Fatalf("persistence failed: %+v", got)
	}
}

func TestUserSnapshotsDoNotExposeBrightnessMode(t *testing.T) {
	store, err := OpenUser(filepath.Join(t.TempDir(), "settings.json"), testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := store.Get()
	snapshot.Values.BrightnessMode["light.ring.main"] = BrightnessAuto
	if mode := store.Get().Values.BrightnessMode["light.ring.main"]; mode != "" {
		t.Fatalf("mutating a caller snapshot changed live brightness mode to %q", mode)
	}
}

func TestAppSettingsMaskedRedactionAndClear(t *testing.T) {
	dir := t.TempDir()
	lookup := func(appID string) (*ManifestSchema, bool) {
		if appID != "org.example.weather" {
			return nil, false
		}
		return &ManifestSchema{AppID: appID, Settings: []SchemaSetting{
			{Key: "city", Type: "text", DefaultJSON: `"Kazan"`},
			{Key: "api_key", Type: "text_masked"},
			{Key: "refresh", Type: "integer", DefaultJSON: "15"},
		}}, true
	}
	store, err := OpenApps(filepath.Join(dir, "apps"), testLogs(t), lookup)
	if err != nil {
		t.Fatal(err)
	}
	snap, rev, private, err := store.Update("org.example.weather", 0, []Change{
		{Key: "api_key", Value: "sekrit"},
		{Key: "refresh", Value: json.Number("30")},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var masked *AppSettingView
	for i := range snap.Values {
		if snap.Values[i].Key == "api_key" {
			masked = &snap.Values[i]
		}
	}
	if masked == nil || masked.Value != "" || !masked.HasValue {
		t.Fatalf("masked setting was not redacted: %+v", masked)
	}
	if string(private["api_key"]) != `"sekrit"` {
		t.Fatalf("private value missing: %s", private["api_key"])
	}
	if rev == 0 {
		t.Fatal("revision not advanced")
	}
	// Reuse the stale revision.
	if _, _, _, err := store.Update("org.example.weather", 1, []Change{{Key: "city", Value: "Moscow"}}); err == nil {
		t.Fatal("stale update accepted")
	}
	if _, _, _, err := store.Update("org.example.weather", rev, []Change{{Key: "api_key", Value: ""}}); err == nil {
		t.Fatal("empty masked value cleared the secret without an explicit clear")
	}
	if _, _, _, err := store.Update("org.example.weather", rev, []Change{{Key: "api_key", Clear: true}}); err != nil {
		t.Fatalf("explicit clear failed: %v", err)
	}
	snap, _, _, _ = store.Update("org.example.weather", rev+1, []Change{{Key: "city", Value: "Moscow"}})
	for _, v := range snap.Values {
		if v.Key == "api_key" && v.HasValue {
			t.Fatalf("cleared secret still reported as set: %+v", v)
		}
	}
	// Reopen from disk: the secret must stay cleared and city persisted.
	reopened, err := OpenApps(filepath.Join(dir, "apps"), testLogs(t), lookup)
	if err != nil {
		t.Fatal(err)
	}
	snap, err = reopened.Snapshot("org.example.weather")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range snap.Values {
		if v.Key == "api_key" && v.HasValue {
			t.Fatal("secret reappeared after reopen")
		}
		if v.Key == "city" && v.Value != `"Moscow"` {
			t.Fatalf("city not persisted: %s", v.Value)
		}
	}
}

func TestAppWatchSnapshotIsAtomicAndConvergesOnNewest(t *testing.T) {
	lookup := func(appID string) (*ManifestSchema, bool) {
		if appID != "test.watch" {
			return nil, false
		}
		return &ManifestSchema{AppID: appID, Settings: []SchemaSetting{
			{Key: "value", Type: "text", DefaultJSON: `"0"`},
		}}, true
	}
	store, err := OpenApps(filepath.Join(t.TempDir(), "apps"), testLogs(t), lookup)
	if err != nil {
		t.Fatal(err)
	}
	initial, ch, cancel, err := store.WatchApp("test.watch")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if initial.Revision != 1 || len(initial.Values) != 1 || initial.Values[0].Value != `"0"` {
		t.Fatalf("initial snapshot = %+v", initial)
	}
	// More updates than the watcher buffer without reading: the subscription
	// must converge on the newest revision instead of dropping it.
	for i := 1; i <= 8; i++ {
		if _, _, _, err := store.Update("test.watch", 0, []Change{{
			Key: "value", Value: fmt.Sprintf("%d", i),
		}}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	var last AppSnapshot
	for {
		select {
		case snap := <-ch:
			last = snap
			continue
		default:
		}
		break
	}
	if last.Revision != 9 || last.Values[0].Value != `"8"` {
		t.Fatalf("coalesced last snapshot = %+v, want revision 9 value 8", last)
	}
}

func TestUserSettingsTimezoneValidation(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenUser(filepath.Join(dir, "settings.json"), testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Get()
	patch := snap.Values
	patch.Timezone = "Asia/Shanghai"
	next, err := store.Update(snap.Revision, patch, []string{"timezone"})
	if err != nil {
		t.Fatalf("valid IANA zone rejected: %v", err)
	}
	if next.Values.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone not stored: %+v", next.Values)
	}
	patch.Timezone = "Not/AZone"
	if _, err := store.Update(next.Revision, patch, []string{"timezone"}); err == nil {
		t.Fatal("invalid zone name accepted")
	}
	// An empty value means "follow the firmware /etc/localtime".
	patch.Timezone = ""
	if _, err := store.Update(next.Revision, patch, []string{"timezone"}); err != nil {
		t.Fatalf("empty timezone rejected: %v", err)
	}
}

// A host name must be one valid DNS label. Rejected values must not replace
// the stored name, and a successful value must survive a daemon restart.
func TestUserSettingsHostnameValidationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := OpenUser(path, testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Get()
	if snap.Values.Hostname != DefaultHostname {
		t.Fatalf("default hostname = %q", snap.Values.Hostname)
	}
	patch := snap.Values
	patch.Hostname = "  Living-Room  "
	next, err := store.Update(snap.Revision, patch, []string{"hostname"})
	if err != nil {
		t.Fatalf("valid hostname rejected: %v", err)
	}
	if next.Values.Hostname != "living-room" {
		t.Fatalf("hostname not canonicalized: %q", next.Values.Hostname)
	}
	// A stale revision must not replace the host name.
	patch.Hostname = "stale"
	if _, err := store.Update(snap.Revision, patch, []string{"hostname"}); err == nil {
		t.Fatal("stale hostname update was accepted")
	}
	if got := store.Get(); got.Values.Hostname != "living-room" {
		t.Fatalf("stale update changed hostname: %q", got.Values.Hostname)
	}

	invalid := []string{"", "capsula.local", "-capsula", "capsula-", "capsula_2", "two words", strings.Repeat("a", 64)}
	for _, bad := range invalid {
		patch.Hostname = bad
		if _, err := store.Update(next.Revision, patch, []string{"hostname"}); err == nil {
			t.Fatalf("invalid hostname %q was accepted", bad)
		}
		got := store.Get()
		if got.Values.Hostname != "living-room" || got.Revision != next.Revision {
			t.Fatalf("rejected hostname %q changed stored state: %+v", bad, got)
		}
	}

	reopened, err := OpenUser(path, testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get(); got.Values.Hostname != "living-room" || got.Revision != next.Revision {
		t.Fatalf("hostname persistence failed: %+v", got)
	}
}

func TestLegacyStoreGetsDefaultHostname(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"revision":7,"values":{"master_volume":0.5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenUser(path, testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Get()
	if snap.Revision != 7 || snap.Values.MasterVolume != 0.5 {
		t.Fatalf("legacy values changed: %+v", snap)
	}
	if snap.Values.Hostname != DefaultHostname {
		t.Fatalf("legacy store hostname = %q, want %q", snap.Values.Hostname, DefaultHostname)
	}
}

func TestLegacyBluetoothPairableMigrates(t *testing.T) {
	for _, tc := range []struct {
		name           string
		legacy         string
		wantIncoming   bool
		wantDiscoveral bool
	}{
		{"pairable true keeps the pairing window", `true`, true, true},
		{"pairable false keeps paired reconnects", `false`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			doc := `{"revision":3,"values":{"master_volume":0.4,"bluetooth_pairable":` + tc.legacy + `}}`
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := OpenUser(path, testLogs(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			snap := store.Get()
			if snap.Revision != 3 || snap.Values.MasterVolume != 0.4 {
				t.Fatalf("unrelated values changed: %+v", snap)
			}
			if snap.Values.BluetoothIncomingConnections != tc.wantIncoming ||
				snap.Values.BluetoothDiscoverable != tc.wantDiscoveral {
				t.Fatalf("migrated Bluetooth = incoming:%v discoverable:%v, want incoming:%v discoverable:%v",
					snap.Values.BluetoothIncomingConnections, snap.Values.BluetoothDiscoverable,
					tc.wantIncoming, tc.wantDiscoveral)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "bluetooth_pairable") {
				t.Fatalf("obsolete key survived migration: %s", data)
			}
			if !strings.Contains(string(data), "bluetooth_incoming_connections") ||
				!strings.Contains(string(data), "bluetooth_discoverable") {
				t.Fatalf("migrated keys missing: %s", data)
			}
		})
	}
}

func TestBluetoothDependentSettingsStayCoherent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := OpenUser(path, testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := store.Get()
	if !snap.Values.BluetoothIncomingConnections || snap.Values.BluetoothDiscoverable {
		t.Fatalf("new-store Bluetooth defaults = %+v", snap.Values)
	}

	snap, err = store.Update(snap.Revision, snap.Values, []string{"bluetooth_discoverable"})
	if err != nil {
		t.Fatal(err)
	}
	snap.Values.BluetoothDiscoverable = true
	snap, err = store.Update(snap.Revision, snap.Values, []string{"bluetooth_discoverable"})
	if err != nil {
		t.Fatalf("enable discoverability: %v", err)
	}
	if !snap.Values.BluetoothIncomingConnections || !snap.Values.BluetoothDiscoverable {
		t.Fatalf("discoverability did not imply incoming: %+v", snap.Values)
	}

	snap.Values.BluetoothIncomingConnections = false
	snap, err = store.Update(snap.Revision, snap.Values, []string{"bluetooth_incoming_connections"})
	if err != nil {
		t.Fatalf("disable incoming: %v", err)
	}
	if snap.Values.BluetoothIncomingConnections || snap.Values.BluetoothDiscoverable {
		t.Fatalf("incoming off left discoverability on: %+v", snap.Values)
	}
	reopened, err := OpenUser(path, testLogs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().Values; got.BluetoothIncomingConnections || got.BluetoothDiscoverable {
		t.Fatalf("dependent state did not persist: %+v", got)
	}
}

func TestDefaultHostnameIsValid(t *testing.T) {
	if err := discovery.ValidateHostname(DefaultHostname); err != nil {
		t.Fatalf("default hostname invalid: %v", err)
	}
}

// The muted indication must not disappear on its own by default: it is the
// only visible proof that the microphone is gated.
func TestDefaultMutedIndicatorPersistsUntilUnmute(t *testing.T) {
	if got := DefaultUserSettings().MutedIndicator; got != MutedUntilUnmute {
		t.Fatalf("default muted indicator = %v, want MutedUntilUnmute", got)
	}
}

// The semantic validator must reject before any write: the revision and the
// previous value stay exactly as they were, so a rejected Update is visible to
// the caller with no partial state change.
func TestAppSettingsValidatorRejectsBeforeWrite(t *testing.T) {
	lookup := func(appID string) (*ManifestSchema, bool) {
		if appID != "org.example.assistant" {
			return nil, false
		}
		return &ManifestSchema{AppID: appID, Settings: []SchemaSetting{
			{Key: "delay", Type: "text", DefaultJSON: `"1000"`},
		}}, true
	}
	store, err := OpenApps(filepath.Join(t.TempDir(), "apps"), testLogs(t), lookup)
	if err != nil {
		t.Fatal(err)
	}
	store.SetValidator(func(appID string, values map[string]json.RawMessage) error {
		if appID != "org.example.assistant" {
			return nil
		}
		var delay string
		_ = json.Unmarshal(values["delay"], &delay)
		if delay == "bad" {
			return errRejectedDelay
		}
		return nil
	})
	snap, _, _, err := store.Update("org.example.assistant", 0, []Change{{Key: "delay", Value: "100"}})
	if err != nil {
		t.Fatalf("valid update: %v", err)
	}
	if _, _, _, err := store.Update("org.example.assistant", snap.Revision, []Change{{Key: "delay", Value: "bad"}}); err == nil {
		t.Fatal("invalid value was accepted")
	}
	after, err := store.Snapshot("org.example.assistant")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != snap.Revision {
		t.Fatalf("rejected update advanced revision %d -> %d", snap.Revision, after.Revision)
	}
	for _, v := range after.Values {
		if v.Key == "delay" && v.Value != `"100"` {
			t.Fatalf("rejected update changed the value: %s", v.Value)
		}
	}
}

var errRejectedDelay = errors.New("delay: rejected by validator")
