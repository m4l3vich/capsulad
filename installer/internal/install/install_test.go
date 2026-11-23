package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capsulad/installer/internal/device"
	"capsulad/installer/internal/release"
)

func TestGeneratePassword(t *testing.T) {
	p, err := GeneratePassword(GeneratedPasswordLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) < GeneratedPasswordLength {
		t.Fatalf("password length = %d", len(p))
	}
	for _, r := range p {
		if !strings.ContainsRune(passwordAlphabet, r) {
			t.Fatalf("password has unexpected character %q", r)
		}
	}
	q, err := GeneratePassword(GeneratedPasswordLength)
	if err != nil {
		t.Fatal(err)
	}
	if p == q {
		t.Fatal("two generated passwords are identical")
	}
	if err := ValidatePassword("short"); err == nil {
		t.Fatal("expected a short password to be refused")
	}
	if err := ValidatePassword("long-enough-password"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePassword("line\nbreak"); err == nil {
		t.Fatal("expected a newline to be refused")
	}
}

func TestSelectedAppsOrderAndUnknown(t *testing.T) {
	apps, err := SelectedApps(map[string]bool{
		"org.capsulad.simon":   true,
		"org.capsulad.airplay": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].ID != "org.capsulad.airplay" || apps[1].ID != "org.capsulad.simon" {
		t.Fatalf("order = %+v", apps)
	}
	if _, err := SelectedApps(map[string]bool{"org.capsulad.hardwaretest": true}); err == nil {
		t.Fatal("an unknown app must be rejected")
	}
	if len(Catalogue) != 10 {
		t.Fatalf("catalogue has %d apps", len(Catalogue))
	}
}

func TestAppListIDs(t *testing.T) {
	out := "org.capsulad.simon   Simon Says   0.1.0   quickjs\n" +
		"org.capsulad.pa  PA Speaker 2.0.0  micropython\n" +
		"\n"
	ids := AppListIDs(out)
	if len(ids) != 2 || ids[0] != "org.capsulad.simon" || ids[1] != "org.capsulad.pa" {
		t.Fatalf("ids = %v", ids)
	}
	if !HasAppID(out, "org.capsulad.pa") || HasAppID(out, "org.capsulad.radio") {
		t.Fatal("HasAppID is wrong")
	}
}

func TestWifiConnectArgsSecurity(t *testing.T) {
	open := " " + strings.Join(WifiConnectArgs("/tmp/cli", "/data/capsdemo", "https://127.0.0.1:8443", "Cafe", "open", false), " ") + " "
	if !strings.Contains(open, " -security open ") || strings.Contains(open, " -password-stdin ") {
		t.Fatalf("open args = %q", open)
	}
	secured := " " + strings.Join(WifiConnectArgs("/tmp/cli", "/data/capsdemo", "https://127.0.0.1:8443", "Home", "wpa2", true), " ") + " "
	if !strings.Contains(secured, " -security wpa2 ") || !strings.Contains(secured, " -password-stdin ") {
		t.Fatalf("secured args = %q", secured)
	}
	defaultMode := " " + strings.Join(WifiConnectArgs("/tmp/cli", "/data/capsdemo", "https://127.0.0.1:8443", "Home", "", true), " ") + " "
	if strings.Contains(defaultMode, " -security ") || !strings.Contains(defaultMode, " -password-stdin ") {
		t.Fatalf("default-mode args = %q", defaultMode)
	}
}

func TestDFAndSpaceMath(t *testing.T) {
	info, err := ParseDF("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/root 1000000 400000 552000 43% /var/lib\n")
	if err != nil {
		t.Fatal(err)
	}
	if info.Available != 552000*1024 || info.MountedOn != "/var/lib" {
		t.Fatalf("info = %+v", info)
	}
	check := CheckSpace(info.Available, 100<<20, 20<<20, AlpineReserveBytes)
	if !check.OK || check.RequiredBytes != (100<<20)+(20<<20)+AlpineReserveBytes {
		t.Fatalf("check = %+v", check)
	}
	tight := CheckSpace(10<<20, 100<<20, 0, AlpineReserveBytes)
	if tight.OK {
		t.Fatal("expected insufficient space")
	}
	if _, err := ParseDF("garbage"); err == nil {
		t.Fatal("expected a parse error")
	}
	if got := BytesLabel(2 << 20); got != "2.0 MiB" {
		t.Fatalf("label = %q", got)
	}
}

func TestWiFiParsingAndDedupe(t *testing.T) {
	status := []byte(`{"wifi":{"available":true,"state":"connected","interface":"wlan0","ssid":"Home","ip_address":"192.168.1.20","security":"wpa2","ap_active":false,"scan_supported":true,"known_networks":[{"ssid":"Home","security":"wpa2","has_password":true,"connected":true}]}}`)
	parsed, err := ParseWiFiStatus(status)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.WiFi.Connected() || parsed.WiFi.IPAddress != "192.168.1.20" {
		t.Fatalf("parsed = %+v", parsed.WiFi)
	}
	if parsed.WiFi.KnownNetworks[0].SSID != "Home" {
		t.Fatal("known networks were not parsed")
	}

	scan := []byte(`{"message":"ok","results":[
		{"ssid":"Home","security":"wpa2","signal_dbm":-80,"frequency_mhz":2412,"connected":false},
		{"ssid":"Home","security":"wpa2","signal_dbm":-50,"frequency_mhz":5180,"connected":false},
		{"ssid":"Cafe","security":"","signal_dbm":-40,"frequency_mhz":2412,"connected":false},
		{"ssid":"","security":"wpa2","signal_dbm":-10,"frequency_mhz":2412,"connected":false},
		{"ssid":"Old","security":"wpa2","signal_dbm":-90,"frequency_mhz":2412,"connected":true}
	],"wifi":{"state":"disconnected"}}`)
	ws, err := ParseWiFiScan(scan)
	if err != nil {
		t.Fatal(err)
	}
	nets := DedupeNetworks(ws.Results)
	if len(nets) != 3 {
		t.Fatalf("dedupe = %+v", nets)
	}
	if nets[0].SSID != "Old" {
		t.Fatalf("connected network must sort first: %+v", nets)
	}
	for _, n := range nets {
		if n.SSID == "Home" && n.SignalDBM != -50 {
			t.Fatalf("Home kept weak signal %d", n.SignalDBM)
		}
	}
	if SecurityNeedsPassword("") || !SecurityNeedsPassword("WPA2") {
		t.Fatal("security classification is wrong")
	}
}

func TestMarusiaSessionParse(t *testing.T) {
	if _, ok := ParseMarusiaSession([]byte(`{"session":"abc"}`)); ok {
		t.Fatal("a session without a secret must not migrate")
	}
	if _, ok := ParseMarusiaSession([]byte(`not json`)); ok {
		t.Fatal("invalid JSON must not migrate")
	}
	s, ok := ParseMarusiaSession([]byte(`{"session":" vendor-session-value ","secret":" vendor-secret-value "}`))
	if !ok || s.Session != "vendor-session-value" || s.Secret != "vendor-secret-value" {
		t.Fatalf("session = %+v, %v", s, ok)
	}
	cmds := MarusiaMigrationCommands("/tmp/cli", "/data/capsdemo", s)
	if len(cmds) != 2 {
		t.Fatalf("migration commands = %d", len(cmds))
	}
	for i, key := range []string{"session_id", "session_secret"} {
		joined := strings.Join(cmds[i].Args, " ")
		if strings.Contains(joined, "vendor-session-value") || strings.Contains(joined, "vendor-secret-value") {
			t.Fatalf("secret value appeared in argv: %v", cmds[i].Args)
		}
		if !strings.Contains(joined, "settings app org.capsulad.marusia "+key+"@-") {
			t.Fatalf("%s stdin args = %v", key, cmds[i].Args)
		}
	}
	if string(cmds[0].Stdin) != "vendor-session-value\n" || string(cmds[1].Stdin) != "vendor-secret-value\n" {
		t.Fatalf("stdin values = %q, %q", cmds[0].Stdin, cmds[1].Stdin)
	}
	secretArgs := strings.Join(SettingsStdinArgs("/tmp/cli", "/data/capsdemo", "org.capsulad.marusia", "vk_access_token"), " ")
	if strings.Contains(secretArgs, "token-value") || !strings.Contains(secretArgs, "vk_access_token@-") {
		t.Fatalf("token args = %q", secretArgs)
	}
	if err := ValidateSecretSetting("two\nlines"); err == nil {
		t.Fatal("expected a newline to be refused")
	}
}

func makeBundle(t *testing.T, path string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBundleCLICheckAndExtract(t *testing.T) {
	dir := t.TempDir()
	with := filepath.Join(dir, "with.tar.gz")
	makeBundle(t, with, map[string]string{"capsulad": "daemon", "capsulad-cli": "cli-bytes"})
	has, err := BundleHasCLI(with)
	if err != nil || !has {
		t.Fatalf("BundleHasCLI = %v, %v", has, err)
	}
	out, err := ExtractBundleCLI(with, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "cli-bytes" {
		t.Fatalf("extracted %q, %v", data, err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("extracted CLI is not executable: %v", err)
	}

	without := filepath.Join(dir, "without.tar.gz")
	makeBundle(t, without, map[string]string{"capsulad": "daemon"})
	if has, err := BundleHasCLI(without); err != nil || has {
		t.Fatalf("BundleHasCLI without CLI = %v, %v", has, err)
	}
	if _, err := ExtractBundleCLI(without, filepath.Join(dir, "out2")); err == nil {
		t.Fatal("expected an extraction failure")
	}
}

func TestAuthStatusParsing(t *testing.T) {
	for out, want := range map[string]AuthState{
		"admin password set: true":  {Configured: true, Known: true},
		"admin password set: false": {Configured: false, Known: true},
		"Password Set: False":       {Configured: false, Known: true},
		"admin password: set":       {Configured: true, Known: true},
		"not configured":            {Configured: false, Known: true},
		"":                          {Configured: false, Known: false},
	} {
		got := ParseAuthStatus(out)
		if got.Configured != want.Configured || got.Known != want.Known {
			t.Fatalf("ParseAuthStatus(%q) = %+v, want %+v", out, got, want)
		}
	}
}

func TestSelectedAppBytes(t *testing.T) {
	refs := []release.AppRef{{ID: "a", Size: 10}, {ID: "b", Size: 20}}
	if got := SelectedAppBytes(refs, nil); got != 30 {
		t.Fatalf("nil selection = %d", got)
	}
	if got := SelectedAppBytes(refs, []string{}); got != 0 {
		t.Fatalf("empty selection = %d", got)
	}
	if got := SelectedAppBytes(refs, []string{"b"}); got != 20 {
		t.Fatalf("subset = %d", got)
	}
	if got := SelectedAppBytes(refs, []string{"a", "b"}); got != 30 {
		t.Fatalf("all selected = %d", got)
	}
	if got := SelectedAppBytes(refs, []string{"missing"}); got != 0 {
		t.Fatalf("unknown = %d", got)
	}
}

func TestPinnedTLSAndWaitHealth(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HealthPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	cfg, err := PinnedTLSConfig(certPEM, "127.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("pinned TLS must never skip verification")
	}
	if cfg.RootCAs == nil || cfg.ServerName == "" {
		t.Fatalf("incomplete TLS config: %+v", cfg)
	}
	if _, err := PinnedTLSConfig([]byte("not pem"), "127.0.0.1"); err == nil {
		t.Fatal("expected a certificate parse error")
	}
	fp, err := CertFingerprint(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(fp, ":") != 31 || len(fp) != 95 {
		t.Fatalf("fingerprint = %q", fp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := WaitHealth(ctx, certPEM, srv.URL, 20*time.Millisecond, t.Logf); err != nil {
		t.Fatalf("WaitHealth: %v", err)
	}

	// A certificate from another server must not be accepted.
	otherCert := selfSignedCert(t)
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{otherCert}}
	other.StartTLS()
	defer other.Close()
	otherPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherCert.Certificate[0]})
	short, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if err := WaitHealth(short, otherPEM, srv.URL, 20*time.Millisecond, nil); err == nil {
		t.Fatal("expected the pinned certificate to reject a different server")
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: "not-the-device"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}
}

func TestParseByteCount(t *testing.T) {
	for in, want := range map[string]int64{
		"29163780 /data/capsdemo/capsulad\n": 29163780,
		" 123\n":                             123,
		"0 /x":                               0,
	} {
		got, ok := parseByteCount([]byte(in))
		if !ok || got != want {
			t.Fatalf("parseByteCount(%q) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "not-a-number /x", "-1 /x"} {
		if _, ok := parseByteCount([]byte(in)); ok {
			t.Fatalf("parseByteCount(%q) should fail", in)
		}
	}
}

func TestStockSpaceMathAndAppStates(t *testing.T) {
	// A larger staged daemon needs its growth on top of the 8 MiB reserve.
	check := CheckStockSpace(6<<20, 42<<20, 40<<20)
	if check.OK || check.RequiredBytes != (8<<20)+(2<<20) {
		t.Fatalf("check = %+v", check)
	}
	// A smaller staged daemon adds nothing beyond the reserve.
	check = CheckStockSpace(8<<20, 30<<20, 40<<20)
	if !check.OK || check.RequiredBytes != StockDeployReserveBytes {
		t.Fatalf("check = %+v", check)
	}
	// The floor is exactly OK at equality.
	check = CheckStockSpace(StockDeployReserveBytes, 10, 10)
	if !check.OK {
		t.Fatalf("equality should pass: %+v", check)
	}

	runs := "org.capsulad.simon run-1 READY pid=42 chime\n"
	if !RunsShowActive(runs, "org.capsulad.simon") {
		t.Fatal("READY should count as running")
	}
	if !RunsShowActive("capsulad.clock builtin:capsulad.clock APP_READY pid=0 sandbox=builtin\n", "capsulad.clock") {
		t.Fatal("APP_READY should count as running")
	}
	if RunsShowActive("org.capsulad.simon run-1 STOPPED pid=0 stopped\n", "org.capsulad.simon") {
		t.Fatal("STOPPED must not count as running")
	}
	if RunsShowActive("org.capsulad.simon run-1 APP_STOPPED pid=0 stopped\n", "org.capsulad.simon") {
		t.Fatal("APP_STOPPED must not count as running")
	}
	if RunsShowActive(runs, "org.capsulad.pa") {
		t.Fatal("another app's run must not count")
	}
}

func TestBundleEntrySize(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "b.tar.gz")
	makeBundle(t, bundle, map[string]string{"capsulad": "12345", "capsulad-cli": "cli"})
	size, err := BundleEntrySize(bundle, "capsulad")
	if err != nil {
		t.Fatal(err)
	}
	if size != 5 {
		t.Fatalf("size = %d", size)
	}
	if _, err := BundleEntrySize(bundle, "missing"); err == nil {
		t.Fatal("expected a missing entry error")
	}
}

func TestHTTPSBase(t *testing.T) {
	if got := HTTPSBase("192.168.1.5:5555", 0); got != "https://192.168.1.5:8443" {
		t.Fatalf("base = %q", got)
	}
	if got := HTTPSBase("capsula.local", 9443); got != "https://capsula.local:9443" {
		t.Fatalf("base = %q", got)
	}
}

func TestValidateSSID(t *testing.T) {
	for _, ok := range []string{"Home", "My Wi-Fi", "Café", "a.b"} {
		if err := ValidateSSID(ok); err != nil {
			t.Fatalf("ValidateSSID(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "-evil", "bad\x1b]52;c;x", "bad\x07"} {
		if err := ValidateSSID(bad); err == nil {
			t.Fatalf("ValidateSSID(%q) should fail", bad)
		}
	}
	nets := DedupeNetworks([]Network{
		{SSID: "-evil", Security: "wpa2"},
		{SSID: "bad\x1b", Security: "wpa2"},
		{SSID: "Home", Security: "wpa2"},
	})
	if len(nets) != 1 || nets[0].SSID != "Home" {
		t.Fatalf("dedupe = %+v", nets)
	}
}

func TestSpaceMathSaturates(t *testing.T) {
	const max = int64(^uint64(0) >> 1)
	check := CheckSpace(max-1, max-1, 10, 10)
	if check.RequiredBytes != max || check.OK {
		t.Fatalf("saturating check = %+v", check)
	}
	refs := []release.AppRef{{ID: "a", Size: max - 1}, {ID: "b", Size: 10}}
	if got := SelectedAppBytes(refs, []string{"a", "b"}); got != max {
		t.Fatalf("saturating app sum = %d", got)
	}
}

func TestWaitHealthRefusesRedirects(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := WaitHealth(ctx, certPEM, srv.URL, 20*time.Millisecond, nil); err == nil {
		t.Fatal("expected a redirect to be refused")
	}
}

func TestEnsureStagingArgs(t *testing.T) {
	args := EnsureStagingArgs("/tmp/capsulad-installer")
	script := strings.Join(args, " ")
	for _, want := range []string{
		"fail() { echo \"capsulad-installer: $*\" >&2; exit 1; }",
		"current_uid() {",
		"id -u 2>/dev/null && return 0",
		"[ -r /proc/self/status ] || return 1",
		"if [ \"$key\" = \"Uid:\" ]",
		"[ -O / ] || fail \"the device shell must be root (cannot read the uid and / is not owned by this user)\"",
		"elif [ \"$u\" != \"0\" ]; then",
		"fail \"the device shell must be root (uid $u)\"",
		"d=/tmp/capsulad-installer",
		"[ -L \"$d\" ]",
		"mkdir -p \"$d\" || fail \"cannot create $d\"",
		"chmod 0700 \"$d\" || fail \"cannot chmod 0700 $d\"",
		"[ -O \"$d\" ] || fail",
		"a=\"$d/apps\"",
		"mkdir -p \"$a\" || fail \"cannot create $a\"",
		"chmod 0700 \"$a\" || fail \"cannot chmod 0700 $a\"",
		"[ -O \"$a\" ] || fail",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("staging script missing %q:\n%s", want, script)
		}
	}
	// Parse every branch, including the root-only paths this host may not run.
	if out, err := exec.Command(args[0], "-n", "-c", args[2]).CombinedOutput(); err != nil {
		t.Fatalf("staging script does not parse: %v\n%s", err, out)
	}
}

// fakeID puts a stub `id` first on PATH so the staging script's root check is
// deterministic whether or not the test host runs as root.
func fakeID(t *testing.T, uid string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho "+uid+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

func runStaging(t *testing.T, pathEnv, dir string) (string, error) {
	t.Helper()
	args := EnsureStagingArgs(dir)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+pathEnv)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestEnsureStagingScriptAlwaysExplainsAFailure covers the reported bug: the
// step exited 1 with an empty stderr and no hint about which check failed.
func TestEnsureStagingScriptAlwaysExplainsAFailure(t *testing.T) {
	out, err := runStaging(t, fakeID(t, "501"), filepath.Join(t.TempDir(), "capsulad-installer"))
	if err == nil {
		t.Fatalf("non-root staging unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(out, "capsulad-installer: the device shell must be root (uid 501)") {
		t.Fatalf("staging failure did not explain itself: %q", out)
	}
}

// toolOnlyPath returns a PATH with only the named host tools, so a missing
// `id` can be simulated the way a trimmed stock BusyBox presents it.
func toolOnlyPath(t *testing.T, tools ...string) string {
	t.Helper()
	bin := t.TempDir()
	for _, name := range tools {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// TestEnsureStagingScriptWithoutID reproduces the reported stock image: the
// shell is root but BusyBox ships no id/whoami. The uid then comes from
// /proc/self/status (Linux) or the root-directory owner probe.
func TestEnsureStagingScriptWithoutID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capsulad-installer")
	out, err := runStaging(t, toolOnlyPath(t, "mkdir", "chmod", "ls"), dir)

	if os.Geteuid() != 0 {
		if err == nil {
			t.Fatalf("non-root staging without id unexpectedly succeeded: %s", out)
		}
		if !strings.Contains(out, "the device shell must be root") {
			t.Fatalf("failure did not name the root requirement: %q", out)
		}
		// On Linux the uid must come from /proc/self/status.
		if _, statErr := os.Stat("/proc/self/status"); statErr == nil {
			want := fmt.Sprintf("must be root (uid %d)", os.Geteuid())
			if !strings.Contains(out, want) {
				t.Fatalf("output = %q, want %q", out, want)
			}
		}
		return
	}

	if err != nil {
		t.Fatalf("root staging without id failed: %v\n%s", err, out)
	}
	for _, p := range []string{dir, filepath.Join(dir, "apps")} {
		info, statErr := os.Stat(p)
		if statErr != nil {
			t.Fatalf("staging did not create %s: %v", p, statErr)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 700", p, got)
		}
	}
}

func TestEnsureStagingScriptCreatesStagingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "capsulad-installer")
	out, err := runStaging(t, fakeID(t, "0"), dir)
	if err != nil {
		t.Fatalf("root staging failed: %v\n%s", err, out)
	}
	for _, p := range []string{dir, filepath.Join(dir, "apps")} {
		info, statErr := os.Stat(p)
		if statErr != nil {
			t.Fatalf("staging did not create %s: %v", p, statErr)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 700", p, got)
		}
	}
}

// errorDevice returns one canned result for every command.
type errorDevice struct{ res device.Result }

func (d errorDevice) Connect(context.Context) error { return nil }
func (d errorDevice) Run(context.Context, device.Command) (device.Result, error) {
	return d.res, nil
}
func (d errorDevice) Push(context.Context, string, string) error { return nil }
func (d errorDevice) Addr() string                               { return "192.168.1.20:5555" }
func (d errorDevice) Kind() string                               { return "adb" }
func (d errorDevice) Close() error                               { return nil }

func TestRunReportsMergedStdoutOnFailure(t *testing.T) {
	i := &Installer{Dev: errorDevice{res: device.Result{
		ExitCode: 1,
		Stdout:   []byte("capsulad-installer: /tmp/capsulad-installer is a symlink\n"),
	}}}
	_, err := i.run(context.Background(), device.Command{Args: []string{"sh", "-c", "check"}})
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("err = %v, want the device-side reason", err)
	}
}

func TestMarusiaSessionRejectsMultilineSecrets(t *testing.T) {
	if _, ok := ParseMarusiaSession([]byte(`{"session":"a\nb","secret":"c"}`)); ok {
		t.Fatal("a multi-line session must not migrate through the one-line stdin form")
	}
}
