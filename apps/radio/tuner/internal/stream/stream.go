// Package stream opens one live HTTP(S) audio stream through the shared native
// capsulad transport.
//
// The framework exposes completed HTTP exchanges and raw TCP/TLS streams, not
// a streaming HTTP client, and a radio stream never completes. This package
// therefore performs the smallest HTTP/1.1 exchange the job needs over
// capsulad.OpenTCP: request line, headers, redirects, chunked or
// content-length framing, and Shoutcast/Icecast ICY metadata stripping. TLS,
// DNS and certificate verification stay in libcapsulad; no Go net/http or
// crypto/tls code is linked into the app.
package stream

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"capsulad/sdk/capsulad"
)

// Codec is the container/codec a stream advertises or that the first bytes
// identify. The tuner decodes only CodecMP3.
type Codec int

const (
	CodecUnknown Codec = iota
	CodecMP3
	CodecAAC
	CodecOgg
	CodecFLAC
	CodecMP4
	CodecWAV
	CodecHLS
)

// Supported reports whether this build can decode the codec.
func (c Codec) Supported() bool { return c == CodecMP3 }

func (c Codec) String() string {
	switch c {
	case CodecMP3:
		return "MP3"
	case CodecAAC:
		return "AAC/HE-AAC"
	case CodecOgg:
		return "Ogg/Opus/Vorbis"
	case CodecFLAC:
		return "FLAC"
	case CodecMP4:
		return "MP4/M4A"
	case CodecWAV:
		return "WAV"
	case CodecHLS:
		return "HLS"
	default:
		return "unknown"
	}
}

// ErrHLS reports an HLS playlist, which this build cannot play.
var ErrHLS = errors.New("HLS playlists are not supported")

// HTTPError is a non-2xx response. URL is redacted (no query or userinfo).
type HTTPError struct {
	StatusCode int
	Reason     string
	URL        string
}

func (e *HTTPError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("HTTP %d from %s", e.StatusCode, e.URL)
	}
	return fmt.Sprintf("HTTP %d %s from %s", e.StatusCode, e.Reason, e.URL)
}

// Config bounds one stream open and its later reads. Zero values select the
// defaults below.
type Config struct {
	Env              capsulad.Env
	UserAgent        string
	MaxRedirects     int
	DialTimeout      time.Duration
	HeaderTimeout    time.Duration
	IdleTimeout      time.Duration
	MaxHeaderBytes   int
	MaxPlaylistBytes int64
}

func (c Config) normalize() Config {
	if c.UserAgent == "" {
		c.UserAgent = "capsulad-radio-tuner/1.0"
	}
	if c.MaxRedirects <= 0 {
		c.MaxRedirects = 5
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	if c.HeaderTimeout <= 0 {
		c.HeaderTimeout = 6 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 10 * time.Second
	}
	if c.MaxHeaderBytes <= 0 {
		c.MaxHeaderBytes = 64 << 10
	}
	if c.MaxPlaylistBytes <= 0 {
		c.MaxPlaylistBytes = 64 << 10
	}
	return c
}

// Stream is one opened radio stream. Its context is the context passed to
// Open; SetContext swaps it for the session lifetime once the handshake is
// complete.
type Stream struct {
	conn        *capsulad.TCPStream
	cr          *connReader
	br          *bufio.Reader
	body        io.Reader
	icy         *icyReader
	codec       Codec
	contentType string
	finalURL    *url.URL
	icyName     string
	icyGenre    string
}

// connReader applies one idle deadline per read and lets the owner replace the
// session context after Open returns. TCPStream.Read closes the stream when its
// context is cancelled, so a per-read timeout is also the idle watchdog.
type connReader struct {
	conn *capsulad.TCPStream
	mu   sync.RWMutex
	ctx  context.Context
	idle time.Duration
}

func (r *connReader) Read(p []byte) (int, error) {
	r.mu.RLock()
	base, idle := r.ctx, r.idle
	r.mu.RUnlock()
	ctx := base
	var cancel context.CancelFunc
	if idle > 0 {
		ctx, cancel = context.WithTimeout(base, idle)
		defer cancel()
	}
	n, err := r.conn.Read(ctx, p)
	if err == nil {
		return n, nil
	}
	if base != nil && base.Err() != nil {
		return n, base.Err()
	}
	if ctx.Err() != nil {
		return n, fmt.Errorf("stream read timed out after %s", idle)
	}
	return n, err
}

// SetContext replaces the context used by later reads. It is called once the
// open deadline must no longer apply to the long-lived body.
func (s *Stream) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.cr.mu.Lock()
	s.cr.ctx = ctx
	s.cr.mu.Unlock()
}

