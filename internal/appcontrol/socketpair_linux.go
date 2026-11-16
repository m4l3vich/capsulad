//go:build linux

package appcontrol

import "golang.org/x/sys/unix"

// controlSocketPair creates the contract channel: AF_UNIX SOCK_SEQPACKET. It
// preserves one message per send and carries SCM_RIGHTS.
func controlSocketPair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
}
