package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/settings"
	"capsulad/internal/supervisor"
)

func TestFrameworkOwnedClockStartsWithNoProviderSelections(t *testing.T) {
	base := shortTempDir(t)
	values := settings.DefaultUserSettings()
	values.DefaultAppByResource = map[string]string{}
	data, err := json.Marshal(struct {
		Revision uint64                `json:"revision"`
		Values   settings.UserSettings `json:"values"`
	}{Revision: 1, Values: values})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	waitFor(t, 5*time.Second, "framework-owned clock run starts without a display selection", func() bool {
		runs := d.sup.List(clockAppID)
		return len(runs) == 1 && runs[0].State == supervisor.Ready
	})
	if runs := d.sup.List(ringAppID); len(runs) != 0 {
		t.Fatalf("unselected ring has runs: %+v", runs)
	}
	waitFor(t, time.Second, "unselected display remains blank", func() bool {
		frame := d.dev.(*backend.Simulator).SegmentSnapshot()
		if len(frame.Cells) == 0 || d.router.Current("display.segment.front").SelectedPresentation != "" {
			return false
		}
		for _, cell := range frame.Cells {
			if cell != 0 {
				return false
			}
		}
		return !frame.Colon
	})
}

func TestExplicitNoRingProviderSurvivesRestart(t *testing.T) {
	for _, representation := range []string{"missing-entry", "empty-value"} {
		t.Run(representation, func(t *testing.T) {
			base := shortTempDir(t)
			values := settings.DefaultUserSettings()
			values.DefaultAppByResource = map[string]string{"display.segment.front": clockAppID}
			if representation == "empty-value" {
				values.DefaultAppByResource["light.ring.main"] = ""
			}
			data, err := json.Marshal(struct {
				Revision uint64                `json:"revision"`
				Values   settings.UserSettings `json:"values"`
			}{Revision: 7, Values: values})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "settings.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, boot := range []string{"first-boot", "restart", "selection"} {
				t.Run(boot, func(t *testing.T) {
					d := startEmptyDaemon(t, base)
					waitForAppSelection(t, d, "display.segment.front", clockAppID)
					if app := d.user.Get().Values.DefaultAppByResource["light.ring.main"]; app != "" {
						t.Fatalf("restart enabled ring provider %q despite saved none choice", app)
					}
					waitFor(t, 5*time.Second, "framework-owned clock is running", func() bool {
						runs := d.sup.List(clockAppID)
						return len(runs) == 1 && runs[0].State == supervisor.Ready
					})
					if runs := d.sup.List(ringAppID); len(runs) != 0 {
						t.Fatalf("unselected ring has runs at boot: %+v", runs)
					}
					waitFor(t, time.Second, "unowned ring is physically blank", func() bool {
						frame := d.dev.(*backend.Simulator).RingSnapshot()
						if len(frame.Pixels) != 12 || d.router.Current("light.ring.main").SelectedPresentation != "" {
							return false
						}
						for _, pixel := range frame.Pixels {
							if pixel != (backend.RGB{}) {
								return false
							}
						}
						return true
					})
					if boot == "selection" {
						if err := d.writeUserSettings([]string{"default_app_by_resource"}, func(v *settings.UserSettings) {
							v.DefaultAppByResource["light.ring.main"] = ringAppID
						}); err != nil {
							t.Fatal(err)
						}
						waitFor(t, 5*time.Second, "selected ring starts after boot", func() bool {
							runs := d.sup.List(ringAppID)
							return len(runs) == 1 && runs[0].State == supervisor.Ready
						})
						if runs := d.sup.List(clockAppID); len(runs) != 1 {
							t.Fatalf("clock runs after ring selection = %+v, want one", runs)
						}
					}
				})
			}
		})
	}
}
