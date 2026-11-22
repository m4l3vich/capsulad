package device

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestADBArgConstruction(t *testing.T) {
	if got := strings.Join(ConnectArgs("192.168.1.9:5555"), " "); got != "connect 192.168.1.9:5555" {
		t.Fatalf("connect args = %q", got)
	}
	addr := NewADB("", "192.168.1.9").Addr()
	if addr != "192.168.1.9:5555" {
		t.Fatalf("default port = %q", addr)
	}
	shell := ShellArgs("192.168.1.9:5555", []string{"sh", "-c", "echo hello world"})
	if len(shell) != 4 || shell[0] != "-s" || shell[2] != "shell" {
		t.Fatalf("shell args = %#v", shell)
	}
	if strings.Contains(shell[3], "echo hello") && !strings.Contains(shell[3], "'echo hello world'") {
		t.Fatalf("remote argv was not quoted as one word: %q", shell[3])
	}
	push := PushArgs("dev:5555", "/tmp/a b", "/tmp/c")
	if strings.Join(push, "|") != "-s|dev:5555|push|--|/tmp/a b|/tmp/c" {
		t.Fatalf("push args = %#v", push)
	}
	if got := ShellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("quote = %q", got)
	}
}

func TestADBStreamTransport(t *testing.T) {
	a := NewADB("", "10.0.0.9")
	captured := a.commandArgs(context.Background(), Command{Args: []string{"cat", "/etc/os-release"}})
	if len(captured) != 4 || captured[2] != "shell" {
		t.Fatalf("captured args = %#v", captured)
	}
	raw := a.streamArgs(false, []string{"sh", "-c", "tar -cf - | gzip -c"})
	if len(raw) != 4 || raw[2] != "shell" {
		t.Fatalf("raw fallback args = %#v", raw)
	}
	if !strings.Contains(raw[3], "stty raw -echo") || !strings.Contains(raw[3], "2>/dev/null") {
		t.Fatalf("raw fallback does not disable pty translation: %q", raw[3])
	}
	if !strings.Contains(raw[3], "gzip -c") {
		t.Fatalf("raw fallback lost the command: %q", raw[3])
	}
	execOut := a.streamArgs(true, []string{"sh", "-c", "tar -cf - | gzip -c"})
	if len(execOut) != 4 || execOut[2] != "exec-out" {
		t.Fatalf("exec-out args = %#v", execOut)
	}
	if got := ExecOutArgs("dev:5555", []string{"true"}); got[2] != "exec-out" {
		t.Fatalf("ExecOutArgs = %#v", got)
	}
	if got := ShellRawArgs("dev:5555", []string{"true"}); got[2] != "shell" {
		t.Fatalf("ShellRawArgs = %#v", got)
	}
}

func TestADBStateFailure(t *testing.T) {
	if err := classifyADBStateFailure("device unauthorized\n", errors.New("exit 1")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized = %v", err)
	}
	err := classifyADBStateFailure("device offline\n", errors.New("exit 1"))
	if !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("offline = %v", err)
	}
	if err := classifyADBStateFailure("something else", errors.New("exit 1")); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("other = %v", err)
	}
}

func TestADBStates(t *testing.T) {
	for out, want := range map[string]ADBState{
		"connected to 10.0.0.2:5555":    ADBConnected,
		"already connected to 10.0.0.2": ADBAlready,
		"failed to connect to 'x'":      ADBFailed,
		"device unauthorized":           ADBUnauthorized,
		"device offline":                ADBOffline,
		"something else":                ADBUnknown,
	} {
		if got := ParseConnectOutput(out); got != want {
			t.Fatalf("ParseConnectOutput(%q) = %s, want %s", out, got, want)
		}
	}
	for out, want := range map[string]ADBState{
		"device\n":       ADBConnected,
		"unauthorized\n": ADBUnauthorized,
		"offline":        ADBOffline,
	} {
		if got := ParseStateOutput(out); got != want {
			t.Fatalf("ParseStateOutput(%q) = %s, want %s", out, got, want)
		}
	}
}

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func TestKnownHostsTOFUAndMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts.json")
	k, err := LoadKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	key := testPublicKey(t)
	status, err := k.Verify("10.0.0.5:22", key)
	if err != nil || status != HostKeyNew {
		t.Fatalf("first verify = %s, %v", status, err)
	}
	if err := k.Accept("10.0.0.5:22", key); err != nil {
		t.Fatal(err)
	}
	// Reload: the pin must survive a new process.
	k2, err := LoadKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := k2.Verify("10.0.0.5:22", key); err != nil || status != HostKeyKnown {
		t.Fatalf("reloaded verify = %s, %v", status, err)
	}
	if fp := FingerprintSHA256(key); !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("fingerprint = %q", fp)
	}
	other := testPublicKey(t)
	status, err = k2.Verify("10.0.0.5:22", other)
	if status != HostKeyMismatch || err == nil {
		t.Fatalf("mismatch = %s, %v", status, err)
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("mismatch error must explain the refusal: %v", err)
	}
	// A different host is new, not a mismatch.
	if status, err := k2.Verify("10.0.0.6:22", other); err != nil || status != HostKeyNew {
		t.Fatalf("other host = %s, %v", status, err)
	}
	// The store is private.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("known_hosts mode = %o", info.Mode().Perm())
	}
}

