package webui

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	webv1 "capsulad/gen/go/capsula/web/v1"
	"capsulad/internal/uichanges"
)

// --- shared harness --------------------------------------------------------

func dialBufconn(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func testWsConn(srv *Server) *wsConn {
	return &wsConn{
		srv:      srv,
		out:      make(chan outFrame, 64),
		done:     make(chan struct{}),
		subs:     map[string]*subscription{},
		training: map[string]context.CancelFunc{},
		commands: map[string]context.CancelFunc{},
	}
}

func nextServerFrame(c *wsConn, wait time.Duration) *webv1.ServerFrame {
	select {
	case raw := <-c.out:
		var msg webv1.ServerFrame
		if err := proto.Unmarshal(raw.payload, &msg); err != nil {
			return nil
		}
		return &msg
	case <-time.After(wait):
		return nil
	}
}

func waitTrainingFrame(t *testing.T, c *wsConn, trainingID string, wait time.Duration) *webv1.Progress {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		frame := nextServerFrame(c, time.Until(deadline))
		if frame == nil {
			break
		}
		if p := frame.GetProgress(); p != nil && p.GetTrainingId() == trainingID {
			return p
		}
	}
	return nil
}

func waitTrainingRegistered(c *wsConn, trainingID string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.trainingMu.Lock()
		_, ok := c.training[trainingID]
		c.trainingMu.Unlock()
		if !ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// --- training stream end semantics ----------------------------------------

type scriptedTrainer struct {
	frameworkv1.UnimplementedAssistantServiceServer
	stages    []*frameworkv1.TrainWakewordEvent
	block     bool
	endErr    error
	cancelled chan struct{}
	started   chan struct{}
}

func (s *scriptedTrainer) TrainWakewordClassifier(_ *frameworkv1.TrainWakewordClassifierRequest, stream frameworkv1.AssistantService_TrainWakewordClassifierServer) error {
	if s.started != nil {
		close(s.started)
	}
	for _, ev := range s.stages {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	if s.block {
		<-stream.Context().Done()
		if s.cancelled != nil {
			close(s.cancelled)
		}
		return status.Error(codes.Canceled, "cancelled")
	}
	return s.endErr
}

func startTraining(t *testing.T, trainer *scriptedTrainer) (*wsConn, uint64, string) {
	t.Helper()
	conn := dialBufconn(t, func(srv *grpc.Server) {
		frameworkv1.RegisterAssistantServiceServer(srv, trainer)
	})
	srv := &Server{Conn: conn}
	c := testWsConn(srv)
	srv.opTrain(c, 7, &webv1.TrainWakeword{
		DisplayName: "Marusia", Language: "ru",
		WordSampleIds: []string{"s1"}, RoomSampleIds: []string{"s2"}, SpeechSampleIds: []string{"s3"},
	})
	frame := nextServerFrame(c, 2*time.Second)
	if frame == nil || frame.GetResult().GetTraining().GetTrainingId() == "" {
		t.Fatalf("training handle: %+v", frame)
	}
	return c, 7, frame.GetResult().GetTraining().GetTrainingId()
}

// TestTrainingCleanEOFIsNotAnError covers the daemon returning nil after its
// final installed event. The client's next Recv yields io.EOF; that is clean
// completion, not a failure the user should see.
func TestTrainingCleanEOFIsNotAnError(t *testing.T) {
	c, _, id := startTraining(t, &scriptedTrainer{stages: []*frameworkv1.TrainWakewordEvent{
		{Stage: "fit", Fraction: 0.5},
		{Stage: "installed", Fraction: 1, Classifier: &frameworkv1.WakewordClassifierInfo{ModelId: "word.personal"}},
	}})
	var sawInstalled bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p := waitTrainingFrame(t, c, id, time.Until(deadline))
		if p == nil {
			break
		}
		if p.GetStage() == "error" {
			t.Fatalf("clean stream end produced an error progress: %q", p.GetLog())
		}
		if p.GetStage() == "installed" {
			sawInstalled = true
		}
	}
	if !sawInstalled {
		t.Fatal("installed progress was not delivered")
	}
	if !waitTrainingRegistered(c, id) {
		t.Fatal("training registration did not finish after a clean stream end")
	}
}

// TestTrainingEOFWithoutInstallIsReported covers a stream that ends before the
// installed event: success must never be synthesized.
func TestTrainingEOFWithoutInstallIsReported(t *testing.T) {
	c, _, id := startTraining(t, &scriptedTrainer{stages: []*frameworkv1.TrainWakewordEvent{{Stage: "fit"}}})
	for {
		p := waitTrainingFrame(t, c, id, 2*time.Second)
		if p == nil {
			t.Fatal("no error progress after an unconfirmed stream end")
		}
		if p.GetStage() == "error" {
			if p.GetLog() != msgTrainingEndedWithoutInstall {
				t.Fatalf("unconfirmed end message = %q, want %q", p.GetLog(), msgTrainingEndedWithoutInstall)
			}
			return
		}
	}
}

// TestTrainingCancelDuringInstallReportsUnknownModel covers the cancel/commit
// race: once the run reported installation stages, a cancel may have lost to
// the atomic rename, so the browser must not be told the old model is intact.
func TestTrainingCancelDuringInstallReportsUnknownModel(t *testing.T) {
	trainer := &scriptedTrainer{
		stages:    []*frameworkv1.TrainWakewordEvent{{Stage: "fit"}, {Stage: "install", Fraction: 0.98}},
		block:     true,
		cancelled: make(chan struct{}),
	}
	c, _, id := startTraining(t, trainer)
	if p := waitTrainingFrame(t, c, id, 2*time.Second); p == nil || p.GetStage() != "fit" {
		t.Fatalf("fit progress: %+v", p)
	}
	if p := waitTrainingFrame(t, c, id, 2*time.Second); p == nil || p.GetStage() != "install" {
		t.Fatalf("install progress: %+v", p)
	}
	c.cancelOperation(id)
	p := waitTrainingFrame(t, c, id, 2*time.Second)
	if p == nil || p.GetStage() != "error" {
		t.Fatalf("cancellation progress: %+v", p)
	}
	if p.GetLog() != msgTrainingResultUnknown {
		t.Fatalf("cancel during install message = %q, want %q", p.GetLog(), msgTrainingResultUnknown)
	}
	select {
	case <-trainer.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("training stream context was not cancelled")
	}
}

// TestTrainingCancelBeforeInstallReportsUnknownModel covers a local cancel
// after early progress. Last observed progress cannot prove the daemon never
// reached installation: gRPC buffering and outbound backpressure can hide a
// later stage, so the outcome stays unknown.
func TestTrainingCancelBeforeInstallReportsUnknownModel(t *testing.T) {
	trainer := &scriptedTrainer{
		stages:    []*frameworkv1.TrainWakewordEvent{{Stage: "fit", Fraction: 0.4}},
		block:     true,
		cancelled: make(chan struct{}),
	}
	c, _, id := startTraining(t, trainer)
	if p := waitTrainingFrame(t, c, id, 2*time.Second); p == nil || p.GetStage() != "fit" {
		t.Fatalf("fit progress: %+v", p)
	}
	c.cancelOperation(id)
	p := waitTrainingFrame(t, c, id, 2*time.Second)
	if p == nil || p.GetStage() != "error" {
		t.Fatalf("cancellation progress: %+v", p)
	}
	if p.GetLog() != msgTrainingResultUnknown {
		t.Fatalf("early cancel message = %q, want %q", p.GetLog(), msgTrainingResultUnknown)
	}
	select {
	case <-trainer.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("training stream context was not cancelled")
	}
}

// TestTrainingLocalCancelBeforeAnyProgressReportsUnknownModel covers a local
// cancel on an established stream with no observed progress at all.
func TestTrainingLocalCancelBeforeAnyProgressReportsUnknownModel(t *testing.T) {
	trainer := &scriptedTrainer{
		block:     true,
		cancelled: make(chan struct{}),
		started:   make(chan struct{}),
	}
	c, _, id := startTraining(t, trainer)
	select {
	case <-trainer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("training stream was not established")
	}
	c.cancelOperation(id)
	p := waitTrainingError(t, c, id)
	if p == nil || p.GetLog() != msgTrainingResultUnknown {
		t.Fatalf("empty-stage cancel = %+v, want %q", p, msgTrainingResultUnknown)
	}
	select {
	case <-trainer.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("training stream context was not cancelled")
	}
}

func waitTrainingError(t *testing.T, c *wsConn, trainingID string) *webv1.Progress {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p := waitTrainingFrame(t, c, trainingID, time.Until(deadline))
		if p == nil {
			return nil
		}
		if p.GetStage() == "error" {
			return p
		}
	}
	return nil
}

