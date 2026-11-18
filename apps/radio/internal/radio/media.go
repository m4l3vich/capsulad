package radio

import (
	"context"

	"capsulad/sdk/capsulad"
)

// ServeMediaContract registers the standard media contract on the app bus.
// The player is always present, including while stopped or in error, so a
// consumer can still discover the selected station and call Play; the Web UI's
// Now Playing panel only lists actively playing entries.
func (a *App) ServeMediaContract(ctx context.Context) {
	a.client.Handle(mediaService, "ListPlayers", func(callCtx context.Context, call *capsulad.ServerCall) {
		if err := call.SendJSON(map[string]any{"players": []any{a.mediaStatus()}}); err != nil {
			_ = call.End(13, err.Error())
			return
		}
		_ = call.End(0, "")
	})
	a.client.Handle(mediaService, "WatchPlayers", func(callCtx context.Context, call *capsulad.ServerCall) {
		a.watchPlayers(callCtx, call)
	})
	a.client.Handle(mediaService, "GetStatus", func(callCtx context.Context, call *capsulad.ServerCall) {
		var request struct {
			PlayerID string `json:"playerId"`
		}
		if err := call.RecvJSON(&request); err != nil {
			_ = call.End(3, err.Error())
			return
		}
		if request.PlayerID == "" {
			_ = call.End(3, "playerId is required")
			return
		}
		if request.PlayerID != playerID {
			_ = call.End(5, "unknown player "+request.PlayerID)
			return
		}
		if err := call.SendJSON(a.mediaStatus()); err != nil {
			_ = call.End(13, err.Error())
			return
		}
		_ = call.End(0, "")
	})
	for _, entry := range []struct {
		method string
		action string
	}{
		{"Play", "play"},
		{"Pause", "pause"},
		{"SkipNext", "next"},
		{"SkipPrevious", "previous"},
	} {
		method, action := entry.method, entry.action
		a.client.Handle(mediaService, method, func(callCtx context.Context, call *capsulad.ServerCall) {
			var request struct {
				PlayerID string `json:"playerId"`
			}
			if err := call.RecvJSON(&request); err != nil {
				_ = call.End(3, err.Error())
				return
			}
			if request.PlayerID == "" {
				_ = call.End(3, "playerId is required")
				return
			}
			if request.PlayerID != playerID {
				_ = call.End(5, "unknown player "+request.PlayerID)
				return
			}
			accepted, detail := a.Control(action)
			if err := call.SendJSON(map[string]any{"accepted": accepted, "detail": detail}); err != nil {
				_ = call.End(13, err.Error())
				return
			}
			_ = call.End(0, "")
		})
	}
}

func (a *App) watchPlayers(ctx context.Context, call *capsulad.ServerCall) {
	updates := make(chan struct{}, 1)
	a.mu.Lock()
	a.nextWatch++
	id := a.nextWatch
	a.watchers[id] = updates
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.watchers, id)
		a.mu.Unlock()
	}()

	if err := call.SendJSON(map[string]any{"snapshot": map[string]any{
		"players": []any{a.mediaStatus()},
	}}); err != nil {
		_ = call.End(13, err.Error())
		return
	}
	for {
		select {
		case <-ctx.Done():
			_ = call.End(0, "")
			return
		case <-a.ctx.Done():
			_ = call.End(0, "")
			return
		case <-updates:
			if err := call.SendJSON(map[string]any{"updated": a.mediaStatus()}); err != nil {
				_ = call.End(13, err.Error())
				return
			}
		}
	}
}

// mediaStatus renders one canonical-protobuf-JSON PlayerStatus. Empty optional
// strings are omitted and 64-bit integers are strings.
func (a *App) mediaStatus() map[string]any {
	a.mu.Lock()
	phase, revision := a.phase, a.revision
	title, artist := a.nowTitle, a.nowArtist
	station, hasStation := a.currentStationLocked()
	a.mu.Unlock()

	source := ""
	if hasStation {
		source = station.Name
	}
	actions := []any{"MEDIA_SKIP_NEXT", "MEDIA_SKIP_PREVIOUS"}
	if phase == PhasePlaying || phase == PhaseBuffering {
		actions = append([]any{"MEDIA_PAUSE"}, actions...)
	} else {
		actions = append([]any{"MEDIA_PLAY"}, actions...)
	}
	status := map[string]any{
		"playerId":         playerID,
		"playbackState":    phase.mediaState(),
		"supportedActions": actions,
		"appName":          appName,
		"sourceName":       source,
		"revision":         formatUint(revision),
	}
	mediaName := title
	if mediaName == "" {
		mediaName = source
	}
	if mediaName != "" {
		status["mediaName"] = mediaName
	}
	if title != "" {
		status["title"] = title
	}
	if artist != "" {
		status["artist"] = artist
	}
	return status
}