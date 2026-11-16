package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"capsulad/internal/assets"
)

// newAssetsEnv attaches a real store to the shared test server. The handler
// reads Server.Assets per request, so attaching after construction is enough.
func newAssetsEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	env := newTestEnv(t)
	root := t.TempDir()
	catalog, err := assets.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	return env, root
}

func rawRequest(t *testing.T, c *http.Client, method, target string, body io.Reader, contentType string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func assetEntry(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	entry, ok := body["asset"].(map[string]any)
	if !ok {
		t.Fatalf("response has no asset entry: %v", body)
	}
	return entry
}

func listedAssets(t *testing.T, c *http.Client, base string) ([]any, float64) {
	t.Helper()
	code, body, _ := doJSON(t, c, http.MethodGet, base+"/api/assets", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("list assets: %d %v", code, body)
	}
	list, _ := body["assets"].([]any)
	total, _ := body["total_bytes"].(float64)
	return list, total
}

func TestAssetsRequireBrowserSessionAndCSRF(t *testing.T) {
	env, _ := newAssetsEnv(t)
	c := env.client(t)

	code, _ := rawRequest(t, c, http.MethodGet, env.ts.URL+"/api/assets", nil, "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}
	code, _ = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=a.wav", strings.NewReader("x"), "application/octet-stream", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upload = %d, want 401", code)
	}

	// A static API key is an automation read credential, not browser admin.
	apiKey, _, err := env.store.CreateAPIKey("assets-test")
	if err != nil {
		t.Fatal(err)
	}
	code, _ = rawRequest(t, c, http.MethodGet, env.ts.URL+"/api/assets", nil, "", map[string]string{"X-API-Key": apiKey})
	if code != http.StatusUnauthorized {
		t.Fatalf("API-key list = %d, want 401", code)
	}
	code, _ = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/assets?path=a.wav", nil, "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated delete = %d, want 401", code)
	}

	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	// The mutation methods all require the CSRF token; a raw upload body is not
	// consumed by sessionOnly's form parsing, so the header is the only proof.
	code, body := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=a.wav", strings.NewReader("x"), "application/octet-stream", nil)
	if code != http.StatusForbidden {
		t.Fatalf("upload without CSRF = %d %v, want 403", code, body)
	}
	code, body = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=a.wav", strings.NewReader("x"), "application/octet-stream",
		map[string]string{"X-Capsulad-CSRF": token, "Origin": "https://attacker.example"})
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin upload = %d %v, want 403", code, body)
	}
	code, body = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=audio/a.wav", strings.NewReader("x"), "application/octet-stream",
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("raw upload with header CSRF = %d %v", code, body)
	}

	// DELETE is state-changing too: no CSRF or a foreign origin must never
	// remove the file, while the same-origin CSRF-bearing request succeeds.
	code, body = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/assets?path=audio/a.wav", nil, "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("delete without CSRF = %d %v, want 403", code, body)
	}
	code, body = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/assets?path=audio/a.wav", nil, "",
		map[string]string{"X-Capsulad-CSRF": token, "Origin": "https://attacker.example"})
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin delete = %d %v, want 403", code, body)
	}
	if _, err := os.Stat(filepath.Join(env.srv.Assets.Root(), "audio", "a.wav")); err != nil {
		t.Fatalf("rejected delete removed the file: %v", err)
	}
	code, body = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/assets?path=audio/a.wav", nil, "",
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("CSRF-bearing delete = %d %v", code, body)
	}
}

func TestAssetsUploadListOverwriteDelete(t *testing.T) {
	env, root := newAssetsEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)

	wav := []byte("RIFF-test-wave")
	code, body := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=audio/replies/hello.wav",
		bytes.NewReader(wav), "audio/wav", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("upload = %d %v", code, body)
	}
	entry := assetEntry(t, body)
	sum := sha256.Sum256(wav)
	if entry["id"] != "audio/replies/hello.wav" || entry["path"] != "/assets/audio/replies/hello.wav" ||
		entry["media_type"] != "audio/wav" || entry["sha256"] != hex.EncodeToString(sum[:]) ||
		entry["size_bytes"] != float64(len(wav)) {
		t.Fatalf("upload entry = %v", entry)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "audio", "replies", "hello.wav"))
	if err != nil || !bytes.Equal(onDisk, wav) {
		t.Fatalf("stored file = %q, %v", onDisk, err)
	}

	list, total := listedAssets(t, c, env.ts.URL)
	if len(list) != 1 || total != float64(len(wav)) {
		t.Fatalf("list = %v total %v", list, total)
	}
	listed := list[0].(map[string]any)
	if listed["id"] != "audio/replies/hello.wav" || listed["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("listed entry = %v", listed)
	}

	// Another file in a sibling directory keeps the listing recursive.
	code, body = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=images/logo.png",
		strings.NewReader("png"), "image/png", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("second upload = %d %v", code, body)
	}
	if list, total = listedAssets(t, c, env.ts.URL); len(list) != 2 || total != float64(len(wav)+3) {
		t.Fatalf("recursive list = %v total %v", list, total)
	}

	// Overwrite is atomic and updates the hash without leaving staging files.
	replacement := []byte("RIFF-replaced")
	code, body = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=audio/replies/hello.wav",
		bytes.NewReader(replacement), "audio/wav", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("overwrite = %d %v", code, body)
	}
	if entry = assetEntry(t, body); entry["sha256"] == hex.EncodeToString(sum[:]) {
		t.Fatalf("overwrite kept the old hash: %v", entry)
	}
	staged, err := filepath.Glob(filepath.Join(root, "audio", "replies", ".capsulad-upload-*"))
	if err != nil || len(staged) != 0 {
		t.Fatalf("staging files survived: %v %v", staged, err)
	}

	code, body = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/assets?path=audio/replies/hello.wav", nil, "", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("delete = %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "audio", "replies", "hello.wav")); !os.IsNotExist(err) {
		t.Fatalf("file survived delete: %v", err)
	}
	code, _ = rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/assets?path=audio/replies/hello.wav", nil, "", map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", code)
	}
}

