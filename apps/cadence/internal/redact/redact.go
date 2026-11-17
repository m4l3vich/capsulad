// Package redact keeps the Cadence device token and any configured credential
// out of the app's log. A device token is a bearer secret: whoever holds it can
// speak for the device. Every log line and every validation error in this app
// therefore goes through a Redactor, and a URL is shortened before it is echoed
// so a pasted query cannot leak a token either.
package redact

import (
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Placeholder is what a scrubbed secret becomes. Its fixed text makes a
// redaction visible in a log without revealing length or prefix.
const Placeholder = "[redacted]"

// Redactor scrubs a changing set of secrets from text. Concurrent use is safe:
// settings updates call Set while logging reads.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string // non-empty, longest first so a prefix cannot survive
}

// Set replaces the secret set. Empty strings are ignored; duplicates are
// dropped. The longest secret is replaced first so a secret that contains
// another cannot leave the remainder behind.
func (r *Redactor) Set(secrets ...string) {
	cleaned := make([]string, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		cleaned = append(cleaned, s)
	}
	sort.Slice(cleaned, func(i, j int) bool { return len(cleaned[i]) > len(cleaned[j]) })
	r.mu.Lock()
	r.secrets = cleaned
	r.mu.Unlock()
}

// Text returns s with every registered secret replaced. A nil Redactor is
// usable and scrubs nothing.
func (r *Redactor) Text(s string) string {
	if r == nil || s == "" {
		return s
	}
	r.mu.RLock()
	secrets := r.secrets
	r.mu.RUnlock()
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, Placeholder)
	}
	return s
}

// Error renders an error for a log line: its text is scrubbed, and when the
// error wraps a URL the query and fragment are removed first. A nil error is
// the empty string.
func (r *Redactor) Error(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.URL != "" {
		text = strings.Replace(text, uerr.URL, URL(uerr.URL), 1)
	}
	return r.Text(text)
}

// URL shortens a URL to scheme://host/path for a log or a message: userinfo,
// query and fragment are dropped so a pasted credential cannot ride along. It
// never fails; an unparsable value is reduced to its scheme and host when the
// standard parser can recover one, else to a fixed placeholder.
func URL(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "(empty)"
	}
	u, err := url.Parse(text)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Keep only what precedes a query or fragment: never echo the tail of
		// something the parser could not understand.
		if i := strings.IndexAny(text, "?#"); i >= 0 {
			text = text[:i]
		}
		if len(text) > 120 {
			text = text[:120] + "…"
		}
		return text
	}
	return u.Scheme + "://" + u.Host + "/" + strings.TrimPrefix(u.Path, "/")
}
