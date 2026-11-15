package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/artifact"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/registry"
	"capsulad/internal/sandbox"
)

const (
	deployStagingDir  = ".deploy-staging"
	defaultInitScript = "/etc/init.d/S99btctl.sh"
	daemonBinaryName  = "capsulad"
	cliBinaryName     = "capsulad-cli"
	bootHookName      = "S99btctl.sh"
)

// bundleLimits bound a deployment bundle supplied by an operator, possibly
// from external media. They are sized for the 78.5 MiB data partition, where
// the archive is unpacked into the staging tree before it can be inspected.
var bundleLimits = artifact.Limits{
	MaxFileBytes:  64 << 20,
	MaxTotalBytes: 128 << 20,
	MaxEntries:    8192,
}

// extractionReserve is free space kept for the daemon, apps, settings and logs
// while a bundle archive is unpacked.
const extractionReserve = 32 << 20

// deployment is one validated bundle plus the state needed to apply it.
type deployment struct {
	bundlePath string
	bundleRoot string
	archive    bool
	dataDir    string
	socket     string
	initScript string
	restart    string
	dryRun     bool
	timeout    time.Duration
	staging    string

	daemonSrc    string
	cliSrc       string
	daemonStaged string
	cliStaged    string
	libSrc       string
	bootSrc      string

	packages    []*artifact.Package
	libFiles    int
	needRestart bool
	// hook is the replaced boot hook, kept as <init-script>.prev only until
	// the health gate accepts the deployment.
	hook *activation
}

// runDeploy applies a deployment bundle: validate, install/update apps,
// replace the native runtime, activate the daemon and CLI binaries, restart
// through the boot service and gate on a healthy daemon. Nothing is written
// before the bundle has been fully validated.
func runDeploy(args []string, dataDir, socket string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	initScript := fs.String("init-script", defaultInitScript, "boot service invoked to restart the daemon")
	restartMode := fs.String("restart", "auto", "restart policy: auto, always or never")
	dryRun := fs.Bool("dry-run", false, "print the plan without changing installed state")
	timeout := fs.Duration("timeout", 45*time.Second, "time to wait for a healthy daemon after restart")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `usage: capsulad-cli deploy PATH [-init-script PATH] [-restart auto|always|never] [-dry-run] [-timeout DURATION]

PATH is a bundle directory or a .tar.gz containing:
  capsulad              daemon binary (installed as <data-dir>/capsulad, 0755)
  capsulad-cli          CLI binary (installed as <data-dir>/capsulad-cli, 0755)
  lib/                  shared native runtime and headers (replaces managed files)
  apps/*.tar.gz         application packages (installed or atomically updated)
  boot/%s       boot hook (installed as -init-script when present)

Only those top-level entries are meaningful; anything else is reported and
ignored. Nothing is written outside <data-dir> except the boot hook bundled at
boot/%s.

Flags:
`, bootHookName, bootHookName)
		fs.PrintDefaults()
	}
	positional, err := parseSubcommandFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: capsulad-cli deploy PATH [-init-script PATH] [-restart auto|always|never] [-dry-run] [-timeout DURATION]")
	}
	switch *restartMode {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("-restart must be auto, always or never, got %q", *restartMode)
	}

	resolvedDataDir, err := filepath.Abs(resolveDataDir(dataDir))
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	d := &deployment{
		bundlePath: positional[0],
		dataDir:    resolvedDataDir,
		socket:     resolveSocket(resolvedDataDir, socket),
		initScript: *initScript,
		restart:    *restartMode,
		dryRun:     *dryRun,
		timeout:    *timeout,
	}
	// A dry run stages the bundle (an archive must be unpacked to be
	// inspected) outside the data directory and changes nothing durable.
	if d.dryRun {
		temp, err := os.MkdirTemp("", "capsula-deploy-")
		if err != nil {
			return err
		}
		d.staging = temp
	} else {
		d.staging = filepath.Join(d.dataDir, deployStagingDir)
		if err := os.RemoveAll(d.staging); err != nil {
			return err
		}
		if err := os.MkdirAll(d.staging, 0o750); err != nil {
			return err
		}
	}
	defer os.RemoveAll(d.staging)

	if err := d.prepare(); err != nil {
		return err
	}
	defer d.close()
	d.printPlan(os.Stdout)
	if d.dryRun {
		return nil
	}
	return d.apply()
}

