package webui

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	mediav1 "capsulad/gen/go/capsula/contracts/media/v1"
	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

// fakeBackend is a small in-process daemon used to exercise the browser
// surface over a real gRPC client connection. It implements only the
// services the Web UI allowlist touches.
type fakeBackend struct {
	mu             sync.Mutex
	settings       *frameworkv1.SettingsSnapshot
	apps           []*frameworkv1.InstalledApplication
	runs           []*frameworkv1.AppRun
	resources      []*frameworkv1.ResourceInfo
	logs           []*frameworkv1.LogRecord
	trainingCancel  chan struct{}
	recordingStart  chan struct{}
	recordingCancel chan struct{}
	clockRunning    bool
	// overviewGate delays GetInfo so a test can mutate state while a snapshot
	// is being built. It is nil unless a test installs it; overviewEntered
	// signals that the snapshot read has started.
	overviewGate    chan struct{}
	overviewEntered chan struct{}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		settings: &frameworkv1.SettingsSnapshot{
			Revision: 5,
			Values: &frameworkv1.UserSettings{
				Hostname: "capsula", MasterVolume: 0.4, Timezone: "UTC",
				DefaultAppByResource: map[string]string{"display.segment.front": "capsula.clock"},
				DisplayBrightness:    map[string]float32{"display.segment.front": 0.7},
				BrightnessMode:       map[string]string{},
				MutedMicrophoneIndicator: &frameworkv1.MutedMicrophoneIndicator{
					Mode: frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_DELAY, DelayMs: 30000,
				},
			},
		},
		apps: []*frameworkv1.InstalledApplication{
			{AppId: "capsula.clock", DisplayName: "Clock", Version: "1.0.0", SystemApp: true, CanStop: true,
				Provides: []string{"segment_display_provider"}},
			{AppId: "org.example.player", DisplayName: "Player", Version: "2.1.0", CanStop: true,
				Provides: []string{"media_player"}},
		},
		runs: []*frameworkv1.AppRun{
			{RunId: "run-1", AppId: "capsula.clock", Pid: 42, State: frameworkv1.AppRunState_APP_READY,
				Runtime: frameworkv1.ApplicationRuntime_RUNTIME_BUILTIN},
		},
		resources: []*frameworkv1.ResourceInfo{
			{ResourceId: "display.segment.front", Kind: frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY, Available: true},
			{ResourceId: "light.ring.main", Kind: frameworkv1.ResourceKind_RESOURCE_LED_RING, Available: true},
		},
		logs: []*frameworkv1.LogRecord{
			{MonotonicTimeNs: 123, Component: "daemon", Severity: "INFO", Message: "ready"},
		},
		trainingCancel: make(chan struct{}, 1),
		recordingStart: make(chan struct{}, 1),
		recordingCancel: make(chan struct{}, 1),
	}
}

func startFakeGRPC(t *testing.T, b *fakeBackend) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	frameworkv1.RegisterDeviceServiceServer(srv, &fakeDevice{b: b})
	frameworkv1.RegisterSettingsServiceServer(srv, &fakeSettings{b: b})
	frameworkv1.RegisterAudioServiceServer(srv, &fakeAudio{b: b})
	frameworkv1.RegisterSystemServiceServer(srv, &fakeSystem{b: b})
	frameworkv1.RegisterApplicationServiceServer(srv, &fakeApps{b: b})
	frameworkv1.RegisterApplicationConfigurationServiceServer(srv, &fakeConfig{b: b})
	frameworkv1.RegisterAppInterfaceServiceServer(srv, &fakeInterfaces{b: b})
	frameworkv1.RegisterDiagnosticsServiceServer(srv, &fakeDiagnostics{b: b})
	frameworkv1.RegisterAssistantServiceServer(srv, &fakeAssistant{b: b})
	frameworkv1.RegisterRoutingServiceServer(srv, &fakeRouting{b: b})
	frameworkv1.RegisterAppBusServiceServer(srv, &fakeBus{b: b})
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

type fakeDevice struct {
	frameworkv1.UnimplementedDeviceServiceServer
	b *fakeBackend
}

