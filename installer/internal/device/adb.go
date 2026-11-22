package device

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ADB states reported by `adb connect` and `adb get-state`.
type ADBState string

const (
	ADBConnected      ADBState = "connected"
	ADBAlready        ADBState = "already connected"
	ADBUnauthorized   ADBState = "unauthorized"
	ADBOffline        ADBState = "offline"
	ADBFailed         ADBState = "failed"
	ADBUnknown        ADBState = "unknown"
	adbDefaultTimeout          = 20 * time.Second
)

// ADB is a Device over the adb binary. Every invocation names the serial
// explicitly, so several attached devices cannot make it act on the wrong one.
type ADB struct {
	Path     string
	Address  string
	Timeout  time.Duration
	Log      func(string, ...any)
	Interval time.Duration
	execOut  *bool
}

// NewADB returns an ADB transport. An empty path resolves `adb` from PATH.
func NewADB(path, addr string) *ADB {
	if strings.TrimSpace(path) == "" {
		path = "adb"
	}
	if !strings.Contains(addr, ":") {
		addr += ":5555"
	}
	return &ADB{Path: path, Address: addr, Timeout: adbDefaultTimeout, Interval: 2 * time.Second}
}

func (a *ADB) Addr() string { return a.Address }
func (a *ADB) Kind() string { return "adb" }
func (a *ADB) Close() error { return nil }

// ConnectArgs is the adb command that attaches a network device.
func ConnectArgs(addr string) []string { return []string{"connect", addr} }

// GetStateArgs is the adb command that reports authorization state.
func GetStateArgs(addr string) []string { return []string{"-s", addr, "get-state"} }

// ShellArgs builds the adb invocation for a remote argv. The remote argv is
// shell-quoted into a single command string so adb's space joining cannot
// re-split an argument (a deploy script, for example).
func ShellArgs(addr string, argv []string) []string {
	return []string{"-s", addr, "shell", ShellJoin(argv)}
}

// ExecOutArgs builds the byte-exact invocation used for a binary stream when
// adbd supports exec-out. Some stock adbd builds reject it, so the caller must
// probe and use ShellRawArgs instead.
func ExecOutArgs(addr string, argv []string) []string {
	return []string{"-s", addr, "exec-out", ShellJoin(argv)}
}

// ShellRawArgs streams through a pty shell with output processing disabled.
// This is the fallback for adbd builds that reject exec-out and non-pty
// shells ("device only supports allocating a pty"). `stty raw -echo` must run
// before the command writes any bytes. stderr is merged into the pty, so a
// trailing success marker on stderr is stripped by the caller; other stderr
// bytes would corrupt the stream and are caught by validation.
func ShellRawArgs(addr string, argv []string) []string {
	script := "stty raw -echo 2>/dev/null; " + ShellJoin(argv)
	return []string{"-s", addr, "shell", script}
}

// PushArgs builds `adb push` for one regular file.
func PushArgs(addr, local, remote string) []string {
	return []string{"-s", addr, "push", "--", local, remote}
}

// ShellJoin quotes an argv for a POSIX shell. A secret is never part of argv,
// but quoting every word keeps caller-supplied paths intact.
func ShellJoin(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = ShellQuote(a)
	}
	return strings.Join(words, " ")
}

// ShellQuote returns one POSIX shell word.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ParseConnectOutput classifies `adb connect` output.
func ParseConnectOutput(out string) ADBState {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "unauthorized"):
		return ADBUnauthorized
	case strings.Contains(low, "already connected"):
		return ADBAlready
	case strings.Contains(low, "connected to"):
		return ADBConnected
	case strings.Contains(low, "failed"), strings.Contains(low, "cannot"), strings.Contains(low, "unable"), strings.Contains(low, "refused"):
		return ADBFailed
	case strings.Contains(low, "offline"):
		return ADBOffline
	}
	return ADBUnknown
}

