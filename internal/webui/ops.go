package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	mediav1 "capsulad/gen/go/capsula/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	webv1 "capsulad/gen/go/capsula/web/v1"
	"capsulad/internal/uichanges"
	"capsulad/internal/version"
)

const (
	errInvalid   = webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
	errNotFound  = webv1.ErrorCode_ERROR_CODE_NOT_FOUND
	errConflict  = webv1.ErrorCode_ERROR_CODE_CONFLICT
	errForbidden = webv1.ErrorCode_ERROR_CODE_FORBIDDEN
	errInternal  = webv1.ErrorCode_ERROR_CODE_INTERNAL
)

const (
	maxAppSettingChanges    = 64
	maxAppSettingKeyBytes   = 128
	maxAppSettingValueBytes = 8 << 10
)

// browserOperationVocabulary is the closed set of Command oneof fields this
// server understands. A test compares it with the proto descriptor, and the
// set contains no capture, audio, asset, presentation, ForceRelease or generic
// AppBus operation.
var browserOperationVocabulary = []string{
	"settings", "launch", "stop", "action", "app_settings", "media",
	"bluetooth", "record_sample", "delete_sample", "train",
	"cancel_training", "clock", "create_api_key", "revoke_api_key", "wifi",
}

// dispatch is an explicit allowlist: each typed command maps to one fixed
// daemon call. There is no caller-supplied service name, RPC name, or raw
// protobuf payload, and no capture/audio/asset/generic AppBus operation.
func (s *Server) dispatch(c *wsConn, reqID uint64, cmd *webv1.Command, ctx context.Context) {
	if cmd == nil || cmd.GetOp() == nil {
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "empty command")
		return
	}
	switch op := cmd.GetOp().(type) {
	case *webv1.Command_Settings:
		s.opSettings(c, reqID, op.Settings)
	case *webv1.Command_Launch:
		s.opLaunch(c, reqID, op.Launch)
	case *webv1.Command_Stop:
		s.opStop(c, reqID, op.Stop)
	case *webv1.Command_Action:
		s.opAction(c, reqID, op.Action)
	case *webv1.Command_AppSettings:
		s.opAppSettings(c, reqID, op.AppSettings)
	case *webv1.Command_Media:
		s.opMedia(c, reqID, op.Media)
	case *webv1.Command_Bluetooth:
		s.opBluetooth(c, reqID, op.Bluetooth)
	case *webv1.Command_RecordSample:
		s.opRecordSample(c, reqID, op.RecordSample, ctx)
	case *webv1.Command_DeleteSample:
		s.opDeleteSample(c, reqID, op.DeleteSample)
	case *webv1.Command_Train:
		s.opTrain(c, reqID, op.Train)
	case *webv1.Command_CancelTraining:
		s.opCancelTraining(c, reqID, op.CancelTraining)
	case *webv1.Command_Clock:
		s.opClock(c, reqID, op.Clock)
	case *webv1.Command_CreateApiKey:
		s.opCreateKey(c, reqID, op.CreateApiKey)
	case *webv1.Command_RevokeApiKey:
		s.opRevokeKey(c, reqID, op.RevokeApiKey)
	case *webv1.Command_Wifi:
		s.opWifi(c, reqID, op.Wifi)
	default:
		c.sendError(reqID, webv1.ErrorCode_ERROR_CODE_FORBIDDEN, "operation is not in the browser allowlist")
	}
}

func (s *Server) daemonVersion() string { return versionString() }

