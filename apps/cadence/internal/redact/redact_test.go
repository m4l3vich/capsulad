package redact_test

import (
	"errors"
	"net/url"
	"testing"

	"capsulad/apps/cadence/internal/redact"
)

func TestTextScrubsEveryOccurrence(t *testing.T) {
	var r redact.Redactor
	r.Set("tok_live_abc")
	got := r.Text("connect tok_live_abc token=tok_live_abc")
	if want := "connect [redacted] token=[redacted]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLongestSecretFirst(t *testing.T) {
	var r redact.Redactor
	// The short secret is a prefix of the long one; replacing the short one
	// first would leave "xyz-secret" behind.
	r.Set("abcdef", "abcdefxyz")
	got := r.Text("abcdefxyz")
	if got != redact.Placeholder {
		t.Fatalf("got %q", got)
	}
}

func TestSetReplacesAndIgnoresEmpty(t *testing.T) {
	var r redact.Redactor
	r.Set("one", "", "one")
	if got := r.Text("one"); got != redact.Placeholder {
		t.Fatalf("got %q", got)
	}
	r.Set("two")
	if got := r.Text("one two"); got != "one [redacted]" {
		t.Fatalf("stale secret survived: %q", got)
	}
}

func TestNilRedactorIsSafe(t *testing.T) {
	var r *redact.Redactor
	if got := r.Text("secret"); got != "secret" {
		t.Fatalf("got %q", got)
	}
	if got := r.Error(nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestErrorScrubsURLAndSecret(t *testing.T) {
	var r redact.Redactor
	r.Set("tok_live_abc")
	uerr := &url.Error{Op: "Get", URL: "https://example.com/x?token=tok_live_abc", Err: errors.New("boom")}
	got := r.Error(uerr)
	if got == "" || contains(got, "tok_live_abc") || contains(got, "?token") {
		t.Fatalf("error leaked: %q", got)
	}
}

func TestURLShortening(t *testing.T) {
	cases := map[string]string{
		"https://user:pass@host.example:8443/a/b?x=1#f": "https://host.example:8443/a/b",
		"http://host/":       "http://host/",
		"":                   "(empty)",
		"not a url?secret=1": "not a url",
	}
	for in, want := range cases {
		if got := redact.URL(in); got != want {
			t.Fatalf("URL(%q) = %q want %q", in, got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
