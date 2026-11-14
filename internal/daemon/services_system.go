package daemon

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/settings"
	"capsulad/internal/version"
)

type deviceService struct {
	frameworkv1.UnimplementedDeviceServiceServer
	d *Daemon
}

func (s *deviceService) GetInfo(ctx context.Context, _ *frameworkv1.GetInfoRequest) (*frameworkv1.DeviceInfo, error) {
	return s.d.deviceInfo(s.d.bootID), nil
}

func (s *deviceService) ListResources(ctx context.Context, _ *frameworkv1.ListResourcesRequest) (*frameworkv1.ListResourcesResponse, error) {
	return &frameworkv1.ListResourcesResponse{Revision: 1, Resources: s.d.resourceList()}, nil
}

func (s *deviceService) WatchResources(_ *frameworkv1.WatchResourcesRequest, stream frameworkv1.DeviceService_WatchResourcesServer) error {
	for _, res := range s.d.resourceList() {
		if err := stream.Send(&frameworkv1.ResourceChange{Revision: 1, Resource: res}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

type settingsService struct {
	frameworkv1.UnimplementedSettingsServiceServer
	d *Daemon
}

func (s *settingsService) GetSettings(ctx context.Context, _ *frameworkv1.GetSettingsRequest) (*frameworkv1.SettingsSnapshot, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "device settings are a local administrator operation"); err != nil {
		return nil, err
	}
	return s.d.settingsSnapshot(s.d.user.Get()), nil
}

func (s *settingsService) UpdateSettings(ctx context.Context, req *frameworkv1.UpdateSettingsRequest) (*frameworkv1.SettingsSnapshot, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "device settings are a local administrator operation"); err != nil {
		return nil, err
	}
	if req.Values == nil {
		return nil, status.Error(codes.InvalidArgument, "values are required")
	}
	patch := userSettingsFromProto(req.Values)
	snap, err := s.d.user.Update(req.ExpectedRevision, patch, req.UpdateFields)
	if err != nil {
		if strings.Contains(err.Error(), "stale") {
			return nil, status.Error(codes.Aborted, err.Error())
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return s.d.settingsSnapshot(snap), nil
}

func (s *settingsService) WatchSettings(_ *frameworkv1.WatchSettingsRequest, stream frameworkv1.SettingsService_WatchSettingsServer) error {
	if err := s.d.requireAdministrativeCaller(stream.Context(), "device settings are a local administrator operation"); err != nil {
		return err
	}
	initial, ch, cancel := s.d.user.Watch()
	defer cancel()
	if err := stream.Send(s.d.settingsSnapshot(initial)); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case snap, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(s.d.settingsSnapshot(snap)); err != nil {
				return err
			}
		}
	}
}

type systemService struct {
	frameworkv1.UnimplementedSystemServiceServer
	d *Daemon
}

func (s *systemService) GetBluetoothStatus(ctx context.Context, _ *frameworkv1.GetBluetoothStatusRequest) (*frameworkv1.BluetoothStatus, error) {
	return s.bluetoothProto(ctx), nil
}

func (s *systemService) SetBluetoothDiscoverable(ctx context.Context, req *frameworkv1.SetBluetoothDiscoverableRequest) (*frameworkv1.BluetoothStatus, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "Bluetooth discoverability is a local administrator operation"); err != nil {
		return nil, err
	}
	if s.d.bt == nil {
		return nil, status.Error(codes.FailedPrecondition, "Bluetooth is unavailable on this platform")
	}
	// Discoverability invites a source to connect. Do not open that window
	// before the A2DP Sink exists: a first connection then fails with
	// "profile unavailable", and the source does not retry just because the
	// endpoint appears afterwards. Turning discoverability off is always
	// allowed and never powers the adapter.
	if req.Discoverable {
		if err := s.d.ensureBluetoothAdvertisement(); err != nil {
			return nil, status.Error(codes.FailedPrecondition, "audio profile advertisement: "+err.Error())
		}
	}
	if err := s.d.bt.SetDiscoverable(ctx, req.Discoverable); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	fields := []string{"bluetooth_discoverable"}
	mutate := func(v *settings.UserSettings) { v.BluetoothDiscoverable = req.Discoverable }
	if req.Discoverable {
		// Advertising for a new device cannot coexist with denying inbound
		// links. Persist the implied incoming state so a restart is coherent.
		fields = append(fields, "bluetooth_incoming_connections")
		mutate = func(v *settings.UserSettings) {
			v.BluetoothDiscoverable = true
			v.BluetoothIncomingConnections = true
		}
	}
	if err := s.d.writeUserSettings(fields, mutate); err != nil {
		return nil, status.Error(codes.Internal, "adapter changed but the setting could not be persisted: "+err.Error())
	}
	return s.bluetoothProto(ctx), nil
}