func (s *Server) opSettings(c *wsConn, reqID uint64, upd *webv1.SettingsUpdate) {
	if upd == nil || upd.GetGroup() == nil {
		c.sendError(reqID, errInvalid, "settings group is required")
		return
	}
	// The store treats expected_revision 0 as unconditional, but every real
	// store starts at revision 1. A browser without a snapshot revision would
	// otherwise silently overwrite another session's change.
	if upd.GetExpectedRevision() == 0 {
		c.sendError(reqID, errInvalid, "settings revision is required; refresh the settings and try again")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := frameworkv1.NewSettingsServiceClient(s.Conn)
	current, err := client.GetSettings(ctx, &frameworkv1.GetSettingsRequest{})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	patch := protoCloneSettings(current.GetValues())
	var fields []string
	message, warning := "settings saved", ""
	hostnameSubmitted := false

	switch group := upd.GetGroup().(type) {
	case *webv1.SettingsUpdate_Display:
		resourceID := strings.TrimSpace(group.Display.GetResourceId())
		if err := s.validOutputResource(ctx, resourceID); err != nil {
			c.sendError(reqID, classifyError(err), rpcMessage(err))
			return
		}
		mask := fieldMask(group.Display.GetUpdateFields())
		if ownsField(mask, "default_app") {
			if patch.DefaultAppByResource == nil {
				patch.DefaultAppByResource = map[string]string{}
			}
			app := strings.TrimSpace(group.Display.GetDefaultApp())
			if app == "" {
				delete(patch.DefaultAppByResource, resourceID)
			} else {
				if err := s.validApp(ctx, app); err != nil {
					c.sendError(reqID, classifyError(err), rpcMessage(err))
					return
				}
				patch.DefaultAppByResource[resourceID] = app
			}
			fields = append(fields, "default_app_by_resource")
		}
		if ownsField(mask, "brightness") {
			if patch.DisplayBrightness == nil {
				patch.DisplayBrightness = map[string]float32{}
			}
			brightness := group.Display.GetBrightness()
			if brightness < 0 {
				brightness = 0
			}
			if brightness > 1 {
				brightness = 1
			}
			patch.DisplayBrightness[resourceID] = brightness
			fields = append(fields, "display_brightness")
		}
		if ownsField(mask, "brightness_mode") {
			if patch.BrightnessMode == nil {
				patch.BrightnessMode = map[string]string{}
			}
			switch group.Display.GetBrightnessMode() {
			case webv1.BrightnessMode_BRIGHTNESS_MODE_AUTO:
				patch.BrightnessMode[resourceID] = "auto"
			default:
				patch.BrightnessMode[resourceID] = "const"
			}
			fields = append(fields, "brightness_mode")
		}
	case *webv1.SettingsUpdate_Assistant:
		mask := fieldMask(group.Assistant.GetUpdateFields())
		if ownsField(mask, "default_assistant_app") {
			app := strings.TrimSpace(group.Assistant.GetDefaultAssistantApp())
			if app != "" {
				if err := s.validApp(ctx, app); err != nil {
					c.sendError(reqID, classifyError(err), rpcMessage(err))
					return
				}
			}
			patch.DefaultAssistantAppId = app
			fields = append(fields, "default_assistant_app_id")
		}
		if ownsField(mask, "wakeword_model") {
			model := strings.TrimSpace(group.Assistant.GetWakewordModel())
			if model != "" {
				if err := s.validModel(ctx, model); err != nil {
					c.sendError(reqID, classifyError(err), rpcMessage(err))
					return
				}
			}
			patch.WakewordModelId = model
			fields = append(fields, "wakeword_model_id")
		}
		if ownsField(mask, "wakeword_enabled") {
			patch.WakewordEnabled = group.Assistant.GetWakewordEnabled()
			fields = append(fields, "wakeword_enabled")
		}
		if ownsField(mask, "touch_hold_mode") {
			switch group.Assistant.GetTouchHold() {
			case webv1.TouchBehavior_TOUCH_BEHAVIOR_TALK:
				patch.TouchHoldMode = frameworkv1.TouchHoldMode_TOUCH_HOLD_TO_TALK
			default:
				patch.TouchHoldMode = frameworkv1.TouchHoldMode_TOUCH_HOLD_SUMMON
			}
			fields = append(fields, "touch_hold_mode")
		}
	case *webv1.SettingsUpdate_Microphone:
		patch.MicrophoneMuted = group.Microphone.GetMuted()
		fields = append(fields, "microphone_muted")
	case *webv1.SettingsUpdate_MutedIndicator:
		ind := group.MutedIndicator
		currentIndicator := current.GetValues().GetMutedMicrophoneIndicator()
		delay := currentIndicator.GetDelayMs()
		if ind.GetHasDelay() {
			if ind.GetDelayS() < 1 || ind.GetDelayS() > 3600 {
				c.sendError(reqID, errInvalid, "indicator delay must be between 1 and 3600 seconds")
				return
			}
			delay = ind.GetDelayS() * 1000
		}
		if delay == 0 {
			delay = 30000
		}
		mode := frameworkv1.MutedIndicatorMode(currentIndicator.GetMode())
		if ind.GetHasMode() {
			switch ind.GetMode() {
			case webv1.MutedIndicatorMode_MUTED_INDICATOR_MODE_IMMEDIATE:
				mode = frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_IMMEDIATE
			case webv1.MutedIndicatorMode_MUTED_INDICATOR_MODE_UNTIL_UNMUTED:
				mode = frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_UNTIL_UNMUTED
			default:
				mode = frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_DELAY
			}
		}
		switch mode {
		case frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_IMMEDIATE:
			patch.MutedMicrophoneIndicator = &frameworkv1.MutedMicrophoneIndicator{Mode: mode, DelayMs: delay}
		case frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_UNTIL_UNMUTED:
			patch.MutedMicrophoneIndicator = &frameworkv1.MutedMicrophoneIndicator{Mode: mode, DelayMs: delay}
		default:
			patch.MutedMicrophoneIndicator = &frameworkv1.MutedMicrophoneIndicator{Mode: frameworkv1.MutedIndicatorMode_MUTED_INDICATOR_DELAY, DelayMs: delay}
		}
		fields = append(fields, "muted_microphone_indicator")
	case *webv1.SettingsUpdate_Volume:
		patch.MasterVolume = clamp01(group.Volume.GetMasterVolume())
		fields = append(fields, "master_volume")
	case *webv1.SettingsUpdate_StartupSound:
		patch.StartupSoundEnabled = group.StartupSound.GetEnabled()
		fields = append(fields, "startup_sound_enabled")
	case *webv1.SettingsUpdate_Timezone:
		timezone := strings.TrimSpace(group.Timezone.GetTimezone())
		if len(timezone) > 64 {
			c.sendError(reqID, errInvalid, "timezone name is too long")
			return
		}
		patch.Timezone = timezone
		fields = append(fields, "timezone")
	case *webv1.SettingsUpdate_Hostname:
		hostname := strings.TrimSpace(group.Hostname.GetHostname())
		if len(hostname) > 63 {
			c.sendError(reqID, errInvalid, "hostname must be a single DNS label of at most 63 characters")
			return
		}
		patch.Hostname = hostname
		fields = append(fields, "hostname")
		hostnameSubmitted = true
	default:
		c.sendError(reqID, errInvalid, "unknown settings group")
		return
	}

	saved, err := client.UpdateSettings(ctx, &frameworkv1.UpdateSettingsRequest{
		ExpectedRevision: upd.GetExpectedRevision(), Values: patch, UpdateFields: fields,
	})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	if hostnameSubmitted {
		message, warning = s.applyHostname(ctx, saved.GetValues().GetHostname())
	}
	view, err := s.settingsView(ctx)
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Settings{Settings: &webv1.SettingsUpdateResult{
		Revision: saved.GetRevision(), Message: message, Warning: warning,
	}}})
	c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_SETTINGS, Body: &webv1.Event_Settings{Settings: view}})
}

