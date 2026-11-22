package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
	"capsulad/internal/appui"
	"capsulad/internal/discovery"
	"capsulad/internal/logbus"
)

// snapshot builders translate the daemon's framework messages into the
// browser schema. They are the only place that knows framework field names,
// so a daemon contract change fails here rather than in the SPA.

func (s *Server) snapshot(ctx context.Context, topic webv1.Topic, appID string) (*webv1.Snapshot, error) {
	snap := &webv1.Snapshot{Topic: topic, AppId: appID}
	switch topic {
	case webv1.Topic_TOPIC_OVERVIEW:
		view, err := s.overviewView(ctx)
		if err != nil {
			return nil, err
		}
		snap.View = &webv1.Snapshot_Overview{Overview: view}
	case webv1.Topic_TOPIC_SETTINGS:
		view, err := s.settingsView(ctx)
		if err != nil {
			return nil, err
		}
		snap.View = &webv1.Snapshot_Settings{Settings: view}
	case webv1.Topic_TOPIC_APPLICATION:
		if appID == "" {
			return nil, status.Error(codes.InvalidArgument, "application topic needs an app id")
		}
		view, err := s.applicationView(ctx, appID)
		if err != nil {
			return nil, err
		}
		snap.View = &webv1.Snapshot_Application{Application: view}
	case webv1.Topic_TOPIC_DIAGNOSTICS:
		view, err := s.diagnosticsView(ctx)
		if err != nil {
			return nil, err
		}
		snap.View = &webv1.Snapshot_Diagnostics{Diagnostics: view}
	case webv1.Topic_TOPIC_WAKEWORD:
		view, err := s.wakewordView(ctx)
		if err != nil {
			return nil, err
		}
		snap.View = &webv1.Snapshot_Wakeword{Wakeword: view}
	case webv1.Topic_TOPIC_ACCESS:
		view := s.accessView()
		snap.View = &webv1.Snapshot_Access{Access: view}
	case webv1.Topic_TOPIC_WIFI:
		view := s.wifiView(ctx)
		snap.View = &webv1.Snapshot_Wifi{Wifi: view}
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown snapshot topic")
	}
	return snap, nil
}

// assistantSnapshot builds only the data needed to control the speaker. In
// particular it never requests application configuration, log history,
// adapter details, or wakeword data from the local administrator connection.
func (s *Server) assistantSnapshot(ctx context.Context, topic webv1.Topic, appID string) (*webv1.Snapshot, error) {
	snap := &webv1.Snapshot{Topic: topic}
	switch topic {
	case webv1.Topic_TOPIC_OVERVIEW:
		view, err := s.buildOverview(ctx, true)
		if err != nil {
			return nil, err
		}
		snap.View = &webv1.Snapshot_Overview{Overview: view}
	case webv1.Topic_TOPIC_SETTINGS:
		view, err := s.buildSettings(ctx, true)
		if err != nil {
			return nil, err
		}
		snap.View = &webv1.Snapshot_Settings{Settings: view}
	case webv1.Topic_TOPIC_APPLICATION:
		if appID == "" {
			return nil, status.Error(codes.InvalidArgument, "application topic needs an app id")
		}
		app, err := s.appByID(ctx, appID)
		if err != nil {
			return nil, err
		}
		view := &webv1.ApplicationView{App: assistantAppView(app)}
		appsvc := frameworkv1.NewApplicationServiceClient(s.Conn)
		if runs, err := appsvc.ListRuns(ctx, &frameworkv1.ListRunsRequest{AppId: appID}); err == nil {
			view.Runs = assistantRunViews(runs.GetRuns())
		}
		switch appID {
		case clockAppID:
			if state, err := s.clockSnapshot(ctx, appID, clockServiceAPI); err == nil {
				view.Clock = clockView(state)
			} else {
				view.ClockError = "clock state is unavailable"
			}
		case ringAppID:
			if state, err := s.ringSnapshot(ctx, appID, ringServiceAPI); err == nil {
				view.Ring = ringView(state)
			} else {
				view.RingError = "ring state is unavailable"
			}
		}
		snap.AppId = appID
		snap.View = &webv1.Snapshot_Application{Application: view}
	default:
		return nil, status.Error(codes.PermissionDenied, "the assistant scope does not permit this snapshot topic")
	}
	return snap, nil
}

// bluetoothAddressRe matches the address shapes BlueZ may hand back as a
// device name: six colon- or hyphen-separated octets, or twelve bare hex
// digits.
var bluetoothAddressRe = regexp.MustCompile(`^(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$|^[0-9A-Fa-f]{12}$`)

// displayPairedName returns the paired device name that may be shown to the
// browser. BlueZ reports the address as the name for a device without an
// alias, so an address-shaped name is dropped rather than displayed.
func displayPairedName(address, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.EqualFold(name, strings.TrimSpace(address)) || bluetoothAddressRe.MatchString(name) {
		return ""
	}
	return name
}