// SetBluetoothIncomingConnections controls whether the speaker accepts inbound
// links. Off denies even paired devices while keeping the adapter powered for
// BLE; it also clears discoverability, because advertising with no inbound path
// would be incoherent. It is a local administrator operation: a supervised app
// must not open the adapter to new sources.
func (s *systemService) SetBluetoothIncomingConnections(ctx context.Context, req *frameworkv1.SetBluetoothIncomingConnectionsRequest) (*frameworkv1.BluetoothStatus, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "Bluetooth incoming connections is a local administrator operation"); err != nil {
		return nil, err
	}
	if s.d.bt == nil {
		return nil, status.Error(codes.FailedPrecondition, "Bluetooth is unavailable on this platform")
	}
	// Same invariant as the persisted settings path: do not accept a new
	// source before the A2DP sink profile it needs is advertised, or the link
	// is dropped with "profile unavailable" after it bonds. Turning incoming
	// off is always allowed.
	if req.IncomingConnections {
		if err := s.d.ensureBluetoothAdvertisement(); err != nil {
			return nil, status.Error(codes.FailedPrecondition, "audio profile advertisement: "+err.Error())
		}
	}
	if err := s.d.bt.SetIncomingConnections(ctx, req.IncomingConnections); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	fields := []string{"bluetooth_incoming_connections"}
	mutate := func(v *settings.UserSettings) {
		v.BluetoothIncomingConnections = req.IncomingConnections
	}
	if !req.IncomingConnections {
		fields = append(fields, "bluetooth_discoverable")
		mutate = func(v *settings.UserSettings) {
			v.BluetoothIncomingConnections = false
			v.BluetoothDiscoverable = false
		}
	}
	if err := s.d.writeUserSettings(fields, mutate); err != nil {
		return nil, status.Error(codes.Internal, "adapter changed but the setting could not be persisted: "+err.Error())
	}
	return s.bluetoothProto(ctx), nil
}

func (s *systemService) ListPairedDevices(ctx context.Context, _ *frameworkv1.ListPairedDevicesRequest) (*frameworkv1.ListPairedDevicesResponse, error) {
	if err := s.d.requireLocalAdmin(ctx); err != nil {
		return nil, err
	}
	if s.d.bt == nil {
		return nil, status.Error(codes.FailedPrecondition, "Bluetooth is unavailable on this platform")
	}
	devices, err := s.d.bt.PairedDevices(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	out := make([]*frameworkv1.PairedDevice, 0, len(devices))
	for _, d := range devices {
		out = append(out, &frameworkv1.PairedDevice{
			Address: d.Address, Name: d.Name, Connected: d.Connected, Trusted: d.Trusted,
		})
	}
	return &frameworkv1.ListPairedDevicesResponse{Devices: out}, nil
}

func (s *systemService) UnpairDevice(ctx context.Context, req *frameworkv1.UnpairDeviceRequest) (*frameworkv1.UnpairDeviceResponse, error) {
	if err := s.d.requireLocalAdmin(ctx); err != nil {
		return nil, err
	}
	if s.d.bt == nil {
		return nil, status.Error(codes.FailedPrecondition, "Bluetooth is unavailable on this platform")
	}
	if strings.TrimSpace(req.Address) == "" {
		return nil, status.Error(codes.InvalidArgument, "device address is required")
	}
	if err := s.d.bt.Unpair(ctx, req.Address); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &frameworkv1.UnpairDeviceResponse{Removed: true}, nil
}

func (s *systemService) bluetoothProto(ctx context.Context) *frameworkv1.BluetoothStatus {
	if s.d.bt == nil {
		return &frameworkv1.BluetoothStatus{Present: false, Detail: "BlueZ is unavailable"}
	}
	st, err := s.d.bt.Status(ctx)
	if err != nil {
		return &frameworkv1.BluetoothStatus{Present: false, Detail: err.Error()}
	}
	detail := st.Detail
	// Status has no dedicated readiness field, so use the backend explanation
	// it already carries. Pairing and discoverability depend on an advertised
	// Audio Sink; saying so prevents an adapter that is up but cannot accept an
	// audio source from appearing ready.
	if st.Present && !s.d.bluetoothAudioReady() {
		if detail != "" {
			detail += "; "
		}
		detail += "audio profile is not advertised (A2DP sink not ready)"
	}
	return &frameworkv1.BluetoothStatus{
		Present: st.Present, Powered: st.Powered, Discoverable: st.Discoverable,
		IncomingConnections: st.IncomingConnections, Adapter: st.Adapter,
		Name: st.Name, Detail: detail,
	}
}

func (s *systemService) GetStatus(ctx context.Context, _ *frameworkv1.GetStatusRequest) (*frameworkv1.SystemStatus, error) {
	return &frameworkv1.SystemStatus{
		UptimeMs: uint64(time.Duration(s.d.clock.NowNS()).Milliseconds()),
		BootId:   s.d.bootID, DaemonVersion: version.Version,
	}, nil
}

func (s *systemService) GetWebUiSecurity(ctx context.Context, _ *frameworkv1.GetWebUiSecurityRequest) (*frameworkv1.WebUiSecurityStatus, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "Web UI security status is a local administrator operation"); err != nil {
		return nil, err
	}
	listen := []string{}
	if s.d.cfg.WebUIAddr != "" {
		listen = append(listen, s.d.cfg.WebUIAddr)
	}
	st := s.d.websec.Status(listen)
	out := &frameworkv1.WebUiSecurityStatus{
		HttpsEnabled: st.HTTPSEnabled, ClientCertificateRequired: false,
		BootstrapComplete: st.BootstrapComplete, ListenAddresses: st.ListenAddresses,
		ActiveApiKeys: uint32(st.ActiveAPIKeys), AdminPasswordSet: st.AdminPasswordSet,
	}
	if st.ServerFingerprint != "" {
		out.ServerCertificateSha256 = []string{st.ServerFingerprint}
	}
	if !st.ServerExpiry.IsZero() {
		out.ServerCertificateExpiryUnixMs = uint64(st.ServerExpiry.UnixMilli())
	}
	return out, nil
}

