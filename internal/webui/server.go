// Package webui serves the HTTPS browser interface.
//
// The listener uses a self-signed certificate. Authentication is an Argon2id
// admin password with server-side sessions, or a static opaque API key for
// automation. The SPA and its realtime socket are a thin client of the daemon's
// local gRPC API over the Unix socket; there is no second control plane and no
// network gRPC listener. The same listener hosts the MCP automation endpoint
// and the PA Speaker's one app-specific play route; both drive the daemon's
// existing services rather than a private transport.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/assets"
	"capsulad/internal/bluetooth"
	"capsulad/internal/discovery"
	"capsulad/internal/logbus"
	"capsulad/internal/registry"
	"capsulad/internal/settings"
	"capsulad/internal/uichanges"
	"capsulad/internal/version"
	"capsulad/internal/wakeword"
	"capsulad/internal/websecurity"
	"capsulad/internal/wifi"
)

// distFS is the independently built Vue SPA (web/). It contains only static
// assets; all data arrives over /ws or the small session API.
//
//go:embed all:dist
var distFS embed.FS

const mediaService = "capsulad.contracts.media.v1.MediaPlayerService"

type Server struct {
	Addr     string
	TLS      *tls.Config
	Conn     *grpc.ClientConn
	Logs     *logbus.Bus
	Platform string
	Auth     *websecurity.Store
	// Discovery applies the saved mDNS host name. Optional: a daemon without a
	// responder still renders and persists the setting.
	Discovery discovery.Manager
	// Changes signals topic mutations that have no framework watch stream.
	// Optional so focused tests can omit it; live topics stay snapshot-only.
	Changes *uichanges.Hub
	// Apps resolves application package assets. Optional: without it the
	// settings-UI endpoint and declaration are unavailable.
	Apps *registry.Registry
	// UserSettings stores administrator boot preferences.
	UserSettings *settings.UserStore
	// Assets is the shared read-only asset store. Optional: without it the
	// browser asset-management routes report unavailability.
	Assets *assets.Catalog
	// Wifi is the shared station/provisioning controller. Optional: a daemon
	// without Wi-Fi still serves a snapshot explaining unavailability.
	Wifi wifi.Controller
	// Bluetooth is the daemon's adapter manager. It is used only for the
	// administrator-only advertised-name change, which deliberately has no
	// framework/app RPC; the other adapter commands stay on SystemService.
	// Optional so a focused test can omit it and get an unavailable error.
	Bluetooth bluetooth.Manager
	// Wakeword reads the daemon-owned bounded detection history. Optional so
	// focused tests render an empty recent-detections list.
	Wakeword wakewordSource
	limiter  *loginLimiter

	// paGate serializes the one PA HTTP relay. The PA app serves one Play call
	// at a time and its script host has a small inbound frame budget, so a
	// second upload must wait at this gate instead of being forwarded into a
	// busy app. paGateOnce makes a directly constructed test Server safe.
	paGateOnce sync.Once
	paGate     chan struct{}

	connMu sync.Mutex
	conns  map[string]map[*wsConn]struct{}

	// mcpMu guards the live MCP sessions. A session is created by initialize,
	// carries the connection's single wakeword-training slot, and is dropped by
	// an explicit DELETE, by idle expiry, or on listener shutdown.
	mcpMu       sync.Mutex
	mcpSessions map[string]*mcpSession
}

// wakewordSource is the narrow in-process view of daemon wakeword state the
// authenticated browser surface exposes. It is not reachable from the app
// protocol: detection history is a local administrator read.
type wakewordSource interface {
	RecentDetections(limit int) []wakeword.Detection
	// DismissDetection drops one stored detection. A false result means the ID
	// is unknown (already dismissed or never seen).
	DismissDetection(id string) bool
}

type authInfo struct {
	OK         bool
	ViaSession bool
	ViaAPIKey  bool
	Token      string
	// Scopes is the grant of the presented API key. A browser session is the
	// administrator and does not need a scope list.
	Scopes []string
}

type authContextKey struct{}
type csrfContextKey struct{}

func New(addr string, tlsCfg *tls.Config, conn *grpc.ClientConn, logs *logbus.Bus, platform string, auth *websecurity.Store, disc discovery.Manager, changes *uichanges.Hub) (*Server, error) {
	if _, err := fs.Stat(distFS, "dist/index.html"); err != nil {
		return nil, fmt.Errorf("webui: missing embedded SPA build: %w", err)
	}
	return &Server{
		Addr: addr, TLS: tlsCfg, Conn: conn, Logs: logs, Platform: platform, Auth: auth,
		Discovery: disc, Changes: changes, limiter: newLoginLimiter(), conns: map[string]map[*wsConn]struct{}{},
	}, nil
}

