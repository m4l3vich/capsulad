//go:build !linux

package bluetooth

import (
	"context"
	"errors"
)

// The management socket is a Linux kernel interface. Non-Linux hosts run the
// in-memory manager; a BlueZ manager cannot exist there, so this path only
// keeps the package buildable and reports the missing control honestly.
var errConnectableUnsupported = errors.New("Bluetooth connectability control requires Linux")

func setAdapterConnectable(context.Context, string, bool) error {
	return errConnectableUnsupported
}

func clearAdapterAcceptList(context.Context, string) error {
	return errConnectableUnsupported
}

func readAdapterConnectable(context.Context, string) (bool, bool, error) {
	return false, false, errConnectableUnsupported
}