// ParseStateOutput classifies `adb get-state` output.
func ParseStateOutput(out string) ADBState {
	switch strings.TrimSpace(strings.ToLower(out)) {
	case "device":
		return ADBConnected
	case "unauthorized":
		return ADBUnauthorized
	case "offline":
		return ADBOffline
	}
	return ADBUnknown
}

// Connect attaches the device. An unauthorized device returns ErrUnauthorized
// immediately: the caller shows the on-device prompt and then calls
// WaitAuthorized, instead of blocking a screen silently.
func (a *ADB) Connect(ctx context.Context) error {
	out, err := a.exec(ctx, ConnectArgs(a.Address))
	if err != nil {
		return fmt.Errorf("adb connect: %w (is Android platform-tools installed?)", err)
	}
	state := ParseConnectOutput(out)
	a.logf("adb %s: %s", a.Address, firstNonEmptyLine(out))
	switch state {
	case ADBUnauthorized:
		return ErrUnauthorized
	case ADBFailed:
		return fmt.Errorf("%w: %s", ErrUnreachable, firstNonEmptyLine(out))
	}
	return a.checkState(ctx)
}

// WaitAuthorized polls until the device trusts this host or ctx expires. The
// caller is expected to explain the on-device "Allow USB debugging?" prompt.
func (a *ADB) WaitAuthorized(ctx context.Context) error {
	interval := a.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		err := a.checkState(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrUnreachable) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: still unauthorized after waiting; accept the on-device debugging prompt and reach the same network", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (a *ADB) checkState(ctx context.Context) error {
	out, err := a.exec(ctx, GetStateArgs(a.Address))
	if err != nil {
		return classifyADBStateFailure(out, err)
	}
	switch ParseStateOutput(out) {
	case ADBConnected:
		return nil
	case ADBUnauthorized:
		return ErrUnauthorized
	case ADBOffline:
		return fmt.Errorf("%w: adb reports offline", ErrUnreachable)
	default:
		return fmt.Errorf("%w: adb get-state: %s", ErrUnreachable, strings.TrimSpace(out))
	}
}

// classifyADBStateFailure reads the non-zero `adb get-state` case. An
// unauthorized device exits non-zero with "unauthorized" on stdout, so the
// status text, not the exit alone, decides whether to wait for the prompt.
func classifyADBStateFailure(out string, err error) error {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "unauthorized"):
		return ErrUnauthorized
	case strings.Contains(low, "offline"):
		return fmt.Errorf("%w: adb reports offline", ErrUnreachable)
	default:
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
}

// adbExitMarker carries the remote exit status. This adbd (and several stock
// builds) returns 0 from `adb shell` even when the command fails, so every
// captured command is wrapped and the status is read from stdout.
const adbExitMarker = "__CAPSULAD_EXIT__"

func (a *ADB) Run(ctx context.Context, cmd Command) (Result, error) {
	if len(cmd.Args) == 0 {
		return Result{}, errors.New("adb: empty command")
	}
	if cmd.Stdout != nil {
		return a.run(ctx, a.commandArgs(ctx, cmd), cmd)
	}
	script := deviceShellChecked(cmd.Args)
	return a.runChecked(ctx, cmd, script)
}

// deviceShellChecked wraps an argv so its numeric status is printed after the
// command's own output. It is used only by the ADB transport; SSH propagates
// exit status natively.
func deviceShellChecked(argv []string) string {
	return ShellJoin(argv) + "; __capsulad_rc=$?; printf '\n" + adbExitMarker + "%s\n' \"$__capsulad_rc\""
}

// runChecked runs the wrapped command and splits its exit marker from stdout.
func (a *ADB) runChecked(ctx context.Context, cmd Command, script string) (Result, error) {
	res, err := a.run(ctx, ShellArgs(a.Address, []string{"sh", "-c", script}), cmd)
	if err != nil {
		return res, err
	}
	stdout, code, ok := splitExitMarker(res.Stdout)
	if !ok {
		return res, fmt.Errorf("adb: command did not report its exit status")
	}
	res.Stdout = stdout
	res.ExitCode = code
	return res, nil
}