func (s *Server) overviewView(ctx context.Context) (*webv1.OverviewView, error) {
	return s.buildOverview(ctx, false)
}

func (s *Server) buildOverview(ctx context.Context, assistant bool) (*webv1.OverviewView, error) {
	view := &webv1.OverviewView{}
	if assistant {
		if audio, err := frameworkv1.NewAudioServiceClient(s.Conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{}); err == nil {
			view.Audio = &webv1.AudioView{MasterVolume: audio.GetMasterVolume()}
		}
	} else {
		dev := frameworkv1.NewDeviceServiceClient(s.Conn)
		info, err := dev.GetInfo(ctx, &frameworkv1.GetInfoRequest{})
		if err != nil {
			return nil, err
		}
		audio, _ := frameworkv1.NewAudioServiceClient(s.Conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{})
		sys := frameworkv1.NewSystemServiceClient(s.Conn)
		bt, _ := sys.GetBluetoothStatus(ctx, &frameworkv1.GetBluetoothStatusRequest{})
		view.Device, view.Audio, view.Bluetooth = deviceView(info), audioView(audio), bluetoothView(bt)
		if paired, err := sys.ListPairedDevices(ctx, &frameworkv1.ListPairedDevicesRequest{}); err != nil {
			view.BluetoothError = rpcMessage(err)
		} else {
			for _, d := range paired.GetDevices() {
				view.PairedDevices = append(view.PairedDevices, &webv1.PairedDeviceView{
					Name: displayPairedName(d.GetAddress(), d.GetName()), Connected: d.GetConnected(), Trusted: d.GetTrusted(),
					DeviceId: pairedDeviceID(d.GetAddress()),
				})
			}
		}
	}
	appsResp, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx, &frameworkv1.ListApplicationsRequest{})
	if err != nil {
		return nil, err
	}
	// A media provider must be running to be a provider: an installed player
	// with no run advertises a service the daemon cannot route to. A failed
	// ListRuns proves nothing, so it yields no providers rather than stale ones.
	runs, runsErr := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{})
	running := map[string]bool{}
	if runsErr == nil {
		for _, run := range runs.GetRuns() {
			// Only a READY run can serve its declared services. A run that is
			// still starting, stopping or failed is listed with its real state,
			// but it must not mark the app as running: the daemon cannot route
			// to it, and the page would hide Launch behind a badge that lies.
			if run.GetState() != frameworkv1.AppRunState_APP_READY {
				continue
			}
			running[run.GetAppId()] = true
		}
	}
	providers, err := s.mediaProviders(ctx, appsResp.GetApplications(), running, assistant)
	if err != nil {
		return nil, err
	}
	view.Providers = providers
	if assistant {
		for _, app := range appsResp.GetApplications() {
			view.Apps = append(view.Apps, assistantAppView(app))
		}
		view.Runs = assistantRunViews(runs.GetRuns())
		return view, nil
	}
	selected := map[string]bool{}
	if settings, err := frameworkv1.NewSettingsServiceClient(s.Conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{}); err == nil {
		selected = selectedProviders(settings.GetValues())
		view.SettingsRevision = settings.GetRevision()
		view.SettingsAvailable = true
	}
	view.SelectedProviders = selected
	for _, a := range appsResp.GetApplications() {
		view.Apps = append(view.Apps, s.appListing(a))
	}
	view.Runs = runViews(runs.GetRuns(), selected)
	return view, nil
}

func (s *Server) settingsView(ctx context.Context) (*webv1.SettingsView, error) {
	return s.buildSettings(ctx, false)
}

func (s *Server) buildSettings(ctx context.Context, assistant bool) (*webv1.SettingsView, error) {
	settings, err := frameworkv1.NewSettingsServiceClient(s.Conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{})
	if err != nil {
		return nil, err
	}
	view := &webv1.SettingsView{Revision: settings.GetRevision()}
	if assistant {
		values := settings.GetValues()
		view.Values = &webv1.SettingsValues{
			DisplayBrightness: values.GetDisplayBrightness(), BrightnessMode: values.GetBrightnessMode(),
			MasterVolume: values.GetMasterVolume(), StartupSoundEnabled: values.GetStartupSoundEnabled(),
			Timezone: values.GetTimezone(),
		}
	} else {
		view.Values = settingsValues(settings.GetValues())
	}
	resp, _ := frameworkv1.NewDeviceServiceClient(s.Conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	for _, res := range resp.GetResources() {
		switch res.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			view.Display = resourceView(res)
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			view.Ring = resourceView(res)
		case frameworkv1.ResourceKind_RESOURCE_SENSOR:
			if res.ResourceId == "sensor.ambient_light" {
				view.AmbientAvailable = true
			}
		}
	}
	if assistant {
		return view, nil
	}
	if apps, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx, &frameworkv1.ListApplicationsRequest{}); err == nil {
		for _, a := range apps.GetApplications() {
			view.Apps = append(view.Apps, s.appListing(a))
		}
	}
	if models, err := frameworkv1.NewAssistantServiceClient(s.Conn).ListWakewordModels(ctx, &frameworkv1.ListWakewordModelsRequest{}); err == nil {
		for _, m := range models.GetModels() {
			view.Models = append(view.Models, modelView(m))
		}
	}
	if bt, err := frameworkv1.NewSystemServiceClient(s.Conn).GetBluetoothStatus(ctx, &frameworkv1.GetBluetoothStatusRequest{}); err != nil {
		view.BluetoothError = rpcMessage(err)
	} else {
		view.Bluetooth = bluetoothView(bt)
	}
	view.Discovery = &webv1.DiscoveryView{Detail: "this daemon has no mDNS responder"}
	if s.Discovery != nil {
		st := s.Discovery.Status()
		view.Discovery = &webv1.DiscoveryView{
			Supported: st.Supported, Hostname: st.Hostname, Advertised: st.Advertised, Detail: st.Detail,
		}
		if st.Advertised != "" {
			view.DiscoveryUrl = s.advertisedURL(st.Advertised)
		}
	}
	return view, nil
}

