package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	clockv1 "capsulad/gen/go/capsulad/contracts/clock/v1"
	ringv1 "capsulad/gen/go/capsulad/contracts/ring/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
)

// The compiled-in clock and ring apps are ordinary public AppBus targets. A
// supervised application and the native SDK both put binary protobuf frames on
// that bus: the SDK converts JSON only at its reflection edge. These tests use
// the same external socket, declared services and routed call shape as an
// installed app, so a JSON-only handler implementation cannot pass by serving
// the daemon's private internal path.
//
// The user-reported failure was exactly that split: empty requests appeared to
// work (zero bytes decode as an empty protojson message), while a non-empty
// CancelTimer was rejected as a proto syntax error and every native JSON
// response failed to convert. The cases below therefore include a non-empty
// public clock request, a non-empty ring update, and a snapshot-first watch.

// TestBuiltinPublicAppBusBinaryWire is the retained regression: an ordinary
// external client discovers the clock/ring contracts through
// AppInterfaceService and calls them over AppBusService.Call with raw protobuf.
func TestBuiltinPublicAppBusBinaryWire(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	conn := dialTest(t, d.cfg.SocketPath)
	waitReady(t, d, clockAppID, 10*time.Second)
	waitReady(t, d, ringAppID, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ifaces, err := frameworkv1.NewAppInterfaceServiceClient(conn).ListInterfaces(ctx,
		&frameworkv1.ListAppInterfacesRequest{AppId: clockAppID})
	if err != nil {
		t.Fatalf("list clock interfaces: %v", err)
	}
	found := false
	for _, iface := range ifaces.GetInterfaces() {
		if iface.GetGrpcService() == clockServiceName && iface.GetAvailable() {
			found = true
		}
	}
	if !found {
		t.Fatalf("clock contract not advertised: %v", ifaces.GetInterfaces())
	}
	desc, err := frameworkv1.NewAppInterfaceServiceClient(conn).DescribeInterface(ctx,
		&frameworkv1.DescribeAppInterfaceRequest{AppId: clockAppID, GrpcService: clockServiceName})
	if err != nil {
		t.Fatalf("describe clock contract: %v", err)
	}
	if len(desc.GetFileDescriptorSet()) == 0 || len(desc.GetMethods()) == 0 {
		t.Fatalf("clock descriptor/methods missing: %d bytes, %d methods",
			len(desc.GetFileDescriptorSet()), len(desc.GetMethods()))
	}

	// A non-empty request whose successful reply is itself a protobuf message.
	payloads, code, detail := publicRoutedCall(t, conn, clockAppID, clockServiceName, "DismissAlert",
		mustProto(t, &clockv1.DismissAlertRequest{AlertId: "binary-wire-missing"}))
	if code != int32(codes.OK) {
		t.Fatalf("DismissAlert = code %d detail %q", code, detail)
	}
	if len(payloads) != 1 {
		t.Fatalf("DismissAlert returned %d messages", len(payloads))
	}
	var dismissed clockv1.DismissAlertResponse
	if err := proto.Unmarshal(payloads[0], &dismissed); err != nil {
		t.Fatalf("DismissAlert response is not binary protobuf (%q): %v", payloads[0], err)
	}
	if dismissed.GetDismissed() != 0 {
		t.Fatalf("unknown alert dismissed %d alerts", dismissed.GetDismissed())
	}

	// The reported non-empty CancelTimer case: the field must decode, so the
	// error is the app's NotFound semantics rather than a JSON syntax failure.
	payloads, code, detail = publicRoutedCall(t, conn, clockAppID, clockServiceName, "CancelTimer",
		mustProto(t, &clockv1.CancelTimerRequest{TimerId: "binary-wire-missing"}))
	if code != int32(codes.NotFound) {
		t.Fatalf("CancelTimer = code %d detail %q, want NotFound", code, detail)
	}
	if len(payloads) != 0 {
		t.Fatalf("CancelTimer error returned %d response messages", len(payloads))
	}
	if !strings.Contains(detail, "binary-wire-missing") || !strings.Contains(detail, "not active") {
		t.Fatalf("CancelTimer detail %q does not reflect the request field", detail)
	}

	// Ring settings: empty request in, binary snapshot out; then a non-empty
	// update whose response carries the committed snapshot.
	payloads, code, detail = publicRoutedCall(t, conn, ringAppID, ringServiceName, "GetSettings", nil)
	if code != int32(codes.OK) || len(payloads) != 1 {
		t.Fatalf("ring GetSettings = code %d detail %q messages %d", code, detail, len(payloads))
	}
	var before ringv1.RingSettingsSnapshot
	if err := proto.Unmarshal(payloads[0], &before); err != nil {
		t.Fatalf("ring GetSettings response is not binary protobuf (%q): %v", payloads[0], err)
	}
	if before.GetRevision() == 0 || before.GetValues() == nil {
		t.Fatalf("ring settings snapshot = %+v", &before)
	}

	payloads, code, detail = publicRoutedCall(t, conn, ringAppID, ringServiceName, "UpdateSettings",
		mustProto(t, &ringv1.UpdateSettingsRequest{
			ExpectedRevision: before.GetRevision(),
			Values:           &ringv1.RingSettings{Effect: "static"},
			UpdateFields:     []string{"effect"},
		}))
	if code != int32(codes.OK) || len(payloads) != 1 {
		t.Fatalf("ring UpdateSettings = code %d detail %q messages %d", code, detail, len(payloads))
	}
	var after ringv1.RingSettingsSnapshot
	if err := proto.Unmarshal(payloads[0], &after); err != nil {
		t.Fatalf("ring UpdateSettings response is not binary protobuf (%q): %v", payloads[0], err)
	}
	if after.GetValues().GetEffect() != "static" {
		t.Fatalf("updated effect = %q, want static", after.GetValues().GetEffect())
	}
	if after.GetRevision() != before.GetRevision()+1 {
		t.Fatalf("revision = %d, want %d", after.GetRevision(), before.GetRevision()+1)
	}
	if after.GetValues().GetPalette() != before.GetValues().GetPalette() ||
		after.GetValues().GetScheduleStart() != before.GetValues().GetScheduleStart() {
		t.Fatalf("unrelated ring settings changed: before %+v after %+v",
			before.GetValues(), after.GetValues())
	}

	// WatchState must answer with a binary snapshot before any transition. The
	// first frame is decoded with proto.Unmarshal, which rejects the old JSON.
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	stream, err := frameworkv1.NewAppBusServiceClient(conn).Call(watchCtx)
	if err != nil {
		t.Fatalf("watch call: %v", err)
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{
			TargetAppId: clockAppID, GrpcService: clockServiceName,
			RpcName: "WatchState", TimeoutMs: 5000,
		},
	}}); err != nil {
		t.Fatalf("watch start: %v", err)
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{
		RequestMessage: mustProto(t, &clockv1.WatchStateRequest{}),
	}}); err != nil {
		t.Fatalf("watch request: %v", err)
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_HalfClose{
		HalfClose: &frameworkv1.CallHalfClose{},
	}}); err != nil {
		t.Fatalf("watch half-close: %v", err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("watch snapshot: %v", err)
	}
	rawSnapshot := first.GetResponseMessage()
	if rawSnapshot == nil {
		t.Fatalf("watch first frame was not a snapshot: %+v", first)
	}
	var snapshot clockv1.ClockState
	if err := proto.Unmarshal(rawSnapshot, &snapshot); err != nil {
		t.Fatalf("watch snapshot is not binary protobuf (%q): %v", rawSnapshot, err)
	}
	if snapshot.GetTimezone() == "" || snapshot.GetRevision() == 0 {
		t.Fatalf("watch snapshot = %+v", &snapshot)
	}

	// Cancellation ends the stream instead of leaking a watcher, and the
	// daemon still answers the next unary call.
	stopWatch()
	stopped := make(chan error, 1)
	go func() {
		for {
			_, err := stream.Recv()
			if err != nil {
				stopped <- err
				return
			}
		}
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchState did not end after client cancellation")
	}
	payloads, code, detail = publicRoutedCall(t, conn, clockAppID, clockServiceName, "GetState", nil)
	if code != int32(codes.OK) || len(payloads) != 1 {
		t.Fatalf("clock GetState after watch cancellation = code %d detail %q messages %d",
			code, detail, len(payloads))
	}
	var state clockv1.ClockState
	if err := proto.Unmarshal(payloads[0], &state); err != nil {
		t.Fatalf("clock GetState response is not binary protobuf (%q): %v", payloads[0], err)
	}
	if state.GetLocalTimeRfc3339() == "" {
		t.Fatalf("clock GetState snapshot = %+v", &state)
	}
}

