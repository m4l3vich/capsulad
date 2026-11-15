package wifi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	portalTokenLifetime = 15 * time.Minute
	portalBodyLimit     = 4096
)

type portalServer struct {
	srv *http.Server
}

type portal struct {
	backend       Controller
	apAddr        netip.Addr
	prefix        netip.Prefix
	port          int
	password      string
	secret        []byte
	now           func() time.Time
	beforeConnect func()
}

// startPortalServer is a test seam: focused manager tests replace it with an
// in-memory server so they never bind the AP address of the host running the
// test. The captive portal handler itself is covered directly.
var startPortalServer = newPortalServer

func newPortalServer(backend Controller, opts portalOptions) (*portalServer, error) {
	if backend == nil {
		return nil, fmt.Errorf("wifi: captive portal needs a Wi-Fi backend")
	}
	apAddr, err := netip.ParseAddr(opts.APAddress)
	if err != nil || !apAddr.Is4() || apAddr.IsUnspecified() {
		return nil, fmt.Errorf("wifi: captive portal AP address %q is invalid", opts.APAddress)
	}
	if !opts.Prefix.Contains(apAddr) {
		return nil, fmt.Errorf("wifi: captive portal address %s is outside %s", apAddr, opts.Prefix)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("wifi: generate portal token key: %w", err)
	}
	password := opts.Password
	if password == "" {
		password = APPassword
	}
	p := &portal{
		backend: backend, apAddr: apAddr, prefix: opts.Prefix, port: opts.Port,
		password: password, secret: secret, now: time.Now,
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(apAddr.String(), strconv.Itoa(opts.Port)))
	if err != nil {
		return nil, fmt.Errorf("wifi: listen for captive portal on %s: %w", apAddr, err)
	}
	server := &http.Server{
		Handler:           p.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	ps := &portalServer{srv: server}
	go func() { _ = server.Serve(ln) }()
	return ps, nil
}

func (s *portalServer) close() {
	if s == nil || s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	_ = s.srv.Close()
}

func (p *portal) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", p.handlePage)
	mux.HandleFunc("HEAD /{$}", p.handlePage)
	mux.HandleFunc("POST /scan", p.handleScan)
	mux.HandleFunc("POST /connect", p.handleConnect)
	for _, path := range captiveProbePaths {
		mux.HandleFunc("GET "+path, p.handleProbe)
		mux.HandleFunc("HEAD "+path, p.handleProbe)
	}
	mux.HandleFunc("/", p.handleFallback)
	return p.securityHeaders(mux)
}

var captiveProbePaths = []string{
	"/generate_204", "/gen_204", "/hotspot-detect.html", "/connecttest.txt",
	"/redirect", "/ncsi.txt", "/canonical.html", "/success.txt", "/library/test/success.html",
}

func (p *portal) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'none'; script-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (p *portal) handleProbe(w http.ResponseWriter, r *http.Request) {
	if !p.networkAllowed(w, r) {
		return
	}
	// Captive probes carry the public detection host name, not the AP IP.
	// Redirect to the literal AP origin so the portal page passes its Host
	// check instead of returning an error for the probe's host name.
	http.Redirect(w, r, p.url("/"), http.StatusFound)
}

