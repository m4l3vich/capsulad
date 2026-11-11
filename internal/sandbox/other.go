//go:build !linux

package sandbox

import (
	"context"
	"os/exec"

	"capsulad/internal/manifest"
)

// Probe reports that namespace sandboxing requires Linux. Development on other
// platforms runs apps directly and reports sandbox mode "none".
func (l *Launcher) Probe(ctx context.Context) error {
	l.setMode("none", "namespace sandboxing is only implemented on Linux")
	return nil
}

func (l *Launcher) namespaceCommand(m *manifest.Manifest, appDir, appPath, runID, appID string) (*exec.Cmd, error) {
	cmd := exec.Command(appPath)
	cmd.Dir = appDir
	cmd.Env = l.env()
	return cmd, nil
}

func (l *Launcher) uidCommand(appID, appPath string) (*exec.Cmd, error) {
	cmd := exec.Command(appPath)
	cmd.Env = l.env()
	return cmd, nil
}

func (l *Launcher) CleanupRun(runID string) {}

func runExecHelper() int { return 1 }

func seccompProbe() int { return 1 }
