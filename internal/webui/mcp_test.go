package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	webv1 "capsulad/gen/go/capsulad/web/v1"
	"capsulad/internal/assets"
	"capsulad/internal/websecurity"
)

// --- harness ---------------------------------------------------------------

func mcpPost(t *testing.T, env *testEnv, key, session string, body any) (int, http.Header, map[string]any) {
	t.Helper()
	return mcpPostHeaders(t, env, key, session, body, nil)
}

func mcpPostHeaders(t *testing.T, env *testEnv, key, session string, body any, headers map[string]string) (int, http.Header, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/mcp", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if session != "" {
		req.Header.Set(mcpSessionHeader, session)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("response is not JSON: %v (%q)", err, raw)
		}
	}
	return resp.StatusCode, resp.Header, out
}

func mcpInitialize(t *testing.T, env *testEnv, key string) string {
	t.Helper()
	return mcpInitializeVersion(t, env, key, mcpProtocolVersion)
}

func mcpInitializeVersion(t *testing.T, env *testEnv, key, version string) string {
	t.Helper()
	code, header, body := mcpPost(t, env, key, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %v", code, body)
	}
	session := header.Get(mcpSessionHeader)
	if session == "" {
		t.Fatal("initialize did not return an Mcp-Session-Id")
	}
	result, _ := body["result"].(map[string]any)
	if got, _ := result["protocolVersion"].(string); got == "" {
		t.Fatalf("initialize result has no protocolVersion: %v", body)
	}
	return session
}

// mcpPostAny decodes the response without assuming an object, so a batch's
// JSON array is inspectable.
func mcpPostAny(t *testing.T, env *testEnv, key, session string, body any) (int, any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/mcp", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	if session != "" {
		req.Header.Set(mcpSessionHeader, session)
	}
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("response is not JSON: %v (%q)", err, raw)
		}
	}
	return resp.StatusCode, out
}

// mcpPostRaw is the goroutine-safe POST helper: tests that must observe a call
// still in flight cannot call t.Fatal off the test goroutine.
func mcpPostRaw(env *testEnv, key, session string, body any) (int, map[string]any) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/mcp", bytes.NewReader(data))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if session != "" {
		req.Header.Set(mcpSessionHeader, session)
	}
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func operatorKey(t *testing.T, env *testEnv, name string) string {
	t.Helper()
	token, _, err := env.store.CreateAPIKeyWithScopes(name, []string{websecurity.ScopeOperator})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func callTool(t *testing.T, env *testEnv, key, session, name string, args map[string]any) (bool, map[string]any) {
	t.Helper()
	code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	if code != http.StatusOK {
		t.Fatalf("tools/call %s status = %d, body = %v", name, code, body)
	}
	if rpcErr := body["error"]; rpcErr != nil {
		t.Fatalf("tools/call %s protocol error: %v", name, rpcErr)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s has no result: %v", name, body)
	}
	isError, _ := result["isError"].(bool)
	return isError, result
}

func resultJSON(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool result has no content: %v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	out := map[string]any{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("tool content is not JSON: %v (%q)", err, text)
	}
	return out
}

func toolNames(t *testing.T, env *testEnv, key, session string) []string {
	t.Helper()
	code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	})
	if code != http.StatusOK {
		t.Fatalf("tools/list status = %d, body = %v", code, body)
	}
	result, _ := body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	return names
}

// --- transport and authorization -------------------------------------------

func TestMCPRequiresAValidAPIKey(t *testing.T) {
	env := newTestEnv(t)
	code, header, body := mcpPost(t, env, "", "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 did not advertise the bearer realm")
	}
	if body["error"] == nil {
		t.Fatalf("401 body is not a JSON-RPC error: %v", body)
	}
	if code, _, _ := mcpPost(t, env, "capsulad_not-a-key", "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}); code != http.StatusUnauthorized {
		t.Fatalf("unknown key status = %d, want 401", code)
	}
	// An X-API-Key header is the other accepted credential form.
	token, _, err := env.store.CreateAPIKey(websecurity.ScopeStatus)
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ = mcpPostHeaders(t, env, "", "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"},
		map[string]string{"X-API-Key": token})
	if code != http.StatusOK {
		t.Fatalf("X-API-Key initialize status = %d", code)
	}
}

