package webui

import (
	"testing"

	"google.golang.org/protobuf/proto"

	webv1 "capsulad/gen/go/capsulad/web/v1"
)

// TestOperationAdmissionErrorsCorrelateRequestID covers a browser that occupies
// every operation slot: snapshot, subscribe and command rejections must carry
// the caller's request id, otherwise the client cannot settle its promise and
// waits for the transport timeout.
func TestOperationAdmissionErrorsCorrelateRequestID(t *testing.T) {
	c := &wsConn{
		out:  make(chan outFrame, 8),
		done: make(chan struct{}),
		ops:  make(chan struct{}, 1),
	}
	c.ops <- struct{}{} // saturate the single slot

	readError := func(wantID uint64) {
		t.Helper()
		select {
		case frame := <-c.out:
			var msg webv1.ServerFrame
			if err := proto.Unmarshal(frame.payload, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.GetRequestId() != wantID {
				t.Fatalf("error request id = %d, want %d", msg.GetRequestId(), wantID)
			}
			if code := msg.GetError().GetCode(); code != webv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
				t.Fatalf("error code = %v, want UNAVAILABLE", code)
			}
		default:
			t.Fatal("no admission error frame was sent")
		}
	}

	c.handleFrame(&webv1.ClientFrame{RequestId: 41, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_DIAGNOSTICS},
	}})
	readError(41)

	c.handleFrame(&webv1.ClientFrame{RequestId: 42, Body: &webv1.ClientFrame_Subscribe{
		Subscribe: &webv1.Subscribe{Topic: webv1.Topic_TOPIC_DIAGNOSTICS},
	}})
	readError(42)

	c.handleFrame(&webv1.ClientFrame{RequestId: 43, Body: &webv1.ClientFrame_Command{
		Command: &webv1.Command{Op: &webv1.Command_RecordSample{
			RecordSample: &webv1.RecordSample{Label: "word", DurationMs: 1600},
		}},
	}})
	readError(43)
}