func (s *Server) applicationView(ctx context.Context, appID string) (*webv1.ApplicationView, error) {
	appsvc := frameworkv1.NewApplicationServiceClient(s.Conn)
	apps, err := appsvc.ListApplications(ctx, &frameworkv1.ListApplicationsRequest{})
	if err != nil {
		return nil, err
	}
	var app *frameworkv1.InstalledApplication
	for _, a := range apps.GetApplications() {
		if a.GetAppId() == appID {
			app = a
			break
		}
	}
	if app == nil {
		return nil, status.Errorf(codes.NotFound, "application %q is not installed", appID)
	}
	view := &webv1.ApplicationView{App: s.appListing(app)}
	selected := false
	if settings, err := frameworkv1.NewSettingsServiceClient(s.Conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{}); err == nil {
		selected = isSelectedProvider(settings.GetValues(), appID)
	}
	view.SelectedDefault = selected

	cfg := frameworkv1.NewApplicationConfigurationServiceClient(s.Conn)
	if schema, err := cfg.Describe(ctx, &frameworkv1.DescribeAppConfigurationRequest{AppId: appID}); err == nil {
		view.Schema = schemaView(schema)
	}
	if view.Schema != nil && s.Apps != nil {
		if app, ok := s.Apps.Get(appID); ok && app.Manifest.SettingsUI != nil {
			view.Schema.SettingsUi = &webv1.SettingsUIView{
				Available: true, ApiVersion: appui.APIVersion, AssetUrl: appui.AssetPath(appID),
			}
		}
	}
	if snapshot, err := cfg.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: appID}); err == nil {
		view.ConfigRevision = snapshot.GetRevision()
		for _, v := range snapshot.GetValues() {
			view.Values = append(view.Values, &webv1.SettingStateView{Key: v.GetKey(), ValueJson: v.GetValueJson(), HasValue: v.GetHasValue()})
		}
	}
	ifaces, _ := frameworkv1.NewAppInterfaceServiceClient(s.Conn).ListInterfaces(ctx, &frameworkv1.ListAppInterfacesRequest{AppId: appID})
	for _, iface := range ifaces.GetInterfaces() {
		view.Interfaces = append(view.Interfaces, &webv1.InterfaceView{
			AppId: iface.GetAppId(), GrpcService: iface.GetGrpcService(),
			InterfaceVersion: iface.GetInterfaceVersion(), Available: iface.GetAvailable(), Summary: iface.GetSummary(),
		})
		if desc, err := frameworkv1.NewAppInterfaceServiceClient(s.Conn).DescribeInterface(ctx,
			&frameworkv1.DescribeAppInterfaceRequest{AppId: appID, GrpcService: iface.GetGrpcService()}); err == nil {
			d := &webv1.InterfaceDescriptionView{
				GrpcService: iface.GetGrpcService(), DocumentationMarkdown: desc.GetDocumentationMarkdown(),
			}
			for _, m := range desc.GetMethods() {
				d.Methods = append(d.Methods, &webv1.MethodDocumentationView{
					RpcName: m.GetRpcName(), Summary: m.GetSummary(), SemanticsMarkdown: m.GetSemanticsMarkdown(),
					RequestExampleJson: m.GetRequestExampleJson(), ResponseExampleJson: m.GetResponseExampleJson(),
				})
			}
			view.Descriptions = append(view.Descriptions, d)
		}
	}
	if runs, err := appsvc.ListRuns(ctx, &frameworkv1.ListRunsRequest{AppId: appID}); err == nil {
		view.Runs = runViews(runs.GetRuns(), map[string]bool{appID: selected})
	}
	if prov, err := appsvc.GetBinaryProvenance(ctx, &frameworkv1.GetBinaryProvenanceRequest{AppId: appID}); err == nil {
		view.Provenance = &webv1.ProvenanceView{
			Status: prov.GetStatus().String(), Detail: prov.GetDetail(), SubjectSha256Hex: prov.GetSubjectSha256Hex(),
			VerifiedRepository: prov.GetVerifiedRepository(), VerifiedWorkflow: prov.GetVerifiedWorkflow(),
			VerifiedCommitSha: prov.GetVerifiedCommitSha(),
		}
	}
	view.Logs = logViews(s.logsTail(ctx, appID, 120))
	// The clock and ring pages render the built-in apps' standard contracts.
	if appID == clockAppID {
		if state, err := s.clockSnapshot(ctx, appID, clockServiceAPI); err != nil {
			view.ClockError = rpcMessage(err)
		} else {
			view.Clock = clockView(state)
		}
	}
	if appID == ringAppID {
		if state, err := s.ringSnapshot(ctx, appID, ringServiceAPI); err != nil {
			view.RingError = rpcMessage(err)
		} else {
			view.Ring = ringView(state)
		}
	}
	return view, nil
}