// Read returns decoded-ish (metadata-stripped) audio bytes.
func (s *Stream) Read(p []byte) (int, error) { return s.body.Read(p) }

// Close releases the native stream. It is idempotent.
func (s *Stream) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// Codec is the stream's detected codec.
func (s *Stream) Codec() Codec { return s.codec }

// ContentType is the response Content-Type including parameters.
func (s *Stream) ContentType() string { return s.contentType }

// FinalURL is the URL after redirects and playlist resolution.
func (s *Stream) FinalURL() *url.URL { return s.finalURL }

// ICYName is the station name advertised by icy-name, if any.
func (s *Stream) ICYName() string { return s.icyName }

// ICYGenre is the genre advertised by icy-genre, if any.
func (s *Stream) ICYGenre() string { return s.icyGenre }

// ICYMetaInt is the negotiated ICY metadata interval in audio bytes, or 0.
func (s *Stream) ICYMetaInt() int {
	if s.icy == nil {
		return 0
	}
	return s.icy.metaint
}

// Metadata returns the latest ICY StreamTitle split into artist and title.
func (s *Stream) Metadata() (artist, title string) {
	if s.icy == nil {
		return "", ""
	}
	return s.icy.metadata()
}

// Open resolves redirects and one level of .pls/.m3u playlist, opens the
// stream and classifies its codec. The returned Stream uses ctx for the
// handshake; call SetContext before reading the body for longer than the open
// deadline allows.
func Open(ctx context.Context, cfg Config, rawURL string) (*Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalize()
	return open(ctx, cfg, rawURL, 0, 0, map[string]bool{})
}

func open(ctx context.Context, cfg Config, rawURL string, playlistDepth, redirects int, seen map[string]bool) (*Stream, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("stream URL must be an absolute http:// or https:// URL")
	}
	u.Fragment = ""
	key := u.Redacted()
	if seen[key] {
		return nil, errors.New("stream URL redirects to itself")
	}
	seen[key] = true
	if strings.EqualFold(path.Ext(u.Path), ".m3u8") {
		return nil, ErrHLS
	}

	conn, err := dial(ctx, cfg, u)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", safeURL(u), err)
	}
	s := &Stream{conn: conn, finalURL: u}
	cleanup := func() { _ = s.Close() }

	cr := &connReader{conn: conn, ctx: ctx, idle: cfg.IdleTimeout}
	br := bufio.NewReaderSize(cr, 16<<10)
	s.cr, s.br = cr, br

	if err := s.writeRequest(ctx, cfg, u); err != nil {
		cleanup()
		return nil, fmt.Errorf("request %s: %w", safeURL(u), err)
	}
	status, reason, headers, err := readResponseHead(br, cfg.MaxHeaderBytes)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("response %s: %w", safeURL(u), err)
	}
	if status >= 300 && status < 400 {
		location := headers["location"]
		if location == "" {
			cleanup()
			return nil, fmt.Errorf("HTTP %d from %s without a Location header", status, safeURL(u))
		}
		next, err := u.Parse(location)
		if err != nil || next.Host == "" || (next.Scheme != "http" && next.Scheme != "https") {
			cleanup()
			return nil, fmt.Errorf("HTTP %d from %s has an unusable Location", status, safeURL(u))
		}
		if u.Scheme == "https" && next.Scheme == "http" {
			cleanup()
			return nil, errors.New("refusing an https to http redirect")
		}
		cleanup()
		if redirects >= cfg.MaxRedirects {
			return nil, fmt.Errorf("more than %d redirects", cfg.MaxRedirects)
		}
		return open(ctx, cfg, next.String(), playlistDepth, redirects+1, seen)
	}
	if status < 200 || status >= 300 {
		cleanup()
		return nil, &HTTPError{StatusCode: status, Reason: reason, URL: safeURL(u)}
	}
	if status == 204 {
		cleanup()
		return nil, errors.New("server returned no content")
	}
	if enc := strings.ToLower(headers["content-encoding"]); enc != "" && enc != "identity" {
		cleanup()
		return nil, fmt.Errorf("compressed responses are not supported (content-encoding %q)", enc)
	}
	s.contentType = headers["content-type"]
	s.icyName = headers["icy-name"]
	s.icyGenre = headers["icy-genre"]

	body, icy, err := buildBody(br, headers)
	if err != nil {
		cleanup()
		return nil, err
	}
	s.icy = icy

	ct := mediaType(s.contentType)
	if isPlaylistResponse(ct, u.Path) {
		if strings.EqualFold(path.Ext(u.Path), ".m3u8") || ct == "application/vnd.apple.mpegurl" {
			cleanup()
			return nil, ErrHLS
		}
		if playlistDepth >= 1 {
			cleanup()
			return nil, errors.New("nested playlists are not supported")
		}
		data, err := readBounded(body, cfg.MaxPlaylistBytes)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("read playlist %s: %w", safeURL(u), err)
		}
		target, err := parsePlaylist(data, u)
		cleanup()
		if err != nil {
			return nil, err
		}
		return open(ctx, cfg, target.String(), playlistDepth+1, redirects, seen)
	}

	codec, classified, err := classify(ct, body)
	if err != nil {
		cleanup()
		return nil, err
	}
	s.codec = codec
	s.body = classified
	if err := s.guardLeadingID3(); err != nil {
		cleanup()
		return nil, err
	}
	return s, nil
}

