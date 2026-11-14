package capsula

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMarshalProtoJSONQuotesInt64AndBytes(t *testing.T) {
	chunk := AudioChunk{
		Sequence:        1<<63 + 5,
		FrameCount:      3,
		PCM:             []byte{1, 2, 3},
		Discontinuity:   true,
		FirstFrame:      9,
		MonotonicTimeNS: 10,
	}
	encoded, err := marshalProtoJSON(chunk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got, ok := raw["sequence"].(string); !ok || got != "9223372036854775813" {
		t.Fatalf("sequence = %#v, want quoted string", raw["sequence"])
	}
	if got, ok := raw["frameCount"].(float64); !ok || got != 3 {
		t.Fatalf("frameCount = %#v, want number", raw["frameCount"])
	}
	if got, ok := raw["pcm"].(string); !ok || got != "AQID" {
		t.Fatalf("pcm = %#v, want base64", raw["pcm"])
	}
}

func TestMarshalProtoJSONEnumNames(t *testing.T) {
	encoded, err := marshalProtoJSON(struct {
		Format AudioFormat `json:"format"`
	}{Format: DefaultAudioFormat()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"sampleFormat":"PCM_S16LE"`) {
		t.Fatalf("enum name missing: %s", encoded)
	}
}

func TestUnmarshalProtoJSONAcceptsCanonicalForms(t *testing.T) {
	payload := []byte(`{
		"revision": "42",
		"deviceAddress": "aa:bb",
		"durationMs": 1500,
		"unknownField": {"ignored": true}
	}`)
	var state BluetoothMediaState
	if err := unmarshalProtoJSON(payload, &state); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if state.Revision != 42 || state.DurationMS != 1500 || state.DeviceAddress != "aa:bb" {
		t.Fatalf("decoded state = %+v", state)
	}
}

func TestUnmarshalProtoJSONEnumNameAndNumber(t *testing.T) {
	var canonical struct {
		Origin ActivationOrigin `json:"origin"`
	}
	if err := unmarshalProtoJSON([]byte(`{"origin":"ACTIVATION_WAKEWORD"}`), &canonical); err != nil {
		t.Fatalf("unmarshal name: %v", err)
	}
	if canonical.Origin != ActivationOriginWakeword {
		t.Fatalf("origin = %v", canonical.Origin)
	}
	if err := unmarshalProtoJSON([]byte(`{"origin":2}`), &canonical); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if canonical.Origin != ActivationOriginTouch {
		t.Fatalf("origin = %v", canonical.Origin)
	}
	if err := unmarshalProtoJSON([]byte(`{"origin":"NOT_A_REAL_ORIGIN"}`), &canonical); err == nil {
		t.Fatal("unknown enum name accepted")
	}
}

// The framework carries the per-turn hold rule on the activation so an
// assistant does not need the administrator-only settings surface. Proto3
// omits a false field, so both the true and absent forms must decode.
func TestUnmarshalProtoJSONActivationHoldToTalk(t *testing.T) {
	var held Activation
	if err := unmarshalProtoJSON([]byte(`{"activationId":"act_1","origin":"ACTIVATION_TOUCH","holdToTalk":true}`), &held); err != nil {
		t.Fatalf("unmarshal hold-to-talk activation: %v", err)
	}
	if !held.HoldToTalk || held.Origin != ActivationOriginTouch || held.ID != "act_1" {
		t.Fatalf("hold-to-talk activation = %+v", held)
	}
	var summon Activation
	if err := unmarshalProtoJSON([]byte(`{"activationId":"act_2","origin":"ACTIVATION_TOUCH"}`), &summon); err != nil {
		t.Fatalf("unmarshal summon activation: %v", err)
	}
	if summon.HoldToTalk {
		t.Fatalf("absent holdToTalk decoded as true: %+v", summon)
	}
}

func TestUnmarshalProtoJSONBase64AndRGB(t *testing.T) {
	var value struct {
		PCM   []byte `json:"pcm"`
		Color RGB    `json:"color"`
	}
	if err := unmarshalProtoJSON([]byte(`{"pcm":"AQID","color":{"red":1,"green":2,"blue":3}}`), &value); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(value.PCM) != 3 || value.PCM[2] != 3 {
		t.Fatalf("pcm = %v", value.PCM)
	}
	if value.Color != (RGB{R: 1, G: 2, B: 3}) {
		t.Fatalf("color = %+v", value.Color)
	}
}

func TestUnmarshalProtoJSONRejectsOverflow(t *testing.T) {
	var value struct {
		Count uint32 `json:"count"`
	}
	if err := unmarshalProtoJSON([]byte(`{"count":"4294967296"}`), &value); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestMarshalProtoJSONRejectsNonStringMapKeys(t *testing.T) {
	if _, err := marshalProtoJSON(map[int]string{1: "x"}); err == nil {
		t.Fatal("expected map key error")
	}
}

func TestPresetArgsDeterministicAndTyped(t *testing.T) {
	args, err := presetArgs(map[string]any{
		"b": true,
		"a": "text",
		"c": RGB{R: 4, G: 5, B: 6},
		"d": int64(7),
	})
	if err != nil {
		t.Fatalf("presetArgs: %v", err)
	}
	if len(args) != 4 || args[0].Name != "a" || args[1].Name != "b" ||
		args[2].Name != "c" || args[3].Name != "d" {
		t.Fatalf("order = %+v", args)
	}
	if args[2].Value.Color == nil || *args[2].Value.Color != (RGB{R: 4, G: 5, B: 6}) {
		t.Fatalf("color = %+v", args[2].Value)
	}
	if args[3].Value.Integer == nil || *args[3].Value.Integer != 7 {
		t.Fatalf("integer = %+v", args[3].Value)
	}
}

func TestPresetArgsRejectsUnsupportedType(t *testing.T) {
	_, err := presetArgs(map[string]any{"bad": struct{}{}})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v", err)
	}
}

func TestPresentationFrameJSONShape(t *testing.T) {
	frame := &presentationFrame{RingEffect: &ringEffectFrame{
		EffectID: "system.ring.spinner",
		Arguments: []PresetArgument{{
			Name:  "speed",
			Value: &PresetValue{Integer: ptr(int64(2))},
		}},
	}}
	encoded, err := marshalProtoJSON(frame)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"ringEffect"`) ||
		!strings.Contains(string(encoded), `"integer":"2"`) {
		t.Fatalf("frame = %s", encoded)
	}
}

func TestPresentationRequestEnvelopes(t *testing.T) {
	open, err := marshalProtoJSON(presentationRequest{Open: &presentationOpenJSON{ResourceID: "display.segment"}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(string(open), `"open":{"resourceId":"display.segment"}`) {
		t.Fatalf("open envelope = %s", open)
	}
	frame, err := marshalProtoJSON(presentationRequest{Frame: &presentationFrame{
		Segment: &segmentFrame{Cells: []segmentCell{{Mask: 1}}, Colon: true},
	}})
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if !strings.Contains(string(frame), `"frame":{"segment":{"cells":[{"mask":1}],"colon":true}}`) {
		t.Fatalf("frame envelope = %s", frame)
	}
	closeRequest, err := marshalProtoJSON(presentationRequest{Close: &blankFrame{}})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(closeRequest) != `{"close":{}}` {
		t.Fatalf("close envelope = %s", closeRequest)
	}
}

func ptr[T any](v T) *T { return &v }

func TestLibraryErrorIsUnavailable(t *testing.T) {
	err := &LibraryError{Path: "/missing/libcapsula.so.1", Err: errors.New("no such file")}
	if !errors.Is(err, ErrLibraryUnavailable) {
		t.Fatal("LibraryError does not match ErrLibraryUnavailable")
	}
	if !strings.Contains(err.Error(), "/missing/libcapsula.so.1") ||
		!strings.Contains(err.Error(), "CAPSULA_LIBRARY") {
		t.Fatalf("actionable message missing: %v", err)
	}
}

func TestABIAndStatusErrors(t *testing.T) {
	abi := &ABIError{RequiredMajor: 1, ActualMajor: 2, ActualMinor: 1}
	if !strings.Contains(abi.Error(), "1.0") || !strings.Contains(abi.Error(), "2.1") {
		t.Fatalf("abi message: %v", abi)
	}
	status := &StatusError{Op: "GetSettings", Code: 8, Message: "resource exhausted"}
	if !strings.Contains(status.Error(), "GetSettings") ||
		!strings.Contains(status.Error(), "resource exhausted") {
		t.Fatalf("status message: %v", status)
	}
}

func TestMissingCapabilities(t *testing.T) {
	if missing := missingCapabilities([]string{CapabilityABIV1, CapabilityAudioPlayback}); len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if missing := missingCapabilities([]string{"capsula.made.up"}); len(missing) != 1 {
		t.Fatalf("missing = %v", missing)
	}
}

func TestTimeoutMillis(t *testing.T) {
	if got := timeoutMillis(nil, 0); got != 0 {
		t.Fatalf("no timeout = %d", got)
	}
	if got := timeoutMillis(nil, 1500*time.Millisecond); got != 1500 {
		t.Fatalf("timeout = %d", got)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := timeoutMillis(ctx, 0); got != 1 {
		t.Fatalf("expired deadline = %d", got)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("CAPSULA_LIBRARY", "/capsula/lib/libcapsula.so.1")
	t.Setenv("CAPSULA_RUN_ID", "run-1")
	t.Setenv("CAPSULA_APP_ID", "app-1")
	t.Setenv("CAPSULA_SDK", "0.5")
	t.Setenv("CAPSULA_SOCKET", "")
	env := FromEnv()
	if env.Library != "/capsula/lib/libcapsula.so.1" || env.RunID != "run-1" ||
		env.AppID != "app-1" || env.SDK != "0.5" ||
		env.Socket != "/run/capsulad/hal.sock" {
		t.Fatalf("env = %+v", env)
	}
}

func TestIncomingCallInfoSnakeCase(t *testing.T) {
	payload := []byte(`{"call_id":"call_1","caller_run_id":"run_2",` +
		`"grpc_service":"org.example.ring.v1.RingControllerService",` +
		`"rpc_name":"SetMode","deadline_monotonic_ns":"1234"}`)
	var info incomingCallInfo
	if err := unmarshalProtoJSON(payload, &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.CallID != "call_1" || info.CallerRunID != "run_2" ||
		info.GRPCService != "org.example.ring.v1.RingControllerService" ||
		info.RPCName != "SetMode" || info.DeadlineMonoNS != 1234 {
		t.Fatalf("info = %+v", info)
	}
}

func TestLoadRealNativeLibrary(t *testing.T) {
	path := os.Getenv("CAPSULA_TEST_LIBRARY")
	if path == "" {
		t.Skip("set CAPSULA_TEST_LIBRARY to the staged libcapsula artifact")
	}
	if err := ensureLibrary(path); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	major, minor := nativeABI()
	if major != abiMajor || minor < abiMinor {
		t.Fatalf("ABI = %d.%d, want >= %d.%d", major, minor, abiMajor, abiMinor)
	}
	if version := nativeVersion(); version == "" || version == "unloaded" {
		t.Fatalf("version = %q", version)
	}
}
