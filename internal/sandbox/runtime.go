package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"capsulad/internal/manifest"
)

// The shared native SDK runtime lives outside every app package: one copy on
// the host, bind-mounted read-only into namespace sandboxes. Apps locate it
// through the launcher environment instead of bundling their own.
const (
	// RuntimeMetadataFile is emitted by the native build next to the shared
	// library. It describes the ABI and capability vocabulary the installed
	// runtime provides.
	RuntimeMetadataFile = "capsula-native.json"

	// NamespaceLibraryDir is the fixed read-only location of the runtime
	// inside an app mount namespace.
	NamespaceLibraryDir = "/capsula/lib"

	// InterpreterQuickJS is the script host executable installed beside the
	// native runtime. It embeds QuickJS and drives the public C ABI.
	InterpreterQuickJS = "capsula-quickjs"

	// InterpreterMicroPython is the script host executable installed beside the
	// native runtime. It embeds MicroPython and drives the public C ABI.
	InterpreterMicroPython = "capsula-micropython"
)

// Interpreter is one script host installed in the shared runtime directory.
// File is relative to Runtime.Dir; Version is reported by the host's
// --version flag and is only filled by RuntimeStatus.
type Interpreter struct {
	Runtime manifest.Runtime
	File    string
	Version string
}

// RuntimeStatus is the honest answer to ListRuntimes for one runtime. A
// script runtime is available only when its host executable is actually
// installed beside a compatible native SDK runtime.
type RuntimeStatus struct {
	Runtime               manifest.Runtime
	Available             bool
	ImplementationVersion string
	Detail                string
}

// RuntimeInfo is the on-disk metadata format. Unknown fields are ignored so a
// newer native build can add diagnostics without breaking the supervisor.
type RuntimeInfo struct {
	ABIMajor     int      `json:"abi_major"`
	ABIMinor     int      `json:"abi_minor"`
	Capabilities []string `json:"capabilities"`
	Library      string   `json:"library"`
}

// Runtime describes one installed shared native SDK.
type Runtime struct {
	Dir          string
	File         string // file name relative to Dir
	ABIMajor     int
	ABIMinor     int
	Capabilities map[string]bool
	Metadata     bool // capsula-native.json was present and valid
	// Interpreters maps a script runtime to its installed host executable.
	// A runtime without a host is absent from the map; that is the only
	// truthful answer until the host is staged beside the library.
	Interpreters map[manifest.Runtime]Interpreter
}

// ResolveLibraryDir returns the host directory that holds the shared native
// SDK. An explicit value may be relative or absolute; it is normalized to an
// absolute canonical directory once (symlinks resolved) so the mount source
// never depends on the caller's spelling. Without an explicit value the
// launcher looks beside the executable: <exe-dir>/lib first (installed daemon
// layout), then <exe-dir>/../lib (build/bin -> build/lib). It returns an empty
// string when neither candidate exists.
func ResolveLibraryDir(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("native library directory %q: %w", explicit, err)
		}
		canonical, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", fmt.Errorf("native library directory %s: %w", abs, err)
		}
		canonical = filepath.Clean(canonical)
		if canonical == string(filepath.Separator) {
			return "", fmt.Errorf("native library directory must not be the filesystem root")
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return "", fmt.Errorf("native library directory %s: %w", canonical, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("native library directory %s is not a directory", canonical)
		}
		return canonical, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil
	}
	return defaultLibraryDir(exe), nil
}

// DefaultLibraryDir resolves the executable-adjacent runtime directory for
// the running binary. It is the same lookup the launcher performs.
func DefaultLibraryDir() string {
	dir, _ := ResolveLibraryDir("")
	return dir
}

func defaultLibraryDir(exePath string) string {
	exeDir := filepath.Dir(exePath)
	candidates := []string{
		filepath.Join(exeDir, "lib"),
		filepath.Join(exeDir, "..", "lib"),
	}
	// Prefer the candidate that actually contains a library. This makes
	// build/bin -> build/lib work without a stale empty build/bin/lib
	// shadowing it.
	for _, candidate := range candidates {
		if hasLibrary(candidate) {
			return canonicalDir(candidate)
		}
	}
	for _, candidate := range candidates {
		if hasRuntime(candidate) {
			return canonicalDir(candidate)
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return canonicalDir(candidate)
		}
	}
	return ""
}

