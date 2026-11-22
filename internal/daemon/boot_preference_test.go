package daemon

import (
	"testing"

	"capsulad/internal/manifest"
	"capsulad/internal/settings"
)

func TestBootPreference(t *testing.T) {
	m := &manifest.Manifest{ID: "app", StartOnBoot: true}
	values := settings.DefaultUserSettings()
	if !bootEnabled(m, values) {
		t.Fatal("manifest default ignored")
	}
	values.StartOnBootOverrides = map[string]bool{"app": false}
	if bootEnabled(m, values) {
		t.Fatal("disabled override ignored")
	}
	m.Background = true
	if !bootEnabled(m, values) {
		t.Fatal("background app disabled")
	}
	m.Background = false
	m.StartOnBoot = false
	values.StartOnBootOverrides["app"] = true
	if !bootEnabled(m, values) {
		t.Fatal("enabled override ignored")
	}
}