func splitExitMarker(out []byte) ([]byte, int, bool) {
	marker := []byte("\n" + adbExitMarker)
	idx := bytes.LastIndex(out, marker)
	if idx < 0 {
		return out, 0, false
	}
	rest := out[idx+len(marker):]
	if end := bytes.IndexByte(rest, '\n'); end >= 0 {
		rest = rest[:end]
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(rest)))
	if err != nil {
		return out, 0, false
	}
	return out[:idx], code, true
}

// commandArgs chooses the stream transport and keeps `shell` for captured
// output, which carries the remote exit status.
func (a *ADB) commandArgs(ctx context.Context, cmd Command) []string {
	if cmd.Stdout == nil {
		return ShellArgs(a.Address, cmd.Args)
	}
	return a.streamArgs(a.supportsExecOut(ctx), cmd.Args)
}

// streamArgs is the pure transport choice: exec-out when adbd supports it,
// otherwise a raw pty shell.
func (a *ADB) streamArgs(execOut bool, argv []string) []string {
	if execOut {
		return ExecOutArgs(a.Address, argv)
	}
	return ShellRawArgs(a.Address, argv)
}

// supportsExecOut probes once. Live Rockchip stock adbd rejects both exec-out
// and non-pty shells, so the answer must come from the device rather than the
// adb version.
func (a *ADB) supportsExecOut(ctx context.Context) bool {
	if a.execOut != nil {
		return *a.execOut
	}
	_, err := a.exec(ctx, []string{"-s", a.Address, "exec-out", "true"})
	if ctx.Err() != nil {
		return false
	}
	ok := err == nil
	a.execOut = &ok
	if !ok {
		a.logf("adb exec-out is unavailable on this device; streaming through a raw pty shell")
	}
	return ok
}

func (a *ADB) Push(ctx context.Context, local, remote string) error {
	res, err := a.run(ctx, PushArgs(a.Address, local, remote), Command{})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &CommandError{Args: []string{"adb", "push"}, ExitCode: res.ExitCode, Stderr: string(res.Stderr)}
	}
	return nil
}

func (a *ADB) exec(ctx context.Context, args []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	out := &limitedBuffer{limit: MaxTextRead}
	c := exec.CommandContext(cctx, a.Path, args...)
	c.Stdout = out
	c.Stderr = out
	err := c.Run()
	if out.overflow {
		return string(out.Bytes()), fmt.Errorf("%w (adb, %d byte cap)", ErrOutputLimit, MaxTextRead)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out.Bytes()), fmt.Errorf("adb exited %d: %s", exit.ExitCode(), strings.TrimSpace(string(out.Bytes())))
		}
		return string(out.Bytes()), err
	}
	return string(out.Bytes()), nil
}

func (a *ADB) run(ctx context.Context, args []string, cmd Command) (Result, error) {
	c := exec.CommandContext(ctx, a.Path, args...)
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	stderrBuf := &limitedBuffer{limit: MaxTextRead}
	stdout := &limitedBuffer{limit: cmd.MaxStdout}
	if cmd.Stdout != nil {
		c.Stdout = cmd.Stdout
	} else {
		c.Stdout = stdout
	}
	if cmd.Stderr != nil {
		c.Stderr = cmd.Stderr
	} else {
		c.Stderr = stderrBuf
	}
	err := c.Run()
	if stdout.overflow {
		return Result{}, fmt.Errorf("%w (adb, %d byte cap)", ErrOutputLimit, cmd.MaxStdout)
	}
	if stderrBuf.overflow {
		return Result{}, fmt.Errorf("%w (adb stderr, %d byte cap)", ErrOutputLimit, MaxTextRead)
	}
	res := Result{Stdout: stdout.Bytes(), Stderr: stderrBuf.Bytes()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("adb: %w", err)
	}
	return res, nil
}

func (a *ADB) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return adbDefaultTimeout
}

func (a *ADB) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(format, args...)
	}
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
