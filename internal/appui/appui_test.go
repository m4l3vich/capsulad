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

func TestBuiltinGuidesExistAndExplainRecovery(t *testing.T) {
	for _, id := range []string{"capsulad.clock", "capsulad.ring"} {
		doc, ok := BuiltinGuide(id)
		if !ok {
			t.Fatalf("%s has no built-in guide", id)
		}
		text := string(doc)
		for _, want := range []string{"## Controls", "## Troubleshooting"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s guide missing %q", id, want)
			}
		}
		if strings.Contains(text, "http://") || strings.Contains(text, "https://") {
			t.Fatalf("%s guide contains an external URL", id)
		}
	}
	if _, ok := BuiltinGuide("org.example.missing"); ok {
		t.Fatal("unknown built-in guide accepted")
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

// The settings kit is a locally built Tailwind + daisyUI stylesheet, not a
// hand-written component clone and not a CDN reference. Both palettes the Web
// UI offers must be present so a theme message is only an attribute swap.
func TestSettingsKitIsLocalDaisyUIWithBothThemes(t *testing.T) {
	css := string(KitCSS())
	if len(css) > 160*1024 {
		t.Fatalf("kit is %d bytes, want at most 160 KiB", len(css))
	}
	for _, want := range []string{
		"daisyUI", ".btn", ".card", ".input", ".textarea", ".select", ".checkbox", ".alert", ".badge",
		"[data-theme=capsulad]", "[data-theme=capsulad-dark]", "color-scheme: dark",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("kit missing %q", want)
		}
	}
	for _, bad := range []string{"@import", "@font-face", "url(http", "url('http", `url("http`} {
		if strings.Contains(css, bad) {
			t.Errorf("kit contains %q", bad)
		}
	}
}

func TestWrapDocumentCarriesTheFrameThemeBootstrap(t *testing.T) {
	out := string(WrapDocument([]byte("<html><head></head><body>hi</body></html>")))
	for _, want := range []string{
		"capsulad-theme=",
		"function applyTheme",
		"data-theme",
		"capsulad.settings-ui/1:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("wrapped document missing %q", want)
		}
	}
	if !strings.Contains(HeaderContentSecurityPolicy(), "sandbox allow-scripts") ||
		!strings.Contains(HeaderContentSecurityPolicy(), "connect-src 'none'") {
		t.Fatal("frame CSP changed")
	}
}