// TestTrainingExplicitPrecommitCancelIsUnchanged covers the daemon's explicit
// precommit cancellation status, which is the only generic-Canceled shape that
// proves the model file was not replaced.
func TestTrainingExplicitPrecommitCancelIsUnchanged(t *testing.T) {
	c, _, id := startTraining(t, &scriptedTrainer{
		stages: []*frameworkv1.TrainWakewordEvent{{Stage: "fit"}},
		endErr: status.Error(codes.Canceled, "training cancelled"),
	})
	p := waitTrainingError(t, c, id)
	if p == nil || p.GetLog() != msgTrainingCancelledUnchanged {
		t.Fatalf("precommit cancellation = %+v, want %q", p, msgTrainingCancelledUnchanged)
	}
}

// TestTrainingGenericCancelAfterInstallReportsUnknownModel covers a Canceled
// status without the daemon's precommit message: the final installed send can
// fail after the rename committed, so the UI must not claim the model is
// unchanged.
func TestTrainingGenericCancelAfterInstallReportsUnknownModel(t *testing.T) {
	c, _, id := startTraining(t, &scriptedTrainer{
		stages: []*frameworkv1.TrainWakewordEvent{{Stage: "fit"}, {Stage: "install"}},
		endErr: status.Error(codes.Canceled, "context canceled"),
	})
	p := waitTrainingError(t, c, id)
	if p == nil || p.GetLog() != msgTrainingResultUnknown {
		t.Fatalf("generic late cancellation = %+v, want %q", p, msgTrainingResultUnknown)
	}
}