func TestMapPlatform(t *testing.T) {
	cases := []struct {
		name, osr, compat string
		want              Platform
		fail              bool
	}{
		{"rockchip stock", `ID=buildroot\nNAME="Buildroot"`, "rockchip,rk3308-capsula-mini\x00", PlatformRockchipStock, false},
		{"amlogic buildroot", "ID=buildroot", "amlogic,a113x\x00", PlatformAmlogicStock, false},
		{"amlogic alpine", "NAME=\"Alpine Linux\"\nID=alpine", "amlogic,meson-axg\x00", PlatformAmlogicAlpine, false},
		{"rockchip alpine", "ID=alpine", "rockchip,rk3308", "", true},
		{"unknown", "ID=buildroot", "vendor,otherboard", "", true},
		{"empty", "ID=buildroot", "", "", true},
	}
	for _, c := range cases {
		got, err := MapPlatform(c.osr, c.compat)
		if c.fail {
			if err == nil {
				t.Fatalf("%s: expected an error, got %s", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFirmwareMismatch(t *testing.T) {
	if err := CheckFirmware(FirmwareStock, PlatformRockchipStock); err != nil {
		t.Fatal(err)
	}
	if err := CheckFirmware(FirmwareStock, PlatformAmlogicStock); err != nil {
		t.Fatal(err)
	}
	if err := CheckFirmware(FirmwareAlpine, PlatformAmlogicAlpine); err != nil {
		t.Fatal(err)
	}
	if err := CheckFirmware(FirmwareAlpine, PlatformRockchipStock); err == nil {
		t.Fatal("expected a mismatch")
	}
	if err := CheckFirmware(FirmwareStock, PlatformAmlogicAlpine); err == nil {
		t.Fatal("expected a mismatch")
	}
}

func TestBackupPathAndCommand(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 34, 56, 0, time.UTC)
	if got := BackupFilename("192.168.1.20:5555", now); got != "192.168.1.20_5555-20261122T123456Z.tar.gz" {
		t.Fatalf("backup name = %q", got)
	}
	if got := BackupFilename("host/with spaces", now); strings.ContainsAny(got, "/ ") {
		t.Fatalf("unsafe backup name %q", got)
	}
	cmd := BackupCommand(PlatformRockchipStock.DataDir())
	script := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"tar -cf -",
		BackupSentinel,
		"--exclude=data/lost+found",
		"--exclude=data/capsdemo",
		"etc/init.d/S99btctl.sh",
		"etc/wpa_supplicant.conf",
		"set -e",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("backup script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "tar -czf") || strings.Contains(script, "| gzip") {
		t.Fatalf("the device streams raw tar; the host gzips it: %s", script)
	}
	if strings.Contains(script, "exec ") {
		t.Fatalf("the sentinel must follow tar, so tar must not replace the shell: %s", script)
	}
	if strings.Contains(script, "-password") || strings.Contains(script, "rm ") {
		t.Fatalf("backup script must not carry secrets or delete: %s", script)
	}

	// A reinstall on Amlogic stock excludes the active data directory too.
	amlogic := strings.Join(BackupCommand(PlatformAmlogicStock.DataDir()).Args, " ")
	if !strings.Contains(amlogic, "--exclude=data/local/capsulad") || !strings.Contains(amlogic, "--exclude=data/capsdemo") {
		t.Fatalf("Amlogic exclusions = %s", amlogic)
	}
}

func makeRawTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	for _, name := range names {
		body := entries[name]
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(makeRawTar(t, entries)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestValidateBackup(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.tar.gz")
	if err := os.WriteFile(good, makeTarGz(t, map[string]string{
		"data/a":                 "a",
		"etc/init.d/S99btctl.sh": "hook",
		// BusyBox can leave an escaped space in a real vendor filename.
		`data/overlay/upper/var/db/dhcpcd/wlan0-ctOS\040AP.lease`: "lease",
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := ValidateBackup(good)
	if err != nil {
		t.Fatalf("ValidateBackup: %v", err)
	}
	if summary.Entries != 3 || summary.Files != 3 || summary.Bytes == 0 || len(summary.SHA256) != 64 {
		t.Fatalf("summary = %+v", summary)
	}

	truncated := filepath.Join(dir, "trunc.tar.gz")
	data, _ := os.ReadFile(good)
	if err := os.WriteFile(truncated, data[:len(data)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBackup(truncated); err == nil {
		t.Fatal("expected a truncated archive to fail")
	}

	escape := filepath.Join(dir, "escape.tar.gz")
	if err := os.WriteFile(escape, makeTarGz(t, map[string]string{"../etc/shadow": "x"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBackup(escape); err == nil {
		t.Fatal("expected an escaping entry to fail")
	}

	if err := os.WriteFile(truncated, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBackup(truncated); err == nil {
		t.Fatal("expected non-gzip to fail")
	}
}

type streamDevice struct {
	data     []byte
	sentinel bool
	merged   bool
	exit     int
}

func (d *streamDevice) Connect(context.Context) error { return nil }
func (d *streamDevice) Run(ctx context.Context, cmd Command) (Result, error) {
	if cmd.Stdout != nil && len(d.data) > 0 {
		if _, err := cmd.Stdout.Write(d.data); err != nil {
			return Result{}, err
		}
	}
	if d.merged && cmd.Stdout != nil {
		if _, err := cmd.Stdout.Write([]byte(BackupSentinel + "\n")); err != nil {
			return Result{}, err
		}
	}
	if d.sentinel && cmd.Stderr != nil {
		if _, err := cmd.Stderr.Write([]byte(BackupSentinel + "\n")); err != nil {
			return Result{}, err
		}
	}
	return Result{ExitCode: d.exit}, nil
}
func (d *streamDevice) Push(context.Context, string, string) error { return nil }
func (d *streamDevice) Addr() string                               { return "dev" }
func (d *streamDevice) Kind() string                               { return "fake" }
func (d *streamDevice) Close() error                               { return nil }

func TestCreateBackupSentinel(t *testing.T) {
	raw := makeRawTar(t, map[string]string{
		"data/a":                 "a",
		"etc/init.d/S99btctl.sh": "hook",
	})
	cases := []struct {
		name string
		dev  *streamDevice
		err  string
	}{
		{"stderr sentinel", &streamDevice{data: raw, sentinel: true}, ""},
		{"merged pty sentinel", &streamDevice{data: raw, merged: true}, ""},
		{"missing sentinel", &streamDevice{data: raw}, "success marker"},
		{"truncated stream", &streamDevice{data: raw[:len(raw)/2]}, "success marker"},
		{"nonzero exit", &streamDevice{data: raw, sentinel: true, exit: 1}, "stream backup failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			res, err := CreateBackup(context.Background(), tc.dev, dir, "dev", PlatformRockchipStock, time.Now())
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateBackup: %v", err)
			}
			if res.Entries != 2 || res.Bytes == 0 {
				t.Fatalf("result = %+v", res)
			}
			if _, err := os.Stat(res.ManifestPath); err != nil {
				t.Fatalf("manifest: %v", err)
			}
			if summary, err := ValidateBackup(res.Path); err != nil || summary.Entries != 2 {
				t.Fatalf("stored backup = %+v, %v", summary, err)
			}
		})
	}
}

func TestDeployScriptAndMarker(t *testing.T) {
	p := NewDeployPlan("/data/capsdemo", PlatformRockchipStock, "deploy-bundle.tar.gz", false)
	script := DeployScript(p)
	for _, want := range []string{
		"umask 022",
		"/tmp/capsulad-installer/capsulad-cli -data-dir /data/capsdemo deploy /tmp/capsulad-installer/deploy-bundle.tar.gz -platform rockchip-stock -staging /run -timeout 180s > /tmp/capsulad-installer/deploy.log 2>&1",
		"echo exit=$? >> /tmp/capsulad-installer/deploy.log",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	launch := DetachedLaunch(p.ScriptPath)
	if strings.Join(launch[0], " ") != "start-stop-daemon -S -b -x /tmp/capsulad-installer/deploy.sh" {
		t.Fatalf("primary launch = %v", launch[0])
	}
	if strings.Join(launch[1], " ") != "setsid sh /tmp/capsulad-installer/deploy.sh" {
		t.Fatalf("fallback launch = %v", launch[1])
	}

	off := StartupMarker(p)
	want := "sh -c mkdir -p /data/capsdemo && touch /data/capsdemo/startup-sound.played"
	if strings.Join(off.Args, " ") != want {
		t.Fatalf("jingle-off marker = %v", off.Args)
	}
	if !strings.Contains(strings.Join(off.Args, " "), "mkdir -p /data/capsdemo") {
		t.Fatalf("marker must create the data directory first: %v", off.Args)
	}
	p.Jingle = true
	on := StartupMarker(p)
	if strings.Join(on.Args, " ") != "rm -f /data/capsdemo/startup-sound.played" {
		t.Fatalf("jingle-on marker = %v", on.Args)
	}

	log := "step 1\nstep 2\nexit=0\n"
	if code, ok := ParseDeployExit(log); !ok || code != 0 {
		t.Fatalf("exit parse = %d, %v", code, ok)
	}
	if code, ok := ParseDeployExit("still running\n"); ok {
		t.Fatalf("unexpected exit parse %d", code)
	}
	if got := LogTail("a\nb\nc", 2); got != "b\nc" {
		t.Fatalf("tail = %q", got)
	}
}

func TestPlatformContract(t *testing.T) {
	for p, dir := range map[Platform]string{
		PlatformRockchipStock: "/data/capsdemo",
		PlatformAmlogicStock:  "/data/local/capsulad",
		PlatformAmlogicAlpine: "/var/lib/capsulad",
	} {
		if got := p.DataDir(); got != dir {
			t.Fatalf("%s data dir = %q", p, got)
		}
	}
	if !PlatformRockchipStock.Stock() || PlatformAmlogicAlpine.Stock() {
		t.Fatal("Stock() is wrong")
	}
	if PlatformRockchipStock.Rollback() != "/etc/init.d/S99btctl.sh disable" {
		t.Fatalf("rockchip rollback = %q", PlatformRockchipStock.Rollback())
	}
	if PlatformAmlogicAlpine.SessionFile() != "" {
		t.Fatal("Alpine has no vendor session file")
	}
}

func TestLimitWriter(t *testing.T) {
	var out bytes.Buffer
	l := &limitWriter{w: &out, remaining: 4}
	if _, err := l.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("5")); err == nil || !l.overflow {
		t.Fatalf("overflow = %v, %v", err, l.overflow)
	}
	if out.String() != "1234" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{limit: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("5")); err == nil || !b.overflow {
		t.Fatalf("overflow = %v, %v", err, b.overflow)
	}
}

func TestIdentityFromDevinfo(t *testing.T) {
	raw := "BLOB_CHECK=FAIL\nBLOB_SN=\nDEVICE_ID=abc123\nDEVICE_ID_FULL=:x:y:abc123\nSERIAL=03104B1D40E15C55\nMODEL=Capsula-mini\n"
	if got := identityFromSource("devinfo", raw); got != "03104B1D40E15C55" {
		t.Fatalf("devinfo serial = %q", got)
	}
	if got := identityFromSource("devinfo", "DEVICE_ID=abc123\n"); got != "abc123" {
		t.Fatalf("devinfo device id = %q", got)
	}
	if got := identityFromSource("device-tree", " ABC\x00\n"); got != "ABC" {
		t.Fatalf("device-tree = %q", got)
	}
}

func TestADBCheckedExitMarker(t *testing.T) {
	script := deviceShellChecked([]string{"sh", "-c", "exit 7"})
	if !strings.Contains(script, adbExitMarker) {
		t.Fatalf("script = %q", script)
	}
	out := []byte("hello" + "\n" + adbExitMarker + "7\n")
	body, code, ok := splitExitMarker(out)
	if !ok || code != 7 || string(body) != "hello" {
		t.Fatalf("split = %q, %d, %v", body, code, ok)
	}
	// A pty translates LF to CRLF; the code must still parse.
	out = []byte("hello\r\n" + adbExitMarker + "0\r\n")
	body, code, ok = splitExitMarker(out)
	if !ok || code != 0 || string(body) != "hello\r" {
		t.Fatalf("crlf split = %q, %d, %v", body, code, ok)
	}
}
