package daemon

import (
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/manifest"
	"capsulad/internal/provenance"
	"capsulad/internal/registry"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
	"capsulad/internal/supervisor"
)

func (d *Daemon) settingsSnapshot(s settings.UserSnapshot) *frameworkv1.SettingsSnapshot {
	v := s.Values
	out := &frameworkv1.UserSettings{
		DefaultAppByResource:  map[string]string{},
		DefaultAssistantAppId: v.DefaultAssistantAppID,
		WakewordModelId:       v.WakewordModelID,
		WakewordEnabled:       v.WakewordEnabled,
		DisplayBrightness:     map[string]float32{},
		MasterVolume:          float32(v.MasterVolume),
		MicrophoneMuted:       v.MicrophoneMuted,
		Timezone:              v.Timezone,
		BrightnessMode:        map[string]string{},
		BluetoothPairable:     v.BluetoothPairable,
		StartupSoundEnabled:   v.StartupSoundEnabled,
		TouchHoldMode:         frameworkv1.TouchHoldMode(v.TouchHoldMode),
		MutedMicrophoneIndicator: &frameworkv1.MutedMicrophoneIndicator{
			Mode: frameworkv1.MutedIndicatorMode(v.MutedIndicator), DelayMs: v.MutedIndicatorDelayMS,
		},
	}
	for k, val := range v.DefaultAppByResource {
		out.DefaultAppByResource[k] = val
	}
	for k, val := range v.DisplayBrightness {
		out.DisplayBrightness[k] = float32(val)
	}
	for k, val := range v.BrightnessMode {
		out.BrightnessMode[k] = val
	}
	return &frameworkv1.SettingsSnapshot{Revision: s.Revision, Values: out}
}

func userSettingsFromProto(v *frameworkv1.UserSettings) settings.UserSettings {
	if v == nil {
		return settings.DefaultUserSettings()
	}
	out := settings.UserSettings{
		DefaultAppByResource:  map[string]string{},
		DefaultAssistantAppID: v.DefaultAssistantAppId,
		WakewordModelID:       v.WakewordModelId,
		WakewordEnabled:       v.WakewordEnabled,
		DisplayBrightness:     map[string]float64{},
		MasterVolume:          float64(v.MasterVolume),
		MicrophoneMuted:       v.MicrophoneMuted,
		Timezone:              v.Timezone,
		TouchHoldMode:         settings.TouchHoldMode(v.TouchHoldMode),
		BrightnessMode:        map[string]string{},
		BluetoothPairable:     v.BluetoothPairable,
		StartupSoundEnabled:   v.StartupSoundEnabled,
	}
	for k, val := range v.DefaultAppByResource {
		out.DefaultAppByResource[k] = val
	}
	for k, val := range v.DisplayBrightness {
		out.DisplayBrightness[k] = float64(val)
	}
	for k, val := range v.BrightnessMode {
		out.BrightnessMode[k] = val
	}
	if v.MutedMicrophoneIndicator != nil {
		out.MutedIndicator = settings.MutedIndicatorMode(v.MutedMicrophoneIndicator.Mode)
		out.MutedIndicatorDelayMS = v.MutedMicrophoneIndicator.DelayMs
	}
	return out
}

func runtimeProto(rt manifest.Runtime) frameworkv1.ApplicationRuntime {
	switch rt {
	case manifest.RuntimeNative:
		return frameworkv1.ApplicationRuntime_RUNTIME_NATIVE
	case manifest.RuntimeQuickJS:
		return frameworkv1.ApplicationRuntime_RUNTIME_QUICKJS
	case manifest.RuntimeMicroPython:
		return frameworkv1.ApplicationRuntime_RUNTIME_MICROPYTHON
	case manifest.RuntimeBuiltin:
		return frameworkv1.ApplicationRuntime_RUNTIME_BUILTIN
	default:
		return frameworkv1.ApplicationRuntime_APPLICATION_RUNTIME_UNSPECIFIED
	}
}

func (d *Daemon) appProto(app *registry.App) *frameworkv1.InstalledApplication {
	m := app.Manifest
	system := m.Entrypoint.Runtime == manifest.RuntimeBuiltin
	out := &frameworkv1.InstalledApplication{
		AppId: m.ID, DisplayName: m.Name, Version: m.Version,
		StartOnBoot: m.StartOnBoot, SingleInstance: m.SingleInstance,
		Provides: append([]string(nil), m.Provides...), Background: m.Background,
		RestartOnFailure: m.RestartOnFailure,
		SystemApp:        system,
		CanStop:          true,
		CanUninstall:     false,
		Entrypoint: &frameworkv1.ApplicationEntrypoint{
			Runtime: runtimeProto(m.Entrypoint.Runtime), RelativePath: m.Entrypoint.Path,
			CpuArchitecture: m.Entrypoint.Architecture, BinaryAbi: m.Entrypoint.ABI,
			FrameworkSdkVersion: m.Entrypoint.SDK,
		},
	}
	for _, a := range m.Arguments {
		out.LaunchArguments = append(out.LaunchArguments, &frameworkv1.AppArgumentInfo{
			Name: a.Name, Description: a.Description, ValueType: a.Type, Required: a.Required,
		})
	}
	for _, iface := range m.Interfaces {
		out.Interfaces = append(out.Interfaces, &frameworkv1.DeclaredAppInterface{
			GrpcService: iface.Service, InterfaceVersion: iface.Version,
		})
	}
	if g := m.GitHubAttestation; g != nil {
		out.GithubAttestation = &frameworkv1.GitHubAttestationDeclaration{
			BundlePath: g.Bundle, SubjectPath: g.Subject,
			ExpectedRepository: g.Repository, ExpectedWorkflow: g.Workflow,
		}
	}
	return out
}

