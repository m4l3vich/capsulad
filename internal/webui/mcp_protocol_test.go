package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"capsulad/internal/uichanges"
)

// TestMCPBatchIsVersionDependent pins the revision rule the transport spec
// states: 2025-06-18 removed JSON-RPC batching, while 2025-03-26 still allows
// it. The session's negotiated revision governs.
func TestMCPBatchIsVersionDependent(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")

	old := mcpInitializeVersion(t, env, key, "2025-03-26")
	code, body := mcpPostAny(t, env, key, old, []map[string]any{
		{"jsonrpc": "2.0", "id": 11, "method": "ping"},
		{"jsonrpc": "2.0", "id": 12, "method": "ping"},
	})
	if code != http.StatusOK {
		t.Fatalf("2025-03-26 batch status = %d, body = %v", code, body)
	}
	responses, ok := body.([]any)
	if !ok || len(responses) != 2 {
		t.Fatalf("2025-03-26 batch response = %v", body)
	}

	current := mcpInitializeVersion(t, env, key, "2025-06-18")
	code, body = mcpPostAny(t, env, key, current, []map[string]any{
		{"jsonrpc": "2.0", "id": 13, "method": "ping"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("2025-06-18 batch status = %d, want 400 (body %v)", code, body)
	}
	if errObj, _ := body.(map[string]any)["error"].(map[string]any); errObj == nil {
		t.Fatalf("2025-06-18 batch rejection has no JSON-RPC error: %v", body)
	}

	// A single message is the 2025-06-18 form and still works.
	if code, _, _ := mcpPost(t, env, key, current, map[string]any{
		"jsonrpc": "2.0", "id": 14, "method": "ping",
	}); code != http.StatusOK {
		t.Fatalf("single ping status = %d", code)
	}
}

func TestMCPProtocolVersionHeader(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)
	ping := map[string]any{"jsonrpc": "2.0", "id": 21, "method": "ping"}

	// An unsupported revision is a 400, not a silent fallback.
	code, _, body := mcpPostHeaders(t, env, key, session, ping,
		map[string]string{mcpVersionHeader: "1999-01-01"})
	if code != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("unsupported version header = %d %v, want 400", code, body)
	}
	// The negotiated revision and the earlier revision are both accepted.
	for _, version := range []string{mcpProtocolVersion, "2025-03-26"} {
		if code, _, _ := mcpPostHeaders(t, env, key, session, ping,
			map[string]string{mcpVersionHeader: version}); code != http.StatusOK {
			t.Fatalf("supported version header %q status = %d", version, code)
		}
	}
	// Omitting it is allowed because the session already identifies the
	// revision.
	if code, _, _ := mcpPost(t, env, key, session, ping); code != http.StatusOK {
		t.Fatalf("header-less ping status = %d", code)
	}
}

func TestMCPInitializeReusesSession(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	first := mcpInitialize(t, env, key)

	init := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "initialize",
		"params": map[string]any{"protocolVersion": mcpProtocolVersion},
	}
	code, header, body := mcpPostHeaders(t, env, key, first, init, nil)
	if code != http.StatusOK {
		t.Fatalf("second initialize status = %d, body = %v", code, body)
	}
	if got := header.Get(mcpSessionHeader); got != first {
		t.Fatalf("second initialize minted a new session %q (first %q)", got, first)
	}
	env.srv.mcpMu.Lock()
	live := len(env.srv.mcpSessions)
	env.srv.mcpMu.Unlock()
	if live != 1 {
		t.Fatalf("%d sessions after two initializes", live)
	}
}

func TestMCPAcceptsUnsolicitedResponse(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	// This server sends no requests, so an incoming response is accepted and
	// acknowledged with an empty 202 rather than treated as a bad request.
	code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 9, "result": map[string]any{},
	})
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("unsolicited response = %d %v, want 202 empty", code, body)
	}
}

