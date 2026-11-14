package wifi

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type linuxTools struct {
	supplicant string
	cli        string
	ip         string
	dnsmasq    string
	dhcpcd     string
	iw         string
	iptables   string
}

type child struct {
	proc Process
	done chan struct{}
}

func startChild(ctx context.Context, r CommandRunner, name string, args ...string) (*child, error) {
	proc, err := r.Start(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	c := &child{proc: proc, done: make(chan struct{})}
	go func() {
		_ = proc.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *child) running() bool {
	if c == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *child) stop() {
	if c == nil || !c.running() {
		return
	}
	_ = c.proc.Signal(sigterm)
	select {
	case <-c.done:
		return
	case <-time.After(3 * time.Second):
	}
	_ = c.proc.Kill()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
}

func (c *child) output() string {
	if c == nil || c.proc == nil {
		return ""
	}
	return c.proc.Output()
}

type apRuntime struct {
	ssid       string
	supplicant *child
	dnsmasq    *child
	portal     *portalServer
	firewall   func(context.Context)
}

type linuxManager struct {
	cfg          Config
	iface        string
	tools        linuxTools
	store        *networkStore
	stockConfigs []string

	opMu        sync.Mutex
	scanMu      sync.Mutex
	reconcileMu sync.Mutex

	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	closed            bool
	status            Status
	revision          uint64
	known             []storedNetwork
	station           *child
	ap                *apRuntime
	monitor           *eventMonitor
	eventWake         chan struct{}
	apScanUnsupported bool
	connecting        bool
	lastError         string

	stoppedStock bool
	closeOnce    sync.Once
}

func newLinuxManager(cfg Config, iface string) (*linuxManager, error) {
	var tools linuxTools
	for _, spec := range []struct {
		name string
		dst  *string
		req  bool
	}{
		{"wpa_supplicant", &tools.supplicant, true},
		{"wpa_cli", &tools.cli, true},
		{"ip", &tools.ip, true},
		{"dnsmasq", &tools.dnsmasq, false},
		{"dhcpcd", &tools.dhcpcd, false},
		{"iw", &tools.iw, false},
		{"iptables", &tools.iptables, false},
	} {
		path, ok := lookTool(cfg.Runner, spec.name)
		if !ok {
			if spec.req {
				return nil, fmt.Errorf("%w: %s is not installed", ErrUnavailable, spec.name)
			}
			continue
		}
		*spec.dst = path
	}
	store, err := openNetworkStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if _, err := store.list(); err != nil {
		cfg.warnf("network store is unreadable; preserving it as .corrupt: %v", err)
		if qerr := store.quarantine(); qerr != nil {
			cfg.warnf("quarantine network store: %v", qerr)
		}
	}
	stockConfigs := cfg.StockConfigPaths
	if stockConfigs == nil {
		stockConfigs = stockConfigPaths
	}
	lifetime, cancel := context.WithCancel(context.Background())
	m := &linuxManager{
		cfg: cfg, iface: iface, tools: tools, store: store,
		stockConfigs: stockConfigs,
		ctx: lifetime, cancel: cancel, revision: 1,
	}
	m.status = Status{
		Available:     true,
		State:         StateDisconnected,
		Interface:     iface,
		Detail:        "Wi-Fi station is ready",
		PortalPort:    cfg.PortalPort,
		APAddress:     cfg.APAddress,
		ScanSupported: tools.cli != "",
		Revision:      1,
	}
	m.eventWake = make(chan struct{}, 1)
	go m.runReconciler()
	return m, nil
}

// warnf reports a recoverable Wi-Fi problem without failing the operation.
func (m *linuxManager) warnf(format string, args ...any) {
	m.cfg.warnf(format, args...)
}

func (m *linuxManager) Start(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	// The manager lifetime starts in the constructor so Connect/Forget can
	// start long-lived children before Start returns. The daemon context only
	// bridges shutdown into that lifetime.
	go func() {
		select {
		case <-ctx.Done():
			m.cancel()
		case <-m.ctx.Done():
		}
	}()

	importErr := m.importStockNetworks(ctx)
	if importErr != nil {
		m.warnf("adopt stock Wi-Fi networks: %v", importErr)
	}
	known, err := m.store.list()
	if err != nil {
		m.setError(err)
		return err
	}
	if importErr != nil && len(known) == 0 {
		// Do not terminate a working stock association when its credential
		// file cannot be adopted; report the failure instead of destroying the
		// only known configuration.
		err := fmt.Errorf("wifi: could not adopt stock supplicant configuration; leaving the current association untouched: %w", importErr)
		m.setError(err)
		return err
	}
	m.setKnown(known)
	m.cfg.logf("interface=%s saved_networks=%d", m.iface, len(known))
	if len(known) == 0 {
		m.setStatus(StateDisconnected, "no saved Wi-Fi network; starting the setup access point")
		return m.startAP(ctx)
	}
	m.mu.Lock()
	m.connecting = true
	m.mu.Unlock()
	m.setStatus(StateConnecting, "trying saved Wi-Fi networks")
	if err := m.startStation(ctx); err != nil {
		m.warnf("station start failed: %v", err)
		m.setLastError(err.Error())
		return m.startAP(ctx)
	}
	refs, err := m.listNetworks(ctx)
	if err != nil || len(refs) == 0 {
		m.warnf("saved networks are not present in wpa_supplicant: %v", err)
		m.stopStation()
		return m.startAP(ctx)
	}
	deadline := time.Now().Add(m.cfg.StartupTimeout)
	for _, n := range known {
		if time.Until(deadline) <= 0 || ctx.Err() != nil {
			break
		}
		id, ok := networkID(refs, n.SSID)
		if !ok {
			continue
		}
		if _, err := m.wpa(ctx, "select_network", strconv.Itoa(id)); err != nil {
			continue
		}
		wait := 7 * time.Second
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if st, ok := m.waitConnected(ctx, wait); ok {
			m.finishConnected(ctx, st, m.securityFor(st.SSID))
			return nil
		}
	}
	m.stopStation()
	m.setStatus(StateDisconnected, "no saved Wi-Fi network connected; starting the setup access point")
	return m.startAP(ctx)
}

func (m *linuxManager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		cancel := m.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		m.opMu.Lock()
		defer m.opMu.Unlock()

		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		m.stopAP(cleanup)
		m.stopStation()
		if m.tools.ip != "" {
			_, _ = m.run(cleanup, 5*time.Second, m.tools.ip, "addr", "flush", "dev", m.iface)
		}
		if m.stoppedStock {
			if err := m.restoreStockSupplicant(cleanup); err != nil {
				m.warnf("restore stock wpa_supplicant: %v", err)
			}
		}
	})
	return nil
}

