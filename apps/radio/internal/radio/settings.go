package radio

import (
	"context"
	"strconv"
	"strings"
	"time"
)

func formatInt(value int) string { return strconv.Itoa(value) }

const settingsCallTimeout = 5 * time.Second

// persistSelection writes the current 1-based selection through the public
// compare-and-swap configuration API. Multiple rapid next/previous presses are
// serialized and the last selection wins; a stale revision is refreshed and
// retried instead of overwriting a concurrent administrator change.
func (a *App) persistSelection() {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()

	for attempt := 0; attempt < 3; attempt++ {
		a.mu.Lock()
		target := a.stationNumber
		revision := a.settingsRevision
		already := a.persistedStation
		a.mu.Unlock()
		if target < 1 || target == already {
			return
		}
		a.mu.Lock()
		a.pendingStations[target]++
		a.mu.Unlock()
		writtenRevision, err := a.updateStation(target, revision)
		a.mu.Lock()
		if a.pendingStations[target] > 1 {
			a.pendingStations[target]--
		} else {
			delete(a.pendingStations, target)
		}
		if err == nil {
			a.recordWriteLocked(writtenRevision, target)
			if a.persistedStation == already {
				a.persistedStation = target
			}
		}
		a.mu.Unlock()
		if err == nil {
			return
		}
		if isStaleRevision(err) {
			a.refreshRevision()
			continue
		}
		a.logf("station setting not persisted: %v", err)
		return
	}
	a.logf("station setting not persisted: settings revision kept changing")
}

func (a *App) updateStation(station int, revision uint64) (uint64, error) {
	request := map[string]any{
		"appId":            a.env.AppID,
		"expectedRevision": formatUint(revision),
		"changes": []any{map[string]any{
			"key":       "station",
			"valueJson": formatInt(station),
		}},
	}
	var response map[string]any
	ctx, cancel := context.WithTimeout(a.ctx, settingsCallTimeout)
	defer cancel()
	if err := a.client.CallJSON(ctx, "", configService, "Update", request, &response, settingsCallTimeout); err != nil {
		return 0, err
	}
	updated, ok := revisionValue(response["revision"])
	if !ok {
		return 0, nil
	}
	a.mu.Lock()
	if updated > a.settingsRevision {
		a.settingsRevision = updated
	}
	a.mu.Unlock()
	return updated, nil
}

func (a *App) recordWriteLocked(revision uint64, station int) {
	if revision == 0 {
		return
	}
	a.recentWrites[revision] = station
	for seen := range a.recentWrites {
		if seen+32 < revision {
			delete(a.recentWrites, seen)
		}
	}
}

// stationEchoLocked reports whether a settings snapshot merely echoes a write
// this app made for an older selection. While any newer selection is current,
// applying that echo would revert the user's latest next/previous press.
func (a *App) stationEchoLocked(station int, revision uint64) bool {
	if a.stationNumber == station {
		return false
	}
	if a.pendingStations[station] > 0 {
		return true
	}
	if revision == 0 {
		return false
	}
	written, ok := a.recentWrites[revision]
	return ok && written == station
}

func (a *App) refreshRevision() {
	request := map[string]any{"appId": a.env.AppID}
	var response map[string]any
	ctx, cancel := context.WithTimeout(a.ctx, settingsCallTimeout)
	defer cancel()
	if err := a.client.CallJSON(ctx, "", configService, "Get", request, &response, settingsCallTimeout); err != nil {
		a.logf("settings revision refresh failed: %v", err)
		return
	}
	if updated, ok := revisionValue(response["revision"]); ok {
		a.mu.Lock()
		if updated > a.settingsRevision {
			a.settingsRevision = updated
		}
		a.mu.Unlock()
	}
}

func isStaleRevision(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "stale") || strings.Contains(message, "aborted")
}

// revisionValue reads a 64-bit revision encoded either as canonical protobuf
// JSON (a string) or as a JSON number.
func revisionValue(raw any) (uint64, bool) {
	switch value := raw.(type) {
	case string:
		parsed, err := strconv.ParseUint(value, 10, 64)
		return parsed, err == nil
	case float64:
		if value < 0 {
			return 0, false
		}
		return uint64(value), true
	case uint64:
		return value, true
	case int:
		if value < 0 {
			return 0, false
		}
		return uint64(value), true
	default:
		return 0, false
	}
}