// TestMCPCancelledNotificationStopsTool proves the explicit cancellation path:
// a notifications/cancelled sent on a second HTTP request stops the tool call
// whose JSON-RPC id it names.
func TestMCPCancelledNotificationStopsTool(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	type outcome struct {
		code int
		body map[string]any
	}
	done := make(chan outcome, 1)
	go func() {
		code, body := mcpPostRaw(env, key, session, map[string]any{
			"jsonrpc": "2.0", "id": 42, "method": "tools/call",
			"params": map[string]any{
				"name":      "record_sample",
				"arguments": map[string]any{"label": "word", "duration_ms": 4242},
			},
		})
		done <- outcome{code, body}
	}()

	// The fake daemon signals that the device-side take started; only then can
	// the cancellation reach it.
	select {
	case <-env.back.recordingStart:
	case <-time.After(5 * time.Second):
		t.Fatal("the recording never started")
	}
	code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 42, "reason": "user dismissed the take"},
	})
	if code != http.StatusAccepted || len(body) != 0 {
		t.Fatalf("cancellation notification = %d %v, want 202 empty", code, body)
	}

	select {
	case result := <-done:
		if result.code != http.StatusOK {
			t.Fatalf("cancelled tool call status = %d", result.code)
		}
		res, _ := result.body["result"].(map[string]any)
		if res == nil || res["isError"] != true {
			t.Fatalf("cancelled tool call result = %v", result.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled tool call never returned")
	}
	select {
	case <-env.back.recordingCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not end the device-side recording")
	}
}

func TestMCPRevocationEndsSessions(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)
	// wait_seconds proves the daemon-side training stream is established before
	// the revocation, so the cancellation is observable.
	if isErr, result := callTool(t, env, key, session, "train_wakeword", map[string]any{
		"display_name": "capsula", "wait_seconds": 0.2,
	}); isErr {
		t.Fatalf("train_wakeword failed: %v", result)
	}

	if err := env.store.RevokeAPIKey("ops"); err != nil {
		t.Fatal(err)
	}
	env.srv.reconcileMCPKeys()

	select {
	case <-env.back.trainingCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("revocation did not cancel the training run")
	}
	env.srv.mcpMu.Lock()
	live := len(env.srv.mcpSessions)
	env.srv.mcpMu.Unlock()
	if live != 0 {
		t.Fatalf("%d sessions survived the key revocation", live)
	}
	// The revoked key is refused outright on the next request.
	if code, _, _ := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}); code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d, want 401", code)
	}
}