func (s *systemService) ListUnits(ctx context.Context, _ *frameworkv1.ListUnitsRequest) (*frameworkv1.ListUnitsResponse, error) {
	// Vendor system-service control is backend-specific and not verified on the
	// stock firmware yet; an empty list is honest.
	return &frameworkv1.ListUnitsResponse{}, nil
}

func (s *systemService) ControlUnit(ctx context.Context, req *frameworkv1.ControlUnitRequest) (*frameworkv1.UnitInfo, error) {
	return nil, status.Error(codes.Unimplemented, "vendor system-service control is not implemented for this backend")
}

type sensorService struct {
	frameworkv1.UnimplementedSensorServiceServer
	d *Daemon
}

func (s *sensorService) ListSensors(ctx context.Context, _ *frameworkv1.ListSensorsRequest) (*frameworkv1.ListSensorsResponse, error) {
	sensors := s.d.dev.Sensors()
	if sensors == nil {
		return &frameworkv1.ListSensorsResponse{}, nil
	}
	out := &frameworkv1.ListSensorsResponse{}
	for _, info := range sensors.List() {
		out.Sensors = append(out.Sensors, &frameworkv1.SensorInfo{SensorId: info.ID, Unit: info.Unit})
	}
	return out, nil
}

func (s *sensorService) ReadSensor(ctx context.Context, req *frameworkv1.ReadSensorRequest) (*frameworkv1.SensorSample, error) {
	sensors := s.d.dev.Sensors()
	if sensors == nil {
		return nil, status.Error(codes.NotFound, "no sensors are available")
	}
	v, ok := sensors.Read(req.SensorId)
	if !ok {
		return nil, status.Error(codes.NotFound, "unknown sensor")
	}
	unit := ""
	for _, info := range sensors.List() {
		if info.ID == req.SensorId {
			unit = info.Unit
		}
	}
	return &frameworkv1.SensorSample{
		SensorId: req.SensorId, Value: v, Unit: unit, MonotonicTimeNs: s.d.clock.NowNS(),
	}, nil
}

func (s *sensorService) WatchSensor(req *frameworkv1.WatchSensorRequest, stream frameworkv1.SensorService_WatchSensorServer) error {
	interval := time.Duration(req.MinimumIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
			sample, err := s.ReadSensor(stream.Context(), &frameworkv1.ReadSensorRequest{SensorId: req.SensorId})
			if err != nil {
				return err
			}
			if err := stream.Send(sample); err != nil {
				return err
			}
		}
	}
}

type assetService struct {
	frameworkv1.UnimplementedAssetServiceServer
	d *Daemon
}

func (s *assetService) ListAssets(ctx context.Context, req *frameworkv1.ListAssetsRequest) (*frameworkv1.ListAssetsResponse, error) {
	out := &frameworkv1.ListAssetsResponse{}
	for _, a := range s.d.assets.List(req.Prefix) {
		out.Assets = append(out.Assets, &frameworkv1.SharedAssetInfo{
			AssetId: a.ID, Path: a.Path, MediaType: a.MediaType, Sha256Hex: a.SHA256,
			License: a.License, Attribution: a.Attribution, RequiredBaseAsset: a.Required,
		})
	}
	return out, nil
}

func (d *Daemon) registerServices(srv *grpc.Server) {
	frameworkv1.RegisterDeviceServiceServer(srv, &deviceService{d: d})
	frameworkv1.RegisterSettingsServiceServer(srv, &settingsService{d: d})
	frameworkv1.RegisterSystemServiceServer(srv, &systemService{d: d})
	frameworkv1.RegisterBluetoothMediaServiceServer(srv, &bluetoothMediaService{d: d})
	frameworkv1.RegisterSensorServiceServer(srv, &sensorService{d: d})
	frameworkv1.RegisterAssetServiceServer(srv, &assetService{d: d})
	d.registerApplicationServices(srv)
	d.registerMediaPlane(srv)
	d.registerDiagnostics(srv)
}
