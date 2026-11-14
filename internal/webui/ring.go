package webui

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	webv1 "capsulad/gen/go/capsula/web/v1"
)

// The built-in ring app's standard RingService is read over the public binary
// protobuf app-bus wire. Effects and palettes are selected through the app's
// normal typed settings; this read-only card shows what the real app is
// rendering.
const ringServiceAPI = "capsula.contracts.ring.v1.RingService"

const ringAppID = "capsula.ring"

func findRingService() (string, error) {
	return ringv1.RingService_ServiceDesc.ServiceName, nil
}

func (s *Server) ringSnapshot(ctx context.Context, appID, service string) (*ringv1.RingState, error) {
	raw, err := s.routedProtoCall(ctx, appID, service, "GetState", &ringv1.GetStateRequest{})
	if err != nil {
		return nil, err
	}
	var state ringv1.RingState
	if err := proto.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode ring state: %w", err)
	}
	return &state, nil
}

func ringView(state *ringv1.RingState) *webv1.RingView {
	if state == nil {
		return nil
	}
	return &webv1.RingView{
		Effect: state.GetEffect(), Palette: state.GetPalette(), Brightness: state.GetBrightness(),
		Speed: state.GetSpeed(), PresentationId: state.GetPresentationId(), Active: state.GetActive(),
		SettingsRevision: state.GetSettingsRevision(),
	}
}