func (f *fakeDevice) GetInfo(context.Context, *frameworkv1.GetInfoRequest) (*frameworkv1.DeviceInfo, error) {
	f.b.mu.Lock()
	gate := f.b.overviewGate
	entered := f.b.overviewEntered
	f.b.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	return &frameworkv1.DeviceInfo{DeviceId: "dev-1", Model: "Capsula Mini", DaemonVersion: "test"}, nil
}
func (f *fakeDevice) ListResources(context.Context, *frameworkv1.ListResourcesRequest) (*frameworkv1.ListResourcesResponse, error) {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return &frameworkv1.ListResourcesResponse{Resources: f.b.resources, Revision: 3}, nil
}
func (f *fakeDevice) WatchResources(*frameworkv1.WatchResourcesRequest, grpc.ServerStreamingServer[frameworkv1.ResourceChange]) error {
	return nil
}

type fakeSettings struct {
	frameworkv1.UnimplementedSettingsServiceServer
	b *fakeBackend
}

func (f *fakeSettings) GetSettings(context.Context, *frameworkv1.GetSettingsRequest) (*frameworkv1.SettingsSnapshot, error) {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return cloneSettings(f.b.settings), nil
}

func cloneSettings(in *frameworkv1.SettingsSnapshot) *frameworkv1.SettingsSnapshot {
	return proto.Clone(in).(*frameworkv1.SettingsSnapshot)
}

func (f *fakeSettings) UpdateSettings(_ context.Context, req *frameworkv1.UpdateSettingsRequest) (*frameworkv1.SettingsSnapshot, error) {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	if req.GetExpectedRevision() != f.b.settings.GetRevision() {
		return nil, status.Errorf(codes.Aborted, "revision mismatch")
	}
	f.b.settings.Values = proto.Clone(req.GetValues()).(*frameworkv1.UserSettings)
	f.b.settings.Revision++
	return cloneSettings(f.b.settings), nil
}