// TestTrainingGenericCancelEarlyReportsUnknownModel covers a generic Canceled
// after only early progress. Even then the daemon may have advanced past the
// browser's last observed event.
func TestTrainingGenericCancelEarlyReportsUnknownModel(t *testing.T) {
	c, _, id := startTraining(t, &scriptedTrainer{
		stages: []*frameworkv1.TrainWakewordEvent{{Stage: "fit"}},
		endErr: status.Error(codes.Canceled, "context canceled"),
	})
	p := waitTrainingError(t, c, id)
	if p == nil || p.GetLog() != msgTrainingResultUnknown {
		t.Fatalf("generic early cancellation = %+v, want %q", p, msgTrainingResultUnknown)
	}
}

// TestTrainingDeadlineReportsUnknownModel covers a deadline-shaped stream end
// on the established-stream path: trainErrorMessage's "unchanged" deadline
// text must not be reachable after a possible commit.
func TestTrainingDeadlineReportsUnknownModel(t *testing.T) {
	c, _, id := startTraining(t, &scriptedTrainer{
		stages: []*frameworkv1.TrainWakewordEvent{{Stage: "fit"}},
		endErr: status.Error(codes.DeadlineExceeded, "training timed out"),
	})
	p := waitTrainingError(t, c, id)
	if p == nil || p.GetLog() != msgTrainingResultUnknown {
		t.Fatalf("deadline cancellation = %+v, want %q", p, msgTrainingResultUnknown)
	}
}

// --- subscription ordering and revision reservation -----------------------

