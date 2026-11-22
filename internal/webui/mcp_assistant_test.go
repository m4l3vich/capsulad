package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/websecurity"
)

func assistantKey(t *testing.T, env *testEnv) string {
	t.Helper()
	key, _, err := env.store.CreateAPIKeyWithScopes("assistant", []string{websecurity.ScopeAssistant})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func requireMCPCode(t *testing.T, env *testEnv, key, session, name string, args any, want int) {
	t.Helper()
	code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	rpcErr, _ := body["error"].(map[string]any)
	if code != http.StatusOK || rpcErr["code"] != float64(want) {
		t.Fatalf("%s(%v): got HTTP %d %v, want MCP error %d", name, args, code, body, want)
	}
}

func requireToolResult(t *testing.T, env *testEnv, key, session, name string, args map[string]any) map[string]any {
	t.Helper()
	failed, result := callTool(t, env, key, session, name, args)
	if failed {
		t.Fatalf("%s(%v): %v", name, args, result)
	}
	return resultJSON(t, result)
}

func requireAbsent(t *testing.T, value map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := value[key]; ok {
			t.Fatalf("unexpected %q in %v", key, value)
		}
	}
}

func TestMCPAssistantAdvertisesOnlyItsToolVariants(t *testing.T) {
	env := newTestEnv(t)
	operator := operatorKey(t, env, "operator")
	operatorSession := mcpInitialize(t, env, operator)
	created := requireToolResult(t, env, operator, operatorSession, "create_api_key", map[string]any{
		"name": "agent", "scopes": []string{websecurity.ScopeAssistant},
	})
	key, _ := created["value"].(string)
	if scopes, _ := created["scopes"].([]any); key == "" || len(scopes) != 1 || scopes[0] != websecurity.ScopeAssistant {
		t.Fatalf("assistant key response = %v", created)
	}
	session := mcpInitialize(t, env, key)
	var names []string
	schemas := map[string]map[string]any{}
	for _, raw := range listedMCPTools(t, env, key, session) {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		names = append(names, name)
		schemas[name] = tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	}
	want := []string{"get_status", "get_certificate", "get_state", "update_settings", "launch_app", "stop_run", "media_command", "clock_command"}
	if !slices.Equal(names, want) {
		t.Fatalf("assistant tools = %v, want %v", names, want)
	}
	topic := schemas["get_state"]["topic"].(map[string]any)["enum"]
	if raw, _ := json.Marshal(topic); string(raw) != `["overview","settings","application"]` {
		t.Fatalf("assistant topics = %v", topic)
	}
	settings := schemas["update_settings"]
	requireAbsent(t, settings, "assistant", "microphone", "muted_indicator", "hostname")
	revision := settings["expected_revision"].(map[string]any)["anyOf"].([]any)
	if len(revision) != 2 || revision[0].(map[string]any)["type"] != "integer" ||
		revision[1].(map[string]any)["type"] != "string" || revision[1].(map[string]any)["pattern"] != "^[0-9]+$" {
		t.Fatalf("assistant revision schema = %v", revision)
	}
	displaySchema := settings["display"].(map[string]any)
	display := displaySchema["properties"].(map[string]any)
	requireAbsent(t, display, "default_app")
	mask := display["update_fields"].(map[string]any)
	if items, _ := json.Marshal(mask["items"].(map[string]any)["enum"]); string(items) != `["brightness","brightness_mode"]` || mask["minItems"] != float64(1) {
		t.Fatalf("assistant display mask = %v", mask)
	}
	conditions, _ := json.Marshal(displaySchema["allOf"])
	if string(conditions) != `[{"if":{"properties":{"update_fields":{"contains":{"const":"brightness"}}}},"then":{"required":["brightness"]}},{"if":{"properties":{"update_fields":{"contains":{"const":"brightness_mode"}}}},"then":{"required":["brightness_mode"]}}]` {
		t.Fatalf("assistant masked value requirements = %s", conditions)
	}
	requireAbsent(t, schemas["launch_app"], "arguments")
	if scopes, _ := json.Marshal(schemas["clock_command"]["app_id"].(map[string]any)["enum"]); string(scopes) != `["capsulad.clock"]` {
		t.Fatalf("assistant clock app_id = %s", scopes)
	}
	if status := requireToolResult(t, env, key, session, "get_status", map[string]any{}); status["device"] == nil {
		t.Fatalf("assistant status = %v", status)
	}
	_, cert := callTool(t, env, key, session, "get_certificate", map[string]any{})
	if value, _ := cert["structuredContent"].(map[string]any); value["pem"] == nil {
		t.Fatalf("assistant certificate = %v", cert)
	}
}