func (m *linuxManager) Status(ctx context.Context) (Status, error) {
	m.reconcile(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status
	st.Revision = m.revision
	st.Available = true
	st.Interface = m.iface
	st.APActive = m.ap != nil
	st.ScanSupported = m.scanSupportedLocked()
	if m.ap != nil {
		st.APSSID = m.ap.ssid
		st.APAddress = m.cfg.APAddress
		st.APPortalURL = m.cfg.portalURL()
		if m.ap.supplicant == nil || !m.ap.supplicant.running() ||
			m.ap.dnsmasq == nil || !m.ap.dnsmasq.running() {
			st.State = StateError
			st.Detail = "the setup access point process is no longer running"
		}
	}
	st.LastError = m.lastError
	return st, nil
}

func (m *linuxManager) KnownNetworks(ctx context.Context) ([]KnownNetwork, error) {
	m.reconcile(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.status.SSID
	out := make([]KnownNetwork, 0, len(m.known))
	for _, n := range m.known {
		out = append(out, KnownNetwork{
			SSID: n.SSID, Security: n.Security, Priority: n.Priority,
			HasPassword: n.Security != SecurityOpen, Connected: n.SSID == current && m.status.State == StateConnected,
		})
	}
	return out, nil
}

func (m *linuxManager) Connect(ctx context.Context, req ConnectRequest) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	ctx, cancel, err := m.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	security, err := NormalizeSecurity(req.Security)
	if err != nil {
		return err
	}
	if !validSSID(req.SSID) {
		return fmt.Errorf("%w: SSID must be 1-32 UTF-8 bytes without control characters", ErrInvalidNetwork)
	}
	if security == SecurityWEP {
		return fmt.Errorf("%w: WEP networks cannot be joined", ErrUnsupported)
	}
	stored := storedNetwork{SSID: req.SSID, Security: security}
	if security != SecurityOpen {
		switch {
		case req.Passphrase != "":
			psk, err := derivePSK(req.SSID, req.Passphrase)
			if err != nil {
				return err
			}
			stored.PSK = psk
		default:
			// Reconnect to a saved network without asking the user to type a
			// password the daemon already holds as a derived PSK.
			previous, ok := m.storedFor(req.SSID)
			if !ok || previous.PSK == "" {
				return fmt.Errorf("%w: passphrase is required for a new network", ErrInvalidNetwork)
			}
			stored.PSK = previous.PSK
		}
	}
	m.reconcile(ctx)
	previousSSID := m.currentSSID()
	previous, hadPrevious := m.storedFor(req.SSID)
	list, err := m.store.upsert(stored)
	if err != nil {
		return err
	}
	m.setKnown(list)

	m.mu.Lock()
	wasAP := m.ap != nil
	m.connecting = true
	m.lastError = ""
	m.mu.Unlock()
	m.setStatus(StateConnecting, "connecting to "+req.SSID)
	if wasAP {
		m.stopAP(ctx)
	}
	if err := m.startStation(ctx); err != nil {
		m.failConnect(wasAP, req.SSID, previousSSID, previous, hadPrevious, err)
		return err
	}
	refs, err := m.listNetworks(ctx)
	if err != nil {
		m.failConnect(wasAP, req.SSID, previousSSID, previous, hadPrevious, err)
		return err
	}
	id, ok := networkID(refs, req.SSID)
	if !ok {
		// A config reload is the deterministic way to pick up a just-saved
		// network when wpa_supplicant did not expose it yet.
		m.stopStation()
		if err := m.startStation(ctx); err != nil {
			m.failConnect(wasAP, req.SSID, previousSSID, previous, hadPrevious, err)
			return err
		}
		refs, err = m.listNetworks(ctx)
		if err == nil {
			id, ok = networkID(refs, req.SSID)
		}
	}
	if !ok {
		err := fmt.Errorf("wifi: wpa_supplicant did not load %q", req.SSID)
		m.failConnect(wasAP, req.SSID, previousSSID, previous, hadPrevious, err)
		return err
	}
	if _, err := m.wpa(ctx, "select_network", strconv.Itoa(id)); err != nil {
		m.failConnect(wasAP, req.SSID, previousSSID, previous, hadPrevious, err)
		return err
	}
	if live, ok := m.waitConnected(ctx, m.cfg.ConnectTimeout); ok {
		m.finishConnected(ctx, live, security)
		return nil
	}
	err = fmt.Errorf("could not connect to %q within %s", req.SSID, m.cfg.ConnectTimeout)
	m.failConnect(wasAP, req.SSID, previousSSID, previous, hadPrevious, err)
	return err
}

func (m *linuxManager) Forget(ctx context.Context, ssid string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	ctx, cancel, err := m.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	m.reconcile(ctx)

	list, found, err := m.store.remove(ssid)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrNotFound, ssid)
	}
	m.setKnown(list)
	m.mu.Lock()
	apActive := m.ap != nil
	current := m.status.SSID
	m.mu.Unlock()
	if apActive {
		m.setStatus(StateAPFallback, fmt.Sprintf("forgot %q", ssid))
		return nil
	}
	m.mu.Lock()
	station := m.station
	m.mu.Unlock()
	if station != nil {
		// Remove only the forgotten network from the live supplicant. A
		// restart here would drop an unrelated active connection.
		if refs, err := m.listNetworks(ctx); err == nil {
			if id, ok := networkID(refs, ssid); ok {
				if _, err := m.wpa(ctx, "remove_network", strconv.Itoa(id)); err != nil {
					m.warnf("remove live network %q: %v", ssid, err)
				}
			}
		}
		if current == ssid {
			_, _ = m.wpa(ctx, "disconnect")
			m.setStatus(StateDisconnected, "forgot the connected Wi-Fi network")
		}
	}
	if len(list) == 0 {
		m.stopStation()
		m.setStatus(StateDisconnected, "no saved Wi-Fi network remains; starting the setup access point")
		return m.startAP(ctx)
	}
	if station == nil {
		m.setStatus(StateDisconnected, fmt.Sprintf("forgot %q", ssid))
	}
	return nil
}