func TestMCPLegacyKeyStaysReadOnly(t *testing.T) {
	env := newTestEnv(t)
	// A key created the way the browser and `auth init` create one records no
	// scopes: it must keep the legacy status/certificate grant only.
	legacy, _, err := env.store.CreateAPIKey("legacy")
	if err != nil {
		t.Fatal(err)
	}
	session := mcpInitialize(t, env, legacy)
	names := toolNames(t, env, legacy, session)
	if len(names) != 2 || names[0] != "get_status" || names[1] != "get_certificate" {
		t.Fatalf("legacy key tools = %v, want only get_status and get_certificate", names)
	}
	code, _, body := mcpPost(t, env, legacy, session, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "launch_app", "arguments": map[string]any{"app_id": "capsulad.clock"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	rpcErr, _ := body["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != mcpCodeForbidden {
		t.Fatalf("legacy key launch_app error = %v, want forbidden", body["error"])
	}
	// The status read is allowed and reads the live daemon.
	isErr, result := callTool(t, env, legacy, session, "get_status", map[string]any{})
	if isErr {
		t.Fatalf("legacy get_status failed: %v", result)
	}
	if status := resultJSON(t, result); status["device"] == nil {
		t.Fatalf("get_status has no device: %v", status)
	}
}

func TestMCPOperatorSeesEveryTool(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)
	names := toolNames(t, env, key, session)
	for _, want := range []string{
		"get_status", "get_state", "update_settings", "launch_app", "stop_run",
		"trigger_action", "update_app_settings", "media_command", "bluetooth_command",
		"wifi_command", "record_sample", "train_wakeword", "cancel_training",
		"delete_wakeword", "dismiss_detection", "clock_command", "create_api_key",
		"revoke_api_key", "install_app", "remove_app", "upload_asset", "delete_asset",
		"begin_upload", "upload_chunk", "complete_upload", "abort_upload",
		"list_assets", "list_api_keys", "get_certificate",
	} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("operator tools missing %q: %v", want, names)
		}
	}
}

func TestMCPTransportSurface(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	// GET is refused: this server offers no server-initiated stream.
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}

	// A notification is acknowledged with an empty 202.
	code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	})
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("notification status = %d body = %v, want 202 empty", code, body)
	}

	// A request without the session header is refused with 400, as the binding
	// asks; an unknown id is 404.
	code, _, body = mcpPost(t, env, key, "", map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list"})
	if code != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("session-less tools/list = %d %v", code, body)
	}
	code, _, body = mcpPost(t, env, key, "not-a-session", map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list"})
	if code != http.StatusNotFound || body["error"] == nil {
		t.Fatalf("unknown-session tools/list = %d %v", code, body)
	}

	// A wrong content type is refused before any work.
	req, _ = http.NewRequest(http.MethodPost, env.ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err = env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form content type status = %d, want 415", resp.StatusCode)
	}

	// A concrete cross-origin request is refused.
	code, _, body = mcpPostHeaders(t, env, key, session,
		map[string]any{"jsonrpc": "2.0", "id": 6, "method": "ping"},
		map[string]string{"Origin": "https://evil.example"})
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d body = %v, want 403", code, body)
	}
	// A same-origin browser context is accepted.
	code, _, _ = mcpPostHeaders(t, env, key, session,
		map[string]any{"jsonrpc": "2.0", "id": 8, "method": "ping"},
		map[string]string{"Origin": env.ts.URL})
	if code != http.StatusOK {
		t.Fatalf("same-origin ping status = %d", code)
	}

	// An SSE-only client receives an event stream carrying the same JSON-RPC.
	req, _ = http.NewRequest(http.MethodPost, env.ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(mcpSessionHeader, session)
	resp, err = env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(string(raw), `"jsonrpc":"2.0"`) {
		t.Fatalf("SSE response = %q %q", resp.Header.Get("Content-Type"), raw)
	}
}

