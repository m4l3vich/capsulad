package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"capsulad/installer/internal/device"
	"capsulad/installer/internal/release"
)

// Defaults for the unattended waits.
const (
	defaultReconnectWindow = 5 * time.Minute
	defaultHealthTimeout   = 60 * time.Second
	defaultWiFiTimeout     = 60 * time.Second
)

// Installer applies one release to one connected device. Its methods are the
// flow's individual steps; the TUI drives them in order and tests drive the
// same sequence with a fake device.
type Installer struct {
	Dev      device.Device
	Provider release.Provider
	Platform device.Platform
	DataDir  string
	Plan     device.DeployPlan
	CLI      string // remote pushed CLI

	ConfigDir string
	WebURL    string

	Log   func(string, ...any)
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error

	ReconnectWindow time.Duration
	HealthTimeout   time.Duration
	WiFiTimeout     time.Duration
	HealthBaseURL   string // test override; empty derives https://<host>:8443
}

// New builds an installer for an already detected platform.
func New(dev device.Device, provider release.Provider, platform device.Platform, configDir string) (*Installer, error) {
	if !platform.Valid() {
		return nil, fmt.Errorf("unsupported platform %q", platform)
	}
	dataDir := platform.DataDir()
	plan := device.NewDeployPlan(dataDir, platform, "deploy-bundle.tar.gz", false)
	i := &Installer{
		Dev:             dev,
		Provider:        provider,
		Platform:        platform,
		DataDir:         dataDir,
		Plan:            plan,
		CLI:             plan.CLIPath,
		ConfigDir:       configDir,
		WebURL:          "https://127.0.0.1:8443",
		Now:             time.Now,
		ReconnectWindow: defaultReconnectWindow,
		HealthTimeout:   defaultHealthTimeout,
		WiFiTimeout:     defaultWiFiTimeout,
	}
	if i.Sleep == nil {
		i.Sleep = sleepContext
	}
	return i, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (i *Installer) logf(format string, args ...any) {
	if i.Log != nil {
		i.Log(format, args...)
	}
}

func (i *Installer) sleep(ctx context.Context, d time.Duration) error {
	if i.Sleep != nil {
		return i.Sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func (i *Installer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Installer) run(ctx context.Context, cmd device.Command) (device.Result, error) {
	if cmd.MaxStdout == 0 {
		cmd.MaxStdout = device.MaxTextRead
	}
	res, err := i.Dev.Run(ctx, cmd)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, &device.CommandError{Args: cmd.Args, ExitCode: res.ExitCode, Stderr: string(res.Stderr), Stdout: string(res.Stdout)}
	}
	return res, nil
}

// runSecret executes a command whose stdin carries a secret and never includes
// the remote stderr in the error. A CLI that echoed its input must not leak it
// into the installer's log.
func (i *Installer) runSecret(ctx context.Context, cmd device.Command, what string) (device.Result, error) {
	if cmd.MaxStdout == 0 {
		cmd.MaxStdout = device.MaxTextRead
	}
	res, err := i.Dev.Run(ctx, cmd)
	if err != nil {
		return res, fmt.Errorf("%s: device command failed: %w", what, err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s failed (exit %d)", what, res.ExitCode)
	}
	return res, nil
}

// BackupDir is where pre-change archives are stored.
func (i *Installer) BackupDir() string {
	if i.ConfigDir == "" {
		return ""
	}
	return filepath.Join(i.ConfigDir, "backups")
}

// Backup streams the stock userspace to the host. It must run before the
// deployment changes anything.
func (i *Installer) Backup(ctx context.Context, address string) (device.BackupResult, error) {
	if !i.Platform.Stock() {
		return device.BackupResult{}, fmt.Errorf("backup is only used on jailbroken stock images")
	}
	dir := i.BackupDir()
	if dir == "" {
		return device.BackupResult{}, fmt.Errorf("no configuration directory for backups")
	}
	i.logf("streaming a pre-change backup of the vendor userspace (nothing is deleted)")
	res, err := device.CreateBackup(ctx, i.Dev, dir, address, i.Platform, i.now())
	if err != nil {
		return device.BackupResult{}, err
	}
	i.logf("backup verified: %s (%s, %d entries)", res.Path, BytesLabel(res.Bytes), res.Entries)
	return res, nil
}

// PreflightAlpine verifies the data filesystem can hold the bundle, the
// selected apps and the documented reserve. Passing nil selects every app the
// release carries for this platform (the upper bound used before the user has
// chosen). The check reports a shortfall in SpaceCheck; only an operational
// failure is returned as an error.
func (i *Installer) PreflightAlpine(ctx context.Context, selected []string) (SpaceCheck, error) {
	bundle, err := i.Provider.Bundle(ctx, string(i.Platform))
	if err != nil {
		return SpaceCheck{}, err
	}
	refs, err := i.Provider.Apps(string(i.Platform))
	if err != nil {
		return SpaceCheck{}, err
	}
	appBytes := SelectedAppBytes(refs, selected)
	info, err := i.df(ctx, i.DataDir)
	if err != nil {
		return SpaceCheck{}, err
	}
	check := CheckSpace(info.Available, bundle.Size, appBytes, AlpineReserveBytes)
	i.logf("disk preflight on %s: %s available, %s required (%s bundle + %s apps + %s reserve)",
		info.MountedOn, BytesLabel(check.AvailableBytes), BytesLabel(check.RequiredBytes),
		BytesLabel(bundle.Size), BytesLabel(appBytes), BytesLabel(AlpineReserveBytes))
	if !check.OK {
		i.logf("warning: not enough free space on %s; the deployer will refuse", info.MountedOn)
	}
	return check, nil
}

// SelectedAppBytes sums the selected app sizes. A nil selection means "all
// apps the release carries" (the pre-confirmation upper bound); an empty
// non-nil selection means the user chose no apps and sums to zero.
func SelectedAppBytes(refs []release.AppRef, selected []string) int64 {
	if selected == nil {
		var total int64
		for _, ref := range refs {
			total = addSat(total, ref.Size)
		}
		return total
	}
	wanted := make(map[string]bool, len(selected))
	for _, id := range selected {
		wanted[id] = true
	}
	var total int64
	for _, ref := range refs {
		if wanted[ref.ID] {
			total = addSat(total, ref.Size)
		}
	}
	return total
}

// PreflightStock mirrors the stock deployer's own fallible writes: it reads the
// staged daemon size from the bundle, the installed daemon size from the device
// and reports the 8 MiB reserve plus any positive delta against available
// space. A shortfall is returned in SpaceCheck, not as an error.
func (i *Installer) PreflightStock(ctx context.Context) (SpaceCheck, error) {
	bundle, err := i.Provider.Bundle(ctx, string(i.Platform))
	if err != nil {
		return SpaceCheck{}, err
	}
	staged, err := BundleEntrySize(bundle.Path, "capsulad")
	if err != nil {
		return SpaceCheck{}, fmt.Errorf("read daemon size from bundle: %w", err)
	}
	var installed int64
	installed = i.installedDaemonSize(ctx, path.Join(i.DataDir, "capsulad"))
	info, err := i.df(ctx, i.DataDir)
	if err != nil {
		return SpaceCheck{}, err
	}
	check := CheckStockSpace(info.Available, staged, installed)
	i.logf("stock space on %s: %s available, %s required (%s reserve + %s daemon growth)",
		info.MountedOn, BytesLabel(check.AvailableBytes), BytesLabel(check.RequiredBytes),
		BytesLabel(StockDeployReserveBytes), BytesLabel(check.RequiredBytes-StockDeployReserveBytes))
	if !check.OK {
		i.logf("warning: %s is short by %s; the deployment will refuse rather than delete anything",
			info.MountedOn, BytesLabel(check.RequiredBytes-check.AvailableBytes))
	}
	return check, nil
}

// AppState is one installed app and whether it has an active run.
type AppState struct {
	ID      string
	Running bool
}

// InstalledApps lists installed apps and probes each for an active run. It is
// used only to describe a short-space situation: the installer never stops or
// moves app packages.
func (i *Installer) InstalledApps(ctx context.Context) ([]AppState, error) {
	list, err := i.run(ctx, device.Command{Args: AppListArgs(path.Join(i.DataDir, "capsulad-cli"), i.DataDir)})
	if err != nil {
		return nil, err
	}
	var out []AppState
	for _, id := range AppListIDs(string(list.Stdout)) {
		if !strings.Contains(id, ".") {
			// A header row or noise, not a namespaced app ID.
			continue
		}
		state := AppState{ID: id}
		runs, err := i.Dev.Run(ctx, device.Command{Args: AppRunsArgs(path.Join(i.DataDir, "capsulad-cli"), i.DataDir, id), MaxStdout: device.MaxTextRead})
		if err == nil && runs.ExitCode == 0 {
			state.Running = RunsShowActive(string(runs.Stdout), id)
		}
		out = append(out, state)
	}
	return out, nil
}

// installedDaemonSize returns the installed daemon's byte size, or zero when
// the image has no daemon yet. `stat` is absent from some stock BusyBox
// images, so `wc -c` is the fallback; both print the size as the first field.
func (i *Installer) installedDaemonSize(ctx context.Context, binary string) int64 {
	for _, argv := range [][]string{
		{"stat", "-c", "%s", binary},
		{"wc", "-c", binary},
	} {
		res, err := i.Dev.Run(ctx, device.Command{Args: argv, MaxStdout: 4 << 10})
		if err != nil || res.ExitCode != 0 {
			continue
		}
		if n, ok := parseByteCount(res.Stdout); ok {
			return n
		}
	}
	return 0
}

func parseByteCount(out []byte) (int64, bool) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (i *Installer) df(ctx context.Context, target string) (DFInfo, error) {
	res, err := i.Dev.Run(ctx, device.Command{Args: []string{"df", "-Pk", target}, MaxStdout: device.MaxTextRead})
	if err != nil {
		return DFInfo{}, err
	}
	if res.ExitCode != 0 {
		// The data directory does not exist yet on a fresh Alpine image.
		parent := path.Dir(target)
		res, err = i.Dev.Run(ctx, device.Command{Args: []string{"df", "-Pk", parent}, MaxStdout: device.MaxTextRead})
		if err != nil {
			return DFInfo{}, err
		}
		if res.ExitCode != 0 {
			return DFInfo{}, fmt.Errorf("df -Pk %s: %s", target, strings.TrimSpace(string(res.Stderr)))
		}
	}
	return ParseDF(string(res.Stdout))
}

// BeforeLaunch runs after the bundle, CLI and deploy script are pushed and the
// startup marker is written, but before the detached deployer starts. It is
// where credentials are created or changed: the stock daemon opens its HTTPS
// listener only when credentials exist at process start.
type BeforeLaunch func(context.Context) error

// DeployResult is what the detached deployer reported.
type DeployResult struct {
	Script             string
	Log                string
	Exit               int
	CLIPath            string
	Bundle             string
	Unconfirmed        bool
	CredentialsApplied bool
}

// DeployError carries the deploy log and whether the outcome is unknown.
type DeployError struct {
	Exit               int
	Log                string
	Unconfirmed        bool
	CredentialsApplied bool
}

func (e *DeployError) Error() string {
	var msg string
	if e.Unconfirmed {
		msg = "deployment outcome is unconfirmed: the device did not answer before the timeout"
	} else {
		msg = fmt.Sprintf("deployment failed (exit %d)", e.Exit)
	}
	if e.CredentialsApplied {
		msg += "; credentials were written before the deployment"
	}
	return msg
}

// Deploy pushes the bundle and device CLI, writes the startup-jingle
// preference, runs beforeLaunch (credentials), clears the previous deploy log,
// launches the deployment detached and waits for its exit line.
//
// The launch is detached because the stock daemon owns Wi-Fi: stopping it drops
// ADB or SSH mid-deployment, which would otherwise kill the deployer. The
// credential step must happen before the launch: a fresh daemon only serves the
// Web UI when its certificate and admin password exist at startup.
func (i *Installer) Deploy(ctx context.Context, jingle bool, beforeLaunch BeforeLaunch) (DeployResult, error) {
	i.Plan.Jingle = jingle
	bundle, err := i.Provider.Bundle(ctx, string(i.Platform))
	if err != nil {
		return DeployResult{}, err
	}
	hasCLI, err := BundleHasCLI(bundle.Path)
	if err != nil {
		return DeployResult{}, err
	}
	if !hasCLI {
		return DeployResult{}, fmt.Errorf("the deployment bundle does not contain capsulad-cli; rebuild it with BUNDLE_CLI=1 (or use a release that carries the CLI), because every deployment step needs the pushed CLI")
	}
	tmp, err := os.MkdirTemp("", "capsulad-installer-*")
	if err != nil {
		return DeployResult{}, err
	}
	defer os.RemoveAll(tmp)
	cliLocal, err := ExtractBundleCLI(bundle.Path, tmp)
	if err != nil {
		return DeployResult{}, err
	}
	scriptLocal := filepath.Join(tmp, "deploy.sh")
	if err := os.WriteFile(scriptLocal, []byte(device.DeployScript(i.Plan)), 0o755); err != nil {
		return DeployResult{}, err
	}

	if _, err := i.run(ctx, device.Command{Args: EnsureStagingArgs(i.Plan.RemoteDir), MaxStdout: 64 << 10}); err != nil {
		return DeployResult{}, fmt.Errorf("prepare %s: %w", i.Plan.RemoteDir, err)
	}
	i.logf("pushing bundle (%s)", BytesLabel(bundle.Size))
	if err := i.Dev.Push(ctx, bundle.Path, i.Plan.BundlePath); err != nil {
		return DeployResult{}, fmt.Errorf("push bundle: %w", err)
	}
	if err := i.Dev.Push(ctx, cliLocal, i.Plan.CLIPath); err != nil {
		return DeployResult{}, fmt.Errorf("push capsulad-cli: %w", err)
	}
	if err := i.Dev.Push(ctx, scriptLocal, i.Plan.ScriptPath); err != nil {
		return DeployResult{}, fmt.Errorf("push deploy script: %w", err)
	}
	for _, cmd := range [][]string{
		ChmodArgs("0755", i.Plan.CLIPath),
		ChmodArgs("0755", i.Plan.ScriptPath),
	} {
		if _, err := i.run(ctx, device.Command{Args: cmd}); err != nil {
			return DeployResult{}, err
		}
	}

	// Startup jingle: the daemon owns the one-shot cue. Marking the preference
	// before the deployment keeps a first boot silent when the user chose OFF.
	// StartupMarker creates the data directory first.
	if _, err := i.run(ctx, device.StartupMarker(i.Plan)); err != nil {
		return DeployResult{}, fmt.Errorf("startup jingle preference: %w", err)
	}

	credentialsApplied := false
	if beforeLaunch != nil {
		i.logf("creating or changing credentials before the daemon starts")
		if err := beforeLaunch(ctx); err != nil {
			return DeployResult{}, fmt.Errorf("credentials were not fully applied; the deployment was not started: %w", err)
		}
		credentialsApplied = true
	}

	// A previous attempt's log must not satisfy this attempt's wait.
	if _, err := i.run(ctx, device.Command{Args: []string{"rm", "-f", i.Plan.LogPath}}); err != nil {
		msg := fmt.Errorf("clear the previous deploy log: %w", err)
		if credentialsApplied {
			return DeployResult{CredentialsApplied: true}, fmt.Errorf("%w (credentials were written before the deployment)", msg)
		}
		return DeployResult{}, msg
	}

	i.logf("starting the deployment detached (ADB/SSH will drop while the daemon stops)")
	launched := false
	for _, argv := range device.DetachedLaunch(i.Plan.ScriptPath) {
		res, err := i.Dev.Run(ctx, device.Command{Args: argv, MaxStdout: 64 << 10})
		if err != nil {
			i.logf("launch via %s failed: %v", argv[0], err)
			continue
		}
		if res.ExitCode == 0 {
			launched = true
			break
		}
		i.logf("launch via %s exited %d", argv[0], res.ExitCode)
	}
	if !launched {
		msg := "could not launch the detached deployment; the device still runs its previous software"
		if credentialsApplied {
			msg += " (credentials were written before the deployment)"
		}
		return DeployResult{CredentialsApplied: credentialsApplied}, errors.New(msg)
	}

	log, exit, err := i.waitDeploy(ctx)
	result := DeployResult{
		Script:             i.Plan.ScriptPath,
		Log:                log,
		Exit:               exit,
		CLIPath:            i.CLI,
		Bundle:             i.Plan.BundlePath,
		CredentialsApplied: credentialsApplied,
	}
	if err != nil {
		var derr *DeployError
		if errors.As(err, &derr) {
			result.Unconfirmed = derr.Unconfirmed
			result.Exit = derr.Exit
			derr.CredentialsApplied = credentialsApplied
		}
		return result, err
	}
	if exit != 0 {
		return result, &DeployError{Exit: exit, Log: log, CredentialsApplied: credentialsApplied}
	}
	i.logf("deployment finished (exit 0)")
	return result, nil
}

func (i *Installer) waitDeploy(ctx context.Context) (string, int, error) {
	window := i.ReconnectWindow
	if window <= 0 {
		window = defaultReconnectWindow
	}
	deadline := i.now().Add(window)
	var lastLog string
	for {
		if ctx.Err() != nil {
			return lastLog, -1, &DeployError{Exit: -1, Log: lastLog, Unconfirmed: true}
		}
		if err := i.Dev.Connect(ctx); err == nil {
			res, err := i.Dev.Run(ctx, device.Command{Args: []string{"cat", i.Plan.LogPath}, MaxStdout: device.MaxTextRead})
			if errors.Is(err, device.ErrOutputLimit) {
				return lastLog, -1, fmt.Errorf("deploy log exceeded the %d byte read cap", device.MaxTextRead)
			}
			if err == nil && res.ExitCode == 0 {
				lastLog = string(res.Stdout)
				if code, ok := device.ParseDeployExit(lastLog); ok {
					return lastLog, code, nil
				}
				i.logf("deploy is still running: %s", lastLine(lastLog))
			}
		} else {
			i.logf("device offline while the daemon restarts: %v", err)
		}
		if i.now().After(deadline) {
			return lastLog, -1, &DeployError{Exit: -1, Log: lastLog, Unconfirmed: true}
		}
		if err := i.sleep(ctx, 2*time.Second); err != nil {
			return lastLog, -1, &DeployError{Exit: -1, Log: lastLog, Unconfirmed: true}
		}
	}
}

// AuthState is what `auth status` said. Known is false when the output could
// not be classified; the caller then treats the password as unset.
type AuthState struct {
	Configured bool
	Known      bool
	Raw        string
}

// ParseAuthStatus classifies the plain-text auth status output. Negative
// phrases are matched first, so "not configured" cannot read as "configured".
func ParseAuthStatus(out string) AuthState {
	low := strings.ToLower(out)
	state := AuthState{Raw: out}
	if configured, ok := authSetBool(low); ok {
		state.Configured, state.Known = configured, true
		return state
	}
	switch {
	case strings.Contains(low, "not configured"), strings.Contains(low, "not set"),
		strings.Contains(low, "unset"), strings.Contains(low, "no password"),
		strings.Contains(low, "no admin"), strings.Contains(low, "missing"),
		strings.Contains(low, "disabled"):
		state.Configured, state.Known = false, true
	case strings.Contains(low, "configured"),
		strings.Contains(low, "password: set"),
		strings.Contains(low, "password set true"),
		strings.Contains(low, "present"), strings.Contains(low, "enabled"):
		state.Configured, state.Known = true, true
	}
	return state
}

// authSetBool reads the real CLI's `admin password set: true|false` line. The
// boolean is matched before any substring alias, so `set: false` can never be
// classified as configured by the phrase "password set".
func authSetBool(low string) (bool, bool) {
	const key = "password set:"
	i := strings.Index(low, key)
	if i < 0 {
		return false, false
	}
	rest := strings.TrimSpace(low[i+len(key):])
	if j := strings.IndexAny(rest, " \t\r\n,;"); j >= 0 {
		rest = rest[:j]
	}
	switch rest {
	case "true", "1", "yes", "set":
		return true, true
	case "false", "0", "no", "unset":
		return false, true
	}
	return false, false
}

// AuthStatus asks the device whether an admin password exists. It uses the
// installed CLI at <data>/capsulad-cli, so it works before this installer has
// pushed anything and reports unknown when no daemon is installed yet.
func (i *Installer) AuthStatus(ctx context.Context) (AuthState, error) {
	installed := path.Join(i.DataDir, "capsulad-cli")
	res, err := i.Dev.Run(ctx, device.Command{Args: AuthStatusArgs(installed, i.DataDir), MaxStdout: device.MaxTextRead})
	if err != nil {
		return AuthState{}, err
	}
	if res.ExitCode != 0 {
		return AuthState{Raw: string(res.Stderr)}, nil
	}
	return ParseAuthStatus(string(res.Stdout) + string(res.Stderr)), nil
}

// Hostname reads the device hostname, falling back to the generic appliance
// name. `auth init -hostname` embeds it in the certificate.
func (i *Installer) Hostname(ctx context.Context) string {
	res, err := i.Dev.Run(ctx, device.Command{Args: []string{"hostname"}, MaxStdout: 4 << 10})
	if err != nil || res.ExitCode != 0 {
		return "capsula-mini"
	}
	name := strings.TrimSpace(string(res.Stdout))
	if name == "" || strings.ContainsAny(name, " \t\n") {
		return "capsula-mini"
	}
	return name
}

// VerifyKeptCredentials proves a kept password is usable before the launch:
// the password must be set and the server certificate must exist. The daemon
// will not start its HTTPS listener without both.
func (i *Installer) VerifyKeptCredentials(ctx context.Context) error {
	state, err := i.AuthStatus(ctx)
	if err != nil {
		return fmt.Errorf("verify the existing admin password: %w", err)
	}
	if !state.Configured || !state.Known {
		return fmt.Errorf("no admin password is set on the device; choose generate or set your own instead of keeping one")
	}
	cert, err := i.ReadServerCert(ctx)
	if err != nil {
		return fmt.Errorf("an admin password is set but the server certificate is missing; run `capsulad-cli auth init` and `auth set-password` on the device, then retry: %w", err)
	}
	if len(bytes.TrimSpace(cert)) == 0 {
		return fmt.Errorf("an admin password is set but the server certificate is empty; run `capsulad-cli auth init` and `auth set-password` on the device, then retry")
	}
	return nil
}

// VerifyIdentity re-reads the device identity after a reconnect and refuses to
// continue if a serial that was present at the start has changed. ADB has no
// cryptographic peer authentication; a missing serial can only be acknowledged
// by the operator and is documented as residual risk.
func (i *Installer) VerifyIdentity(ctx context.Context, expected device.Identity) error {
	if i.Dev.Kind() != "adb" {
		return nil
	}
	got, err := device.ReadIdentity(ctx, i.Dev)
	if err != nil {
		if expected.Serial == "" {
			i.logf("warning: could not re-read device identity after reconnect: %v", err)
			return nil
		}
		return fmt.Errorf("could not re-read the device identity after reconnect: %w; refusing to send further credentials", err)
	}
	if expected.Serial == "" {
		i.logf("warning: this device has no stable serial; ADB cannot authenticate it across a reconnect")
		return nil
	}
	if got.Serial == "" {
		return fmt.Errorf("device identity is unavailable after reconnect (expected %q); refusing to send further credentials", expected.Serial)
	}
	if got.Serial != expected.Serial {
		return fmt.Errorf("device identity changed after reconnect (was %q, now %q); refusing to send further credentials", expected.Serial, got.Serial)
	}
	i.logf("device identity confirmed: %s", got)
	return nil
}

// ApplyPassword creates the certificate if needed and then always sets the
// displayed password. `auth init` no-ops when a password already exists, so
// init alone would leave a different password in place.
func (i *Installer) ApplyPassword(ctx context.Context, password, hostname, address string) error {
	if err := i.AuthInit(ctx, password, hostname, address); err != nil {
		return err
	}
	return i.AuthSetPassword(ctx, password)
}

// AuthInit creates the server certificate and admin password. The password is
// supplied on stdin and never appears in argv.
func (i *Installer) AuthInit(ctx context.Context, password, hostname, address string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	cmd := device.Command{
		Args:  AuthInitArgs(i.CLI, i.DataDir, hostname, hostOnly(address)),
		Stdin: []byte(password + "\n"),
	}
	if _, err := i.runSecret(ctx, cmd, "auth init"); err != nil {
		return err
	}
	i.logf("created the admin password and device certificate for %s", hostname)
	return nil
}

// AuthSetPassword changes the admin password from stdin.
func (i *Installer) AuthSetPassword(ctx context.Context, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	cmd := device.Command{
		Args:  AuthSetPasswordArgs(i.CLI, i.DataDir),
		Stdin: []byte(password + "\n"),
	}
	if _, err := i.runSecret(ctx, cmd, "auth set-password"); err != nil {
		return err
	}
	i.logf("admin password updated")
	return nil
}

// ReadServerCert loads <data>/security/server.crt from the device.
func (i *Installer) ReadServerCert(ctx context.Context) ([]byte, error) {
	res, err := i.run(ctx, device.Command{Args: []string{"cat", path.Join(i.DataDir, "security", "server.crt")}})
	if err != nil {
		return nil, fmt.Errorf("read device certificate: %w", err)
	}
	if len(res.Stdout) == 0 {
		return nil, fmt.Errorf("device certificate is empty")
	}
	return res.Stdout, nil
}

// WaitHealthy pins the device certificate and polls the Web UI. It returns the
// certificate fingerprint for the final summary.
func (i *Installer) WaitHealthy(ctx context.Context) (string, error) {
	cert, err := i.ReadServerCert(ctx)
	if err != nil {
		return "", err
	}
	base := i.HealthBaseURL
	if base == "" {
		base = HTTPSBase(i.Dev.Addr(), 8443)
	}
	timeout := i.HealthTimeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	i.logf("polling %s%s (trust anchored to the device certificate)", base, HealthPath)
	if err := WaitHealth(hctx, cert, base, time.Second, i.logf); err != nil {
		return "", err
	}
	fp, err := CertFingerprint(cert)
	if err != nil {
		return "", err
	}
	return fp, nil
}

// WiFiStatus reads the station state and remembers it for recovery advice.
func (i *Installer) WiFiStatus(ctx context.Context) (WiFi, error) {
	res, err := i.run(ctx, device.Command{Args: WifiStatusArgs(i.CLI, i.DataDir, i.WebURL)})
	if err != nil {
		return WiFi{}, err
	}
	parsed, err := ParseWiFiStatus(res.Stdout)
	if err != nil {
		return WiFi{}, err
	}
	return parsed.WiFi, nil
}

// WiFiScan returns the deduplicated scan results.
func (i *Installer) WiFiScan(ctx context.Context) ([]Network, error) {
	res, err := i.run(ctx, device.Command{Args: WifiScanArgs(i.CLI, i.DataDir, i.WebURL)})
	if err != nil {
		return nil, err
	}
	parsed, err := ParseWiFiScan(res.Stdout)
	if err != nil {
		return nil, err
	}
	return DedupeNetworks(parsed.Results), nil
}

// WiFiConnect joins SSID and waits for an address. The passphrase goes over
// stdin, never argv.
func (i *Installer) WiFiConnect(ctx context.Context, ssid, security, passphrase string) (WiFi, error) {
	if err := ValidateSSID(ssid); err != nil {
		return WiFi{}, err
	}
	security = strings.TrimSpace(security)
	switch strings.ToLower(security) {
	case "open", "none":
		security = "open"
	}
	if security == "open" && passphrase != "" {
		return WiFi{}, fmt.Errorf("an open network does not take a passphrase")
	}
	if security == "" && passphrase == "" {
		return WiFi{}, fmt.Errorf("a passphrase is required unless the network is explicitly open; retry with -security open for an open network")
	}
	cmd := device.Command{Args: WifiConnectArgs(i.CLI, i.DataDir, i.WebURL, ssid, security, passphrase != "")}
	if passphrase != "" {
		if strings.ContainsAny(passphrase, "\r\n") {
			return WiFi{}, fmt.Errorf("passphrase must be one line")
		}
		cmd.Stdin = []byte(passphrase + "\n")
	}
	res, err := i.runSecret(ctx, cmd, "wifi connect")
	if err != nil {
		if parsed, perr := ParseWiFiStatus(res.Stdout); perr == nil && parsed.WiFi.LastError != "" {
			return WiFi{}, fmt.Errorf("%w: %s", err, parsed.WiFi.LastError)
		}
		return WiFi{}, err
	}
	if parsed, err := ParseWiFiStatus(res.Stdout); err == nil {
		if parsed.WiFi.Connected() {
			i.logf("connected to %s (%s)", parsed.WiFi.SSID, parsed.WiFi.IPAddress)
			return parsed.WiFi, nil
		}
	}
	return i.waitWiFi(ctx, ssid)
}

func (i *Installer) waitWiFi(ctx context.Context, ssid string) (WiFi, error) {
	timeout := i.WiFiTimeout
	if timeout <= 0 {
		timeout = defaultWiFiTimeout
	}
	deadline := i.now().Add(timeout)
	idle := time.Second
	for {
		w, err := i.WiFiStatus(ctx)
		if err != nil {
			return w, err
		}
		if w.Connected() && (ssid == "" || strings.EqualFold(w.SSID, ssid)) {
			i.logf("Wi-Fi connected: %s (%s)", w.SSID, w.IPAddress)
			return w, nil
		}
		if i.now().After(deadline) {
			detail := w.Detail
			if detail == "" {
				detail = w.LastError
			}
			return w, fmt.Errorf("Wi-Fi did not report a connected state with an address within %s (%s)", timeout, detail)
		}
		if err := i.sleep(ctx, idle); err != nil {
			return w, err
		}
	}
}

// InstallApps pushes and installs each selected app in catalogue order. A
// failed or missing artifact is recorded, never silently skipped.
func (i *Installer) InstallApps(ctx context.Context, selected map[string]bool) ([]AppResult, error) {
	apps, err := SelectedApps(selected)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, nil
	}
	if _, err := i.run(ctx, device.Command{Args: EnsureStagingArgs(i.Plan.RemoteDir), MaxStdout: 64 << 10}); err != nil {
		return nil, err
	}
	var results []AppResult
	var failed int
	for _, app := range apps {
		res := AppResult{ID: app.ID, Name: app.Name}
		artifact, err := i.Provider.App(ctx, string(i.Platform), app.ID)
		if err != nil {
			res.Status, res.Detail = "failed", "artifact unavailable: "+err.Error()
			results = append(results, res)
			failed++
			i.logf("%s: %s", app.Name, res.Detail)
			continue
		}
		res.Version = artifactVersion(artifact.Path)
		remote := path.Join(i.Plan.RemoteDir, "apps", filepath.Base(artifact.Path))
		i.logf("pushing %s", app.Name)
		if err := i.Dev.Push(ctx, artifact.Path, remote); err != nil {
			res.Status, res.Detail = "failed", "push: "+err.Error()
			results = append(results, res)
			failed++
			continue
		}
		installRes, err := i.Dev.Run(ctx, device.Command{Args: AppInstallArgs(i.CLI, i.DataDir, remote), MaxStdout: device.MaxTextRead})
		if err != nil || installRes.ExitCode != 0 {
			res.Status = "failed"
			res.Detail = "install: " + errText(err, installRes)
			results = append(results, res)
			failed++
			i.logf("%s: %s", app.Name, res.Detail)
			continue
		}
		list, err := i.run(ctx, device.Command{Args: AppListArgs(i.CLI, i.DataDir)})
		if err != nil {
			res.Status, res.Detail = "failed", "verify: "+err.Error()
			results = append(results, res)
			failed++
			continue
		}
		if !HasAppID(string(list.Stdout), app.ID) {
			res.Status, res.Detail = "failed", "not listed by app list after install"
			results = append(results, res)
			failed++
			i.logf("%s: %s", app.Name, res.Detail)
			continue
		}
		res.Status = "installed"
		results = append(results, res)
		i.logf("%s installed (%s)", app.Name, res.Version)
	}
	if failed > 0 {
		return results, fmt.Errorf("%d of %d selected apps did not install", failed, len(apps))
	}
	return results, nil
}

// MarusiaResult reports what was migrated or set, never the values.
type MarusiaResult struct {
	Migrated bool
	TokenSet bool
	Skipped  bool
	Detail   string
}

// Marusia migrates the stock voice-agent session when it is available, or sets
// a VK access token supplied by the user. The stock session file is read, not
// moved or deleted: cleanup is retire-only.
func (i *Installer) Marusia(ctx context.Context, token string) (MarusiaResult, error) {
	if sessionFile := i.Platform.SessionFile(); sessionFile != "" {
		res, err := i.Dev.Run(ctx, device.Command{Args: []string{"cat", sessionFile}, MaxStdout: device.MaxSessionRead})
		if errors.Is(err, device.ErrOutputLimit) {
			return MarusiaResult{}, fmt.Errorf("stock session file exceeds the %d byte read cap", device.MaxSessionRead)
		}
		if err == nil && res.ExitCode == 0 {
			if session, ok := ParseMarusiaSession(res.Stdout); ok {
				// The vendor session and secret travel on stdin; the generated
				// argv carries only the settings key names. Report only the
				// exit status, never the argv or output.
				for _, cmd := range MarusiaMigrationCommands(i.CLI, i.DataDir, session) {
					mig, migErr := i.Dev.Run(ctx, cmd)
					if migErr != nil {
						return MarusiaResult{}, fmt.Errorf("migrate Marusia session: device command failed: %w", migErr)
					}
					if mig.ExitCode != 0 {
						return MarusiaResult{}, fmt.Errorf("migrate Marusia session: settings command failed (exit %d); the vendor file was left in place", mig.ExitCode)
					}
				}
				i.logf("migrated the existing Marusia session id and secret into app settings (vendor file left in place)")
				return MarusiaResult{Migrated: true, Detail: "existing stock session migrated; vendor file left in place"}, nil
			}
			i.logf("stock Marusia session file was present but incomplete; a VK token is needed instead")
		}
	}
	if strings.TrimSpace(token) == "" {
		return MarusiaResult{Skipped: true, Detail: "no token supplied; Marusia was installed without a VK token"}, nil
	}
	if err := ValidateSecretSetting(token); err != nil {
		return MarusiaResult{}, fmt.Errorf("VK access token: %w", err)
	}
	cmd := device.Command{
		Args:  SettingsStdinArgs(i.CLI, i.DataDir, "org.capsulad.marusia", "vk_access_token"),
		Stdin: []byte(token + "\n"),
	}
	if _, err := i.runSecret(ctx, cmd, "set Marusia token"); err != nil {
		return MarusiaResult{}, err
	}
	i.logf("stored the VK access token in app settings (value never entered argv)")
	return MarusiaResult{TokenSet: true, Detail: "VK access token stored in app settings"}, nil
}

// appVersionFromPath recovers "<version>" from "<id>-<version>.tar.gz".
func artifactVersion(name string) string {
	base := filepath.Base(name)
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".gz"), ".tar")
	if i := strings.LastIndex(base, "-"); i >= 0 && i+1 < len(base) {
		return base[i+1:]
	}
	return ""
}

func errText(err error, res device.Result) string {
	if err != nil {
		return err.Error()
	}
	if len(res.Stderr) > 0 {
		return lastLine(string(res.Stderr))
	}
	return fmt.Sprintf("exit %d", res.ExitCode)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
