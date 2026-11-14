package daemon

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/supervisor"
)

// appMetadata is how the SDK identifies a supervised run. These gates must
// reject it before any device-wide store is touched.
func appMetadata(runID string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("capsula-run-id", runID))
}

func testLogBus(t *testing.T) (*logbus.Bus, *ids.Clock) {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	return logs, clock
}

func TestAdministrativeRPCsRejectApplicationIdentity(t *testing.T) {
	ctx := appMetadata("run_test")
	d := &Daemon{cfg: Config{SandboxUIDBase: 20000}}

	if err := d.requireAdministrativeCaller(ctx, "test operation"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("administrative gate code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := (&settingsService{d: d}).GetSettings(ctx, &frameworkv1.GetSettingsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("GetSettings code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := (&routingService{d: d}).ForceRelease(ctx, &frameworkv1.ForceReleaseRequest{CaptureId: "cap_x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ForceRelease code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := (&audioService{d: d}).SetMicrophoneMuted(ctx, &frameworkv1.SetMicrophoneMutedRequest{Muted: false}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("SetMicrophoneMuted code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := (&assistantService{d: d}).ListWakewordSamples(ctx, &frameworkv1.ListWakewordSamplesRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ListWakewordSamples code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := (&applicationService{d: d}).Launch(ctx, &frameworkv1.LaunchRequest{AppId: "other.app"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Launch code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestInternalBusAuthorizerAllowsOnlyDeclaredInterfaces(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	d.internalToken = "per-boot-secret"
	target := clockAppID
	service := clockServiceName

	// A declared public interface is reachable without any daemon credential.
	if err := d.authorizeInternalBusCall(context.Background(), target, service); err != nil {
		t.Fatalf("declared public interface rejected: %v", err)
	}
	// The retired private name is not declared and stays denied.
	if err := d.authorizeInternalBusCall(context.Background(), target, "capsula.internal.clock.v1.ClockService"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("undeclared service code = %v, want PermissionDenied", status.Code(err))
	}
	// A wrong per-boot token is not identity; the declared-interface rule is
	// what grants access, so an unknown target is still denied.
	wrong := metadata.NewIncomingContext(context.Background(), metadata.Pairs(internalTokenKey, "stale-token"))
	if err := d.authorizeInternalBusCall(wrong, "no.such.app", service); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown target code = %v, want PermissionDenied", status.Code(err))
	}
	right := metadata.NewIncomingContext(context.Background(), metadata.Pairs(internalTokenKey, "per-boot-secret"))
	if err := d.authorizeInternalBusCall(right, "no.such.app", "anything"); err != nil {
		t.Fatalf("daemon-internal token rejected: %v", err)
	}
}

// A supervised app may read and write only its own per-app configuration; the
// daemon's local administrator may address any app.
func TestAppScopeRejectsForeignApplication(t *testing.T) {
	logs, clock := testLogBus(t)
	defer logs.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := supervisor.New(ctx, logs, clock, "", nil)
	if _, err := sup.RegisterBuiltin(clockManifest(), clockAppID); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{SandboxUIDBase: 20000}, sup: sup, logs: logs, internalToken: "scope-token"}

	own := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("capsula-run-id", clockAppID, internalTokenKey, "scope-token"))
	if err := d.requireAppScope(own, clockAppID); err != nil {
		t.Fatalf("own app scope rejected: %v", err)
	}
	if err := d.requireAppScope(own, "other.app"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign app scope code = %v, want PermissionDenied", status.Code(err))
	}
	if err := d.requireAppScope(context.Background(), "other.app"); err != nil {
		t.Fatalf("local administrator scope rejected: %v", err)
	}
}

// The private clock service is reachable through the daemon's own connection
// and denied for a plain socket client (all supervised apps use such a client).
func TestPublicAppBusTargetReachableByOrdinaryClient(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	conn := dialTest(t, d.cfg.SocketPath)
	waitReady(t, d, clockAppID, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Discover the contract exactly like an ordinary app.
	ifaces, err := frameworkv1.NewAppInterfaceServiceClient(conn).ListInterfaces(ctx,
		&frameworkv1.ListAppInterfacesRequest{AppId: clockAppID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iface := range ifaces.GetInterfaces() {
		if iface.GetGrpcService() == clockServiceName && iface.GetAvailable() {
			found = true
		}
	}
	if !found {
		t.Fatalf("clock contract not advertised: %v", ifaces.GetInterfaces())
	}
	if desc, err := frameworkv1.NewAppInterfaceServiceClient(conn).DescribeInterface(ctx,
		&frameworkv1.DescribeAppInterfaceRequest{AppId: clockAppID, GrpcService: clockServiceName}); err != nil {
		t.Fatalf("describe clock contract: %v", err)
	} else if len(desc.GetFileDescriptorSet()) == 0 || len(desc.GetMethods()) == 0 {
		t.Fatalf("clock contract descriptor/methods missing: %d bytes, %d methods",
			len(desc.GetFileDescriptorSet()), len(desc.GetMethods()))
	}

	call := func(service, rpc string) (int32, []byte) {
		t.Helper()
		stream, err := frameworkv1.NewAppBusServiceClient(conn).Call(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
			Start: &frameworkv1.RoutedCallStart{TargetAppId: clockAppID, GrpcService: service, RpcName: rpc, TimeoutMs: 2000},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_HalfClose{
			HalfClose: &frameworkv1.CallHalfClose{},
		}}); err != nil {
			t.Fatal(err)
		}
		var payload []byte
		for {
			resp, err := stream.Recv()
			if err != nil {
				t.Fatalf("%s/%s: %v", service, rpc, err)
			}
			if end := resp.GetEnd(); end != nil {
				return end.GetGrpcStatusCode(), payload
			}
			if body := resp.GetResponseMessage(); body != nil {
				payload = body
			}
		}
	}

	code, payload := call(clockServiceName, "GetState")
	if code != int32(codes.OK) || len(payload) == 0 {
		t.Fatalf("ordinary client public call = code %d, %d bytes", code, len(payload))
	}
	// The retired private name is undeclared, so the bus stream itself is
	// rejected before any request frame reaches the app.
	privateStream, err := frameworkv1.NewAppBusServiceClient(conn).Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := privateStream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{
			TargetAppId: clockAppID, GrpcService: "capsula.internal.clock.v1.ClockService",
			RpcName: "GetState", TimeoutMs: 2000,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := privateStream.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("undeclared service code = %v, want PermissionDenied", status.Code(err))
	}
}