func (s *Server) opLaunch(c *wsConn, reqID uint64, launch *webv1.LaunchApp) {
	appID := strings.TrimSpace(launch.GetAppId())
	if appID == "" {
		c.sendError(reqID, errInvalid, "app id is required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	app, err := s.appByID(ctx, appID)
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	declared := map[string]bool{}
	for _, a := range app.GetLaunchArguments() {
		declared[a.GetName()] = true
	}
	var args []*frameworkv1.LaunchArgument
	for _, a := range launch.GetArguments() {
		name := strings.TrimSpace(a.GetName())
		if name == "" || !declared[name] {
			c.sendError(reqID, errInvalid, "launch argument is not declared by this application")
			return
		}
		args = append(args, &frameworkv1.LaunchArgument{Name: name, Value: a.GetValue()})
	}
	res, err := frameworkv1.NewApplicationServiceClient(s.Conn).Launch(ctx, &frameworkv1.LaunchRequest{AppId: appID, Arguments: args})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Launch{Launch: &webv1.LaunchResult{
		RunId: res.GetRunId(), InvocationId: res.GetInvocationId(), SpawnedNewProcess: res.GetSpawnedNewProcess(),
	}}})
}

func (s *Server) opStop(c *wsConn, reqID uint64, stop *webv1.StopRun) {
	appID, runID := strings.TrimSpace(stop.GetAppId()), strings.TrimSpace(stop.GetRunId())
	if appID == "" || runID == "" {
		c.sendError(reqID, errInvalid, "app id and run id are required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	selected := false
	if settings, err := frameworkv1.NewSettingsServiceClient(s.Conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{}); err == nil {
		selected = isSelectedProvider(settings.GetValues(), appID)
	}
	res, err := frameworkv1.NewApplicationServiceClient(s.Conn).Stop(ctx, &frameworkv1.StopRequest{RunId: runID})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	message := "stop requested"
	if !res.GetRequested() {
		c.sendError(reqID, errConflict, "stop was not accepted")
		return
	}
	if selected {
		message = "stop requested; the selected provider restarts immediately. Choose none or another provider in Settings to keep it stopped."
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Stop{Stop: &webv1.StopResult{
		Requested: true, SelectedDefault: selected, Message: message,
	}}})
}

func (s *Server) opAction(c *wsConn, reqID uint64, action *webv1.TriggerAppAction) {
	appID, actionID := strings.TrimSpace(action.GetAppId()), strings.TrimSpace(action.GetActionId())
	if appID == "" || actionID == "" {
		c.sendError(reqID, errInvalid, "app id and action id are required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	res, err := frameworkv1.NewApplicationConfigurationServiceClient(s.Conn).TriggerAction(ctx,
		&frameworkv1.TriggerAppActionRequest{AppId: appID, ActionId: actionID})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Action{Action: &webv1.ActionResult{
		InvocationId: res.GetActionInvocationId(),
	}}})
}

