package webui

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
)

// TestCancelSessionWorkStopsOwnedOperations proves a closed socket cancels
// every command context (including a device take) and every training stream.
func TestCancelSessionWorkStopsOwnedOperations(t *testing.T) {
	c := &wsConn{
		out:      make(chan outFrame, 4),
		done:     make(chan struct{}),
		commands: map[string]context.CancelFunc{},
		training: map[string]context.CancelFunc{},
	}
	commandCtx, cancelCommand := context.WithCancel(context.Background())
	trainingCtx, cancelTraining := context.WithCancel(context.Background())
	c.commands["request:7"] = cancelCommand
	c.training["train-1"] = cancelTraining

	c.cancelOperation("request:7")
	select {
	case <-commandCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("CancelOperation did not cancel the command context")
	}
	select {
	case <-trainingCtx.Done():
		t.Fatal("a request cancel must not end an unrelated training run")
	default:
	}

	c.cancelOperation("train-1")
	select {
	case <-trainingCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("CancelOperation did not cancel the training context")
	}

	// A socket close repeats the cancellation for anything still registered.
	recordCtx, cancelRecord := context.WithCancel(context.Background())
	trainCtx, cancelTrain := context.WithCancel(context.Background())
	c.commands["request:8"] = cancelRecord
	c.training["train-2"] = cancelTrain
	c.cancelSessionWork()
	select {
	case <-recordCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("socket close did not cancel a recording command")
	}
	select {
	case <-trainCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("socket close did not cancel training")
	}
	if len(c.commands) != 0 || len(c.training) != 0 {
		t.Fatalf("cancelled registries retained entries: %d/%d", len(c.commands), len(c.training))
	}
}

type blockingAssistant struct {
	frameworkv1.UnimplementedAssistantServiceServer
	started chan struct{}
	stopped chan struct{}
}

func (b *blockingAssistant) RecordWakewordSample(ctx context.Context, _ *frameworkv1.RecordWakewordSampleRequest) (*frameworkv1.RecordWakewordSampleResponse, error) {
	close(b.started)
	<-ctx.Done()
	close(b.stopped)
	return nil, status.Error(codes.Canceled, "recording cancelled")
}

// TestRecordingStopsWhenCommandContextCancelled proves CancelOperation ends
// the device-side gRPC take, not merely the browser promise.
func TestRecordingStopsWhenCommandContextCancelled(t *testing.T) {
	assistant := &blockingAssistant{started: make(chan struct{}), stopped: make(chan struct{})}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	frameworkv1.RegisterAssistantServiceServer(srv, assistant)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	server := &Server{Conn: conn}
	c := &wsConn{out: make(chan outFrame, 4), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.opRecordSample(c, 9, &webv1.RecordSample{Label: "word", DurationMs: 1600}, ctx)
		close(done)
	}()
	select {
	case <-assistant.started:
	case <-time.After(3 * time.Second):
		t.Fatal("recording RPC never reached the device")
	}
	cancel()
	select {
	case <-assistant.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the command did not cancel the recording RPC")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("recording handler did not return after cancellation")
	}
	select {
	case frame := <-c.out:
		var msg webv1.ServerFrame
		if err := proto.Unmarshal(frame.payload, &msg); err != nil {
			t.Fatal(err)
		}
		if code := msg.GetError().GetCode(); code != webv1.ErrorCode_ERROR_CODE_CANCELLED {
			t.Fatalf("recording cancellation error = %v, want CANCELLED", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no cancellation error was sent")
	}
}

// TestCancelOperationAbortsRecordingAndKeepsSession exercises the real socket
// path: the browser abort reaches the owner command, the device take ends, and
// the session remains usable for later requests.
func TestCancelOperationAbortsRecordingAndKeepsSession(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 51, Body: &webv1.ClientFrame_Command{
		Command: &webv1.Command{Op: &webv1.Command_RecordSample{
			RecordSample: &webv1.RecordSample{Label: "word", DurationMs: 4242},
		}},
	}})
	select {
	case <-env.back.recordingStart:
	case <-time.After(3 * time.Second):
		t.Fatal("recording never started on the device")
	}
	conn.send(&webv1.ClientFrame{RequestId: 52, Body: &webv1.ClientFrame_Cancel{
		Cancel: &webv1.CancelOperation{OperationId: requestOperationID(51)},
	}})
	select {
	case <-env.back.recordingCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("CancelOperation did not terminate the device recording")
	}
	// The abort is consumer-visible as a CANCELLED error for the aborted request.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("aborted recording produced no CANCELLED error")
		}
		frame := conn.read()
		if frame.GetRequestId() != 51 {
			continue
		}
		if code := frame.GetError().GetCode(); code != webv1.ErrorCode_ERROR_CODE_CANCELLED {
			t.Fatalf("aborted recording error = %v, want CANCELLED", code)
		}
		break
	}
	// The socket is not torn down by the abort; a later request still works.
	conn.send(&webv1.ClientFrame{RequestId: 53, Body: &webv1.ClientFrame_Ping{Ping: &webv1.Ping{}}})
	for {
		frame := conn.read()
		if frame.GetPong() != nil {
			break
		}
	}
}

// TestSocketCloseAbortsRecording proves a disconnected browser leaves no
// device-side capture running.
func TestSocketCloseAbortsRecording(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 61, Body: &webv1.ClientFrame_Command{
		Command: &webv1.Command{Op: &webv1.Command_RecordSample{
			RecordSample: &webv1.RecordSample{Label: "word", DurationMs: 4242},
		}},
	}})
	select {
	case <-env.back.recordingStart:
	case <-time.After(3 * time.Second):
		t.Fatal("recording never started on the device")
	}
	conn.close()
	select {
	case <-env.back.recordingCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("socket close did not terminate the device recording")
	}
}