func (s *Server) diagnosticsView(ctx context.Context) (*webv1.DiagnosticsView, error) {
	info, err := frameworkv1.NewDeviceServiceClient(s.Conn).GetInfo(ctx, &frameworkv1.GetInfoRequest{})
	if err != nil {
		return nil, err
	}
	view := &webv1.DiagnosticsView{
		Device:            deviceView(info),
		Routing:           map[string]*webv1.RoutingView{},
		SelectedProviders: map[string]bool{},
		Metrics:           s.metricsView(),
	}
	if settings, err := frameworkv1.NewSettingsServiceClient(s.Conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{}); err == nil {
		view.SelectedProviders = selectedProviders(settings.GetValues())
	}
	if runs, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{}); err == nil {
		view.Runs = runViews(runs.GetRuns(), view.SelectedProviders)
	}
	res, _ := frameworkv1.NewDeviceServiceClient(s.Conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	rsvc := frameworkv1.NewRoutingServiceClient(s.Conn)
	for _, resource := range res.GetResources() {
		view.Resources = append(view.Resources, resourceView(resource))
		if state, err := rsvc.GetRouting(ctx, &frameworkv1.GetRoutingRequest{ResourceId: resource.GetResourceId()}); err == nil {
			view.Routing[resource.GetResourceId()] = &webv1.RoutingView{
				ResourceId: state.GetResourceId(), ConfiguredDefaultAppId: state.GetConfiguredDefaultAppId(),
				SelectedPresentationId: state.GetSelectedPresentationId(), SelectedAppId: state.GetSelectedAppId(),
				Revision: state.GetRevision(),
			}
		}
	}
	view.Logs = logViews(s.logsTail(ctx, "", 200))
	audio, _ := frameworkv1.NewAudioServiceClient(s.Conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{})
	view.Audio = audioView(audio)
	return view, nil
}

func (s *Server) wakewordView(ctx context.Context) (*webv1.WakewordView, error) {
	view := &webv1.WakewordView{}
	svc := frameworkv1.NewAssistantServiceClient(s.Conn)
	if resp, err := svc.ListWakewordSamples(ctx, &frameworkv1.ListWakewordSamplesRequest{}); err == nil {
		for _, sample := range resp.GetSamples() {
			view.Samples = append(view.Samples, sampleView(sample))
		}
	}
	if resp, err := svc.ListWakewordModels(ctx, &frameworkv1.ListWakewordModelsRequest{}); err == nil {
		for _, m := range resp.GetModels() {
			view.Models = append(view.Models, modelView(m))
		}
	}
	if s.Wakeword != nil {
		for _, detection := range s.Wakeword.RecentDetections(20) {
			name := detection.DisplayName
			if name == "" {
				name = detection.ModelID
			}
			view.Detections = append(view.Detections, &webv1.WakewordDetectionView{
				ModelId: detection.ModelID, ModelDisplayName: name, DetectedUnixMs: detection.UnixMS,
				DetectionId: detection.ID,
			})
		}
	}
	return view, nil
}

// metricsView converts the host reading to the browser schema. A nil or stale
// collector still reports the unsupported-platform state honestly.
func (s *Server) metricsView() *webv1.SystemMetricsView {
	reading := hostMetrics.sample()
	return &webv1.SystemMetricsView{
		Available:            reading.Available,
		Detail:               reading.Detail,
		CpuAvailable:         reading.CPUAvailable,
		CpuUsagePercent:      reading.CPUUsagePercent,
		CpuCount:             reading.CPUCount,
		MemoryAvailable:      reading.MemoryAvailable,
		MemoryTotalBytes:     reading.MemoryTotalBytes,
		MemoryUsedBytes:      reading.MemoryUsedBytes,
		DiskAvailable:        reading.DiskAvailable,
		DiskTotalBytes:       reading.DiskTotalBytes,
		DiskFreeBytes:        reading.DiskFreeBytes,
		TemperatureAvailable: reading.TemperatureAvailable,
		SocTemperatureC:      reading.SoCTemperatureC,
		UptimeAvailable:      reading.UptimeAvailable,
		UptimeSeconds:        reading.UptimeSeconds,
	}
}

