package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/artifact"
	"capsulad/internal/bus"
	"capsulad/internal/manifest"
	"capsulad/internal/provenance"
	"capsulad/internal/registry"
	"capsulad/internal/settings"
	"capsulad/internal/uichanges"
	"capsulad/internal/version"
)

type applicationService struct {
	frameworkv1.UnimplementedApplicationServiceServer
	d *Daemon
}

func (s *applicationService) ListApplications(ctx context.Context, _ *frameworkv1.ListApplicationsRequest) (*frameworkv1.ListApplicationsResponse, error) {
	out := &frameworkv1.ListApplicationsResponse{}
	for _, app := range s.d.reg.List() {
		out.Applications = append(out.Applications, s.d.appProto(app))
	}
	return out, nil
}

func (s *applicationService) ListRuntimes(ctx context.Context, _ *frameworkv1.ListRuntimesRequest) (*frameworkv1.ListRuntimesResponse, error) {
	out := &frameworkv1.ListRuntimesResponse{}
	for _, status := range s.d.launcher.RuntimeStatuses() {
		entry := &frameworkv1.RuntimeAvailability{
			Runtime:               runtimeProto(status.Runtime),
			Available:             status.Available,
			ImplementationVersion: status.ImplementationVersion,
			Detail:                status.Detail,
		}
		switch status.Runtime {
		case manifest.RuntimeNative:
			entry.ImplementationVersion = "capsulad/" + version.Version
			entry.Detail = "Go/Rust SDK connects to the Unix-socket app bus; sandbox mode: " + s.d.launcher.Mode()
		case manifest.RuntimeBuiltin:
			entry.ImplementationVersion = "capsulad/" + version.Version
			entry.Detail = "compiled-in system applications: normal app identity, settings, run lifecycle and app-bus endpoints; no package, process or install/uninstall"
		}
		out.Runtimes = append(out.Runtimes, entry)
	}
	return out, nil
}

func (s *applicationService) GetBinaryProvenance(ctx context.Context, req *frameworkv1.GetBinaryProvenanceRequest) (*frameworkv1.BinaryProvenanceResult, error) {
	// The result describes exactly one package tree. A replacement sets the old
	// tree aside and drops the cached result, so the lookup, the verification
	// and the cache store all happen under the package mutex: otherwise a
	// verification that began before a swap could observe a mixed tree and
	// cache a stale result after the invalidation, and the daemon would keep
	// reporting the old binary for the new package.
	s.d.pkgMu.Lock()
	defer s.d.pkgMu.Unlock()
	app, ok := s.d.reg.Get(req.AppId)
	if !ok {
		return nil, status.Error(codes.NotFound, "application is not installed")
	}
	if app.Manifest.Entrypoint.Runtime == manifest.RuntimeBuiltin {
		return &frameworkv1.BinaryProvenanceResult{
			AppId: req.AppId, Status: frameworkv1.ProvenanceStatus_PROVENANCE_NOT_PROVIDED,
			Detail: "compiled-in system application; there is no package binary to verify",
		}, nil
	}
	s.d.provMu.Lock()
	cached, ok := s.d.prov[req.AppId]
	s.d.provMu.Unlock()
	if !ok {
		cached = provenance.Verify(app.Manifest.ID, app.Dir, app.Manifest, provenance.Config{TrustedRootPath: s.d.cfg.TrustedRootPath})
		s.d.provMu.Lock()
		s.d.prov[req.AppId] = cached
		s.d.provMu.Unlock()
	}
	return provenanceProto(cached), nil
}

func (s *applicationService) Launch(ctx context.Context, req *frameworkv1.LaunchRequest) (*frameworkv1.LaunchResult, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "launching an application is a local administrator operation"); err != nil {
		return nil, err
	}
	args := map[string]string{}
	for _, a := range req.Arguments {
		args[a.Name] = a.Value
	}
	runID, invID, spawned, err := s.d.Launch(req.AppId, args)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &frameworkv1.LaunchResult{RunId: runID, SpawnedNewProcess: spawned, InvocationId: invID}, nil
}

func (s *applicationService) Stop(ctx context.Context, req *frameworkv1.StopRequest) (*frameworkv1.StopResponse, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "stopping an application is a local administrator operation"); err != nil {
		return nil, err
	}
	return &frameworkv1.StopResponse{Requested: s.d.Stop(req.RunId)}, nil
}