func (s *Server) opAppSettings(c *wsConn, reqID uint64, upd *webv1.AppSettingsUpdate) {
	appID := strings.TrimSpace(upd.GetAppId())
	if appID == "" {
		c.sendError(reqID, errInvalid, "app id is required")
		return
	}
	if len(upd.GetChanges()) == 0 {
		c.sendError(reqID, errInvalid, "at least one setting change is required")
		return
	}
	if upd.GetExpectedRevision() == 0 {
		c.sendError(reqID, errInvalid, "app settings revision is required; refresh the application and try again")
		return
	}
	if len(upd.GetChanges()) > maxAppSettingChanges {
		c.sendError(reqID, errInvalid, "too many setting changes in one request")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cfg := frameworkv1.NewApplicationConfigurationServiceClient(s.Conn)
	schema, err := cfg.Describe(ctx, &frameworkv1.DescribeAppConfigurationRequest{AppId: appID})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	declared := map[string]frameworkv1.SettingType{}
	for _, def := range schema.GetSettings() {
		declared[def.GetKey()] = def.GetType()
	}
	var changes []*frameworkv1.AppSettingChange
	for _, change := range upd.GetChanges() {
		key := change.GetKey()
		if _, ok := declared[key]; !ok {
			c.sendError(reqID, errInvalid, fmt.Sprintf("%q is not declared by this application", key))
			return
		}
		if len(key) > maxAppSettingKeyBytes || len(change.GetValueJson()) > maxAppSettingValueBytes {
			c.sendError(reqID, errInvalid, "setting change exceeds the size limit")
			return
		}
		if change.GetClear() == (change.GetValueJson() != "") {
			// Exactly one of clear=true or a non-empty JSON value is allowed.
			c.sendError(reqID, errInvalid, "a setting change needs exactly one of clear or value_json")
			return
		}
		changes = append(changes, &frameworkv1.AppSettingChange{
			Key: key, ValueJson: change.GetValueJson(), Clear: change.GetClear(),
		})
	}
	res, err := cfg.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{
		AppId: appID, ExpectedRevision: upd.GetExpectedRevision(), Changes: changes,
	})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	result := &webv1.AppSettingsUpdateResult{Revision: res.GetRevision(), Message: "settings saved"}
	for _, v := range res.GetValues() {
		result.Values = append(result.Values, &webv1.SettingStateView{
			Key: v.GetKey(), ValueJson: v.GetValueJson(), HasValue: v.GetHasValue(),
		})
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_AppSettings{AppSettings: result}})
}

func (s *Server) opMedia(c *wsConn, reqID uint64, media *webv1.MediaCommand) {
	appID, playerID := strings.TrimSpace(media.GetAppId()), strings.TrimSpace(media.GetPlayerId())
	if appID == "" || playerID == "" {
		c.sendError(reqID, errInvalid, "app id and player id are required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	app, err := s.appByID(ctx, appID)
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	if !providesMedia(app) {
		c.sendError(reqID, errForbidden, "application does not provide the media player contract")
		return
	}
	rpc := map[webv1.MediaAction]string{
		webv1.MediaAction_MEDIA_ACTION_PLAY:     "Play",
		webv1.MediaAction_MEDIA_ACTION_PAUSE:    "Pause",
		webv1.MediaAction_MEDIA_ACTION_NEXT:     "SkipNext",
		webv1.MediaAction_MEDIA_ACTION_PREVIOUS: "SkipPrevious",
	}[media.GetAction()]
	if rpc == "" {
		c.sendError(reqID, errInvalid, "unknown media action")
		return
	}
	body, err := protoMarshal(&mediav1.PlayerRequest{PlayerId: playerID})
	if err != nil {
		c.sendError(reqID, errInternal, err.Error())
		return
	}
	msgs, _, end, err := s.routedCall(ctx, appID, mediaService, rpc, body)
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	if end != nil && end.GetGrpcStatusCode() != 0 {
		c.sendError(reqID, classifyError(status.Error(codes.Code(end.GetGrpcStatusCode()), end.GetDetail())), rpcMessage(status.Error(codes.Code(end.GetGrpcStatusCode()), end.GetDetail())))
		return
	}
	result := &webv1.MediaResult{Accepted: len(msgs) > 0}
	for _, raw := range msgs {
		var resp mediav1.PlayerCommandResult
		if err := protoUnmarshal(raw, &resp); err != nil {
			continue
		}
		result.Accepted = result.Accepted && resp.GetAccepted()
		if resp.GetDetail() != "" {
			result.Detail = resp.GetDetail()
		}
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Media{Media: result}})
}

func (s *Server) opBluetooth(c *wsConn, reqID uint64, cmd *webv1.BluetoothCommand) {
	if cmd == nil || cmd.GetAction() == nil {
		c.sendError(reqID, errInvalid, "bluetooth action is required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sys := frameworkv1.NewSystemServiceClient(s.Conn)
	var message string
	var status *frameworkv1.BluetoothStatus
	switch action := cmd.GetAction().(type) {
	case *webv1.BluetoothCommand_SetDiscoverable:
		res, err := sys.SetBluetoothDiscoverable(ctx, &frameworkv1.SetBluetoothDiscoverableRequest{Discoverable: action.SetDiscoverable.GetDiscoverable()})
		if err != nil {
			c.sendError(reqID, classifyError(err), rpcMessage(err))
			return
		}
		status = res
		if action.SetDiscoverable.GetDiscoverable() {
			message = "Bluetooth discoverable enabled"
		} else {
			message = "Bluetooth discoverable disabled"
		}
	case *webv1.BluetoothCommand_SetIncomingConnections:
		res, err := sys.SetBluetoothIncomingConnections(ctx, &frameworkv1.SetBluetoothIncomingConnectionsRequest{
			IncomingConnections: action.SetIncomingConnections.GetIncomingConnections(),
		})
		if err != nil {
			c.sendError(reqID, classifyError(err), rpcMessage(err))
			return
		}
		status = res
		if action.SetIncomingConnections.GetIncomingConnections() {
			message = "Bluetooth incoming connections enabled"
		} else {
			message = "Bluetooth incoming connections disabled"
		}
	case *webv1.BluetoothCommand_Unpair:
		address := strings.TrimSpace(action.Unpair.GetAddress())
		if address == "" {
			c.sendError(reqID, errInvalid, "device address is required")
			return
		}
		if _, err := sys.UnpairDevice(ctx, &frameworkv1.UnpairDeviceRequest{Address: address}); err != nil {
			c.sendError(reqID, classifyError(err), rpcMessage(err))
			return
		}
		message = "Device " + address + " unpaired"
	default:
		c.sendError(reqID, errInvalid, "unknown bluetooth action")
		return
	}
	if status == nil {
		// Unpair has no status response; read the observed adapter state so
		// the UI still reflects BlueZ truth after the mutation.
		if st, err := sys.GetBluetoothStatus(ctx, &frameworkv1.GetBluetoothStatusRequest{}); err == nil {
			status = st
		}
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Bluetooth{Bluetooth: &webv1.BluetoothResult{
		Message: message, Status: bluetoothView(status),
	}}})
	s.publish(uichanges.Bluetooth)
}

func (s *Server) opRecordSample(c *wsConn, reqID uint64, record *webv1.RecordSample, parent context.Context) {
	label := strings.TrimSpace(record.GetLabel())
	switch label {
	case "word", "speech", "room", "background":
	default:
		c.sendError(reqID, errInvalid, "sample label must be word, speech, room or background")
		return
	}
	duration := record.GetDurationMs()
	if duration == 0 {
		duration = 1600
	}
	if duration > 20000 {
		c.sendError(reqID, errInvalid, "sample duration exceeds the device limit")
		return
	}
	// Recording is device-side and may legitimately take the whole take plus
	// cue time. The browser never receives audio; only the resulting sample.
	// The command context is canceled by CancelOperation (modal dismissal) or
	// by socket close, which ends the device-side capture instead of leaking it.
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	resp, err := frameworkv1.NewAssistantServiceClient(s.Conn).RecordWakewordSample(ctx,
		&frameworkv1.RecordWakewordSampleRequest{Label: label, DurationMs: duration})
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Sample{Sample: &webv1.SampleResult{
		Sample: sampleView(resp.GetSample()),
	}}})
}

