package webui

import (
	"bufio"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// plaintextProbeTimeout bounds the first-byte probe and the plaintext
	// request-head read, so a misbehaving peer is dropped instead of holding
	// a socket indefinitely.
	plaintextProbeTimeout = 2 * time.Second
	// maxPlaintextRequestHead is the most of a plain HTTP request head this
	// listener reads. Only the Host header is used; everything else is
	// discarded before the guidance page is written.
	maxPlaintextRequestHead = 8 << 10
	// plaintextFallbackHost is shown when the request carries no usable Host.
	plaintextFallbackHost = "capsula.local"
	// tlsClientHello is the first byte of a TLS handshake record.
	tlsClientHello = 0x16
)

// probeListener distinguishes TLS from plain HTTP on the first byte of each
// accepted connection. A TLS ClientHello starts with 0x16 and is handed,
// untouched, to the TLS layer. Everything else receives a short HTML page
// explaining the HTTPS Web UI, then the connection is closed. This replaces
// net/http's bare "Client sent an HTTP request to an HTTPS server." response.
//
// Classification never runs on the accept path: one idle peer must not delay
// the TLS handshakes of everyone else. A background loop accepts raw
// connections, classifies each in its own goroutine, and queues only TLS
// connections for the TLS listener's Accept.
type probeListener struct {
	net.Listener

	once    sync.Once
	conns   chan net.Conn
	done    chan struct{}
	replies chan struct{}
	wg      sync.WaitGroup

	errMu     sync.Mutex
	acceptErr error
}

const (
	// probeConnQueue bounds TLS connections waiting for the TLS listener.
	probeConnQueue = 64
	// plaintextReplyLimit bounds concurrent plaintext replies; excess peers are
	// closed after the probe.
	plaintextReplyLimit = 64
)

func (l *probeListener) Accept() (net.Conn, error) {
	l.once.Do(l.start)
	select {
	case conn, ok := <-l.conns:
		if !ok {
			return nil, l.err()
		}
		return conn, nil
	case <-l.done:
		return nil, l.err()
	}
}

func (l *probeListener) start() {
	l.conns = make(chan net.Conn, probeConnQueue)
	l.done = make(chan struct{})
	l.replies = make(chan struct{}, plaintextReplyLimit)
	go l.acceptLoop()
}

func (l *probeListener) acceptLoop() {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			l.setErr(err)
			break
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.classify(conn)
		}()
	}
	// Release classifiers that are blocked queueing a TLS conn before waiting
	// for them: a full conns queue would otherwise make wg.Wait deadlock.
	close(l.done)
	l.wg.Wait()
	for {
		select {
		case conn := <-l.conns:
			_ = conn.Close()
		default:
			close(l.conns)
			return
		}
	}
}

func (l *probeListener) classify(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(plaintextProbeTimeout))
	br := bufio.NewReaderSize(conn, 4096)
	first, err := br.Peek(1)
	if err != nil {
		// Idle, closed or timed-out before speaking: drop silently instead
		// of turning it into a TLS handshake error.
		_ = conn.Close()
		return
	}
	if first[0] == tlsClientHello {
		_ = conn.SetReadDeadline(time.Time{})
		select {
		case l.conns <- &bufferedConn{Conn: conn, r: br}:
		case <-l.done:
			_ = conn.Close()
		}
		return
	}
	select {
	case l.replies <- struct{}{}:
		defer func() { <-l.replies }()
	default:
		// Too many concurrent plaintext replies; shed this connection rather
		// than growing goroutines without a bound.
		_ = conn.Close()
		return
	}
	l.answerPlaintext(conn, br)
}

func (l *probeListener) setErr(err error) {
	l.errMu.Lock()
	l.acceptErr = err
	l.errMu.Unlock()
}

func (l *probeListener) err() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	if l.acceptErr == nil {
		return net.ErrClosed
	}
	return l.acceptErr
}

// bufferedConn replays bytes buffered by the probe ahead of the underlying
// connection, so the TLS handshake sees an unmodified byte stream.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (l *probeListener) answerPlaintext(conn net.Conn, br *bufio.Reader) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(plaintextProbeTimeout))

	host := ""
	req, err := http.ReadRequest(bufio.NewReaderSize(
		&io.LimitedReader{R: br, N: maxPlaintextRequestHead}, 4096))
	if err == nil {
		host = req.Host
	}

	page := renderPlaintextPage(host)

	var head strings.Builder
	fmt.Fprintf(&head, "HTTP/1.1 200 OK\r\n")
	head.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	fmt.Fprintf(&head, "Content-Length: %d\r\n", len(page))
	head.WriteString("Connection: close\r\n")
	head.WriteString("Cache-Control: no-store\r\n")
	head.WriteString("X-Content-Type-Options: nosniff\r\n")
	head.WriteString("Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'\r\n")
	head.WriteString("\r\n")

	_ = conn.SetWriteDeadline(time.Now().Add(plaintextProbeTimeout))
	_, _ = io.WriteString(conn, head.String())
	_, _ = conn.Write(page)
}

