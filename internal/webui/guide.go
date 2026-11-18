package webui

import (
	"errors"
	"net/http"

	"capsulad/internal/appui"
	"capsulad/internal/manifest"
)

// appGuide serves one application's optional user guide to the administrator's
// browser.
//
// The guide is inert Markdown delivered as JSON, never HTML: the SPA renders it
// with interpolated text and no markup, link target or network primitive. The
// route is session-authenticated like the other administrator surfaces, returns
// no package path, and is independent of the app's run state, so a stopped app
// can still explain its controls, launch and recovery steps.
func (s *Server) appGuide(w http.ResponseWriter, r *http.Request) {
	if s.Apps == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "application packages are unavailable")
		return
	}
	appID := r.PathValue("appID")
	app, ok := s.Apps.Get(appID)
	if !ok || app.Manifest == nil {
		writeJSONError(w, http.StatusNotFound, "This application is not installed.")
		return
	}

	var (
		doc    []byte
		source string
	)
	if s.Apps.IsBuiltin(appID) {
		var found bool
		doc, found = appui.BuiltinGuide(appID)
		if !found {
			s.Logs.Warnf("webui", appID, "built-in user guide is missing")
			writeJSONError(w, http.StatusNotFound, "This application does not include a user guide.")
			return
		}
		source = "builtin"
	} else {
		var err error
		doc, err = manifest.ReadGuide(app.Dir)
		if err != nil {
			if errors.Is(err, manifest.ErrGuideNotFound) {
				writeJSONError(w, http.StatusNotFound, "This application does not include a user guide.")
				return
			}
			// The reason is for diagnostics only. Never reflect the package
			// path, file name or host filesystem error to the browser.
			s.Logs.Warnf("webui", appID, "user guide unavailable: %v", err)
			writeJSONError(w, http.StatusUnprocessableEntity, "The application's user guide cannot be read.")
			return
		}
		source = "package"
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSONValue(w, http.StatusOK, map[string]any{
		"app_id":   appID,
		"title":    app.Manifest.Name,
		"source":   source,
		"format":   "markdown",
		"markdown": string(doc),
	})
}