func TestAssetsListIsLiveAndSkipsSymlinkedDirectories(t *testing.T) {
	env, root := newAssetsEnv(t)
	c := env.client(t)
	loginSession(t, env, c)

	// No startup snapshot: a file that appears on disk after the first listing
	// is visible on the next request.
	if list, total := listedAssets(t, c, env.ts.URL); len(list) != 0 || total != 0 {
		t.Fatalf("initial list = %v total %v", list, total)
	}
	if err := os.MkdirAll(filepath.Join(root, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "audio", "late.wav"), []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, total := listedAssets(t, c, env.ts.URL)
	if len(list) != 1 || total != 4 {
		t.Fatalf("live list = %v total %v", list, total)
	}

	// A symlinked directory is never followed into the catalog, and a symlinked
	// file is never published.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.wav"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.wav"), filepath.Join(root, "linked.wav")); err != nil {
		t.Fatal(err)
	}
	list, total = listedAssets(t, c, env.ts.URL)
	if len(list) != 1 || list[0].(map[string]any)["id"] != "audio/late.wav" || total != 4 {
		t.Fatalf("list followed a symlink: %v total %v", list, total)
	}
}

func TestAssetsRejectTraversalAbsoluteReservedSymlinkAndOversize(t *testing.T) {
	env, root := newAssetsEnv(t)
	c := env.client(t)
	loginSession(t, env, c)
	token := csrfToken(t, c, env.ts.URL)
	headers := map[string]string{"X-Capsulad-CSRF": token}

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.wav")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"../escape.wav", "/etc/passwd", "audio/../../escape.wav", `audio\escape.wav`,
		".capsulad-upload-123", "audio//double.wav", "./dot.wav", "linkdir/inner.wav",
	} {
		code, body := rawRequest(t, c, http.MethodPost,
			env.ts.URL+"/api/assets?path="+url.QueryEscape(target), strings.NewReader("x"), "application/octet-stream", headers)
		if code != http.StatusBadRequest {
			t.Fatalf("upload %q = %d %v, want 400", target, code, body)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "secret" {
		t.Fatalf("outside file changed: %q %v", data, err)
	}

	// A missing or ambiguous path parameter is a client error, not a write.
	for _, query := range []string{"", "?path=a.wav&path=b.wav"} {
		code, _ := rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets"+query, strings.NewReader("x"), "application/octet-stream", headers)
		if code != http.StatusBadRequest {
			t.Fatalf("upload with query %q = %d, want 400", query, code)
		}
	}

	// A directory is never removed through the asset route.
	if err := os.MkdirAll(filepath.Join(root, "audio", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, body := rawRequest(t, c, http.MethodDelete, env.ts.URL+"/api/assets?path=audio/sub", nil, "", headers)
	if code != http.StatusBadRequest {
		t.Fatalf("delete directory = %d %v, want 400", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "audio", "sub")); err != nil {
		t.Fatalf("directory was removed: %v", err)
	}

	// One byte over the store limit is rejected and leaves nothing behind.
	oversize := io.LimitReader(zeroReader{}, assets.MaxFileBytes+1)
	code, body = rawRequest(t, c, http.MethodPost, env.ts.URL+"/api/assets?path=big.bin", oversize, "application/octet-stream", headers)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d %v, want 413", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "big.bin")); !os.IsNotExist(err) {
		t.Fatalf("oversized file was published: %v", err)
	}
	staged, err := filepath.Glob(filepath.Join(root, ".capsulad-upload-*"))
	if err != nil || len(staged) != 0 {
		t.Fatalf("oversized upload left staging files: %v %v", staged, err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// Store exhaustion and a full device map to distinct statuses, and no client
// message may carry a host path or filesystem detail.
func TestAssetErrorsMapStorageExhaustion(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		want   string
		leak   string
	}{
		{
			name:   "store budget",
			err:    assets.ErrStoreFull,
			status: http.StatusRequestEntityTooLarge,
			want:   "asset store is full",
		},
		{
			name:   "device full",
			err:    fmt.Errorf("write asset: %w", syscall.ENOSPC),
			status: http.StatusInsufficientStorage,
			want:   "device storage is full",
			leak:   "write asset",
		},
		{
			name:   "unexpected io failure",
			err:    fmt.Errorf("/var/lib/capsulad/assets: %w", errors.New("boom")),
			status: http.StatusInternalServerError,
			want:   "asset store operation failed",
			leak:   "/var/lib/capsulad",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			rec := httptest.NewRecorder()
			srv.writeAssetError(rec, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response body: %v", err)
			}
			if body["error"] != tc.want {
				t.Fatalf("message = %q, want %q", body["error"], tc.want)
			}
			if tc.leak != "" && strings.Contains(body["error"], tc.leak) {
				t.Fatalf("message %q leaks %q", body["error"], tc.leak)
			}
		})
	}
}
