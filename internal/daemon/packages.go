package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"capsulad/internal/artifact"
	"capsulad/internal/manifest"
	"capsulad/internal/registry"
	"capsulad/internal/sandbox"
	"capsulad/internal/uichanges"
)

// Package administration is the daemon's live view of the installed-application
// set. The registry owns the package tree and the artifact package owns the
// on-disk format; these operations add the daemon's part: the native-SDK
// preflight, the refusal to touch a package whose app is running, the atomic
// swap the registry already performs, and the caches and UI signals that must
// follow a change to the installed set.
//
// A successful install or update is visible to every subsequent read without a
// daemon restart: the registry is rescanned in place. A stopped daemon keeps
// the documented on-disk path (the CLI and the deployment bundle install while
// nothing is running), and the package becomes active at the next start.

// PackageInstall is the outcome of one install call: the installed
// application, whether an existing installation was replaced, and the version
// that installation carried before (empty for a fresh install). Replaced and
// the version are reported separately so a caller can tell an update from a
// reinstall of the same version instead of guessing from one of them.
type PackageInstall struct {
	App             *registry.App
	Replaced        bool
	PreviousVersion string
}

// installWaitBudget bounds how long a second upload waits for the single
// staging slot before it is refused. It is long enough for one package to be
// delivered over Wi-Fi and short enough that a caller is never pinned behind a
// stalled upload.
const installWaitBudget = 30 * time.Second

// defaultInstallIdleBudget is how long one upload may deliver no archive bytes
// before the daemon ends the RPC. A live Wi-Fi client sends continuously, so a
// silent minute means it is gone or wedged; ending the RPC unblocks the
// extraction read, removes the staging tree and frees the admission slot.
const defaultInstallIdleBudget = 60 * time.Second

// installTotalBudget bounds one upload end to end, so even a client that drips
// bytes slowly cannot hold the staging slot indefinitely.
const installTotalBudget = 30 * time.Minute

// admitInstall reserves the single package-staging slot. The data partition is
// 78.5 MiB and one package may unpack to 64 MiB, so two uploads unpacking at
// once can consume the space a retry needs and break logging and the next
// upgrade. One staging tree at a time is the only safe number; a second upload
// waits for the bounded budget and is then refused with ResourceExhausted
// instead of queueing without limit. Cancelling the call while it waits is
// honoured, so a browser that navigates away does not leave a reservation.
func (d *Daemon) admitInstall(ctx context.Context, wait time.Duration) (func(), error) {
	if d.installSlot == nil {
		// A focused test that builds a Daemon literal has no admission state;
		// production daemons always get the slot from New.
		return func() {}, nil
	}
	release := func() { <-d.installSlot }
	select {
	case d.installSlot <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case d.installSlot <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, status.Error(codes.Canceled, "package install cancelled while another package was being installed")
	case <-timer.C:
		return nil, status.Error(codes.ResourceExhausted, "another package is being installed; try again when it finishes")
	}
}

// installCommit is the handshake between the supervising RPC handler and the
// worker that performs one install. Exactly one side wins the transition out
// of staging: the supervisor may declare the upload abandoned (and is then
// entitled to report that nothing was installed), or the worker may begin the
// commit (and the supervisor must then await and report the real outcome). A
// context check alone is not enough: the two decisions can interleave between
// the check and the swap.
type installCommit struct{ state atomic.Int32 }

const (
	installStaging int32 = iota
	installCommitting
	installAbandoned
)

// abandon stops an upload that has not begun its commit. It reports whether
// the supervisor won; false means the worker is already past the point of no
// return.
func (c *installCommit) abandon() bool {
	return c != nil && c.state.CompareAndSwap(installStaging, installAbandoned)
}

// beginCommit is the worker's side of the handshake. It reports whether the
// swap may proceed; false means the supervisor abandoned the upload first.
func (c *installCommit) beginCommit() bool {
	if c == nil {
		return true
	}
	return c.state.CompareAndSwap(installStaging, installCommitting)
}

// committing reports whether the worker won the handshake.
func (c *installCommit) committing() bool {
	return c != nil && c.state.Load() == installCommitting
}

// InstallPackageStream installs or replaces one application package delivered
// as a gzip-compressed tar. The archive is unpacked directly into a staging
// tree on the applications filesystem, so a package is never written twice and
// an upload cannot exhaust the data partition with the archive and its tree at
// once. Both the compressed input and its expansion are bounded: the caller
// caps the stream at artifact.MaxArchiveBytes and the artifact limits cap the
// unpacked tree. Staging holds the single admission slot, so concurrent uploads
// cannot stack staging trees on the partition, and a cancelled upload releases
// its tree as soon as its reads fail.
func (d *Daemon) InstallPackageStream(ctx context.Context, r io.Reader) (PackageInstall, error) {
	return d.installPackageStream(ctx, r, nil)
}

