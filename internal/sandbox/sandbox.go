// Package sandbox launches supervised app processes.
//
// On Linux, apps run in an unprivileged user namespace with an isolated mount
// namespace: /app and /assets are read-only bind mounts, the framework socket
// is the only filesystem endpoint and there is no writable /tmp or home. A
// small seccomp deny list plus no_new_privs limits kernel-surface abuse when
// the kernel accepts a filter; the launcher probes for that and drops the
// claim rather than failing the launch. The contract is deliberately uniform;
// it is not a per-app permission system.
//
// The Rockchip stock kernel (4.4.167) must be probed for user/mount namespace
// support before stronger claims are made; when the probe fails the daemon
// falls back explicitly and reports the actual mode in diagnostics.
//
// The shared native SDK runtime is installed once on the host, mounted
// read-only at /capsulad/lib in namespace modes and exposed through
// CAPSULAD_LIBRARY. Apps never bundle a copy; see runtime.go.
package sandbox

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"capsulad/internal/manifest"
	"capsulad/internal/version"
)

// Config controls launch behavior. Paths are host paths.
type Config struct {
	Mode         string // auto, namespaces, uid, none
	SocketPath   string
	AssetsDir    string
	RuntimePaths []string
	// LibraryDir is the host directory holding the shared native SDK runtime
	// (libcapsulad.so.1 and capsulad-native.json). Empty means resolve beside
	// the daemon binary: <exe-dir>/lib, then <exe-dir>/../lib.
	LibraryDir string
	// DeviceSerial is the SoC serial the daemon read at startup. It is handed
	// to every app as CAPSULAD_DEVICE_SERIAL; an empty value omits the variable
	// instead of exporting an empty one, so an app can tell "unknown" from a
	// value the launcher assigned.
	DeviceSerial string
	UIDBase      int
	PIDNamespace bool
	Seccomp      bool
	Executable   string // daemon binary used for the in-namespace helper
	// SandboxRoot is the per-run helper root. Tests override it; production
	// uses the fixed /run path below so the helper can wipe a dead run.
	SandboxRoot string
}

type Launcher struct {
	cfg Config

	mu      sync.Mutex
	mode    string
	det     string
	seccomp bool
	tz      string
	uids    map[string]int
	next    int

	libOnce sync.Once
	libDir  string
	libErr  error
}

func New(cfg Config) *Launcher {
	if cfg.UIDBase == 0 {
		cfg.UIDBase = 20000
	}
	if len(cfg.RuntimePaths) == 0 {
		cfg.RuntimePaths = DefaultRuntimePaths()
	}
	l := &Launcher{cfg: cfg, mode: cfg.Mode, uids: map[string]int{}}
	if l.mode == "" || l.mode == "auto" {
		l.mode = "auto"
	}
	return l
}

func DefaultRuntimePaths() []string {
	return []string{
		"/lib", "/lib64", "/usr/lib", "/usr/lib64",
		"/etc/ld.so.cache", "/etc/ssl", "/etc/ca-certificates",
		"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/localtime",
		"/usr/share/zoneinfo",
		"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom",
	}
}

// Mode returns the effective sandbox mode after probing.
func (l *Launcher) Mode() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode
}

func (l *Launcher) Detail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.det
}

// SetProbeResult records a successful probe.
func (l *Launcher) setMode(mode, detail string) {
	l.mu.Lock()
	l.mode = mode
	l.det = detail
	l.mu.Unlock()
}

// SetTimezone sets the device wall clock handed to every app as TZ. An empty
// value leaves the app on /etc/localtime, which is bind-mounted into the
// sandbox. Apps started before the change keep their launch value.
func (l *Launcher) SetTimezone(tz string) {
	l.mu.Lock()
	l.tz = tz
	l.mu.Unlock()
}

func (l *Launcher) Timezone() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tz
}

