package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"capsulad/internal/artifact"
)

const cliTestManifest = `
id = "org.example.tested"
name = "Tested app"
version = "0.3.0"
[entrypoint]
runtime = "native"
path = "bin/app"
architecture = "aarch64"
abi = "linux-gnu"
`

func writeTestFile(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestAppPackageWritesCanonicalArtifact(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "manifest.toml", cliTestManifest, 0o644)
	writeTestFile(t, dir, "bin/app", "#!/bin/sh\n", 0o755)
	out := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := runAppPackage([]string{dir, "-out", out}); err != nil {
		t.Fatalf("package: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	pkg, err := artifact.Open(out)
	if err != nil {
		t.Fatalf("open written artifact: %v", err)
	}
	defer pkg.Close()
	if pkg.Manifest.ID != "org.example.tested" || pkg.Manifest.Version != "0.3.0" {
		t.Fatalf("manifest = %s %s", pkg.Manifest.ID, pkg.Manifest.Version)
	}
}

func TestAppPackageDefaultName(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "manifest.toml", cliTestManifest, 0o644)
	writeTestFile(t, dir, "bin/app", "#!/bin/sh\n", 0o755)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatal(err)
		}
	}()
	if err := runAppPackage([]string{dir}); err != nil {
		t.Fatalf("package: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "org.example.tested-0.3.0.tar.gz")); err != nil {
		t.Fatalf("default artifact name: %v", err)
	}
}

// A dry run inspects the bundle, executes the incoming daemon's -version probe
// and prints a plan without writing into the data directory.
func TestDeployDryRunChangesNothing(t *testing.T) {
	bundle := t.TempDir()
	writeTestFile(t, bundle, "capsulad", "#!/bin/sh\necho 'capsulad test (sdk 0.5)'\n", 0o755)
	writeTestFile(t, bundle, "lib/capsula-native.json", `{"abi_major":1,"abi_minor":0,"capabilities":[],"library":"libcapsula.so.1"}`, 0o644)
	writeTestFile(t, bundle, "boot/S99btctl.sh", "#!/bin/sh\nexit 0\n", 0o755)
	dataDir := t.TempDir()
	initScript := filepath.Join(t.TempDir(), "S99btctl.sh")
	writeTestFile(t, filepath.Dir(initScript), filepath.Base(initScript), "#!/bin/sh\nexit 0\n", 0o755)

	if err := runDeploy([]string{bundle, "-dry-run", "-init-script", initScript}, dataDir, ""); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dry run wrote into the data dir: %v", entries)
	}
}

func TestDeployRejectsInvalidBootHook(t *testing.T) {
	bundle := t.TempDir()
	writeTestFile(t, bundle, "boot/S99btctl.sh", "#!/bin/sh\nif [ -f x ]; then\n", 0o755)
	err := runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "not a valid POSIX shell script") {
		t.Fatalf("error = %v, want a shell validation failure", err)
	}
}

func TestDeployRejectsEmptyBundle(t *testing.T) {
	err := runDeploy([]string{t.TempDir(), "-dry-run"}, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "no actionable payload") {
		t.Fatalf("error = %v, want an empty-bundle rejection", err)
	}
}

func TestDeployIgnoresUnknownEntries(t *testing.T) {
	bundle := t.TempDir()
	writeTestFile(t, bundle, "capsulad", "#!/bin/sh\necho 'capsulad test'\n", 0o755)
	writeTestFile(t, bundle, "unexpected.txt", "ignored", 0o644)
	initScript := filepath.Join(t.TempDir(), "S99btctl.sh")
	writeTestFile(t, filepath.Dir(initScript), filepath.Base(initScript), "#!/bin/sh\nexit 0\n", 0o755)
	if err := runDeploy([]string{bundle, "-dry-run", "-init-script", initScript}, t.TempDir(), ""); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// the command printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), runErr
}