func TestMCPUnknownMethodAndTool(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	code, _, body := mcpPost(t, env, key, session, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	rpcErr, _ := body["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != mcpCodeMethodNotFound {
		t.Fatalf("unknown method error = %v", body["error"])
	}

	code, _, body = mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "no_such_tool"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	rpcErr, _ = body["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != mcpCodeInvalidParams {
		t.Fatalf("unknown tool error = %v", body["error"])
	}

	// Unknown arguments are rejected rather than silently ignored.
	code, _, body = mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "launch_app", "arguments": map[string]any{"application": "x"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	rpcErr, _ = body["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != mcpCodeInvalidParams {
		t.Fatalf("unknown argument error = %v", body["error"])
	}
}

// --- shared operation semantics --------------------------------------------

func TestMCPToolCallDrivesTheSharedOperation(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	isErr, result := callTool(t, env, key, session, "launch_app", map[string]any{"app_id": "capsulad.clock"})
	if isErr {
		t.Fatalf("launch_app failed: %v", result)
	}
	launch := resultJSON(t, result)["launch"].(map[string]any)
	if launch["run_id"] != "run-2" || launch["spawned_new_process"] != true {
		t.Fatalf("launch result = %v", launch)
	}

	// A daemon-side refusal arrives as a failed tool call, not a protocol error,
	// and carries the shared operation's typed error.
	isErr, result = callTool(t, env, key, session, "update_settings", map[string]any{"expected_revision": 0})
	if !isErr {
		t.Fatalf("update_settings without a revision succeeded: %v", result)
	}
	failure := resultJSON(t, result)["error"].(map[string]any)
	if failure["code"] != "ERROR_CODE_INVALID_ARGUMENT" {
		t.Fatalf("update_settings error = %v", failure)
	}

	// get_state reuses the browser snapshot builder, including the access view.
	isErr, result = callTool(t, env, key, session, "get_state", map[string]any{"topic": "access"})
	if isErr {
		t.Fatalf("get_state failed: %v", result)
	}
	access := resultJSON(t, result)["access"].(map[string]any)
	keys, _ := access["keys"].([]any)
	if len(keys) == 0 {
		t.Fatalf("access snapshot has no keys: %v", access)
	}
}

func TestMCPCreateAPIKeyGrantsExplicitScopes(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	isErr, result := callTool(t, env, key, session, "create_api_key", map[string]any{
		"name": "agent", "scopes": []string{"operator"},
	})
	if isErr {
		t.Fatalf("create_api_key failed: %v", result)
	}
	created := resultJSON(t, result)
	secret, _ := created["value"].(string)
	if secret == "" {
		t.Fatalf("create_api_key returned no secret: %v", created)
	}
	if scopes, _ := created["scopes"].([]any); len(scopes) != 1 || scopes[0] != websecurity.ScopeOperator {
		t.Fatalf("created scopes = %v", created["scopes"])
	}
	// The minted key really works on the MCP surface.
	newSession := mcpInitialize(t, env, secret)
	if len(toolNames(t, env, secret, newSession)) < 3 {
		t.Fatal("minted operator key does not see the operator tools")
	}
	// A legacy (status) key cannot mint keys.
	legacy, _, err := env.store.CreateAPIKey("legacy")
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := mcpPost(t, env, legacy, mcpInitialize(t, env, legacy), map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "create_api_key", "arguments": map[string]any{"name": "nope"}},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	rpcErr, _ := body["error"].(map[string]any)
	if rpcErr == nil || int(rpcErr["code"].(float64)) != mcpCodeForbidden {
		t.Fatalf("legacy create_api_key error = %v", body["error"])
	}
	// list_api_keys reports the grants.
	isErr, result = callTool(t, env, key, session, "list_api_keys", map[string]any{})
	if isErr {
		t.Fatalf("list_api_keys failed: %v", result)
	}
	if keys, _ := resultJSON(t, result)["keys"].([]any); len(keys) < 2 {
		t.Fatalf("list_api_keys = %v", keys)
	}
}

// --- uploads and file parity ----------------------------------------------

func TestMCPAssetUploadListDelete(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	body := []byte("RIFF....WAVE")
	isErr, result := callTool(t, env, key, session, "upload_asset", map[string]any{
		"path": "sounds/ding.wav", "content_base64": base64.StdEncoding.EncodeToString(body),
	})
	if isErr {
		t.Fatalf("upload_asset failed: %v", result)
	}
	asset := resultJSON(t, result)["asset"].(map[string]any)
	if asset["id"] != "sounds/ding.wav" || int(asset["size_bytes"].(float64)) != len(body) {
		t.Fatalf("asset = %v", asset)
	}
	stored, err := os.ReadFile(filepath.Join(catalog.Root(), "sounds", "ding.wav"))
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("stored asset = %q, %v", stored, err)
	}

	isErr, result = callTool(t, env, key, session, "list_assets", map[string]any{})
	if isErr {
		t.Fatalf("list_assets failed: %v", result)
	}
	list := resultJSON(t, result)
	if assets, _ := list["assets"].([]any); len(assets) != 1 {
		t.Fatalf("list_assets = %v", list)
	}
	if int(list["total_bytes"].(float64)) != len(body) {
		t.Fatalf("total_bytes = %v", list["total_bytes"])
	}

	// A traversal attempt is refused by the store, not silently normalized.
	isErr, result = callTool(t, env, key, session, "upload_asset", map[string]any{
		"path": "../escape", "content_base64": base64.StdEncoding.EncodeToString(body),
	})
	if !isErr {
		t.Fatalf("traversal upload succeeded: %v", result)
	}

	isErr, result = callTool(t, env, key, session, "delete_asset", map[string]any{"path": "sounds/ding.wav"})
	if isErr {
		t.Fatalf("delete_asset failed: %v", result)
	}
	if len(catalog.List("")) != 0 {
		t.Fatal("delete_asset left the file in place")
	}
}

func TestMCPInstallAndRemoveApp(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	archive := []byte("not-a-real-archive-but-the-daemon-owns-validation")
	isErr, result := callTool(t, env, key, session, "install_app", map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(archive),
		"filename":       "new-1.2.0.tar.gz",
	})
	if isErr {
		t.Fatalf("install_app failed: %v", result)
	}
	installed := resultJSON(t, result)
	if installed["app_id"] != "org.example.new" || installed["replaced"] != true {
		t.Fatalf("install result = %v", installed)
	}
	env.back.mu.Lock()
	received := append([]byte(nil), env.back.installedArchive...)
	env.back.mu.Unlock()
	if !bytes.Equal(received, archive) {
		t.Fatalf("daemon received %q, want %q", received, archive)
	}

	isErr, result = callTool(t, env, key, session, "remove_app", map[string]any{"app_id": "org.example.idle"})
	if isErr {
		t.Fatalf("remove_app failed: %v", result)
	}
	env.back.mu.Lock()
	removed := append([]string(nil), env.back.uninstallCalls...)
	env.back.mu.Unlock()
	if len(removed) != 1 || removed[0] != "org.example.idle" {
		t.Fatalf("uninstall calls = %v", removed)
	}
}