func (s *Server) accessView() *webv1.AccessView {
	st := s.Auth.Status([]string{s.Addr})
	view := &webv1.AccessView{Status: &webv1.AccessStatusView{
		HttpsEnabled: st.HTTPSEnabled, BootstrapComplete: st.BootstrapComplete,
		AdminPasswordSet: st.AdminPasswordSet, ActiveApiKeys: uint32(st.ActiveAPIKeys),
		ListenAddresses: st.ListenAddresses, ServerFingerprint: st.ServerFingerprint,
		ServerExpiryUnixMs: st.ServerExpiry.UnixMilli(), ServerDnsNames: st.ServerDNSNames,
		ServerIpAddresses: st.ServerIPAddresses,
	}}
	for _, k := range s.Auth.ListAPIKeys() {
		view.Keys = append(view.Keys, &webv1.ApiKeyView{
			Name: k.Name, Prefix: k.Prefix, CreatedUnixMs: k.CreatedAt.UnixMilli(), Revoked: k.Revoked,
			// The grant is what tells an operator key from a status key; the
			// Access page cannot describe its own list truthfully without it.
			Scopes: append([]string(nil), k.Scopes...),
		})
	}
	return view
}

// --- conversions -----------------------------------------------------------

func deviceView(info *frameworkv1.DeviceInfo) *webv1.DeviceInfo {
	if info == nil {
		return nil
	}
	v := &webv1.DeviceInfo{
		DeviceId: info.GetDeviceId(), Model: info.GetModel(), BoardRevision: info.GetBoardRevision(),
		Soc: info.GetSoc(), Kernel: info.GetKernel(), Firmware: info.GetFirmware(),
		DaemonVersion: info.GetDaemonVersion(), BootId: info.GetBootId(),
		TargetPlatform: info.GetTargetPlatform().String(),
	}
	for _, b := range info.GetSubsystemBackends() {
		v.SubsystemBackends = append(v.SubsystemBackends, &webv1.BackendView{
			Subsystem: b.GetSubsystem(), BackendId: b.GetBackendId(), Status: b.GetStatus().String(), Detail: b.GetDetail(),
		})
	}
	return v
}

func audioView(a *frameworkv1.AudioState) *webv1.AudioView {
	if a == nil {
		return nil
	}
	return &webv1.AudioView{
		MasterVolume: a.GetMasterVolume(), MicrophoneMuted: a.GetMicrophoneMuted(),
		MicrophoneMuteIsHardware: a.GetMicrophoneMuteIsHardware(),
	}
}

func bluetoothView(b *frameworkv1.BluetoothStatus) *webv1.BluetoothView {
	if b == nil {
		return nil
	}
	return &webv1.BluetoothView{
		Present: b.GetPresent(), Powered: b.GetPowered(), Discoverable: b.GetDiscoverable(),
		IncomingConnections: b.GetIncomingConnections(), Adapter: b.GetAdapter(), Detail: b.GetDetail(),
		Name: b.GetName(),
	}
}

// pairedDeviceID derives the stable opaque identifier the browser uses for a
// paired device. Bluetooth addresses are device-local identity and must never
// appear in a web view, result, log line or error, so the address is hashed
// and only a short prefix crosses the boundary. Hashing is normalized first so
// "AA:BB:…" and "aa:bb:…" resolve to the same device.
func pairedDeviceID(address string) string {
	normalized := strings.ToLower(strings.TrimSpace(address))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:12]
}

func assistantAppView(app *frameworkv1.InstalledApplication) *webv1.AppView {
	return &webv1.AppView{
		AppId: app.GetAppId(), DisplayName: app.GetDisplayName(), Version: app.GetVersion(),
		Provides: app.GetProvides(), SystemApp: app.GetSystemApp(), CanStop: app.GetCanStop(),
	}
}

func assistantRunViews(runs []*frameworkv1.AppRun) []*webv1.RunView {
	out := make([]*webv1.RunView, 0, len(runs))
	for _, run := range runs {
		out = append(out, &webv1.RunView{RunId: run.GetRunId(), AppId: run.GetAppId(), State: run.GetState().String()})
	}
	return out
}

// appListing projects an installed application for the browser. Declared
// actions are not part of the framework's application listing, so they come
// from the registry manifest: the same definitions DescribeAppConfiguration
// returns, attached here so a listing can offer them without one configuration
// read per installed application.
func (s *Server) appListing(a *frameworkv1.InstalledApplication) *webv1.AppView {
	v := appView(a)
	if v == nil || s.Apps == nil {
		return v
	}
	app, ok := s.Apps.Get(a.GetAppId())
	if !ok || app.Manifest == nil {
		return v
	}
	for _, action := range app.Manifest.Actions {
		v.Actions = append(v.Actions, &webv1.ActionDefinitionView{
			ActionId: action.ID, Label: action.Label, Description: action.Description,
			ConfirmBeforeRun: action.ConfirmBeforeRun,
		})
	}
	return v
}