func (s *Server) opDeleteSample(c *wsConn, reqID uint64, del *webv1.DeleteSample) {
	id := strings.TrimSpace(del.GetSampleId())
	if id == "" {
		c.sendError(reqID, errInvalid, "sample id is required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := frameworkv1.NewAssistantServiceClient(s.Conn).DeleteWakewordSample(ctx,
		&frameworkv1.DeleteWakewordSampleRequest{SampleId: id}); err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Deleted{Deleted: &webv1.Empty{}}})
}

func (s *Server) opTrain(c *wsConn, reqID uint64, train *webv1.TrainWakeword) {
	word := strings.TrimSpace(train.GetDisplayName())
	if word == "" {
		c.sendError(reqID, errInvalid, "name the wake word before training")
		return
	}
	language := strings.TrimSpace(train.GetLanguage())
	if language == "" {
		language = "ru"
	}
	c.trainingMu.Lock()
	if len(c.training) >= 1 {
		c.trainingMu.Unlock()
		c.sendError(reqID, errConflict, "a training run is already active")
		return
	}
	trainingID := newTrainingID()
	ctx, cancel := context.WithCancel(context.Background())
	c.training[trainingID] = cancel
	c.trainingMu.Unlock()

	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Training{Training: &webv1.TrainingHandle{TrainingId: trainingID}}})

	go func() {
		defer func() {
			cancel()
			c.trainingMu.Lock()
			delete(c.training, trainingID)
			c.trainingMu.Unlock()
		}()
		stream, err := frameworkv1.NewAssistantServiceClient(c.srv.Conn).TrainWakewordClassifier(ctx, &frameworkv1.TrainWakewordClassifierRequest{
			DisplayName: word, Language: language, Seed: train.GetSeed(),
			WordSampleIds: train.GetWordSampleIds(), RoomSampleIds: train.GetRoomSampleIds(),
			SpeechSampleIds: train.GetSpeechSampleIds(), FalseSampleIds: train.GetFalseSampleIds(),
		})
		if err != nil {
			c.sendProgress(reqID, &webv1.Progress{TrainingId: trainingID, Stage: "error", Log: trainErrorMessage(err)})
			return
		}
		lastStage := ""
		for {
			ev, err := stream.Recv()
			if err != nil {
				switch {
				case lastStage == "installed":
					// The final installation event was observed; the run
					// succeeded. A trailing cancel must not report a failure.
				case precommitTrainingCancel(err):
					// The only authoritative proof that the model file was not
					// replaced: the daemon returned its exact precommit
					// cancellation status (writeClassifier removed its temp
					// file before the rename).
					c.sendProgress(reqID, &webv1.Progress{TrainingId: trainingID, Stage: "error", Log: msgTrainingCancelledUnchanged})
				case trainingInstallMayHaveStarted(lastStage):
					// Late progress was observed; a commit may have won.
					c.sendProgress(reqID, &webv1.Progress{TrainingId: trainingID, Stage: "error", Log: msgTrainingResultUnknown})
				case ctx.Err() != nil:
					// Local/client cancellation at ANY observed stage. Last
					// observed progress is not proof: gRPC buffers events and
					// sendProgress can lag behind the daemon, so install may
					// already have run.
					c.sendProgress(reqID, &webv1.Progress{TrainingId: trainingID, Stage: "error", Log: msgTrainingResultUnknown})
				case errors.Is(err, io.EOF):
					// The daemon returns nil after its final `installed` event.
					// Never synthesize success or claim the model is unchanged.
					c.sendProgress(reqID, &webv1.Progress{TrainingId: trainingID, Stage: "error", Log: msgTrainingEndedWithoutInstall})
				case trainingStreamInterrupted(err):
					// Generic Canceled/DeadlineExceeded without the explicit
					// precommit status can also be a final installed send
					// failing after a committed install.
					c.sendProgress(reqID, &webv1.Progress{TrainingId: trainingID, Stage: "error", Log: msgTrainingResultUnknown})
				default:
					c.sendProgress(reqID, &webv1.Progress{TrainingId: trainingID, Stage: "error", Log: trainErrorMessage(err)})
				}
				return
			}
			lastStage = ev.GetStage()
			p := &webv1.Progress{TrainingId: trainingID, Stage: ev.GetStage(), Fraction: ev.GetFraction(), Log: ev.GetLog()}
			if cl := ev.GetClassifier(); cl != nil {
				p.Classifier = &webv1.WakewordClassifierView{
					ModelId: cl.GetModelId(), DisplayName: cl.GetDisplayName(), Language: cl.GetLanguage(),
					Threshold: cl.GetThreshold(), ContextFrames: cl.GetContextFrames(), EmbeddingDim: cl.GetEmbeddingDim(),
					ProjectionWidth: cl.GetProjectionWidth(), HiddenWidth: cl.GetHiddenWidth(),
				}
			}
			c.sendProgress(reqID, p)
		}
	}()
}

