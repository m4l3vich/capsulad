package wifi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Process is one owned long-running child (wpa_supplicant or dnsmasq). Closing
// the manager always signals the process group, waits a bounded time and then
// kills it, so a cancelled daemon cannot leave a radio or DHCP server behind.
type Process interface {
	Wait() error
	Signal(os.Signal) error
	Kill() error
	Output() string
}

// CommandRunner is the test seam around the target's stock tooling. The real
// implementation runs stock wpa_supplicant/wpa_cli/ip/dnsmasq; tests script the
// same sequences without touching a radio.
type CommandRunner interface {
	LookPath(file string) (string, error)
	Run(ctx context.Context, name string, args ...string) (string, error)
	Start(ctx context.Context, name string, args ...string) (Process, error)
}

type execRunner struct{}

func (execRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s: %w: %s", name, err, text)
		}
		return text, fmt.Errorf("%s: %w", name, err)
	}
	return text, nil
}

func (execRunner) Start(ctx context.Context, name string, args ...string) (Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 3 * time.Second
	buf := &boundedBuffer{limit: 4096}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &commandProcess{cmd: cmd, buf: buf, done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

type commandProcess struct {
	cmd     *exec.Cmd
	buf     *boundedBuffer
	done    chan struct{}
	waitErr error
	once    sync.Once
}

func (p *commandProcess) Wait() error {
	<-p.done
	return p.waitErr
}

func (p *commandProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}
	unixSig, ok := sig.(syscall.Signal)
	if !ok {
		return p.cmd.Process.Signal(sig)
	}
	return syscall.Kill(-p.cmd.Process.Pid, unixSig)
}

func (p *commandProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

func (p *commandProcess) Output() string { return p.buf.String() }

type boundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.limit > 0 && b.buf.Len()+len(p) > b.limit {
		drop := b.buf.Len() + len(p) - b.limit
		existing := b.buf.Bytes()
		if drop >= len(existing) {
			b.buf.Reset()
		} else {
			kept := append([]byte(nil), existing[drop:]...)
			b.buf.Reset()
			b.buf.Write(kept)
		}
	}
	b.buf.Write(p)
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}
