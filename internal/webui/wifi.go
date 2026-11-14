package webui

import (
	"context"
	"fmt"
	"strings"
	"time"

	webv1 "capsulad/gen/go/capsula/web/v1"
	"capsulad/internal/uichanges"
	"capsulad/internal/wifi"
)

// The Wi-Fi surface is status + explicit commands. Snapshots and events never
// scan: scan_results is populated only by a Scan result, so opening Settings
// or subscribing cannot energize the radio.
func (s *Server) wifiView(ctx context.Context) *webv1.WifiView {
	if s.Wifi == nil {
		return &webv1.WifiView{Available: false, State: string(wifi.StateUnavailable),
			Detail: "Wi-Fi is not configured on this daemon"}
	}
	view := &webv1.WifiView{}
	status, err := s.Wifi.Status(ctx)
	if err != nil {
		view.Available = false
		view.State = string(wifi.StateError)
		view.LastError = rpcMessage(err)
		return view
	}
	view.Revision = status.Revision
	view.Available = status.Available
	view.State = string(status.State)
	view.Interface = status.Interface
	view.Ssid = status.SSID
	view.IpAddress = status.IPAddress
	view.Detail = status.Detail
	view.ApActive = status.APActive
	view.ApSsid = status.APSSID
	view.ApPortalUrl = status.APPortalURL
	view.LastError = status.LastError
	view.ScanSupported = status.ScanSupported
	if status.APActive {
		view.ApPasswordHint = wifi.APPassword
	}
	known, err := s.Wifi.KnownNetworks(ctx)
	if err != nil {
		view.LastError = rpcMessage(err)
		return view
	}
	for _, n := range known {
		view.KnownNetworks = append(view.KnownNetworks, &webv1.WifiKnownNetwork{
			Ssid: n.SSID, Security: n.Security, Connected: n.Connected, HasPassword: n.HasPassword,
		})
	}
	return view
}

func wifiScanViews(results []wifi.ScanResult) []*webv1.WifiScanResult {
	out := make([]*webv1.WifiScanResult, 0, len(results))
	for _, r := range results {
		out = append(out, &webv1.WifiScanResult{
			Ssid: r.SSID, Security: r.Security, SignalDbm: int32(r.SignalDBM),
			FrequencyMhz: r.FrequencyMHz, Connected: r.Connected,
		})
	}
	return out
}

func (s *Server) opWifi(c *wsConn, reqID uint64, cmd *webv1.WifiCommand) {
	if s.Wifi == nil {
		c.sendError(reqID, errNotFound, "Wi-Fi is not configured on this daemon")
		return
	}
	if cmd == nil || cmd.GetAction() == nil {
		c.sendError(reqID, errInvalid, "wifi action is required")
		return
	}
	switch action := cmd.GetAction().(type) {
	case *webv1.WifiCommand_Scan:
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		results, err := s.Wifi.Scan(ctx)
		if err != nil {
			c.sendError(reqID, classifyError(err), rpcMessage(err))
			return
		}
		view := s.wifiView(ctx)
		view.ScanResults = wifiScanViews(results)
		message := fmt.Sprintf("Scan complete: %d network(s)", len(results))
		c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Wifi{Wifi: &webv1.WifiResult{
			Message: message, Wifi: view,
		}}})
	case *webv1.WifiCommand_Connect:
		ssid := strings.TrimSpace(action.Connect.GetSsid())
		if ssid == "" {
			c.sendError(reqID, errInvalid, "network name is required")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// The passphrase is forwarded once and never logged, stored in this
		// process, or included in a response.
		if err := s.Wifi.Connect(ctx, wifi.ConnectRequest{
			SSID: ssid, Passphrase: action.Connect.GetPassphrase(), Security: action.Connect.GetSecurity(),
		}); err != nil {
			c.sendError(reqID, classifyError(err), rpcMessage(err))
			return
		}
		view := s.wifiView(ctx)
		c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Wifi{Wifi: &webv1.WifiResult{
			Message: "Wi-Fi network saved and selected: " + ssid, Wifi: view,
		}}})
	case *webv1.WifiCommand_Forget:
		ssid := strings.TrimSpace(action.Forget.GetSsid())
		if ssid == "" {
			c.sendError(reqID, errInvalid, "network name is required")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Wifi.Forget(ctx, ssid); err != nil {
			c.sendError(reqID, classifyError(err), rpcMessage(err))
			return
		}
		view := s.wifiView(ctx)
		c.sendResult(reqID, &webv1.Result{Value: &webv1.Result_Wifi{Wifi: &webv1.WifiResult{
			Message: "Wi-Fi network forgotten: " + ssid, Wifi: view,
		}}})
	default:
		c.sendError(reqID, errInvalid, "unknown wifi action")
	}
}

// watchWifiSignals republishes status when the backend publishes an
// invalidation. It never scans.
func (s *Server) watchWifiSignals(ctx context.Context, c *wsConn, signals <-chan uichanges.Topic) {
	if signals == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case signal, ok := <-signals:
			if !ok {
				return
			}
			if signal != uichanges.Wifi {
				continue
			}
			viewCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			view := s.wifiView(viewCtx)
			cancel()
			c.sendEvent(&webv1.Event{Topic: webv1.Topic_TOPIC_WIFI, Body: &webv1.Event_Wifi{Wifi: view}})
		}
	}
}