// publicRoutedCall performs one unary routed call with raw protobuf request
// bytes. The connection is an ordinary external local client; no daemon token
// or private handler is involved.
func publicRoutedCall(t *testing.T, conn *grpc.ClientConn, target, service, rpc string, request []byte) ([][]byte, int32, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := frameworkv1.NewAppBusServiceClient(conn).Call(ctx)
	if err != nil {
		t.Fatalf("%s/%s: open call: %v", service, rpc, err)
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{
			TargetAppId: target, GrpcService: service, RpcName: rpc, TimeoutMs: 5000,
		},
	}}); err != nil {
		t.Fatalf("%s/%s: start: %v", service, rpc, err)
	}
	if request != nil {
		if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{
			RequestMessage: request,
		}}); err != nil {
			t.Fatalf("%s/%s: request: %v", service, rpc, err)
		}
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_HalfClose{
		HalfClose: &frameworkv1.CallHalfClose{},
	}}); err != nil {
		t.Fatalf("%s/%s: half-close: %v", service, rpc, err)
	}
	var out [][]byte
	for {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("%s/%s: recv: %v", service, rpc, err)
		}
		if body := resp.GetResponseMessage(); body != nil {
			out = append(out, body)
			continue
		}
		if end := resp.GetEnd(); end != nil {
			return out, end.GetGrpcStatusCode(), end.GetDetail()
		}
	}
}