// env is the launch environment: baseEnv plus the device timezone when one is
// configured. An explicit setting replaces an inherited TZ rather than racing
// it as a duplicate entry.
func (l *Launcher) env() []string {
	env := baseEnv()
	out := make([]string, 0, len(env)+2)
	if tz := l.Timezone(); tz != "" {
		for _, entry := range env {
			if strings.HasPrefix(entry, "TZ=") {
				continue
			}
			out = append(out, entry)
		}
		env = append(out, "TZ="+tz)
		out = env
	}
	// CAPSULAD_ASSETS is the app-facing read-only asset store. Namespace modes
	// bind it at /assets; uid/none modes receive the host path so the same
	// contract works without a mount namespace.
	assets := l.cfg.AssetsDir
	if assets != "" && namespaceMode(l.Mode()) {
		assets = "/assets"
	}
	if assets != "" {
		env = append(env, "CAPSULAD_ASSETS="+assets)
	}
	// CAPSULAD_DEVICE_SERIAL is the descriptor-free path to the SoC serial. A
	// native runtime built before DeviceInfo.hardware_serial existed drops the
	// field from JSON replies, so apps cannot rely on the protocol there.
	if serial := strings.TrimSpace(l.cfg.DeviceSerial); serial != "" {
		env = append(env, "CAPSULAD_DEVICE_SERIAL="+serial)
	}
	return env
}

// Seccomp reports whether the kernel accepted the deny-list filter. It is only
// true after Probe ran and the filter was installed by a helper process; the
// requested Config.Seccomp is a request, not a capability.
func (l *Launcher) Seccomp() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seccomp
}

func (l *Launcher) setProbeResult(mode, detail string, seccomp bool) {
	l.mu.Lock()
	l.mode = mode
	l.det = detail
	l.seccomp = seccomp
	l.mu.Unlock()
}

// libraryDir resolves the shared native SDK directory once. An explicitly
// configured but unusable directory is an operator error surfaced at launch;
// an absent default directory simply means apps run without the SDK.
func (l *Launcher) libraryDir() (string, error) {
	l.libOnce.Do(func() {
		l.libDir, l.libErr = ResolveLibraryDir(l.cfg.LibraryDir)
	})
	return l.libDir, l.libErr
}

func (l *Launcher) sandboxRoot() string {
	if l.cfg.SandboxRoot != "" {
		return l.cfg.SandboxRoot
	}
	return "/run/capsulad/sandbox"
}