// sanitisePlaintextHost strips any port and accepts only a hostname or IP
// literal. Anything else falls back to a neutral name, so the page never
// reflects arbitrary request text.
func sanitisePlaintextHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return plaintextFallbackHost
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	if ip := net.ParseIP(raw); ip != nil {
		return ip.String()
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		if ip := net.ParseIP(raw[1 : len(raw)-1]); ip != nil {
			return ip.String()
		}
	}
	if len(raw) > 253 || strings.HasPrefix(raw, ".") {
		return plaintextFallbackHost
	}
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			continue
		}
		return plaintextFallbackHost
	}
	return strings.ToLower(raw)
}

func httpsAddress(host, port string) string {
	if port == "" {
		port = "8443"
	}
	return "https://" + net.JoinHostPort(host, port) + "/"
}

// requestedPort returns the port the client used, so a daemon on a
// non-default listener shows the address that actually reaches it.
func requestedPort(rawHost string) string {
	if _, port, err := net.SplitHostPort(strings.TrimSpace(rawHost)); err == nil {
		return port
	}
	return ""
}

type plaintextPageData struct {
	URL string
}

func renderPlaintextPage(rawHost string) []byte {
	host := sanitisePlaintextHost(rawHost)
	url := httpsAddress(host, requestedPort(rawHost))
	var b strings.Builder
	if err := plaintextPage.Execute(&b, plaintextPageData{URL: url}); err != nil {
		// The template is static and its data is already sanitised; a failure
		// here would mean a programming error, so keep a minimal safe page.
		return []byte("<!doctype html><html lang=\"en-GB\"><body><h1>Open this Web UI over HTTPS</h1><p>Use " +
			template.HTMLEscapeString(url) + " instead.</p></body></html>")
	}
	return []byte(b.String())
}

var plaintextPage = template.Must(template.New("plaintext").Parse(`<!doctype html>
<html lang="en-GB">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Open the speaker's Web UI over HTTPS</title>
<style>
:root { color-scheme: light dark; }
body {
  margin: 0;
  padding: 2rem 1rem;
  font-family: system-ui, -apple-system, "Segoe UI", sans-serif;
  line-height: 1.55;
  background: Canvas;
  color: CanvasText;
}
main { max-width: 44rem; margin: 0 auto; }
h1 { font-size: 1.5rem; margin-bottom: 0.5rem; }
h2 { font-size: 1.125rem; margin-top: 1.75rem; }
a { color: inherit; text-decoration: underline; text-underline-offset: 0.15em; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
pre {
  overflow-x: auto;
  padding: 0.85rem 1rem;
  border: 1px solid color-mix(in srgb, CanvasText 20%, transparent);
  border-radius: 0.4rem;
  background: color-mix(in srgb, CanvasText 5%, Canvas);
}
:focus-visible { outline: 2px solid currentColor; outline-offset: 2px; }
</style>
</head>
<body>
<main>
<h1>This is the speaker's HTTPS Web UI</h1>
<p>You reached the speaker over plain HTTP. Its Web UI is served only over HTTPS, so open
<a href="{{.URL}}">{{.URL}}</a> instead.</p>

<h2>About the certificate</h2>
<p>The speaker uses a self-signed certificate. Your browser will show a warning until that
certificate is trusted; the warning is expected on first use.</p>

<h2>Trust the certificate</h2>
<ol>
<li>Sign in at <a href="{{.URL}}">{{.URL}}</a> once the HTTPS listener is reachable, then
open <strong>Access</strong> and download the CA or server certificate there.</li>
<li>Import the downloaded certificate as a trusted root certificate or CA in your browser or
operating system.</li>
<li>Compare its SHA-256 fingerprint with the one shown by
<code>capsulad-cli cert status</code>.</li>
</ol>

<h2>Device commands</h2>
<p>Run these commands on the speaker's shell:</p>
<pre><code>capsulad-cli cert status
capsulad-cli cert init
capsulad-cli auth init
capsulad-cli cert renew
capsulad-cli status</code></pre>
<p><code>capsulad-cli auth init</code> creates the server certificate and the administrator
password. <code>cert init</code> creates a certificate on its own, <code>cert renew</code>
replaces it, and <code>cert status</code> prints the fingerprint, names and expiry. A renewed
certificate is reloaded by restarting the daemon.</p>
</main>
</body>
</html>
`))
