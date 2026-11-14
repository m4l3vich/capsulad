// Package appui serves application-provided settings documents.
//
// A manifest may declare one self-contained HTML document. The document is
// untrusted: it is framed by the Web UI as an opaque-origin document with
// a restrictive CSP, no network access, no forms, no frames, and no host
// session. The only accepted output is a bounded settings-value patch sent
// through the authenticated bridge in the Web UI parent.
package appui

import (
	"bytes"
	_ "embed"
	"fmt"
	"path"
	"strings"
)

// APIVersion is the settings bridge protocol version understood by the
// injected bootstrap and the Web UI host.
const APIVersion = 1

// AssetPath returns the authenticated same-origin endpoint for an app's
// settings document.
func AssetPath(appID string) string {
	return "/api/apps/" + pathEscape(appID) + "/settings-ui"
}

func pathEscape(s string) string {
	// App IDs are restricted by the manifest grammar, so only characters that
	// are legal there need escaping. Kept local to avoid a URL-parser
	// dependency for one deterministic call site.
	return strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "#", "%23", " ", "%20").Replace(s)
}

//go:embed assets/kit.css
var kitCSS []byte

//go:embed assets/bridge.js
var bridgeJS []byte

//go:embed assets/clock.html
var clockHTML []byte

//go:embed assets/ring.html
var ringHTML []byte

var builtinDocuments = map[string]struct {
	file string
	doc  []byte
}{
	"capsula.clock": {"clock.html", clockHTML},
	"capsula.ring":  {"ring.html", ringHTML},
}

// BuiltinHTML returns the embedded document for a compiled-in application.
// The manifest still declares the document, and the returned bytes follow the
// same wrapping rules as a packaged app.
func BuiltinHTML(appID, declaredPath string) ([]byte, error) {
	entry, ok := builtinDocuments[appID]
	if !ok {
		return nil, fmt.Errorf("no built-in settings UI for %s", appID)
	}
	if strings.TrimSpace(declaredPath) == "" {
		return nil, fmt.Errorf("built-in settings UI path is empty")
	}
	if path.Clean(declaredPath) != entry.file {
		return nil, fmt.Errorf("built-in settings UI declaration %q does not match %s", declaredPath, entry.file)
	}
	return append([]byte(nil), entry.doc...), nil
}

// ContentSecurityPolicy is applied both to the served document and inside the
// framed document. Unsafe-inline is limited to script/style because the document
// is self-contained; nothing can be fetched, framed, submitted or resized into
// a host document.
const ContentSecurityPolicy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
	"img-src data:; font-src 'none'; connect-src 'none'; media-src 'none'; object-src 'none'; " +
	"frame-src 'none'; child-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'; navigate-to 'none'"

// HeaderContentSecurityPolicy adds the directives that only apply as an HTTP
// header (frame-ancestors is ignored in a meta element).
func HeaderContentSecurityPolicy() string {
	return ContentSecurityPolicy + "; frame-ancestors 'self'; sandbox allow-scripts"
}

// WrapDocument injects the CSP, the system UI kit and the bridge bootstrap
// ahead of the application document. The kit is local CSS and the bootstrap is
// local JavaScript; neither performs network I/O.
func WrapDocument(doc []byte) []byte {
	payload := fmt.Sprintf(
		"<meta http-equiv=\"Content-Security-Policy\" content=\"%s\">\n<style id=\"capsula-ui-kit\">\n%s\n</style>\n<script id=\"capsula-ui-bridge\">\n%s\n</script>\n",
		ContentSecurityPolicy, kitCSS, bridgeJS)

	lower := bytes.ToLower(doc)
	trimmed := bytes.TrimLeft(doc, "\ufeff \t\r\n")
	offset := len(doc) - len(trimmed)

	// Keep a leading doctype first: it must precede any element for standards
	// mode, and the meta CSP is still parsed into the implicit head.
	if bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<!doctype")) {
		if end := bytes.IndexByte(trimmed, '>'); end >= 0 {
			insert := offset + end + 1
			out := make([]byte, 0, len(doc)+len(payload))
			out = append(out, doc[:insert]...)
			out = append(out, '\n')
			out = append(out, payload...)
			out = append(out, doc[insert:]...)
			return out
		}
	}

	// Prefer inserting immediately after an explicit <head> opening tag so
	// CSP/meta and the kit precede the document's own styles and scripts.
	if idx := bytes.Index(lower, []byte("<head")); idx >= 0 && tagBoundary(lower, idx+5) {
		if end := bytes.IndexByte(lower[idx:], '>'); end >= 0 {
			insert := idx + end + 1
			out := make([]byte, 0, len(doc)+len(payload))
			out = append(out, doc[:insert]...)
			out = append(out, '\n')
			out = append(out, payload...)
			out = append(out, doc[insert:]...)
			return out
		}
	}

	// A document without <head> still gets one; inserting the meta before the
	// <html> element would leave it outside the document element.
	if idx := bytes.Index(lower, []byte("<html")); idx >= 0 && tagBoundary(lower, idx+5) {
		if end := bytes.IndexByte(lower[idx:], '>'); end >= 0 {
			insert := idx + end + 1
			out := make([]byte, 0, len(doc)+len(payload)+13)
			out = append(out, doc[:insert]...)
			out = append(out, []byte("\n<head>")...)
			out = append(out, payload...)
			out = append(out, []byte("</head>")...)
			out = append(out, doc[insert:]...)
			return out
		}
	}

	out := make([]byte, 0, len(payload)+len(doc))
	out = append(out, payload...)
	out = append(out, doc...)
	return out
}

// KitCSS exposes the system stylesheet for documentation and preview tools.
func KitCSS() []byte { return append([]byte(nil), kitCSS...) }

func tagBoundary(s []byte, i int) bool {
	return i >= len(s) || s[i] == '>' || s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '/'
}
