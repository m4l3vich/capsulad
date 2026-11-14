package wifi

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Open chooses the real Linux implementation on a Linux target with a usable
// wireless interface and stock wpa_supplicant tooling. Anything else yields an
// explicitly unavailable manager: no simulated success is reported as hardware
// capability.
func Open(cfg Config) (Manager, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" {
		return NewSimulator(SimulatorOptions{Detail: "Wi-Fi provisioning requires the Linux target"}), nil
	}
	iface, err := detectWirelessInterface(cfg.Interface)
	if err != nil {
		return NewSimulator(SimulatorOptions{
			Detail: fmt.Sprintf("no usable Wi-Fi interface: %v", err),
		}), nil
	}
	m, err := newLinuxManager(cfg, iface)
	if err != nil {
		return NewSimulator(SimulatorOptions{
			Detail: err.Error(),
		}), nil
	}
	return m, nil
}

func detectWirelessInterface(want string) (string, error) {
	if want != "" {
		if !wirelessInterface(want) {
			return "", fmt.Errorf("%s is not a wireless interface", want)
		}
		return want, nil
	}
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return "", err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if name == "lo" || !wirelessInterface(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Errorf("no /sys/class/net entry has a wireless/phy80211 link")
	}
	return names[0], nil
}

func wirelessInterface(name string) bool {
	base := filepath.Join("/sys/class/net", name)
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		return true
	}
	return false
}

// lookTool resolves stock binaries even when the daemon's PATH omits /usr/sbin
// or /sbin. The first argument is the bare name used for test doubles.
func lookTool(r CommandRunner, name string) (string, bool) {
	candidates := []string{name}
	if !strings.Contains(name, "/") {
		candidates = append(candidates,
			filepath.Join("/usr/sbin", name),
			filepath.Join("/sbin", name),
			filepath.Join("/usr/bin", name),
			filepath.Join("/bin", name),
		)
	}
	for _, candidate := range candidates {
		if path, err := r.LookPath(candidate); err == nil && path != "" {
			return path, true
		}
	}
	return "", false
}
