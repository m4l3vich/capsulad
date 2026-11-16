package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/ids"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
	"capsulad/internal/supervisor"
	"capsulad/internal/uichanges"
	"capsulad/internal/wakeword"
)

// callerIdentity resolves an app connection to a supervised run.
//
// The run ID is carried in gRPC metadata by the SDK; when local peer
// credentials are available, the PID must match the supervisor's run. A
// compiled-in system app is never resolved from the peer PID: see
// supervisedRunForPID. This is identity bookkeeping, not a per-app permission
// model.
func (d *Daemon) callerIdentity(ctx context.Context) (appID, runID string, ok bool) {
	if d.sup == nil {
		return "", "", false
	}
	if md, found := metadata.FromIncomingContext(ctx); found {
		if vals := md.Get("capsulad-run-id"); len(vals) > 0 {
			runID = vals[0]
			run, found := d.sup.Get(runID)
			if !found {
				return "", "", false
			}
			if run.Virtual && !d.isInternalPeer(ctx) {
				// Built-in run IDs are stable by design. Without the per-boot
				// daemon token, a local client could claim to be the clock or
				// ring; the daemon's in-process clients are the only legitimate
				// users of a virtual run identity.
				d.logs.Warnf("daemon", run.AppID, "rejected virtual run %s from a non-daemon connection", runID)
				return "", "", false
			}
			if creds := grpcpeer.FromContext(ctx); creds.OK && creds.PID != run.PID {
				d.logs.Warnf("daemon", run.AppID, "run %s claimed by pid %d but socket peer is %d", runID, run.PID, creds.PID)
				return "", "", false
			}
			return run.AppID, runID, true
		}
	}
	if run, found := d.supervisedRunForPID(ctx); found {
		return run.AppID, run.RunID, true
	}
	return "", "", false
}

// supervisedRunForPID resolves a socket peer to an external supervised run.
//
// A compiled-in system app executes inside this process, so every virtual run
// records the daemon's own PID — and so does every connection the daemon opens
// to its own socket (the Web UI, framework cues and built-in app clients). A
// PID match therefore proves nothing about a built-in and must never select
// one: whichever virtual run was registered first would win and the Web UI's
// own administrator reads would be attributed to that app. Built-in identities
// are explicit instead (the stable run ID plus the per-boot internal token),
// checked by the caller above. Only a real child process is resolved by PID.
func (d *Daemon) supervisedRunForPID(ctx context.Context) (supervisor.Run, bool) {
	creds := grpcpeer.FromContext(ctx)
	if !creds.OK {
		return supervisor.Run{}, false
	}
	return d.runForPID(creds.PID)
}

func (d *Daemon) requireCaller(ctx context.Context) (string, string, error) {
	appID, runID, ok := d.callerIdentity(ctx)
	if !ok {
		return "", "", status.Error(codes.PermissionDenied, "this RPC requires a supervised application connection")
	}
	return appID, runID, nil
}

// --- presentations ---------------------------------------------------------

type presentationService struct {
	frameworkv1.UnimplementedPresentationServiceServer
	d *Daemon
}

func (s *presentationService) Open(stream frameworkv1.PresentationService_OpenServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first message must be OpenPresentation")
	}
	appID, runID, err := s.d.requireCaller(stream.Context())
	if err != nil {
		return err
	}
	kind, ok := s.d.presentationKind(open.ResourceId)
	if !ok {
		return status.Errorf(codes.NotFound, "resource %s does not accept presentations", open.ResourceId)
	}
	p, err := s.d.openAppPresentation(appID, runID, open.ResourceId)
	if err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	defer s.d.closeAppPresentation(appID, runID, p)
	sub := p.Subscribe(16)
	defer sub.Close()

	if err := stream.Send(&frameworkv1.PresentationEvent{Event: &frameworkv1.PresentationEvent_Ready{
		Ready: &frameworkv1.PresentationReady{PresentationId: p.ID, ResourceId: p.ResourceID, HasValidFrame: p.Ready()},
	}}); err != nil {
		return err
	}
	if p.Visible() {
		if err := stream.Send(&frameworkv1.PresentationEvent{Event: &frameworkv1.PresentationEvent_Visibility{
			Visibility: &frameworkv1.PresentationVisibility{Visible: true, Reason: "already selected"},
		}}); err != nil {
			return err
		}
	}

	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stream.Context().Done():
				return
			case ev, ok := <-sub.C:
				if !ok {
					return
				}
				var msg *frameworkv1.PresentationEvent
				switch ev.Type {
				case presentations.EventReady:
					msg = &frameworkv1.PresentationEvent{Event: &frameworkv1.PresentationEvent_Ready{
						Ready: &frameworkv1.PresentationReady{PresentationId: p.ID, ResourceId: p.ResourceID, HasValidFrame: ev.HasContent},
					}}
				case presentations.EventVisibility:
					msg = &frameworkv1.PresentationEvent{Event: &frameworkv1.PresentationEvent_Visibility{
						Visibility: &frameworkv1.PresentationVisibility{Visible: ev.Visible, Reason: ev.Reason},
					}}
				}
				if msg == nil {
					continue
				}
				if err := stream.Send(msg); err != nil {
					select {
					case sendErr <- err:
					default:
					}
					return
				}
			}
		}
	}()

	for {
		select {
		case err := <-sendErr:
			return err
		default:
		}
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch r := req.Request.(type) {
		case *frameworkv1.PresentationRequest_Frame:
			content, err := contentFromProto(r.Frame, kind)
			if err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			if err := s.d.updateAppPresentation(appID, runID, p, content); err != nil {
				return status.Error(codes.InvalidArgument, err.Error())
			}
		case *frameworkv1.PresentationRequest_Close:
			return nil
		case *frameworkv1.PresentationRequest_Open:
			return status.Error(codes.InvalidArgument, "presentation is already open")
		}
	}
}