const (
	msgTrainingCancelledUnchanged  = "training cancelled; the installed model is unchanged"
	msgTrainingResultUnknown       = "training ended before the result was confirmed; the new model may already be installed — refresh the wakeword models to check"
	msgTrainingEndedWithoutInstall = "training ended before installation was confirmed; refresh the wakeword models to check"
)

// trainingInstallMayHaveStarted reports whether an observed stage is close
// enough to the atomic commit that a cancelled or failed stream cannot prove
// the previous model is still installed. `installed` itself is handled as
// success before this check.
func trainingInstallMayHaveStarted(stage string) bool {
	switch stage {
	case "calibrate", "finaltest", "install":
		return true
	default:
		return false
	}
}

// precommitTrainingCancel recognizes the daemon's explicit precommit
// cancellation status. A generic Canceled is not equivalent: the final
// `installed` send can fail after the rename committed and also surface as a
// cancelled stream.
func precommitTrainingCancel(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.Canceled && st.Message() == "training cancelled"
}

// trainingStreamInterrupted reports a Canceled/DeadlineExceeded stream end
// whether it arrived as a context value or as a gRPC status. Neither proves
// the daemon's atomic commit did not already run.
func trainingStreamInterrupted(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	st, ok := status.FromError(err)
	return ok && (st.Code() == codes.Canceled || st.Code() == codes.DeadlineExceeded)
}

func (s *Server) opCancelTraining(c *wsConn, reqID uint64, cancelReq *webv1.CancelTraining) {
	id := strings.TrimSpace(cancelReq.GetTrainingId())
	if id == "" {
		c.sendError(reqID, errInvalid, "training id is required")
		return
	}
	c.trainingMu.Lock()
	cancel := c.training[id]
	delete(c.training, id)
	c.trainingMu.Unlock()
	if cancel == nil {
		c.sendError(reqID, errNotFound, "training run is not active")
		return
	}
	cancel()
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Deleted{Deleted: &webv1.Empty{}}})
}

