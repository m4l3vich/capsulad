package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsula/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/logbus"
	"capsulad/internal/websecurity"
)

type overviewData struct {
	Info         *frameworkv1.DeviceInfo
	Audio        *frameworkv1.AudioState
	Bluetooth    *frameworkv1.BluetoothStatus
	Paired       []*frameworkv1.PairedDevice
	BluetoothErr string
	Providers    []mediaProvider
	Apps         []*frameworkv1.InstalledApplication
	Runs         []*frameworkv1.AppRun
}

type settingsPageData struct {
	Settings         *frameworkv1.SettingsSnapshot
	Display          *frameworkv1.ResourceInfo
	Ring             *frameworkv1.ResourceInfo
	DisplayDefault   string
	RingDefault      string
	DisplayPct       int
	RingPct          int
	DisplayAuto      bool
	RingAuto         bool
	Apps             []*frameworkv1.InstalledApplication
	Models           []*frameworkv1.WakewordModelInfo
	Audio            *frameworkv1.AudioState
	AmbientAvailable bool
}

type appPageData struct {
	App        *frameworkv1.InstalledApplication
	Schema     *frameworkv1.AppConfigurationSchema
	Snapshot   *frameworkv1.AppConfigurationSnapshot
	Values     map[string]*frameworkv1.AppSettingState
	Interfaces []*frameworkv1.AppInterfaceInfo
	Described  map[string]*frameworkv1.AppInterfaceDescription
	Runs       []*frameworkv1.AppRun
	Provenance *frameworkv1.BinaryProvenanceResult
	Logs       []logbus.Entry
	Args       []*frameworkv1.AppArgumentInfo

	ClockService string
	Clock        *clockState
	ClockErr     string
	AlarmForm    clockAlarmForm

	RingService string
	Ring        *ringState
	RingErr     string
}

type mediaProvider struct {
	AppID   string
	Name    string
	Players []*mediav1.PlayerStatus
	Err     string
}

type diagPageData struct {
	Info      *frameworkv1.DeviceInfo
	Resources []*frameworkv1.ResourceInfo
	Routing   map[string]*frameworkv1.RoutingState
	Runs      []*frameworkv1.AppRun
	Logs      []logbus.Entry
	Audio     *frameworkv1.AudioState
}

type accessPageData struct {
	Status     websecurity.Status
	Keys       []websecurity.APIKeyInfo
	NewKey     string
	NewKeyName string
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	data := overviewData{}
	dev := frameworkv1.NewDeviceServiceClient(s.Conn)
	data.Info, _ = dev.GetInfo(ctx, &frameworkv1.GetInfoRequest{})
	data.Audio, _ = frameworkv1.NewAudioServiceClient(s.Conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{})
	data.Bluetooth, _ = frameworkv1.NewSystemServiceClient(s.Conn).GetBluetoothStatus(ctx, &frameworkv1.GetBluetoothStatusRequest{})
	if paired, err := frameworkv1.NewSystemServiceClient(s.Conn).ListPairedDevices(ctx, &frameworkv1.ListPairedDevicesRequest{}); err != nil {
		data.BluetoothErr = err.Error()
	} else {
		data.Paired = paired.GetDevices()
	}
	if runs, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{}); err == nil {
		data.Runs = runs.Runs
	}
	if apps, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx, &frameworkv1.ListApplicationsRequest{}); err == nil {
		data.Apps = apps.Applications
	}
	data.Providers = s.mediaProviders(ctx)
	p := s.base(w, r, "Overview", "overview")
	p.Data = data
	s.render(w, "overview", p)
}