func contentFromProto(frame *frameworkv1.PresentationFrame, kind presentations.Kind) (presentations.Content, error) {
	if frame == nil {
		return presentations.Content{}, fmt.Errorf("frame is required")
	}
	switch p := frame.Payload.(type) {
	case *frameworkv1.PresentationFrame_Blank:
		return presentations.Blank(), nil
	case *frameworkv1.PresentationFrame_Segment:
		if kind != presentations.Segment {
			return presentations.Content{}, fmt.Errorf("segment frames are only valid on segment presentations")
		}
		cells := make([]uint16, 0, len(p.Segment.Cells))
		for _, c := range p.Segment.Cells {
			if c.Mask > 0xffff {
				return presentations.Content{}, fmt.Errorf("segment mask %d does not fit a 16-bit cell", c.Mask)
			}
			cells = append(cells, uint16(c.Mask))
		}
		return presentations.SegmentFrame(cells, p.Segment.Colon), nil
	case *frameworkv1.PresentationFrame_Ring:
		if kind != presentations.Ring {
			return presentations.Content{}, fmt.Errorf("ring frames are only valid on ring presentations")
		}
		px := make([]backend.RGB, 0, len(p.Ring.Pixels))
		for _, c := range p.Ring.Pixels {
			px = append(px, backend.RGB{R: clamp8(c.Red), G: clamp8(c.Green), B: clamp8(c.Blue)})
		}
		return presentations.RingFrame(px), nil
	case *frameworkv1.PresentationFrame_RingEffect:
		if kind != presentations.Ring {
			return presentations.Content{}, fmt.Errorf("ring effects are only valid on ring presentations")
		}
		info, ok := presets.Lookup(p.RingEffect.EffectId)
		if !ok || info.Kind != presets.KindRingEffect {
			return presentations.Content{}, fmt.Errorf("unknown ring effect %q", p.RingEffect.EffectId)
		}
		params, err := presetParams(p.RingEffect.Arguments)
		if err != nil {
			return presentations.Content{}, err
		}
		valid, err := presets.ValidateParams(*info, params)
		if err != nil {
			return presentations.Content{}, err
		}
		return presentations.RingEffect(p.RingEffect.EffectId, valid), nil
	case *frameworkv1.PresentationFrame_SegmentFace:
		if kind != presentations.Segment {
			return presentations.Content{}, fmt.Errorf("segment faces are only valid on segment presentations")
		}
		info, ok := presets.Lookup(p.SegmentFace.FaceId)
		if !ok || info.Kind != presets.KindSegmentFace {
			return presentations.Content{}, fmt.Errorf("unknown segment face %q", p.SegmentFace.FaceId)
		}
		params, err := presetParams(p.SegmentFace.Arguments)
		if err != nil {
			return presentations.Content{}, err
		}
		valid, err := presets.ValidateParams(*info, params)
		if err != nil {
			return presentations.Content{}, err
		}
		return presentations.SegmentFace(p.SegmentFace.FaceId, valid), nil
	default:
		return presentations.Content{}, fmt.Errorf("unknown presentation payload")
	}
}