func mustProto(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %T: %v", msg, err)
	}
	return raw
}

// TestBuiltinNativeSDKJSONBoundary launches a real supervised app process that
// speaks the native SDK C ABI. It discovers the public contracts from the
// embedded/registered descriptors, sends JSON requests through
// call_send_json, and decodes every response through call_recv_json. The code
// under test is the public builtin handler and the bus; the native library is
// the current host artifact, not a test double.
func TestBuiltinNativeSDKJSONBoundary(t *testing.T) {
	libDir := nativeLibDir(t)
	if _, err := os.Stat(filepath.Join(libDir, "capsulad-native.json")); err != nil {
		t.Skipf("host native runtime unavailable in %s: %v", libDir, err)
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler for the native SDK probe")
	}

	base := shortTempDir(t)
	appID := nativeWireAppID
	appDir := filepath.Join(base, "apps", appID)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(base, "native_wire_probe.c")
	if err := os.WriteFile(source, []byte(nativeWireProbeSource), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(appDir, "probe")
	args := []string{
		"-std=c11", "-O2", "-Wall", "-Wextra",
		"-I", filepath.Join(moduleRoot(t), "sdk", "native", "include"),
		"-o", binary, source,
	}
	if runtime.GOOS == "linux" {
		args = append(args, "-ldl")
	}
	if output, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
		t.Fatalf("compile native SDK probe: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(appDir, "manifest.toml"), []byte(nativeWireManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "contracts.protoset"),
		nativeWireDescriptorSet(t, clockv1.File_capsulad_contracts_clock_v1_clock_proto,
			ringv1.File_capsulad_contracts_ring_v1_ring_proto), 0o644); err != nil {
		t.Fatal(err)
	}

	d := startEmptyDaemon(t, base)
	waitReady(t, d, clockAppID, 10*time.Second)
	waitReady(t, d, ringAppID, 10*time.Second)

	resultPath := filepath.Join(appDir, "native-wire-result.txt")
	waitFor(t, 30*time.Second, "native SDK probe result", func() bool {
		_, err := os.Stat(resultPath)
		return err == nil
	})
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native SDK wire result:\n%s", raw)
	lines, err := nativeWireLines(string(raw))
	if err != nil {
		t.Fatalf("native probe output:\n%s\n%v", raw, err)
	}

	if got := lineStatus(t, lines, "CONNECT"); got != "0" {
		t.Fatalf("CONNECT status = %s, output:\n%s", got, raw)
	}
	if got := lineStatus(t, lines, "DESCRIPTORS"); got != "0" {
		t.Fatalf("DESCRIPTORS status = %s, output:\n%s", got, raw)
	}

	// Non-empty JSON request, binary response through the SDK reflection edge.
	if got := lineStatus(t, lines, "CLOCK_DISMISS"); got != "0" {
		t.Fatalf("CLOCK_DISMISS status = %s, output:\n%s", got, raw)
	}
	var dismissed clockv1.DismissAlertResponse
	decodeNativeJSON(t, lines, "CLOCK_DISMISS_JSON", &dismissed)
	if dismissed.GetDismissed() != 0 {
		t.Fatalf("native DismissAlert dismissed %d alerts", dismissed.GetDismissed())
	}

	if got := lineStatus(t, lines, "CLOCK_STATE"); got != "0" {
		t.Fatalf("CLOCK_STATE status = %s, output:\n%s", got, raw)
	}
	var state clockv1.ClockState
	decodeNativeJSON(t, lines, "CLOCK_STATE_JSON", &state)
	if state.GetTimezone() == "" || state.GetRevision() == 0 {
		t.Fatalf("native ClockState = %+v", &state)
	}

	// The reported failing case: a non-empty CancelTimer must reach the
	// handler's NotFound path, not a proto/JSON syntax error.
	cancelLine := lines["CANCEL_TIMER"]
	if cancelLine == "" {
		t.Fatalf("missing CANCEL_TIMER line, output:\n%s", raw)
	}
	if got := lineStatus(t, lines, "CANCEL_TIMER"); got == "0" {
		t.Fatalf("native CancelTimer unexpectedly succeeded: %s", cancelLine)
	}
	if code := lineField(t, lines, "CANCEL_TIMER", "grpc"); code != "5" {
		t.Fatalf("native CancelTimer grpc = %s, want NotFound(5): %s", code, cancelLine)
	}
	detail := lineDetail(t, lines, "CANCEL_TIMER")
	if !strings.Contains(detail, "native-wire-none") || !strings.Contains(detail, "not active") {
		t.Fatalf("native CancelTimer detail %q does not reflect the request: %s", detail, cancelLine)
	}
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "invalid character") || strings.Contains(lower, "syntax error") ||
		strings.Contains(lower, "protojson") {
		t.Fatalf("native CancelTimer detail retains a JSON decode failure: %s", cancelLine)
	}

	if got := lineStatus(t, lines, "RING_SETTINGS"); got != "0" {
		t.Fatalf("RING_SETTINGS status = %s, output:\n%s", got, raw)
	}
	var before ringv1.RingSettingsSnapshot
	decodeNativeJSON(t, lines, "RING_SETTINGS_JSON", &before)
	if before.GetRevision() == 0 || before.GetValues() == nil {
		t.Fatalf("native ring settings = %+v", &before)
	}

	if got := lineStatus(t, lines, "RING_UPDATE"); got != "0" {
		t.Fatalf("RING_UPDATE status = %s, output:\n%s", got, raw)
	}
	var after ringv1.RingSettingsSnapshot
	decodeNativeJSON(t, lines, "RING_UPDATE_JSON", &after)
	if after.GetValues().GetEffect() != "static" {
		t.Fatalf("native ring update effect = %q, want static", after.GetValues().GetEffect())
	}
	if after.GetRevision() <= before.GetRevision() {
		t.Fatalf("native ring revision = %d, want > %d", after.GetRevision(), before.GetRevision())
	}

	// Snapshot-first watch response converted by call_recv_json, then a clean
	// cancel through the same native call handle.
	if got := lineStatus(t, lines, "WATCH"); got != "0" {
		t.Fatalf("WATCH status = %s, output:\n%s", got, raw)
	}
	var snapshot clockv1.ClockState
	decodeNativeJSON(t, lines, "WATCH_JSON", &snapshot)
	if snapshot.GetTimezone() == "" || snapshot.GetRevision() == 0 {
		t.Fatalf("native watch snapshot = %+v", &snapshot)
	}
	if got := lineStatus(t, lines, "WATCH_CANCEL"); got != "0" {
		t.Fatalf("WATCH_CANCEL status = %s, output:\n%s", got, raw)
	}
}

