//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// vendorProcess is one running process the framework intends to replace.
type vendorProcess struct {
	PID  int
	Name string
}

// supersedeVendor removes the vendor assistant stack so the framework owns the
// microphone, the key input and the hardware watchdog. On the stock images the
// vendor chain is sysmon -> voice-agent (master + app); voice-agent also holds
// /dev/input/event0 and /dev/watchdog, and sysmon respawns it.
//
// The takeover is runtime only: init scripts are left alone so a failure to
// start capsulad still leaves the vendor stack working after a reboot.
func (d *Daemon) supersedeVendor() {
	names := d.supersedeNames()
	if len(names) == 0 {
		return
	}
	d.stopVendor(names)
	go d.reapVendor(names)
}

// supersedeNames resolves the configured policy: "off" disables takeover,
// "auto" targets the profile's vendor stack, otherwise a comma-separated list.
func (d *Daemon) supersedeNames() []string {
	switch strings.TrimSpace(strings.ToLower(d.cfg.Supersede)) {
	case "off", "none":
		return nil
	case "", "auto":
		return append([]string(nil), d.profile().VendorProcesses...)
	}
	var out []string
	for _, name := range strings.Split(d.cfg.Supersede, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// stopVendor terminates the named processes, escalating to SIGKILL, and then
// takes over any other holder of /dev/watchdog so the timer has one owner.
func (d *Daemon) stopVendor(names []string) {
	procs := findVendor(names)
	for _, p := range procs {
		d.logs.Infof("daemon", "", "superseding %s pid=%d (SIGTERM)", p.Name, p.PID)
		_ = syscall.Kill(p.PID, syscall.SIGTERM)
	}
	if !waitGone(procs, 3*time.Second) {
		for _, p := range findVendor(names) {
			d.logs.Warnf("daemon", "", "superseding %s pid=%d (SIGKILL, ignored SIGTERM)", p.Name, p.PID)
			_ = syscall.Kill(p.PID, syscall.SIGKILL)
		}
	}
	watchdogPath := d.profile().Watchdog.Path
	if watchdogPath != "" {
		for _, p := range watchdogHolders(watchdogPath) {
			d.logs.Infof("daemon", "", "superseding %s pid=%d (holds %s)", p.Name, p.PID, watchdogPath)
			_ = syscall.Kill(p.PID, syscall.SIGTERM)
			time.Sleep(150 * time.Millisecond)
		}
	}
}

// reapVendor keeps superseded processes from coming back, e.g. a supervisor
// that was not in the supersede list.
func (d *Daemon) reapVendor(names []string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			for _, p := range findVendor(names) {
				d.logs.Warnf("daemon", "", "superseded %s pid=%d respawned; stopping it", p.Name, p.PID)
				_ = syscall.Kill(p.PID, syscall.SIGKILL)
			}
		}
	}
}

// findVendor lists processes whose comm or command name matches one of names.
func findVendor(names []string) []vendorProcess {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []vendorProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if !want[name] {
			// Scripts and wrappers show up under the interpreter's comm.
			if argv, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline")); err == nil {
				for _, arg := range strings.Split(string(argv), "\x00") {
					if arg == "" {
						continue
					}
					if want[strings.TrimPrefix(filepath.Base(arg), "-")] {
						name = strings.TrimPrefix(filepath.Base(arg), "-")
						break
					}
				}
			}
		}
		if want[name] {
			out = append(out, vendorProcess{PID: pid, Name: name})
		}
	}
	return out
}

// watchdogHolders lists processes holding the configured watchdog device (and
// its aliases).
func watchdogHolders(watchdogPath string) []vendorProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []vendorProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", e.Name(), "fd"))
		if err != nil {
			continue
		}
		holds := false
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", e.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			if strings.HasPrefix(link, watchdogPath) {
				holds = true
				break
			}
		}
		if !holds {
			continue
		}
		name := strconv.Itoa(pid)
		if comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")); err == nil {
			name = strings.TrimSpace(string(comm))
		}
		out = append(out, vendorProcess{PID: pid, Name: name})
	}
	return out
}

// waitGone reports whether every process exited within timeout.
func waitGone(procs []vendorProcess, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive := false
		for _, p := range procs {
			if err := syscall.Kill(p.PID, 0); err == nil {
				alive = true
				break
			}
		}
		if !alive {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
