package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/artifact"
	"capsulad/internal/backend/profile"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/registry"
	"capsulad/internal/sandbox"
)

// The deployment contract (boot service, bundle limits and free-space floors)
// comes from the device profile.
var (
	deployProfile      = profile.RK3308Rev4Stock.Deploy
	deployArchitecture = profile.RK3308Rev4Stock.Architecture

	defaultInitScript = deployProfile.InitScript
	bootHookName      = deployProfile.BootHookName

	// The detached watchdog feeder publishes its pid under run/ and watches a
	// handback marker there. A pid file that names the shell feeder means the
	// old daemon binary is not pinned by that process.
	daemonPIDName     = "capsulad.pid"
	watchdogFeederPID = "watchdog.feeder"
	watchdogHandback  = "watchdog.handback"
	shellFeederMarker = "shell"

	// feederArgvMarker and legacyFeederArgvMarker identify a feeder in its
	// command line: the current shell feeder's argv[0] and the hidden
	// subcommand of the Go feeder shipped before it. A pid file whose pid runs
	// neither is stale, not a pinned inode.
	feederArgvMarker       = "capsulad-watchdog-feeder"
	legacyFeederArgvMarker = "__capsulad_watchdog_feeder"

	daemonBinaryName = "capsulad"
	cliBinaryName    = "capsulad-cli"

	// extractionRatio estimates the unpacked size of a bundle archive as a
	// multiple of its compressed size. Deploy bundles are Go binaries, which
	// compress to roughly half their size.
	extractionRatio = deployProfile.ExtractionRatio

	// stagingReserve is free space kept available on the staging filesystem
	// besides the extracted bundle: the compressed backup of the replaced
	// daemon is ~24.7 MiB for the profile's daemon binary, so the reserve is
	// that plus a margin.
	stagingReserve = deployProfile.StagingReserveBytes

	// dataReserve is free space kept on the data filesystem besides the binary
	// writes themselves, for the package staging directories and lib/.
	dataReserve = deployProfile.DataReserveBytes
)

// bundleLimits bound a deployment bundle supplied by an operator, possibly
// from external media. The archive is unpacked into the staging tree, which
// lives outside the data partition.
var bundleLimits = artifact.Limits{
	MaxFileBytes:  int64(deployProfile.BundleMaxFileBytes),
	MaxTotalBytes: int64(deployProfile.BundleMaxTotalBytes),
	MaxEntries:    deployProfile.BundleMaxEntries,
}

// deployProfiles maps a -platform selection to its deployment contract. The
// bundle format is identical on every target; only the boot service, the hook
// name and the free-space floors differ.
var deployProfiles = map[string]profile.Profile{
	"rockchip-stock": profile.RK3308Rev4Stock,
	"rockchip":       profile.RK3308Rev4Stock,
	"amlogic-stock":  profile.AmlogicA113XStock,
	"amlogic-alpine": profile.AmlogicA113XAlpine,
}

// selectDeployProfile points the package-level deployment contract at one
// target. It is called once per deployment from runDeploy, so a process that
// deploys one bundle always works from one profile.
func selectDeployProfile(platform string) (profile.Profile, error) {
	p, ok := deployProfiles[platform]
	if !ok {
		return profile.Profile{}, fmt.Errorf("-platform must be rockchip-stock, amlogic-stock or amlogic-alpine, got %q", platform)
	}
	dep := p.Deploy
	deployProfile = dep
	defaultInitScript = dep.InitScript
	bootHookName = dep.BootHookName
	extractionRatio = dep.ExtractionRatio
	stagingReserve = dep.StagingReserveBytes
	dataReserve = dep.DataReserveBytes
	bundleLimits = artifact.Limits{
		MaxFileBytes:  int64(dep.BundleMaxFileBytes),
		MaxTotalBytes: int64(dep.BundleMaxTotalBytes),
		MaxEntries:    dep.BundleMaxEntries,
	}
	deployArchitecture = p.Architecture
	return p, nil
}

// platformFlag reads -platform from the command line before the flag set exists,
// because the flag's value decides the default boot service the other flags are
// created with. Both the `-platform X` and `-platform=X` forms are accepted,
// and a missing value is a parse error the flag set reports later.
func platformFlag(args []string) string {
	platform := "rockchip-stock"
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-platform" || args[i] == "--platform":
			if i+1 < len(args) {
				platform = args[i+1]
			}
		case strings.HasPrefix(args[i], "-platform="):
			platform = strings.TrimPrefix(args[i], "-platform=")
		case strings.HasPrefix(args[i], "--platform="):
			platform = strings.TrimPrefix(args[i], "--platform=")
		}
	}
	return platform
}

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
	// stopped records that the daemon was stopped through the boot service and
	// has not been restarted yet: every failure after that point must restart
	// the previous stack before returning.
	stopped bool
	// keepStaging preserves the staging tree after a failure, because it holds
	// the only remaining copy of a binary that could not be restored.
	keepStaging bool
	// hook is the replaced boot hook, kept as <init-script>.prev only until
	// the health gate accepts the deployment.
	hook *activation
}