// maxID3TagBytes bounds a leading ID3v2 tag. go-mp3 reads the 28-bit syncsafe
// size and allocates that many bytes before decoding anything, so a stream
// that declares 256 MiB would OOM a small device; radio streams either have no
// tag or a small one, so a large declaration is refused instead.
//
// The numeric cap is a second line of defence, not the guarantee: because this
// function always consumes the ten-byte ID3 header (masked size or not) before
// go-mp3 sees the stream, go-mp3's own skipTags can never enter its ID3 branch
// and can never reach its allocating read. Do not "optimize" that header
// consumption away.
const maxID3TagBytes = 64 << 10

// guardLeadingID3 reads the first ten bytes of the body, checks a leading
// ID3v2 tag, and refuses (or skips) it before go-mp3 can act on the declared
// size. The prefix survives in front of the stream, so no audio is lost.
func (s *Stream) guardLeadingID3() error {
	pr := &prefixReader{r: s.body}
	s.body = pr
	if err := pr.fill(10); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read stream prefix: %w", err)
	}
	if len(pr.prefix) < 10 || string(pr.prefix[:3]) != "ID3" {
		return nil
	}
	size := syncSafeSize(pr.prefix[6:10])
	if size > maxID3TagBytes {
		return fmt.Errorf("ID3v2 tag declares %d bytes, above the %d byte limit", size, maxID3TagBytes)
	}
	// The ten header bytes are still pending in the prefix reader; consume the
	// whole tag (header plus payload) so go-mp3 starts at the first frame. An
	// ID3v2.4 footer is another ten bytes after the payload.
	total := 10 + size
	if pr.prefix[3] == 4 && pr.prefix[5]&0x10 != 0 {
		total += 10
	}
	if _, err := io.CopyN(io.Discard, pr, int64(total)); err != nil {
		return fmt.Errorf("skip ID3v2 tag: %w", err)
	}
	return nil
}

// syncSafeSize decodes the four-byte syncsafe integer of an ID3v2 header.
func syncSafeSize(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

func dial(ctx context.Context, cfg Config, u *url.URL) (*capsulad.TCPStream, error) {
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("URL has no host")
	}
	plain := u.Scheme == "http"
	port := 80
	if !plain {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("URL has an invalid port")
		}
		port = n
	}
	dialCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	return capsulad.OpenTCP(dialCtx, cfg.Env, host, uint16(port), capsulad.TCPOptions{
		TLS:        !plain,
		Roots:      "system",
		ServerName: host,
	})
}

