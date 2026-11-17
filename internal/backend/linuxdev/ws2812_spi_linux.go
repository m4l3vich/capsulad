//go:build linux && (amd64 || arm64)

package linuxdev

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	spiIoctlWRMode        = 0x40016b01
	spiIoctlWRBitsPerWord = 0x40016b03
	spiIoctlWRMaxSpeedHz  = 0x40046b04
	spiIoctlRDMaxSpeedHz  = 0x80046b04
)

type spiDev struct{ f *os.File }

func openSPIDevice(path string, speedHz, bitsPerWord int) (spiDevice, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	if err := unix.IoctlSetInt(fd, spiIoctlWRMode, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("spi mode: %w", err)
	}
	if bitsPerWord > 0 {
		if err := unix.IoctlSetInt(fd, spiIoctlWRBitsPerWord, bitsPerWord); err != nil {
			f.Close()
			return nil, fmt.Errorf("spi bits per word: %w", err)
		}
	}
	if speedHz > 0 {
		if err := unix.IoctlSetInt(fd, spiIoctlWRMaxSpeedHz, speedHz); err != nil {
			f.Close()
			return nil, fmt.Errorf("spi speed: %w", err)
		}
	}
	return &spiDev{f: f}, nil
}

func (d *spiDev) Transfer(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	for {
		n, err := d.f.Write(p)
		if err == nil && n == len(p) {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err == nil {
			return fmt.Errorf("spi short transfer: %d of %d", n, len(p))
		}
		return err
	}
}

func (d *spiDev) Close() error { return d.f.Close() }

func (d *spiDev) SpeedHz() (int, error) {
	return unix.IoctlGetInt(int(d.f.Fd()), spiIoctlRDMaxSpeedHz)
}

func (d *spiDev) SetSpeed(hz int) error {
	if hz <= 0 {
		return nil
	}
	return unix.IoctlSetInt(int(d.f.Fd()), spiIoctlWRMaxSpeedHz, hz)
}
