package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/settings"
)

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
			for _, boot := range []string{"first-boot", "restart"} {
				t.Run(boot, func(t *testing.T) {
					d := startEmptyDaemon(t, base)
					waitForAppSelection(t, d, "display.segment.front", clockAppID)
					if app := d.user.Get().Values.DefaultAppByResource["light.ring.main"]; app != "" {
						t.Fatalf("restart enabled ring provider %q despite saved none choice", app)
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
				})
			}
		})
	}
}
