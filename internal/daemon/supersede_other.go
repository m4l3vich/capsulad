//go:build !linux

package daemon

// supersedeVendor is a no-op where the vendor stack does not exist.
func (d *Daemon) supersedeVendor() {}
