//go:build linux

package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// setAdapterConnectable controls BR/EDR page scan and LE connectable
// advertising through the public Linux Bluetooth management socket. The
// controller stays powered; this is not a fake power-off.
func setAdapterConnectable(ctx context.Context, adapter string, on bool) error {
	index, err := controllerIndex(adapter)
	if err != nil {
		return err
	}
	result, err := mgmtExchange(ctx, index, mgmtOpSetConnectable, mgmtSetConnectableParams(on))
	if err != nil {
		return err
	}
	if result.Status != mgmtStatusSuccess {
		return fmt.Errorf("set Bluetooth connectable=%v: management status 0x%02x", on, result.Status)
	}
	return nil
}

// clearAdapterAcceptList removes every kernel action-list entry. The public
// Remove Device command documents an all-zero address as "remove all"; it is
// required because a known device is allowed to connect even when the global
// connectable setting is off. LE auto-connect entries are cleared too; an app
// that needs them can add its own entry again after the policy changes.
func clearAdapterAcceptList(ctx context.Context, adapter string) error {
	index, err := controllerIndex(adapter)
	if err != nil {
		return err
	}
	result, err := mgmtExchange(ctx, index, mgmtOpRemoveDevice, mgmtRemoveAllDevicesParams())
	if err != nil {
		return err
	}
	if result.Status != mgmtStatusSuccess {
		return fmt.Errorf("clear Bluetooth action list: management status 0x%02x", result.Status)
	}
	return nil
}

// readAdapterConnectable returns the observed Connectable setting. ok=false
// means the management socket could not report it (for example a permission
// failure); callers surface that instead of inventing a value.
func readAdapterConnectable(ctx context.Context, adapter string) (on bool, ok bool, err error) {
	index, err := controllerIndex(adapter)
	if err != nil {
		return false, false, err
	}
	result, err := mgmtExchange(ctx, index, mgmtOpReadControllerInfo, nil)
	if err != nil {
		return false, false, err
	}
	if result.Status != mgmtStatusSuccess {
		return false, false, fmt.Errorf("read Bluetooth controller info: management status 0x%02x", result.Status)
	}
	on, ok = parseConnectableSettings(result.Data)
	if !ok {
		return false, false, errors.New("read Bluetooth controller info: short reply")
	}
	return on, true, nil
}

func mgmtExchange(ctx context.Context, index, code uint16, params []byte) (mgmtCommandResult, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return mgmtCommandResult{}, fmt.Errorf("open Bluetooth management socket: %w", err)
	}
	defer unix.Close(fd)
	// HCI_DEV_NONE: the management channel is not bound to a controller.
	addr := &unix.SockaddrHCI{Dev: 0xffff, Channel: unix.HCI_CHANNEL_CONTROL}
	if err := unix.Bind(fd, addr); err != nil {
		return mgmtCommandResult{}, fmt.Errorf("bind Bluetooth management socket: %w", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return mgmtCommandResult{}, context.DeadlineExceeded
	}
	tv := unix.NsecToTimeval(timeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return mgmtCommandResult{}, fmt.Errorf("set Bluetooth management timeout: %w", err)
	}
	if err := unix.Sendto(fd, encodeMgmtCommand(code, index, params), 0, nil); err != nil {
		return mgmtCommandResult{}, fmt.Errorf("send Bluetooth management command 0x%04x: %w", code, err)
	}
	buf := make([]byte, 512)
	for {
		if err := ctx.Err(); err != nil {
			return mgmtCommandResult{}, err
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
				if time.Now().After(deadline) {
					return mgmtCommandResult{}, fmt.Errorf("Bluetooth management command 0x%04x timed out", code)
				}
				continue
			}
			return mgmtCommandResult{}, fmt.Errorf("receive Bluetooth management reply: %w", err)
		}
		event, ok := decodeMgmtEvent(buf[:n])
		if !ok || event.Index != index {
			continue
		}
		result, ok := parseMgmtCommandResult(event, code)
		if !ok {
			continue
		}
		return result, nil
	}
}
