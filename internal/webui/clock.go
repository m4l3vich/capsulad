package webui

import (
	"context"
	"fmt"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	webv1 "capsulad/gen/go/capsula/web/v1"
)

// The compiled-in clock app publishes the standard ClockService contract; the
// browser learns its methods through AppInterfaceService like any other app.
// This file is the Web UI's local adapter for the clock page only: it speaks
// the public binary protobuf wire at the app-bus boundary and renders the Web
// UI's own protobuf view. JSON exists only inside browser frames, never on
// AppBus.

const (
	clockAppID      = "capsula.clock"
	clockServiceAPI = "capsula.contracts.clock.v1.ClockService"
)

// findClockService returns the standard service name used by the clock page.
// The browser cannot choose another service, and an absent clock app simply
// fails the call.
func findClockService() (string, error) {
	return clockv1.ClockService_ServiceDesc.ServiceName, nil
}

func (s *Server) clockSnapshot(ctx context.Context, appID, service string) (*clockv1.ClockState, error) {
	raw, err := s.routedProtoCall(ctx, appID, service, "GetState", &clockv1.GetStateRequest{})
	if err != nil {
		return nil, err
	}
	return decodeClockState(raw)
}

func decodeClockState(raw []byte) (*clockv1.ClockState, error) {
	var state clockv1.ClockState
	if err := proto.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode clock state: %w", err)
	}
	return &state, nil
}

func clockView(state *clockv1.ClockState) *webv1.ClockView {
	if state == nil {
		return nil
	}
	v := &webv1.ClockView{LocalTimeRfc3339: state.GetLocalTimeRfc3339(), Timezone: state.GetTimezone()}
	if sw := state.GetStopwatch(); sw != nil {
		v.Stopwatch = &webv1.StopwatchView{
			State: sw.GetStatus().String(), ElapsedMs: int64(sw.GetElapsedMs()), StartedUnixMs: int64(sw.GetStartedUnixMs()),
		}
	}
	for _, t := range state.GetTimers() {
		v.Timers = append(v.Timers, &webv1.TimerView{
			TimerId: t.GetTimerId(), Label: t.GetLabel(), DurationMs: int64(t.GetDurationMs()),
			RemainingMs: int64(t.GetRemainingMs()), Status: t.GetStatus().String(),
			StartedUnixMs: int64(t.GetStartedUnixMs()), FinishedUnixMs: int64(t.GetFinishedUnixMs()),
		})
	}
	for _, a := range state.GetAlarms() {
		label := a.GetLabel()
		if label == "" {
			label = a.GetSchedule()
		}
		if a.GetOneShot() {
			label += " (once)"
		}
		v.Alarms = append(v.Alarms, &webv1.AlarmView{
			AlarmId: a.GetAlarmId(), Label: label, Schedule: a.GetSchedule(),
			Enabled: a.GetEnabled(), Timezone: a.GetTimezone(), EffectiveTimezone: a.GetEffectiveTimezone(),
			NextTriggerUnixMs: int64(a.GetNextTriggerUnixMs()), NextTriggerRfc3339: a.GetNextTriggerRfc3339(),
			LastTriggeredUnixMs: int64(a.GetLastTriggeredUnixMs()),
		})
	}
	for _, a := range state.GetActiveAlerts() {
		v.ActiveAlerts = append(v.ActiveAlerts, &webv1.ActiveAlertView{
			AlertId: a.GetAlertId(), Kind: a.GetKind().String(), SourceId: a.GetSourceId(),
			Label: a.GetLabel(), StartedUnixMs: int64(a.GetStartedUnixMs()),
		})
	}
	return v
}

func (s *Server) clockCall(ctx context.Context, appID, rpc string, request proto.Message) (*clockv1.ClockState, error) {
	raw, err := s.routedProtoCall(ctx, appID, clockServiceAPI, rpc, request)
	if err != nil {
		return nil, err
	}
	if rpc == "GetState" {
		return decodeClockState(raw)
	}
	// Every mutation returns the authoritative state in a follow-up read; the
	// command responses themselves are not rendered.
	return s.clockSnapshot(ctx, appID, clockServiceAPI)
}

// rpcMessage extracts the app-authored message from a gRPC error so the
// browser shows validation text instead of the transport wrapper.
func rpcMessage(err error) string {
	if err == nil {
		return ""
	}
	if msg := status.Convert(err).Message(); msg != "" {
		return msg
	}
	return err.Error()
}
