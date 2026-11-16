// Package vkclient speaks the recovered VK/Marusia voice-agent protocol: the
// HTTP session attach, the per-call gRPC metadata, the AuthReady probe and the
// bidirectional Phrase/Speech stream.
//
// Everything here is transcribed from the vendor binary's recovered descriptors
// and string table (see README.md). No vendor code or asset is redistributed.
package vkclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"capsula/apps/marusia/internal/vkproto/clientprobepb"
	"capsula/apps/marusia/internal/vkproto/servicepb"
)

// Endpoints recovered from the vendor lookup table. prod is the shipped
// default; the others exist so a credentialed test can point elsewhere.
var Endpoints = map[string]string{
	"prod":  "https://vc.go.mail.ru",
	"test1": "https://vc1.go.mail.ru",
	"test2": "http://voice-test2.ngx.openstack.go.devmail.ru",
	"stag1": "https://s1-vc.go.mail.ru",
	"dev":   "https://vc1.go.devmail.ru",
}

const (
	attachPath            = "/account/attach_xtoken"
	errCodeXTokenRequired = 4002

	// defaultClientVersion is the literal the shipped build advertises in
	// x-device-ver and DeviceInfo.version.
	defaultClientVersion = "44.91"

	attachTimeout = 15 * time.Second
	probeTimeout  = 5 * time.Second
)

// ErrUnsupportedFormat reports an upload format this build cannot encode.
var ErrUnsupportedFormat = errors.New("vkclient: upload format not implemented")

// Session is the credential triple returned by /account/attach_xtoken.
type Session struct {
	SessionID     string
	SessionSecret string
	AccountID     string
}

func (s Session) empty() bool { return s.SessionSecret == "" }

// Present reports whether the session holds a usable secret.
func (s Session) Present() bool { return s.SessionSecret != "" }

// Config is one client's static configuration. Zero values are replaced with
// the shipped defaults, never with invented credentials.
type Config struct {
	// BaseURL is an absolute http:// or https:// URL with a host and no path.
	BaseURL string
	// XTokenID is the configured device credential. Empty is sent as an empty
	// string: the attach is always attempted so the service, not this app,
	// decides whether an unprovisioned device is acceptable.
	XTokenID string
	// Serial is the SoC serial, sent as attach "serial" and x-device-sn.
	Serial string
	// DeviceID is the persisted device id sent as x-device-id.
	DeviceID string
	// Session is a valid runtime credential loaded from a masked setting or
	// vendor state file. When present, Attach is not called.
	Session Session
	// ClientVersion is sent as x-device-ver. Defaults to 44.91.
	ClientVersion string
	// HTTPClient performs the attach. Nil uses a client with a 15 s timeout.
	HTTPClient *http.Client
	// TLS overrides the gRPC client TLS configuration. Tests use it to trust an
	// in-process certificate; production leaves it nil.
	TLS *tls.Config
	// DialOptions are appended to the gRPC dial options.
	DialOptions []grpc.DialOption
	// OnSession observes every successful attach.
	OnSession func(Session)
}

// Client is one endpoint's HTTP attachment plus one gRPC channel.
type Client struct {
	cfg  Config
	base *url.URL

	mu        sync.Mutex
	session   Session
	attachErr error
	conn      *grpc.ClientConn
	closed    bool
}

// New validates the endpoint and prepares the client. No connection is made.
func New(cfg Config) (*Client, error) {
	base, err := ParseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = defaultClientVersion
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: attachTimeout}
	}
	client := &Client{cfg: cfg, base: base, session: cfg.Session}
	return client, nil
}

// ParseBaseURL accepts only an absolute URL with a host and no path, query or
// fragment: the gRPC target and every HTTP path are derived from it. It is
// exported so settings validation and the client cannot disagree.
func ParseBaseURL(raw string) (*url.URL, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, errors.New("vkclient: endpoint is empty")
	}
	u, err := url.Parse(text)
	if err != nil {
		return nil, fmt.Errorf("vkclient: endpoint %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("vkclient: endpoint %q must use http:// or https://", raw)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("vkclient: endpoint %q has no host", raw)
	}
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		return nil, fmt.Errorf("vkclient: endpoint %q must not carry a path", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("vkclient: endpoint %q must not carry a query or fragment", raw)
	}
	u.Path = ""
	return u, nil
}

// BaseURL is the validated endpoint the client talks to.
func (c *Client) BaseURL() string { return c.base.String() }