func (m *linuxManager) Scan(ctx context.Context) ([]ScanResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	ctx, cancel, err := m.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	m.reconcile(ctx)

	m.mu.Lock()
	apActive := m.ap != nil
	station := m.station
	m.mu.Unlock()
	if !apActive && station == nil {
		return nil, ErrUnavailable
	}
	m.mu.Lock()
	apUnsupported := apActive && m.apScanUnsupported && m.tools.iw == ""
	m.mu.Unlock()
	if apUnsupported {
		return nil, fmt.Errorf("%w: this supplicant cannot scan while the setup AP is active", ErrUnsupported)
	}
	// AP mode uses the same control socket; a driver that refuses to scan while
	// the AP is up returns FAIL and falls through to the optional iw path. A
	// scan is never attempted from Status/KnownNetworks.
	scanStarted := false
	waitFor := m.cfg.ScanTimeout
	if apActive && waitFor > 5*time.Second {
		// A supplicant that accepts SCAN but never produces results in AP mode
		// must not keep the user waiting for the full station timeout.
		waitFor = 5 * time.Second
	}
	if out, err := m.wpa(ctx, "scan"); err == nil && !strings.Contains(strings.ToUpper(out), "FAIL") {
		scanStarted = true
		if results, ok := m.waitScanResults(ctx, waitFor); ok {
			return results, nil
		}
	}
	if m.tools.iw != "" {
		out, err := m.run(ctx, m.cfg.ScanTimeout, m.tools.iw, "dev", m.iface, "scan")
		if err != nil {
			return nil, err
		}
		results := parseIWScan(out)
		if len(results) == 0 {
			return nil, fmt.Errorf("wifi: scan returned no networks")
		}
		return results, nil
	}
	if apActive && !scanStarted {
		m.setAPScanUnsupported()
		return nil, fmt.Errorf("%w: this supplicant cannot scan while the setup AP is active", ErrUnsupported)
	}
	return nil, fmt.Errorf("wifi: scan returned no networks")
}

