package wifi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct {
	mu      sync.Mutex
	done    chan struct{}
	once    sync.Once
	out     string
	signals []os.Signal
}

func newFakeProcess(ctx context.Context, out string) *fakeProcess {
	p := &fakeProcess{done: make(chan struct{}), out: out}
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = p.Kill()
			case <-p.done:
			}
		}()
	}
	return p
}

func (p *fakeProcess) Wait() error {
	<-p.done
	return nil
}

func (p *fakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *fakeProcess) Kill() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *fakeProcess) Output() string { return p.out }

type runCall struct {
	name string
	args []string
}

type fakeRunner struct {
	mu   sync.Mutex
	path map[string]string

	runs   []runCall
	starts []runCall

	started         int
	connected       bool
	ssid            string
	networks        []string
	scanRows        []string
	scanRequests    int
	scanFAIL        bool
	connectOnSelect bool
	failSSID        string
	statusOverride  func() (string, error)
	runFunc         func(ctx context.Context, name string, args []string) (string, error)
	startFunc       func(name string, args []string) (Process, error)
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		path: map[string]string{
			"wpa_supplicant": "/usr/sbin/wpa_supplicant",
			"wpa_cli":        "/usr/sbin/wpa_cli",
			"ip":             "/sbin/ip",
			"dnsmasq":        "/usr/sbin/dnsmasq",
			"dhcpcd":         "/sbin/dhcpcd",
		},
		ssid: "Home",
	}
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path, ok := f.path[name]; ok {
		return path, nil
	}
	return "", os.ErrNotExist
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, runCall{name: name, args: append([]string(nil), args...)})
	fn := f.runFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, args)
	}
	return f.defaultRun(name, args)
}

func (f *fakeRunner) Start(ctx context.Context, name string, args ...string) (Process, error) {
	f.mu.Lock()
	f.starts = append(f.starts, runCall{name: name, args: append([]string(nil), args...)})
	f.started++
	fn := f.startFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(name, args)
	}
	return newFakeProcess(ctx, ""), nil
}

func (f *fakeRunner) defaultRun(name string, args []string) (string, error) {
	switch {
	case strings.HasSuffix(name, "wpa_cli"):
		return f.wpaCLI(args)
	case strings.HasSuffix(name, "ip"):
		return f.ip(args)
	case strings.HasSuffix(name, "dhcpcd"):
		return "", nil
	default:
		return "", fmt.Errorf("fake runner: unexpected command %s %v", name, args)
	}
}

func (f *fakeRunner) wpaCLI(args []string) (string, error) {
	dir := ""
	cmd := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-p":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case "-i":
			if i+1 < len(args) {
				i++
			}
		default:
			// wpa_cli takes flags first, then one command and its operand:
			// `wpa_cli -i wlan0 select_network 0`.
			if !strings.HasPrefix(arg, "-") && cmd == "" {
				cmd = arg
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd {
	case "ping":
		if dir == "/var/run/wpa_supplicant" || dir == "/run/wpa_supplicant" {
			return "", errors.New("no stock supplicant")
		}
		if f.started > 0 {
			return "PONG", nil
		}
		return "", errors.New("no supplicant")
	case "terminate":
		f.started = 0
		return "OK", nil
	case "status":
		if f.statusOverride != nil {
			return f.statusOverride()
		}
		if f.connected {
			return "wpa_state=COMPLETED\nssid=" + f.ssid + "\nip_address=192.168.1.5\nbssid=aa:bb:cc:dd:ee:ff", nil
		}
		return "wpa_state=DISCONNECTED", nil
	case "list_networks":
		var b strings.Builder
		b.WriteString("network id / ssid / bssid / flags\n")
		for i, ssid := range f.networks {
			fmt.Fprintf(&b, "%d\t%s\tany\t\n", i, ssid)
		}
		return b.String(), nil
	case "scan":
		f.scanRequests++
		if f.scanFAIL {
			return "FAIL", nil
		}
		return "OK", nil
	case "scan_results":
		if len(f.scanRows) == 0 {
			return "bssid / frequency / signal level / flags / ssid\n", nil
		}
		return "bssid / frequency / signal level / flags / ssid\n" + strings.Join(f.scanRows, "\n"), nil
	case "select_network":
		selected := ""
		if len(args) > 0 {
			if id, err := strconv.Atoi(args[len(args)-1]); err == nil && id >= 0 && id < len(f.networks) {
				selected = f.networks[id]
			}
		}
		if f.connectOnSelect && (f.failSSID == "" || selected != f.failSSID) {
			f.connected = true
		} else {
			f.connected = false
		}
		return "OK", nil
	case "remove_network":
		return "OK", nil
	case "disconnect":
		f.connected = false
		return "OK", nil
	default:
		return "", fmt.Errorf("fake wpa_cli: unexpected command %q", cmd)
	}
}

func (f *fakeRunner) ip(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "addr flush"), strings.Contains(joined, "addr add"), strings.Contains(joined, "link set"):
		return "", nil
	case strings.Contains(joined, "-4 -o addr show"):
		if f.connected {
			return "2: wlan0    inet 192.168.1.5/24 brd 192.168.1.255 scope global wlan0", nil
		}
		return "", nil
	default:
		return "", fmt.Errorf("fake ip: unexpected args %v", args)
	}
}

func (f *fakeRunner) snapshotRuns() []runCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runCall(nil), f.runs...)
}

func (f *fakeRunner) snapshotStarts() []runCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runCall(nil), f.starts...)
}

func (f *fakeRunner) scanCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scanRequests
}

func testManager(t *testing.T, runner *fakeRunner) *linuxManager {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		DataDir: dir, RunDir: dir + "/run", Interface: "wlan0",
		APAddress: "192.168.4.1", APPrefix: 24, PortalPort: 8080,
		APPassword:       "capsulad",
		StockConfigPaths: []string{},
		AdoptStock:       true,
		APName:           func(context.Context) (string, error) { return "Capsule-Test", nil },
		StartupTimeout:   200 * time.Millisecond,
		ConnectTimeout:   600 * time.Millisecond,
		ScanTimeout:      300 * time.Millisecond,
		CommandTimeout:   time.Second,
		Runner:           runner,
	}
	m, err := newLinuxManager(cfg, "wlan0")
	if err != nil {
		t.Fatalf("newLinuxManager: %v", err)
	}
	oldPortal := startPortalServer
	startPortalServer = func(Controller, portalOptions) (*portalServer, error) {
		return &portalServer{}, nil
	}
	t.Cleanup(func() { startPortalServer = oldPortal })
	return m
}

func seedNetwork(t *testing.T, m *linuxManager, ssid, passphrase, security string) storedNetwork {
	t.Helper()
	sec, err := NormalizeSecurity(security)
	if err != nil {
		t.Fatalf("security: %v", err)
	}
	n := storedNetwork{SSID: ssid, Security: sec}
	if sec != SecurityOpen {
		psk, err := derivePSK(ssid, passphrase)
		if err != nil {
			t.Fatalf("derive psk: %v", err)
		}
		n.PSK = psk
	}
	if _, err := m.store.upsert(n); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	return n
}

func runContains(calls []runCall, name, needle string) bool {
	for _, call := range calls {
		if strings.HasSuffix(call.name, name) && strings.Contains(strings.Join(call.args, " "), needle) {
			return true
		}
	}
	return false
}

func startContains(calls []runCall, name, needle string) bool {
	for _, call := range calls {
		if strings.HasSuffix(call.name, name) && strings.Contains(strings.Join(call.args, " "), needle) {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}
