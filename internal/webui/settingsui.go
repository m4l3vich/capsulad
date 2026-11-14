package webui

import (
	"net/http"

	"capsulad/internal/appui"
	"capsulad/internal/manifest"
)

// appSettingsUI serves one application's manifest-declared settings document.
//
// The route is session-authenticated because the administrator's browser
// fetches it, but the returned document is untrusted: the Web UI renders it in
// a sandboxed opaque-origin iframe and the document itself receives no
// cookie, WebSocket, host DOM or API access. The response CSP forbids every
// network and navigation primitive, and the injected wrapper adds the same
// policy as a meta element plus the local system kit and bridge bootstrap.
func (s *Server) appSettingsUI(w http.ResponseWriter, r *http.Request) {
	if s.Apps == nil {
		http.NotFound(w, r)
		return
	}
	appID := r.PathValue("appID")
	app, ok := s.Apps.Get(appID)
	if !ok || app.Manifest.SettingsUI == nil {
		http.NotFound(w, r)
		return
	}
	var (
		doc []byte
		err error
	)
	if s.Apps.IsBuiltin(appID) {
		doc, err = appui.BuiltinHTML(appID, app.Manifest.SettingsUI.HTML)
	} else {
		doc, err = manifest.ReadSettingsUIAsset(app.Dir, app.Manifest.SettingsUI.HTML)
	}
	if err != nil {
		// Never reflect a host path or package layout to the browser.
		s.Logs.Warnf("webui", appID, "settings UI unavailable: %v", err)
		http.NotFound(w, r)
		return
	}

	body := appui.WrapDocument(doc)
	// The document is meant to be framed by the admin SPA; its own CSP
	// (sandbox allow-scripts, no network/frames/forms) is the boundary. The
	// global DENY header would otherwise block the legitimate frame.
	w.Header().Del("X-Frame-Options")
	// The frame is fully isolated: never mint or refresh a host cookie in its
	// response, even though the surrounding handler may have prepared one.
	w.Header().Del("Set-Cookie")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", appui.HeaderContentSecurityPolicy())
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Vary", "Cookie")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
