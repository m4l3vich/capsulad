package rockchip

import (
	"encoding/binary"
	"path/filepath"
	"testing"
)

func writeKeyNode(t *testing.T, dir, name string, code uint32, label string) {
	t.Helper()
	node := filepath.Join(dir, name)
	if err := mkdirAll(node); err != nil {
		t.Fatal(err)
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], code)
	writeDTFile(t, node, "linux,code", buf[:])
	writeDTFile(t, node, "label", []byte(label+"\x00"))
}

func TestProbeKeyMapFromDeviceTree(t *testing.T) {
	root := t.TempDir()
	gpio := filepath.Join(root, "proc/device-tree/gpio-keys")
	if err := mkdirAll(gpio); err != nil {
		t.Fatal(err)
	}
	writeDTFile(t, gpio, "compatible", []byte("gpio-keys\x00"))
	writeKeyNode(t, gpio, "micmute-key", keyMicMute, "mute")
	writeKeyNode(t, gpio, "volumeup-key", keyVolumeUp, "volup")
	writeKeyNode(t, gpio, "playpause-key", keyPlayPause, "play-pause")
	writeKeyNode(t, gpio, "volumedown-key", keyVolumeDown, "voldown")
	// The two stock DTBs disagree on the centre key label; the node name is
	// authoritative.
	writeKeyNode(t, gpio, "voice-key", keyHappy, "happy")

	keys := probeKeyMap(root)
	want := map[int]string{
		keyMicMute:    "mic_toggle",
		keyVolumeUp:   "volume_up",
		keyPlayPause:  "play_pause",
		keyVolumeDown: "volume_down",
		keyHappy:      "center",
	}
	for code, name := range want {
		if keys[code] != name {
			t.Fatalf("code %#x = %q, want %q", code, keys[code], name)
		}
	}
}

func TestProbeKeyMapFallbackCoversBothCentreCodes(t *testing.T) {
	keys := probeKeyMap(t.TempDir())
	if keys[keyVoiceCmd] != "center" || keys[keyHappy] != "center" {
		t.Fatalf("centre codes = %q/%q", keys[keyVoiceCmd], keys[keyHappy])
	}
	if keys[keyPlayPause] != "play_pause" {
		t.Fatalf("play/pause = %q", keys[keyPlayPause])
	}
}

func TestKeySemantic(t *testing.T) {
	for _, tc := range []struct {
		node, label, want string
	}{
		{"voice-key", "happy", "center"},
		{"voice-key", "GPIO Volume Voice-assistant", "center"},
		{"center-key", "unmarked", "center"},
		{"micmute-key", "mute", "mic_toggle"},
		{"volumeup-key", "GPIO Volume Up", "volume_up"},
		{"volumedown-key", "GPIO Volume Down", "volume_down"},
		{"playpause-key", "GPIO Play/Pause", "play_pause"},
		{"unknown-key", "something", ""},
	} {
		if got := keySemantic(tc.node, tc.label); got != tc.want {
			t.Fatalf("keySemantic(%q,%q) = %q, want %q", tc.node, tc.label, got, tc.want)
		}
	}
}