func hasLibrary(dir string) bool {
	for _, name := range libraryCandidates() {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func canonicalDir(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func hasRuntime(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, RuntimeMetadataFile)); err == nil {
		return true
	}
	for _, name := range libraryCandidates() {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// HasRuntime reports whether dir holds a shared runtime or its metadata. It is
// used to prefer a populated install directory over an empty one.
func HasRuntime(dir string) bool {
	return hasRuntime(dir)
}

func libraryCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"libcapsula.1.dylib", "libcapsula.dylib", "libcapsula.so.1", "libcapsula.so"}
	case "linux":
		return []string{"libcapsula.so.1", "libcapsula.so", "libcapsula.1.dylib", "libcapsula.dylib"}
	default:
		return []string{"libcapsula.so.1", "libcapsula.so", "libcapsula.1.dylib", "libcapsula.dylib"}
	}
}

// LocateRuntime reads an installed runtime and requires its metadata. It is
// the strict lookup used to admit an app that declares [native_sdk]: without
// metadata the supervisor cannot check the declared requirement. The metadata
// is a preflight claim, not proof that the library's exported ABI matches.
func LocateRuntime(dir string) (*Runtime, error) {
	return findRuntime(dir, true)
}

// findRuntime discovers the shared library. In non-strict mode, missing
// metadata or a missing library yields (nil, nil): optional SDK use must not
// break apps that never asked for it.
func findRuntime(dir string, strict bool) (*Runtime, error) {
	if dir == "" {
		if strict {
			return nil, fmt.Errorf("no native SDK runtime directory is configured; install the runtime in <daemon-dir>/lib or pass -native-lib-dir DIR")
		}
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if strict {
			return nil, fmt.Errorf("native SDK runtime directory %s: %w; install the runtime there or pass -native-lib-dir DIR", dir, err)
		}
		return nil, nil
	}
	if !info.IsDir() {
		if strict {
			return nil, fmt.Errorf("native SDK runtime path %s is not a directory", dir)
		}
		return nil, nil
	}

	rt := &Runtime{
		Dir: dir, Capabilities: map[string]bool{},
		Interpreters: map[manifest.Runtime]Interpreter{},
	}
	metaPath := filepath.Join(dir, RuntimeMetadataFile)
	metaOK := true
	if resolved, err := filepath.EvalSymlinks(metaPath); err == nil {
		if !withinDir(dir, resolved) {
			if strict {
				return nil, fmt.Errorf("native SDK metadata %s resolves outside %s; refusing to read it", metaPath, dir)
			}
			metaOK = false
		}
	} else if !os.IsNotExist(err) {
		if strict {
			return nil, fmt.Errorf("native SDK metadata %s: %w", metaPath, err)
		}
		metaOK = false
	}
	var data []byte
	var readErr error
	if metaOK {
		data, readErr = os.ReadFile(metaPath)
	} else {
		readErr = os.ErrNotExist
	}
	switch {
	case readErr == nil:
		var md RuntimeInfo
		if err := json.Unmarshal(data, &md); err != nil {
			if strict {
				return nil, fmt.Errorf("native SDK metadata %s is malformed: %w; rebuild the native SDK", metaPath, err)
			}
			metaOK = false
		} else if err := validateMetadata(&md); err != nil {
			if strict {
				return nil, fmt.Errorf("native SDK metadata %s: %w", metaPath, err)
			}
			metaOK = false
		} else {
			rt.ABIMajor = md.ABIMajor
			rt.ABIMinor = md.ABIMinor
			rt.Metadata = true
			rt.File = md.Library
			for _, capability := range md.Capabilities {
				rt.Capabilities[capability] = true
			}
		}
	case os.IsNotExist(readErr):
		if strict {
			return nil, fmt.Errorf("native SDK metadata %s is missing; install a native build that emits %s", metaPath, RuntimeMetadataFile)
		}
	default:
		if strict {
			return nil, fmt.Errorf("native SDK metadata %s: %w", metaPath, readErr)
		}
	}
	if !metaOK {
		rt.Capabilities = map[string]bool{}
		rt.File = ""
	}

	name := rt.File
	if name != "" && !validLibraryName(name) {
		if strict {
			return nil, fmt.Errorf("native SDK metadata %s names %q, which is not a file inside the runtime directory", metaPath, name)
		}
		name = ""
	}
	if name == "" {
		for _, candidate := range libraryCandidates() {
			if _, err := os.Stat(filepath.Join(dir, candidate)); err == nil {
				name = candidate
				break
			}
		}
	}
	if name == "" {
		if strict {
			return nil, fmt.Errorf("native SDK runtime library is missing from %s (expected one of %s); install the native SDK runtime there", dir, strings.Join(libraryCandidates(), ", "))
		}
		return nil, nil
	}
	full := filepath.Join(dir, name)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if strict {
			return nil, fmt.Errorf("native SDK runtime library %s: %w; rebuild or reinstall the native SDK", full, err)
		}
		return nil, nil
	}
	if !withinDir(dir, resolved) {
		return nil, fmt.Errorf("native SDK runtime library %s resolves outside %s; refusing to expose it", full, dir)
	}
	fileInfo, statErr := os.Stat(resolved)
	if statErr != nil || !fileInfo.Mode().IsRegular() {
		if strict {
			return nil, fmt.Errorf("native SDK runtime library %s is not a readable regular file", full)
		}
		return nil, nil
	}
	rt.File = name
	discoverInterpreters(rt)
	return rt, nil
}

