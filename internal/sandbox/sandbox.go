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
	UIDBase      int
	PIDNamespace bool
	Seccomp      bool
	Executable   string // daemon binary used for the in-namespace helper
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
	tz := l.Timezone()
	if tz == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "TZ=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "TZ="+tz)
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

// Command builds the app process command. Native binaries only: script
// runtimes return an explicit unavailable error rather than fake execution.
func (l *Launcher) Command(m *manifest.Manifest, appDir, runID, appID string) (*exec.Cmd, error) {
	if m.Entrypoint.Runtime != manifest.RuntimeNative {
		return nil, fmt.Errorf("runtime %s is not available in this build", m.Entrypoint.Runtime)
	}
	absDir, err := filepath.Abs(appDir)
	if err != nil {
		return nil, err
	}
	appDir = absDir
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
	l.mu.Lock()
	mode := l.mode
	l.mu.Unlock()
	switch mode {
	case "namespaces", "mountns":
		return l.namespaceCommand(m, appDir, appPath, runID, appID)
	case "uid":
		return l.uidCommand(appID, appPath)
	case "none":
		cmd := exec.Command(appPath)
		cmd.Dir = appDir
		cmd.Env = baseEnv()
		return cmd, nil
	default:
		return nil, fmt.Errorf("sandbox has not been probed")
	}
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
	SocketPath    string   `json:"socket_path"`
	AssetsDir     string   `json:"assets_dir"`
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
	if mode == "namespaces" || mode == "mountns" {
		return sandboxSocketPath
	}
	return hostPath
}

func (l *Launcher) specJSON(m *manifest.Manifest, appDir, appPath, runID, appID, mode string) (string, error) {
	uid, gid := l.AssignUID(appID)
	root := filepath.Join("/run/capsulad/sandbox", runID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// The socket is bind-mounted at a fixed in-namespace path, so namespace
	// modes must not hand the app the host path the daemon listens on.
	socket := sandboxSocketEnv(mode, l.cfg.SocketPath)
	env := append(l.env(),
		"CAPSULA_SOCKET="+socket,
		"CAPSULA_RUN_ID="+runID,
		"CAPSULA_APP_ID="+appID,
		"CAPSULA_SDK="+version.SDKVersion,
	)
	spec := execSpec{
		Root: root, AppDir: appDir, AppPath: filepath.Join("/app", m.Entrypoint.Path),
		SocketPath: l.cfg.SocketPath, AssetsDir: l.cfg.AssetsDir,
		UID: uid, GID: gid, PIDNamespace: l.cfg.PIDNamespace, UserNamespace: mode == "namespaces",
		Seccomp: l.Seccomp(), RuntimePaths: l.cfg.RuntimePaths, Env: env,
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
	internalExecArg         = "__capsula_sandbox_exec"
	internalProbeArg        = "__capsula_sandbox_probe"
	internalSeccompProbeArg = "__capsula_seccomp_probe"
	specEnvVar              = "CAPSULA_SANDBOX_SPEC"

	// sandboxSocketPath is where the framework socket is bind-mounted inside
	// an app's mount namespace.
	sandboxSocketPath = "/run/capsulad/hal.sock"
)