func clamp8(v uint32) uint8 {
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func presetParams(args []*frameworkv1.PresetArgument) (presets.Params, error) {
	out := presets.Params{}
	for _, a := range args {
		if a.Value == nil || a.Value.Value == nil {
			return nil, fmt.Errorf("preset argument %q has no value", a.Name)
		}
		switch v := a.Value.Value.(type) {
		case *frameworkv1.PresetValue_Text:
			out[a.Name] = v.Text
		case *frameworkv1.PresetValue_Integer:
			out[a.Name] = v.Integer
		case *frameworkv1.PresetValue_Number:
			out[a.Name] = v.Number
		case *frameworkv1.PresetValue_Boolean:
			out[a.Name] = v.Boolean
		case *frameworkv1.PresetValue_Color:
			out[a.Name] = backend.RGB{R: clamp8(v.Color.Red), G: clamp8(v.Color.Green), B: clamp8(v.Color.Blue)}
		}
	}
	return out, nil
}

func (s *presentationService) ListPresets(ctx context.Context, req *frameworkv1.ListPresentationPresetsRequest) (*frameworkv1.ListPresentationPresetsResponse, error) {
	kind := presets.Kind("")
	if req.ResourceId != "" {
		if info, ok := s.d.resources[req.ResourceId]; ok {
			switch info.Kind {
			case frameworkv1.ResourceKind_RESOURCE_LED_RING:
				kind = presets.KindRingEffect
			case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
				kind = presets.KindSegmentFace
			default:
				return nil, status.Error(codes.NotFound, "resource does not accept presentations")
			}
		} else {
			return nil, status.Error(codes.NotFound, "unknown resource")
		}
	}
	out := &frameworkv1.ListPresentationPresetsResponse{}
	for _, info := range presets.Catalog(kind, req.ResourceId) {
		entry := &frameworkv1.PresentationPresetInfo{
			PresetId: info.ID, Kind: presetKindProto(info.Kind),
			DisplayName: info.DisplayName, Description: info.Description,
		}
		if req.ResourceId != "" {
			entry.SupportedResourceIds = []string{req.ResourceId}
		}
		for _, def := range info.Params {
			p := &frameworkv1.PresetParameterDefinition{
				Name: def.Name, Description: def.Description, ValueType: def.Type,
			}
			if def.HasMin {
				v := def.Min
				p.Min = &v
			}
			if def.HasMax {
				v := def.Max
				p.Max = &v
			}
			p.DefaultValue = presetDefault(def)
			entry.Parameters = append(entry.Parameters, p)
		}
		out.Presets = append(out.Presets, entry)
	}
	return out, nil
}

func presetKindProto(k presets.Kind) frameworkv1.PresetKind {
	if k == presets.KindRingEffect {
		return frameworkv1.PresetKind_PRESET_RING_EFFECT
	}
	return frameworkv1.PresetKind_PRESET_SEGMENT_FACE
}

func presetDefault(def presets.ParamDef) *frameworkv1.PresetValue {
	switch v := def.Default.(type) {
	case string:
		return &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Text{Text: v}}
	case bool:
		return &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Boolean{Boolean: v}}
	case int64:
		return &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Integer{Integer: v}}
	case float64:
		return &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Number{Number: v}}
	case backend.RGB:
		return &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Color{Color: &frameworkv1.RgbPixel{
			Red: uint32(v.R), Green: uint32(v.G), Blue: uint32(v.B),
		}}}
	default:
		return nil
	}
}

// --- routing ---------------------------------------------------------------

type routingService struct {
	frameworkv1.UnimplementedRoutingServiceServer
	d *Daemon
}

func (s *routingService) GetRouting(ctx context.Context, req *frameworkv1.GetRoutingRequest) (*frameworkv1.RoutingState, error) {
	if _, ok := s.d.resources[req.ResourceId]; !ok {
		return nil, status.Error(codes.NotFound, "unknown resource")
	}
	sel, caps := s.d.router.State(req.ResourceId)
	return routingStateProto(sel, caps), nil
}

func (s *routingService) WatchRouting(req *frameworkv1.WatchRoutingRequest, stream frameworkv1.RoutingService_WatchRoutingServer) error {
	if _, ok := s.d.resources[req.ResourceId]; !ok {
		return status.Error(codes.NotFound, "unknown resource")
	}
	last := uint64(0)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		sel, caps := s.d.router.State(req.ResourceId)
		state := routingStateProto(sel, caps)
		if state.Revision != last {
			last = state.Revision
			if err := stream.Send(state); err != nil {
				return err
			}
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *routingService) Capture(req *frameworkv1.CaptureRequest, stream frameworkv1.RoutingService_CaptureServer) error {
	appID, runID, err := s.d.requireCaller(stream.Context())
	if err != nil {
		return err
	}
	info, ok := s.d.resources[req.ResourceId]
	if !ok {
		return status.Error(codes.NotFound, "unknown resource")
	}
	mode := routing.TouchMode(0)
	switch req.TouchMode {
	case frameworkv1.TouchCaptureMode_TOUCH_PARTIAL_GESTURES:
		mode = routing.TouchPartial
	case frameworkv1.TouchCaptureMode_TOUCH_FULL_RAW:
		mode = routing.TouchFull
	}
	if info.Kind == frameworkv1.ResourceKind_RESOURCE_TOUCH {
		caps := touchCaps(s.d.dev.Touch())
		if mode == routing.TouchFull && !caps.FullPanel {
			return status.Error(codes.FailedPrecondition, "this touch backend cannot provide a full-panel stream; full capture is unavailable")
		}
		if mode == routing.TouchPartial && !caps.PartialGestures {
			return status.Error(codes.FailedPrecondition, "this touch backend provides no semantic gestures")
		}
	}
	cap, err := s.d.acquireAppCapture(appID, runID, req.ResourceId, req.PresentationId, mode)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	view, events, closeSub := s.d.router.SubscribeCapture(cap)
	defer func() {
		closeSub()
		s.d.releaseAppCapture(appID, runID, cap.ID)
	}()
	_ = view // the granted event is queued by SubscribeCapture
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-cap.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			var out *frameworkv1.CaptureEvent
			if ev.Granted != nil {
				out = &frameworkv1.CaptureEvent{Event: &frameworkv1.CaptureEvent_Granted{
					Granted: &frameworkv1.CaptureGranted{Capture: captureInfoProto(*ev.Granted)},
				}}
			} else if ev.Change != nil {
				out = &frameworkv1.CaptureEvent{Event: &frameworkv1.CaptureEvent_Changed{
					Changed: &frameworkv1.CaptureSelectionChanged{Selected: ev.Change.Selected, Reason: ev.Change.Reason},
				}}
			}
			if out != nil {
				if err := stream.Send(out); err != nil {
					return err
				}
			}
		}
	}
}

