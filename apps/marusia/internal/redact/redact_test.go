package redact

import (
	"strings"
	"testing"
)

func TestURLDropsQueryAndFragment(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "signed query and fragment",
			in:   "https://tts.example/voice/reply.wav?X-Amz-Signature=deadbeef&Expires=1700000000#t=1",
			want: "https://tts.example/voice/reply.wav",
		},
		{
			name: "query only",
			in:   "http://vc.go.mail.ru/media/a.ogg?token=secret",
			want: "http://vc.go.mail.ru/media/a.ogg",
		},
		{
			name: "fragment only",
			in:   "https://tts.example/reply.wav#sig=secret",
			want: "https://tts.example/reply.wav",
		},
		{
			name: "no path keeps the host",
			in:   "https://tts.example?sig=secret",
			want: "https://tts.example",
		},
		{
			name: "userinfo is a credential too",
			in:   "https://user:pass@tts.example/a.wav?sig=secret",
			want: "https://tts.example/a.wav",
		},
		{
			name: "port survives",
			in:   "https://tts.example:8443/a/b.wav?X-Amz-Signature=deadbeef",
			want: "https://tts.example:8443/a/b.wav",
		},
		{
			name: "empty input stays empty",
			in:   "   ",
			want: "",
		},
		{
			name: "plain path without a tail is unchanged",
			in:   "https://tts.example/a.wav",
			want: "https://tts.example/a.wav",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := URL(tc.in); got != tc.want {
				t.Fatalf("URL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got := URL(tc.in); strings.ContainsAny(got, "?#") {
				t.Fatalf("URL(%q) = %q still carries a query or fragment", tc.in, got)
			}
		})
	}
}

// TestURLMalformedInputIsSafe covers the bypass an escaping error would
// otherwise open: url.Parse fails, so a parser-based redaction that falls back
// to the raw string would echo the signed query verbatim.
func TestURLMalformedInputIsSafe(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "invalid escape before a signed query",
			in:   "https://tts.example/%zz?X-Amz-Signature=deadbeef&X-Amz-Credential=secret",
			want: "https://tts.example/%zz",
		},
		{
			name: "no scheme but a signed query",
			in:   "tts.example/a.wav?token=secret#frag",
			want: "tts.example/a.wav",
		},
		{
			name: "relative garbage with a fragment",
			in:   "not a url#sig=secret",
			want: "not a url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := URL(tc.in)
			if got != tc.want {
				t.Fatalf("URL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "deadbeef") ||
				strings.ContainsAny(got, "?#") {
				t.Fatalf("URL(%q) = %q leaked the signed tail", tc.in, got)
			}
		})
	}
}

// TestURLStripsControlCharacters covers a server-supplied URL forging a second
// log line.
func TestURLStripsControlCharacters(t *testing.T) {
	got := URL("https://tts.example/a.wav\nmarusia: forged line?token=secret")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("URL() kept a control character: %q", got)
	}
	if strings.Contains(got, "token") {
		t.Fatalf("URL() kept the query: %q", got)
	}
}

func TestAccountAndSessionAreShortened(t *testing.T) {
	const account = "account-0123456789"
	const session = "session-abcdef0123456789"

	if got := Account(account); got != "account-…" {
		t.Fatalf("Account(%q) = %q", account, got)
	}
	if got := SessionID(session); got != "session-…" {
		t.Fatalf("SessionID(%q) = %q", session, got)
	}
	for _, full := range []string{account, session} {
		shortened := shorten(full)
		if strings.Contains(shortened, full) {
			t.Fatalf("shorten(%q) = %q still holds the value", full, shortened)
		}
	}
	if got := SessionID(session); len(got) >= len(session)/2 {
		t.Fatalf("SessionID(%q) = %q keeps too much", session, got)
	}
	if got := Account("short"); got != "…" {
		t.Fatalf("Account of a short value = %q, want the ellipsis alone", got)
	}
	if got := SessionID(""); got != "…" {
		t.Fatalf("SessionID(\"\") = %q", got)
	}
	if got := Account("  padded-account  "); got != "padded-a…" {
		t.Fatalf("Account() must shorten the trimmed value, got %q", got)
	}
}
