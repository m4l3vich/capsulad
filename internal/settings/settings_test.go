package settings

import (
	"encoding/json"
	"path/filepath"
	"testing"

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

// The muted indication must not disappear on its own by default: it is the
// only visible proof that the microphone is gated.
func TestDefaultMutedIndicatorPersistsUntilUnmute(t *testing.T) {
	if got := DefaultUserSettings().MutedIndicator; got != MutedUntilUnmute {
		t.Fatalf("default muted indicator = %v, want MutedUntilUnmute", got)
	}
}