func (s *applicationService) ListRuns(ctx context.Context, req *frameworkv1.ListRunsRequest) (*frameworkv1.ListRunsResponse, error) {
	filter := req.AppId
	if callerApp, isApp := s.d.applicationCaller(ctx); isApp {
		if callerApp == "" {
			return nil, status.Error(codes.PermissionDenied, "application run is not live")
		}
		if req.AppId != "" && req.AppId != callerApp {
			s.d.logs.Warnf("daemon", callerApp, "denied run listing for %q", req.AppId)
			return nil, status.Error(codes.PermissionDenied, "applications may only list their own runs")
		}
		filter = callerApp
	}
	out := &frameworkv1.ListRunsResponse{}
	for _, run := range s.d.sup.List(filter) {
		out.Runs = append(out.Runs, runProto(run))
	}
	return out, nil
}

func (s *applicationService) WatchRuns(req *frameworkv1.WatchRunsRequest, stream frameworkv1.ApplicationService_WatchRunsServer) error {
	filter := req.AppId
	if callerApp, isApp := s.d.applicationCaller(stream.Context()); isApp {
		if callerApp == "" {
			return status.Error(codes.PermissionDenied, "application run is not live")
		}
		if req.AppId != "" && req.AppId != callerApp {
			s.d.logs.Warnf("daemon", callerApp, "denied run watch for %q", req.AppId)
			return status.Error(codes.PermissionDenied, "applications may only watch their own runs")
		}
		filter = callerApp
	}
	ch, cancel := s.d.subscribeRuns()
	defer cancel()
	for _, run := range s.d.sup.List(filter) {
		if err := stream.Send(&frameworkv1.AppRunEvent{Run: runProto(run)}); err != nil {
			return err
		}
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case run, ok := <-ch:
			if !ok {
				return nil
			}
			if filter != "" && run.AppID != filter {
				continue
			}
			if err := stream.Send(&frameworkv1.AppRunEvent{Run: runProto(run)}); err != nil {
				return err
			}
		}
	}
}

// --- package administration -------------------------------------------------

// InstallApplication accepts one application package as a .tar.gz stream and
// activates it in the live registry. It is a local-administrator operation:
// the caller gate runs before a single byte of the stream is read, so a
// supervised application can never install a package.
// installOutcome is one worker result.
type installOutcome struct {
	install PackageInstall
	err     error
}

// superviseInstall applies the upload bounds to one running install. A timeout
// is reported only when the supervisor wins the commit handshake; once the
// worker has begun its commit, the real outcome is awaited and returned, so a
// completed install can never be reported as "nothing was installed". The
// clock is injected so the decision is testable without waiting.
func superviseInstall(done <-chan installOutcome, src *packageStream, commit *installCommit,
	idle, total, interval time.Duration, now func() time.Time) (installOutcome, error) {
	started := now()
	abandoned := func(why string) (installOutcome, error) {
		// A finish that raced this tick wins: never turn a completed install
		// into a timeout.
		if out, ok := drainInstall(done); ok {
			return out, nil
		}
		if !commit.abandon() {
			// The worker is past the point of no return: report what actually
			// happened, however long the swap takes.
			return <-done, nil
		}
		// The abandon decision is final. If the worker still produced a
		// pre-commit result before noticing, its reason is more useful than
		// the timeout.
		if out, ok := drainInstall(done); ok {
			return out, nil
		}
		return installOutcome{}, status.Errorf(codes.DeadlineExceeded, "%s; nothing was installed", why)
	}
	for {
		select {
		case out := <-done:
			return out, nil
		case <-time.After(interval):
			if silent := now().Sub(src.lastActivity()); silent >= idle {
				return abandoned(fmt.Sprintf("the package upload stalled for %s", silent.Round(time.Second)))
			}
			if elapsed := now().Sub(started); elapsed >= total {
				return abandoned(fmt.Sprintf("the package upload exceeded %s", total))
			}
		}
	}
}

// drainInstall takes a result that is already available without waiting.
func drainInstall(done <-chan installOutcome) (installOutcome, bool) {
	select {
	case out := <-done:
		return out, true
	default:
		return installOutcome{}, false
	}
}

