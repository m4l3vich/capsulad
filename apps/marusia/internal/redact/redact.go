// Package redact keeps values that must not be logged out of log lines and
// operator messages.
//
// A vendor TTS URL carries its signature in the query string, so only
// scheme://host/path may be written: the query and the fragment are the
// credential. Session ids and account ids are bearer-adjacent material, so they
// are shortened to a correlation prefix instead of being echoed.
package redact

import (
	"net/url"
	"strings"
	"unicode"
)

// identifierKeep is how many leading characters of an id survive redaction:
// enough to match two log lines, not enough to be the id.
const identifierKeep = 8

// URL keeps scheme://host/path and drops the query, the fragment and any
// userinfo. Malformed input is safe by construction: when the string cannot be
// parsed it is cut at the first '?' or '#' instead, so an unparseable value can
// never leak a signed query. Control characters are removed so a
// server-supplied URL cannot forge an extra log line.
func URL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return clean(cutSignedTail(trimmed))
	}
	// Userinfo is a credential in the same class as the query.
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return clean(parsed.String())
}

// Account shortens an account id; empty input yields the ellipsis alone rather
// than an empty field.
func Account(account string) string { return shorten(account) }

// SessionID shortens a session id. Session ids are bearer material: the result
// is for correlation only and is never a usable credential.
func SessionID(session string) string { return shorten(session) }

func shorten(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= identifierKeep {
		return "…"
	}
	return string(runes[:identifierKeep]) + "…"
}

// ErrorText returns err's message with every occurrence of rawURL replaced by
// its redacted form. A transport error quotes the request URL, query included,
// so the raw string must not reach an operator message through it.
func ErrorText(err error, rawURL string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return message
	}
	return strings.ReplaceAll(message, trimmed, URL(trimmed))
}

// cutSignedTail removes everything from the first '?' or '#' on.
func cutSignedTail(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}
