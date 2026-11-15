package wifi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDerivePSKMatchesIEEEVector(t *testing.T) {
	got, err := derivePSK("IEEE", "password")
	if err != nil {
		t.Fatalf("derivePSK: %v", err)
	}
	const want = "f42c6fc52df0ebef9ebb4b90b38a5f902e83fe1b135a70e23aed762e9710a12e"
	if got != want {
		t.Fatalf("PSK = %s, want %s", got, want)
	}
}

func TestNormalizeSecurityAndScanFlags(t *testing.T) {
	cases := map[string]string{
		"":          SecurityWPA2,
		"wpa2":      SecurityWPA2,
		"WPA2-PSK":  SecurityWPA2,
		"wpa2_wpa3": SecurityWPA2WPA3,
		"WPA2/WPA3": SecurityWPA2WPA3,
		"wpa3":      SecurityWPA3,
		"SAE":       SecurityWPA3,
		"open":      SecurityOpen,
		"none":      SecurityOpen,
		"wpa":       SecurityWPA,
		"wep":       SecurityWEP,
	}
	for in, want := range cases {
		got, err := NormalizeSecurity(in)
		if err != nil {
			t.Fatalf("NormalizeSecurity(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizeSecurity(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := NormalizeSecurity("wpa4"); err == nil {
		t.Fatal("unknown security accepted")
	}
	if got := securityFromFlags("[WPA2-PSK-CCMP][ESS]"); got != SecurityWPA2 {
		t.Fatalf("WPA2 flags = %q", got)
	}
	if got := securityFromFlags("[WPA2-PSK-CCMP][WPA3-SAE-CCMP][ESS]"); got != SecurityWPA2WPA3 {
		t.Fatalf("transition flags = %q", got)
	}
	if got := securityFromFlags("[WPA3-SAE-CCMP][ESS]"); got != SecurityWPA3 {
		t.Fatalf("WPA3 flags = %q", got)
	}
	if got := securityFromFlags("[WPA-PSK-TKIP][ESS]"); got != SecurityWPA {
		t.Fatalf("WPA flags = %q", got)
	}
	if got := securityFromFlags("[ESS]"); got != SecurityOpen {
		t.Fatalf("open flags = %q", got)
	}
}

func TestParseScanResultsKeepsStrongestAndSkipsHidden(t *testing.T) {
	out := "bssid / frequency / signal level / flags / ssid\n" +
		"aa:bb:cc:dd:ee:01\t2412\t-70\t[WPA2-PSK-CCMP][ESS]\tHome\n" +
		"aa:bb:cc:dd:ee:02\t2437\t-40\t[WPA2-PSK-CCMP][ESS]\tHome\n" +
		"aa:bb:cc:dd:ee:03\t2412\t-55\t[ESS]\tCafe with spaces\n" +
		"aa:bb:cc:dd:ee:04\t2462\t-80\t[WPA2-PSK-CCMP][ESS]\t\n"
	got := parseScanResults(out, "Cafe with spaces")
	if len(got) != 2 {
		t.Fatalf("got %d networks: %+v", len(got), got)
	}
	if got[0].SSID != "Home" || got[0].SignalDBM != -40 {
		t.Fatalf("strongest first: %+v", got[0])
	}
	if got[1].SSID != "Cafe with spaces" || !got[1].Connected {
		t.Fatalf("connected/open result: %+v", got[1])
	}
}

func TestParseSupplicantStatusAndIPAddress(t *testing.T) {
	st := parseSupplicantStatus("wpa_state=COMPLETED\nssid=My Home\nip_address=192.168.1.4\n")
	if st.State != "COMPLETED" || st.SSID != "My Home" || st.IPAddress != "192.168.1.4" {
		t.Fatalf("status = %+v", st)
	}
	if ip := parseIPAddress("2: wlan0    inet 10.0.0.7/24 brd 10.0.0.255 scope global wlan0"); ip != "10.0.0.7" {
		t.Fatalf("ip = %q", ip)
	}
}

func TestNetworkStoreIsPrivateAndDoesNotPersistPassphrase(t *testing.T) {
	dir := t.TempDir()
	store, err := openNetworkStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	psk, err := derivePSK("Home", "correct horse battery")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if _, err := store.upsert(storedNetwork{SSID: "Home", Security: SecurityWPA2, PSK: psk}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("store permissions = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "correct horse battery") {
		t.Fatal("plaintext passphrase persisted")
	}
	if !strings.Contains(string(data), psk) {
		t.Fatal("derived PSK missing")
	}
	dirInfo, err := os.Stat(filepath.Dir(store.path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("store directory permissions = %v, want 0700", dirInfo.Mode().Perm())
	}
}

func TestCorruptNetworkStoreIsPreservedAndRecoverable(t *testing.T) {
	dir := t.TempDir()
	store, err := openNetworkStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := os.WriteFile(store.path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt store: %v", err)
	}
	if _, err := store.list(); err == nil {
		t.Fatal("corrupt store was accepted")
	}
	if err := store.quarantine(); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	list, err := store.list()
	if err != nil || len(list) != 0 {
		t.Fatalf("store after quarantine = %+v err=%v", list, err)
	}
	matches, err := filepath.Glob(store.path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("preserved copies = %v err=%v", matches, err)
	}
}

func TestStationAndAPConfigsUseDerivedPSKAndBluetoothSSID(t *testing.T) {
	psk, _ := derivePSK("Home", "password123")
	cfg := stationConfig("/run/wpa", []storedNetwork{
		{SSID: "Home", Security: SecurityWPA2, PSK: psk, Priority: 7},
		{SSID: "Open Café", Security: SecurityOpen},
	})
	if !strings.Contains(cfg, "ssid=\"Home\"") || !strings.Contains(cfg, "psk="+psk) {
		t.Fatalf("station config missing Home: %s", cfg)
	}
	if !strings.Contains(cfg, "key_mgmt=NONE") || !strings.Contains(cfg, "ssid=\"Open Café\"") {
		t.Fatalf("station config missing open network: %s", cfg)
	}
	apPSK, _ := derivePSK("Capsule-Test", "capsulad")
	ap := apConfig("/run/wpa", "Capsule-Test", apPSK)
	if !strings.Contains(ap, "ssid=\"Capsule-Test\"") || !strings.Contains(ap, "mode=2") ||
		!strings.Contains(ap, "key_mgmt=WPA-PSK") || !strings.Contains(ap, "frequency=2412") {
		t.Fatalf("AP config: %s", ap)
	}
	if strings.Contains(ap, "capsulad") {
		t.Fatal("AP config contains plaintext provisioning password")
	}
}

func TestConnectRejectsInvalidCredentialsBeforeTouchingRadio(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	ctx := context.Background()
	if err := m.Connect(ctx, ConnectRequest{SSID: "", Passphrase: "password123"}); err == nil {
		t.Fatal("empty SSID accepted")
	}
	if err := m.Connect(ctx, ConnectRequest{SSID: "Home", Passphrase: "short"}); err == nil {
		t.Fatal("short passphrase accepted")
	}
	if err := m.Connect(ctx, ConnectRequest{SSID: "Home", Passphrase: "password123", Security: "wep"}); err == nil {
		t.Fatal("WEP accepted")
	}
	if len(runner.snapshotRuns()) != 0 || len(runner.snapshotStarts()) != 0 {
		t.Fatal("invalid request touched the radio tooling")
	}
	_ = m.Close()
}

func TestStartupNoSavedNetworkBringsUpAPWithoutScanning(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != StateAPFallback || !status.APActive || status.APSSID != "Capsule-Test" {
		t.Fatalf("status = %+v", status)
	}
	if status.APAddress != "192.168.4.1" || status.APPortalURL != "http://192.168.4.1:8080/" {
		t.Fatalf("AP address = %+v", status)
	}
	if !startContains(runner.snapshotStarts(), "dnsmasq", "--address=/#/192.168.4.1") {
		t.Fatal("dnsmasq did not receive the captive DNS hijack")
	}
	if !startContains(runner.snapshotStarts(), "dnsmasq", "--dhcp-range=192.168.4.10,192.168.4.100") {
		t.Fatal("dnsmasq did not receive the DHCP range")
	}
	if !startContains(runner.snapshotStarts(), "wpa_supplicant", "wpa-ap.conf") {
		t.Fatal("AP supplicant did not start from the AP config")
	}
	if runner.scanCallCount() != 0 {
		t.Fatalf("startup scanned %d times", runner.scanCallCount())
	}
	apConfigData, err := os.ReadFile(filepath.Join(m.cfg.RunDir, "wpa-ap.conf"))
	if err != nil {
		t.Fatalf("read AP config: %v", err)
	}
	if !strings.Contains(string(apConfigData), "ssid=\"Capsule-Test\"") {
		t.Fatalf("AP config does not use the Bluetooth name: %s", apConfigData)
	}
	if info, err := os.Stat(filepath.Join(m.cfg.RunDir, "wpa-ap.conf")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("AP config permissions: %v %v", info, err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStartupConnectsSavedNetworkBeforeAP(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	runner.started = 0
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, _ := m.Status(context.Background())
	if status.State != StateConnected || status.SSID != "Home" {
		t.Fatalf("status = %+v", status)
	}
	if startContains(runner.snapshotStarts(), "dnsmasq", "dhcp-range") {
		t.Fatal("AP started although a saved network connected")
	}
	if !runContains(runner.snapshotRuns(), "wpa_cli", "select_network") {
		t.Fatal("saved network was not selected")
	}
	known, _ := m.KnownNetworks(context.Background())
	if len(known) != 1 || !known[0].Connected || !known[0].HasPassword {
		t.Fatalf("known = %+v", known)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStartupFallsBackWhenSavedNetworkNeverConnects(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	runner.networks = []string{"Home"}
	runner.connectOnSelect = false
	start := time.Now()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("startup was not bounded: %s", elapsed)
	}
	status, _ := m.Status(context.Background())
	if status.State != StateAPFallback || !status.APActive {
		t.Fatalf("status = %+v", status)
	}
	if !startContains(runner.snapshotStarts(), "dnsmasq", "dhcp-range") {
		t.Fatal("fallback AP did not start")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStatusAndKnownNetworksNeverScan(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	runner.scanRows = []string{"aa:bb:cc:dd:ee:01\t2412\t-40\t[WPA2-PSK-CCMP][ESS]\tNeighbour"}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 3 {
		if _, err := m.Status(context.Background()); err != nil {
			t.Fatalf("Status: %v", err)
		}
		if _, err := m.KnownNetworks(context.Background()); err != nil {
			t.Fatalf("KnownNetworks: %v", err)
		}
	}
	if got := runner.scanCallCount(); got != 0 {
		t.Fatalf("status reads triggered %d scans", got)
	}
	results, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if runner.scanCallCount() != 1 {
		t.Fatalf("explicit Scan made %d scan requests", runner.scanCallCount())
	}
	if len(results) != 1 || results[0].SSID != "Neighbour" {
		t.Fatalf("results = %+v", results)
	}
	_ = m.Close()
}

func TestConnectFromAPStopsPortalAndJoinsStation(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if status, _ := m.Status(context.Background()); !status.APActive {
		t.Fatalf("expected AP active: %+v", status)
	}
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	if err := m.Connect(context.Background(), ConnectRequest{SSID: "Home", Passphrase: "password123", Security: SecurityWPA2}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	status, _ := m.Status(context.Background())
	if status.State != StateConnected || status.SSID != "Home" || status.APActive {
		t.Fatalf("status = %+v", status)
	}
	if !runContains(runner.snapshotRuns(), "ip", "addr flush") {
		t.Fatal("station address was not reset after AP teardown")
	}
	_ = m.Close()
}

func TestFailedConnectRestoresAPWithError(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	runner.networks = []string{"Home"}
	runner.connectOnSelect = false
	err := m.Connect(context.Background(), ConnectRequest{SSID: "Home", Passphrase: "password123", Security: SecurityWPA2})
	if err == nil {
		t.Fatal("failed association reported success")
	}
	status, _ := m.Status(context.Background())
	if !status.APActive || status.State != StateAPFallback {
		t.Fatalf("AP was not restored: %+v", status)
	}
	if status.LastError == "" || !strings.Contains(status.LastError, "Home") {
		t.Fatalf("last error = %q", status.LastError)
	}
	_ = m.Close()
}

func TestFailedConnectFromStationRestoresPreviousAssociation(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	runner.networks = []string{"Home", "New"}
	runner.failSSID = "New"
	err := m.Connect(context.Background(), ConnectRequest{SSID: "New", Passphrase: "password456", Security: SecurityWPA2})
	if err == nil {
		t.Fatal("failed station connect reported success")
	}
	status, _ := m.Status(context.Background())
	if status.State != StateConnected || status.SSID != "Home" || status.APActive {
		t.Fatalf("previous association was not restored: %+v", status)
	}
	if status.LastError == "" {
		t.Fatal("failed attempt error was lost")
	}
	_ = m.Close()
}

func TestForgetRemovesNetworkFromStoreAndLiveSupplicant(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	seedNetwork(t, m, "Cafe", "password456", SecurityWPA2)
	runner.networks = []string{"Home", "Cafe"}
	runner.connectOnSelect = true
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Forget(context.Background(), "Home"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	known, _ := m.KnownNetworks(context.Background())
	if len(known) != 1 || known[0].SSID != "Cafe" {
		t.Fatalf("known = %+v", known)
	}
	store, err := m.store.list()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if len(store) != 1 || store[0].SSID != "Cafe" {
		t.Fatalf("store = %+v", store)
	}
	if err := m.Forget(context.Background(), "Missing"); err == nil {
		t.Fatal("forgetting an unknown network succeeded")
	}
	_ = m.Close()
}

func TestForgetOtherNetworkKeepsActiveConnection(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	seedNetwork(t, m, "Cafe", "password456", SecurityWPA2)
	runner.networks = []string{"Home", "Cafe"}
	runner.connectOnSelect = true
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Forget(context.Background(), "Cafe"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	status, _ := m.Status(context.Background())
	if status.State != StateConnected || status.SSID != "Home" {
		t.Fatalf("unrelated active connection was dropped: %+v", status)
	}
	_ = m.Close()
}

func TestReconnectToSavedNetworkWithoutReenteringPassword(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Connect(context.Background(), ConnectRequest{SSID: "Home", Security: SecurityWPA2}); err != nil {
		t.Fatalf("reconnect without passphrase: %v", err)
	}
	if err := m.Connect(context.Background(), ConnectRequest{SSID: "New", Security: SecurityWPA2}); err == nil {
		t.Fatal("new network with no passphrase was accepted")
	}
	_ = m.Close()
}

func TestSimulatorIsHonestWhenUnavailable(t *testing.T) {
	sim := NewSimulator(SimulatorOptions{})
	status, err := sim.Status(context.Background())
	if err != nil || status.Available || status.State != StateUnavailable {
		t.Fatalf("simulator status = %+v err=%v", status, err)
	}
	if _, err := sim.Scan(context.Background()); err == nil {
		t.Fatal("unavailable simulator scanned")
	}
	if err := sim.Connect(context.Background(), ConnectRequest{SSID: "Home", Passphrase: "password123"}); err == nil {
		t.Fatal("unavailable simulator connected")
	}
	if sim.ScanCalls() != 0 {
		t.Fatal("unavailable simulator counted a scan")
	}
	_ = sim.Close()
}

func TestParseStockConfigMigratesWPAAndOpenButSkipsWEP(t *testing.T) {
	data := `
network={
	ssid="Home"
	psk="password123"
	key_mgmt=WPA-PSK
	proto=RSN
	priority=3
}
network={
	ssid="Legacy"
	psk="password456"
	key_mgmt=WPA-PSK
	proto=WPA
}
network={
	ssid="Open Cafe"
	key_mgmt=NONE
}
network={
	ssid="Mixed"
	psk="password789"
	key_mgmt=WPA-PSK SAE
	proto=RSN
}
network={
	ssid="Old WEP"
	wep_key0="abcde"
}
network={
	ssid="Disabled"
	psk="password000"
	key_mgmt=WPA-PSK
	disabled=1
}
`
	got := parseStockConfig(data)
	if len(got) != 4 {
		t.Fatalf("parsed %d networks: %+v", len(got), got)
	}
	bySSID := map[string]storedNetwork{}
	for _, n := range got {
		bySSID[n.SSID] = n
		if n.Security != SecurityOpen && (!validPSK(n.PSK) || n.PSK == "password123") {
			t.Fatalf("network %q did not store a derived PSK", n.SSID)
		}
	}
	if bySSID["Home"].Security != SecurityWPA2 || bySSID["Legacy"].Security != SecurityWPA {
		t.Fatalf("security migration = %+v", bySSID)
	}
	if bySSID["Open Cafe"].Security != SecurityOpen || bySSID["Mixed"].Security != SecurityWPA2WPA3 {
		t.Fatalf("open/mixed migration = %+v", bySSID)
	}
	if _, ok := bySSID["Old WEP"]; ok {
		t.Fatal("WEP network was imported")
	}
	if _, ok := bySSID["Disabled"]; ok {
		t.Fatal("disabled network was imported")
	}
}

func TestStartupAdoptsStockKnownNetworksBeforeAP(t *testing.T) {
	stockPath := filepath.Join(t.TempDir(), "wpa_supplicant.conf")
	stock := `network={
	ssid="Home"
	psk="password123"
	key_mgmt=WPA-PSK
	proto=RSN
}
network={
	ssid="Open Cafe"
	key_mgmt=NONE
}
`
	if err := os.WriteFile(stockPath, []byte(stock), 0o600); err != nil {
		t.Fatalf("write stock config: %v", err)
	}
	runner := newFakeRunner()
	runner.connected = true
	runner.ssid = "Home"
	runner.networks = []string{"Home", "Open Cafe"}
	runner.connectOnSelect = true
	m := testManager(t, runner)
	m.stockConfigs = []string{stockPath}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	known, err := m.KnownNetworks(context.Background())
	if err != nil || len(known) != 2 {
		t.Fatalf("known = %+v err=%v", known, err)
	}
	if known[0].SSID != "Home" || !known[0].Connected {
		t.Fatalf("active stock network was not preferred: %+v", known)
	}
	if startContains(runner.snapshotStarts(), "dnsmasq", "dhcp-range") {
		t.Fatal("AP started even though stock had a usable association")
	}
	data, err := os.ReadFile(filepath.Join(m.store.dir, "networks.json"))
	if err != nil {
		t.Fatalf("read imported store: %v", err)
	}
	if strings.Contains(string(data), "password123") || !strings.Contains(string(data), "psk") {
		t.Fatalf("imported store leaked plaintext: %s", data)
	}
	if _, err := os.Stat(m.store.stockMarkerPath()); err != nil {
		t.Fatalf("stock import marker missing: %v", err)
	}
	_ = m.Close()
}

func TestStatusReconcilesSupplicantDropAndNotifiesOnce(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	runner.scanRows = []string{"aa:bb:cc:dd:ee:01\t2412\t-40\t[WPA2-PSK-CCMP][ESS]\tHome"}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := 0
	m.cfg.OnChange = func() { calls++ }
	runner.connected = false
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != StateDisconnected || status.SSID != "" {
		t.Fatalf("status did not reconcile the drop: %+v", status)
	}
	if calls != 1 {
		t.Fatalf("drop published %d changes, want 1", calls)
	}
	known, _ := m.KnownNetworks(context.Background())
	if len(known) != 1 || known[0].Connected {
		t.Fatalf("known network stayed connected after drop: %+v", known)
	}
	if _, _ = m.Status(context.Background()); calls != 1 {
		t.Fatalf("unchanged status published again: %d", calls)
	}
	results, err := m.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(results) != 1 || results[0].Connected {
		t.Fatalf("scan marked a disconnected network connected: %+v", results)
	}
	_ = m.Close()
}

func TestAPScanUnsupportedIsReportedAndDisabled(t *testing.T) {
	runner := newFakeRunner()
	runner.scanFAIL = true
	m := testManager(t, runner)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := time.Now()
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("AP scan error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("unsupported AP scan waited for %s", elapsed)
	}
	status, _ := m.Status(context.Background())
	if status.ScanSupported {
		t.Fatal("ScanSupported stayed true after the supplicant refused AP scan")
	}
	before := runner.scanCallCount()
	if _, err := m.Scan(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("second AP scan error = %v", err)
	}
	if runner.scanCallCount() != before {
		t.Fatal("known-unsupported scan still invoked wpa_cli")
	}
	_ = m.Close()
}

func TestConnectKeepsStationSupplicantAliveAfterOperation(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	if err := m.Connect(context.Background(), ConnectRequest{SSID: "Home", Passphrase: "password123", Security: SecurityWPA2}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	m.mu.Lock()
	station := m.station
	m.mu.Unlock()
	if station == nil || !station.running() {
		t.Fatal("station supplicant died when the Connect operation returned")
	}
	_ = m.Close()
}

func TestFailedConnectKeepsRecoveryAPAliveAfterOperation(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	runner.networks = []string{"Home"}
	runner.connectOnSelect = false
	if err := m.Connect(context.Background(), ConnectRequest{SSID: "Home", Passphrase: "password123", Security: SecurityWPA2}); err == nil {
		t.Fatal("failed association reported success")
	}
	m.mu.Lock()
	ap := m.ap
	m.mu.Unlock()
	if ap == nil || ap.supplicant == nil || !ap.supplicant.running() ||
		ap.dnsmasq == nil || !ap.dnsmasq.running() {
		t.Fatal("recovery AP died when the failed Connect operation returned")
	}
	_ = m.Close()
}

func TestForgetLastNetworkKeepsAPAliveAfterOperation(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	seedNetwork(t, m, "Home", "password123", SecurityWPA2)
	runner.networks = []string{"Home"}
	runner.connectOnSelect = true
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Forget(context.Background(), "Home"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	m.mu.Lock()
	ap := m.ap
	m.mu.Unlock()
	if ap == nil || ap.supplicant == nil || !ap.supplicant.running() ||
		ap.dnsmasq == nil || !ap.dnsmasq.running() {
		t.Fatal("setup AP died when Forget returned")
	}
	_ = m.Close()
}

func TestCloseStopsAPChildren(t *testing.T) {
	runner := newFakeRunner()
	m := testManager(t, runner)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if status, _ := m.Status(context.Background()); status.APActive {
		t.Fatalf("AP still active after Close: %+v", status)
	}
}
