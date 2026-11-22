package install

import (
	"fmt"
	"strings"
)

// App is one first-party package the installer can offer.
type App struct {
	ID          string
	Name        string
	Description string
}

// Catalogue is the complete list of offered apps, in menu order. Hardware Test
// is deliberately absent: it is a factory diagnostic, not a consumer app.
var Catalogue = []App{
	{ID: "org.capsulad.airplay", Name: "AirPlay 2", Description: "Receive audio from Apple devices"},
	{ID: "org.capsulad.cadence", Name: "Cadence", Description: "Connect to a self-hosted Cadence assistant"},
	{ID: "org.capsulad.dumbassistant", Name: "Dumb Assistant", Description: "A simple, private voice assistant"},
	{ID: "org.capsulad.homeassistant", Name: "Home Assistant", Description: "Bridge the speaker into Home Assistant"},
	{ID: "org.capsulad.marusia", Name: "Marusia", Description: "VK Marusia voice assistant"},
	{ID: "org.capsulad.opencapsula", Name: "OpenCapsula", Description: "Connect to a self-hosted OpenCapsula server"},
	{ID: "org.capsulad.pa", Name: "PA Speaker", Description: "Stream raw audio to the speaker"},
	{ID: "org.capsulad.radio", Name: "Internet Radio", Description: "Play internet radio stations"},
	{ID: "org.capsulad.simon", Name: "Simon Says", Description: "A small game for the speaker"},
	{ID: "org.capsulad.soundboard", Name: "Soundboard", Description: "Play sound effects from the panel"},
}

// AppByID finds a catalogue entry.
func AppByID(id string) (App, bool) {
	for _, a := range Catalogue {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// SelectedApps returns the chosen IDs in catalogue order and rejects an
// unknown ID instead of silently dropping it.
func SelectedApps(selected map[string]bool) ([]App, error) {
	var out []App
	for id := range selected {
		if !selected[id] {
			continue
		}
		if _, ok := AppByID(id); !ok {
			return nil, fmt.Errorf("unknown app %q", id)
		}
	}
	for _, a := range Catalogue {
		if selected[a.ID] {
			out = append(out, a)
		}
	}
	return out, nil
}

// AppResult is one app's installation outcome. Status is "installed" or
// "failed"; a failure always carries a human-readable detail.
type AppResult struct {
	ID      string
	Name    string
	Version string
	Status  string
	Detail  string
}

// AppListIDs parses `app list` output: one line per app, app ID first.
func AppListIDs(out string) []string {
	var ids []string
	for _, line := range splitLines(out) {
		fields := fields(line)
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		ids = append(ids, fields[0])
	}
	return ids
}

// HasAppID reports whether `app list` output names id as its first field.
func HasAppID(out, id string) bool {
	for _, got := range AppListIDs(out) {
		if got == id {
			return true
		}
	}
	return false
}

// RunsShowActive reports whether `app runs ID` output has an active run. The
// line format is `<appID> <runID> <state> pid=<pid> <detail>`; a stopped or
// exited record is not active. It is used to name apps that are not running
// when free space is short.
func RunsShowActive(out, id string) bool {
	for _, line := range splitLines(out) {
		f := fields(line)
		if len(f) < 3 || f[0] != id {
			continue
		}
		state := strings.ToLower(f[2])
		// The CLI reports APP_READY / APP_STOPPED / ...; accept both the
		// prefixed vocabulary and a bare state word.
		state = strings.TrimPrefix(state, "app_")
		switch state {
		case "ready", "running", "starting", "active":
			return true
		}
	}
	return false
}

func splitLines(s string) []string { return strings.Split(s, "\n") }

func fields(s string) []string { return strings.Fields(s) }
