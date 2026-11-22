package device

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startStallServer accepts SSH connections and never completes an exec
// request, which is how a hung remote command behaves. It is loopback-only.
func startStallServer(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(signer)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					conn.Close()
					return
				}
				defer sconn.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "session" {
						nc.Reject(ssh.UnknownChannelType, "sessions only")
						continue
					}
					ch, chReqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range chReqs {
							if req.Type == "exec" || req.Type == "shell" {
								req.Reply(true, nil)
								continue // deliberately never report exit
							}
							req.Reply(false, nil)
						}
						ch.Close()
					}()
				}
			}()
		}
	}()
	return lis.Addr().String()
}

func newStallSSH(t *testing.T, addr string) *SSH {
	t.Helper()
	known, err := LoadKnownHosts(filepath.Join(t.TempDir(), "known_hosts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewSSH(addr, "root")
	s.Password = "test"
	s.Known = known
	s.Prompt = func(string, string) bool { return true }
	return s
}

func TestSSHContextCancellation(t *testing.T) {
	addr := startStallServer(t)
	s := newStallSSH(t, addr)
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Run(ctx, Command{Args: []string{"sleep", "100"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestSSHPushContextCancellation(t *testing.T) {
	addr := startStallServer(t)
	s := newStallSSH(t, addr)
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	local := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(local, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := s.Push(ctx, local, "/tmp/payload")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Push error = %v, want deadline exceeded", err)
	}
}

func TestSSHHandshakeDeadline(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Hold the socket without speaking SSH.
		time.Sleep(2 * time.Second)
	}()
	s := newStallSSH(t, lis.Addr().String())
	s.Timeout = 200 * time.Millisecond
	start := time.Now()
	if err := s.Connect(context.Background()); err == nil {
		t.Fatal("expected a handshake timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("handshake deadline took %s", elapsed)
	}
}
