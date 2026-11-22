package webui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
	"capsulad/internal/bluetooth"
	"capsulad/internal/manifest"
	"capsulad/internal/registry"
	"capsulad/internal/uichanges"
	"capsulad/internal/wakeword"
	"capsulad/internal/websecurity"
)

type wsTestConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func wsHost(t *testing.T, ts *httptest.Server) string {
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// dialWS performs the RFC 6455 handshake with an explicit cookie and Origin.
// The returned status is the HTTP status when the upgrade is refused.
func dialWS(t *testing.T, ts *httptest.Server, session, origin string) (*wsTestConn, int) {
	t.Helper()
	host := wsHost(t, ts)
	conn, err := tls.Dial("tcp", host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nOrigin: %s\r\n",
		host, base64.StdEncoding.EncodeToString(key[:]), origin)
	if session != "" {
		request += "Cookie: " + websecurity.SessionCookieName() + "=" + session + "\r\n"
	}
	request += "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if !strings.Contains(statusLine, "101") {
		conn.Close()
		parts := strings.Fields(statusLine)
		code := 0
		if len(parts) >= 2 {
			fmt.Sscanf(parts[1], "%d", &code)
		}
		return nil, code
	}
	return &wsTestConn{t: t, conn: conn, br: br}, http.StatusSwitchingProtocols
}

func (c *wsTestConn) close() { _ = c.conn.Close() }

func (c *wsTestConn) writeFrame(opcode byte, payload []byte) error {
	header := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xffff:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		header = append(header, buf[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)
	data := make([]byte, n)
	copy(data, payload)
	for i := range data {
		data[i] ^= mask[i%4]
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(data)
	return err
}

func (c *wsTestConn) readFrame() (byte, []byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0f
	length := uint64(head[1] & 0x7f)
	if head[1]&0x80 != 0 {
		return 0, nil, fmt.Errorf("server frame masked")
	}
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

func (c *wsTestConn) send(frame *webv1.ClientFrame) {
	c.t.Helper()
	data, err := proto.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.writeFrame(opBinary, data); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsTestConn) read() *webv1.ServerFrame {
	c.t.Helper()
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			c.t.Fatalf("read frame: %v", err)
		}
		switch opcode {
		case opPing:
			_ = c.writeFrame(opPong, payload)
		case opBinary:
			var frame webv1.ServerFrame
			if err := proto.Unmarshal(payload, &frame); err != nil {
				c.t.Fatalf("decode frame: %v", err)
			}
			return &frame
		case opClose:
			c.t.Fatalf("unexpected close frame")
		}
	}
}

func (c *wsTestConn) readClose(t *testing.T) uint16 {
	t.Helper()
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			t.Fatalf("read close: %v", err)
		}
		if opcode == opPing {
			_ = c.writeFrame(opPong, payload)
			continue
		}
		if opcode != opClose {
			continue
		}
		if len(payload) < 2 {
			return 0
		}
		return binary.BigEndian.Uint16(payload)
	}
}

func (c *wsTestConn) waitResult(t *testing.T, requestID uint64) *webv1.Result {
	t.Helper()
	for {
		frame := c.read()
		if frame.GetRequestId() != requestID {
			continue
		}
		if err := frame.GetError(); err != nil {
			t.Fatalf("request %d failed: %s", requestID, err.GetMessage())
		}
		if result := frame.GetResult(); result != nil {
			return result
		}
	}
}

func (c *wsTestConn) waitSnapshot(t *testing.T, requestID uint64) *webv1.Snapshot {
	t.Helper()
	for {
		frame := c.read()
		if frame.GetRequestId() != requestID {
			continue
		}
		if snapshot := frame.GetSnapshot(); snapshot != nil {
			return snapshot
		}
	}
}

func connectSession(t *testing.T, env *testEnv) (*wsTestConn, string, *http.Client) {
	t.Helper()
	c := env.client(t)
	session := loginSession(t, env, c)
	origin := "https://" + wsHost(t, env.ts)
	conn, code := dialWS(t, env.ts, session, origin)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("websocket upgrade: %d", code)
	}
	t.Cleanup(conn.close)
	return conn, session, c
}

func TestWebSocketAdmission(t *testing.T) {
	env := newTestEnv(t)
	origin := "https://" + wsHost(t, env.ts)

	if _, code := dialWS(t, env.ts, "", origin); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated upgrade: %d, want 401", code)
	}
	c := env.client(t)
	session := loginSession(t, env, c)
	if _, code := dialWS(t, env.ts, session, "https://attacker.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin upgrade: %d, want 403", code)
	}
	if _, code := dialWS(t, env.ts, session, ""); code != http.StatusForbidden {
		t.Fatalf("missing-origin upgrade: %d, want 403", code)
	}
	if _, code := dialWS(t, env.ts, session, "http://"+wsHost(t, env.ts)); code != http.StatusForbidden {
		t.Fatalf("non-https origin upgrade: %d, want 403", code)
	}

	conn, _ := dialWS(t, env.ts, session, origin)
	if conn == nil {
		t.Fatal("valid upgrade refused")
	}
	defer conn.close()
	conn.send(&webv1.ClientFrame{RequestId: 1, Body: &webv1.ClientFrame_Hello{Hello: &webv1.Hello{}}})
	frame := conn.read()
	if frame.GetHello() == nil || frame.GetHello().GetDaemonVersion() == "" {
		t.Fatalf("hello result: %v", frame)
	}
	if frame.GetHello().GetMaxClientFrameBytes() != maxClientFrameBytes {
		t.Fatalf("frame budget = %d", frame.GetHello().GetMaxClientFrameBytes())
	}
	conn.send(&webv1.ClientFrame{RequestId: 2, Body: &webv1.ClientFrame_Ping{Ping: &webv1.Ping{}}})
	for {
		frame = conn.read()
		if frame.GetPong() != nil {
			break
		}
	}
}