func (m *linuxManager) scanSupportedLocked() bool {
	if m.ap != nil && m.apScanUnsupported && m.tools.iw == "" {
		return false
	}
	return m.tools.cli != ""
}

func (m *linuxManager) setAPScanUnsupported() {
	m.mu.Lock()
	if m.apScanUnsupported {
		m.mu.Unlock()
		return
	}
	m.apScanUnsupported = true
	m.revision++
	m.mu.Unlock()
	m.notify()
}

func (m *linuxManager) waitScanResults(ctx context.Context, timeout time.Duration) ([]ScanResult, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, false
		}
		out, err := m.wpa(ctx, "scan_results")
		if err == nil {
			if results := parseScanResults(out, m.currentSSID()); len(results) > 0 {
				return results, true
			}
		}
		if !sleepCtx(ctx, 350*time.Millisecond) {
			return nil, false
		}
	}
	return nil, false
}

func (m *linuxManager) startStation(ctx context.Context) error {
	m.ensureIfaceUp(ctx)
	m.stopStation()
	m.terminateExistingSupplicant(ctx)
	m.mu.Lock()
	known := append([]storedNetwork(nil), m.known...)
	m.mu.Unlock()
	ctrlDir := m.ctrlDir()
	if err := ensureCtrlDir(ctrlDir); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(m.cfg.RunDir, "wpa-station.conf"), stationConfig(ctrlDir, known)); err != nil {
		return err
	}
	if _, err := m.run(ctx, m.cfg.CommandTimeout, m.tools.ip, "addr", "flush", "dev", m.iface); err != nil {
		m.warnf("flush station address: %v", err)
	}
	supp, err := startChild(m.ctx, m.cfg.Runner, m.tools.supplicant,
		"-D", "nl80211", "-i", m.iface, "-c", filepath.Join(m.cfg.RunDir, "wpa-station.conf"), "-s")
	if err != nil {
		return err
	}
	if err := m.waitSupplicant(ctx, supp); err != nil {
		supp.stop()
		return fmt.Errorf("station wpa_supplicant: %w", err)
	}
	m.mu.Lock()
	m.station = supp
	m.mu.Unlock()
	m.startSupplicantMonitor()
	return nil
}

func (m *linuxManager) stopStation() {
	m.stopMonitor()
	m.mu.Lock()
	supp := m.station
	m.station = nil
	m.mu.Unlock()
	if supp != nil {
		supp.stop()
	}
}