func runProto(run supervisor.Run) *frameworkv1.AppRun {
	state := frameworkv1.AppRunState_APP_RUN_STATE_UNSPECIFIED
	switch run.State {
	case supervisor.Starting:
		state = frameworkv1.AppRunState_APP_STARTING
	case supervisor.Ready:
		state = frameworkv1.AppRunState_APP_READY
	case supervisor.Stopping:
		state = frameworkv1.AppRunState_APP_STOPPING
	case supervisor.Exited:
		state = frameworkv1.AppRunState_APP_EXITED
	case supervisor.Failed:
		state = frameworkv1.AppRunState_APP_FAILED
	}
	detail := run.Detail
	if run.Sandbox != "" {
		detail = "sandbox=" + run.Sandbox + " " + detail
	}
	return &frameworkv1.AppRun{
		RunId: run.RunID, AppId: run.AppID, Pid: uint32(run.PID), State: state,
		StartedMonotonicNs: run.StartedNS, ExitCode: int32(run.ExitCode),
		StatusDetail: detail, Runtime: runtimeProto(run.Runtime),
	}
}

func settingTypeProto(t string) frameworkv1.SettingType {
	switch t {
	case "text":
		return frameworkv1.SettingType_SETTING_TEXT
	case "text_masked":
		return frameworkv1.SettingType_SETTING_TEXT_MASKED
	case "boolean":
		return frameworkv1.SettingType_SETTING_BOOLEAN
	case "integer":
		return frameworkv1.SettingType_SETTING_INTEGER
	case "number":
		return frameworkv1.SettingType_SETTING_NUMBER
	case "select":
		return frameworkv1.SettingType_SETTING_SELECT
	default:
		return frameworkv1.SettingType_SETTING_TYPE_UNSPECIFIED
	}
}

func provenanceProto(res provenance.Result) *frameworkv1.BinaryProvenanceResult {
	status := frameworkv1.ProvenanceStatus_PROVENANCE_STATUS_UNSPECIFIED
	switch res.Status {
	case provenance.StatusNotProvided:
		status = frameworkv1.ProvenanceStatus_PROVENANCE_NOT_PROVIDED
	case provenance.StatusVerified:
		status = frameworkv1.ProvenanceStatus_PROVENANCE_VERIFIED
	case provenance.StatusFailed:
		status = frameworkv1.ProvenanceStatus_PROVENANCE_VERIFICATION_FAILED
	case provenance.StatusUnverified:
		status = frameworkv1.ProvenanceStatus_PROVENANCE_UNVERIFIED
	case provenance.StatusUnsupported:
		status = frameworkv1.ProvenanceStatus_PROVENANCE_UNSUPPORTED_RUNTIME
	}
	out := &frameworkv1.BinaryProvenanceResult{
		AppId: res.AppID, Status: status, Detail: res.Detail,
		SubjectSha256Hex: res.SubjectSHA256, VerifiedRepository: res.Repository,
		VerifiedWorkflow: res.Workflow, VerifiedCommitSha: res.CommitSHA,
		TrustedRootCurrent: res.TrustedRootCurrent,
	}
	if !res.VerifiedAt.IsZero() {
		out.VerifiedAtUnixMs = uint64(res.VerifiedAt.UnixMilli())
	}
	return out
}

func touchModeProto(m routing.TouchMode) frameworkv1.TouchCaptureMode {
	switch m {
	case routing.TouchPartial:
		return frameworkv1.TouchCaptureMode_TOUCH_PARTIAL_GESTURES
	case routing.TouchFull:
		return frameworkv1.TouchCaptureMode_TOUCH_FULL_RAW
	default:
		return frameworkv1.TouchCaptureMode_TOUCH_CAPTURE_MODE_UNSPECIFIED
	}
}

func captureInfoProto(view routing.CaptureView) *frameworkv1.CaptureInfo {
	return &frameworkv1.CaptureInfo{
		CaptureId: view.ID, ResourceId: view.ResourceID, AppId: view.AppID, RunId: view.RunID,
		PresentationId: view.PresentationID, AcquiredMonotonicNs: view.AcquiredNS,
		Selected: view.Selected, Ready: view.Ready, TouchMode: touchModeProto(view.TouchMode),
	}
}

func routingStateProto(sel routing.Selection, captures []routing.CaptureView) *frameworkv1.RoutingState {
	out := &frameworkv1.RoutingState{
		ResourceId: sel.ResourceID, ConfiguredDefaultAppId: sel.ConfiguredDefaultApp,
		SelectedPresentationId: sel.SelectedPresentation, SelectedAppId: sel.SelectedAppID,
		Revision: sel.Revision,
	}
	for _, c := range captures {
		out.Captures = append(out.Captures, captureInfoProto(c))
	}
	return out
}
