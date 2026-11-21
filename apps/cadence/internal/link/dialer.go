package link

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"capsulad/sdk/capsulad"

	"capsulad/apps/cadence/internal/settings"
	"capsulad/apps/cadence/internal/websocket"
)

// Conn is one established link: the SDK's verified TLS stream after the
// websocket upgrade. Tests substitute an in-process peer.
type Conn interface {
	ReadMessage(ctx context.Context) (websocket.Opcode, []byte, error)
	WriteBinary(ctx context.Context, payload []byte) error
	WritePing(ctx context.Context, payload []byte) error
	SetPongHandler(func(payload []byte))
	CloseNow() error
}

// Dialer opens and upgrades one link. It is the only network surface the link
// uses: production is *SDKDialer, tests an in-process peer.
type Dialer interface {
	Dial(ctx context.Context) (Conn, error)
}

// SDKDialer dials <base>/v2/device through the shared SDK's verified TLS
// stream. It never bundles net/http or crypto/tls: the handshake bytes go over
// capsulad.OpenTCP, and no redirect or plaintext downgrade is possible.
type SDKDialer struct {
	Env capsulad.Env
	// Config returns the current validated configuration, so a settings change
	// takes effect on the next dial without rebuilding the client.
	Config func() settings.Config
	// ConnectTimeout bounds DNS + TCP + TLS + the websocket handshake.
	ConnectTimeout time.Duration
}

// ErrNoServer means the configuration has no base URL yet.
var ErrNoServer = errors.New("link: no Cadence server configured")

// Dial opens the TLS connection and performs the websocket upgrade.
func (d *SDKDialer) Dial(ctx context.Context) (Conn, error) {
	cfg := settings.Config{}
	if d.Config != nil {
		cfg = d.Config()
	}
	if cfg.BaseURL == "" || cfg.Token == "" {
		// An enrollment code is not a bearer credential: the link stays down
		// until the exchange stored a token.
		return nil, ErrNoServer
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Hostname() == "" {
		// The settings validator already accepted it; a value that no longer
		// parses is a programming error, not a network condition.
		return nil, fmt.Errorf("link: invalid base URL")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNum, err := parsePort(port)
	if err != nil {
		return nil, err
	}
	path := strings.TrimRight(u.Path, "/") + "/v2/device"

	if d.ConnectTimeout <= 0 {
		d.ConnectTimeout = 15 * time.Second
	}
	// Only a context deadline bounds the native dial, so supply one even when
	// the caller's context has none.
	dialCtx, cancel := context.WithTimeout(ctx, d.ConnectTimeout)
	defer cancel()

	stream, err := capsulad.OpenTCP(dialCtx, d.Env, u.Hostname(), portNum, capsulad.TCPOptions{
		TLS:        u.Scheme == "https",
		Roots:      "system",
		CAPEM:      cfg.CAPEM,
		ServerName: u.Hostname(),
	})
	if err != nil {
		return nil, fmt.Errorf("link: connect: %w", err)
	}
	conn, err := websocket.Dial(dialCtx, stream, websocket.Options{
		Host:  u.Host,
		Path:  path,
		Token: cfg.Token,
	})
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	return conn, nil
}

func parsePort(s string) (uint16, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' || n > 65535 {
			return 0, fmt.Errorf("link: invalid port")
		}
		n = n*10 + int(r-'0')
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("link: invalid port")
	}
	return uint16(n), nil
}
