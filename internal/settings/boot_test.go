package settings

import (
	"path/filepath"
	"testing"
)

func TestBootOverridesPersistAndAreIsolated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := OpenUser(path, testLogs(t), nil)
	if err != nil { t.Fatal(err) }
	initial := store.Get()
	initial.Values.StartOnBootOverrides = map[string]bool{"app": false, "other": true}
	saved, err := store.Update(initial.Revision, initial.Values, []string{"start_on_boot_overrides"})
	if err != nil { t.Fatal(err) }
	saved.Values.StartOnBootOverrides["app"] = true
	if store.Get().Values.StartOnBootOverrides["app"] { t.Fatal("snapshot shares preference map") }
	if _, err := store.Update(initial.Revision, initial.Values, []string{"start_on_boot_overrides"}); err == nil { t.Fatal("accepted stale revision") }
	// Full framework settings replacement cannot erase browser-owned preferences.
	if _, err := store.Update(0, DefaultUserSettings(), nil); err != nil { t.Fatal(err) }
	reopened, err := OpenUser(path, testLogs(t), nil)
	if err != nil { t.Fatal(err) }
	values := reopened.Get().Values.StartOnBootOverrides
	if enabled, ok := values["app"]; !ok || enabled || !values["other"] { t.Fatalf("preferences not preserved: %v", values) }
}
