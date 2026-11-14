package rockchip

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// keyMap maps Linux input key codes to the semantic key names consumed by the
// framework. The stock device tree declares five gpio-keys; two of them were
// silently dropped by the backend before this map existed.
type keyMap map[int]string

const (
	keyVolumeDown = 114 // KEY_VOLUMEDOWN
	keyVolumeUp   = 115 // KEY_VOLUMEUP
	keyPlayPause  = 164 // KEY_PLAYPAUSE
	keyMicMute    = 248 // KEY_MICMUTE
	keyEnter      = 28  // KEY_ENTER
	keyOK         = 0x160
	keyBtn0       = 0x100
	// The two stock DTBs use different codes and labels for the same unmarked
	// centre key. It is not a voice key: on Capsula Mini the circle/play-pause
	// key is the talk button and the centre key is neutral.
	keyVoiceCmd = 0x246 // KEY_VOICECOMMAND label on the boot_b DTB
	keyHappy    = 0x2c0 // BTN_TRIGGER_HAPPY1 label on the boot_a DTB
)

// defaultKeyMap is used when the live device tree cannot be read. It covers
// both stock device-tree variants seen in the boot image. KEY_VOICECOMMAND and
// BTN_TRIGGER_HAPPY1 name the same unmarked centre key on the Mini, so both map
// to the neutral centre control rather than to an assistant activation.
func defaultKeyMap() keyMap {
	return keyMap{
		keyVolumeDown: "volume_down",
		keyVolumeUp:   "volume_up",
		keyPlayPause:  "play_pause",
		keyMicMute:    "mic_toggle",
		keyEnter:      "primary",
		keyOK:         "primary",
		keyBtn0:       "primary",
		keyVoiceCmd:   "center",
		keyHappy:      "center",
	}
}

// probeKeyMap reads the gpio-keys node and maps each declared linux,code to a
// semantic name using the node name and label. Node names are authoritative
// because the two stock DTBs use different labels ("happy" vs "GPIO Volume
// Voice-assistant") for the same unmarked centre key.
func probeKeyMap(prefix string) keyMap {
	keys := defaultKeyMap()
	base := deviceTreeBase(prefix)
	if base == "" {
		return keys
	}
	node := findCompatible(base, "gpio-keys")
	if node == "" {
		return keys
	}
	entries, err := os.ReadDir(node)
	if err != nil {
		return keys
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		child := filepath.Join(node, name)
		code, ok := readDTUint32(filepath.Join(child, "linux,code"))
		if !ok {
			continue
		}
		if semantic := keySemantic(name, readTrim(filepath.Join(child, "label"))); semantic != "" {
			keys[int(code)] = semantic
		}
	}
	return keys
}

func readDTUint32(path string) (uint32, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(data[:4]), true
}

func keySemantic(nodeName, label string) string {
	name := strings.ToLower(nodeName)
	label = strings.ToLower(label)
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(name, part) || strings.Contains(label, part) {
				return true
			}
		}
		return false
	}
	switch {
	case has("micmute", "mic mute", "mic-mute", "mute"):
		return "mic_toggle"
	case has("volumeup", "volup", "volume up", "volumeup"):
		return "volume_up"
	case has("volumedown", "voldown", "volume down"):
		return "volume_down"
	case has("playpause", "play-pause", "play/pause", "play"):
		return "play_pause"
	case has("center", "centre", "happy", "voice", "assistant"):
		// The Mini's stock centre key is labelled "happy" in one DTB and
		// "GPIO Volume Voice-assistant" in the other. It is neutral: the
		// play/pause circle is the talk button.
		return "center"
	case has("primary", "enter", "ok"):
		return "primary"
	}
	return ""
}
