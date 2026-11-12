//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"capsulad/internal/manifest"
)

// Probe determines which isolation this kernel actually provides. It never
// claims a stronger mode than the probe proves.
func (l *Launcher) Probe(ctx context.Context) error {
	exe, err := l.executable()
	if err != nil {
		l.setProbeResult("none", "cannot locate daemon executable: "+err.Error(), false)
		return nil
	}
	if l.cfg.Mode == "none" {
		l.setProbeResult("none", "configured off", false)
		return nil
	}
	mode, detail := "", ""
	if l.cfg.Mode == "auto" || l.cfg.Mode == "namespaces" {
		if err := probeUserNamespace(ctx, exe); err == nil {
			mode = "namespaces"
			detail = "user+mount namespaces, read-only mounts"
			if os.Geteuid() != 0 {
				detail += "; daemon is unprivileged so apps share its host uid"
			}
		} else if l.cfg.Mode == "namespaces" {
			l.setProbeResult("none", "user namespace probe failed: "+err.Error(), false)
			return nil
		}
	}
	if mode == "" && os.Geteuid() == 0 {
		if err := probeMountNS(ctx, exe); err == nil {
			mode = "mountns"
			detail = "root mount namespace without user namespace; apps get distinct UIDs"
		}
	}
	if mode == "" {
		if l.cfg.Mode == "uid" {
			l.setProbeResult("uid", "credential drop only; no filesystem isolation", false)
			return nil
		}
		l.setProbeResult("none", "kernel lacks usable user/mount namespaces; apps are not filesystem-isolated", false)
		return nil
	}
	seccomp := false
	if l.cfg.Seccomp {
		if err := probeSeccomp(ctx, exe); err == nil {
			seccomp = true
			detail += ", seccomp deny list"
		} else {
			detail += "; seccomp deny list unavailable on this kernel"
		}
	}
	l.setProbeResult(mode, detail, seccomp)
	return nil
}

func (l *Launcher) executable() (string, error) {
	if l.cfg.Executable != "" {
		return l.cfg.Executable, nil
	}
	return os.Executable()
}

func probeUserNamespace(ctx context.Context, exe string) error {
	cmd := exec.CommandContext(ctx, exe, internalProbeArg)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	return cmd.Run()
}

func probeMountNS(ctx context.Context, exe string) error {
	cmd := exec.CommandContext(ctx, exe, internalProbeArg)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	return cmd.Run()
}

// probeSeccomp asks a helper process to install the deny list. The filter is
// irreversible for the process that installs it, so it cannot be probed
// in-daemon. CONFIG_SECCOMP_FILTER is absent on the RK3308 stock kernel, where
// PR_SET_SECCOMP returns EINVAL.
func probeSeccomp(ctx context.Context, exe string) error {
	return exec.CommandContext(ctx, exe, internalSeccompProbeArg).Run()
}

// seccompProbe is the helper side of probeSeccomp.
func seccompProbe() int {
	if err := applySeccomp(); err != nil {
		return 1
	}
	return 0
}

func (l *Launcher) namespaceCommand(m *manifest.Manifest, appDir, appPath, runID, appID string, env []string, libraryDir string) (*exec.Cmd, error) {
	exe, err := l.executable()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	mode := l.mode
	l.mu.Unlock()
	specJSON, err := l.specJSON(m, appDir, appPath, runID, appID, mode, env, libraryDir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, internalExecArg)
	cmd.Env = append(baseEnv(), specEnvVar+"="+specJSON)
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if mode == "namespaces" {
		uid, gid := l.AssignUID(appID)
		hostUID, hostGID := os.Geteuid(), os.Getegid()
		if os.Geteuid() == 0 {
			hostUID, hostGID = uid, gid
		}
		attr.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: hostUID, Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: hostGID, Size: 1}}
		attr.GidMappingsEnableSetgroups = false
		if l.cfg.PIDNamespace {
			attr.Cloneflags |= syscall.CLONE_NEWPID
		}
	} else {
		attr.Cloneflags = syscall.CLONE_NEWNS
		if l.cfg.PIDNamespace {
			attr.Cloneflags |= syscall.CLONE_NEWPID
		}
	}
	cmd.SysProcAttr = attr
	return cmd, nil
}

func (l *Launcher) uidCommand(appID, appPath string, env []string) (*exec.Cmd, error) {
	uid, gid := l.AssignUID(appID)
	cmd := exec.Command(appPath)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Credential: &syscall.Credential{
			Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true,
		},
	}
	return cmd, nil
}

// CleanupRun removes an empty per-run root directory left by a dead namespace.
func (l *Launcher) CleanupRun(runID string) {
	_ = os.RemoveAll(filepath.Join("/run/capsulad/sandbox", runID))
}

func runExecHelper() int {
	raw := os.Getenv(specEnvVar)
	if raw == "" {
		fmt.Fprintln(os.Stderr, "capsulad sandbox: missing spec")
		return 2
	}
	var spec execSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintf(os.Stderr, "capsulad sandbox: bad spec: %v\n", err)
		return 2
	}
	if err := setupAndExec(spec); err != nil {
		fmt.Fprintf(os.Stderr, "capsulad sandbox: %v\n", err)
		return 1
	}
	return 0
}

