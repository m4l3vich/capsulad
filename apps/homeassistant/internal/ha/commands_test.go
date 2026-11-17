package ha

import (
	"strings"
	"testing"
)

func ringPresets() []Preset {
	return []Preset{
		{ID: "system.ring.pulse", Kind: KindRingEffect},
		{ID: "system.ring.static", Kind: KindRingEffect},
	}
}

func facePresets() []Preset {
	return []Preset{
		{ID: "system.display.face.neutral", Kind: KindSegmentFace},
	}
}

func TestParseLightCommand(t *testing.T) {
	command, err := ParseLightCommand([]byte(`{"state":"ON","brightness":128,"color":{"r":1,"g":2,"b":3}}`))
	if err != nil {
		t.Fatalf("ParseLightCommand: %v", err)
	}
	if command.State != PayloadOn || !command.HasBrightness || command.Brightness != 128 {
		t.Fatalf("unexpected command %+v", command)
	}
	if !command.HasColor || command.Color != (RGB{R: 1, G: 2, B: 3}) {
		t.Fatalf("colour not parsed: %+v", command)
	}

	off, err := ParseLightCommand([]byte(`{"state":"OFF"}`))
	if err != nil {
		t.Fatalf("OFF: %v", err)
	}
	if off.State != PayloadOff || off.HasBrightness || off.HasColor {
		t.Fatalf("unexpected OFF command %+v", off)
	}

	effect, err := ParseLightCommand([]byte(`{"state":"ON","effect":"system.ring.pulse"}`))
	if err != nil {
		t.Fatalf("effect: %v", err)
	}
	if !effect.HasEffect || effect.Effect != "system.ring.pulse" {
		t.Fatalf("effect not parsed: %+v", effect)
	}

	// Home Assistant sends color_mode on modern versions; it is accepted and
	// does not change the interpretation.
	if _, err := ParseLightCommand([]byte(`{"state":"ON","color_mode":"rgb","color":{"r":1,"g":2,"b":3}}`)); err != nil {
		t.Fatalf("color_mode rejected: %v", err)
	}

	// flash defaults to true in Home Assistant's JSON light schema, so an
	// automation can send it even though the discovery payload declares
	// flash: false. Rejecting the whole command would leave the ring off.
	flashed, err := ParseLightCommand([]byte(`{"state":"ON","brightness":10,"flash":2}`))
	if err != nil {
		t.Fatalf("flash rejected: %v", err)
	}
	if flashed.State != PayloadOn || flashed.Brightness != 10 {
		t.Fatalf("flash command lost its meaning: %+v", flashed)
	}
	if _, err := ParseLightCommand([]byte(`{"state":"ON","flash":-1}`)); err == nil {
		t.Fatal("a negative flash duration was accepted")
	}

	bad := []struct {
		name    string
		payload string
	}{
		{"empty", ``},
		{"not json", `ON`},
		{"unknown key", `{"state":"ON","nope":1}`},
		{"bad state", `{"state":"on"}`},
		{"brightness high", `{"state":"ON","brightness":256}`},
		{"brightness negative", `{"state":"ON","brightness":-1}`},
		{"colour incomplete", `{"state":"ON","color":{"r":1,"g":2}}`},
		{"colour high", `{"state":"ON","color":{"r":300,"g":0,"b":0}}`},
		{"unknown colour mode", `{"state":"ON","color_mode":"xy"}`},
		{"no operation", `{"transition":1}`},
		{"negative transition", `{"state":"ON","transition":-1}`},
		{"trailing object", `{"state":"ON"}{"state":"OFF"}`},
	}
	for _, tc := range bad {
		if _, err := ParseLightCommand([]byte(tc.payload)); err == nil {
			t.Fatalf("%s: ParseLightCommand accepted %q", tc.name, tc.payload)
		}
	}
	if _, err := ParseLightCommand(make([]byte, maxCommandPayload+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestParseOnOff(t *testing.T) {
	for _, payload := range []string{"ON", "OFF", " ON "} {
		if _, err := ParseOnOff([]byte(payload)); err != nil {
			t.Fatalf("ParseOnOff(%q): %v", payload, err)
		}
	}
	for _, payload := range []string{"", "on", "1", "true", "ONOFF"} {
		if _, err := ParseOnOff([]byte(payload)); err == nil {
			t.Fatalf("ParseOnOff(%q) succeeded", payload)
		}
	}
}

func TestParseFaceCommand(t *testing.T) {
	options := facePresets()
	if _, err := ParseFaceCommand([]byte("system.display.face.neutral"), options); err != nil {
		t.Fatalf("ParseFaceCommand: %v", err)
	}
	for _, payload := range []string{"", "system.display.face.unknown", "neutral"} {
		if _, err := ParseFaceCommand([]byte(payload), options); err == nil {
			t.Fatalf("ParseFaceCommand(%q) succeeded", payload)
		}
	}
	if _, err := ParseFaceCommand([]byte("neutral"), nil); err == nil {
		t.Fatal("a preset command was accepted with an empty preset list")
	}
}

func TestParseEffectCommand(t *testing.T) {
	options := ringPresets()
	id, clear, err := ParseEffectCommand([]byte("system.ring.pulse"), options)
	if err != nil || clear || id != "system.ring.pulse" {
		t.Fatalf("ParseEffectCommand = %q, %v, %v", id, clear, err)
	}
	for _, payload := range []string{"None", "none"} {
		_, clear, err := ParseEffectCommand([]byte(payload), options)
		if err != nil || !clear {
			t.Fatalf("ParseEffectCommand(%q) = clear %v err %v, want a clear", payload, clear, err)
		}
	}
	// An empty payload is not a command at all, so it is refused rather than
	// interpreted as "clear the effect".
	if _, _, err := ParseEffectCommand(nil, options); err == nil {
		t.Fatal("an empty effect payload was accepted")
	}
	if _, _, err := ParseEffectCommand([]byte("system.ring.nope"), options); err == nil {
		t.Fatal("unknown effect accepted")
	}
}

func TestParseMediaCommand(t *testing.T) {
	for _, payload := range []string{"PLAY", "PLAY", PayloadPause, PayloadNext, PayloadPrevious} {
		if _, err := ParseMediaCommand([]byte(payload)); err != nil {
			t.Fatalf("ParseMediaCommand(%q): %v", payload, err)
		}
	}
	if _, err := ParseMediaCommand([]byte("play")); err != nil {
		t.Fatalf("lower-case media command rejected: %v", err)
	}
	for _, payload := range []string{"", "TOGGLE", "STOP", "PLAYPAUSE"} {
		if _, err := ParseMediaCommand([]byte(payload)); err == nil {
			t.Fatalf("ParseMediaCommand(%q) succeeded", payload)
		}
	}
}

func TestParseButtonCommand(t *testing.T) {
	if err := ParseButtonCommand([]byte(PayloadPress)); err != nil {
		t.Fatalf("ParseButtonCommand: %v", err)
	}
	for _, payload := range []string{"", "press", "PRESSED"} {
		if err := ParseButtonCommand([]byte(payload)); err == nil {
			t.Fatalf("ParseButtonCommand(%q) succeeded", payload)
		}
	}
}

func TestTruncateBoundsEchoedPayloads(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncate([]byte(long))
	if len(got) > 64 {
		t.Fatalf("truncate returned %d bytes", len(got))
	}
}
