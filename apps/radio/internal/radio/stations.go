package radio

import (
	"strconv"
	"strings"
)

// MaxStations bounds the parsed station list. The manifest declares the same
// limit, and the settings UI renders whatever the list contains.
const MaxStations = 99

// Station is one configured internet-radio stream.
type Station struct {
	Name string
	URL  string
}

// ParseStations parses the stations setting: one "Name | URL" per line, blank
// lines and # comments ignored, and a bare http(s) URL accepted with the URL
// as its own name. Invalid lines are reported to logf and skipped rather than
// failing the whole list, matching the documented setting contract.
func ParseStations(text string, logf func(string, ...any)) []Station {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	found := make([]Station, 0, 8)
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := ""
		url := line
		if split := strings.IndexByte(line, '|'); split > 0 {
			candidate := strings.TrimSpace(line[split+1:])
			if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
				name = strings.TrimSpace(line[:split])
				url = candidate
			}
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			logf("stations: line is neither 'Name | URL' nor an http(s) URL: %s", line)
			continue
		}
		if name == "" {
			name = url
		}
		found = append(found, Station{Name: name, URL: url})
		if len(found) >= MaxStations {
			logf("stations: list truncated at %d entries", MaxStations)
			break
		}
	}
	return found
}

// SelectText resolves a launch argument to a 1-based station number. It
// accepts a 1-based number, an exact case-insensitive name, or the URL.
func SelectText(stations []Station, text string) (int, bool) {
	wanted := strings.TrimSpace(text)
	if wanted == "" || len(stations) == 0 {
		return 0, false
	}
	if number, err := strconv.Atoi(wanted); err == nil && number >= 1 && number <= len(stations) {
		return number, true
	}
	lowered := strings.ToLower(wanted)
	for i, station := range stations {
		if strings.ToLower(station.Name) == lowered {
			return i + 1, true
		}
	}
	for i, station := range stations {
		if station.URL == wanted {
			return i + 1, true
		}
	}
	return 0, false
}

// StepNumber wraps a 1-based selection by delta inside the list.
func StepNumber(number, delta, count int) int {
	if count <= 0 {
		return 1
	}
	if number < 1 || number > count {
		number = 1
	}
	number += delta
	for number < 1 {
		number += count
	}
	for number > count {
		number -= count
	}
	return number
}

// MissingStationReason explains why the stored selection has no station, and
// is empty when the selection is valid.
func MissingStationReason(number, count int) string {
	if count == 0 {
		return "no stations configured"
	}
	if number >= 1 && number <= count {
		return ""
	}
	return "station " + strconv.Itoa(number) + " is not in the " + strconv.Itoa(count) + " configured stations"
}