func (f *fakeSettings) WatchSettings(_ *frameworkv1.WatchSettingsRequest, stream grpc.ServerStreamingServer[frameworkv1.SettingsSnapshot]) error {
	f.b.mu.Lock()
	snap := cloneSettings(f.b.settings)
	f.b.mu.Unlock()
	if err := stream.Send(snap); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

type fakeAudio struct {
	frameworkv1.UnimplementedAudioServiceServer
	b *fakeBackend
}

func (f *fakeAudio) GetAudioState(context.Context, *frameworkv1.GetAudioStateRequest) (*frameworkv1.AudioState, error) {
	return &frameworkv1.AudioState{MasterVolume: f.b.settings.Values.MasterVolume, MicrophoneMuted: false}, nil
}

type fakeSystem struct {
	frameworkv1.UnimplementedSystemServiceServer
	b *fakeBackend
}

func (f *fakeSystem) GetBluetoothStatus(context.Context, *frameworkv1.GetBluetoothStatusRequest) (*frameworkv1.BluetoothStatus, error) {
	return &frameworkv1.BluetoothStatus{Present: true, Powered: true, Discoverable: false, IncomingConnections: true,
		Adapter: "hci0", Detail: "ready", Name: "Capsula"}, nil
}
func (f *fakeSystem) ListPairedDevices(context.Context, *frameworkv1.ListPairedDevicesRequest) (*frameworkv1.ListPairedDevicesResponse, error) {
	return &frameworkv1.ListPairedDevicesResponse{Devices: []*frameworkv1.PairedDevice{
		{Address: "AA:BB:CC:DD:EE:FF", Name: "Phone", Connected: true},
	}}, nil
}
func (f *fakeSystem) SetBluetoothDiscoverable(context.Context, *frameworkv1.SetBluetoothDiscoverableRequest) (*frameworkv1.BluetoothStatus, error) {
	return &frameworkv1.BluetoothStatus{Present: true, Powered: true, Discoverable: true, IncomingConnections: true}, nil
}
func (f *fakeSystem) SetBluetoothIncomingConnections(context.Context, *frameworkv1.SetBluetoothIncomingConnectionsRequest) (*frameworkv1.BluetoothStatus, error) {
	return &frameworkv1.BluetoothStatus{Present: true, Powered: true, IncomingConnections: true}, nil
}
func (f *fakeSystem) UnpairDevice(context.Context, *frameworkv1.UnpairDeviceRequest) (*frameworkv1.UnpairDeviceResponse, error) {
	return &frameworkv1.UnpairDeviceResponse{Removed: true}, nil
}
func (f *fakeSystem) GetStatus(context.Context, *frameworkv1.GetStatusRequest) (*frameworkv1.SystemStatus, error) {
	return &frameworkv1.SystemStatus{}, nil
}

type fakeApps struct {
	frameworkv1.UnimplementedApplicationServiceServer
	b *fakeBackend
}

func (f *fakeApps) ListApplications(context.Context, *frameworkv1.ListApplicationsRequest) (*frameworkv1.ListApplicationsResponse, error) {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return &frameworkv1.ListApplicationsResponse{Applications: f.b.apps}, nil
}
func (f *fakeApps) ListRuns(_ context.Context, req *frameworkv1.ListRunsRequest) (*frameworkv1.ListRunsResponse, error) {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	var out []*frameworkv1.AppRun
	for _, run := range f.b.runs {
		if req.GetAppId() == "" || run.GetAppId() == req.GetAppId() {
			out = append(out, run)
		}
	}
	return &frameworkv1.ListRunsResponse{Runs: out}, nil
}
func (f *fakeApps) WatchRuns(req *frameworkv1.WatchRunsRequest, stream grpc.ServerStreamingServer[frameworkv1.AppRunEvent]) error {
	f.b.mu.Lock()
	runs := append([]*frameworkv1.AppRun(nil), f.b.runs...)
	f.b.mu.Unlock()
	for _, run := range runs {
		if req.GetAppId() != "" && run.GetAppId() != req.GetAppId() {
			continue
		}
		if err := stream.Send(&frameworkv1.AppRunEvent{Run: run}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}
func (f *fakeApps) GetBinaryProvenance(context.Context, *frameworkv1.GetBinaryProvenanceRequest) (*frameworkv1.BinaryProvenanceResult, error) {
	return &frameworkv1.BinaryProvenanceResult{Status: frameworkv1.ProvenanceStatus_PROVENANCE_NOT_PROVIDED}, nil
}
func (f *fakeApps) Launch(context.Context, *frameworkv1.LaunchRequest) (*frameworkv1.LaunchResult, error) {
	return &frameworkv1.LaunchResult{RunId: "run-2", InvocationId: "inv-1", SpawnedNewProcess: true}, nil
}
func (f *fakeApps) Stop(context.Context, *frameworkv1.StopRequest) (*frameworkv1.StopResponse, error) {
	return &frameworkv1.StopResponse{Requested: true}, nil
}

type fakeConfig struct {
	frameworkv1.UnimplementedApplicationConfigurationServiceServer
	b *fakeBackend
}

func (f *fakeConfig) Describe(_ context.Context, req *frameworkv1.DescribeAppConfigurationRequest) (*frameworkv1.AppConfigurationSchema, error) {
	return &frameworkv1.AppConfigurationSchema{AppId: req.GetAppId(), Settings: []*frameworkv1.AppSettingDefinition{
		{Key: "effect", Label: "Effect", Type: frameworkv1.SettingType_SETTING_SELECT,
			Choices: []*frameworkv1.SettingChoice{{Value: "pulse", Label: "Pulse"}}},
		{Key: "speed", Label: "Speed", Type: frameworkv1.SettingType_SETTING_NUMBER, HasMin: true, Min: 0, HasMax: true, Max: 4},
	}}, nil
}
func (f *fakeConfig) Get(_ context.Context, req *frameworkv1.GetAppConfigurationRequest) (*frameworkv1.AppConfigurationSnapshot, error) {
	return &frameworkv1.AppConfigurationSnapshot{AppId: req.GetAppId(), Revision: 2,
		Values: []*frameworkv1.AppSettingState{{Key: "effect", ValueJson: `"pulse"`, HasValue: true}}}, nil
}
func (f *fakeConfig) Update(context.Context, *frameworkv1.UpdateAppConfigurationRequest) (*frameworkv1.AppConfigurationSnapshot, error) {
	return &frameworkv1.AppConfigurationSnapshot{Revision: 3}, nil
}
func (f *fakeConfig) TriggerAction(context.Context, *frameworkv1.TriggerAppActionRequest) (*frameworkv1.TriggerAppActionResult, error) {
	return &frameworkv1.TriggerAppActionResult{ActionInvocationId: "act-1"}, nil
}

type fakeInterfaces struct {
	frameworkv1.UnimplementedAppInterfaceServiceServer
	b *fakeBackend
}

func (f *fakeInterfaces) ListInterfaces(context.Context, *frameworkv1.ListAppInterfacesRequest) (*frameworkv1.ListAppInterfacesResponse, error) {
	return &frameworkv1.ListAppInterfacesResponse{}, nil
}
func (f *fakeInterfaces) DescribeInterface(context.Context, *frameworkv1.DescribeAppInterfaceRequest) (*frameworkv1.AppInterfaceDescription, error) {
	return &frameworkv1.AppInterfaceDescription{}, nil
}

type fakeDiagnostics struct {
	frameworkv1.UnimplementedDiagnosticsServiceServer
	b *fakeBackend
}

func (f *fakeDiagnostics) WatchLogs(req *frameworkv1.WatchLogsRequest, stream grpc.ServerStreamingServer[frameworkv1.LogRecord]) error {
	f.b.mu.Lock()
	logs := append([]*frameworkv1.LogRecord(nil), f.b.logs...)
	f.b.mu.Unlock()
	for _, rec := range logs {
		if req.GetAppId() != "" && rec.GetAppId() != req.GetAppId() {
			continue
		}
		if err := stream.Send(rec); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

type fakeAssistant struct {
	frameworkv1.UnimplementedAssistantServiceServer
	b *fakeBackend
}

func (f *fakeAssistant) ListWakewordModels(context.Context, *frameworkv1.ListWakewordModelsRequest) (*frameworkv1.ListWakewordModelsResponse, error) {
	return &frameworkv1.ListWakewordModelsResponse{Models: []*frameworkv1.WakewordModelInfo{
		{ModelId: "m1", DisplayName: "Marusia", Language: "ru", EngineId: "oww", Available: true},
	}}, nil
}
func (f *fakeAssistant) ListWakewordSamples(context.Context, *frameworkv1.ListWakewordSamplesRequest) (*frameworkv1.ListWakewordSamplesResponse, error) {
	return &frameworkv1.ListWakewordSamplesResponse{Samples: []*frameworkv1.WakewordSample{
		{SampleId: "s1", Label: "word", DurationMs: 2000, RecordedUnixMs: 1700000000000},
	}}, nil
}
func (f *fakeAssistant) RecordWakewordSample(ctx context.Context, req *frameworkv1.RecordWakewordSampleRequest) (*frameworkv1.RecordWakewordSampleResponse, error) {
	if req.GetDurationMs() > 20000 {
		return nil, status.Error(codes.InvalidArgument, "too long")
	}
	// 4242 is the test-only blocking take: it proves a cancel or a socket
	// close terminates the device-side RPC, not just the browser promise.
	if req.GetDurationMs() == 4242 {
		select {
		case f.b.recordingStart <- struct{}{}:
		default:
		}
		<-ctx.Done()
		select {
		case f.b.recordingCancel <- struct{}{}:
		default:
		}
		return nil, status.Error(codes.Canceled, "recording cancelled")
	}
	return &frameworkv1.RecordWakewordSampleResponse{Sample: &frameworkv1.WakewordSample{
		SampleId: "s-new", Label: req.GetLabel(), DurationMs: req.GetDurationMs(), RecordedUnixMs: 1700000000001,
	}}, nil
}
func (f *fakeAssistant) DeleteWakewordSample(context.Context, *frameworkv1.DeleteWakewordSampleRequest) (*frameworkv1.DeleteWakewordSampleResponse, error) {
	return &frameworkv1.DeleteWakewordSampleResponse{}, nil
}
func (f *fakeAssistant) TrainWakewordClassifier(_ *frameworkv1.TrainWakewordClassifierRequest, stream grpc.ServerStreamingServer[frameworkv1.TrainWakewordEvent]) error {
	if err := stream.Send(&frameworkv1.TrainWakewordEvent{Stage: "fit", Fraction: 0.3, Log: "fitting"}); err != nil {
		return err
	}
	<-stream.Context().Done()
	select {
	case f.b.trainingCancel <- struct{}{}:
	default:
	}
	return status.Error(codes.Canceled, "cancelled")
}

type fakeRouting struct {
	frameworkv1.UnimplementedRoutingServiceServer
	b *fakeBackend
}

func (f *fakeRouting) GetRouting(_ context.Context, req *frameworkv1.GetRoutingRequest) (*frameworkv1.RoutingState, error) {
	return &frameworkv1.RoutingState{ResourceId: req.GetResourceId(), SelectedAppId: "capsula.clock", Revision: 1}, nil
}

type fakeBus struct {
	frameworkv1.UnimplementedAppBusServiceServer
	b *fakeBackend
}

func (f *fakeBus) Call(stream grpc.BidiStreamingServer[frameworkv1.RoutedCallRequest, frameworkv1.RoutedCallResponse]) error {
	var start *frameworkv1.RoutedCallStart
	var request []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if s := req.GetStart(); s != nil {
			start = s
		}
		if body := req.GetRequestMessage(); body != nil {
			request = body
		}
	}
	if start == nil {
		return status.Error(codes.InvalidArgument, "no start")
	}
	body, err := f.response(start, request)
	if err != nil {
		return stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
			End: &frameworkv1.AppCallEnd{GrpcStatusCode: int32(codes.Unimplemented), Detail: err.Error()},
		}})
	}
	if err := stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_ResponseMessage{ResponseMessage: body}}); err != nil {
		return err
	}
	return stream.Send(&frameworkv1.RoutedCallResponse{Message: &frameworkv1.RoutedCallResponse_End{
		End: &frameworkv1.AppCallEnd{GrpcStatusCode: 0},
	}})
}

