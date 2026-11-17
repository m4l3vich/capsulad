package webui

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capsulad/internal/artifact"
	"capsulad/internal/assets"
)

// TestMCPStagedAssetUploadAndAbort proves the staged path preserves the store's
// own 32 MiB limit and publishes through the same writer the Web UI uses, with
// the body split across small chunks.
func TestMCPStagedAssetUploadAndAbort(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
		"kind": "asset", "path": "sounds/staged.wav",
	})
	if isErr {
		t.Fatalf("begin_upload failed: %v", result)
	}
	handle := resultJSON(t, result)
	uploadID, _ := handle["upload_id"].(string)
	if uploadID == "" {
		t.Fatalf("begin_upload returned no upload_id: %v", handle)
	}
	if got := int64(handle["max_bytes"].(float64)); got != assets.MaxFileBytes {
		t.Fatalf("asset upload limit = %d, want the store's %d", got, assets.MaxFileBytes)
	}

	first := bytes.Repeat([]byte("a"), 700<<10)
	second := bytes.Repeat([]byte("b"), 300<<10)
	for _, part := range [][]byte{first, second} {
		isErr, result = callTool(t, env, key, session, "upload_chunk", map[string]any{
			"upload_id": uploadID, "content_base64": base64.StdEncoding.EncodeToString(part),
		})
		if isErr {
			t.Fatalf("upload_chunk failed: %v", result)
		}
	}
	if got := int(resultJSON(t, result)["received_bytes"].(float64)); got != len(first)+len(second) {
		t.Fatalf("received_bytes = %d, want %d", got, len(first)+len(second))
	}

	isErr, result = callTool(t, env, key, session, "complete_upload", map[string]any{"upload_id": uploadID})
	if isErr {
		t.Fatalf("complete_upload failed: %v", result)
	}
	asset := resultJSON(t, result)["asset"].(map[string]any)
	if asset["id"] != "sounds/staged.wav" {
		t.Fatalf("completed asset = %v", asset)
	}
	// A chunk send after completion is refused: the handle is consumed.
	if code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "upload_chunk", "arguments": map[string]any{
			"upload_id": uploadID, "content_base64": base64.StdEncoding.EncodeToString(first),
		}},
	}); code != 200 || body["error"] == nil {
		t.Fatalf("chunk after completion = %d %v", code, body)
	}
	stored, err := os.ReadFile(filepath.Join(catalog.Root(), "sounds", "staged.wav"))
	if err != nil || !bytes.Equal(stored, append(append([]byte(nil), first...), second...)) {
		t.Fatalf("stored asset = %d bytes, err %v", len(stored), err)
	}

	// Abort discards the partial upload and is idempotent.
	isErr, result = callTool(t, env, key, session, "begin_upload", map[string]any{
		"kind": "asset", "path": "sounds/aborted.wav",
	})
	if isErr {
		t.Fatalf("begin_upload (abort case) failed: %v", result)
	}
	abortedID := resultJSON(t, result)["upload_id"].(string)
	if isErr, result = callTool(t, env, key, session, "upload_chunk", map[string]any{
		"upload_id": abortedID, "content_base64": base64.StdEncoding.EncodeToString(first),
	}); isErr {
		t.Fatalf("upload_chunk (abort case) failed: %v", result)
	}
	isErr, result = callTool(t, env, key, session, "abort_upload", map[string]any{"upload_id": abortedID})
	if isErr || resultJSON(t, result)["aborted"] != true {
		t.Fatalf("abort_upload = %v", result)
	}
	isErr, result = callTool(t, env, key, session, "abort_upload", map[string]any{"upload_id": abortedID})
	if isErr || resultJSON(t, result)["aborted"] != false {
		t.Fatalf("second abort_upload = %v", result)
	}
	if _, err := os.Stat(filepath.Join(catalog.Root(), "sounds", "aborted.wav")); !os.IsNotExist(err) {
		t.Fatalf("aborted upload left a published file: %v", err)
	}
	if len(catalog.List("sounds")) != 1 {
		t.Fatalf("catalog = %v, want only the completed asset", catalog.List("sounds"))
	}
}

