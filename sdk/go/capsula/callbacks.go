package capsula

/*
#include <stdint.h>
#include "shim.h"
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// callbackClient resolves the cgo.Handle passed as the callback table's
// user_data. The handle keeps the Client alive for the bus lifetime, so the
// dispatcher never needs a global bus registry (and events cannot race the
// registration of a freshly opened bus).
func callbackClient(userData unsafe.Pointer) *Client {
	if userData == nil {
		return nil
	}
	value := cgo.Handle(uintptr(userData)).Value()
	client, _ := value.(*Client)
	return client
}

//export capsulaGoOnReady
func capsulaGoOnReady(bus C.capsula_handle, userData unsafe.Pointer) {
	defer func() { _ = recover() }()
	// Start observes readiness by bus_ready returning; nothing else to do.
}

//export capsulaGoOnDisconnected
func capsulaGoOnDisconnected(bus C.capsula_handle, status C.int32_t, detail *C.char, userData unsafe.Pointer) {
	defer func() { _ = recover() }()
	client := callbackClient(userData)
	if client == nil {
		return
	}
	text := ""
	if detail != nil {
		text = C.GoString(detail)
	}
	client.dispatchDisconnect(int32(status), text)
}

//export capsulaGoOnEvent
func capsulaGoOnEvent(bus C.capsula_handle, kind C.int32_t, data *C.uint8_t, length C.size_t, userData unsafe.Pointer) {
	defer func() { _ = recover() }()
	client := callbackClient(userData)
	if client == nil {
		return
	}
	client.onBusEvent(int32(kind), copyCallbackBytes(data, length))
}

//export capsulaGoOnCall
func capsulaGoOnCall(bus C.capsula_handle, call C.capsula_handle, info *C.uint8_t, length C.size_t, userData unsafe.Pointer) {
	defer func() { _ = recover() }()
	client := callbackClient(userData)
	if client == nil {
		return
	}
	client.onIncomingCall(uint64(call), copyCallbackBytes(info, length))
}

//export capsulaGoOnLog
func capsulaGoOnLog(level C.int32_t, message *C.char, userData unsafe.Pointer) {}

func copyCallbackBytes(data *C.uint8_t, length C.size_t) []byte {
	if data == nil || length == 0 {
		return nil
	}
	out := make([]byte, int(length))
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(data)), int(length)))
	return out
}

// dispatchDisconnect must not block the native callback: bus_close joins
// dispatch threads and waits for on_disconnected to return.
func (c *Client) dispatchDisconnect(status int32, detail string) {
	go c.onBusDisconnected(status, detail)
}