func (s *routingService) ReleaseCapture(ctx context.Context, req *frameworkv1.ReleaseCaptureRequest) (*frameworkv1.ReleaseCaptureResponse, error) {
	appID, runID, err := s.d.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	return &frameworkv1.ReleaseCaptureResponse{Released: s.d.releaseAppCapture(appID, runID, req.CaptureId)}, nil
}

func (s *routingService) ForceRelease(ctx context.Context, req *frameworkv1.ForceReleaseRequest) (*frameworkv1.ForceReleaseResponse, error) {
	// ForceRelease removes another owner's capture, so it is reserved for the
	// local administrator/diagnostics surface. Applications release only their
	// own capture through ReleaseCapture and their run lifecycle.
	if err := s.d.requireAdministrativeCaller(ctx, "force release is a local administrator operation"); err != nil {
		return nil, err
	}
	return &frameworkv1.ForceReleaseResponse{Released: s.d.router.ForceRelease(req.CaptureId)}, nil
}

// --- input -----------------------------------------------------------------

type inputService struct {
	frameworkv1.UnimplementedInputServiceServer
	d *Daemon
}

func (s *inputService) WatchInput(req *frameworkv1.WatchInputRequest, stream frameworkv1.InputService_WatchInputServer) error {
	appID, _, err := s.d.requireCaller(stream.Context())
	if err != nil {
		return err
	}
	if req.ResourceId != "" && req.ResourceId != "touch.panel.main" {
		return status.Error(codes.NotFound, "unknown touch resource")
	}
	sel, caps := s.d.router.State("touch.panel.main")
	found := false
	for _, c := range caps {
		if c.ID == req.CaptureId && c.AppID == appID {
			found = true
		}
	}
	if !found || sel.SelectedCaptureID != req.CaptureId {
		return status.Error(codes.FailedPrecondition, "capture_id must be the caller's currently selected touch capture")
	}
	sub, err := s.d.input.Subscribe(req.CaptureId)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	defer sub.Close()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-sub.C:
			if !ok {
				return nil
			}
			out := &frameworkv1.InputEvent{ResourceId: "touch.panel.main", MonotonicTimeNs: ev.MonotonicNS}
			switch {
			case ev.Key != nil:
				out.Event = &frameworkv1.InputEvent_Key{Key: &frameworkv1.KeyInput{Key: ev.Key.Key, Pressed: ev.Key.Pressed}}
			case ev.Gesture != nil:
				out.Event = &frameworkv1.InputEvent_Gesture{Gesture: &frameworkv1.GestureInput{
					GestureId: ev.Gesture.ID, Phase: ev.Gesture.Phase,
				}}
			case ev.Report != nil:
				raw := &frameworkv1.RawTouchReport{
					BackendSample: ev.Report.BackendSample, BackendSampleFormat: ev.Report.SampleFormat,
					CoordinatesValid: ev.Report.CoordinatesValid,
				}
				for _, c := range ev.Report.Contacts {
					raw.Contacts = append(raw.Contacts, &frameworkv1.RawTouchContact{
						ContactId: c.ID, XNormalized: c.X, YNormalized: c.Y,
						Pressure: c.Pressure, Touching: c.Touching,
					})
				}
				out.Event = &frameworkv1.InputEvent_Raw{Raw: raw}
			}
			if err := stream.Send(out); err != nil {
				return err
			}
		}
	}
}

// --- audio -----------------------------------------------------------------

type audioService struct {
	frameworkv1.UnimplementedAudioServiceServer
	d *Daemon
}

func (s *audioService) ListEndpoints(ctx context.Context, _ *frameworkv1.ListAudioEndpointsRequest) (*frameworkv1.ListAudioEndpointsResponse, error) {
	if s.d.dev.Audio() == nil {
		return &frameworkv1.ListAudioEndpointsResponse{}, nil
	}
	in := s.d.audio.CaptureFormat()
	out := s.d.audio.PlaybackFormat()
	return &frameworkv1.ListAudioEndpointsResponse{Endpoints: []*frameworkv1.AudioEndpointInfo{
		{EndpointId: audio.CaptureEndpoint, Direction: frameworkv1.AudioDirection_AUDIO_CAPTURE,
			SupportedFormats: []*frameworkv1.AudioFormat{formatProto(in)},
			ProcessingTap:    s.d.audio.CaptureProcessingTap()},
		{EndpointId: audio.RawCaptureEndpoint, Direction: frameworkv1.AudioDirection_AUDIO_CAPTURE,
			SupportedFormats: []*frameworkv1.AudioFormat{formatProto(in)}},
		{EndpointId: "audio.output.speaker", Direction: frameworkv1.AudioDirection_AUDIO_PLAYBACK,
			SupportedFormats: []*frameworkv1.AudioFormat{formatProto(out)}},
	}}, nil
}