func TestWebSocketFrameLimits(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)

	// Text frames are not part of the protocol.
	if err := conn.writeFrame(opText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if code := conn.readClose(t); code != closeUnsupportedData {
		t.Fatalf("text frame close = %d, want %d", code, closeUnsupportedData)
	}

	conn2, _, _ := connectSession(t, env)
	oversized := make([]byte, maxClientFrameBytes+1)
	if err := conn2.writeFrame(opBinary, oversized); err != nil {
		// The server rejects from the length header and may close before the
		// payload write lands; the close code below is the contract.
	}
	if code := conn2.readClose(t); code != closeTooLarge {
		t.Fatalf("oversized frame close = %d, want %d", code, closeTooLarge)
	}

	conn3, _, _ := connectSession(t, env)
	if err := conn3.writeFrame(opBinary, []byte{0xff, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	for {
		frame := conn3.read()
		if e := frame.GetError(); e != nil {
			if e.GetCode() != webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
				t.Fatalf("malformed frame error = %v", e)
			}
			break
		}
	}
}

func TestLogoutClosesWebSocket(t *testing.T) {
	env := newTestEnv(t)
	conn, _, c := connectSession(t, env)
	// Start a request to prove the socket is live.
	conn.send(&webv1.ClientFrame{RequestId: 1, Body: &webv1.ClientFrame_Ping{Ping: &webv1.Ping{}}})

	token := csrfToken(t, c, env.ts.URL)
	code, body, _ := doJSON(t, c, http.MethodPost, env.ts.URL+"/api/logout", nil,
		map[string]string{"X-Capsulad-CSRF": token})
	if code != http.StatusOK {
		t.Fatalf("logout: %d %v", code, body)
	}
	if code := conn.readClose(t); code != closePolicyViolation {
		t.Fatalf("logout close = %d, want %d", code, closePolicyViolation)
	}
}

func TestSessionRevocationClosesWebSocket(t *testing.T) {
	env := newTestEnv(t)
	conn, session, _ := connectSession(t, env)
	env.store.RevokeSession(session)
	// The monitor validates every 15s; a client frame validates immediately.
	conn.send(&webv1.ClientFrame{RequestId: 1, Body: &webv1.ClientFrame_Ping{Ping: &webv1.Ping{}}})
	if code := conn.readClose(t); code != closePolicyViolation {
		t.Fatalf("revoked session close = %d, want %d", code, closePolicyViolation)
	}
}

func TestSnapshotAndCommands(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 10, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	snapshot := conn.waitSnapshot(t, 10)
	overview := snapshot.GetOverview()
	if overview == nil || overview.GetDevice().GetModel() != "Capsula Mini" {
		t.Fatalf("overview device: %v", snapshot)
	}
	if len(overview.GetPairedDevices()) != 1 || overview.GetPairedDevices()[0].GetName() != "Phone" {
		t.Fatalf("paired devices: %v", overview.GetPairedDevices())
	}
	if len(overview.GetProviders()) != 1 || overview.GetProviders()[0].GetPlayers()[0].GetTitle() != "Scarlet Fire" {
		t.Fatalf("media providers: %v", overview.GetProviders())
	}
	// The SPA gates its transport buttons on these exact labels ("PLAY",
	// "SKIP_NEXT", …), not on the contract's MEDIA_* or the command's
	// MEDIA_ACTION_* enum spellings.
	actions := overview.GetProviders()[0].GetPlayers()[0].GetSupportedActions()
	if len(actions) != 2 || actions[0] != "PAUSE" || actions[1] != "SKIP_NEXT" {
		t.Fatalf("supported actions = %v, want [PAUSE SKIP_NEXT]", actions)
	}
	if overview.GetSettingsRevision() != 5 {
		t.Fatalf("settings revision = %d", overview.GetSettingsRevision())
	}
	if overview.GetSelectedProviders()["capsulad.clock"] != true {
		t.Fatalf("selected providers = %v", overview.GetSelectedProviders())
	}

	// Settings volume update uses the snapshot revision as its CAS token.
	conn.send(&webv1.ClientFrame{RequestId: 11, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Settings{Settings: &webv1.SettingsUpdate{
			ExpectedRevision: overview.GetSettingsRevision(),
			Group:            &webv1.SettingsUpdate_Volume{Volume: &webv1.VolumeSettings{MasterVolume: 0.9}},
		}},
	}}})
	result := conn.waitResult(t, 11)
	if result.GetSettings().GetRevision() != 6 || result.GetSettings().GetMessage() != "settings saved" {
		t.Fatalf("settings result: %v", result)
	}

	// A stale revision must surface as a conflict, not a silent overwrite.
	conn.send(&webv1.ClientFrame{RequestId: 12, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Settings{Settings: &webv1.SettingsUpdate{
			ExpectedRevision: 5,
			Group:            &webv1.SettingsUpdate_Volume{Volume: &webv1.VolumeSettings{MasterVolume: 0.1}},
		}},
	}}})
	for {
		frame := conn.read()
		if frame.GetRequestId() != 12 {
			continue
		}
		if e := frame.GetError(); e != nil {
			if e.GetCode() != webv1.ErrorCode_ERROR_CODE_CONFLICT {
				t.Fatalf("stale revision error = %v", e)
			}
			break
		}
	}

	// Recording returns only a sample descriptor; no audio bytes exist in the
	// schema at all.
	conn.send(&webv1.ClientFrame{RequestId: 13, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_RecordSample{RecordSample: &webv1.RecordSample{Label: "speech", DurationMs: 4000}},
	}}})
	result = conn.waitResult(t, 13)
	if result.GetSample().GetSample().GetSampleId() != "s-new" || result.GetSample().GetSample().GetDurationMs() != 4000 {
		t.Fatalf("sample result: %v", result)
	}

	// Clock command round-trips through the app's public clock contract.
	conn.send(&webv1.ClientFrame{RequestId: 14, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Clock{Clock: &webv1.ClockCommand{AppId: "capsulad.clock",
			Action: &webv1.ClockCommand_StopwatchStart{StopwatchStart: &webv1.Empty{}}}},
	}}})
	result = conn.waitResult(t, 14)
	if result.GetClock().GetClock().GetStopwatch().GetState() != "STOPWATCH_STATUS_RUNNING" {
		t.Fatalf("clock result: %v", result)
	}

	// A non-clock app cannot reach the clock protocol.
	conn.send(&webv1.ClientFrame{RequestId: 15, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Clock{Clock: &webv1.ClockCommand{AppId: "org.example.player",
			Action: &webv1.ClockCommand_StopwatchStart{StopwatchStart: &webv1.Empty{}}}},
	}}})
	for {
		frame := conn.read()
		if frame.GetRequestId() == 15 && frame.GetError() != nil {
			if frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_FORBIDDEN {
				t.Fatalf("clock boundary error = %v", frame.GetError())
			}
			break
		}
	}
}

