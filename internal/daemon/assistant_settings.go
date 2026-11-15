package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"capsulad/internal/assets"
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

// validateAssistantAsset checks a configured reply asset against the real
// store. The daemon's assets directory is mounted read-only as /assets inside
// every app sandbox, so the value is a store-relative path: it must not be
// absolute or traverse, may not pass through a symlink, must name a regular
// file and must resolve to the WAV media type the reply decoder accepts. The
// directory is the catalog; membership is the file's presence, read fresh so
// an asset uploaded after daemon start is accepted. Empty selects the packaged
// fallback.
func (d *Daemon) validateAssistantAsset(value string) error {
	rel := strings.TrimSpace(value)
	if rel == "" {
		return nil
	}
	info, err := d.assets.Lookup(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("reply_asset: %q does not exist in the shared assets store", rel)
		}
		if errors.Is(err, assets.ErrInvalidPath) {
			return fmt.Errorf("reply_asset: %s", strings.TrimPrefix(err.Error(), assets.ErrInvalidPath.Error()+": "))
		}
		return fmt.Errorf("reply_asset: %s", err)
	}
	if !isAssistantReplyMediaType(info.MediaType) {
		return fmt.Errorf("reply_asset: %s has unsupported media type %q", info.Path, info.MediaType)
	}
	return nil
}

// isAssistantReplyMediaType keeps the reply to the one type the packaged WAV
// decoder reads. The store's extension table maps .wav to audio/wav.
func isAssistantReplyMediaType(mediaType string) bool { return mediaType == "audio/wav" }
