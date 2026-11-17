//go:build !linux || (!amd64 && !arm64)

package linuxdev

import "errors"

var errNoSPI = errors.New("spidev ring output is only implemented for linux/amd64 and linux/arm64")

func openSPIDevice(string, int, int) (spiDevice, error) { return nil, errNoSPI }
