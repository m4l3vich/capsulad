package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"capsulad/internal/manifest"
)

func writeRuntime(t *testing.T, dir, metadata string, files ...string) {
	t.Helper()
	if metadata != "" {
		if err := os.WriteFile(filepath.Join(dir, RuntimeMetadataFile), []byte(metadata), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("shared object"), 0o555); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveLibraryDirExplicit(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveLibraryDir(dir)
	if err != nil {
		t.Fatalf("explicit existing dir: %v", err)
	}
	if got != want {
		t.Fatalf("explicit dir = %q, want canonical %q", got, want)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err = ResolveLibraryDir(rel)
	if err != nil {
		t.Fatalf("relative explicit dir %q: %v", rel, err)
	}
	if got != want {
		t.Fatalf("relative explicit dir = %q, want canonical %q", got, want)
	}

	if _, err := ResolveLibraryDir(filepath.Join("missing", "native-lib")); err == nil {
		t.Fatal("missing relative native library directory must be rejected")
	}
	if _, err := ResolveLibraryDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing native library directory must be rejected")
	}
}

func TestDefaultLibraryDirPrefersRuntimeBesideBinary(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	lib := filepath.Join(root, "lib")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRuntime(t, lib, `{"abi_major":1,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	wantLib, err := filepath.EvalSymlinks(lib)
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultLibraryDir(filepath.Join(bin, "capsulad")); got != wantLib {
		t.Fatalf("build layout resolved %q, want %q", got, wantLib)
	}

	installed := t.TempDir()
	installedLib := filepath.Join(installed, "lib")
	if err := os.MkdirAll(installedLib, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRuntime(t, installedLib, `{"abi_major":1,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	wantInstalled, err := filepath.EvalSymlinks(installedLib)
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultLibraryDir(filepath.Join(installed, "capsulad")); got != wantInstalled {
		t.Fatalf("installed layout resolved %q, want %q", got, wantInstalled)
	}

	if got := defaultLibraryDir(filepath.Join(t.TempDir(), "bin", "capsulad")); got != "" {
		t.Fatalf("absent runtime resolved %q, want empty", got)
	}
}

func TestLocateRuntimeRequiresMetadata(t *testing.T) {
	dir := t.TempDir()
	writeRuntime(t, dir, "", "libcapsulad.so.1")
	_, err := LocateRuntime(dir)
	if err == nil || !strings.Contains(err.Error(), RuntimeMetadataFile) {
		t.Fatalf("missing metadata error = %v, want mention of %s", err, RuntimeMetadataFile)
	}

	// Legacy metadata still carrying the removed capability list must load.
	writeRuntime(t, dir, `{"abi_major":1,"abi_minor":2,"capabilities":["audio","events"],"library":"libcapsulad.so.1"}`)
	rt, err := LocateRuntime(dir)
	if err != nil {
		t.Fatalf("valid metadata: %v", err)
	}
	if rt.ABIMajor != 1 || rt.ABIMinor != 2 || !rt.Metadata || rt.File != "libcapsulad.so.1" {
		t.Fatalf("runtime = %+v", rt)
	}
}

func TestLocateRuntimeRejectsTraversalAndEscapingSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeRuntime(t, dir, `{"abi_major":1,"library":"../capsulad-native.json"}`)
	if _, err := LocateRuntime(dir); err == nil || !strings.Contains(err.Error(), "not a file inside") {
		t.Fatalf("traversal error = %v", err)
	}

	outside := t.TempDir()
	target := filepath.Join(outside, "libcapsulad.so.1")
	if err := os.WriteFile(target, []byte("outside"), 0o555); err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(linkDir, "libcapsulad.so.1")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeRuntime(t, linkDir, `{"abi_major":1,"library":"libcapsulad.so.1"}`)
	if _, err := LocateRuntime(linkDir); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("escaping library error = %v", err)
	}

	metaDir := t.TempDir()
	metaTarget := filepath.Join(outside, RuntimeMetadataFile)
	if err := os.WriteFile(metaTarget, []byte(`{"abi_major":1,"library":"libcapsulad.so.1"}`), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(metaTarget, filepath.Join(metaDir, RuntimeMetadataFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeRuntime(t, metaDir, "", "libcapsulad.so.1")
	if _, err := LocateRuntime(metaDir); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("escaping metadata error = %v", err)
	}
}

func TestCheckRequirement(t *testing.T) {
	rt := &Runtime{ABIMajor: 1, ABIMinor: 2}
	if err := CheckRequirement(rt, manifest.NativeSDK{ABIMajor: 1}); err != nil {
		t.Fatalf("same major: %v", err)
	}
	if err := CheckRequirement(rt, manifest.NativeSDK{ABIMajor: 2}); err == nil || !strings.Contains(err.Error(), "major") {
		t.Fatalf("major mismatch error = %v", err)
	}
	if err := CheckRequirement(rt, manifest.NativeSDK{ABIMajor: 1, MinMinor: 3}); err == nil || !strings.Contains(err.Error(), "1.3") {
		t.Fatalf("minor mismatch error = %v", err)
	}
	if err := CheckRequirement(nil, manifest.NativeSDK{ABIMajor: 1}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("absent runtime error = %v", err)
	}
}

func TestLibraryEnvModes(t *testing.T) {
	rt := &Runtime{Dir: "/opt/capsulad/lib", File: "libcapsulad.so.1"}
	wantLoader := "LD_LIBRARY_PATH"
	if runtime.GOOS == "darwin" {
		wantLoader = "DYLD_LIBRARY_PATH"
	}

	host := strings.Join(LibraryEnv("none", rt), "\n")
	for _, want := range []string{
		"CAPSULAD_LIBRARY=/opt/capsulad/lib/libcapsulad.so.1",
		"CAPSULAD_LIBRARY_DIR=/opt/capsulad/lib",
		wantLoader + "=/opt/capsulad/lib",
	} {
		if !strings.Contains(host, want) {
			t.Fatalf("host env missing %q:\n%s", want, host)
		}
	}

	ns := strings.Join(LibraryEnv("namespaces", rt), "\n")
	for _, want := range []string{
		"CAPSULAD_LIBRARY=" + NamespaceLibraryDir + "/libcapsulad.so.1",
		"CAPSULAD_LIBRARY_DIR=" + NamespaceLibraryDir,
		wantLoader + "=" + NamespaceLibraryDir,
	} {
		if !strings.Contains(ns, want) {
			t.Fatalf("namespace env missing %q:\n%s", want, ns)
		}
	}

	if env := LibraryEnv("none", nil); env != nil {
		t.Fatalf("nil runtime env = %v, want nil", env)
	}
}

func nativeApp(t *testing.T) (*Launcher, *manifest.Manifest) {
	t.Helper()
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, "app"), []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ID: "org.example.native",
		Entrypoint: manifest.Entrypoint{
			Runtime: manifest.RuntimeNative,
			Path:    "app",
		},
		Dir: appDir,
	}
	l := New(Config{Mode: "none"})
	l.setMode("none", "test")
	return l, m
}

func TestCommandCarriesDeviceSerialInNamespaceAndHostModes(t *testing.T) {
	for _, mode := range []string{"none", "uid", "namespaces"} {
		t.Run(mode, func(t *testing.T) {
			l, m := nativeApp(t)
			l.cfg.DeviceSerial = "11bfcbc5d63d55fc"
			l.cfg.SandboxRoot = t.TempDir()
			if mode != "none" {
				l.cfg.Executable = "/capsulad-test-helper"
				l.setMode(mode, "test")
			}
			cmd, err := l.Command(m, m.Dir, "run1", m.ID)
			if err != nil {
				t.Fatalf("command: %v", err)
			}
			env := cmd.Env
			for _, entry := range env {
				if strings.HasPrefix(entry, specEnvVar+"=") {
					var spec execSpec
					if err := json.Unmarshal([]byte(strings.TrimPrefix(entry, specEnvVar+"=")), &spec); err != nil {
						t.Fatalf("decode namespace helper spec: %v", err)
					}
					env = spec.Env
					break
				}
			}
			if !containsEnv(env, "CAPSULAD_DEVICE_SERIAL=11bfcbc5d63d55fc") {
				t.Fatalf("mode %s launch does not carry device serial: %v", mode, env)
			}
		})
	}
}

// The in-namespace helper receives its environment through the spec payload,
// not the helper's own process environment; verify the serial is in it so the
// pid-1 exec inherits the same launch environment.
func TestSpecJSONCarriesDeviceSerial(t *testing.T) {
	l, m := nativeApp(t)
	l.cfg.DeviceSerial = "11bfcbc5d63d55fc"
	l.cfg.SandboxRoot = t.TempDir()
	l.setMode("namespaces", "test")

	raw, err := l.specJSON(m.Dir, "/app/app", nil, "run1", m.ID, "namespaces", l.env(), "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	var spec execSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if !containsEnv(spec.Env, "CAPSULAD_DEVICE_SERIAL=11bfcbc5d63d55fc") {
		t.Fatalf("helper spec env lacks the device serial: %v", spec.Env)
	}
}

func TestCommandOmitsUnknownDeviceSerial(t *testing.T) {
	l, m := nativeApp(t)
	l.cfg.DeviceSerial = " \t "
	cmd, err := l.Command(m, m.Dir, "run1", m.ID)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if containsEnv(cmd.Env, "CAPSULAD_DEVICE_SERIAL=") {
		t.Fatalf("unknown device serial must be omitted: %v", cmd.Env)
	}
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func TestCommandStaticAppWithoutRuntime(t *testing.T) {
	l, m := nativeApp(t)
	l.cfg.LibraryDir = t.TempDir() // empty trusted dir, no runtime
	cmd, err := l.Command(m, m.Dir, "run1", m.ID)
	if err != nil {
		t.Fatalf("static app without runtime: %v", err)
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "CAPSULAD_LIBRARY") || strings.Contains(entry, "LIBRARY_PATH") {
			t.Fatalf("static app received runtime env %q", entry)
		}
	}
}

func TestCommandRequiredRuntimeMissingOrIncompatible(t *testing.T) {
	l, m := nativeApp(t)
	l.cfg.LibraryDir = t.TempDir() // no metadata, no library
	m.NativeSDK = &manifest.NativeSDK{ABIMajor: 1}
	if _, err := l.Command(m, m.Dir, "run1", m.ID); err == nil || !strings.Contains(err.Error(), RuntimeMetadataFile) {
		t.Fatalf("missing runtime error = %v", err)
	}

	dir := t.TempDir()
	writeRuntime(t, dir, `{"abi_major":2,"abi_minor":0,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	l2 := New(Config{Mode: "none", LibraryDir: dir})
	l2.setMode("none", "test")
	if _, err := l2.Command(m, m.Dir, "run1", m.ID); err == nil || !strings.Contains(err.Error(), "major 1") {
		t.Fatalf("incompatible requirement error = %v", err)
	}
}

func TestCommandCompatibleRuntimeSetsEnvironment(t *testing.T) {
	l, m := nativeApp(t)
	dir := t.TempDir()
	writeRuntime(t, dir, `{"abi_major":1,"abi_minor":3,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	l.cfg.LibraryDir = dir
	m.NativeSDK = &manifest.NativeSDK{ABIMajor: 1, MinMinor: 1}
	cmd, err := l.Command(m, m.Dir, "run1", m.ID)
	if err != nil {
		t.Fatalf("compatible app: %v", err)
	}
	env := strings.Join(cmd.Env, "\n")
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env, "CAPSULAD_LIBRARY="+filepath.Join(canonical, "libcapsulad.so.1")) {
		t.Fatalf("CAPSULAD_LIBRARY missing from env:\n%s", env)
	}
}

func writeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDiscoversInstalledInterpreters(t *testing.T) {
	dir := t.TempDir()
	writeRuntime(t, dir, `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	writeExecutable(t, dir, InterpreterQuickJS, "#!/bin/sh\necho 'quickjs-ng 0.18.0'\n")
	if err := os.WriteFile(filepath.Join(dir, InterpreterMicroPython), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt, err := LocateRuntime(dir)
	if err != nil {
		t.Fatalf("locate runtime: %v", err)
	}
	if _, ok := rt.Interpreters[manifest.RuntimeQuickJS]; !ok {
		t.Fatal("QuickJS host was not discovered")
	}
	if _, ok := rt.Interpreters[manifest.RuntimeMicroPython]; ok {
		t.Fatal("a non-executable MicroPython file must not be advertised")
	}
	interp, err := rt.Interpreter(manifest.RuntimeQuickJS)
	if err != nil || interp.File != InterpreterQuickJS {
		t.Fatalf("interpreter = %+v, err = %v", interp, err)
	}
	if _, err := rt.Interpreter(manifest.RuntimeMicroPython); err == nil || !strings.Contains(err.Error(), InterpreterMicroPython) {
		t.Fatalf("missing MicroPython error = %v", err)
	}

	l := New(Config{Mode: "none", LibraryDir: dir})
	l.setMode("none", "test")
	status := l.RuntimeStatus(manifest.RuntimeQuickJS)
	if !status.Available || status.ImplementationVersion != "quickjs-ng 0.18.0" {
		t.Fatalf("QuickJS status = %+v", status)
	}
	if status.Detail == "" {
		t.Fatal("available runtime must describe its implementation")
	}
	missing := l.RuntimeStatus(manifest.RuntimeMicroPython)
	if missing.Available || !strings.Contains(missing.Detail, "MicroPython") {
		t.Fatalf("MicroPython status = %+v", missing)
	}

	all := l.RuntimeStatuses()
	if len(all) != 4 || all[0].Runtime != manifest.RuntimeNative || !all[0].Available ||
		all[3].Runtime != manifest.RuntimeBuiltin || !all[3].Available {
		t.Fatalf("runtime statuses = %+v", all)
	}
}

func TestRuntimeStatusIsHonestWithoutRuntime(t *testing.T) {
	l := New(Config{Mode: "none", LibraryDir: t.TempDir()})
	l.setMode("none", "test")
	status := l.RuntimeStatus(manifest.RuntimeQuickJS)
	if status.Available {
		t.Fatalf("QuickJS advertised without a runtime: %+v", status)
	}
	if !strings.Contains(status.Detail, RuntimeMetadataFile) {
		t.Fatalf("detail does not explain the missing runtime: %q", status.Detail)
	}
}

func TestCommandScriptRuntime(t *testing.T) {
	dir := t.TempDir()
	writeRuntime(t, dir, `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	writeExecutable(t, dir, InterpreterQuickJS, "#!/bin/sh\nexit 0\n")

	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, "main.js"), []byte("capsulad.start()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ID: "org.example.quickjs", Dir: appDir,
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeQuickJS, Path: "main.js"},
	}
	l := New(Config{Mode: "none", LibraryDir: dir})
	l.setMode("none", "test")
	cmd, err := l.Command(m, appDir, "run1", m.ID)
	if err != nil {
		t.Fatalf("script command: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantHost := filepath.Join(canonical, InterpreterQuickJS)
	if cmd.Path != wantHost {
		t.Fatalf("exec path = %q, want %q", cmd.Path, wantHost)
	}
	wantScript, err := filepath.EvalSymlinks(filepath.Join(appDir, "main.js"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Args) != 2 || cmd.Args[1] != wantScript {
		t.Fatalf("args = %v, want the package script %s", cmd.Args, wantScript)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{
		"CAPSULAD_LIBRARY=" + filepath.Join(canonical, "libcapsulad.so.1"),
		"CAPSULAD_APP_DIR=" + appDir,
		"CAPSULAD_INTERPRETER=" + wantHost,
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("script env missing %q:\n%s", want, env)
		}
	}
}

func TestCommandScriptRuntimeMissingHost(t *testing.T) {
	dir := t.TempDir()
	writeRuntime(t, dir, `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	appDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(appDir, "main.py"), []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ID: "org.example.micropython", Dir: appDir,
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeMicroPython, Path: "main.py"},
	}
	l := New(Config{Mode: "none", LibraryDir: dir})
	l.setMode("none", "test")
	if _, err := l.Command(m, appDir, "run1", m.ID); err == nil || !strings.Contains(err.Error(), InterpreterMicroPython) {
		t.Fatalf("missing host error = %v", err)
	}
}

func TestCommandScriptRejectsEntrypointEscape(t *testing.T) {
	dir := t.TempDir()
	writeRuntime(t, dir, `{"abi_major":1,"abi_minor":0,"library":"libcapsulad.so.1"}`, "libcapsulad.so.1")
	writeExecutable(t, dir, InterpreterQuickJS, "#!/bin/sh\nexit 0\n")

	outside := filepath.Join(t.TempDir(), "main.js")
	if err := os.WriteFile(outside, []byte("capsulad.start()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(appDir, "main.js")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m := &manifest.Manifest{
		ID: "org.example.quickjs", Dir: appDir,
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeQuickJS, Path: "main.js"},
	}
	l := New(Config{Mode: "none", LibraryDir: dir})
	l.setMode("none", "test")
	if _, err := l.Command(m, appDir, "run1", m.ID); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("escaping entrypoint error = %v", err)
	}
}

func TestSpecJSONCarriesInterpreterExecPathAndArgs(t *testing.T) {
	l := New(Config{Mode: "namespaces", SocketPath: "/run/hal.sock", LibraryDir: "/rt", SandboxRoot: t.TempDir()})
	l.setMode("namespaces", "test")
	raw, err := l.specJSON("/apps/demo", "/capsulad/lib/capsulad-quickjs", []string{"/app/main.js"}, "run1", "org.example.demo", "namespaces", nil, "/rt")
	if err != nil {
		t.Fatal(err)
	}
	var spec execSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.AppPath != "/capsulad/lib/capsulad-quickjs" {
		t.Fatalf("spec app path = %q", spec.AppPath)
	}
	if len(spec.Args) != 1 || spec.Args[0] != "/app/main.js" {
		t.Fatalf("spec args = %v", spec.Args)
	}
	if spec.LibraryDir != "/rt" {
		t.Fatalf("spec library dir = %q", spec.LibraryDir)
	}
}

func TestCommandRejectsExplicitInvalidLibraryDir(t *testing.T) {
	l, m := nativeApp(t)
	l.cfg.LibraryDir = filepath.Join(t.TempDir(), "missing")
	if _, err := l.Command(m, m.Dir, "run1", m.ID); err == nil || !strings.Contains(err.Error(), "native library directory") {
		t.Fatalf("invalid explicit dir error = %v", err)
	}
}