func (d *deployment) daemonPath() string { return filepath.Join(d.dataDir, daemonBinaryName) }

func (d *deployment) appPath(appID string) string { return filepath.Join(d.dataDir, "apps", appID) }

func (d *deployment) prepare() error {
	root, archive, err := unpackBundle(d.bundlePath, d.staging, d.dataDir)
	if err != nil {
		return err
	}
	d.bundleRoot, d.archive = root, archive
	if err := d.scanBundle(); err != nil {
		return err
	}
	if d.daemonSrc != "" {
		d.daemonStaged = filepath.Join(d.staging, daemonBinaryName+".new")
		if err := copyExecutable(d.daemonSrc, d.daemonStaged); err != nil {
			return fmt.Errorf("stage daemon: %w", err)
		}
		// A dry run never executes bundle content: the incoming daemon is
		// untrusted until it is installed and probed after activation.
		if !d.dryRun {
			if err := verifyDaemonBinary(d.daemonStaged); err != nil {
				return err
			}
		}
	}
	if d.cliSrc != "" {
		d.cliStaged = filepath.Join(d.staging, cliBinaryName+".new")
		if err := copyExecutable(d.cliSrc, d.cliStaged); err != nil {
			return fmt.Errorf("stage %s: %w", cliBinaryName, err)
		}
	}
	if err := d.checkNativeRequirements(); err != nil {
		return err
	}
	if d.bootSrc != "" {
		if err := validateShellScript(d.bootSrc); err != nil {
			return err
		}
	}

	switch d.restart {
	case "always":
		d.needRestart = true
	case "never":
		d.needRestart = false
	case "auto":
		// The daemon opens the registry once at startup; new binaries, a new
		// native runtime or new packages are only active after a restart.
		d.needRestart = d.daemonSrc != "" || d.libSrc != "" || len(d.packages) > 0
	}
	if d.needRestart {
		if err := requireBootService(d.initScript); err != nil {
			if !d.dryRun {
				return err
			}
			fmt.Fprintf(os.Stderr, "deploy: warning: %v\n", err)
		}
	}
	return nil
}

// unpackBundle accepts a directory or a .tar.gz bundle and returns its root.
func unpackBundle(path, staging, dataDir string) (string, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, fmt.Errorf("deployment bundle %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("deployment bundle %s: symlinks are not supported; use a directory or regular .tar.gz file", path)
	}
	if info.IsDir() {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", false, err
		}
		return abs, false, nil
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("deployment bundle %s: expected a directory or a .tar.gz archive", path)
	}
	dst := filepath.Join(staging, "bundle")
	if err := checkExtractionSpace(dataDir, bundleLimits.MaxTotalBytes); err != nil {
		return "", false, fmt.Errorf("deployment bundle %s: %w", path, err)
	}
	if err := artifact.Extract(path, dst, bundleLimits); err != nil {
		return "", false, fmt.Errorf("deployment bundle %s: %w", path, err)
	}
	return dst, true, nil
}

// freeSpace reports the bytes available on the filesystem holding a directory.
// It is a variable so tests can exercise the preflight without a full disk.
var freeSpace = freeSpaceBytes

// checkExtractionSpace refuses to unpack a bundle archive when the target
// filesystem cannot hold it plus the reserve the daemon needs while the
// archive is staged. When free space cannot be determined (see freeSpaceBytes)
// the preflight is skipped; extraction still fails safely and the staging tree
// is removed.
func checkExtractionSpace(dir string, archiveBytes int64) error {
	free, known, err := freeSpace(dir)
	if err != nil || !known {
		return nil
	}
	need := uint64(0)
	if archiveBytes > 0 {
		need = uint64(archiveBytes)
	}
	if free < extractionReserve || free-extractionReserve < need {
		return fmt.Errorf("%s has %s free space; unpacking this bundle needs %s plus a %s reserve, so free space on the data partition and retry",
			dir, mib(free), mib(need), mib(extractionReserve))
	}
	return nil
}

func mib(n uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
}