// TestMCPStagedPackageUpload proves a multi-chunk package reaches the daemon's
// InstallApplication stream intact and reports the artifact archive limit.
func TestMCPStagedPackageUpload(t *testing.T) {
	env := newTestEnv(t)
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
		"kind": "app", "filename": "staged-1.0.0.tar.gz",
	})
	if isErr {
		t.Fatalf("begin_upload failed: %v", result)
	}
	handle := resultJSON(t, result)
	uploadID := handle["upload_id"].(string)
	if got := int64(handle["max_bytes"].(float64)); got != artifact.MaxArchiveBytes {
		t.Fatalf("package upload limit = %d, want the artifact limit %d", got, artifact.MaxArchiveBytes)
	}

	archive := bytes.Repeat([]byte("pkg-"), 1024)
	half := len(archive) / 2
	for _, part := range [][]byte{archive[:half], archive[half:]} {
		if isErr, result = callTool(t, env, key, session, "upload_chunk", map[string]any{
			"upload_id": uploadID, "content_base64": base64.StdEncoding.EncodeToString(part),
		}); isErr {
			t.Fatalf("upload_chunk failed: %v", result)
		}
	}
	isErr, result = callTool(t, env, key, session, "complete_upload", map[string]any{"upload_id": uploadID})
	if isErr {
		t.Fatalf("complete_upload failed: %v", result)
	}
	installed := resultJSON(t, result)
	if installed["app_id"] != "org.example.new" || installed["replaced"] != true {
		t.Fatalf("install result = %v", installed)
	}
	env.back.mu.Lock()
	received := append([]byte(nil), env.back.installedArchive...)
	env.back.mu.Unlock()
	if !bytes.Equal(received, archive) {
		t.Fatalf("daemon received %d bytes, want %d", len(received), len(archive))
	}
}

func TestMCPUploadChunkBoundAndSlots(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
		"kind": "asset", "path": "one.bin",
	})
	if isErr {
		t.Fatalf("begin_upload failed: %v", result)
	}
	uploadID := resultJSON(t, result)["upload_id"].(string)

	// One chunk is capped so a single request cannot allocate a large string.
	oversized := base64.StdEncoding.EncodeToString(make([]byte, mcpMaxChunkBytes+1))
	code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "upload_chunk", "arguments": map[string]any{
			"upload_id": uploadID, "content_base64": oversized,
		}},
	})
	if code != 200 {
		t.Fatalf("oversized chunk status = %d", code)
	}
	if rpcErr, _ := body["error"].(map[string]any); rpcErr == nil ||
		int(rpcErr["code"].(float64)) != mcpCodeInvalidParams {
		t.Fatalf("oversized chunk = %v, want invalid params", body["error"])
	}

	// A second staged upload fits; a third is refused until one is released.
	for i, path := range []string{"two.bin", "three.bin"} {
		code, _, body = mcpPost(t, env, key, session, map[string]any{
			"jsonrpc": "2.0", "id": 10 + i, "method": "tools/call",
			"params": map[string]any{"name": "begin_upload", "arguments": map[string]any{
				"kind": "asset", "path": path,
			}},
		})
		if i == 0 && (code != 200 || body["error"] != nil) {
			t.Fatalf("second staged upload = %d %v", code, body)
		}
		if i == 1 {
			rpcErr, _ := body["error"].(map[string]any)
			if rpcErr == nil || int(rpcErr["code"].(float64)) != mcpCodeUnavailable {
				t.Fatalf("third staged upload = %v, want unavailable", body["error"])
			}
		}
	}

	// Ending the session discards the staged uploads (no published file).
	if code, _, _ := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 20, "method": "tools/call",
		"params": map[string]any{"name": "upload_chunk", "arguments": map[string]any{
			"upload_id": uploadID, "content_base64": base64.StdEncoding.EncodeToString([]byte("x")),
		}},
	}); code != 200 {
		t.Fatalf("chunk status = %d", code)
	}
	env.srv.reconcileMCPKeys() // no revocation: the session survives
	env.srv.closeAllMCPSessions()
	if _, err := os.Stat(filepath.Join(catalog.Root(), "one.bin")); !os.IsNotExist(err) {
		t.Fatalf("session teardown published a partial upload: %v", err)
	}
}