func (m *linuxManager) startAP(ctx context.Context) error {
	ssid, err := m.cfg.APName(ctx)
	if err != nil {
		m.setError(fmt.Errorf("resolve Bluetooth advertised name: %w", err))
		return err
	}
	if !validSSID(ssid) {
		err := fmt.Errorf("%w: access point SSID %q is invalid", ErrInvalidNetwork, ssid)
		m.setError(err)
		return err
	}
	if m.tools.dnsmasq == "" {
		err := fmt.Errorf("%w: dnsmasq is not installed, so the setup network cannot provide DHCP/DNS", ErrUnsupported)
		m.setError(err)
		return err
	}
	m.stopStation()
	m.terminateExistingSupplicant(ctx)
	m.ensureIfaceUp(ctx)
	if m.tools.dhcpcd != "" {
		// A station DHCP client must not keep leasing the interface while it is
		// acting as the setup AP.
		_, _ = m.run(ctx, 5*time.Second, m.tools.dhcpcd, "-x", m.iface)
	}
	ctrlDir := m.ctrlDir()
	psk, err := derivePSK(ssid, m.cfg.APPassword)
	if err != nil {
		m.setError(err)
		return err
	}
	if err := ensureCtrlDir(ctrlDir); err != nil {
		m.setError(err)
		return err
	}
	if err := writePrivateFile(filepath.Join(m.cfg.RunDir, "wpa-ap.conf"), apConfig(ctrlDir, ssid, psk)); err != nil {
		m.setError(err)
		return err
	}
	if _, err := m.run(ctx, m.cfg.CommandTimeout, m.tools.ip, "addr", "flush", "dev", m.iface); err != nil {
		m.warnf("flush AP address: %v", err)
	}
	prefix := netip.PrefixFrom(netip.MustParseAddr(m.cfg.APAddress), m.cfg.APPrefix)
	supp, err := startChild(m.ctx, m.cfg.Runner, m.tools.supplicant,
		"-D", "nl80211", "-i", m.iface, "-c", filepath.Join(m.cfg.RunDir, "wpa-ap.conf"), "-s")
	if err != nil {
		m.setError(err)
		return err
	}
	if err := m.waitSupplicant(ctx, supp); err != nil {
		supp.stop()
		m.setError(fmt.Errorf("AP wpa_supplicant: %w", err))
		return err
	}
	m.startSupplicantMonitor()
	// Add the AP address after the supplicant has put the radio into AP mode;
	// an early address can be lost when the driver reinitializes the interface.
	if _, err := m.run(ctx, m.cfg.CommandTimeout, m.tools.ip, "addr", "add", prefix.String(), "dev", m.iface); err != nil {
		supp.stop()
		m.setError(fmt.Errorf("configure AP address: %w", err))
		return err
	}
	dns, err := m.startDNSMasq(ctx)
	if err != nil {
		supp.stop()
		m.setError(err)
		return err
	}
	portal, err := startPortalServer(m, portalOptions{
		APAddress: m.cfg.APAddress,
		Prefix:    prefix.Masked(),
		Port:      m.cfg.PortalPort,
		Password:  m.cfg.APPassword,
	})
	if err != nil {
		dns.stop()
		supp.stop()
		m.setError(err)
		return err
	}
	firewall, ferr := m.installAPFirewall(ctx)
	if ferr != nil {
		m.warnf("AP firewall: %v", ferr)
	}
	runtime := &apRuntime{ssid: ssid, supplicant: supp, dnsmasq: dns, portal: portal, firewall: firewall}
	m.mu.Lock()
	m.ap = runtime
	m.apScanUnsupported = false
	m.connecting = false
	m.status.APActive = true
	m.status.APSSID = ssid
	m.status.APAddress = m.cfg.APAddress
	m.status.APPortalURL = m.cfg.portalURL()
	m.status.SSID = ""
	m.status.Security = ""
	m.status.IPAddress = m.cfg.APAddress
	m.status.State = StateAPFallback
	m.status.Detail = fmt.Sprintf("setup access point %q on %s", ssid, m.cfg.APAddress)
	m.revision++
	m.mu.Unlock()
	m.notify()
	m.cfg.logf("setup AP %q active on %s (%s)", ssid, m.cfg.APAddress, m.cfg.portalURL())
	return nil
}

func (m *linuxManager) startDNSMasq(ctx context.Context) (*child, error) {
	prefix := netip.PrefixFrom(netip.MustParseAddr(m.cfg.APAddress), m.cfg.APPrefix).Masked()
	network := prefix.Addr()
	low := addIPv4(network, 10)
	high := addIPv4(network, 100)
	netmask := net.IP(net.CIDRMask(prefix.Bits(), 32)).String()
	args := []string{
		"-k",
		"--conf-file=/dev/null",
		"--bind-interfaces",
		"--interface=" + m.iface,
		"--except-interface=lo",
		"--dhcp-authoritative",
		"--dhcp-range=" + low.String() + "," + high.String() + "," + netmask + ",1h",
		"--dhcp-option=3," + m.cfg.APAddress,
		"--dhcp-option=6," + m.cfg.APAddress,
		"--address=/#/" + m.cfg.APAddress,
		"--no-resolv",
		"--no-hosts",
		"--no-poll",
		// Run unprivileged after binding and keep leases in memory: the run
		// directory is root-owned, and a provisioning AP does not need durable
		// client leases.
		"--user=nobody",
		"--leasefile-ro",
	}
	dns, err := startChild(m.ctx, m.cfg.Runner, m.tools.dnsmasq, args...)
	if err != nil {
		return nil, err
	}
	// dnsmasq exits immediately on a bind conflict (the stock instance is
	// bound to lo, but a previous daemon may have left one on the AP address).
	select {
	case <-dns.done:
		return nil, fmt.Errorf("dnsmasq exited: %s", dns.output())
	case <-time.After(400 * time.Millisecond):
	}
	return dns, nil
}

