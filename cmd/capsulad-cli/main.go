// capsulad-cli is the local administration CLI: certificate bootstrap, package
// installation, deployment, supervised app control, diagnostics and firmware
// inspection.
//
// Administrative certificate operations work on the local certificate store
// (physical console or authenticated local maintenance session). Everything
// else uses the framework Unix socket exactly like any other local client.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/artifact"
	"capsulad/internal/backend/rockchip/inspect"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/sandbox"
	"capsulad/internal/version"
	"capsulad/internal/websecurity"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	dataDir := ""
	socket := ""
	securityDir := ""
	nativeLibDir := ""
	jsonOut := false
	// Global flags may appear before the command.
	args := os.Args[1:]
	for len(args) > 0 {
		switch args[0] {
		case "-data-dir":
			dataDir = args[1]
			args = args[2:]
		case "-socket":
			socket = args[1]
			args = args[2:]
		case "-security-dir":
			securityDir = args[1]
			args = args[2:]
		case "-native-lib-dir":
			nativeLibDir = args[1]
			args = args[2:]
		case "-json":
			jsonOut = true
			args = args[1:]
		case "-h", "--help", "help":
			usage()
		default:
			goto command
		}
	}
command:
	if len(args) == 0 {
		usage()
	}
	cmd := args[0]
	args = args[1:]
	switch cmd {
	case "version":
		fmt.Printf("capsulad-cli %s (sdk %s)\n", version.Version, version.SDKVersion)
	case "status", "doctor":
		exit(runStatus(resolveSocket(dataDir, socket), jsonOut))
	case "app":
		exit(runApp(args, dataDir, socket, nativeLibDir))
	case "settings":
		exit(runSettings(args, resolveSocket(dataDir, socket), jsonOut))
	case "logs":
		exit(runLogs(args, resolveSocket(dataDir, socket)))
	case "media":
		exit(runMedia(args, resolveSocket(dataDir, socket)))
	case "cert":
		exit(runCert(args, resolveDataDir(dataDir), securityDir))
	case "auth":
		exit(runAuth(args, resolveDataDir(dataDir), securityDir))
	case "platform":
		exit(runPlatform(args, jsonOut))
	case "deploy":
		exit(runDeploy(args, dataDir, socket))
	case "dev":
		exit(runDev(args, dataDir))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `capsulad-cli — capsulad administration CLI

Global flags (before the command): -data-dir DIR, -socket PATH, -security-dir DIR, -native-lib-dir DIR, -json

Commands:
  status                         device, resources and runs
  doctor                         same as status with backend diagnostics
  app list                       installed applications
  app install PATH               install or atomically replace a package (dir or .tar.gz);
                                 activated immediately when capsulad is running
  app package DIR [-out FILE]    write the canonical .tar.gz artifact
  app remove ID                  uninstall an application (refused while it runs)
  app start ID [NAME=VALUE ...]  launch or deliver an invocation
  app stop RUN_ID                request graceful stop
  app runs [ID]                  list supervised runs
  app provenance ID              computed native-binary provenance status
  deploy PATH                    apply a deployment bundle (dir or .tar.gz)
  settings show                  device settings
  settings app APPID [k=v ...]   show or update one application's settings
  logs [-app ID] [-n N]          recent structured logs
  media list                     players published via the media contract
  cert init                      create the self-signed HTTPS server certificate
  cert renew                     renew the self-signed server certificate
  cert status                    show certificate fingerprint, names and expiry
  auth init                      initialize admin password, certificate and first API key
  auth set-password              change the admin password
  auth key create NAME           create a static API key (printed once)
                                 [--scope status|operator] (repeatable; default status)
  auth key list                  list API keys, revocation state and scopes
  auth key revoke NAME           revoke an API key
  auth status                    show password/API-key/certificate status
  platform inspect -root DIR     inspect an extracted firmware tree offline (detects
                                 rockchip-stock, amlogic-stock or amlogic-alpine)
  platform ledtest [flags]       drive the display channels directly (bring-up)
  dev <wakeword|gesture|key|raw|state>  simulator injection (development)

Deploy flags: -platform rockchip-stock|amlogic-stock|amlogic-alpine, -init-script PATH,
-restart auto|always|never, -dry-run, -staging DIR, -timeout D. The platform selects the
boot service, the hook name and the space floors; the bundle layout is identical.
The bundle is staged under the temporary directory (or -staging), never inside
the data directory and on the data filesystem only with room for the largest staged tree;
the data partition is checked for the space the binary writes cannot free themselves
before the daemon is stopped.
A deployment bundle contains capsulad, capsulad-cli, lib/, apps/ and boot/%s;
unknown top-level entries are reported and ignored. A restarting deployment stops the
daemon first, installs with nothing running, then restarts it. Binaries are installed as
<data-dir>/capsulad and <data-dir>/capsulad-cli (0755), lib/ as <data-dir>/lib and
apps/ under <data-dir>/apps; the boot hook is installed only when the bundle
carries it and the daemon is restarted.
Certificate flags: -hostname H, -ip IP (repeatable), -days N. Security store default: <data-dir>/security.
The Web UI uses self-signed TLS; browsers will warn until the fingerprint is trusted.
`, bootHookName)
	os.Exit(2)
}

