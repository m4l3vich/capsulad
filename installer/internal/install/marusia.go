package install

import (
	"encoding/json"
	"fmt"
	"strings"

	"capsulad/installer/internal/device"
)

// MarusiaSession is the migrated subset of the stock voice agent's session
// file. Both fields are required for a migration to make sense.
type MarusiaSession struct {
	Session string
	Secret  string
}

// ParseMarusiaSession reads {"session": "...", "secret": "..."} without
// logging or returning the values in errors.
func ParseMarusiaSession(data []byte) (MarusiaSession, bool) {
	var raw struct {
		Session string `json:"session"`
		Secret  string `json:"secret"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return MarusiaSession{}, false
	}
	session := strings.TrimSpace(raw.Session)
	secret := strings.TrimSpace(raw.Secret)
	if session == "" || secret == "" {
		return MarusiaSession{}, false
	}
	if strings.ContainsAny(session, "\r\n\x00") || strings.ContainsAny(secret, "\r\n\x00") {
		return MarusiaSession{}, false
	}
	return MarusiaSession{Session: session, Secret: secret}, true
}

// MarusiaMigrationCommands copies the vendor session into app settings. Both
// values use the CLI's `KEY@-` stdin form: the generated argv contains only the
// key name, never the session or its secret.
func MarusiaMigrationCommands(cli, dataDir string, s MarusiaSession) []device.Command {
	return []device.Command{
		{
			Args:  SettingsStdinArgs(cli, dataDir, "org.capsulad.marusia", "session_id"),
			Stdin: []byte(s.Session + "\n"),
		},
		{
			Args:  SettingsStdinArgs(cli, dataDir, "org.capsulad.marusia", "session_secret"),
			Stdin: []byte(s.Secret + "\n"),
		},
	}
}

// ValidateSecretSetting rejects a value that cannot survive a one-line stdin
// value (a token with a newline would leave a partial setting).
func ValidateSecretSetting(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("empty value")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("value must be one line")
	}
	return nil
}