// mediaProviders collects players published by running apps.
func (s *Server) mediaProviders(ctx context.Context) []mediaProvider {
	ifaces, _ := frameworkv1.NewAppInterfaceServiceClient(s.Conn).ListInterfaces(ctx,
		&frameworkv1.ListAppInterfacesRequest{RunningOnly: true})
	apps, _ := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx, &frameworkv1.ListApplicationsRequest{})
	names := map[string]string{}
	for _, a := range apps.GetApplications() {
		names[a.AppId] = a.DisplayName
	}
	var out []mediaProvider
	seen := map[string]bool{}
	for _, iface := range ifaces.GetInterfaces() {
		if iface.GrpcService != "capsula.contracts.media.v1.MediaPlayerService" || seen[iface.AppId] {
			continue
		}
		seen[iface.AppId] = true
		provider := mediaProvider{AppID: iface.AppId, Name: names[iface.AppId]}
		players, err := s.listPlayers(ctx, iface.AppId)
		if err != nil {
			provider.Err = err.Error()
		}
		provider.Players = players
		out = append(out, provider)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	data := settingsPageData{}
	data.Settings, _ = frameworkv1.NewSettingsServiceClient(s.Conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{})
	resp, _ := frameworkv1.NewDeviceServiceClient(s.Conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	for _, res := range resp.GetResources() {
		switch res.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			data.Display = res
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			data.Ring = res
		case frameworkv1.ResourceKind_RESOURCE_SENSOR:
			if res.ResourceId == "sensor.ambient_light" {
				data.AmbientAvailable = true
			}
		}
	}
	if apps, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx, &frameworkv1.ListApplicationsRequest{}); err == nil {
		data.Apps = apps.Applications
	}
	if models, err := frameworkv1.NewAssistantServiceClient(s.Conn).ListWakewordModels(ctx, &frameworkv1.ListWakewordModelsRequest{}); err == nil {
		data.Models = models.Models
	}
	data.Audio, _ = frameworkv1.NewAudioServiceClient(s.Conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{})
	if v := data.Settings.GetValues(); v != nil {
		if data.Display != nil {
			data.DisplayDefault = v.DefaultAppByResource[data.Display.ResourceId]
			data.DisplayPct = int(v.DisplayBrightness[data.Display.ResourceId] * 100)
			data.DisplayAuto = v.BrightnessMode[data.Display.ResourceId] == "auto"
		}
		if data.Ring != nil {
			data.RingDefault = v.DefaultAppByResource[data.Ring.ResourceId]
			data.RingPct = int(v.DisplayBrightness[data.Ring.ResourceId] * 100)
			data.RingAuto = v.BrightnessMode[data.Ring.ResourceId] == "auto"
		}
	}
	p := s.base(w, r, "Settings", "settings")
	p.Data = data
	s.render(w, "settings", p)
}

func (s *Server) application(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	appID := r.PathValue("id")
	data := appPageData{Described: map[string]*frameworkv1.AppInterfaceDescription{}, AlarmForm: defaultClockAlarmForm()}
	appsvc := frameworkv1.NewApplicationServiceClient(s.Conn)
	apps, err := appsvc.ListApplications(ctx, &frameworkv1.ListApplicationsRequest{})
	if err != nil {
		s.redirect(w, r, "/applications", "", err.Error())
		return
	}
	for _, a := range apps.Applications {
		if a.AppId == appID {
			data.App = a
			data.Args = a.LaunchArguments
			break
		}
	}
	if data.App == nil {
		s.redirect(w, r, "/applications", "", "application not found")
		return
	}
	cfg := frameworkv1.NewApplicationConfigurationServiceClient(s.Conn)
	data.Schema, _ = cfg.Describe(ctx, &frameworkv1.DescribeAppConfigurationRequest{AppId: appID})
	data.Snapshot, _ = cfg.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: appID})
	data.Values = map[string]*frameworkv1.AppSettingState{}
	if data.Snapshot != nil {
		for _, v := range data.Snapshot.Values {
			data.Values[v.Key] = v
		}
	}
	ifaces, _ := frameworkv1.NewAppInterfaceServiceClient(s.Conn).ListInterfaces(ctx, &frameworkv1.ListAppInterfacesRequest{AppId: appID})
	if ifaces != nil {
		data.Interfaces = ifaces.Interfaces
		for _, iface := range ifaces.Interfaces {
			if desc, err := frameworkv1.NewAppInterfaceServiceClient(s.Conn).DescribeInterface(ctx, &frameworkv1.DescribeAppInterfaceRequest{AppId: appID, GrpcService: iface.GrpcService}); err == nil {
				data.Described[iface.GrpcService] = desc
			}
		}
	}
	if service, err := s.findClockService(ctx, appID); err == nil {
		data.ClockService = service
		if state, err := s.clockSnapshot(ctx, appID, service); err != nil {
			data.ClockErr = rpcMessage(err)
		} else {
			data.Clock = state
			if editID := r.URL.Query().Get("edit"); editID != "" {
				for _, alarm := range state.Alarms {
					if alarm.AlarmID != editID {
						continue
					}
					data.AlarmForm = clockAlarmForm{
						AlarmID: alarm.AlarmID, Label: alarm.Label,
						Hour: alarm.Hour, Minute: alarm.Minute, Days: alarm.DaysOfWeek,
						Timezone: alarm.Timezone, Enabled: alarm.Enabled,
					}
					break
				}
			}
		}
	}
	if service, err := s.findRingService(ctx, appID, data.App.Provides); err == nil {
		data.RingService = service
		if state, err := s.ringSnapshot(ctx, appID, service); err != nil {
			data.RingErr = rpcMessage(err)
		} else {
			data.Ring = state
		}
	}
	runs, _ := appsvc.ListRuns(ctx, &frameworkv1.ListRunsRequest{AppId: appID})
	if runs != nil {
		data.Runs = runs.Runs
	}
	data.Provenance, _ = appsvc.GetBinaryProvenance(ctx, &frameworkv1.GetBinaryProvenanceRequest{AppId: appID})
	data.Logs = s.logsTail(ctx, appID, 120)
	p := s.base(w, r, data.App.DisplayName, "overview")
	p.Data = data
	s.render(w, "application", p)
}