func (s *Server) opClock(c *wsConn, reqID uint64, cmd *webv1.ClockCommand) {
	appID := strings.TrimSpace(cmd.GetAppId())
	if appID != clockAppID {
		c.sendError(reqID, errForbidden, "clock controls are only available for the built-in clock app")
		return
	}
	if cmd.GetAction() == nil {
		c.sendError(reqID, errInvalid, "clock action is required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var rpc string
	// The public clock calls take binary protobuf messages; the stopwatch RPCs
	// take an empty one, and each other action replaces this value.
	var request proto.Message = &clockv1.StopwatchCommandRequest{}
	message := "clock updated"
	switch action := cmd.GetAction().(type) {
	case *webv1.ClockCommand_StopwatchStart:
		rpc, message = "StopwatchStart", "stopwatch started"
	case *webv1.ClockCommand_StopwatchPause:
		rpc, message = "StopwatchPause", "stopwatch paused"
	case *webv1.ClockCommand_StopwatchResume:
		rpc, message = "StopwatchResume", "stopwatch resumed"
	case *webv1.ClockCommand_StopwatchReset:
		rpc, message = "StopwatchReset", "stopwatch reset"
	case *webv1.ClockCommand_StartTimer:
		ms := action.StartTimer.GetDurationMs()
		if ms < 1000 || ms > 2592000000 {
			c.sendError(reqID, errInvalid, "timer duration must be between 1 and 2592000 seconds")
			return
		}
		rpc, message = "StartTimer", "timer started"
		request = &clockv1.StartTimerRequest{DurationMs: ms, Label: strings.TrimSpace(action.StartTimer.GetLabel())}
	case *webv1.ClockCommand_CancelTimer:
		id := strings.TrimSpace(action.CancelTimer.GetTimerId())
		if id == "" {
			c.sendError(reqID, errInvalid, "timer id is required")
			return
		}
		rpc, message = "CancelTimer", "timer cancelled"
		request = &clockv1.CancelTimerRequest{TimerId: id}
	case *webv1.ClockCommand_SetAlarm:
		a := action.SetAlarm
		if a.GetHour() > 23 || a.GetMinute() > 59 {
			c.sendError(reqID, errInvalid, "alarm hour must be 0-23 and minute 0-59")
			return
		}
		schedule := clockScheduleFromMask(a.GetHour(), a.GetMinute(), a.GetDaysOfWeek())
		if previous := strings.TrimSpace(a.GetAlarmId()); previous != "" && previous != schedule {
			// The legacy form could edit one alarm in place; the canonical
			// alarm identity is its schedule, so retire the old expression.
			_, _ = s.clockCall(ctx, appID, "DeleteAlarm", &clockv1.DeleteAlarmRequest{AlarmId: previous})
		}
		rpc, message = "AddAlarm", "alarm saved"
		request = &clockv1.AddAlarmRequest{
			Schedule: schedule, Timezone: strings.TrimSpace(a.GetTimezone()),
			Label: strings.TrimSpace(a.GetLabel()), Disabled: !a.GetEnabled(),
		}
	case *webv1.ClockCommand_SetAlarmEnabled:
		id := strings.TrimSpace(action.SetAlarmEnabled.GetAlarmId())
		if id == "" {
			c.sendError(reqID, errInvalid, "alarm id is required")
			return
		}
		rpc = "SetAlarmEnabled"
		if action.SetAlarmEnabled.GetEnabled() {
			message = "alarm enabled"
		} else {
			message = "alarm disabled"
		}
		request = &clockv1.SetAlarmEnabledRequest{AlarmId: id, Enabled: action.SetAlarmEnabled.GetEnabled()}
	case *webv1.ClockCommand_DeleteAlarm:
		id := strings.TrimSpace(action.DeleteAlarm.GetAlarmId())
		if id == "" {
			c.sendError(reqID, errInvalid, "alarm id is required")
			return
		}
		rpc, message = "DeleteAlarm", "alarm deleted"
		request = &clockv1.DeleteAlarmRequest{AlarmId: id}
	case *webv1.ClockCommand_DismissAlert:
		id := strings.TrimSpace(action.DismissAlert.GetAlertId())
		if id == "" {
			c.sendError(reqID, errInvalid, "alert id is required")
			return
		}
		rpc, message = "DismissAlert", "alert dismissed"
		request = &clockv1.DismissAlertRequest{AlertId: id}
	default:
		c.sendError(reqID, errInvalid, "unknown clock action")
		return
	}
	state, err := s.clockCall(ctx, appID, rpc, request)
	if err != nil {
		c.sendError(reqID, classifyError(err), rpcMessage(err))
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Clock{Clock: &webv1.ClockCommandResult{
		Clock: clockView(state), Message: message,
	}}})
}

// clockScheduleFromMask renders one five-field dcron expression from the Web
// UI's legacy weekday bitmask (bit 0 Monday through bit 6 Sunday). Zero means
// every day.
func clockScheduleFromMask(hour, minute, days uint32) string {
	if days&0x7f == 0 {
		return fmt.Sprintf("%d %d * * *", minute, hour)
	}
	parts := make([]string, 0, 7)
	if days&(1<<6) != 0 {
		parts = append(parts, "0")
	}
	for bit := uint32(0); bit < 6; bit++ {
		if days&(1<<bit) != 0 {
			parts = append(parts, fmt.Sprintf("%d", bit+1))
		}
	}
	return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(parts, ","))
}