// --- sessions and lifecycle ------------------------------------------------

func TestMCPSessionDeleteEndsIt(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	req, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(mcpSessionHeader, session)
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	code, _, body := mcpPost(t, env, key, session, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if code != http.StatusNotFound || body["error"] == nil {
		t.Fatalf("deleted session still usable: %d %v", code, body)
	}
}

func TestMCPSessionIsBoundToItsKey(t *testing.T) {
	env := newTestEnv(t)
	first := operatorKey(t, env, "first")
	second := operatorKey(t, env, "second")
	session := mcpInitialize(t, env, first)

	// Another valid key cannot borrow the session.
	code, _, body := mcpPost(t, env, second, session, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if code != http.StatusNotFound || body["error"] == nil {
		t.Fatalf("a different key used another key's session: %d %v", code, body)
	}
	// It cannot end it either.
	req, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+second)
	req.Header.Set(mcpSessionHeader, session)
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign DELETE status = %d, want 404", resp.StatusCode)
	}
	// The owner is unaffected.
	if got := len(toolNames(t, env, first, session)); got < 3 {
		t.Fatalf("owner tools/list after a foreign attempt = %d", got)
	}
}

func TestMCPTrainingWaitAndSessionCancel(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	// A bounded wait returns the observed stage while the run is still active.
	isErr, result := callTool(t, env, key, session, "train_wakeword", map[string]any{
		"display_name": "capsula", "wait_seconds": 0.2,
	})
	if isErr {
		t.Fatalf("train_wakeword failed: %v", result)
	}
	trained := resultJSON(t, result)
	if trained["training_id"] == "" || trained["status"] != "running" || trained["stage"] != "fit" {
		t.Fatalf("training outcome = %v", trained)
	}

	// Ending the session cancels the daemon-side training run.
	req, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(mcpSessionHeader, session)
	resp, err := env.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case <-env.back.trainingCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("ending the MCP session did not cancel the training run")
	}
}

