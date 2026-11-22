package device

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH is a Device over golang.org/x/crypto/ssh. Host keys are accepted on
// first use after an explicit prompt and then pinned; a later mismatch always
// fails the connection.
type SSH struct {
	Address   string
	User      string
	Password  string
	KeyPath   string
	Known     *KnownHosts
	Prompt    func(host, fingerprint string) bool
	Timeout   time.Duration
	Log       func(string, ...any)
	client    *ssh.Client
	connected bool
}

// NewSSH builds an SSH transport. The user defaults to root on the Alpine
// image, which runs no other account for this purpose.
func NewSSH(addr, user string) *SSH {
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}
	return &SSH{Address: addr, User: user, Timeout: 15 * time.Second}
}

func (s *SSH) Addr() string { return s.Address }
func (s *SSH) Kind() string { return "ssh" }

// Connect dials and authenticates.
func (s *SSH) Connect(ctx context.Context) error {
	s.Close()
	cfg := &ssh.ClientConfig{
		User:            s.User,
		HostKeyCallback: s.hostKeyCallback(),
		Timeout:         s.timeout(),
	}
	if s.Password != "" {
		cfg.Auth = append(cfg.Auth, ssh.Password(s.Password))
	}
	if s.KeyPath != "" {
		data, err := os.ReadFile(s.KeyPath)
		if err != nil {
			return fmt.Errorf("SSH key: %w", err)
		}
		key, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return fmt.Errorf("SSH key %s: %w", s.KeyPath, err)
		}
		cfg.Auth = append(cfg.Auth, ssh.PublicKeys(key))
	}
	if len(cfg.Auth) == 0 {
		return errors.New("SSH needs a password or a private key")
	}

	d := net.Dialer{Timeout: s.timeout()}
	conn, err := d.DialContext(ctx, "tcp", s.Address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	// NewClientConn has no context; bound the handshake on the socket so a
	// silent peer cannot hold the installer forever.
	_ = conn.SetDeadline(time.Now().Add(s.timeout()))
	c, chans, reqs, err := ssh.NewClientConn(conn, s.Address, cfg)
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		conn.Close()
		if errors.Is(err, ErrHostKey) {
			return err
		}
		return fmt.Errorf("SSH %s: %w", s.Address, err)
	}
	s.client = ssh.NewClient(c, chans, reqs)
	s.connected = true
	s.logf("SSH connected to %s as %s", s.Address, s.User)
	return nil
}

func (s *SSH) ensure(ctx context.Context) error {
	if s.client != nil && s.connected {
		return nil
	}
	return s.Connect(ctx)
}

func (s *SSH) Run(ctx context.Context, cmd Command) (Result, error) {
	if len(cmd.Args) == 0 {
		return Result{}, errors.New("ssh: empty command")
	}
	if err := s.ensure(ctx); err != nil {
		return Result{}, err
	}
	sess, err := s.client.NewSession()
	if err != nil {
		s.drop()
		return Result{}, fmt.Errorf("SSH session: %w", err)
	}
	defer sess.Close()
	stderrBuf := &limitedBuffer{limit: MaxTextRead}
	stdout := &limitedBuffer{limit: cmd.MaxStdout}
	if cmd.Stdout != nil {
		sess.Stdout = cmd.Stdout
	} else {
		sess.Stdout = stdout
	}
	if cmd.Stderr != nil {
		sess.Stderr = cmd.Stderr
	} else {
		sess.Stderr = stderrBuf
	}
	if len(cmd.Stdin) > 0 {
		sess.Stdin = bytes.NewReader(cmd.Stdin)
	}
	err = s.runSession(ctx, sess, ShellJoin(cmd.Args))
	if stdout.overflow {
		return Result{}, fmt.Errorf("%w (SSH, %d byte cap)", ErrOutputLimit, cmd.MaxStdout)
	}
	if stderrBuf.overflow {
		return Result{}, fmt.Errorf("%w (SSH stderr, %d byte cap)", ErrOutputLimit, MaxTextRead)
	}
	res := Result{Stdout: stdout.Bytes(), Stderr: stderrBuf.Bytes()}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return res, err
	}
	if err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitStatus()
			return res, nil
		}
		s.drop()
		return res, fmt.Errorf("SSH: %w", err)
	}
	return res, nil
}

// runSession runs one session while honouring ctx. x/crypto/ssh has no
// context-aware Run, so cancellation kills and closes the session.
func (s *SSH) runSession(ctx context.Context, sess *ssh.Session, command string) error {
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		sess.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return ctx.Err()
	}
}

// Push streams a local file to a remote path through the SSH channel.
func (s *SSH) Push(ctx context.Context, local, remote string) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	sess, err := s.client.NewSession()
	if err != nil {
		s.drop()
		return err
	}
	defer sess.Close()
	sess.Stdin = f
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	cmd := "cat > " + ShellQuote(remote)
	err = s.runSession(ctx, sess, cmd)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			return &CommandError{Args: []string{"cat", ">", remote}, ExitCode: exit.ExitStatus(), Stderr: stderr.String()}
		}
		s.drop()
		return fmt.Errorf("SSH push: %w", err)
	}
	return nil
}

func (s *SSH) Close() error {
	s.connected = false
	if s.client != nil {
		err := s.client.Close()
		s.client = nil
		return err
	}
	return nil
}

func (s *SSH) drop() {
	s.connected = false
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

func (s *SSH) hostKeyCallback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if s.Known == nil {
			return fmt.Errorf("%w: no host-key store configured", ErrHostKey)
		}
		status, err := s.Known.Verify(s.Address, key)
		switch status {
		case HostKeyKnown:
			return nil
		case HostKeyMismatch:
			return err
		case HostKeyNew:
			fp := FingerprintSHA256(key)
			if s.Prompt == nil || !s.Prompt(s.Address, fp) {
				return fmt.Errorf("%w: host key for %s was not accepted", ErrHostKey, s.Address)
			}
			return s.Known.Accept(s.Address, key)
		}
		return fmt.Errorf("%w: unknown host-key status", ErrHostKey)
	}
}

func (s *SSH) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 15 * time.Second
}

func (s *SSH) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}