func (d *deployment) scanBundle() error {
	entries, err := os.ReadDir(d.bundleRoot)
	if err != nil {
		return fmt.Errorf("deployment bundle %s: %w", d.bundleRoot, err)
	}
	known := map[string]bool{
		daemonBinaryName: true, cliBinaryName: true, "lib": true, "apps": true, "boot": true,
	}
	for _, e := range entries {
		name := e.Name()
		// BSD tar stores extended attributes and resource forks in "._name"
		// entries; they are not part of the bundle layout.
		if isAppleDouble(name) {
			continue
		}
		if !known[name] {
			fmt.Fprintf(os.Stderr, "deploy: ignoring unknown bundle entry %q\n", name)
			continue
		}
		p := filepath.Join(d.bundleRoot, name)
		// Lstat, not Stat: a directory bundle is inspected in place, so a
		// symlink would otherwise pull a file from outside the bundle into an
		// installation. Archive bundles cannot contain links; directory
		// bundles must match that rule.
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle entry %q: symlinks are not supported; copy the target into the bundle instead", p)
		}
		switch name {
		case daemonBinaryName, cliBinaryName:
			if !info.Mode().IsRegular() {
				return fmt.Errorf("bundle entry %q must be a regular file", p)
			}
			if name == daemonBinaryName {
				d.daemonSrc = p
			} else {
				d.cliSrc = p
			}
		case "lib":
			if !info.IsDir() {
				return fmt.Errorf("bundle entry %q must be a directory", p)
			}
			d.libSrc = p
			if err := filepath.WalkDir(p, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if isAppleDouble(entry.Name()) {
					if entry.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				if entry.IsDir() {
					return nil
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("bundle lib entry %q: symlinks are not supported; store a regular file instead", path)
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("bundle lib entry %q: only regular files are supported", path)
				}
				d.libFiles++
				return nil
			}); err != nil {
				return err
			}
		case "apps":
			if !info.IsDir() {
				return fmt.Errorf("bundle entry %q must be a directory", p)
			}
			names, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			sort.Slice(names, func(i, j int) bool { return names[i].Name() < names[j].Name() })
			stage := filepath.Join(d.staging, "packages")
			if err := os.MkdirAll(stage, 0o750); err != nil {
				return err
			}
			for _, entry := range names {
				if isAppleDouble(entry.Name()) {
					continue
				}
				artifactPath := filepath.Join(p, entry.Name())
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("bundle app %q: symlinks are not supported; store a regular .tar.gz instead", artifactPath)
				}
				if entry.IsDir() || !isAppArchive(entry.Name()) {
					fmt.Fprintf(os.Stderr, "deploy: ignoring non-artifact bundle entry %q\n", artifactPath)
					continue
				}
				if !entry.Type().IsRegular() {
					return fmt.Errorf("bundle app %q: only regular .tar.gz files are supported", artifactPath)
				}
				pkg, err := artifact.OpenIn(stage, artifactPath)
				if err != nil {
					return fmt.Errorf("bundle app %s: %w", entry.Name(), err)
				}
				d.packages = append(d.packages, pkg)
			}
		case "boot":
			if !info.IsDir() {
				return fmt.Errorf("bundle entry %q must be a directory", p)
			}
			hook := filepath.Join(p, bootHookName)
			if hookInfo, err := os.Lstat(hook); err == nil {
				if hookInfo.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("bundle boot hook %q: symlinks are not supported; copy the script into the bundle instead", hook)
				}
				if !hookInfo.Mode().IsRegular() {
					return fmt.Errorf("bundle boot hook %q must be a regular file", hook)
				}
				d.bootSrc = hook
			}
			bootEntries, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			for _, entry := range bootEntries {
				if isAppleDouble(entry.Name()) {
					continue
				}
				bootPath := filepath.Join(p, entry.Name())
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("bundle boot entry %q: symlinks are not supported; copy the target into the bundle instead", bootPath)
				}
				if entry.Name() != bootHookName {
					fmt.Fprintf(os.Stderr, "deploy: ignoring unknown boot entry %q\n", bootPath)
				}
			}
		}
	}
	if d.daemonSrc == "" && d.cliSrc == "" && d.libSrc == "" && len(d.packages) == 0 && d.bootSrc == "" {
		return fmt.Errorf("deployment bundle %s has no actionable payload (expected capsulad, capsulad-cli, lib/, apps/ or boot/%s)", d.bundleRoot, bootHookName)
	}
	return nil
}