func TestMCPConcurrentSessionLifecycle(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")

	// Helpers below deliberately avoid t.Fatal: they run on goroutines, where a
	// Fatalf would exit the wrong goroutine.
	client := env.ts.Client()
	post := func(session string, body any) int {
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/mcp", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		if session != "" {
			req.Header.Set(mcpSessionHeader, session)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	errs := make(chan string, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "initialize",
				"params": map[string]any{"protocolVersion": mcpProtocolVersion},
			})
			req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/mcp", bytes.NewReader(data))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := client.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			session := resp.Header.Get(mcpSessionHeader)
			if resp.StatusCode != http.StatusOK || session == "" {
				errs <- "initialize did not create a session"
				return
			}
			if code := post(session, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}); code != http.StatusOK {
				errs <- "tools/list on the new session failed"
				return
			}
			req, _ = http.NewRequest(http.MethodDelete, env.ts.URL+"/mcp", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set(mcpSessionHeader, session)
			resp, err = client.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				errs <- "DELETE did not end the session"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for message := range errs {
		t.Error(message)
	}
	env.srv.mcpMu.Lock()
	remaining := len(env.srv.mcpSessions)
	env.srv.mcpMu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d sessions survived their DELETE", remaining)
	}
}

func TestMCPToolsCloseCancelsSessions(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)
	// wait_seconds proves the daemon-side training stream is established before
	// the session is torn down; without it, shutdown could race stream creation.
	if isErr, result := callTool(t, env, key, session, "train_wakeword", map[string]any{
		"display_name": "capsula", "wait_seconds": 0.2,
	}); isErr {
		t.Fatalf("train_wakeword failed: %v", result)
	}
	env.srv.closeAllMCPSessions()
	select {
	case <-env.back.trainingCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel the training run")
	}
}

// --- schema -----------------------------------------------------------------

func TestMCPSchemaIsGeneratedFromTheProto(t *testing.T) {
	byName := map[string]mcpTool{}
	for _, tool := range mcpToolCatalogue {
		byName[tool.Name] = tool
	}
	settings, ok := byName["update_settings"]
	if !ok {
		t.Fatal("update_settings tool is missing")
	}
	props, _ := settings.Schema["properties"].(map[string]any)
	for _, want := range []string{"expected_revision", "display", "assistant", "microphone", "volume"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("update_settings schema missing %q: %v", want, props)
		}
	}
	bluetooth := byName["bluetooth_command"]
	props, _ = bluetooth.Schema["properties"].(map[string]any)
	if _, ok := props["set_adapter_name"]; !ok {
		t.Fatalf("bluetooth schema = %v", props)
	}
	if _, ok := props["address"]; ok {
		t.Fatal("bluetooth schema exposes the raw address")
	}
	media := byName["media_command"]
	props, _ = media.Schema["properties"].(map[string]any)
	if enum, _ := props["action"].(map[string]any)["enum"].([]string); len(enum) == 0 {
		t.Fatalf("media action schema = %v", props["action"])
	}
}

// TestAccessSnapshotShowsKeyScopesToTheBrowser is the browser-surface proof for
// the Access view: the same WebSocket snapshot the SPA renders must carry each
// key's grant, or the page cannot tell a status key from an administration key.
func TestAccessSnapshotShowsKeyScopesToTheBrowser(t *testing.T) {
	env := newTestEnv(t)
	operatorKey(t, env, "agent")
	if _, _, err := env.store.CreateAPIKey("probe"); err != nil {
		t.Fatal(err)
	}

	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 41, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_ACCESS},
	}})
	snap := conn.waitSnapshot(t, 41)
	if snap.GetAccess() == nil {
		t.Fatalf("access snapshot missing: %+v", snap)
	}
	scopes := map[string][]string{}
	for _, key := range snap.GetAccess().GetKeys() {
		scopes[key.GetName()] = key.GetScopes()
	}
	if got := scopes["agent"]; len(got) != 1 || got[0] != websecurity.ScopeOperator {
		t.Fatalf("agent scopes in the browser snapshot = %v", got)
	}
	if got := scopes["probe"]; len(got) != 1 || got[0] != websecurity.ScopeStatus {
		t.Fatalf("probe scopes in the browser snapshot = %v", got)
	}
}