// TestMCPRevocationViaAccessTopic proves the daemon's credential watcher path:
// an API-key edit publishes the access topic and the listener drops the
// affected sessions without any MCP request arriving.
func TestMCPRevocationViaAccessTopic(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	mcpInitialize(t, env, key)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.srv.reconcileMCPKeysOnAccess(ctx)

	if err := env.store.RevokeAPIKey("ops"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		// Publishing in the loop also covers the moment before the watcher's
		// subscription is installed.
		env.srv.Changes.Publish(uichanges.Access)
		env.srv.mcpMu.Lock()
		live := len(env.srv.mcpSessions)
		env.srv.mcpMu.Unlock()
		if live == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions survived the access-topic invalidation", live)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMCPRejectsNullOrigin: "null" is a browser context (sandboxed or
// certificate-bypassed document), not an API client.
func TestMCPRejectsNullOrigin(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)
	code, _, body := mcpPostHeaders(t, env, key, session,
		map[string]any{"jsonrpc": "2.0", "id": 31, "method": "ping"},
		map[string]string{"Origin": "null"})
	if code != http.StatusForbidden {
		t.Fatalf("null origin status = %d, want 403 (body %v)", code, body)
	}
}

// TestMCPSchemaDeclaresEnforcedFields: the machine-readable schema must agree
// with the daemon's validation instead of leaving mandatory fields to prose.
func TestMCPSchemaDeclaresEnforcedFields(t *testing.T) {
	byName := map[string]mcpTool{}
	for _, tool := range mcpToolCatalogue {
		byName[tool.Name] = tool
	}
	settings := byName["update_settings"]
	if settings.Schema["additionalProperties"] != false {
		t.Fatalf("update_settings additionalProperties = %v", settings.Schema["additionalProperties"])
	}
	required, _ := settings.Schema["required"].([]string)
	if !contains(required, "expected_revision") {
		t.Fatalf("update_settings required = %v", settings.Schema["required"])
	}
	launch := byName["launch_app"]
	if required, _ := launch.Schema["required"].([]string); !contains(required, "app_id") {
		t.Fatalf("launch_app required = %v", launch.Schema["required"])
	}
	// Each oneof group is one allOf entry holding an exactly-one oneOf.
	clock := byName["clock_command"]
	groups, _ := clock.Schema["allOf"].([]map[string]any)
	if len(groups) != 1 {
		t.Fatalf("clock_command allOf = %v", clock.Schema["allOf"])
	}
	if members, _ := groups[0]["oneOf"].([]map[string]any); len(members) != 10 {
		t.Fatalf("clock_command oneof members = %v", groups[0]["oneOf"])
	}
	if required, _ := clock.Schema["required"].([]string); !contains(required, "app_id") {
		t.Fatalf("clock_command required = %v", clock.Schema["required"])
	}
	if groups, _ := settings.Schema["allOf"].([]map[string]any); len(groups) != 1 {
		t.Fatalf("update_settings allOf = %v", settings.Schema["allOf"])
	}
	// Hand-written schemas are just as strict: the tool decoder rejects unknown
	// arguments, so the schema says so.
	for _, name := range []string{"get_state", "begin_upload", "train_wakeword"} {
		if byName[name].Schema["additionalProperties"] != false {
			t.Fatalf("%s hand schema allows unknown fields: %v", name, byName[name].Schema)
		}
	}
}

// TestMCPSchemaHandlesMultipleOneofGroups proves the generator is general: one
// message with two oneofs yields two allOf constraints, not one flat exactly-one
// across both groups. web.proto has no such message, so the descriptor is built
// in the test.
func TestMCPSchemaHandlesMultipleOneofGroups(t *testing.T) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("test/two_oneofs.proto"),
		Package: proto.String("test"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Both"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("alpha"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), OneofIndex: proto.Int32(0)},
				{Name: proto.String("beta"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), OneofIndex: proto.Int32(1)},
			},
			OneofDecl: []*descriptorpb.OneofDescriptorProto{
				{Name: proto.String("group_one")},
				{Name: proto.String("group_two")},
			},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := file.Messages().Get(0)
	schema := mcpMessageSchema(msg, 0)
	groups, _ := schema["allOf"].([]map[string]any)
	if len(groups) != 2 {
		t.Fatalf("allOf for two oneof groups = %v", schema["allOf"])
	}
	for i, want := range []string{"alpha", "beta"} {
		members, _ := groups[i]["oneOf"].([]map[string]any)
		if len(members) != 1 {
			t.Fatalf("group %d members = %v", i, groups[i]["oneOf"])
		}
		required, _ := members[0]["required"].([]string)
		if len(required) != 1 || required[0] != want {
			t.Fatalf("group %d required = %v, want %s", i, required, want)
		}
	}
	if _, flat := schema["oneOf"]; flat {
		t.Fatal("the generator flattened multiple oneof groups into one exactly-one constraint")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestMCPKeyCreationPublishesAccessChange proves the consumer-visible path the
// browser Access view subscribes to: an MCP key mutation invalidates the topic
// every open Web UI socket watches.
func TestMCPKeyCreationPublishesAccessChange(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	signals, cancel := env.srv.Changes.Subscribe()
	defer cancel()
	isErr, result := callTool(t, env, key, session, "create_api_key", map[string]any{"name": "delegated"})
	if isErr {
		t.Fatalf("create_api_key failed: %v", result)
	}
	select {
	case topic := <-signals:
		if topic != uichanges.Access {
			t.Fatalf("published topic = %v, want Access", topic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("create_api_key did not invalidate the access topic")
	}
}

// slowBodyReader delivers a request body in small slow pieces, modelling a
// client on a slow link.
type slowBodyReader struct {
	reader *strings.Reader
	delay  time.Duration
	chunk  int
}

func (b *slowBodyReader) Read(p []byte) (int, error) {
	if b.reader.Len() == 0 {
		return 0, io.EOF
	}
	if len(p) > b.chunk {
		p = p[:b.chunk]
	}
	n, err := b.reader.Read(p)
	time.Sleep(b.delay)
	return n, err
}

// TestMCPExtendsTheReadDeadlineForSlowBodies is the regression for P2-5: the
// listener's short read timeout must not kill a body that is still arriving.
func TestMCPExtendsTheReadDeadlineForSlowBodies(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")

	ts := httptest.NewUnstartedServer(env.srv.Handler())
	ts.Config.ReadTimeout = 100 * time.Millisecond
	ts.StartTLS()
	t.Cleanup(ts.Close)

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": mcpProtocolVersion},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := &slowBodyReader{reader: strings.NewReader(string(payload)), delay: 60 * time.Millisecond, chunk: 32}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("slow-body request failed (read deadline not extended?): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow-body status = %d (%s)", resp.StatusCode, raw)
	}
	if resp.Header.Get(mcpSessionHeader) == "" {
		t.Fatal("slow-body initialize returned no session")
	}
}