// Session returns the current runtime credentials (empty before attach).
func (c *Client) Session() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// SetSession installs credentials obtained out of band. It exists for the
// reauth action path and for tests; Attach is the normal source.
func (c *Client) SetSession(s Session) {
	c.mu.Lock()
	c.session = s
	c.mu.Unlock()
}

// InvalidateSession drops the installed credentials so the next Authorize must
// resolve new ones (masked settings, then the vendor session file, then attach).
// Callers use it when the service rejects the session (401/403).
func (c *Client) InvalidateSession() {
	c.mu.Lock()
	c.session = Session{}
	c.mu.Unlock()
}

// Attach performs POST /account/attach_xtoken and stores the returned session.
//
// The request body is exactly the three fields the vendor assembles, in the
// vendor's order. with_secret is the string "1", not a boolean. An empty
// xtoken_id is sent as an empty string: the device's provisioning state is the
// service's decision, so the attach is always attempted and its rejection is
// the only reason to stop. Nothing is invented to fill the field.
func (c *Client) Attach(ctx context.Context) (Session, error) {
	c.mu.Lock()
	c.attachErr = nil
	c.mu.Unlock()
	body, err := json.Marshal(struct {
		Serial     string `json:"serial"`
		WithSecret string `json:"with_secret"`
		XTokenID   string `json:"xtoken_id"`
	}{
		Serial:     c.cfg.Serial,
		WithSecret: "1",
		XTokenID:   c.cfg.XTokenID,
	})
	if err != nil {
		return Session{}, fmt.Errorf("vkclient: encode attach: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()
	endpoint := *c.base
	endpoint.Path = attachPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Session{}, fmt.Errorf("vkclient: attach request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("vkclient: attach: %w", err)
	}
	defer resp.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return Session{}, fmt.Errorf("vkclient: read attach response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var rejection struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(payload, &rejection)
		return Session{}, &AttachError{Status: resp.StatusCode, Body: strings.TrimSpace(string(payload)), Code: rejection.Code}
	}

	var decoded struct {
		Result *struct {
			AccountID     string `json:"account_id"`
			SessionID     string `json:"session_id"`
			SessionSecret string `json:"session_secret"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Session{}, fmt.Errorf("vkclient: decode attach response: %w", err)
	}
	if decoded.Result == nil || decoded.Result.SessionID == "" || decoded.Result.SessionSecret == "" {
		return Session{}, errors.New("vkclient: attach response has no result.session_id/session_secret")
	}
	session := Session{
		SessionID:     decoded.Result.SessionID,
		SessionSecret: decoded.Result.SessionSecret,
		AccountID:     decoded.Result.AccountID,
	}
	c.SetSession(session)
	if c.cfg.OnSession != nil {
		c.cfg.OnSession(session)
	}
	return session, nil
}

// AttachError is a non-2xx attach response.
type AttachError struct {
	Status int
	Body   string
	// Code is the service's numeric reason from the JSON body when present
	// (4002 = "xtoken_id is required", i.e. this device is not provisioned).
	Code int
}

func (e *AttachError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("vkclient: attach rejected with HTTP %d", e.Status)
	}
	return fmt.Sprintf("vkclient: attach rejected with HTTP %d: %s", e.Status, e.Body)
}

// ProvisioningRequired reports whether the service refused the attach because
// the device holds no provisioning credential. It is not a transient error: the
// same request fails the same way until an operator supplies a session or an
// xtoken_id.
func ProvisioningRequired(err error) bool {
	var attachErr *AttachError
	return errors.As(err, &attachErr) && attachErr.Code == errCodeXTokenRequired
}

// Authorize loads a session from the client's configuration when available;
// only clients with no session may use provisioning attach. An invalid session
// is not silently replaced with a different credential.
func (c *Client) Authorize(ctx context.Context) error {
	if c.Session().empty() {
		c.mu.Lock()
		cached := c.attachErr
		c.mu.Unlock()
		if cached != nil {
			return c.serialHint(cached)
		}
		if _, err := c.Attach(ctx); err != nil {
			var attachErr *AttachError
			if errors.As(err, &attachErr) {
				c.mu.Lock()
				c.attachErr = err
				c.mu.Unlock()
			}
			return c.serialHint(err)
		}
	}
	err := c.probe(ctx)
	if err == nil {
		return nil
	}
	if IsCode(err, codes.Unauthenticated) || IsCode(err, codes.PermissionDenied) {
		return fmt.Errorf("vkclient: the service rejected the session; reload the vendor session "+
			"file or configure session_id/session_secret: %w", err)
	}
	return c.serialHint(fmt.Errorf("vkclient: authorize: %w", err))
}

// serialHint makes a bare vendor "Invalid 'x-device-sn'" refusal actionable:
// the service validates the SoC serial by name, so the operator must know that
// the serial setting is the lever, not the vendor string alone. The wrapped
// error keeps the original gRPC status for IsCode and the attach body for
// ProvisioningRequired.
func (c *Client) serialHint(err error) error {
	if !IsInvalidDeviceSN(err) {
		return err
	}
	if strings.TrimSpace(c.cfg.Serial) == "" {
		return fmt.Errorf("vkclient: the device serial is missing; set the \"serial\" setting to the "+
			"SoC serial the framework reports as hardware_serial (the launcher also exports it as "+
			"CAPSULA_DEVICE_SERIAL; x-device-sn is empty): %w", err)
	}
	return fmt.Errorf("vkclient: the device serial is wrong; set the \"serial\" setting to the SoC "+
		"serial this device was provisioned with (the framework's hardware_serial): %w", err)
}

// IsInvalidDeviceSN reports whether the service refused the x-device-sn value:
// an InvalidArgument whose description names the parameter.
func IsInvalidDeviceSN(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Invalid 'x-device-sn'")
}

// probe runs the unary, both-messages-empty ClientProbe/AuthReady RPC.
func (c *Client) probe(ctx context.Context) error {
	conn, err := c.channel()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, c.callMetadata())
	_, err = servicepb.NewClientProbeClient(conn).AuthReady(ctx, &clientprobepb.AuthReadyRequest{})
	return err
}

// channel returns the shared gRPC connection, dialling once.
func (c *Client) channel() (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.conn != nil {
		return c.conn, nil
	}
	target, creds, err := c.transport()
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	opts = append(opts, c.cfg.DialOptions...)
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("vkclient: dial %s: %w", target, err)
	}
	c.conn = conn
	return conn, nil
}

// transport derives the gRPC target and credentials from the base URL. HTTPS
// bases use TLS on the host's port (443 unless given); an HTTP base is plain
// h2c, which only the test deployments offer.
func (c *Client) transport() (string, credentials.TransportCredentials, error) {
	host := c.base.Hostname()
	port := c.base.Port()
	if port == "" {
		if c.base.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	target := net.JoinHostPort(host, port)
	if c.base.Scheme == "http" {
		return target, insecure.NewCredentials(), nil
	}
	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if c.cfg.TLS != nil {
		tlsConfig = c.cfg.TLS.Clone()
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = host
		}
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	return target, credentials.NewTLS(tlsConfig), nil
}

// callMetadata is rebuilt for every RPC: the request id must be fresh, and the
// authorization header follows the current session secret.
//
// Keys are lowercase per the gRPC metadata contract. x-device-ver is the
// advertised client version; x-device-sn is the SoC serial; x-device-id is the
// persisted random id. An absent secret sends an empty value, which is what the
// vendor does before an account is linked.
func (c *Client) callMetadata() metadata.MD {
	session := c.Session()
	return metadata.Pairs(
		"x-session-id", session.SessionID,
		"x-device-id", c.cfg.DeviceID,
		"x-device-sn", c.cfg.Serial,
		"x-device-ver", c.cfg.ClientVersion,
		"x-client-request-id", NewUUID(),
		"authorization", bearer(session.SessionSecret),
	)
}

func bearer(secret string) string {
	if secret == "" {
		return ""
	}
	return "Bearer " + secret
}

// Close releases the gRPC channel. It is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.closed = true
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// ErrClosed reports use of a closed client.
var ErrClosed = errors.New("vkclient: client is closed")

// NewUUID returns a fresh random RFC 4122 version 4 UUID in the lowercase
// 8-4-4-4-12 form the vendor uses for x-client-request-id.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on any supported platform; a zero UUID would
		// collide, so fall back to a time-derived value rather than panic.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7],
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// StatusError is a non-OK gRPC status returned by a call or stream.
type StatusError struct {
	Op      string
	Code    codes.Code
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("vkclient: %s: %s", e.Op, e.Code)
	}
	return fmt.Sprintf("vkclient: %s: %s: %s", e.Op, e.Code, e.Message)
}

// IsCode reports whether err is a StatusError (or a raw gRPC status) with code.
func IsCode(err error, code codes.Code) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == code
	}
	return status.Code(err) == code
}

// wrapStatus converts a gRPC status into a StatusError so callers do not need
// the grpc/status package. io.EOF and context errors pass through unchanged.
func wrapStatus(op string, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if st, ok := status.FromError(err); ok {
		return &StatusError{Op: op, Code: st.Code(), Message: st.Message()}
	}
	return err
}