func (s *Server) appLaunch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	appID := r.PathValue("id")
	apps, _ := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx, &frameworkv1.ListApplicationsRequest{})
	var declared []*frameworkv1.AppArgumentInfo
	for _, a := range apps.GetApplications() {
		if a.AppId == appID {
			declared = a.LaunchArguments
		}
	}
	var args []*frameworkv1.LaunchArgument
	for _, a := range declared {
		v := r.PostFormValue("arg_" + a.Name)
		if v == "" {
			continue
		}
		args = append(args, &frameworkv1.LaunchArgument{Name: a.Name, Value: v})
	}
	res, err := frameworkv1.NewApplicationServiceClient(s.Conn).Launch(ctx, &frameworkv1.LaunchRequest{AppId: appID, Arguments: args})
	if err != nil {
		s.redirect(w, r, "/applications/"+appID, "", err.Error())
		return
	}
	s.redirect(w, r, "/applications/"+appID, "launch requested: run "+short(res.RunId)+" invocation "+short(res.InvocationId), "")
}

func (s *Server) appStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	appID := r.PathValue("id")
	runID := r.PostFormValue("run_id")
	res, err := frameworkv1.NewApplicationServiceClient(s.Conn).Stop(ctx, &frameworkv1.StopRequest{RunId: runID})
	if err != nil {
		s.redirect(w, r, "/applications/"+appID, "", err.Error())
		return
	}
	if !res.Requested {
		s.redirect(w, r, "/applications/"+appID, "", "stop was not accepted")
		return
	}
	s.redirect(w, r, "/applications/"+appID, "stop requested", "")
}

func (s *Server) appAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	appID := r.PathValue("id")
	actionID := r.PostFormValue("action_id")
	res, err := frameworkv1.NewApplicationConfigurationServiceClient(s.Conn).TriggerAction(ctx,
		&frameworkv1.TriggerAppActionRequest{AppId: appID, ActionId: actionID})
	if err != nil {
		s.redirect(w, r, "/applications/"+appID, "", err.Error())
		return
	}
	s.redirect(w, r, "/applications/"+appID, "action sent: "+short(res.ActionInvocationId), "")
}