// --- ordering and lifecycle regressions (do not regress silently) ----------

// TestMCPUploadCompletionWaitsForInFlightChunk pins the state machine: closing
// the pipe while a chunk is inside pw.Write would make the sink see EOF
// mid-chunk and publish a truncated file.
func TestMCPUploadCompletionWaitsForInFlightChunk(t *testing.T) {
	pr, pw := io.Pipe()
	upload := &mcpUpload{pw: pw, cancel: func() {}, limit: 8 << 20, done: make(chan struct{})}
	payload := bytes.Repeat([]byte("x"), 1<<20)

	writeErr := make(chan *mcpError, 1)
	go func() {
		_, rpcErr := upload.write(payload)
		writeErr <- rpcErr
	}()
	// Wait until the chunk is actually inside pw.Write; nothing is reading yet,
	// so it stays blocked there.
	deadline := time.Now().Add(2 * time.Second)
	for {
		upload.mu.Lock()
		writes := upload.writes
		upload.mu.Unlock()
		if writes == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the chunk never entered the pipe")
		}
		time.Sleep(time.Millisecond)
	}

	drained, ended, rpcErr := upload.beginComplete()
	if rpcErr != nil {
		t.Fatalf("beginComplete: %v", rpcErr)
	}
	if ended != nil {
		t.Fatalf("completion of an open upload returned %v", ended)
	}
	if drained == nil {
		t.Fatal("completion did not wait for the in-flight chunk")
	}
	// Completion must refuse new chunks while it drains; the refusal is a
	// failed tool result, consistent with every other ended-upload outcome.
	if result, rpcErr := upload.write([]byte("late")); rpcErr != nil || result == nil || !result.IsError {
		t.Fatalf("chunk while completing = (%v, %v), want an isError tool result", result, rpcErr)
	}

	var got bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = io.Copy(&got, pr)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight chunk never drained")
	}
	// The drain signal means the writer accepted every byte; closing now is the
	// completion path and must not lose any of them.
	_ = pw.Close()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader did not observe the end of the body")
	}
	if got.Len() != len(payload) {
		t.Fatalf("published %d bytes, want the whole %d-byte chunk", got.Len(), len(payload))
	}
	if rpcErr := <-writeErr; rpcErr != nil {
		t.Fatalf("the in-flight chunk failed: %v", rpcErr)
	}
}

