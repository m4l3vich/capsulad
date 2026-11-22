package webui

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/settings"
)

// The application detail view loads and saves its boot preference over the
// same-origin JSON route. A missing registration used to fall through to the
// SPA and answer with index.html, which the browser reported as invalid JSON.
func TestAppBootRouteReadsAndWritesJSON(t *testing.T) {
	env := newTestEnv(t)
	reg, _ := testRegistryWithGuide(t)
	env.srv.Apps = reg
	user, err := settings.OpenUser(filepath.Join(t.TempDir(), "settings.json"), env.srv.Logs, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.srv.UserSettings = user

	client := env.client(t)
	loginSession(t, env, client)
	token := csrfToken(t, client, env.ts.URL)
	bootURL := env.ts.URL + "/api/apps/org.example.guide/boot"

	code, body, headers := doJSON(t, client, http.MethodGet, bootURL, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("boot read = %d %v", code, body)
	}
	if contentType := headers.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("boot content type = %q, want JSON", contentType)
	}
	if body["enabled"] != false || body["editable"] != true {
		t.Fatalf("initial boot state = %v", body)
	}
	revision, ok := body["revision"].(float64)
	if !ok || revision == 0 {
		t.Fatalf("boot revision = %v", body["revision"])
	}

	code, body, _ = doJSON(t, client, http.MethodPut, bootURL,
		map[string]any{"enabled": true, "revision": revision},
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK || body["enabled"] != true {
		t.Fatalf("boot write = %d %v", code, body)
	}

	code, body, _ = doJSON(t, client, http.MethodGet, bootURL, nil, nil)
	if code != http.StatusOK || body["enabled"] != true {
		t.Fatalf("boot re-read = %d %v", code, body)
	}
}