// Handler builds the routing table. Static SPA files are public so the login
// view can load; every data route authenticates explicitly.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /api/session", s.apiSession)
	mux.HandleFunc("POST /api/login", s.loginSubmit)
	mux.HandleFunc("POST /api/logout", s.sessionOnly(s.logout))
	mux.HandleFunc("POST /api/password", s.sessionOnly(s.changePassword))
	mux.HandleFunc("GET /api/status", s.anyAuth(s.apiStatus))
	mux.HandleFunc("GET /api/cert.pem", s.anyAuth(s.certPEM))
	mux.HandleFunc("GET /api/apps/{appID}/settings-ui", s.sessionOnly(s.appSettingsUI))
	mux.HandleFunc("GET /api/apps/{appID}/guide", s.sessionOnly(s.appGuide))
	mux.HandleFunc("GET /api/apps/{appID}/boot", s.sessionOnly(s.appBoot))
	mux.HandleFunc("PUT /api/apps/{appID}/boot", s.sessionOnly(s.appBoot))
	// The PA Speaker route is the one app-specific HTTP operation. It is not a
	// generic proxy: the handler names org.capsulad.pa and relays its Play RPC.
	// The methodless registration answers 405 for anything but POST instead of
	// letting the SPA fallback serve index.html.
	mux.HandleFunc("POST /api/apps/org.capsulad.pa/play", s.operatorOnly(s.playPASpeaker))
	mux.HandleFunc("/api/apps/org.capsulad.pa/play", s.paMethodNotAllowed)
	mux.HandleFunc("POST /api/apps", s.sessionOnly(s.installApp))
	mux.HandleFunc("DELETE /api/apps/{appID}", s.sessionOnly(s.removeApp))
	mux.HandleFunc("GET /api/assets", s.sessionOnly(s.apiAssets))
	mux.HandleFunc("POST /api/assets", s.sessionOnly(s.uploadAsset))
	mux.HandleFunc("POST /api/assets/folder", s.sessionOnly(s.createAssetFolder))
	mux.HandleFunc("GET /api/assets/file", s.sessionOnly(s.downloadAsset))
	mux.HandleFunc("DELETE /api/assets", s.sessionOnly(s.deleteAsset))
	mux.HandleFunc("GET /ws", s.sessionOnly(s.serveWS))
	// The MCP frontend is an API-key surface, not a browser session: it does
	// its own authentication and never accepts a session cookie or CSRF token.
	mux.HandleFunc("/mcp", s.serveMCP)
	mux.Handle("/", s.static())
	return s.middleware(mux)
}

func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.Handler(),
		TLSConfig:         s.TLS,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.Logs.Infof("webui", "", "HTTPS listening on %s (self-signed certificate, password/API-key auth; realtime WebSocket)", ln.Addr())
	// A credential edit publishes the access topic; drop MCP sessions whose key
	// was revoked so their staged work does not outlive the credential.
	go s.reconcileMCPKeysOnAccess(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeTLS(ln, "", "") }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		s.closeAllConns()
		s.closeAllMCPSessions()
		return nil
	case err := <-errCh:
		return err
	}
}

// --- middleware ------------------------------------------------------------

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; frame-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		token := s.ensureCSRF(w, r)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfContextKey{}, token)))
	})
}

// sessionOnly wraps a handler that requires a browser session. API keys are
// deliberately not accepted: a browser mutation proves the session's CSRF and
// origin, and an operator key uses operatorOnly for the one machine-facing
// mutation route.
func (s *Server) sessionOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := s.authenticate(r)
		if !auth.OK || !auth.ViaSession {
			writeUnauthorized(w, r)
			return
		}
		// Every browser mutation proves same-origin intent and the session's
		// CSRF token, not just POST. Only form-encoded bodies are parsed here:
		// a raw body (an asset upload) is left unread for its handler, and its
		// CSRF proof arrives in the X-Capsulad-CSRF header.
		if isMutatingMethod(r.Method) {
			if isFormBody(r) {
				if err := r.ParseForm(); err != nil {
					writeJSONError(w, http.StatusBadRequest, "invalid request")
					return
				}
			}
			if !s.checkOrigin(r) {
				writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
			if !s.checkCSRF(r) {
				writeJSONError(w, http.StatusForbidden, "invalid CSRF token")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, auth)))
	}
}