func (s *Stream) writeRequest(ctx context.Context, cfg Config, u *url.URL) error {
	wctx, cancel := context.WithTimeout(ctx, cfg.HeaderTimeout)
	defer cancel()
	payload := buildRequest(cfg, u)
	for len(payload) > 0 {
		n, err := s.conn.Write(wctx, payload)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

// buildRequest renders the one HTTP/1.1 request a radio stream needs. Header
// names are fixed, so no URL or header value can inject a second header.
func buildRequest(cfg Config, u *url.URL) []byte {
	target := u.EscapedPath()
	if target == "" {
		target = "/"
	}
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", target)
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	fmt.Fprintf(&b, "User-Agent: %s\r\n", cfg.UserAgent)
	b.WriteString("Accept: */*\r\n")
	b.WriteString("Accept-Encoding: identity\r\n")
	b.WriteString("Icy-MetaData: 1\r\n")
	b.WriteString("Connection: close\r\n")
	if u.User != nil {
		password, _ := u.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
		fmt.Fprintf(&b, "Authorization: Basic %s\r\n", token)
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

func buildBody(br *bufio.Reader, headers map[string]string) (io.Reader, *icyReader, error) {
	var body io.Reader = br
	if te := strings.ToLower(strings.TrimSpace(headers["transfer-encoding"])); te != "" {
		// Only the framing we actually decode is accepted; a stack such as
		// "gzip, chunked" would otherwise silently pass the gzip bytes on.
		if te != "chunked" {
			return nil, nil, fmt.Errorf("unsupported transfer-encoding %q", te)
		}
		body = newChunkedReader(br)
	} else if raw, ok := headers["content-length"]; ok {
		length, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || length < 0 {
			return nil, nil, fmt.Errorf("invalid content-length %q", raw)
		}
		body = io.LimitReader(br, length)
	}
	var icy *icyReader
	if raw, ok := headers["icy-metaint"]; ok {
		metaint, err := strconv.Atoi(strings.TrimSpace(raw))
		if err == nil && metaint > 0 && metaint <= 1<<20 {
			icy = newICYReader(body, metaint)
			body = icy
		}
	}
	return body, icy, nil
}

func readResponseHead(br *bufio.Reader, maxBytes int) (int, string, map[string]string, error) {
	remaining := maxBytes
	for range 8 {
		line, used, err := readLine(br, remaining)
		if err != nil {
			return 0, "", nil, err
		}
		remaining -= used
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return 0, "", nil, fmt.Errorf("malformed status line %q", line)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil || code < 100 || code > 999 {
			return 0, "", nil, fmt.Errorf("malformed status line %q", line)
		}
		headers, used, err := readHeaders(br, remaining)
		if err != nil {
			return 0, "", nil, err
		}
		remaining -= used
		if code >= 100 && code < 200 {
			continue
		}
		reason := ""
		if len(parts) > 2 {
			reason = strings.Join(parts[2:], " ")
		}
		return code, reason, headers, nil
	}
	return 0, "", nil, errors.New("too many interim responses")
}

func readHeaders(br *bufio.Reader, remaining int) (map[string]string, int, error) {
	headers := make(map[string]string, 16)
	used := 0
	for {
		line, n, err := readLine(br, remaining-used)
		if err != nil {
			return nil, used, err
		}
		used += n
		if line == "" {
			return headers, used, nil
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, used, fmt.Errorf("malformed response header %q", line)
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		if name == "" {
			return nil, used, errors.New("empty response header name")
		}
		value := strings.TrimSpace(line[colon+1:])
		if previous, ok := headers[name]; ok {
			headers[name] = previous + ", " + value
		} else {
			headers[name] = value
		}
	}
}

func classify(contentType string, body io.Reader) (Codec, io.Reader, error) {
	if codec, ok := codecFromContentType(contentType); ok {
		if codec == CodecUnknown {
			return CodecUnknown, body, fmt.Errorf("unsupported codec: unknown (content-type %q)", contentType)
		}
		return codec, body, nil
	}
	// No usable Content-Type: read a prefix to identify the container. The
	// prefix reader stays in front of the stream, so no audio is lost.
	pr := &prefixReader{r: body}
	_ = pr.fill(12)
	codec := codecFromMagic(pr.prefix)
	if codec == CodecUnknown {
		if len(pr.prefix) == 0 {
			return CodecUnknown, pr, errors.New("stream ended before any audio arrived")
		}
		return CodecUnknown, pr, fmt.Errorf("unsupported codec: unrecognised stream (content-type %q)", contentType)
	}
	return codec, pr, nil
}

func mediaType(raw string) string {
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

func codecFromContentType(ct string) (Codec, bool) {
	switch ct {
	case "":
		return CodecUnknown, false
	case "audio/mpeg", "audio/mp3", "audio/mpeg3", "audio/x-mpeg", "audio/x-mp3":
		return CodecMP3, true
	case "audio/aac", "audio/aacp", "audio/x-aac", "audio/he-aac", "audio/x-hx-aac-adts":
		return CodecAAC, true
	case "application/ogg", "audio/ogg", "audio/opus", "audio/vorbis":
		return CodecOgg, true
	case "audio/flac", "audio/x-flac":
		return CodecFLAC, true
	case "audio/mp4", "audio/x-m4a", "application/mp4":
		return CodecMP4, true
	case "audio/wav", "audio/x-wav", "audio/wave":
		return CodecWAV, true
	case "application/vnd.apple.mpegurl", "audio/mpegurl", "audio/x-mpegurl", "application/x-mpegurl":
		return CodecHLS, true
	case "application/octet-stream", "binary/octet-stream", "application/x-unknown",
		"text/plain", "multipart/x-mixed-replace":
		return CodecUnknown, false
	default:
		return CodecUnknown, false
	}
}

func codecFromMagic(b []byte) Codec {
	switch {
	case len(b) >= 3 && string(b[:3]) == "ID3":
		return CodecMP3
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return CodecOgg
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return CodecFLAC
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return CodecMP4
	case len(b) >= 4 && (string(b[:4]) == "RIFF" || string(b[:4]) == "RF64"):
		return CodecWAV
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		// Twelve-bit ADTS sync has layer bits 00; MPEG audio never does.
		if (b[1]>>1)&0x03 == 0 {
			return CodecAAC
		}
		return CodecMP3
	}
	return CodecUnknown
}

func isPlaylistType(ct string) bool {
	switch ct {
	case "audio/x-scpls", "application/pls+xml", "audio/x-mpegurl", "audio/mpegurl",
		"application/vnd.apple.mpegurl", "application/x-mpegurl":
		return true
	}
	return false
}

// isPlaylistResponse decides whether the response body is a playlist. The URL
// path is weak evidence: a server that streams audio from /radio.m3u with
// content-type audio/mpeg must not be parsed as a playlist. The path is only
// trusted when the content type is missing or explicitly opaque.
func isPlaylistResponse(contentType, urlPath string) bool {
	if isPlaylistType(contentType) {
		return true
	}
	switch contentType {
	case "", "application/octet-stream", "binary/octet-stream", "application/x-unknown",
		"text/plain", "text/uri-list":
		return isPlaylistPath(urlPath)
	}
	return false
}

func isPlaylistPath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".pls", ".m3u", ".m3u8":
		return true
	}
	return false
}

// prefixReader fills a small prefix for codec detection and then serves that
// prefix before the rest of the stream.
type prefixReader struct {
	r      io.Reader
	prefix []byte
	off    int
}

func (p *prefixReader) fill(n int) error {
	for len(p.prefix) < n {
		buf := make([]byte, n-len(p.prefix))
		read, err := p.r.Read(buf)
		if read > 0 {
			p.prefix = append(p.prefix, buf[:read]...)
		}
		if err != nil {
			return err
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (p *prefixReader) Read(out []byte) (int, error) {
	if p.off < len(p.prefix) {
		n := copy(out, p.prefix[p.off:])
		p.off += n
		return n, nil
	}
	return p.r.Read(out)
}

func safeURL(u *url.URL) string {
	if u == nil {
		return "invalid URL"
	}
	copy := *u
	copy.User = nil
	copy.RawQuery = ""
	copy.Fragment = ""
	return copy.String()
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("playlist exceeds %d bytes", limit)
	}
	return data, nil
}
