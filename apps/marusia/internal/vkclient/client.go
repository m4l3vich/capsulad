// Package vkclient speaks the recovered VK/Marusia voice-agent protocol: the
// HTTP session attach, the per-call gRPC metadata, the AuthReady probe and the
// bidirectional Phrase/Speech stream.
//
// Every network operation goes through the Transport seam (transport.go). The
// production adapter is the shared capsulad SDK, so this package links no
// net/http, crypto/tls or grpc-go; tests inject an in-process adapter.
//
// Everything here is transcribed from the vendor binary's recovered descriptors
// and string table (see README.md). No vendor code or asset is redistributed.
package vkclient

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"capsulad/apps/marusia/internal/redact"
	"capsulad/apps/marusia/internal/vkproto/clientprobepb"
	"capsulad/sdk/capsulad"
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
	// Transport replaces the production native adapter. Tests inject an
	// in-process gRPC/HTTP transport; production leaves it nil. The Client owns
	// the transport and closes it.
	Transport Transport
	// OnSession observes every successful attach.
	OnSession func(Session)
}

// Client is one endpoint's HTTP attachment plus one gRPC channel.
type Client struct {
	cfg       Config
	base      *url.URL
	transport Transport

	mu        sync.Mutex
	session   Session
	attachErr error
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
	transport := cfg.Transport
	if transport == nil {
		transport, err = NewSDKTransport(cfg.BaseURL, nil)
		if err != nil {
			return nil, err
		}
	}
	client := &Client{cfg: cfg, base: base, transport: transport, session: cfg.Session}
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
	// The messages name the endpoint, and a rejected endpoint is echoed into the
	// settings log: the URL is therefore shortened to scheme://host/path so a
	// pasted query or fragment cannot leak. The parser's own error is reduced to
	// its reason for the same reason.
	safe := redact.URL(raw)
	if err != nil {
		return nil, fmt.Errorf("vkclient: endpoint %s: %s", safe, redact.ErrorText(err, raw))
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("vkclient: endpoint %s must use http:// or https://", safe)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("vkclient: endpoint %s has no host", safe)
	}
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		return nil, fmt.Errorf("vkclient: endpoint %s must not carry a path", safe)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("vkclient: endpoint %s must not carry a query or fragment", safe)
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

	endpoint := *c.base
	endpoint.Path = attachPath
	response, err := c.transport.Do(ctx, HTTPRequest{
		Method: "POST",
		URL:    endpoint.String(),
		Header: map[string]string{
			"Accept":       "application/json",
			"Content-Type": "application/json",
		},
		Body: body,
	}, attachTimeout)
	if err != nil {
		return Session{}, fmt.Errorf("vkclient: attach: %w", err)
	}
	payload := response.Body
	if len(payload) > 1<<20 {
		return Session{}, errors.New("vkclient: attach response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var rejection struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(payload, &rejection)
		return Session{}, &AttachError{
			Status: response.StatusCode,
			Body:   strings.TrimSpace(string(payload)),
			Code:   rejection.Code,
		}
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
	if IsCode(err, CodeUnauthenticated) || IsCode(err, CodePermissionDenied) {
		return fmt.Errorf("vkclient: the service rejected the session; reload the vendor session "+
			"file or configure session_id/session_secret: %w", err)
	}
	return c.serialHint(fmt.Errorf("vkclient: authorize: %w", err))
}

// serialHint makes a bare vendor "Invalid 'x-device-sn'" refusal actionable:
// the service validates the SoC serial by name, so the operator must know that
// the serial setting is the lever, not the vendor string alone. The wrapped
// error keeps the original status for IsCode and the attach body for
// ProvisioningRequired.
func (c *Client) serialHint(err error) error {
	if !IsInvalidDeviceSN(err) {
		return err
	}
	if strings.TrimSpace(c.cfg.Serial) == "" {
		return fmt.Errorf("vkclient: the device serial is missing; set the \"serial\" setting to the "+
			"SoC serial the framework reports as hardware_serial (the launcher also exports it as "+
			"CAPSULAD_DEVICE_SERIAL; x-device-sn is empty): %w", err)
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
	request, err := proto.Marshal(&clientprobepb.AuthReadyRequest{})
	if err != nil {
		return fmt.Errorf("vkclient: encode probe: %w", err)
	}
	_, err = c.transport.Unary(ctx, probeService, "AuthReady", c.callMetadata(), request, probeTimeout)
	return err
}

// callMetadata is rebuilt for every RPC: the request id must be fresh, and the
// authorization header follows the current session secret.
//
// Keys are lowercase per the gRPC metadata contract. x-device-ver is the
// advertised client version; x-device-sn is the SoC serial; x-device-id is the
// persisted random id. An absent secret sends an empty value, which is what the
// vendor does before an account is linked.
func (c *Client) callMetadata() map[string]string {
	session := c.Session()
	return map[string]string{
		"x-session-id":        session.SessionID,
		"x-device-id":         c.cfg.DeviceID,
		"x-device-sn":         c.cfg.Serial,
		"x-device-ver":        c.cfg.ClientVersion,
		"x-client-request-id": NewUUID(),
		"authorization":       bearer(session.SessionSecret),
	}
}

func bearer(secret string) string {
	if secret == "" {
		return ""
	}
	return "Bearer " + secret
}

// Close releases the transport and every channel it owns. It is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	transport := c.transport
	c.mu.Unlock()
	if transport == nil {
		return nil
	}
	return transport.Close()
}

// ErrClosed reports use of a closed client.
var ErrClosed = errors.New("vkclient: client is closed")

// Code is a canonical gRPC status code. The values match the gRPC numbering so
// the numeric codes the native SDK reports map without a table, but this
// package does not import grpc-go.
type Code int32

const (
	CodeOK                 Code = 0
	CodeCanceled           Code = 1
	CodeUnknown            Code = 2
	CodeInvalidArgument    Code = 3
	CodeDeadlineExceeded   Code = 4
	CodeNotFound           Code = 5
	CodeAlreadyExists      Code = 6
	CodePermissionDenied   Code = 7
	CodeResourceExhausted  Code = 8
	CodeFailedPrecondition Code = 9
	CodeAborted            Code = 10
	CodeOutOfRange         Code = 11
	CodeUnimplemented      Code = 12
	CodeInternal           Code = 13
	CodeUnavailable        Code = 14
	CodeDataLoss           Code = 15
	CodeUnauthenticated    Code = 16
)

func (c Code) String() string {
	switch c {
	case CodeOK:
		return "OK"
	case CodeCanceled:
		return "Canceled"
	case CodeUnknown:
		return "Unknown"
	case CodeInvalidArgument:
		return "InvalidArgument"
	case CodeDeadlineExceeded:
		return "DeadlineExceeded"
	case CodeNotFound:
		return "NotFound"
	case CodeAlreadyExists:
		return "AlreadyExists"
	case CodePermissionDenied:
		return "PermissionDenied"
	case CodeResourceExhausted:
		return "ResourceExhausted"
	case CodeFailedPrecondition:
		return "FailedPrecondition"
	case CodeAborted:
		return "Aborted"
	case CodeOutOfRange:
		return "OutOfRange"
	case CodeUnimplemented:
		return "Unimplemented"
	case CodeInternal:
		return "Internal"
	case CodeUnavailable:
		return "Unavailable"
	case CodeDataLoss:
		return "DataLoss"
	case CodeUnauthenticated:
		return "Unauthenticated"
	default:
		return fmt.Sprintf("Code(%d)", int32(c))
	}
}

// NewUUID returns a fresh random RFC 4122 version 4 UUID in the lowercase
// 8-4-4-4-12 form the vendor uses for x-client-request-id. It is produced by
// the native SDK's getrandom-backed primitive.
func NewUUID() string {
	if uuid, err := capsulad.UUIDv4(); err == nil {
		return uuid
	}
	// The native library is unavailable (a pure unit test, or an early failure
	// before OpenHTTP/OpenGRPC). Keep the identifier fresh with the standard
	// library rather than collapsing every request onto one id.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
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
	Code    Code
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("vkclient: %s: %s", e.Op, e.Code)
	}
	return fmt.Sprintf("vkclient: %s: %s: %s", e.Op, e.Code, e.Message)
}

// IsCode reports whether err is a StatusError with the given code.
func IsCode(err error, code Code) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}