func (m *linuxManager) stopAP(ctx context.Context) {
	m.mu.Lock()
	ap := m.ap
	m.ap = nil
	m.mu.Unlock()
	if ap == nil {
		return
	}
	m.stopMonitor()
	if ap.portal != nil {
		ap.portal.close()
	}
	if ap.dnsmasq != nil {
		ap.dnsmasq.stop()
	}
	if ap.supplicant != nil {
		ap.supplicant.stop()
	}
	if ap.firewall != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ap.firewall(cleanup)
		cancel()
	}
	if m.tools.ip != "" {
		_, _ = m.run(context.Background(), 5*time.Second, m.tools.ip, "addr", "flush", "dev", m.iface)
	}
	m.mu.Lock()
	m.connecting = false
	m.apScanUnsupported = false
	m.status.APActive = false
	m.status.APSSID = ""
	m.status.APAddress = ""
	m.status.APPortalURL = ""
	m.status.SSID = ""
	m.status.Security = ""
	m.status.IPAddress = ""
	m.status.State = StateDisconnected
	m.status.Detail = "setup access point stopped"
	m.revision++
	m.mu.Unlock()
	m.notify()
}

func (m *linuxManager) waitConnected(ctx context.Context, timeout time.Duration) (supplicantStatus, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return supplicantStatus{}, false
		}
		st, err := m.queryStatus(ctx)
		if err == nil && st.State == "COMPLETED" {
			ip := st.IPAddress
			if ip == "" {
				ip = m.ensureDHCP(ctx)
			}
			st.IPAddress = ip
			return st, true
		}
		if !sleepCtx(ctx, 400*time.Millisecond) {
			return supplicantStatus{}, false
		}
	}
	return supplicantStatus{}, false
}

func (m *linuxManager) finishConnected(ctx context.Context, st supplicantStatus, security string) {
	ip := st.IPAddress
	if ip == "" {
		ip = m.queryIP(ctx)
	}
	detail := "connected"
	if ip == "" {
		detail = "associated; waiting for an IPv4 lease"
	}
	m.mu.Lock()
	m.connecting = false
	m.lastError = ""
	m.status.State = StateConnected
	m.status.SSID = st.SSID
	m.status.IPAddress = ip
	m.status.Security = security
	m.status.Detail = detail
	m.status.APActive = false
	m.status.APSSID = ""
	m.status.APPortalURL = ""
	m.revision++
	m.mu.Unlock()
	m.notify()
	m.cfg.logf("connected to %q ip=%s", st.SSID, ip)
}

func (m *linuxManager) failConnect(wasAP bool, failedSSID, previousSSID string, previous storedNetwork, hadPrevious bool, cause error) {
	m.mu.Lock()
	m.connecting = false
	m.lastError = cause.Error()
	m.status.State = StateError
	m.status.Detail = cause.Error()
	m.revision++
	m.mu.Unlock()
	m.notify()

	// A failed attempt to replace an existing network must not overwrite the
	// credential that previously worked. Restore it before recovery, and
	// reload the station config so the live supplicant uses the old PSK.
	if hadPrevious && previous.SSID == failedSSID {
		if list, err := m.store.upsert(previous); err != nil {
			m.warnf("restore previous network credential: %v", err)
		} else {
			m.setKnown(list)
		}
		m.stopStation()
	}
	if wasAP {
		m.restoreAP(cause)
		return
	}
	base, stop, err := m.operationContext(context.Background())
	if err != nil {
		return
	}
	defer stop()
	recoveryCtx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()
	if previousSSID != "" && m.recoverNetwork(recoveryCtx, previousSSID) {
		m.markRecovered(cause, previousSSID)
		return
	}
	if m.recoverAnyKnown(recoveryCtx, failedSSID) {
		m.markRecovered(cause, m.currentSSID())
		return
	}
	m.restoreAP(cause)
}

func (m *linuxManager) restoreAP(cause error) {
	base, stop, err := m.operationContext(context.Background())
	if err != nil {
		return
	}
	defer stop()
	recoverCtx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()
	if err := m.startAP(recoverCtx); err != nil {
		m.warnf("restore setup AP after failed connect: %v", err)
		return
	}
	m.mu.Lock()
	m.status.LastError = cause.Error()
	m.status.Detail = "the saved network did not connect; the setup access point is available again"
	m.revision++
	m.mu.Unlock()
	m.notify()
}