// Command builds the app process command. Native entrypoints run their own
// binary; script entrypoints run the installed interpreter host with the
// package script as its first argument. Both paths use the same shared SDK
// environment and the same sandbox modes. A script runtime without its real
// engine host is an explicit unavailable error, never a stand-in interpreter.
func (l *Launcher) Command(m *manifest.Manifest, appDir, runID, appID string) (*exec.Cmd, error) {
	absDir, err := filepath.Abs(appDir)
	if err != nil {
		return nil, err
	}
	appDir = absDir
	l.mu.Lock()
	mode := l.mode
	l.mu.Unlock()

	libDir, err := l.libraryDir()
	if err != nil {
		return nil, err
	}

	switch m.Entrypoint.Runtime {
	case manifest.RuntimeNative:
		appPath, err := filepath.Abs(filepath.Join(appDir, m.Entrypoint.Path))
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(appPath)
		if err != nil {
			return nil, fmt.Errorf("entrypoint: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("entrypoint %s is a directory", m.Entrypoint.Path)
		}
		// Optional SDK use: discover the runtime for the environment but never
		// fail an app that did not declare a requirement because the runtime is
		// missing or its metadata is unusable.
		rt, _ := findRuntime(libDir, false)
		if m.NativeSDK != nil {
			// Required SDK use: strict discovery requires an installed runtime and
			// generated metadata, then the requirement preflight runs. This does
			// not verify the library's exported ABI; the binding still does that.
			rt, err = LocateRuntime(libDir)
			if err != nil {
				return nil, fmt.Errorf("app %s requires native SDK ABI %d.%d: %w", m.ID, m.NativeSDK.ABIMajor, m.NativeSDK.MinMinor, err)
			}
			if err := CheckRequirement(rt, *m.NativeSDK); err != nil {
				return nil, fmt.Errorf("app %s: %w", m.ID, err)
			}
		}
		env := append(l.env(), LibraryEnv(mode, rt)...)
		mountLib := ""
		if rt != nil {
			mountLib = libDir
		}
		switch mode {
		case "namespaces", "mountns":
			return l.namespaceCommand(m, appDir, filepath.Join("/app", m.Entrypoint.Path), nil, runID, appID, env, mountLib)
		case "uid":
			return l.uidCommand(appID, appPath, nil, env)
		case "none":
			cmd := exec.Command(appPath)
			cmd.Dir = appDir
			cmd.Env = env
			return cmd, nil
		default:
			return nil, fmt.Errorf("sandbox has not been probed")
		}

	case manifest.RuntimeQuickJS, manifest.RuntimeMicroPython:
		if m.NativeSDK != nil {
			return nil, fmt.Errorf("app %s: native_sdk is only valid for a native entrypoint", m.ID)
		}
		scriptPath, err := scriptPath(appDir, m.Entrypoint.Path)
		if err != nil {
			return nil, err
		}
		rt, err := LocateRuntime(libDir)
		if err != nil {
			return nil, fmt.Errorf("app %s requires the %s runtime: %w", m.ID, m.Entrypoint.Runtime, err)
		}
		if rt.ABIMajor != 1 {
			return nil, fmt.Errorf("app %s requires the %s runtime: installed native SDK ABI major %d is not supported", m.ID, m.Entrypoint.Runtime, rt.ABIMajor)
		}
		interp, err := rt.Interpreter(m.Entrypoint.Runtime)
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", m.ID, err)
		}
		env := append(l.env(), LibraryEnv(mode, rt)...)
		switch mode {
		case "namespaces", "mountns":
			execPath := filepath.Join(NamespaceLibraryDir, interp.File)
			env = append(env,
				"CAPSULAD_APP_DIR=/app",
				"CAPSULAD_INTERPRETER="+execPath,
			)
			return l.namespaceCommand(m, appDir, execPath, []string{filepath.Join("/app", m.Entrypoint.Path)}, runID, appID, env, libDir)
		case "uid":
			host := filepath.Join(libDir, interp.File)
			env = append(env,
				"CAPSULAD_APP_DIR="+appDir,
				"CAPSULAD_INTERPRETER="+host,
			)
			return l.uidCommand(appID, host, []string{scriptPath}, env)
		case "none":
			host := filepath.Join(libDir, interp.File)
			cmd := exec.Command(host, scriptPath)
			cmd.Dir = appDir
			cmd.Env = append(env,
				"CAPSULAD_APP_DIR="+appDir,
				"CAPSULAD_INTERPRETER="+host,
			)
			return cmd, nil
		default:
			return nil, fmt.Errorf("sandbox has not been probed")
		}
	default:
		return nil, fmt.Errorf("runtime %s is not available in this build", m.Entrypoint.Runtime)
	}
}

// scriptPath resolves a package script and rejects symlink escapes. The
// interpreter executes only a regular file inside the immutable package root.
func scriptPath(appDir, entry string) (string, error) {
	full := filepath.Join(appDir, entry)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("entrypoint: %w", err)
	}
	if !withinDir(appDir, resolved) {
		return "", fmt.Errorf("entrypoint %s resolves outside the package root", entry)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("entrypoint: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("entrypoint %s is not a regular file", entry)
	}
	return resolved, nil
}

// withControlFD forwards the inherited control-socket fd number into the app
// environment. The namespace helper receives the value in its own environment
// and must re-export it because syscall.Exec replaces the environment.
func withControlFD(env []string, fd string) []string {
	if fd == "" {
		return env
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "CAPSULAD_CONTROL_FD=") {
			return env
		}
	}
	return append(env, "CAPSULAD_CONTROL_FD="+fd)
}

// baseEnv is the environment every sandboxed app starts with. The timezone is
// deliberately not forced: /etc/localtime is bind-mounted into the sandbox, and
// a daemon-level TZ is passed through, so apps show the device's wall clock
// instead of UTC.
func baseEnv() []string {
	env := []string{
		"PATH=/usr/bin:/bin",
		"LANG=C.UTF-8",
	}
	if tz, ok := os.LookupEnv("TZ"); ok && tz != "" {
		env = append(env, "TZ="+tz)
	}
	return env
}