func TestTrainingProgressAndCancel(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 20, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Train{Train: &webv1.TrainWakeword{
			DisplayName: "Marusia", Language: "ru", Seed: 1,
			WordSampleIds: []string{"s1"}, RoomSampleIds: []string{"s2"}, SpeechSampleIds: []string{"s3"},
		}},
	}}})
	result := conn.waitResult(t, 20)
	trainingID := result.GetTraining().GetTrainingId()
	if trainingID == "" {
		t.Fatal("no training id")
	}
	var progressSeen bool
	for {
		frame := conn.read()
		if frame.GetProgress() == nil || frame.GetProgress().GetTrainingId() != trainingID {
			continue
		}
		if frame.GetProgress().GetStage() == "fit" {
			progressSeen = true
			break
		}
	}
	if !progressSeen {
		t.Fatal("no training progress")
	}
	conn.send(&webv1.ClientFrame{RequestId: 21, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_CancelTraining{CancelTraining: &webv1.CancelTraining{TrainingId: trainingID}},
	}}})
	conn.waitResult(t, 21)
	select {
	case <-env.back.trainingCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("training RPC was not cancelled")
	}
}

func TestOperationAllowlistMatchesProto(t *testing.T) {
	descriptor := (&webv1.Command{}).ProtoReflect().Descriptor()
	oneof := descriptor.Oneofs().Get(0)
	got := map[string]bool{}
	for i := 0; i < oneof.Fields().Len(); i++ {
		got[string(oneof.Fields().Get(i).Name())] = true
	}
	if len(got) != len(browserOperationVocabulary) {
		t.Fatalf("command vocabulary size = %d, proto fields = %d", len(browserOperationVocabulary), len(got))
	}
	for _, name := range browserOperationVocabulary {
		if !got[name] {
			t.Fatalf("vocabulary names %q, which is not a Command field", name)
		}
	}
	prohibited := []string{"capture", "audio", "assets", "app_bus", "force_release", "release_capture", "presentation", "descriptor", "raw", "bytes"}
	for _, name := range browserOperationVocabulary {
		for _, bad := range prohibited {
			if strings.Contains(name, bad) {
				t.Fatalf("prohibited operation %q in vocabulary", name)
			}
		}
	}
}

