package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/daemon"
)

// checkHealth is the daemon's own readiness probe for boot services and the
// upgrade path. It exits 0 when the framework API answers, 1 when it does not.
//
// A pre-upgrade binary does not know -health, so flag parsing exits 2. The boot
// service treats 2 as "running but not probeable" only while a capsulad process
// exists, which is what makes the first upgrade safe.
func checkHealth(cfg daemon.Config) int {
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = ".capsulad"
	}
	socket := cfg.SocketPath
	if socket == "" {
		runDir := cfg.RunDir
		if runDir == "" {
			runDir = filepath.Join(dataDir, "run")
		}
		socket = filepath.Join(runDir, "hal.sock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("passthrough:///capsulad",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "capsulad health: %v\n", err)
		return 1
	}
	defer conn.Close()
	st, err := frameworkv1.NewSystemServiceClient(conn).GetStatus(ctx, &frameworkv1.GetStatusRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "capsulad health: daemon is not serving %s: %v\n", socket, err)
		return 1
	}
	fmt.Printf("capsulad healthy: version=%s uptime=%s\n",
		st.GetDaemonVersion(), time.Duration(st.GetUptimeMs())*time.Millisecond)
	return 0
}