func (s *Server) appSettings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	appID := r.PathValue("id")
	cfg := frameworkv1.NewApplicationConfigurationServiceClient(s.Conn)
	schema, err := cfg.Describe(ctx, &frameworkv1.DescribeAppConfigurationRequest{AppId: appID})
	if err != nil {
		s.redirect(w, r, "/applications/"+appID, "", err.Error())
		return
	}
	expected, _ := strconv.ParseUint(r.PostFormValue("revision"), 10, 64)
	var changes []*frameworkv1.AppSettingChange
	for _, def := range schema.Settings {
		field := "set_" + def.Key
		if def.Type == frameworkv1.SettingType_SETTING_BOOLEAN && r.PostFormValue("present_"+def.Key) != "1" {
			continue
		}
		if def.Type == frameworkv1.SettingType_SETTING_TEXT_MASKED {
			if r.PostFormValue("clear_"+def.Key) == "1" {
				changes = append(changes, &frameworkv1.AppSettingChange{Key: def.Key, Clear: true})
				continue
			}
			if v := r.PostFormValue(field); v != "" {
				changes = append(changes, &frameworkv1.AppSettingChange{Key: def.Key, ValueJson: jsonString(v)})
			}
			continue
		}
		if def.Type == frameworkv1.SettingType_SETTING_BOOLEAN {
			v := r.PostFormValue(field)
			changes = append(changes, &frameworkv1.AppSettingChange{Key: def.Key, ValueJson: boolJSON(v == "on" || v == "true")})
			continue
		}
		raw, ok := r.PostForm[field]
		if !ok {
			continue
		}
		v := raw[0]
		switch def.Type {
		case frameworkv1.SettingType_SETTING_INTEGER, frameworkv1.SettingType_SETTING_NUMBER:
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				s.redirect(w, r, "/applications/"+appID, "", fmt.Sprintf("%s must be a number", def.Label))
				return
			}
			changes = append(changes, &frameworkv1.AppSettingChange{Key: def.Key, ValueJson: v})
		default:
			changes = append(changes, &frameworkv1.AppSettingChange{Key: def.Key, ValueJson: jsonString(v)})
		}
	}
	_, err = cfg.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{AppId: appID, ExpectedRevision: expected, Changes: changes})
	if err != nil {
		s.redirect(w, r, "/applications/"+appID, "", rpcMessage(err))
		return
	}
	s.redirect(w, r, "/applications/"+appID, "settings saved", "")
}