const nativeWireAppID = "native.wire.consumer"

const nativeWireManifest = `
id = "native.wire.consumer"
name = "Native wire consumer"
version = "0.1.0"
background = true
start_on_boot = true
restart_on_failure = false
single_instance = true

[entrypoint]
runtime = "native"
path = "probe"
architecture = "aarch64"
abi = "linux-musl"
`

// nativeWireDescriptorSet builds the descriptor closure the probe registers
// through client_register_descriptors, matching how an app supplies its own
// contract descriptors to the native reflection pool.
func nativeWireDescriptorSet(t *testing.T, files ...protoreflect.FileDescriptor) []byte {
	t.Helper()
	seen := map[string]bool{}
	var set descriptorpb.FileDescriptorSet
	var add func(protoreflect.FileDescriptor)
	add = func(file protoreflect.FileDescriptor) {
		if file == nil || seen[file.Path()] {
			return
		}
		seen[file.Path()] = true
		imports := file.Imports()
		for i := range imports.Len() {
			add(imports.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(file))
	}
	for _, file := range files {
		add(file)
	}
	raw, err := proto.Marshal(&set)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return raw
}

func nativeWireLines(output string) (map[string]string, error) {
	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			lines[key] = ""
			continue
		}
		lines[key] = value
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("probe produced no result lines")
	}
	return lines, nil
}

