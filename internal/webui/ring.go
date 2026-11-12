package webui

import (
	"context"
	"encoding/json"
	"fmt"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

// ringState mirrors the built-in ring app's RingService.GetState response.
// Effects and palettes are selected through the app's normal typed settings;
// this read-only card shows the effect currently rendered by the real app.
type ringState struct {
	Effect         string  `json:"effect"`
	Palette        string  `json:"palette"`
	Brightness     float64 `json:"brightness"`
	Speed          float64 `json:"speed"`
	PresentationID string  `json:"presentation_id"`
}

// findRingService identifies the ring controller from manifest provides and
// published interface metadata. GetState alone is shared with the clock, so a
// ring app must also declare led_ring_controller.
func (s *Server) findRingService(ctx context.Context, appID string, provides []string) (string, error) {
	providesRing := false
	for _, p := range provides {
		if p == "led_ring_controller" {
			providesRing = true
			break
		}
	}
	if !providesRing {
		return "", fmt.Errorf("application does not provide the LED ring controller")
	}
	ifaces, err := frameworkv1.NewAppInterfaceServiceClient(s.Conn).ListInterfaces(ctx,
		&frameworkv1.ListAppInterfacesRequest{AppId: appID})
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces.GetInterfaces() {
		desc, err := frameworkv1.NewAppInterfaceServiceClient(s.Conn).DescribeInterface(ctx,
			&frameworkv1.DescribeAppInterfaceRequest{AppId: appID, GrpcService: iface.GrpcService})
		if err != nil {
			continue
		}
		if len(desc.GetMethods()) == 1 && desc.GetMethods()[0].RpcName == "GetState" {
			return iface.GrpcService, nil
		}
	}
	return "", fmt.Errorf("no ring state service is published by this application")
}

func (s *Server) ringSnapshot(ctx context.Context, appID, service string) (*ringState, error) {
	raw, err := s.routedJSONCall(ctx, appID, service, "GetState", struct{}{})
	if err != nil {
		return nil, err
	}
	var state ringState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode ring state: %w", err)
	}
	return &state, nil
}
