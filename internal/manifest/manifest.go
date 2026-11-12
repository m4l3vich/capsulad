// Package manifest parses and validates supervised application manifests.
//
// A manifest declares identity, exactly one entrypoint runtime, lifecycle
// flags, optional `provides` hints, typed settings, action buttons, launch
// arguments, custom interface descriptors and optional provenance evidence.
// It never declares output ports or permissions: manifests are declarative
// metadata for the supervisor and UI, not a security policy.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

type Runtime string

const (
	RuntimeNative      Runtime = "native"
	RuntimeQuickJS     Runtime = "quickjs"
	RuntimeMicroPython Runtime = "micropython"
	// RuntimeBuiltin marks an application compiled into capsulad. It has a
	// normal registry identity, app settings, routing ownership, run lifecycle
	// and app-bus endpoint, but no package directory or child process.
	RuntimeBuiltin Runtime = "builtin"
)

type Entrypoint struct {
	Runtime      Runtime `toml:"runtime"`
	Path         string  `toml:"path"`
	Architecture string  `toml:"architecture"`
	ABI          string  `toml:"abi"`
	SDK          string  `toml:"sdk"`
}

type Setting struct {
	Key             string   `toml:"key"`
	Label           string   `toml:"label"`
	Description     string   `toml:"description"`
	Type            string   `toml:"type"`
	Default         any      `toml:"default"`
	Choices         []Choice `toml:"choices"`
	Required        bool     `toml:"required"`
	RestartRequired bool     `toml:"restart_required"`
	Min             *float64 `toml:"min"`
	Max             *float64 `toml:"max"`
}

type Choice struct {
	Value string `toml:"value"`
	Label string `toml:"label"`
}

type Action struct {
	ID               string `toml:"id"`
	Label            string `toml:"label"`
	Description      string `toml:"description"`
	ConfirmBeforeRun bool   `toml:"confirm_before_run"`
}

type Argument struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Type        string `toml:"type"`
	Required    bool   `toml:"required"`
}

type Interface struct {
	Service    string `toml:"service"`
	Version    string `toml:"version"`
	Descriptor string `toml:"descriptor"`
	Docs       string `toml:"docs"`
}

type GitHubAttestation struct {
	Subject    string `toml:"subject"`
	Bundle     string `toml:"bundle"`
	Repository string `toml:"repository"`
	Workflow   string `toml:"workflow"`
}

// NativeSDK is an app's optional requirement on the shared native SDK
// runtime. It is requirement metadata checked by the supervisor before an app
// is installed or launched; it is not a negotiation. A binding that loads the
// library still verifies the ABI independently at startup.
type NativeSDK struct {
	ABIMajor             int      `toml:"abi_major"`
	MinMinor             int      `toml:"min_minor"`
	RequiredCapabilities []string `toml:"required_capabilities"`
}