// InstallApplication accepts one application package as a .tar.gz stream and
// activates it in the live registry. It is a local-administrator operation:
// the caller gate runs before a single byte of the stream is read, so a
// supervised application can never install a package.
func (s *applicationService) InstallApplication(stream frameworkv1.ApplicationService_InstallApplicationServer) error {
	if err := s.d.requireAdministrativeCaller(stream.Context(), "installing an application package is a local administrator operation"); err != nil {
		return err
	}
	src, err := openPackageStream(stream)
	if err != nil {
		return err
	}
	// The install runs in a worker so this handler can bound how long a client
	// that stops sending may hold the single staging slot. Returning from this
	// handler ends the RPC and cancels the stream context, which unblocks the
	// worker's parked transport read (verified against the pinned
	// google.golang.org/grpc v1.67.1); the worker then aborts extraction,
	// removes its staging tree and releases the admission slot in its own
	// defers. A derived context alone cannot wake that read, which is why the
	// handler-return mechanism stays, and why the swap decision uses the
	// installCommit handshake instead of a context check.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	commit := &installCommit{}
	done := make(chan installOutcome, 1)
	go func() {
		install, err := s.d.installPackageStream(ctx, src, commit)
		done <- installOutcome{install: install, err: err}
	}()
	idle := s.d.installIdleBudget
	if idle <= 0 {
		idle = defaultInstallIdleBudget
	}
	interval := idle / 10
	if interval > 2*time.Second {
		interval = 2 * time.Second
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	out, err := superviseInstall(done, src, commit, idle, installTotalBudget, interval, time.Now)
	if err != nil {
		return err
	}
	if out.err != nil {
		return src.refine(out.err)
	}
	return stream.SendAndClose(&frameworkv1.InstallApplicationResult{
		Application:     s.d.appProto(out.install.App),
		Replaced:        out.install.Replaced,
		PreviousVersion: out.install.PreviousVersion,
	})
}

func (s *applicationService) UninstallApplication(ctx context.Context, req *frameworkv1.UninstallApplicationRequest) (*frameworkv1.UninstallApplicationResult, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "removing an application package is a local administrator operation"); err != nil {
		return nil, err
	}
	if err := s.d.UninstallPackage(req.GetAppId()); err != nil {
		return nil, err
	}
	return &frameworkv1.UninstallApplicationResult{AppId: req.GetAppId()}, nil
}

// packageStream adapts the install request stream to an io.Reader for the
// artifact layer. Chunk boundaries carry no meaning, and the running total is
// bounded by artifact.MaxArchiveBytes before any byte reaches the staging
// tree. A transport-level refusal is remembered so the caller can report it
// instead of the generic archive error it caused.
type packageStream struct {
	stream frameworkv1.ApplicationService_InstallApplicationServer
	// limit bounds the compressed archive. It is a field so a test can prove
	// the refusal without streaming 65 MiB; production always uses the
	// artifact bound.
	limit int64
	// lastRecvNS is the wall-clock time of the last received message. The
	// supervising handler reads it to detect a client that stopped sending
	// without closing the stream.
	lastRecvNS atomic.Int64
	rest       []byte
	total      int64
	problem    error
}

// touch records that the client delivered something.
func (s *packageStream) touch() { s.lastRecvNS.Store(time.Now().UnixNano()) }

// lastActivity reports when the stream last delivered a message.
func (s *packageStream) lastActivity() time.Time { return time.Unix(0, s.lastRecvNS.Load()) }

// openPackageStream consumes the stream's first message, which must be the
// open frame, and returns the archive reader for the remaining messages.
func openPackageStream(stream frameworkv1.ApplicationService_InstallApplicationServer) (*packageStream, error) {
	first, err := stream.Recv()
	if err == io.EOF {
		return nil, status.Error(codes.InvalidArgument, "the package stream carries no messages")
	}
	if err != nil {
		return nil, err
	}
	if first.GetOpen() == nil {
		return nil, status.Error(codes.InvalidArgument, "the first message of an application package stream must be the open frame")
	}
	src := &packageStream{stream: stream, limit: artifact.MaxArchiveBytes}
	src.touch()
	return src, nil
}

func (s *packageStream) Read(p []byte) (int, error) {
	for {
		if s.problem != nil {
			return 0, s.problem
		}
		if len(s.rest) > 0 {
			n := copy(p, s.rest)
			s.rest = s.rest[n:]
			s.touch()
			return n, nil
		}
		msg, err := s.stream.Recv()
		if err == io.EOF {
			return 0, io.EOF
		}
		if err != nil {
			s.problem = err
			return 0, err
		}
		s.touch()
		switch chunk := msg.GetChunk().(type) {
		case *frameworkv1.InstallApplicationRequest_Content:
			if len(chunk.Content) == 0 {
				continue
			}
			s.total += int64(len(chunk.Content))
			if s.total > s.limit {
				s.problem = status.Errorf(codes.InvalidArgument,
					"the package archive exceeds the %d-byte upload limit", s.limit)
				return 0, s.problem
			}
			s.rest = chunk.Content
		default:
			s.problem = status.Error(codes.InvalidArgument,
				"the open frame may only be the first message of an application package stream")
			return 0, s.problem
		}
	}
}