func TestMCPAssistantRejectsPrivilegedCalls(t *testing.T) {
	env, _ := newAssetsEnv(t)
	key := assistantKey(t, env)
	session := mcpInitialize(t, env, key)
	operator := operatorKey(t, env, "operator")
	operatorSession := mcpInitialize(t, env, operator)
	asset := map[string]any{"path": "tone.wav", "content_base64": "dGVzdA=="}
	requireToolResult(t, env, operator, operatorSession, "upload_asset", asset)
	staged := requireToolResult(t, env, operator, operatorSession, "begin_upload", map[string]any{"kind": "asset", "path": "staged.wav"})["upload_id"]
	before := cloneSettings(env.back.settings)

	for _, topic := range []string{"diagnostics", "wakeword", "access", "wifi"} {
		requireMCPCode(t, env, key, session, "get_state", map[string]any{"topic": topic}, mcpCodeForbidden)
	}
	for _, patch := range []string{
		`{"microphone":{"muted":false}}`,
		`{"assistant":{"default_assistant_app":""}}`,
		`{"muted_indicator":{"has_mode":false}}`,
		`{"hostname":{"hostname":"new-host"}}`,
		`{"volume":{"master_volume":0.2},"microphone":null}`,
		`{"display":{"resource_id":"display.segment.front","brightness":0.1}}`,
		`{"display":{"resource_id":"display.segment.front","update_fields":[]}}`,
		`{"display":{"resource_id":"display.segment.front","update_fields":null}}`,
		`{"display":{"resource_id":"display.segment.front","update_fields":["brightness","default_app"],"brightness":0.1}}`,
		`{"display":{"resource_id":"display.segment.front","update_fields":["brightness"],"default_app":"","brightness":0.1}}`,
		`{"display":{"resource_id":"display.segment.front","update_fields":["brightness"],"brightness":0.1,"defaultApp":null}}`,
	} {
		var args map[string]any
		if err := json.Unmarshal([]byte(patch), &args); err != nil {
			t.Fatal(err)
		}
		args["expected_revision"] = before.GetRevision()
		requireMCPCode(t, env, key, session, "update_settings", args, mcpCodeForbidden)
	}
	for _, fields := range []map[string]any{
		{"update_fields": []string{"brightness"}},
		{"update_fields": []string{"brightness"}, "brightness": nil},
		{"update_fields": []string{"brightness_mode"}},
		{"update_fields": []string{"brightness", "brightness_mode"}, "brightness": 0},
	} {
		fields["resource_id"] = "display.segment.front"
		requireMCPCode(t, env, key, session, "update_settings", map[string]any{
			"expected_revision": before.GetRevision(), "display": fields,
		}, mcpCodeInvalidParams)
	}
	assetPath := map[string]any{"path": "tone.wav"}
	for name, args := range map[string]any{
		"list_assets":         map[string]any{},
		"upload_asset":        map[string]any{"path": "denied.wav", "content_base64": "dGVzdA=="},
		"delete_asset":        assetPath,
		"begin_upload":        map[string]any{"kind": "asset", "path": "denied.wav"},
		"upload_chunk":        map[string]any{"upload_id": staged, "content_base64": "dGVzdA=="},
		"complete_upload":    map[string]any{"upload_id": staged},
		"abort_upload":       map[string]any{"upload_id": staged},
		"install_app":        map[string]any{"content_base64": "dGVzdA=="},
		"remove_app":         map[string]any{"app_id": "org.example.player"},
		"list_api_keys":      map[string]any{},
		"create_api_key":     map[string]any{"name": "privileged", "scopes": []string{"operator"}},
		"revoke_api_key":     map[string]any{"name": "operator"},
		"trigger_action":     map[string]any{"app_id": "org.example.player", "action_id": "enroll"},
		"update_app_settings": map[string]any{"app_id": "org.example.player", "expected_revision": 2, "changes": []map[string]any{{"key": "effect", "value_json": `"pulse"`}}},
		"wifi_command":       map[string]any{"scan": map[string]any{}},
		"bluetooth_command":  map[string]any{"set_discoverable": map[string]any{"discoverable": true}},
		"record_sample":      map[string]any{"label": "word", "duration_ms": 1000},
		"delete_sample":      map[string]any{"sample_id": "s1"},
		"train_wakeword":     map[string]any{"display_name": "wake up"},
		"cancel_training":    map[string]any{"training_id": "run"},
		"delete_wakeword":    map[string]any{"model_id": "m1"},
		"dismiss_detection":  map[string]any{"detection_id": "d1"},
	} {
		requireMCPCode(t, env, key, session, name, args, mcpCodeForbidden)
	}
	for _, value := range []any{nil, []any{}, []map[string]any{{"name": "station", "value": "1"}}} {
		requireMCPCode(t, env, key, session, "launch_app", map[string]any{"app_id": "org.example.player", "arguments": value}, mcpCodeForbidden)
	}
	requireMCPCode(t, env, key, session, "clock_command", map[string]any{"app_id": "org.example.player", "stopwatch_start": map[string]any{}}, mcpCodeForbidden)
	requireMCPCode(t, env, key, session, "media_command", map[string]any{"kind": "MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA", "player_id": "bluetooth", "action": "MEDIA_ACTION_UNSPECIFIED"}, mcpCodeForbidden)

	headers := map[string]string{"Authorization": "Bearer " + key}
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/password"}, {http.MethodPost, "/api/logout"},
		{http.MethodGet, "/api/assets"}, {http.MethodPost, "/api/assets?path=denied.wav"}, {http.MethodDelete, "/api/assets?path=tone.wav"},
		{http.MethodPost, "/api/apps"}, {http.MethodDelete, "/api/apps/org.example.player"}, {http.MethodGet, "/ws"},
	} {
		if code, body := rawRequest(t, env.ts.Client(), route.method, env.ts.URL+route.path, nil, "", headers); code != http.StatusUnauthorized {
			t.Fatalf("assistant %s %s = %d %v, want 401", route.method, route.path, code, body)
		}
	}
	if code, body := paPost(t, env.ts.Client(), env, bytes.NewReader([]byte("RIFF....WAVEfmt ")), headers); code != http.StatusForbidden {
		t.Fatalf("assistant PA status = %d %v, want 403", code, body)
	}
	if code, body := rawRequest(t, env.ts.Client(), http.MethodPost, env.ts.URL+"/api/password", nil, "", map[string]string{"Authorization": "Bearer " + operator}); code != http.StatusUnauthorized {
		t.Fatalf("operator key password status = %d %v, want 401", code, body)
	}
	env.back.mu.Lock()
	after := cloneSettings(env.back.settings)
	mutations := len(env.back.launchCalls) + len(env.back.uninstallCalls) + len(env.back.installedArchive) + len(env.back.paCalls) + len(env.back.btCommands)
	clockRunning := env.back.clockRunning
	env.back.mu.Unlock()
	if !proto.Equal(before, after) || mutations != 0 || clockRunning {
		t.Fatalf("denied calls changed daemon state: before=%v after=%v mutations=%d clock_running=%t", before, after, mutations, clockRunning)
	}
	files := requireToolResult(t, env, operator, operatorSession, "list_assets", map[string]any{})["assets"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["id"] != "tone.wav" {
		t.Fatalf("denied calls changed assets: %v", files)
	}
	requireToolResult(t, env, operator, operatorSession, "abort_upload", map[string]any{"upload_id": staged})
	keys := requireToolResult(t, env, operator, operatorSession, "list_api_keys", map[string]any{})["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("denied calls changed keys: %v", keys)
	}
	for _, raw := range keys {
		if raw.(map[string]any)["revoked"] != false {
			t.Fatalf("denied call revoked a key: %v", raw)
		}
	}
}