type Manifest struct {
	ID                string             `toml:"id"`
	Name              string             `toml:"name"`
	Version           string             `toml:"version"`
	Description       string             `toml:"description"`
	Background        bool               `toml:"background"`
	StartOnBoot       bool               `toml:"start_on_boot"`
	RestartOnFailure  bool               `toml:"restart_on_failure"`
	SingleInstance    bool               `toml:"single_instance"`
	Provides          []string           `toml:"provides"`
	Entrypoint        Entrypoint         `toml:"entrypoint"`
	Settings          []Setting          `toml:"settings"`
	Actions           []Action           `toml:"actions"`
	Arguments         []Argument         `toml:"arguments"`
	Interfaces        []Interface        `toml:"interfaces"`
	NativeSDK         *NativeSDK         `toml:"native_sdk"`
	GitHubAttestation *GitHubAttestation `toml:"github_attestation"`

	// Dir is the immutable package root the manifest was loaded from. It is
	// not serialized and is always an absolute path.
	Dir string `toml:"-"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

const capabilityPattern = "lowercase letters, digits, '.', '_' or '-'"

var capabilityRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// ValidNativeCapability reports whether name is a well-formed capability
// identifier. Capabilities are a shared vocabulary between apps and the native
// runtime build, so the manifest and runtime metadata use one rule.
func ValidNativeCapability(name string) bool {
	return capabilityRe.MatchString(name)
}

var settingTypes = map[string]bool{
	"text": true, "text_masked": true, "boolean": true,
	"integer": true, "number": true, "select": true,
}

// Load reads and validates one manifest file. root is the package directory
// the manifest belongs to.
func Load(root, file string) (*Manifest, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var m Manifest
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		keys := make([]string, 0, len(extra))
		for _, k := range extra {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("unknown manifest keys: %s", strings.Join(keys, ", "))
	}
	m.Dir = root
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate enforces the manifest contract before an app can be installed.
func (m *Manifest) Validate() error {
	if !idRe.MatchString(m.ID) {
		return fmt.Errorf("app id %q is not a valid stable identifier", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("app %s: name is required", m.ID)
	}
	if m.Version == "" {
		return fmt.Errorf("app %s: version is required", m.ID)
	}
	switch m.Entrypoint.Runtime {
	case RuntimeNative:
		if m.Entrypoint.Architecture == "" || m.Entrypoint.ABI == "" {
			return fmt.Errorf("app %s: native entrypoint requires architecture and abi", m.ID)
		}
	case RuntimeQuickJS, RuntimeMicroPython:
		if m.Entrypoint.Architecture != "" || m.Entrypoint.ABI != "" {
			return fmt.Errorf("app %s: script entrypoint must not declare architecture/abi", m.ID)
		}
	case RuntimeBuiltin:
		if m.Entrypoint.Architecture != "" || m.Entrypoint.ABI != "" || m.Entrypoint.Path != "" {
			return fmt.Errorf("app %s: built-in entrypoint must not declare path, architecture or abi", m.ID)
		}
		if m.NativeSDK != nil {
			return fmt.Errorf("app %s: built-in applications do not load the native SDK", m.ID)
		}
	default:
		return fmt.Errorf("app %s: unknown runtime %q", m.ID, m.Entrypoint.Runtime)
	}
	if n := m.NativeSDK; n != nil {
		if m.Entrypoint.Runtime != RuntimeNative {
			return fmt.Errorf("app %s: native_sdk is only valid for a native entrypoint", m.ID)
		}
		if n.ABIMajor < 1 {
			return fmt.Errorf("app %s: native_sdk.abi_major must be at least 1", m.ID)
		}
		if n.MinMinor < 0 {
			return fmt.Errorf("app %s: native_sdk.min_minor must not be negative", m.ID)
		}
		seenCaps := map[string]bool{}
		for _, c := range n.RequiredCapabilities {
			if !ValidNativeCapability(c) {
				return fmt.Errorf("app %s: native_sdk capability %q must match %s", m.ID, c, capabilityPattern)
			}
			if seenCaps[c] {
				return fmt.Errorf("app %s: duplicate native_sdk capability %q", m.ID, c)
			}
			seenCaps[c] = true
		}
	}
	if err := validateRelPath("entrypoint.path", m.Entrypoint.Path, m.Entrypoint.Runtime != RuntimeBuiltin); err != nil {
		return fmt.Errorf("app %s: %w", m.ID, err)
	}
	seenSettings := map[string]bool{}
	for _, s := range m.Settings {
		if s.Key == "" || seenSettings[s.Key] {
			return fmt.Errorf("app %s: duplicate or empty setting key %q", m.ID, s.Key)
		}
		seenSettings[s.Key] = true
		if !settingTypes[s.Type] {
			return fmt.Errorf("app %s: setting %s has unknown type %q", m.ID, s.Key, s.Type)
		}
		if s.Type == "select" && len(s.Choices) == 0 {
			return fmt.Errorf("app %s: setting %s is a select without choices", m.ID, s.Key)
		}
		if s.Min != nil && s.Max != nil && *s.Min > *s.Max {
			return fmt.Errorf("app %s: setting %s min > max", m.ID, s.Key)
		}
		if s.Default != nil {
			if err := validateValue(s, s.Default); err != nil {
				return fmt.Errorf("app %s: setting %s default: %w", m.ID, s.Key, err)
			}
		}
	}
	seenActions := map[string]bool{}
	for _, a := range m.Actions {
		if a.ID == "" || seenActions[a.ID] {
			return fmt.Errorf("app %s: duplicate or empty action id %q", m.ID, a.ID)
		}
		seenActions[a.ID] = true
	}
	for _, a := range m.Arguments {
		if a.Name == "" {
			return fmt.Errorf("app %s: launch argument name is required", m.ID)
		}
		if a.Type != "text" && a.Type != "integer" && a.Type != "number" && a.Type != "boolean" {
			return fmt.Errorf("app %s: launch argument %s has unsupported type %q", m.ID, a.Name, a.Type)
		}
	}
	for _, i := range m.Interfaces {
		if i.Service == "" {
			return fmt.Errorf("app %s: interface service name is required", m.ID)
		}
		if i.Descriptor != "" {
			if err := validateRelPath("interface descriptor", i.Descriptor, false); err != nil {
				return fmt.Errorf("app %s: %w", m.ID, err)
			}
		}
		if i.Docs != "" {
			if err := validateRelPath("interface docs", i.Docs, false); err != nil {
				return fmt.Errorf("app %s: %w", m.ID, err)
			}
		}
	}
	if g := m.GitHubAttestation; g != nil {
		if g.Subject == "" || g.Bundle == "" || g.Repository == "" {
			return fmt.Errorf("app %s: github_attestation requires subject, bundle and repository", m.ID)
		}
		if err := validateRelPath("attestation subject", g.Subject, false); err != nil {
			return fmt.Errorf("app %s: %w", m.ID, err)
		}
		if err := validateRelPath("attestation bundle", g.Bundle, false); err != nil {
			return fmt.Errorf("app %s: %w", m.ID, err)
		}
	}
	for _, p := range m.Provides {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("app %s: empty provides hint", m.ID)
		}
	}
	return nil
}

func validateRelPath(label, p string, required bool) error {
	if p == "" {
		if required {
			return fmt.Errorf("%s is required", label)
		}
		return nil
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%s must be relative to the package root", label)
	}
	clean := filepath.Clean(p)
	if clean == "." || strings.HasPrefix(clean, "..") || strings.Contains(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s escapes the package root", label)
	}
	return nil
}

// validateValue checks a setting value against its declared type and bounds.
func validateValue(s Setting, v any) error {
	switch s.Type {
	case "text", "text_masked":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected text")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean")
		}
	case "integer":
		n, ok := asFloat(v)
		if !ok || n != float64(int64(n)) {
			return fmt.Errorf("expected integer")
		}
		if err := bounds(s, n); err != nil {
			return err
		}
	case "number":
		n, ok := asFloat(v)
		if !ok {
			return fmt.Errorf("expected number")
		}
		if err := bounds(s, n); err != nil {
			return err
		}
	case "select":
		val, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected select string")
		}
		for _, c := range s.Choices {
			if c.Value == val {
				return nil
			}
		}
		return fmt.Errorf("value %q is not a declared choice", val)
	}
	return nil
}

func bounds(s Setting, n float64) error {
	if s.Min != nil && n < *s.Min {
		return fmt.Errorf("%v is below minimum %v", n, *s.Min)
	}
	if s.Max != nil && n > *s.Max {
		return fmt.Errorf("%v is above maximum %v", n, *s.Max)
	}
	return nil
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// ValidateSetting checks a JSON-decoded value for setting key.
func (m *Manifest) ValidateSetting(key string, value any) (*Setting, error) {
	for i := range m.Settings {
		if m.Settings[i].Key == key {
			if err := validateValue(m.Settings[i], value); err != nil {
				return nil, fmt.Errorf("setting %s: %w", key, err)
			}
			return &m.Settings[i], nil
		}
	}
	return nil, fmt.Errorf("setting %s is not declared by %s", key, m.ID)
}

// ValidateLaunchArguments checks typed launch arguments without ever
// interpolating them into a shell command.
func (m *Manifest) ValidateLaunchArguments(args map[string]string) error {
	declared := map[string]Argument{}
	for _, a := range m.Arguments {
		declared[a.Name] = a
	}
	for name, raw := range args {
		a, ok := declared[name]
		if !ok {
			return fmt.Errorf("launch argument %q is not declared", name)
		}
		switch a.Type {
		case "integer":
			var n int64
			if _, err := fmt.Sscan(raw, &n); err != nil {
				return fmt.Errorf("launch argument %s must be an integer", name)
			}
		case "number":
			var f float64
			if _, err := fmt.Sscan(raw, &f); err != nil {
				return fmt.Errorf("launch argument %s must be a number", name)
			}
		case "boolean":
			if raw != "true" && raw != "false" {
				return fmt.Errorf("launch argument %s must be true or false", name)
			}
		}
	}
	for _, a := range m.Arguments {
		if a.Required {
			if _, ok := args[a.Name]; !ok {
				return fmt.Errorf("launch argument %s is required", a.Name)
			}
		}
	}
	return nil
}

// Defaults returns setting defaults keyed by setting key.
func (m *Manifest) Defaults() map[string]any {
	out := map[string]any{}
	for _, s := range m.Settings {
		if s.Default != nil {
			out[s.Key] = s.Default
		}
	}
	return out
}

// ValueJSON converts a Go value to its canonical JSON representation.
func ValueJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