// operatorOnly authorizes one machine-facing mutation: an operator-scoped API
// key (the automation credential the MCP frontend uses) or an administrator
// browser session with the usual same-origin and CSRF proof. It is deliberately
// narrower than anyAuth, which accepts a status key, and unlike sessionOnly it
// accepts API keys so a headless caller can drive the one app-specific route.
func (s *Server) operatorOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := s.authenticate(r)
		switch {
		case auth.ViaAPIKey:
			if !websecurity.ScopesAllow(auth.Scopes, websecurity.ScopeOperator) {
				writeJSONError(w, http.StatusForbidden, "an operator-scoped API key is required")
				return
			}
			// An API key is an explicit credential, not an ambient browser
			// cookie, so there is no CSRF token. Do not hand an automation
			// client the CSRF cookie the middleware minted for the SPA.
			w.Header().Del("Set-Cookie")
			// A concrete cross-origin request is still refused, matching the
			// MCP endpoint.
			if !s.checkOrigin(r) {
				writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		case auth.ViaSession:
			if !s.checkOrigin(r) {
				writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
			if !s.checkCSRF(r) {
				writeJSONError(w, http.StatusForbidden, "invalid CSRF token")
				return
			}
		default:
			writeUnauthorized(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, auth)))
	}
}

// anyAuth accepts a session or a static API key. Only the read-only status and
// public certificate routes use it.
func (s *Server) anyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := s.authenticate(r)
		if !auth.OK {
			writeUnauthorized(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, auth)))
	}
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// isFormBody reports whether ParseForm would consume the body. Requests with
// any other content type (JSON, raw upload bytes) keep their body intact.
func isFormBody(r *http.Request) bool {
	switch mediaTypeOf(r) {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		return true
	}
	return false
}

func mediaTypeOf(r *http.Request) string {
	contentType := r.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

func (s *Server) authenticate(r *http.Request) authInfo {
	if token := apiKeyFromRequest(r); token != "" {
		_, scopes, ok := s.Auth.APIKeyScopes(token)
		if ok {
			return authInfo{OK: true, ViaAPIKey: true, Scopes: scopes}
		}
		return authInfo{}
	}
	if cookie, err := r.Cookie(websecurity.SessionCookieName()); err == nil {
		if s.Auth.ValidateSession(cookie.Value) {
			return authInfo{OK: true, ViaSession: true, Token: cookie.Value}
		}
	}
	return authInfo{}
}

func apiKeyFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

func authFrom(ctx context.Context) authInfo {
	a, _ := ctx.Value(authContextKey{}).(authInfo)
	return a
}

func writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
}

func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	token := csrfFrom(r.Cookies())
	if token == "" {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return ""
		}
		token = hex.EncodeToString(b[:])
		http.SetCookie(w, &http.Cookie{
			Name: "capsulad_csrf", Value: token, Path: "/",
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		})
	}
	return token
}

func csrfFrom(cookies []*http.Cookie) string {
	for _, c := range cookies {
		if c.Name == "capsulad_csrf" {
			return c.Value
		}
	}
	return ""
}