// refine replaces the archive error caused by a failed stream with the failure
// itself, so a limit or framing violation keeps its own status and message.
func (s *packageStream) refine(err error) error {
	if s.problem != nil {
		return status.Convert(s.problem).Err()
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "the package archive is incomplete")
	}
	return err
}

// --- per-app configuration -------------------------------------------------

type appConfigService struct {
	frameworkv1.UnimplementedApplicationConfigurationServiceServer
	d *Daemon
}

func (s *appConfigService) Describe(ctx context.Context, req *frameworkv1.DescribeAppConfigurationRequest) (*frameworkv1.AppConfigurationSchema, error) {
	app, ok := s.d.reg.Get(req.AppId)
	if !ok {
		return nil, status.Error(codes.NotFound, "application is not installed")
	}
	out := &frameworkv1.AppConfigurationSchema{AppId: app.Manifest.ID}
	for _, def := range app.Manifest.Settings {
		entry := &frameworkv1.AppSettingDefinition{
			Key: def.Key, Label: def.Label, Description: def.Description,
			Type: settingTypeProto(def.Type), Required: def.Required, RestartRequired: def.RestartRequired,
		}
		if def.Default != nil {
			if raw, err := manifest.ValueJSON(def.Default); err == nil {
				entry.DefaultValueJson = raw
			}
		}
		for _, c := range def.Choices {
			entry.Choices = append(entry.Choices, &frameworkv1.SettingChoice{Value: c.Value, Label: c.Label})
		}
		if def.Min != nil {
			entry.HasMin = true
			entry.Min = *def.Min
		}
		if def.Max != nil {
			entry.HasMax = true
			entry.Max = *def.Max
		}
		out.Settings = append(out.Settings, entry)
	}
	for _, a := range app.Manifest.Actions {
		out.Actions = append(out.Actions, &frameworkv1.AppActionDefinition{
			ActionId: a.ID, Label: a.Label, Description: a.Description, ConfirmBeforeRun: a.ConfirmBeforeRun,
		})
	}
	return out, nil
}

func (s *appConfigService) Get(ctx context.Context, req *frameworkv1.GetAppConfigurationRequest) (*frameworkv1.AppConfigurationSnapshot, error) {
	if err := s.d.requireAppScope(ctx, req.AppId); err != nil {
		return nil, err
	}
	snap, err := s.d.appCfg.Snapshot(req.AppId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	out := &frameworkv1.AppConfigurationSnapshot{AppId: snap.AppID, Revision: snap.Revision}
	for _, v := range snap.Values {
		out.Values = append(out.Values, &frameworkv1.AppSettingState{Key: v.Key, ValueJson: v.Value, HasValue: v.HasValue})
	}
	return out, nil
}

func (s *appConfigService) Update(ctx context.Context, req *frameworkv1.UpdateAppConfigurationRequest) (*frameworkv1.AppConfigurationSnapshot, error) {
	if err := s.d.requireAppScope(ctx, req.AppId); err != nil {
		return nil, err
	}
	var changes []settings.Change
	for _, c := range req.Changes {
		if c.Clear {
			changes = append(changes, settings.Change{Key: c.Key, Clear: true})
			continue
		}
		dec := json.NewDecoder(strings.NewReader(c.ValueJson))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "setting %s: %v", c.Key, err)
		}
		changes = append(changes, settings.Change{Key: c.Key, Value: v})
	}
	snap, rev, private, err := s.d.appCfg.Update(req.AppId, req.ExpectedRevision, changes)
	if err != nil {
		return nil, s.d.appSettingsStatus(req.AppId, err)
	}
	s.d.pushAppSettings(req.AppId, rev, private)
	if s.d.uiChanges != nil {
		s.d.uiChanges.Publish(uichanges.AppSettings)
	}
	out := &frameworkv1.AppConfigurationSnapshot{AppId: snap.AppID, Revision: snap.Revision}
	for _, v := range snap.Values {
		out.Values = append(out.Values, &frameworkv1.AppSettingState{Key: v.Key, ValueJson: v.Value, HasValue: v.HasValue})
	}
	return out, nil
}

