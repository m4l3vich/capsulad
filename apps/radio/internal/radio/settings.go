package radio

import (
	"context"
	"strconv"
	"strings"
	"time"
)

func formatInt(value int) string { return strconv.Itoa(value) }

const (
	settingsCallTimeout = 5 * time.Second
	// selectionFlushTimeout bounds the final write after audio and display are
	// released. A self-exit must not depend on the bus context staying alive,
	// but shutdown must not wait on an unresponsive configuration service
	// either.
	selectionFlushTimeout = 2 * time.Second
)

// acquirePersist serializes settings writes. The gate is a one-token channel
// rather than a mutex so shutdown can stop waiting at its bounded deadline.
func (a *App) acquirePersist(ctx context.Context) bool {
	a.persistGateOnce.Do(func() {
		a.persistGate = make(chan struct{}, 1)
		a.persistGate <- struct{}{}
	})
	select {
	case <-a.persistGate:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *App) releasePersist() {
	a.persistGate <- struct{}{}
}

// persistSelection writes the current 1-based selection through the public
// compare-and-swap configuration API. Multiple rapid next/previous presses are
// serialized and the last selection wins; a stale revision is refreshed and
// retried instead of overwriting a concurrent administrator change. A write
// only happens for an explicit, not-yet-durable selection and only after a
// settings snapshot has supplied a nonzero revision.
func (a *App) persistSelection() {
	if !a.acquirePersist(a.ctx) {
		return
	}
	defer a.releasePersist()
	a.persistSelectionLocked(a.ctx)
}

// flushSelection is the shutdown path for an accepted selection. Close calls
// it after stopping audio and releasing the display, with a fresh bounded
// context: the app context has already been cancelled, and the public launch
// API must restore the newest selected station on the next run.
func (a *App) flushSelection(timeout time.Duration) {
	if timeout <= 0 {
		timeout = selectionFlushTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !a.acquirePersist(ctx) {
		return
	}
	defer a.releasePersist()
	a.persistSelectionLocked(ctx)
}

func (a *App) persistSelectionLocked(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		a.mu.Lock()
		target := a.stationNumber
		revision := a.settingsRevision
		already := a.persistedStation
		if a.persistSampledFn != nil {
			// Tests use this while a.mu is held to prove the writer never
			// exposes a gap between sampling the selection and arming the
			// uncertain write, where a step or a same-revision echo could
			// clear selectionDirty behind an obsolete CAS.
			a.persistSampledFn()
		}
		if !a.selectionDirty || target < 1 {
			a.mu.Unlock()
			return
		}
		if target == already && a.unsettledRevision == 0 {
			// The stored value already matches the accepted selection; a
			// snapshot (or an earlier acknowledged write) made it durable.
			a.selectionDirty = false
			a.mu.Unlock()
			return
		}
		if revision == 0 {
			// The daemon treats expected revision zero as an unconditional
			// write. Before the first snapshot initializes a revision, that
			// could overwrite a saved station the app has not seen yet.
			a.mu.Unlock()
			return
		}
		a.pendingStations[target]++
		// The write can still commit after its RPC is cancelled or times out.
		// Until an acknowledged response or a snapshot past its expected
		// revision settles the outcome, no cached station may justify
		// skipping the shutdown flush: an older write may have stored another
		// station without the app seeing the response. a.mu stays held from
		// the samples above through this registration: a step or an echo in
		// between could otherwise clear selectionDirty while this obsolete
		// CAS is still about to be sent.
		a.unsettledRevision = revision
		a.persistedStation = 0
		a.mu.Unlock()
		writtenRevision, err := a.updateStation(ctx, target, revision)
		a.mu.Lock()
		if a.pendingStations[target] > 1 {
			a.pendingStations[target]--
		} else {
			delete(a.pendingStations, target)
		}
		if err == nil {
			// The response acknowledges this write, so no older attempt can
			// still commit behind it.
			a.unsettledRevision = 0
			a.recordWriteLocked(writtenRevision, target)
			// A snapshot applied while the write was in flight may be newer
			// than the write response. Only claim durability when no newer
			// settings state superseded this selection; otherwise the dirty
			// flag must survive for the next write attempt.
			if writtenRevision > 0 && writtenRevision >= a.settingsRevision {
				a.persistedStation = target
				if a.stationNumber == target {
					a.selectionDirty = false
				}
			}
		}
		a.mu.Unlock()
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			// The run is leaving; Close's bounded flush owns the final write
			// and must not log a cancellation caused by its own shutdown.
			return
		}
		if isStaleRevision(err) {
			a.refreshRevision(ctx)
			continue
		}
		a.logf("station setting not persisted: %v", err)
		return
	}
	a.logf("station setting not persisted: settings revision kept changing")
}

func (a *App) updateStation(ctx context.Context, station int, revision uint64) (uint64, error) {
	if a.updateStationFn != nil {
		return a.updateStationFn(ctx, station, revision)
	}
	if a.client == nil {
		return 0, nil
	}
	request := map[string]any{
		"appId":            a.env.AppID,
		"expectedRevision": formatUint(revision),
		"changes": []any{map[string]any{
			"key":       "station",
			"valueJson": formatInt(station),
		}},
	}
	var response map[string]any
	callCtx, cancel := context.WithTimeout(ctx, settingsCallTimeout)
	defer cancel()
	if err := a.client.CallJSON(callCtx, "", configService, "Update", request, &response, settingsCallTimeout); err != nil {
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

func (a *App) refreshRevision(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, settingsCallTimeout)
	defer cancel()
	if a.readRevisionFn != nil {
		if updated, ok := a.readRevisionFn(ctx); ok {
			a.bumpSettingsRevision(updated)
		}
		return
	}
	if a.client == nil {
		return
	}
	request := map[string]any{"appId": a.env.AppID}
	var response map[string]any
	if err := a.client.CallJSON(ctx, "", configService, "Get", request, &response, settingsCallTimeout); err != nil {
		if parent.Err() == nil {
			a.logf("settings revision refresh failed: %v", err)
		}
		return
	}
	if updated, ok := revisionValue(response["revision"]); ok {
		a.bumpSettingsRevision(updated)
	}
}

// bumpSettingsRevision advances the compare-and-swap revision never backwards.
func (a *App) bumpSettingsRevision(updated uint64) {
	a.mu.Lock()
	if updated > a.settingsRevision {
		a.settingsRevision = updated
	}
	a.mu.Unlock()
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