// TestMCPUploadAbortReleasesBlockedChunk proves the escape hatch: an abort
// unblocks a chunk that is waiting for a sink that will never read.
func TestMCPUploadAbortReleasesBlockedChunk(t *testing.T) {
	_, pw := io.Pipe()
	upload := &mcpUpload{pw: pw, cancel: func() {}, limit: 8 << 20, done: make(chan struct{})}
	type writeOutcome struct {
		result *mcpToolResult
		err    *mcpError
	}
	writeErr := make(chan writeOutcome, 1)
	go func() {
		result, rpcErr := upload.write(bytes.Repeat([]byte("x"), 512<<10))
		writeErr <- writeOutcome{result, rpcErr}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		upload.mu.Lock()
		writes := upload.writes
		upload.mu.Unlock()
		if writes == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the chunk never entered the pipe")
		}
		time.Sleep(time.Millisecond)
	}
	// A real upload's sink goroutine closes done when it returns; this unit
	// object has no sink, so the test supplies that end-of-upload signal.
	release := make(chan struct{})
	go func() {
		<-release
		close(upload.done)
	}()
	upload.abort(errMCPUploadExpired)
	close(release)
	select {
	case outcome := <-writeErr:
		if outcome.err != nil {
			t.Fatalf("aborted chunk protocol error = %v", outcome.err)
		}
		if outcome.result == nil || !outcome.result.IsError {
			t.Fatalf("aborted chunk = %v, want an isError tool result", outcome.result)
		}
		structured, _ := outcome.result.StructuredContent.(map[string]any)
		failure, _ := structured["error"].(map[string]any)
		message, _ := failure["message"].(string)
		if !strings.Contains(message, "expired") {
			t.Fatalf("aborted chunk message = %q, want the expiry reason", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not release the blocked chunk")
	}
}

// TestMCPPausedUploadDoesNotStallTheStore is the consumer-visible form of the
// store-lock fix: the asset store must stay readable while a staged upload is
// paused mid-body.
func TestMCPPausedUploadDoesNotStallTheStore(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
		"kind": "asset", "path": "paused.bin",
	})
	if isErr {
		t.Fatalf("begin_upload failed: %v", result)
	}
	uploadID := resultJSON(t, result)["upload_id"].(string)
	part := bytes.Repeat([]byte("p"), 700<<10)
	if isErr, result := callTool(t, env, key, session, "upload_chunk", map[string]any{
		"upload_id": uploadID, "content_base64": base64.StdEncoding.EncodeToString(part),
	}); isErr {
		t.Fatalf("upload_chunk failed: %v", result)
	}

	// The upload is now paused: its sink is blocked reading the next chunk and
	// must not be holding the catalog lock.
	listed := make(chan bool, 1)
	go func() {
		code, body := mcpPostRaw(env, key, session, map[string]any{
			"jsonrpc": "2.0", "id": 90, "method": "tools/call",
			"params": map[string]any{"name": "list_assets", "arguments": map[string]any{}},
		})
		listed <- code == http.StatusOK && body["error"] == nil
	}()
	select {
	case ok := <-listed:
		if !ok {
			t.Fatal("list_assets failed while an upload was paused")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("list_assets blocked behind a paused upload")
	}

	if isErr, result := callTool(t, env, key, session, "complete_upload", map[string]any{
		"upload_id": uploadID,
	}); isErr {
		t.Fatalf("complete_upload failed: %v", result)
	}
	stored, err := os.ReadFile(filepath.Join(catalog.Root(), "paused.bin"))
	if err != nil || !bytes.Equal(stored, part) {
		t.Fatalf("published asset = %d bytes, err %v", len(stored), err)
	}
}

// TestMCPConcurrentStagedAssetUploads interleaves two uploads through the same
// shared store: both must publish their own bytes, neither may stall.
func TestMCPConcurrentStagedAssetUploads(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	begin := func(name string) (string, []byte) {
		t.Helper()
		isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
			"kind": "asset", "path": name,
		})
		if isErr {
			t.Fatalf("begin %s failed: %v", name, result)
		}
		return resultJSON(t, result)["upload_id"].(string), bytes.Repeat([]byte(name[:1]), 400<<10)
	}
	firstID, firstData := begin("one.bin")
	secondID, secondData := begin("two.bin")

	// Interleave the chunk requests: one upload's sink is consuming while the
	// other's body is in flight. Each half is sent exactly once.
	for half := 1; half <= 2; half++ {
		for _, pair := range []struct {
			id   string
			data []byte
		}{
			{firstID, firstData[(half-1)*len(firstData)/2 : half*len(firstData)/2]},
			{secondID, secondData[(half-1)*len(secondData)/2 : half*len(secondData)/2]},
		} {
			if isErr, result := callTool(t, env, key, session, "upload_chunk", map[string]any{
				"upload_id": pair.id, "content_base64": base64.StdEncoding.EncodeToString(pair.data),
			}); isErr {
				t.Fatalf("interleaved chunk failed: %v", result)
			}
		}
	}
	for _, pair := range []struct {
		id   string
		name string
		data []byte
	}{{firstID, "one.bin", firstData}, {secondID, "two.bin", secondData}} {
		if isErr, result := callTool(t, env, key, session, "complete_upload", map[string]any{
			"upload_id": pair.id,
		}); isErr {
			t.Fatalf("complete %s failed: %v", pair.name, result)
		}
		stored, err := os.ReadFile(filepath.Join(catalog.Root(), pair.name))
		if err != nil || !bytes.Equal(stored, pair.data) {
			t.Fatalf("%s = %d bytes, err %v", pair.name, len(stored), err)
		}
	}
}