// runDeploy applies a deployment bundle: validate, stop the daemon, install
// apps and the native runtime, replace the daemon and CLI binaries, install
// the boot hook, restart through the boot service and gate on a healthy daemon.
// Nothing is written before the bundle has been fully validated.
func runDeploy(args []string, dataDir, socket string) error {
	// The platform decides the boot service, the hook name and the space floors,
	// and it has to be known before -init-script gets its default value.
	platform := platformFlag(args)
	if _, err := selectDeployProfile(platform); err != nil {
		return err
	}
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	platformName := fs.String("platform", platform, "deployment target: rockchip-stock, amlogic-stock or amlogic-alpine")
	initScript := fs.String("init-script", defaultInitScript, "boot service invoked to stop and restart the daemon")
	restartMode := fs.String("restart", "auto", "restart policy: auto, always or never")
	dryRun := fs.Bool("dry-run", false, "print the plan without changing installed state")
	stagingDir := fs.String("staging", "", "staging parent directory (default: the operating system temporary directory)")
	timeout := fs.Duration("timeout", 45*time.Second, "time to wait for a healthy daemon after restart")
	fs.Usage = func() {
		dataPartition := "the data partition"
		if deployProfile.DataPartitionMiB > 0 {
			dataPartition = fmt.Sprintf("the %.1f MiB data partition", deployProfile.DataPartitionMiB)
		}
		fmt.Fprintf(fs.Output(), `usage: capsulad-cli deploy PATH [-platform rockchip-stock|amlogic-stock|amlogic-alpine] [-init-script PATH] [-restart auto|always|never] [-dry-run] [-staging DIR] [-timeout DURATION]

PATH is a bundle directory or a .tar.gz containing:
  capsulad              daemon binary (installed as <data-dir>/capsulad, 0755)
  capsulad-cli          optional CLI binary (present in the default bundle, omitted
                        by BUNDLE_CLI=0; installed as <data-dir>/capsulad-cli, 0755)
  lib/                  shared native runtime and headers (replaces managed files)
  apps/*.tar.gz         application packages (installed or atomically updated)
  boot/%s       boot hook (installed as -init-script when present)

Only those top-level entries are meaningful; anything else is reported and
ignored. The bundle is staged under -staging (default: the temporary
directory), never inside <data-dir>: %s cannot hold a
second copy of the daemon while the installed one is still there. Nothing is
written outside <data-dir> except the boot hook bundled at boot/%s.

Flags:
`, bootHookName, dataPartition, bootHookName)
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
		return fmt.Errorf("usage: capsulad-cli deploy PATH [-platform rockchip-stock|amlogic-stock|amlogic-alpine] [-init-script PATH] [-restart auto|always|never] [-dry-run] [-staging DIR] [-timeout DURATION]")
	}
	// The parsed value is authoritative; the pre-scan only seeded the defaults.
	if _, err := selectDeployProfile(*platformName); err != nil {
		return err
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
	// Staging lives outside the data directory. An archive has to be unpacked
	// to be inspected, the replaced daemon is backed up compressed beside it,
	// and neither may compete for space with the installed daemon.
	staging, err := createStaging(*stagingDir)
	if err != nil {
		return err
	}
	d.staging = staging
	defer func() {
		if d.keepStaging {
			fmt.Fprintf(os.Stderr, "deploy: staging kept at %s\n", d.staging)
			return
		}
		_ = os.RemoveAll(d.staging)
	}()
	// Both sides are resolved first, so a symlink or a bind mount cannot hide a
	// shared partition.
	stagingReal := resolvePath(d.staging)
	dataReal := resolvePath(d.dataDir)
	if insideDir(dataReal, stagingReal) {
		return fmt.Errorf("-staging %s is inside the data directory %s; stage on another filesystem", d.staging, d.dataDir)
	}
	if sameFilesystem(stagingReal, dataReal) {
		// Staging on the data filesystem recreates the space shortage this
		// command exists to avoid, so it is only allowed when that filesystem
		// can hold the largest possible staged tree beside both reserves. A
		// host or simulator data directory on one big volume qualifies; the
		// Capsula Mini's 78.5 MiB /data never does, and /tmp is a separate
		// tmpfs there anyway.
		need := uint64(bundleLimits.MaxTotalBytes) + stagingReserve + dataReserve
		free, known, err := freeSpace(dataReal)
		if err == nil && known && free < need {
			return fmt.Errorf("-staging %s is on the data filesystem %s with %s free; staging there needs %s beside the installed daemon, so point -staging at another filesystem",
				d.staging, d.dataDir, mib(free), mib(need))
		}
	}

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

// createStaging makes a private staging directory below parent, or below the
// operating system temporary directory when parent is empty.
func createStaging(parent string) (string, error) {
	if parent == "" {
		parent = os.TempDir()
	}
	abs, err := filepath.Abs(parent)
	if err != nil {
		return "", fmt.Errorf("staging directory %s: %w", parent, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return "", fmt.Errorf("staging directory %s: %w", abs, err)
	}
	staging, err := os.MkdirTemp(abs, "capsulad-deploy-")
	if err != nil {
		return "", fmt.Errorf("staging directory %s: %w", abs, err)
	}
	return staging, nil
}

// resolvePath returns path with symlinks resolved. A path that does not exist
// yet is returned unchanged: the caller only needs the best available answer.
func resolvePath(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return real
}

func (d *deployment) daemonPath() string { return filepath.Join(d.dataDir, daemonBinaryName) }

func (d *deployment) appPath(appID string) string { return filepath.Join(d.dataDir, "apps", appID) }

func (d *deployment) prepare() error {
	root, archive, err := unpackBundle(d.bundlePath, d.staging)
	if err != nil {
		return err
	}
	d.bundleRoot, d.archive = root, archive
	if err := d.scanBundle(); err != nil {
		return err
	}
	if err := d.checkBundleELF(); err != nil {
		return err
	}
	// The bundle (or the tree extracted from it) is already in staging, and a
	// directory bundle is read in place. Copying the daemon again would need a
	// second 40 MiB in staging for no benefit: activation installs this file
	// without ever holding two copies of it.
	if d.daemonSrc != "" {
		d.daemonStaged = d.daemonSrc
		// A dry run never executes bundle content: the incoming daemon is
		// untrusted until it is installed and probed after activation.
		if !d.dryRun {
			if err := ensureExecutable(d.daemonStaged); err != nil {
				return err
			}
			if err := verifyDaemonBinary(d.daemonStaged); err != nil {
				return err
			}
		}
	}
	if d.cliSrc != "" {
		d.cliStaged = d.cliSrc
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

// checkBundleELF validates every native artifact this deployment would install:
// the lib/ tree (runtime and its dependencies), the daemon and CLI binaries, and
// every file inside a staged app package. It runs before the daemon is stopped,
// so a wrong-libc, wrong-glibc or non-ELF artifact is refused with the old stack
// still running. Only bundle content is scanned, never device state.
func (d *deployment) checkBundleELF() error {
	want := sandbox.LibcForABI(deployArchitecture.ABIContains)
	maxGLIBC := deployArchitecture.MaxGLIBC
	if d.libSrc != "" {
		if err := checkELFTree(d.libSrc, want, maxGLIBC, deployArchitecture.Target, true, ""); err != nil {
			return err
		}
	}
	for _, pkg := range d.packages {
		if pkg.Dir == "" {
			continue
		}
		// An entrypoint that needs the shared runtime must be dynamic on musl.
		needDynamic := ""
		if pkg.Manifest.NativeSDK != nil && want == sandbox.LibcMusl && pkg.Manifest.Entrypoint.Path != "" {
			needDynamic = filepath.Join(pkg.Dir, pkg.Manifest.Entrypoint.Path)
		}
		if err := checkELFTree(pkg.Dir, want, maxGLIBC, deployArchitecture.Target, false, needDynamic); err != nil {
			return fmt.Errorf("app %s: %w", pkg.Manifest.ID, err)
		}
	}
	for _, bin := range []string{d.daemonSrc, d.cliSrc} {
		if bin == "" {
			continue
		}
		ok, err := sandbox.IsELF(bin)
		if err != nil {
			return err
		}
		if !ok {
			// The daemon/CLI probe executes this file before activation, so a
			// host binary is caught there; only ELF artifacts get ELF checks.
			continue
		}
		if err := checkELFFile(bin, want, maxGLIBC, deployArchitecture.Target, false, true, false, false); err != nil {
			return err
		}
	}
	return nil
}

// checkELFTree walks one bundle subtree. Only regular files are considered; a
// shared library that is not an ELF file is an error (that is how a Darwin build
// would otherwise pass), while a script with a shebang is skipped. When
// requireLibc is true, a dynamically linked library must match the target libc.
func checkELFTree(root string, want sandbox.LibcClass, maxGLIBC, targetArch string, requireRuntime bool, needDynamic string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			base, err := filepath.EvalSymlinks(root)
			if err != nil {
				return err
			}
			if !insideDir(base, resolved) {
				return fmt.Errorf("bundle entry %s resolves outside the bundle (%s)", path, root)
			}
			path = resolved
			if info, err = os.Stat(path); err != nil {
				return err
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		name := entry.Name()
		isLib := strings.HasPrefix(name, "lib") && (strings.Contains(name, ".so") || strings.HasSuffix(name, ".dylib"))
		isRuntime := isLib && strings.HasPrefix(name, "libcapsulad.so.")
		if ok, err := sandbox.IsELF(path); err != nil {
			return err
		} else if !ok {
			if isLib {
				return fmt.Errorf("%s is not an ELF file; a %s target cannot load it", path, want)
			}
			// Executable with a shebang is a script, not a native artifact.
			if info.Mode()&0o111 != 0 {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				var magic [2]byte
				_, _ = io.ReadFull(f, magic[:])
				f.Close()
				if string(magic[:]) == "#!" {
					return nil
				}
				return fmt.Errorf("%s is executable but neither ELF nor a script", path)
			}
			return nil
		}
		exec := info.Mode()&0o111 != 0 && !isLib
		strict := isRuntime || (requireRuntime && isLib)
		required := needDynamic != "" && path == needDynamic
		return checkELFFile(path, want, maxGLIBC, targetArch, isLib, exec, strict, required)
	})
}

// checkELFFile applies the libc and glibc ceilings. A statically linked binary
// has no libc dependency and is accepted; a shared object is required to name
// the target libc.
// checkELFFile applies architecture, libc, interpreter and glibc ceilings to one
// artifact. Executables must have an interpreter matching the target libc when
// they are dynamic (static passes); shared objects may be self-contained, but
// the installed runtime must be dynamically linked.
func checkELFFile(path string, want sandbox.LibcClass, maxGLIBC, targetArch string, isLib, exec, strictRuntime, needDynamic bool) error {
	if err := sandbox.CheckMachine(path, targetArch); err != nil {
		return err
	}
	if isLib {
		if strictRuntime {
			if err := sandbox.CheckLibcRuntime(path, want); err != nil {
				return err
			}
		} else if err := sandbox.CheckLibc(path, want); err != nil {
			return err
		}
	}
	if exec {
		if needDynamic {
			if err := sandbox.CheckInterpRequired(path, want); err != nil {
				return err
			}
		} else if err := sandbox.CheckInterp(path, want); err != nil {
			return err
		}
	}
	if want == sandbox.LibcGNU && maxGLIBC != "" {
		return sandbox.CheckGLIBC(path, maxGLIBC)
	}
	return nil
}

// unpackBundle accepts a directory or a .tar.gz bundle and returns its root.
func unpackBundle(path, staging string) (string, bool, error) {
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
	// Only an archive consumes staging space: a directory bundle is inspected
	// in place.
	if err := checkExtractionSpace(staging, info.Size()); err != nil {
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

// checkExtractionSpace refuses to unpack a bundle archive when the staging
// filesystem cannot hold the archive's extracted tree plus the reserve the
// in-flight deployment needs. When free space cannot be determined (see
// freeSpaceBytes) the preflight is skipped; extraction still fails safely and
// the staging tree is removed.
func checkExtractionSpace(dir string, archiveBytes int64) error {
	free, known, err := freeSpace(dir)
	if err != nil || !known {
		return nil
	}
	extracted := uint64(0)
	if archiveBytes > 0 {
		extracted = uint64(archiveBytes) * uint64(extractionRatio)
	}
	// Only the extracted tree and the compressed backup live in staging; the
	// operator's archive stays where it was.
	need := extracted + stagingReserve
	if free < need {
		return fmt.Errorf("%s has %s free space; unpacking this bundle needs about %s for the extracted tree plus a %s reserve for the compressed backup, so point -staging at a larger filesystem and retry",
			dir, mib(free), mib(extracted), mib(stagingReserve))
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
		locate := sandbox.LocateRuntime
		if d.libSrc != "" {
			// A bundle carries the library as one regular file and gains its
			// ABI links during installation, so the installed layout that
			// LocateRuntime resolves does not exist yet.
			locate = bundleRuntime
		}
		rt, err := locate(nativeDir)
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
	fmt.Fprintf(w, "staging: %s\n", d.staging)
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
		fmt.Fprintf(w, "schedule: %s stop, install, %s restart\n", d.initScript, d.initScript)
		fmt.Fprintf(w, "health: %s -health -socket %s (up to %s)\n", d.daemonPath(), d.socket, d.timeout)
	} else if d.restart == "never" {
		fmt.Fprintln(w, "restart: skipped (-restart never)")
	} else {
		fmt.Fprintln(w, "restart: not required (no daemon, lib or app change)")
	}
	fmt.Fprintln(w, "trust: bundle contents are applied as supplied; verifying their origin is the operator's decision")
}

// apply installs the bundle while the daemon is stopped: the boot service is
// asked to stop it first, packages and the native runtime are installed with
// nothing running, the binaries and boot hook are replaced, and the daemon is
// started again through the same service. A restart is therefore never
// observed by a live daemon, and a selected provider app is replaced without a
// stop-for-restart round trip.
func (d *deployment) apply() error {
	// Refuse before the stop: once the daemon is down, a data partition that
	// cannot take the new binaries has no safe way back.
	if err := d.checkDataSpace(); err != nil {
		return err
	}
	if d.needRestart {
		if err := stopViaBootService(d.initScript, d.dataDir); err != nil {
			return err
		}
		// The hook blocks until the process is gone, but verify it: installing
		// packages under a live daemon is the failure the online path avoids.
		// Nothing has been changed yet, so no rollback is needed — but the
		// daemon may well be stopped now, and the device must not be left
		// without one.
		if err := waitDaemonStoppedFn(d.socket, filepath.Join(d.dataDir, "run", daemonPIDName), 2*time.Second); err != nil {
			if rerr := restartViaBootService(d.initScript, d.dataDir); rerr != nil {
				return fmt.Errorf("%w; the deployment changed nothing and restarting the previous stack also failed: %v", err, rerr)
			}
			return fmt.Errorf("%w; the deployment changed nothing and the previous stack was restarted", err)
		}
		d.stopped = true
		fmt.Printf("stopped the daemon through %s stop\n", d.initScript)
	} else if err := d.stopReplacedApps(); err != nil {
		// -restart never keeps the daemon running, so live runs of a replaced
		// package must be stopped before the package tree changes.
		return err
	}
	var activated []activation
	// fail returns the deployment to a runnable state before reporting. After
	// the stop, that means restoring whatever was replaced and restarting the
	// previous stack; before an activation there is nothing to restore but the
	// daemon is still stopped, so the previous binaries must be restarted.
	fail := func(cause error) error {
		if d.stopped {
			return d.rollback(activated, cause)
		}
		if problems := restoreActivations(activated); len(problems) > 0 {
			return fmt.Errorf("%w; restoring the replaced files failed: %s", cause, strings.Join(problems, "; "))
		}
		return cause
	}
	root := filepath.Join(d.dataDir, "apps")
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		return fail(err)
	}
	for _, pkg := range d.packages {
		installed := false
		if _, err := os.Stat(d.appPath(pkg.Manifest.ID)); err == nil {
			installed = true
		}
		app, err := reg.InstallPackage(pkg)
		if err != nil {
			return fail(fmt.Errorf("install app %s: %w", pkg.Manifest.ID, err))
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
			return fail(err)
		}
		fmt.Printf("updated native runtime in %s\n", libDst)
	}
	if d.bootSrc != "" {
		if d.needRestart {
			// A tiny direct copy, and rollback restores it: install it before
			// the binaries so the post-handback window holds only the daemon.
			if err := d.activateBootHook(d.bootSrc, d.initScript); err != nil {
				return fail(err)
			}
			fmt.Printf("installed boot hook %s\n", d.initScript)
		} else {
			fmt.Println("boot hook not installed because -restart never")
		}
	}
	activated, err = d.activateBinaries()
	if err != nil {
		return fail(err)
	}
	if d.cliStaged != "" {
		fmt.Printf("installed %s\n", filepath.Join(d.dataDir, cliBinaryName))
	}
	if d.daemonStaged != "" {
		fmt.Printf("installed daemon %s\n", d.daemonPath())
	}
	if d.needRestart {
		if err := restartViaBootService(d.initScript, d.dataDir); err != nil {
			return fail(fmt.Errorf("restart after deployment: %w", err))
		}
		fmt.Printf("restarted through %s restart\n", d.initScript)
		if err := waitDaemonHealthy(d.daemonPath(), d.socket, d.timeout); err != nil {
			return fail(err)
		}
		fmt.Println("daemon healthy")
	} else if d.restart == "never" && (d.daemonStaged != "" || d.cliStaged != "" || len(d.packages) > 0 || d.libSrc != "") {
		fmt.Println("restart skipped (-restart never): the installed changes take effect when capsulad restarts")
	} else if d.cliStaged != "" && d.daemonStaged == "" && len(d.packages) == 0 && d.libSrc == "" {
		fmt.Println("CLI installed; no daemon restart is required")
	}
	// The deployment is accepted: the transient replacements are no longer
	// needed for recovery.
	d.accept(activated)
	return nil
}

// accept drops the transient backups once the deployment passed the health
// gate. There is no durable backup: the previous files are only kept long
// enough to undo a failed deployment.
func (d *deployment) accept(activated []activation) {
	for _, a := range activated {
		a.discard()
	}
	if d.hook != nil {
		d.hook.discard()
	}
}

// checkDataSpace refuses to start when the data filesystem cannot absorb the
// binary writes that do not free space of their own: the replacement is written
// on the same partition as the file it replaces, and a mapped executable keeps
// its blocks after the directory entry is gone.
func (d *deployment) checkDataSpace() error {
	need := uint64(dataReserve)
	// The running executable is the installed CLI when the operator invoked it
	// from the data directory; its inode stays mapped, so replacing it frees
	// nothing. The daemon is stopped (or refused) before it is written, so its
	// old blocks do come back.
	pinned := runningExecutable(filepath.Join(d.dataDir, cliBinaryName))
	if size, ok := d.pinnedWriteSize(d.cliStaged, filepath.Join(d.dataDir, cliBinaryName), pinned); ok {
		need += size
	}
	if size, ok := d.pinnedWriteSize(d.daemonStaged, d.daemonPath(), false); ok {
		need += size
	}
	if need == dataReserve {
		return nil
	}
	free, known, err := freeSpace(d.dataDir)
	if err != nil || !known {
		return nil
	}
	if free < need {
		return fmt.Errorf("%s has %s free space; replacing the binaries needs about %s plus a %s reserve, so free space on the data partition and retry",
			d.dataDir, mib(free), mib(need-dataReserve), mib(dataReserve))
	}
	return nil
}

// pinnedWriteSize reports the bytes a cross-filesystem binary write has to draw
// from the free space of the data filesystem. ok is false when the staged file
// is renamed into place on that filesystem and needs no space at all.
func (d *deployment) pinnedWriteSize(staged, target string, pinned bool) (uint64, bool) {
	if staged == "" || sameFilesystem(staged, d.dataDir) {
		return 0, false
	}
	info, err := os.Stat(staged)
	if err != nil {
		return 0, false
	}
	written := uint64(info.Size())
	released := uint64(0)
	if !pinned {
		if targetInfo, err := os.Stat(target); err == nil {
			released = uint64(targetInfo.Size())
		}
	}
	if written <= released {
		return 0, true
	}
	return written - released, true
}

// runningExecutable reports whether path is the executable of this process.
func runningExecutable(path string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return false
	}
	targetInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(selfInfo, targetInfo)
}

// daemonServing reports whether a daemon answers on the framework socket. It
// is a variable so tests can exercise the serving-daemon refusal without a
// framework server.
var daemonServing = daemonServingOn

func daemonServingOn(socket string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return probeDaemon(ctx, socket) == nil
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

// activationKind records how the replaced file is kept recoverable until the
// deployment is accepted.
type activationKind int

const (
	// activationRename keeps the replaced file as a sibling, which is free and
	// atomic when the staged file lives on the target filesystem.
	activationRename activationKind = iota
	// activationCompressed keeps a gzip copy in the staging tree, for a staged
	// file on another filesystem: the 78.5 MiB data partition cannot hold the
	// old and new 40 MiB daemon at once.
	activationCompressed
	// activationCopy keeps a plain copy beside the target, for the overlayfs
	// boot hook where the lower-layer file may not be renamed.
	activationCopy
)

// activation is one replaced file plus the backup that undoes it.
type activation struct {
	target    string
	previous  string
	kind      activationKind
	mode      os.FileMode
	hadBefore bool
}

// restore puts the replaced file back. It is used by both the mid-flight
// cleanup and the rollback path.
func (a activation) restore() error {
	if !a.hadBefore {
		if err := os.Remove(a.target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	switch a.kind {
	case activationCompressed:
		if err := decompressFile(a.previous, a.target, a.mode); err != nil {
			return err
		}
		return os.Remove(a.previous)
	case activationCopy:
		// Written in place, like the install: the overlayfs hook cannot be
		// replaced by renaming over it either.
		if err := copyFileInPlace(a.previous, a.target, a.mode); err != nil {
			return err
		}
		return os.Remove(a.previous)
	default:
		if err := os.Remove(a.target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Rename(a.previous, a.target)
	}
}

// discard drops the transient backup once the deployment passed the health
// gate.
func (a activation) discard() {
	if a.previous != "" {
		_ = os.Remove(a.previous)
	}
}

// activateBinaries puts each staged binary in place, keeping the replaced file
// recoverable until the deployment is accepted.
func (d *deployment) activateBinaries() ([]activation, error) {
	// The CLI is installed first: its space may have to be released and its
	// bytes copied, and none of that should happen between the daemon's
	// watchdog handback and its restart. The daemon goes last.
	items := []struct {
		name   string
		staged string
	}{
		{cliBinaryName, d.cliStaged},
		{daemonBinaryName, d.daemonStaged},
	}
	var activated []activation
	for _, it := range items {
		if it.staged == "" {
			continue
		}
		target := filepath.Join(d.dataDir, it.name)
		a, err := d.installBinary(it.staged, target, it.name == daemonBinaryName)
		if err != nil {
			if problems := restoreActivations(activated); len(problems) > 0 {
				return nil, fmt.Errorf("%w; restoring the files replaced before it failed: %s", err, strings.Join(problems, "; "))
			}
			return nil, err
		}
		activated = append(activated, a)
	}
	return activated, nil
}

// installBinary replaces one executable. A staged file on the target
// filesystem is renamed into place and the replaced file is kept as a sibling
// `.prev`, which costs nothing. A staged file elsewhere is copied: the replaced
// file is compressed into staging, the target is removed to release its space
// and the staged bytes are written directly to the target path, so the data
// partition never holds two copies of the daemon.
func (d *deployment) installBinary(staged, target string, isDaemon bool) (activation, error) {
	a := activation{target: target, mode: 0o755}
	info, err := os.Lstat(target)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return a, fmt.Errorf("%s is a symlink; refusing to replace it", target)
	case err == nil && !info.Mode().IsRegular():
		return a, fmt.Errorf("%s is not a regular file", target)
	case err == nil:
		a.hadBefore = true
	case !errors.Is(err, fs.ErrNotExist):
		return a, err
	}

	if sameFilesystem(staged, filepath.Dir(target)) {
		// A rename needs no space, so the handback can happen right before it.
		// A feeder that will not exit leaves the old inode pinned, which costs
		// space but does not block the rename.
		if err := d.releaseDaemonFeeder(isDaemon); err != nil {
			fmt.Fprintf(os.Stderr, "deploy: warning: %v\n", err)
		}
		a.kind = activationRename
		if a.hadBefore {
			a.previous = target + ".prev"
			_ = os.Remove(a.previous)
			if err := os.Rename(target, a.previous); err != nil {
				return a, fmt.Errorf("preserve %s: %w", target, err)
			}
		}
		if err := os.Rename(staged, target); err != nil {
			if a.hadBefore {
				_ = os.Rename(a.previous, target)
			}
			return a, fmt.Errorf("activate %s: %w", target, err)
		}
		if err := os.Chmod(target, a.mode); err != nil {
			return a, fmt.Errorf("set mode on %s: %w", target, err)
		}
		return a, nil
	}

	a.kind = activationCompressed
	if a.hadBefore {
		a.previous = filepath.Join(d.staging, filepath.Base(target)+".prev.gz")
		if err := compressFile(target, a.previous); err != nil {
			_ = os.Remove(a.previous)
			return a, fmt.Errorf("back up %s: %w", target, err)
		}
	}
	// The backup is compressed before the feeder hands the timer back: gzip of
	// the 40 MiB binary must happen while the feeder still feeds the armed
	// watchdog, not inside the ~21 s the deployment has to finish writing.
	if err := d.releaseDaemonFeeder(isDaemon); err != nil {
		// Nothing was removed: the target is intact and its space is still
		// pinned, so a direct write could not succeed anyway.
		if a.hadBefore {
			_ = os.Remove(a.previous)
		}
		return a, fmt.Errorf("replace %s: %w", target, err)
	}
	if a.hadBefore {
		if err := os.Remove(target); err != nil {
			_ = os.Remove(a.previous)
			return a, fmt.Errorf("release %s: %w", target, err)
		}
	}
	if err := copyFileInPlace(staged, target, a.mode); err != nil {
		if a.hadBefore {
			if rerr := a.restore(); rerr != nil {
				if insideDir(d.staging, a.previous) {
					d.keepStaging = true
				}
				return a, fmt.Errorf("write %s: %v; restoring the previous file also failed: %v (backup kept at %s)", target, err, rerr, a.previous)
			}
		}
		return a, fmt.Errorf("write %s: %w", target, err)
	}
	// A staged copy inside the staging tree is one-shot and staging is removed
	// at the end anyway; dropping it now keeps the temporary filesystem below
	// its limit. A directory bundle outside staging belongs to the operator and
	// is left alone.
	if insideDir(d.staging, staged) {
		if err := os.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "deploy: warning: could not remove staged %s: %v\n", staged, err)
		}
	}
	return a, nil
}

// insideDir reports whether path is dir itself or below it.
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// restoreActivations undoes a partially activated list and reports every file
// that could not be restored.
// releaseDaemonFeeder releases a legacy feeder's hold on the replaced daemon's
// inode, or warns that a running daemon keeps it mapped. It runs for the daemon
// only, and only after the backup has been compressed.
func (d *deployment) releaseDaemonFeeder(isDaemon bool) error {
	if !isDaemon {
		return nil
	}
	if d.stopped {
		return releaseLegacyFeederFn(d)
	}
	// This deployment did not stop the daemon. Removing a binary that a live
	// process still has mapped frees nothing, and the 40 MiB write would have
	// to fit in the space that is left: refuse instead of gambling.
	if daemonServing(d.socket) {
		return fmt.Errorf("the daemon still answers on %s; stop it (or use -restart auto) before replacing its binary", d.socket)
	}
	// Not serving: the inode is not mapped by a live daemon, so removing the
	// file does free its space. A feeder left by an older daemon may still pin
	// it, so the handback still applies.
	return releaseLegacyFeederFn(d)
}

// releaseLegacyFeederFn is the watchdog handback step. It is a variable so a
// test can assert that it runs after the backup compression.
var releaseLegacyFeederFn = func(d *deployment) error { return d.releaseLegacyFeeder() }

func restoreActivations(activated []activation) []string {
	var problems []string
	for i := len(activated) - 1; i >= 0; i-- {
		if err := activated[i].restore(); err != nil {
			problems = append(problems, fmt.Sprintf("restore %s: %v", activated[i].target, err))
		}
	}
	return problems
}

// sameFilesystem reports whether two paths live on one filesystem, where
// replacing a target is a free rename and a staged copy costs nothing. It is a
// variable so tests can exercise the cross-filesystem path without a second
// mount.
var sameFilesystem = sameFilesystemOnDisk

// watchdogHandbackWait bounds how long a deploy waits for a legacy feeder to
// exit. It is short because the armed timer the feeder keeps alive fires within
// about 21 s, and the binary still has to be copied. It is a variable so tests
// need not wait for a feeder that never appears.
var watchdogHandbackWait = 5 * time.Second

// releaseLegacyFeeder asks a feeder left behind by the previous daemon to give
// the hardware watchdog back and waits for it to exit. The legacy Go feeder is
// a copy of the old daemon process, so it pins that binary's inode on the data
// partition: the space only comes back once the process is gone. A shell feeder
// says so in its pid file and holds no daemon inode, so nothing is requested.
//
// The wait is bounded: the feeder keeps an armed timer alive with a ~21 s
// budget, and the caller still has to copy the new binary into place. A feeder
// that is still there afterwards is reported as an error: the target must not
// be removed while its inode is pinned.
func (d *deployment) releaseLegacyFeeder() error {
	runDir := filepath.Join(d.dataDir, "run")
	pidFile := filepath.Join(runDir, watchdogFeederPID)
	body, err := os.ReadFile(pidFile)
	if err != nil {
		return nil
	}
	if strings.Contains(string(body), shellFeederMarker) {
		return nil
	}
	if live, known := feederPIDLive(string(body)); known && !live {
		// The feeder was killed, the pid file survived a reboot, or the pid was
		// reused by another process. Nothing pins the replaced inode, so waiting
		// for a handback that can never arrive would abort a deployment for
		// nothing.
		_ = os.Remove(pidFile)
		_ = os.Remove(filepath.Join(runDir, watchdogHandback))
		return nil
	}
	handback := filepath.Join(runDir, watchdogHandback)
	if err := os.WriteFile(handback, []byte("handback"), 0o644); err != nil {
		return fmt.Errorf("ask the watchdog feeder to release %s: %w", pidFile, err)
	}
	// A stale marker would make the next daemon's feeder exit immediately.
	defer func() { _ = os.Remove(handback) }()
	deadline := time.Now().Add(watchdogHandbackWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the watchdog feeder in %s is still running after %s; the previous daemon's space is still pinned", pidFile, watchdogHandbackWait)
}

// feederPIDLive reports whether the pid recorded in a feeder's pid file still
// runs a capsulad feeder. A live pid that runs something else is a reused pid,
// not a pinned inode, so the file is stale all the same. known is false where
// the host exposes no procfs (the CLI is built for the host too); the caller
// then keeps waiting for the pid file to disappear instead of guessing.
func feederPIDLive(body string) (live, known bool) {
	if fi, err := os.Stat("/proc"); err != nil || !fi.IsDir() {
		return false, false
	}
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return false, false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return false, true
		}
		return false, false
	}
	argv := string(cmdline)
	return strings.Contains(argv, feederArgvMarker) || strings.Contains(argv, legacyFeederArgvMarker), true
}

// rollback restores the binaries and boot hook that were replaced and restarts
// the previous stack through the same boot service. It reports the original
// failure and, separately, any step that could not be undone.
func (d *deployment) rollback(activated []activation, cause error) error {
	var problems []string
	for i := len(activated) - 1; i >= 0; i-- {
		a := activated[i]
		if err := a.restore(); err != nil {
			problem := fmt.Sprintf("restore %s: %v", a.target, err)
			if a.hadBefore {
				problem += fmt.Sprintf(" (backup kept at %s)", a.previous)
				if insideDir(d.staging, a.previous) {
					// It is the only copy left: the staging tree must survive.
					d.keepStaging = true
				}
			}
			problems = append(problems, problem)
		}
	}
	if d.hook != nil {
		// The hook is restored before the restart below: a syntactically
		// valid but runtime-broken hook must not drive the rollback restart.
		if err := d.hook.restore(); err != nil {
			problems = append(problems, fmt.Sprintf("restore boot hook %s: %v", d.hook.target, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w; rollback incomplete: %s", cause, strings.Join(problems, "; "))
	}
	// A failure before any activation leaves the binaries alone, but packages
	// and lib/ are already installed, and the daemon is stopped and still has
	// to be started again.
	state := "previous binaries restored"
	if len(activated) == 0 && d.hook == nil {
		state = "no binary or boot hook was replaced, but packages and lib/ may have been updated"
	}
	if d.needRestart {
		if err := restartViaBootService(d.initScript, d.dataDir); err != nil {
			return fmt.Errorf("%w; %s but restart failed: %v", cause, state, err)
		}
		if err := waitDaemonHealthy(d.daemonPath(), d.socket, d.timeout); err != nil {
			return fmt.Errorf("%w; %s but the daemon is not healthy: %v", cause, state, err)
		}
	}
	return fmt.Errorf("deployment failed; %s: %w", state, cause)
}

// syncLib installs the managed runtime files, leaving unrelated operator files
// in place. Two rules keep a redeploy affordable on the 78.5 MiB data
// partition, which cannot hold a second copy of the runtime:
//
//   - a regular file whose installed copy is byte-identical is not rewritten,
//     so an unchanged 4 MiB library costs no free space at all;
//   - the loader's ABI layout is rebuilt as relative links from the metadata
//     that was just installed, because the bundle carries the library as one
//     regular file. libcapsulad.so -> libcapsulad.so.1 -> libcapsulad.so.1.0.0
//     therefore survives a redeploy instead of becoming three copies of it.
//
// Each file is written to a temporary name and renamed so the loader never
// observes a partial shared library.
func syncLib(src, dst string) error {
	if err := prepareRuntimeDir(dst); err != nil {
		return err
	}
	if err := filepath.WalkDir(src, func(p string, entry fs.DirEntry, err error) error {
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
		same, err := sameRuntimeFile(p, target)
		if err != nil {
			return fmt.Errorf("native runtime file %s: %w", filepath.ToSlash(rel), err)
		}
		if same {
			return nil
		}
		if err := copyFileAtomic(p, target, mode); err != nil {
			return fmt.Errorf("native runtime file %s: %w", filepath.ToSlash(rel), err)
		}
		return nil
	}); err != nil {
		return err
	}
	return syncRuntimeLayout(src, dst)
}

// sameRuntimeFile reports whether dst already holds src's bytes. Only a
// regular installed file can match: a symlink or a directory at the managed
// path is replaced through the temp-and-rename path, never written through.
func sameRuntimeFile(src, dst string) (bool, error) {
	installed, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !installed.Mode().IsRegular() {
		return false, nil
	}
	bundled, err := os.Lstat(src)
	if err != nil {
		return false, err
	}
	if !bundled.Mode().IsRegular() || bundled.Size() != installed.Size() {
		return false, nil
	}
	return sameContents(src, dst)
}

// sameContents compares two files in fixed 32 KiB chunks, so a 4 MiB library
// costs two buffers instead of two full copies in memory.
func sameContents(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	bufA := make([]byte, 32<<10)
	bufB := make([]byte, 32<<10)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if errA != nil && !errors.Is(errA, io.EOF) && !errors.Is(errA, io.ErrUnexpectedEOF) {
			return false, errA
		}
		if errB != nil && !errors.Is(errB, io.EOF) && !errors.Is(errB, io.ErrUnexpectedEOF) {
			return false, errB
		}
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if na < len(bufA) {
			return true, nil
		}
	}
}

// syncRuntimeLayout recreates the loader's ABI layout for the runtime that was
// just installed: the metadata names the library (libcapsulad.so.1, or
// libcapsulad.1.dylib on macOS), the installed regular files name the real one,
// and the two link names the loader and the linker expect are created beside
// them as relative links. A runtime without metadata, or one whose metadata
// names a library nothing installed, is left alone: a partial runtime is the
// launcher's business, not the deployer's.
func syncRuntimeLayout(src, dst string) error {
	md, err := readRuntimeMetadata(dst)
	if err != nil || md == nil || md.Library == "" {
		return err
	}
	library := md.Library
	if err := runtimeName(library); err != nil {
		return fmt.Errorf("native runtime metadata %s: %w", filepath.Join(dst, sandbox.RuntimeMetadataFile), err)
	}
	real, err := runtimeLibraryName(src, library)
	if err != nil || real == "" {
		return err
	}
	if real != library {
		if err := placeRuntimeLink(dst, library, real); err != nil {
			return fmt.Errorf("native runtime link %s: %w", library, err)
		}
	}
	if short := runtimeShortName(library); short != "" && short != library {
		if err := placeRuntimeLink(dst, short, library); err != nil {
			return fmt.Errorf("native runtime link %s: %w", short, err)
		}
	}
	return nil
}

// bundleRuntime describes the native runtime a bundle's lib/ provides once it
// is installed: the metadata names the library and the versioned regular file
// beside it is the real one, while the loader's ABI links are only created by
// syncLib.
func bundleRuntime(dir string) (*sandbox.Runtime, error) {
	md, err := readRuntimeMetadata(dir)
	if err != nil {
		return nil, err
	}
	if md == nil || md.Library == "" {
		return nil, fmt.Errorf("native SDK metadata %s is missing; install a native build that emits %s", filepath.Join(dir, sandbox.RuntimeMetadataFile), sandbox.RuntimeMetadataFile)
	}
	if err := runtimeName(md.Library); err != nil {
		return nil, fmt.Errorf("native SDK metadata %s: %w", filepath.Join(dir, sandbox.RuntimeMetadataFile), err)
	}
	if md.ABIMajor < 1 || md.ABIMinor < 0 {
		return nil, fmt.Errorf("native SDK metadata %s declares ABI %d.%d; rebuild the native SDK", filepath.Join(dir, sandbox.RuntimeMetadataFile), md.ABIMajor, md.ABIMinor)
	}
	real, err := runtimeLibraryName(dir, md.Library)
	if err != nil {
		return nil, err
	}
	if real == "" {
		return nil, fmt.Errorf("native SDK runtime library is missing from %s (expected the file %s names); install the native SDK runtime there", dir, md.Library)
	}
	return &sandbox.Runtime{Dir: dir, File: real, ABIMajor: md.ABIMajor, ABIMinor: md.ABIMinor, Metadata: true}, nil
}

// readRuntimeMetadata parses the metadata beside a bundled or installed
// library. A missing file returns nil: the deployment leaves such a partial
// runtime to the launcher to report.
func readRuntimeMetadata(dir string) (*sandbox.RuntimeInfo, error) {
	path := filepath.Join(dir, sandbox.RuntimeMetadataFile)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("native runtime metadata %s is a symlink; refusing to read it", path)
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var md sandbox.RuntimeInfo
	if err := json.Unmarshal(body, &md); err != nil {
		return nil, fmt.Errorf("native SDK metadata %s is malformed: %w; rebuild the native SDK", path, err)
	}
	return &md, nil
}

// runtimeLibraryName returns the regular file in the bundle that the
// metadata's library name stands for: the versioned file beside it
// (libcapsulad.so.1.0.0 for libcapsulad.so.1), or the name itself when the
// platform keeps the real file under it (libcapsulad.1.dylib). Only the bundle
// is consulted, so a stale installed library cannot make an incomplete bundle
// synthesize links to the wrong runtime. An empty result means the bundle has
// no matching regular library, which leaves the links alone.
func runtimeLibraryName(dir, library string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	prefix := library + "."
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && entry.Type().IsRegular() {
			return entry.Name(), nil
		}
	}
	info, err := os.Lstat(filepath.Join(dir, library))
	switch {
	case err == nil && info.Mode().IsRegular():
		return library, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	return "", nil
}

// runtimeShortName returns the linker name beside a versioned library:
// libcapsulad.so.1 -> libcapsulad.so, libcapsulad.1.dylib -> libcapsulad.dylib.
// A name without a numeric version has no second link.
func runtimeShortName(library string) string {
	if i := strings.Index(library, ".so."); i > 0 && isVersion(library[i+len(".so."):]) {
		return library[:i+len(".so")]
	}
	if stem, ok := strings.CutSuffix(library, ".dylib"); ok {
		if dot := strings.LastIndexByte(stem, '.'); dot > 0 && isVersion(stem[dot+1:]) {
			return stem[:dot] + ".dylib"
		}
	}
	return ""
}

// isVersion reports whether s is a bare version such as "1" or "1.0.0".
func isVersion(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// runtimeName rejects a name that is not a plain file name in the lib
// directory. Library names come from bundle metadata, so an absolute name or
// one that climbs out through ".." must never become a link target.
func runtimeName(name string) error {
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("library %q is not a plain file name inside the lib directory", name)
	}
	return nil
}

// placeRuntimeLink points link at target, both plain names in dir: the target
// stays inside the managed lib directory and is relative, so the link resolves
// the same way wherever the data directory is mounted. An identical link is
// left untouched. A regular file at that name is swapped for the link only
// when it is a byte-for-byte copy of the real library, which is how a tree that
// an older bundle stuffed with dereferenced copies heals back into one file
// plus links and gives the duplicate back to the data partition; anything else
// there is the operator's own layout and wins. A new link is created under a
// temporary name and renamed, so the loader never resolves a half-created link.
func placeRuntimeLink(dir, link, target string) error {
	if err := runtimeName(link); err != nil {
		return err
	}
	if err := runtimeName(target); err != nil {
		return err
	}
	path := filepath.Join(dir, link)
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		current, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if current == target {
			return nil
		}
	case err == nil && info.Mode().IsRegular():
		same, err := sameContents(path, filepath.Join(dir, target))
		if err != nil {
			return err
		}
		if !same {
			fmt.Fprintf(os.Stderr, "deploy: warning: %s is a regular file, not the link to %s; leaving it in place\n", path, target)
			return nil
		}
	case err == nil:
		fmt.Fprintf(os.Stderr, "deploy: warning: %s is not a regular file or symlink; leaving the installed layout alone\n", path)
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".deploy-link-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
//
// Both steps copy rather than rename. /etc/init.d sits on the overlayfs root,
// where renaming a file that still lives in the lower layer fails with
// EINVAL; writing the path directly forces the copy-up that rename refuses.
func (d *deployment) activateBootHook(src, dst string) error {
	a := activation{target: dst, previous: dst + ".prev", kind: activationCopy, mode: 0o755}
	info, err := os.Lstat(dst)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("boot hook %s is a symlink; refusing to write through it", dst)
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("boot hook %s is not a regular file", dst)
	case err == nil:
		a.hadBefore = true
		_ = os.Remove(a.previous)
		if err := copyFileInPlace(dst, a.previous, a.mode); err != nil {
			return fmt.Errorf("preserve boot hook %s: %w", dst, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := copyFileInPlace(src, dst, a.mode); err != nil {
		installed := fmt.Errorf("install boot hook %s: %w", dst, err)
		if !a.hadBefore {
			// The failed write created the file; leave the system as it was.
			_ = os.Remove(dst)
			return installed
		}
		// The write truncates the hook before it can fail, so the previous
		// script has to be put back here: the activation is not recorded and
		// the rollback path below never learns about this file.
		if rerr := copyFileInPlace(a.previous, dst, a.mode); rerr != nil {
			return fmt.Errorf("%w; restoring the previous hook also failed: %v (backup kept at %s)", installed, rerr, a.previous)
		}
		_ = os.Remove(a.previous)
		return installed
	}
	d.hook = &a
	return nil
}

// copyFileInPlace writes src over dst through a single open/truncate/write. It
// needs no sibling, which is what an overlayfs lower-layer file and a
// single-slot partition both require.
func copyFileInPlace(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// The creation mode only applies to a new file.
	return os.Chmod(dst, mode)
}

// compressFile is the staging backup step. It is a variable so a test can
// assert that the backup is compressed before the watchdog is handed back.
var compressFile = compressFileTo

// compressFileTo writes src to dst through gzip. The staging backup of the
// replaced daemon costs a fraction of its 40 MiB on disk, which is what lets a
// single-slot data partition absorb an upgrade.
func compressFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		out.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// decompressFile restores a compressed backup directly to dst: it needs room
// for one copy of the file, never two.
func decompressFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer gz.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, gz); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// ensureExecutable makes a bundle binary runnable in place. The probe executes
// the file directly, and a copied bundle does not always carry an execute bit;
// raising the mode costs nothing, unlike a second copy of the binary.
func ensureExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0o111 != 0 {
		return nil
	}
	if err := os.Chmod(path, info.Mode().Perm()|0o755); err != nil {
		return fmt.Errorf("%s is not executable and its mode cannot be raised: %w", path, err)
	}
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
// root, so the operator's environment (credentials, CAPSULAD_* overrides,
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
	home, err := os.MkdirTemp("", "capsulad-probe-")
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

// bootServiceEnv is the environment for the boot hook: the operator's
// environment otherwise reaches it, minus a data directory that would override
// the one this deployment targets.
func bootServiceEnv(dataDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CAPSULAD_DATA_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "CAPSULAD_DATA_DIR="+dataDir)
}

// stopViaBootService stops the daemon through the boot hook. Stopping first
// means packages are installed with nothing running: a selected provider app
// is replaced without the daemon noticing, and the new binaries are never
// mapped by the old process.
func stopViaBootService(script, dataDir string) error {
	cmd := exec.Command(script, "stop")
	// Stock hooks fix their own data directory; exporting the chosen one keeps
	// a non-stock hook usable without a second flag vocabulary.
	cmd.Env = bootServiceEnv(dataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s stop: %v (%s)", script, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func restartViaBootService(script, dataDir string) error {
	cmd := exec.Command(script, "restart")
	cmd.Env = bootServiceEnv(dataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s restart: %v (%s)", script, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitDaemonStoppedFn is the stop verification. It is a variable so tests can
// exercise the uncertain-stop path without a serving daemon.
var waitDaemonStoppedFn = waitDaemonStopped

// waitDaemonStopped confirms the boot service actually stopped the daemon. The
// stock hook blocks for its own shutdown; a non-stock hook may return early,
// and installing packages under a live daemon would be unsafe.
//
// A daemon that is alive but not answering (starting up, wedged, or just killed
// and still shutting down) still holds the socket and the binary, so an RPC
// failure alone is not enough: the pid from the daemon's own pid file has to be
// gone as well.
func waitDaemonStopped(socket, pidFile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := probeDaemon(ctx, socket)
		cancel()
		if err != nil {
			gone, why := daemonPIDGone(pidFile)
			if gone {
				return nil
			}
			last = why
		} else {
			last = fmt.Errorf("the daemon still answers on %s", socket)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon did not stop within %s (%v); stop it before deploying", timeout, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// daemonPIDGone reports whether the daemon recorded in pidFile is gone. A
// missing, empty or stale pid file is good enough: nothing is running.
func daemonPIDGone(pidFile string) (bool, error) {
	body, err := os.ReadFile(pidFile)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", pidFile, err)
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return true, nil
	}
	pid, err := strconv.Atoi(text)
	if err != nil {
		return false, fmt.Errorf("%s holds %q, not a pid", pidFile, text)
	}
	if !pidAlive(pid) {
		return true, nil
	}
	return false, fmt.Errorf("%s still names running pid %d", pidFile, pid)
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
