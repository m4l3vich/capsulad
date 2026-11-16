package appui

import (
	"bytes"
	"strings"
	"testing"
)

func TestWrapDocumentInjectsPolicyBeforeAppContent(t *testing.T) {
	doc := []byte("<!doctype html>\n<html><head><title>x</title></head><body><script>window.app = true</script></body></html>")
	out := WrapDocument(doc)
	s := string(out)
	if !strings.HasPrefix(strings.ToLower(s), "<!doctype") {
		t.Fatalf("doctype moved: %q", s[:40])
	}
	csp := strings.Index(s, "http-equiv=\"Content-Security-Policy\"")
	kit := strings.Index(s, "id=\"capsulad-ui-kit\"")
	bridge := strings.Index(s, "id=\"capsulad-ui-bridge\"")
	app := strings.Index(s, "window.app = true")
	if csp < 0 || kit < 0 || bridge < 0 || app < 0 {
		t.Fatalf("missing injected pieces csp=%d kit=%d bridge=%d app=%d", csp, kit, bridge, app)
	}
	if !(csp < app && kit < app && bridge < app) {
		t.Fatal("injected policy or code appears after application script")
	}
	for _, want := range []string{"default-src 'none'", "connect-src 'none'", "form-action 'none'", "frame-src 'none'"} {
		if !strings.Contains(s, want) {
			t.Fatalf("CSP missing %q", want)
		}
	}
	if bytes.Contains(out, []byte(`src="http`)) {
		t.Fatal("kit must not reference the network")
	}
}

func TestWrapDocumentWithoutHead(t *testing.T) {
	out := string(WrapDocument([]byte("<html><body>hi</body></html>")))
	if !strings.Contains(out, "<head>") || strings.Index(out, "<head>") > strings.Index(out, "<body>") {
		t.Fatalf("no head wrapping: %q", out[:80])
	}
}

func TestBuiltinDocumentsExistAndAreSelfContained(t *testing.T) {
	for _, id := range []string{"capsulad.clock", "capsulad.ring"} {
		doc, err := BuiltinHTML(id, id[len("capsulad."):]+".html")
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if !bytes.Contains(doc, []byte("CapsuladUI")) {
			t.Fatalf("%s does not use the bridge API", id)
		}
		if bytes.Contains(doc, []byte("http://")) || bytes.Contains(doc, []byte("https://")) {
			t.Fatalf("%s contains an external URL", id)
		}
	}
	if _, err := BuiltinHTML("org.example.missing", "x.html"); err == nil {
		t.Fatal("unknown built-in document accepted")
	}
}