func jsonString(v string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	group := r.PostFormValue("group")
	back := r.PostFormValue("back")
	if back == "" || !strings.HasPrefix(back, "/") {
		back = "/"
	}
	client := frameworkv1.NewSettingsServiceClient(s.Conn)
	current, err := client.GetSettings(ctx, &frameworkv1.GetSettingsRequest{})
	if err != nil {
		s.redirect(w, r, back, "", err.Error())
		return
	}
	expected, _ := strconv.ParseUint(r.PostFormValue("revision"), 10, 64)
	patch := proto.Clone(current.Values).(*frameworkv1.UserSettings)
	var fields []string
	switch group {
	case "display", "ring":
		resourceID := "display.segment.front"
		if group == "ring" {
			resourceID = "light.ring.main"
		}
		if rid := r.PostFormValue("resource_id"); rid != "" {
			resourceID = rid
		}
		if patch.DefaultAppByResource == nil {
			patch.DefaultAppByResource = map[string]string{}
		}
		app := r.PostFormValue("default_app")
		if app == "" {
			delete(patch.DefaultAppByResource, resourceID)
		} else {
			patch.DefaultAppByResource[resourceID] = app
		}
		fields = append(fields, "default_app_by_resource")
		if v := r.PostFormValue("brightness"); v != "" {
			f, _ := strconv.ParseFloat(v, 64)
			if patch.DisplayBrightness == nil {
				patch.DisplayBrightness = map[string]float32{}
			}
			patch.DisplayBrightness[resourceID] = float32(f / 100)
			fields = append(fields, "display_brightness")
		}
		if mode := r.PostFormValue("brightness_mode"); mode != "" {
			if patch.BrightnessMode == nil {
				patch.BrightnessMode = map[string]string{}
			}
			patch.BrightnessMode[resourceID] = mode
			fields = append(fields, "brightness_mode")
		}
	case "assistant":
		patch.DefaultAssistantAppId = r.PostFormValue("default_assistant_app")
		patch.WakewordModelId = r.PostFormValue("wakeword_model")
		patch.WakewordEnabled = r.PostFormValue("wakeword_enabled") == "on"
		fields = append(fields, "default_assistant_app_id", "wakeword_model_id", "wakeword_enabled")
		switch r.PostFormValue("touch_hold_mode") {
		case "summon":
			patch.TouchHoldMode = frameworkv1.TouchHoldMode_TOUCH_HOLD_SUMMON
		case "talk":
			patch.TouchHoldMode = frameworkv1.TouchHoldMode_TOUCH_HOLD_TO_TALK
		}
		fields = append(fields, "touch_hold_mode")
	case "microphone":
		patch.MicrophoneMuted = r.PostFormValue("microphone_muted") == "on"
		switch r.PostFormValue("indicator_mode") {
		case "immediate":
			patch.MutedMicrophoneIndicator = &frameworkv1.MutedMicrophoneIndicator{Mode: frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_IMMEDIATE}
		case "never":
			patch.MutedMicrophoneIndicator = &frameworkv1.MutedMicrophoneIndicator{Mode: frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_UNTIL_UNMUTED}
		default:
			delay, _ := strconv.ParseUint(r.PostFormValue("indicator_delay_s"), 10, 32)
			patch.MutedMicrophoneIndicator = &frameworkv1.MutedMicrophoneIndicator{
				Mode: frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_DELAY, DelayMs: uint32(delay) * 1000,
			}
		}
		fields = append(fields, "microphone_muted", "muted_microphone_indicator")
	case "volume":
		v, _ := strconv.ParseFloat(r.PostFormValue("master_volume"), 64)
		patch.MasterVolume = float32(v / 100)
		fields = append(fields, "master_volume")
	case "device":
		v, _ := strconv.ParseFloat(r.PostFormValue("master_volume"), 64)
		patch.MasterVolume = float32(v / 100)
		patch.Timezone = strings.TrimSpace(r.PostFormValue("timezone"))
		fields = append(fields, "master_volume", "timezone")
	default:
		s.redirect(w, r, back, "", "unknown settings group")
		return
	}
	_, err = client.UpdateSettings(ctx, &frameworkv1.UpdateSettingsRequest{
		ExpectedRevision: expected, Values: patch, UpdateFields: fields,
	})
	if err != nil {
		s.redirect(w, r, back, "", err.Error())
		return
	}
	s.redirect(w, r, back, "settings saved", "")
}

