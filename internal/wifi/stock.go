package wifi

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var stockConfigPaths = []string{
	"/etc/wpa_supplicant.conf",
	"/etc/wpa_supplicant/wpa_supplicant.conf",
}

// importStockNetworks adopts the vendor/stock wpa_supplicant configuration
// once, before the daemon terminates that supplicant. The stock file may hold
// plaintext passphrases; only derived PSKs are written to the private 0600
// store, and its contents are never logged.
func (m *linuxManager) importStockNetworks(ctx context.Context) error {
	if m.store.stockImported() {
		return nil
	}
	stock, found, err := readStockNetworks(m.stockConfigs)
	if err != nil {
		return err
	}
	if !found || len(stock) == 0 {
		return m.store.markStockImported()
	}
	active := m.stockActiveSSID(ctx)
	list, err := m.store.list()
	if err != nil {
		return err
	}
	present := make(map[string]int, len(list))
	maxPriority := 0
	for i := range list {
		present[list[i].SSID] = i
		if list[i].Priority > maxPriority {
			maxPriority = list[i].Priority
		}
	}
	imported := 0
	for _, n := range stock {
		if idx, ok := present[n.SSID]; ok {
			if n.SSID == active {
				list[idx].Priority = maxPriority + 1
				maxPriority++
			}
			continue
		}
		if n.SSID == active {
			n.Priority = maxPriority + 1
			maxPriority++
		}
		list = append(list, n)
		present[n.SSID] = len(list) - 1
		imported++
	}
	if active != "" {
		if idx, ok := present[active]; ok && list[idx].Priority <= maxPriority {
			list[idx].Priority = maxPriority + 1
		}
	}
	sortStoredNetworks(list)
	if err := m.store.replace(list); err != nil {
		return err
	}
	if imported > 0 {
		m.cfg.logf("adopted %d stock Wi-Fi network(s)", imported)
	}
	return m.store.markStockImported()
}

func (m *linuxManager) stockActiveSSID(ctx context.Context) string {
	if m.tools.cli == "" {
		return ""
	}
	for _, dir := range []string{"/var/run/wpa_supplicant", "/run/wpa_supplicant"} {
		out, err := m.run(ctx, 3*time.Second, m.tools.cli, "-p", dir, "-i", m.iface, "status")
		if err != nil {
			continue
		}
		status := parseSupplicantStatus(out)
		if status.State == "COMPLETED" && status.SSID != "" {
			return status.SSID
		}
	}
	return ""
}

func readStockNetworks(paths []string) ([]storedNetwork, bool, error) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, true, fmt.Errorf("wifi: read stock supplicant config: %w", err)
		}
		return parseStockConfig(string(data)), true, nil
	}
	return nil, false, nil
}

func sortStoredNetworks(list []storedNetwork) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Priority != list[j].Priority {
			return list[i].Priority > list[j].Priority
		}
		return list[i].SSID < list[j].SSID
	})
}

// parseStockConfig is deliberately small and non-logging. Unsupported blocks
// (WEP, malformed values) are skipped rather than copied into the private
// store.
func parseStockConfig(data string) []storedNetwork {
	var out []storedNetwork
	var block []string
	inBlock := false
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(stripWPAComment(raw))
		if line == "" {
			continue
		}
		if !inBlock {
			if strings.HasPrefix(line, "network") && strings.Contains(line, "{") {
				inBlock = true
				block = block[:0]
			}
			continue
		}
		if strings.HasPrefix(line, "}") {
			if n, ok := parseStockBlock(block); ok {
				out = append(out, n)
			}
			inBlock = false
			continue
		}
		block = append(block, line)
	}
	return dedupeStoredNetworks(out)
}

func parseStockBlock(lines []string) (storedNetwork, bool) {
	values := map[string]string{}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	ssid := unquoteWPA(values["ssid"])
	if !validSSID(ssid) {
		return storedNetwork{}, false
	}
	if truthyWPA(values["disabled"]) {
		return storedNetwork{}, false
	}
	if values["wep_key0"] != "" || values["wep_key1"] != "" {
		return storedNetwork{}, false
	}
	km := strings.ToLower(values["key_mgmt"])
	proto := strings.ToLower(values["proto"])
	rawPSK := values["psk"]
	security := SecurityWPA2
	switch {
	case strings.Contains(km, "sae"):
		if strings.Contains(km, "wpa-psk") || strings.Contains(km, "wpa2-psk") {
			security = SecurityWPA2WPA3
		} else {
			security = SecurityWPA3
		}
	case strings.Contains(km, "wpa-psk"), strings.Contains(km, "wpa2-psk"):
		if strings.Contains(proto, "wpa") && !strings.Contains(proto, "rsn") && !strings.Contains(proto, "wpa2") {
			security = SecurityWPA
		} else {
			security = SecurityWPA2
		}
	case strings.Contains(km, "none") || rawPSK == "":
		security = SecurityOpen
	}
	n := storedNetwork{SSID: ssid, Security: security}
	if p, err := strconv.Atoi(values["priority"]); err == nil {
		n.Priority = p
	}
	if security != SecurityOpen {
		psk, ok := stockPSK(ssid, rawPSK)
		if !ok {
			return storedNetwork{}, false
		}
		n.PSK = psk
	}
	return n, true
}

func stockPSK(ssid, raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	quoted := strings.HasPrefix(raw, `"`)
	value := unquoteWPA(raw)
	if !quoted && len(value) == 64 && validPSK(value) {
		return strings.ToLower(value), true
	}
	psk, err := derivePSK(ssid, value)
	if err != nil {
		return "", false
	}
	return psk, true
}

func dedupeStoredNetworks(list []storedNetwork) []storedNetwork {
	best := map[string]storedNetwork{}
	for _, n := range list {
		if prev, ok := best[n.SSID]; ok && prev.Priority >= n.Priority {
			continue
		}
		best[n.SSID] = n
	}
	out := make([]storedNetwork, 0, len(best))
	for _, n := range best {
		out = append(out, n)
	}
	sortStoredNetworks(out)
	return out
}

func truthyWPA(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func unquoteWPA(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return value
	}
	body := value[1 : len(value)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' || i+1 >= len(body) {
			b.WriteByte(body[i])
			continue
		}
		i++
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'x':
			if i+2 < len(body) {
				if v, err := strconv.ParseUint(body[i+1:i+3], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteByte('x')
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String()
}

func stripWPAComment(line string) string {
	quoted := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quoted {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
			continue
		}
		if c == '#' && !quoted {
			return line[:i]
		}
	}
	return line
}
