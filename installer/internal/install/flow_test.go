package install

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capsulad/installer/internal/device"
	"capsulad/installer/internal/release"
)

type recordedCall struct {
	Args  []string
	Stdin string
}

type recordedPush struct {
	Local  string
	Remote string
}

type fakeDevice struct {
	t              *testing.T
	calls          []recordedCall
	pushes         []recordedPush
	connects       int
	certPEM        []byte
	deployLog      string
	authInitExit   int
	authSetExit    int
	sessionPresent bool
	sessionJSON    string
	wifiConnect    bool
	connectSSID    string
	authStatus     string
	installed      []string
	installFail    map[string]bool
	settingsFail   map[string]bool
	serial         string
	addr           string
}

func newFakeDevice(t *testing.T, certPEM []byte) *fakeDevice {
	return &fakeDevice{
		t:              t,
		certPEM:        certPEM,
		deployLog:      "capsulad deploy: installing\nexit=0\n",
		sessionPresent: true,
		sessionJSON:    `{"session":"sess-1","secret":"sec-1"}`,
		authStatus:     "admin password set: false\n",
		installFail:    map[string]bool{},
		settingsFail:   map[string]bool{},
		serial:         "RK3308-TEST-SERIAL",
		addr:           "192.168.1.20:5555",
	}
}

func (f *fakeDevice) Addr() string { return f.addr }
func (f *fakeDevice) Kind() string { return "adb" }
func (f *fakeDevice) Close() error { return nil }

func (f *fakeDevice) Connect(ctx context.Context) error {
	f.connects++
	return nil
}

func (f *fakeDevice) Push(ctx context.Context, local, remote string) error {
	if _, err := os.Stat(local); err != nil {
		f.t.Fatalf("push of %s: local file missing: %v", remote, err)
	}
	f.pushes = append(f.pushes, recordedPush{Local: local, Remote: remote})
	return nil
}

func (f *fakeDevice) Run(ctx context.Context, cmd device.Command) (device.Result, error) {
	f.calls = append(f.calls, recordedCall{Args: append([]string(nil), cmd.Args...), Stdin: string(cmd.Stdin)})
	out := func(s string) (device.Result, error) {
		return device.Result{Stdout: []byte(s)}, nil
	}
	if len(cmd.Args) == 0 {
		f.t.Fatal("empty command")
	}

	if cmd.Args[0] == "sh" && len(cmd.Args) >= 3 && cmd.Args[1] == "-c" {
		script := cmd.Args[2]
		switch {
		case strings.Contains(script, "tar -cf -"):
			if cmd.Stdout == nil {
				f.t.Fatal("backup tar has no stdout sink")
			}
			cmd.Stdout.Write(backupTar(f.t))
			if cmd.Stderr != nil {
				cmd.Stderr.Write([]byte(device.BackupSentinel + "\n"))
			}
			return device.Result{}, nil
		case strings.Contains(script, "mkdir -p"):
			return device.Result{}, nil
		}
		f.t.Fatalf("unexpected shell script: %s", script)
	}

	switch cmd.Args[0] {
	case "cat":
		p := cmd.Args[1]
		switch {
		case p == "/etc/os-release":
			return out("ID=buildroot\nNAME=\"Buildroot\"\n")
		case strings.Contains(p, "device-tree/compatible"):
			return out("rockchip,rk3308-capsula-mini\x00")
		case p == "/proc/device-tree/serial-number":
			return out(f.serial + "\x00")
		case p == "/tmp/capsulad-installer/deploy.log":
			return out(f.deployLog)
		case strings.HasSuffix(p, "/security/server.crt"):
			return out(string(f.certPEM))
		case p == "/data/voice-agent/session.json":
			if !f.sessionPresent {
				return device.Result{ExitCode: 1, Stderr: []byte("No such file or directory")}, nil
			}
			return out(f.sessionJSON)
		}
		f.t.Fatalf("unexpected cat %s", p)
	case "uname":
		return out("aarch64\n")
	case "hostname":
		return out("capsula-mini\n")
	case "getprop", "devinfo":
		return out("")
	case "stat":
		return device.Result{ExitCode: 127, Stderr: []byte("stat: not found")}, nil
	case "wc":
		return out("6 /data/capsdemo/capsulad\n")
	case "df":
		return out("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/root 1000000 400000 552000 43% /data\n")
	case "chmod", "touch", "rm", "mkdir":
		return device.Result{}, nil
	case "start-stop-daemon", "setsid":
		return device.Result{}, nil
	}

	if strings.HasSuffix(cmd.Args[0], "capsulad-cli") {
		return f.cli(cmd)
	}
	f.t.Fatalf("unexpected command: %v", cmd.Args)
	return device.Result{}, nil
}

