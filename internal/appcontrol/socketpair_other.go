//go:build !linux

package appcontrol

import "golang.org/x/sys/unix"

// controlSocketPair uses AF_UNIX SOCK_DGRAM off Linux. Darwin and the BSDs do
// not implement SOCK_SEQPACKET; SOCK_DGRAM is the closest equivalent that
// preserves one message per send and carries SCM_RIGHTS, so host tests and the
// simulator exercise the same protocol. Device builds always use the
// contract's SOCK_SEQPACKET.
func controlSocketPair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
}
