package wifi

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// stationConfig renders the station-mode supplicant configuration from the
// saved networks. It never contains a plaintext passphrase: only the derived
// PSK is written, and the manager creates it with mode 0600.
func stationConfig(ctrlDir string, networks []storedNetwork) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ctrl_interface=%s\n", ctrlDir)
	b.WriteString("ctrl_interface_group=0\n")
	b.WriteString("update_config=0\n")
	b.WriteString("ap_scan=1\n")
	for _, n := range networks {
		b.WriteString("\nnetwork={\n")
		fmt.Fprintf(&b, "\tssid=%s\n", quoteSSID(n.SSID))
		switch n.Security {
		case SecurityOpen:
			b.WriteString("\tkey_mgmt=NONE\n")
		case SecurityWPA:
			fmt.Fprintf(&b, "\tpsk=%s\n", n.PSK)
			b.WriteString("\tkey_mgmt=WPA-PSK\n")
			b.WriteString("\tproto=WPA\n")
		case SecurityWPA3:
			fmt.Fprintf(&b, "\tpsk=%s\n", n.PSK)
			b.WriteString("\tkey_mgmt=SAE\n")
			b.WriteString("\tproto=RSN\n")
			b.WriteString("\tieee80211w=2\n")
		case SecurityWPA2WPA3:
			fmt.Fprintf(&b, "\tpsk=%s\n", n.PSK)
			b.WriteString("\tkey_mgmt=WPA-PSK SAE\n")
			b.WriteString("\tproto=RSN\n")
			b.WriteString("\tieee80211w=1\n")
		default:
			fmt.Fprintf(&b, "\tpsk=%s\n", n.PSK)
			b.WriteString("\tkey_mgmt=WPA-PSK\n")
			b.WriteString("\tproto=RSN\n")
		}
		fmt.Fprintf(&b, "\tpriority=%d\n", n.Priority)
		b.WriteString("}\n")
	}
	return b.String()
}

// apConfig renders a WPA2-PSK access point. The SSID is the Bluetooth
// advertised name; the password is the documented provisioning password.
// Channel 1 (2412 MHz) is a fixed, globally valid 2.4 GHz channel and avoids
// depending on an extra scan before the AP can exist.
func apConfig(ctrlDir, ssid, psk string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ctrl_interface=%s\n", ctrlDir)
	b.WriteString("ctrl_interface_group=0\n")
	b.WriteString("update_config=0\n")
	b.WriteString("\nnetwork={\n")
	fmt.Fprintf(&b, "\tssid=%s\n", quoteSSID(ssid))
	fmt.Fprintf(&b, "\tpsk=%s\n", psk)
	b.WriteString("\tkey_mgmt=WPA-PSK\n")
	b.WriteString("\tproto=RSN\n")
	b.WriteString("\tpairwise=CCMP\n")
	b.WriteString("\tgroup=CCMP\n")
	b.WriteString("\tauth_alg=OPEN\n")
	b.WriteString("\tmode=2\n")
	b.WriteString("\tfrequency=2412\n")
	b.WriteString("}\n")
	return b.String()
}

type supplicantStatus struct {
	State     string
	SSID      string
	IPAddress string
	BSSID     string
	Frequency string
	KeyMgmt   string
}

func parseSupplicantStatus(out string) supplicantStatus {
	var st supplicantStatus
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "wpa_state":
			st.State = value
		case "ssid":
			st.SSID = value
		case "ip_address":
			st.IPAddress = value
		case "bssid":
			st.BSSID = value
		case "freq":
			st.Frequency = value
		case "key_mgmt":
			st.KeyMgmt = value
		}
	}
	return st
}

type networkRef struct {
	ID   int
	SSID string
}

// parseListNetworks reads the tab-separated `list_networks` table. SSIDs may
// contain tabs in theory; the first two fields are the stable prefix and the
// remainder up to the BSSID/flags columns is not needed here.
func parseListNetworks(out string) []networkRef {
	var refs []networkRef
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "network id") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			fields = strings.Fields(line)
		}
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		refs = append(refs, networkRef{ID: id, SSID: fields[1]})
	}
	return refs
}

func networkID(refs []networkRef, ssid string) (int, bool) {
	for _, ref := range refs {
		if ref.SSID == ssid {
			return ref.ID, true
		}
	}
	return 0, false
}