func (m *linuxManager) markRecovered(cause error, ssid string) {
	m.mu.Lock()
	m.lastError = cause.Error()
	m.status.Detail = fmt.Sprintf("restored saved network %q after a failed connect", ssid)
	m.revision++
	m.mu.Unlock()
	m.notify()
}

func (m *linuxManager) recoverNetwork(ctx context.Context, ssid string) bool {
	m.mu.Lock()
	station := m.station
	m.mu.Unlock()
	if station == nil {
		if err := m.startStation(ctx); err != nil {
			m.warnf("restart station for recovery: %v", err)
			return false
		}
	}
	refs, err := m.listNetworks(ctx)
	if err != nil {
		return false
	}
	id, ok := networkID(refs, ssid)
	if !ok {
		return false
	}
	if _, err := m.wpa(ctx, "select_network", strconv.Itoa(id)); err != nil {
		return false
	}
	live, ok := m.waitConnected(ctx, min(m.cfg.ConnectTimeout, 15*time.Second))
	if !ok {
		return false
	}
	m.finishConnected(ctx, live, m.securityFor(ssid))
	return true
}

func (m *linuxManager) recoverAnyKnown(ctx context.Context, failedSSID string) bool {
	m.mu.Lock()
	known := append([]storedNetwork(nil), m.known...)
	m.mu.Unlock()
	for _, n := range known {
		if n.SSID == failedSSID {
			continue
		}
		if m.recoverNetwork(ctx, n.SSID) {
			return true
		}
	}
	return false
}

func (m *linuxManager) ensureDHCP(ctx context.Context) string {
	if ip := m.queryIP(ctx); ip != "" {
		return ip
	}
	// The stock dhcpcd often already owns the interface. Poll briefly before
	// starting a second client, then tolerate "already running".
	for range 5 {
		if !sleepCtx(ctx, 200*time.Millisecond) {
			return ""
		}
		if ip := m.queryIP(ctx); ip != "" {
			return ip
		}
	}
	if m.tools.dhcpcd != "" {
		_, _ = m.run(ctx, 20*time.Second, m.tools.dhcpcd, "-4", "-t", "15", m.iface)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ip := m.queryIP(ctx); ip != "" {
			return ip
		}
		if !sleepCtx(ctx, 300*time.Millisecond) {
			break
		}
	}
	return ""
}

func (m *linuxManager) queryStatus(ctx context.Context) (supplicantStatus, error) {
	out, err := m.wpa(ctx, "status")
	if err != nil {
		return supplicantStatus{}, err
	}
	return parseSupplicantStatus(out), nil
}

func (m *linuxManager) queryIP(ctx context.Context) string {
	if m.tools.ip == "" {
		return ""
	}
	out, err := m.run(ctx, 3*time.Second, m.tools.ip, "-4", "-o", "addr", "show", "dev", m.iface)
	if err != nil {
		return ""
	}
	return parseIPAddress(out)
}

func (m *linuxManager) listNetworks(ctx context.Context) ([]networkRef, error) {
	out, err := m.wpa(ctx, "list_networks")
	if err != nil {
		return nil, err
	}
	return parseListNetworks(out), nil
}

func (m *linuxManager) wpa(ctx context.Context, args ...string) (string, error) {
	cliArgs := append([]string{"-p", m.ctrlDir(), "-i", m.iface}, args...)
	return m.run(ctx, m.cfg.CommandTimeout, m.tools.cli, cliArgs...)
}

func (m *linuxManager) run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if name == "" {
		return "", ErrUnsupported
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return m.cfg.Runner.Run(runCtx, name, args...)
}

func (m *linuxManager) terminateExistingSupplicant(ctx context.Context) {
	for _, dir := range m.ctrlDirs() {
		out, err := m.run(ctx, 2*time.Second, m.tools.cli, "-p", dir, "-i", m.iface, "ping")
		if err != nil || !strings.Contains(strings.ToUpper(out), "PONG") {
			continue
		}
		if dir != m.ctrlDir() {
			m.stoppedStock = true
		}
		_, _ = m.run(ctx, 2*time.Second, m.tools.cli, "-p", dir, "-i", m.iface, "terminate")
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			out, err := m.run(ctx, time.Second, m.tools.cli, "-p", dir, "-i", m.iface, "ping")
			if err != nil || !strings.Contains(strings.ToUpper(out), "PONG") {
				break
			}
			if !sleepCtx(ctx, 100*time.Millisecond) {
				break
			}
		}
	}
}