// A dry run must not execute the bundle's daemon: the plan says the probe is
// deferred, even when the binary would fail the real probe.
func TestDeployDryRunDefersDaemonProbe(t *testing.T) {
	bundle := t.TempDir()
	writeTestFile(t, bundle, "capsulad", "#!/bin/sh\nexit 7\n", 0o755)
	dataDir := t.TempDir()
	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run"}, dataDir, "")
	})
	if err != nil {
		t.Fatalf("dry run with a non-runnable daemon: %v", err)
	}
	if !strings.Contains(out, "daemon probe: deferred (-dry-run)") {
		t.Fatalf("plan does not defer the probe:\n%s", out)
	}
	if !strings.Contains(out, "verifying their origin is the operator's decision") {
		t.Fatalf("plan does not state the bundle-trust decision:\n%s", out)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dry run wrote into the data dir: %v", entries)
	}
}

// The bundle binary runs as root, so the probe must not inherit the operator's
// environment.
func TestVerifyDaemonBinaryUsesMinimalEnvironment(t *testing.T) {
	t.Setenv("CAPSULA_PROBE_LEAK", "secret")
	t.Setenv("OPERATOR_SECRET", "secret")
	binary := filepath.Join(t.TempDir(), "capsulad")
	script := "#!/bin/sh\n" +
		"[ -z \"$CAPSULA_PROBE_LEAK\" ] || exit 9\n" +
		"[ -z \"$OPERATOR_SECRET\" ] || exit 9\n" +
		"[ -n \"$HOME\" ] || exit 8\n" +
		"[ \"$LANG\" = C ] || exit 7\n" +
		"echo 'capsulad test (sdk 0.5)'\n"
	writeTestFile(t, filepath.Dir(binary), filepath.Base(binary), script, 0o755)
	if err := verifyDaemonBinary(binary); err != nil {
		t.Fatalf("probe with a minimal environment: %v", err)
	}
}

// The bundle budget is sized for the 78.5 MiB data partition.
func TestBundleLimitsFitDataPartition(t *testing.T) {
	if bundleLimits.MaxFileBytes != 64<<20 || bundleLimits.MaxTotalBytes != 128<<20 || bundleLimits.MaxEntries != 8192 {
		t.Fatalf("bundle limits = %+v, want 64 MiB per file, 128 MiB total, 8192 entries", bundleLimits)
	}
	if extractionReserve != 32<<20 {
		t.Fatalf("extraction reserve = %d, want 32 MiB", extractionReserve)
	}
}

// The preflight refuses extraction when the archive plus reserve exceeds the
// free space, and stays silent when free space is unknown.
func TestCheckExtractionSpace(t *testing.T) {
	old := freeSpace
	defer func() { freeSpace = old }()
	dir := t.TempDir()

	freeSpace = func(string) (uint64, bool, error) { return 8 << 20, true, nil }
	err := checkExtractionSpace(dir, 1)
	if err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("low space error = %v, want an actionable free-space error", err)
	}

	freeSpace = func(string) (uint64, bool, error) { return 256 << 20, true, nil }
	if err := checkExtractionSpace(dir, 1<<20); err != nil {
		t.Fatalf("sufficient space: %v", err)
	}

	freeSpace = func(string) (uint64, bool, error) { return 0, false, nil }
	if err := checkExtractionSpace(dir, 1<<20); err != nil {
		t.Fatalf("unknown free space should skip the preflight: %v", err)
	}
}

func TestUnpackBundleRefusesWhenDataPartitionIsFull(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bundle.tar.gz")
	buildTestArchive(t, archive, map[string]string{"capsulad": "#!/bin/sh\nexit 0\n"})
	old := freeSpace
	defer func() { freeSpace = old }()
	freeSpace = func(string) (uint64, bool, error) { return 1 << 20, true, nil }
	_, _, err := unpackBundle(archive, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("unpack error = %v, want a free-space rejection", err)
	}
}