// AssignUID returns a stable, distinct host UID for an app.
func (l *Launcher) AssignUID(appID string) (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if uid, ok := l.uids[appID]; ok {
		return uid, uid
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(appID))
	uid := l.cfg.UIDBase + int(h.Sum32()%8000)
	for {
		collision := false
		for _, used := range l.uids {
			if used == uid {
				collision = true
				break
			}
		}
		if !collision {
			break
		}
		uid++
	}
	l.uids[appID] = uid
	return uid, uid
}

// execSpec is passed to the re-executed helper.
type execSpec struct {
	Root          string   `json:"root"`
	AppDir        string   `json:"app_dir"`
	AppPath       string   `json:"app_path"`
	Args          []string `json:"args,omitempty"`
	SocketPath    string   `json:"socket_path"`
	AssetsDir     string   `json:"assets_dir"`
	LibraryDir    string   `json:"library_dir"`
	UID           int      `json:"uid"`
	GID           int      `json:"gid"`
	PIDNamespace  bool     `json:"pid_namespace"`
	UserNamespace bool     `json:"user_namespace"`
	Seccomp       bool     `json:"seccomp"`
	RuntimePaths  []string `json:"runtime_paths"`
	Env           []string `json:"env"`
}

// sandboxSocketEnv maps the daemon's listen path to the path an app must dial.
// In namespace modes the framework socket is bind-mounted at a fixed location
// inside the app's mount namespace, so the host path does not resolve.
func sandboxSocketEnv(mode, hostPath string) string {
	if namespaceMode(mode) {
		return sandboxSocketPath
	}
	return hostPath
}

// specJSON builds the helper payload. env already contains the launch
// environment including any native SDK variables; libraryDir is the host
// directory the helper bind-mounts read-only at /capsulad/lib. execPath is the
// absolute path the helper executes after pivot_root; args are passed to it
// verbatim (a script host receives the in-namespace script path).
func (l *Launcher) specJSON(appDir, execPath string, args []string, runID, appID, mode string, env []string, libraryDir string) (string, error) {
	uid, gid := l.AssignUID(appID)
	root := filepath.Join(l.sandboxRoot(), runID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// The socket is bind-mounted at a fixed in-namespace path, so namespace
	// modes must not hand the app the host path the daemon listens on.
	socket := sandboxSocketEnv(mode, l.cfg.SocketPath)
	fullEnv := make([]string, 0, len(env)+4)
	fullEnv = append(fullEnv, env...)
	fullEnv = append(fullEnv,
		"CAPSULAD_SOCKET="+socket,
		"CAPSULAD_RUN_ID="+runID,
		"CAPSULAD_APP_ID="+appID,
		"CAPSULAD_SDK="+version.SDKVersion,
	)
	spec := execSpec{
		Root: root, AppDir: appDir, AppPath: execPath, Args: args,
		SocketPath: l.cfg.SocketPath, AssetsDir: l.cfg.AssetsDir, LibraryDir: libraryDir,
		UID: uid, GID: gid, PIDNamespace: l.cfg.PIDNamespace, UserNamespace: mode == "namespaces",
		Seccomp: l.Seccomp(), RuntimePaths: l.cfg.RuntimePaths, Env: fullEnv,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// HandleHelper executes the sandbox helper when the daemon is re-executed with
// the internal argument. It returns handled=false for normal daemon startup.
func HandleHelper(args []string) (handled bool, code int) {
	if len(args) < 2 {
		return false, 0
	}
	switch args[1] {
	case internalExecArg:
		return true, runExecHelper()
	case internalProbeArg:
		return true, 0
	case internalSeccompProbeArg:
		return true, seccompProbe()
	default:
		return false, 0
	}
}

const (
	internalExecArg         = "__capsulad_sandbox_exec"
	internalProbeArg        = "__capsulad_sandbox_probe"
	internalSeccompProbeArg = "__capsulad_seccomp_probe"
	specEnvVar              = "CAPSULAD_SANDBOX_SPEC"

	// sandboxSocketPath is where the framework socket is bind-mounted inside
	// an app's mount namespace.
	sandboxSocketPath = "/run/capsulad/hal.sock"
)
