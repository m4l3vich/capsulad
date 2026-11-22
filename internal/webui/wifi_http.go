package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"capsulad/internal/wifi"
)

// The HTTPS Wi-Fi routes are the machine-facing form of the same controller
// the WebSocket surface uses. They exist so the installer's terminal client
// drives one control plane instead of shelling out to a second daemon. A view
// read never scans; only an explicit POST /api/wifi/scan energizes the radio.

// wifiViewJSON is the snake_case HTTP projection. Passphrases and derived PSKs
// have no field here.
type wifiViewJSON struct {
	Available      bool                   `json:"available"`
	State          string                 `json:"state"`
	Interface      string                 `json:"interface"`
	SSID           string                 `json:"ssid"`
	IPAddress      string                 `json:"ip_address"`
	Security       string                 `json:"security"`
	Detail         string                 `json:"detail"`
	APActive       bool                   `json:"ap_active"`
	APSSID         string                 `json:"ap_ssid"`
	APAddress      string                 `json:"ap_address"`
	APPortalURL    string                 `json:"ap_portal_url"`
	APPasswordHint string                 `json:"ap_password_hint"`
	ScanSupported  bool                   `json:"scan_supported"`
	LastError      string                 `json:"last_error"`
	KnownNetworks  []wifiKnownNetworkJSON `json:"known_networks"`
}

type wifiKnownNetworkJSON struct {
	SSID        string `json:"ssid"`
	Security    string `json:"security"`
	HasPassword bool   `json:"has_password"`
	Connected   bool   `json:"connected"`
}

type wifiScanResultJSON struct {
	SSID         string `json:"ssid"`
	Security     string `json:"security"`
	SignalDBM    int    `json:"signal_dbm"`
	FrequencyMHz uint32 `json:"frequency_mhz"`
	Connected    bool   `json:"connected"`
}

type wifiConnectBody struct {
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Security   string `json:"security"`
}

type wifiForgetBody struct {
	SSID string `json:"ssid"`
}

// httpWifiView builds the redacted snapshot. A controller that cannot report
// status still yields a view: the caller gets an honest error state instead of
// a transport failure for a read-only request.
func (s *Server) httpWifiView(ctx context.Context) wifiViewJSON {
	view := wifiViewJSON{
		State:         string(wifi.StateUnavailable),
		Detail:        "Wi-Fi is not configured on this daemon",
		KnownNetworks: []wifiKnownNetworkJSON{},
	}
	if s.Wifi == nil {
		return view
	}
	status, err := s.Wifi.Status(ctx)
	if err != nil {
		view.State = string(wifi.StateError)
		view.LastError = err.Error()
		return view
	}
	view.Available = status.Available
	view.State = string(status.State)
	view.Interface = status.Interface
	view.SSID = status.SSID
	view.IPAddress = status.IPAddress
	view.Security = status.Security
	view.Detail = status.Detail
	view.APActive = status.APActive
	view.APSSID = status.APSSID
	view.APAddress = status.APAddress
	view.APPortalURL = status.APPortalURL
	view.ScanSupported = status.ScanSupported
	view.LastError = status.LastError
	if status.APActive {
		view.APPasswordHint = wifi.APPassword
	}
	known, err := s.Wifi.KnownNetworks(ctx)
	if err != nil {
		view.LastError = err.Error()
		return view
	}
	for _, n := range known {
		view.KnownNetworks = append(view.KnownNetworks, wifiKnownNetworkJSON{
			SSID: n.SSID, Security: n.Security, HasPassword: n.HasPassword, Connected: n.Connected,
		})
	}
	return view
}

// httpWifiStatus returns the current view without scanning.
func (s *Server) httpWifiStatus(w http.ResponseWriter, r *http.Request) {
	if s.Wifi == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Wi-Fi is not configured on this daemon")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSONValue(w, http.StatusOK, map[string]any{"wifi": s.httpWifiView(ctx)})
}

func (s *Server) httpWifiScan(w http.ResponseWriter, r *http.Request) {
	if s.Wifi == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Wi-Fi is not configured on this daemon")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	results, err := s.Wifi.Scan(ctx)
	if err != nil {
		writeWifiError(w, err)
		return
	}
	out := make([]wifiScanResultJSON, 0, len(results))
	for _, result := range results {
		out = append(out, wifiScanResultJSON{
			SSID: result.SSID, Security: result.Security, SignalDBM: result.SignalDBM,
			FrequencyMHz: result.FrequencyMHz, Connected: result.Connected,
		})
	}
	writeJSONValue(w, http.StatusOK, map[string]any{
		"message": fmt.Sprintf("Scan complete: %d network(s) found", len(results)),
		"results": out,
		"wifi":    s.httpWifiView(ctx),
	})
}

func (s *Server) httpWifiConnect(w http.ResponseWriter, r *http.Request) {
	if s.Wifi == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Wi-Fi is not configured on this daemon")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body wifiConnectBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ssid := strings.TrimSpace(body.SSID)
	if ssid == "" {
		writeJSONError(w, http.StatusBadRequest, "network name is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// The passphrase is forwarded once and never logged or echoed.
	if err := s.Wifi.Connect(ctx, wifi.ConnectRequest{
		SSID: ssid, Passphrase: body.Passphrase, Security: body.Security,
	}); err != nil {
		writeWifiError(w, err)
		return
	}
	writeJSONValue(w, http.StatusOK, map[string]any{
		"message": "Wi-Fi network saved and selected: " + ssid,
		"wifi":    s.httpWifiView(ctx),
	})
}

func (s *Server) httpWifiForget(w http.ResponseWriter, r *http.Request) {
	if s.Wifi == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Wi-Fi is not configured on this daemon")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body wifiForgetBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ssid := strings.TrimSpace(body.SSID)
	if ssid == "" {
		writeJSONError(w, http.StatusBadRequest, "network name is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Wifi.Forget(ctx, ssid); err != nil {
		writeWifiError(w, err)
		return
	}
	writeJSONValue(w, http.StatusOK, map[string]any{
		"message": "Wi-Fi network forgotten: " + ssid,
		"wifi":    s.httpWifiView(ctx),
	})
}

// writeWifiError maps controller failures onto meaningful HTTP statuses. The
// message is the controller's own safe detail.
func writeWifiError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, wifi.ErrInvalidNetwork):
		status = http.StatusBadRequest
	case errors.Is(err, wifi.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, wifi.ErrUnsupported):
		status = http.StatusNotImplemented
	case errors.Is(err, wifi.ErrUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status = http.StatusGatewayTimeout
	}
	writeJSONError(w, status, err.Error())
}

// wifiMethodNotAllowed keeps an unsupported method on a Wi-Fi route a JSON 405
// instead of letting the SPA fallback serve index.html.
func wifiMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
}