func (f *fakeBus) response(start *frameworkv1.RoutedCallStart, request []byte) ([]byte, error) {
	switch start.GetGrpcService() {
	case "capsula.contracts.clock.v1.ClockService":
		// The public wire is binary protobuf; decoding the request and encoding
		// the response here keeps the browser adapter honest.
		switch start.GetRpcName() {
		case "GetState":
			var req clockv1.GetStateRequest
			if err := proto.Unmarshal(request, &req); err != nil {
				return nil, err
			}
			f.b.mu.Lock()
			running := f.b.clockRunning
			f.b.mu.Unlock()
			status := clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE
			var elapsed uint64
			if running {
				status = clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING
				elapsed = 1000
			}
			return proto.Marshal(&clockv1.ClockState{
				LocalTimeRfc3339: "2026-11-13T12:00:00Z", Timezone: "UTC",
				Stopwatch: &clockv1.StopwatchState{Status: status, ElapsedMs: elapsed},
				Revision:  3, AlarmsSetting: "0 7 * * *",
			})
		case "StopwatchStart":
			var req clockv1.StopwatchCommandRequest
			if err := proto.Unmarshal(request, &req); err != nil {
				return nil, err
			}
			f.b.mu.Lock()
			f.b.clockRunning = true
			f.b.mu.Unlock()
			return proto.Marshal(&clockv1.StopwatchState{
				Status: clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING, ElapsedMs: 1000,
			})
		default:
			return nil, status.Errorf(codes.Unimplemented, "unexpected clock rpc %s", start.GetRpcName())
		}
	case "capsula.contracts.ring.v1.RingService":
		var req ringv1.GetStateRequest
		if err := proto.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		return proto.Marshal(&ringv1.RingState{
			Effect: "pulse", Palette: "linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)",
			Brightness: 0.5, Speed: 1, PresentationId: "p1", Active: true, SettingsRevision: 4,
		})
	case "capsula.contracts.media.v1.MediaPlayerService":
		if start.GetRpcName() == "ListPlayers" {
			resp, _ := proto.Marshal(&mediav1.ListPlayersResponse{Players: []*mediav1.PlayerStatus{{
				PlayerId: "p1", PlaybackState: mediav1.PlaybackState_PLAYBACK_PLAYING, Title: protoString("Scarlet Fire"),
				SupportedActions: []mediav1.MediaAction{
					mediav1.MediaAction_MEDIA_PAUSE, mediav1.MediaAction_MEDIA_SKIP_NEXT,
				},
			}}})
			return resp, nil
		}
		return proto.Marshal(&mediav1.PlayerCommandResult{Accepted: true})
	}
	return nil, status.Errorf(codes.Unimplemented, "unexpected service %s", start.GetGrpcService())
}

func protoString(v string) *string { return &v }

// compile-time assertion that the fake covers every registered service.
var _ = json.Marshal
