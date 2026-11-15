package daemon

import (
	"context"
	"encoding/json"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
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