func (s *Server) logsTail(ctx context.Context, appID string, limit int) []logbus.Entry {
	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	stream, err := frameworkv1.NewDiagnosticsServiceClient(s.Conn).WatchLogs(wctx, &frameworkv1.WatchLogsRequest{AppId: appID})
	if err != nil {
		return nil
	}
	var out []logbus.Entry
	for {
		rec, err := stream.Recv()
		if err != nil {
			break
		}
		out = append(out, logbus.Entry{
			TimeNS: rec.MonotonicTimeNs, Component: rec.Component, AppID: rec.AppId,
			Severity: rec.Severity, Message: rec.Message,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	data := diagPageData{Routing: map[string]*frameworkv1.RoutingState{}}
	data.Info, _ = frameworkv1.NewDeviceServiceClient(s.Conn).GetInfo(ctx, &frameworkv1.GetInfoRequest{})
	if runs, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{}); err == nil {
		data.Runs = runs.Runs
	}
	res, _ := frameworkv1.NewDeviceServiceClient(s.Conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	if res != nil {
		data.Resources = res.Resources
	}
	rsvc := frameworkv1.NewRoutingServiceClient(s.Conn)
	for _, resource := range res.GetResources() {
		if state, err := rsvc.GetRouting(ctx, &frameworkv1.GetRoutingRequest{ResourceId: resource.ResourceId}); err == nil {
			data.Routing[resource.ResourceId] = state
		}
	}
	data.Logs = s.logsTail(ctx, "", 200)
	data.Audio, _ = frameworkv1.NewAudioServiceClient(s.Conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{})
	p := s.base(w, r, "Diagnostics", "diagnostics")
	p.Data = data
	s.render(w, "diagnostics", p)
}

func (s *Server) bluetoothToggle(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	on := r.PostFormValue("discoverable") == "1"
	if _, err := frameworkv1.NewSystemServiceClient(s.Conn).SetBluetoothDiscoverable(ctx,
		&frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: on}); err != nil {
		s.redirect(w, r, "/", "", err.Error())
		return
	}
	if on {
		s.redirect(w, r, "/", "Bluetooth advertising enabled", "")
		return
	}
	s.redirect(w, r, "/", "Bluetooth advertising disabled", "")
}

// bluetoothPairable toggles whether new devices may pair while the adapter is
// hidden. Paired devices can always reconnect while Bluetooth is powered.
func (s *Server) bluetoothPairable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	on := r.PostFormValue("pairable") == "1"
	if _, err := frameworkv1.NewSystemServiceClient(s.Conn).SetBluetoothPairable(ctx,
		&frameworkv1.SetBluetoothPairableRequest{Pairable: on}); err != nil {
		s.redirect(w, r, "/", "", err.Error())
		return
	}
	if on {
		s.redirect(w, r, "/", "Incoming pairing enabled", "")
		return
	}
	s.redirect(w, r, "/", "Incoming pairing disabled", "")
}

// bluetoothUnpair forgets one bond. This is deliberately Web-UI-only: the
// daemon rejects the RPC for supervised applications.
func (s *Server) bluetoothUnpair(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	address := strings.TrimSpace(r.PostFormValue("address"))
	if address == "" {
		s.redirect(w, r, "/", "", "device address is required")
		return
	}
	if _, err := frameworkv1.NewSystemServiceClient(s.Conn).UnpairDevice(ctx,
		&frameworkv1.UnpairDeviceRequest{Address: address}); err != nil {
		s.redirect(w, r, "/", "", err.Error())
		return
	}
	s.redirect(w, r, "/", "Device "+address+" unpaired", "")
}

func (s *Server) accessPage(w http.ResponseWriter, r *http.Request) {
	data := accessPageData{
		Status: s.Auth.Status([]string{s.Addr}),
		Keys:   s.Auth.ListAPIKeys(),
	}
	p := s.base(w, r, "Access", "access")
	p.Data = data
	s.render(w, "access", p)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	current := r.PostFormValue("current")
	next := r.PostFormValue("next")
	confirm := r.PostFormValue("confirm")
	if !s.Auth.VerifyPassword(current) {
		s.redirect(w, r, "/access", "", "current password is incorrect")
		return
	}
	if len(next) < 8 {
		s.redirect(w, r, "/access", "", "new password must be at least 8 characters")
		return
	}
	if next != confirm {
		s.redirect(w, r, "/access", "", "new passwords do not match")
		return
	}
	if err := s.Auth.SetPassword(next); err != nil {
		s.redirect(w, r, "/access", "", err.Error())
		return
	}
	// SetPassword invalidates every session. Reissue one for a browser that
	// just proved knowledge of both passwords; API-key clients need none.
	if authFrom(r.Context()).ViaSession {
		token, expiry, err := s.Auth.NewSession()
		if err == nil {
			http.SetCookie(w, &http.Cookie{
				Name: websecurity.SessionCookieName(), Value: token, Path: "/",
				HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: expiry,
			})
		}
	}
	s.redirect(w, r, "/access", "admin password changed; other sessions were revoked", "")
}

// createAPIKey issues one static key and shows the value once, on the redirect
// target, because the store keeps only a hash.
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirect(w, r, "/access", "", "key name is required")
		return
	}
	value, _, err := s.Auth.CreateAPIKey(name)
	if err != nil {
		s.redirect(w, r, "/access", "", err.Error())
		return
	}
	data := accessPageData{
		Status: s.Auth.Status([]string{s.Addr}), Keys: s.Auth.ListAPIKeys(),
		NewKey: value, NewKeyName: name,
	}
	p := s.base(w, r, "Access", "access")
	p.Message = "API key created: copy it now, it is not shown again"
	p.Data = data
	s.render(w, "access", p)
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirect(w, r, "/access", "", "key name is required")
		return
	}
	if err := s.Auth.RevokeAPIKey(name); err != nil {
		s.redirect(w, r, "/access", "", err.Error())
		return
	}
	s.redirect(w, r, "/access", "API key revoked", "")
}