// checkNativeRequirements compares every incoming app's declared ABI with the
// runtime this deployment would leave installed: the bundle's own lib/ when
// present, otherwise the existing <data-dir>/lib.
func (d *deployment) checkNativeRequirements() error {
	nativeDir := d.libSrc
	if nativeDir == "" {
		candidate := filepath.Join(d.dataDir, "lib")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			nativeDir = candidate
		}
	}
	for _, pkg := range d.packages {
		req := pkg.Manifest.NativeSDK
		if req == nil {
			continue
		}
		if nativeDir == "" {
			return fmt.Errorf("app %s requires native SDK ABI %d.%d but the bundle has no lib/ and no runtime is installed", pkg.Manifest.ID, req.ABIMajor, req.MinMinor)
		}
		rt, err := sandbox.LocateRuntime(nativeDir)
		if err != nil {
			return fmt.Errorf("app %s requires native SDK ABI %d.%d: %w", pkg.Manifest.ID, req.ABIMajor, req.MinMinor, err)
		}
		if err := sandbox.CheckRequirement(rt, *req); err != nil {
			return fmt.Errorf("app %s: %w", pkg.Manifest.ID, err)
		}
	}
	return nil
}

func isAppArchive(name string) bool {
	return strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}

// isAppleDouble reports a macOS tar metadata entry. BSD tar stores extended
// attributes and resource forks in "._name" files; they carry no bundle data.
func isAppleDouble(name string) bool {
	return strings.HasPrefix(name, "._")
}

func (d *deployment) close() {
	for _, pkg := range d.packages {
		_ = pkg.Close()
	}
	d.packages = nil
}

func (d *deployment) printPlan(w io.Writer) {
	kind := "directory"
	if d.archive {
		kind = "archive"
	}
	fmt.Fprintf(w, "bundle: %s (%s)\n", d.bundlePath, kind)
	fmt.Fprintf(w, "data dir: %s\n", d.dataDir)
	for _, pkg := range d.packages {
		action := "install"
		if _, err := os.Stat(d.appPath(pkg.Manifest.ID)); err == nil {
			action = "update"
		}
		fmt.Fprintf(w, "app: %s %s (%s)\n", action, pkg.Manifest.ID, pkg.Manifest.Version)
	}
	if d.libSrc != "" {
		fmt.Fprintf(w, "lib: %d files -> %s\n", d.libFiles, filepath.Join(d.dataDir, "lib"))
	}
	if d.daemonStaged != "" {
		fmt.Fprintf(w, "daemon: %s -> %s\n", d.daemonSrc, d.daemonPath())
		if d.dryRun {
			fmt.Fprintln(w, "daemon probe: deferred (-dry-run)")
		}
	}
	if d.cliStaged != "" {
		fmt.Fprintf(w, "cli: %s -> %s\n", d.cliSrc, filepath.Join(d.dataDir, cliBinaryName))
	}
	if d.bootSrc != "" {
		if d.needRestart {
			fmt.Fprintf(w, "boot hook: %s -> %s (restored on a failed restart)\n", d.bootSrc, d.initScript)
		} else {
			fmt.Fprintf(w, "boot hook: %s (not installed: -restart never)\n", d.bootSrc)
		}
	}
	if d.needRestart {
		fmt.Fprintf(w, "restart: %s restart\n", d.initScript)
		fmt.Fprintf(w, "health: %s -health -socket %s (up to %s)\n", d.daemonPath(), d.socket, d.timeout)
	} else {
		fmt.Fprintln(w, "restart: skipped (-restart never)")
	}
	fmt.Fprintln(w, "trust: bundle contents are applied as supplied; verifying their origin is the operator's decision")
}