func TestUnwatchedTopicsBroadcastToEverySession(t *testing.T) {
	conn := startFakeGRPC(t, newFakeBackend())
	store := websecurity.Open(t.TempDir(), websecurity.DefaultOptions())
	hub := uichanges.New()
	srv := &Server{Conn: conn, Auth: store, Changes: hub}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns := []*wsConn{
		{out: make(chan outFrame, 8), done: make(chan struct{})},
		{out: make(chan outFrame, 8), done: make(chan struct{})},
	}
	for _, c := range conns {
		go srv.watchAccess(ctx, c)
		go srv.watchWakeword(ctx, c)
	}
	// Let both watchers install their hub subscriptions before publishing.
	time.Sleep(50 * time.Millisecond)

	readEvent := func(c *wsConn, want webv1.Topic) *webv1.Event {
		t.Helper()
		select {
		case frame := <-c.out:
			var msg webv1.ServerFrame
			if err := proto.Unmarshal(frame.payload, &msg); err != nil {
				t.Fatalf("unmarshal event: %v", err)
			}
			event := msg.GetEvent()
			if event == nil || event.GetTopic() != want {
				t.Fatalf("event = %+v, want topic %v", event, want)
			}
			return event
		case <-time.After(3 * time.Second):
			t.Fatalf("no %v event delivered", want)
			return nil
		}
	}

	hub.Publish(uichanges.Access)
	for _, c := range conns {
		event := readEvent(c, webv1.Topic_TOPIC_ACCESS)
		if event.GetAccess() == nil || event.GetAccess().GetStatus() == nil {
			t.Fatalf("access event has no view: %+v", event)
		}
	}
	hub.Publish(uichanges.Wakeword)
	for _, c := range conns {
		event := readEvent(c, webv1.Topic_TOPIC_WAKEWORD)
		if event.GetWakeword() == nil || len(event.GetWakeword().GetModels()) == 0 {
			t.Fatalf("wakeword event has no catalog: %+v", event)
		}
	}
}

func TestAccessChangesReachEveryWebSocketSession(t *testing.T) {
	env := newTestEnv(t)
	first, _, _ := connectSession(t, env)
	second, _, _ := connectSession(t, env)

	for _, c := range []*wsTestConn{first, second} {
		id := uint64(10)
		if c == second {
			id = 20
		}
		c.send(&webv1.ClientFrame{RequestId: id, Body: &webv1.ClientFrame_Subscribe{
			Subscribe: &webv1.Subscribe{Topic: webv1.Topic_TOPIC_ACCESS},
		}})
		if snap := c.waitSnapshot(t, id); snap.GetAccess() == nil {
			t.Fatalf("access snapshot missing: %+v", snap)
		}
	}

	const keyName = "cross-session-key"
	first.send(&webv1.ClientFrame{RequestId: 30, Body: &webv1.ClientFrame_Command{
		Command: &webv1.Command{Op: &webv1.Command_CreateApiKey{
			CreateApiKey: &webv1.CreateApiKey{Name: keyName},
		}},
	}})
	first.waitResult(t, 30)

	for name, c := range map[string]*wsTestConn{"first": first, "second": second} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if time.Now().After(deadline) {
				t.Fatalf("%s session did not receive the access event", name)
			}
			frame := c.read()
			event := frame.GetEvent()
			if event == nil || event.GetTopic() != webv1.Topic_TOPIC_ACCESS {
				continue
			}
			found := false
			for _, key := range event.GetAccess().GetKeys() {
				if key.GetName() == keyName {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s access event lacks %q: %+v", name, keyName, event.GetAccess())
			}
			break
		}
	}
}

func TestOutboundQueueMarksResync(t *testing.T) {
	c := &wsConn{
		out:    make(chan outFrame, 1),
		resync: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	c.send(&webv1.ServerFrame{})
	c.send(&webv1.ServerFrame{})
	select {
	case <-c.resync:
	default:
		t.Fatal("a full outbound queue did not request a resync")
	}
}

func TestAppSettingsPatchValidationAndSnapshot(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 40, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_AppSettings{AppSettings: &webv1.AppSettingsUpdate{
			AppId: "org.example.player", ExpectedRevision: 2,
			Changes: []*webv1.AppSettingChangeView{{Key: "effect", ValueJson: `"static"`}},
		}},
	}}})
	result := conn.waitResult(t, 40)
	if result.GetAppSettings().GetRevision() != 3 {
		t.Fatalf("app settings result: %v", result)
	}

	cases := []struct {
		id      uint64
		changes []*webv1.AppSettingChangeView
		want    webv1.ErrorCode
	}{
		{41, []*webv1.AppSettingChangeView{{Key: "undeclared", ValueJson: `1`}}, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{42, []*webv1.AppSettingChangeView{{Key: "effect"}}, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{43, []*webv1.AppSettingChangeView{{Key: "effect", ValueJson: `"x"`, Clear: true}}, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
		{44, nil, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT},
	}
	tooMany := make([]*webv1.AppSettingChangeView, maxAppSettingChanges+1)
	for i := range tooMany {
		tooMany[i] = &webv1.AppSettingChangeView{Key: "effect", ValueJson: `"pulse"`}
	}
	cases = append(cases, struct {
		id      uint64
		changes []*webv1.AppSettingChangeView
		want    webv1.ErrorCode
	}{45, tooMany, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT})

	for _, tc := range cases {
		conn.send(&webv1.ClientFrame{RequestId: tc.id, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
			Op: &webv1.Command_AppSettings{AppSettings: &webv1.AppSettingsUpdate{
				AppId: "org.example.player", ExpectedRevision: 2, Changes: tc.changes,
			}},
		}}})
		for {
			frame := conn.read()
			if frame.GetRequestId() != tc.id {
				continue
			}
			if frame.GetError() == nil || frame.GetError().GetCode() != tc.want {
				t.Fatalf("request %d error = %v", tc.id, frame.GetError())
			}
			break
		}
	}
}