func formatProto(f backend.AudioFormat) *frameworkv1.AudioFormat {
	sf := frameworkv1.SampleFormat_PCM_S16LE
	if f.Format == backend.F32LE {
		sf = frameworkv1.SampleFormat_PCM_F32LE
	}
	return &frameworkv1.AudioFormat{SampleFormat: sf, SampleRateHz: uint32(f.RateHz), Channels: uint32(f.Channels)}
}

func formatFromProto(f *frameworkv1.AudioFormat) backend.AudioFormat {
	if f == nil {
		return backend.AudioFormat{}
	}
	sf := backend.S16LE
	if f.SampleFormat == frameworkv1.SampleFormat_PCM_F32LE {
		sf = backend.F32LE
	}
	return backend.AudioFormat{Format: sf, RateHz: int(f.SampleRateHz), Channels: int(f.Channels)}
}

func (s *audioService) Capture(req *frameworkv1.CaptureAudioRequest, stream frameworkv1.AudioService_CaptureServer) error {
	appID, runID, err := s.d.requireCaller(stream.Context())
	if err != nil {
		return err
	}
	activationID := req.ActivationId
	if activationID != "" && !s.d.activationGrantAllowed(appID, activationID) {
		// Pre-roll reaches back before this capture. An arbitrary caller-chosen
		// ID would replay audio around someone else's turn; drop the request but
		// let the live, indicated capture proceed.
		s.d.logs.Warnf("audio", appID, "ignored activation %q for microphone pre-roll: not issued to this app", activationID)
		activationID = ""
	}
	cap, err := s.d.audio.StartCapture(appID, runID, req.EndpointId, formatFromProto(req.PreferredFormat), req.PacketMs, activationID, req.ConversationId)
	if err != nil {
		s.d.logs.Warnf("audio", appID, "capture denied endpoint=%s: %v", req.EndpointId, err)
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if activationID != "" {
		s.d.consumeActivationGrant(appID, activationID)
	}
	defer s.d.audio.StopCapture(cap.ID())
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-cap.Events():
			if !ok {
				return nil
			}
			out := &frameworkv1.CaptureAudioEvent{}
			switch {
			case ev.Started != nil:
				out.Event = &frameworkv1.CaptureAudioEvent_Started{Started: &frameworkv1.CaptureAudioStarted{
					StreamId: ev.Started.StreamID, ActualFormat: formatProto(ev.Started.Format),
					IndicationWatchdogMs: ev.Started.WatchdogMS,
				}}
			case ev.Chunk != nil:
				out.Event = &frameworkv1.CaptureAudioEvent_Chunk{Chunk: &frameworkv1.AudioChunk{
					Sequence: ev.Chunk.Sequence, FirstFrame: ev.Chunk.FirstFrame,
					MonotonicTimeNs: ev.Chunk.MonotonicNS, FrameCount: ev.Chunk.Frames,
					Pcm: ev.Chunk.PCM, Discontinuity: ev.Chunk.Discontinuity,
					DroppedFramesBefore: ev.Chunk.DroppedFrames,
				}}
			case ev.Notice != nil:
				out.Event = &frameworkv1.CaptureAudioEvent_Notice{Notice: &frameworkv1.AudioNotice{
					Code: ev.Notice.Code, Detail: ev.Notice.Detail,
				}}
			}
			if err := stream.Send(out); err != nil {
				return err
			}
		}
	}
}

func (s *audioService) Playback(stream frameworkv1.AudioService_PlaybackServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first Playback message must be OpenPlayback")
	}
	appID, runID, err := s.d.requireCaller(stream.Context())
	if err != nil {
		return err
	}
	pb, err := s.d.openAppPlayback(appID, runID, open.EndpointId, formatFromProto(open.Format), open.ConversationId)
	if err != nil {
		// One app may hold at most the documented number of concurrent
		// playback streams; that is a quota, not a malformed request.
		if errors.Is(err, audio.ErrPlaybackLimit) {
			return status.Error(codes.ResourceExhausted, err.Error())
		}
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	defer pb.Close()
	sendErr := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for ev := range pb.Events() {
			out := &frameworkv1.PlaybackEvent{}
			switch {
			case ev.Started != nil:
				out.Event = &frameworkv1.PlaybackEvent_Started{Started: &frameworkv1.PlaybackStarted{
					StreamId: ev.Started.StreamID, ActualFormat: formatProto(ev.Started.Format),
				}}
			case ev.Progress != nil:
				out.Event = &frameworkv1.PlaybackEvent_Progress{Progress: &frameworkv1.PlaybackProgress{
					FramesPlayed: ev.Progress.FramesPlayed, FramesQueued: ev.Progress.FramesQueued,
				}}
			case ev.Notice != nil:
				out.Event = &frameworkv1.PlaybackEvent_Notice{Notice: &frameworkv1.AudioNotice{
					Code: ev.Notice.Code, Detail: ev.Notice.Detail,
				}}
			}
			if err := stream.Send(out); err != nil {
				select {
				case sendErr <- err:
				default:
				}
				return
			}
		}
	}()
	// Cancellation must also unblock Write when a paused/full playback queue
	// is applying backpressure. Normal half-close still waits for the mixer
	// to drain and finish the event stream before this handler returns.
	go func() {
		select {
		case <-stream.Context().Done():
			pb.Close()
		case <-finished:
		}
	}()
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			pb.CloseInput()
			select {
			case <-stream.Context().Done():
				return nil
			case <-finished:
				return nil
			case err := <-sendErr:
				return err
			}
		}
		if err != nil {
			return err
		}
		switch r := req.Request.(type) {
		case *frameworkv1.PlaybackRequest_Chunk:
			if err := pb.Write(r.Chunk.Pcm); err != nil {
				return status.Error(codes.ResourceExhausted, err.Error())
			}
		case *frameworkv1.PlaybackRequest_Control:
			switch r.Control {
			case frameworkv1.PlaybackCommand_PLAYBACK_FLUSH:
				pb.Control("FLUSH")
			case frameworkv1.PlaybackCommand_PLAYBACK_PAUSE:
				pb.Control("PAUSE")
			case frameworkv1.PlaybackCommand_PLAYBACK_RESUME:
				pb.Control("RESUME")
			}
		case *frameworkv1.PlaybackRequest_Open:
			return status.Error(codes.InvalidArgument, "playback stream is already open")
		}
	}
}