func (s *appConfigService) TriggerAction(ctx context.Context, req *frameworkv1.TriggerAppActionRequest) (*frameworkv1.TriggerAppActionResult, error) {
	if err := s.d.requireAppScope(ctx, req.AppId); err != nil {
		return nil, err
	}
	id, err := s.d.TriggerAction(req.AppId, req.ActionId)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &frameworkv1.TriggerAppActionResult{ActionInvocationId: id}, nil
}

// --- interface discovery ---------------------------------------------------

type appInterfaceService struct {
	frameworkv1.UnimplementedAppInterfaceServiceServer
	d *Daemon
}

func (s *appInterfaceService) ListInterfaces(ctx context.Context, req *frameworkv1.ListAppInterfacesRequest) (*frameworkv1.ListAppInterfacesResponse, error) {
	out := &frameworkv1.ListAppInterfacesResponse{}
	apps := s.d.reg.List()
	if req.AppId != "" {
		app, ok := s.d.reg.Get(req.AppId)
		if !ok {
			return nil, status.Error(codes.NotFound, "application is not installed")
		}
		apps = []*registry.App{app}
	}
	for _, app := range apps {
		runID, running := s.d.bus.ReadyApp(app.ID())
		if req.RunningOnly && !running {
			continue
		}
		for _, iface := range app.Interfaces {
			out.Interfaces = append(out.Interfaces, &frameworkv1.AppInterfaceInfo{
				AppId: app.ID(), RunId: runID, GrpcService: iface.Service,
				InterfaceVersion: iface.Version, Available: running, Summary: iface.Summary,
			})
		}
	}
	return out, nil
}

func (s *appInterfaceService) DescribeInterface(ctx context.Context, req *frameworkv1.DescribeAppInterfaceRequest) (*frameworkv1.AppInterfaceDescription, error) {
	app, ok := s.d.reg.Get(req.AppId)
	if !ok {
		return nil, status.Error(codes.NotFound, "application is not installed")
	}
	iface, ok := app.InterfacesFor(req.GrpcService)
	if !ok {
		return nil, status.Error(codes.NotFound, "interface is not declared by this application")
	}
	runID, running := s.d.bus.ReadyApp(req.AppId)
	desc := &frameworkv1.AppInterfaceDescription{
		Info: &frameworkv1.AppInterfaceInfo{
			AppId: req.AppId, RunId: runID, GrpcService: iface.Service,
			InterfaceVersion: iface.Version, Available: running, Summary: iface.Summary,
		},
		FileDescriptorSet:     iface.Descriptor,
		DocumentationMarkdown: iface.Docs,
	}
	for _, m := range iface.Methods {
		desc.Methods = append(desc.Methods, &frameworkv1.AppMethodDocumentation{
			RpcName: m.Name, Summary: methodSummary(iface.Docs, m.Name),
			SemanticsMarkdown: methodSection(iface.Docs, m.Name),
		})
	}
	return desc, nil
}

// methodSummary finds a line mentioning the RPC name in the app's markdown.
func methodSummary(docs, method string) string {
	for _, line := range strings.Split(docs, "\n") {
		if strings.Contains(line, method) {
			line = strings.TrimSpace(line)
			if i := strings.Index(line, ":"); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
			return line
		}
	}
	return ""
}

func methodSection(docs, method string) string {
	var b strings.Builder
	capture := false
	for _, line := range strings.Split(docs, "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "`") {
			capture = strings.Contains(line, method)
			if capture {
				b.WriteString(line)
				b.WriteByte('\n')
			}
			continue
		}
		if capture {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

// --- app bus ---------------------------------------------------------------

type appBusService struct {
	frameworkv1.UnimplementedAppBusServiceServer
	b *bus.Bus
}

func (s *appBusService) Connect(stream frameworkv1.AppBusService_ConnectServer) error {
	return s.b.Connect(stream)
}

func (s *appBusService) Call(stream frameworkv1.AppBusService_CallServer) error {
	return s.b.Call(stream)
}

func (d *Daemon) registerApplicationServices(srv *grpc.Server) {
	frameworkv1.RegisterApplicationServiceServer(srv, &applicationService{d: d})
	frameworkv1.RegisterApplicationConfigurationServiceServer(srv, &appConfigService{d: d})
	frameworkv1.RegisterAppInterfaceServiceServer(srv, &appInterfaceService{d: d})
	frameworkv1.RegisterAppBusServiceServer(srv, &appBusService{b: d.bus})
}