func TestAppSettingsChangesReachEveryApplicationSession(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 50, Body: &webv1.ClientFrame_Subscribe{Subscribe: &webv1.Subscribe{
		Topic: webv1.Topic_TOPIC_APPLICATION, AppId: "org.example.player",
	}}})
	conn.waitSnapshot(t, 50)

	env.srv.Changes.Publish(uichanges.AppSettings)
	for {
		frame := conn.read()
		event := frame.GetEvent()
		if event == nil || event.GetApplication() == nil {
			continue
		}
		if event.GetTopic() != webv1.Topic_TOPIC_APPLICATION || event.GetAppId() != "org.example.player" {
			t.Fatalf("unexpected app event: %v", event)
		}
		return
	}
}

func TestImmediateSettingsGroupsDoNotSubmitUnrelatedFields(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 60, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Settings{Settings: &webv1.SettingsUpdate{
			ExpectedRevision: 5,
			Group: &webv1.SettingsUpdate_MutedIndicator{MutedIndicator: &webv1.MutedIndicatorSettings{
				Mode:    webv1.MutedIndicatorMode_MUTED_INDICATOR_MODE_IMMEDIATE,
				HasMode: true,
			}},
		}},
	}}})
	conn.waitResult(t, 60)
	env.back.mu.Lock()
	mode := env.back.settings.Values.GetMutedMicrophoneIndicator().GetMode()
	delay := env.back.settings.Values.GetMutedMicrophoneIndicator().GetDelayMs()
	timezone := env.back.settings.Values.GetTimezone()
	muted := env.back.settings.Values.GetMicrophoneMuted()
	env.back.mu.Unlock()
	if mode != frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_IMMEDIATE || delay != 30000 {
		t.Fatalf("indicator mode change rewrote delay: mode=%v delay=%d", mode, delay)
	}
	if timezone != "UTC" || muted {
		t.Fatalf("unrelated fields changed: timezone=%q muted=%v", timezone, muted)
	}

	// The microphone gate is its own group; it must not carry a delay draft or
	// any device text field.
	conn.send(&webv1.ClientFrame{RequestId: 61, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Settings{Settings: &webv1.SettingsUpdate{
			ExpectedRevision: 6,
			Group:            &webv1.SettingsUpdate_Microphone{Microphone: &webv1.MicrophoneSettings{Muted: true}},
		}},
	}}})
	conn.waitResult(t, 61)
	env.back.mu.Lock()
	timezone = env.back.settings.Values.GetTimezone()
	hostname := env.back.settings.Values.GetHostname()
	startup := env.back.settings.Values.GetStartupSoundEnabled()
	muted = env.back.settings.Values.GetMicrophoneMuted()
	env.back.mu.Unlock()
	if !muted || timezone != "UTC" || hostname != "capsula" || startup {
		t.Fatalf("microphone gate changed unrelated fields: muted=%v timezone=%q hostname=%q startup=%v",
			muted, timezone, hostname, startup)
	}
}

