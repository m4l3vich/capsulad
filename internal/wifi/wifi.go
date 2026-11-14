// Package wifi owns the speaker's Wi-Fi station and provisioning access point.
//
// The same business layer serves the authenticated Web UI and the isolated
// captive portal. It never scans except for an explicit Scan call: startup
// association uses the saved network list, and opening a status view only reads
// the current state.
package wifi

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"capsulad/internal/logbus"
)

const (
	// APPassword is the documented provisioning password. It is intentionally
	// short and public: physical possession of the speaker is the credential.
	APPassword = "capsulad"

	DefaultAPAddress = "192.168.4.1"
	DefaultAPPrefix  = 24
	DefaultPortalPort = 80
)

var (
	// ErrUnavailable means this platform has no usable Wi-Fi interface or
	// control tooling. It is not a successful no-op.
	ErrUnavailable = errors.New("wifi is unavailable")
	// ErrUnsupported means the platform or the requested security mode cannot
	// be implemented with the installed tooling.
	ErrUnsupported = errors.New("wifi operation is not supported")
	// ErrInvalidNetwork is returned for an invalid SSID, passphrase or
	// security value. The wrapped detail is safe to show a user.
	ErrInvalidNetwork = errors.New("invalid wifi network")
	// ErrNotFound means the requested known network is not stored.
	ErrNotFound = errors.New("wifi network is not known")
)

// State is the externally visible Wi-Fi state. The set is closed so the Web UI
// and the captive portal render the same labels.
type State string

const (
	StateUnavailable State = "unavailable"
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateAPFallback   State = "ap_fallback"
	StateError        State = "error"
)

// Status is a snapshot of the radio and the provisioning fallback.
type Status struct {
	Available     bool
	State         State
	Interface     string
	SSID          string
	IPAddress     string
	Security      string
	Detail        string
	APActive      bool
	APSSID        string
	APAddress     string
	APPortalURL   string
	PortalPort    int
	LastError     string
	ScanSupported bool
	Revision      uint64
}

// KnownNetwork is the redacted view of a saved network. Passphrases and
// derived PSKs never cross this boundary.
type KnownNetwork struct {
	SSID        string
	Security    string
	Priority    int
	HasPassword bool
	Connected   bool
}

// ScanResult is one explicit scan result. Security is normalized to the same
// vocabulary as ConnectRequest.Security.
type ScanResult struct {
	SSID         string
	Security     string
	SignalDBM    int
	FrequencyMHz uint32
	Connected    bool
}

// ConnectRequest adds or updates a saved network and associates to it. The
// passphrase is used to derive the WPA PSK and is never persisted or echoed.
type ConnectRequest struct {
	SSID       string
	Passphrase string
	Security   string
}

// Controller is the narrow business interface used by the Web UI. The captive
// portal uses the same methods.
type Controller interface {
	Status(context.Context) (Status, error)
	KnownNetworks(context.Context) ([]KnownNetwork, error)
	Scan(context.Context) ([]ScanResult, error)
	Connect(context.Context, ConnectRequest) error
	Forget(context.Context, string) error
}

// Manager adds process lifecycle to Controller.
type Manager interface {
	Controller
	// Start performs the bounded startup association attempt and, when no
	// saved network connects, brings up the provisioning access point.
	Start(context.Context) error
	Close() error
}

// APIdentity returns the SSID the provisioning access point must advertise.
// The daemon binds this to the actual Bluetooth adapter alias, not to a
// separate Wi-Fi-only name. An error means the AP cannot be started honestly.
type APIdentity func(context.Context) (string, error)

// Config controls the Linux implementation. Zero values take documented
// defaults; tests override the runner, address and durations.
type Config struct {
	DataDir string
	RunDir  string
	// Interface is the wireless interface name. Empty auto-detects the first
	// /sys/class/net/*/wireless or /sys/class/net/*/phy80211 entry.
	Interface string
	// APAddress is the local address of the provisioning AP. The captive
	// portal binds only to this address.
	APAddress string
	APPrefix  int
	PortalPort int
	// APPassword must be the documented provisioning password in production.
	// It is a field so tests can use a short fixture without weakening the
	// daemon default.
	APPassword string
	APName     APIdentity

	Logs *logbus.Bus
	// OnChange, when set, invalidates the Web UI's Wi-Fi snapshot after a
	// status transition. It is never called for a Status read and never
	// triggers a scan.
	OnChange func()

	// StockConfigPaths overrides the vendor wpa_supplicant configuration paths
	// imported once before takeover. Nil uses the stock defaults; an empty
	// non-nil slice disables stock import for focused tests.
	StockConfigPaths []string

	StartupTimeout time.Duration
	ConnectTimeout time.Duration
	ScanTimeout    time.Duration
	CommandTimeout time.Duration

	Runner CommandRunner
}

func (c Config) normalized() (Config, error) {
	if strings.TrimSpace(c.DataDir) == "" {
		return c, fmt.Errorf("wifi: data directory is required")
	}
	if c.RunDir == "" {
		c.RunDir = c.DataDir + "/run"
	}
	if c.APAddress == "" {
		c.APAddress = DefaultAPAddress
	}
	addr, err := netip.ParseAddr(c.APAddress)
	if err != nil || !addr.Is4() || addr.IsUnspecified() {
		return c, fmt.Errorf("wifi: AP address %q is not a usable IPv4 address", c.APAddress)
	}
	c.APAddress = addr.String()
	if c.APPrefix == 0 {
		c.APPrefix = DefaultAPPrefix
	}
	if c.APPrefix < 8 || c.APPrefix > 30 {
		return c, fmt.Errorf("wifi: AP prefix /%d is outside the supported range", c.APPrefix)
	}
	if c.PortalPort == 0 {
		c.PortalPort = DefaultPortalPort
	}
	if c.PortalPort < 1 || c.PortalPort > 65535 {
		return c, fmt.Errorf("wifi: portal port %d is invalid", c.PortalPort)
	}
	if c.APPassword == "" {
		c.APPassword = APPassword
	}
	if len(c.APPassword) < 8 || len(c.APPassword) > 63 {
		return c, fmt.Errorf("wifi: AP password must be 8-63 bytes")
	}
	if c.StartupTimeout <= 0 {
		c.StartupTimeout = 25 * time.Second
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 45 * time.Second
	}
	if c.ScanTimeout <= 0 {
		c.ScanTimeout = 20 * time.Second
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = 10 * time.Second
	}
	if c.APName == nil {
		c.APName = func(context.Context) (string, error) { return "Capsula", nil }
	}
	if c.Runner == nil {
		c.Runner = execRunner{}
	}
	return c, nil
}

func (c Config) portalURL() string {
	host := c.APAddress
	if c.PortalPort != 80 {
		host = host + ":" + strconv.Itoa(c.PortalPort)
	}
	return "http://" + host + "/"
}

func (c Config) logf(format string, args ...any) {
	if c.Logs != nil {
		c.Logs.Infof("wifi", "", format, args...)
	}
}

func (c Config) warnf(format string, args ...any) {
	if c.Logs != nil {
		c.Logs.Warnf("wifi", "", format, args...)
	}
}
