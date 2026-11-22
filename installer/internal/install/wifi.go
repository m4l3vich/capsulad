package install

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// WiFi is the status object returned by `wifi status|scan|connect|forget` with
// the global -json flag.
type WiFi struct {
	Available      bool           `json:"available"`
	State          string         `json:"state"`
	Interface      string         `json:"interface"`
	SSID           string         `json:"ssid"`
	IPAddress      string         `json:"ip_address"`
	Security       string         `json:"security"`
	Detail         string         `json:"detail"`
	APActive       bool           `json:"ap_active"`
	APSSID         string         `json:"ap_ssid"`
	APAddress      string         `json:"ap_address"`
	APPortalURL    string         `json:"ap_portal_url"`
	APPasswordHint string         `json:"ap_password_hint"`
	ScanSupported  bool           `json:"scan_supported"`
	LastError      string         `json:"last_error"`
	KnownNetworks  []KnownNetwork `json:"known_networks"`
}

// KnownNetwork is one remembered network.
type KnownNetwork struct {
	SSID        string `json:"ssid"`
	Security    string `json:"security"`
	HasPassword bool   `json:"has_password"`
	Connected   bool   `json:"connected"`
}

// Network is one scan result.
type Network struct {
	SSID         string `json:"ssid"`
	Security     string `json:"security"`
	SignalDBM    int    `json:"signal_dbm"`
	FrequencyMHz int    `json:"frequency_mhz"`
	Connected    bool   `json:"connected"`
}

// WiFiStatusResponse is the status/connect/forget envelope.
type WiFiStatusResponse struct {
	Message string `json:"message"`
	WiFi    WiFi   `json:"wifi"`
}

// WiFiScanResponse is the scan envelope.
type WiFiScanResponse struct {
	Message string    `json:"message"`
	Results []Network `json:"results"`
	WiFi    WiFi      `json:"wifi"`
}

// ParseWiFiStatus decodes a status, connect or forget reply.
func ParseWiFiStatus(data []byte) (WiFiStatusResponse, error) {
	var out WiFiStatusResponse
	if err := json.Unmarshal(trimJSON(data), &out); err != nil {
		return out, fmt.Errorf("parse wifi json: %w", err)
	}
	return out, nil
}

// ParseWiFiScan decodes a scan reply.
func ParseWiFiScan(data []byte) (WiFiScanResponse, error) {
	var out WiFiScanResponse
	if err := json.Unmarshal(trimJSON(data), &out); err != nil {
		return out, fmt.Errorf("parse wifi scan json: %w", err)
	}
	return out, nil
}

// Connected reports a usable station link: connected with an IPv4/IPv6
// address. A stale "connected" state without an address is not usable.
func (w WiFi) Connected() bool {
	return strings.EqualFold(w.State, "connected") && strings.TrimSpace(w.IPAddress) != ""
}

// DedupeNetworks keeps the strongest advertisement per SSID, never hides the
// connected one and sorts usable networks first.
func DedupeNetworks(nets []Network) []Network {
	best := map[string]Network{}
	var order []string
	for _, n := range nets {
		ssid := strings.TrimSpace(n.SSID)
		if ssid == "" || ValidateSSID(ssid) != nil {
			continue
		}
		n.SSID = ssid
		prev, ok := best[ssid]
		if !ok {
			best[ssid] = n
			order = append(order, ssid)
			continue
		}
		if n.Connected && !prev.Connected {
			best[ssid] = n
			continue
		}
		if n.Connected == prev.Connected && n.SignalDBM > prev.SignalDBM {
			best[ssid] = n
		}
	}
	out := make([]Network, 0, len(order))
	for _, ssid := range order {
		out = append(out, best[ssid])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Connected != out[j].Connected {
			return out[i].Connected
		}
		if out[i].SignalDBM != out[j].SignalDBM {
			return out[i].SignalDBM > out[j].SignalDBM
		}
		return out[i].SSID < out[j].SSID
	})
	return out
}

// ValidateSSID rejects values that cannot be passed safely to the CLI or a
// terminal. A leading dash would be parsed as a flag; control characters would
// corrupt terminal output.
func ValidateSSID(ssid string) error {
	if strings.TrimSpace(ssid) == "" {
		return fmt.Errorf("SSID is empty")
	}
	if strings.HasPrefix(ssid, "-") {
		return fmt.Errorf("SSID %q starts with '-'; enter it manually with a different spelling or choose another network", ssid)
	}
	for _, r := range ssid {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Errorf("SSID contains a control character")
		}
	}
	return nil
}

// SecurityNeedsPassword reports whether a scan security label requires a
// passphrase. An empty label means an open network.
func SecurityNeedsPassword(security string) bool {
	s := strings.ToLower(strings.TrimSpace(security))
	if s == "" || s == "open" || s == "none" {
		return false
	}
	return true
}

// SecurityMode maps a scan result's label to the CLI's explicit `-security`
// value. An empty or open/none label becomes "open"; any other mode is passed
// through as reported. A manual secured entry with no selected mode passes ""
// so the backend keeps its documented WPA2 default.
func SecurityMode(label string) string {
	s := strings.TrimSpace(label)
	switch strings.ToLower(s) {
	case "", "open", "none":
		return "open"
	}
	return s
}

func trimJSON(data []byte) []byte {
	s := strings.TrimSpace(string(data))
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	return []byte(s)
}
