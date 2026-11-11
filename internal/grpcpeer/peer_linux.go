//go:build linux

package grpcpeer

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerCreds(c net.Conn) (pid, uid int, ok bool) {
	uc, isUnix := c.(*net.UnixConn)
	if !isUnix {
		return 0, 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil || cred == nil {
		return 0, 0, false
	}
	return int(cred.Pid), int(cred.Uid), true
}