// TestMCPDeadUploadSlotsAreReaped: an upload whose sink failed is finished and
// must not occupy one of the session's two slots forever.
func TestMCPDeadUploadSlotsAreReaped(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	sess, ok := env.srv.mcpSession(session, mcpKeyID(env.store.ListAPIKeys()[0]))
	if !ok {
		t.Fatal("session not found")
	}
	var deadIDs []string
	for _, path := range []string{"../escape-a", "../escape-b"} {
		isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
			"kind": "asset", "path": path,
		})
		if isErr {
			t.Fatalf("begin_upload(%q) failed: %v", path, result)
		}
		deadIDs = append(deadIDs, resultJSON(t, result)["upload_id"].(string))
	}
	// Both sinks reject the traversal path immediately; wait until each is
	// finished (the next begin_upload reaps them).
	isDead := func(id string) bool {
		u := sess.upload(id)
		return u == nil || u.stateOf() == mcpUploadDone
	}
	deadline := time.Now().Add(5 * time.Second)
	for !(isDead(deadIDs[0]) && isDead(deadIDs[1])) {
		if time.Now().After(deadline) {
			t.Fatal("the dead uploads never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Without the reap both slots would still be counted and this would be
	// refused with -32003.
	for _, path := range []string{"fresh-one.bin", "fresh-two.bin"} {
		if isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
			"kind": "asset", "path": path,
		}); isErr {
			t.Fatalf("begin_upload(%q) after dead slots failed: %v", path, result)
		}
	}
	if code, _, body := mcpPost(t, env, key, session, map[string]any{
		"jsonrpc": "2.0", "id": 91, "method": "tools/call",
		"params": map[string]any{"name": "begin_upload", "arguments": map[string]any{
			"kind": "asset", "path": "third.bin",
		}},
	}); code != http.StatusOK {
		t.Fatalf("third begin status = %d", code)
	} else if rpcErr, _ := body["error"].(map[string]any); rpcErr == nil ||
		int(rpcErr["code"].(float64)) != mcpCodeUnavailable {
		t.Fatalf("third live begin = %v, want the slot limit", body["error"])
	}
}

func TestMCPUploadExpiryReasonsAreLegible(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	isErr, result := callTool(t, env, key, session, "begin_upload", map[string]any{
		"kind": "asset", "path": "expired.bin",
	})
	if isErr {
		t.Fatalf("begin_upload failed: %v", result)
	}
	uploadID := resultJSON(t, result)["upload_id"].(string)
	sess, _ := env.srv.mcpSession(session, mcpKeyID(env.store.ListAPIKeys()[0]))
	upload := sess.upload(uploadID)
	if upload == nil {
		t.Fatal("staged upload not found")
	}
	// Force the path the 15-minute lifetime would take without waiting for it.
	upload.abort(errMCPUploadExpired)

	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"upload_chunk", map[string]any{"upload_id": uploadID,
			"content_base64": base64.StdEncoding.EncodeToString([]byte("late"))}},
		{"complete_upload", map[string]any{"upload_id": uploadID}},
	} {
		code, _, body := mcpPost(t, env, key, session, map[string]any{
			"jsonrpc": "2.0", "id": 92, "method": "tools/call",
			"params": map[string]any{"name": call.tool, "arguments": call.args},
		})
		if code != http.StatusOK {
			t.Fatalf("%s status = %d", call.tool, code)
		}
		// An upload that already ended is an operation failure, not a protocol
		// error: the same shape every other failed tool call uses.
		if body["error"] != nil {
			t.Fatalf("%s after expiry returned a protocol error: %v", call.tool, body["error"])
		}
		result, _ := body["result"].(map[string]any)
		if result == nil || result["isError"] != true {
			t.Fatalf("%s after expiry = %v, want an isError tool result", call.tool, body["result"])
		}
		message := resultJSON(t, result)["error"].(map[string]any)["message"].(string)
		if !strings.Contains(message, "expired") {
			t.Fatalf("%s after expiry message = %q, want the expiry reason", call.tool, message)
		}
	}
}