func (f *fakeDevice) cli(cmd device.Command) (device.Result, error) {
	args := cmd.Args
	// cli -data-dir DIR subcommand...
	i := 0
	for i < len(args) && args[i] != "-data-dir" {
		i++
	}
	if i+1 >= len(args) {
		f.t.Fatalf("CLI command without -data-dir: %v", args)
	}
	tail := args[i+2:]
	if len(tail) == 0 {
		f.t.Fatalf("CLI command without a subcommand: %v", args)
	}
	has := func(word string) bool {
		for _, a := range tail {
			if a == word {
				return true
			}
		}
		return false
	}
	switch {
	case has("auth") && has("status"):
		return device.Result{Stdout: []byte(f.authStatus)}, nil
	case has("auth") && has("init"):
		return device.Result{ExitCode: f.authInitExit, Stderr: []byte("auth failed")}, nil
	case has("auth") && has("set-password"):
		return device.Result{ExitCode: f.authSetExit, Stderr: []byte("set-password failed")}, nil
	case has("wifi") && has("status"):
		if f.wifiConnect {
			ssid := f.connectSSID
			if ssid == "" {
				ssid = "Home"
			}
			return device.Result{Stdout: []byte(fmt.Sprintf(`{"wifi":{"available":true,"state":"connected","ssid":%q,"ip_address":"192.168.1.21","scan_supported":true}}`, ssid))}, nil
		}
		return device.Result{Stdout: []byte(`{"wifi":{"available":true,"state":"disconnected","ap_active":true,"ap_ssid":"Capsula-Setup","ap_address":"192.168.4.1","ap_portal_url":"http://192.168.4.1","ap_password_hint":"on the label","scan_supported":true}}`)}, nil
	case has("wifi") && has("scan"):
		return device.Result{Stdout: []byte(`{"message":"scan complete","results":[
			{"ssid":"Home","security":"wpa2","signal_dbm":-70,"frequency_mhz":2412,"connected":false},
			{"ssid":"Home","security":"wpa2","signal_dbm":-45,"frequency_mhz":5180,"connected":false},
			{"ssid":"Cafe","security":"","signal_dbm":-60,"frequency_mhz":2412,"connected":false}
		],"wifi":{"state":"disconnected","scan_supported":true}}`)}, nil
	case has("wifi") && has("connect"):
		f.wifiConnect = true
		for i := 0; i+1 < len(tail); i++ {
			if tail[i] == "connect" {
				f.connectSSID = tail[i+1]
				break
			}
		}
		return device.Result{Stdout: []byte(`{"message":"connecting","wifi":{"state":"connecting","scan_supported":true}}`)}, nil
	case has("app") && has("install"):
		remote := tail[len(tail)-1]
		id := appIDFromArchive(remote)
		if f.installFail[id] {
			return device.Result{ExitCode: 1, Stderr: []byte("install failed")}, nil
		}
		f.installed = append(f.installed, id)
		return device.Result{Stdout: []byte("installed " + id + "\n")}, nil
	case has("app") && has("list"):
		var b strings.Builder
		for _, id := range f.installed {
			fmt.Fprintf(&b, "%s  Display Name  0.1.0  quickjs\n", id)
		}
		return device.Result{Stdout: []byte(b.String())}, nil
	case has("settings"):
		for key, fail := range f.settingsFail {
			if fail && has(key+"@-") {
				return device.Result{ExitCode: 1, Stderr: []byte("settings failed for a secret value")}, nil
			}
		}
		return device.Result{Stdout: []byte("ok\n")}, nil
	}
	f.t.Fatalf("unexpected CLI command: %v", args)
	return device.Result{}, nil
}

func backupTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct{ name, body string }{
		{"data/lost+found", "excluded"},
		{"data/voice-agent/session.json", `{"session":"x"}`},
		{"etc/init.d/S99btctl.sh", "# hook"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func appIDFromArchive(remote string) string {
	base := filepath.Base(remote)
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".gz"), ".tar")
	if i := strings.LastIndex(base, "-"); i > 0 {
		return base[:i]
	}
	return base
}