// The overview app listing carries each installed app's declared actions, so
// the SPA can offer them from the card without a configuration read per app.
func TestOverviewListingCarriesAppActions(t *testing.T) {
	env := newTestEnv(t)
	reg, err := registry.Open(t.TempDir(), env.srv.Logs)
	if err != nil {
		t.Fatal(err)
	}
	err = reg.RegisterBuiltin(&registry.App{Manifest: &manifest.Manifest{
		ID: "capsulad.clock", Name: "Clock", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
		Actions: []manifest.Action{
			{ID: "timer_start", Label: "Start timer"},
			{ID: "timer_cancel", Label: "Cancel timer",
				Description: "Cancel the running timer.", ConfirmBeforeRun: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	env.srv.Apps = reg

	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 12, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	apps := conn.waitSnapshot(t, 12).GetOverview().GetApps()

	var clock, player *webv1.AppView
	for _, app := range apps {
		switch app.GetAppId() {
		case "capsulad.clock":
			clock = app
		case "org.example.player":
			player = app
		}
	}
	if clock == nil || player == nil {
		t.Fatalf("overview apps = %v", apps)
	}
	actions := clock.GetActions()
	if len(actions) != 2 {
		t.Fatalf("clock actions = %v, want 2", actions)
	}
	if actions[0].GetActionId() != "timer_start" || actions[0].GetLabel() != "Start timer" ||
		actions[0].GetConfirmBeforeRun() {
		t.Fatalf("start action = %v", actions[0])
	}
	if actions[1].GetActionId() != "timer_cancel" || !actions[1].GetConfirmBeforeRun() ||
		actions[1].GetDescription() != "Cancel the running timer." {
		t.Fatalf("cancel action = %v", actions[1])
	}
	// An app the registry does not know is listed without invented actions.
	if len(player.GetActions()) != 0 {
		t.Fatalf("player actions = %v, want none", player.GetActions())
	}
}

func TestApplicationSnapshotSelectedRoles(t *testing.T) {
	env := newTestEnv(t)
	reg, err := registry.Open(t.TempDir(), env.srv.Logs)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterBuiltin(&registry.App{Manifest: &manifest.Manifest{
		ID: "capsulad.clock", Name: "Clock", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
	}}); err != nil {
		t.Fatal(err)
	}
	env.srv.Apps = reg
	env.back.mu.Lock()
	env.back.settings.Values.DefaultAssistantAppId = "capsulad.clock"
	env.back.settings.Values.DefaultAppByResource["light.ring.main"] = "capsulad.clock"
	env.back.settings.Values.DefaultAppByResource["other"] = "org.example.player"
	env.back.mu.Unlock()

	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 32, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_APPLICATION, AppId: "capsulad.clock"},
	}})
	view := conn.waitSnapshot(t, 32).GetApplication()
	if !view.GetSelectedDefault() || strings.Join(view.GetSelectedDefaultRoles(), ",") != "assistant,display.segment.front,light.ring.main" {
		t.Fatalf("selected default = %v, roles = %v", view.GetSelectedDefault(), view.GetSelectedDefaultRoles())
	}

	conn.send(&webv1.ClientFrame{RequestId: 33, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_APPLICATION, AppId: "org.example.idle"},
	}})
	view = conn.waitSnapshot(t, 33).GetApplication()
	if view.GetSelectedDefault() || len(view.GetSelectedDefaultRoles()) != 0 {
		t.Fatalf("unselected app snapshot = %v", view)
	}
}

// TestOverviewHidesPairedAddressAndUnlaunchedProviders covers two browser
// boundaries at once: a paired device is identified by an opaque ID (never its
// Bluetooth address), and an installed media player that was never launched is
// not offered as a provider because nothing can route to its service.
func TestOverviewHidesPairedAddressAndUnlaunchedProviders(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 30, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 30).GetOverview()

	paired := overview.GetPairedDevices()
	if len(paired) != 1 || paired[0].GetName() != "Phone" {
		t.Fatalf("paired devices = %v", paired)
	}
	if paired[0].GetDeviceId() == "" {
		t.Fatal("paired device has no opaque id")
	}
	if want := pairedDeviceID("AA:BB:CC:DD:EE:FF"); paired[0].GetDeviceId() != want {
		t.Fatalf("device id = %q, want %q", paired[0].GetDeviceId(), want)
	}
	// The address must not survive anywhere in the serialized view, including
	// unknown fields or a stray mirrored field.
	raw, err := proto.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("AA:BB:CC:DD:EE:FF")) ||
		bytes.Contains(bytes.ToLower(raw), []byte("aa:bb:cc:dd:ee:ff")) {
		t.Fatalf("overview leaks the device address: %q", raw)
	}

	providers := overview.GetProviders()
	if len(providers) != 1 || providers[0].GetAppId() != "org.example.player" {
		t.Fatalf("providers = %v, want only the running player", providers)
	}
}

// stubAdapterName records the names the browser asked for. The embedded
// simulator supplies the rest of bluetooth.Manager, so the test observes the
// same status the daemon would publish after a rename.
type stubAdapterName struct {
	*bluetooth.Memory
	mu    sync.Mutex
	names []string
}

func (s *stubAdapterName) SetAdapterName(ctx context.Context, name string) error {
	s.mu.Lock()
	s.names = append(s.names, name)
	s.mu.Unlock()
	return s.Memory.SetAdapterName(ctx, name)
}

func (s *stubAdapterName) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

