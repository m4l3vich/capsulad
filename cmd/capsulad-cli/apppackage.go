package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/artifact"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/registry"
)

// Package administration has two paths, and the choice is not a retry policy.
//
// While capsulad is listening, the daemon owns the package tree: the install is
// validated, swapped atomically and activated in the live registry, and the
// daemon refuses an app that is running. When nothing is listening the package
// is installed on disk exactly as before, which is what a deployment or a
// recovery needs, and the next daemon start activates it.
//
// The offline path is chosen only when the daemon is *proven* absent: its
// socket does not exist or actively refuses a connection. A timeout, a
// permission error or any other transport failure proves nothing, so the CLI
// reports it instead of writing to a tree a live daemon may be reading.

// installAppPackage installs or atomically replaces one package.
func installAppPackage(src, dataDir, socket, nativeLibDir string) error {
	listening, err := daemonListening(resolveSocket(dataDir, socket))
	if err != nil {
		return err
	}
	if listening {
		return installThroughDaemon(src, dataDir, socket)
	}
	return installOffline(src, dataDir, socket, nativeLibDir)
}

// removeAppPackage removes one installed package.
func removeAppPackage(appID, dataDir, socket string) error {
	listening, err := daemonListening(resolveSocket(dataDir, socket))
	if err != nil {
		return err
	}
	if listening {
		return removeThroughDaemon(appID, dataDir, socket)
	}
	return removeOffline(appID, dataDir, socket)
}

// daemonListening reports whether a daemon accepts connections on socket.
// Only an absent socket file or an actively refused connection reports false;
// every other failure is returned because it cannot distinguish a stopped
// daemon from an unreachable one.
func daemonListening(socket string) (bool, error) {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ENOTSOCK):
		return false, nil
	}
	return false, fmt.Errorf("cannot tell whether capsulad is running on %s (%v); refusing to change the package tree behind a daemon that may be live", socket, err)
}

// installThroughDaemon streams the package to the running daemon, which
// activates it in the live registry.
func installThroughDaemon(src, dataDir, socket string) error {
	archive, cleanup, err := packageForStream(src)
	if err != nil {
		return err
	}
	defer cleanup()
	conn, err := dial(resolveSocket(dataDir, socket))
	if err != nil {
		return err
	}
	defer conn.Close()
	// The upload is local and the daemon unpacks as it receives, but a
	// maximum-size package on a slow device still needs more than the usual
	// administration timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	stream, err := frameworkv1.NewApplicationServiceClient(conn).InstallApplication(ctx)
	if err != nil {
		return daemonPackageError(err, "install")
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := stream.Send(&frameworkv1.InstallApplicationRequest{
		Chunk: &frameworkv1.InstallApplicationRequest_Open{
			Open: &frameworkv1.InstallApplicationOpen{FileName: filepath.Base(archive)},
		},
	}); err != nil {
		return daemonPackageError(err, "install")
	}
	buf := make([]byte, 64<<10)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&frameworkv1.InstallApplicationRequest{
				Chunk: &frameworkv1.InstallApplicationRequest_Content{Content: buf[:n]},
			}); err != nil {
				return daemonPackageError(err, "install")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = stream.CloseSend()
			return readErr
		}
	}
	res, err := stream.CloseAndRecv()
	if err != nil {
		return daemonPackageError(err, "install")
	}
	app := res.GetApplication()
	dir := filepath.Join(resolveDataDir(dataDir), "apps", app.GetAppId())
	switch {
	case !res.GetReplaced():
		fmt.Printf("installed %s (%s) into %s\n", app.GetAppId(), app.GetVersion(), dir)
	case res.GetPreviousVersion() == app.GetVersion():
		fmt.Printf("reinstalled %s (%s) into %s\n", app.GetAppId(), app.GetVersion(), dir)
	default:
		fmt.Printf("updated %s (%s -> %s) into %s\n", app.GetAppId(), res.GetPreviousVersion(), app.GetVersion(), dir)
	}
	fmt.Println("active now: the daemon reloaded its application registry")
	return nil
}