func (s *Server) checkCSRF(r *http.Request) bool {
	want := csrfFrom(r.Cookies())
	got := r.Header.Get("X-Capsulad-CSRF")
	if got == "" {
		got = r.PostFormValue("csrf")
	}
	return want != "" && got != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	// Browsers report an opaque "null" origin for pages loaded through a
	// certificate-error bypass (the common self-signed first-run case) and for
	// sandboxed documents. The CSRF token still authenticates the request;
	// only a concrete mismatched origin is rejected.
	if origin == "" || origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// --- session API -----------------------------------------------------------

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	auth := s.authenticate(r)
	csrf := csrfFrom(r.Cookies())
	if csrf == "" {
		csrf, _ = r.Context().Value(csrfContextKey{}).(string)
	}
	resp := map[string]any{
		"authenticated":  auth.ViaSession,
		"via_api_key":    auth.ViaAPIKey,
		"csrf":           csrf,
		"daemon_version": version.Version,
		"platform":       s.Platform,
		"user":           "",
	}
	if auth.ViaSession {
		resp["user"] = "admin"
	}
	writeJSONValue(w, http.StatusOK, resp)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !s.checkOrigin(r) || !s.checkCSRF(r) {
		writeJSONError(w, http.StatusForbidden, "invalid login request")
		return
	}
	var body struct {
		Password string `json:"password"`
		Next     string `json:"next"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && r.PostFormValue("password") != "" {
		body.Password = r.PostFormValue("password")
		body.Next = r.PostFormValue("next")
	}
	next := sanitizeNext(body.Next)
	if !s.Auth.PasswordSet() {
		writeJSONValue(w, http.StatusServiceUnavailable, map[string]any{
			"error": "administrator password is not initialized; run `capsulad-cli auth init` on the device",
			"next":  next,
		})
		return
	}
	host := remoteHost(r)
	if !s.limiter.Allow(host) {
		writeJSONError(w, http.StatusTooManyRequests, "too many failed attempts; wait and try again")
		return
	}
	if !s.Auth.VerifyPassword(body.Password) {
		s.limiter.Fail(host)
		s.Logs.Warnf("webui", "", "failed login from %s", host)
		writeJSONError(w, http.StatusUnauthorized, "incorrect password")
		return
	}
	s.limiter.Success(host)
	token, expiry, err := s.Auth.NewSession()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: websecurity.SessionCookieName(), Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: expiry,
	})
	s.Logs.Infof("webui", "", "admin login from %s", host)
	writeJSONValue(w, http.StatusOK, map[string]any{"ok": true, "next": next})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	auth := authFrom(r.Context())
	if auth.ViaSession {
		s.Auth.RevokeSession(auth.Token)
		s.closeSessionConns(auth.Token)
	}
	http.SetCookie(w, &http.Cookie{
		Name: websecurity.SessionCookieName(), Value: "", Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSONValue(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !s.Auth.VerifyPassword(body.Current) {
		writeJSONError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	if len(body.Next) < 8 {
		writeJSONError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if body.Next != body.Confirm {
		writeJSONError(w, http.StatusBadRequest, "new passwords do not match")
		return
	}
	if err := s.Auth.SetPassword(body.Next); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// SetPassword invalidates every session. Reissue one for the browser that
	// just proved knowledge of both passwords, and drop every live socket:
	// their cookies no longer validate.
	s.closeAllConns()
	token, expiry, err := s.Auth.NewSession()
	if err == nil {
		http.SetCookie(w, &http.Cookie{
			Name: websecurity.SessionCookieName(), Value: token, Path: "/",
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: expiry,
		})
	}
	writeJSONValue(w, http.StatusOK, map[string]any{"ok": true, "message": "admin password changed; other sessions were revoked"})
}

// --- automation API --------------------------------------------------------

// apiStatus is the small authenticated JSON surface for API-key automation.
func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	info, err := frameworkv1.NewDeviceServiceClient(s.Conn).GetInfo(ctx, &frameworkv1.GetInfoRequest{})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	resources, _ := frameworkv1.NewDeviceServiceClient(s.Conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	runs, _ := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{})
	status, _ := frameworkv1.NewSystemServiceClient(s.Conn).GetStatus(ctx, &frameworkv1.GetStatusRequest{})
	writeJSONValue(w, http.StatusOK, map[string]any{
		"device": info, "resources": resources.GetResources(),
		"runs": runs.GetRuns(), "status": status,
	})
}

// certPEM serves the public server certificate for TLS clients to trust.
func (s *Server) certPEM(w http.ResponseWriter, r *http.Request) {
	pem, err := os.ReadFile(s.Auth.CertPath())
	if err != nil {
		http.Error(w, "certificate unavailable: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="capsulad-server.crt"`)
	_, _ = w.Write(pem)
}

// --- static SPA ------------------------------------------------------------