// parseScanResults reads the tab-separated `scan_results` table. Hidden
// networks with an empty SSID are skipped: a manual SSID can still be entered
// in the connect form.
func parseScanResults(out string, currentSSID string) []ScanResult {
	seen := map[string]ScanResult{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "bssid") {
			continue
		}
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) < 5 {
			continue
		}
		ssid := fields[4]
		if !validSSID(ssid) {
			continue
		}
		freq, _ := strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 32)
		signal, _ := strconv.Atoi(strings.TrimSpace(fields[2]))
		result := ScanResult{
			SSID:         ssid,
			Security:     securityFromFlags(fields[3]),
			SignalDBM:    signal,
			FrequencyMHz: uint32(freq),
			Connected:    ssid == currentSSID,
		}
		// One row per BSSID. Keep the strongest BSS for an SSID so the list is
		// a network picker rather than a radio dump.
		if prev, ok := seen[ssid]; !ok || result.SignalDBM > prev.SignalDBM {
			seen[ssid] = result
		}
	}
	outList := make([]ScanResult, 0, len(seen))
	for _, r := range seen {
		outList = append(outList, r)
	}
	sort.Slice(outList, func(i, j int) bool {
		if outList[i].SignalDBM != outList[j].SignalDBM {
			return outList[i].SignalDBM > outList[j].SignalDBM
		}
		return outList[i].SSID < outList[j].SSID
	})
	return outList
}

func securityFromFlags(flags string) string {
	f := strings.ToLower(flags)
	switch {
	case strings.Contains(f, "wep"):
		return SecurityWEP
	case strings.Contains(f, "sae") || strings.Contains(f, "wpa3"):
		if strings.Contains(f, "wpa2") || strings.Contains(f, "psk") {
			return SecurityWPA2WPA3
		}
		return SecurityWPA3
	case strings.Contains(f, "wpa2") || strings.Contains(f, "rsn"):
		return SecurityWPA2
	case strings.Contains(f, "wpa"):
		return SecurityWPA
	default:
		return SecurityOpen
	}
}

// parseIWScan reads the optional `iw dev <iface> scan` fallback used when the
// supplicant refuses SCAN in AP mode. The stock image has no iw binary, so this
// path exists only for other supported Linux installations.
func parseIWScan(out string) []ScanResult {
	var (
		results []ScanResult
		cur     *ScanResult
		rsn     bool
		sae     bool
		wpa     bool
		wep     bool
	)
	flush := func() {
		if cur == nil || !validSSID(cur.SSID) {
			cur = nil
			rsn, sae, wpa, wep = false, false, false, false
			return
		}
		switch {
		case wep && !rsn && !wpa:
			cur.Security = SecurityWEP
		case sae && rsn:
			cur.Security = SecurityWPA3
		case rsn:
			cur.Security = SecurityWPA2
		case wpa:
			cur.Security = SecurityWPA
		default:
			cur.Security = SecurityOpen
		}
		results = append(results, *cur)
		cur = nil
		rsn, sae, wpa, wep = false, false, false, false
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "BSS ") {
			flush()
			cur = &ScanResult{}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "SSID:"):
			cur.SSID = strings.TrimSpace(strings.TrimPrefix(line, "SSID:"))
		case strings.HasPrefix(line, "freq:"):
			fields := strings.Fields(strings.TrimPrefix(line, "freq:"))
			if len(fields) > 0 {
				if f, err := strconv.ParseFloat(fields[0], 64); err == nil {
					cur.FrequencyMHz = uint32(f)
				}
			}
		case strings.HasPrefix(line, "signal:"):
			fields := strings.Fields(strings.TrimPrefix(line, "signal:"))
			if len(fields) > 0 {
				if s, err := strconv.ParseFloat(fields[0], 64); err == nil {
					cur.SignalDBM = int(s)
				}
			}
		case strings.HasPrefix(line, "RSN:"):
			rsn = true
		case strings.Contains(line, "Authentication suites: SAE"):
			sae = true
		case strings.HasPrefix(line, "WPA:"):
			wpa = true
		case strings.HasPrefix(line, "WEP:"), strings.Contains(line, "capability:") && strings.Contains(line, "Privacy"):
			wep = true
		}
	}
	flush()
	sort.Slice(results, func(i, j int) bool {
		if results[i].SignalDBM != results[j].SignalDBM {
			return results[i].SignalDBM > results[j].SignalDBM
		}
		return results[i].SSID < results[j].SSID
	})
	return results
}

// parseIPAddress reads the first IPv4 address from `ip -4 -o addr show dev X`.
func parseIPAddress(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field != "inet" || i+1 >= len(fields) {
				continue
			}
			addr, _, _ := strings.Cut(fields[i+1], "/")
			if addr != "" {
				return addr
			}
		}
	}
	return ""
}