// TestSubscribeSnapshotPrecedesRacingEvent forces a mutation while the initial
// snapshot is still being built. The snapshot must be the first frame for the
// subscription, and the later event must carry a higher envelope revision even
// though it was produced before the snapshot finished.
func TestSubscribeSnapshotPrecedesRacingEvent(t *testing.T) {
	env := newTestEnv(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	env.back.mu.Lock()
	env.back.overviewGate = gate
	env.back.overviewEntered = entered
	env.back.mu.Unlock()

	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 70, Body: &webv1.ClientFrame_Subscribe{
		Subscribe: &webv1.Subscribe{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	// Wait until the snapshot read is underway: the invalidation subscription
	// is installed synchronously before it, so this publish cannot be lost.
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the overview snapshot never started")
	}
	env.srv.Changes.Publish(uichanges.Bluetooth)
	close(gate)

	first := conn.read()
	snapshot := first.GetSnapshot()
	if first.GetRequestId() != 70 || snapshot == nil {
		t.Fatalf("first subscription frame = %+v, want the snapshot", first)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("racing overview event was never delivered")
		}
		frame := conn.read()
		event := frame.GetEvent()
		if event == nil || event.GetTopic() != webv1.Topic_TOPIC_OVERVIEW || event.GetOverview() == nil {
			continue
		}
		if event.GetRevision() <= snapshot.GetRevision() {
			t.Fatalf("event revision %d is not newer than snapshot revision %d",
				event.GetRevision(), snapshot.GetRevision())
		}
		return
	}
}

// --- log tail and backlog --------------------------------------------------

func TestLogsTailKeepsNewestRecords(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.logs = nil
	for i := range 600 {
		env.back.logs = append(env.back.logs, &frameworkv1.LogRecord{
			MonotonicTimeNs: uint64(i + 1), Component: "daemon", Severity: "INFO",
			Message: fmt.Sprintf("record-%d", i+1),
		})
	}
	env.back.mu.Unlock()

	tail := env.srv.logsTail(context.Background(), "", 200)
	if len(tail) != 200 {
		t.Fatalf("tail length = %d, want 200", len(tail))
	}
	if tail[0].TimeNS != 401 || tail[len(tail)-1].TimeNS != 600 {
		t.Fatalf("tail covers %d..%d, want 401..600", tail[0].TimeNS, tail[len(tail)-1].TimeNS)
	}
}

type backlogDiagnostics struct {
	frameworkv1.UnimplementedDiagnosticsServiceServer
	entries []*frameworkv1.LogRecord
}

func (b *backlogDiagnostics) WatchLogs(_ *frameworkv1.WatchLogsRequest, stream frameworkv1.DiagnosticsService_WatchLogsServer) error {
	for _, entry := range b.entries {
		if err := stream.Send(entry); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

// TestLogsWatcherSkipsSnapshotBacklog proves the live watcher does not replay
// the ring records already delivered by the initial snapshot, while still
// delivering newer records and distinct records that merely share the coarse
// boundary timestamp.
func TestLogsWatcherSkipsSnapshotBacklog(t *testing.T) {
	diag := &backlogDiagnostics{entries: []*frameworkv1.LogRecord{
		{MonotonicTimeNs: 10, Component: "daemon", Message: "old-1"},
		{MonotonicTimeNs: 20, Component: "daemon", Message: "old-2"},
		{MonotonicTimeNs: 20, Component: "daemon", Message: "same-tick-distinct"},
		{MonotonicTimeNs: 30, Component: "daemon", Message: "new-1"},
	}}
	conn := dialBufconn(t, func(srv *grpc.Server) {
		frameworkv1.RegisterDiagnosticsServiceServer(srv, diag)
	})
	srv := &Server{Conn: conn}
	c := testWsConn(srv)
	boundary := logSnapshotBoundary{
		through: 20,
		seen: map[logIdentity]struct{}{
			{timeNS: 20, component: "daemon", message: "old-2"}: {},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		srv.watchLogs(ctx, c, "", boundary)
		close(done)
	}()

	frame := nextServerFrame(c, 2*time.Second)
	if frame == nil {
		t.Fatal("no log event was delivered")
	}
	entries := frame.GetEvent().GetLogs().GetEntries()
	if len(entries) != 2 {
		t.Fatalf("live log batch = %+v, want the distinct boundary record and the newer record", entries)
	}
	if entries[0].GetMessage() != "same-tick-distinct" || entries[1].GetMonotonicTimeNs() != 30 {
		t.Fatalf("live log batch = %+v", entries)
	}
	cancel()
	<-done
}

// --- reliable control frames and teardown ---------------------------------

func TestControlResponseIsNeverSilentlyDropped(t *testing.T) {
	previous := controlSendTimeout
	controlSendTimeout = 50 * time.Millisecond
	defer func() { controlSendTimeout = previous }()

	c := &wsConn{
		srv:      &Server{},
		out:      make(chan outFrame, 1),
		done:     make(chan struct{}),
		subs:     map[string]*subscription{},
		training: map[string]context.CancelFunc{},
		commands: map[string]context.CancelFunc{},
	}
	c.send(&webv1.ServerFrame{}) // occupy the only event slot
	c.sendResult(9, &webv1.Result{})
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("a result that could not be queued did not close the socket")
	}
}

func TestControlResponseFitsWhenTheWriterDrains(t *testing.T) {
	c := testWsConn(&Server{})
	c.sendResult(11, &webv1.Result{Value: &webv1.Result_ApiKey{ApiKey: &webv1.ApiKeyCreated{Name: "k", Value: "secret"}}})
	frame := nextServerFrame(c, time.Second)
	if frame == nil || frame.GetRequestId() != 11 || frame.GetResult().GetApiKey().GetValue() != "secret" {
		t.Fatalf("control result = %+v", frame)
	}
	select {
	case <-c.done:
		t.Fatal("healthy control delivery closed the socket")
	default:
	}
}

// TestCloseCancelsOwnedWorkBeforeBlockedWriter simulates a writer stuck inside
// a large frame. Teardown must release device/training ownership immediately,
// without waiting for writeMu or the write deadline.
func TestCloseCancelsOwnedWorkBeforeBlockedWriter(t *testing.T) {
	c := testWsConn(&Server{})
	commandCtx, cancelCommand := context.WithCancel(context.Background())
	trainingCtx, cancelTraining := context.WithCancel(context.Background())
	subCtx, cancelSub := context.WithCancel(context.Background())
	c.commands["request:1"] = cancelCommand
	c.training["train-1"] = cancelTraining
	c.subs["1:"] = &subscription{cancel: cancelSub}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	closed := make(chan struct{})
	go func() {
		c.closeWith(closePolicyViolation, "session ended")
		close(closed)
	}()
	select {
	case <-commandCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("command context was not cancelled while the writer was blocked")
	}
	select {
	case <-trainingCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("training context was not cancelled while the writer was blocked")
	}
	select {
	case <-subCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("subscription was not cancelled while the writer was blocked")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("closeWith blocked on the writer lock")
	}
}

// --- upstream watch failure ------------------------------------------------

type failingWatchSettings struct {
	frameworkv1.UnimplementedSettingsServiceServer
}

func (f *failingWatchSettings) WatchSettings(*frameworkv1.WatchSettingsRequest, grpc.ServerStreamingServer[frameworkv1.SettingsSnapshot]) error {
	return status.Error(codes.Unavailable, "settings watch dropped")
}

type endedWatchRuns struct {
	frameworkv1.UnimplementedApplicationServiceServer
}

func (e *endedWatchRuns) WatchRuns(*frameworkv1.WatchRunsRequest, grpc.ServerStreamingServer[frameworkv1.AppRunEvent]) error {
	return nil
}

// TestUpstreamWatchFailureClosesSocket covers a dead live stream: the
// subscription must not stay registered with no source of updates. The socket
// closes so the browser reconnects and re-subscribes.
func TestUpstreamWatchFailureClosesSocket(t *testing.T) {
	for name, register := range map[string]func(*grpc.Server){
		"settings-error": func(srv *grpc.Server) {
			frameworkv1.RegisterSettingsServiceServer(srv, &failingWatchSettings{})
		},
		"runs-eof": func(srv *grpc.Server) {
			frameworkv1.RegisterApplicationServiceServer(srv, &endedWatchRuns{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			conn := dialBufconn(t, register)
			srv := &Server{Conn: conn}
			c := testWsConn(srv)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "settings-error" {
				srv.watchSettings(ctx, c)
			} else {
				srv.watchRuns(ctx, c, "", webv1.Topic_TOPIC_DIAGNOSTICS, func() {})
			}
			select {
			case <-c.done:
			case <-time.After(3 * time.Second):
				t.Fatal("an ended upstream watch left the subscription alive")
			}
		})
	}
}

// --- revision and typed error contracts -----------------------------------

func TestZeroExpectedRevisionIsRejected(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 80, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Settings{Settings: &webv1.SettingsUpdate{
			Group: &webv1.SettingsUpdate_Volume{Volume: &webv1.VolumeSettings{MasterVolume: 0.2}},
		}},
	}}})
	frame := conn.read()
	if frame.GetRequestId() != 80 || frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("zero settings revision error = %+v", frame)
	}

	conn.send(&webv1.ClientFrame{RequestId: 81, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_AppSettings{AppSettings: &webv1.AppSettingsUpdate{
			AppId:   "org.example.player",
			Changes: []*webv1.AppSettingChangeView{{Key: "effect", ValueJson: `"pulse"`}},
		}},
	}}})
	frame = conn.read()
	if frame.GetRequestId() != 81 || frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("zero app settings revision error = %+v", frame)
	}
	env.back.mu.Lock()
	revision := env.back.settings.GetRevision()
	env.back.mu.Unlock()
	if revision != 5 {
		t.Fatalf("zero-revision updates mutated the store: revision %d", revision)
	}
}

