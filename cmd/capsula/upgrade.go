package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

func runDaemon(args []string, dataDir, socket string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capsula daemon <install|rollback> [...]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return daemonUpgrade(rest, dataDir, socket)
	case "rollback":
		return daemonRollback(rest, dataDir, socket)
	default:
		return fmt.Errorf("unknown daemon subcommand %q (install, rollback)", sub)
	}
}

// daemonUpgrade replaces the running capsulad binary in place. The new binary
// is staged beside the target, verified by executing its -version, activated by
// rename, and accepted only after the daemon answers the framework API. If any
// step fails the previous binary is put back and restarted.
//
// The restart goes through the boot service so exactly one owner of the
// hardware watchdog survives the handover.
func daemonUpgrade(args []string, dataDir, socket string) error {
	fs := flag.NewFlagSet("daemon install", flag.ContinueOnError)
	binary := fs.String("binary", "", "new capsulad binary to install")
	target := fs.String("target", "", "installed daemon path (default <data-dir>/capsulad)")
	initScript := fs.String("init-script", "/etc/init.d/S60capsulad", "boot service used to restart the daemon")
	timeout := fs.Duration("timeout", 45*time.Second, "time to wait for a healthy daemon after restart")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*binary) == "" {
		return fmt.Errorf("usage: capsula -data-dir DIR daemon install -binary PATH [-target PATH] [-init-script PATH]")
	}
	dir := resolveDataDir(dataDir)
	if *target == "" {
		*target = filepath.Join(dir, "capsulad")
	}
	if err := requireBootService(*initScript); err != nil {
		return err
	}
	if err := verifyDaemonBinary(*binary); err != nil {
		return err
	}
	staged := *target + ".new"
	_ = os.Remove(staged)
	if err := copyExecutable(*binary, staged); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	if err := verifyDaemonBinary(staged); err != nil {
		_ = os.Remove(staged)
		return err
	}
	previous := *target + ".prev"
	hadPrevious := false
	if _, err := os.Stat(*target); err == nil {
		hadPrevious = true
		_ = os.Remove(previous)
		if err := os.Rename(*target, previous); err != nil {
			_ = os.Remove(staged)
			return fmt.Errorf("preserve previous binary: %w", err)
		}
	}
	if err := os.Rename(staged, *target); err != nil {
		if hadPrevious {
			_ = os.Rename(previous, *target)
		}
		return fmt.Errorf("activate new binary: %w", err)
	}
	if err := restartViaBootService(*initScript); err != nil {
		return undoInstall(*target, previous, hadPrevious, *initScript, socket, dir, *timeout, err)
	}
	if err := waitDaemonHealthy(resolveSocket(dir, socket), *timeout); err != nil {
		return undoInstall(*target, previous, hadPrevious, *initScript, socket, dir, *timeout, err)
	}
	fmt.Printf("installed %s\n", *target)
	if hadPrevious {
		fmt.Printf("previous binary kept at %s; `capsula daemon rollback` restores it\n", previous)
	}
	return nil
}

// undoInstall puts the previous binary back after a failed activation.
func undoInstall(target, previous string, hadPrevious bool, initScript, socket, dir string, timeout time.Duration, cause error) error {
	if !hadPrevious {
		return fmt.Errorf("new daemon failed and there is no previous binary to restore: %w", cause)
	}
	failed := target + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(target, failed); err != nil {
		return fmt.Errorf("new daemon failed (%v) and the failed binary could not be moved aside: %w", cause, err)
	}
	if err := os.Rename(previous, target); err != nil {
		return fmt.Errorf("new daemon failed (%v) and the previous binary could not be restored: %w", cause, err)
	}
	_ = os.Remove(failed)
	if err := restartViaBootService(initScript); err != nil {
		return fmt.Errorf("new daemon failed (%v); restored the previous binary but restart failed: %w", cause, err)
	}
	if err := waitDaemonHealthy(resolveSocket(dir, socket), timeout); err != nil {
		return fmt.Errorf("new daemon failed (%v); restored the previous binary but it is not healthy: %w", cause, err)
	}
	return fmt.Errorf("new daemon failed and was rolled back: %w", cause)
}

// daemonRollback restores the binary preserved by the last install. The
// replaced binary becomes the new previous one, so rollback is reversible.
func daemonRollback(args []string, dataDir, socket string) error {
	fs := flag.NewFlagSet("daemon rollback", flag.ContinueOnError)
	target := fs.String("target", "", "installed daemon path (default <data-dir>/capsulad)")
	initScript := fs.String("init-script", "/etc/init.d/S60capsulad", "boot service used to restart the daemon")
	timeout := fs.Duration("timeout", 45*time.Second, "time to wait for a healthy daemon after restart")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := resolveDataDir(dataDir)
	if *target == "" {
		*target = filepath.Join(dir, "capsulad")
	}
	previous := *target + ".prev"
	if _, err := os.Stat(previous); err != nil {
		return fmt.Errorf("no previous binary at %s", previous)
	}
	if err := requireBootService(*initScript); err != nil {
		return err
	}
	if err := verifyDaemonBinary(previous); err != nil {
		return err
	}
	displaced := *target + ".failed"
	_ = os.Remove(displaced)
	if err := os.Rename(*target, displaced); err != nil {
		return fmt.Errorf("set aside current binary: %w", err)
	}
	if err := os.Rename(previous, *target); err != nil {
		_ = os.Rename(displaced, *target)
		return fmt.Errorf("restore previous binary: %w", err)
	}
	if err := restartViaBootService(*initScript); err != nil {
		_ = os.Rename(*target, previous)
		_ = os.Rename(displaced, *target)
		_ = restartViaBootService(*initScript)
		return fmt.Errorf("restart after rollback: %w", err)
	}
	if err := waitDaemonHealthy(resolveSocket(dir, socket), *timeout); err != nil {
		_ = os.Rename(*target, previous)
		_ = os.Rename(displaced, *target)
		_ = restartViaBootService(*initScript)
		return fmt.Errorf("rolled back binary is not healthy; restored the newer one: %w", err)
	}
	if err := os.Rename(displaced, previous); err != nil {
		_ = os.Remove(displaced)
	}
	fmt.Printf("rolled back to %s\n", previous)
	return nil
}

func requireBootService(script string) error {
	info, err := os.Stat(script)
	if err != nil {
		return fmt.Errorf("boot service %s is missing; deploy it before upgrading the daemon", script)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("boot service %s is not executable", script)
	}
	return nil
}

// verifyDaemonBinary proves the file executes here and reports itself as
// capsulad. A broken or wrong-architecture binary fails before activation.
func verifyDaemonBinary(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("%s does not run: %v (%s)", path, err, text)
	}
	if !strings.HasPrefix(text, "capsulad ") {
		return fmt.Errorf("%s is not a capsulad binary: %q", path, text)
	}
	return nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
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
	if dir, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func restartViaBootService(script string) error {
	out, err := exec.Command(script, "restart").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s restart: %v (%s)", script, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitDaemonHealthy polls the framework API. It is deliberately independent of
// the daemon's own -health flag so a rollback to an older binary can still be
// verified.
func waitDaemonHealthy(socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
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