// TestSetBluetoothAdapterName pins the administrator-only rename: the trimmed
// name reaches the manager, the result carries the observed adapter status,
// and an invalid name is refused without touching the adapter.
func TestSetBluetoothAdapterName(t *testing.T) {
	env := newTestEnv(t)
	bt := &stubAdapterName{Memory: bluetooth.NewMemory()}
	env.srv.Bluetooth = bt
	conn, _, _ := connectSession(t, env)

	command := func(reqID uint64, name string) *webv1.ClientFrame {
		return &webv1.ClientFrame{RequestId: reqID, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
			Op: &webv1.Command_Bluetooth{Bluetooth: &webv1.BluetoothCommand{
				Action: &webv1.BluetoothCommand_SetAdapterName{SetAdapterName: &webv1.SetAdapterName{Name: name}},
			}},
		}}}
	}

	// The framework status path is authoritative: it carries the A2DP-readiness
	// detail that the manager's own Status does not, and the browser must
	// receive exactly that observed state.
	env.back.mu.Lock()
	env.back.btStatus = &frameworkv1.BluetoothStatus{
		Present: true, Powered: true, IncomingConnections: true, Adapter: "hci0",
		Name: "Kitchen Speaker", Detail: "BlueZ; audio profile is not advertised",
	}
	env.back.mu.Unlock()

	conn.send(command(51, "  Kitchen Speaker  "))
	result := conn.waitResult(t, 51).GetBluetooth()
	if got := result.GetStatus().GetName(); got != "Kitchen Speaker" {
		t.Fatalf("result status name = %q, want the observed name", got)
	}
	if !result.GetStatus().GetPresent() {
		t.Fatalf("result status lost the adapter presence: %+v", result.GetStatus())
	}
	if got := result.GetStatus().GetDetail(); !strings.Contains(got, "audio profile is not advertised") {
		t.Fatalf("result status lost the framework detail: %q", got)
	}
	if got, want := result.GetMessage(), "Device name changed to Kitchen Speaker"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	if calls := bt.calls(); len(calls) != 1 || calls[0] != "Kitchen Speaker" {
		t.Fatalf("manager calls = %v, want the trimmed name", calls)
	}

	// Invalid names never reach the manager: empty, whitespace-only, a control
	// character and one byte over the limit are all refused as invalid.
	bad := []string{"", "   ", "bad\x00name", "line\nbreak", strings.Repeat("a", bluetooth.MaxAdapterNameBytes+1)}
	for i, name := range bad {
		reqID := uint64(52 + i)
		conn.send(command(reqID, name))
		for {
			frame := conn.read()
			if frame.GetRequestId() != reqID {
				continue
			}
			e := frame.GetError()
			if e == nil {
				t.Fatalf("name %d was accepted", i)
			}
			if e.GetCode() != webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
				t.Fatalf("name %d error = %v, want invalid argument", i, e)
			}
			break
		}
	}
	if calls := bt.calls(); len(calls) != 1 {
		t.Fatalf("invalid names reached the manager: %v", calls)
	}

	// A failed observed-status read is reported like the other adapter
	// commands instead of returning a nil status with a success message.
	env.back.mu.Lock()
	env.back.btStatusErr = status.Error(codes.Unavailable, "bluetooth disappeared")
	env.back.mu.Unlock()
	conn.send(command(59, "Another name"))
	for {
		frame := conn.read()
		if frame.GetRequestId() != 59 {
			continue
		}
		e := frame.GetError()
		if e == nil {
			t.Fatal("rename succeeded despite an unreadable status")
		}
		if e.GetCode() != webv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
			t.Fatalf("status read error = %v, want unavailable", e)
		}
		break
	}
	env.back.mu.Lock()
	env.back.btStatusErr = nil
	env.back.mu.Unlock()

	// Without a manager the command is unavailable, never a nil dereference.
	env.srv.Bluetooth = nil
	conn.send(command(60, "Another name"))
	for {
		frame := conn.read()
		if frame.GetRequestId() != 60 {
			continue
		}
		e := frame.GetError()
		if e == nil {
			t.Fatal("rename without a Bluetooth manager was accepted")
		}
		if e.GetCode() != webv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
			t.Fatalf("nil manager error = %v, want unavailable", e)
		}
		break
	}
}

// TestUnpairDeviceByOpaqueID proves the browser unpairs without ever holding
// the address: the opaque ID resolves against the pairing list and only the
// framework call carries the real address.
func TestUnpairDeviceByOpaqueID(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	unpairedCalls := func() []string {
		env.back.mu.Lock()
		defer env.back.mu.Unlock()
		return append([]string(nil), env.back.unpaired...)
	}

	conn.send(&webv1.ClientFrame{RequestId: 31, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Bluetooth{Bluetooth: &webv1.BluetoothCommand{
			Action: &webv1.BluetoothCommand_Unpair{Unpair: &webv1.UnpairDevice{
				DeviceId: pairedDeviceID("aa:bb:cc:dd:ee:ff"),
			}},
		}},
	}}})
	result := conn.waitResult(t, 31).GetBluetooth()
	if !strings.Contains(result.GetMessage(), "Phone") {
		t.Fatalf("unpair message = %q, want the device name", result.GetMessage())
	}
	if strings.Contains(result.GetMessage(), "AA:BB") || strings.Contains(result.GetMessage(), "aa:bb") {
		t.Fatalf("unpair message leaks the address: %q", result.GetMessage())
	}
	if got := unpairedCalls(); len(got) != 1 || got[0] != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("framework unpair calls = %v, want the real address", got)
	}

	// An unknown ID is refused before any framework call.
	conn.send(&webv1.ClientFrame{RequestId: 32, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Bluetooth{Bluetooth: &webv1.BluetoothCommand{
			Action: &webv1.BluetoothCommand_Unpair{Unpair: &webv1.UnpairDevice{DeviceId: "000000000000"}},
		}},
	}}})
	for {
		frame := conn.read()
		if frame.GetRequestId() != 32 {
			continue
		}
		if e := frame.GetError(); e != nil {
			if e.GetCode() != webv1.ErrorCode_ERROR_CODE_NOT_FOUND {
				t.Fatalf("unknown device error = %v", e)
			}
			break
		}
	}
	if got := unpairedCalls(); len(got) != 1 {
		t.Fatalf("unknown device still unpair-ed: %v", got)
	}
}