func (s *audioService) GetAudioState(ctx context.Context, _ *frameworkv1.GetAudioStateRequest) (*frameworkv1.AudioState, error) {
	return s.d.audioState(), nil
}

func (s *audioService) SetMasterVolume(ctx context.Context, req *frameworkv1.SetMasterVolumeRequest) (*frameworkv1.AudioState, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "master volume is a local administrator operation"); err != nil {
		return nil, err
	}
	if req.Volume < 0 || req.Volume > 1 {
		return nil, status.Error(codes.InvalidArgument, "volume must be in [0,1]")
	}
	if err := s.d.applyMasterVolume(float64(req.Volume)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.d.audioState(), nil
}

func (s *audioService) SetMicrophoneMuted(ctx context.Context, req *frameworkv1.SetMicrophoneMutedRequest) (*frameworkv1.AudioState, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "microphone mute is a local administrator operation"); err != nil {
		return nil, err
	}
	if err := s.d.applyMicrophoneMute(req.Muted); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// The settings watcher applies the muted-indicator policy so there is a
	// single writer for that state.
	return s.d.audioState(), nil
}

// --- assistant -------------------------------------------------------------

type assistantService struct {
	frameworkv1.UnimplementedAssistantServiceServer
	d *Daemon
}

func (s *assistantService) ListWakewordModels(ctx context.Context, _ *frameworkv1.ListWakewordModelsRequest) (*frameworkv1.ListWakewordModelsResponse, error) {
	return &frameworkv1.ListWakewordModelsResponse{Models: s.d.wakeModelsProto()}, nil
}

func (s *assistantService) RecordWakewordSample(ctx context.Context, req *frameworkv1.RecordWakewordSampleRequest) (*frameworkv1.RecordWakewordSampleResponse, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "wakeword recording is a local administrator operation"); err != nil {
		return nil, err
	}
	sam, err := s.d.RecordSample(ctx, req.Label, req.DurationMs)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	s.d.publishUI(uichanges.Wakeword)
	return &frameworkv1.RecordWakewordSampleResponse{Sample: sampleProto(sam)}, nil
}

func (s *assistantService) ListWakewordSamples(ctx context.Context, _ *frameworkv1.ListWakewordSamplesRequest) (*frameworkv1.ListWakewordSamplesResponse, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "wakeword samples are a local administrator operation"); err != nil {
		return nil, err
	}
	samples, err := s.d.listSamples()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*frameworkv1.WakewordSample, 0, len(samples))
	for _, sam := range samples {
		out = append(out, sampleProto(sam))
	}
	return &frameworkv1.ListWakewordSamplesResponse{Samples: out}, nil
}

func (s *assistantService) DeleteWakewordSample(ctx context.Context, req *frameworkv1.DeleteWakewordSampleRequest) (*frameworkv1.DeleteWakewordSampleResponse, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "wakeword samples are a local administrator operation"); err != nil {
		return nil, err
	}
	if err := s.d.deleteSample(req.SampleId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.d.publishUI(uichanges.Wakeword)
	return &frameworkv1.DeleteWakewordSampleResponse{}, nil
}

