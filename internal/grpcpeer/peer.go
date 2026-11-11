// Package grpcpeer extracts local peer credentials for gRPC connections over a
// Unix domain socket.
//
// On Linux the accepted socket's SO_PEERCRED identifies the connecting process
// and UID; that is bookkeeping used to verify that a bus connection really
// comes from the process the supervisor launched. It is not a permission
// model. Other platforms report no credentials and rely on the unguessable run
// ID plus the development sandbox mode.
package grpcpeer

import (
	"context"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc/peer"
)

type Creds struct {
	PID int
	UID int
	OK  bool
}

type credKey struct{}

// Listener wraps a Unix listener and tags each connection's RemoteAddr with
// peer credentials so gRPC places them in the peer context.
type Listener struct {
	net.Listener
}

func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pid, uid, ok := peerCreds(c)
	if !ok {
		return c, nil
	}
	return &taggedConn{Conn: c, pid: pid, uid: uid}, nil
}

type taggedConn struct {
	net.Conn
	pid int
	uid int
}

func (c *taggedConn) RemoteAddr() net.Addr {
	return &net.UnixAddr{Name: "peercred:" + strconv.Itoa(c.pid) + ":" + strconv.Itoa(c.uid), Net: "unix"}
}

// FromContext returns credentials tagged by Listener or captured by a
// platform interceptor.
func FromContext(ctx context.Context) Creds {
	if v, ok := ctx.Value(credKey{}).(Creds); ok {
		return v
	}
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return Creds{}
	}
	name := p.Addr.String()
	if !strings.HasPrefix(name, "peercred:") {
		return Creds{}
	}
	parts := strings.Split(strings.TrimPrefix(name, "peercred:"), ":")
	if len(parts) != 2 {
		return Creds{}
	}
	pid, err1 := strconv.Atoi(parts[0])
	uid, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return Creds{}
	}
	return Creds{PID: pid, UID: uid, OK: true}
}

// WithCreds is used by tests and by platforms that capture credentials before
// gRPC context construction.
func WithCreds(ctx context.Context, c Creds) context.Context {
	return context.WithValue(ctx, credKey{}, c)
}
