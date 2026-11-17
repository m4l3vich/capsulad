package stream

import (
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
)

// icyReader strips Shoutcast/Icecast interleaved metadata. After every
// metaint bytes of audio the server sends one length byte (in 16-byte units)
// followed by that many bytes of "StreamTitle='…';" metadata.
type icyReader struct {
	r         io.Reader
	metaint   int
	remaining int

	mu     sync.Mutex
	title  string
	artist string
}

func newICYReader(r io.Reader, metaint int) *icyReader {
	return &icyReader{r: r, metaint: metaint, remaining: metaint}
}

func (r *icyReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		if err := r.readMetadata(); err != nil {
			return 0, err
		}
		r.remaining = r.metaint
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= n
	return n, err
}

func (r *icyReader) readMetadata() error {
	var length [1]byte
	if _, err := io.ReadFull(r.r, length[:]); err != nil {
		return err
	}
	size := int(length[0]) * 16
	if size == 0 {
		return nil
	}
	block := make([]byte, size)
	if _, err := io.ReadFull(r.r, block); err != nil {
		return err
	}
	artist, title := parseICYMetadata(block)
	r.mu.Lock()
	r.artist, r.title = artist, title
	r.mu.Unlock()
	return nil
}

func (r *icyReader) metadata() (artist, title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.artist, r.title
}

// parseICYMetadata extracts StreamTitle from one metadata block. The value is
// conventionally "Artist - Title"; a value without that separator is reported
// as a title only, never invented into an artist.
func parseICYMetadata(block []byte) (artist, title string) {
	s := strings.TrimRight(string(block), "\x00")
	const prefix = "StreamTitle="
	start := strings.Index(s, prefix)
	if start < 0 {
		return "", ""
	}
	value := s[start+len(prefix):]
	value = strings.TrimLeft(value, " ")
	if value == "" {
		return "", ""
	}
	if value[0] == '\'' || value[0] == '"' {
		quote := value[0]
		value = value[1:]
		if end := strings.IndexByte(value, quote); end >= 0 {
			value = value[:end]
		}
	} else if semi := strings.IndexByte(value, ';'); semi >= 0 {
		value = value[:semi]
	}
	value = strings.ReplaceAll(value, `\'`, `'`)
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if at := strings.Index(value, " - "); at > 0 && at+3 < len(value) {
		return strings.TrimSpace(value[:at]), strings.TrimSpace(value[at+3:])
	}
	return "", value
}

var errNoStreamURL = errors.New("playlist contains no http(s) stream URL")

// parsePlaylist resolves the first stream URL in a PLS, M3U or bare-URL
// playlist. Relative entries are resolved against the playlist URL.
func parsePlaylist(data []byte, base *url.URL) (*url.URL, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errNoStreamURL
	}
	if strings.Contains(text, "#EXT-X-") {
		return nil, ErrHLS
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "[playlist]") {
		if target, ok := parsePLS(text); ok {
			return resolvePlaylistURL(base, target)
		}
		return nil, errNoStreamURL
	}
	if u, err := parseAbsoluteHTTP(trimmed); err == nil {
		return u, nil
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if target, err := resolvePlaylistURL(base, line); err == nil {
			return target, nil
		}
	}
	return nil, errNoStreamURL
}

func parsePLS(text string) (string, bool) {
	var first string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		value := strings.TrimSpace(line[eq+1:])
		if key == "file" || (strings.HasPrefix(key, "file") && isDigits(key[4:])) {
			if first == "" || key == "file1" {
				first = value
			}
		}
	}
	return first, first != ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func resolvePlaylistURL(base *url.URL, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"'`)
	parsed, err := base.Parse(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errNoStreamURL
	}
	return parsed, nil
}

func parseAbsoluteHTTP(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errNoStreamURL
	}
	return parsed, nil
}