// interpreterCandidates lists the installed host names for a script runtime.
// The unsuffixed name is the device layout; the .exe spelling keeps a host
// directory usable when it was staged on a Windows development machine.
func interpreterCandidates(rt manifest.Runtime) []string {
	switch rt {
	case manifest.RuntimeQuickJS:
		return []string{InterpreterQuickJS, InterpreterQuickJS + ".exe"}
	case manifest.RuntimeMicroPython:
		return []string{InterpreterMicroPython, InterpreterMicroPython + ".exe"}
	default:
		return nil
	}
}

// discoverInterpreters records executable script hosts that live beside the
// shared library. A host is optional: native apps never need one, so a missing
// or non-executable host simply leaves that runtime unavailable.
func discoverInterpreters(rt *Runtime) {
	for _, scriptRuntime := range []manifest.Runtime{manifest.RuntimeQuickJS, manifest.RuntimeMicroPython} {
		file, ok := findInterpreterFile(rt.Dir, scriptRuntime)
		if !ok {
			continue
		}
		rt.Interpreters[scriptRuntime] = Interpreter{Runtime: scriptRuntime, File: file}
	}
}

func findInterpreterFile(dir string, scriptRuntime manifest.Runtime) (string, bool) {
	for _, name := range interpreterCandidates(scriptRuntime) {
		full := filepath.Join(dir, name)
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil || !withinDir(dir, resolved) {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return name, true
	}
	return "", false
}

// Interpreter returns the installed host for a script runtime.
func (rt *Runtime) Interpreter(scriptRuntime manifest.Runtime) (Interpreter, error) {
	if rt == nil {
		return Interpreter{}, fmt.Errorf("%s runtime is unavailable: the shared native SDK runtime is not installed", scriptRuntime)
	}
	if interp, ok := rt.Interpreters[scriptRuntime]; ok {
		return interp, nil
	}
	want := strings.Join(interpreterCandidates(scriptRuntime), " or ")
	return Interpreter{}, fmt.Errorf("%s runtime is unavailable: %s is not installed in %s", scriptRuntime, want, rt.Dir)
}

// interpreterVersion asks the host for its embedded engine version. The host
// prints it without loading the shared library or touching the app bus.
func interpreterVersion(dir, file string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(dir, file), "--version").Output()
	if err != nil {
		return ""
	}
	version := strings.TrimSpace(string(out))
	if i := strings.IndexByte(version, '\n'); i >= 0 {
		version = strings.TrimSpace(version[:i])
	}
	return version
}

// validateMetadata checks the generated metadata contract. It describes what
// the installed runtime claims to provide; it is a preflight requirement
// check, not proof that the library's exported ABI matches.
func validateMetadata(md *RuntimeInfo) error {
	if md.ABIMajor < 1 {
		return fmt.Errorf("declares abi_major %d; expected at least 1", md.ABIMajor)
	}
	if md.ABIMinor < 0 {
		return fmt.Errorf("declares negative abi_minor %d", md.ABIMinor)
	}
	for _, capability := range md.Capabilities {
		if !manifest.ValidNativeCapability(capability) {
			return fmt.Errorf("declares malformed capability %q", capability)
		}
	}
	return nil
}

