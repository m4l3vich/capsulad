// Package webui serves the HTTPS browser interface.
//
// The listener uses a self-signed certificate; authentication is an Argon2id
// admin password with server-side sessions, or a static opaque API key for
// automation. The UI backend speaks the same local gRPC API over the Unix
// socket as every other client and is not a second privileged control plane.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"capsulad/internal/logbus"
	"capsulad/internal/version"
	"capsulad/internal/websecurity"
)

type Server struct {
	Addr     string
	TLS      *tls.Config
	Conn     *grpc.ClientConn
	Logs     *logbus.Bus
	Platform string
	Auth     *websecurity.Store
	limiter  *loginLimiter

	tmpl *template.Template
}

type pageData struct {
	Title    string
	Nav      string
	Version  string
	Platform string
	User     string
	CSRF     string
	Message  string
	Error    string
	Data     any
}

type loginData struct {
	Error string
	Next  string
	CSRF  string
}

type authInfo struct {
	OK         bool
	ViaSession bool
	ViaAPIKey  bool
}

type authContextKey struct{}

func New(addr string, tlsCfg *tls.Config, conn *grpc.ClientConn, logs *logbus.Bus, platform string, auth *websecurity.Store) (*Server, error) {
	s := &Server{
		Addr: addr, TLS: tlsCfg, Conn: conn, Logs: logs, Platform: platform, Auth: auth,
		limiter: newLoginLimiter(),
	}
	tmpl, err := template.New("capsula").Funcs(template.FuncMap{
		"pct": func(v any) int {
			switch n := v.(type) {
			case float32:
				return int(n * 100)
			case float64:
				return int(n * 100)
			}
			return 0
		},
		"rgb":  func(r, g, b uint32) string { return fmt.Sprintf("#%02x%02x%02x", r, g, b) },
		"json": func(v any) string { return fmt.Sprintf("%v", v) },
		"secs": func(ms uint32) int { return int(ms / 1000) },
		"msdate": func(ms int64) string {
			if ms == 0 {
				return "unknown"
			}
			return time.UnixMilli(ms).UTC().Format(time.RFC3339)
		},
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "unknown"
			}
			return t.UTC().Format(time.RFC3339)
		},
		"unq": func(v string) string {
			if unq, err := strconv.Unquote(v); err == nil {
				return unq
			}
			return v
		},
		"clockStatus": clockStatus,
		"clockDur":    clockDuration,
		"clockDays":   clockDays,
		"clockHasDay": clockHasDay,
	}).Parse(templates)
	if err != nil {
		return nil, err
	}
	s.tmpl = tmpl
	return s, nil
}

// Handler builds the full routing table with authentication middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /", s.overview)
	mux.HandleFunc("GET /settings", s.settingsPage)
	mux.HandleFunc("POST /settings", s.updateSettings)
	mux.HandleFunc("POST /bluetooth", s.bluetoothToggle)
	mux.HandleFunc("POST /bluetooth/pairable", s.bluetoothPairable)
	mux.HandleFunc("POST /bluetooth/unpair", s.bluetoothUnpair)
	mux.HandleFunc("GET /access", s.accessPage)
	mux.HandleFunc("POST /access/password", s.changePassword)
	mux.HandleFunc("POST /access/keys", s.createAPIKey)
	mux.HandleFunc("POST /access/keys/revoke", s.revokeAPIKey)
	mux.HandleFunc("GET /access/cert.pem", s.certPEM)
	mux.HandleFunc("GET /diagnostics", s.diagnostics)
	mux.HandleFunc("GET /wakeword", s.wakewordPage)
	mux.HandleFunc("GET /wakeword/trainer.js", s.wakewordTrainerJS)
	mux.HandleFunc("GET /wakeword/samples", s.wakewordSamplesJSON)
	mux.HandleFunc("POST /wakeword/record", s.wakewordRecord)
	mux.HandleFunc("POST /wakeword/delete", s.wakewordDelete)
	mux.HandleFunc("POST /wakeword/train", s.wakewordTrain)
	mux.HandleFunc("GET /applications/{id}", s.application)
	mux.HandleFunc("POST /applications/{id}/launch", s.appLaunch)
	mux.HandleFunc("POST /applications/{id}/stop", s.appStop)
	mux.HandleFunc("POST /applications/{id}/action", s.appAction)
	mux.HandleFunc("POST /applications/{id}/clock", s.clockAction)
	mux.HandleFunc("POST /applications/{id}/settings", s.appSettings)
	mux.HandleFunc("POST /media", s.mediaAction)
	mux.HandleFunc("GET /api/status", s.apiStatus)
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
	s.Logs.Infof("webui", "", "HTTPS listening on %s (self-signed certificate, password/API-key auth)", ln.Addr())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeTLS(ln, "", "") }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		s.ensureCSRF(w, r)

		if r.URL.Path == "/login" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		auth := s.authenticate(r)
		if !auth.OK {
			wantsJSON := strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.Header.Get("Accept"), "application/json")
			if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !wantsJSON {
				target := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
				http.Redirect(w, r, target, http.StatusSeeOther)
				return
			}
			writeUnauthorized(w, r)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			if auth.ViaSession {
				if !s.checkOrigin(r) {
					http.Error(w, "cross-origin form submission rejected", http.StatusForbidden)
					return
				}
				if !s.checkCSRF(r) {
					http.Error(w, "invalid CSRF token", http.StatusForbidden)
					return
				}
			}
		}
		ctx := context.WithValue(r.Context(), authContextKey{}, auth)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) authenticate(r *http.Request) authInfo {
	if token := apiKeyFromRequest(r); token != "" {
		if s.Auth.VerifyAPIKey(token) {
			return authInfo{OK: true, ViaAPIKey: true}
		}
		return authInfo{}
	}
	if cookie, err := r.Cookie(websecurity.SessionCookieName()); err == nil {
		if s.Auth.ValidateSession(cookie.Value) {
			return authInfo{OK: true, ViaSession: true}
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
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
		return
	}
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) {
	token := csrfFrom(r.Cookies())
	if token == "" {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return
		}
		token = hex.EncodeToString(b[:])
		http.SetCookie(w, &http.Cookie{
			Name: "capsula_csrf", Value: token, Path: "/",
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		})
	}
	r.Header.Set("X-Capsula-CSRF", token)
}