func lineStatus(t *testing.T, lines map[string]string, key string) string {
	t.Helper()
	return lineField(t, lines, key, "status")
}

func lineDetail(t *testing.T, lines map[string]string, key string) string {
	t.Helper()
	line, ok := lines[key]
	if !ok {
		t.Fatalf("missing %s line", key)
	}
	_, detail, found := strings.Cut(line, "detail=")
	if !found {
		t.Fatalf("%s has no detail field: %q", key, line)
	}
	return strings.TrimSpace(detail)
}

func lineField(t *testing.T, lines map[string]string, key, field string) string {
	t.Helper()
	line, ok := lines[key]
	if !ok {
		t.Fatalf("missing %s line", key)
	}
	for _, part := range strings.Fields(line) {
		if value, found := strings.CutPrefix(part, field+"="); found {
			return value
		}
	}
	t.Fatalf("%s has no %s field: %q", key, field, line)
	return ""
}

func decodeNativeJSON(t *testing.T, lines map[string]string, key string, msg proto.Message) {
	t.Helper()
	line, ok := lines[key]
	if !ok {
		t.Fatalf("missing %s line", key)
	}
	if err := protojson.Unmarshal([]byte(line), msg); err != nil {
		t.Fatalf("%s is not valid protobuf JSON (%q): %v", key, line, err)
	}
}

