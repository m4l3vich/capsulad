package webui

import (
	"testing"

	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
)

// The clock adapter must decode the app's binary protobuf response; JSON is
// only the browser frame encoding, never the AppBus wire.
func TestDecodeClockStateUsesBinaryProtobuf(t *testing.T) {
	raw, err := proto.Marshal(&clockv1.ClockState{
		LocalTimeRfc3339: "2026-11-12T12:00:00Z",
		Stopwatch:        &clockv1.StopwatchState{},
		Timers: []*clockv1.Timer{
			{TimerId: "t1", Status: clockv1.TimerStatus_TIMER_STATUS_COMPLETED},
			{TimerId: "t2"},
		},
		ActiveAlerts: []*clockv1.ActiveAlert{
			{AlertId: "a1", Label: "timer"},
			{AlertId: "a2", Kind: clockv1.AlertKind_ALERT_KIND_ALARM},
		},
		Revision: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeClockState(raw)
	if err != nil {
		t.Fatal(err)
	}
	view := clockView(state)
	// The adapter reports the wire values faithfully. The clock app always
	// sets an explicit state, so an unset enum means UNSPECIFIED and must not
	// be rewritten into an invented IDLE/RUNNING/TIMER state.
	if got := view.GetStopwatch().GetState(); got != "STOPWATCH_STATUS_UNSPECIFIED" {
		t.Fatalf("stopwatch state = %q, want UNSPECIFIED", got)
	}
	if got := view.GetTimers()[0].GetStatus(); got != "TIMER_STATUS_COMPLETED" {
		t.Fatalf("completed timer status = %q, want preserved", got)
	}
	if got := view.GetTimers()[1].GetStatus(); got != "TIMER_STATUS_UNSPECIFIED" {
		t.Fatalf("zero timer status = %q, want UNSPECIFIED", got)
	}
	if got := view.GetActiveAlerts()[0].GetKind(); got != "ALERT_KIND_UNSPECIFIED" {
		t.Fatalf("zero alert kind = %q, want UNSPECIFIED", got)
	}
	if got := view.GetActiveAlerts()[1].GetKind(); got != "ALERT_KIND_ALARM" {
		t.Fatalf("alarm alert kind = %q, want preserved", got)
	}
	if state.GetRevision() != 3 {
		t.Fatalf("decoded state = %+v", state)
	}

	// A protojson body is not the public wire and must not be accepted as an
	// alternative encoding.
	if _, err := decodeClockState([]byte(`{"local_time_rfc3339":"2026-11-12T12:00:00Z"}`)); err == nil {
		t.Fatal("clock adapter accepted a JSON body on the protobuf wire")
	}
}