func appView(a *frameworkv1.InstalledApplication) *webv1.AppView {
	if a == nil {
		return nil
	}
	v := &webv1.AppView{
		AppId: a.GetAppId(), DisplayName: a.GetDisplayName(), Version: a.GetVersion(),
		StartOnBoot: a.GetStartOnBoot(), Provides: a.GetProvides(),
		Background: a.GetBackground(), RestartOnFailure: a.GetRestartOnFailure(),
		SystemApp: a.GetSystemApp(), CanStop: a.GetCanStop(), CanUninstall: a.GetCanUninstall(),
	}
	for _, arg := range a.GetLaunchArguments() {
		v.LaunchArguments = append(v.LaunchArguments, &webv1.LaunchArgumentView{
			Name: arg.GetName(), Description: arg.GetDescription(), ValueType: arg.GetValueType(), Required: arg.GetRequired(),
		})
	}
	if ep := a.GetEntrypoint(); ep != nil {
		v.Entrypoint = &webv1.EntrypointView{
			Runtime: ep.GetRuntime().String(), RelativePath: ep.GetRelativePath(),
			CpuArchitecture: ep.GetCpuArchitecture(), BinaryAbi: ep.GetBinaryAbi(),
			FrameworkSdkVersion: ep.GetFrameworkSdkVersion(),
		}
	}
	return v
}

func runViews(runs []*frameworkv1.AppRun, selected map[string]bool) []*webv1.RunView {
	out := make([]*webv1.RunView, 0, len(runs))
	for _, r := range runs {
		out = append(out, &webv1.RunView{
			RunId: r.GetRunId(), AppId: r.GetAppId(), Pid: r.GetPid(), State: r.GetState().String(),
			StartedMonotonicNs: r.GetStartedMonotonicNs(), ExitCode: r.GetExitCode(),
			StatusDetail: r.GetStatusDetail(), Runtime: r.GetRuntime().String(),
			SelectedDefault: selected[r.GetAppId()],
		})
	}
	return out
}

func resourceView(r *frameworkv1.ResourceInfo) *webv1.ResourceView {
	if r == nil {
		return nil
	}
	return &webv1.ResourceView{
		ResourceId: r.GetResourceId(), Kind: r.GetKind().String(), Available: r.GetAvailable(),
		Routing: r.GetRouting().String(), Features: r.GetFeatures(), BackendName: r.GetBackendName(),
	}
}

func settingsValues(v *frameworkv1.UserSettings) *webv1.SettingsValues {
	if v == nil {
		return nil
	}
	out := &webv1.SettingsValues{
		DefaultAppByResource:  map[string]string{},
		DefaultAssistantAppId: v.GetDefaultAssistantAppId(),
		Wakewords:             bindingViews(v.GetWakewords()),
		WakewordEnabled:       v.GetWakewordEnabled(),
		DisplayBrightness:     map[string]float32{},
		MasterVolume:          v.GetMasterVolume(),
		MicrophoneMuted:       v.GetMicrophoneMuted(),
		TouchHoldMode:         v.GetTouchHoldMode().String(),
		Timezone:              v.GetTimezone(),
		BrightnessMode:        map[string]string{},
		StartupSoundEnabled:   v.GetStartupSoundEnabled(),
		Hostname:              v.GetHostname(),

		BluetoothIncomingConnections: v.GetBluetoothIncomingConnections(),
		BluetoothDiscoverable:        v.GetBluetoothDiscoverable(),
	}
	for k, value := range v.GetDefaultAppByResource() {
		out.DefaultAppByResource[k] = value
	}
	for k, value := range v.GetDisplayBrightness() {
		out.DisplayBrightness[k] = value
	}
	for k, value := range v.GetBrightnessMode() {
		out.BrightnessMode[k] = value
	}
	if ind := v.GetMutedMicrophoneIndicator(); ind != nil {
		out.MutedMicrophoneIndicator = &webv1.MutedIndicatorView{Mode: ind.GetMode().String(), DelayMs: ind.GetDelayMs()}
	}
	return out
}

// bindingViews copies the per-word configuration. A nil wire list stays nil (the
// legacy state); an empty list stays an explicit empty list.
func bindingViews(list []*frameworkv1.WakewordBinding) []*webv1.WakewordBindingView {
	if list == nil {
		return nil
	}
	out := make([]*webv1.WakewordBindingView, 0, len(list))
	for _, entry := range list {
		out = append(out, &webv1.WakewordBindingView{
			ModelId: entry.GetModelId(), AppId: entry.GetAppId(), Enabled: entry.GetEnabled(),
		})
	}
	return out
}