func exit(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "capsulad-cli:", err)
		os.Exit(1)
	}
}

func resolveDataDir(dataDir string) string {
	if dataDir != "" {
		return dataDir
	}
	if v := os.Getenv("CAPSULAD_DATA_DIR"); v != "" {
		return v
	}
	return ".capsulad"
}

// resolveNativeLibDir locates the shared native SDK for install-time checks.
// Precedence: -native-lib-dir, CAPSULAD_NATIVE_LIB_DIR, <data-dir>/lib beside
// the installed daemon, then the executable-adjacent launcher default.
func resolveNativeLibDir(dataDir, explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("CAPSULAD_NATIVE_LIB_DIR")
	}
	if explicit != "" {
		return sandbox.ResolveLibraryDir(explicit)
	}
	if abs, err := filepath.Abs(filepath.Join(resolveDataDir(dataDir), "lib")); err == nil {
		if info, err := os.Stat(abs); err == nil && info.IsDir() && sandbox.HasRuntime(abs) {
			return sandbox.ResolveLibraryDir(abs)
		}
	}
	return sandbox.DefaultLibraryDir(), nil
}

// checkNativeSDKRequirement rejects an install whose declared native SDK
// requirement the installed runtime cannot satisfy. Apps without a
// [native_sdk] section are unaffected. This is a preflight check against the
// runtime's generated metadata; the binding still verifies the library it
// loads.
func checkNativeSDKRequirement(m *manifest.Manifest, dataDir, nativeLibDir string) error {
	dir, err := resolveNativeLibDir(dataDir, nativeLibDir)
	if m.NativeSDK == nil {
		// No requirement, but an explicitly configured directory must still be
		// usable; a typo should not be silently ignored.
		return err
	}
	if err != nil {
		return fmt.Errorf("install rejected: app %s requires native SDK ABI %d.%d: %w", m.ID, m.NativeSDK.ABIMajor, m.NativeSDK.MinMinor, err)
	}
	rt, err := sandbox.LocateRuntime(dir)
	if err != nil {
		return fmt.Errorf("install rejected: app %s requires native SDK ABI %d.%d: %w", m.ID, m.NativeSDK.ABIMajor, m.NativeSDK.MinMinor, err)
	}
	if err := sandbox.CheckRequirement(rt, *m.NativeSDK); err != nil {
		return fmt.Errorf("install rejected: app %s: %w", m.ID, err)
	}
	return nil
}

func resolveSocket(dataDir, socket string) string {
	if socket != "" {
		return socket
	}
	if v := os.Getenv("CAPSULAD_SOCKET"); v != "" {
		return v
	}
	return filepath.Join(resolveDataDir(dataDir), "run", "hal.sock")
}

func dial(socket string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///capsulad",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}),
	)
}