// installPackageStream is InstallPackageStream with the supervisor handshake.
// The RPC handler passes the guard it also holds, so a timeout and the swap
// decision cannot disagree; a direct caller passes nil and always commits.
func (d *Daemon) installPackageStream(ctx context.Context, r io.Reader, commit *installCommit) (PackageInstall, error) {
	release, err := d.admitInstall(ctx, installWaitBudget)
	if err != nil {
		return PackageInstall{}, err
	}
	defer release()
	if err := d.requireFreeSpace("installing a package"); err != nil {
		return PackageInstall{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	stage, err := os.MkdirTemp(d.cfg.AppsDir, ".upload-")
	if err != nil {
		return PackageInstall{}, status.Errorf(codes.Internal, "stage package: %v", err)
	}
	defer os.RemoveAll(stage)
	// Unpacking is the slow part and touches nothing the running daemon reads:
	// it happens before the package lock so an upload never blocks a launch for
	// as long as a browser takes to deliver the archive.
	pkg, err := artifact.OpenReader(stage, r, artifact.DefaultLimits)
	if err != nil {
		return PackageInstall{}, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	defer pkg.Close()

	m := pkg.Manifest
	if err := d.checkNativeSDKRequirement(m); err != nil {
		return PackageInstall{}, status.Errorf(codes.FailedPrecondition, "%v", err)
	}

	// From here on the installed set changes: the running-app check, the swap
	// and the rescan must not interleave with starting a process from the tree
	// being replaced.
	d.pkgMu.Lock()
	defer d.pkgMu.Unlock()
	// The commit decision is taken under the same lock that performs the swap,
	// and it is a compare-and-swap against the supervisor's abandon decision:
	// from here a timeout can no longer claim nothing was installed, and the
	// supervisor must await this call's real outcome instead.
	if !commit.beginCommit() {
		return PackageInstall{}, status.Error(codes.Canceled, "the package install was abandoned before it started")
	}
	if d.reg.IsBuiltin(m.ID) {
		return PackageInstall{}, status.Errorf(codes.FailedPrecondition, "app %s is a built-in system application and cannot be replaced", m.ID)
	}
	out := PackageInstall{}
	if previous, ok := d.reg.Get(m.ID); ok {
		out.Replaced = true
		out.PreviousVersion = previous.Manifest.Version
	}
	// A running app keeps its binary mapped and its package directory live:
	// replacing the tree under it would leave the old process running from a
	// removed directory and consume the space a retry needs. The operation is
	// refused instead of stopped behind the administrator's back.
	if run, ok := d.liveRun(m.ID); ok {
		return PackageInstall{}, status.Errorf(codes.FailedPrecondition, "app %s is running (run %s); stop it before replacing the package", m.ID, run)
	}
	app, err := d.reg.InstallPackage(pkg)
	if err != nil {
		return PackageInstall{}, status.Errorf(codes.Internal, "%v", err)
	}
	out.App = app

	verb := "installed"
	if out.Replaced {
		verb = "updated"
	}
	d.logs.Infof("daemon", app.ID(), "%s package version=%s dir=%s", verb, app.Manifest.Version, app.Dir)
	// The cached provenance is keyed by app ID alone, so a replaced binary
	// would otherwise keep reporting the old subject hash.
	d.forgetProvenance(m.ID)
	// A new or replaced package is not started here. start_on_boot and
	// background describe what the daemon does at start; an administrator who
	// just uploaded a package decides by launching it, and an update must not
	// resurrect an app that had to be stopped for the replacement.
	d.publishUI(uichanges.Apps)
	return out, nil
}

// UninstallPackage removes one installed package. App settings are deliberately
// retained: reinstalling the same ID finds them again, and a stored provider
// selection is the administrator's choice rather than something a removal
// rewrites.
func (d *Daemon) UninstallPackage(appID string) error {
	if appID == "" {
		return status.Error(codes.InvalidArgument, "app id is required")
	}
	if d.reg.IsBuiltin(appID) {
		return status.Errorf(codes.FailedPrecondition, "app %s is a built-in system application and cannot be uninstalled", appID)
	}
	d.pkgMu.Lock()
	defer d.pkgMu.Unlock()
	if _, ok := d.reg.Get(appID); !ok {
		return status.Errorf(codes.NotFound, "app %s is not installed", appID)
	}
	if run, live := d.liveRun(appID); live {
		return status.Errorf(codes.FailedPrecondition, "app %s is running (run %s); stop it before removing", appID, run)
	}
	if err := d.reg.Uninstall(appID); err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	d.logs.Infof("daemon", appID, "removed package")
	d.forgetProvenance(appID)
	d.publishUI(uichanges.Apps)
	return nil
}

// liveRun returns a supervised run ID for appID. Only live runs count: an
// exited process no longer holds the package tree, and the framework's own run
// listing uses the same rule.
func (d *Daemon) liveRun(appID string) (string, bool) {
	for _, run := range d.sup.List(appID) {
		return run.RunID, true
	}
	return "", false
}

// checkNativeSDKRequirement rejects an install whose declared native SDK
// requirement the installed runtime cannot satisfy. Apps without a [native_sdk]
// section are unaffected: the runtime status is still reported at launch.
func (d *Daemon) checkNativeSDKRequirement(m *manifest.Manifest) error {
	if m.NativeSDK == nil {
		return nil
	}
	req := *m.NativeSDK
	unusable := fmt.Errorf("app %s requires native SDK ABI %d.%d", m.ID, req.ABIMajor, req.MinMinor)
	if d.launcher == nil {
		return fmt.Errorf("%w: this daemon has no sandbox launcher", unusable)
	}
	dir, err := d.launcher.LibraryDir()
	if err != nil {
		return fmt.Errorf("%w: %v", unusable, err)
	}
	rt, err := sandbox.LocateRuntime(dir)
	if err != nil {
		return fmt.Errorf("%w: %v", unusable, err)
	}
	if err := sandbox.CheckRequirement(rt, req); err != nil {
		return fmt.Errorf("app %s: %w", m.ID, err)
	}
	return nil
}

// forgetProvenance drops the cached binary-provenance result for an app whose
// package tree was replaced or removed.
func (d *Daemon) forgetProvenance(appID string) {
	d.provMu.Lock()
	delete(d.prov, appID)
	d.provMu.Unlock()
}