// TestMCPDeleteWinsOverStagedReplacement is the MCP-level form of the catalog
// ordering rule: deleting an asset while its replacement streams succeeds, and
// the in-flight replacement must not silently recreate it.
func TestMCPDeleteWinsOverStagedReplacement(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := assets.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Assets = catalog
	key := operatorKey(t, env, "ops")
	session := mcpInitialize(t, env, key)

	original := bytes.Repeat([]byte("o"), 64<<10)
	isErr, result := callTool(t, env, key, session, "upload_asset", map[string]any{
		"path": "replace.bin", "content_base64": base64.StdEncoding.EncodeToString(original),
	})
	if isErr {
		t.Fatalf("seed upload failed: %v", result)
	}

	// Begin a replacement and prove its sink is live (the chunk was accepted),
	// which means the write has already reserved its budget.
	isErr, result = callTool(t, env, key, session, "begin_upload", map[string]any{
		"kind": "asset", "path": "replace.bin",
	})
	if isErr {
		t.Fatalf("begin_upload failed: %v", result)
	}
	uploadID := resultJSON(t, result)["upload_id"].(string)
	replacement := bytes.Repeat([]byte("r"), 32<<10)
	if isErr, result = callTool(t, env, key, session, "upload_chunk", map[string]any{
		"upload_id": uploadID, "content_base64": base64.StdEncoding.EncodeToString(replacement),
	}); isErr {
		t.Fatalf("upload_chunk failed: %v", result)
	}

	// The delete lands while the replacement is in flight and must win.
	if isErr, result = callTool(t, env, key, session, "delete_asset", map[string]any{
		"path": "replace.bin",
	}); isErr {
		t.Fatalf("delete during replacement failed: %v", result)
	}
	if isErr, result = callTool(t, env, key, session, "complete_upload", map[string]any{
		"upload_id": uploadID,
	}); !isErr {
		t.Fatalf("completed replacement after a delete = %v, want a failed tool call", result)
	} else {
		structured, _ := result["structuredContent"].(map[string]any)
		failure, _ := structured["error"].(map[string]any)
		if message, _ := failure["message"].(string); !strings.Contains(message, "deleted") {
			t.Fatalf("replacement failure = %v, want the deletion reason", failure)
		}
	}
	if _, err := os.Stat(filepath.Join(catalog.Root(), "replace.bin")); !os.IsNotExist(err) {
		t.Fatalf("the deleted asset was resurrected: %v", err)
	}
	isErr, result = callTool(t, env, key, session, "list_assets", map[string]any{})
	if isErr {
		t.Fatalf("list_assets failed: %v", result)
	}
	if list, _ := resultJSON(t, result)["assets"].([]any); len(list) != 0 {
		t.Fatalf("catalog after the delete = %v", list)
	}
	// A fresh upload is a deliberate recreation and succeeds.
	if isErr, result = callTool(t, env, key, session, "upload_asset", map[string]any{
		"path": "replace.bin", "content_base64": base64.StdEncoding.EncodeToString(original),
	}); isErr {
		t.Fatalf("recreate after the delete won: %v", result)
	}
}