// certPEM serves the public server certificate for TLS clients to trust.
func (s *Server) certPEM(w http.ResponseWriter, r *http.Request) {
	pem, err := os.ReadFile(s.Auth.CertPath())
	if err != nil {
		http.Error(w, "certificate unavailable: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="capsula-server.crt"`)
	_, _ = w.Write(pem)
}

// apiStatus is a small authenticated JSON surface demonstrating static API-key
// access for automation.
func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	info, err := frameworkv1.NewDeviceServiceClient(s.Conn).GetInfo(ctx, &frameworkv1.GetInfoRequest{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resources, _ := frameworkv1.NewDeviceServiceClient(s.Conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	runs, _ := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{})
	status, _ := frameworkv1.NewSystemServiceClient(s.Conn).GetStatus(ctx, &frameworkv1.GetStatusRequest{})
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"device": info, "resources": resources.GetResources(),
		"runs": runs.GetRuns(), "status": status,
	})
}

// --- media -----------------------------------------------------------------

func (s *Server) mediaAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	appID := r.PostFormValue("app_id")
	playerID := r.PostFormValue("player_id")
	action := r.PostFormValue("action")
	req, _ := proto.Marshal(&mediav1.PlayerRequest{PlayerId: playerID})
	rpc := map[string]string{"play": "Play", "pause": "Pause", "next": "SkipNext", "previous": "SkipPrevious"}[action]
	if rpc == "" {
		s.redirect(w, r, "/", "", "unknown media action")
		return
	}
	_, _, end, err := s.routedCall(ctx, appID, "capsula.contracts.media.v1.MediaPlayerService", rpc, req)
	if err != nil {
		s.redirect(w, r, "/", "", err.Error())
		return
	}
	if end != nil && end.GrpcStatusCode != 0 {
		s.redirect(w, r, "/", "", fmt.Sprintf("%s: %s", codes.Code(end.GrpcStatusCode), end.Detail))
		return
	}
	s.redirect(w, r, "/", "command accepted", "")
}

func (s *Server) listPlayers(ctx context.Context, appID string) ([]*mediav1.PlayerStatus, error) {
	req, _ := proto.Marshal(&mediav1.ListPlayersRequest{})
	msgs, _, end, err := s.routedCall(ctx, appID, "capsula.contracts.media.v1.MediaPlayerService", "ListPlayers", req)
	if err != nil {
		return nil, err
	}
	if end != nil && end.GrpcStatusCode != 0 {
		return nil, fmt.Errorf("%s: %s", codes.Code(end.GrpcStatusCode), end.Detail)
	}
	var out []*mediav1.PlayerStatus
	for _, raw := range msgs {
		var resp mediav1.ListPlayersResponse
		if err := proto.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Players...)
	}
	return out, nil
}

// routedCall performs one unary app-to-app call through the framework bus.
func (s *Server) routedCall(ctx context.Context, appID, service, rpc string, request []byte) ([][]byte, string, *frameworkv1.AppCallEnd, error) {
	stream, err := frameworkv1.NewAppBusServiceClient(s.Conn).Call(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{TargetAppId: appID, GrpcService: service, RpcName: rpc, TimeoutMs: 5000},
	}}); err != nil {
		return nil, "", nil, err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{RequestMessage: request}}); err != nil {
		return nil, "", nil, err
	}
	_ = stream.CloseSend()
	var out [][]byte
	var end *frameworkv1.AppCallEnd
	callID := ""
	for {
		resp, err := stream.Recv()
		if err != nil {
			return out, callID, end, err
		}
		switch m := resp.Message.(type) {
		case *frameworkv1.RoutedCallResponse_ResponseMessage:
			out = append(out, m.ResponseMessage)
		case *frameworkv1.RoutedCallResponse_End:
			end = m.End
			return out, callID, end, nil
		}
	}
}

func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