func (p *portal) handleFallback(w http.ResponseWriter, r *http.Request) {
	if !p.networkAllowed(w, r) {
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		http.Redirect(w, r, p.url("/"), http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

func (p *portal) url(path string) string {
	host := p.apAddr.String()
	if p.port != 80 {
		host = net.JoinHostPort(host, strconv.Itoa(p.port))
	}
	return "http://" + host + path
}

func (p *portal) handlePage(w http.ResponseWriter, r *http.Request) {
	if !p.networkAllowed(w, r) || !p.hostAllowed(w, r) || !p.originAllowed(w, r) {
		return
	}
	p.renderPage(w, r, portalPage{Token: p.issueToken(p.clientIP(r))})
}

func (p *portal) handleScan(w http.ResponseWriter, r *http.Request) {
	if !p.networkAllowed(w, r) || !p.hostAllowed(w, r) || !p.originAllowed(w, r) {
		return
	}
	if !p.postAllowed(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	results, err := p.backend.Scan(ctx)
	page := portalPage{Token: p.issueToken(p.clientIP(r)), Scanned: true}
	if err != nil {
		page.Notice = "Scan failed: " + err.Error()
	} else if len(results) == 0 {
		page.Notice = "No Wi-Fi networks were found. You can still type a network name below."
	}
	page.Scans = results
	p.renderPage(w, r, page)
}

func (p *portal) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !p.networkAllowed(w, r) || !p.hostAllowed(w, r) || !p.originAllowed(w, r) {
		return
	}
	if !p.postAllowed(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, portalBodyLimit)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	req := ConnectRequest{
		SSID:       strings.TrimSpace(r.FormValue("ssid")),
		Passphrase: r.FormValue("passphrase"),
		Security:   r.FormValue("security"),
	}
	security, err := NormalizeSecurity(req.Security)
	if err != nil || !validSSID(req.SSID) {
		p.renderPage(w, r, portalPage{
			Token:       p.issueToken(p.clientIP(r)),
			Notice:      "Enter a valid network name and security mode.",
			ConnectSSID: req.SSID,
		})
		return
	}
	req.Security = security
	if security != SecurityOpen && (len(req.Passphrase) < 8 || len(req.Passphrase) > 63 ||
		strings.ContainsAny(req.Passphrase, "\x00\r\n")) {
		p.renderPage(w, r, portalPage{
			Token:       p.issueToken(p.clientIP(r)),
			Notice:      "The password must be 8-63 characters without control characters.",
			ConnectSSID: req.SSID,
		})
		return
	}
	if security == SecurityWEP {
		p.renderPage(w, r, portalPage{
			Token:       p.issueToken(p.clientIP(r)),
			Notice:      "WEP networks are not supported.",
			ConnectSSID: req.SSID,
		})
		return
	}

	// The AP client must receive the accepted result before the radio leaves AP
	// mode. The actual association runs after this handler has flushed; a
	// failure restores the AP and the next page carries LastError.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	_ = portalConnectTemplate.Execute(w, portalConnectPage{SSID: req.SSID})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if p.beforeConnect != nil {
		p.beforeConnect()
	}
	connectReq := req
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = p.backend.Connect(ctx, connectReq)
	}()
}

func (p *portal) renderPage(w http.ResponseWriter, r *http.Request, page portalPage) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	status, err := p.backend.Status(ctx)
	known, kerr := p.backend.KnownNetworks(ctx)
	if kerr != nil && err == nil {
		err = kerr
	}
	page.Status = status
	page.Known = known
	page.APPassword = p.password
	if page.APPassword == "" {
		page.APPassword = APPassword
	}
	if page.Token == "" {
		page.Token = p.issueToken(p.clientIP(r))
	}
	if page.ConnectSecurity == "" {
		page.ConnectSecurity = SecurityWPA2
	}
	if err != nil {
		page.Notice = strings.TrimSpace(page.Notice + " " + err.Error())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = portalPageTemplate.Execute(w, page)
}

// postAllowed validates the HMAC token and method. The token is bound to the
// client address and remains valid for 15 minutes; it is not single-use, so a
// page can submit Scan and then Connect. Origin and Host were checked first.
func (p *portal) postAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, portalBodyLimit)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	if !p.validToken(p.clientIP(r), r.FormValue("token")) {
		http.Error(w, "invalid or expired portal token", http.StatusForbidden)
		return false
	}
	return true
}

func (p *portal) networkAllowed(w http.ResponseWriter, r *http.Request) bool {
	ip, ok := remoteIP(r)
	if !ok || !p.prefix.Contains(ip) || ip == p.apAddr {
		http.Error(w, "portal is available only on the setup network", http.StatusForbidden)
		return false
	}
	return true
}

func (p *portal) hostAllowed(w http.ResponseWriter, r *http.Request) bool {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		http.Error(w, "missing Host", http.StatusBadRequest)
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	addr, err := netip.ParseAddr(host)
	if err != nil || addr != p.apAddr {
		http.Error(w, "unexpected Host", http.StatusBadRequest)
		return false
	}
	return true
}