func (s *Server) static() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: embedded SPA: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if info, err := fs.Stat(sub, path); err == nil && !info.IsDir() {
			if strings.HasPrefix(path, "assets/") {
				// Vite fingerprints asset names; they never change.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		// Unknown paths are SPA routes: serve the shell so a deep link works.
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "SPA unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

// --- shared helpers --------------------------------------------------------

func sanitizeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func (s *Server) applyHostname(ctx context.Context, hostname string) (msg, warn string) {
	if s.Discovery == nil {
		return "settings saved", ""
	}
	if err := s.Discovery.Apply(ctx, hostname); err != nil {
		return "settings saved", "hostname saved, but advertising failed: " + err.Error()
	}
	if status := s.Discovery.Status(); status.Advertised != "" {
		return "settings saved; advertising as " + status.Advertised, ""
	}
	return "settings saved", ""
}

func (s *Server) advertisedURL(fqdn string) string {
	if fqdn == "" {
		return ""
	}
	port := "8443"
	if _, p, err := net.SplitHostPort(s.Addr); err == nil && p != "" {
		port = p
	}
	return "https://" + fqdn + ":" + port
}

func (s *Server) routedCall(ctx context.Context, appID, service, rpc string, request []byte) ([][]byte, string, *frameworkv1.AppCallEnd, error) {
	stream, err := frameworkv1.NewAppBusServiceClient(s.Conn).Call(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{TargetAppId: appID, GrpcService: service, RpcName: rpc, TimeoutMs: 5000},
	}}); err != nil {
		return nil, "", nil, err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{RequestMessage: request}}); err != nil {
		return nil, "", nil, err
	}
	_ = stream.CloseSend()
	var out [][]byte
	var end *frameworkv1.AppCallEnd
	callID := ""
	for {
		resp, err := stream.Recv()
		if err != nil {
			return out, callID, end, err
		}
		switch m := resp.GetMessage().(type) {
		case *frameworkv1.RoutedCallResponse_ResponseMessage:
			out = append(out, m.ResponseMessage)
		case *frameworkv1.RoutedCallResponse_End:
			return out, callID, m.End, nil
		}
	}
}

// routedProtoCall performs one unary app RPC over the public app bus with a
// binary protobuf request and returns the raw protobuf response. The browser
// never supplies the service or method names; callers here are fixed entries,
// and the built-in clock/ring pages use the same public contracts that
// ordinary applications discover.
func (s *Server) routedProtoCall(ctx context.Context, appID, service, rpc string, request proto.Message) ([]byte, error) {
	body, err := proto.Marshal(request)
	if err != nil {
		return nil, err
	}
	msgs, _, end, err := s.routedCall(ctx, appID, service, rpc, body)
	if err != nil {
		return nil, err
	}
	if end != nil && end.GetGrpcStatusCode() != 0 {
		detail := strings.TrimSpace(end.GetDetail())
		if detail == "" {
			detail = "application returned an error"
		}
		return nil, fmt.Errorf("%s: %s", grpcCodeName(end.GetGrpcStatusCode()), detail)
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("expected one response message, got %d", len(msgs))
	}
	return msgs[0], nil
}

func protoMarshal(m proto.Message) ([]byte, error) { return proto.Marshal(m) }

func protoUnmarshal(b []byte, m proto.Message) error { return proto.Unmarshal(b, m) }

func ctxTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSONValue(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSONValue(w, status, map[string]string{"error": message})
}

// --- session connection registry -------------------------------------------

func (s *Server) registerConn(token string, c *wsConn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	set := s.conns[token]
	if set == nil {
		set = map[*wsConn]struct{}{}
		s.conns[token] = set
	}
	set[c] = struct{}{}
}

func (s *Server) unregisterConn(token string, c *wsConn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	set := s.conns[token]
	delete(set, c)
	if len(set) == 0 {
		delete(s.conns, token)
	}
}

func (s *Server) closeSessionConns(token string) {
	s.connMu.Lock()
	list := make([]*wsConn, 0, len(s.conns[token]))
	for c := range s.conns[token] {
		list = append(list, c)
	}
	s.connMu.Unlock()
	for _, c := range list {
		c.closeWith(closePolicyViolation, "session ended")
	}
}

// CloseAllSessions closes every live socket after an out-of-process password
// change invalidated the session store.
func (s *Server) CloseAllSessions() { s.closeAllConns() }

func (s *Server) closeAllConns() {
	s.connMu.Lock()
	var list []*wsConn
	for _, set := range s.conns {
		for c := range set {
			list = append(list, c)
		}
	}
	s.connMu.Unlock()
	for _, c := range list {
		c.closeWith(closePolicyViolation, "session ended")
	}
}

// --- login rate limiting ---------------------------------------------------

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginEntry
}

type loginEntry struct {
	failures int
	until    time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: map[string]*loginEntry{}}
}

func (l *loginLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return true
	}
	if time.Now().Before(e.until) {
		return false
	}
	if e.failures >= 5 && time.Now().After(e.until) {
		e.failures = 0
	}
	return true
}

func (l *loginLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		e = &loginEntry{}
		l.entries[key] = e
	}
	e.failures++
	if e.failures >= 5 {
		backoff := time.Duration(e.failures-4) * 30 * time.Second
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		e.until = time.Now().Add(backoff)
	}
}

func (l *loginLimiter) Success(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func parseUint(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}