// removeThroughDaemon removes the package through the running daemon, which
// refuses an app that is running and keeps the app's stored settings.
func removeThroughDaemon(appID, dataDir, socket string) error {
	conn, err := dial(resolveSocket(dataDir, socket))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := frameworkv1.NewApplicationServiceClient(conn).UninstallApplication(ctx,
		&frameworkv1.UninstallApplicationRequest{AppId: appID}); err != nil {
		return daemonPackageError(err, "remove")
	}
	fmt.Printf("removed %s\n", appID)
	return nil
}

// daemonPackageError keeps the daemon's own refusal text (a running app, a
// rejected archive, a missing app) and gives a version-skew answer its own
// explanation. It never falls back to the on-disk path: the daemon is live.
func daemonPackageError(err error, operation string) error {
	st, _ := status.FromError(err)
	if st.Code() == codes.Unimplemented {
		return fmt.Errorf("the running daemon does not support package %s over its control socket; deploy the matching capsulad and capsulad-cli together, or stop capsulad and run this command offline", operation)
	}
	if st.Message() != "" {
		return errors.New(st.Message())
	}
	return err
}

// packageForStream returns a .tar.gz path for src. An archive is used as it
// is; a package directory is packaged into a temporary archive because the
// transport carries an archive, not a directory. The returned cleanup removes
// only the temporary file this call created.
func packageForStream(src string) (string, func(), error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", nil, err
	}
	if !info.IsDir() {
		return src, func() {}, nil
	}
	out, err := os.CreateTemp("", "capsulad-package-*.tar.gz")
	if err != nil {
		return "", nil, err
	}
	name := out.Name()
	if err := out.Close(); err != nil {
		_ = os.Remove(name)
		return "", nil, err
	}
	if err := artifact.Write(src, name); err != nil {
		_ = os.Remove(name)
		return "", nil, err
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// installOffline installs into <data-dir>/apps with nothing running. This is
// the documented deployment and recovery path; it prints the restart reminder
// because the daemon is not there to reload its registry.
func installOffline(src, dataDir, socket, nativeLibDir string) error {
	// Stage under the apps root (same filesystem as the installation) so the
	// package is copied once and can be renamed into place.
	root := filepath.Join(resolveDataDir(dataDir), "apps")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	pkg, err := artifact.OpenIn(staging, src)
	if err != nil {
		return err
	}
	defer pkg.Close()
	m := pkg.Manifest
	if err := checkNativeSDKRequirement(m, dataDir, nativeLibDir); err != nil {
		return err
	}
	installed := false
	if _, err := os.Stat(filepath.Join(root, m.ID)); err == nil {
		installed = true
	}
	// A running app keeps its binary mapped: replacing the package under a
	// live run would leave the old process running from a deleted tree and
	// consume the space a retry needs. Require an explicit stop first. This is
	// a defensive re-check: the caller already proved no daemon is listening,
	// so an unreachable-daemon answer here can only mean it appeared since.
	if run := liveRun(dataDir, socket, m.ID); run != "" {
		return fmt.Errorf("app %s is running (run %s); stop it before replacing the package", m.ID, run)
	}
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		return err
	}
	app, err := reg.InstallPackage(pkg)
	if err != nil {
		return err
	}
	verb := "installed"
	if installed {
		verb = "updated"
	}
	fmt.Printf("%s %s (%s) into %s\n", verb, app.ID(), app.Manifest.Version, app.Dir)
	fmt.Println("restart capsulad to activate the installed package")
	return nil
}

// removeOffline deletes one installed package directory with nothing running.
func removeOffline(appID, dataDir, socket string) error {
	if run := liveRun(dataDir, socket, appID); run != "" {
		return fmt.Errorf("app %s is running (run %s); stop it before removing", appID, run)
	}
	root := filepath.Join(resolveDataDir(dataDir), "apps")
	logs, _ := logbus.New(ids.NewClock(), 100, "")
	reg, err := registry.Open(root, logs)
	if err != nil {
		return err
	}
	if err := reg.Uninstall(appID); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", appID)
	return nil
}
