// Package device talks to a Capsula Mini over ADB (jailbroken stock images) or
// SSH (Alpine), detects the firmware, streams the stock userspace backup and
// launches the deployment detached.
//
// All command construction here is pure so it can be tested without a device.
package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Command is one remote command. Stdin carries secrets (an admin password, a
// Wi-Fi passphrase, a token): it is passed on the remote process's standard
// input, never spliced into Args, which are the only words visible in a
// process list and in logs.
type Command struct {
	Args      []string
	Stdin     []byte
	Stdout    io.Writer
	Stderr    io.Writer
	MaxStdout int64
}

// Result is a completed remote command. ExitCode is the remote program's
// status; Error is reserved for transport failures.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Device is the transport seam. Fake implementations drive the flow tests.
type Device interface {
	// Connect establishes the transport. ADB runs `adb connect`; SSH dials.
	Connect(ctx context.Context) error
	// Run executes a command and streams Stdout to Command.Stdout when set.
	Run(ctx context.Context, cmd Command) (Result, error)
	// Push copies a local regular file to a remote path.
	Push(ctx context.Context, local, remote string) error
	// Addr is the address the user entered (host or host:port).
	Addr() string
	// Kind is "adb" or "ssh".
	Kind() string
	Close() error
}

// Errors the installer distinguishes.
var (
	ErrUnauthorized = errors.New("device is not authorized yet")
	ErrUnreachable  = errors.New("device is unreachable")
	ErrHostKey      = errors.New("SSH host key mismatch")
)

// CommandError is a remote command that ran and failed.
type CommandError struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("%s exited %d", strings.Join(e.Args, " "), e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + firstLine(e.Stderr)
	}
	return msg
}

func firstLine(s string) string {
	for i := range len(s) {
		if s[i] == '\n' || s[i] == '\r' {
			return s[:i]
		}
	}
	return s
}
