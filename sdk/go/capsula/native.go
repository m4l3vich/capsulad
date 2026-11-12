package capsula

/*
#cgo linux LDFLAGS: -ldl

#include <stdint.h>
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"fmt"
	"io"
	"runtime"
	"sync"
	"unsafe"
)

// Compile-time ABI identity expected by this binding.
const (
	abiMajor = 1
	abiMinor = 0
)

const (
	nativeErrSize = 512

	callKindUnary        = 0
	callKindServerStream = 1
	callKindBidi         = 3

	waitForever = 0xFFFFFFFF
)

// Native status values, mirrored as untyped constants so the rest of the
// package does not depend on cgo types.
const (
	statusOK          = 0
	statusClosed      = 4
	statusGRPC        = 8
	statusEndOfStream = 9
	statusTimeout     = 10
	statusCancelled   = 11
	statusBusy        = 13
)

var (
	loadOnce sync.Once
	loadErr  error
)

// ensureLibrary loads and validates the shared engine exactly once. The first
// requested path wins; a process has a single native runtime.
func ensureLibrary(path string) error {
	loadOnce.Do(func() {
		if path == "" {
			path = soname
		}
		cpath := C.CString(path)
		defer C.free(unsafe.Pointer(cpath))

		var message [1024]byte
		code := C.capsula_shim_load(cpath,
			(*C.char)(unsafe.Pointer(&message[0])), C.size_t(len(message)))
		switch int(code) {
		case 0:
			return
		case -2:
			loadErr = &ABIError{
				RequiredMajor: abiMajor,
				RequiredMinor: abiMinor,
				ActualMajor:   int(C.capsula_shim_abi_major()),
				ActualMinor:   int(C.capsula_shim_abi_minor()),
			}
		default:
			detail := C.GoString((*C.char)(unsafe.Pointer(&message[0])))
			if detail == "" {
				detail = "unknown loader failure"
			}
			loadErr = &LibraryError{Path: path, Err: errString(detail)}
		}
	})
	return loadErr
}

func nativeVersion() string { return C.GoString(C.capsula_shim_version()) }

func nativeABI() (int, int) {
	return int(C.capsula_shim_abi_major()), int(C.capsula_shim_abi_minor())
}

type errString string

func (e errString) Error() string { return string(e) }

// cstr borrows a Go string's bytes for the duration of one C call. The native
// ABI takes explicit lengths and never retains the pointer.
func cstr(s string) (*C.char, C.size_t) {
	if len(s) == 0 {
		return nil, 0
	}
	return (*C.char)(unsafe.Pointer(unsafe.StringData(s))), C.size_t(len(s))
}

// cbytes borrows a Go byte slice for one C call. The native ABI borrows input
// only for the duration of the call.
func cbytes(b []byte) (*C.uint8_t, C.size_t) {
	if len(b) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(b))), C.size_t(len(b))
}

func errPtr(buf *[nativeErrSize]byte) (*C.char, C.size_t) {
	return (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))
}

func errText(buf *[nativeErrSize]byte) string {
	if buf[0] == 0 {
		return ""
	}
	return C.GoString((*C.char)(unsafe.Pointer(buf)))
}

// takeBuf copies a library buffer into Go memory and frees it exactly once.
func takeBuf(buf C.capsula_buf) []byte {
	if buf.data == nil || buf.len == 0 {
		C.capsula_shim_buf_free(buf)
		return nil
	}
	out := make([]byte, int(buf.len))
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(buf.data)), int(buf.len)))
	C.capsula_shim_buf_free(buf)
	return out
}

func nativeError(op string, status C.int32_t, detail string) error {
	switch status {
	case C.CAPSULA_STATUS_OK:
		return nil
	case C.CAPSULA_STATUS_END_OF_STREAM:
		return io.EOF
	case C.CAPSULA_STATUS_CLOSED, C.CAPSULA_STATUS_CANCELLED:
		return ErrClosed
	}
	if detail == "" {
		detail = fmt.Sprintf("native status %d", int32(status))
	}
	return &StatusError{Op: op, Code: int32(status), Message: detail}
}

func connectNative(path, socket, runID, appID string, timeoutMS uint32) (uint64, error) {
	if err := ensureLibrary(path); err != nil {
		return 0, err
	}
	s, sl := cstr(socket)
	r, rl := cstr(runID)
	a, al := cstr(appID)
	var handle C.capsula_handle
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_client_connect(s, sl, r, rl, a, al, C.uint32_t(timeoutMS),
		&handle, ep, el)
	runtime.KeepAlive(socket)
	runtime.KeepAlive(runID)
	runtime.KeepAlive(appID)
	if err := nativeError("connect", status, errText(&errBuf)); err != nil {
		return 0, err
	}
	return uint64(handle), nil
}

func closeNative(handle uint64) error {
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_client_close(C.capsula_handle(handle), ep, el)
	return nativeError("close", status, errText(&errBuf))
}

func registerDescriptorsNative(handle uint64, set []byte) error {
	data, length := cbytes(set)
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_client_register_descriptors(C.capsula_handle(handle), data,
		length, ep, el)
	runtime.KeepAlive(set)
	return nativeError("register descriptors", status, errText(&errBuf))
}

func callStartNative(client uint64, service, method string, kind int32, timeoutMS uint32) (uint64, error) {
	svc, svcLen := cstr(service)
	m, mLen := cstr(method)
	var handle C.capsula_handle
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_start(C.capsula_handle(client), svc, svcLen, m, mLen,
		C.int32_t(kind), C.uint32_t(timeoutMS), &handle, ep, el)
	runtime.KeepAlive(service)
	runtime.KeepAlive(method)
	if err := nativeError("start "+service+"/"+method, status, errText(&errBuf)); err != nil {
		return 0, err
	}
	return uint64(handle), nil
}

func callStartRoutedNative(client uint64, targetAppID, service, method string, timeoutMS uint32) (uint64, error) {
	target, targetLen := cstr(targetAppID)
	svc, svcLen := cstr(service)
	m, mLen := cstr(method)
	var handle C.capsula_handle
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_start_routed(C.capsula_handle(client), target, targetLen,
		svc, svcLen, m, mLen, C.uint32_t(timeoutMS), &handle, ep, el)
	runtime.KeepAlive(targetAppID)
	runtime.KeepAlive(service)
	runtime.KeepAlive(method)
	if err := nativeError("start routed "+service+"/"+method, status, errText(&errBuf)); err != nil {
		return 0, err
	}
	return uint64(handle), nil
}

func callSendJSONNative(handle uint64, payload []byte) error {
	data, length := cbytes(payload)
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_send_json(C.capsula_handle(handle), data, length, ep, el)
	runtime.KeepAlive(payload)
	return nativeError("send", status, errText(&errBuf))
}

func callSendProtoNative(handle uint64, payload []byte) error {
	data, length := cbytes(payload)
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_send_proto(C.capsula_handle(handle), data, length, ep, el)
	runtime.KeepAlive(payload)
	return nativeError("send", status, errText(&errBuf))
}

func callHalfCloseNative(handle uint64) error {
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_half_close(C.capsula_handle(handle), ep, el)
	return nativeError("half-close", status, errText(&errBuf))
}

func callRecvJSONNative(handle uint64, timeoutMS uint32) ([]byte, C.int32_t, string) {
	var out C.capsula_buf
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_recv_json(C.capsula_handle(handle), C.uint32_t(timeoutMS),
		&out, ep, el)
	return takeBuf(out), status, errText(&errBuf)
}

func callRecvProtoNative(handle uint64, timeoutMS uint32) ([]byte, C.int32_t, string) {
	var out C.capsula_buf
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_recv_proto(C.capsula_handle(handle), C.uint32_t(timeoutMS),
		&out, ep, el)
	return takeBuf(out), status, errText(&errBuf)
}

func callEndNative(handle uint64, code int32, detail string) error {
	text, length := cstr(detail)
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_end(C.capsula_handle(handle), C.int32_t(code), text,
		length, ep, el)
	runtime.KeepAlive(detail)
	return nativeError("end", status, errText(&errBuf))
}

func callCancelNative(handle uint64) error {
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_cancel(C.capsula_handle(handle), ep, el)
	return nativeError("cancel", status, errText(&errBuf))
}

func callReleaseNative(handle uint64) error {
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_release(C.capsula_handle(handle), ep, el)
	return nativeError("release", status, errText(&errBuf))
}

// callTerminalError maps a finished gRPC call to an error using call_status.
func callTerminalError(op string, handle uint64) error {
	var code C.int32_t
	var detail C.capsula_buf
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_call_status(C.capsula_handle(handle), &code, &detail, ep, el)
	message := string(takeBuf(detail))
	if status != C.CAPSULA_STATUS_OK {
		if message == "" {
			message = errText(&errBuf)
		}
		if message == "" {
			message = fmt.Sprintf("native status %d", int32(status))
		}
		return &StatusError{Op: op, Code: int32(status), Message: message}
	}
	if code == 0 {
		return nil
	}
	return &StatusError{Op: op, Code: int32(code), Message: message}
}

func busOpenNative(client uint64, userData uintptr) (uint64, error) {
	var handle C.capsula_handle
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_bus_open(C.capsula_handle(client), C.uintptr_t(userData),
		&handle, ep, el)
	if err := nativeError("open app bus", status, errText(&errBuf)); err != nil {
		return 0, err
	}
	return uint64(handle), nil
}

func busCloseNative(handle uint64) error {
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_bus_close(C.capsula_handle(handle), ep, el)
	return nativeError("close app bus", status, errText(&errBuf))
}

func busReadyNative(handle uint64, timeoutMS uint32) error {
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_bus_ready(C.capsula_handle(handle), C.uint32_t(timeoutMS), ep, el)
	return nativeError("bus ready", status, errText(&errBuf))
}

func busNotifyInvocationNative(handle uint64, invocationID string, accepted bool, detail string) error {
	id, idLen := cstr(invocationID)
	d, dLen := cstr(detail)
	flag := C.int32_t(0)
	if accepted {
		flag = 1
	}
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_bus_notify_invocation(C.capsula_handle(handle), id, idLen,
		flag, d, dLen, ep, el)
	runtime.KeepAlive(invocationID)
	runtime.KeepAlive(detail)
	return nativeError("notify invocation", status, errText(&errBuf))
}

func busNotifyActionNative(handle uint64, actionID string, succeeded bool, detail string) error {
	id, idLen := cstr(actionID)
	d, dLen := cstr(detail)
	flag := C.int32_t(0)
	if succeeded {
		flag = 1
	}
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_bus_notify_action(C.capsula_handle(handle), id, idLen,
		flag, d, dLen, ep, el)
	runtime.KeepAlive(actionID)
	runtime.KeepAlive(detail)
	return nativeError("notify action", status, errText(&errBuf))
}

func pcmFormat(format AudioFormat) C.capsula_pcm_format {
	return C.capsula_pcm_format{
		sample_format:  C.int32_t(format.SampleFormat),
		sample_rate_hz: C.uint32_t(format.SampleRateHz),
		channels:       C.uint32_t(format.Channels),
		reserved:       0,
	}
}

func audioCaptureOpenNative(client uint64, endpoint string, format AudioFormat, packetMS uint32,
	activationID, conversationID string) (uint64, error) {
	ep, epLen := cstr(endpoint)
	act, actLen := cstr(activationID)
	conv, convLen := cstr(conversationID)
	cfg := pcmFormat(format)
	var handle C.capsula_handle
	var errBuf [nativeErrSize]byte
	errp, errl := errPtr(&errBuf)
	status := C.capsula_shim_audio_capture_open(C.capsula_handle(client), ep, epLen, &cfg,
		C.uint32_t(packetMS), act, actLen, conv, convLen, &handle, errp, errl)
	runtime.KeepAlive(endpoint)
	runtime.KeepAlive(activationID)
	runtime.KeepAlive(conversationID)
	if err := nativeError("open microphone", status, errText(&errBuf)); err != nil {
		return 0, err
	}
	return uint64(handle), nil
}

type nativePCMEvent struct {
	kind          int32
	discontinuity bool
	sequence      uint64
	firstFrame    uint64
	monotonic     uint64
	frameCount    uint32
	dropped       uint64
	pcm           []byte
	json          []byte
}

func audioCaptureRecvNative(handle uint64, timeoutMS uint32) (nativePCMEvent, C.int32_t, string) {
	var event C.capsula_pcm_event
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_audio_capture_recv(C.capsula_handle(handle),
		C.uint32_t(timeoutMS), &event, ep, el)
	out := nativePCMEvent{
		kind:          int32(event.kind),
		discontinuity: event.discontinuity != 0,
		sequence:      uint64(event.sequence),
		firstFrame:    uint64(event.first_frame),
		monotonic:     uint64(event.monotonic_time_ns),
		frameCount:    uint32(event.frame_count),
		dropped:       uint64(event.dropped_frames_before),
		pcm:           takeBuf(event.pcm),
		json:          takeBuf(event.json),
	}
	return out, status, errText(&errBuf)
}

func audioPlaybackOpenNative(client uint64, endpoint string, format AudioFormat, latencyMS uint32,
	conversationID string) (uint64, error) {
	ep, epLen := cstr(endpoint)
	conv, convLen := cstr(conversationID)
	cfg := pcmFormat(format)
	var handle C.capsula_handle
	var errBuf [nativeErrSize]byte
	errp, errl := errPtr(&errBuf)
	status := C.capsula_shim_audio_playback_open(C.capsula_handle(client), ep, epLen, &cfg,
		C.uint32_t(latencyMS), conv, convLen, &handle, errp, errl)
	runtime.KeepAlive(endpoint)
	runtime.KeepAlive(conversationID)
	if err := nativeError("open playback", status, errText(&errBuf)); err != nil {
		return 0, err
	}
	return uint64(handle), nil
}

func audioPlaybackWriteNative(handle uint64, pcm []byte, frameCount uint32, monotonicNS uint64,
	discontinuity bool, dropped uint64) error {
	data, length := cbytes(pcm)
	flag := C.int32_t(0)
	if discontinuity {
		flag = 1
	}
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_audio_playback_write(C.capsula_handle(handle), data, length,
		C.uint32_t(frameCount), C.uint64_t(monotonicNS), flag, C.uint64_t(dropped), ep, el)
	runtime.KeepAlive(pcm)
	return nativeError("playback write", status, errText(&errBuf))
}

func audioPlaybackControlNative(handle uint64, command PlaybackCommand) error {
	var errBuf [nativeErrSize]byte
	ep, el := errPtr(&errBuf)
	status := C.capsula_shim_audio_playback_control(C.capsula_handle(handle),
		C.int32_t(command), ep, el)
	return nativeError("playback control", status, errText(&errBuf))
}
