package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The app interface is published as a protobuf descriptor and served through
// canonical protobuf JSON: the native engine converts the Go struct's JSON into
// the descriptor's message. A field name that drifts from the .proto therefore
// fails at runtime, inside a call the daemon reports only as an error. These
// tests hold the wire shapes against the descriptor source.

func protoSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "proto", "homeassistant.proto")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

var (
	messageStart = regexp.MustCompile(`(?m)^message\s+(\w+)\s*\{`)
	fieldDecl    = regexp.MustCompile(`(?m)^\s*(?:bool|string|uint32|uint64|int32|int64|double)\s+(\w+)\s*=\s*\d+;`)
	serviceDecl  = regexp.MustCompile(`(?m)^service\s+(\w+)\s*\{`)
	rpcDecl      = regexp.MustCompile(`(?m)^\s*rpc\s+(\w+)\s*\(`)
)

// protoMessageFields returns the declared field names of one message, converted
// to canonical protobuf JSON (lowerCamelCase).
func protoMessageFields(t *testing.T, message string) []string {
	t.Helper()
	source := protoSource(t)
	start := -1
	for _, match := range messageStart.FindAllStringSubmatchIndex(source, -1) {
		if source[match[2]:match[3]] == message {
			start = match[0]
			break
		}
	}
	if start < 0 {
		t.Fatalf("message %s is not declared", message)
	}
	// These messages have no nested declarations, so the first closing brace
	// after the opening one ends the body (and an empty message is "{}").
	end := strings.Index(source[start:], "}")
	if end < 0 {
		t.Fatalf("message %s is not terminated", message)
	}
	body := source[start : start+end]
	var fields []string
	for _, match := range fieldDecl.FindAllStringSubmatch(body, -1) {
		fields = append(fields, camel(match[1]))
	}
	sort.Strings(fields)
	return fields
}

// protoMethods returns the RPC names of the published service.
func protoMethods(t *testing.T) []string {
	t.Helper()
	source := protoSource(t)
	if !serviceDecl.MatchString(source) {
		t.Fatal("the .proto declares no service")
	}
	var methods []string
	for _, match := range rpcDecl.FindAllStringSubmatch(source, -1) {
		methods = append(methods, match[1])
	}
	sort.Strings(methods)
	return methods
}

// camel converts a protobuf snake_case field name to canonical JSON.
func camel(name string) string {
	parts := strings.Split(name, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, "")
}

// jsonFields returns the JSON object keys a value marshals to.
func jsonFields(t *testing.T, value any) []string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("%T does not marshal to a JSON object: %v", value, err)
	}
	fields := make([]string, 0, len(decoded))
	for key := range decoded {
		fields = append(fields, key)
	}
	sort.Strings(fields)
	return fields
}

func assertFieldsMatch(t *testing.T, message string, value any) {
	t.Helper()
	want := protoMessageFields(t, message)
	got := jsonFields(t, value)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: JSON keys [%s] do not match the descriptor fields [%s]",
			message, strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

func TestInterfacePayloadsMatchTheDescriptor(t *testing.T) {
	if got, want := len(protoMethods(t)), 3; got != want {
		t.Fatalf("the service declares %d RPCs, want %d", got, want)
	}
	methods := protoMethods(t)
	for _, want := range []string{"GetStatus", "PublishDiscovery", "PublishEvent"} {
		found := false
		for _, method := range methods {
			if method == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("the descriptor lacks %s (has %v)", want, methods)
		}
	}

	assertFieldsMatch(t, "GetStatusRequest", getStatusRequest{})
	assertFieldsMatch(t, "Status", statusJSON{
		Configured:      true,
		Connected:       true,
		Broker:          "broker:1883",
		NodeID:          "capsula",
		DiscoveryPrefix: "homeassistant",
		BaseTopic:       "capsulad",
		Entities:        12,
		RingOn:          true,
		RingEffect:      "system.ring.pulse",
		DisplayOn:       true,
		DisplayFace:     "system.display.face.neutral",
		BridgeStatus:    "online",
		LastError:       "",
		Published:       3,
		Commands:        2,
		Failures:        0,
	})
	assertFieldsMatch(t, "PublishDiscoveryRequest", publishDiscoveryRequest{})
	assertFieldsMatch(t, "PublishDiscoveryResponse", publishDiscoveryResponse{Published: 4})
	assertFieldsMatch(t, "PublishEventRequest", publishEventRequest{EventType: "center_press", PayloadJSON: "{}"})
	assertFieldsMatch(t, "PublishEventResponse", publishEventResponse{Published: true})
}

func TestInterfaceServiceNameIsDeclared(t *testing.T) {
	source := protoSource(t)
	match := serviceDecl.FindStringSubmatch(source)
	if match == nil {
		t.Fatal("no service declaration")
	}
	if !strings.HasPrefix(interfaceService, "org.capsulad.homeassistant.v1.") {
		t.Fatalf("interfaceService %q is not in the published package", interfaceService)
	}
	if got := "org.capsulad.homeassistant.v1." + match[1]; got != interfaceService {
		t.Fatalf("the descriptor declares %q, the app serves %q", got, interfaceService)
	}
}

func TestStatusReflectsConfigurationOnly(t *testing.T) {
	app := &app{}
	// No settings snapshot yet: the bridge is unconfigured and publishes no
	// broker address.
	status := app.status()
	if status.Configured {
		t.Fatal("status reports configured without a settings snapshot")
	}
	if status.Broker != "" || status.NodeID != "" || status.Entities != 0 {
		t.Fatalf("unexpected status %+v", status)
	}
	if status.BridgeStatus != "stopped" {
		t.Fatalf("bridge status %q, want stopped", status.BridgeStatus)
	}
	if detail := app.describe(); !strings.Contains(detail, "no settings") {
		t.Fatalf("describe() = %q", detail)
	}
}
