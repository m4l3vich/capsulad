package linuxdev

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
	writeFile(t, filepath.Join(node, "linux,code"), string(buf[:]))
	writeFile(t, filepath.Join(node, "label"), label+"\x00")
}

func TestProbeKeyMapFromDeviceTree(t *testing.T) {
	root := t.TempDir()
	gpio := filepath.Join(root, "proc/device-tree/gpio-keys")
	if err := mkdirAll(gpio); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gpio, "compatible"), "gpio-keys\x00")
	writeKeyNode(t, gpio, "micmute-key", KeyMicMute, "mute")
	writeKeyNode(t, gpio, "volumeup-key", KeyVolumeUp, "volup")
	writeKeyNode(t, gpio, "playpause-key", KeyPlayPause, "play-pause")
	writeKeyNode(t, gpio, "volumedown-key", KeyVolumeDown, "voldown")
	// The two stock DTBs disagree on the talk-key label; the node name is
	// authoritative.
	writeKeyNode(t, gpio, "voice-key", KeyHappy, "happy")

	keys := ProbeKeyMap(root, "gpio-keys")
	want := map[int]string{
		KeyMicMute:    "mic_toggle",
		KeyVolumeUp:   "volume_up",
		KeyPlayPause:  "play_pause",
		KeyVolumeDown: "volume_down",
		KeyHappy:      "center",
	}
	for code, name := range want {
		if keys[code] != name {
			t.Fatalf("code %#x = %q, want %q", code, keys[code], name)
		}
	}
}

func TestProbeKeyMapFallbackCoversBothCentreCodes(t *testing.T) {
	keys := ProbeKeyMap(t.TempDir(), "gpio-keys")
	if keys[KeyVoiceCmd] != "center" || keys[KeyHappy] != "center" {
		t.Fatalf("centre codes = %q/%q", keys[KeyVoiceCmd], keys[KeyHappy])
	}
	if keys[KeyPlayPause] != "play_pause" {
		t.Fatalf("play/pause = %q", keys[KeyPlayPause])
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
		{"action-key", "GPIO Action", "play_pause"},
		{"unknown-key", "something", ""},
	} {
		if got := KeySemantic(tc.node, tc.label); got != tc.want {
			t.Fatalf("KeySemantic(%q,%q) = %q, want %q", tc.node, tc.label, got, tc.want)
		}
	}
}
