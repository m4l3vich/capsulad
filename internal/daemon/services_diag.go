package daemon

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/logbus"
)

type diagnosticsService struct {
	frameworkv1.UnimplementedDiagnosticsServiceServer
	d *Daemon
}

// GetSnapshot exposes every run, stream and capture ID on the device. It is a
// local administrator surface: a supervised app learns only its own state
// through the normal services, never other applications' identities.
func (s *diagnosticsService) GetSnapshot(ctx context.Context, _ *frameworkv1.GetDiagnosticsRequest) (*frameworkv1.DiagnosticsSnapshot, error) {
	if appID, isApp := s.d.applicationCaller(ctx); isApp {
		s.d.logs.Warnf("diagnostics", appID, "denied device-wide snapshot")
		return nil, status.Error(codes.PermissionDenied, "diagnostics are a local administrator operation")
	}
	out := &frameworkv1.DiagnosticsSnapshot{MonotonicTimeNs: s.d.clock.NowNS()}
	for _, res := range s.d.resourceList() {
		switch res.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY,
			frameworkv1.ResourceKind_RESOURCE_LED_RING,
			frameworkv1.ResourceKind_RESOURCE_TOUCH,
			frameworkv1.ResourceKind_RESOURCE_VOICE_ASSISTANT_ROLE:
			sel, caps := s.d.router.State(res.ResourceId)
			out.RoutedResources = append(out.RoutedResources, routingStateProto(sel, caps))
		}
	}
	for _, run := range s.d.sup.List("") {
		out.Runs = append(out.Runs, runProto(run))
	}
	for _, stream := range s.d.audio.ActiveStreams() {
		info := &frameworkv1.ActiveAudioStreamInfo{
			StreamId: stream.StreamID, AppId: stream.AppID, EndpointId: stream.EndpointID,
			Input: stream.Input, OpenedMonotonicNs: stream.OpenedNS,
		}
		if stream.Input {
			info.IndicationActive = s.d.indicationActive(stream.AppID)
		}
		out.AudioStreams = append(out.AudioStreams, info)
	}
	return out, nil
}

func (s *diagnosticsService) WatchLogs(req *frameworkv1.WatchLogsRequest, stream frameworkv1.DiagnosticsService_WatchLogsServer) error {
	// A supervised app may tail its own records only. Framework records and
	// other applications' records stay on the administrator surface; otherwise
	// an app could ask for AppId="" or a foreign app and read host paths,
	// backend errors and another app's operational detail.
	appID, isApp := s.d.applicationCaller(stream.Context())
	filter := req.AppId
	if isApp {
		if appID == "" {
			return status.Error(codes.PermissionDenied, "application run is not live")
		}
		if req.AppId != "" && req.AppId != appID {
			s.d.logs.Warnf("diagnostics", appID, "denied log stream for %q", req.AppId)
			return status.Error(codes.PermissionDenied, "applications may only read their own logs")
		}
		filter = appID
	}
	visible := func(e logbus.Entry) bool { return !isApp || e.AppID == appID }
	for _, e := range s.d.logs.Snapshot(filter) {
		if !visible(e) {
			continue
		}
		if err := stream.Send(logProto(e)); err != nil {
			return err
		}
	}
	sub := s.d.logs.Subscribe(filter, 128)
	defer sub.Close()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case e, ok := <-sub.C:
			if !ok {
				return nil
			}
			if !visible(e) {
				continue
			}
			if err := stream.Send(logProto(e)); err != nil {
				return err
			}
		}
	}
}

func logProto(e logbus.Entry) *frameworkv1.LogRecord {
	return &frameworkv1.LogRecord{
		MonotonicTimeNs: e.TimeNS, Component: e.Component, AppId: e.AppID,
		Severity: e.Severity, Message: e.Message,
	}
}

func (d *Daemon) registerDiagnostics(srv *grpc.Server) {
	frameworkv1.RegisterDiagnosticsServiceServer(srv, &diagnosticsService{d: d})
}