func modelView(m *frameworkv1.WakewordModelInfo) *webv1.WakewordModelView {
	return &webv1.WakewordModelView{
		ModelId: m.GetModelId(), DisplayName: m.GetDisplayName(), Language: m.GetLanguage(),
		EngineId: m.GetEngineId(), Available: m.GetAvailable(),
		Metrics: gateMetricsView(m.GetMetrics()), Threshold: m.GetThreshold(),
		Enabled: m.GetEnabled(), AppId: m.GetAppId(), Detail: m.GetDetail(),
	}
}

// gateMetricsView copies the trainer's measured splits for display. The browser
// renders these numbers as they are; it never recomputes a metric.
func gateMetricsView(splits []*frameworkv1.WakewordGateMetrics) []*webv1.WakewordGateMetricsView {
	out := make([]*webv1.WakewordGateMetricsView, 0, len(splits))
	for _, split := range splits {
		out = append(out, &webv1.WakewordGateMetricsView{
			Split:            split.GetSplit(),
			WordRecordings:   split.GetWordRecordings(),
			Detected:         split.GetDetected(),
			Recall:           split.GetRecall(),
			FalseActivations: split.GetFalseActivations(),
		})
	}
	return out
}

// trainingFailureView copies a structured refusal for the browser.
func trainingFailureView(failure *frameworkv1.WakewordTrainingFailure) *webv1.WakewordTrainingFailureView {
	if failure == nil {
		return nil
	}
	out := &webv1.WakewordTrainingFailureView{
		Causes:    append([]string(nil), failure.GetCauses()...),
		Threshold: failure.GetThreshold(),
		Detail:    failure.GetDetail(),
		Splits:    gateMetricsView(failure.GetSplits()),
		Takes:     make([]*webv1.WakewordFailingTakeView, 0, len(failure.GetTakes())),
	}
	for _, take := range failure.GetTakes() {
		out.Takes = append(out.Takes, &webv1.WakewordFailingTakeView{
			Partition: take.GetPartition(),
			Index:     take.GetIndex(),
			SampleId:  take.GetSampleId(),
			Cause:     take.GetCause(),
			Detail:    take.GetDetail(),
		})
	}
	return out
}

func sampleView(s *frameworkv1.WakewordSample) *webv1.WakewordSampleView {
	return &webv1.WakewordSampleView{
		SampleId: s.GetSampleId(), Label: s.GetLabel(), DurationMs: s.GetDurationMs(),
		RecordedUnixMs: s.GetRecordedUnixMs(),
	}
}

func schemaView(s *frameworkv1.AppConfigurationSchema) *webv1.AppConfigSchemaView {
	if s == nil {
		return nil
	}
	v := &webv1.AppConfigSchemaView{AppId: s.GetAppId()}
	for _, def := range s.GetSettings() {
		d := &webv1.SettingDefinitionView{
			Key: def.GetKey(), Label: def.GetLabel(), Description: def.GetDescription(),
			Type: def.GetType().String(), DefaultValueJson: def.GetDefaultValueJson(),
			Required: def.GetRequired(), RestartRequired: def.GetRestartRequired(),
			HasMin: def.GetHasMin(), Min: def.GetMin(), HasMax: def.GetHasMax(), Max: def.GetMax(),
		}
		for _, c := range def.GetChoices() {
			d.Choices = append(d.Choices, &webv1.SettingChoiceView{Value: c.GetValue(), Label: c.GetLabel()})
		}
		v.Settings = append(v.Settings, d)
	}
	for _, a := range s.GetActions() {
		v.Actions = append(v.Actions, &webv1.ActionDefinitionView{
			ActionId: a.GetActionId(), Label: a.GetLabel(), Description: a.GetDescription(),
			ConfirmBeforeRun: a.GetConfirmBeforeRun(),
		})
	}
	return v
}

func logViews(entries []logbus.Entry) []*webv1.LogEntry {
	out := make([]*webv1.LogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &webv1.LogEntry{
			MonotonicTimeNs: e.TimeNS, Component: e.Component, AppId: e.AppID,
			Severity: e.Severity, Message: e.Message,
		})
	}
	return out
}

