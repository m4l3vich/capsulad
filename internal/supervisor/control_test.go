package supervisor

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The launch plumbing must hand the child a real fd 3 and tell it the number
// through CAPSULAD_CONTROL_FD. A message-oriented AF_UNIX pair keeps the check
// portable between the Linux device and the host.
func TestControlChannelPassesFDAndEnv(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(fds[0]), "control-parent")
	child := os.NewFile(uintptr(fds[1]), "control-child")
	defer parent.Close()
	if err := unix.SetsockoptTimeval(fds[0], unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 5}); err != nil {
		t.Fatal(err)
	}

	spawner := &testSpawner{args: []string{"/bin/sh", "-c", `printf "%s" "$CAPSULAD_CONTROL_FD" >&3`}}
	sup := newSupervisor(t, spawner)
	var aborts atomic.Int32
	sup.SetControlChannel(func(string, string) (*os.File, func(), error) {
		return child, func() { aborts.Add(1) }, nil
	})
	if _, err := sup.Start(testManifest(false), "inv-control"); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 8)
	n, _, _, _, err := unix.Recvmsg(fds[0], buf, nil, 0)
	if err != nil {
		t.Fatalf("read control message: %v", err)
	}
	if string(buf[:n]) != "3" {
		t.Fatalf("CAPSULAD_CONTROL_FD = %q, want 3", buf[:n])
	}
	if got := aborts.Load(); got != 0 {
		t.Fatalf("abort called %d times on a successful start", got)
	}
}

// A failed spawn must release the daemon side through the abort hook instead
// of leaving a channel behind.
func TestControlChannelAbortOnFailedSpawn(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(fds[0]), "control-parent")
	child := os.NewFile(uintptr(fds[1]), "control-child")
	defer parent.Close()

	spawner := &testSpawner{args: []string{"/nonexistent/capsulad-test-binary"}}
	sup := newSupervisor(t, spawner)
	aborted := make(chan struct{}, 1)
	sup.SetControlChannel(func(string, string) (*os.File, func(), error) {
		return child, func() { aborted <- struct{}{} }, nil
	})
	if _, err := sup.Start(testManifest(false), "inv-control"); err == nil {
		t.Fatal("start unexpectedly succeeded")
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("abort hook was not called for a failed spawn")
	}
}