func (s *Server) opCreateKey(c *wsConn, reqID uint64, create *webv1.CreateApiKey) {
	name := strings.TrimSpace(create.GetName())
	if name == "" {
		c.sendError(reqID, errInvalid, "key name is required")
		return
	}
	value, _, err := s.Auth.CreateAPIKey(name)
	if err != nil {
		// The store reports client-actionable validation as plain errors. Keep
		// them distinguishable from a persistence failure instead of letting
		// every rejection look like an internal fault.
		message := err.Error()
		switch {
		case strings.HasPrefix(message, "key name must match"):
			c.sendError(reqID, errInvalid, message)
		case strings.Contains(message, "already exists"), strings.HasPrefix(message, "maximum of"):
			c.sendError(reqID, errConflict, message)
		default:
			c.sendError(reqID, errInternal, message)
		}
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_ApiKey{ApiKey: &webv1.ApiKeyCreated{
		Name: name, Value: value,
	}}})
	s.publish(uichanges.Access)
}

func (s *Server) opRevokeKey(c *wsConn, reqID uint64, revoke *webv1.RevokeApiKey) {
	name := strings.TrimSpace(revoke.GetName())
	if name == "" {
		c.sendError(reqID, errInvalid, "key name is required")
		return
	}
	if err := s.Auth.RevokeAPIKey(name); err != nil {
		c.sendError(reqID, errNotFound, err.Error())
		return
	}
	c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Revoked{Revoked: &webv1.Empty{}}})
	s.publish(uichanges.Access)
}

// --- validation helpers ----------------------------------------------------

func (s *Server) appByID(ctx context.Context, appID string) (*frameworkv1.InstalledApplication, error) {
	apps, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx, &frameworkv1.ListApplicationsRequest{})
	if err != nil {
		return nil, err
	}
	for _, a := range apps.GetApplications() {
		if a.GetAppId() == appID {
			return a, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "application %q is not installed", appID)
}

func (s *Server) validApp(ctx context.Context, appID string) error {
	_, err := s.appByID(ctx, appID)
	return err
}

func (s *Server) validModel(ctx context.Context, modelID string) error {
	models, err := frameworkv1.NewAssistantServiceClient(s.Conn).ListWakewordModels(ctx, &frameworkv1.ListWakewordModelsRequest{})
	if err != nil {
		return err
	}
	for _, m := range models.GetModels() {
		if m.GetModelId() == modelID {
			return nil
		}
	}
	return status.Errorf(codes.NotFound, "wakeword model %q is not installed", modelID)
}

func (s *Server) validOutputResource(ctx context.Context, resourceID string) error {
	if resourceID == "" {
		return status.Error(codes.InvalidArgument, "resource id is required")
	}
	resources, err := frameworkv1.NewDeviceServiceClient(s.Conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	if err != nil {
		return err
	}
	for _, r := range resources.GetResources() {
		if r.GetResourceId() != resourceID {
			continue
		}
		if r.GetKind() == frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY || r.GetKind() == frameworkv1.ResourceKind_RESOURCE_LED_RING {
			return nil
		}
		return status.Errorf(codes.InvalidArgument, "resource %q is not a display or ring output", resourceID)
	}
	return status.Errorf(codes.NotFound, "output resource %q is not available", resourceID)
}

func protoCloneSettings(v *frameworkv1.UserSettings) *frameworkv1.UserSettings {
	if v == nil {
		return &frameworkv1.UserSettings{}
	}
	return proto.Clone(v).(*frameworkv1.UserSettings)
}

func fieldMask(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	mask := make(map[string]bool, len(names))
	for _, name := range names {
		mask[name] = true
	}
	return mask
}

// ownsField reports whether a masked settings patch owns name. A nil mask is
// the legacy "whole group" form.
func ownsField(mask map[string]bool, name string) bool {
	return mask == nil || mask[name]
}

func clamp01(v float32) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func newTrainingID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func versionString() string { return version.Version }

// classifyError maps transport failures onto the browser's error vocabulary.
func classifyError(err error) webv1.ErrorCode {
	code := status.Code(err)
	switch code {
	case codes.InvalidArgument, codes.OutOfRange:
		return webv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT
	case codes.NotFound:
		return webv1.ErrorCode_ERROR_CODE_NOT_FOUND
	case codes.AlreadyExists, codes.Aborted, codes.FailedPrecondition:
		return webv1.ErrorCode_ERROR_CODE_CONFLICT
	case codes.PermissionDenied, codes.Unauthenticated:
		return webv1.ErrorCode_ERROR_CODE_FORBIDDEN
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return webv1.ErrorCode_ERROR_CODE_UNAVAILABLE
	case codes.Canceled:
		return webv1.ErrorCode_ERROR_CODE_CANCELLED
	default:
		return webv1.ErrorCode_ERROR_CODE_INTERNAL
	}
}

func grpcCodeName(code int32) string { return codes.Code(code).String() }

// trainErrorMessage removes transport noise so the browser can show the
// device's own actionable explanation, including the enrollment privacy gate.
func trainErrorMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "training timed out; the installed model is unchanged"
	}
	if errors.Is(err, context.Canceled) {
		return "training cancelled; the installed model is unchanged"
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.DeadlineExceeded:
			return "training timed out; the installed model is unchanged"
		case codes.Canceled:
			return "training cancelled; the installed model is unchanged"
		}
		if st.Message() != "" {
			return st.Message()
		}
	}
	return err.Error()
}
