// Package config turns this app's manifest settings into the one validated
// configuration the connection and turn loops use. Every value has a documented
// default; an invalid edit is rejected by itself so the last good value stays in
// effect instead of the whole form silently reverting.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"capsulad/sdk/capsulad"
)

// Manifest setting keys, in one place.
const (
	KeyServerURL       = "server_url"
	KeyToken           = "token"
	KeyDeviceID        = "device_id"
	KeyLanguage        = "language"
	KeyLocation        = "location"
	KeyTimezone        = "timezone"
	KeyFollowUp        = "follow_up"
	KeyFollowUpTimeout = "follow_up_timeout_ms"
	KeyEarcons         = "earcons"
)

const (
	// DefaultServerURL is the OpenCapsule streaming endpoint on the same host.
	DefaultServerURL = "ws://127.0.0.1:8080/v1/stream"
	// DefaultLanguage is the reference deployment's speech language.
	DefaultLanguage = "ru-RU"
	// DefaultFollowUpTimeout is how long the app waits for speech after a reply.
	DefaultFollowUpTimeout = 4 * time.Second
	// MaxFollowUpTimeoutMS bounds the follow-up window.
	MaxFollowUpTimeoutMS = 60000
)

// Config is the validated application configuration.
type Config struct {
	ServerURL       string
	Token           string
	DeviceID        string
	Language        string
	Location        string
	Timezone        string
	FollowUp        bool
	FollowUpTimeout time.Duration
	Earcons         bool
}

// Default returns the manifest defaults for a fresh installation.
func Default() Config {
	return Config{
		ServerURL:       DefaultServerURL,
		Language:        DefaultLanguage,
		FollowUp:        true,
		FollowUpTimeout: DefaultFollowUpTimeout,
		Earcons:         true,
	}
}

// Apply overlays one settings snapshot on current and returns the accepted
// configuration plus one human-readable rejection per invalid field. The
// snapshot is whole, so a key absent from it keeps its current value.
func Apply(current Config, changed *capsulad.SettingsChanged) (Config, []string) {
	next := current
	var rejected []string
	if changed == nil {
		return next, nil
	}
	for _, setting := range changed.Values {
		if setting.ValueJSON == "" {
			// An empty document means "no stored value". A cleared masked
			// token must actually clear the in-memory credential; the other
			// declared settings have manifest defaults and keep their value.
			if setting.Key == KeyToken {
				next.Token = ""
			}
			continue
		}
		switch setting.Key {
		case KeyServerURL:
			text, ok := jsonString(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyServerURL+": expected a JSON string")
				continue
			}
			endpoint, err := ParseEndpoint(text)
			if err != nil {
				rejected = append(rejected, fmt.Sprintf("%s %q: %v", KeyServerURL, text, err))
				continue
			}
			next.ServerURL = endpoint.String()
		case KeyToken:
			text, ok := jsonString(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyToken+": expected a JSON string")
				continue
			}
			text = strings.TrimSpace(text)
			if strings.ContainsAny(text, "\r\n") {
				rejected = append(rejected, KeyToken+": must not contain CR or LF")
				continue
			}
			next.Token = text
		case KeyDeviceID:
			text, ok := jsonString(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyDeviceID+": expected a JSON string")
				continue
			}
			next.DeviceID = strings.TrimSpace(text)
		case KeyLanguage:
			text, ok := jsonString(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyLanguage+": expected a JSON string")
				continue
			}
			next.Language = strings.TrimSpace(text)
			if next.Language == "" {
				next.Language = DefaultLanguage
			}
		case KeyLocation:
			text, ok := jsonString(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyLocation+": expected a JSON string")
				continue
			}
			next.Location = strings.TrimSpace(text)
		case KeyTimezone:
			text, ok := jsonString(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyTimezone+": expected a JSON string")
				continue
			}
			text = strings.TrimSpace(text)
			if text != "" {
				if _, err := time.LoadLocation(text); err != nil {
					rejected = append(rejected, fmt.Sprintf("%s %q: not an IANA time zone", KeyTimezone, text))
					continue
				}
			}
			next.Timezone = text
		case KeyFollowUp:
			value, ok := jsonBool(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyFollowUp+": expected true or false")
				continue
			}
			next.FollowUp = value
		case KeyFollowUpTimeout:
			value, ok := jsonInt(setting.ValueJSON)
			if !ok || value < 0 || value > MaxFollowUpTimeoutMS {
				rejected = append(rejected, fmt.Sprintf(
					"%s: expected a whole number of milliseconds from 0 to %d", KeyFollowUpTimeout, MaxFollowUpTimeoutMS))
				continue
			}
			next.FollowUpTimeout = time.Duration(value) * time.Millisecond
		case KeyEarcons:
			value, ok := jsonBool(setting.ValueJSON)
			if !ok {
				rejected = append(rejected, KeyEarcons+": expected true or false")
				continue
			}
			next.Earcons = value
		}
	}
	next.Language = strings.TrimSpace(next.Language)
	if next.Language == "" {
		next.Language = DefaultLanguage
	}
	return next, rejected
}

// Endpoint is the validated parsed form of ServerURL.
type Endpoint struct {
	TLS        bool
	Host       string
	Port       uint16
	HostHeader string
	Path       string
	Raw        string
}

// String returns the normalized URL the next connection uses.
func (e Endpoint) String() string { return e.Raw }

// ParseEndpoint validates a ws:// or wss:// streaming URL. Only an absolute
// URL with a host and a path is accepted; credentials, queries and fragments
// are refused rather than silently dropped.
func ParseEndpoint(raw string) (Endpoint, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Endpoint{}, fmt.Errorf("server URL is required")
	}
	u, err := url.Parse(text)
	if err != nil {
		return Endpoint{}, fmt.Errorf("not a URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "ws" && scheme != "wss" {
		return Endpoint{}, fmt.Errorf("scheme must be ws or wss")
	}
	u.Scheme = scheme
	if u.User != nil {
		return Endpoint{}, fmt.Errorf("user info is not supported; use the token setting")
	}
	if u.Hostname() == "" {
		return Endpoint{}, fmt.Errorf("host is required")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Endpoint{}, fmt.Errorf("query and fragment are not supported")
	}
	path := u.EscapedPath()
	if path == "" || path == "/" {
		return Endpoint{}, fmt.Errorf("stream path is required, for example /v1/stream")
	}
	port := u.Port()
	portNumber := 0
	if port == "" {
		if scheme == "wss" {
			portNumber = 443
		} else {
			portNumber = 80
		}
	} else {
		portNumber, err = strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return Endpoint{}, fmt.Errorf("port must be 1..65535")
		}
	}
	return Endpoint{
		TLS:        scheme == "wss",
		Host:       u.Hostname(),
		Port:       uint16(portNumber),
		HostHeader: u.Host,
		Path:       path,
		Raw:        u.String(),
	}, nil
}

func jsonString(raw string) (string, bool) {
	var text string
	if err := json.Unmarshal([]byte(raw), &text); err != nil {
		return "", false
	}
	return text, true
}

func jsonBool(raw string) (bool, bool) {
	var value bool
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return false, false
	}
	return value, true
}

func jsonInt(raw string) (int64, bool) {
	var value int64
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return 0, false
	}
	return value, true
}