func runStatus(socket string, jsonOut bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	info, err := frameworkv1.NewDeviceServiceClient(conn).GetInfo(ctx, &frameworkv1.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("daemon is not reachable on %s: %w", socket, err)
	}
	res, _ := frameworkv1.NewDeviceServiceClient(conn).ListResources(ctx, &frameworkv1.ListResourcesRequest{})
	runs, _ := frameworkv1.NewApplicationServiceClient(conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{})
	st, _ := frameworkv1.NewSystemServiceClient(conn).GetStatus(ctx, &frameworkv1.GetStatusRequest{})
	audio, _ := frameworkv1.NewAudioServiceClient(conn).GetAudioState(ctx, &frameworkv1.GetAudioStateRequest{})
	if jsonOut {
		return printJSON(map[string]any{"device": info, "resources": res.GetResources(), "runs": runs.GetRuns(), "status": st, "audio": audio})
	}
	fmt.Printf("%s  %s\n", info.Model, info.DeviceId)
	fmt.Printf("platform %s · SoC %s · kernel %s\n", info.TargetPlatform, info.Soc, info.Kernel)
	fmt.Printf("firmware %s · daemon %s · boot %s\n", info.Firmware, info.DaemonVersion, info.BootId)
	fmt.Println("\nsubsystems:")
	for _, b := range info.SubsystemBackends {
		fmt.Printf("  %-10s %-9s %s\n", b.Subsystem, b.Status, b.Detail)
	}
	fmt.Println("\nresources:")
	for _, r := range res.GetResources() {
		fmt.Printf("  %-26s %-22s available=%v\n", r.ResourceId, r.Kind, r.Available)
	}
	fmt.Println("\nruns:")
	if len(runs.GetRuns()) == 0 {
		fmt.Println("  (none)")
	}
	for _, r := range runs.GetRuns() {
		fmt.Printf("  %-28s %-12s %-9s pid=%d\n", r.AppId, r.RunId, r.State, r.Pid)
	}
	fmt.Printf("\nmaster volume %d%% · microphone %s\n", int(audio.GetMasterVolume()*100), map[bool]string{true: "muted", false: "live"}[audio.GetMicrophoneMuted()])
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// parseSubcommandFlags accepts the documented `command PATH -flag ...` form:
// the standard flag package stops at the first non-flag argument, so leading
// positional arguments are collected before parsing the flags. Returned are
// the positionals in their original order.
func parseSubcommandFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var leading []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		leading = append(leading, args[0])
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return append(leading, fs.Args()...), nil
}