type flowFixture struct {
	inst    *Installer
	dev     *fakeDevice
	root    string
	server  *httptest.Server
	config  string
	release *release.Manifest
}

func newFlowFixture(t *testing.T, withCLI bool, appIDs ...string) *flowFixture {
	t.Helper()
	root := t.TempDir()
	bundleFiles := map[string]string{"capsulad": "daemon", "boot/S99btctl.sh": "hook"}
	if withCLI {
		bundleFiles["capsulad-cli"] = "cli-bytes"
	}
	makeBundle(t, filepath.Join(root, "rockchip-stock", "deploy-bundle.tar.gz"), bundleFiles)
	for _, id := range appIDs {
		makeBundle(t, filepath.Join(root, "rockchip-stock", "apps", id+"-0.1.0.tar.gz"), map[string]string{"manifest.toml": "id=\"" + id + "\""})
	}
	m, err := release.DiscoverLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == HealthPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	dev := newFakeDevice(t, certPEM)
	configDir := filepath.Join(t.TempDir(), "capsulad-installer")
	inst, err := New(dev, release.NewLocalProvider(root, m), device.PlatformRockchipStock, configDir)
	if err != nil {
		t.Fatal(err)
	}
	inst.HealthBaseURL = srv.URL
	inst.HealthTimeout = 5 * time.Second
	inst.ReconnectWindow = 5 * time.Second
	inst.Sleep = func(context.Context, time.Duration) error { return nil }
	return &flowFixture{inst: inst, dev: dev, root: root, server: srv, config: inst.ConfigDir, release: m}
}

type wantCall struct {
	desc  string
	match func(recordedCall) bool
}

func firstArgIs(c recordedCall, name string) bool {
	return len(c.Args) > 0 && c.Args[len(c.Args)-1] != "" && c.Args[0] == name
}

func cliHas(c recordedCall, words ...string) bool {
	if len(c.Args) == 0 || !strings.HasSuffix(c.Args[0], "capsulad-cli") {
		return false
	}
	joined := " " + strings.Join(c.Args, " ") + " "
	for _, w := range words {
		if !strings.Contains(joined, " "+w+" ") {
			return false
		}
	}
	return true
}

func assertOrder(t *testing.T, calls []recordedCall, wants []wantCall) {
	t.Helper()
	next := 0
	var found []string
	for _, c := range calls {
		if next >= len(wants) {
			break
		}
		if wants[next].match(c) {
			found = append(found, wants[next].desc)
			next++
		}
	}
	if next != len(wants) {
		var all []string
		for _, c := range calls {
			all = append(all, strings.Join(c.Args, " "))
		}
		t.Fatalf("command sequence is incomplete: found %v, missing %q\ncalls:\n%s",
			found, wants[next].desc, strings.Join(all, "\n"))
	}
}