func (m *linuxManager) restoreStockSupplicant(ctx context.Context) error {
	if m.tools.supplicant == "" {
		return ErrUnsupported
	}
	if _, err := m.run(ctx, 5*time.Second, m.tools.supplicant, "-B", "-i", m.iface, "-c", "/etc/wpa_supplicant.conf", "-s"); err != nil {
		// The child's output may echo a user-managed stock config, so report
		// only the failed action.
		return fmt.Errorf("stock wpa_supplicant did not start")
	}
	m.stoppedStock = false
	return nil
}

func (m *linuxManager) waitSupplicant(ctx context.Context, c *child) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !c.running() {
			// Do not surface the child's raw output: a config parse error can
			// echo a psk line. Recognized build limitations are safe to name.
			if strings.Contains(c.output(), "AP mode support not included") {
				return fmt.Errorf("installed wpa_supplicant was built without AP mode")
			}
			return fmt.Errorf("wpa_supplicant exited before its control interface answered")
		}
		out, err := m.run(ctx, time.Second, m.tools.cli, "-p", m.ctrlDir(), "-i", m.iface, "ping")
		if err == nil && strings.Contains(strings.ToUpper(out), "PONG") {
			return nil
		}
		if !sleepCtx(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("control interface did not answer")
}

func (m *linuxManager) ensureIfaceUp(ctx context.Context) {
	if m.tools.ip == "" {
		return
	}
	_, _ = m.run(ctx, 5*time.Second, m.tools.ip, "link", "set", m.iface, "up")
}

func (m *linuxManager) ctrlDir() string { return filepath.Join(m.cfg.RunDir, "wpa") }

func (m *linuxManager) ctrlDirs() []string {
	return []string{
		m.ctrlDir(),
		"/var/run/wpa_supplicant",
		"/run/wpa_supplicant",
	}
}

func ensureCtrlDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("wifi: create supplicant control directory: %w", err)
	}
	return os.Chmod(dir, 0o700)
}

func (m *linuxManager) currentSSID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status.SSID
}

func (m *linuxManager) storedFor(ssid string) (storedNetwork, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.known {
		if n.SSID == ssid {
			return n, true
		}
	}
	return storedNetwork{}, false
}

func (m *linuxManager) securityFor(ssid string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.known {
		if n.SSID == ssid {
			return n.Security
		}
	}
	return ""
}

func (m *linuxManager) setKnown(list []storedNetwork) {
	m.mu.Lock()
	m.known = append([]storedNetwork(nil), list...)
	m.mu.Unlock()
}

func (m *linuxManager) setStatus(state State, detail string) {
	m.mu.Lock()
	m.status.State = state
	m.status.Detail = detail
	if state == StateDisconnected {
		m.status.SSID = ""
		m.status.Security = ""
		m.status.IPAddress = ""
	}
	m.revision++
	m.mu.Unlock()
	m.notify()
}

func (m *linuxManager) setError(err error) {
	m.mu.Lock()
	m.connecting = false
	m.status.State = StateError
	m.status.Detail = err.Error()
	m.lastError = err.Error()
	m.revision++
	m.mu.Unlock()
	m.notify()
}

func (m *linuxManager) setLastError(text string) {
	m.mu.Lock()
	m.lastError = text
	m.revision++
	m.mu.Unlock()
	m.notify()
}

// operationContext joins a caller's deadline to the manager lifecycle. Close
// cancels the manager context, so a slow portal connect cannot restart an AP
// after the daemon has begun shutting down.
func (m *linuxManager) operationContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	m.mu.Lock()
	closed := m.closed
	parent := m.ctx
	m.mu.Unlock()
	if closed {
		return nil, nil, ErrUnavailable
	}
	merged, cancel := context.WithCancel(ctx)
	if parent == nil {
		return merged, cancel, nil
	}
	stop := context.AfterFunc(parent, cancel)
	return merged, func() {
		stop()
		cancel()
	}, nil
}

func (m *linuxManager) notify() {
	if m.cfg.OnChange != nil {
		m.cfg.OnChange()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func addIPv4(addr netip.Addr, n int) netip.Addr {
	b := addr.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(n)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func writePrivateFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".wpa-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// portalOptions is shared with portal.go.
type portalOptions struct {
	APAddress string
	Prefix    netip.Prefix
	Port      int
	Password  string
}

var sigterm = syscall.SIGTERM
