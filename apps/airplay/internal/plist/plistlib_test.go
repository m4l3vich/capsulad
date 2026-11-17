package plist

// Independent validation: Python's plistlib must accept what EncodeBinary
// writes. Gated on PLISTLIB_PYTHON so the default suite never depends on a
// Python environment; the full pyatv flow in internal/session exercises the
// same encoder over the wire.

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

const plistlibCheck = `
import plistlib, sys, json
failures = []
for path in sys.argv[1:]:
    try:
        plistlib.loads(open(path, "rb").read())
    except Exception as exc:
        failures.append((path, type(exc).__name__, str(exc)))
print(json.dumps(failures))
`

func TestPlistlibAcceptsEncodedDocuments(t *testing.T) {
	python := os.Getenv("PLISTLIB_PYTHON")
	if python == "" {
		t.Skip("set PLISTLIB_PYTHON to a python with plistlib to run the independent encoder check")
	}
	documents := map[string]any{
		"info": map[string]any{
			"deviceID": "aabbccddeeff", "features": int64(1 << 18), "statusFlags": int64(0),
			"name": "Loopback", "model": "AudioAccessory5,1", "pi": "pi-uuid", "psi": "psi-uuid",
			"pk": make([]byte, 32), "protocolVersion": "1.1", "sourceVersion": "366.0",
			"keepAliveLowPower": true, "keepAliveSendStatsAsBody": false,
			"audioLatencies":   []any{map[string]any{"inputLatencyMicros": int64(0), "outputLatencyMicros": int64(1500000)}},
			"supportedFormats": map[string]any{"audioStream": int64(262144), "bufferStream": int64(262144)},
			"txtAirPlay":       make([]byte, 40), "volumeControlType": int64(3), "vv": int64(2),
		},
		"setup": map[string]any{"streams": []any{map[string]any{
			"type": int64(96), "controlPort": int64(6001), "dataPort": int64(6000),
			"audioBufferSize": int64(1 << 20), "streamID": int64(1),
		}}},
		"update": map[string]any{"type": "updateInfo", "value": map[string]any{"name": "Loopback"}},
	}
	script := t.TempDir() + "/check.py"
	if err := os.WriteFile(script, []byte(plistlibCheck), 0o600); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for name, document := range documents {
		blob, err := EncodeBinary(document)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		path := t.TempDir() + "/" + name + ".plist"
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	command := exec.Command(python, append([]string{script}, paths...)...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("plistlib check failed to run: %v", err)
	}
	var failures [][3]string
	if err := json.Unmarshal(output, &failures); err != nil {
		t.Fatalf("plistlib output %q: %v", output, err)
	}
	if len(failures) > 0 {
		t.Fatalf("plistlib rejected encoded documents: %v", failures)
	}
}
