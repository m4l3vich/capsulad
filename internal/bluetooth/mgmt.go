package bluetooth

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Linux Bluetooth management API (public kernel ABI).
//
// BlueZ 5.65, the supported adapter stack, does not expose BR/EDR
// connectability as an Adapter1 D-Bus property. The kernel's public management
// socket interface does: MGMT_OP_SET_CONNECTABLE controls page scan (BR/EDR) and
// LE advertising connectivity, keeps the controller powered, and documents
// that switching connectable off also switches discoverable off. Because the
// kernel separately permits known devices added by BlueZ, denying incoming
// connections also clears the documented action list with MGMT_OP_REMOVE_DEVICE.
// We use only those public commands and the documented Read Controller
// Information reply; no private BlueZ or D-Bus API is involved.
//
// These helpers contain no socket code so the wire shape is testable on the
// host; mgmt_linux.go owns the Linux socket and other platforms report the
// control as unsupported.

const (
	mgmtOpReadControllerInfo = 0x0004
	mgmtOpSetConnectable     = 0x0007
	mgmtOpRemoveDevice       = 0x0034

	mgmtEventCommandComplete = 0x0001
	mgmtEventCommandStatus   = 0x0002

	mgmtStatusSuccess = 0x00

	// mgmtSettingConnectable is bit 1 of Current_Settings in the Read
	// Controller Information reply.
	mgmtSettingConnectable = 1 << 1
)

// These indirections keep the platform socket code out of policy tests. Tests
// replace them to assert ordering; production points at mgmt_*.go.
var (
	adapterSetConnectable  = setAdapterConnectable
	adapterReadConnectable = readAdapterConnectable
	adapterClearAcceptList = clearAdapterAcceptList
)

type mgmtEvent struct {
	Code    uint16
	Index   uint16
	Payload []byte
}

type mgmtCommandResult struct {
	Status uint8
	Data   []byte
}

func encodeMgmtCommand(code, index uint16, params []byte) []byte {
	out := make([]byte, 6+len(params))
	binary.LittleEndian.PutUint16(out[0:2], code)
	binary.LittleEndian.PutUint16(out[2:4], index)
	binary.LittleEndian.PutUint16(out[4:6], uint16(len(params)))
	copy(out[6:], params)
	return out
}

func decodeMgmtEvent(buf []byte) (mgmtEvent, bool) {
	if len(buf) < 6 {
		return mgmtEvent{}, false
	}
	length := int(binary.LittleEndian.Uint16(buf[4:6]))
	if len(buf) < 6+length {
		return mgmtEvent{}, false
	}
	return mgmtEvent{
		Code:    binary.LittleEndian.Uint16(buf[0:2]),
		Index:   binary.LittleEndian.Uint16(buf[2:4]),
		Payload: buf[6 : 6+length],
	}, true
}

// parseMgmtCommandResult matches the completion event for code. Command Status
// carries only a status; Command Complete carries return parameters.
func parseMgmtCommandResult(event mgmtEvent, code uint16) (mgmtCommandResult, bool) {
	if event.Code != mgmtEventCommandComplete && event.Code != mgmtEventCommandStatus {
		return mgmtCommandResult{}, false
	}
	if len(event.Payload) < 3 {
		return mgmtCommandResult{}, false
	}
	if binary.LittleEndian.Uint16(event.Payload[0:2]) != code {
		return mgmtCommandResult{}, false
	}
	result := mgmtCommandResult{Status: event.Payload[2]}
	if event.Code == mgmtEventCommandComplete {
		result.Data = event.Payload[3:]
	}
	return result, true
}

func mgmtSetConnectableParams(on bool) []byte {
	if on {
		return []byte{1}
	}
	return []byte{0}
}

// mgmtRemoveAllDevicesParams addresses BDADDR_ANY with the BR/EDR address
// type. The documented Remove Device semantics then clear every entry from the
// kernel action list, including devices BlueZ added as "known" for incoming
// connections. Without this, BlueZ 5.65's known-device allow list keeps page
// scan alive even after Set Connectable off, and a paired phone could still
// connect.
func mgmtRemoveAllDevicesParams() []byte {
	return make([]byte, 7) // Address 00:00:00:00:00:00, Address_Type BR/EDR
}

// parseConnectableSettings extracts the Connectable bit from the Read
// Controller Information return parameters.
func parseConnectableSettings(data []byte) (bool, bool) {
	// Address (6) + Bluetooth_Version (1) + Manufacturer (2) +
	// Supported_Settings (4) precede Current_Settings (4).
	const settingsOffset = 6 + 1 + 2 + 4
	if len(data) < settingsOffset+4 {
		return false, false
	}
	current := binary.LittleEndian.Uint32(data[settingsOffset : settingsOffset+4])
	return current&mgmtSettingConnectable != 0, true
}

// controllerIndex maps the BlueZ adapter id (hci0) to its management index.
func controllerIndex(adapter string) (uint16, error) {
	if adapter == "" || !strings.HasPrefix(adapter, "hci") {
		return 0, fmt.Errorf("unsupported Bluetooth adapter id %q", adapter)
	}
	index, err := strconv.ParseUint(strings.TrimPrefix(adapter, "hci"), 10, 16)
	if err != nil {
		return 0, fmt.Errorf("unsupported Bluetooth adapter id %q: %w", adapter, err)
	}
	return uint16(index), nil
}