func TestFakeFlowStockHappyPath(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon", "org.capsulad.marusia")
	ctx := context.Background()
	password := "generated-Password-1234"
	passphrase := "wifi-passphrase-value"

	det, err := device.ConnectAndDetect(ctx, fx.dev)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if det.Platform != device.PlatformRockchipStock || det.Arch != "aarch64" {
		t.Fatalf("detection = %+v", det)
	}
	if err := device.CheckFirmware(device.FirmwareStock, det.Platform); err != nil {
		t.Fatal(err)
	}

	auth, err := fx.inst.AuthStatus(ctx)
	if err != nil || auth.Configured || !auth.Known {
		t.Fatalf("pre-existing auth = %+v, %v", auth, err)
	}
	hostname := fx.inst.Hostname(ctx)
	if hostname != "capsula-mini" {
		t.Fatalf("hostname = %q", hostname)
	}

	backup, err := fx.inst.Backup(ctx, fx.dev.Addr())
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := os.Stat(backup.Path); err != nil {
		t.Fatalf("backup file: %v", err)
	}
	if _, err := os.Stat(backup.ManifestPath); err != nil {
		t.Fatalf("backup manifest: %v", err)
	}
	if backup.Entries == 0 || backup.Bytes == 0 {
		t.Fatalf("backup = %+v", backup)
	}
	if !strings.Contains(backup.Path, filepath.Join("capsulad-installer", "backups")) {
		t.Fatalf("backup path = %q", backup.Path)
	}

	space, err := fx.inst.PreflightStock(ctx)
	if err != nil {
		t.Fatalf("stock preflight: %v", err)
	}
	if !space.OK || space.ReserveBytes != StockDeployReserveBytes {
		t.Fatalf("stock preflight = %+v", space)
	}
	if space.StagedDaemon != 6 {
		t.Fatalf("staged daemon size = %d", space.StagedDaemon)
	}
	if space.RequiredBytes != StockDeployReserveBytes {
		t.Fatalf("fresh-device requirement = %d", space.RequiredBytes)
	}
	if space.InstalledDaemon != 6 {
		t.Fatalf("installed daemon size = %d", space.InstalledDaemon)
	}

	dep, err := fx.inst.Deploy(ctx, false, func(ctx context.Context) error {
		return fx.inst.ApplyPassword(ctx, password, hostname, fx.dev.Addr())
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if dep.Exit != 0 || dep.Unconfirmed || !dep.CredentialsApplied {
		t.Fatalf("deploy result = %+v", dep)
	}
	fp, err := fx.inst.WaitHealthy(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if fp == "" {
		t.Fatal("empty certificate fingerprint")
	}

	status, err := fx.inst.WiFiStatus(ctx)
	if err != nil {
		t.Fatalf("wifi status: %v", err)
	}
	if status.Connected() {
		t.Fatalf("fixture should start disconnected: %+v", status)
	}
	if !status.APActive || status.APSSID == "" || status.APPasswordHint == "" || status.APPortalURL == "" {
		t.Fatalf("setup AP details were not parsed: %+v", status)
	}
	nets, err := fx.inst.WiFiScan(ctx)
	if err != nil {
		t.Fatalf("wifi scan: %v", err)
	}
	if len(nets) != 2 || nets[0].SSID != "Home" || nets[0].SignalDBM != -45 {
		t.Fatalf("networks = %+v", nets)
	}
	security := SecurityMode(nets[0].Security)
	w, err := fx.inst.WiFiConnect(ctx, "Home", security, passphrase)
	if err != nil {
		t.Fatalf("wifi connect: %v", err)
	}
	if !w.Connected() || w.IPAddress == "" {
		t.Fatalf("wifi after connect = %+v", w)
	}

	results, err := fx.inst.InstallApps(ctx, map[string]bool{
		"org.capsulad.simon":   true,
		"org.capsulad.marusia": true,
	})
	if err != nil {
		t.Fatalf("install apps: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	for _, r := range results {
		if r.Status != "installed" {
			t.Fatalf("%s: %+v", r.ID, r)
		}
	}

	mr, err := fx.inst.Marusia(ctx, "")
	if err != nil {
		t.Fatalf("marusia: %v", err)
	}
	if !mr.Migrated || mr.TokenSet {
		t.Fatalf("marusia = %+v", mr)
	}
	var migrations []recordedCall
	for _, c := range fx.dev.calls {
		if cliHas(c, "settings", "app", "org.capsulad.marusia", "session_id@-") ||
			cliHas(c, "settings", "app", "org.capsulad.marusia", "session_secret@-") {
			migrations = append(migrations, c)
		}
	}
	if len(migrations) != 2 {
		t.Fatalf("migration commands = %d", len(migrations))
	}
	for _, c := range migrations {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "sess-1") || strings.Contains(joined, "sec-1") {
			t.Fatalf("migrated secret appeared in argv: %v", c.Args)
		}
	}
	if migrations[0].Stdin != "sess-1\n" || migrations[1].Stdin != "sec-1\n" {
		t.Fatalf("migration stdins = %q, %q", migrations[0].Stdin, migrations[1].Stdin)
	}

	// Secrets never reach argv; they travel on stdin.
	for _, c := range fx.dev.calls {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, password) {
			t.Fatalf("admin password appeared in argv: %v", c.Args)
		}
		if strings.Contains(joined, passphrase) {
			t.Fatalf("Wi-Fi passphrase appeared in argv: %v", c.Args)
		}
	}
	authInit := findCall(fx.dev.calls, func(c recordedCall) bool {
		return cliHas(c, "auth", "init", "-password-stdin")
	})
	if authInit == nil || authInit.Stdin != password+"\n" {
		t.Fatalf("auth init stdin = %q", authInitStdin(authInit))
	}
	authSet := findCall(fx.dev.calls, func(c recordedCall) bool {
		return cliHas(c, "auth", "set-password", "-password-stdin")
	})
	if authSet == nil || authSet.Stdin != password+"\n" {
		t.Fatalf("auth set-password stdin = %q", authInitStdin(authSet))
	}
	connect := findCall(fx.dev.calls, func(c recordedCall) bool { return cliHas(c, "wifi", "connect") })
	if connect == nil || connect.Stdin != passphrase+"\n" {
		t.Fatalf("wifi connect stdin = %q", connect.Stdin)
	}
	if !strings.Contains(strings.Join(connect.Args, " "), "wifi connect Home -password-stdin -security wpa2") {
		t.Fatalf("wifi connect args = %v", connect.Args)
	}

	// Pushes: bundle, device CLI, deploy script — in that order.
	if len(fx.dev.pushes) < 3 {
		t.Fatalf("pushes = %+v", fx.dev.pushes)
	}
	if !strings.HasSuffix(fx.dev.pushes[0].Remote, "deploy-bundle.tar.gz") {
		t.Fatalf("first push = %+v", fx.dev.pushes[0])
	}
	if !strings.HasSuffix(fx.dev.pushes[1].Remote, "capsulad-cli") {
		t.Fatalf("second push = %+v", fx.dev.pushes[1])
	}
	if !strings.HasSuffix(fx.dev.pushes[2].Remote, "deploy.sh") {
		t.Fatalf("third push = %+v", fx.dev.pushes[2])
	}

	assertOrder(t, fx.dev.calls, []wantCall{
		{"read os-release", func(c recordedCall) bool { return firstArgIs(c, "cat") && c.Args[1] == "/etc/os-release" }},
		{"read compatible", func(c recordedCall) bool { return firstCallHas(c, "cat", "compatible") }},
		{"auth status before changes", func(c recordedCall) bool { return cliHas(c, "auth", "status") }},
		{"backup tar stream", func(c recordedCall) bool { return c.Args[0] == "sh" && strings.Contains(c.Args[2], "tar -cf -") }},
		{"installed daemon size", func(c recordedCall) bool { return callHas(c, "stat", "/data/capsdemo/capsulad") }},
		{"installed size fallback", func(c recordedCall) bool { return callHas(c, "wc", "/data/capsdemo/capsulad") }},
		{"data filesystem space", func(c recordedCall) bool { return callHas(c, "df", "/data/capsdemo") }},
		{"remote mkdir", func(c recordedCall) bool { return c.Args[0] == "sh" && strings.Contains(c.Args[2], "mkdir -p") }},
		{"jingle-off marker", func(c recordedCall) bool {
			return c.Args[0] == "sh" && strings.Contains(c.Args[2], "startup-sound.played")
		}},
		{"auth init before launch", func(c recordedCall) bool { return cliHas(c, "auth", "init", "-password-stdin") }},
		{"auth set-password before launch", func(c recordedCall) bool {
			return cliHas(c, "auth", "set-password", "-password-stdin")
		}},
		{"clear previous deploy log", func(c recordedCall) bool {
			return c.Args[0] == "rm" && strings.Contains(strings.Join(c.Args, " "), "deploy.log")
		}},
		{"detached launch", func(c recordedCall) bool { return c.Args[0] == "start-stop-daemon" }},
		{"deploy log", func(c recordedCall) bool { return firstArgIs(c, "cat") && strings.HasSuffix(c.Args[1], "deploy.log") }},
		{"read device certificate", func(c recordedCall) bool {
			return firstArgIs(c, "cat") && strings.HasSuffix(c.Args[1], "security/server.crt")
		}},
		{"wifi status", func(c recordedCall) bool { return cliHas(c, "wifi", "status") }},
		{"wifi scan", func(c recordedCall) bool { return cliHas(c, "wifi", "scan") }},
		{"wifi connect", func(c recordedCall) bool { return cliHas(c, "wifi", "connect") }},
		{"install marusia", func(c recordedCall) bool {
			return cliHas(c, "app", "install") && strings.Contains(strings.Join(c.Args, " "), "org.capsulad.marusia")
		}},
		{"list after marusia", func(c recordedCall) bool { return cliHas(c, "app", "list") }},
		{"install simon", func(c recordedCall) bool {
			return cliHas(c, "app", "install") && strings.Contains(strings.Join(c.Args, " "), "org.capsulad.simon")
		}},
		{"list after simon", func(c recordedCall) bool { return cliHas(c, "app", "list") }},
		{"read stock session", func(c recordedCall) bool {
			return firstArgIs(c, "cat") && c.Args[1] == "/data/voice-agent/session.json"
		}},
		{"migrate session", func(c recordedCall) bool {
			return cliHas(c, "settings", "app", "org.capsulad.marusia", "session_id@-")
		}},
	})
}

func firstCallHas(c recordedCall, first, contains string) bool {
	return len(c.Args) > 1 && c.Args[0] == first && strings.Contains(c.Args[1], contains)
}

// callHas matches the command name and every substring anywhere in the argv.
func callHas(c recordedCall, first string, contains ...string) bool {
	if len(c.Args) == 0 || c.Args[0] != first {
		return false
	}
	joined := " " + strings.Join(c.Args, " ") + " "
	for _, s := range contains {
		if !strings.Contains(joined, s) {
			return false
		}
	}
	return true
}

func findCall(calls []recordedCall, match func(recordedCall) bool) *recordedCall {
	for i := range calls {
		if match(calls[i]) {
			return &calls[i]
		}
	}
	return nil
}

func authInitStdin(c *recordedCall) string {
	if c == nil {
		return "<missing>"
	}
	return c.Stdin
}

func TestFakeFlowOpenNetwork(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	w, err := fx.inst.WiFiConnect(context.Background(), "Cafe", "open", "")
	if err != nil {
		t.Fatalf("open wifi connect: %v", err)
	}
	if !w.Connected() || w.SSID != "Cafe" {
		t.Fatalf("wifi after open connect = %+v", w)
	}
	connect := findCall(fx.dev.calls, func(c recordedCall) bool { return cliHas(c, "wifi", "connect") })
	if connect == nil {
		t.Fatal("no connect command")
	}
	joined := " " + strings.Join(connect.Args, " ") + " "
	if !strings.Contains(joined, " -security open ") {
		t.Fatalf("open connect lacks -security open: %v", connect.Args)
	}
	if strings.Contains(joined, " -password-stdin ") {
		t.Fatalf("open connect must not request a stdin password: %v", connect.Args)
	}
	if connect.Stdin != "" {
		t.Fatalf("open connect received stdin %q", connect.Stdin)
	}
}

func TestFakeFlowDeployFailure(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	fx.dev.deployLog = "capsulad deploy: failed to start\nboom\nexit=1\n"
	dep, err := fx.inst.Deploy(context.Background(), false, func(ctx context.Context) error {
		return fx.inst.ApplyPassword(ctx, "generated-Password-1234", "capsula-mini", fx.dev.Addr())
	})
	if err == nil {
		t.Fatal("expected a deploy failure")
	}
	var derr *DeployError
	if !asDeployError(err, &derr) || derr.Exit != 1 || derr.Unconfirmed {
		t.Fatalf("error = %#v (%v)", err, err)
	}
	if !strings.Contains(derr.Log, "boom") {
		t.Fatalf("log = %q", derr.Log)
	}
	if !derr.CredentialsApplied || !dep.CredentialsApplied {
		t.Fatalf("credentials note missing: %+v / %+v", derr, dep)
	}
	if !strings.Contains(err.Error(), "credentials were written") {
		t.Fatalf("error does not name the credential state: %v", err)
	}
}

func asDeployError(err error, target **DeployError) bool {
	e, ok := err.(*DeployError)
	if ok {
		*target = e
	}
	return ok
}

func TestFakeFlowBundleWithoutCLI(t *testing.T) {
	fx := newFlowFixture(t, false, "org.capsulad.simon")
	_, err := fx.inst.Deploy(context.Background(), false, nil)
	if err == nil || !strings.Contains(err.Error(), "BUNDLE_CLI=1") {
		t.Fatalf("error = %v", err)
	}
	if len(fx.dev.pushes) != 0 {
		t.Fatalf("pushes before the bundle check = %+v", fx.dev.pushes)
	}
	if findCall(fx.dev.calls, func(c recordedCall) bool { return c.Args[0] == "touch" }) != nil {
		t.Fatal("a refused bundle must not change the device")
	}
}

func TestFakeFlowAuthFailure(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	ctx := context.Background()
	fx.dev.authInitExit = 1
	_, err := fx.inst.Deploy(ctx, false, func(ctx context.Context) error {
		return fx.inst.ApplyPassword(ctx, "generated-Password-1234", "capsula-mini", fx.dev.Addr())
	})
	if err == nil || !strings.Contains(err.Error(), "auth init") {
		t.Fatalf("error = %v", err)
	}
	if findCall(fx.dev.calls, func(c recordedCall) bool { return c.Args[0] == "start-stop-daemon" }) != nil {
		t.Fatal("a failed credential step must not launch the deployment")
	}
	for _, c := range fx.dev.calls {
		if strings.Contains(strings.Join(c.Args, " "), "generated-Password-1234") {
			t.Fatalf("password in argv: %v", c.Args)
		}
	}
}

func TestFakeFlowFreshAuthBeforeDeploy(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	ctx := context.Background()
	password := "generated-Password-1234"
	dep, err := fx.inst.Deploy(ctx, false, func(ctx context.Context) error {
		return fx.inst.ApplyPassword(ctx, password, "capsula-mini", fx.dev.Addr())
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !dep.CredentialsApplied {
		t.Fatalf("deploy result = %+v", dep)
	}
	authIx, setIx, rmIx, launchIx := -1, -1, -1, -1
	for i, c := range fx.dev.calls {
		joined := strings.Join(c.Args, " ")
		switch {
		case cliHas(c, "auth", "init", "-password-stdin"):
			authIx = i
		case cliHas(c, "auth", "set-password", "-password-stdin"):
			setIx = i
		case c.Args[0] == "rm" && strings.Contains(joined, "deploy.log"):
			rmIx = i
		case c.Args[0] == "start-stop-daemon":
			launchIx = i
		}
	}
	if authIx < 0 || setIx < 0 || rmIx < 0 || launchIx < 0 {
		t.Fatalf("missing order anchors: init=%d set=%d rm=%d launch=%d", authIx, setIx, rmIx, launchIx)
	}
	if !(authIx < setIx && setIx < rmIx && rmIx < launchIx) {
		t.Fatalf("credentials must precede log removal and launch: init=%d set=%d rm=%d launch=%d", authIx, setIx, rmIx, launchIx)
	}
	for _, ix := range []int{authIx, setIx} {
		c := fx.dev.calls[ix]
		if c.Stdin != password+"\n" || strings.Contains(strings.Join(c.Args, " "), password) {
			t.Fatalf("credential stdin/argv = %q / %v", c.Stdin, c.Args)
		}
	}
}

func TestFakeFlowKeepExistingPassword(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	fx.dev.authStatus = "admin password set: true\n"
	auth, err := fx.inst.AuthStatus(context.Background())
	if err != nil || !auth.Configured || !auth.Known {
		t.Fatalf("auth status = %+v, %v", auth, err)
	}
	if err := fx.inst.VerifyKeptCredentials(context.Background()); err != nil {
		t.Fatalf("verify kept credentials: %v", err)
	}
	dep, err := fx.inst.Deploy(context.Background(), false, nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if dep.CredentialsApplied {
		t.Fatalf("keep-existing must not report applied credentials: %+v", dep)
	}
	if findCall(fx.dev.calls, func(c recordedCall) bool { return cliHas(c, "auth", "init") || cliHas(c, "auth", "set-password") }) != nil {
		t.Fatal("keep-existing must not run a credential command")
	}
	if findCall(fx.dev.calls, func(c recordedCall) bool { return c.Args[0] == "start-stop-daemon" }) == nil {
		t.Fatal("keep-existing must still launch the deployment")
	}
}

func TestFakeFlowKeepMissingCertificate(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	fx.dev.authStatus = "admin password set: true\n"
	fx.dev.certPEM = nil
	err := fx.inst.VerifyKeptCredentials(context.Background())
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("error = %v", err)
	}
	if findCall(fx.dev.calls, func(c recordedCall) bool { return c.Args[0] == "start-stop-daemon" }) != nil {
		t.Fatal("missing certificate must not launch anything")
	}
}

func TestFakeFlowSetPasswordFailure(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	fx.dev.authSetExit = 1
	_, err := fx.inst.Deploy(context.Background(), false, func(ctx context.Context) error {
		return fx.inst.ApplyPassword(ctx, "generated-Password-1234", "capsula-mini", fx.dev.Addr())
	})
	if err == nil || !strings.Contains(err.Error(), "auth set-password") {
		t.Fatalf("error = %v", err)
	}
	if findCall(fx.dev.calls, func(c recordedCall) bool { return c.Args[0] == "start-stop-daemon" }) != nil {
		t.Fatal("a failed set-password must not launch the deployment")
	}
}

func TestFakeFlowMissingArtifact(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	results, err := fx.inst.InstallApps(context.Background(), map[string]bool{
		"org.capsulad.simon":   true,
		"org.capsulad.marusia": true,
	})
	if err == nil {
		t.Fatal("expected a missing-artifact error")
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	for _, r := range results {
		if r.ID == "org.capsulad.marusia" {
			if r.Status != "failed" || !strings.Contains(r.Detail, "artifact unavailable") {
				t.Fatalf("missing artifact was not reported: %+v", r)
			}
		}
	}
}

func TestFakeFlowTokenNeverInArgv(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.marusia")
	fx.dev.sessionPresent = false
	token := "vk-token-value-123"
	mr, err := fx.inst.Marusia(context.Background(), token)
	if err != nil {
		t.Fatalf("marusia: %v", err)
	}
	if !mr.TokenSet || mr.Migrated {
		t.Fatalf("result = %+v", mr)
	}
	secret := findCall(fx.dev.calls, func(c recordedCall) bool {
		return cliHas(c, "settings", "app", "org.capsulad.marusia", "vk_access_token@-")
	})
	if secret == nil {
		t.Fatal("no token-setting command")
	}
	if strings.Contains(strings.Join(secret.Args, " "), token) {
		t.Fatalf("token appeared in argv: %v", secret.Args)
	}
	if secret.Stdin != token+"\n" {
		t.Fatalf("token stdin = %q", secret.Stdin)
	}
	if !strings.Contains(strings.Join(secret.Args, " "), "vk_access_token@-") {
		t.Fatalf("token args = %v", secret.Args)
	}
}

func TestFakeFlowMarusiaMigrationFailureDoesNotLeak(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.marusia")
	fx.dev.settingsFail["session_secret"] = true
	_, err := fx.inst.Marusia(context.Background(), "")
	if err == nil {
		t.Fatal("expected a settings failure")
	}
	for _, leak := range []string{"sess-1", "sec-1"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaked the session value %q: %v", leak, err)
		}
	}
}

func TestFakeFlowDeployUnconfirmed(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	fx.inst.ReconnectWindow = time.Nanosecond
	fx.dev.deployLog = "still going\n"
	// A no-op sleep plus a 1ns window makes the first loop the last.
	_, err := fx.inst.Deploy(context.Background(), false, nil)
	if err == nil {
		t.Fatal("expected an unconfirmed result")
	}
	derr, ok := err.(*DeployError)
	if !ok || !derr.Unconfirmed {
		t.Fatalf("error = %#v", err)
	}
}

func TestFakeFlowIdentityMatchAndMismatch(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	ctx := context.Background()
	id, err := device.ReadIdentity(ctx, fx.dev)
	if err != nil || !id.Available() {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if id.Serial != "RK3308-TEST-SERIAL" || id.Source != "device-tree" {
		t.Fatalf("identity = %+v", id)
	}
	if err := fx.inst.VerifyIdentity(ctx, id); err != nil {
		t.Fatalf("matching identity: %v", err)
	}
	fx.dev.serial = "OTHER-DEVICE"
	err = fx.inst.VerifyIdentity(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("mismatch = %v", err)
	}
}

func TestFakeFlowIdentityUnavailable(t *testing.T) {
	fx := newFlowFixture(t, true, "org.capsulad.simon")
	fx.dev.serial = ""
	id, err := device.ReadIdentity(context.Background(), fx.dev)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id.Available() {
		t.Fatalf("identity = %+v", id)
	}
	if err := fx.inst.VerifyIdentity(context.Background(), id); err != nil {
		t.Fatalf("unavailable identity should only warn: %v", err)
	}
}
