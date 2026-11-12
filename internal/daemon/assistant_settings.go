package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// orgCapsulaDumbAssistantID is the optional first-party assistant package.
const orgCapsulaDumbAssistantID = "org.capsula.dumbassistant"

const (
	assistantMaxDelayMS    = 60000
	assistantDelayKey      = "delay"
	assistantReplyAssetKey = "reply_asset"
)

// registerAssistantSettingsValidator makes invalid delay syntax and unsafe
// asset paths fail synchronously in ApplicationConfigurationService.Update
// instead of only being logged by the running app. The grammar mirrors
// apps/dumbassistant's parseDelay/resolveReplyAsset so both layers agree.
func (d *Daemon) registerAssistantSettingsValidator() {
	d.RegisterAppSettingsValidator(orgCapsulaDumbAssistantID, func(values map[string]json.RawMessage) error {
		if raw, ok := values[assistantDelayKey]; ok {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return fmt.Errorf("delay: expected text")
			}
			if _, _, err := parseAssistantDelay(text); err != nil {
				return err
			}
		}
		if raw, ok := values[assistantReplyAssetKey]; ok {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return fmt.Errorf("reply_asset: expected text")
			}
			if err := d.validateAssistantAsset(text); err != nil {
				return err
			}
		}
		return nil
	})
}

// parseAssistantDelay accepts only the documented single-field grammar:
// empty, a decimal millisecond count, or an ascending inclusive range.
func parseAssistantDelay(raw string) (minMS, maxMS int64, err error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, 0, nil
	}
	parts := strings.Split(text, "-")
	switch len(parts) {
	case 1:
		ms, err := parseAssistantDelayBound(parts[0])
		if err != nil {
			return 0, 0, err
		}
		return ms, ms, nil
	case 2:
		lo, err := parseAssistantDelayBound(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, err
		}
		hi, err := parseAssistantDelayBound(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, err
		}
		if lo > hi {
			return 0, 0, fmt.Errorf("delay: minimum %d is greater than maximum %d", lo, hi)
		}
		return lo, hi, nil
	default:
		return 0, 0, errAssistantDelaySyntax
	}
}

var errAssistantDelaySyntax = fmt.Errorf(`delay: use empty, "100", or an ascending "200-1500" range`)

func parseAssistantDelayBound(text string) (int64, error) {
	if text == "" {
		return 0, errAssistantDelaySyntax
	}
	for i := range len(text) {
		if text[i] < '0' || text[i] > '9' {
			return 0, errAssistantDelaySyntax
		}
	}
	ms, err := strconv.ParseInt(text, 10, 64)
	if err != nil || ms > assistantMaxDelayMS {
		return 0, fmt.Errorf("delay: %s ms exceeds the %d ms limit", text, assistantMaxDelayMS)
	}
	return ms, nil
}

// validateAssistantAsset mirrors the app's containment rule for the shared
// app-facing asset store: the daemon's assets directory is mounted read-only
// as /assets inside every app sandbox. A configured value is relative to that
// store, must not traverse or pass through a symlink, and must name a regular
// file. Values are relative to the store root; a leading "assets/" is a real
// subdirectory, not an alias. Empty selects the packaged fallback.
func (d *Daemon) validateAssistantAsset(value string) error {
	rel := strings.TrimSpace(value)
	if rel == "" {
		return nil
	}
	if strings.IndexByte(rel, 0) >= 0 {
		return fmt.Errorf("reply_asset: path contains a NUL byte")
	}
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return fmt.Errorf("reply_asset: path must be relative to the shared assets store")
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf(`reply_asset: path must not contain ".."`)
		}
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || clean == "" {
		return fmt.Errorf("reply_asset: path does not name a file")
	}
	root := d.cfg.AssetsDir
	full := filepath.Join(root, clean)
	inside, err := filepath.Rel(root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return fmt.Errorf("reply_asset: path is outside the shared assets store")
	}
	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("reply_asset: %s", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("reply_asset: asset must not use a symlink")
		}
	}
	info, err := os.Stat(full)
	if err != nil {
		return fmt.Errorf("reply_asset: %s", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("reply_asset: asset is not a regular file")
	}
	return d.validateAssistantAssetRegistration(clean)
}

// validateAssistantAssetRegistration requires the file to be published by the
// shared asset catalog, not merely present in the directory. The manifest is
// read fresh so an asset registered after daemon start is accepted.
func (d *Daemon) validateAssistantAssetRegistration(clean string) error {
	data, err := os.ReadFile(filepath.Join(d.cfg.AssetsDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("reply_asset: shared asset manifest is unavailable: %v", err)
	}
	var doc struct {
		Assets []struct {
			ID        string `json:"id"`
			Path      string `json:"path"`
			MediaType string `json:"media_type"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("reply_asset: shared asset manifest: %v", err)
	}
	want := "/assets/" + filepath.ToSlash(clean)
	for _, a := range doc.Assets {
		path := a.Path
		if path == "" {
			path = a.ID
		}
		path = filepath.ToSlash(path)
		if !strings.HasPrefix(path, "/assets/") {
			path = "/assets/" + strings.TrimPrefix(path, "/")
		}
		if path != want {
			continue
		}
		mediaType := strings.ToLower(strings.TrimSpace(a.MediaType))
		switch mediaType {
		case "", "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
			return nil
		default:
			return fmt.Errorf("reply_asset: asset %s has unsupported media type %q", want, a.MediaType)
		}
	}
	return fmt.Errorf("reply_asset: %s is not registered in the shared asset manifest", want)
}