// TestUnnamedPairedDeviceNeverExposesAddress covers BlueZ handing back the
// address as the name of a device without an alias: neither the overview's
// display name nor the unpair success message may carry the address.
func TestUnnamedPairedDeviceNeverExposesAddress(t *testing.T) {
	env := newTestEnv(t)
	const ghostAddress = "11:22:33:44:55:66"
	env.back.mu.Lock()
	env.back.extraPaired = []*frameworkv1.PairedDevice{{Address: ghostAddress, Name: ghostAddress, Trusted: true}}
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 40, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 40).GetOverview()

	var ghost *webv1.PairedDeviceView
	for _, device := range overview.GetPairedDevices() {
		if device.GetDeviceId() == pairedDeviceID(ghostAddress) {
			ghost = device
		}
	}
	if ghost == nil {
		t.Fatalf("unnamed device missing from overview: %v", overview.GetPairedDevices())
	}
	if ghost.GetName() != "" {
		t.Fatalf("unnamed device display name = %q, want empty", ghost.GetName())
	}
	raw, err := proto.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(ghostAddress)) ||
		bytes.Contains(raw, []byte(strings.ReplaceAll(ghostAddress, ":", ""))) {
		t.Fatalf("overview leaks the unnamed device address: %q", raw)
	}

	conn.send(&webv1.ClientFrame{RequestId: 41, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Bluetooth{Bluetooth: &webv1.BluetoothCommand{
			Action: &webv1.BluetoothCommand_Unpair{Unpair: &webv1.UnpairDevice{DeviceId: ghost.GetDeviceId()}},
		}},
	}}})
	result := conn.waitResult(t, 41).GetBluetooth()
	if result.GetMessage() != "Device unpaired" {
		t.Fatalf("unpair message = %q, want the unnamed fallback", result.GetMessage())
	}
	if strings.Contains(result.GetMessage(), ghostAddress) ||
		strings.Contains(result.GetMessage(), strings.ReplaceAll(ghostAddress, ":", "")) {
		t.Fatalf("unpair message leaks the address: %q", result.GetMessage())
	}
	env.back.mu.Lock()
	unpaired := append([]string(nil), env.back.unpaired...)
	env.back.mu.Unlock()
	if len(unpaired) != 1 || unpaired[0] != ghostAddress {
		t.Fatalf("framework unpair calls = %v, want the real address", unpaired)
	}
}

// TestDismissFalseDetection covers the dismissal round trip: the snapshot
// carries an opaque detection ID, the command removes exactly that entry, and
// an unknown ID is rejected rather than silently ignored.
func TestDismissFalseDetection(t *testing.T) {
	env := newTestEnv(t)
	env.srv.Wakeword = &fakeWakeword{detections: []wakeword.Detection{
		{ID: "det_keep", ModelID: "m1", DisplayName: "Marusia", UnixMS: 200},
		{ID: "det_drop", ModelID: "m1", DisplayName: "Marusia", UnixMS: 100},
	}}
	conn, _, _ := connectSession(t, env)

	detections := func(requestID uint64) []*webv1.WakewordDetectionView {
		conn.send(&webv1.ClientFrame{RequestId: requestID, Body: &webv1.ClientFrame_SnapshotRequest{
			SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_WAKEWORD},
		}})
		return conn.waitSnapshot(t, requestID).GetWakeword().GetDetections()
	}

	before := detections(40)
	if len(before) != 2 {
		t.Fatalf("detections = %v, want 2", before)
	}
	for _, d := range before {
		if d.GetDetectionId() == "" {
			t.Fatalf("detection %v has no id", d)
		}
	}

	conn.send(&webv1.ClientFrame{RequestId: 41, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_FalseDetection{FalseDetection: &webv1.FalseDetection{DetectionId: " det_drop "}},
	}}})
	if conn.waitResult(t, 41).GetDeleted() == nil {
		t.Fatal("dismissal did not return a deleted result")
	}

	after := detections(42)
	if len(after) != 1 || after[0].GetDetectionId() != "det_keep" {
		t.Fatalf("detections after dismissal = %v", after)
	}

	conn.send(&webv1.ClientFrame{RequestId: 43, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_FalseDetection{FalseDetection: &webv1.FalseDetection{DetectionId: "det_gone"}},
	}}})
	for {
		frame := conn.read()
		if frame.GetRequestId() != 43 {
			continue
		}
		if e := frame.GetError(); e != nil {
			if e.GetCode() != webv1.ErrorCode_ERROR_CODE_NOT_FOUND {
				t.Fatalf("unknown detection error = %v", e)
			}
			break
		}
	}
}

// A failed run listing proves nothing about which providers are live, so the
// overview offers none instead of listing installed-but-unroutable players.
func TestOverviewDropsProvidersWhenRunsAreUnknown(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.runsErr = status.Error(codes.Unavailable, "run table unavailable")
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 50, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 50).GetOverview()
	if len(overview.GetProviders()) != 0 {
		t.Fatalf("providers = %v, want none when runs are unknown", overview.GetProviders())
	}
	if len(overview.GetRuns()) != 0 {
		t.Fatalf("runs = %v, want none", overview.GetRuns())
	}
}