func TestMCPAssistantControlsSpeakerAndFiltersSnapshots(t *testing.T) {
	env := newTestEnv(t)
	key := assistantKey(t, env)
	session := mcpInitialize(t, env, key)
	operator := operatorKey(t, env, "operator")
	operatorSession := mcpInitialize(t, env, operator)
	env.back.mu.Lock()
	env.back.settings.Values.DefaultAssistantAppId = "org.example.private-assistant"
	env.back.apps[1].LaunchArguments = []*frameworkv1.AppArgumentInfo{{Name: "station", ValueType: "text"}}
	env.back.runs[1].StatusDetail = "private run detail"
	env.back.logs = append(env.back.logs, &frameworkv1.LogRecord{AppId: "org.example.player", Message: "private credential log"})
	env.back.mu.Unlock()
	env.back.setMediaPlayers("org.example.player", &mediav1.PlayerStatus{
		PlayerId: "player", PlaybackState: mediav1.PlaybackState_PLAYBACK_PLAYING, Title: proto.String("Track"),
		SourceName: "https://user:token@station.example", MediaName: proto.String("https://user:token@station.example"),
	})
	env.back.setBluetoothMedia(&frameworkv1.BluetoothMediaState{Present: true, ObservedPlaying: true, DeviceName: "Private phone"})

	overview := requireToolResult(t, env, key, session, "get_state", map[string]any{"topic": "overview"})["overview"].(map[string]any)
	requireAbsent(t, overview, "bluetooth", "paired_devices", "bluetooth_error", "selected_providers")
	if overview["apps"] == nil || overview["runs"] == nil || overview["audio"].(map[string]any)["master_volume"] != 0.4 {
		t.Fatalf("assistant speaker overview = %v", overview)
	}
	for _, raw := range overview["runs"].([]any) {
		requireAbsent(t, raw.(map[string]any), "pid", "status_detail")
	}
	providers, _ := overview["providers"].([]any)
	if len(providers) != 2 || providers[0].(map[string]any)["app_id"] != "org.example.player" {
		t.Fatalf("assistant now playing = %v", providers)
	}
	for _, raw := range providers {
		player := raw.(map[string]any)["players"].([]any)[0].(map[string]any)
		requireAbsent(t, player, "source_name", "media_name")
	}
	settings := requireToolResult(t, env, key, session, "get_state", map[string]any{"topic": "settings"})["settings"].(map[string]any)
	requireAbsent(t, settings, "models", "discovery", "discovery_url", "bluetooth", "bluetooth_error")
	values := settings["values"].(map[string]any)
	requireAbsent(t, values, "default_app_by_resource", "default_assistant_app_id", "wakewords", "hostname", "microphone_muted", "muted_microphone_indicator")
	if settings["revision"] != "5" || values["master_volume"] != 0.4 || values["timezone"] != "UTC" {
		t.Fatalf("assistant settings = %v", settings)
	}
	application := map[string]any{"topic": "application", "app_id": "org.example.player"}
	app := requireToolResult(t, env, key, session, "get_state", application)["application"].(map[string]any)
	requireAbsent(t, app, "schema", "config_revision", "values", "interfaces", "descriptions", "provenance", "logs")
	requireAbsent(t, app["app"].(map[string]any), "actions", "launch_arguments", "entrypoint")
	if app["app"].(map[string]any)["app_id"] != "org.example.player" || app["runs"].([]any)[0].(map[string]any)["run_id"] != "run-2" {
		t.Fatalf("assistant application state = %v", app)
	}
	fullApp := requireToolResult(t, env, operator, operatorSession, "get_state", application)["application"].(map[string]any)
	if fullApp["logs"] == nil || fullApp["values"] == nil || fullApp["schema"] == nil {
		t.Fatalf("operator application lost its full state: %v", fullApp)
	}

	requireToolResult(t, env, key, session, "update_settings", map[string]any{
		"expected_revision": settings["revision"], "volume": map[string]any{"master_volume": 0.25},
	})
	requireToolResult(t, env, key, session, "update_settings", map[string]any{
		"expected_revision": 6, "display": map[string]any{"resource_id": "display.segment.front", "brightness": 0,
			"update_fields": []string{"brightness", "brightness_mode"}, "brightness_mode": "BRIGHTNESS_MODE_AUTO"},
	})
	requireToolResult(t, env, key, session, "launch_app", map[string]any{"app_id": "org.example.player"})
	requireToolResult(t, env, key, session, "stop_run", map[string]any{"app_id": "org.example.player", "run_id": "run-2"})
	requireToolResult(t, env, key, session, "media_command", map[string]any{
		"kind": "MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA", "player_id": "bluetooth", "action": "MEDIA_ACTION_PAUSE",
	})
	clockArgs := map[string]any{"topic": "application", "app_id": "capsulad.clock"}
	clock := requireToolResult(t, env, key, session, "get_state", clockArgs)["application"].(map[string]any)["clock"].(map[string]any)
	if clock["local_time_rfc3339"] != "2026-11-13T12:00:00Z" || clock["timers"] == nil || clock["alarms"] == nil {
		t.Fatalf("assistant clock view = %v", clock)
	}
	requireToolResult(t, env, key, session, "clock_command", map[string]any{"app_id": "capsulad.clock", "stopwatch_start": map[string]any{}})
	clock = requireToolResult(t, env, key, session, "get_state", clockArgs)["application"].(map[string]any)["clock"].(map[string]any)
	if clock["stopwatch"].(map[string]any)["state"] != "STOPWATCH_STATUS_RUNNING" {
		t.Fatalf("assistant clock command did not start the stopwatch: %v", clock)
	}

	requireToolResult(t, env, operator, operatorSession, "update_settings", map[string]any{"expected_revision": 7, "microphone": map[string]any{"muted": true}})
	requireToolResult(t, env, operator, operatorSession, "launch_app", map[string]any{
		"app_id": "org.example.player", "arguments": []map[string]any{{"name": "station", "value": "1"}},
	})
	env.back.mu.Lock()
	defer env.back.mu.Unlock()
	saved := env.back.settings.GetValues()
	brightness, hasBrightness := saved.GetDisplayBrightness()["display.segment.front"]
	if env.back.settings.GetRevision() != 8 || saved.GetMasterVolume() != 0.25 || !saved.GetMicrophoneMuted() ||
		!hasBrightness || brightness != 0 || saved.GetBrightnessMode()["display.segment.front"] != "auto" ||
		saved.GetDefaultAppByResource()["display.segment.front"] != "capsulad.clock" || saved.GetDefaultAssistantAppId() != "org.example.private-assistant" {
		t.Fatalf("shared settings writes = %v", env.back.settings)
	}
	launches := env.back.launchCalls
	if len(launches) != 2 || launches[0].GetAppId() != "org.example.player" || len(launches[0].GetArguments()) != 0 ||
		len(launches[1].GetArguments()) != 1 || launches[1].GetArguments()[0].GetValue() != "1" {
		t.Fatalf("shared app launches = %v", launches)
	}
	if !slices.Equal(env.back.stopCalls, []string{"run-2"}) || !slices.Equal(env.back.btCommands, []frameworkv1.BluetoothMediaCommand{frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PAUSE}) {
		t.Fatalf("shared stop/media commands = %v / %v", env.back.stopCalls, env.back.btCommands)
	}
}

