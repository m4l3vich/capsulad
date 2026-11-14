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

func (l *Launcher) namespaceCommand(m *manifest.Manifest, appDir, execPath string, args []string, runID, appID string, env []string, libraryDir string) (*exec.Cmd, error) {
	cmd := exec.Command(execPath, args...)
	cmd.Dir = appDir
	cmd.Env = env
	return cmd, nil
}

func (l *Launcher) uidCommand(appID, appPath string, args []string, env []string) (*exec.Cmd, error) {
	cmd := exec.Command(appPath, args...)
	cmd.Env = env
	return cmd, nil
}

func (l *Launcher) CleanupRun(runID string) {}

func runExecHelper() int { return 1 }

func seccompProbe() int { return 1 }