func (p *portal) originAllowed(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// A captive-browser top-level navigation is often cross-site by design
		// (detection host -> AP address); Sec-Fetch-Site is enforced only for
		// state-changing POSTs in postAllowed.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return false
	}
	want := p.apAddr.String()
	host := u.Host
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port != strconv.Itoa(p.port) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return false
		}
		host = h
	} else if p.port != 80 {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return false
	}
	if strings.Trim(host, "[]") != want {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return false
	}
	return true
}

func (p *portal) clientIP(r *http.Request) string {
	ip, ok := remoteIP(r)
	if !ok {
		return ""
	}
	return ip.String()
}

func remoteIP(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func (p *portal) issueToken(clientIP string) string {
	expiry := p.now().Add(portalTokenLifetime).Unix()
	payload := strconv.FormatInt(expiry, 10)
	return payload + "." + p.tokenMAC(clientIP, payload)
}

func (p *portal) validToken(clientIP, token string) bool {
	payload, mac, ok := strings.Cut(token, ".")
	if !ok || payload == "" || mac == "" {
		return false
	}
	expiry, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || expiry < p.now().Unix() || expiry > p.now().Add(portalTokenLifetime+time.Minute).Unix() {
		return false
	}
	want := p.tokenMAC(clientIP, payload)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) != 1 {
		return false
	}
	return true
}