func (s *Server) logsTail(ctx context.Context, appID string, limit int) []logbus.Entry {
	if limit <= 0 {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	stream, err := frameworkv1.NewDiagnosticsServiceClient(s.Conn).WatchLogs(wctx, &frameworkv1.WatchLogsRequest{AppId: appID})
	if err != nil {
		return nil
	}
	// The upstream stream replays its ring oldest-first. Keep a rolling window
	// of the newest records instead of stopping at the first `limit`, which
	// would show the oldest retained logs and then duplicate them when the
	// live watcher replays the same backlog.
	tail := make([]logbus.Entry, 0, limit)
	for {
		rec, err := stream.Recv()
		if err != nil {
			break
		}
		entry := logbus.Entry{
			TimeNS: rec.GetMonotonicTimeNs(), Component: rec.GetComponent(), AppID: rec.GetAppId(),
			Severity: rec.GetSeverity(), Message: rec.GetMessage(),
		}
		if len(tail) == limit {
			copy(tail, tail[1:])
			tail[limit-1] = entry
			continue
		}
		tail = append(tail, entry)
	}
	return tail
}

func (s *Server) mediaProviders(ctx context.Context, apps []*frameworkv1.InstalledApplication, running map[string]bool, assistant bool) ([]*webv1.MediaProviderView, error) {
	var out []*webv1.MediaProviderView
	for _, a := range apps {
		if !providesMedia(a) || !running[a.GetAppId()] {
			continue
		}
		players, err := s.listPlayers(ctx, a.GetAppId())
		if err != nil {
			// The overview is Now Playing, not provider discovery: a provider
			// whose players cannot be read has no observed playing player to
			// show, and an error row would be a card for a state the user
			// cannot act on. Discovery keeps reporting the provider.
			continue
		}
		provider := &webv1.MediaProviderView{AppId: a.GetAppId(), Name: a.GetDisplayName()}
		for _, p := range players {
			if p.GetPlaybackState() != mediav1.PlaybackState_PLAYBACK_PLAYING {
				continue
			}
			player := &webv1.MediaPlayerView{
				PlayerId: p.GetPlayerId(), PlaybackState: p.GetPlaybackState().String(),
				SupportedActions: mediaActions(p.GetSupportedActions()),
				AppName: p.GetAppName(),
				Title: p.GetTitle(), Artist: p.GetArtist(), Album: p.GetAlbum(),
				DurationMs: p.GetDurationMs(), PositionMs: p.GetPositionMs(), Revision: p.GetRevision(),
			}
			// A bare configured radio URL can be the source/media name.
			// Keep arbitrary source text (including peer names) on the operator view.
			if !assistant {
				player.SourceName, player.MediaName = p.GetSourceName(), p.GetMediaName()
			}
			provider.Players = append(provider.Players, player)
		}
		// A playing sibling keeps the provider in the projection; an idle,
		// paused or stopped-only provider is omitted rather than shown empty.
		if len(provider.Players) == 0 {
			continue
		}
		out = append(out, provider)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetAppId() < out[j].GetAppId() })
	// The connected Bluetooth source is a framework-owned provider, not an app:
	// no manifest, no run, no app id. It comes from the public
	// BluetoothMediaService on this socket and is appended after the application
	// providers, so a running player stays the default view. It appears only
	// while playback is actually observed; a connected but idle, paused or
	// stopped source is provider discovery, not Now Playing, and adds no row.
	if provider := s.frameworkMediaProvider(ctx, assistant); provider != nil {
		out = append(out, provider)
	}
	return out, nil
}

func providesMedia(a *frameworkv1.InstalledApplication) bool {
	for _, p := range a.GetProvides() {
		if p == "media_player" {
			return true
		}
	}
	return false
}

// mediaActions exposes the contract enum names without the MEDIA_ prefix
// ("PLAY", "SKIP_NEXT", …), which is what the SPA compares against. The
// internal names are MEDIA_PLAY etc.; MEDIA_ACTION_* is the browser command
// enum, a different vocabulary.
func mediaActions(actions []mediav1.MediaAction) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, strings.TrimPrefix(a.String(), "MEDIA_"))
	}
	return out
}

func (s *Server) listPlayers(ctx context.Context, appID string) ([]*mediav1.PlayerStatus, error) {
	req, _ := protoMarshal(&mediav1.ListPlayersRequest{})
	msgs, _, end, err := s.routedCall(ctx, appID, mediaService, "ListPlayers", req)
	if err != nil {
		return nil, err
	}
	if end != nil && end.GetGrpcStatusCode() != 0 {
		return nil, fmt.Errorf("%s: %s", codes.Code(end.GetGrpcStatusCode()), end.GetDetail())
	}
	var out []*mediav1.PlayerStatus
	for _, raw := range msgs {
		var resp mediav1.ListPlayersResponse
		if err := protoUnmarshal(raw, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.GetPlayers()...)
	}
	return out, nil
}

func isSelectedProvider(values *frameworkv1.UserSettings, appID string) bool {
	if values == nil || appID == "" {
		return false
	}
	if values.GetDefaultAssistantAppId() == appID {
		return true
	}
	for _, selected := range values.GetDefaultAppByResource() {
		if selected == appID {
			return true
		}
	}
	return false
}

func selectedProviders(values *frameworkv1.UserSettings) map[string]bool {
	out := map[string]bool{}
	if values == nil {
		return out
	}
	if v := values.GetDefaultAssistantAppId(); v != "" {
		out[v] = true
	}
	for _, selected := range values.GetDefaultAppByResource() {
		if selected != "" {
			out[selected] = true
		}
	}
	return out
}

func discoveryDetail(d discovery.Status) string { return d.Detail }