func TestMCPAssistantMediaHidesProviderDetails(t *testing.T) {
	env := newTestEnv(t)
	assistant := assistantKey(t, env)
	assistantSession := mcpInitialize(t, env, assistant)
	operator := operatorKey(t, env, "operator")
	operatorSession := mcpInitialize(t, env, operator)
	const detail = "station https://user:secret@station.example"
	env.back.mu.Lock()
	env.back.mediaCommandDetail = detail
	env.back.mu.Unlock()
	args := map[string]any{"app_id": "org.example.player", "player_id": "p1", "action": "MEDIA_ACTION_NEXT"}

	failed, safe := callTool(t, env, assistant, assistantSession, "media_command", args)
	media := resultJSON(t, safe)["media"].(map[string]any)
	if failed || len(media) != 1 || media["accepted"] != true {
		t.Fatalf("assistant media success = %v", safe)
	}
	if raw, _ := json.Marshal(safe); bytes.Contains(raw, []byte(detail)) {
		t.Fatalf("assistant media leaked detail: %s", raw)
	}
	full := requireToolResult(t, env, operator, operatorSession, "media_command", args)["media"].(map[string]any)
	if full["detail"] != detail {
		t.Fatalf("operator media success lost detail: %v", full)
	}

	env.back.mu.Lock()
	env.back.mediaCommandErr = errors.New(detail)
	env.back.mu.Unlock()
	failed, safe = callTool(t, env, assistant, assistantSession, "media_command", args)
	assistantError := resultJSON(t, safe)["error"].(map[string]any)
	if !failed || assistantError["message"] != "media command failed" {
		t.Fatalf("assistant media error = %v", safe)
	}
	if raw, _ := json.Marshal(safe); bytes.Contains(raw, []byte(detail)) {
		t.Fatalf("assistant media error leaked detail: %s", raw)
	}
	failed, result := callTool(t, env, operator, operatorSession, "media_command", args)
	operatorError := resultJSON(t, result)["error"].(map[string]any)
	if !failed || operatorError["message"] != detail || assistantError["code"] != operatorError["code"] {
		t.Fatalf("assistant or operator media errors changed classification: %v / %v", assistantError, operatorError)
	}
}