func setupAndExec(spec execSpec) error {
	// Make every mount in this namespace private so nothing propagates back.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("private mount propagation: %w", err)
	}
	if err := unix.Mount("tmpfs", spec.Root, "tmpfs", 0, "size=32m,mode=0755"); err != nil {
		return fmt.Errorf("mount sandbox root: %w", err)
	}
	for _, d := range []string{"app", "assets", "run/capsulad", "proc", "dev", "etc", "lib", "lib64", "usr", "usr/lib", "usr/lib64"} {
		if err := os.MkdirAll(filepath.Join(spec.Root, d), 0o755); err != nil {
			return err
		}
	}

	if err := bindReadOnly(spec.AppDir, filepath.Join(spec.Root, "app")); err != nil {
		return fmt.Errorf("bind /app: %w", err)
	}
	if spec.AssetsDir != "" {
		if _, err := os.Stat(spec.AssetsDir); err == nil {
			if err := bindReadOnly(spec.AssetsDir, filepath.Join(spec.Root, "assets")); err != nil {
				return fmt.Errorf("bind /assets: %w", err)
			}
		}
	}
	sockDst := filepath.Join(spec.Root, "run/capsulad/hal.sock")
	if err := touch(sockDst); err != nil {
		return err
	}
	if err := unix.Mount(spec.SocketPath, sockDst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind framework socket: %w", err)
	}
	// Read-only remount also prevents path replacement inside the sandbox.
	_ = unix.Mount("", sockDst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")

	for _, src := range spec.RuntimePaths {
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			continue // optional runtime path missing on this firmware
		}
		info, err := os.Stat(resolved)
		if err != nil {
			continue
		}
		dst := filepath.Join(spec.Root, strings.TrimPrefix(src, "/"))
		if info.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		} else {
			if err := touch(dst); err != nil {
				return err
			}
		}
		if err := bindReadOnly(resolved, dst); err != nil {
			return fmt.Errorf("bind %s: %w", src, err)
		}
	}

	// One host copy of the shared native SDK, mounted read-only at a fixed
	// path. Only the trusted LibraryDir is ever a mount source; metadata is
	// never allowed to name a host path.
	if spec.LibraryDir != "" {
		if !filepath.IsAbs(spec.LibraryDir) {
			return fmt.Errorf("native SDK library directory %q is not absolute", spec.LibraryDir)
		}
		dst := filepath.Join(spec.Root, strings.TrimPrefix(NamespaceLibraryDir, "/"))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := bindReadOnly(spec.LibraryDir, dst); err != nil {
			return fmt.Errorf("bind %s: %w", NamespaceLibraryDir, err)
		}
	}

	if spec.PIDNamespace {
		if err := unix.Mount("proc", filepath.Join(spec.Root, "proc"), "proc", 0, "hidepid=2"); err != nil {
			return fmt.Errorf("mount proc: %w", err)
		}
	}

	oldroot := filepath.Join(spec.Root, ".oldroot")
	if err := os.MkdirAll(oldroot, 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(spec.Root, oldroot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	_ = unix.Unmount("/.oldroot", unix.MNT_DETACH)
	_ = os.Remove("/.oldroot")

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if spec.Seccomp {
		if err := applySeccomp(); err != nil {
			return fmt.Errorf("seccomp: %w", err)
		}
	}

	// In mountns mode the helper is host root and must drop to the app UID.
	if !spec.UserNamespace {
		if err := unix.Setgroups([]int{}); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := unix.Setgid(spec.GID); err != nil {
			return fmt.Errorf("setgid: %w", err)
		}
		if err := unix.Setuid(spec.UID); err != nil {
			return fmt.Errorf("setuid: %w", err)
		}
	}
	env := spec.Env
	if len(env) == 0 {
		env = baseEnv()
	}
	return syscall.Exec(spec.AppPath, []string{spec.AppPath}, env)
}

func bindReadOnly(src, dst string) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	if err != nil && err != unix.EINVAL {
		return err
	}
	return nil
}

func touch(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o444)
	if err != nil {
		return err
	}
	return f.Close()
}

// applySeccomp installs a small deny list. It is not a capability policy; it
// removes operations an unprivileged local app has no reason to perform.
func applySeccomp() error {
	deny := []uintptr{
		unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
		unix.SYS_PTRACE, unix.SYS_KEXEC_LOAD, unix.SYS_REBOOT, unix.SYS_SWAPON,
		unix.SYS_SWAPOFF, unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE,
		unix.SYS_DELETE_MODULE, unix.SYS_ACCT, unix.SYS_SETTIMEOFDAY,
		unix.SYS_CLOCK_SETTIME, unix.SYS_SYSLOG, unix.SYS_BPF,
		unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD, unix.SYS_OPEN_BY_HANDLE_AT,
		unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_KCMP,
	}
	prog := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}}
	for _, nr := range deny {
		prog = append(prog,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: uint32(nr)},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	prog = append(prog, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := unix.Syscall(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_SECCOMP): %w", errno)
	}
	return nil
}