func runApp(args []string, dataDir, socket, nativeLibDir string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capsulad-cli app <list|install|package|remove|start|stop|runs|provenance>")
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "install":
		if len(args) == 0 {
			return fmt.Errorf("usage: capsulad-cli app install PATH")
		}
		return installAppPackage(args[0], dataDir, socket, nativeLibDir)
	case "package":
		return runAppPackage(args)
	case "remove":
		if len(args) == 0 {
			return fmt.Errorf("usage: capsulad-cli app remove ID")
		}
		return removeAppPackage(args[0], dataDir, socket)
	}
	// -data-dir is the supported way to locate the socket; only a literal
	// -socket overrides it.
	conn, err := dial(resolveSocket(dataDir, socket))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	appsvc := frameworkv1.NewApplicationServiceClient(conn)
	switch sub {
	case "list":
		apps, err := appsvc.ListApplications(ctx, &frameworkv1.ListApplicationsRequest{})
		if err != nil {
			return err
		}
		for _, a := range apps.Applications {
			fmt.Printf("%-30s %-22s %-9s %s\n", a.AppId, a.DisplayName, a.Version, a.Entrypoint.Runtime)
		}
		return nil
	case "runs":
		appID := ""
		if len(args) > 0 {
			appID = args[0]
		}
		runs, err := appsvc.ListRuns(ctx, &frameworkv1.ListRunsRequest{AppId: appID})
		if err != nil {
			return err
		}
		for _, r := range runs.Runs {
			fmt.Printf("%-28s %-36s %-9s pid=%d %s\n", r.AppId, r.RunId, r.State, r.Pid, r.StatusDetail)
		}
		return nil
	case "start":
		if len(args) == 0 {
			return fmt.Errorf("usage: capsulad-cli app start ID [NAME=VALUE ...]")
		}
		var launchArgs []*frameworkv1.LaunchArgument
		for _, kv := range args[1:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("arguments must be NAME=VALUE, got %q", kv)
			}
			launchArgs = append(launchArgs, &frameworkv1.LaunchArgument{Name: parts[0], Value: parts[1]})
		}
		res, err := appsvc.Launch(ctx, &frameworkv1.LaunchRequest{AppId: args[0], Arguments: launchArgs})
		if err != nil {
			return err
		}
		fmt.Printf("run=%s invocation=%s spawned=%v\n", res.RunId, res.InvocationId, res.SpawnedNewProcess)
		return nil
	case "stop":
		if len(args) == 0 {
			return fmt.Errorf("usage: capsulad-cli app stop RUN_ID")
		}
		res, err := appsvc.Stop(ctx, &frameworkv1.StopRequest{RunId: args[0]})
		if err != nil {
			return err
		}
		fmt.Printf("stop requested: %v\n", res.Requested)
		return nil
	case "provenance":
		if len(args) == 0 {
			return fmt.Errorf("usage: capsulad-cli app provenance ID")
		}
		res, err := appsvc.GetBinaryProvenance(ctx, &frameworkv1.GetBinaryProvenanceRequest{AppId: args[0]})
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n%s\n", res.AppId, res.Status, res.Detail)
		if res.SubjectSha256Hex != "" {
			fmt.Printf("subject sha256: %s\n", res.SubjectSha256Hex)
		}
		return nil
	default:
		return fmt.Errorf("unknown app command %q", sub)
	}
}

// runAppPackage writes the canonical artifact for a package directory. The
// default name is <id>-<version>.tar.gz in the current directory.
func runAppPackage(args []string) error {
	fs := flag.NewFlagSet("app package", flag.ContinueOnError)
	out := fs.String("out", "", "output file (default <id>-<version>.tar.gz)")
	maxGlibc := fs.String("max-glibc", os.Getenv("CAPSULAD_MAX_GLIBC"), "reject a package binary importing GLIBC symbols above this version")
	targetABI := fs.String("target-abi", os.Getenv("TARGET_ABI"), "target ABI (linux-gnu|linux-musl); gates every ELF executable and shared library")
	targetArch := fs.String("target-arch", os.Getenv("CAPSULAD_TARGET_ARCH"), "target architecture (aarch64); rejects other ELF machines")
	positional, err := parseSubcommandFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: capsulad-cli app package DIR [-out FILE]")
	}
	dir := positional[0]
	m, err := manifest.Load(dir, filepath.Join(dir, artifact.ManifestFile))
	if err != nil {
		return err
	}
	if *targetABI != "" {
		if err := checkPackageELF(dir, *targetABI, *maxGlibc, *targetArch); err != nil {
			return err
		}
		// A package that needs the shared runtime must be dynamically linked:
		// a fully static musl binary cannot dlopen it.
		if m.NativeSDK != nil && sandbox.LibcForABI(*targetABI) == sandbox.LibcMusl && m.Entrypoint.Path != "" {
			bin := filepath.Join(dir, m.Entrypoint.Path)
			if ok, err := sandbox.IsELF(bin); err != nil {
				return err
			} else if ok {
				if err := sandbox.CheckInterpRequired(bin, sandbox.LibcMusl); err != nil {
					return err
				}
			}
		}
	}
	if *out == "" {
		*out = m.ID + "-" + m.Version + ".tar.gz"
	}
	if err := artifact.Write(dir, *out); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%s %s)\n", *out, m.ID, m.Version)
	return nil
}

// checkPackageELF enforces the target libc and the glibc ceiling on every ELF
// executable and shared library in a staged package. Scripts with a shebang are
// skipped; a non-ELF shared library or executable is rejected (that is how a
// host build would otherwise slip into a Linux package). Assets are untouched.
func checkPackageELF(dir, abi, max, arch string) error {
	want := sandbox.LibcForABI(abi)
	return checkELFTree(dir, want, max, arch, false, "")
}