func (d *deployment) apply() error {
	if err := d.stopReplacedApps(); err != nil {
		return err
	}
	root := filepath.Join(d.dataDir, "apps")
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		return err
	}
	for _, pkg := range d.packages {
		installed := false
		if _, err := os.Stat(d.appPath(pkg.Manifest.ID)); err == nil {
			installed = true
		}
		app, err := reg.InstallPackage(pkg)
		if err != nil {
			return fmt.Errorf("install app %s: %w", pkg.Manifest.ID, err)
		}
		verb := "installed"
		if installed {
			verb = "updated"
		}
		fmt.Printf("%s app %s (%s)\n", verb, app.ID(), app.Manifest.Version)
	}
	if d.libSrc != "" {
		libDst := filepath.Join(d.dataDir, "lib")
		if err := syncLib(d.libSrc, libDst); err != nil {
			return err
		}
		fmt.Printf("updated native runtime in %s\n", libDst)
	}
	activated, err := d.activateBinaries()
	if err != nil {
		return err
	}
	if d.daemonStaged != "" {
		fmt.Printf("installed daemon %s\n", d.daemonPath())
	}
	if d.cliStaged != "" {
		fmt.Printf("installed %s\n", filepath.Join(d.dataDir, cliBinaryName))
	}
	if d.bootSrc != "" {
		if d.needRestart {
			if err := d.activateBootHook(d.bootSrc, d.initScript); err != nil {
				return d.rollback(activated, err)
			}
			fmt.Printf("installed boot hook %s\n", d.initScript)
		} else {
			fmt.Println("boot hook not installed because -restart never")
		}
	}
	if d.needRestart {
		if err := restartViaBootService(d.initScript, d.dataDir); err != nil {
			return d.rollback(activated, fmt.Errorf("restart after deployment: %w", err))
		}
		fmt.Printf("restarted through %s restart\n", d.initScript)
		if err := waitDaemonHealthy(d.daemonPath(), d.socket, d.timeout); err != nil {
			return d.rollback(activated, err)
		}
		fmt.Println("daemon healthy")
	} else if len(d.packages) > 0 || d.libSrc != "" {
		fmt.Println("restart skipped: restart capsulad to activate the installed packages")
	}
	// The deployment is accepted: the transient replacements are no longer
	// needed for recovery.
	d.accept(activated)
	return nil
}

// accept drops the transient .prev backups once the deployment passed the
// health gate. There is no durable backup: the previous files are only kept
// long enough to undo a failed deployment.
func (d *deployment) accept(activated []activation) {
	for _, a := range activated {
		_ = os.Remove(a.previous)
	}
	if d.hook != nil {
		_ = os.Remove(d.hook.previous)
	}
}

