package webui

import (
	"encoding/json"
	"net/http"
	"strings"

	"capsulad/internal/manifest"
	"capsulad/internal/settings"
)

// appBoot is a browser-admin operation. The stored preference takes effect on
// the next daemon start; it does not start or stop a running application.
func (s *Server) appBoot(w http.ResponseWriter, r *http.Request) {
	if s.Apps == nil || s.UserSettings == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "boot preferences are unavailable")
		return
	}
	app, ok := s.Apps.Get(r.PathValue("appID"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "application not found")
		return
	}
	m := app.Manifest
	editable := !m.Background && m.Entrypoint.Runtime != manifest.RuntimeBuiltin
	snap := s.UserSettings.Get()
	if r.Method == http.MethodPut {
		if !editable {
			writeJSONError(w, http.StatusForbidden, "background and system applications must start at boot")
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
			Revision uint64 `json:"revision"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.Enabled == nil || body.Revision == 0 {
			writeJSONError(w, http.StatusBadRequest, "enabled and revision are required")
			return
		}
		if snap.Values.StartOnBootOverrides == nil {
			snap.Values.StartOnBootOverrides = make(map[string]bool)
		}
		snap.Values.StartOnBootOverrides[m.ID] = *body.Enabled
		var err error
		snap, err = s.UserSettings.Update(body.Revision, snap.Values, []string{"start_on_boot_overrides"})
		if err != nil {
			if strings.Contains(err.Error(), "stale") {
				writeJSONError(w, http.StatusConflict, "settings changed; reload and try again")
			} else {
				writeJSONError(w, http.StatusInternalServerError, "could not save boot preference")
			}
			return
		}
	}
	writeBootState(w, m, snap, editable)
}

func writeBootState(w http.ResponseWriter, m *manifest.Manifest, snap settings.UserSnapshot, editable bool) {
	enabled := m.StartOnBoot || !editable
	if override, ok := snap.Values.StartOnBootOverrides[m.ID]; ok && editable {
		enabled = override
	}
	writeJSONValue(w, http.StatusOK, map[string]any{
		"enabled": enabled,
		"editable": editable,
		"revision": snap.Revision,
	})
}