func (s *assistantService) TrainWakewordClassifier(req *frameworkv1.TrainWakewordClassifierRequest, stream frameworkv1.AssistantService_TrainWakewordClassifierServer) error {
	if err := s.d.requireAdministrativeCaller(stream.Context(), "wakeword training is a local administrator operation"); err != nil {
		return err
	}
	sel := trainSelection{
		ModelID:  req.GetModelId(),
		Display:  req.DisplayName,
		Language: req.Language,
		Seed:     req.Seed,
		Word:     req.WordSampleIds,
		Room:     req.RoomSampleIds,
		Speech:   req.SpeechSampleIds,
		False:    req.FalseSampleIds,
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	var sendMu sync.Mutex
	// The last fraction the browser saw, so the terminal failure frame reports
	// real progress instead of resetting the bar.
	var sentFraction atomic.Uint32
	send := func(ev *frameworkv1.TrainWakewordEvent) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		if ev.Fraction > 0 {
			sentFraction.Store(math.Float32bits(ev.Fraction))
		}
		return stream.Send(ev)
	}
	var lastStage string
	progress := func(stage string, fraction float64) {
		if ctx.Err() != nil {
			return
		}
		msg := ""
		if stage != lastStage {
			msg = trainStageLog[stage]
			lastStage = stage
		}
		if err := send(&frameworkv1.TrainWakewordEvent{Stage: stage, Fraction: float32(fraction), Log: msg}); err != nil {
			// The browser went away; stop the fit instead of finishing a job
			// whose result nobody can receive.
			cancel()
		}
	}
	clf, err := s.d.TrainWakeword(ctx, sel, progress)
	if err != nil {
		// A typed refusal carries the cause, the failing takes and the measured
		// gates. Emit them before the stream ends with its error status, so
		// existing clients still see a terminating status but the operator gets
		// actionable detail instead of one flattened sentence.
		var failure *wakeword.TrainingFailure
		if errors.As(err, &failure) && stream.Context().Err() == nil {
			sendTrainingFailure(send, failure, math.Float32frombits(sentFraction.Load()))
		}
		return trainRPCStatus(err)
	}
	s.d.publishUI(uichanges.Wakeword)
	return send(installedEvent(clf))
}

// DeleteWakewordClassifier removes one trained word. It is an administrator
// operation, refused while a training run is active, and never removes the
// recordings: takes are shared training data.
func (s *assistantService) DeleteWakewordClassifier(ctx context.Context, req *frameworkv1.DeleteWakewordClassifierRequest) (*frameworkv1.DeleteWakewordClassifierResponse, error) {
	if err := s.d.requireAdministrativeCaller(ctx, "deleting a wakeword is a local administrator operation"); err != nil {
		return nil, err
	}
	if err := s.d.DeleteWakewordClassifier(req.GetModelId()); err != nil {
		return nil, deleteWakewordRPCStatus(err)
	}
	s.d.publishUI(uichanges.Wakeword)
	return &frameworkv1.DeleteWakewordClassifierResponse{}, nil
}