// nativeWireProbeSource is a temporary host probe, compiled into the app
// package by the test. It is not production code: it only exercises the
// documented C ABI as a supervised app process.
const nativeWireProbeSource = `
#include <dlfcn.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "capsulad/capsulad.h"

static capsulad_api api;
static FILE *result;

static void emit(const char *fmt, ...) {
    if (result == NULL) return;
    va_list ap;
    va_start(ap, fmt);
    vfprintf(result, fmt, ap);
    va_end(ap);
    fputc('\n', result);
    fflush(result);
}

static uint8_t *read_file(const char *path, size_t *out_len) {
    FILE *f = fopen(path, "rb");
    if (f == NULL) return NULL;
    if (fseek(f, 0, SEEK_END) != 0) { fclose(f); return NULL; }
    long size = ftell(f);
    if (size <= 0) { fclose(f); return NULL; }
    if (fseek(f, 0, SEEK_SET) != 0) { fclose(f); return NULL; }
    uint8_t *buf = malloc((size_t)size);
    if (buf == NULL) { fclose(f); return NULL; }
    size_t got = fread(buf, 1, (size_t)size, f);
    fclose(f);
    if (got != (size_t)size) { free(buf); return NULL; }
    *out_len = got;
    return buf;
}

static void free_buf(capsulad_buf *buf) {
    if (buf->data != NULL) {
        api.buf_free(*buf);
        buf->data = NULL;
        buf->len = 0;
    }
}

static int32_t begin_call(capsulad_handle client, const char *app,
                          const char *service, const char *method,
                          capsulad_handle *out) {
    *out = 0;
    return api.call_start_routed(client, app, strlen(app),
                                 service, strlen(service),
                                 method, strlen(method), 5000, out);
}

static void unary(capsulad_handle client, const char *label, const char *app,
                  const char *service, const char *method, const char *request) {
    capsulad_handle call = 0;
    int32_t start = begin_call(client, app, service, method, &call);
    if (start != 0) {
        emit("%s status=-1 grpc=-1 detail=start:%s", label, api.last_error());
        return;
    }
    int32_t send = api.call_send_json(call, (const uint8_t *)request, strlen(request));
    int32_t half = api.call_half_close(call);
    capsulad_buf body = {0};
    int32_t recv = api.call_recv_json(call, 5000, &body);
    int32_t grpc = 0;
    capsulad_buf detail = {0};
    api.call_status(call, &grpc, &detail);
    if (send != 0 || half != 0 || recv != 0) {
        emit("%s status=%d grpc=%d detail=send:%d half:%d recv:%d %.*s",
             label, recv, grpc, send, half, recv,
             (int)detail.len, detail.data != NULL ? (const char *)detail.data : "");
    } else {
        emit("%s status=0", label);
        emit("%s_JSON %.*s", label, (int)body.len,
             body.data != NULL ? (const char *)body.data : "");
    }
    free_buf(&body);
    free_buf(&detail);
    api.call_release(call);
}

/* A boot-time app may race the daemon's builtin starts. Retry only the
 * transient "not running/not ready" statuses so a real failure is reported by
 * the following assertion instead of being hidden here. */
static void wait_ready(capsulad_handle client, const char *app,
                       const char *service, const char *method,
                       const char *request) {
    for (int attempt = 0; attempt < 100; attempt++) {
        capsulad_handle call = 0;
        if (begin_call(client, app, service, method, &call) != 0) {
            usleep(100000);
            continue;
        }
        int32_t send = api.call_send_json(call, (const uint8_t *)request, strlen(request));
        int32_t half = api.call_half_close(call);
        capsulad_buf body = {0};
        int32_t recv = api.call_recv_json(call, 1000, &body);
        int32_t grpc = 0;
        capsulad_buf detail = {0};
        api.call_status(call, &grpc, &detail);
        free_buf(&body);
        free_buf(&detail);
        api.call_release(call);
        if (send == 0 && half == 0 && recv == 0) return;
        if (recv != CAPSULAD_STATUS_GRPC || (grpc != 9 && grpc != 14)) return;
        usleep(100000);
    }
}

static void watch_first(capsulad_handle client, const char *app,
                        const char *service, const char *method) {
    capsulad_handle call = 0;
    int32_t start = begin_call(client, app, service, method, &call);
    if (start != 0) {
        emit("WATCH status=-1 grpc=-1 detail=start:%s", api.last_error());
        return;
    }
    int32_t send = api.call_send_json(call, (const uint8_t *)"{}", 2);
    int32_t half = api.call_half_close(call);
    capsulad_buf body = {0};
    int32_t recv = api.call_recv_json(call, 5000, &body);
    int32_t grpc = 0;
    capsulad_buf detail = {0};
    api.call_status(call, &grpc, &detail);
    if (send != 0 || half != 0 || recv != 0) {
        emit("WATCH status=%d grpc=%d detail=send:%d half:%d recv:%d %.*s",
             recv, grpc, send, half, recv,
             (int)detail.len, detail.data != NULL ? (const char *)detail.data : "");
    } else {
        emit("WATCH status=0");
        emit("WATCH_JSON %.*s", (int)body.len,
             body.data != NULL ? (const char *)body.data : "");
    }
    free_buf(&body);
    free_buf(&detail);
    emit("WATCH_CANCEL status=%d", api.call_cancel(call));
    api.call_release(call);
}

static int finish(int code) {
    if (result != NULL) {
        fclose(result);
        result = NULL;
        /* Publish the complete report atomically so the test never reads a
         * half-written file while the app is still running. */
        rename("native-wire-result.tmp", "native-wire-result.txt");
    }
    return code;
}

int main(void) {
    result = fopen("native-wire-result.tmp", "w");
    if (result == NULL) return 2;

    const char *lib = getenv("CAPSULAD_LIBRARY");
    const char *sock = getenv("CAPSULAD_SOCKET");
    const char *run = getenv("CAPSULAD_RUN_ID");
    const char *app = getenv("CAPSULAD_APP_ID");
    if (lib == NULL || sock == NULL || run == NULL || app == NULL) {
        emit("ENV status=-1 detail=missing CAPSULAD_* environment");
        return finish(1);
    }

    void *handle = dlopen(lib, RTLD_NOW);
    if (handle == NULL) {
        emit("DLOPEN status=-1 detail=%s", dlerror());
        return finish(1);
    }
    size_t (*api_size)(void) = (size_t (*)(void))dlsym(handle, "capsulad_api_size");
    int32_t (*get_api)(uint32_t, size_t, capsulad_api *) =
        (int32_t (*)(uint32_t, size_t, capsulad_api *))dlsym(handle, "capsulad_get_api");
    if (api_size == NULL || get_api == NULL) {
        emit("ABI status=-1 detail=missing exports");
        return finish(1);
    }
    memset(&api, 0, sizeof api);
    if (get_api(CAPSULAD_ABI_MAJOR, sizeof api, &api) != 0) {
        emit("ABI status=-1 detail=get_api rejected");
        return finish(1);
    }

    capsulad_handle client = 0;
    int32_t connect = -1;
    for (int attempt = 0; attempt < 100; attempt++) {
        connect = api.client_connect(sock, strlen(sock), run, strlen(run),
                                     app, strlen(app), 5000, &client);
        if (connect == 0 && client != 0) break;
        usleep(100000);
    }
    if (connect != 0 || client == 0) {
        emit("CONNECT status=%d detail=%s", connect, api.last_error());
        return finish(1);
    }
    emit("CONNECT status=0");

    size_t set_len = 0;
    uint8_t *set = read_file("contracts.protoset", &set_len);
    if (set == NULL) {
        emit("DESCRIPTORS status=-1 detail=contracts.protoset missing");
    } else {
        int32_t registered = api.client_register_descriptors(client, set, set_len);
        emit("DESCRIPTORS status=%d", registered);
        free(set);
        if (registered != 0) {
            emit("DESCRIPTORS_DETAIL %s", api.last_error());
            return finish(1);
        }
    }

    const char *clock_app = "capsulad.clock";
    const char *clock_service = "capsulad.contracts.clock.v1.ClockService";
    const char *ring_app = "capsulad.ring";
    const char *ring_service = "capsulad.contracts.ring.v1.RingService";
    const char *dismiss = "{\"alertId\":\"native-wire-none\"}";

    wait_ready(client, clock_app, clock_service, "DismissAlert", dismiss);
    unary(client, "CLOCK_DISMISS", clock_app, clock_service, "DismissAlert", dismiss);
    unary(client, "CLOCK_STATE", clock_app, clock_service, "GetState", "{}");
    unary(client, "CANCEL_TIMER", clock_app, clock_service, "CancelTimer",
          "{\"timerId\":\"native-wire-none\"}");

    wait_ready(client, ring_app, ring_service, "GetSettings", "{}");
    unary(client, "RING_SETTINGS", ring_app, ring_service, "GetSettings", "{}");
    unary(client, "RING_UPDATE", ring_app, ring_service, "UpdateSettings",
          "{\"values\":{\"effect\":\"static\"},\"updateFields\":[\"effect\"]}");

    watch_first(client, clock_app, clock_service, "WatchState");

    api.client_close(client);
    return finish(0);
}
`
