// Package remote is the websocket link to an OpenCapsule server. It dials
// through capsulad.OpenTCP (verified TLS for wss) and the shared RFC 6455
// client, so the app binary never bundles net/http or crypto/tls.
package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"capsulad/apps/opencapsula/internal/config"
	"capsulad/apps/opencapsula/internal/protocol"
	"capsulad/sdk/capsulad"
	"capsulad/sdk/websocket"
)

const (
	dialTimeout    = 15 * time.Second
	handshakeLimit = 10 * time.Second
	writeTimeout   = 5 * time.Second
	maxMessageSize = 4 << 20
)

// ErrReadyTimeout means the server accepted the socket but never acknowledged
// the hello.
var ErrReadyTimeout = errors.New("remote: server did not send ready")

// Link is one established connection.
type Link struct {
	stream *capsulad.TCPStream
	conn   *websocket.Conn
	ready  protocol.Ready
}

// Dial opens the configured endpoint, sends the device hello and waits for
// ready. The returned link owns the stream; every failure closes it.
func Dial(ctx context.Context, env capsulad.Env, cfg config.Config, hello *protocol.Message) (*Link, error) {
	endpoint, err := config.ParseEndpoint(cfg.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	stream, err := capsulad.OpenTCP(dialCtx, env, endpoint.Host, endpoint.Port, capsulad.TCPOptions{
		TLS:        endpoint.TLS,
		Roots:      "system",
		ServerName: endpoint.Host,
	})
	if err != nil {
		return nil, fmt.Errorf("remote: connect %s: %w", endpoint.Host, err)
	}
	conn, err := websocket.Dial(dialCtx, stream, websocket.Options{
		Host:            endpoint.HostHeader,
		Path:            endpoint.Path,
		Token:           cfg.Token,
		MaxMessageBytes: maxMessageSize,
	})
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	link := &Link{stream: stream, conn: conn}
	if err := link.send(dialCtx, hello); err != nil {
		link.CloseNow()
		return nil, err
	}
	deadline := time.Now().Add(handshakeLimit)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			link.CloseNow()
			return nil, ErrReadyTimeout
		}
		readCtx, readCancel := context.WithTimeout(ctx, remaining)
		op, payload, err := conn.ReadMessage(readCtx)
		readCancel()
		if err != nil {
			link.CloseNow()
			return nil, fmt.Errorf("remote: waiting for ready: %w", err)
		}
		if op != websocket.OpText {
			continue
		}
		message, err := protocol.Decode(payload)
		if err != nil {
			link.CloseNow()
			return nil, fmt.Errorf("remote: %w", err)
		}
		switch message.Type {
		case protocol.TypeReady:
			if err := message.Ready.OutputFormat.Validate(); err != nil {
				link.CloseNow()
				return nil, fmt.Errorf("remote: server output format: %w", err)
			}
			link.ready = *message.Ready
			return link, nil
		case protocol.TypeError:
			link.CloseNow()
			return nil, fmt.Errorf("remote: server refused hello: %s: %s",
				message.Error.Code, message.Error.Message)
		}
	}
}

// Ready is the accepted session description.
func (l *Link) Ready() protocol.Ready { return l.ready }

// Read returns the next text or binary frame.
func (l *Link) Read(ctx context.Context) (websocket.Opcode, []byte, error) {
	return l.conn.ReadMessage(ctx)
}

// SendMessage writes one JSON text frame.
func (l *Link) SendMessage(ctx context.Context, message *protocol.Message) error {
	return l.send(ctx, message)
}

func (l *Link) send(ctx context.Context, message *protocol.Message) error {
	payload, err := protocol.Encode(message)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := l.conn.WriteText(writeCtx, payload); err != nil {
		return fmt.Errorf("remote: send %s: %w", message.Type, err)
	}
	return nil
}

// SendAudio writes one raw PCM binary frame.
func (l *Link) SendAudio(ctx context.Context, pcm []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := l.conn.WriteBinary(writeCtx, pcm); err != nil {
		return fmt.Errorf("remote: send audio: %w", err)
	}
	return nil
}

// Ping sends a websocket ping.
func (l *Link) Ping(ctx context.Context) error { return l.conn.WritePing(ctx, nil) }

// SetPongHandler installs the keepalive callback.
func (l *Link) SetPongHandler(handler func([]byte)) { l.conn.SetPongHandler(handler) }

// CloseNow drops the stream without a close handshake. The link is closed this
// way on every teardown: a dead peer must not delay the reconnect.
func (l *Link) CloseNow() { _ = l.conn.CloseNow() }