// deleteWakewordRPCStatus maps a delete failure to the code the caller can act
// on, the same way trainRPCStatus does: an id that cannot name an artifact is
// the caller's mistake, a running fit is a device precondition, and anything
// else (an unreadable or unwritable store) is a local fault.
func deleteWakewordRPCStatus(err error) error {
	var bad wakeword.ErrBadModel
	switch {
	case errors.As(err, &bad):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, errWakeTrainingBusy):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// sendTrainingFailure emits the structured refusal frame that precedes the
// stream's terminating status. The stream still ends with an error, so existing
// clients do not hang; the frame only makes the refusal actionable.
func sendTrainingFailure(send func(*frameworkv1.TrainWakewordEvent) error, failure *wakeword.TrainingFailure, fraction float32) {
	_ = send(&frameworkv1.TrainWakewordEvent{
		Stage:    "failed",
		Fraction: fraction,
		Log:      failure.Detail,
		Failure:  trainingFailureProto(failure),
	})
}

// installedEvent is the final success frame: the installed classifier plus the
// gate certificate read from the artifact, which the browser displays as-is.
func installedEvent(clf *wakeword.Classifier) *frameworkv1.TrainWakewordEvent {
	event := &frameworkv1.TrainWakewordEvent{
		Stage: "installed", Fraction: 1,
		Log: fmt.Sprintf("installed %s (threshold %.2f); select it in Settings and enable wakeword detection", clf.ID, clf.Threshold),
		Classifier: &frameworkv1.WakewordClassifierInfo{
			ModelId:         clf.ID,
			DisplayName:     clf.Display,
			Language:        clf.Language,
			Threshold:       float32(clf.Threshold),
			ContextFrames:   uint32(clf.ContextFrames),
			EmbeddingDim:    uint32(clf.EmbeddingDim),
			ProjectionWidth: uint32(clf.Projection),
			HiddenWidth:     uint32(clf.Hidden),
		},
	}
	if report, ok := clf.GateReport(); ok {
		event.Metrics = gateMetricsProto(report.Splits)
	}
	return event
}

// gateMetricsProto copies measured splits into their wire form.
func gateMetricsProto(splits []wakeword.SplitMetrics) []*frameworkv1.WakewordGateMetrics {
	out := make([]*frameworkv1.WakewordGateMetrics, 0, len(splits))
	for _, split := range splits {
		out = append(out, &frameworkv1.WakewordGateMetrics{
			Split:            split.Split,
			WordRecordings:   uint32(max(split.WordRecordings, 0)),
			Detected:         uint32(max(split.Detected, 0)),
			Recall:           float32(split.Recall),
			FalseActivations: uint32(max(split.FalseActivations, 0)),
		})
	}
	return out
}

// trainingFailureProto copies a structured refusal into its wire form.
func trainingFailureProto(failure *wakeword.TrainingFailure) *frameworkv1.WakewordTrainingFailure {
	out := &frameworkv1.WakewordTrainingFailure{
		Causes:    make([]string, 0, len(failure.Causes)),
		Takes:     make([]*frameworkv1.WakewordFailingTake, 0, len(failure.Takes)),
		Splits:    gateMetricsProto(failure.Splits),
		Threshold: float32(failure.Threshold),
		Detail:    failure.Detail,
	}
	for _, cause := range failure.Causes {
		out.Causes = append(out.Causes, string(cause))
	}
	for _, take := range failure.Takes {
		out.Takes = append(out.Takes, &frameworkv1.WakewordFailingTake{
			Partition: take.Partition,
			Index:     uint32(max(take.Index, 0)),
			SampleId:  take.SampleID,
			Cause:     string(take.Cause),
			Detail:    take.Detail,
		})
	}
	return out
}

// trainRPCStatus maps a training failure to the code the browser can act on.
// Selection mistakes are the caller's; gate and policy refusals are a
// precondition of the device and the takes; a fault in the trainer is internal.
// Cancellation keeps its own code because the Web UI distinguishes a proven
// pre-commit cancellation from a result it cannot confirm.
func trainRPCStatus(err error) error {
	var bad badSelectionError
	var failure *wakeword.TrainingFailure
	switch {
	case errors.As(err, &bad):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "training cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "training timed out")
	case errors.As(err, &failure):
		return status.Error(trainingFailureCode(failure), trainingFailureText(failure))
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// trainingFailureCode maps the primary cause to the gRPC code. A take the
// caller must fix is INVALID_ARGUMENT; a gate, policy or device input problem
// is FAILED_PRECONDITION; a trainer or artifact fault is INTERNAL.
func trainingFailureCode(failure *wakeword.TrainingFailure) codes.Code {
	switch failure.Cause() {
	case wakeword.CauseInvalidRequest,
		wakeword.CauseTooFewWordTakes,
		wakeword.CauseTooFewRoomTakes,
		wakeword.CauseTooFewSpeechTakes,
		wakeword.CauseTakeTooShort,
		wakeword.CauseUnusableTake,
		wakeword.CauseDuplicateTakes,
		wakeword.CauseShortContext,
		wakeword.CauseNoPositiveWindows,
		wakeword.CauseNoNegativeWindows,
		wakeword.CauseInsufficientHeldout:
		return codes.InvalidArgument
	case wakeword.CauseFitFailed, wakeword.CauseInternal:
		return codes.Internal
	default:
		return codes.FailedPrecondition
	}
}

// trainingFailureText names every cause and every take that must change, so a
// client that only prints the status message still gives actionable advice.
func trainingFailureText(failure *wakeword.TrainingFailure) string {
	var text strings.Builder
	text.WriteString(failure.Error())
	if len(failure.Causes) > 0 {
		causes := make([]string, 0, len(failure.Causes))
		for _, cause := range failure.Causes {
			causes = append(causes, string(cause))
		}
		label := "cause"
		if len(causes) > 1 {
			label = "causes"
		}
		text.WriteString(" [" + label + ": " + strings.Join(causes, ", ") + "]")
	}
	if len(failure.Takes) > 0 {
		takes := make([]string, 0, len(failure.Takes))
		for _, take := range failure.Takes {
			takes = append(takes, fmt.Sprintf("%s #%d", take.Partition, take.Index))
		}
		text.WriteString("; takes to fix: " + strings.Join(takes, ", "))
	}
	return text.String()
}

func sampleProto(sam sample) *frameworkv1.WakewordSample {
	return &frameworkv1.WakewordSample{
		SampleId: sam.ID, Label: sam.Label, DurationMs: sam.DurationMS, RecordedUnixMs: sam.RecordedMS,
	}
}

func (s *assistantService) BeginConversation(req *frameworkv1.BeginConversationRequest, stream frameworkv1.AssistantService_BeginConversationServer) error {
	appID, runID, ok := s.d.callerIdentity(stream.Context())
	if !ok {
		appID, runID = "", ""
	}
	conversationID := ids.New("conv")
	s.d.audio.BeginConversation(conversationID, appID, runID)
	defer s.d.audio.EndConversation(conversationID)
	if err := stream.Send(&frameworkv1.ConversationEvent{Event: &frameworkv1.ConversationEvent_Started{
		Started: &frameworkv1.ConversationStarted{ConversationId: conversationID},
	}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func (d *Daemon) registerMediaPlane(srv *grpc.Server) {
	frameworkv1.RegisterPresentationServiceServer(srv, &presentationService{d: d})
	frameworkv1.RegisterRoutingServiceServer(srv, &routingService{d: d})
	frameworkv1.RegisterInputServiceServer(srv, &inputService{d: d})
	frameworkv1.RegisterAudioServiceServer(srv, &audioService{d: d})
	frameworkv1.RegisterAssistantServiceServer(srv, &assistantService{d: d})
}