// buildTestArchive writes a .tar.gz with the given regular files.
func buildTestArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Directory bundles are inspected in place, so a symlink would smuggle a file
// from outside the bundle into an installation.
func TestDeployRejectsSymlinkedBundleEntries(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(payload, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		set  func(t *testing.T, bundle string)
	}{
		{
			name: "daemon binary",
			set: func(t *testing.T, bundle string) {
				if err := os.Symlink(payload, filepath.Join(bundle, "capsulad")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lib directory",
			set: func(t *testing.T, bundle string) {
				link := filepath.Join(bundle, "lib")
				if err := os.Symlink(filepath.Dir(payload), link); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "app archive",
			set: func(t *testing.T, bundle string) {
				if err := os.MkdirAll(filepath.Join(bundle, "apps"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(payload, filepath.Join(bundle, "apps", "app.tar.gz")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "boot hook",
			set: func(t *testing.T, bundle string) {
				if err := os.MkdirAll(filepath.Join(bundle, "boot"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(payload, filepath.Join(bundle, "boot", bootHookName)); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			tc.set(t, bundle)
			err := runDeploy([]string{bundle, "-dry-run"}, t.TempDir(), "")
			if err == nil || !strings.Contains(err.Error(), "symlinks are not supported") {
				t.Fatalf("error = %v, want a symlink rejection", err)
			}
		})
	}
}

func TestSyncLibRefusesSymlinkedRuntimeDirectories(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, src, "capsula-native.json", "{}", 0o644)
	writeTestFile(t, src, "nested/lib.so", "x", 0o644)

	outside := t.TempDir()
	dataDir := t.TempDir()

	linkLib := filepath.Join(dataDir, "lib")
	if err := os.Symlink(outside, linkLib); err != nil {
		t.Fatal(err)
	}
	if err := syncLib(src, linkLib); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked lib dir error = %v, want a symlink refusal", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("wrote through a symlinked lib dir: %v, %v", entries, err)
	}

	realLib := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(realLib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(realLib, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := syncLib(src, realLib); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked lib subdirectory error = %v, want a symlink refusal", err)
	}
}

// The boot hook is only transiently replaced: a failed deployment restores the
// previous script so the rollback restart runs the known-good hook.
func TestActivateBootHookRestoresPreviousOnRollback(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "S99btctl.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n# original\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &deployment{}
	if err := d.activateBootHook(src, hook); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(hook); err != nil || strings.Contains(string(body), "original") {
		t.Fatalf("hook was not replaced: %q, %v", body, err)
	}
	if _, err := os.Stat(hook + ".prev"); err != nil {
		t.Fatalf("previous hook was not preserved: %v", err)
	}

	err := d.rollback(nil, errors.New("health gate failed"))
	if err == nil || !strings.Contains(err.Error(), "health gate failed") {
		t.Fatalf("rollback error = %v, want the original failure", err)
	}
	body, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "original") {
		t.Fatalf("previous hook was not restored: %q", string(body))
	}
	if _, err := os.Stat(hook + ".prev"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient hook backup survived rollback: %v", err)
	}
}

// A relative -data-dir is resolved once and reported as an absolute path.
func TestDeployResolvesDataDirAbsolute(t *testing.T) {
	work := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatal(err)
		}
	}()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	bundle := t.TempDir()
	writeTestFile(t, bundle, "lib/capsula-native.json", `{"abi_major":1,"abi_minor":0,"capabilities":[],"library":"libcapsula.so.1"}`, 0o644)

	out, err := captureStdout(t, func() error {
		return runDeploy([]string{bundle, "-dry-run"}, "relative-data", "")
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	want := "data dir: " + filepath.Join(cwd, "relative-data")
	if !strings.Contains(out, want) {
		t.Fatalf("plan does not print the absolute data dir %q:\n%s", want, out)
	}
}

// A successful deployment deletes the transient backups instead of keeping a
// durable copy of the replaced binaries or hook.
func TestAcceptDropsTransientBackups(t *testing.T) {
	dir := t.TempDir()
	daemon := filepath.Join(dir, "capsulad")
	hook := filepath.Join(dir, "S99btctl.sh")
	for _, p := range []string{daemon, daemon + ".prev", hook, hook + ".prev"} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := &deployment{hook: &activation{target: hook, previous: hook + ".prev", hadBefore: true}}
	d.accept([]activation{{target: daemon, previous: daemon + ".prev", hadBefore: true}})
	for _, p := range []string{daemon + ".prev", hook + ".prev"} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("transient backup %s survived acceptance: %v", p, err)
		}
	}
	for _, p := range []string{daemon, hook} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("activated file %s disappeared: %v", p, err)
		}
	}
}