func validLibraryName(name string) bool {
	return name != "" && filepath.Base(name) == name &&
		!strings.ContainsAny(name, `/\`) && name != "." && name != ".."
}

// withinDir reports whether path resolves to a location inside dir. Both are
// symlink-resolved so a link cannot point the sandbox at an arbitrary host
// directory.
func withinDir(dir, path string) bool {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		realDir = filepath.Clean(dir)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath = filepath.Clean(path)
	}
	rel, err := filepath.Rel(realDir, realPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// CheckRequirement reports whether the installed runtime satisfies an app's
// declared ABI and capability requirements. Major versions must match, the
// installed minor must cover the requested minimum, and every requested
// capability must be present.
func CheckRequirement(rt *Runtime, req manifest.NativeSDK) error {
	if rt == nil {
		return fmt.Errorf("native SDK runtime is not installed")
	}
	if req.ABIMajor != rt.ABIMajor {
		return fmt.Errorf("app requires native SDK ABI major %d but the installed runtime provides major %d (install a compatible runtime)", req.ABIMajor, rt.ABIMajor)
	}
	if req.MinMinor > rt.ABIMinor {
		return fmt.Errorf("app requires native SDK ABI %d.%d or newer but the installed runtime provides %d.%d (install a newer runtime)", req.ABIMajor, req.MinMinor, rt.ABIMajor, rt.ABIMinor)
	}
	var missing []string
	for _, capability := range req.RequiredCapabilities {
		if !rt.Capabilities[capability] {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("installed native SDK runtime lacks required capabilities %s (available: %s)", strings.Join(missing, ", "), describeCapabilities(rt.Capabilities))
	}
	return nil
}

func describeCapabilities(capabilities map[string]bool) string {
	if len(capabilities) == 0 {
		return "none"
	}
	names := make([]string, 0, len(capabilities))
	for name := range capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// LibraryEnv is the launch environment that points an app at the shared SDK.
// In namespace modes the runtime is mounted read-only at NamespaceLibraryDir;
// in uid/none modes the app receives the host path. CAPSULA_LIBRARY names the
// exact file a binding should dlopen; the loader path names the directory so
// the runtime's own dependencies/companion libraries resolve.
func LibraryEnv(mode string, rt *Runtime) []string {
	if rt == nil || rt.File == "" {
		return nil
	}
	name := filepath.Base(rt.File)
	dir := rt.Dir
	file := filepath.Join(rt.Dir, name)
	if namespaceMode(mode) {
		dir = NamespaceLibraryDir
		file = filepath.Join(NamespaceLibraryDir, name)
	}
	loader := "LD_LIBRARY_PATH"
	if runtime.GOOS == "darwin" {
		loader = "DYLD_LIBRARY_PATH"
	}
	return []string{
		"CAPSULA_LIBRARY=" + file,
		"CAPSULA_LIBRARY_DIR=" + dir,
		loader + "=" + dir,
	}
}

// RuntimeStatus reports whether one runtime can actually start an app on this
// host. It is the source of truth for ApplicationService.ListRuntimes: a
// script runtime is never advertised as available until its real embedded
// engine host is installed beside a compatible shared SDK.
func (l *Launcher) RuntimeStatus(r manifest.Runtime) RuntimeStatus {
	status := RuntimeStatus{Runtime: r}
	switch r {
	case manifest.RuntimeNative:
		status.Available = true
		status.Detail = "native entrypoints run directly in the probed sandbox"
		return status
	case manifest.RuntimeBuiltin:
		status.Available = true
		status.Detail = "compiled into capsulad; no child process"
		return status
	case manifest.RuntimeQuickJS, manifest.RuntimeMicroPython:
	default:
		status.Detail = "unknown runtime"
		return status
	}

	name := "QuickJS"
	if r == manifest.RuntimeMicroPython {
		name = "MicroPython"
	}
	dir, err := l.libraryDir()
	if err != nil {
		status.Detail = fmt.Sprintf("no %s host runner: %v", name, err)
		return status
	}
	rt, err := LocateRuntime(dir)
	if err != nil {
		status.Detail = fmt.Sprintf("no %s host runner: %v", name, err)
		return status
	}
	if rt.ABIMajor != 1 {
		status.Detail = fmt.Sprintf("no %s host runner: installed native SDK ABI %d is not supported (need major 1)", name, rt.ABIMajor)
		return status
	}
	interp, err := rt.Interpreter(r)
	if err != nil {
		status.Detail = fmt.Sprintf("no %s host runner is built for this target; script packages are rejected rather than executed without bindings (install %s in %s)", name, strings.Join(interpreterCandidates(r), " or "), dir)
		return status
	}
	status.Available = true
	status.ImplementationVersion = interpreterVersion(dir, interp.File)
	status.Detail = fmt.Sprintf("%s host %s with shared native SDK ABI %d.%d", name, interp.File, rt.ABIMajor, rt.ABIMinor)
	if status.ImplementationVersion == "" {
		status.Detail += "; engine version did not report from --version"
	}
	return status
}

// RuntimeStatuses lists every runtime in the order the framework API has
// always used: native, QuickJS, MicroPython, built-in.
func (l *Launcher) RuntimeStatuses() []RuntimeStatus {
	return []RuntimeStatus{
		l.RuntimeStatus(manifest.RuntimeNative),
		l.RuntimeStatus(manifest.RuntimeQuickJS),
		l.RuntimeStatus(manifest.RuntimeMicroPython),
		l.RuntimeStatus(manifest.RuntimeBuiltin),
	}
}

// namespaceMode reports whether apps see a pivoted, read-only mount tree.
func namespaceMode(mode string) bool {
	return mode == "namespaces" || mode == "mountns"
}