func (p *portal) tokenMAC(clientIP, payload string) string {
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write([]byte(clientIP))
	_, _ = mac.Write([]byte{'|'})
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

type portalPage struct {
	Token           string
	Status          Status
	Known           []KnownNetwork
	Scans           []ScanResult
	Scanned         bool
	Notice          string
	APPassword      string
	ConnectSSID     string
	ConnectSecurity string
}

type portalConnectPage struct{ SSID string }

var portalPageTemplate = template.Must(template.New("portal").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>Capsula setup</title>
<style>
:root{color-scheme:light dark;--bg:#f6f5f1;--card:#fffdf8;--ink:#20201d;--muted:#6b6a63;--line:#d9d5c8;--accent:#2c5f4f;--err:#8a2d23}
@media(prefers-color-scheme:dark){:root{--bg:#171816;--card:#20221f;--ink:#f0eee6;--muted:#aaa69b;--line:#3b3d37;--accent:#8ec7ae;--err:#f0a79e}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:34rem;margin:0 auto;padding:2rem 1.1rem 3rem}.brand{font-weight:700;letter-spacing:.04em;color:var(--accent)}
h1{font-size:1.6rem;line-height:1.2;margin:.5rem 0 1rem}h2{font-size:1.05rem;margin:1.6rem 0 .5rem}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:1rem;margin:.7rem 0}
.muted{color:var(--muted)}.notice{border-left:4px solid var(--accent);padding:.6rem .8rem;background:var(--card);border-radius:8px;margin:.8rem 0}
.err{border-color:var(--err)}label{display:block;font-weight:600;margin:.6rem 0 .25rem}
input,select{width:100%;padding:.65rem .7rem;border:1px solid var(--line);border-radius:9px;background:var(--bg);color:var(--ink);font:inherit}
button{margin-top:.9rem;width:100%;padding:.7rem;border:0;border-radius:10px;background:var(--accent);color:var(--bg);font:inherit;font-weight:700}
table{width:100%;border-collapse:collapse;margin:.5rem 0}th,td{text-align:left;padding:.4rem .3rem;border-bottom:1px solid var(--line);font-size:.92rem}
code{font-family:ui-monospace,SFMono-Regular,monospace}.pill{display:inline-block;padding:.1rem .45rem;border:1px solid var(--line);border-radius:999px;font-size:.8rem;color:var(--muted)}
</style>
</head>
<body><main>
<div class="brand">Capsula</div>
<h1>Set up this speaker’s Wi-Fi</h1>
{{if .Notice}}<div class="notice err" role="alert">{{.Notice}}</div>{{end}}
<div class="card">
<p><strong>Status:</strong> <span class="pill">{{.Status.State}}</span>{{if .Status.SSID}} <strong>{{.Status.SSID}}</strong>{{end}}</p>
{{if .Status.APActive}}<p>{{.Status.Detail}}</p><p>Join the setup network <strong>{{.Status.APSSID}}</strong> with password <code>{{.APPassword}}</code>. This page is available only on that network.</p>{{else}}<p>{{.Status.Detail}}</p>{{end}}
{{if .Status.LastError}}<p class="muted">Last connection attempt: {{.Status.LastError}}</p>{{end}}
</div>
{{if .Known}}<h2>Saved networks</h2>
<div class="card"><table><thead><tr><th>Network</th><th>Security</th></tr></thead><tbody>
{{range .Known}}<tr><td>{{.SSID}}{{if .Connected}} <span class="pill">connected</span>{{end}}</td><td>{{.Security}}</td></tr>{{end}}
</tbody></table><p class="muted">Saved networks are managed from the speaker’s authenticated Web UI.</p></div>{{end}}
<h2>Find a network</h2>
<div class="card">
{{if .Status.ScanSupported}}<form method="post" action="/scan">
<input type="hidden" name="token" value="{{.Token}}">
<button type="submit">Scan for Wi-Fi networks</button>
</form>
{{else}}<p class="muted">Scanning is unavailable on this supplicant while the setup AP is active. Type the network name below.</p>{{end}}
{{if .Scanned}}<p class="muted">Scan results are a snapshot from this request; this page does not scan automatically.</p>{{end}}
{{if .Scans}}<table><thead><tr><th>Network</th><th>Signal</th><th>Security</th></tr></thead><tbody>
{{range .Scans}}<tr><td>{{.SSID}}</td><td>{{.SignalDBM}} dBm</td><td>{{.Security}}</td></tr>{{end}}
</tbody></table>{{end}}
</div>
<h2>Connect</h2>
<div class="card">
<form method="post" action="/connect">
<input type="hidden" name="token" value="{{.Token}}">
<label for="ssid">Network name</label>
<input id="ssid" name="ssid" value="{{.ConnectSSID}}" maxlength="32" required autocomplete="off"{{if .Scans}} list="scan-networks"{{end}}>
{{if .Scans}}<datalist id="scan-networks">{{range .Scans}}<option value="{{.SSID}}">{{.SSID}} — {{.Security}}</option>{{end}}</datalist>{{end}}
<label for="security">Security</label>
<select id="security" name="security">
<option value="wpa2"{{if eq .ConnectSecurity "wpa2"}} selected{{end}}>WPA2 / WPA2-WPA3</option>
<option value="wpa3"{{if eq .ConnectSecurity "wpa3"}} selected{{end}}>WPA3</option>
<option value="wpa"{{if eq .ConnectSecurity "wpa"}} selected{{end}}>WPA</option>
<option value="open"{{if eq .ConnectSecurity "open"}} selected{{end}}>Open network</option>
</select>
<label for="passphrase">Password</label>
<input id="passphrase" name="passphrase" type="password" maxlength="63" autocomplete="new-password">
<p class="muted">The password is used once to derive the network key. It is not shown again or sent anywhere except this speaker.</p>
<button type="submit">Connect speaker</button>
</form>
</div>
<p class="muted">This setup page exposes only Wi-Fi scan and connect. It cannot change speaker settings or reach the administrator interface.</p>
</main></body></html>`))

var portalConnectTemplate = template.Must(template.New("connect").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow"><title>Capsula is connecting</title>
<style>body{margin:0;background:#f6f5f1;color:#20201d;font:16px/1.5 system-ui,sans-serif}main{max-width:32rem;margin:0 auto;padding:3rem 1.2rem}h1{font-size:1.5rem}.muted{color:#6b6a63}</style>
</head><body><main><h1>Connecting to {{.SSID}}</h1>
<p>The speaker is saving the network and leaving setup mode. This setup network will close; if the connection fails, it returns and this page shows the error.</p>
<p class="muted">You can reconnect to the speaker on your home network after a moment.</p>
</main></body></html>`))