func runSettings(args []string, socket string, jsonOut bool) error {
	if len(args) > 0 && args[0] == "app" {
		return runAppSettings(args[1:], socket, jsonOut)
	}
	if len(args) == 0 || args[0] != "show" {
		return fmt.Errorf("usage: capsulad-cli settings show\n       capsulad-cli settings app APPID [key=value ...]")
	}
	conn, err := dial(socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := frameworkv1.NewSettingsServiceClient(conn).GetSettings(ctx, &frameworkv1.GetSettingsRequest{})
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(snap)
	}
	v := snap.Values
	fmt.Printf("revision %d\n", snap.Revision)
	fmt.Printf("master volume      %d%%\n", int(v.MasterVolume*100))
	tz := v.Timezone
	if tz == "" {
		tz = "(device /etc/localtime)"
	}
	fmt.Printf("timezone          %s\n", tz)
	fmt.Printf("microphone        %v\n", map[bool]string{true: "muted", false: "live"}[v.MicrophoneMuted])
	fmt.Printf("startup sound      %v\n", map[bool]string{true: "on", false: "off"}[v.StartupSoundEnabled])
	fmt.Printf("assistant          %s\n", v.DefaultAssistantAppId)
	fmt.Printf("wakeword detection %v\n", v.WakewordEnabled)
	if v.Wakewords == nil {
		fmt.Printf("wakewords          legacy: every installed word is live under the switch\n")
	}
	for _, entry := range v.Wakewords {
		app := entry.AppId
		if app == "" {
			app = "(selected assistant)"
		}
		fmt.Printf("wakeword           %s enabled=%v app=%s\n", entry.ModelId, entry.Enabled, app)
	}
	for id, app := range v.DefaultAppByResource {
		fmt.Printf("%-26s %s\n", id, app)
	}
	return nil
}

// runAppSettings shows or updates one application's typed settings. Values are
// read as JSON when they parse and as strings otherwise, so
// `timezone=Asia/Shanghai` needs no quoting.
func runAppSettings(args []string, socket string, jsonOut bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capsulad-cli settings app APPID [key=value ...]")
	}
	appID := args[0]
	pairs := args[1:]
	conn, err := dial(socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := frameworkv1.NewApplicationConfigurationServiceClient(conn)

	if len(pairs) == 0 {
		snap, err := client.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: appID})
		if err != nil {
			return err
		}
		if jsonOut {
			return printJSON(snap)
		}
		fmt.Printf("%s revision %d\n", snap.AppId, snap.Revision)
		for _, v := range snap.Values {
			value := v.ValueJson
			if !v.HasValue {
				value = "(unset)"
			}
			fmt.Printf("  %-16s %s\n", v.Key, value)
		}
		return nil
	}

	snap, err := client.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: appID})
	if err != nil {
		return err
	}
	changes := make([]*frameworkv1.AppSettingChange, 0, len(pairs))
	for _, pair := range pairs {
		key, raw, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return fmt.Errorf("expected key=value, got %q", pair)
		}
		value := raw
		if !json.Valid([]byte(raw)) {
			value = strconv.Quote(raw)
		}
		changes = append(changes, &frameworkv1.AppSettingChange{Key: key, ValueJson: value})
	}
	out, err := client.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{
		AppId: appID, ExpectedRevision: snap.Revision, Changes: changes,
	})
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(out)
	}
	fmt.Printf("%s revision %d\n", out.AppId, out.Revision)
	for _, v := range out.Values {
		fmt.Printf("  %-16s %s\n", v.Key, v.ValueJson)
	}
	return nil
}