// stopReplacedApps stops live runs of apps this bundle replaces. Replacing a
// package under a running process would leave the old binary mapped and the
// new version undetectable until the daemon restarts. An unreachable daemon is
// reported once and the deployment continues: package management also works
// offline.
func (d *deployment) stopReplacedApps() error {
	if len(d.packages) == 0 {
		return nil
	}
	replaced := make(map[string]bool, len(d.packages))
	for _, pkg := range d.packages {
		replaced[pkg.Manifest.ID] = true
	}
	conn, err := dial(d.socket)
	if err != nil {
		return nil
	}
	defer conn.Close()
	client := frameworkv1.NewApplicationServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	runs, err := client.ListRuns(ctx, &frameworkv1.ListRunsRequest{})
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "deploy: daemon is not reachable (%v); cannot stop running apps\n", err)
		return nil
	}
	var live []*frameworkv1.AppRun
	for _, run := range runs.GetRuns() {
		if replaced[run.AppId] {
			live = append(live, run)
		}
	}
	for _, run := range live {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := client.Stop(ctx, &frameworkv1.StopRequest{RunId: run.RunId})
		cancel()
		if err != nil {
			return fmt.Errorf("stop app %s (run %s) before replacing it: %w", run.AppId, run.RunId, err)
		}
		fmt.Printf("stopped app %s (run %s)\n", run.AppId, run.RunId)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		runs, err := client.ListRuns(ctx, &frameworkv1.ListRunsRequest{})
		cancel()
		if err == nil {
			remaining := ""
			for _, run := range runs.GetRuns() {
				if replaced[run.AppId] {
					remaining = fmt.Sprintf("%s (run %s)", run.AppId, run.RunId)
					break
				}
			}
			if remaining == "" {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("app %s is still running after a stop request; stop it manually before deploying", remaining)
			}
		} else if time.Now().After(deadline) {
			return fmt.Errorf("could not verify that replaced apps stopped: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

type activation struct {
	target    string
	previous  string
	hadBefore bool
}

// activateBinaries renames each staged binary into place, keeping the replaced
// file as a transient .prev until the deployment is accepted.
func (d *deployment) activateBinaries() ([]activation, error) {
	items := []struct {
		name   string
		staged string
	}{
		{daemonBinaryName, d.daemonStaged},
		{cliBinaryName, d.cliStaged},
	}
	var activated []activation
	for _, it := range items {
		if it.staged == "" {
			continue
		}
		target := filepath.Join(d.dataDir, it.name)
		a := activation{target: target, previous: target + ".prev"}
		if _, err := os.Stat(target); err == nil {
			a.hadBefore = true
			_ = os.Remove(a.previous)
			if err := os.Rename(target, a.previous); err != nil {
				restoreActivations(activated)
				return nil, fmt.Errorf("preserve %s: %w", target, err)
			}
		}
		if err := os.Rename(it.staged, target); err != nil {
			if a.hadBefore {
				_ = os.Rename(a.previous, target)
			}
			restoreActivations(activated)
			return nil, fmt.Errorf("activate %s: %w", target, err)
		}
		activated = append(activated, a)
	}
	return activated, nil
}

func restoreActivations(activated []activation) {
	for i := len(activated) - 1; i >= 0; i-- {
		a := activated[i]
		_ = os.Remove(a.target)
		if a.hadBefore {
			_ = os.Rename(a.previous, a.target)
		}
	}
}

// rollback restores the binaries and boot hook that were replaced and restarts
// the previous stack through the same boot service. It reports the original
// failure and, separately, any step that could not be undone.
func (d *deployment) rollback(activated []activation, cause error) error {
	var problems []string
	for i := len(activated) - 1; i >= 0; i-- {
		a := activated[i]
		failed := a.target + ".failed"
		_ = os.Remove(failed)
		if err := os.Rename(a.target, failed); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", a.target, err))
			continue
		}
		if a.hadBefore {
			if err := os.Rename(a.previous, a.target); err != nil {
				problems = append(problems, fmt.Sprintf("restore %s: %v", a.target, err))
				_ = os.Rename(failed, a.target)
				continue
			}
		}
		_ = os.Remove(failed)
	}
	if d.hook != nil {
		// The hook is restored before the restart below: a syntactically
		// valid but runtime-broken hook must not drive the rollback restart.
		if err := os.Remove(d.hook.target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("remove failed boot hook %s: %v", d.hook.target, err))
		} else if d.hook.hadBefore {
			if err := os.Rename(d.hook.previous, d.hook.target); err != nil {
				problems = append(problems, fmt.Sprintf("restore boot hook %s: %v", d.hook.target, err))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w; rollback incomplete: %s", cause, strings.Join(problems, "; "))
	}
	if d.needRestart {
		if err := restartViaBootService(d.initScript, d.dataDir); err != nil {
			return fmt.Errorf("%w; previous binaries restored but restart failed: %v", cause, err)
		}
		if err := waitDaemonHealthy(d.daemonPath(), d.socket, d.timeout); err != nil {
			return fmt.Errorf("%w; previous binaries restored but the daemon is not healthy: %v", cause, err)
		}
	}
	return fmt.Errorf("deployment failed; previous binaries restored: %w", cause)
}

// syncLib replaces the managed runtime files, leaving unrelated operator files
// in place. Each file is written to a temporary name and renamed so the
// loader never observes a partial shared library.
func syncLib(src, dst string) error {
	if err := prepareRuntimeDir(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if isAppleDouble(entry.Name()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return prepareRuntimeDir(target)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := copyFileAtomic(p, target, mode); err != nil {
			return fmt.Errorf("native runtime file %s: %w", filepath.ToSlash(rel), err)
		}
		return nil
	})
}

// prepareRuntimeDir creates dir when missing and otherwise requires a real
// directory. The managed lib tree is written into <data-dir>, where a symlink
// planted at "lib" or below would redirect the deployment outside its own
// directory.
func prepareRuntimeDir(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("native runtime path %s is a symlink; refusing to write through it", dir)
	case err == nil && !info.IsDir():
		return fmt.Errorf("native runtime path %s is not a directory", dir)
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	info, err = os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("native runtime path %s is not a directory", dir)
	}
	return nil
}

// activateBootHook installs the bundle's hook, keeping the replaced script as
// <init-script>.prev until the deployment passes the health gate. The previous
// hook must be available for the rollback restart: a hook that passes `sh -n`
// can still fail at runtime, and an unrestorable hook would then block
// recovery.
func (d *deployment) activateBootHook(src, dst string) error {
	a := activation{target: dst, previous: dst + ".prev"}
	if _, err := os.Lstat(dst); err == nil {
		a.hadBefore = true
		_ = os.Remove(a.previous)
		if err := os.Rename(dst, a.previous); err != nil {
			return fmt.Errorf("preserve boot hook %s: %w", dst, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := copyFileAtomic(src, dst, 0o755); err != nil {
		if a.hadBefore {
			_ = os.Rename(a.previous, dst)
		}
		return fmt.Errorf("install boot hook %s: %w", dst, err)
	}
	d.hook = &a
	return nil
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".deploy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func validateShellScript(path string) error {
	out, err := exec.Command("sh", "-n", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("boot hook %s is not a valid POSIX shell script: %v (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func requireBootService(script string) error {
	info, err := os.Stat(script)
	if err != nil {
		return fmt.Errorf("boot service %s is missing; deploy it before restarting the daemon", script)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("boot service %s is not executable", script)
	}
	return nil
}

// verifyDaemonBinary proves the file executes here and reports itself as
// capsulad. A broken or wrong-architecture binary fails before activation.
// The probe runs with a minimal environment: the bundle binary executes as
// root, so the operator's environment (credentials, CAPSULA_* overrides,
// language state) must not reach it.
func verifyDaemonBinary(path string) error {
	env, cleanup := probeEnv()
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("%s does not run: %v (%s)", path, err, text)
	}
	if !strings.HasPrefix(text, "capsulad ") {
		return fmt.Errorf("%s is not a capsulad binary: %q", path, text)
	}
	return nil
}

// probeEnv builds the environment for executing a bundle-provided binary.
// HOME points at an empty temporary directory so the probe cannot read or
// write operator configuration.
func probeEnv() ([]string, func()) {
	home, err := os.MkdirTemp("", "capsula-probe-")
	cleanup := func() {}
	if err == nil {
		cleanup = func() { _ = os.RemoveAll(home) }
	} else {
		home = os.TempDir()
	}
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + home,
		"TMPDIR=" + os.TempDir(),
		"LANG=C",
	}, cleanup
}

func copyExecutable(src, dst string) error {
	return copyFileAtomic(src, dst, 0o755)
}

func restartViaBootService(script, dataDir string) error {
	cmd := exec.Command(script, "restart")
	// Stock hooks fix their own data directory; exporting the chosen one keeps
	// a non-stock hook usable without a second flag vocabulary.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CAPSULA_DATA_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "CAPSULA_DATA_DIR="+dataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s restart: %v (%s)", script, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitDaemonHealthy gates on the installed daemon's -health probe, the same
// check the boot service uses. A binary that predates -health (exit 2) or
// cannot be executed falls back to the framework API probe.
func waitDaemonHealthy(binary, socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	useAPI := false
	if _, err := os.Stat(binary); err != nil {
		useAPI = true
	}
	var last error
	for {
		if !useAPI {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, err := exec.CommandContext(ctx, binary, "-health", "-socket", socket).CombinedOutput()
			cancel()
			if err == nil {
				return nil
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
				useAPI = true
			} else {
				last = fmt.Errorf("%s -health: %v (%s)", binary, err, strings.TrimSpace(string(out)))
				if time.Now().After(deadline) {
					return fmt.Errorf("daemon did not become healthy within %s: %w", timeout, last)
				}
				time.Sleep(250 * time.Millisecond)
				continue
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := probeDaemon(ctx, socket)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not become healthy within %s: %w", timeout, last)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func probeDaemon(ctx context.Context, socket string) error {
	conn, err := grpc.NewClient("passthrough:///capsulad",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = frameworkv1.NewSystemServiceClient(conn).GetStatus(ctx, &frameworkv1.GetStatusRequest{})
	return err
}

// liveRun returns a supervised run ID for appID. An unreachable daemon reports
// no runs: package administration still works offline.
func liveRun(dataDir, socket, appID string) string {
	conn, err := dial(resolveSocket(dataDir, socket))
	if err != nil {
		return ""
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runs, err := frameworkv1.NewApplicationServiceClient(conn).ListRuns(ctx, &frameworkv1.ListRunsRequest{AppId: appID})
	if err != nil {
		return ""
	}
	for _, run := range runs.GetRuns() {
		if run.AppId == appID {
			return run.RunId
		}
	}
	return ""
}