func TestMissingApplicationErrorsAreTyped(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 82, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_APPLICATION},
	}})
	frame := conn.read()
	if frame.GetRequestId() != 82 || frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("missing app id error = %+v", frame)
	}

	conn.send(&webv1.ClientFrame{RequestId: 83, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_APPLICATION, AppId: "org.example.gone"},
	}})
	frame = conn.read()
	if frame.GetRequestId() != 83 || frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("unknown app snapshot error = %+v", frame)
	}

	conn.send(&webv1.ClientFrame{RequestId: 84, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Launch{Launch: &webv1.LaunchApp{AppId: "org.example.gone"}},
	}}})
	frame = conn.read()
	if frame.GetRequestId() != 84 || frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("launch missing app error = %+v", frame)
	}
}

func TestCreateApiKeyValidationErrorsAreTyped(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	const name = "duplicate-key"
	conn.send(&webv1.ClientFrame{RequestId: 85, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_CreateApiKey{CreateApiKey: &webv1.CreateApiKey{Name: name}},
	}}})
	if result := conn.waitResult(t, 85); result.GetApiKey().GetValue() == "" {
		t.Fatal("first key creation returned no secret")
	}
	conn.send(&webv1.ClientFrame{RequestId: 86, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_CreateApiKey{CreateApiKey: &webv1.CreateApiKey{Name: name}},
	}}})
	frame := conn.read()
	if frame.GetRequestId() != 86 || frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_CONFLICT {
		t.Fatalf("duplicate key error = %+v", frame)
	}
	conn.send(&webv1.ClientFrame{RequestId: 87, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_CreateApiKey{CreateApiKey: &webv1.CreateApiKey{Name: "bad name!"}},
	}}})
	frame = conn.read()
	if frame.GetRequestId() != 87 || frame.GetError().GetCode() != webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("invalid key name error = %+v", frame)
	}
}
