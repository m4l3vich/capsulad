//go:build !linux

package grpcpeer

import "net"

func peerCreds(c net.Conn) (pid, uid int, ok bool) { return 0, 0, false }
