package linuxdev

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// KeyMap maps Linux input key codes to the semantic key names consumed by the
// framework. The stock device tree declares the panel keys; a key the device
// tree does not describe keeps its default semantic.
type KeyMap map[int]string

// Linux input key codes used by the Capsula Mini panels.
const (
	KeyVolumeDown = 114   // KEY_VOLUMEDOWN
	KeyVolumeUp   = 115   // KEY_VOLUMEUP
	KeyPlayPause  = 164   // KEY_PLAYPAUSE
	KeyMicMute    = 248   // KEY_MICMUTE
	KeyEnter      = 28    // KEY_ENTER
	KeyOK         = 0x160 // KEY_OK
	KeyBtn0       = 0x100 // BTN_0
	// The two stock RK3308 DTBs use different codes and labels for the same
	// key: the unmarked centre of the five-key layout, which is the neutral
	// centre control. The circle at the bottom is the separate play/pause
	// talk key.
	KeyVoiceCmd = 0x246 // KEY_VOICECOMMAND label on the boot_b DTB
	KeyHappy    = 0x2c0 // BTN_TRIGGER_HAPPY1 label on the boot_a DTB
)

// DefaultKeyMap is used when the live device tree cannot be read. It covers
// every layout named by the supported profiles. KEY_VOICECOMMAND and
// BTN_TRIGGER_HAPPY1 name the same unmarked centre key on the five-key Mini, so
// both map to the neutral centre control rather than to an assistant
// activation.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		KeyVolumeDown: "volume_down",
		KeyVolumeUp:   "volume_up",
		KeyPlayPause:  "play_pause",
		KeyMicMute:    "mic_toggle",
		KeyEnter:      "primary",
		KeyOK:         "primary",
		KeyBtn0:       "primary",
		KeyVoiceCmd:   "center",
		KeyHappy:      "center",
	}
}

// ProbeKeyMap reads the gpio-keys node and maps each declared linux,code to a
// semantic name using the node name and label. Node names are authoritative
// because the two stock DTBs use different labels ("happy" vs "GPIO Volume
// Voice-assistant") for the same unmarked centre key.
func ProbeKeyMap(prefix, compatible string) KeyMap {
	keys := DefaultKeyMap()
	base := DeviceTreeBase(prefix)
	if base == "" {
		return keys
	}
	node := FindCompatible(base, compatible)
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
		if semantic := KeySemantic(name, ReadTrim(filepath.Join(child, "label"))); semantic != "" {
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

// KeySemantic derives the framework key name from one device-tree key node.
// The node name and label are both consulted so a board that names the same
// physical control differently still maps to one semantic key.
func KeySemantic(nodeName, label string) string {
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
	case has("volumeup", "volup", "volume up"):
		return "volume_up"
	case has("volumedown", "voldown", "volume down"):
		return "volume_down"
	case has("playpause", "play-pause", "play/pause", "action"):
		// The circle at the bottom of the five-key layout: the play/pause talk
		// key. One short press toggles media, a hold is hold-to-talk. The
		// Amlogic Mini calls its single front key "action"; it is the same
		// media/assistant button.
		return "play_pause"
	case has("center", "centre", "happy", "voice", "assistant"):
		// The unmarked centre key: BTN_TRIGGER_HAPPY1 (label "happy") in one
		// DTB, KEY_VOICECOMMAND (label "GPIO Volume Voice-assistant") in the
		// other. It is neutral — it dismisses and anchors panel gestures,
		// never summons. "assistant" stays here: the RK3308 label
		// "GPIO Volume Voice-assistant" names the centre key, not play/pause.
		return "center"
	case has("primary", "enter", "ok"):
		return "primary"
	}
	return ""
}