func runLogs(args []string, socket string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	appID := fs.String("app", "", "filter by app")
	n := fs.Int("n", 80, "maximum records")
	timeout := fs.Duration("timeout", 700*time.Millisecond, "how long to wait for new records")
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, err := dial(socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	stream, err := frameworkv1.NewDiagnosticsServiceClient(conn).WatchLogs(ctx, &frameworkv1.WatchLogsRequest{AppId: *appID})
	if err != nil {
		return err
	}
	count := 0
	for count < *n {
		rec, err := stream.Recv()
		if err != nil {
			break
		}
		fmt.Println(logbus.Entry{
			TimeNS: rec.MonotonicTimeNs, Component: rec.Component, AppID: rec.AppId,
			Severity: rec.Severity, Message: rec.Message,
		}.FormatLine())
		count++
	}
	return nil
}

func runMedia(args []string, socket string) error {
	if len(args) == 0 || args[0] != "list" {
		return fmt.Errorf("usage: capsulad-cli media list")
	}
	conn, err := dial(socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ifaces, err := frameworkv1.NewAppInterfaceServiceClient(conn).ListInterfaces(ctx, &frameworkv1.ListAppInterfacesRequest{RunningOnly: true})
	if err != nil {
		return err
	}
	found := false
	for _, iface := range ifaces.Interfaces {
		if iface.GrpcService != "capsulad.contracts.media.v1.MediaPlayerService" {
			continue
		}
		found = true
		fmt.Printf("%s (%s) run=%s\n", iface.AppId, iface.Summary, iface.RunId)
	}
	if !found {
		fmt.Println("no running app publishes the standard media contract")
	}
	return nil
}

// --- certificates ----------------------------------------------------------

func securityStore(dataDir, explicit string) *websecurity.Store {
	dir := explicit
	if dir == "" {
		dir = filepath.Join(dataDir, "security")
	}
	return websecurity.Open(dir, websecurity.DefaultOptions())
}

func runCert(args []string, dataDir, securityDir string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capsulad-cli cert <init|renew|status>")
	}
	sub := args[0]
	fs := flag.NewFlagSet("cert", flag.ContinueOnError)
	hostname := fs.String("hostname", "", "certificate hostname")
	days := fs.Int("days", 0, "certificate lifetime in days")
	var ips stringList
	fs.Var(&ips, "ip", "additional IP SAN (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	store := securityStore(dataDir, securityDir)
	if err := store.Load(); err != nil {
		return err
	}
	opts := websecurity.CertOptions{Hostname: *hostname, Days: *days, IPs: ips}
	switch sub {
	case "init":
		info, err := store.InitServerCert(opts)
		if err != nil {
			return err
		}
		printCert(info)
		fmt.Println("next: capsulad-cli auth init")
		return nil
	case "renew":
		info, err := store.RenewServerCert(opts)
		if err != nil {
			return err
		}
		printCert(info)
		fmt.Println("restart capsulad to reload the listener certificate")
		return nil
	case "status":
		info, err := store.CertStatus()
		if err != nil {
			return err
		}
		printCert(info)
		fmt.Printf("admin password set: %v\nactive API keys: %d\n", store.PasswordSet(), store.ActiveAPIKeyCount())
		return nil
	default:
		return fmt.Errorf("unknown cert command %q", sub)
	}
}

func printCert(info websecurity.CertInfo) {
	fmt.Printf("subject:      %s\n", info.Subject)
	fmt.Printf("SHA-256:      %s\n", info.Fingerprint)
	fmt.Printf("valid until:  %s\n", info.NotAfter.Format(time.RFC3339))
	fmt.Printf("DNS names:    %s\n", strings.Join(info.DNSNames, ", "))
	fmt.Printf("IP addresses: %s\n", strings.Join(info.IPAddresses, ", "))
	fmt.Println("self-signed: browsers will warn until this fingerprint is trusted")
}

func runAuth(args []string, dataDir, securityDir string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capsulad-cli auth <init|set-password|key|status>")
	}
	store := securityStore(dataDir, securityDir)
	if err := store.Load(); err != nil {
		return err
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("auth init", flag.ContinueOnError)
		password := fs.String("password", "", "admin password (empty generates one)")
		hostname := fs.String("hostname", "", "certificate hostname")
		days := fs.Int("days", 0, "certificate lifetime in days")
		noKey := fs.Bool("no-api-key", false, "do not create the bootstrap API key")
		keyName := fs.String("key-name", "bootstrap", "bootstrap API key name")
		var ips stringList
		fs.Var(&ips, "ip", "additional IP SAN (repeatable)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if _, err := store.TLSConfig(); err != nil {
			info, err := store.RenewServerCert(websecurity.CertOptions{Hostname: *hostname, Days: *days, IPs: ips})
			if err != nil {
				return err
			}
			printCert(info)
		}
		if !store.PasswordSet() {
			pw := *password
			generated := false
			if pw == "" {
				pw = randomPassword()
				generated = true
			}
			if err := store.SetPassword(pw); err != nil {
				return err
			}
			if generated {
				fmt.Printf("generated admin password: %s\n", pw)
			}
			fmt.Println("admin password set")
		} else {
			fmt.Println("admin password already set; use `capsulad-cli auth set-password` to change it")
		}
		if !*noKey {
			token, info, err := store.CreateAPIKey(*keyName)
			if err != nil {
				if strings.Contains(err.Error(), "already exists") {
					fmt.Printf("API key %q already exists; not printing it again\n", *keyName)
				} else {
					return err
				}
			} else {
				fmt.Printf("created API key %q (%s…): %s\n", info.Name, info.Prefix, token)
				fmt.Println("store it now; only its hash is saved")
			}
		}
		fmt.Println("start capsulad, then open https://HOST:8443/ and sign in with the admin password")
		return nil
	case "set-password":
		fs := flag.NewFlagSet("auth set-password", flag.ContinueOnError)
		password := fs.String("password", "", "new admin password (empty prompts)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		pw := *password
		if pw == "" {
			if term.IsTerminal(int(os.Stdin.Fd())) {
				first, err := readPassword("New admin password: ")
				if err != nil {
					return err
				}
				second, err := readPassword("Confirm password: ")
				if err != nil {
					return err
				}
				if first != second {
					return fmt.Errorf("passwords do not match")
				}
				pw = first
			} else {
				pw = randomPassword()
				fmt.Printf("generated admin password: %s\n", pw)
			}
		}
		if err := store.SetPassword(pw); err != nil {
			return err
		}
		fmt.Println("admin password changed; all browser sessions were revoked")
		return nil
	case "key":
		if len(args) < 2 {
			return fmt.Errorf("usage: capsulad-cli auth key <create NAME|list|revoke NAME>")
		}
		switch args[1] {
		case "create":
			if len(args) < 3 {
				return fmt.Errorf("usage: capsulad-cli auth key create NAME [--scope status|operator]...")
			}
			fs := flag.NewFlagSet("auth key create", flag.ContinueOnError)
			var scopes stringList
			fs.Var(&scopes, "scope", "key scope: status (read-only status/certificate) or operator (full daemon administration, including the MCP tools); repeatable, default status")
			if err := fs.Parse(args[3:]); err != nil {
				return err
			}
			token, info, err := store.CreateAPIKeyWithScopes(args[2], scopes)
			if err != nil {
				return err
			}
			fmt.Printf("created API key %q (%s…): %s\n", info.Name, info.Prefix, token)
			fmt.Printf("scopes:       %s\n", strings.Join(info.Scopes, ", "))
			fmt.Println("store it now; only its hash is saved")
			return nil
		case "list":
			keys := store.ListAPIKeys()
			if len(keys) == 0 {
				fmt.Println("no API keys issued")
				return nil
			}
			for _, k := range keys {
				state := "active"
				if k.Revoked {
					state = "revoked"
				}
				fmt.Printf("%-24s %s… created=%s %s scopes=%s\n", k.Name, k.Prefix,
					k.CreatedAt.Format(time.RFC3339), state, strings.Join(k.Scopes, ","))
			}
			return nil
		case "revoke":
			if len(args) < 3 {
				return fmt.Errorf("usage: capsulad-cli auth key revoke NAME")
			}
			if err := store.RevokeAPIKey(args[2]); err != nil {
				return err
			}
			fmt.Printf("revoked API key %q\n", args[2])
			return nil
		default:
			return fmt.Errorf("unknown auth key command %q", args[1])
		}
	case "status":
		info, err := store.CertStatus()
		if err != nil {
			fmt.Printf("certificate: not initialized (%v)\n", err)
		} else {
			printCert(info)
		}
		fmt.Printf("admin password set: %v\nactive API keys: %d\n", store.PasswordSet(), store.ActiveAPIKeyCount())
		return nil
	default:
		return fmt.Errorf("unknown auth command %q", args[0])
	}
}

func randomPassword() string {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// readPassword reads without echo on a terminal, or from stdin when piped.
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", fmt.Errorf("no password on stdin")
	}
	return sc.Text(), nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// --- firmware inspection ---------------------------------------------------

func runPlatform(args []string, jsonOut bool) error {
	if len(args) > 0 && args[0] == "ledtest" {
		return runLEDTest(args[1:], jsonOut)
	}
	if len(args) == 0 || args[0] != "inspect" {
		return fmt.Errorf("usage: capsulad-cli platform inspect -root DIR\n       capsulad-cli platform ledtest [flags]")
	}
	fs := flag.NewFlagSet("platform inspect", flag.ContinueOnError)
	root := fs.String("root", "", "extracted firmware rootfs")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *root == "" {
		return fmt.Errorf("-root is required")
	}
	report, err := inspect.Inspect(*root)
	if err != nil {
		return err
	}
	if jsonOut {
		return printJSON(report)
	}
	fmt.Printf("platform   %s\n", report.Platform)
	fmt.Printf("model      %s\nSoC        %s\nfirmware   %s\n", report.Model, report.SoC, report.Firmware)
	fmt.Printf("ring       %d RGB pixels\ndisplay    %d monochrome LEDs\nmic GPIO   %d\n", report.RingPixels, report.DisplayLEDs, report.MicEnableGPIO)
	fmt.Printf("daemons    %s\n", strings.Join(report.VendorDaemons, ", "))
	fmt.Printf("sinks      %s\nsources    %s\n", strings.Join(report.PulseSinks, ", "), strings.Join(report.PulseSources, ", "))
	fmt.Println("subsystems:")
	for _, s := range report.Statuses {
		fmt.Printf("  %-10s %-11s %s\n", s.Subsystem, s.Status, s.Detail)
	}
	fmt.Println("evidence:")
	for _, e := range report.Evidence {
		fmt.Printf("  - %s\n", e)
	}
	return nil
}

// --- development admin -----------------------------------------------------

func runDev(args []string, dataDir string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capsulad-cli dev <wakeword|gesture|key|raw|state>")
	}
	sock := filepath.Join(resolveDataDir(dataDir), "run", "dev.sock")
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}, Timeout: 3 * time.Second}
	sub := args[0]
	var method, path string
	var body any
	switch sub {
	case "wakeword":
		method, path = "POST", "/wakeword"
		model := ""
		if len(args) > 1 {
			model = args[1]
		}
		body = map[string]string{"model": model}
	case "gesture":
		if len(args) < 3 {
			return fmt.Errorf("usage: capsulad-cli dev gesture ID PHASE")
		}
		method, path = "POST", "/input/gesture"
		body = map[string]string{"id": args[1], "phase": args[2]}
	case "key":
		if len(args) < 3 {
			return fmt.Errorf("usage: capsulad-cli dev key KEY true|false")
		}
		method, path = "POST", "/input/key"
		body = map[string]any{"key": args[1], "pressed": args[2] == "true"}
	case "raw":
		if len(args) < 4 {
			return fmt.Errorf("usage: capsulad-cli dev raw X Y true|false")
		}
		method, path = "POST", "/input/raw"
		var x, y float64
		fmt.Sscan(args[1], &x)
		fmt.Sscan(args[2], &y)
		body = map[string]any{"x": x, "y": y, "touching": args[3] == "true"}
	case "state":
		method, path = "GET", "/state"
	default:
		return fmt.Errorf("unknown dev command %q", sub)
	}
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, "http://capsulad"+path, reader)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dev admin socket %s: %w", sock, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}