func csrfFrom(cookies []*http.Cookie) string {
	for _, c := range cookies {
		if c.Name == "capsula_csrf" {
			return c.Value
		}
	}
	return ""
}

func (s *Server) checkCSRF(r *http.Request) bool {
	want := csrfFrom(r.Cookies())
	got := r.PostFormValue("csrf")
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

// --- login / logout --------------------------------------------------------

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.authenticate(r).OK {
		http.Redirect(w, r, safeNext(r), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, http.StatusOK, "", safeNext(r))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if !s.checkOrigin(r) || !s.checkCSRF(r) {
		http.Error(w, "invalid login request", http.StatusForbidden)
		return
	}
	next := safeNext(r)
	if !s.Auth.PasswordSet() {
		s.renderLogin(w, r, http.StatusServiceUnavailable,
			"administrator password is not initialized; run `capsula auth init` on the device", next)
		return
	}
	host := remoteHost(r)
	if !s.limiter.Allow(host) {
		s.renderLogin(w, r, http.StatusTooManyRequests, "too many failed attempts; wait and try again", next)
		return
	}
	if !s.Auth.VerifyPassword(r.PostFormValue("password")) {
		s.limiter.Fail(host)
		s.Logs.Warnf("webui", "", "failed login from %s", host)
		s.renderLogin(w, r, http.StatusUnauthorized, "incorrect password", next)
		return
	}
	s.limiter.Success(host)
	token, expiry, err := s.Auth.NewSession()
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: websecurity.SessionCookieName(), Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: expiry,
	})
	s.Logs.Infof("webui", "", "admin login from %s", host)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(websecurity.SessionCookieName()); err == nil {
		s.Auth.RevokeSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: websecurity.SessionCookieName(), Value: "", Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func safeNext(r *http.Request) string {
	next := r.URL.Query().Get("next")
	if next == "" {
		next = r.PostFormValue("next")
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, message, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := loginData{Error: message, Next: next, CSRF: r.Header.Get("X-Capsula-CSRF")}
	if err := s.tmpl.ExecuteTemplate(w, "login", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- shared page helpers ---------------------------------------------------

func (s *Server) base(w http.ResponseWriter, r *http.Request, title, nav string) pageData {
	a := authFrom(r.Context())
	user := ""
	if a.ViaSession {
		user = "admin"
	} else if a.ViaAPIKey {
		user = "API key"
	}
	return pageData{
		Title: title, Nav: nav, Version: version.Version, Platform: s.Platform,
		User: user, CSRF: r.Header.Get("X-Capsula-CSRF"),
		Message: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"),
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, page, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	http.Redirect(w, r, path+"?"+q.Encode(), http.StatusSeeOther)
}

func ctxTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
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
	if e.failures >= 5 && time.Since(e.until) > 0 && time.Now().After(e.until) {
		// Lockout elapsed; reset the counter.
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
