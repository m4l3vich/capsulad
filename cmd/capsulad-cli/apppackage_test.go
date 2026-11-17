package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"capsulad/internal/artifact"
)

// The live/offline decision is the security boundary of package
// administration: writing <data-dir>/apps while a daemon is reading it can
// corrupt an installation, and silently refusing when the daemon merely timed
// out hides a real failure. These tests pin which failures prove absence.

// shortSocketPath returns a socket path inside a short temporary directory.
// The Unix socket path limit (104 bytes on macOS) is below what t.TempDir()
// produces for a long test name.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "capcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "hal.sock")
}

func TestDaemonListeningProvesAbsence(t *testing.T) {
	socket := shortSocketPath(t)

	// No socket file: the daemon has never run or cleaned up after itself.
	if listening, err := daemonListening(socket); listening || err != nil {
		t.Fatalf("missing socket = (%v, %v), want (false, nil)", listening, err)
	}

	// A live socket accepts a connection.
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if listening, err := daemonListening(socket); !listening || err != nil {
		t.Fatalf("live socket = (%v, %v), want (true, nil)", listening, err)
	}

	// A socket file left behind by a crashed daemon actively refuses: that is
	// a proven-absent daemon, not an unknown one.
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	if listening, err := daemonListening(socket); listening || err != nil {
		t.Fatalf("closed socket = (%v, %v), want (false, nil)", listening, err)
	}

	// A path that is not a socket has no framework listener either.
	regular := filepath.Join(filepath.Dir(socket), "not-a-socket")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if listening, err := daemonListening(regular); listening || err != nil {
		t.Fatalf("regular file = (%v, %v), want (false, nil)", listening, err)
	}

	// A permission failure proves nothing, so the CLI must refuse to guess.
	if os.Geteuid() != 0 {
		locked := filepath.Join(filepath.Dir(socket), "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if _, err := daemonListening(filepath.Join(locked, "hal.sock")); err == nil {
			t.Fatal("an unreadable socket directory was treated as a proven-absent daemon")
		}
	}
}

func TestInstallUsesOfflinePathOnlyWhenNoDaemonListens(t *testing.T) {
	dataDir := t.TempDir()
	socket := shortSocketPath(t)
	archive := writeCLITestPackage(t, "test.cli.offline")

	if err := installAppPackage(archive, dataDir, socket, ""); err != nil {
		t.Fatalf("offline install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "apps", "test.cli.offline", "manifest.toml")); err != nil {
		t.Fatalf("offline install did not write the package: %v", err)
	}
}

func TestInstallNeverWritesWhileADaemonListens(t *testing.T) {
	dataDir := t.TempDir()
	socket := shortSocketPath(t)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// A listener that is not a framework server still proves a live daemon.
	// The control path must fail there instead of writing the package tree
	// behind a process that may be reading it.
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = lis.Close() })

	archive := writeCLITestPackage(t, "test.cli.live")
	done := make(chan error, 1)
	go func() { done <- installAppPackage(archive, dataDir, socket, "") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("install against a refusing daemon reported success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("install did not return while a daemon was listening; it must not block or fall through")
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "apps", "test.cli.live")); !os.IsNotExist(statErr) {
		t.Fatalf("package was written on disk while a daemon was listening: %v", statErr)
	}
}

// writeCLITestPackage stages a minimal valid package and returns its archive.
func writeCLITestPackage(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := `id = "` + id + `"
name = "CLI test app"
version = "1.0.0"

[entrypoint]
runtime = "native"
path = "app"
architecture = "arm64"
abi = "darwin"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), id+".tar.gz")
	if err := artifact.Write(dir, out); err != nil {
		t.Fatal(err)
	}
	return out
}
